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
	"fmt"

	adreg "k8s.io/api/admissionregistration/v1"
	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	rbac "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	resv1 "k8s.io/api/resource/v1"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	v1alpha "github.com/intel/gpu-base-operator/api/v1alpha1"
	"github.com/intel/gpu-base-operator/config/deployments"
)

type DRAReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Opts   ControllerOpts
}

const (
	draValue           = "intel-gpu-resource-driver-kubelet-plugin"
	healthCheckPort    = 51516
	gpuDeviceClass     = "gpu.intel.com"
	vfioGpuDeviceClass = "gpu-vfio.intel.com"

	draResourcePart = "gpu-dra"
)

func (r *DRAReconciler) createAll(ctx context.Context, cp *v1alpha.ClusterPolicy) error {
	objects := []client.Object{}

	objName := fmt.Sprintf("%s-gpu-dra", cp.Name)

	// Service account
	sa := deployments.DynamicResourceAllocationServiceAccount()
	sa.Name = objName
	sa.Namespace = r.Opts.Namespace
	objects = append(objects, sa)

	// Cluster role
	cr := deployments.DynamicResourceAllocationClusterRole()
	cr.Name = objName
	objects = append(objects, cr)

	// Cluster role binding
	crb := deployments.DynamicResourceAllocationClusterRoleBinding()
	crb.Name = objName
	crb.Namespace = r.Opts.Namespace

	for i := range crb.Subjects {
		crb.Subjects[i].Namespace = r.Opts.Namespace
		crb.Subjects[i].Name = objName
	}
	crb.RoleRef.Name = objName
	objects = append(objects, crb)

	// Device classes
	objects = append(objects, deployments.DynamicResourceAllocationDeviceClass())

	// Device Class for VFIO and configure it based on the ManageBinding setting in the CR.
	mb := cp.Spec.DynamicResourceAllocationSpec.ManageBinding
	objects = append(objects, deployments.DynamicResourceAllocationDeviceClassVfio(!mb))

	// Validating admission policy
	ap := deployments.DynamicResourceAllocationValidatingAdmissionPolicy()
	ap.Name = objName
	ap.Spec.MatchConditions[0].Expression = fmt.Sprintf("request.userInfo.username == \"system:serviceaccount:%s:%s\"", r.Opts.Namespace, objName)
	objects = append(objects, ap)

	// Validating admission policy binding
	apb := deployments.DynamicResourceAllocationValidatingAdmissionPolicyBinding()
	apb.Name = objName
	apb.Spec.PolicyName = apb.Name
	objects = append(objects, apb)

	for _, o := range objects {
		if err := r.Create(ctx, o); err != nil {
			if errors.IsAlreadyExists(err) {
				klog.Info("object already exists: "+o.GetName(), o)

				continue
			}

			klog.Error(err, "unable to create object", "object", o)

			return err
		}
	}

	return r.createDaemonSet(ctx, cp)
}

func (r *DRAReconciler) ensureVfioDeviceClass(ctx context.Context, manageBinding bool) {
	existing := deployments.DynamicResourceAllocationDeviceClassVfio(!manageBinding)

	if err := r.Get(ctx, client.ObjectKey{Name: existing.Name}, existing); err != nil {
		if errors.IsNotFound(err) {
			klog.Info("VFIO device class not found, creating")

			if err := r.Create(ctx, existing); err != nil {
				klog.Error(err, "unable to create VFIO device class")
			}

			return
		}

		klog.Error(err, "unable to get VFIO device class")
		return
	}

	desired := deployments.DynamicResourceAllocationDeviceClassVfio(!manageBinding)

	diff := cmp.Diff(existing.Spec.Selectors, desired.Spec.Selectors, cmpopts.EquateEmpty())
	if len(diff) > 0 {
		klog.V(2).Info("Updating VFIO device class due to ManageBinding change", "diff", diff)

		existing.Spec.Selectors = desired.Spec.Selectors

		if err := r.Update(ctx, existing); err != nil {
			klog.Error(err, "unable to update VFIO device class")
		}
	}
}

