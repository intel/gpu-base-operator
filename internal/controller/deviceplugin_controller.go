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
	"slices"
	"strings"

	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	v1alpha "github.com/intel/gpu-base-operator/api/v1alpha1"
	"github.com/intel/gpu-base-operator/config/deployments"
)

type DevicePluginReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Opts   ControllerOpts
}

const (
	dpValue         = "intel-gpu-plugin"
	xpumdVolumeName = "runxpumd"

	dpResourcePart = "gpu-dp"
)

func logLevelForDp(spec *v1alpha.ClusterPolicy) int32 {
	logLevel := int32(0)
	logLevel = max(logLevel, spec.Spec.LogLevel)
	logLevel = max(logLevel, spec.Spec.DevicePluginSpec.LogLevel)

	return logLevel
}

func hexArgStr(s []string) string {
	a := []string{}
	for _, str := range s {
		if !strings.HasPrefix(str, "0x") {
			str = "0x" + str
		}

		a = append(a, str)
	}
	return strings.Join(a, ",")
}

func addXpumdMounts(spec *core.PodSpec) {
	for _, v := range spec.Volumes {
		if v.Name == xpumdVolumeName {
			return
		}
	}

	dirOrCreate := core.HostPathDirectoryOrCreate

	spec.Volumes = append(spec.Volumes, core.Volume{
		Name: xpumdVolumeName,
		VolumeSource: core.VolumeSource{
			HostPath: &core.HostPathVolumeSource{
				Path: "/run/xpumd",
				Type: &dirOrCreate,
			},
		},
	})

	spec.Containers[0].VolumeMounts = append(spec.Containers[0].VolumeMounts,
		core.VolumeMount{
			Name:      xpumdVolumeName,
			MountPath: "/run/xpumd",
		},
	)
}

func removeXpumdMounts(spec *core.PodSpec) {
	for i, v := range spec.Volumes {
		if v.Name == xpumdVolumeName {
			spec.Volumes = slices.Delete(spec.Volumes, i, i+1)
			break
		}
	}

	for i, vm := range spec.Containers[0].VolumeMounts {
		if vm.Name == xpumdVolumeName {
			spec.Containers[0].VolumeMounts = slices.Delete(spec.Containers[0].VolumeMounts, i, i+1)
			break
		}
	}
}

func dpArgs(spec *v1alpha.ClusterPolicy) []string {
	args := []string{}

	dpspec := spec.Spec.DevicePluginSpec

	if spec.Spec.ResourceMonitoring {
		args = append(args,
			"-enable-monitoring",
			"-xpumd-endpoint=/run/xpumd/intelxpuinfo.sock")
	}

	logLevel := logLevelForDp(spec)
	if logLevel > 0 {
		args = append(args, fmt.Sprintf("-v=%d", logLevel))
	}

	if len(dpspec.ByPathMode) > 0 {
		args = append(args, fmt.Sprintf("-bypath=%s", dpspec.ByPathMode))
	}

	if len(dpspec.AllowIDs) > 0 {
		args = append(args, fmt.Sprintf("-allow-ids=%s", hexArgStr(dpspec.AllowIDs)))
	}

	if len(dpspec.DenyIDs) > 0 {
		args = append(args, fmt.Sprintf("-deny-ids=%s", hexArgStr(dpspec.DenyIDs)))
	}

	return args
}

func (r *DevicePluginReconciler) updateDaemonSetObject(ds *apps.DaemonSet, spec *v1alpha.ClusterPolicy) {
	name := fmt.Sprintf("%s-device-plugin", spec.Name)

	ds.Name = name
	ds.Namespace = r.Opts.Namespace
	ds.Labels[ownerKey] = spec.Name

	dspec := &spec.Spec.DevicePluginSpec

	ds.Spec.Template.Spec.Containers[0].Image = dspec.PluginImage

	ds.Spec.Template.Spec.Containers[0].Args = dpArgs(spec)

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

	if spec.Spec.ResourceMonitoring {
		addXpumdMounts(cspec)
	} else {
		removeXpumdMounts(cspec)
	}

	if r.Opts.OpenShift {
		_, _, _, saName := buildOpenShiftNames(spec.Name, dpResourcePart)
		cspec.ServiceAccountName = saName
	}
}

func (r *DevicePluginReconciler) createOpenShiftResourcesIfNotExists(ctx context.Context, cpName string) error {
	sccName, roleName, bindingName, saName := buildOpenShiftNames(cpName, dpResourcePart)

	if err := createServiceAccount(ctx, r.Client, saName, r.Opts.Namespace); err != nil {
		return fmt.Errorf("failed to ensure DP ServiceAccount: %w", err)
	}

	if err := ensureSCC(ctx, r.Client, buildDevicePluginSCC(sccName)); err != nil {
		return fmt.Errorf("failed to ensure DP SCC: %w", err)
	}

	if err := createSCCRole(ctx, r.Client, roleName, sccName); err != nil {
		return fmt.Errorf("failed to ensure DP SCC ClusterRole: %w", err)
	}

	if err := createSCCRoleBinding(ctx, r.Client, bindingName, roleName, saName, r.Opts.Namespace); err != nil {
		return fmt.Errorf("failed to ensure DP SCC ClusterRoleBinding: %w", err)
	}

	return nil
}

