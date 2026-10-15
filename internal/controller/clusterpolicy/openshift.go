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

package clusterpolicy

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/intel/gpu-base-operator/internal/controller/common"
)

// buildDevicePluginSCC returns the SCC for the GPU device plugin daemonset.
func buildDevicePluginSCC(name string) *unstructured.Unstructured {
	return common.BuildSCC(name, map[string]interface{}{
		"allowPrivilegedContainer": false,
		"allowHostDirVolumePlugin": true,
		"allowHostIPC":             false,
		"allowHostNetwork":         false,
		"allowHostPID":             false,
		"allowHostPorts":           false,
		"allowPrivilegeEscalation": false,
		"allowedCapabilities":      nil,
		"defaultAddCapabilities":   nil,
		"fsGroup":                  map[string]interface{}{"type": "RunAsAny"},
		"readOnlyRootFilesystem":   false,
		"requiredDropCapabilities": []interface{}{"ALL"},
		"runAsUser":                map[string]interface{}{"type": "RunAsAny"},
		"seLinuxContext":           map[string]interface{}{"type": "RunAsAny"},
		"seccompProfiles":          []interface{}{"*"},
		"supplementalGroups":       map[string]interface{}{"type": "RunAsAny"},
		"volumes":                  []interface{}{"hostPath", "emptyDir"},
		"users":                    []interface{}{},
		"groups":                   []interface{}{},
	})
}

// buildXpuManagerSCC returns the SCC for the XPU Manager daemonset.
// The daemonset runs as root and requires SYS_ADMIN capability.
func buildXpuManagerSCC(name string) *unstructured.Unstructured {
	return common.BuildSCC(name, map[string]interface{}{
		"allowPrivilegedContainer": false,
		"allowHostDirVolumePlugin": true,
		"allowHostIPC":             false,
		"allowHostNetwork":         false,
		"allowHostPID":             false,
		"allowHostPorts":           false,
		"allowPrivilegeEscalation": false,
		"allowedCapabilities":      []interface{}{"SYS_ADMIN"},
		"defaultAddCapabilities":   nil,
		"fsGroup":                  map[string]interface{}{"type": "RunAsAny"},
		"readOnlyRootFilesystem":   false,
		"requiredDropCapabilities": []interface{}{"ALL"},
		"runAsUser":                map[string]interface{}{"type": "RunAsAny"},
		"seLinuxContext":           map[string]interface{}{"type": "RunAsAny"},
		"seccompProfiles":          []interface{}{"*"},
		"supplementalGroups":       map[string]interface{}{"type": "RunAsAny"},
		"volumes":                  []interface{}{"hostPath", "configMap", "secret"},
		"users":                    []interface{}{},
		"groups":                   []interface{}{},
	})
}

// buildDRASCC returns the SCC for the GPU DRA driver daemonset.
// The daemonset uses hostPath volumes and mounts a service account token (projected volume).
func buildDRASCC(name string) *unstructured.Unstructured {
	return common.BuildSCC(name, map[string]interface{}{
		"allowPrivilegedContainer": false,
		"allowHostDirVolumePlugin": true,
		"allowHostIPC":             false,
		"allowHostNetwork":         false,
		"allowHostPID":             false,
		"allowHostPorts":           false,
		"allowPrivilegeEscalation": false,
		"allowedCapabilities":      nil,
		"defaultAddCapabilities":   nil,
		"fsGroup":                  map[string]interface{}{"type": "RunAsAny"},
		"readOnlyRootFilesystem":   false,
		"requiredDropCapabilities": []interface{}{"ALL"},
		"runAsUser":                map[string]interface{}{"type": "RunAsAny"},
		"seLinuxContext":           map[string]interface{}{"type": "RunAsAny"},
		"seccompProfiles":          []interface{}{"*"},
		"supplementalGroups":       map[string]interface{}{"type": "RunAsAny"},
		"volumes":                  []interface{}{"hostPath", "projected"},
		"users":                    []interface{}{},
		"groups":                   []interface{}{},
	})
}
