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
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/intel/gpu-base-operator/config/deployments"
)

var _ = Describe("GPUFirmwareUpdate OpenShift SCC builders", func() {
	It("buildFWUpdateSCC sets correct fields", func() {
		scc := buildFWUpdateSCC("fwupdate-builder-test")

		Expect(scc.GetName()).To(Equal("fwupdate-builder-test"))
		Expect(scc.GetKind()).To(Equal("SecurityContextConstraints"))
		Expect(scc.Object["allowPrivilegedContainer"]).To(BeTrue())
		Expect(scc.Object["allowPrivilegeEscalation"]).To(BeTrue())
		Expect(scc.Object["allowHostNetwork"]).To(BeFalse())

		// hostPath: the updater mounts /sys so the update script can find the target GPU's
		// MEI device. An SCC saying false while the template mounts it fails at admission.
		Expect(scc.Object["allowHostDirVolumePlugin"]).To(BeTrue())

		vols, ok := scc.Object["volumes"].([]interface{})
		Expect(ok).To(BeTrue())
		Expect(vols).To(ContainElements("hostPath", "emptyDir"))
	})

	// The SCC is only useful if it permits the pod the firmware update controller actually
	// creates. Comparing it against the embedded template rather than a hand-copied list
	// means a template change that outgrows the SCC is caught here instead of at admission
	// on a customer cluster.

	It("buildFWUpdateSCC should permit every volume type the update Job template uses", func() {
		scc := buildFWUpdateSCC("fwupdate-volume-coverage")

		allowed, ok := scc.Object["volumes"].([]interface{})
		Expect(ok).To(BeTrue())

		allowedSet := map[string]bool{}
		for _, v := range allowed {
			allowedSet[v.(string)] = true
		}

		for _, vol := range deployments.XpuManagerFWUpdateJob().Spec.Template.Spec.Volumes {
			switch {
			case vol.HostPath != nil:
				Expect(allowedSet["hostPath"]).To(BeTrue(),
					"update Job mounts hostPath %s but the SCC forbids it", vol.Name)
			case vol.EmptyDir != nil:
				Expect(allowedSet["emptyDir"]).To(BeTrue(),
					"update Job uses emptyDir %s but the SCC forbids it", vol.Name)
			default:
				Fail(fmt.Sprintf("update Job volume %s is a type buildFWUpdateSCC does not account for",
					vol.Name))
			}
		}
	})
})