func (r *DRAReconciler) deleteAll(ctx context.Context, crName string) error {
	klog.Info("Deleting all DRA related objects")

	objName := fmt.Sprintf("%s-gpu-dra", crName)

	objects := []client.Object{
		&core.ServiceAccount{
			ObjectMeta: metav1.ObjectMeta{
				Name:      objName,
				Namespace: r.Opts.Namespace,
			},
		},
		&rbac.ClusterRole{
			ObjectMeta: metav1.ObjectMeta{
				Name: objName,
			},
		},
		&rbac.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name:      objName,
				Namespace: r.Opts.Namespace,
			},
		},
		&resv1.DeviceClass{
			ObjectMeta: metav1.ObjectMeta{
				Name: gpuDeviceClass,
			},
		},
		&resv1.DeviceClass{
			ObjectMeta: metav1.ObjectMeta{
				Name: vfioGpuDeviceClass,
			},
		},
		&adreg.ValidatingAdmissionPolicy{
			ObjectMeta: metav1.ObjectMeta{
				Name: objName,
			},
		},
		&adreg.ValidatingAdmissionPolicyBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name: objName,
			},
		},
		&apps.DaemonSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      objName,
				Namespace: r.Opts.Namespace,
			},
		},
	}

	if r.Opts.OpenShift {
		sccName, roleName, bindingName, _ := buildOpenShiftNames(crName, draResourcePart)

		scc := &unstructured.Unstructured{}
		scc.SetAPIVersion(sccAPIVersion)
		scc.SetKind(sccKind)
		scc.SetName(sccName)

		objects = append(objects,
			scc,
			&rbac.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: roleName}},
			&rbac.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: bindingName}},
		)
	}

	for _, o := range objects {
		if err := r.Delete(ctx, o); err != nil {
			if errors.IsNotFound(err) {
				klog.Warningf("unable to delete object, not found: %+v (%+v)", err, o)

				continue
			}

			klog.Error(err, "unable to delete object", "object", o)

			return err
		}
	}

	klog.Info("Objects deleted")

	return nil
}

func (r *DRAReconciler) createOpenShiftResourcesIfNotExists(ctx context.Context, crName string) error {
	sccName, roleName, bindingName, _ := buildOpenShiftNames(crName, draResourcePart)
	// Use the same service account name as the DaemonSet, which is based on the CR name.
	saName := fmt.Sprintf("%s-gpu-dra", crName)

	if err := ensureSCC(ctx, r.Client, buildDRASCC(sccName)); err != nil {
		klog.Error(err, "unable to ensure DRA SCC")

		return err
	}

	if err := createSCCRole(ctx, r.Client, roleName, sccName); err != nil {
		klog.Error(err, "unable to ensure DRA SCC ClusterRole")

		return err
	}

	if err := createSCCRoleBinding(ctx, r.Client, bindingName, roleName, saName, r.Opts.Namespace); err != nil {
		klog.Error(err, "unable to ensure DRA SCC ClusterRoleBinding")

		return err
	}

	return nil
}

func (r *DRAReconciler) anyAllocatedResourceClaims(ctx context.Context) bool {
	var rcList resv1.ResourceClaimList

	klog.Info("Checking for allocated ResourceClaims that would prevent DRA removal")

	if err := r.List(ctx, &rcList); err != nil {
		klog.Error(err, "unable to list ResourceClaims")

		return false
	}

	klog.Infof("Found %d ResourceClaims", len(rcList.Items))
	for _, claim := range rcList.Items {
		alloc := claim.Status.Allocation

		if alloc == nil {
			continue
		}
		if len(alloc.Devices.Results) == 0 {
			continue
		}

		for _, dev := range alloc.Devices.Results {
			if dev.Driver == gpuDeviceClass {
				klog.Infof("Found allocated ResourceClaim with GPU device: %s", claim.Name)

				return true
			}
		}
	}

	return false
}

func addHealthCheckIfMissing(container *core.Container, port int32) {
	for _, p := range container.Ports {
		if p.ContainerPort == port {
			// If port is already there, we assume the health check is already set up.
			return
		}
	}

	service := "liveness"

	container.Ports = append(container.Ports, core.ContainerPort{
		Name:          "healthcheck",
		ContainerPort: port,
	})

	container.StartupProbe = &core.Probe{
		FailureThreshold: 60,
		PeriodSeconds:    10,
		TimeoutSeconds:   10,
		ProbeHandler: core.ProbeHandler{
			GRPC: &core.GRPCAction{
				Port:    port,
				Service: &service,
			},
		},
	}

	container.LivenessProbe = &core.Probe{
		FailureThreshold: 3,
		PeriodSeconds:    30,
		TimeoutSeconds:   10,
		ProbeHandler: core.ProbeHandler{
			GRPC: &core.GRPCAction{
				Port:    port,
				Service: &service,
			},
		},
	}
}

