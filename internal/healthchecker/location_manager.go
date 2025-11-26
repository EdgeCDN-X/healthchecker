package healthchecker

import (
	"context"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"

	infrastructurev1alpha1 "github.com/EdgeCDN-X/edgecdnx-controller/api/v1alpha1"
	"k8s.io/apimachinery/pkg/types"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

type locationManagerOps struct {
	action   string
	key      string
	hash     string
	location *infrastructurev1alpha1.Location
}

type LocationManager struct {
	nodeManagers   map[string]*NodeManager
	changeFunc     func(nodeCheckList *NodeCheckList, location *infrastructurev1alpha1.Location, oldCode, newCode int)
	specChangeFunc func(location *infrastructurev1alpha1.Location)
	mu             sync.Mutex
	opsCh          chan locationManagerOps
	closed         bool
}

func (l *LocationManager) LocationKey(namespace string, name string) string {
	return namespace + "/" + name
}

func (l *LocationManager) AddLocation(location *infrastructurev1alpha1.Location) {
	marshalled, err := json.Marshal(location.Spec)
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
			l.mu.Lock()
			logf.Log.Info("Processing add operation for location", "location", op.key)
			nm, exists := l.nodeManagers[op.key]
			if exists {
				if nm.locationHash == op.hash {
					logf.Log.Info("Location hash unchanged, skipping update", "location", op.key)
					l.mu.Unlock()
					continue
				}

				for k, nl := range nm.nodeCheckList {
					for k2, cancel := range nl.CancelFuncs {
						logf.Log.Info("Calling Cancel", "key", k, "location", op.key)
						cancel()
						nl.CancelFuncs[k2] = nil
					}

					delete(nm.nodeCheckList, k)
				}

				logf.Log.Info("Location already exists in manager Spec changed", "location", op.key)
				l.specChangeFunc(op.location)
				delete(l.nodeManagers, op.key)
			}

			l.nodeManagers[op.key] = &NodeManager{
				location:      op.location,
				locationHash:  op.hash,
				nodeCheckList: make(map[string]*NodeCheckList),
				changeFunc:    l.changeFunc,
			}

			for _, nodeGroup := range op.location.Spec.NodeGroups {
				for _, node := range nodeGroup.Nodes {
					nodeCheckList := &NodeCheckList{
						Name:        node.Name,
						Checks:      make([]*NodeCheck, 0),
						CancelFuncs: make([]context.CancelFunc, 0),
					}

					if node.Ipv4 != "" {
						nodeCheckList.Checks = append(nodeCheckList.Checks, &NodeCheck{
							Condition: infrastructurev1alpha1.IPV4HealthCheckSuccessful,
							IP:        net.ParseIP(node.Ipv4),
							Port:      80,
							Path:      "/healthz",
							Protocol:  "http",
							Interval:  10 * time.Second,
							Timeout:   3 * time.Second,
						})
					}

					if node.Ipv6 != "" {
						nodeCheckList.Checks = append(nodeCheckList.Checks, &NodeCheck{
							Condition: infrastructurev1alpha1.IPV6HealthCheckSuccessful,
							IP:        net.ParseIP(node.Ipv6),
							Port:      80,
							Path:      "/healthz",
							Protocol:  "http",
							Interval:  10 * time.Second,
							Timeout:   3 * time.Second,
						})
					}

					l.nodeManagers[op.key].nodeCheckList[nm.NodeKey(&node)] = nodeCheckList
					l.nodeManagers[op.key].StartHealthChecks(nm.NodeKey(&node), op.key)
				}
			}
			l.mu.Unlock()

		case "remove":
			l.mu.Lock()
			logf.Log.Info("Processing remove operation for location", "location", op.key)

			for key, nm := range l.nodeManagers {
				logf.Log.Info("Current managed location", "location", key, "nodes", len(nm.nodeCheckList))
			}

			nm, exists := l.nodeManagers[op.key]
			if exists {
				logf.Log.V(1).Info("Found location in manager during remove operation", "location", op.key)
				for nodeKey, nodeCheckList := range nm.nodeCheckList {

					logf.Log.V(1).Info("Removing health checks for node", "key", nodeKey, "location", op.key, "cancelFuncs", len(nodeCheckList.CancelFuncs))

					for ckey, cancel := range nodeCheckList.CancelFuncs {
						logf.Log.Info("Calling Cancel", "key", ckey, "location", op.key)
						cancel()
						nodeCheckList.CancelFuncs[ckey] = nil
					}
					nodeCheckList.CancelFuncs = nil
					delete(nm.nodeCheckList, nodeKey)
				}
				delete(l.nodeManagers, op.key)
			} else {
				logf.Log.Info("Location not found in manager during remove operation", "location", op.key)
			}
			l.mu.Unlock()
		}
	}

	// Shutdown hook
	for _, nm := range l.nodeManagers {
		for _, ncklist := range nm.nodeCheckList {
			for _, cancel := range ncklist.CancelFuncs {
				cancel()
			}
		}
	}
}

func NewLocationManager(changeFn func(nodeCheckList *NodeCheckList, location *infrastructurev1alpha1.Location, oldCode, newCode int), specChangeFunc func(location *infrastructurev1alpha1.Location)) *LocationManager {
	lm := &LocationManager{
		nodeManagers:   make(map[string]*NodeManager),
		opsCh:          make(chan locationManagerOps, 100),
		closed:         false,
		changeFunc:     changeFn,
		specChangeFunc: specChangeFunc,
		mu:             sync.Mutex{},
	}

	go lm.loop()
	return lm
}
