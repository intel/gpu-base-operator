/*
Copyright 2026 Intel Corporation. All Rights Reserved.

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

package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	core "k8s.io/api/core/v1"
	rbac "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var _ = Describe("OpenShift SCC helpers", func() {
	const testOpenshiftNs = "default"
	ctx := context.Background()

	Context("SCC builder functions", func() {
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

		It("buildFWUpdateSCC sets correct fields", func() {
			scc := buildFWUpdateSCC("fwupdate-builder-test")

			Expect(scc.GetName()).To(Equal("fwupdate-builder-test"))
			Expect(scc.GetKind()).To(Equal("SecurityContextConstraints"))
			Expect(scc.Object["allowPrivilegedContainer"]).To(BeTrue())
			Expect(scc.Object["allowPrivilegeEscalation"]).To(BeTrue())
			Expect(scc.Object["allowHostDirVolumePlugin"]).To(BeFalse())
			Expect(scc.Object["allowHostNetwork"]).To(BeFalse())

			vols, ok := scc.Object["volumes"].([]interface{})
			Expect(ok).To(BeTrue())
			Expect(vols).To(ContainElement("emptyDir"))
			Expect(vols).NotTo(ContainElement("hostPath"))
		})
	})

	Context("ensureSCC", func() {
		It("creates SCC when not found and is idempotent", func() {
			name := "test-ensure-scc-create"
			scc := buildDevicePluginSCC(name)

			By("first call creates the SCC")
			Expect(ensureSCC(ctx, k8sClient, scc)).To(Succeed())

			existing := &unstructured.Unstructured{}
			existing.SetAPIVersion(sccAPIVersion)
			existing.SetKind(sccKind)
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name}, existing)).To(Succeed())

			By("second call is idempotent")
			Expect(ensureSCC(ctx, k8sClient, buildDevicePluginSCC(name))).To(Succeed())

			DeferCleanup(func() {
				scc := &unstructured.Unstructured{}
				scc.SetAPIVersion(sccAPIVersion)
				scc.SetKind(sccKind)
				scc.SetName(name)
				_ = k8sClient.Delete(ctx, scc)
			})
		})
	})

	Context("ensureSCCRole", func() {
		It("creates ClusterRole when not found and is idempotent", func() {
			roleName := "test-ensure-scc-role"
			sccName := "test-some-scc"

			By("first call creates the ClusterRole")
			Expect(createSCCRole(ctx, k8sClient, roleName, sccName)).To(Succeed())

			cr := &rbac.ClusterRole{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: roleName}, cr)).To(Succeed())
			Expect(cr.Rules).To(HaveLen(1))
			Expect(cr.Rules[0].ResourceNames).To(ContainElement(sccName))
			Expect(cr.Rules[0].Verbs).To(ContainElement("use"))

			By("second call is idempotent")
			Expect(createSCCRole(ctx, k8sClient, roleName, sccName)).To(Succeed())

			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, &rbac.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: roleName}})
			})
		})
	})

	Context("ensureSCCRoleBinding", func() {
		It("creates ClusterRoleBinding when not found and is idempotent", func() {
			bindingName := "test-ensure-scc-binding"
			roleName := "test-role-for-binding"
			saName := "test-sa"
			namespace := testOpenshiftNs

			By("first call creates the ClusterRoleBinding")
			Expect(createSCCRoleBinding(ctx, k8sClient, bindingName, roleName, saName, namespace)).To(Succeed())

			crb := &rbac.ClusterRoleBinding{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: bindingName}, crb)).To(Succeed())
			Expect(crb.RoleRef.Name).To(Equal(roleName))
			Expect(crb.Subjects).To(HaveLen(1))
			Expect(crb.Subjects[0].Name).To(Equal(saName))
			Expect(crb.Subjects[0].Namespace).To(Equal(namespace))

			By("second call is idempotent")
			Expect(createSCCRoleBinding(ctx, k8sClient, bindingName, roleName, saName, namespace)).To(Succeed())

			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, &rbac.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: bindingName}})
			})
		})
	})

	Context("ensureServiceAccount", func() {
		ensureNs := testOpenshiftNs

		It("creates ServiceAccount when not found and is idempotent", func() {
			saName := "test-ensure-sa"

			By("first call creates the ServiceAccount")
			Expect(createServiceAccount(ctx, k8sClient, saName, ensureNs)).To(Succeed())

			sa := &core.ServiceAccount{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: saName, Namespace: ensureNs}, sa)).To(Succeed())

			By("second call is idempotent")
			Expect(createServiceAccount(ctx, k8sClient, saName, ensureNs)).To(Succeed())

			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, &core.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: saName, Namespace: ensureNs}})
			})
		})
	})

	Context("deleteOpenShiftSCCResources", func() {
		deleteNs := testOpenshiftNs

		It("deletes all resources when they exist", func() {
			sccName := "test-delete-scc"
			roleName := "test-delete-role"
			bindingName := "test-delete-binding"
			saName := "test-delete-sa"

			By("creating the resources")
			Expect(ensureSCC(ctx, k8sClient, buildDevicePluginSCC(sccName))).To(Succeed())
			Expect(createSCCRole(ctx, k8sClient, roleName, sccName)).To(Succeed())
			Expect(createSCCRoleBinding(ctx, k8sClient, bindingName, roleName, saName, deleteNs)).To(Succeed())
			Expect(createServiceAccount(ctx, k8sClient, saName, deleteNs)).To(Succeed())

			By("deleting all resources")
			deleteOpenShiftSCCResources(ctx, k8sClient, sccName, roleName, bindingName, saName, deleteNs)

			scc := &unstructured.Unstructured{}
			scc.SetAPIVersion(sccAPIVersion)
			scc.SetKind(sccKind)
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: sccName}, scc)).To(Satisfy(errors.IsNotFound))
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: roleName}, &rbac.ClusterRole{})).To(Satisfy(errors.IsNotFound))
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: bindingName}, &rbac.ClusterRoleBinding{})).To(Satisfy(errors.IsNotFound))
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: saName, Namespace: deleteNs}, &core.ServiceAccount{})).To(Satisfy(errors.IsNotFound))
		})

		It("tolerates NotFound for all resources", func() {
			// None of these exist — should not panic or return error
			deleteOpenShiftSCCResources(ctx, k8sClient,
				"nonexistent-scc", "nonexistent-role", "nonexistent-binding",
				"nonexistent-sa", deleteNs)
		})

		It("skips ServiceAccount deletion when saName is empty", func() {
			sccName := "test-skip-sa-scc"
			roleName := "test-skip-sa-role"
			bindingName := "test-skip-sa-binding"
			saName := "test-skip-sa"

			By("creating the resources")
			Expect(ensureSCC(ctx, k8sClient, buildDevicePluginSCC(sccName))).To(Succeed())
			Expect(createSCCRole(ctx, k8sClient, roleName, sccName)).To(Succeed())
			Expect(createSCCRoleBinding(ctx, k8sClient, bindingName, roleName, saName, deleteNs)).To(Succeed())
			Expect(createServiceAccount(ctx, k8sClient, saName, deleteNs)).To(Succeed())

			By("deleting with empty saName — SA should remain")
			deleteOpenShiftSCCResources(ctx, k8sClient, sccName, roleName, bindingName, "", deleteNs)

			sa := &core.ServiceAccount{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: saName, Namespace: deleteNs}, sa)).To(Succeed())

			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, &core.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: saName, Namespace: deleteNs}})
			})
		})
	})
})
