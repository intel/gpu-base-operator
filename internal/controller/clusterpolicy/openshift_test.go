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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("ClusterPolicy OpenShift SCC builders", func() {
	It("buildDevicePluginSCC sets correct fields", func() {
		scc := buildDevicePluginSCC("dp-builder-test")

		Expect(scc.GetName()).To(Equal("dp-builder-test"))
		Expect(scc.GetKind()).To(Equal("SecurityContextConstraints"))
		Expect(scc.GetAPIVersion()).To(Equal("security.openshift.io/v1"))
		Expect(scc.Object["allowPrivilegedContainer"]).To(BeFalse())
		Expect(scc.Object["allowPrivilegeEscalation"]).To(BeFalse())
		Expect(scc.Object["allowHostDirVolumePlugin"]).To(BeTrue())
		Expect(scc.Object["allowHostNetwork"]).To(BeFalse())

		drops, ok := scc.Object["requiredDropCapabilities"].([]interface{})
		Expect(ok).To(BeTrue())
		Expect(drops).To(ContainElement("ALL"))

		vols, ok := scc.Object["volumes"].([]interface{})
		Expect(ok).To(BeTrue())
		Expect(vols).To(ContainElements("hostPath", "emptyDir"))
	})

	It("buildXpuManagerSCC sets correct fields", func() {
		scc := buildXpuManagerSCC("xpum-builder-test")

		Expect(scc.GetName()).To(Equal("xpum-builder-test"))
		Expect(scc.Object["allowPrivilegedContainer"]).To(BeFalse())

		caps, ok := scc.Object["allowedCapabilities"].([]interface{})
		Expect(ok).To(BeTrue())
		Expect(caps).To(ContainElement("SYS_ADMIN"))

		drops, ok := scc.Object["requiredDropCapabilities"].([]interface{})
		Expect(ok).To(BeTrue())
		Expect(drops).To(ContainElement("ALL"))

		vols, ok := scc.Object["volumes"].([]interface{})
		Expect(ok).To(BeTrue())
		Expect(vols).To(ContainElements("hostPath", "configMap"))
		// secret: the OTLP client certificate is mounted from a Secret.
		Expect(vols).To(ContainElements("hostPath", "configMap", "secret"))
	})

	It("buildDRASCC sets correct fields", func() {
		scc := buildDRASCC("dra-builder-test")

		Expect(scc.GetName()).To(Equal("dra-builder-test"))
		Expect(scc.Object["allowPrivilegedContainer"]).To(BeFalse())
		Expect(scc.Object["allowPrivilegeEscalation"]).To(BeFalse())

		drops, ok := scc.Object["requiredDropCapabilities"].([]interface{})
		Expect(ok).To(BeTrue())
		Expect(drops).To(ContainElement("ALL"))

		vols, ok := scc.Object["volumes"].([]interface{})
		Expect(ok).To(BeTrue())
		Expect(vols).To(ContainElements("hostPath", "projected"))
	})
})
