package controller

import (
	"context"

	infrastructurev1alpha1 "github.com/EdgeCDN-X/edgecdnx-controller/api/v1alpha1"

	"github.com/EdgeCDN-X/healthchecker.git/internal/healthchecker"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

type LocationHealthcheckController struct {
	client.Client
	Scheme          *runtime.Scheme
	LocationManager *healthchecker.LocationManager
	Prometheus      healthchecker.PrometheusConfig
}

func (r *LocationHealthcheckController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	loc := &infrastructurev1alpha1.Location{}
	if err := r.Get(ctx, req.NamespacedName, loc); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("Location resource not found. Removing location from registry")
			r.LocationManager.RemoveLocation(req.NamespacedName)

			return ctrl.Result{}, nil
		}
		log.Error(err, "unable to fetch Location")
		return ctrl.Result{}, err
	}

	log.V(1).Info("Reconciling Location", "version", loc.ObjectMeta.ResourceVersion)

	if loc.DeletionTimestamp != nil {
		log.Info("Location is being deleted. Removing location from registry")
		r.LocationManager.RemoveLocation(req.NamespacedName)
		return ctrl.Result{}, nil
	}

	if loc.Status.Status == "Healthy" {
		r.LocationManager.AddLocation(loc)
		log.Info("Location added/updated in registry", "location", req.NamespacedName)
	} else {
		r.LocationManager.RemoveLocation(req.NamespacedName)
		log.Info("Location not healthy. Removed from registry", "location", req.NamespacedName)
	}

	return ctrl.Result{}, nil
}

func (r *LocationHealthcheckController) HandleChangeFunc(nodeCheckList *healthchecker.NodeCheckList, location *infrastructurev1alpha1.Location, oldCode, newCode int) {
	logf.Log.Info("Health check status changed",
		"checks", nodeCheckList.Checks,
		"location", location.Name,
		"oldCode", oldCode,
		"newCode", newCode,
	)

	loc := &infrastructurev1alpha1.Location{}
	err := r.Get(context.Background(), client.ObjectKey{Namespace: location.Namespace, Name: location.Name}, loc)
	if err != nil {
		logf.Log.Error(err, "unable to fetch Location for handling health check change", "location", location.Name)
		return
	}
	logf.Log.V(1).Info("Fetched Location Info", "name", loc.Name, "namespace", loc.Namespace, "observerGeneration", loc.ObjectMeta.Generation, "ResourceVersion", loc.ObjectMeta.ResourceVersion)

	conditions := make([]infrastructurev1alpha1.NodeCondition, 0)
	for _, nc := range nodeCheckList.Checks {
		time := metav1.Now()
		condition := infrastructurev1alpha1.NodeCondition{
			Type:               nc.Condition,
			Status:             nc.Alive,
			Reason:             nc.LastRetMessage,
			LastTransitionTime: time,
			ObservedGeneration: location.Generation,
		}

		conditions = append(conditions, condition)
	}
	newLoc := loc.DeepCopy()

	if newLoc.Status.NodeStatus == nil {
		newLoc.Status.NodeStatus = make(map[string]infrastructurev1alpha1.NodeInstanceStatus)
	}
	current := newLoc.Status.NodeStatus[nodeCheckList.Name]

	newLoc.Status.NodeStatus[nodeCheckList.Name] = infrastructurev1alpha1.NodeInstanceStatus{
		Conditions: conditions,
		Alerts:     current.Alerts,
	}

	err = r.Status().Patch(context.Background(), newLoc, client.MergeFrom(loc))

	if err != nil {
		logf.Log.Error(err, "unable to patch Location status after health check change", "location", location.Name)
		logf.Log.Info("Last Fetched Location Info", "name", loc.Name, "namespace", loc.Namespace, "observerGeneration", loc.ObjectMeta.Generation, "ResourceVersion", loc.ObjectMeta.ResourceVersion)
		return
	}

	return
}

