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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/yaml"

	v1alpha "github.com/intel/gpu-base-operator/api/v1alpha1"
	"github.com/intel/gpu-base-operator/config/deployments"
)

const (
	xpuLabel = "app"
	xpuValue = "intel-xpumanager"

	xpumdContainerName = "xpumd"
	xpumdConfigVolume  = "config"

	monResourcePrefix = "gpu.intel.com"
	monClaim          = "monitor-claim"

	otelConfigMapKey    = "config.yaml"
	otelConfigMountDir  = "/etc/xpumd"
	otelConfigMountPath = otelConfigMountDir + "/otel-config.yaml"
	otelConfigHashKey   = "gpu.intel.com/xpum-otel-config-hash"

	xpumResourcePart = "xpu-manager"
)

type XpuManagerReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Opts   ControllerOpts
}

func processXpumdConfigMapMount(ds *apps.DaemonSet, otelConfigMapName string) {
	cspec := &ds.Spec.Template.Spec

	// Reuse the existing xpumd config volume and remove any legacy otel-specific volume.
	vols := make([]core.Volume, 0, len(cspec.Volumes))

	configVolumeFound := false

	for _, v := range cspec.Volumes {
		switch v.Name {
		case xpumdConfigVolume:
			v.VolumeSource = core.VolumeSource{
				ConfigMap: &core.ConfigMapVolumeSource{
					LocalObjectReference: core.LocalObjectReference{Name: otelConfigMapName},
					DefaultMode:          ptr.To[int32](0420),
				},
			}
			configVolumeFound = true
		}

		vols = append(vols, v)
	}

	if !configVolumeFound {
		vols = append(vols, core.Volume{
			Name: xpumdConfigVolume,
			VolumeSource: core.VolumeSource{
				ConfigMap: &core.ConfigMapVolumeSource{
					LocalObjectReference: core.LocalObjectReference{Name: otelConfigMapName},
					DefaultMode:          ptr.To[int32](0420),
				},
			},
		})
	}

	cspec.Volumes = vols

	for c := range cspec.Containers {
		cont := &cspec.Containers[c]

		if cont.Name != xpumdContainerName {
			continue
		}

		mounts := make([]core.VolumeMount, 0, len(cont.VolumeMounts))
		configMountFound := false

		for _, m := range cont.VolumeMounts {
			switch m.Name {
			case xpumdConfigVolume:
				m.MountPath = otelConfigMountDir
				m.ReadOnly = true
				configMountFound = true
			}

			mounts = append(mounts, m)
		}

		if !configMountFound {
			mounts = append(mounts, core.VolumeMount{
				Name:      xpumdConfigVolume,
				MountPath: otelConfigMountDir,
				ReadOnly:  true,
			})
		}

		cont.VolumeMounts = mounts
	}
}

func processContainerResources(ds *apps.DaemonSet, spec *v1alpha.ClusterPolicy, draClaim string) {
	xspec := &spec.Spec.XpuManagerSpec

	removePrevMonitoring := func(list core.ResourceList) {
		for res := range list {
			if strings.HasPrefix(string(res), monResourcePrefix) && strings.HasSuffix(string(res), "monitoring") {
				delete(list, res)
			}
		}
	}

	// Set resource claim for monitoring.
	if draClaim != "" {
		ds.Spec.Template.Spec.ResourceClaims = []core.PodResourceClaim{
			{Name: monClaim, ResourceClaimTemplateName: &draClaim},
		}
	} else {
		ds.Spec.Template.Spec.ResourceClaims = nil
	}

	for c := range ds.Spec.Template.Spec.Containers {
		cont := &ds.Spec.Template.Spec.Containers[c]

		cont.Image = xspec.Image

		if cont.Name == xpumdContainerName {
			removePrevMonitoring(cont.Resources.Limits)
			removePrevMonitoring(cont.Resources.Requests)

			if draClaim == "" {
				selectedResource := "monitoring"
				if xspec.MonitoringResource != "" {
					selectedResource = xspec.MonitoringResource
				}

				resName := core.ResourceName(fmt.Sprintf("%s/%s", monResourcePrefix, selectedResource))

				if cont.Resources.Limits == nil {
					cont.Resources.Limits = core.ResourceList{
						resName: resource.MustParse("1"),
					}
				} else {
					cont.Resources.Limits[resName] = resource.MustParse("1")
				}
				if cont.Resources.Requests == nil {
					cont.Resources.Requests = core.ResourceList{
						resName: resource.MustParse("1"),
					}
				} else {
					cont.Resources.Requests[resName] = resource.MustParse("1")
				}

				cont.Resources.Claims = nil
			} else {
				cont.Resources.Claims = []core.ResourceClaim{
					{Name: monClaim},
				}
			}
		}
	}
}

