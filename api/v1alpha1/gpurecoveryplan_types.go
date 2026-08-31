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
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RecoveryType represents the type of GPU recovery operation.
// +kubebuilder:validation:Enum=sbr;slot;amc;reflash
type RecoveryType string

// RecoveryEventState represents the lifecycle state of a recovery event.
// +kubebuilder:validation:Enum=waiting-approval;missing-firmware;blocked;draining;in-progress;succeeded;failed
type RecoveryEventState string

// PlanState represents the overall state of the recovery plan.
// +kubebuilder:validation:Enum=idle;error;active
type PlanState string

const (
	// Secondary Bus Reset
	RecoveryTypeSBR RecoveryType = "sbr"
	// Power cycle/cold reset through the PCIe slot, also called the PM reset.
	RecoveryTypeSlot RecoveryType = "slot"
	// Out-of-band reset through the card's AMC (Advanced Management Controller).
	RecoveryTypeAMC RecoveryType = "amc"
	// Reflash to bring a GPU with corrupted FW into an operational state.
	RecoveryTypeReflash RecoveryType = "reflash"

	// RecoveryEventStateWaitingApproval means the event is pending admin approval. This is the first state
	// for an event, but an error in event processing can also return it to this state.
	RecoveryEventStateWaitingApproval RecoveryEventState = "waiting-approval"
	// RecoveryEventStateMissingFirmware means reflash cannot proceed due to missing firmware.
	RecoveryEventStateMissingFirmware RecoveryEventState = "missing-firmware"
	// RecoveryEventStateBlocked means the event is approved but another recovery is already
	// running on the same node, so this one is held back.
	RecoveryEventStateBlocked RecoveryEventState = "blocked"
	// RecoveryEventStateDraining means the event is approved and the node hosting the GPU is
	// being drained before the reset runs. Reset-type recoveries only: a reflash writes
	// firmware without resetting the PCIe bus, so it goes straight to in-progress.
	RecoveryEventStateDraining RecoveryEventState = "draining"
	// RecoveryEventStateInProgress means a recovery Job is currently running.
	RecoveryEventStateInProgress RecoveryEventState = "in-progress"
	// RecoveryEventStateSucceeded means the recovery Job completed successfully.
	RecoveryEventStateSucceeded RecoveryEventState = "succeeded"
	// RecoveryEventStateFailed means the recovery Job failed.
	RecoveryEventStateFailed RecoveryEventState = "failed"

	PlanStateIdle   PlanState = "idle"
	PlanStateError  PlanState = "error"
	PlanStateActive PlanState = "active"
)

// GPURecoveryPlanSpec defines the desired state of GPURecoveryPlan.
type GPURecoveryPlanSpec struct {
	// DeviceID is the mandatory PCI device ID of the target GPU. Format: '0x' followed by 4 hex digits.
	// +kubebuilder:validation:Pattern=`^0x[0-9a-fA-F]{4}$`
	DeviceID string `json:"deviceId"`

	// SubDeviceID is the optional PCI sub-device ID. Format: '0x' followed by 4 hex digits.
	// +kubebuilder:validation:Pattern=`^0x[0-9a-fA-F]{4}$`
	// +optional
	SubDeviceID string `json:"subDeviceId,omitempty"`

	// SubVendorID is the optional PCI sub-vendor ID. Format: '0x' followed by 4 hex digits.
	// +kubebuilder:validation:Pattern=`^0x[0-9a-fA-F]{4}$`
	// +optional
	SubVendorID string `json:"subVendorId,omitempty"`

	// Approvals contains admin-provided authorisations for specific or grouped recovery events.
	// The operator generates an ID for any entry that is missing one.
	// +optional
	Approvals []RecoveryApproval `json:"approvals,omitempty"`

	// XpuSmi configures the container image providing the xpu-smi tool.
	// +kubebuilder:default={pullPolicy: "IfNotPresent"}
	// +optional
	XpuSmi XpuSmiSpec `json:"xpuSmi,omitempty"`

	// DefaultResetType is the reset the operator runs for every reset-type recovery event it
	// creates on this plan. Either "slot" (PCIe slot power cycle, also called the PM reset) or
	// "amc" (out-of-band reset through the card's AMC).
	// +kubebuilder:validation:Enum=slot;amc
	// +kubebuilder:validation:Required
	DefaultResetType RecoveryType `json:"defaultResetType"`

	// Firmware holds configuration for reflash-type recovery operations.
	// These fields are protected: changes are rejected while any reflash event is active.
	// +optional
	Firmware *FirmwareSpec `json:"firmware,omitempty"`

	// Tolerations are added to recovery Job pods on top of the blanket toleration the operator
	// always sets, for cases where a cluster needs an extra entry.
	// +optional
	Tolerations []core.Toleration `json:"tolerations,omitempty"`

	// Drain configures the node drain that precedes a reset-type recovery.
	// +kubebuilder:default={enable: true, timeoutSeconds: 300}
	// +optional
	Drain DrainSpec `json:"drain,omitempty"`

	// Timeouts bounds how long the recovery Jobs themselves may run.
	// +kubebuilder:default={resetSeconds: 300, reflashSeconds: 600}
	// +optional
	Timeouts RecoveryTimeoutsSpec `json:"timeouts,omitempty"`

	// SkipImageVerification disables the pre-flight registry check on the images a recovery Job
	// needs (spec.xpuSmi.image, and the firmware image for a reflash).
	// +optional
	SkipImageVerification bool `json:"skipImageVerification,omitempty"`

	// MaxRetries is the maximum number of times a failed recovery event is automatically
	// re-queued for approval and retried while its device taint persists. Once this limit
	// is reached the event stays in the failed state and requires manual intervention
	// (e.g. delete the event entry or increase MaxRetries). Setting 0 disables automatic
	// retries entirely.
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=0
	MaxRetries int32 `json:"maxRetries"`
}

