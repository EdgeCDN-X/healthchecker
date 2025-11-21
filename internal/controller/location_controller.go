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
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if loc.DeletionTimestamp != nil {
		log.Info("Location is being deleted. Removing location from registry")
		r.LocationManager.RemoveLocation(req.NamespacedName)
		return ctrl.Result{}, nil
	}

	r.LocationManager.AddLocation(loc)
	log.Info("Location added/updated in registry", "location", req.NamespacedName)

	return ctrl.Result{}, nil
}

func (r *LocationHealthcheckController) HandleChangeFunc(node *healthchecker.NodeCheck, location *infrastructurev1alpha1.Location, oldCode, newCode int) {
	logf.Log.Info("Health check status changed",
		"condition", node.Condition,
		"nodeName", node.Name,
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

	nodeInstanceStatus, nodeExists := loc.Status.NodeStatus[node.Name]

	newNC := infrastructurev1alpha1.NodeCondition{
		Type:               node.Condition,
		Status:             node.Alive,
		Reason:             node.LastRetMessage,
		LastTransitionTime: metav1.Now(),
		ObservedGeneration: location.Generation,
	}

	if nodeExists {
		conditionExists := false
		for i, condition := range nodeInstanceStatus.Conditions {
			if condition.Type == node.Condition {
				conditionExists = true
				nodeInstanceStatus.Conditions[i] = newNC
				break
			}
		}
		if !conditionExists {
			nodeInstanceStatus.Conditions = append(nodeInstanceStatus.Conditions, newNC)
		}
	} else {
		nodeInstanceStatus = infrastructurev1alpha1.NodeInstanceStatus{
			Conditions: []infrastructurev1alpha1.NodeCondition{
				newNC,
			},
		}
	}

	if loc.Status.NodeStatus == nil {
		loc.Status.NodeStatus = make(map[string]infrastructurev1alpha1.NodeInstanceStatus)
	}

	loc.Status.NodeStatus[node.Name] = nodeInstanceStatus

	err = r.Status().Update(context.Background(), loc)
	if err != nil {
		logf.Log.Error(err, "unable to update Location status after health check change", "location", location.Name)
		return
	}

	return
}

func (r *LocationHealthcheckController) HandleSpecChange(location *infrastructurev1alpha1.Location) {
	// TODO perhaps clean up old / new nodes
	logf.Log.Info("Location spec changed",
		"location", location.Name,
	)

	loc := &infrastructurev1alpha1.Location{}
	err := r.Get(context.Background(), client.ObjectKey{Namespace: location.Namespace, Name: location.Name}, loc)
	if err != nil {
		logf.Log.Error(err, "unable to fetch Location for handling spec change", "location", location.Name)
		return
	}
}

func (r *LocationHealthcheckController) SetupWithManager(mgr ctrl.Manager) error {
	locationManager := healthchecker.NewLocationManager(r.HandleChangeFunc, r.HandleSpecChange)
	r.LocationManager = locationManager

	return ctrl.NewControllerManagedBy(mgr).
		For(&infrastructurev1alpha1.Location{}).
		Complete(r)
}
