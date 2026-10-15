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
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batch "k8s.io/api/batch/v1"
	"k8s.io/utils/ptr"

	"github.com/intel/gpu-base-operator/config/deployments"
)

// recoveryJobTemplates returns the Job templates a GPURecoveryPlan creates pods from, keyed by the
// recovery they carry out. One SCC covers both, so the coverage specs check both.
func recoveryJobTemplates() map[string]*batch.Job {
	return map[string]*batch.Job{
		"reset":   deployments.XpuManagerResetJob(),
		"reflash": deployments.XpuManagerFWUpdateJob(),
	}
}

var _ = Describe("GPURecoveryPlan OpenShift SCC builders", func() {
	It("buildRecoverySCC sets correct fields", func() {
		scc := buildRecoverySCC("recovery-builder-test")

		Expect(scc.GetName()).To(Equal("recovery-builder-test"))
		Expect(scc.GetKind()).To(Equal("SecurityContextConstraints"))

		// A PCIe reset and a firmware reflash genuinely need these: xpu-smi drives the device
		// through sysfs, which is a hostPath mount and a privileged root container.
		Expect(scc.Object["allowPrivilegedContainer"]).To(BeTrue())
		Expect(scc.Object["allowPrivilegeEscalation"]).To(BeTrue())
		Expect(scc.Object["allowHostDirVolumePlugin"]).To(BeTrue())

		// Nothing beyond that: a recovery pod talks to a device, not to the network or to the
		// other processes on the node.
		Expect(scc.Object["allowHostNetwork"]).To(BeFalse())
		Expect(scc.Object["allowHostPID"]).To(BeFalse())
		Expect(scc.Object["allowHostIPC"]).To(BeFalse())
		Expect(scc.Object["allowHostPorts"]).To(BeFalse())
		Expect(scc.Object["allowedCapabilities"]).To(BeNil())

		drops, ok := scc.Object["requiredDropCapabilities"].([]interface{})
		Expect(ok).To(BeTrue())
		Expect(drops).To(ContainElement("ALL"))

		vols, ok := scc.Object["volumes"].([]interface{})
		Expect(ok).To(BeTrue())
		Expect(vols).To(ContainElements("hostPath", "emptyDir"))
	})

	// Same reasoning as the fwupdate coverage spec above, over both templates: one SCC has to
	// admit the reset Job and the reflash Job alike, so a change to either that outgrows it is
	// caught here rather than at admission on a customer cluster.

	It("buildRecoverySCC should permit every volume type the recovery Job templates use", func() {
		scc := buildRecoverySCC("recovery-volume-coverage")

		allowed, ok := scc.Object["volumes"].([]interface{})
		Expect(ok).To(BeTrue())

		allowedSet := map[string]bool{}
		for _, v := range allowed {
			allowedSet[v.(string)] = true
		}

		for name, job := range recoveryJobTemplates() {
			for _, vol := range job.Spec.Template.Spec.Volumes {
				switch {
				case vol.HostPath != nil:
					Expect(allowedSet["hostPath"]).To(BeTrue(),
						"%s Job mounts hostPath %s but the SCC forbids it", name, vol.Name)
				case vol.EmptyDir != nil:
					Expect(allowedSet["emptyDir"]).To(BeTrue(),
						"%s Job uses emptyDir %s but the SCC forbids it", name, vol.Name)
				default:
					Fail(fmt.Sprintf("%s Job volume %s is a type buildRecoverySCC does not account for",
						name, vol.Name))
				}
			}
		}
	})

	It("buildRecoverySCC should permit the privilege level the recovery Job templates request", func() {
		scc := buildRecoverySCC("recovery-priv-coverage")

		for name, job := range recoveryJobTemplates() {
			podSpec := job.Spec.Template.Spec

			// initContainers count too: fw-copy is admitted under the same SCC as the container
			// that follows it, so a privilege it grows is a privilege the SCC has to allow.
			for _, c := range append(podSpec.InitContainers, podSpec.Containers...) {
				if c.SecurityContext == nil {
					continue
				}

				if ptr.Deref(c.SecurityContext.Privileged, false) {
					Expect(scc.Object["allowPrivilegedContainer"]).To(BeTrue(),
						"%s Job container %s is privileged but the SCC forbids it", name, c.Name)
				}

				if ptr.Deref(c.SecurityContext.AllowPrivilegeEscalation, false) {
					Expect(scc.Object["allowPrivilegeEscalation"]).To(BeTrue(),
						"%s Job container %s escalates privilege but the SCC forbids it", name, c.Name)
				}
			}
		}
	})
})