// DrainSpec configures the node drain a reset-type recovery performs before it touches the
// hardware.
type DrainSpec struct {
	// Enable controls whether the node is drained before a reset.
	// +kubebuilder:default=true
	// +optional
	Enable *bool `json:"enable,omitempty"`

	// TimeoutSeconds bounds how long a reset waits for the node to drain before the event is
	// failed.
	// +kubebuilder:default=300
	// +kubebuilder:validation:Minimum=1
	// +optional
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`

	// NamespacesToSkip lists namespaces the drain leaves alone: their pods are neither evicted
	// nor waited for. Use it for cluster infrastructure that is fine to keep running through a
	// reset and that a whole-node drain would otherwise have to evict.
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MaxLength=63
	// +kubebuilder:validation:items:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +optional
	NamespacesToSkip []string `json:"namespacesToSkip,omitempty"`
}

// Enabled reports whether the pre-reset drain should run. A nil Enable is enabled; see the field.
func (d *DrainSpec) Enabled() bool {
	return d.Enable == nil || *d.Enable
}

// RecoveryTimeoutsSpec bounds the runtime of the recovery Jobs, i.e. the Job's activeDeadlineSeconds.
type RecoveryTimeoutsSpec struct {
	// ResetSeconds bounds a reset-type recovery Job (SBR, slot power cycle, AMC reset).
	// +kubebuilder:default=300
	// +kubebuilder:validation:Minimum=1
	// +optional
	ResetSeconds int32 `json:"resetSeconds,omitempty"`

	// ReflashSeconds bounds a reflash recovery Job.
	// +kubebuilder:default=600
	// +kubebuilder:validation:Minimum=1
	// +optional
	ReflashSeconds int32 `json:"reflashSeconds,omitempty"`
}

// XpuSmiSpec describes the xpu-smi container image every recovery Job runs.
type XpuSmiSpec struct {
	// Image is the container image providing the xpu-smi tool, used for both hardware resets
	// and firmware reflash operations.
	// +optional
	Image string `json:"image,omitempty"`

	// PullPolicy controls the image pull policy for the xpu-smi container.
	// +kubebuilder:validation:Enum=Always;IfNotPresent;Never
	// +kubebuilder:default="IfNotPresent"
	// +optional
	PullPolicy string `json:"pullPolicy,omitempty"`

	// InsecureSkipTLSVerify disables TLS certificate verification when the operator contacts the
	// registry to check that Image resolves. Use it where the registry serves a self-signed or
	// private-CA certificate.
	// +optional
	InsecureSkipTLSVerify bool `json:"insecureSkipTLSVerify,omitempty"`
}

// RecoveryApproval authorises one or more recovery events.
// Exactly one of EventID or Selector must be set.
// An approval is active as long as it exists in the list and Consumed is false.
type RecoveryApproval struct {
	// ID is the unique identifier for this approval entry.
	// The operator generates an ID when this field is empty.
	// +optional
	ID string `json:"id,omitempty"`

	// EventID references a specific event by its status.events[].id.
	// Mutually exclusive with Selector.
	// +optional
	EventID string `json:"eventId,omitempty"`

	// Selector applies this approval to all currently matching events.
	// Mutually exclusive with EventID.
	// +optional
	Selector *ApprovalSelector `json:"selector,omitempty"`

	// Override substitutes a different recovery type than the system recommendation.
	// When set on an EventID approval, the system records the original suggestion in
	// status.events[].recoveryType.suggestedType for audit purposes.
	// +optional
	Override *RecoveryOverride `json:"override,omitempty"`

	// Persistent keeps this approval alive after matched events are processed so that
	// future events matching the Selector are approved automatically.
	// Only meaningful when Selector is set.
	// +optional
	Persistent bool `json:"persistent,omitempty"`

	// Consumed is set to true by the operator after a non-persistent approval has been
	// matched and acted upon. Persistent approvals are never consumed.
	// +optional
	Consumed bool `json:"consumed,omitempty"`

	// Comment is an optional human-readable note about why this approval was granted.
	// +optional
	Comment string `json:"comment,omitempty"`
}

// ApprovalSelector filters recovery events by type, node, or node labels.
type ApprovalSelector struct {
	// RecoveryType selects events whose suggested recovery type matches.
	// +kubebuilder:validation:Enum=sbr;slot;amc;reflash
	// +optional
	RecoveryType RecoveryType `json:"recoveryType,omitempty"`

	// NodeName selects events on a specific node.
	// +optional
	NodeName string `json:"nodeName,omitempty"`

	// NodeSelector selects events on nodes that have all given labels.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
}

// RecoveryOverride allows the admin to escalate or change the suggested recovery type.
type RecoveryOverride struct {
	// RecoveryType is the recovery operation to use instead of the system suggestion.
	// +kubebuilder:validation:Enum=sbr;slot;amc;reflash
	RecoveryType RecoveryType `json:"recoveryType"`
}

// FirmwareSpec holds everything required for a firmware reflash operation.
type FirmwareSpec struct {
	// Source specifies where the firmware file can be found.
	Source FirmwareSource `json:"source"`

	// File is the filename of the FDO firmware image to flash, relative to the root of the
	// source (no path components).
	File string `json:"file"`
}

// FirmwareSource describes where the firmware file is located.
// At least one of ContainerSource or VolumeSource must be set.
type FirmwareSource struct {
	// ContainerSource specifies a container image that holds the firmware file.
	// +optional
	ContainerSource *ContainerFirmwareSource `json:"containerSource,omitempty"`

	// VolumeSource specifies a Kubernetes volume that holds the firmware file.
	//
	// NOT YET SUPPORTED. The field is validated but not acted upon: a reflash event on a plan
	// whose source sets only volumeSource stays in the missing-firmware state, because the
	// reflash Job copies firmware from a container image. Use containerSource until this is
	// implemented.
	// +optional
	VolumeSource *VolumeFirmwareSource `json:"volumeSource,omitempty"`
}

// ContainerFirmwareSource references a container image holding the firmware file.
type ContainerFirmwareSource struct {
	// Name is the container image reference (e.g. registry/image:tag).
	Name string `json:"name"`

	// InsecureSkipTLSVerify disables TLS certificate verification when the operator contacts the
	// registry to verify this image and the firmware file inside it. Use it where the registry
	// serves a self-signed or private-CA certificate.
	// +optional
	InsecureSkipTLSVerify bool `json:"insecureSkipTLSVerify,omitempty"`
}

// VolumeFirmwareSource references a Kubernetes volume holding the firmware file.
// See FirmwareSource.VolumeSource: not yet supported.
type VolumeFirmwareSource struct {
	// Name is the name of the PersistentVolumeClaim.
	Name string `json:"name"`
}

// GPURecoveryPlanStatus defines the observed state of GPURecoveryPlan.
type GPURecoveryPlanStatus struct {
	// State is the overall state of this recovery plan.
	// +optional
	State PlanState `json:"state,omitempty"`

	// Messages contains recent informational or error messages (most recent last, capped at ~50).
	// +optional
	Messages []string `json:"messages,omitempty"`

	// Events is the list of active and recently completed recovery needs (capped at 1000).
	// +optional
	Events []RecoveryEvent `json:"events,omitempty"`
}

// RecoveryEvent represents a single detected GPU recovery need and its lifecycle state.
type RecoveryEvent struct {
	// ID is the operator-assigned unique event identifier (e.g. "evt-a3f2").
	ID string `json:"id"`

	// ApprovalID is the ID of the spec.approvals entry that matched and authorised this event.
	// +optional
	ApprovalID string `json:"approvalId,omitempty"`

	// ApprovalMatchedAt is the timestamp when an approval first matched this event.
	// +optional
	ApprovalMatchedAt *metav1.Time `json:"approvalMatchedAt,omitempty"`

	// NodeName is the Kubernetes node hosting the affected GPU.
	NodeName string `json:"nodeName"`

	// GPUBDF is the PCI Bus:Device.Function address of the affected GPU (e.g. "0000:02:00.0").
	GPUBDF string `json:"gpuBDF"`

	// Reason is the human-readable cause of this event (e.g. "gpu-wedged", "survivability-mode").
	// +optional
	Reason string `json:"reason,omitempty"`

	// RecoveryType describes the recovery operation to perform.
	RecoveryType RecoveryTypeSpec `json:"recoveryType"`

	// State is the current lifecycle state of this recovery event.
	// +kubebuilder:validation:Enum=waiting-approval;missing-firmware;blocked;draining;in-progress;succeeded;failed
	State RecoveryEventState `json:"state"`

	// StateMessage explains, in one sentence, why this event is in the state it is in, where the
	// state alone does not say: which other recovery is holding the node, which spec field carries
	// an image that cannot be pulled, what a timed-out drain was still waiting on, which Job
	// failed and on which attempt, why an approved event is unapproved again.
	// +optional
	StateMessage string `json:"stateMessage,omitempty"`

	// ImageVerifyGeneration is the plan's metadata.generation at the time of the last failed
	// pre-flight image verification for this event, and the reason the event is back in
	// waiting-approval with its approval still in place.
	// The failure itself is reported in StateMessage, which is what an admin reads; this field is
	// only the bookkeeping that keeps the check from repeating. Both are cleared once verification
	// succeeds.
	// +optional
	ImageVerifyGeneration int64 `json:"imageVerifyGeneration,omitempty"`

	// DrainStartedAt is when the node drain for this event began. Reset from nil on each
	// attempt so a retry gets a full drain timeout rather than inheriting the previous one.
	// +optional
	DrainStartedAt *metav1.Time `json:"drainStartedAt,omitempty"`

	// PodsBlockingDrain names the pods still keeping the node from being drained, in
	// "namespace/name" form, so an admin can see what a stalled drain is waiting on without
	// reading operator logs. Capped at a handful of entries.
	// +optional
	PodsBlockingDrain []string `json:"podsBlockingDrain,omitempty"`

	// ClaimsBlockingReset names the ResourceClaims that still reserve this GPU, in
	// "namespace/name" form. A non-empty list means the reset is held back because a workload
	// has not released the device yet.
	// +optional
	ClaimsBlockingReset []string `json:"claimsBlockingReset,omitempty"`

	// JobName is the name of the Kubernetes Job created to execute the recovery, if any.
	// +optional
	JobName string `json:"jobName,omitempty"`

	// PastJobs is the names of all Jobs created for this event across all attempts.
	// Jobs are retained alive until the event is removed (i.e. the device taint clears),
	// so their Pods remain available for diagnostics throughout the event lifecycle.
	// +optional
	PastJobs []string `json:"pastJobs,omitempty"`

	// RetryCount is the number of times this recovery has been retried after failure.
	RetryCount int32 `json:"retryCount"`

	// LastUpdated is the timestamp of the most recent state change for this event.
	LastUpdated *metav1.Time `json:"lastUpdated"`
}

// RecoveryTypeSpec describes which recovery operation to perform, including any
// admin override.
type RecoveryTypeSpec struct {
	// Type is the recovery operation to execute: a reset (SBR, slot power cycle, or
	// AMC reset) or a firmware reflash.
	// +kubebuilder:validation:Enum=sbr;slot;amc;reflash
	Type RecoveryType `json:"type"`

	// SuggestedType is the recovery type originally recommended by the system before
	// any admin override was applied. Recorded for audit purposes.
	// +kubebuilder:validation:Enum=sbr;slot;amc;reflash
	// +optional
	SuggestedType RecoveryType `json:"suggestedType,omitempty"`
}

// IsReflash returns true when this recovery operation is a firmware reflash rather
// than a hardware reset.
func (rt RecoveryTypeSpec) IsReflash() bool {
	return rt.Type == RecoveryTypeReflash
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="DeviceID",type=string,JSONPath=`.spec.deviceId`
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=`.status.state`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// GPURecoveryPlan is the Schema for the gpurecoveryplans API.
type GPURecoveryPlan struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GPURecoveryPlanSpec   `json:"spec,omitempty"`
	Status GPURecoveryPlanStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// GPURecoveryPlanList contains a list of GPURecoveryPlan.
type GPURecoveryPlanList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GPURecoveryPlan `json:"items"`
}
