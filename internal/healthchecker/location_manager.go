package healthchecker

import (
	"context"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	infrastructurev1alpha1 "github.com/EdgeCDN-X/edgecdnx-controller/api/v1alpha1"
	"k8s.io/apimachinery/pkg/types"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

type locationManagerOps struct {
	action              string
	key                 string
	hash                string
	location            *infrastructurev1alpha1.Location
	healthCheckProfiles *[]infrastructurev1alpha1.HealthCheckProfile
}

type LocationManager struct {
	nodeManagers   map[string]*NodeManager
	promManager    *PrometheusManager
	changeFunc     func(nodeCheckList *NodeCheckList, location *infrastructurev1alpha1.Location, oldCode, newCode int)
	specChangeFunc func(location *infrastructurev1alpha1.Location)
	mu             sync.Mutex
	opsCh          chan locationManagerOps
	closed         bool
}

func (l *LocationManager) LocationKey(namespace string, name string) string {
	return namespace + "/" + name
}

func (l *LocationManager) AddLocation(location *infrastructurev1alpha1.Location, healthCheckProfiles *[]infrastructurev1alpha1.HealthCheckProfile) {

	logf.Log.Info("Adding location", "location", l.LocationKey(location.Namespace, location.Name), "healthCheckProfiles", healthCheckProfiles)

	marshallable := struct {
		LocationSpec        infrastructurev1alpha1.LocationSpec
		HealthCheckProfiles []infrastructurev1alpha1.HealthCheckProfileSpec
	}{
		LocationSpec:        location.Spec,
		HealthCheckProfiles: make([]infrastructurev1alpha1.HealthCheckProfileSpec, len(*healthCheckProfiles)),
	}

	for i, hcp := range *healthCheckProfiles {
		marshallable.HealthCheckProfiles[i] = hcp.Spec
	}

	marshalled, err := json.Marshal(marshallable)
	if err != nil {
		return
	}

	hash := fmt.Sprintf("%x", md5.Sum(marshalled))

	l.opsCh <- locationManagerOps{
		action:              "add",
		key:                 l.LocationKey(location.Namespace, location.Name),
		location:            location,
		healthCheckProfiles: healthCheckProfiles,
		hash:                hash,
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

		logf.Log.Info("Received operation for location manager", "action", op.action, "key", op.key, "location", op.location, "healthcheckProfiles", op.healthCheckProfiles)

		switch op.action {
		case "add":
			l.mu.Lock()
			logf.Log.Info("Processing add operation for location", "location", op.key)
			if l.promManager != nil {
				l.promManager.AddLocation(op.location)
			}
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

					for _, healthCheckProfile := range *op.healthCheckProfiles {
						if nodeGroup.HealthCheck.Name == healthCheckProfile.Name {
							for _, probe := range healthCheckProfile.Spec.Probes {
								switch probe.Type {
								case infrastructurev1alpha1.HealthCheckProbeTypeHTTP:
									if probe.HTTP == nil {
										break
									}

									if probe.HTTP.Stack == infrastructurev1alpha1.StackTypeDual {
										for _, stack := range []infrastructurev1alpha1.StackType{infrastructurev1alpha1.StackTypeIPv4, infrastructurev1alpha1.StackTypeIPv6} {
											if stack == infrastructurev1alpha1.StackTypeIPv4 && node.Ipv4 == "" || stack == infrastructurev1alpha1.StackTypeIPv6 && node.Ipv6 == "" {
												logf.Log.Info("Skipping stack due to missing IP", "stack", stack, "node", node.Name)
												continue
											}

											target := ""

											if stack == infrastructurev1alpha1.StackTypeIPv4 {
												target = node.Ipv4
											}

											if stack == infrastructurev1alpha1.StackTypeIPv6 {
												target = node.Ipv6
											}

											if probe.HTTP.Target != "" {
												target = probe.HTTP.Target
											}

											nodeCheck := &NodeCheck{
												Name:  probe.Name,
												Type:  probe.Type,
												Stack: stack,
												// HTTP(s) Specific Fields
												Protocol: probe.HTTP.Protocol,
												Host:     probe.HTTP.Host,
												Target:   target,
												Port:     probe.HTTP.Port,
												Path:     probe.HTTP.Path,
												// Generic Fields
												Interval: probe.Interval,
												Timeout:  probe.Timeout,
											}
											nodeCheckList.Checks = append(nodeCheckList.Checks, nodeCheck)
										}
									} else {
										target := ""

										if probe.HTTP.Stack == infrastructurev1alpha1.StackTypeIPv4 {
											target = node.Ipv4
										}

										if probe.HTTP.Stack == infrastructurev1alpha1.StackTypeIPv6 {
											target = node.Ipv6
										}

										if probe.HTTP.Target != "" {
											target = probe.HTTP.Target
										}

										nodeCheck := &NodeCheck{
											Name:  probe.Name,
											Type:  probe.Type,
											Stack: probe.HTTP.Stack,
											// HTTP(s) Specific Fields
											Protocol: probe.HTTP.Protocol,
											Host:     probe.HTTP.Host,
											Target:   target,
											Port:     probe.HTTP.Port,
											Path:     probe.HTTP.Path,
											// Generic Fields
											Interval: probe.Interval,
											Timeout:  probe.Timeout,
										}
										nodeCheckList.Checks = append(nodeCheckList.Checks, nodeCheck)
									}

									break
								case infrastructurev1alpha1.HealthCheckProbeTypeTCP:

									if probe.TCP == nil {
										break
									}

									if probe.TCP.Stack == infrastructurev1alpha1.StackTypeDual {
										for _, stack := range []infrastructurev1alpha1.StackType{infrastructurev1alpha1.StackTypeIPv4, infrastructurev1alpha1.StackTypeIPv6} {
											if stack == infrastructurev1alpha1.StackTypeIPv4 && node.Ipv4 == "" || stack == infrastructurev1alpha1.StackTypeIPv6 && node.Ipv6 == "" {
												logf.Log.Info("Skipping stack due to missing IP", "stack", stack, "node", node.Name)
												continue
											}

											target := ""
											if stack == infrastructurev1alpha1.StackTypeIPv4 {
												target = node.Ipv4
											}
											if stack == infrastructurev1alpha1.StackTypeIPv6 {
												target = node.Ipv6
											}
											if probe.TCP.Target != "" {
												target = probe.TCP.Target
											}

											nodeCheck := &NodeCheck{
												Name:     probe.Name,
												Type:     probe.Type,
												Stack:    stack,
												Target:   target,
												Port:     probe.TCP.Port,
												Interval: probe.Interval,
												Timeout:  probe.Timeout,
											}
											nodeCheckList.Checks = append(nodeCheckList.Checks, nodeCheck)
										}
									} else {
										target := ""

										if probe.TCP.Stack == infrastructurev1alpha1.StackTypeIPv4 {
											target = node.Ipv4
										}
										if probe.TCP.Stack == infrastructurev1alpha1.StackTypeIPv6 {
											target = node.Ipv6
										}
										if probe.TCP.Target != "" {
											target = probe.TCP.Target
										}

										nodeCheck := &NodeCheck{
											Name:     probe.Name,
											Type:     probe.Type,
											Stack:    probe.TCP.Stack,
											Target:   target,
											Port:     probe.TCP.Port,
											Interval: probe.Interval,
											Timeout:  probe.Timeout,
										}
										nodeCheckList.Checks = append(nodeCheckList.Checks, nodeCheck)
									}

									break
								case infrastructurev1alpha1.HealthCheckProbeTypeASSUME:

									if probe.Assume == nil {
										break
									}

									if probe.Assume.Stack == infrastructurev1alpha1.StackTypeDual {
										for _, stack := range []infrastructurev1alpha1.StackType{infrastructurev1alpha1.StackTypeIPv4, infrastructurev1alpha1.StackTypeIPv6} {
											nodeCheck := &NodeCheck{
												Name:     probe.Name,
												Type:     probe.Type,
												Stack:    stack,
												Target:   string(probe.Assume.Status),
												Interval: probe.Interval,
												Timeout:  probe.Timeout,
											}
											nodeCheckList.Checks = append(nodeCheckList.Checks, nodeCheck)
										}
										break
									} else {
										nodeCheck := &NodeCheck{
											Name:     probe.Name,
											Type:     probe.Type,
											Stack:    probe.Assume.Stack,
											Target:   string(probe.Assume.Status),
											Interval: probe.Interval,
											Timeout:  probe.Timeout,
										}
										nodeCheckList.Checks = append(nodeCheckList.Checks, nodeCheck)
									}

									break
								default:
									break
								}
							}
						}
					}

					logf.Log.Info("Adding node check list for node", "key", nm.NodeKey(&node), "location", op.key, "checks", len(nodeCheckList.Checks))

					l.nodeManagers[op.key].nodeCheckList[nm.NodeKey(&node)] = nodeCheckList
					l.nodeManagers[op.key].StartHealthChecks(nm.NodeKey(&node), op.key)
				}
			}
			l.mu.Unlock()

		case "remove":
			l.mu.Lock()
			logf.Log.Info("Processing remove operation for location", "location", op.key)
			if l.promManager != nil {
				l.promManager.RemoveLocation(parseLocationKey(op.key))
			}

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

func NewLocationManager(changeFn func(nodeCheckList *NodeCheckList, location *infrastructurev1alpha1.Location, oldCode, newCode int), specChangeFunc func(location *infrastructurev1alpha1.Location), promManager *PrometheusManager) *LocationManager {
	lm := &LocationManager{
		nodeManagers:   make(map[string]*NodeManager),
		promManager:    promManager,
		opsCh:          make(chan locationManagerOps, 100),
		closed:         false,
		changeFunc:     changeFn,
		specChangeFunc: specChangeFunc,
		mu:             sync.Mutex{},
	}

	go lm.loop()
	return lm
}

func parseLocationKey(key string) types.NamespacedName {
	parts := strings.SplitN(key, "/", 2)
	if len(parts) != 2 {
		return types.NamespacedName{}
	}
	return types.NamespacedName{Namespace: parts[0], Name: parts[1]}
}
