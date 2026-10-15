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

package gpufirmwareupdate

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/intel/gpu-base-operator/internal/controller/common"
)

// buildFWUpdateSCC returns the SCC for the GPU firmware update Job pods.
// The updater container runs privileged as root to access GPU firmware interfaces.
//
// hostPath is allowed because xpum-fwupdate-job.yaml mounts /sys into the updater: the update
// script resolves the target GPU's MEI device through sysfs. An SCC forbidding it would make
// every firmware update fail at admission on OpenShift.
func buildFWUpdateSCC(name string) *unstructured.Unstructured {
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
		"requiredDropCapabilities": nil,
		"runAsUser":                map[string]interface{}{"type": "RunAsAny"},
		"seLinuxContext":           map[string]interface{}{"type": "RunAsAny"},
		"seccompProfiles":          []interface{}{"*"},
		"supplementalGroups":       map[string]interface{}{"type": "RunAsAny"},
		"volumes":                  []interface{}{"hostPath", "emptyDir"},
		"users":                    []interface{}{},
		"groups":                   []interface{}{},
	})
}