func removeHealthCheckIfExists(container *core.Container) {
	found := false

	for i, p := range container.Ports {
		if p.Name == "healthcheck" {
			container.Ports = append(container.Ports[:i], container.Ports[i+1:]...)
			found = true
			break
		}
	}

	if !found {
		return
	}

	container.StartupProbe = nil
	container.LivenessProbe = nil
}

func (r *DRAReconciler) updateDaemonSetObject(ds *apps.DaemonSet, spec *v1alpha.ClusterPolicy) {
	name := fmt.Sprintf("%s-gpu-dra", spec.Name)

	ds.Name = name
	ds.Namespace = r.Opts.Namespace
	ds.Labels[ownerKey] = spec.Name

	ds.Spec.Template.Spec.ServiceAccountName = name

	dspec := spec.Spec.DynamicResourceAllocationSpec

	ds.Spec.Template.Spec.Containers[0].Image = dspec.Image
	ds.Spec.Template.Spec.Containers[0].Args = r.generateArgs(spec)

	ds.Spec.Template.Spec.NodeSelector = map[string]string{
		"kubernetes.io/arch": "amd64",
	}

	if len(spec.Spec.NodeSelector) > 0 {
		for k, v := range spec.Spec.NodeSelector {
			ds.Spec.Template.Spec.NodeSelector[k] = v
		}
	}

	if spec.Spec.UseNFDLabeling {
		ds.Spec.Template.Spec.NodeSelector["intel.feature.node.kubernetes.io/gpu"] = trueValue
	}

	if len(spec.Spec.Tolerations) > 0 {
		ds.Spec.Template.Spec.Tolerations = spec.Spec.Tolerations
	} else {
		ds.Spec.Template.Spec.Tolerations = nil
	}

	cspec := &ds.Spec.Template.Spec

	// Enable health check for the DRA Pod
	if spec.Spec.DynamicResourceAllocationSpec.PodHealthCheck {
		addHealthCheckIfMissing(&cspec.Containers[0], healthCheckPort)
	} else {
		removeHealthCheckIfExists(&cspec.Containers[0])
	}

	secrets := []core.LocalObjectReference{}
	if r.Opts.SecretName != "" {
		secrets = append(secrets, core.LocalObjectReference{Name: r.Opts.SecretName})
	}
	if spec.Spec.PullSecret != nil {
		secrets = append(secrets, *spec.Spec.PullSecret)
	}

	if len(secrets) > 0 {
		cspec.ImagePullSecrets = secrets
	} else {
		cspec.ImagePullSecrets = nil
	}

	if r.Opts.OpenShift {
		// On OpenShift, SELinux labels the container process as container_t which cannot
		// write to host directories (e.g. /etc/cdi labeled etc_t). spc_t bypasses SELinux
		// confinement for the container so it can write CDI specs to the host filesystem.
		if cspec.Containers[0].SecurityContext == nil {
			cspec.Containers[0].SecurityContext = &core.SecurityContext{}
		}

		cspec.Containers[0].SecurityContext.SELinuxOptions = &core.SELinuxOptions{
			Type: "spc_t",
		}
	}
}

func (r *DRAReconciler) createDaemonSet(ctx context.Context, spec *v1alpha.ClusterPolicy) error {
	ds := deployments.DynamicResourceAllocationDaemonset()

	r.updateDaemonSetObject(ds, spec)

	if err := r.Create(ctx, ds); err != nil {
		klog.Error(err, "unable to create DaemonSet")

		return err
	}

	return nil
}

func (r *DRAReconciler) removeDeploymentIfExists(ctx context.Context) (ctrl.Result, error) {
	crName := r.Opts.ReqName

	dss := &apps.DaemonSetList{}
	labels := client.MatchingLabels{
		appLabel: draValue,
		ownerKey: crName,
	}

	if err := r.List(ctx, dss, client.InNamespace(r.Opts.Namespace), labels); err != nil {
		klog.Error(err, "unable to list child DaemonSets")

		return ctrl.Result{}, err
	}

	if len(dss.Items) == 0 {
		return ctrl.Result{}, nil
	}

	// If there are any allocated ResourceClaims, removal of DRA will cause
	// the Pods using them to be stuck at Terminating.
	// Requeue and try again later.
	if r.anyAllocatedResourceClaims(ctx) {
		return ctrl.Result{RequeueAfter: r.Opts.RequeueDelay}, requeueReconcileErr{}
	}

	if err := r.deleteAll(ctx, r.Opts.ReqName); err != nil {
		return ctrl.Result{}, err
	}

	klog.Info("DRA deployment removed")

	return ctrl.Result{}, nil
}

