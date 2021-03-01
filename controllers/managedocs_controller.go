/*


Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	controller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/go-logr/logr"
	ocsv1 "github.com/openshift/ocs-operator/pkg/apis/ocs/v1"
	v1 "github.com/openshift/ocs-osd-deployer/api/v1alpha1"
	"github.com/openshift/ocs-osd-deployer/templates"
	"github.com/openshift/ocs-osd-deployer/utils"
	promv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	promv1a1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1alpha1"
)

const (
	storageClusterName     = "ocs-storagecluster"
	prometheusName         = "managed-ocs-prometheus"
	alertmanagerName       = "managed-ocs-alertmanager"
	alertmanagerConfigName = "managed-ocs-alertmanager-config"

	storageClassSizeKey = "size"
	deviceSetName       = "default"
	monLabelKey         = "app"
	monLabelValue       = "managed-ocs"
)

// ManagedOCSReconciler reconciles a ManagedOCS object
type ManagedOCSReconciler struct {
	client.Client

	Log                  logr.Logger
	Scheme               *runtime.Scheme
	AddonParamSecretName string

	ctx                context.Context
	managedOCS         *v1.ManagedOCS
	storageCluster     *ocsv1.StorageCluster
	prometheus         *promv1.Prometheus
	alertmanager       *promv1.Alertmanager
	alertmanagerConfig *promv1a1.AlertmanagerConfig
	namespace          string
	reconcileStrategy  v1.ReconcileStrategy
}

// Add necessary rbac permissions for managedocs finalizer in order to set blockOwnerDeletion.
// +kubebuilder:rbac:groups=ocs.openshift.io,namespace=system,resources={managedocs,managedocs/finalizers},verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=ocs.openshift.io,namespace=system,resources=managedocs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=ocs.openshift.io,namespace=system,resources=storageclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="monitoring.coreos.com",namespace=system,resources=alertmanagerconfigs,verbs=get;list;watch
// +kubebuilder:rbac:groups="monitoring.coreos.com",namespace=system,resources=alertmanagers,verbs=get;list;watch
// +kubebuilder:rbac:groups="monitoring.coreos.com",namespace=system,resources=prometheuses,verbs=get;list;watch
// +kubebuilder:rbac:groups="monitoring.coreos.com",namespace=system,resources=podmonitors,verbs=get;list;watch
// +kubebuilder:rbac:groups="monitoring.coreos.com",namespace=system,resources=servicemonitors,verbs=get;list;watch
// +kubebuilder:rbac:groups="monitoring.coreos.com",namespace=system,resources=prometheuserules,verbs=get;list;watch
// +kubebuilder:rbac:groups="",namespace=system,resources=secrets,verbs=get;list;watch

// SetupWithManager creates an setup a ManagedOCSReconciler to work with the provided manager
func (r *ManagedOCSReconciler) SetupWithManager(mgr ctrl.Manager) error {
	ctrlOptions := controller.Options{
		MaxConcurrentReconciles: 1,
	}
	managedOCSPredicates := builder.WithPredicates(
		predicate.GenerationChangedPredicate{},
	)
	addonParamsSecretPredicates := builder.WithPredicates(
		predicate.NewPredicateFuncs(
			func(meta metav1.Object, _ runtime.Object) bool {
				return meta.GetName() == r.AddonParamSecretName
			},
		),
	)
	monResourcesPredicates := builder.WithPredicates(
		predicate.NewPredicateFuncs(
			func(meta metav1.Object, _ runtime.Object) bool {
				labels := meta.GetLabels()
				return labels == nil || labels[monLabelKey] != monLabelValue
			},
		),
	)
	enqueueManangedOCSRequest := handler.EnqueueRequestsFromMapFunc{
		ToRequests: handler.ToRequestsFunc(
			func(obj handler.MapObject) []reconcile.Request {
				return []reconcile.Request{{
					NamespacedName: types.NamespacedName{
						Name:      "managedocs",
						Namespace: obj.Meta.GetNamespace(),
					},
				}}
			},
		),
	}

	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(ctrlOptions).
		For(&v1.ManagedOCS{}, managedOCSPredicates).

		// Watch owned resources
		Owns(&ocsv1.StorageCluster{}).
		Owns(&promv1.Prometheus{}).
		Owns(&promv1.Alertmanager{}).
		Owns(&promv1a1.AlertmanagerConfig{}).

		// Watch non-owned resources
		Watches(
			&source.Kind{Type: &corev1.Secret{}},
			&enqueueManangedOCSRequest,
			addonParamsSecretPredicates,
		).
		Watches(
			&source.Kind{Type: &promv1.PodMonitor{}},
			&enqueueManangedOCSRequest,
			monResourcesPredicates,
		).
		Watches(
			&source.Kind{Type: &promv1.ServiceMonitor{}},
			&enqueueManangedOCSRequest,
			monResourcesPredicates,
		).
		Watches(
			&source.Kind{Type: &promv1.PrometheusRule{}},
			&enqueueManangedOCSRequest,
			monResourcesPredicates,
		).

		// Create the controller
		Complete(r)
}

// Reconcile TODO
func (r *ManagedOCSReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("req.Namespace", req.Namespace, "req.Name", req.Name)
	log.Info("Starting reconcile for ManagedOCS")

	r.ctx = context.Background()
	r.namespace = req.Namespace

	// Setup the reconciler from the request
	r.reconileSetup(req)

	// Load the managed ocs resource (input)
	if err := r.Get(r.ctx, req.NamespacedName, r.managedOCS); err != nil {
		return ctrl.Result{}, err
	}

	// Run the reconcile phases
	err := r.reconcilePhases()
	if err != nil {
		r.Log.Error(err, "An error was encountered during reconcilePhases")
	}

	// Ensure status is updated once even on failed reconciles
	statusErr := r.Status().Update(r.ctx, r.managedOCS)

	// Reconcile errors have priority to status update errors
	if err != nil {
		return ctrl.Result{}, err
	} else if statusErr != nil {
		return ctrl.Result{}, statusErr
	} else {
		return ctrl.Result{}, nil
	}
}
func (r *ManagedOCSReconciler) reconileSetup(req ctrl.Request) {
	r.ctx = context.Background()
	r.namespace = req.NamespacedName.Namespace

	r.managedOCS = &v1.ManagedOCS{}
	r.managedOCS.Name = req.NamespacedName.Name
	r.managedOCS.Namespace = r.namespace

	r.storageCluster = &ocsv1.StorageCluster{}
	r.storageCluster.Name = storageClusterName
	r.storageCluster.Namespace = r.namespace

	r.prometheus = &promv1.Prometheus{}
	r.prometheus.Name = prometheusName
	r.prometheus.Namespace = r.namespace

	r.alertmanager = &promv1.Alertmanager{}
	r.alertmanager.Name = alertmanagerName
	r.alertmanager.Namespace = r.namespace

	r.alertmanagerConfig = &promv1a1.AlertmanagerConfig{}
	r.alertmanagerConfig.Name = alertmanagerConfigName
	r.alertmanagerConfig.Namespace = r.namespace
}

func (r *ManagedOCSReconciler) reconcilePhases() error {
	// Update the status of the components
	r.managedOCS.Status.Components = r.getComponentStatus()

	// Find the effective reconcile strategy
	r.reconcileStrategy = v1.ReconcileStrategyStrict
	if strings.EqualFold(string(r.managedOCS.Spec.ReconcileStrategy), string(v1.ReconcileStrategyNone)) {
		r.reconcileStrategy = v1.ReconcileStrategyNone
	}

	// Reconcile the different owned resources
	if err := r.reconcileStorageCluster(); err != nil {
		return err
	}
	if err := r.reconcilePrometheus(); err != nil {
		return err
	}
	if err := r.reconcileAlertmanager(); err != nil {
		return err
	}
	if err := r.reconcileAlertmanagerConfig(); err != nil {
		return err
	}
	if err := r.reconcileMonitoringResources(); err != nil {
		return err
	}

	r.managedOCS.Status.ReconcileStrategy = r.reconcileStrategy
	return nil
}

func (r *ManagedOCSReconciler) getComponentStatus() v1.ComponentStatusMap {
	var components v1.ComponentStatusMap

	// Checking the StorageCluster component.
	var sc ocsv1.StorageCluster

	scNamespacedName := types.NamespacedName{
		Name:      storageClusterName,
		Namespace: r.namespace,
	}

	err := r.Get(r.ctx, scNamespacedName, &sc)
	if err == nil {
		if sc.Status.Phase == "Ready" {
			components.StorageCluster.State = v1.ComponentReady
		} else {
			components.StorageCluster.State = v1.ComponentPending
		}
	} else if errors.IsNotFound(err) {
		components.StorageCluster.State = v1.ComponentNotFound
	} else {
		r.Log.Error(err, "error getting StorageCluster, setting compoment status to Unknown")
		components.StorageCluster.State = v1.ComponentUnknown
	}

	return components
}

func (r *ManagedOCSReconciler) reconcileStorageCluster() error {
	r.Log.Info("Reconciling StorageCluster")

	_, err := controllerutil.CreateOrUpdate(r.ctx, r.Client, r.storageCluster, func() error {
		if err := r.own(r.storageCluster); err != nil {
			return err
		}

		// Handle only strict mode reconciliation
		if r.reconcileStrategy == v1.ReconcileStrategyStrict {
			// Get an instance of the desired state
			desired := utils.ObjectFromTemplate(templates.StorageClusterTemplate, r.Scheme).(*ocsv1.StorageCluster)

			if err := r.updateStorageClusterFromAddOnParamsSecret(desired); err != nil {
				return err
			}

			// Override storage cluster spec with desired spec from the template.
			// We do not replace meta or status on purpose
			r.storageCluster.Spec = desired.Spec
		}
		return nil
	})
	if err != nil {
		return err
	}

	return nil
}

func (r *ManagedOCSReconciler) reconcilePrometheus() error {
	r.Log.Info("Reconciling Prometheus")

	_, err := controllerutil.CreateOrUpdate(r.ctx, r.Client, r.prometheus, func() error {
		if err := r.own(r.prometheus); err != nil {
			return err
		}

		desired := utils.ObjectFromTemplate(templates.PrometheusTemplate, r.Scheme).(*promv1.Prometheus)
		r.prometheus.ObjectMeta.Labels = map[string]string{monLabelKey: monLabelValue}
		r.prometheus.Spec = desired.Spec

		return nil
	})
	if err != nil {
		return err
	}

	return nil
}

func (r *ManagedOCSReconciler) reconcileAlertmanager() error {
	r.Log.Info("Reconciling Alertmanager")
	_, err := controllerutil.CreateOrUpdate(r.ctx, r.Client, r.alertmanager, func() error {
		if err := r.own(r.alertmanager); err != nil {
			return err
		}

		desired := utils.ObjectFromTemplate(templates.AlertmanagerTemplate, r.Scheme).(*promv1.Alertmanager)
		r.alertmanager.ObjectMeta.Labels = map[string]string{monLabelKey: monLabelValue}
		r.alertmanager.Spec = desired.Spec

		return nil
	})
	if err != nil {
		return err
	}

	return nil
}

func (r *ManagedOCSReconciler) reconcileAlertmanagerConfig() error {
	r.Log.Info("Reconciling AlertmanagerConfig")
	_, err := controllerutil.CreateOrUpdate(r.ctx, r.Client, r.alertmanagerConfig, func() error {
		if err := r.own(r.alertmanagerConfig); err != nil {
			return err
		}

		desired := utils.ObjectFromTemplate(templates.AlertmanagerConfigTemplate, r.Scheme).(*promv1a1.AlertmanagerConfig)
		r.alertmanagerConfig.ObjectMeta.Labels = map[string]string{monLabelKey: monLabelValue}
		// TODO: Setup pagerduty and deadman sqtich on the desired alertmanagr config
		r.alertmanagerConfig.Spec = desired.Spec

		return nil
	})
	if err != nil {
		return err
	}

	return nil
}

func (r *ManagedOCSReconciler) reconcileMonitoringResources() error {
	r.Log.Info("Reconciling Monitoring Resources")
	listOptions := client.InNamespace(r.namespace)

	serviceMonitorList := promv1.ServiceMonitorList{}
	if err := r.List(r.ctx, &serviceMonitorList, listOptions); err == nil {
		for _, obj := range serviceMonitorList.Items {
			utils.AddLabel(obj, monLabelKey, monLabelValue)
			if err := r.Update(r.ctx, obj); err != nil {
				return err
			}
		}
	} else {
		return err
	}

	podMonitorList := promv1.PodMonitorList{}
	if err := r.List(r.ctx, &podMonitorList, listOptions); err == nil {
		for _, obj := range podMonitorList.Items {
			utils.AddLabel(obj, monLabelKey, monLabelValue)
			if err := r.Update(r.ctx, obj); err != nil {
				return err
			}
		}
	} else {
		return err
	}

	promRuleList := promv1.PrometheusRuleList{}
	if err := r.List(r.ctx, &promRuleList, listOptions); err != nil {
		for _, obj := range promRuleList.Items {
			utils.AddLabel(obj, monLabelKey, monLabelValue)
			if err := r.Update(r.ctx, obj); err != nil {
				return err
			}
		}
	} else {
		return err
	}

	return nil
}

func (r *ManagedOCSReconciler) updateStorageClusterFromAddOnParamsSecret(sc *ocsv1.StorageCluster) error {
	// The addon param secret will contain the capacity of the cluster in Ti
	// size = 1,  creates a cluster of 1 Ti capacity
	// size = 2,  creates a cluster of 2 Ti capacity etc

	addonParamSecret := corev1.Secret{}
	if err := r.Get(
		r.ctx,
		types.NamespacedName{Namespace: r.namespace, Name: r.AddonParamSecretName},
		&addonParamSecret,
	); err != nil {
		// Do not create the StorageCluster if the we fail to get the addon param secret
		return fmt.Errorf("Failed to get the addon param secret, Secret Name: %v", r.AddonParamSecretName)
	}
	addonParams := addonParamSecret.Data

	sizeAsString := string(addonParams[storageClassSizeKey])
	sdsCount, err := strconv.Atoi(sizeAsString)
	if err != nil {
		return fmt.Errorf("Invalid storage cluster size value: %v", sizeAsString)
	}

	r.Log.Info("Storage cluster parameters", "count", sdsCount)

	var ds *ocsv1.StorageDeviceSet = nil
	for _, item := range sc.Spec.StorageDeviceSets {
		if item.Name == deviceSetName {
			ds = &item
			break
		}
	}
	if ds == nil {
		return fmt.Errorf("cloud not find default device set on stroage cluster")
	}

	sc.Spec.StorageDeviceSets[0].Count = sdsCount
	return nil
}

func (r *ManagedOCSReconciler) own(resource metav1.Object) error {
	// Ensure managedOCS ownership on a resource
	if err := ctrl.SetControllerReference(r.managedOCS, resource, r.Scheme); err != nil {
		return err
	}
	return nil
}
