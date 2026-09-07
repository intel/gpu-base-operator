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
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batch "k8s.io/api/batch/v1"
	core "k8s.io/api/core/v1"
	rbac "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

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
