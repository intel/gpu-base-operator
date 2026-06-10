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

	"k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/distribution/reference"
)

// Default images used when no image is specified in the ClusterPolicy spec.
// These match the pinned images shipped in the Helm chart values.
const (
	DefaultDPImage  = "docker.io/intel/intel-gpu-plugin:0.36.0@sha256:2db679be62b52ac985169084ca711cab6e6c59fe543ab2ddee58163d6f8d29e0"
	DefaultDRAImage = "ghcr.io/intel/intel-resource-drivers-for-kubernetes/intel-gpu-resource-driver:v0.10.0@sha256:746150e64010881dbfdaeb74771703b13cac365a89ee47c4d7499d686ea4163f"
	DefaultXPUImage = "ghcr.io/intel/xpumanager/xpumd:v2.0.0-rc.0@sha256:8f020012f68888314402c0332a53718ace4ade9913476bbd125af89edb760a8b"
)

// SetupClusterPolicyWebhookWithManager registers the webhook for ClusterPolicy in the manager.
func SetupClusterPolicyWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &ClusterPolicy{}).
		WithDefaulter(&ClusterPolicyCustomDefaulter{}).
		WithValidator(&ClusterPolicyCustomValidator{}).
		Complete()
}

// +kubebuilder:webhook:path=/mutate-intel-com-v1alpha1-clusterpolicy,mutating=true,failurePolicy=fail,sideEffects=None,groups=intel.com,resources=clusterpolicies,verbs=create;update,versions=v1alpha1,name=mclusterpolicy-v1alpha1.kb.io,admissionReviewVersions=v1

// ClusterPolicyCustomDefaulter sets default image values on ClusterPolicy resources.
type ClusterPolicyCustomDefaulter struct{}

var _ admission.Defaulter[*ClusterPolicy] = &ClusterPolicyCustomDefaulter{}

// Default implements webhook.CustomDefaulter. It fills in missing image references
// with the pinned known-good images shipped with this version of the operator.
func (d *ClusterPolicyCustomDefaulter) Default(_ context.Context, cp *ClusterPolicy) error {
	spec := &cp.Spec

	if spec.DevicePluginSpec.PluginImage == "" {
		spec.DevicePluginSpec.PluginImage = DefaultDPImage
	}

	if spec.DynamicResourceAllocationSpec.Image == "" {
		spec.DynamicResourceAllocationSpec.Image = DefaultDRAImage
	}

	if spec.XpuManagerSpec.Image == "" {
		spec.XpuManagerSpec.Image = DefaultXPUImage
	}

	if spec.XpuManagerSpec.MonitoringResource == "" {
		spec.XpuManagerSpec.MonitoringResource = "monitoring"
	}

	return nil
}

// +kubebuilder:webhook:path=/validate-intel-com-v1alpha1-clusterpolicy,mutating=false,failurePolicy=fail,sideEffects=None,groups=intel.com,resources=clusterpolicies,verbs=create;update,versions=v1alpha1,name=vclusterpolicy-v1alpha1.kb.io,admissionReviewVersions=v1

// ClusterPolicyCustomValidator validates ClusterPolicy resources on create and update.
type ClusterPolicyCustomValidator struct{}

var _ admission.Validator[*ClusterPolicy] = &ClusterPolicyCustomValidator{}

var pciIDRegexp = regexp.MustCompile(`^0x[0-9a-f]{4}$`)

func validateClusterPolicySpec(spec *ClusterPolicySpec) (admission.Warnings, error) {
	var errs []error
	var warnings admission.Warnings

	errs = append(errs, validateDPSpec(&spec.DevicePluginSpec)...)
	errs = append(errs, validateImageFields(spec)...)
	errs = append(errs, validatePullSecret(spec)...)
	errs = append(errs, validateConfigMapOverride(spec)...)
	errs = append(errs, validateKueueSpec(spec)...)

	if w := warnForSpecProblems(spec); w != "" {
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

// warnForSpecProblems returns a warning message if some old or deprecated option is set.
func warnForSpecProblems(spec *ClusterPolicySpec) string {
	if spec.ResourceRegistration == "dp" && spec.DevicePluginSpec.LevelzeroImage != "" {
		return "dp.levelzero is no longer used and will be ignored."
	}

	return ""
}

// ValidateCreate implements webhook.CustomValidator.
func (v *ClusterPolicyCustomValidator) ValidateCreate(_ context.Context, cp *ClusterPolicy) (admission.Warnings, error) {
	return validateClusterPolicySpec(&cp.Spec)
}

// ValidateUpdate implements webhook.CustomValidator.
func (v *ClusterPolicyCustomValidator) ValidateUpdate(_ context.Context, _ *ClusterPolicy, cp *ClusterPolicy) (admission.Warnings, error) {
	return validateClusterPolicySpec(&cp.Spec)
}

// ValidateDelete implements webhook.CustomValidator.
func (v *ClusterPolicyCustomValidator) ValidateDelete(_ context.Context, _ *ClusterPolicy) (admission.Warnings, error) {
	return nil, nil
}
