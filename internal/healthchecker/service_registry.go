package healthchecker

import (
	"context"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"math/rand"
	"net"
	"strconv"
	"sync"
	"time"

	infrastructurev1alpha1 "github.com/EdgeCDN-X/edgecdnx-controller/api/v1alpha1"
	"k8s.io/apimachinery/pkg/types"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

type NodeManager struct {
	location     *infrastructurev1alpha1.Location
	locationHash string
	nodes        map[string]*NodeCheck
	cancelFunc   map[string]context.CancelFunc
	changeFunc   func(node *NodeCheck, location *infrastructurev1alpha1.Location, oldCode, newCode int)
}

func (nm *NodeManager) NodeKey(node *infrastructurev1alpha1.NodeSpec, version int) string {
	return node.Name + ":ipv" + strconv.Itoa(version)
}

func (nm *NodeManager) StartHealthChecks(key string) {

	node, exists := nm.nodes[key]
	if !exists {
		logf.Log.Error(nil, "Node not found for health checks", "key", key)
		return
	}

	logf.Log.Info("Starting health checks for node", "key", key, "node", nm.nodes[key])

	ctx, cancel := context.WithCancel(context.Background())
	nm.cancelFunc[key] = cancel

	go func() {
		// Initial random delay to avoid thundering herd
		sleepDuration := time.Duration(1+rand.Intn(int(node.Interval.Seconds()))) * time.Second
		time.Sleep(sleepDuration)

		ticker := time.NewTicker(node.Interval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				oldCode := node.LastRetCode
				newCode, message, alive := node.HealthCheck()
				node.LastRetCode = newCode
				node.LastRetMessage = message
				node.LastCheckTime = time.Now()
				node.Alive = alive

				logf.Log.V(1).Info("Health check for node", "key", key, "code", newCode, "message", message, "alive", alive)

				if oldCode != newCode {
					nm.changeFunc(node, nm.location, oldCode, newCode)
					logf.Log.Info("Health status changed for node", "key", key, "oldCode", oldCode, "newCode", newCode)
				}
			case <-ctx.Done():
				logf.Log.Info("Stopping health checks for node", "key", key)
				return
			}
		}
	}()
}

type locationManagerOps struct {
	action   string
	key      string
	hash     string
	location *infrastructurev1alpha1.Location
}

type LocationManager struct {
	nodeManagers   map[string]*NodeManager
	changeFunc     func(node *NodeCheck, location *infrastructurev1alpha1.Location, oldCode, newCode int)
	specChangeFunc func(location *infrastructurev1alpha1.Location)
	mu             sync.Mutex
	opsCh          chan locationManagerOps
	closed         bool
}

func (l *LocationManager) LocationKey(namespace string, name string) string {
	return namespace + "/" + name
}

func (l *LocationManager) AddLocation(location *infrastructurev1alpha1.Location) {

	marshalled, err := json.Marshal(location)
	if err != nil {
		return
	}

	hash := fmt.Sprintf("%x", md5.Sum(marshalled))

	l.opsCh <- locationManagerOps{
		action:   "add",
		key:      l.LocationKey(location.Namespace, location.Name),
		location: location,
		hash:     hash,
	}
}

func (l *LocationManager) RemoveLocation(location types.NamespacedName) {
	l.opsCh <- locationManagerOps{
		action:   "remove",
		key:      l.LocationKey(location.Namespace, location.Name),
		location: nil,
	}
}

func (l *LocationManager) loop() {
	for op := range l.opsCh {
		switch op.action {
		case "add":
			logf.Log.Info("Processing add operation for location", "location", op.key)
			nm, exists := l.nodeManagers[op.key]
			if exists {

				if nm.locationHash == op.hash {
					logf.Log.Info("Location hash unchanged, skipping update", "location", op.key)
					continue
				}

				for k, cancel := range nm.cancelFunc {
					cancel()
					delete(nm.cancelFunc, k)
				}

				for k, _ := range nm.nodes {
					delete(nm.nodes, k)
				}

				logf.Log.Info("Location already exists in manager Spec changed", "location", op.key)
				l.specChangeFunc(op.location)
			} else {
				l.nodeManagers[op.key] = &NodeManager{
					location:     op.location,
					locationHash: op.hash,
					nodes:        make(map[string]*NodeCheck),
					changeFunc:   l.changeFunc,
					cancelFunc:   make(map[string]context.CancelFunc),
				}
			}

			for _, nodeGroup := range op.location.Spec.NodeGroups {
				for _, node := range nodeGroup.Nodes {
					if node.Ipv4 != "" {
						l.nodeManagers[op.key].nodes[l.nodeManagers[op.key].NodeKey(&node, 4)] = &NodeCheck{
							IP:       net.ParseIP(node.Ipv4),
							Port:     80,
							Path:     "/healthz",
							Protocol: "http",
							Interval: 10 * time.Second,
							Timeout:  3 * time.Second,
						}
						l.nodeManagers[op.key].StartHealthChecks(l.nodeManagers[op.key].NodeKey(&node, 4))
					}
					if node.Ipv6 != "" {
						l.nodeManagers[op.key].nodes[l.nodeManagers[op.key].NodeKey(&node, 6)] = &NodeCheck{
							IP:       net.ParseIP(node.Ipv6),
							Port:     80,
							Path:     "/healthz",
							Protocol: "http",
							Interval: 10 * time.Second,
							Timeout:  3 * time.Second,
						}
						l.nodeManagers[op.key].StartHealthChecks(l.nodeManagers[op.key].NodeKey(&node, 6))
					}
				}
			}

		case "remove":
			logf.Log.Info("Processing remove operation for location", "location", op.key)
			nm, exists := l.nodeManagers[op.key]
			if exists {
				for k, cancel := range nm.cancelFunc {
					cancel()
					delete(nm.cancelFunc, k)
				}

				for k, _ := range nm.nodes {
					delete(nm.nodes, k)
				}

				logf.Log.Info("Location already exists in manager", "location", op.key)
				delete(l.nodeManagers, op.key)
			}
		}
	}

	// Clean shutdown
	for _, nm := range l.nodeManagers {
		for _, cancel := range nm.cancelFunc {
			cancel()
		}
	}
}

func NewLocationManager(changeFn func(node *NodeCheck, location *infrastructurev1alpha1.Location, oldCode, newCode int), specChangeFunc func(location *infrastructurev1alpha1.Location)) *LocationManager {
	lm := &LocationManager{
		nodeManagers:   make(map[string]*NodeManager),
		opsCh:          make(chan locationManagerOps, 100),
		closed:         false,
		changeFunc:     changeFn,
		specChangeFunc: specChangeFunc,
	}

	go lm.loop()
	return lm
}
