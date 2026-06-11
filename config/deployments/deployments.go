// Copyright 2025 Intel Corporation. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package deployments

import (
	_ "embed"
	"fmt"

	adreg "k8s.io/api/admissionregistration/v1"
	apps "k8s.io/api/apps/v1"
	batch "k8s.io/api/batch/v1"
	core "k8s.io/api/core/v1"
	rbac "k8s.io/api/rbac/v1"

	prometheusv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	resv1 "k8s.io/api/resource/v1"
	nfdcrd "sigs.k8s.io/node-feature-discovery/api/nfd/v1alpha1"

	"sigs.k8s.io/yaml"
)

const (
	vfioExpression   = `device.attributes["gpu.intel.com"].driver == 'vfio-pci'`
	xeVfioExpression = `device.attributes["gpu.intel.com"].driver == 'xe-vfio-pci'`
)

// XPU Manager

//go:embed xpum/xpum.yaml
var contentXpumDs []byte

func XpuManagerDaemonset() *apps.DaemonSet {
	return getDaemonset(contentXpumDs).DeepCopy()
}

//go:embed xpum/otel-config.yaml
var contentXpumOTelConfig []byte

func XpuManagerOTelConfig() *OTelConfig {
	return getOTelConfig(contentXpumOTelConfig)
}

// Device Plugin

//go:embed dp/dp.yaml
var contentDPDs []byte

func DevicePluginDaemonset() *apps.DaemonSet {
	return getDaemonset(contentDPDs).DeepCopy()
}

// DRA

//go:embed dra/daemonset.yaml
var contentDRADs []byte

func DynamicResourceAllocationDaemonset() *apps.DaemonSet {
	return getDaemonset(contentDRADs).DeepCopy()
}

//go:embed dra/clusterrole.yaml
var contentDRACR []byte

func DynamicResourceAllocationClusterRole() *rbac.ClusterRole {
	return getClusterRole(contentDRACR).DeepCopy()
}

//go:embed dra/clusterrolebinding.yaml
var contentDRACRB []byte

func DynamicResourceAllocationClusterRoleBinding() *rbac.ClusterRoleBinding {
	return getClusterRoleBinding(contentDRACRB).DeepCopy()
}

//go:embed dra/serviceaccount.yaml
var contentDRASA []byte

func DynamicResourceAllocationServiceAccount() *core.ServiceAccount {
	return getServiceAccount(contentDRASA).DeepCopy()
}

//go:embed dra/device-class.yaml
var contentDRADC []byte

func DynamicResourceAllocationDeviceClass() *resv1.DeviceClass {
	return getDeviceClass(contentDRADC).DeepCopy()
}

//go:embed dra/device-class-vfio.yaml
var contentDRADCVfio []byte

func DynamicResourceAllocationDeviceClassVfio(limitToVfio bool) *resv1.DeviceClass {
	dc := getDeviceClass(contentDRADCVfio).DeepCopy()

	// Limit VFIO device class to only VFIO-bound devices.
	if limitToVfio {
		dc.Spec.Selectors = append(dc.Spec.Selectors, resv1.DeviceSelector{
			CEL: &resv1.CELDeviceSelector{
				Expression: fmt.Sprintf("%s || %s", vfioExpression, xeVfioExpression),
			},
		})
	}

	return dc
}

//go:embed dra/admissionpolicy.yaml
var contentDRAAP []byte

func DynamicResourceAllocationValidatingAdmissionPolicy() *adreg.ValidatingAdmissionPolicy {
	return getAdmissionPolicy(contentDRAAP).DeepCopy()
}

//go:embed dra/admissionpolicybinding.yaml
var contentDRAAPB []byte

func DynamicResourceAllocationValidatingAdmissionPolicyBinding() *adreg.ValidatingAdmissionPolicyBinding {
	return getAdmissionPolicyBinding(contentDRAAPB).DeepCopy()
}

//go:embed dra/monitorclaimtemplate.yaml
var contentDRAMCT []byte

func DynamicResourceAllocationMonitorClaimTemplate() *resv1.ResourceClaimTemplate {
	return getResourceClaimTemplate(contentDRAMCT).DeepCopy()
}

