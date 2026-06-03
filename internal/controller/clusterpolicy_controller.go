/*
Copyright 2025 Intel Corporation. All Rights Reserved.

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

package controller

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"time"

	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha "github.com/intel/gpu-base-operator/api/v1alpha1"
)

// ClusterPolicyReconciler reconciles a ClusterPolicy object
type ClusterPolicyReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Opts   ControllerOpts
}

type ControllerOpts struct {
	ReqName      string
	Namespace    string
	SecretName   string
	RequeueDelay time.Duration
	DRAEnable    bool
	OpenShift    bool
}

type requeueReconcileErr struct {
	error
}

type SubControllerInterface interface {
	Reconcile(ctx context.Context, cp *v1alpha.ClusterPolicy) (ctrl.Result, error)
}

const (
	appLabel = "app"
	ownerKey = "owner"

	draNotEnabledMsg = "DRA is not enabled in the cluster, but ClusterPolicy requests it."

	resourceModeDRA = "dra"
	resourceModeDP  = "dp"

	trueValue = "true"

	notAvailableStatus = "N/A"

	clusterPolicyFinalizer = "gpu.intel.com/clusterpolicy-protection"

	maxKeptErrors = 10
)

func addIfMissing(slice *[]string, s string) {
	if slices.Contains(*slice, s) {
		return
	}

	*slice = append(*slice, s)
}

// Namespace-scoped resources (apps, batch, core workloads, rbac roles/rolebindings, servicemonitors)
// are intentionally omitted here; they are granted via the namespaced Role in config/rbac/namespaced_role.yaml.

// Except for Pods as we need to list and possible delete them in other namespaces for FW update
// +kubebuilder:rbac:groups="",resources=pods,verbs=delete;get;list;watch

// +kubebuilder:rbac:groups=intel.com,resources=clusterpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=intel.com,resources=clusterpolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=intel.com,resources=clusterpolicies/finalizers,verbs=update

// +kubebuilder:rbac:groups=nfd.k8s-sigs.io,resources=nodefeaturerules,verbs=create;get;update;delete;list;watch

// +kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get;list;watch

// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles,verbs=get;list;create;delete;watch;update
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterrolebindings,verbs=get;list;create;delete;watch
// +kubebuilder:rbac:groups=admissionregistration.k8s.io,resources=validatingadmissionpolicies,verbs=get;list;create;delete
// +kubebuilder:rbac:groups=admissionregistration.k8s.io,resources=validatingadmissionpolicybindings,verbs=get;list;create;delete
// +kubebuilder:rbac:groups=resource.k8s.io,resources=deviceclasses,verbs=get;list;create;delete;watch;update

// +kubebuilder:rbac:groups=resource.k8s.io,resources=resourceclaims,verbs=get;list;watch
// +kubebuilder:rbac:groups=resource.k8s.io,resources=resourceslices,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=resource.k8s.io,resources=resourceclaimtemplates,verbs=get;list;watch;create;update;patch;delete

// +kubebuilder:rbac:groups="",resources=namespaces,verbs=watch;list

// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;patch;update

// +kubebuilder:rbac:groups=security.openshift.io,resources=securitycontextconstraints,verbs=create;delete;get;list;watch;use;update

// Main Reconcile function for ClusterPolicy. Individual sub-controllers will be called from here to handle their
// respective resources, and any errors they return will be aggregated into the ClusterPolicy status.
func (r *ClusterPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = logf.FromContext(ctx)

	cp := &v1alpha.ClusterPolicy{}

	klog.V(2).Info("Reconciling ClusterPolicy: " + req.Name)

	if err := r.Get(ctx, req.NamespacedName, cp); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}

		klog.V(2).Info("ClusterPolicy removal: " + req.Name)

		cp = nil
	}

	var origCp *v1alpha.ClusterPolicy
	if cp != nil {
		origCp = cp.DeepCopy()
	}

	// Defer status update at the end of reconciliation, to ensure we capture any changes made by sub-controllers.
	defer func() {
		// Update status if changed
		if origCp != nil && cp != nil && !reflect.DeepEqual(origCp.Status, cp.Status) {
			if err := r.Status().Update(ctx, cp); err != nil {
				klog.Error(err, "unable to update ClusterPolicy status")
			}
		}
	}()

	// Create a local copy of the options, in case we ever have parallel reconciles with
	// different request names.
	opts := r.Opts
	opts.ReqName = req.Name

	subControllers := make([]SubControllerInterface, 0, 4)

	// Initialize sub-controllers
	subControllers = append(subControllers, &DevicePluginReconciler{Client: r.Client, Scheme: r.Scheme, Opts: opts})
	subControllers = append(subControllers, &XpuManagerReconciler{Client: r.Client, Scheme: r.Scheme, Opts: opts})
	// Include DRA subcontroller even though cluster might not be configured to use DRA, so it can report a status correctly.
	subControllers = append(subControllers, &DRAReconciler{Client: r.Client, Scheme: r.Scheme, Opts: opts})
	subControllers = append(subControllers, &MiscReconciler{Client: r.Client, Scheme: r.Scheme, Opts: opts})

	// Ensure finalizer is present on live (non-deleted) ClusterPolicy objects.
	if cp != nil && cp.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(cp, clusterPolicyFinalizer) {
			controllerutil.AddFinalizer(cp, clusterPolicyFinalizer)

			if err := r.Update(ctx, cp); err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	// Handle deletion: run sub-controllers so they can clean up their resources in the
	// correct order, then remove the finalizer once everything is gone.
	if cp != nil && !cp.DeletionTimestamp.IsZero() {
		for _, subController := range subControllers {
			if ret, err := subController.Reconcile(ctx, cp); err != nil {
				if errors.Is(err, requeueReconcileErr{}) {
					klog.Info("Requeueing deletion reconciliation after sub-controller request")

					return ret, nil
				}

				return ctrl.Result{}, err
			}
		}

		// All sub-controllers completed successfully — safe to remove the finalizer.
		// Suppress the deferred status update to avoid a resource-version conflict
		// after the Update call below.
		origCp = nil

		controllerutil.RemoveFinalizer(cp, clusterPolicyFinalizer)

		if err := r.Update(ctx, cp); err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{}, nil
	}

	var retErr error

	// Delegate reconciliation
	for _, subController := range subControllers {
		if ret, err := subController.Reconcile(ctx, cp); err != nil {
			if errors.Is(err, requeueReconcileErr{}) {
				klog.Info("Requeueing reconciliation after sub-controller request")
				return ret, nil
			}

			klog.Error("Return sub-controller error", err)

			retErr = err

			for len(cp.Status.Errors) > maxKeptErrors {
				cp.Status.Errors = cp.Status.Errors[1:]
			}

			cp.Status.Errors = append(cp.Status.Errors, err.Error())
		}
	}

	return ctrl.Result{}, retErr
}

// draPodToClusterPolicy maps any DRA pod event to reconcile requests for all existing
// ClusterPolicy objects. This avoids relying on r.Opts.ReqName (startup config) as a
// source of truth — instead it queries the actual state of the cluster.
func (r *ClusterPolicyReconciler) draPodToClusterPolicy(ctx context.Context, _ client.Object) []reconcile.Request {
	cpList := &v1alpha.ClusterPolicyList{}
	if err := r.List(ctx, cpList); err != nil {
		klog.Error(err, "failed to list ClusterPolicies for DRA pod event")
		return nil
	}

	reqs := make([]reconcile.Request, len(cpList.Items))
	for i := range cpList.Items {
		reqs[i] = reconcile.Request{
			NamespacedName: types.NamespacedName{Name: cpList.Items[i].Name},
		}
	}

	return reqs
}

// draPodReadinessPredicate returns a predicate that passes only DRA pod events where
// the Ready condition has changed (or the pod was created/deleted). This avoids
// spurious reconciles while still refreshing ClusterPolicy status when a DRA pod
// health check transitions between passing and failing.
func draPodReadinessPredicate() predicate.Predicate {
	isDRAPod := func(obj client.Object) bool {
		return obj.GetLabels()[appLabel] == draValue
	}

	isPodReady := func(pod *core.Pod) bool {
		for _, c := range pod.Status.Conditions {
			if c.Type == core.PodReady {
				return c.Status == core.ConditionTrue
			}
		}

		return false
	}

	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return isDRAPod(e.Object)
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			if !isDRAPod(e.ObjectNew) {
				return false
			}

			oldPod, ok1 := e.ObjectOld.(*core.Pod)
			newPod, ok2 := e.ObjectNew.(*core.Pod)

			if !ok1 || !ok2 {
				return false
			}

			return isPodReady(oldPod) != isPodReady(newPod)
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return isDRAPod(e.Object)
		},
		GenericFunc: func(e event.GenericEvent) bool {
			return false
		},
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *ClusterPolicyReconciler) SetupWithManager(mgr ctrl.Manager, opts ControllerOpts) error {
	r.Opts = opts

	b := ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha.ClusterPolicy{}).
		Named("clusterpolicy").
		Owns(&apps.DaemonSet{})

	// Only watch DRA pods when DRA is enabled in the cluster, to avoid unnecessary
	// pod list/watch permissions and reconcile noise when DRA is not in use.
	if opts.DRAEnable {
		b = b.Watches(
			&core.Pod{},
			handler.EnqueueRequestsFromMapFunc(r.draPodToClusterPolicy),
			builder.WithPredicates(draPodReadinessPredicate()),
		)
	}

	return b.Complete(r)
}
