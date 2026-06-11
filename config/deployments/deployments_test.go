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
	"testing"

	core "k8s.io/api/core/v1"
)

const (
	allCaps = "ALL"
)

// findContainer returns a pointer to the named container, or nil if not found.
func findContainer(containers []core.Container, name string) *core.Container {
	for i := range containers {
		if containers[i].Name == name {
			return &containers[i]
		}
	}
	return nil
}

// findInitContainer returns a pointer to the named init container, or nil if not found.
func findInitContainer(containers []core.Container, name string) *core.Container {
	for i := range containers {
		if containers[i].Name == name {
			return &containers[i]
		}
	}
	return nil
}

func TestXpuManagerDaemonset(t *testing.T) {
	ds := XpuManagerDaemonset()
	if ds == nil {
		t.Error("XpuManagerDaemonset returned nil")
	}
}

func TestDevicePluginDaemonset(t *testing.T) {
	ds := DevicePluginDaemonset()
	if ds == nil {
		t.Error("DevicePluginDaemonset returned nil")
	}
}

func TestDynamicResourceAllocationDaemonset(t *testing.T) {
	ds := DynamicResourceAllocationDaemonset()
	if ds == nil {
		t.Error("DynamicResourceAllocationDaemonset returned nil")
	}
}

func TestDynamicResourceAllocationClusterRole(t *testing.T) {
	cr := DynamicResourceAllocationClusterRole()
	if cr == nil {
		t.Error("DynamicResourceAllocationClusterRole returned nil")
	}
}

func TestDynamicResourceAllocationClusterRoleBinding(t *testing.T) {
	crb := DynamicResourceAllocationClusterRoleBinding()
	if crb == nil {
		t.Error("DynamicResourceAllocationClusterRoleBinding returned nil")
	}
}

func TestDynamicResourceAllocationServiceAccount(t *testing.T) {
	sa := DynamicResourceAllocationServiceAccount()
	if sa == nil {
		t.Error("DynamicResourceAllocationServiceAccount returned nil")
	}
}

func TestDynamicResourceAllocationDeviceClass(t *testing.T) {
	dc := DynamicResourceAllocationDeviceClass()
	if dc == nil {
		t.Error("DynamicResourceAllocationDeviceClass returned nil")
	}
}

func TestDynamicResourceAllocationDeviceClassVfio(t *testing.T) {
	dc := DynamicResourceAllocationDeviceClassVfio(false)
	if dc == nil {
		t.Error("DynamicResourceAllocationDeviceClassVfio returned nil")
	}
}

func TestDynamicResourceAllocationValidatingAdmissionPolicy(t *testing.T) {
	ap := DynamicResourceAllocationValidatingAdmissionPolicy()
	if ap == nil {
		t.Error("DynamicResourceAllocationValidatingAdmissionPolicy returned nil")
	}
}

func TestDynamicResourceAllocationValidatingAdmissionPolicyBinding(t *testing.T) {
	apb := DynamicResourceAllocationValidatingAdmissionPolicyBinding()
	if apb == nil {
		t.Error("DynamicResourceAllocationValidatingAdmissionPolicyBinding returned nil")
	}
}

func TestDynamicResourceAllocationMonitorClaimTemplate(t *testing.T) {
	mct := DynamicResourceAllocationMonitorClaimTemplate()
	if mct == nil {
		t.Error("DynamicResourceAllocationMonitorClaimTemplate returned nil")
	}
}

func TestNFDNodeFeatureRulesGpu(t *testing.T) {
	rule := NFDNodeFeatureRulesGpu()
	if rule == nil {
		t.Error("NFDNodeFeatureRulesGpu returned nil")
	}
}

func TestPrometheusServiceMonitor(t *testing.T) {
	sm := PrometheusServiceMonitor()
	if sm == nil {
		t.Error("PrometheusServiceMonitor returned nil")
	}
}

func TestXpuManagerService(t *testing.T) {
	svc := XpuManagerService()
	if svc == nil {
		t.Error("XpuManagerService returned nil")
	}
}

