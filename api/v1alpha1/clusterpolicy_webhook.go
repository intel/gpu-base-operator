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

package v1alpha1

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/distribution/reference"
)

// Default images used when no image is specified in the ClusterPolicy spec.
// These match the pinned images shipped in the Helm chart values.
const (
	DefaultDPPluginImage    = "docker.io/intel/intel-gpu-plugin:0.35.0@sha256:34697f9c286857da986381595ac2a693524a83c831955247dae47dfda4d2f526"
	DefaultDPLevelzeroImage = "docker.io/intel/intel-gpu-levelzero:0.35.0@sha256:a8a0729f253de6e8e117a7ef621883a1228f7304747076dcd1446c3e18804021"
	DefaultDRAImage         = "ghcr.io/intel/intel-resource-drivers-for-kubernetes/intel-gpu-resource-driver:v0.10.0@sha256:746150e64010881dbfdaeb74771703b13cac365a89ee47c4d7499d686ea4163f"
	DefaultXPUImage         = "ghcr.io/intel/xpumanager/xpumd:v2.0.0-rc.0@sha256:8f020012f68888314402c0332a53718ace4ade9913476bbd125af89edb760a8b"
)

// SetupClusterPolicyWebhookWithManager registers the webhook for ClusterPolicy in the manager.
func SetupClusterPolicyWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &ClusterPolicy{}).
		WithCustomDefaulter(&ClusterPolicyCustomDefaulter{}).
		WithCustomValidator(&ClusterPolicyCustomValidator{}).
		Complete()
}

// +kubebuilder:webhook:path=/mutate-intel-com-v1alpha1-clusterpolicy,mutating=true,failurePolicy=fail,sideEffects=None,groups=intel.com,resources=clusterpolicies,verbs=create;update,versions=v1alpha1,name=mclusterpolicy-v1alpha1.kb.io,admissionReviewVersions=v1

// ClusterPolicyCustomDefaulter sets default image values on ClusterPolicy resources.
type ClusterPolicyCustomDefaulter struct{}

var _ webhook.CustomDefaulter = &ClusterPolicyCustomDefaulter{}

// Default implements webhook.CustomDefaulter. It fills in missing image references
// with the pinned known-good images shipped with this version of the operator.
func (d *ClusterPolicyCustomDefaulter) Default(_ context.Context, obj runtime.Object) error {
	cp, ok := obj.(*ClusterPolicy)
	if !ok {
		return fmt.Errorf("expected a ClusterPolicy object")
	}

	spec := &cp.Spec

	if spec.DevicePluginSpec.PluginImage == "" {
		spec.DevicePluginSpec.PluginImage = DefaultDPPluginImage
	}

	if spec.DevicePluginSpec.LevelzeroImage == "" {
		spec.DevicePluginSpec.LevelzeroImage = DefaultDPLevelzeroImage
	}

	if spec.DynamicResourceAllocationSpec.Image == "" {
		spec.DynamicResourceAllocationSpec.Image = DefaultDRAImage
	}

	if spec.XpuManagerSpec.Image == "" {
		spec.XpuManagerSpec.Image = DefaultXPUImage
	}

	return nil
}

// +kubebuilder:webhook:path=/validate-intel-com-v1alpha1-clusterpolicy,mutating=false,failurePolicy=fail,sideEffects=None,groups=intel.com,resources=clusterpolicies,verbs=create;update,versions=v1alpha1,name=vclusterpolicy-v1alpha1.kb.io,admissionReviewVersions=v1

// ClusterPolicyCustomValidator validates ClusterPolicy resources on create and update.
type ClusterPolicyCustomValidator struct{}

var _ webhook.CustomValidator = &ClusterPolicyCustomValidator{}

var pciIDRegexp = regexp.MustCompile(`^0x[0-9a-f]{4}$`)

func validateClusterPolicySpec(spec *ClusterPolicySpec) (admission.Warnings, error) {
	var errs []error
	var warnings admission.Warnings

	errs = append(errs, validateDPSpec(&spec.DevicePluginSpec)...)
	errs = append(errs, validateImageFields(spec)...)
	errs = append(errs, validatePullSecret(spec)...)
	errs = append(errs, validateConfigMapOverride(spec)...)
	errs = append(errs, validateKueueSpec(spec)...)

	if w := warnHealthSpec(spec); w != "" {
		warnings = append(warnings, w)
	}

	return warnings, errors.Join(errs...)
}

func validateDPSpec(dp *DevicePluginSpec) []error {
	var errs []error

	if len(dp.AllowIDs) > 0 && len(dp.DenyIDs) > 0 {
		errs = append(errs, fmt.Errorf("dp.allowIDs and dp.denyIDs cannot both be set"))
	}

	for _, id := range dp.AllowIDs {
		if !pciIDRegexp.MatchString(id) {
			errs = append(errs, fmt.Errorf("invalid PCI device ID in dp.allowIDs: %q (must match 0x[0-9a-f]{4})", id))
		}
	}

	for _, id := range dp.DenyIDs {
		if !pciIDRegexp.MatchString(id) {
			errs = append(errs, fmt.Errorf("invalid PCI device ID in dp.denyIDs: %q (must match 0x[0-9a-f]{4})", id))
		}
	}

	return errs
}

