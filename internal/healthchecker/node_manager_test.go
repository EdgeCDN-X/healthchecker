package healthchecker

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	infrastructurev1alpha1 "github.com/EdgeCDN-X/edgecdnx-controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestLocationProfileThresholdsGateCallbacks(t *testing.T) {
	var probes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if probes.Add(1) <= 2 {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer server.Close()
	host, portString, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portString)
	if err != nil {
		t.Fatal(err)
	}
	type transition struct {
		probes int32
		alive  bool
		code   int
	}
	changes := make(chan transition, 10)
	manager := NewLocationManager(func(checks *NodeCheckList, _ *infrastructurev1alpha1.Location, _, code int) {
		changes <- transition{probes.Load(), checks.Checks[0].Alive, code}
	}, func(*infrastructurev1alpha1.Location) {}, nil)
	defer close(manager.opsCh)
	manager.AddLocation(&infrastructurev1alpha1.Location{
		ObjectMeta: metav1.ObjectMeta{Name: "location", Namespace: "tenant"},
		Spec: infrastructurev1alpha1.LocationSpec{NodeGroups: []infrastructurev1alpha1.NodeGroupSpec{{
			Name: "group", HealthCheck: &corev1.LocalObjectReference{Name: "profile"},
			Nodes: []infrastructurev1alpha1.NodeSpec{{Name: "node", Ipv4: host}},
		}}},
	}, &[]infrastructurev1alpha1.HealthCheckProfile{{
		ObjectMeta: metav1.ObjectMeta{Name: "profile"},
		Spec: infrastructurev1alpha1.HealthCheckProfileSpec{
			SuccessThreshold: 2, FailureThreshold: 3,
			Probes: []infrastructurev1alpha1.HealthCheckProbeSpec{{
				Name: "http", Type: infrastructurev1alpha1.HealthCheckProbeTypeHTTP,
				Interval: metav1.Duration{Duration: 5 * time.Millisecond},
				Timeout:  metav1.Duration{Duration: time.Second},
				HTTP: &infrastructurev1alpha1.HTTPHealthCheckProbeSpec{
					Protocol: "http", Port: int32(port),
				},
			}},
		},
	}})
	for _, want := range []transition{{2, true, 200}, {5, false, 503}} {
		select {
		case got := <-changes:
			if got != want {
				t.Fatalf("transition=%+v, want %+v", got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for threshold transition")
		}
	}
}

func TestThresholdsRequireConsecutiveResults(t *testing.T) {
	check := &NodeCheck{SuccessThreshold: 2, FailureThreshold: 3}
	steps := []struct {
		alive, effective, changed bool
		successes, failures       int32
	}{
		{true, false, false, 1, 0},
		{false, false, false, 0, 1},
		{true, false, false, 1, 0},
		{true, true, true, 2, 0},
		{true, true, false, 2, 0},
		{false, true, false, 0, 1},
		{false, true, false, 0, 2},
		{true, true, false, 1, 0},
		{false, true, false, 0, 1},
		{false, true, false, 0, 2},
		{false, false, true, 0, 3},
		{false, false, false, 0, 3},
		{true, false, false, 1, 0},
		{true, true, true, 2, 0},
	}
	for i, step := range steps {
		code, message := 200, "OK"
		if !step.alive {
			code, message = -1, "failed"
		}
		changed := check.applyResult(code, message, step.alive)
		if changed != step.changed || check.Alive != step.effective ||
			check.consecutiveSuccesses != step.successes || check.consecutiveFailures != step.failures {
			t.Fatalf("step %d: changed=%t check=%+v", i, changed, check)
		}
		if check.LastCheckTime.IsZero() {
			t.Fatalf("step %d: missing check time", i)
		}
		if check.Alive && (check.LastRetCode != 200 || check.LastRetMessage != "OK") {
			t.Fatalf("step %d: pending failure overwrote accepted status: %+v", i, check)
		}
	}
}

func TestThresholdDefaultsAndCodeChanges(t *testing.T) {
	for _, threshold := range []int32{0, 1} {
		check := &NodeCheck{SuccessThreshold: threshold, FailureThreshold: threshold}
		for i, result := range []struct {
			code    int
			alive   bool
			changed bool
		}{
			{-1, false, true}, {-1, false, false}, {200, true, true},
			{204, true, true}, {503, false, true}, {502, false, true},
		} {
			if changed := check.applyResult(result.code, "result", result.alive); changed != result.changed || check.Alive != result.alive {
				t.Fatalf("threshold %d step %d: changed=%t check=%+v", threshold, i, changed, check)
			}
		}
	}
}

func TestThresholdCountersSaturateAndAreIndependent(t *testing.T) {
	ipv4 := &NodeCheck{SuccessThreshold: math.MaxInt32, consecutiveSuccesses: math.MaxInt32 - 1}
	ipv6 := &NodeCheck{SuccessThreshold: 2}
	ipv4.applyResult(200, "OK", true)
	ipv4.applyResult(200, "OK", true)
	ipv6.applyResult(200, "OK", true)
	if ipv4.consecutiveSuccesses != math.MaxInt32 || !ipv4.Alive || ipv6.Alive || ipv6.consecutiveSuccesses != 1 {
		t.Fatalf("counters must saturate and stay independent: ipv4=%+v ipv6=%+v", ipv4, ipv6)
	}
	ipv4.FailureThreshold = math.MaxInt32
	ipv4.consecutiveFailures = math.MaxInt32 - 1
	ipv4.applyResult(-1, "failed", false)
	ipv4.applyResult(-1, "failed", false)
	if ipv4.consecutiveFailures != math.MaxInt32 || ipv4.Alive {
		t.Fatalf("failure counter overflow: %+v", ipv4)
	}
}

func TestLogResultV2(t *testing.T) {
	check := &NodeCheck{Name: "http", SuccessThreshold: 2, FailureThreshold: 3}
	check.applyResult(200, "OK", true)
	var buffer bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buffer, nil))
	check.logResult(context.Background(), logger, time.Now(), time.Millisecond, "node", "tenant/location", 0, 200, "OK", true, nil)
	var record map[string]interface{}
	if err := json.Unmarshal(buffer.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]interface{}{
		"v": "2", "alive": true, "effectiveAlive": false, "thresholdPending": true,
		"successThreshold": float64(2), "failureThreshold": float64(3),
		"consecutiveSuccesses": float64(1), "consecutiveFailures": float64(0),
		"node": "node", "location": "tenant/location",
	} {
		if record[key] != want {
			t.Errorf("%s = %v, want %v", key, record[key], want)
		}
	}
}