// NFD

//go:embed nfd/node-feature-rules-gpu.yaml
var nfdNodeFeatureRulesGpu []byte

func NFDNodeFeatureRulesGpu() *nfdcrd.NodeFeatureRule {
	return getNodeFeatureRule(nfdNodeFeatureRulesGpu).DeepCopy()
}

// Prometheus

//go:embed prometheus/service-monitor.yaml
var prometheusServiceMonitor []byte

func PrometheusServiceMonitor() *prometheusv1.ServiceMonitor {
	return getServiceMonitor(prometheusServiceMonitor).DeepCopy()
}

//go:embed xpum/service.yaml
var xpumService []byte

func XpuManagerService() *core.Service {
	return getService(xpumService).DeepCopy()
}

//go:embed xpum/xpum-fwupdate-job.yaml
var xpumFWUpdateJob []byte

func XpuManagerFWUpdateJob() *batch.Job {
	return getJob(xpumFWUpdateJob).DeepCopy()
}

// generic functions

func getDaemonset(content []byte) *apps.DaemonSet {
	var result apps.DaemonSet

	err := yaml.Unmarshal(content, &result)
	if err != nil {
		panic(err)
	}

	return &result
}

func getService(content []byte) *core.Service {
	var result core.Service

	err := yaml.Unmarshal(content, &result)
	if err != nil {
		panic(err)
	}

	return &result
}

func getServiceAccount(content []byte) *core.ServiceAccount {
	var result core.ServiceAccount

	err := yaml.Unmarshal(content, &result)
	if err != nil {
		panic(err)
	}

	return &result
}

func getClusterRole(content []byte) *rbac.ClusterRole {
	var result rbac.ClusterRole

	err := yaml.Unmarshal(content, &result)
	if err != nil {
		panic(err)
	}

	return &result
}

func getClusterRoleBinding(content []byte) *rbac.ClusterRoleBinding {
	var result rbac.ClusterRoleBinding

	err := yaml.Unmarshal(content, &result)
	if err != nil {
		panic(err)
	}

	return &result
}

func getAdmissionPolicy(content []byte) *adreg.ValidatingAdmissionPolicy {
	var result adreg.ValidatingAdmissionPolicy

	err := yaml.Unmarshal(content, &result)
	if err != nil {
		panic(err)
	}

	return &result
}

func getAdmissionPolicyBinding(content []byte) *adreg.ValidatingAdmissionPolicyBinding {
	var result adreg.ValidatingAdmissionPolicyBinding

	err := yaml.Unmarshal(content, &result)
	if err != nil {
		panic(err)
	}

	return &result
}

func getDeviceClass(content []byte) *resv1.DeviceClass {
	var result resv1.DeviceClass

	err := yaml.Unmarshal(content, &result)
	if err != nil {
		panic(err)
	}

	return &result
}

func getResourceClaimTemplate(content []byte) *resv1.ResourceClaimTemplate {
	var result resv1.ResourceClaimTemplate

	err := yaml.Unmarshal(content, &result)
	if err != nil {
		panic(err)
	}

	return &result
}

func getNodeFeatureRule(content []byte) *nfdcrd.NodeFeatureRule {
	var result nfdcrd.NodeFeatureRule

	err := yaml.Unmarshal(content, &result)
	if err != nil {
		panic(err)
	}

	return &result
}

func getServiceMonitor(content []byte) *prometheusv1.ServiceMonitor {
	var result prometheusv1.ServiceMonitor

	err := yaml.Unmarshal(content, &result)
	if err != nil {
		panic(err)
	}

	return &result
}

func getJob(content []byte) *batch.Job {
	var result batch.Job

	err := yaml.Unmarshal(content, &result)
	if err != nil {
		panic(err)
	}

	return &result
}

// getOTelConfig parses the embedded otel-config.yaml into an OTelConfig struct.
func getOTelConfig(content []byte) *OTelConfig {
	var result OTelConfig

	err := yaml.Unmarshal(content, &result)
	if err != nil {
		panic(err)
	}

	return &result
}