func validateImageFields(spec *ClusterPolicySpec) []error {
	type imageField struct {
		field string
		value string
	}

	fields := []imageField{
		{"dra.image", spec.DynamicResourceAllocationSpec.Image},
		{"dp.plugin", spec.DevicePluginSpec.PluginImage},
		{"dp.levelzero", spec.DevicePluginSpec.LevelzeroImage},
		{"xpu.image", spec.XpuManagerSpec.Image},
	}

	var errs []error

	for _, f := range fields {
		if f.value == "" {
			continue
		}

		if _, err := reference.ParseNormalizedNamed(f.value); err != nil {
			errs = append(errs, fmt.Errorf("invalid image reference in %s: %q: %w", f.field, f.value, err))
		}
	}

	return errs
}

func validatePullSecret(spec *ClusterPolicySpec) []error {
	if spec.PullSecret == nil {
		return nil
	}

	if msgs := validation.IsDNS1123Subdomain(spec.PullSecret.Name); len(msgs) > 0 {
		return []error{fmt.Errorf("pullSecret.name %q is not a valid Kubernetes object name: %s",
			spec.PullSecret.Name, strings.Join(msgs, "; "))}
	}

	return nil
}

func validateConfigMapOverride(spec *ClusterPolicySpec) []error {
	if spec.XpuManagerSpec.ConfigMapOverride == "" {
		return nil
	}

	if msgs := validation.IsDNS1123Subdomain(spec.XpuManagerSpec.ConfigMapOverride); len(msgs) > 0 {
		return []error{fmt.Errorf("xpu.configMapOverride %q is not a valid Kubernetes object name: %s",
			spec.XpuManagerSpec.ConfigMapOverride, strings.Join(msgs, "; "))}
	}

	return nil
}

func validateKueueSpec(spec *ClusterPolicySpec) []error {
	if spec.Kueue == nil {
		return nil
	}

	var errs []error
	seenCQNames := map[string]bool{}

	for cqIdx, cq := range spec.Kueue.EqualResources {
		if msgs := validation.IsDNS1123Subdomain(cq.Name); len(msgs) > 0 {
			errs = append(errs, fmt.Errorf("kueue.equalResources[%d].name %q is not a valid Kubernetes object name: %s",
				cqIdx, cq.Name, strings.Join(msgs, "; ")))
		}

		if seenCQNames[cq.Name] {
			errs = append(errs, fmt.Errorf("kueue.equalResources: duplicate clusterQueue name %q", cq.Name))
		}

		seenCQNames[cq.Name] = true
		seenLQKeys := map[string]bool{}

		for lqIdx, lq := range cq.LocalQueues {
			if msgs := validation.IsDNS1123Subdomain(lq.Name); len(msgs) > 0 {
				errs = append(errs, fmt.Errorf("kueue.equalResources[%d].localQueues[%d].name %q is not a valid Kubernetes object name: %s",
					cqIdx, lqIdx, lq.Name, strings.Join(msgs, "; ")))
			}

			if msgs := validation.IsDNS1123Label(lq.Namespace); len(msgs) > 0 {
				errs = append(errs, fmt.Errorf("kueue.equalResources[%d].localQueues[%d].namespace %q is not a valid namespace name: %s",
					cqIdx, lqIdx, lq.Namespace, strings.Join(msgs, "; ")))
			}

			key := lq.Namespace + "/" + lq.Name
			if seenLQKeys[key] {
				errs = append(errs, fmt.Errorf("kueue.equalResources[%d]: duplicate localQueue %q", cqIdx, key))
			}

			seenLQKeys[key] = true
		}
	}

	return errs
}

// warnHealthSpec returns a warning message when checkIntervalSeconds is set
// for Device Plugin mode, where it has no effect.
func warnHealthSpec(spec *ClusterPolicySpec) string {
	if spec.ResourceRegistration == "dp" &&
		spec.HealthinessSpec != nil &&
		spec.HealthinessSpec.CheckIntervalSeconds > 0 {
		return "health.checkIntervalSeconds is not supported by Device Plugin and will be ignored"
	}

	return ""
}

// ValidateCreate implements webhook.CustomValidator.
func (v *ClusterPolicyCustomValidator) ValidateCreate(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	cp, ok := obj.(*ClusterPolicy)
	if !ok {
		return nil, fmt.Errorf("expected a ClusterPolicy object")
	}

	return validateClusterPolicySpec(&cp.Spec)
}

// ValidateUpdate implements webhook.CustomValidator.
func (v *ClusterPolicyCustomValidator) ValidateUpdate(_ context.Context, _, newObj runtime.Object) (admission.Warnings, error) {
	cp, ok := newObj.(*ClusterPolicy)
	if !ok {
		return nil, fmt.Errorf("expected a ClusterPolicy object")
	}

	return validateClusterPolicySpec(&cp.Spec)
}

// ValidateDelete implements webhook.CustomValidator.
func (v *ClusterPolicyCustomValidator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}
