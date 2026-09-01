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
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/distribution/reference"
)

const (
	approvalPrefix = "app-"
)

// nolint:unused
// log is for logging in this package.
var gpurecoveryplanlog = logf.Log.WithName("gpurecoveryplan-resource")

// SetupGPURecoveryPlanWebhookWithManager registers the webhook for GPURecoveryPlan in the manager.
func SetupGPURecoveryPlanWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &GPURecoveryPlan{}).
		WithValidator(&GPURecoveryPlanCustomValidator{}).
		WithDefaulter(&GPURecoveryPlanCustomDefaulter{}).
		Complete()
}

// +kubebuilder:webhook:path=/mutate-intel-com-v1alpha1-gpurecoveryplan,mutating=true,failurePolicy=fail,sideEffects=None,groups=intel.com,resources=gpurecoveryplans,verbs=create;update,versions=v1alpha1,name=mgpurecoveryplan-v1alpha1.kb.io,admissionReviewVersions=v1

// GPURecoveryPlanCustomDefaulter struct is responsible for setting default values on the custom resource of the
// Kind GPURecoveryPlan when those are created or updated.
type GPURecoveryPlanCustomDefaulter struct{}

var _ admission.Defaulter[*GPURecoveryPlan] = &GPURecoveryPlanCustomDefaulter{}

// Default implements webhook.CustomDefaulter so a webhook will be registered for the Kind GPURecoveryPlan.
// It generates IDs for any spec.approvals entries that are missing one.
func (d *GPURecoveryPlanCustomDefaulter) Default(_ context.Context, plan *GPURecoveryPlan) error {
	if plan == nil {
		return fmt.Errorf("expected a GPURecoveryPlan object but got nil")
	}

	gpurecoveryplanlog.Info("Defaulting for GPURecoveryPlan", "name", plan.GetName())

	presentIDs := make(map[string]bool)

	for i := range plan.Spec.Approvals {
		if plan.Spec.Approvals[i].ID != "" {
			presentIDs[plan.Spec.Approvals[i].ID] = true
		}
	}

	for i := range plan.Spec.Approvals {
		if plan.Spec.Approvals[i].ID == "" {
			id, err := generateApprovalID(presentIDs)
			if err != nil {
				return fmt.Errorf("failed to generate approval ID: %w", err)
			}

			plan.Spec.Approvals[i].ID = id
		}
	}

	if plan.Spec.XpuSmi.PullPolicy == "" {
		plan.Spec.XpuSmi.PullPolicy = "IfNotPresent"
	}

	return nil
}

// generateApprovalID returns a random ID in the form "app-XXXXXXXX" (8 random hex chars).
func generateApprovalID(presentIDs map[string]bool) (string, error) {
	// Try up to 10 times to generate a unique ID.
	for i := 0; i < 10; i++ {
		b := make([]byte, 4)
		if _, err := rand.Read(b); err != nil {
			return "", err
		}

		// Create an ID and check if it already exists in the presentIDs map.
		// If it does, generate a new one.
		id := approvalPrefix + hex.EncodeToString(b)
		if presentIDs[id] {
			continue
		}

		presentIDs[id] = true

		return id, nil
	}

	return "", fmt.Errorf("failed to generate a unique approval ID after 10 attempts")
}

// +kubebuilder:webhook:path=/validate-intel-com-v1alpha1-gpurecoveryplan,mutating=false,failurePolicy=fail,sideEffects=None,groups=intel.com,resources=gpurecoveryplans,verbs=create;update,versions=v1alpha1,name=vgpurecoveryplan-v1alpha1.kb.io,admissionReviewVersions=v1

// GPURecoveryPlanCustomValidator struct is responsible for validating the GPURecoveryPlan resource
// when it is created, updated, or deleted.
type GPURecoveryPlanCustomValidator struct{}

var _ admission.Validator[*GPURecoveryPlan] = &GPURecoveryPlanCustomValidator{}

var pciIDPattern = regexp.MustCompile(`^0x[0-9a-fA-F]{4}$`)

