/*
Copyright 2026 Intel Corporation. All Rights Reserved.

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

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	v1alpha "github.com/intel/gpu-base-operator/api/v1alpha1"
	kmmv1beta1 "github.com/kubernetes-sigs/kernel-module-management/api/v1beta1"
)

// +kubebuilder:rbac:groups=kmm.sigs.x-k8s.io,resources=modules,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kmm.sigs.x-k8s.io,resources=modules/status,verbs=get

// KMMReconciler manages a KMM Module CR for out-of-tree kernel module loading.
// It only configures the moduleLoader section — DP/DRA lifecycle remains with the native controllers.
type KMMReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Opts   ControllerOpts
}

const (
	kmmModuleSuffix = "-gpu"

	kmmNotEnabledMsg = "KMM is not installed in the cluster."
)

func kmmModuleName(cpName string) string {
	return cpName + kmmModuleSuffix
}

func (r *KMMReconciler) Reconcile(ctx context.Context, cp *v1alpha.ClusterPolicy) (ctrl.Result, error) {
	moduleName := kmmModuleName(r.Opts.ReqName)

	if !r.Opts.KMMEnable {
		if cp != nil && cp.Spec.KernelModule != nil {
			addIfMissing(&cp.Status.Errors, kmmNotEnabledMsg)
		}

		return ctrl.Result{}, nil
	}

	if cp == nil || cp.Spec.KernelModule == nil {
		return r.deleteModuleIfExists(ctx, cp, moduleName)
	}

	// KMM's Module webhook forbids toggling container.version between empty and
	// non-empty in place, so recreate the Module when the user does so.
	if res, done, err := r.recreateOnVersionToggle(ctx, cp, moduleName); done || err != nil {
		return res, err
	}

	mod := &kmmv1beta1.Module{
		ObjectMeta: metav1.ObjectMeta{
			Name:      moduleName,
			Namespace: r.Opts.Namespace,
		},
	}

	result, err := controllerutil.CreateOrPatch(ctx, r.Client, mod, func() error {
		return r.setModuleDesiredState(mod, cp)
	})

	r.updateStatus(cp, mod)

	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to reconcile KMM Module %s for ClusterPolicy %s: %w", moduleName, cp.Name, err)
	}

	klog.Infof("KMM Module %s %s", moduleName, result)

	// Setting Version opts into KMM's ordered upgrade, where KMM loads the module
	// only onto nodes carrying a matching version-module label. Flag this so admins
	// aren't left wondering why the driver silently isn't loading (see
	// KernelModuleSpec.Version).
	if cp.Spec.KernelModule.Version != "" {
		klog.Infof("KMM Module %s uses ordered upgrade (version=%q); nodes must be labeled "+
			"kmm.node.kubernetes.io/version-module.%s.%s=%s before the module loads",
			moduleName, cp.Spec.KernelModule.Version, r.Opts.Namespace, moduleName, cp.Spec.KernelModule.Version)
	}

	return ctrl.Result{}, nil
}

// recreateOnVersionToggle deletes an existing Module when its
// moduleLoader.container.version is being toggled to or from empty, which KMM's
// webhook rejects as an in-place update. It returns done=true when the caller
// should stop and requeue; the next reconcile recreates the Module with the new
// version once the old one is gone.
func (r *KMMReconciler) recreateOnVersionToggle(ctx context.Context, cp *v1alpha.ClusterPolicy, name string) (ctrl.Result, bool, error) {
	existing := &kmmv1beta1.Module{}
	key := types.NamespacedName{Name: name, Namespace: r.Opts.Namespace}
	if err := r.Get(ctx, key, existing); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, false, nil
		}
		return ctrl.Result{}, false, fmt.Errorf("failed to get KMM Module %s: %w", name, err)
	}

	var currentVersion string
	if existing.Spec.ModuleLoader != nil {
		currentVersion = existing.Spec.ModuleLoader.Container.Version
	}

	// Same emptiness (both set or both empty) means an in-place patch is allowed.
	if (currentVersion == "") == (cp.Spec.KernelModule.Version == "") {
		return ctrl.Result{}, false, nil
	}

	if !existing.DeletionTimestamp.IsZero() {
		// Deletion already underway; wait for it to finish before recreating.
		cp.Status.KMMStatus = "Recreating"
		return ctrl.Result{RequeueAfter: r.Opts.RequeueDelay}, true, nil
	}

	// Recreating unloads the driver, so don't yank it out from under in-use GPUs.
	if r.Opts.DRAEnable && anyAllocatedResourceClaims(ctx, r.Client, gpuDeviceClass) {
		addIfMissing(&cp.Status.Errors,
			fmt.Sprintf("allocated GPU ResourceClaims blocking modules.kmm.sigs.x-k8s.io/%s recreation for version change",
				name))
		return ctrl.Result{RequeueAfter: r.Opts.RequeueDelay}, true, requeueReconcileErr{}
	}

	klog.Infof("KMM Module %s version toggled (%q -> %q); recreating", name, currentVersion, cp.Spec.KernelModule.Version)
	if err := r.Delete(ctx, existing); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, false, fmt.Errorf("failed to delete KMM Module %s for version change: %w", name, err)
	}

	cp.Status.KMMStatus = "Recreating"
	return ctrl.Result{RequeueAfter: r.Opts.RequeueDelay}, true, nil
}

func (r *KMMReconciler) setModuleDesiredState(mod *kmmv1beta1.Module, cp *v1alpha.ClusterPolicy) error {
	if err := ctrl.SetControllerReference(cp, mod, r.Scheme); err != nil {
		return fmt.Errorf("failed to set controller reference: %w", err)
	}

	mod.Spec.Selector = generateNodeSelector(cp, r.Opts)
	mod.Spec.Tolerations = generateTolerations(cp)
	mod.Spec.ImageRepoSecret = cp.Spec.PullSecret

	r.setModuleLoader(mod, cp)

	return nil
}

func (r *KMMReconciler) setModuleLoader(mod *kmmv1beta1.Module, cp *v1alpha.ClusterPolicy) {
	km := cp.Spec.KernelModule

	container := kmmv1beta1.ModuleLoaderContainerSpec{
		Modprobe: kmmv1beta1.ModprobeSpec{
			ModuleName:          km.ModuleName,
			FirmwarePath:        km.FirmwarePath,
			ModulesLoadingOrder: km.ModulesLoadingOrder,
		},
		Version:               km.Version,
		InTreeModulesToRemove: []string{km.ModuleName},
		ImagePullPolicy:       v1.PullIfNotPresent,
	}

	if km.RegistryTLS != nil {
		container.RegistryTLS = kmmv1beta1.TLSOptions{
			Insecure:              km.RegistryTLS.Insecure,
			InsecureSkipTLSVerify: km.RegistryTLS.InsecureSkipTLSVerify,
		}
	}

	mappings := make([]kmmv1beta1.KernelMapping, 0, len(km.KernelMappings))
	for _, m := range km.KernelMappings {
		mapping := kmmv1beta1.KernelMapping{
			Regexp:         m.Regexp,
			ContainerImage: m.ContainerImage,
		}
		if len(m.InTreeModulesToRemove) > 0 {
			mapping.InTreeModulesToRemove = dedupeStrings(
				append([]string{km.ModuleName}, m.InTreeModulesToRemove...))
		}
		if m.RegistryTLS != nil {
			mapping.RegistryTLS = &kmmv1beta1.TLSOptions{
				Insecure:              m.RegistryTLS.Insecure,
				InsecureSkipTLSVerify: m.RegistryTLS.InsecureSkipTLSVerify,
			}
		}
		if m.Build != nil {
			mapping.Build = convertBuildSpec(m.Build, mod.Spec.Selector)
		}
		mappings = append(mappings, mapping)
	}
	container.KernelMappings = mappings

	mod.Spec.ModuleLoader = &kmmv1beta1.ModuleLoaderSpec{
		Container:          container,
		ServiceAccountName: r.Opts.ModuleLoaderServiceAccountName,
	}
}

func convertBuildSpec(src *v1alpha.KernelModuleBuildSpec, selector map[string]string) *kmmv1beta1.Build {
	build := &kmmv1beta1.Build{
		DockerfileConfigMap: &v1.LocalObjectReference{Name: src.DockerfileConfigMap.Name},
		Secrets:             append([]v1.LocalObjectReference{}, src.Secrets...),
		// Build on the same nodes the module targets: in-cluster driver builds
		// need the GPU nodes' kernel headers/toolchain to compile against.
		Selector: selector,
	}

	if len(src.BuildArgs) > 0 {
		args := make([]kmmv1beta1.BuildArg, len(src.BuildArgs))
		for i, a := range src.BuildArgs {
			args[i] = kmmv1beta1.BuildArg{Name: a.Name, Value: a.Value}
		}
		build.BuildArgs = args
	}

	return build
}

func dedupeStrings(s []string) []string {
	seen := make(map[string]bool, len(s))
	result := make([]string, 0, len(s))

	for _, v := range s {
		if !seen[v] {
			seen[v] = true
			result = append(result, v)
		}
	}

	return result
}

func (r *KMMReconciler) deleteModuleIfExists(ctx context.Context, cp *v1alpha.ClusterPolicy, name string) (ctrl.Result, error) {
	mod := &kmmv1beta1.Module{}
	key := types.NamespacedName{Name: name, Namespace: r.Opts.Namespace}

	if err := r.Get(ctx, key, mod); err != nil {
		if apierrors.IsNotFound(err) {
			if cp != nil {
				cp.Status.KMMStatus = notAvailableStatus
			}
			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, fmt.Errorf("failed to get KMM Module %s: %w", name, err)
	}

	if r.Opts.DRAEnable && anyAllocatedResourceClaims(ctx, r.Client, gpuDeviceClass) {
		if cp != nil {
			addIfMissing(&cp.Status.Errors,
				fmt.Sprintf("allocated GPU ResourceClaims blocking modules.kmm.sigs.x-k8s.io/%s deletion",
					name))
		}
		return ctrl.Result{RequeueAfter: r.Opts.RequeueDelay}, requeueReconcileErr{}
	}

	if !mod.DeletionTimestamp.IsZero() {
		if cp != nil {
			cp.Status.KMMStatus = "Removing"
			addIfMissing(&cp.Status.Errors,
				fmt.Sprintf("modules.kmm.sigs.x-k8s.io/%s is pending deletion",
					name))
		}
		return ctrl.Result{RequeueAfter: r.Opts.RequeueDelay}, nil
	}

	klog.Infof("Deleting KMM Module %s", name)

	if err := r.Delete(ctx, mod); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("failed to delete KMM Module %s: %w", name, err)
	}

	return ctrl.Result{}, nil
}

func (r *KMMReconciler) updateStatus(cp *v1alpha.ClusterPolicy, mod *kmmv1beta1.Module) {
	if cp.Spec.KernelModule != nil {
		mlStatus := mod.Status.ModuleLoader
		cp.Status.KMMStatus = fmt.Sprintf("%d/%d", mlStatus.AvailableNumber, mlStatus.DesiredNumber)

		if mlStatus.DesiredNumber > 0 && mlStatus.AvailableNumber < mlStatus.DesiredNumber {
			addIfMissing(&cp.Status.Errors,
				fmt.Sprintf("module loader not fully available (%d/%d) for modules.kmm.sigs.x-k8s.io/%s",
					mlStatus.AvailableNumber, mlStatus.DesiredNumber, mod.Name))
		}
	} else {
		cp.Status.KMMStatus = notAvailableStatus
	}
}