// buildOTelConfigData constructs the otel-config.yaml content with thresholds
// from the ClusterPolicy health spec applied to the default embedded config.
func (r *XpuManagerReconciler) buildOTelConfigData(cp *v1alpha.ClusterPolicy) (string, error) {
	cfg := deployments.XpuManagerOTelConfig()

	cfg.Service.Telemetry.Logs.Level = logLevelForXpum(cp)

	if health := cp.Spec.HealthinessSpec; health != nil {
		cfg.Receivers.IntelXPU.CollectionInterval = fmt.Sprintf("%ds", health.CheckIntervalSeconds)

		for i := range cfg.Processors.IntelXPUStatus.Rules {
			rule := &cfg.Processors.IntelXPUStatus.Rules[i]

			switch rule.SourceMetric {
			case "hw.temperature":
				for _, filter := range rule.ComponentFilters {
					for _, loc := range filter.Values {
						switch loc {
						case "gpu":
							setCriticalThreshold(rule, float64(health.CoreTemperatureThreshold))
						case "memory":
							setCriticalThreshold(rule, float64(health.MemoryTemperatureThreshold))
						}
					}
				}
			}
		}
	}

	out, err := yaml.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("failed to marshal otel config: %w", err)
	}

	return string(out), nil
}

// setCriticalThreshold updates the condition values on the "critical" state of a rule.
// All conditions are set to the same threshold, overriding any device-specific defaults.
func setCriticalThreshold(rule *deployments.StatusRule, threshold float64) {
	for i := range rule.States {
		if rule.States[i].StateName == "critical" {
			for j := range rule.States[i].Conditions {
				rule.States[i].Conditions[j].Value = threshold
			}

			return
		}
	}
}

func otelConfigHash(data string) string {
	hash := sha256.Sum256([]byte(data))

	return hex.EncodeToString(hash[:])
}

func (r *XpuManagerReconciler) generateXpumdConfigEntries(ctx context.Context, cp *v1alpha.ClusterPolicy) (string, string, error) {
	if cp.Spec.XpuManagerSpec.ConfigMapOverride == "" {
		cmName := cp.Name + "-xpumanager-otel-config"

		configHash, err := r.createOrUpdateOTelConfigMap(ctx, cp, cmName)
		if err != nil {
			return "", "", fmt.Errorf("unable to create or update otel ConfigMap: %w", err)
		}

		return cmName, configHash, nil
	} else {
		cmName := cp.Spec.XpuManagerSpec.ConfigMapOverride

		configHash, err := r.fetchConfigHashFromConfigMap(ctx, cmName)
		if err != nil {
			return "", "", fmt.Errorf("unable to fetch otel ConfigMap hash: %w", err)
		}

		return cmName, configHash, nil
	}
}

// createOrUpdateOTelConfigMap ensures the otel ConfigMap exists and is up to date.
// It returns true if the ConfigMap was created or updated, false otherwise.
func (r *XpuManagerReconciler) createOrUpdateOTelConfigMap(ctx context.Context, cp *v1alpha.ClusterPolicy, cmName string) (string, error) {
	data, err := r.buildOTelConfigData(cp)
	if err != nil {
		return "", err
	}

	configHash := otelConfigHash(data)

	existing := &core.ConfigMap{}
	if err := r.Get(ctx, client.ObjectKey{Name: cmName, Namespace: r.Opts.Namespace}, existing); err != nil {
		if !errors.IsNotFound(err) {
			return "", err
		}

		cm := &core.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      cmName,
				Namespace: r.Opts.Namespace,
				Labels:    map[string]string{xpuLabel: xpuValue},
			},
			Data: map[string]string{otelConfigMapKey: data},
		}

		if setErr := ctrl.SetControllerReference(cp, cm, r.Scheme); setErr != nil {
			return "", setErr
		}

		if createErr := r.Create(ctx, cm); createErr != nil {
			return "", createErr
		}

		klog.V(2).Infof("Created OTel ConfigMap: %s", cmName)

		return configHash, nil
	}

	if existing.Data[otelConfigMapKey] != data {
		existing.Data[otelConfigMapKey] = data

		if updateErr := r.Update(ctx, existing); updateErr != nil {
			return "", updateErr
		}

		klog.V(2).Infof("Updated OTel ConfigMap: %s", cmName)

		return configHash, nil
	}

	return configHash, nil
}