// firmwareFileNamePattern is the allow-list for spec.firmware.file. Deliberately an
// allow-list and not a deny-list of shell metacharacters: the name ends up in a command line
// inside a privileged root container, where anything unanticipated is worse than a rejected CR.
var firmwareFileNamePattern = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// ValidateCreate implements webhook.CustomValidator so a webhook will be registered for the type GPURecoveryPlan.
func (v *GPURecoveryPlanCustomValidator) ValidateCreate(_ context.Context, plan *GPURecoveryPlan) (admission.Warnings, error) {
	if plan == nil {
		return nil, fmt.Errorf("expected a GPURecoveryPlan object but got nil")
	}

	gpurecoveryplanlog.Info("Validation for GPURecoveryPlan upon creation", "name", plan.GetName())

	return nil, validateRecoveryPlanSpec(&plan.Spec)
}

// ValidateUpdate implements webhook.CustomValidator so a webhook will be registered for the type GPURecoveryPlan.
// It also prevents firmware changes while any reflash event is in-progress.
func (v *GPURecoveryPlanCustomValidator) ValidateUpdate(_ context.Context, oldPlan, newPlan *GPURecoveryPlan) (admission.Warnings, error) {
	if oldPlan == nil || newPlan == nil {
		return nil, fmt.Errorf("expected GPURecoveryPlan objects but got nil")
	}

	gpurecoveryplanlog.Info("Validation for GPURecoveryPlan upon update", "name", newPlan.GetName())

	if err := validateRecoveryPlanSpec(&newPlan.Spec); err != nil {
		return nil, err
	}

	if firmwareUpdateActive(oldPlan) && !reflect.DeepEqual(oldPlan.Spec.Firmware, newPlan.Spec.Firmware) {
		return nil, fmt.Errorf(
			"spec.firmware is immutable while a reflash event is in-progress or blocked")
	}

	return nil, nil
}

// ValidateDelete implements webhook.CustomValidator so a webhook will be registered for the type GPURecoveryPlan.
func (v *GPURecoveryPlanCustomValidator) ValidateDelete(_ context.Context, plan *GPURecoveryPlan) (admission.Warnings, error) {
	if plan == nil {
		return nil, fmt.Errorf("expected a GPURecoveryPlan object but got nil")
	}

	gpurecoveryplanlog.Info("Validation for GPURecoveryPlan upon deletion", "name", plan.GetName())

	return nil, nil
}

// validateRecoveryPlanSpec validates the spec fields of a GPURecoveryPlan.
func validateRecoveryPlanSpec(spec *GPURecoveryPlanSpec) error {
	if spec.DeviceID == "" {
		return fmt.Errorf("spec.deviceId is required")
	}

	if !pciIDPattern.MatchString(spec.DeviceID) {
		return fmt.Errorf("spec.deviceId %q must match pattern 0x[0-9a-fA-F]{4}", spec.DeviceID)
	}

	// Mandatory, and restricted to the two platform-selected resets.
	if spec.DefaultResetType == "" {
		return fmt.Errorf("spec.defaultResetType is required: %q where the PCIe slots support hot-plug, %q otherwise",
			RecoveryTypeSlot, RecoveryTypeAMC)
	}

	if spec.DefaultResetType != RecoveryTypeSlot && spec.DefaultResetType != RecoveryTypeAMC {
		return fmt.Errorf("spec.defaultResetType %q is not a platform reset; use %q or %q, and "+
			"spec.approvals[].override to run %q on a single event",
			spec.DefaultResetType, RecoveryTypeSlot, RecoveryTypeAMC, RecoveryTypeSBR)
	}

	if spec.SubDeviceID != "" && !pciIDPattern.MatchString(spec.SubDeviceID) {
		return fmt.Errorf("spec.subDeviceId %q must match pattern 0x[0-9a-fA-F]{4}", spec.SubDeviceID)
	}

	if spec.SubVendorID != "" && !pciIDPattern.MatchString(spec.SubVendorID) {
		return fmt.Errorf("spec.subVendorId %q must match pattern 0x[0-9a-fA-F]{4}", spec.SubVendorID)
	}

	if err := validateApprovals(spec.Approvals); err != nil {
		return err
	}

	if err := validateDrain(&spec.Drain); err != nil {
		return err
	}

	if spec.XpuSmi.Image != "" {
		if _, err := reference.ParseAnyReference(spec.XpuSmi.Image); err != nil {
			return fmt.Errorf("spec.xpuSmi.image %q is not a valid image reference: %w", spec.XpuSmi.Image, err)
		}
	}

	if spec.Firmware != nil {
		if err := validateFirmware(spec.Firmware); err != nil {
			return err
		}
	}

	return nil
}