func TestXpuFwUpdateJob(t *testing.T) {
	job := XpuManagerFWUpdateJob()
	if job == nil {
		t.Error("XpuManagerFWUpdateJob returned nil")
	}
}

func TestOTelConfig(t *testing.T) {
	cfg := XpuManagerOTelConfig()
	if cfg == nil {
		t.Error("XpuManagerOTelConfig returned nil")
	}
}

func TestDevicePluginDaemonset_AutomountServiceAccountToken(t *testing.T) {
	ds := DevicePluginDaemonset()
	if ds.Spec.Template.Spec.AutomountServiceAccountToken == nil {
		t.Fatal("automountServiceAccountToken must be set (non-nil)")
	}
	if *ds.Spec.Template.Spec.AutomountServiceAccountToken != false {
		t.Error("automountServiceAccountToken must be false")
	}
}

func TestDynamicResourceAllocationDaemonset_KubeletPluginAllowPrivilegeEscalation(t *testing.T) {
	ds := DynamicResourceAllocationDaemonset()
	c := findContainer(ds.Spec.Template.Spec.Containers, "kubelet-plugin")
	if c == nil {
		t.Fatal("kubelet-plugin container not found")
	}
	if c.SecurityContext == nil {
		t.Fatal("SecurityContext must be set on kubelet-plugin")
	}
	if c.SecurityContext.AllowPrivilegeEscalation == nil {
		t.Fatal("AllowPrivilegeEscalation must be set (non-nil) on kubelet-plugin")
	}
	if *c.SecurityContext.AllowPrivilegeEscalation != false {
		t.Error("AllowPrivilegeEscalation must be false on kubelet-plugin")
	}
}

func TestXpuManagerDaemonset_AutomountServiceAccountToken(t *testing.T) {
	ds := XpuManagerDaemonset()
	if ds.Spec.Template.Spec.AutomountServiceAccountToken == nil {
		t.Fatal("automountServiceAccountToken must be set (non-nil)")
	}
	if *ds.Spec.Template.Spec.AutomountServiceAccountToken != false {
		t.Error("automountServiceAccountToken must be false")
	}
}

func TestXpuFwUpdateJob_AutomountServiceAccountToken(t *testing.T) {
	job := XpuManagerFWUpdateJob()
	if job.Spec.Template.Spec.AutomountServiceAccountToken == nil {
		t.Fatal("automountServiceAccountToken must be set (non-nil)")
	}
	if *job.Spec.Template.Spec.AutomountServiceAccountToken != false {
		t.Error("automountServiceAccountToken must be false")
	}
}

func TestXpuFwUpdateJob_FwCopyInitContainerSecurityContext(t *testing.T) {
	job := XpuManagerFWUpdateJob()
	c := findInitContainer(job.Spec.Template.Spec.InitContainers, "fw-copy")
	if c == nil {
		t.Fatal("fw-copy init container not found")
	}
	if c.SecurityContext == nil {
		t.Fatal("SecurityContext must be set on fw-copy")
	}
	if c.SecurityContext.AllowPrivilegeEscalation == nil {
		t.Fatal("AllowPrivilegeEscalation must be set (non-nil) on fw-copy")
	}
	if *c.SecurityContext.AllowPrivilegeEscalation != false {
		t.Error("AllowPrivilegeEscalation must be false on fw-copy")
	}
	if c.SecurityContext.Capabilities == nil {
		t.Fatal("Capabilities must be set on fw-copy")
	}
	found := false
	for _, cap := range c.SecurityContext.Capabilities.Drop {
		if cap == allCaps {
			found = true
			break
		}
	}
	if !found {
		t.Error("capabilities.drop must contain ALL on fw-copy")
	}
	if c.SecurityContext.SeccompProfile == nil {
		t.Fatal("SeccompProfile must be set on fw-copy")
	}
	if c.SecurityContext.SeccompProfile.Type != core.SeccompProfileTypeRuntimeDefault {
		t.Errorf("SeccompProfile.Type: got %v, want RuntimeDefault on fw-copy", c.SecurityContext.SeccompProfile.Type)
	}
}

