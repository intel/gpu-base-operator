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

	// KernelModule configures out-of-tree kernel module loading via KMM.
	// When set, KMM loads the specified OOT driver module on each node.
	// When nil, the in-tree kernel driver is used.
	// +optional
	KernelModule *KernelModuleSpec `json:"kernelModule,omitempty"`
}

// DynamicResourceAllocationSpec defines the desired state of DynamicResourceAllocation.
type DynamicResourceAllocationSpec struct {
	Image string `json:"image,omitempty"`

	LogLevel int32 `json:"logLevel,omitempty"`

	// Enable DRA Pod's health check.
	// +kubebuilder:default=true
	PodHealthCheck bool `json:"podHealthCheck,omitempty"`

	// Allow DRA plugin to bind/unbind devices from xe/i915 driver to vfio/xe-vfio driver and back.
	// Needed if cluster is supposed to support dynamic switching from drivers. Not needed, if hosts are
	// preconfigured to either target KubeVirt or normal workloads.
	ManageBinding bool `json:"manageBinding,omitempty"`
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

// RegistryTLSSpec configures TLS behavior for accessing container image registries.
type RegistryTLSSpec struct {
	Insecure              bool `json:"insecure,omitempty"`
	InsecureSkipTLSVerify bool `json:"insecureSkipTLSVerify,omitempty"`
}

// KernelModuleSpec configures out-of-tree kernel module loading via KMM.
type KernelModuleSpec struct {
	// ModuleName is the kernel module to load (defaults to "xe").
	// +kubebuilder:default=xe
	ModuleName string `json:"moduleName,omitempty"`

	// Version opts into KMM's ordered upgrade
	// (https://kmm.sigs.k8s.io/documentation/ordered_upgrade) for advanced,
	// low-disruption driver rollouts. When set, KMM loads the module onto a node
	// only once a cluster admin labels that node
	// "kmm.node.kubernetes.io/version-module.<module-namespace>.<module-name>=<version>",
	// letting the admin sequence the upgrade node-by-node and drain GPU workloads
	// first. Nodes without a matching label are left untouched.
	//
	// Most users should leave Version unset and instead upgrade by changing the
	// containerImage of the relevant kernelMappings entry (see ContainerImage),
	// which rolls the new driver out to all selected nodes at once without any
	// per-node label choreography.
	// +optional
	Version string `json:"version,omitempty"`

	// KernelMappings maps kernel version patterns to container images or
	// build specifications. Translates directly to KMM KernelMapping objects.
	// +kubebuilder:validation:MinItems=1
	KernelMappings []KernelMappingSpec `json:"kernelMappings"`

	// ModulesLoadingOrder specifies softdep-style loading order for
	// multi-module drivers. First element must be ModuleName (defaults
	// to "xe"); KMM loads in order and unloads in reverse. Must have
	// >=2 entries if set.
	// +optional
	ModulesLoadingOrder []string `json:"modulesLoadingOrder,omitempty"`

	// FirmwarePath is the in-container path where firmware files are stored.
	// +optional
	FirmwarePath string `json:"firmwarePath,omitempty"`

	// RegistryTLS configures TLS for accessing the module image registry.
	// +optional
	RegistryTLS *RegistryTLSSpec `json:"registryTLS,omitempty"`
}

// KernelMappingSpec maps a kernel version pattern to a container image
// or build specification.
type KernelMappingSpec struct {
	// Regexp is a regular expression matched against node kernel versions.
	// Use anchored patterns (e.g. "^5\\.14\\.0-.*$") for exact matches.
	Regexp string `json:"regexp"`

	// ContainerImage is the full image reference for this kernel version.
	// Required when Build is nil. KMM template vars (e.g. ${KERNEL_FULL_VERSION},
	// $MOD_NAME) are supported and resolved by KMM at reconcile time.
	//
	// Changing ContainerImage is the recommended way to upgrade the driver: KMM
	// rolls the new image out to all selected nodes at once, briefly disrupting
	// GPU workloads as the module reloads.
	// +optional
	ContainerImage string `json:"containerImage,omitempty"`

	// Build configures in-cluster building of the driver image via KMM.
	// When set, KMM builds the image if it doesn't exist in the registry.
	// +optional
	Build *KernelModuleBuildSpec `json:"build,omitempty"`

	// InTreeModulesToRemove lists additional in-tree modules to unload
	// for this mapping. ModuleName is always included automatically.
	// +optional
	InTreeModulesToRemove []string `json:"inTreeModulesToRemove,omitempty"`

	// RegistryTLS overrides parent-level TLS settings for this mapping.
	// +optional
	RegistryTLS *RegistryTLSSpec `json:"registryTLS,omitempty"`
}

// KernelModuleBuildSpec configures in-cluster driver image building.
type KernelModuleBuildSpec struct {
	// DockerfileConfigMap references a ConfigMap containing the Dockerfile.
	DockerfileConfigMap v1.LocalObjectReference `json:"dockerfileConfigMap"`

	// BuildArgs are key-value pairs passed to the image builder.
	// +optional
	BuildArgs []BuildArg `json:"buildArgs,omitempty"`

	// Secrets are made available during the build (e.g., for private
	// source repos). Not for registry auth -- use pullSecret on
	// ClusterPolicySpec.
	// +optional
	Secrets []v1.LocalObjectReference `json:"secrets,omitempty"`
}

// BuildArg is a key-value pair passed as a build argument.
type BuildArg struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// ClusterPolicyStatus defines the observed state of ClusterPolicy.
type ClusterPolicyStatus struct {
	DevicePluginStatus string   `json:"devicePluginStatus,omitempty"`
	DRAStatus          string   `json:"draStatus,omitempty"`
	XPUManagerStatus   string   `json:"xpuManagerStatus,omitempty"`
	KMMStatus          string   `json:"kmmStatus,omitempty"`
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
// +kubebuilder:printcolumn:name="KMM",type=string,JSONPath=`.status.kmmStatus`
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
