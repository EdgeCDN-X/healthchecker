package healthchecker

import (
	"fmt"
	"net"
	"net/http"
	"time"

	infrastructurev1alpha1 "github.com/EdgeCDN-X/edgecdnx-controller/api/v1alpha1"
)

type NodeCheck struct {
	Name      string
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
