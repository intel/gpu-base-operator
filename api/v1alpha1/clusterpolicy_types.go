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

// ClusterPolicySpec defines the desired state of ClusterPolicy.
type ClusterPolicySpec struct {
	// Which type of resource registration is to be used: Device plugin (dp) or Dynamic Resource Allocation (dra).
	// +kubebuilder:validation:Enum=dp;dra
	ResourceRegistration string `json:"resourceRegistration"`

	// To enable resource monitoring via XPU or not. Deploys GPU Plugin or DRA with monitoring enabled and
	// XPU Manager DaemonSet if true.
	ResourceMonitoring bool `json:"resourceMonitoring,omitempty"`

	// Use NFD rule to label nodes.
	UseNFDLabeling bool `json:"useNFDLabeling,omitempty"`

	// Deploy Kubernetes components to integrate with Prometheus.
	PrometheusIntegration bool `json:"prometheusIntegration,omitempty"`

	// Set up Kueue queues for node resources
	// +optional
	EnableKueue bool `json:"enableKueue,omitempty"`

	// Define Kueue queues
	Kueue *KueueQueueSpec `json:"kueue,omitempty"`

	// Enable health monitoring in DP/DRA
	// These values are applied to all the Intel GPU devices in the cluster.
	// Mechanism to monitor the values differ between DP and DRA. DP uses LevelZero API
	// directly, while DRA relies on the health status provided by XPU Manager.
	HealthinessSpec *HealthinessSpec `json:"health,omitempty"`

	// +optional
	DynamicResourceAllocationSpec DynamicResourceAllocationSpec `json:"dra"`
	// +optional
	DevicePluginSpec DevicePluginSpec `json:"dp"`
	// +optional
	XpuManagerSpec XpuManagerSpec `json:"xpu"`

	// Pull secret is shared with all the deployments.
	// +optional
	PullSecret *v1.LocalObjectReference `json:"pullSecret,omitempty"`

	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	Tolerations  []v1.Toleration   `json:"tolerations,omitempty"`

	// LogLevel to overwrite the default log level of the components.
	// +kubebuilder:validation:Range=0:4
	// +kubebuilder:validation:Default=2
	LogLevel int32 `json:"logLevel,omitempty"`
}

// DynamicResourceAllocationSpec defines the desired state of DynamicResourceAllocation.
type DynamicResourceAllocationSpec struct {
	Image string `json:"image,omitempty"`

	LogLevel int32 `json:"logLevel,omitempty"`

	// DeviceTaints controls whether DRA applies taints to the GPU devices if
	// the devices are indicated as unhealthy by the health monitoring.
	DeviceTaints bool `json:"deviceTaints,omitempty"`

	// Enable DRA Pod's health check.
	// +kubebuilder:default=true
	PodHealthCheck bool `json:"podHealthCheck,omitempty"`
}

// HealthinessSpec defines the thresholds for health monitoring.
type HealthinessSpec struct {
	// Not supported by Device Plugin
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=3600
	// +kubebuilder:default:=5
	CheckIntervalSeconds int32 `json:"checkIntervalSeconds,omitempty"`

	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=130
	// +kubebuilder:default:=100
	CoreTemperatureThreshold int32 `json:"coreTemperatureThreshold,omitempty"`

	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=130
	// +kubebuilder:default:=100
	MemoryTemperatureThreshold int32 `json:"memoryTemperatureThreshold,omitempty"`
}

// DevicePluginSpec defines the desired state of DevicePlugin.
type DevicePluginSpec struct {
	// Container image for the GPU plugin
	PluginImage string `json:"plugin,omitempty"`
	// Container image for the Level Zero companion container
	// Deprecated: LevelzeroImage is no longer used and this configuration item will be removed in the future.
	LevelzeroImage string `json:"levelzero,omitempty"`

	// AllowIDs and DenyIDs are used to control which devices are registered as resources.
	// Allow or deny certain PCI Device IDs. Both cannot be used together. Format is '0xabcd'.
	AllowIDs []string `json:"allowIDs,omitempty"`
	DenyIDs  []string `json:"denyIDs,omitempty"`

	// ByPathMode controls DRI by-path entries are exposed by the plugin.
	// +kubebuilder:validation:Enum=single;all;none
	ByPathMode string `json:"byPathMode,omitempty"`

	// +kubebuilder:validation:Range=0:4
	// +kubebuilder:validation:Default=1
	LogLevel int32 `json:"logLevel,omitempty"`
}

// XpuManagerSpec defines the desired state of XpuManager.
type XpuManagerSpec struct {
	Image string `json:"image,omitempty"`

	// +kubebuilder:validation:Range=0:3
	// +kubebuilder:validation:Default=2
	LogLevel int32 `json:"logLevel,omitempty"`

	// ConfigMapOverride allows overriding the default OpenTelemetry Collector configuration used by XPU Manager.
	// Configmap has to be in the same namespace as the operator and contain a key "config.yaml" with the configuration content.
	// The value should be a YAML string containing the configuration. If not set, a default configuration will be used.
	ConfigMapOverride string `json:"configMapOverride,omitempty"`

	// Set monitoring resource name for Device Plugin use. If not set, the default resource
	// name "gpu.intel.com/monitoring" will be used.
	// +kubebuilder:validation:Enum=i915_monitoring;xe_monitoring;monitoring
	MonitoringResource string `json:"monitoringResource,omitempty"`
}

// ClusterPolicyStatus defines the observed state of ClusterPolicy.
type ClusterPolicyStatus struct {
	DevicePluginStatus string   `json:"devicePluginStatus,omitempty"`
	DRAStatus          string   `json:"draStatus,omitempty"`
	XPUManagerStatus   string   `json:"xpuManagerStatus,omitempty"`
	Errors             []string `json:"errors,omitempty"`
}

// KueueQueueSpec defines Kueue cluster and local queues
type KueueQueueSpec struct {
	// Cluster queues for dividing resources evenly
	EqualResources []ClusterQueueSpec `json:"equalResources"`
}

// ClusterQueueSpec defines a Kueue ClusterQueues
type ClusterQueueSpec struct {
	// Name of the cluster queue
	Name string `json:"name"`
	// List of Kueue LocalQueues to create for this ClusterQueue
	LocalQueues []LocalQueueSpec `json:"localQueues"`
}

// LocalQueueSpec defines a Kueue Local Queue
type LocalQueueSpec struct {
	// Name of the cluster queue
	Name string `json:"name"`
	// Namespace for the local queue
	Namespace string `json:"namespace"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=clusterpolicies,scope=Cluster
// +kubebuilder:printcolumn:name="DP",type=string,JSONPath=`.status.devicePluginStatus`
// +kubebuilder:printcolumn:name="DRA",type=string,JSONPath=`.status.draStatus`
// +kubebuilder:printcolumn:name="XPU",type=string,JSONPath=`.status.xpuManagerStatus`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +operator-sdk:csv:customresourcedefinitions:displayName="Intel GPU Cluster Policy"
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// ClusterPolicy is the Schema for the clusterpolicies API.
type ClusterPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ClusterPolicySpec   `json:"spec,omitempty"`
	Status ClusterPolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ClusterPolicyList contains a list of ClusterPolicy.
type ClusterPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterPolicy `json:"items"`
}