func (r *LocationHealthcheckController) HandleAlertChangeFunc(location *infrastructurev1alpha1.Location, locationAlerts []infrastructurev1alpha1.PrometheusAlertStatus, nodeAlerts map[string][]infrastructurev1alpha1.PrometheusAlertStatus) {
	loc := &infrastructurev1alpha1.Location{}
	err := r.Get(context.Background(), client.ObjectKey{Namespace: location.Namespace, Name: location.Name}, loc)
	if err != nil {
		logf.Log.Error(err, "unable to fetch Location for handling alert change", "location", location.Name)
		return
	}

	newLoc := loc.DeepCopy()
	newLoc.Status.Alerts = stampTransitionTime(locationAlerts)

	if newLoc.Status.NodeStatus == nil {
		newLoc.Status.NodeStatus = make(map[string]infrastructurev1alpha1.NodeInstanceStatus)
	}

	for nodeKey, alerts := range nodeAlerts {
		status := newLoc.Status.NodeStatus[nodeKey]
		status.Alerts = stampTransitionTime(alerts)
		newLoc.Status.NodeStatus[nodeKey] = status
	}

	if err := r.Status().Patch(context.Background(), newLoc, client.MergeFrom(loc)); err != nil {
		logf.Log.Error(err, "unable to patch Location status after alert change", "location", location.Name)
	}
}

func (r *LocationHealthcheckController) HandleSpecChange(location *infrastructurev1alpha1.Location) {
	logf.Log.Info("Location spec changed",
		"location", location.Name,
	)

	loc := &infrastructurev1alpha1.Location{}
	err := r.Get(context.Background(), client.ObjectKey{Namespace: location.Namespace, Name: location.Name}, loc)
	if err != nil {
		logf.Log.Error(err, "unable to fetch Location for handling spec change", "location", location.Name)
		return
	}

	newLoc := loc.DeepCopy()
	defs := buildNodeAlertDefinitions(location)

	newLoc.Status.Alerts = filterStatusAlertsBySpec(newLoc.Status.Alerts, location.Spec.Alerts)

	for nodeKey, nodeStatus := range newLoc.Status.NodeStatus {
		matchers, found := defs[nodeKey]
		if !found {
			logf.Log.Info("Removing node status for deleted node", "location", location.Name, "node", nodeKey)
			delete(newLoc.Status.NodeStatus, nodeKey)
			continue
		}

		nodeStatus.Alerts = filterStatusAlertsBySpec(nodeStatus.Alerts, matchers)
		newLoc.Status.NodeStatus[nodeKey] = nodeStatus
	}

	err = r.Status().Patch(context.Background(), newLoc, client.MergeFrom(loc))
}

func (r *LocationHealthcheckController) SetupWithManager(mgr ctrl.Manager) error {
	promManager, err := healthchecker.NewPrometheusManager(r.Prometheus, r.HandleAlertChangeFunc)
	if err != nil {
		return err
	}

	locationManager := healthchecker.NewLocationManager(r.HandleChangeFunc, r.HandleSpecChange, promManager)
	r.LocationManager = locationManager

	return ctrl.NewControllerManagedBy(mgr).
		For(&infrastructurev1alpha1.Location{}).
		Complete(r)
}

func buildNodeAlertDefinitions(location *infrastructurev1alpha1.Location) map[string][]infrastructurev1alpha1.PrometheusAlertMatcherSpec {
	defs := make(map[string][]infrastructurev1alpha1.PrometheusAlertMatcherSpec)
	for _, node := range location.Spec.Nodes {
		defs[node.Name] = node.Alerts
	}

	for _, nodeGroup := range location.Spec.NodeGroups {
		for _, node := range nodeGroup.Nodes {
			defs[node.Name] = node.Alerts
		}
	}

	return defs
}

func filterStatusAlertsBySpec(statuses []infrastructurev1alpha1.PrometheusAlertStatus, matchers []infrastructurev1alpha1.PrometheusAlertMatcherSpec) []infrastructurev1alpha1.PrometheusAlertStatus {
	if len(matchers) == 0 {
		return nil
	}

	filtered := make([]infrastructurev1alpha1.PrometheusAlertStatus, 0, len(statuses))
	for _, status := range statuses {
		for _, matcher := range matchers {
			if status.AlertName != matcher.AlertName {
				continue
			}
			if !labelsMatch(matcher.Labels, status.Labels) {
				continue
			}

			filtered = append(filtered, status)
			break
		}
	}

	return filtered
}

func labelsMatch(selector map[string]string, labels map[string]string) bool {
	for key, expected := range selector {
		if labels[key] != expected {
			return false
		}
	}

	return true
}

func stampTransitionTime(statuses []infrastructurev1alpha1.PrometheusAlertStatus) []infrastructurev1alpha1.PrometheusAlertStatus {
	now := metav1.Now()
	stamped := make([]infrastructurev1alpha1.PrometheusAlertStatus, 0, len(statuses))
	for _, status := range statuses {
		copyStatus := status
		copyStatus.LastTransitionTime = now
		stamped = append(stamped, copyStatus)
	}
	return stamped
}
