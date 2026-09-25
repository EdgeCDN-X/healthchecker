package healthchecker

import (
	"context"
	"crypto/tls"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	infrastructurev1alpha1 "github.com/EdgeCDN-X/edgecdnx-controller/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

	logf.Log.Info("Number of checks for node", "key", nodeKey, "location", locationName, "count", len(nodeCheckList.Checks))

	for _, check := range nodeCheckList.Checks {
		ctx, cancel := context.WithCancel(context.Background())
		nodeCheckList.CancelFuncs = append(nodeCheckList.CancelFuncs, cancel)

		go func() {
			// Initial random delay to avoid thundering herd

			logf.Log.Info("Initial random delay before starting health checks", "key", nodeKey, "location", locationName)

			sleepDuration := time.Duration(1+rand.Intn(int(check.Interval.Duration.Seconds()))) * time.Second
			time.Sleep(sleepDuration)

			ticker := time.NewTicker(check.Interval.Duration)
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
	Name  string
	Type  infrastructurev1alpha1.HealthCheckProbeType
	Stack infrastructurev1alpha1.StackType

	// HTTP(s) Specific Fields
	Protocol string
	Host     string
	Path     string

	// HTTP(s)/TCP Specific Fields
	Target string
	Port   int32

	Timeout  metav1.Duration
	Interval metav1.Duration

	Alive          bool
	LastRetCode    int
	LastCheckTime  time.Time
	LastRetMessage string
}

func (check *NodeCheck) healthCheckHTTP() (int, string, bool) {
	protocol := strings.ToLower(check.Protocol)

	if protocol != "http" && protocol != "https" {
		return -1, fmt.Sprintf("Unsupported protocol: %s", protocol), false
	}

	target := net.JoinHostPort(check.Target, fmt.Sprintf("%d", func() int32 {
		if check.Port == 0 && protocol == "http" {
			return 80
		}
		if check.Port == 0 && protocol == "https" {
			return 443
		}
		return check.Port
	}()))
	transport := http.DefaultTransport.(*http.Transport).Clone()

	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialNetwork := "tcp"

		if check.Stack == infrastructurev1alpha1.StackTypeIPv4 {
			dialNetwork = "tcp4"
		}
		if check.Stack == infrastructurev1alpha1.StackTypeIPv6 {
			dialNetwork = "tcp6"
		}

		d := net.Dialer{}
		return d.DialContext(ctx, dialNetwork, target)
	}

	if protocol == "https" && check.Host != "" {
		transport.TLSClientConfig = &tls.Config{
			ServerName: check.Host,
		}
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   check.Timeout.Duration,
	}

	url := protocol + "://"
	if check.Host != "" {
		url += check.Host
	} else {
		url += target
	}

	if check.Path != "" {
		url += check.Path
	}

	fmt.Printf("Health check URL: %s\n", url)

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return -1, err.Error(), false
	}
	resp, err := client.Do(req)
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

func (check *NodeCheck) healthCheckTCP() (int, string, bool) {
	dialNetwork := "tcp"
	if check.Stack == infrastructurev1alpha1.StackTypeIPv4 {
		dialNetwork = "tcp4"
	}
	if check.Stack == infrastructurev1alpha1.StackTypeIPv6 {
		dialNetwork = "tcp6"
	}

	target := net.JoinHostPort(check.Target, fmt.Sprintf("%d", check.Port))

	conn, err := (&net.Dialer{Timeout: check.Timeout.Duration}).Dial(dialNetwork, target)
	if err != nil {
		return -1, err.Error(), false
	}
	defer conn.Close()

	return 200, "Healthy", true
}

func (check *NodeCheck) HealthCheck() (int, string, bool) {

	logf.Log.Info("Starting health check", "check", check)

	switch check.Type {
	case infrastructurev1alpha1.HealthCheckProbeTypeASSUME:
		return 200, "Healthy", true
	case infrastructurev1alpha1.HealthCheckProbeTypeHTTP:
		return check.healthCheckHTTP()
	case infrastructurev1alpha1.HealthCheckProbeTypeTCP:
		return check.healthCheckTCP()
	}

	return -1, "Unknown Check Type", false
}