func (r *DevicePluginReconciler) cleanupOpenShiftResources(ctx context.Context, cpName string) {
	sccName, roleName, bindingName, saName := buildOpenShiftNames(cpName, dpResourcePart)

	deleteOpenShiftSCCResources(ctx, r.Client, sccName, roleName, bindingName, saName, r.Opts.Namespace)
}

func (r *DevicePluginReconciler) createDaemonSet(ctx context.Context, obj client.Object) (ctrl.Result, error) {
	spec := obj.(*v1alpha.ClusterPolicy)

	ds := deployments.DevicePluginDaemonset()

	r.updateDaemonSetObject(ds, spec)

	if err := ctrl.SetControllerReference(obj, ds, r.Scheme); err != nil {
		klog.Error(err, "unable to set controller reference")

		return ctrl.Result{}, err
	}

	if err := r.Create(ctx, ds); err != nil {
		klog.Error(err, "unable to create DaemonSet")

		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *DevicePluginReconciler) removeDeploymentIfExists(ctx context.Context) (ctrl.Result, error) {
	klog.V(4).Info("Removing Device Plugin deployment")

	crName := r.Opts.ReqName

	if r.Opts.OpenShift {
		r.cleanupOpenShiftResources(ctx, crName)
	}

	dss := &apps.DaemonSetList{}
	labels := client.MatchingLabels{
		appLabel: dpValue,
		ownerKey: crName,
	}

	if err := r.List(ctx, dss, client.InNamespace(r.Opts.Namespace), labels); err != nil {
		klog.Error(err, "unable to list child DaemonSets")

		return ctrl.Result{}, err
	}

	if len(dss.Items) == 0 {
		klog.V(4).Info("No DevicePlugin deployment found, nothing to do")

		return ctrl.Result{}, nil
	}

	if err := r.Delete(ctx, &dss.Items[0]); err != nil {
		return ctrl.Result{}, err
	}

	klog.V(4).Info("DevicePlugin deployment removed")

	return ctrl.Result{}, nil
}

func (r *DevicePluginReconciler) Reconcile(ctx context.Context, cp *v1alpha.ClusterPolicy) (ctrl.Result, error) {
	_ = logf.FromContext(ctx)

	if cp == nil || !cp.DeletionTimestamp.IsZero() {
		return r.removeDeploymentIfExists(ctx)
	}

	if cp.Spec.ResourceRegistration != "dp" {
		cp.Status.DevicePluginStatus = notAvailableStatus

		return r.removeDeploymentIfExists(ctx)
	}

	var olderDs apps.DaemonSetList
	if err := r.List(ctx, &olderDs, client.InNamespace(r.Opts.Namespace), client.MatchingLabels{appLabel: dpValue}); err != nil {
		klog.Error(err, "unable to list child DaemonSets")

		return ctrl.Result{}, err
	}

	if r.Opts.OpenShift {
		if err := r.createOpenShiftResourcesIfNotExists(ctx, cp.Name); err != nil {
			klog.Error(err, "unable to ensure OpenShift resources for DP")

			return ctrl.Result{}, err
		}
	}

	if len(olderDs.Items) == 0 {
		return r.createDaemonSet(ctx, cp)
	}

	// Update DaemonSet

	ds := &olderDs.Items[0]
	originalDs := ds.DeepCopy()

	r.updateDaemonSetObject(ds, cp)

	dsDiff := cmp.Diff(originalDs.Spec.Template.Spec, ds.Spec.Template.Spec, cmpopts.EquateEmpty())
	if len(dsDiff) > 0 {
		klog.Info("DS difference", "diff", dsDiff)

		if err := r.Update(ctx, ds); err != nil {
			klog.Error(err, "unable to update daemonset", "DaemonSet", ds)

			return ctrl.Result{}, err
		}
	}

	if err := r.List(ctx, &olderDs, client.InNamespace(r.Opts.Namespace), client.MatchingLabels{appLabel: dpValue}); err != nil {
		klog.Error(err, "unable to list child DaemonSets")

		return ctrl.Result{}, err
	}

	cp.Status.DevicePluginStatus = fmt.Sprintf("%d/%d",
		olderDs.Items[0].Status.NumberReady, olderDs.Items[0].Status.DesiredNumberScheduled)

	return ctrl.Result{}, nil
}
