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

package v1alpha1

import (
	v1 "k8s.io/api/core/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GPUFirmwareUpdateSpec defines the desired state of GPUFirmwareUpdate.
type GPUFirmwareUpdateSpec struct {
	// List of GPU firmwares to be updated.
	Content GPUFirmwareContent `json:"content"`

	// +kubebuilder:validation:Enum=canary;direct
	UpdateMethod string `json:"updateMethod"`

	// Updater container to run the firmware update.
	// Should container xpu-smi tool that is used to perform the updates.
	UpdaterImage string `json:"updaterImage"`

	// Pull secret for the updater image and the content image, if needed.
	ImagePullSecret string `json:"imagePullSecret,omitempty"`

	// Target PCI Device ID in case the node has multiple GPU types. Format is '0xabcd'.
	PCIDeviceID string `json:"pciDeviceID,omitempty"`

	// Taint key to be applied to nodes during firmware update.
	// Taint value is always NoSchedule.
	// +kubebuilder:default=gpufirmware-update
	UpdateTaint string `json:"updateTaint"`

	// Tolerations needed to be scheduled to the node.
	Tolerations []v1.Toleration `json:"tolerations,omitempty"`

	// Node selector to target specific nodes for firmware update.
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// AMCCredentialsSecret is the name of a Kubernetes Secret (in the operator namespace)
	// containing 'username' and 'password' keys used for the AMC firmware update method
	// (redfish interface). Required when any firmware file has type AMC.
	// +optional
	AMCCredentialsSecret string `json:"amcCredentialsSecret,omitempty"`

	// InsecureSkipTLSVerify disables TLS certificate verification when the operator
	// contacts the content image registry to check reachability or verify file checksums.
	// Use this when the registry uses a self-signed or private-CA certificate.
	// Note: this flag only affects the operator's pre-verification calls; the actual
	// firmware-update DaemonSet image pull is controlled by the node's container runtime.
	// +optional
	InsecureSkipTLSVerify bool `json:"insecureSkipTLSVerify,omitempty"`

	// HoldAfterCanary, when true, pauses the update in the canary_done state after a
	// successful canary update and waits for HoldAfterCanary to be set to false.
	// When false (default), the full rollout starts automatically.
	// +optional
	HoldAfterCanary bool `json:"holdAfterCanary,omitempty"`
}

// GPUFirmwareUpdateStatus defines the observed state of GPUFirmwareUpdate.
type GPUFirmwareUpdateStatus struct {
	// Indicates the current state of the firmware update process.
	// Moves from "" (not started) -> "draining" -> "updating" -> "cleanup" -> "completed" or "error".
	State string `json:"state"`
	// Messages contains informational or error messages related to the update process.
	Messages []string `json:"messages"`

	// Substatus for nodes.
	NodeInfos GPUFirmwareUpdateSubsetStatus `json:"nodeInfos"`
}

// GPUFirmwareUpdate is the Schema for the gpufirmwareupdates API.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=gpufirmwareupdates,scope=Cluster
type GPUFirmwareUpdate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GPUFirmwareUpdateSpec   `json:"spec,omitempty"`
	Status GPUFirmwareUpdateStatus `json:"status,omitempty"`
}

// GPUFirmwareUpdateList contains a list of GPUFirmwareUpdate.
// +kubebuilder:object:root=true
type GPUFirmwareUpdateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GPUFirmwareUpdate `json:"items"`
}

type GPUFirmwareFile struct {
	// +kubebuilder:validation:Enum=GFX;GFX_DATA;GFX_CODE_DATA;GFX_PSCBIN;AMC;FAN_TABLE;VR_CONFIG;OPROM_CODE;OPROM_DATA
	Type string `json:"type"`
	// Filename of the firmware file without any directories.
	FileName string `json:"filename"`
	// SHA256 checksum of the firmware file. Format: sha256:<64 hex characters>.
	// When set, content.containerImage must be digest-pinned (image@sha256:...).
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	// +optional
	Checksum string `json:"checksum,omitempty"`
}

type GPUFirmwareContent struct {
	// Container image containing the firmware files under /update directory.
	ContainerImage string `json:"containerImage"`

	Files []GPUFirmwareFile `json:"files"`
}

type GPUFirmwareUpdateSubsetStatus struct {
	All       []string `json:"allNodes,omitempty"`
	Pending   []string `json:"pendingNodes,omitempty"`
	Draining  []string `json:"drainingNodes,omitempty"`
	Updating  []string `json:"updatingNodes,omitempty"`
	Completed []string `json:"completedNodes,omitempty"`
	Error     []string `json:"errorNodes,omitempty"`
	Jobs      []string `json:"jobs,omitempty"`
}
