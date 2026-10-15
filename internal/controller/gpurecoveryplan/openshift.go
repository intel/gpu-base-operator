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

package gpurecoveryplan

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/intel/gpu-base-operator/internal/controller/common"
)

// buildRecoverySCC returns the SCC for GPURecoveryPlan recovery Job pods. It has to cover both
// Job shapes the recovery controller creates:
//
//   - xpum-reset-job.yaml: hostPath /sys, one privileged root container running xpu-smi.
//   - xpum-fwupdate-job.yaml: the same, plus an emptyDir the fw-copy initContainer stages the
//     firmware into.
//
// privileged, root and hostPath are what a PCIe reset and a firmware reflash actually need —
// xpu-smi drives the device through sysfs, and a reset that cannot reach the device is the
// problem it was created to solve. Everything not needed is denied: no host network, PID, IPC
// or ports, every capability dropped, and none allowed back. Requiring the drop costs nothing,
// since both templates already set capabilities.drop: [ALL ] themselves.
func buildRecoverySCC(name string) *unstructured.Unstructured {
	return common.BuildSCC(name, map[string]interface{}{
		"allowPrivilegedContainer": true,
		"allowHostDirVolumePlugin": true,
		"allowHostIPC":             false,
		"allowHostNetwork":         false,
		"allowHostPID":             false,
		"allowHostPorts":           false,
		"allowPrivilegeEscalation": true,
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