func TestXpuFwUpdateJob_UpdaterContainerSecurityContext(t *testing.T) {
	job := XpuManagerFWUpdateJob()
	c := findContainer(job.Spec.Template.Spec.Containers, "updater")
	if c == nil {
		t.Fatal("updater container not found")
	}
	if c.SecurityContext == nil {
		t.Fatal("SecurityContext must be set on updater")
	}
	if c.SecurityContext.SeccompProfile == nil {
		t.Fatal("SeccompProfile must be set on updater")
	}
	if c.SecurityContext.SeccompProfile.Type != core.SeccompProfileTypeRuntimeDefault {
		t.Errorf("SeccompProfile.Type: got %v, want RuntimeDefault on updater", c.SecurityContext.SeccompProfile.Type)
	}
	if c.SecurityContext.Capabilities == nil {
		t.Fatal("Capabilities must be set on updater")
	}
	found := false
	for _, cap := range c.SecurityContext.Capabilities.Drop {
		if cap == allCaps {
			found = true
			break
		}
	}
	if !found {
		t.Error("capabilities.drop must contain ALL on updater")
	}
	if c.SecurityContext.ReadOnlyRootFilesystem == nil {
		t.Fatal("ReadOnlyRootFilesystem must be set (non-nil) on updater")
	}
	if !*c.SecurityContext.ReadOnlyRootFilesystem {
		t.Error("ReadOnlyRootFilesystem must be true on updater")
	}
}

func TestDevicePluginDaemonset_PluginContainerSecurityContext(t *testing.T) {
	ds := DevicePluginDaemonset()
	c := findContainer(ds.Spec.Template.Spec.Containers, "intel-gpu-plugin")
	if c == nil {
		t.Fatal("intel-gpu-plugin container not found")
	}
	if c.SecurityContext == nil {
		t.Fatal("SecurityContext must be set on intel-gpu-plugin")
	}
	if c.SecurityContext.AllowPrivilegeEscalation == nil {
		t.Fatal("AllowPrivilegeEscalation must be set (non-nil) on intel-gpu-plugin")
	}
	if *c.SecurityContext.AllowPrivilegeEscalation {
		t.Error("AllowPrivilegeEscalation must be false on intel-gpu-plugin")
	}
	if c.SecurityContext.ReadOnlyRootFilesystem == nil {
		t.Fatal("ReadOnlyRootFilesystem must be set (non-nil) on intel-gpu-plugin")
	}
	if !*c.SecurityContext.ReadOnlyRootFilesystem {
		t.Error("ReadOnlyRootFilesystem must be true on intel-gpu-plugin")
	}
	if c.SecurityContext.Capabilities == nil {
		t.Fatal("Capabilities must be set on intel-gpu-plugin")
	}
	found := false
	for _, cap := range c.SecurityContext.Capabilities.Drop {
		if cap == allCaps {
			found = true
			break
		}
	}
	if !found {
		t.Error("capabilities.drop must contain ALL on intel-gpu-plugin")
	}
	if c.SecurityContext.SeccompProfile == nil {
		t.Fatal("SeccompProfile must be set on intel-gpu-plugin")
	}
	if c.SecurityContext.SeccompProfile.Type != core.SeccompProfileTypeRuntimeDefault {
		t.Errorf("SeccompProfile.Type: got %v, want RuntimeDefault on intel-gpu-plugin",
			c.SecurityContext.SeccompProfile.Type)
	}
}

