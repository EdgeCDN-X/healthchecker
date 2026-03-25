package healthchecker

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"sync"
	"time"

	infrastructurev1alpha1 "github.com/EdgeCDN-X/edgecdnx-controller/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

const defaultPrometheusScrapeInterval = 30 * time.Second

type PrometheusConfig struct {
	Endpoint       string
	ClientCertFile string
	ClientKeyFile  string
	CAFile         string
	Interval       time.Duration
}

type AlertChangeFunc func(location *infrastructurev1alpha1.Location, locationAlerts []infrastructurev1alpha1.PrometheusAlertStatus, nodeAlerts map[string][]infrastructurev1alpha1.PrometheusAlertStatus)

type PrometheusManager struct {
	client          *http.Client
	config          PrometheusConfig
	alertChangeFunc AlertChangeFunc

	mu         sync.RWMutex
	locations  map[string]*infrastructurev1alpha1.Location
	lastHashes map[string]string
	stopCh     chan struct{}
}

type prometheusQueryResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Value  []interface{}     `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

type firedAlert struct {
	AlertName string
	State     string
	Labels    map[string]string
}

func NewPrometheusManager(config PrometheusConfig, alertChangeFunc AlertChangeFunc) (*PrometheusManager, error) {
	if config.Endpoint == "" {
		return nil, nil
	}

	httpClient, err := newPrometheusHTTPClient(config)
	if err != nil {
		return nil, err
	}

	interval := config.Interval
	if interval <= 0 {
		interval = defaultPrometheusScrapeInterval
	}

	pm := &PrometheusManager{
		client:          httpClient,
		alertChangeFunc: alertChangeFunc,
		locations:       make(map[string]*infrastructurev1alpha1.Location),
		lastHashes:      make(map[string]string),
		stopCh:          make(chan struct{}),
		config: PrometheusConfig{
			Endpoint:       config.Endpoint,
			ClientCertFile: config.ClientCertFile,
			ClientKeyFile:  config.ClientKeyFile,
			CAFile:         config.CAFile,
			Interval:       interval,
		},
	}

	go pm.loop()
	return pm, nil
}

func (p *PrometheusManager) AddLocation(location *infrastructurev1alpha1.Location) {
	if p == nil {
		return
	}

	key := location.Namespace + "/" + location.Name
	p.mu.Lock()
	p.locations[key] = location.DeepCopy()
	p.mu.Unlock()
}

func (p *PrometheusManager) RemoveLocation(location types.NamespacedName) {
	if p == nil {
		return
	}

	key := location.Namespace + "/" + location.Name
	p.mu.Lock()
	delete(p.locations, key)
	delete(p.lastHashes, key)
	p.mu.Unlock()
}

func (p *PrometheusManager) loop() {
	ticker := time.NewTicker(p.config.Interval)
	defer ticker.Stop()

	for {
		if err := p.syncOnce(); err != nil {
			logf.Log.Error(err, "failed to sync Prometheus alerts")
		}

		select {
		case <-ticker.C:
		case <-p.stopCh:
			return
		}
	}
}

func (p *PrometheusManager) syncOnce() error {
	alerts, err := p.scrapeFiringAlerts()
	if err != nil {
		return err
	}

	p.mu.RLock()
	locations := make(map[string]*infrastructurev1alpha1.Location, len(p.locations))
	for key, loc := range p.locations {
		locations[key] = loc.DeepCopy()
	}
	p.mu.RUnlock()

	for key, location := range locations {
		locationAlerts, nodeAlerts := evaluateLocationAlerts(location, alerts)
		hashInput := map[string]interface{}{
			"location": locationAlerts,
			"nodes":    nodeAlerts,
		}
		encoded, err := json.Marshal(hashInput)
		if err != nil {
			return err
		}
		hash := string(encoded)

		p.mu.Lock()
		oldHash := p.lastHashes[key]
		if oldHash == hash {
			p.mu.Unlock()
			continue
		}
		p.lastHashes[key] = hash
		p.mu.Unlock()

		p.alertChangeFunc(location, locationAlerts, nodeAlerts)
	}

	return nil
}

func (p *PrometheusManager) scrapeFiringAlerts() ([]firedAlert, error) {
	queryURL, err := url.Parse(p.config.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("invalid prometheus endpoint: %w", err)
	}

	queryURL.Path = "/api/v1/query"
	params := queryURL.Query()
	params.Set("query", "ALERTS{alertstate=\"firing\"}")
	queryURL.RawQuery = params.Encode()

	resp, err := p.client.Get(queryURL.String())
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("prometheus query failed with status %d", resp.StatusCode)
	}

	decoded := &prometheusQueryResponse{}
	if err := json.NewDecoder(resp.Body).Decode(decoded); err != nil {
		return nil, err
	}

	firing := make([]firedAlert, 0, len(decoded.Data.Result))
	for _, res := range decoded.Data.Result {
		if res.Metric["alertstate"] != "firing" {
			continue
		}
		alertName := res.Metric["alertname"]
		if alertName == "" {
			continue
		}

		labels := make(map[string]string, len(res.Metric))
		for k, v := range res.Metric {
			labels[k] = v
		}

		firing = append(firing, firedAlert{
			AlertName: alertName,
			State:     "firing",
			Labels:    labels,
		})
	}

	return firing, nil
}

func evaluateLocationAlerts(location *infrastructurev1alpha1.Location, alerts []firedAlert) ([]infrastructurev1alpha1.PrometheusAlertStatus, map[string][]infrastructurev1alpha1.PrometheusAlertStatus) {
	locationStatuses := matchStatuses(location.Spec.Alerts, alerts)

	nodeStatuses := make(map[string][]infrastructurev1alpha1.PrometheusAlertStatus)
	for _, node := range location.Spec.Nodes {
		if len(node.Alerts) > 0 {
			nodeStatuses[node.Name] = matchStatuses(node.Alerts, alerts)
		}
	}

	for _, nodeGroup := range location.Spec.NodeGroups {
		for _, node := range nodeGroup.Nodes {
			if len(node.Alerts) > 0 {
				nodeStatuses[node.Name] = matchStatuses(node.Alerts, alerts)
			}
		}
	}

	return locationStatuses, nodeStatuses
}

func matchStatuses(matchers []infrastructurev1alpha1.PrometheusAlertMatcherSpec, alerts []firedAlert) []infrastructurev1alpha1.PrometheusAlertStatus {
	results := make([]infrastructurev1alpha1.PrometheusAlertStatus, 0)

	for _, matcher := range matchers {
		for _, alert := range alerts {
			if alert.AlertName != matcher.AlertName {
				continue
			}
			if !labelsMatch(matcher.Labels, alert.Labels) {
				continue
			}

			status := infrastructurev1alpha1.PrometheusAlertStatus{
				AlertName:          alert.AlertName,
				State:              alert.State,
				Labels:             alert.Labels,
				LastTransitionTime: metav1.Time{},
			}
			results = append(results, status)
		}
	}

	sort.Slice(results, func(i, j int) bool {
		if results[i].AlertName != results[j].AlertName {
			return results[i].AlertName < results[j].AlertName
		}
		return formatLabels(results[i].Labels) < formatLabels(results[j].Labels)
	})

	return results
}

func labelsMatch(selector map[string]string, labels map[string]string) bool {
	for key, expected := range selector {
		if labels[key] != expected {
			return false
		}
	}
	return true
}

func formatLabels(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	result := ""
	for _, key := range keys {
		result += key + "=" + labels[key] + ";"
	}
	return result
}

func newPrometheusHTTPClient(config PrometheusConfig) (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()

	if config.CAFile == "" && config.ClientCertFile == "" && config.ClientKeyFile == "" {
		return &http.Client{Timeout: 10 * time.Second, Transport: transport}, nil
	}

	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}

	if config.CAFile != "" {
		caData, err := os.ReadFile(config.CAFile)
		if err != nil {
			return nil, fmt.Errorf("unable to read prometheus ca file: %w", err)
		}

		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caData) {
			return nil, fmt.Errorf("unable to parse prometheus ca file")
		}
		tlsConfig.RootCAs = pool
	}

	if config.ClientCertFile != "" || config.ClientKeyFile != "" {
		if config.ClientCertFile == "" || config.ClientKeyFile == "" {
			return nil, fmt.Errorf("both prometheus client cert and key must be provided")
		}
		cert, err := tls.LoadX509KeyPair(config.ClientCertFile, config.ClientKeyFile)
		if err != nil {
			return nil, fmt.Errorf("unable to load prometheus client certificate: %w", err)
		}
		tlsConfig.Certificates = []tls.Certificate{cert}
	}

	transport.TLSClientConfig = tlsConfig
	return &http.Client{Timeout: 10 * time.Second, Transport: transport}, nil
}
