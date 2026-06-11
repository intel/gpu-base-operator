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
	"fmt"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/distribution/reference"
)

// SetupGPUFirmwareUpdateWebhookWithManager registers the webhook for GPUFirmwareUpdate in the manager.
func SetupGPUFirmwareUpdateWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &GPUFirmwareUpdate{}).
		WithValidator(&GPUFirmwareUpdateCustomValidator{}).
		Complete()
}

// +kubebuilder:webhook:path=/validate-intel-com-v1alpha1-gpufirmwareupdate,mutating=false,failurePolicy=fail,sideEffects=None,groups=intel.com,resources=gpufirmwareupdates,verbs=create;update,versions=v1alpha1,name=vgpufirmwareupdate-v1alpha1.kb.io,admissionReviewVersions=v1

// GPUFirmwareUpdateCustomValidator struct is responsible for validating the GPUFirmwareUpdate resource
// when it is created, updated, or deleted.
//

// as this struct is used only for temporary operations and does not need to be deeply copied.
type GPUFirmwareUpdateCustomValidator struct {
}

// inProgressStates are states in which the update is actively running and
// spec mutations (other than approval fields) must be rejected.
var inProgressStates = map[string]bool{
	"draining":      true,
	"updating":      true,
	"canary_done":   true,
	"cleanup":       true,
	"error_cleanup": true,
}

var _ admission.Validator[*GPUFirmwareUpdate] = &GPUFirmwareUpdateCustomValidator{}

func validateInputs(spec *GPUFirmwareUpdateSpec) error {
	fileNameReg := regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

	hasChecksum := false

	uniqueFileNames := make(map[string]bool)
	for _, fwFile := range spec.Content.Files {
		if uniqueFileNames[fwFile.FileName] {
			return fmt.Errorf("duplicate firmware file entry: %q", fwFile.FileName)
		}
		uniqueFileNames[fwFile.FileName] = true
	}

	for _, fwFile := range spec.Content.Files {
		if filepath.Base(fwFile.FileName) != fwFile.FileName {
			return fmt.Errorf("content cannot contain firmware file entries with path components: %q", fwFile.FileName)
		}

		if !fileNameReg.MatchString(fwFile.FileName) {
			return fmt.Errorf("invalid firmware filename: %q", fwFile.FileName)
		}

		if fwFile.Checksum != "" {
			hasChecksum = true

			if !strings.HasPrefix(fwFile.Checksum, "sha256:") {
				return fmt.Errorf("unsupported checksum format for file %q: %q (only sha256 is supported)", fwFile.FileName, fwFile.Checksum)
			}

			if len(fwFile.Checksum) != len("sha256:")+64 {
				return fmt.Errorf("invalid sha256 checksum length for file %q: %q", fwFile.FileName, fwFile.Checksum)
			}
		}
	}

	if hasChecksum && !strings.Contains(spec.Content.ContainerImage, "@sha256:") {
		return fmt.Errorf("checksum verification requires a digest-pinned content image (e.g. image@sha256:<digest>), got %q", spec.Content.ContainerImage)
	}

	devIdReg := regexp.MustCompile(`^0x[0-9a-f]{4}$`)
	if !devIdReg.MatchString(spec.PCIDeviceID) {
		return fmt.Errorf("invalid PCI device ID format: %q", spec.PCIDeviceID)
	}

	if spec.UpdaterImage == "" {
		return fmt.Errorf("image must not be empty")
	}

	if _, err := reference.ParseAnyReference(spec.UpdaterImage); err != nil {
		return fmt.Errorf("invalid image reference %q: %w", spec.UpdaterImage, err)
	}

	if spec.Content.ContainerImage == "" {
		return fmt.Errorf("image must not be empty")
	}

	if _, err := reference.ParseAnyReference(spec.Content.ContainerImage); err != nil {
		return fmt.Errorf("invalid image reference %q: %w", spec.Content.ContainerImage, err)
	}

	return nil
}

// ValidateCreate implements webhook.CustomValidator so a webhook will be registered for the type GPUFirmwareUpdate.
func (v *GPUFirmwareUpdateCustomValidator) ValidateCreate(newObj context.Context, newFU *GPUFirmwareUpdate) (admission.Warnings, error) {
	return nil, validateInputs(&newFU.Spec)
}

// ValidateUpdate rejects changes to fields that are immutable once an update is in progress.
func (v *GPUFirmwareUpdateCustomValidator) ValidateUpdate(_ context.Context, oldFU, newFU *GPUFirmwareUpdate) (admission.Warnings, error) {
	if !inProgressStates[oldFU.Status.State] {
		return nil, nil
	}

	type check struct {
		name   string
		oldVal any
		newVal any
	}

	checks := []check{
		{"spec.updateMethod", oldFU.Spec.UpdateMethod, newFU.Spec.UpdateMethod},
		{"spec.nodeSelector", oldFU.Spec.NodeSelector, newFU.Spec.NodeSelector},
		{"spec.updaterImage", oldFU.Spec.UpdaterImage, newFU.Spec.UpdaterImage},
		{"spec.pciDeviceID", oldFU.Spec.PCIDeviceID, newFU.Spec.PCIDeviceID},
		{"spec.content", oldFU.Spec.Content, newFU.Spec.Content},
		{"spec.amcCredentialsSecret", oldFU.Spec.AMCCredentialsSecret, newFU.Spec.AMCCredentialsSecret},
		{"spec.updateTaint", oldFU.Spec.UpdateTaint, newFU.Spec.UpdateTaint},
	}

	for _, c := range checks {
		if !reflect.DeepEqual(c.oldVal, c.newVal) {
			return nil, fmt.Errorf("%s is immutable while update is in state %q", c.name, oldFU.Status.State)
		}
	}

	return nil, validateInputs(&newFU.Spec)
}

// ValidateDelete implements webhook.CustomValidator so a webhook will be registered for the type GPUFirmwareUpdate.
func (v *GPUFirmwareUpdateCustomValidator) ValidateDelete(ctx context.Context, obj *GPUFirmwareUpdate) (admission.Warnings, error) {
	return nil, nil
}