func TestDynamicResourceAllocationDaemonset_KubeletPluginFullSecurityContext(t *testing.T) {
	ds := DynamicResourceAllocationDaemonset()
	c := findContainer(ds.Spec.Template.Spec.Containers, "kubelet-plugin")
	if c == nil {
		t.Fatal("kubelet-plugin container not found")
	}
	if c.SecurityContext == nil {
		t.Fatal("SecurityContext must be set on kubelet-plugin")
	}
	if c.SecurityContext.ReadOnlyRootFilesystem == nil {
		t.Fatal("ReadOnlyRootFilesystem must be set (non-nil) on kubelet-plugin")
	}
	if !*c.SecurityContext.ReadOnlyRootFilesystem {
		t.Error("ReadOnlyRootFilesystem must be true on kubelet-plugin")
	}
	if c.SecurityContext.Capabilities == nil {
		t.Fatal("Capabilities must be set on kubelet-plugin")
	}
	found := false
	for _, cap := range c.SecurityContext.Capabilities.Drop {
		if cap == allCaps {
			found = true
			break
		}
	}
	if !found {
		t.Error("capabilities.drop must contain ALL on kubelet-plugin")
	}
	if c.SecurityContext.SeccompProfile == nil {
		t.Fatal("SeccompProfile must be set on kubelet-plugin")
	}
	if c.SecurityContext.SeccompProfile.Type != core.SeccompProfileTypeRuntimeDefault {
		t.Errorf("SeccompProfile.Type: got %v, want RuntimeDefault on kubelet-plugin", c.SecurityContext.SeccompProfile.Type)
	}
}

func TestXpuManagerDaemonset_XpumdContainerSecurityContext(t *testing.T) {
	ds := XpuManagerDaemonset()
	c := findContainer(ds.Spec.Template.Spec.Containers, "xpumd")
	if c == nil {
		t.Fatal("xpumd container not found")
	}
	if c.SecurityContext == nil {
		t.Fatal("SecurityContext must be set on xpumd")
	}
	if c.SecurityContext.AllowPrivilegeEscalation == nil {
		t.Fatal("AllowPrivilegeEscalation must be set (non-nil) on xpumd")
	}
	if *c.SecurityContext.AllowPrivilegeEscalation {
		t.Error("AllowPrivilegeEscalation must be false on xpumd")
	}
	if c.SecurityContext.ReadOnlyRootFilesystem == nil {
		t.Fatal("ReadOnlyRootFilesystem must be set (non-nil) on xpumd")
	}
	if !*c.SecurityContext.ReadOnlyRootFilesystem {
		t.Error("ReadOnlyRootFilesystem must be true on xpumd")
	}
	if c.SecurityContext.Capabilities == nil {
		t.Fatal("Capabilities must be set on xpumd")
	}
	foundDrop := false
	for _, cap := range c.SecurityContext.Capabilities.Drop {
		if cap == allCaps {
			foundDrop = true
			break
		}
	}
	if !foundDrop {
		t.Error("capabilities.drop must contain ALL on xpumd")
	}
	foundAdd := false
	for _, cap := range c.SecurityContext.Capabilities.Add {
		if cap == "SYS_ADMIN" {
			foundAdd = true
			break
		}
	}
	if !foundAdd {
		t.Error("capabilities.add must contain SYS_ADMIN on xpumd (required for engine utilization metrics)")
	}
}

func TestDRAClusterRole_NoWildcardsAndNoSecrets(t *testing.T) {
	cr := DynamicResourceAllocationClusterRole()

	for _, rule := range cr.Rules {
		for _, apiGroup := range rule.APIGroups {
			if apiGroup == "*" {
				t.Errorf("DRA ClusterRole rule has wildcard apiGroup: %+v", rule)
			}
		}

		for _, resource := range rule.Resources {
			if resource == "*" {
				t.Errorf("DRA ClusterRole rule has wildcard resource: %+v", rule)
			}

			if resource == "secrets" {
				t.Errorf("DRA ClusterRole must not grant access to secrets: %+v", rule)
			}
		}

		for _, verb := range rule.Verbs {
			if verb == "*" {
				t.Errorf("DRA ClusterRole rule has wildcard verb: %+v", rule)
			}
		}
	}
}
