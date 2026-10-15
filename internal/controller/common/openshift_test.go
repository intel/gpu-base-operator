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

package common

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

// testSCC returns a minimal SCC for exercising the generic helpers.
func testSCC(name string) *unstructured.Unstructured {
	return BuildSCC(name, map[string]interface{}{"allowPrivilegedContainer": false})
}

var _ = Describe("OpenShift SCC helpers", func() {
	const testOpenshiftNs = "default"
	ctx := context.Background()

	Context("EnsureSCC", func() {
		It("creates SCC when not found and is idempotent", func() {
			name := "test-ensure-scc-create"
			scc := testSCC(name)

			By("first call creates the SCC")
			Expect(EnsureSCC(ctx, k8sClient, scc)).To(Succeed())

			existing := &unstructured.Unstructured{}
			existing.SetAPIVersion(SCCAPIVersion)
			existing.SetKind(SCCKind)
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name}, existing)).To(Succeed())

			By("second call is idempotent")
			Expect(EnsureSCC(ctx, k8sClient, testSCC(name))).To(Succeed())

			DeferCleanup(func() {
				scc := &unstructured.Unstructured{}
				scc.SetAPIVersion(SCCAPIVersion)
				scc.SetKind(SCCKind)
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
			Expect(CreateSCCRole(ctx, k8sClient, roleName, sccName)).To(Succeed())

			cr := &rbac.ClusterRole{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: roleName}, cr)).To(Succeed())
			Expect(cr.Rules).To(HaveLen(1))
			Expect(cr.Rules[0].ResourceNames).To(ContainElement(sccName))
			Expect(cr.Rules[0].Verbs).To(ContainElement("use"))

			By("second call is idempotent")
			Expect(CreateSCCRole(ctx, k8sClient, roleName, sccName)).To(Succeed())

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
			Expect(CreateSCCRoleBinding(ctx, k8sClient, bindingName, roleName, saName, namespace)).To(Succeed())

			crb := &rbac.ClusterRoleBinding{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: bindingName}, crb)).To(Succeed())
			Expect(crb.RoleRef.Name).To(Equal(roleName))
			Expect(crb.Subjects).To(HaveLen(1))
			Expect(crb.Subjects[0].Name).To(Equal(saName))
			Expect(crb.Subjects[0].Namespace).To(Equal(namespace))

			By("second call is idempotent")
			Expect(CreateSCCRoleBinding(ctx, k8sClient, bindingName, roleName, saName, namespace)).To(Succeed())

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
			Expect(CreateServiceAccount(ctx, k8sClient, saName, ensureNs)).To(Succeed())

			sa := &core.ServiceAccount{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: saName, Namespace: ensureNs}, sa)).To(Succeed())

			By("second call is idempotent")
			Expect(CreateServiceAccount(ctx, k8sClient, saName, ensureNs)).To(Succeed())

			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, &core.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: saName, Namespace: ensureNs}})
			})
		})
	})

	Context("DeleteOpenShiftSCCResources", func() {
		deleteNs := testOpenshiftNs

		It("deletes all resources when they exist", func() {
			sccName := "test-delete-scc"
			roleName := "test-delete-role"
			bindingName := "test-delete-binding"
			saName := "test-delete-sa"

			By("creating the resources")
			Expect(EnsureSCC(ctx, k8sClient, testSCC(sccName))).To(Succeed())
			Expect(CreateSCCRole(ctx, k8sClient, roleName, sccName)).To(Succeed())
			Expect(CreateSCCRoleBinding(ctx, k8sClient, bindingName, roleName, saName, deleteNs)).To(Succeed())
			Expect(CreateServiceAccount(ctx, k8sClient, saName, deleteNs)).To(Succeed())

			By("deleting all resources")
			DeleteOpenShiftSCCResources(ctx, k8sClient, sccName, roleName, bindingName, saName, deleteNs)

			scc := &unstructured.Unstructured{}
			scc.SetAPIVersion(SCCAPIVersion)
			scc.SetKind(SCCKind)
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: sccName}, scc)).To(Satisfy(errors.IsNotFound))
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: roleName}, &rbac.ClusterRole{})).To(Satisfy(errors.IsNotFound))
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: bindingName}, &rbac.ClusterRoleBinding{})).To(Satisfy(errors.IsNotFound))
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: saName, Namespace: deleteNs}, &core.ServiceAccount{})).To(Satisfy(errors.IsNotFound))
		})

		It("tolerates NotFound for all resources", func() {
			// None of these exist — should not panic or return error
			DeleteOpenShiftSCCResources(ctx, k8sClient,
				"nonexistent-scc", "nonexistent-role", "nonexistent-binding",
				"nonexistent-sa", deleteNs)
		})

		It("skips ServiceAccount deletion when saName is empty", func() {
			sccName := "test-skip-sa-scc"
			roleName := "test-skip-sa-role"
			bindingName := "test-skip-sa-binding"
			saName := "test-skip-sa"

			By("creating the resources")
			Expect(EnsureSCC(ctx, k8sClient, testSCC(sccName))).To(Succeed())
			Expect(CreateSCCRole(ctx, k8sClient, roleName, sccName)).To(Succeed())
			Expect(CreateSCCRoleBinding(ctx, k8sClient, bindingName, roleName, saName, deleteNs)).To(Succeed())
			Expect(CreateServiceAccount(ctx, k8sClient, saName, deleteNs)).To(Succeed())

			By("deleting with empty saName — SA should remain")
			DeleteOpenShiftSCCResources(ctx, k8sClient, sccName, roleName, bindingName, "", deleteNs)

			sa := &core.ServiceAccount{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: saName, Namespace: deleteNs}, sa)).To(Succeed())

			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, &core.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: saName, Namespace: deleteNs}})
			})
		})
	})
})