func (r *XpuManagerReconciler) createMonitoringResourceClaim(ctx context.Context, obj client.Object, claimName string) error {
	// Create ResourceClaimTemplate for monitoring.
	mct := deployments.DynamicResourceAllocationMonitorClaimTemplate()
	mct.Name = claimName
	mct.Namespace = r.Opts.Namespace

	klog.V(2).Infof("Creating claim: %s", claimName)

	if err := ctrl.SetControllerReference(obj, mct, r.Scheme); err != nil {
		klog.Error(err, "unable to set controller reference")

		return err
	}

	if err := r.Create(ctx, mct); err != nil {
		if errors.IsAlreadyExists(err) {
			klog.V(4).Info(err, "ResourceClaimTemplate already exists")

			return nil
		}

		klog.Error(err, "unable to create ResourceClaimTemplate")

		return err
	}

	return nil
}

func (r *XpuManagerReconciler) fetchConfigHashFromConfigMap(ctx context.Context, cmName string) (string, error) {
	overrideCm := &core.ConfigMap{}

	err := r.Get(ctx, client.ObjectKey{Name: cmName, Namespace: r.Opts.Namespace}, overrideCm)
	if err != nil {
		klog.Error(err, "unable to get override ConfigMap")

		return "", err
	}

	if _, ok := overrideCm.Data[otelConfigMapKey]; !ok {
		err := fmt.Errorf("override ConfigMap %s does not contain key %s", cmName, otelConfigMapKey)
		klog.Error(err)

		return "", err
	}

	xpxumConfig := deployments.OTelConfig{}

	if err := yaml.Unmarshal([]byte(overrideCm.Data[otelConfigMapKey]), &xpxumConfig); err != nil {
		err := fmt.Errorf("failed to parse OTel config from override ConfigMap: %w", err)
		klog.Error(err)

		return "", err
	}

	return otelConfigHash(overrideCm.Data[otelConfigMapKey]), nil
}

func (r *XpuManagerReconciler) buildDaemonSetName(cpName string) string {
	return fmt.Sprintf("%s-xpu-manager", cpName)
}

func (r *XpuManagerReconciler) updateDaemonSetObject(ds *apps.DaemonSet, spec *v1alpha.ClusterPolicy, draClaim string, otelConfigMapName string, otelConfigHash string) {
	name := r.buildDaemonSetName(spec.Name)

	ds.Name = name
	ds.Namespace = r.Opts.Namespace
	ds.Labels = map[string]string{
		xpuLabel: xpuValue,
		ownerKey: spec.Name,
	}

	ds.Spec.Template.Annotations = map[string]string{
		otelConfigHashKey: otelConfigHash,
	}

	processContainerResources(ds, spec, draClaim)
	processXpumdConfigMapMount(ds, otelConfigMapName)
	ds.Spec.Template.Spec.NodeSelector = generateNodeSelector(spec)
	ds.Spec.Template.Spec.Tolerations = generateTolerations(spec)

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

	if r.Opts.OpenShift {
		_, _, _, saName := buildOpenShiftNames(spec.Name, xpumResourcePart)
		cspec.ServiceAccountName = saName

		if cspec.Containers[0].SecurityContext == nil {
			cspec.Containers[0].SecurityContext = &core.SecurityContext{}
		}

		// On OpenShift, SELinux labels the container process as container_t which cannot
		// write to host directories or sysfs GPU PMU control files. spc_t bypasses SELinux
		// confinement so xpumd can write to /sys/.../control and /run/xpumd.
		cspec.Containers[0].SecurityContext.SELinuxOptions = &core.SELinuxOptions{
			Type: "spc_t",
		}
	}
}

