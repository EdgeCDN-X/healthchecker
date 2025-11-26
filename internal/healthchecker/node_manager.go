package healthchecker

import (
	"context"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"sync"
	"time"

	infrastructurev1alpha1 "github.com/EdgeCDN-X/edgecdnx-controller/api/v1alpha1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

type NodeManager struct {
	location      *infrastructurev1alpha1.Location
	locationHash  string
	nodeCheckList map[string]*NodeCheckList

	// nodes      map[string]*NodeCheck
	// cancelFunc map[string]context.CancelFunc
	changeFunc func(nodeCheckList *NodeCheckList, location *infrastructurev1alpha1.Location, oldCode, newCode int)
}

func (nm *NodeManager) NodeKey(node *infrastructurev1alpha1.NodeSpec) string {
	return node.Name
}

func (nm *NodeManager) StartHealthChecks(nodeKey string, locationName string) {
	nodeCheckList, exists := nm.nodeCheckList[nodeKey]
	if !exists {
		logf.Log.Error(nil, "Node not found for health checks", "key", nodeKey)
		return
	}

	logf.Log.Info("Starting health checks for node", "key", nodeKey, "location", locationName)

	for _, check := range nodeCheckList.Checks {
		ctx, cancel := context.WithCancel(context.Background())
		nodeCheckList.CancelFuncs = append(nodeCheckList.CancelFuncs, cancel)

		go func() {
			// Initial random delay to avoid thundering herd
			sleepDuration := time.Duration(1+rand.Intn(int(check.Interval.Seconds()))) * time.Second
			time.Sleep(sleepDuration)

			ticker := time.NewTicker(check.Interval)
			defer ticker.Stop()

			counter := 0

			for {
				select {
				case <-ticker.C:
					oldCode := check.LastRetCode
					newCode, message, alive := check.HealthCheck()
					check.LastRetCode = newCode
					check.LastRetMessage = message
					check.LastCheckTime = time.Now()
					check.Alive = alive

					if err := ctx.Err(); err != nil {
						logf.Log.Info("Health check context canceled", "key", nodeKey, "location", locationName)
						return
					}

					logf.Log.V(1).Info("Health check for node", "location", locationName, "key", nodeKey, "code", newCode, "oldCode", oldCode, "message", message, "alive", alive, "counter", counter)

					if oldCode != newCode {
						nm.changeFunc(nodeCheckList, nm.location, oldCode, newCode)
						logf.Log.Info("Health status changed for node", "key", nodeKey, "oldCode", oldCode, "newCode", newCode, "location", locationName)
					}
					counter++
				case <-ctx.Done():
					logf.Log.Info("Stopping health checks for node", "key", nodeKey, "location", locationName)
					return
				}
			}
		}()
	}

}

type NodeCheckList struct {
	Name        string
	Checks      []*NodeCheck
	CancelFuncs []context.CancelFunc
	mu          sync.Mutex
}

type NodeCheck struct {
	Condition infrastructurev1alpha1.NodeConditionType
	IP        net.IP
	Port      uint16
	Path      string
	Protocol  string
	Timeout   time.Duration
	Interval  time.Duration

	Alive          bool
	LastRetCode    int
	LastCheckTime  time.Time
	LastRetMessage string
}

func (node *NodeCheck) HealthCheck() (int, string, bool) {
	address := fmt.Sprintf("%s://%s:%d%s", node.Protocol, node.IP.String(), node.Port, node.Path)

	client := http.Client{
		Timeout: node.Timeout,
	}

	resp, err := client.Get(address)
	if err != nil {
		return -1, err.Error(), false
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp.StatusCode, "OK", true
	} else {
		return resp.StatusCode, "Non-2xx response", false
	}
}