func (r *DRAReconciler) generateArgs(spec *v1alpha.ClusterPolicy) []string {
	targetLevel := int32(0)

	targetLevel = max(targetLevel, spec.Spec.DynamicResourceAllocationSpec.LogLevel)
	targetLevel = max(targetLevel, spec.Spec.LogLevel)

	args := []string{fmt.Sprintf("-v=%d", targetLevel)}

	if spec.Spec.HealthinessSpec != nil {
		args = append(args, "--health-monitoring=true")

		if spec.Spec.DynamicResourceAllocationSpec.DeviceTaints {
			args = append(args, "--ignore-health-warning=false")
		}
	}

	if spec.Spec.DynamicResourceAllocationSpec.PodHealthCheck {
		args = append(args, fmt.Sprintf("--healthcheck-port=%d", healthCheckPort))
	} else {
		args = append(args, "--healthcheck-port=-1")
	}

	manageBinding := "false"
	if spec.Spec.DynamicResourceAllocationSpec.ManageBinding {
		manageBinding = "true"
	}

	args = append(args, fmt.Sprintf("--manage-binding=%s", manageBinding))

	return args
}

func (r *DRAReconciler) Reconcile(ctx context.Context, cp *v1alpha.ClusterPolicy) (ctrl.Result, error) {
	_ = logf.FromContext(ctx)

	// If DRA is not enable in the cluster, we shouldn't cause errors in trying to do things.
	if !r.Opts.DRAEnable {
		if cp != nil && cp.Spec.ResourceRegistration == resourceModeDRA {
			addIfMissing(&cp.Status.Errors, draNotEnabledMsg)
		}

		return ctrl.Result{}, nil
	}

	if cp == nil || !cp.DeletionTimestamp.IsZero() {
		return r.removeDeploymentIfExists(ctx)
	}

	// DRA not selected, remove existing deployment if exists
	if cp.Spec.ResourceRegistration != resourceModeDRA {
		cp.Status.DRAStatus = notAvailableStatus

		return r.removeDeploymentIfExists(ctx)
	}

	labels := client.MatchingLabels{appLabel: draValue}

	var olderDs apps.DaemonSetList
	if err := r.List(ctx, &olderDs, client.InNamespace(r.Opts.Namespace), labels); err != nil {
		klog.Error(err, "unable to list child DaemonSets")

		return ctrl.Result{}, err
	}

	if r.Opts.OpenShift {
		if err := r.createOpenShiftResourcesIfNotExists(ctx, cp.Name); err != nil {
			klog.Error(err, "unable to ensure OpenShift resources for DRA")

			return ctrl.Result{}, err
		}
	}

	if len(olderDs.Items) == 0 {
		return ctrl.Result{}, r.createAll(ctx, cp)
	}

	// Update DaemonSet

	ds := &olderDs.Items[0]
	originalDs := ds.DeepCopy()

	r.updateDaemonSetObject(ds, cp)

	dsDiff := cmp.Diff(originalDs.Spec.Template.Spec, ds.Spec.Template.Spec, cmpopts.EquateEmpty())
	if len(dsDiff) > 0 {
		klog.Info("DRA difference", "diff", dsDiff)

		if err := r.Update(ctx, ds); err != nil {
			klog.Error(err, "unable to update daemonset", "DaemonSet", ds)

			return ctrl.Result{}, err
		}
	}

	r.ensureVfioDeviceClass(ctx, cp.Spec.DynamicResourceAllocationSpec.ManageBinding)

	if err := r.List(ctx, &olderDs, client.InNamespace(r.Opts.Namespace), labels); err != nil {
		klog.Error(err, "unable to list child DaemonSets")

		return ctrl.Result{}, err
	}

	cp.Status.DRAStatus = fmt.Sprintf("%d/%d",
		olderDs.Items[0].Status.NumberReady, olderDs.Items[0].Status.DesiredNumberScheduled)

	return ctrl.Result{}, nil
}