func (r *XpuManagerReconciler) createOpenShiftResourcesIfNotExists(ctx context.Context, cpName string) error {
	sccName, roleName, bindingName, saName := buildOpenShiftNames(cpName, xpumResourcePart)

	if err := createServiceAccount(ctx, r.Client, saName, r.Opts.Namespace); err != nil {
		return fmt.Errorf("failed to ensure XPUM ServiceAccount: %w", err)
	}

	if err := ensureSCC(ctx, r.Client, buildXpuManagerSCC(sccName)); err != nil {
		return fmt.Errorf("failed to ensure XPUM SCC: %w", err)
	}

	if err := createSCCRole(ctx, r.Client, roleName, sccName); err != nil {
		return fmt.Errorf("failed to ensure XPUM SCC ClusterRole: %w", err)
	}

	if err := createSCCRoleBinding(ctx, r.Client, bindingName, roleName, saName, r.Opts.Namespace); err != nil {
		return fmt.Errorf("failed to ensure XPUM SCC ClusterRoleBinding: %w", err)
	}

	return nil
}

func (r *XpuManagerReconciler) cleanupOpenShiftResources(ctx context.Context, cpName string) {
	sccName, roleName, bindingName, saName := buildOpenShiftNames(cpName, xpumResourcePart)

	deleteOpenShiftSCCResources(ctx, r.Client, sccName, roleName, bindingName, saName, r.Opts.Namespace)
}

func (r *XpuManagerReconciler) buildDaemonSet(cp *v1alpha.ClusterPolicy, draClaim, cmName, configHash string) *apps.DaemonSet {
	ds := deployments.XpuManagerDaemonset()

	r.updateDaemonSetObject(ds, cp, draClaim, cmName, configHash)

	return ds
}

func (r *XpuManagerReconciler) removeDeploymentIfExists(ctx context.Context, cp *v1alpha.ClusterPolicy) (ctrl.Result, error) {
	if r.Opts.OpenShift {
		r.cleanupOpenShiftResources(ctx, r.Opts.ReqName)
	}

	ds := &apps.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      r.buildDaemonSetName(r.Opts.ReqName),
			Namespace: r.Opts.Namespace,
		},
	}

	if err := r.Delete(ctx, ds); client.IgnoreNotFound(err) != nil {
		return ctrl.Result{}, err
	}

	if cp != nil {
		cp.Status.XPUManagerStatus = notAvailableStatus
	}

	klog.Info("XPU Manager deployment removed")

	return ctrl.Result{}, nil
}

func (r *XpuManagerReconciler) updateStatus(ctx context.Context, cp *v1alpha.ClusterPolicy) error {
	ds := &apps.DaemonSet{}
	if err := r.Get(ctx, client.ObjectKey{Name: r.buildDaemonSetName(cp.Name), Namespace: r.Opts.Namespace}, ds); err != nil {
		klog.Error(err, "unable to get XPU Manager DaemonSet to update status")

		return err
	}

	cp.Status.XPUManagerStatus = fmt.Sprintf("%d/%d",
		ds.Status.NumberReady, ds.Status.DesiredNumberScheduled)

	return nil
}

func (r *XpuManagerReconciler) Reconcile(ctx context.Context, cp *v1alpha.ClusterPolicy) (ctrl.Result, error) {
	_ = logf.FromContext(ctx)

	if shouldRemoveXpumd(cp) {
		return r.removeDeploymentIfExists(ctx, cp)
	}

	if r.Opts.OpenShift {
		if err := r.createOpenShiftResourcesIfNotExists(ctx, cp.Name); err != nil {
			klog.Error(err, "unable to ensure OpenShift resources for XPU Manager")

			return ctrl.Result{}, err
		}
	}

	useDra := r.Opts.DRAEnable && cp.Spec.ResourceRegistration == resourceModeDRA
	var draClaim string

	if useDra {
		draClaim = cp.Name + "-monitor-claim"

		// Ensure the claim exists
		if err := r.createMonitoringResourceClaim(ctx, cp, draClaim); err != nil {
			klog.Error(err, "unable to create ResourceClaimTemplate")

			return ctrl.Result{}, err
		}
	}

	cmName, configHash, err := r.generateXpumdConfigEntries(ctx, cp)
	if err != nil {
		klog.Error(err, "unable to create or fetch Xpumd config")

		return ctrl.Result{}, err
	}

	ds := r.buildDaemonSet(cp, draClaim, cmName, configHash)

	if _, err := controllerutil.CreateOrPatch(ctx, r.Client, ds, func() error {
		r.updateDaemonSetObject(ds, cp, draClaim, cmName, configHash)

		return nil
	}); err != nil {
		klog.Error(err, "unable to create or patch XPU Manager DaemonSet")

		return ctrl.Result{}, err
	}

	return ctrl.Result{}, r.updateStatus(ctx, cp)
}
