package healthchecker

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"math/rand"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	infrastructurev1alpha1 "github.com/EdgeCDN-X/edgecdnx-controller/api/v1alpha1"
	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

const otelName = "healthchecker"

type NodeManager struct {
	location      *infrastructurev1alpha1.Location
	locationHash  string
	nodeCheckList map[string]*NodeCheckList
	changeFunc    func(nodeCheckList *NodeCheckList, location *infrastructurev1alpha1.Location, oldCode, newCode int)
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

	var ologger *slog.Logger
	if os.Getenv("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT") != "" {
		ologger = otelslog.NewLogger("healthchecker")
	}

	logf.Log.Info("Starting health checks for node", "key", nodeKey, "location", locationName)

	for _, check := range nodeCheckList.Checks {
		ctx, cancel := context.WithCancel(context.Background())
		nodeCheckList.CancelFuncs = append(nodeCheckList.CancelFuncs, cancel)

		go func() {
			// Initial random delay to avoid thundering herd
			logf.Log.Info("Initial random delay before starting health checks", "key", nodeKey, "location", locationName)

			if check.Interval.Duration <= 0 {
				logf.Log.Error(nil, "Invalid health check interval", "key", nodeKey, "location", locationName)
				return
			}

			sleepDuration := time.Duration(rand.Int63n(int64(check.Interval.Duration)))
			sleeptimer := time.NewTimer(sleepDuration)
			select {
			case <-sleeptimer.C:
			case <-ctx.Done():
				logf.Log.Info("Health check initial sleep canceled", "key", nodeKey, "location", locationName)
				sleeptimer.Stop()
				return
			}

			ticker := time.NewTicker(check.Interval.Duration)
			defer ticker.Stop()

			counter := 0

			for {
				select {
				case <-ticker.C:
					ctx, span := otel.Tracer(otelName).Start(ctx, "healthcheck.probe", trace.WithSpanKind(trace.SpanKindClient))
					span.SetAttributes(
						// Set Attribute version, v1
						attribute.String("healthcheck.name", check.Name),
						attribute.String("healthcheck.type", string(check.Type)),
						attribute.String("healthcheck.protocol", check.Protocol),
						attribute.String("healthcheck.target", check.Target),
						attribute.Int("healthcheck.port", int(check.Port)),
						attribute.String("healthcheck.stack", string(check.Stack)),
						attribute.String("node.name", nodeKey),
						attribute.String("location.name", locationName),
					)

					oldCode := check.LastRetCode
					start := time.Now()
					newCode, message, alive, hcerr := check.HealthCheck(ctx)
					duration := time.Since(start)
					span.SetAttributes(
						attribute.Int("healthcheck.previous_code", oldCode),
						attribute.Int("healthcheck.code", newCode),
						attribute.Bool("healthcheck.alive", alive),
						attribute.String("healthcheck.message", message),
						attribute.Float64("healthcheck.duration_ms", float64(duration)/float64(time.Millisecond)),
					)
					if hcerr != nil {
						span.RecordError(hcerr)
						span.SetStatus(codes.Error, message)
					}

					if ologger != nil {
						ologger.Info(fmt.Sprintf("healthcheck alive: %s", strconv.FormatBool(alive)), "v", "1", "start", start, "node", nodeKey, "location", locationName, "code", newCode, "oldCode", oldCode, "message", message, "alive", alive, "duration", duration, "type", string(check.Type), "name", check.Name, "target", check.Target, "error", hcerr)
					}

					check.LastRetCode = newCode
					check.LastRetMessage = message
					check.LastCheckTime = time.Now()
					check.Alive = alive

					if err := ctx.Err(); err != nil {
						span.End()
						logf.Log.Info("Health check context canceled", "key", nodeKey, "location", locationName)
						return
					}

					logf.Log.V(1).Info("Health check for node", "location", locationName, "key", nodeKey, "code", newCode, "oldCode", oldCode, "message", message, "alive", alive, "counter", counter)

					if oldCode != newCode {
						span.AddEvent("healthcheck.status_changed", trace.WithAttributes(
							attribute.Int("healthcheck.previous_code", oldCode),
							attribute.Int("healthcheck.code", newCode),
							attribute.Bool("healthcheck.alive", alive),
						))
						nm.changeFunc(nodeCheckList, nm.location, oldCode, newCode)
						logf.Log.Info("Health status changed for node", "key", nodeKey, "oldCode", oldCode, "newCode", newCode, "location", locationName)
					}
					span.End()
					if counter == int(^uint(0)>>1) {
						counter = 0
					} else {
						counter++
					}
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

func (check *NodeCheck) healthCheckHTTP(ctx context.Context) (int, string, bool, error) {
	protocol := strings.ToLower(check.Protocol)

	if protocol != "http" && protocol != "https" {
		return -1, fmt.Sprintf("Unsupported protocol: %s", protocol), false, fmt.Errorf("unsupported protocol: %s", protocol)
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

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return -1, err.Error(), false, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return -1, err.Error(), false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp.StatusCode, "OK", true, nil
	} else {
		return resp.StatusCode, "Non-2xx response", false, fmt.Errorf("non-2xx response: %d", resp.StatusCode)
	}
}

func (check *NodeCheck) healthCheckTCP(ctx context.Context) (int, string, bool, error) {
	dialNetwork := "tcp"
	if check.Stack == infrastructurev1alpha1.StackTypeIPv4 {
		dialNetwork = "tcp4"
	}
	if check.Stack == infrastructurev1alpha1.StackTypeIPv6 {
		dialNetwork = "tcp6"
	}

	target := net.JoinHostPort(check.Target, fmt.Sprintf("%d", check.Port))

	conn, err := (&net.Dialer{Timeout: check.Timeout.Duration}).DialContext(ctx, dialNetwork, target)
	if err != nil {
		return -1, err.Error(), false, err
	}
	defer conn.Close()

	return 200, "Healthy", true, nil
}

func (check *NodeCheck) HealthCheck(ctx context.Context) (int, string, bool, error) {
	logf.Log.Info("Starting health check", "check", check)

	switch check.Type {
	case infrastructurev1alpha1.HealthCheckProbeTypeASSUME:
		if check.Target == "Unhealthy" {
			return -1, "Unhealthy", false, fmt.Errorf("assume check marked as unhealthy")
		}
		return 200, "Healthy", true, nil
	case infrastructurev1alpha1.HealthCheckProbeTypeHTTP:
		return check.healthCheckHTTP(ctx)
	case infrastructurev1alpha1.HealthCheckProbeTypeTCP:
		return check.healthCheckTCP(ctx)
	}

	return -1, "Unknown Check Type", false, fmt.Errorf("unknown check type: %v", check.Type)
}
