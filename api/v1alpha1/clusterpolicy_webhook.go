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
	"net/url"
	"path"
	"regexp"
	"strings"
	"unicode"

	"k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/distribution/reference"
)

// Default images used when no image is specified in the ClusterPolicy spec.
// These match the pinned images shipped in the Helm chart values.
const (
	DefaultDPImage  = "docker.io/intel/intel-gpu-plugin:0.37.0@sha256:7001200071db10587fc31184eedda4f5cf291efba737adf85a8c80539f7fa5e1"
	DefaultDRAImage = "ghcr.io/intel/intel-gpu-resource-driver:v0.12.1@sha256:1479956240397c7207c3d38c0d296bd591cd66f26d432452f9a2d75336843776"
	DefaultXPUImage = "ghcr.io/intel/xpumanager/xpumd:v2.3.0@sha256:539009b75c64159ca2202f52b94ae736ab3883c3497d30d65bd3d179357e1859"
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

	if spec.KernelModule != nil && spec.KernelModule.ModuleName == "" {
		spec.KernelModule.ModuleName = "xe"
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
	errs = append(errs, validateKernelModuleSpec(spec)...)
	errs = append(errs, validateCDIPaths(spec)...)

	otlpErrs, otlpWarnings := validateOTLPSpec(spec.XpuManagerSpec.OTLP)
	errs = append(errs, otlpErrs...)
	warnings = append(warnings, otlpWarnings...)

	if w := warnForSpecProblems(spec); w != "" {
		warnings = append(warnings, w)
	}

	return warnings, errors.Join(errs...)
}

func validateCDIPaths(spec *ClusterPolicySpec) []error {
	fields := []struct {
		name  string
		value string
	}{
		{"cdi.staticHostPath", spec.CDI.StaticHostPath},
		{"cdi.dynamicHostPath", spec.CDI.DynamicHostPath},
	}

	var errs []error

	for _, field := range fields {
		if field.value == "" {
			continue
		}

		if !path.IsAbs(field.value) {
			errs = append(errs, fmt.Errorf("%s must be an absolute path", field.name))
		}

		for _, component := range strings.Split(field.value, "/") {
			if component == ".." {
				errs = append(errs, fmt.Errorf("%s must not contain '..' path components", field.name))
				break
			}
		}

		if strings.ContainsFunc(field.value, unicode.IsControl) {
			errs = append(errs, fmt.Errorf("%s must not contain control characters", field.name))
		}
	}

	return errs
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

func validateObjectName(field, name string) error {
	if msgs := validation.IsDNS1123Subdomain(name); len(msgs) > 0 {
		return fmt.Errorf("%s %q is not a valid Kubernetes object name: %s", field, name, strings.Join(msgs, "; "))
	}

	return nil
}

func validateObjectKey(field, key string) error {
	if msgs := validation.IsConfigMapKey(key); len(msgs) > 0 {
		return fmt.Errorf("%s %q is not a valid key: %s", field, key, strings.Join(msgs, "; "))
	}

	return nil
}

func validateOTLPSpec(otlp *OTLPSpec) ([]error, admission.Warnings) {
	if otlp == nil {
		return nil, nil
	}

	switch {
	case otlp.HTTP != nil && otlp.GRPC != nil:
		return []error{fmt.Errorf("xpu.otlp: only one of http or grpc can be set")}, nil
	case otlp.HTTP != nil:
		return validateOTLPExporterSpec("xpu.otlp.http", otlp.HTTP, false)
	case otlp.GRPC != nil:
		return validateOTLPExporterSpec("xpu.otlp.grpc", otlp.GRPC, true)
	default:
		return []error{fmt.Errorf("xpu.otlp: one of http or grpc has to be set")}, nil
	}
}

var grpcMetadataKeyRegexp = regexp.MustCompile(`^[0-9a-z_.-]+$`)

// grpcTransportHeaders are set by the gRPC client itself or are forbidden by HTTP/2, so a
// user supplied value is overwritten, dropped or makes the request fail.
var grpcTransportHeaders = map[string]bool{
	"content-type":      true,
	"te":                true,
	"user-agent":        true,
	"host":              true,
	"connection":        true,
	"keep-alive":        true,
	"proxy-connection":  true,
	"transfer-encoding": true,
	"upgrade":           true,
}

// validateGRPCMetadataKey checks a header name against the gRPC metadata rules, which are
// stricter than the HTTP header token syntax enforced by the CRD schema. gRPC lowercases the
// keys and only allows digits, lowercase letters, '_', '-' and '.' in them. The "grpc-" prefix
// is reserved for gRPC and the "-bin" suffix marks binary values, which the Secret value isn't.
func validateGRPCMetadataKey(field, name string) (string, error) {
	lower := strings.ToLower(name)

	switch {
	case !grpcMetadataKeyRegexp.MatchString(lower):
		return "", fmt.Errorf("%s %q is not a valid gRPC metadata key: only letters, digits, '_', '-' and '.' are allowed", field, name)
	case strings.HasPrefix(lower, "grpc-"):
		return "", fmt.Errorf("%s %q: the grpc- prefix is reserved for gRPC", field, name)
	case strings.HasSuffix(lower, "-bin"):
		return "", fmt.Errorf("%s %q: the -bin suffix is reserved for binary gRPC metadata", field, name)
	case grpcTransportHeaders[lower]:
		return fmt.Sprintf("%s %q is a transport header managed by the gRPC client, the configured value may be overwritten or dropped.", field, name), nil
	}

	return "", nil
}

func validateOTLPExporterSpec(prefix string, exp *OTLPExporterSpec, grpc bool) ([]error, admission.Warnings) {
	var errs []error
	var warnings admission.Warnings

	u, err := url.Parse(exp.Endpoint)

	switch {
	case err != nil:
		errs = append(errs, fmt.Errorf("%s.endpoint %q is not a valid URL: %w", prefix, exp.Endpoint, err))
	case u.Scheme != "http" && u.Scheme != "https":
		errs = append(errs, fmt.Errorf("%s.endpoint %q has to use the http:// or https:// scheme", prefix, exp.Endpoint))
	case u.Host == "":
		errs = append(errs, fmt.Errorf("%s.endpoint %q is missing the host", prefix, exp.Endpoint))
	case u.User != nil:
		// Don't echo the endpoint back, it contains the credentials.
		errs = append(errs, fmt.Errorf("%s.endpoint cannot contain user info, use headers with Secret references for authentication", prefix))
	}

	plainText := err == nil && u.Scheme == "http"

	seen := map[string]bool{}

	for i, h := range exp.Headers {
		field := fmt.Sprintf("%s.headers[%d]", prefix, i)

		if h.Name == "" {
			errs = append(errs, fmt.Errorf("%s.name cannot be empty", field))
		} else if lower := strings.ToLower(h.Name); seen[lower] {
			errs = append(errs, fmt.Errorf("%s.name: duplicate header %q", field, h.Name))
		} else {
			seen[lower] = true
		}

		if grpc && h.Name != "" {
			warning, err := validateGRPCMetadataKey(field+".name", h.Name)
			if err != nil {
				errs = append(errs, err)
			}

			if warning != "" {
				warnings = append(warnings, warning)
			}
		}

		if err := validateObjectName(field+".valueFrom.name", h.ValueFrom.Name); err != nil {
			errs = append(errs, err)
		}

		if err := validateObjectKey(field+".valueFrom.key", h.ValueFrom.Key); err != nil {
			errs = append(errs, err)
		}
	}

	if plainText && len(exp.Headers) > 0 {
		warnings = append(warnings, fmt.Sprintf("%s.endpoint uses plain-text http://, the headers are sent unencrypted.", prefix))
	}

	// If TLS is not configured, return early as there is nothing more to validate.
	if exp.TLS == nil {
		return errs, warnings
	}

	tls := exp.TLS

	if plainText {
		errs = append(errs, fmt.Errorf("%s.tls cannot be set for a plain-text http:// endpoint", prefix))
	}

	if tls.CA != nil {
		if tls.InsecureSkipVerify {
			errs = append(errs, fmt.Errorf("%s.tls.ca and %s.tls.insecureSkipVerify cannot both be set", prefix, prefix))
		}

		if err := validateObjectName(prefix+".tls.ca.name", tls.CA.Name); err != nil {
			errs = append(errs, err)
		}

		if err := validateObjectKey(prefix+".tls.ca.key", tls.CA.Key); err != nil {
			errs = append(errs, err)
		}
	}

	if tls.ClientCertSecret != nil {
		if err := validateObjectName(prefix+".tls.clientCertSecret.name", tls.ClientCertSecret.Name); err != nil {
			errs = append(errs, err)
		}
	}

	if tls.ServerName != "" {
		if msgs := validation.IsDNS1123Subdomain(tls.ServerName); len(msgs) > 0 {
			errs = append(errs, fmt.Errorf("%s.tls.serverName %q is not a valid host name: %s",
				prefix, tls.ServerName, strings.Join(msgs, "; ")))
		}
	}

	if tls.InsecureSkipVerify {
		warnings = append(warnings, fmt.Sprintf("%s.tls.insecureSkipVerify disables the gateway certificate verification, use tls.ca for self-signed certificates instead.", prefix))
	}

	return errs, warnings
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

func validateKernelModuleSpec(spec *ClusterPolicySpec) []error {
	if spec.KernelModule == nil {
		return nil
	}

	km := spec.KernelModule
	var errs []error

	for i, m := range km.KernelMappings {
		prefix := fmt.Sprintf("kernelModule.kernelMappings[%d]", i)
		errs = append(errs, validateKernelMapping(prefix, &m)...)
	}

	if len(km.ModulesLoadingOrder) > 0 {
		errs = append(errs, validateModulesLoadingOrder(km.ModuleName, km.ModulesLoadingOrder)...)
	}

	return errs
}

// kmmTemplateVarRegexp matches KMM's ${VAR} and $VAR template placeholders
// (e.g. ${KERNEL_FULL_VERSION}, $MOD_NAME), which KMM resolves at reconcile time.
var kmmTemplateVarRegexp = regexp.MustCompile(`\$\{[^}]*\}|\$[A-Za-z_][A-Za-z0-9_]*`)

// substituteKMMTemplateVars swaps KMM template placeholders for a tag-safe token
// so the reference can be validated as a normal image reference.
func substituteKMMTemplateVars(image string) string {
	return kmmTemplateVarRegexp.ReplaceAllString(image, "0")
}

func validateImageTagOrDigest(field, image string) error {
	// Validate a copy with template vars resolved; report the original on error.
	resolved := substituteKMMTemplateVars(image)

	if _, err := reference.ParseNormalizedNamed(resolved); err != nil {
		return fmt.Errorf("invalid image reference in %s: %q: %w", field, image, err)
	}

	if !strings.Contains(resolved, ":") && !strings.Contains(resolved, "@") {
		return fmt.Errorf("%s: image %q must include an explicit tag or digest", field, image)
	}

	return nil
}

func validateKernelMapping(prefix string, m *KernelMappingSpec) []error {
	var errs []error

	if m.Regexp == "" {
		errs = append(errs, fmt.Errorf("%s: regexp is required", prefix))
	} else {
		if _, err := regexp.Compile(m.Regexp); err != nil {
			errs = append(errs, fmt.Errorf("%s.regexp: invalid regular expression: %w", prefix, err))
		}
	}

	if m.ContainerImage != "" {
		if err := validateImageTagOrDigest(prefix+".containerImage", m.ContainerImage); err != nil {
			errs = append(errs, err)
		}
	}

	if m.ContainerImage == "" && m.Build == nil {
		errs = append(errs, fmt.Errorf("%s: one of containerImage or build must be set", prefix))
	}

	if m.Build != nil {
		if m.Build.DockerfileConfigMap.Name == "" {
			errs = append(errs, fmt.Errorf("%s.build.dockerfileConfigMap.name is required", prefix))
		}
	}

	return errs
}

func validateModulesLoadingOrder(moduleName string, order []string) []error {
	var errs []error

	if len(order) < 2 {
		errs = append(errs, fmt.Errorf("kernelModule.modulesLoadingOrder must have at least 2 entries"))
	}

	if len(order) > 0 && moduleName != "" && order[0] != moduleName {
		errs = append(errs, fmt.Errorf("kernelModule.modulesLoadingOrder[0] must be moduleName %q", moduleName))
	}

	seen := make(map[string]bool, len(order))
	for i, entry := range order {
		if seen[entry] {
			errs = append(errs, fmt.Errorf("kernelModule.modulesLoadingOrder[%d]: duplicate entry %q", i, entry))
			break
		}
		seen[entry] = true
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
	// Skip spec validation once deletion has started. The controller drops its
	// finalizer with a plain Update, which this webhook sees; rejecting it would
	// leave the policy undeletable if its spec no longer passes current validation.
	if !cp.DeletionTimestamp.IsZero() {
		return nil, nil
	}

	return validateClusterPolicySpec(&cp.Spec)
}

// ValidateDelete implements webhook.CustomValidator.
func (v *ClusterPolicyCustomValidator) ValidateDelete(_ context.Context, _ *ClusterPolicy) (admission.Warnings, error) {
	return nil, nil
}
