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
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/yaml"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
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

func processNodeSelectors(ds *apps.DaemonSet, spec *v1alpha.ClusterPolicy) {
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
}

// Convert the integer based log level to a string based log level for the OTel config.
func logLevelForXpum(cp *v1alpha.ClusterPolicy) string {
	v := cp.Spec.XpuManagerSpec.LogLevel
	v = max(cp.Spec.LogLevel, v)

	switch v {
	case 0:
		return "error"
	case 1:
		return "warn"
	case 2:
		return "info"
	default:
		return "debug"
	}
}

// buildOTelConfigData constructs the otel-config.yaml content with thresholds
// from the ClusterPolicy health spec applied to the default embedded config.
func (r *XpuManagerReconciler) buildOTelConfigData(cp *v1alpha.ClusterPolicy) (string, error) {
	cfg := deployments.XpuManagerOTelConfig()

	cfg.Receivers.IntelXPU.LogLevel = logLevelForXpum(cp)

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
							setWarningThreshold(rule, float64(health.CoreTemperatureThreshold))
						case "memory":
							setWarningThreshold(rule, float64(health.MemoryTemperatureThreshold))
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

// setWarningThreshold updates the first condition value on the "warning" state of a rule.
func setWarningThreshold(rule *deployments.StatusRule, threshold float64) {
	for i := range rule.States {
		if rule.States[i].StateName == "warning" && len(rule.States[i].Conditions) > 0 {
			rule.States[i].Conditions[0].Value = threshold

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
			klog.Warning(err, "ResourceClaimTemplate already exists")

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

func (r *XpuManagerReconciler) updateDaemonSetObject(ds *apps.DaemonSet, spec *v1alpha.ClusterPolicy, draClaim string, otelConfigMapName string, otelConfigHash string) {
	name := fmt.Sprintf("%s-xpu-manager", spec.Name)

	ds.Name = name
	ds.Namespace = r.Opts.Namespace
	ds.Labels = map[string]string{
		xpuLabel: xpuValue,
		ownerKey: spec.Name,
	}

	if ds.Spec.Template.Annotations == nil {
		ds.Spec.Template.Annotations = map[string]string{}
	}
	ds.Spec.Template.Annotations[otelConfigHashKey] = otelConfigHash

	processContainerResources(ds, spec, draClaim)
	processXpumdConfigMapMount(ds, otelConfigMapName)
	processNodeSelectors(ds, spec)

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
}

func (r *XpuManagerReconciler) createDaemonSet(ctx context.Context, obj client.Object) (ctrl.Result, error) {
	cp := obj.(*v1alpha.ClusterPolicy)

	ds := deployments.XpuManagerDaemonset()

	useDra := r.Opts.DRAEnable && cp.Spec.ResourceRegistration == resourceModeDRA
	var draClaim string

	if useDra {
		draClaim = cp.Name + "-monitor-claim"

		if err := r.createMonitoringResourceClaim(ctx, obj, draClaim); err != nil {
			klog.Error(err, "unable to create ResourceClaimTemplate")

			return ctrl.Result{}, err
		}
	}

	cmName, configHash, err := r.generateXpumdConfigEntries(ctx, cp)
	if err != nil {
		klog.Error(err, "unable to create or fetch Xpumd config")

		return ctrl.Result{}, err
	}

	r.updateDaemonSetObject(ds, cp, draClaim, cmName, configHash)

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

func (r *XpuManagerReconciler) removeDeploymentIfExists(ctx context.Context) (ctrl.Result, error) {
	dss := &apps.DaemonSetList{}

	matching := client.MatchingLabels{
		xpuLabel: xpuValue,
		ownerKey: r.Opts.ReqName,
	}

	if err := r.List(ctx, dss, client.InNamespace(r.Opts.Namespace), matching); err != nil {
		klog.Error(err, "unable to list XPU Manager DaemonSets")

		return ctrl.Result{}, err
	}

	if len(dss.Items) == 0 {
		return ctrl.Result{}, nil
	}

	if err := r.Delete(ctx, &dss.Items[0]); err != nil {
		return ctrl.Result{}, err
	}

	klog.Info("XPU Manager deployment removed")

	return ctrl.Result{}, nil
}

func (r *XpuManagerReconciler) Reconcile(ctx context.Context, cp *v1alpha.ClusterPolicy) (ctrl.Result, error) {
	_ = logf.FromContext(ctx)

	// Delete DaemonSet if ClusterPolicy is being deleted
	if cp == nil || cp.DeletionTimestamp != nil {
		return r.removeDeploymentIfExists(ctx)
	}

	if !cp.Spec.ResourceMonitoring {
		cp.Status.XPUManagerStatus = notAvailableStatus

		return r.removeDeploymentIfExists(ctx)
	}

	var olderDs apps.DaemonSetList
	if err := r.List(ctx, &olderDs, client.InNamespace(r.Opts.Namespace), client.MatchingLabels{xpuLabel: xpuValue}); err != nil {
		klog.Error(err, "unable to list child DaemonSets")

		return ctrl.Result{}, err
	}

	if len(olderDs.Items) == 0 {
		return r.createDaemonSet(ctx, cp)
	}

	// Update DaemonSet

	ds := &olderDs.Items[0]
	originalDs := ds.DeepCopy()

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

	configMapChanged := originalDs.Spec.Template.Annotations[otelConfigHashKey] != configHash

	r.updateDaemonSetObject(ds, cp, draClaim, cmName, configHash)

	dsDiff := cmp.Diff(originalDs.Spec.Template.Spec, ds.Spec.Template.Spec, cmpopts.EquateEmpty())
	if configMapChanged || len(dsDiff) > 0 {
		klog.Info("DS difference", "diff", dsDiff)

		if err := r.Update(ctx, ds); err != nil {
			klog.Error(err, "unable to update daemonset", "DaemonSet", ds)

			return ctrl.Result{}, err
		}
	}

	if err := r.List(ctx, &olderDs, client.InNamespace(r.Opts.Namespace), client.MatchingLabels{xpuLabel: xpuValue}); err != nil {
		klog.Error(err, "unable to list child DaemonSets")

		return ctrl.Result{}, err
	}

	cp.Status.XPUManagerStatus = fmt.Sprintf("%d/%d",
		olderDs.Items[0].Status.NumberReady, olderDs.Items[0].Status.DesiredNumberScheduled)

	return ctrl.Result{}, nil
}