// validateApprovals checks that each approval entry is internally consistent.
func validateApprovals(approvals []RecoveryApproval) error {
	seenIDs := make(map[string]bool)

	for i, a := range approvals {
		if a.ID != "" {
			if seenIDs[a.ID] {
				return fmt.Errorf("spec.approvals[%d]: duplicate approval ID %q", i, a.ID)
			}

			seenIDs[a.ID] = true
		}

		hasEventID := a.EventID != ""
		hasSelector := a.Selector != nil

		if hasEventID && hasSelector {
			return fmt.Errorf("spec.approvals[%d]: eventId and selector are mutually exclusive", i)
		}

		if !hasEventID && !hasSelector {
			return fmt.Errorf("spec.approvals[%d]: one of eventId or selector must be set", i)
		}

		if a.Persistent && !hasSelector {
			return fmt.Errorf("spec.approvals[%d]: persistent=true is only valid with a selector", i)
		}

		// Reject label selectors that can never match a real Node. Node label matching
		// uses labels.SelectorFromSet, which does not validate its input, so an invalid
		// key would silently match nothing and the approval would appear to be ignored.
		if hasSelector && len(a.Selector.NodeSelector) > 0 {
			if _, err := labels.ValidatedSelectorFromSet(labels.Set(a.Selector.NodeSelector)); err != nil {
				return fmt.Errorf("spec.approvals[%d].selector.nodeSelector is not a valid label selector: %w", i, err)
			}
		}
	}

	return nil
}

// validateDrain checks the pre-reset drain configuration.
func validateDrain(drain *DrainSpec) error {
	seen := make(map[string]bool, len(drain.NamespacesToSkip))

	for i, ns := range drain.NamespacesToSkip {
		if errs := validation.IsDNS1123Label(ns); len(errs) > 0 {
			return fmt.Errorf("spec.drain.namespacesToSkip[%d] %q is not a valid namespace name: %s",
				i, ns, strings.Join(errs, "; "))
		}

		if seen[ns] {
			return fmt.Errorf("spec.drain.namespacesToSkip[%d]: duplicate namespace %q", i, ns)
		}

		seen[ns] = true
	}

	return nil
}

// validateFirmware validates the reflash firmware spec.
func validateFirmware(fw *FirmwareSpec) error {
	if fw.Source.ContainerSource == nil && fw.Source.VolumeSource == nil {
		return fmt.Errorf("spec.firmware.source: at least one of containerSource or volumeSource must be set")
	}

	if fw.Source.ContainerSource != nil {
		if fw.Source.ContainerSource.Name == "" {
			return fmt.Errorf("spec.firmware.source.containerSource.name must not be empty")
		}

		if _, err := reference.ParseAnyReference(fw.Source.ContainerSource.Name); err != nil {
			return fmt.Errorf("spec.firmware.source.containerSource.name %q is not a valid image reference: %w", fw.Source.ContainerSource.Name, err)
		}
	}

	if fw.Source.VolumeSource != nil && fw.Source.VolumeSource.Name == "" {
		return fmt.Errorf("spec.firmware.source.volumeSource.name must not be empty")
	}

	// The filename is interpolated into the shell command the reflash Job runs, so both checks
	// below are load-bearing rather than cosmetic: a path component would let the flash read
	// outside the firmware mount, and the character allow-list keeps shell metacharacters out of
	// a command running privileged as root.
	if fw.File == "" {
		return fmt.Errorf("spec.firmware.file must not be empty")
	}

	if filepath.Base(fw.File) != fw.File {
		return fmt.Errorf("spec.firmware.file %q must not contain path components", fw.File)
	}

	if !firmwareFileNamePattern.MatchString(fw.File) {
		return fmt.Errorf("spec.firmware.file %q contains invalid characters", fw.File)
	}

	return nil
}

// firmwareUpdateActive returns true when any reflash-type event is blocking
// changes to spec.firmware.
func firmwareUpdateActive(plan *GPURecoveryPlan) bool {
	for _, evt := range plan.Status.Events {
		if !evt.RecoveryType.IsReflash() {
			continue
		}

		switch evt.State {
		case RecoveryEventStateInProgress, RecoveryEventStateBlocked:
			return true
		}
	}

	return false
}
