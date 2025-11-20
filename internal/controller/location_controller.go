package controller

import (
	"context"

	infrastructurev1alpha1 "github.com/EdgeCDN-X/edgecdnx-controller/api/v1alpha1"
	"github.com/EdgeCDN-X/healthchecker.git/internal/healthchecker"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
		"location", location.Name,
		"node", node,
		"oldCode", oldCode,
		"newCode", newCode,
	)

	loc := &infrastructurev1alpha1.Location{}
	err := r.Get(context.Background(), client.ObjectKey{Namespace: location.Namespace, Name: location.Name}, loc)
	if err != nil {
		logf.Log.Error(err, "unable to fetch Location for handling health check change", "location", location.Name)
		return
	}

	logf.Log.Info("Updating Node Location Status")

	return
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

	loc.Status = infrastructurev1alpha1.LocationStatus{
		Status: "Healthy",
		NodeStatuses: []infrastructurev1alpha1.NodeInstanceStatus{
			{
				Name:     "n1",
				Instance: "ipv4",
				RetCode:  200,
				IP:       "192.0.2.1",
			},
		},
	}

	err = r.Status().Update(context.Background(), loc)
	if err != nil {
		logf.Log.Error(err, "unable to update Location status after spec change", "location", location.Name)
		return
	}

	logf.Log.Info("Location status updated after spec change", "location", location.Name)
}

func (r *LocationHealthcheckController) SetupWithManager(mgr ctrl.Manager) error {
	locationManager := healthchecker.NewLocationManager(r.HandleChangeFunc, r.HandleSpecChange)
	r.LocationManager = locationManager

	return ctrl.NewControllerManagedBy(mgr).
		For(&infrastructurev1alpha1.Location{}).
		Complete(r)
}
