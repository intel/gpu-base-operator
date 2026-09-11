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
	batch "k8s.io/api/batch/v1"
	core "k8s.io/api/core/v1"
	rbac "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	intelv1a1 "github.com/intel/gpu-base-operator/api/v1alpha1"
)

// OpenShift: the SCC quadruple the privileged recovery pods need, created per plan
// and only on an OpenShift cluster.
var _ = Describe("GPURecoveryPlan Controller: OpenShift SCC", func() {
	ctx := context.Background()

	// Recovery Jobs are privileged, run as root and mount hostPath /sys. On OpenShift the default
	// restricted SCC rejects that at admission, so without an SCC of their own the recovery never
	// happens — and the event is already marked in-progress against a pod that never runs.
	Context("OpenShift SCC handling", func() {
		// sccNames returns the SCC/Role/Binding/SA quadruple for a plan and registers cleanup for
		// all four: they are cluster-scoped (bar the SA) and outlive the plan otherwise.
		sccNames := func(planName string) (string, string, string, string) {
			sccName, roleName, bindingName, saName := buildOpenShiftNames(planName, recoveryResourcePart)

			DeferCleanup(func() {
				deleteOpenShiftSCCResources(context.Background(), k8sClient,
					sccName, roleName, bindingName, saName, "default")
			})

			return sccName, roleName, bindingName, saName
		}

		getSCC := func(sccName string) (*unstructured.Unstructured, error) {
			scc := &unstructured.Unstructured{}
			scc.SetAPIVersion(sccAPIVersion)
			scc.SetKind(sccKind)

			return scc, k8sClient.Get(ctx, types.NamespacedName{Name: sccName}, scc)
		}

		// planWithEvent builds a plan carrying one event in waiting-approval, ready for a direct
		// createRecoveryJob call.
		planWithEvent := func(planName, evtID string, rt intelv1a1.RecoveryType) *intelv1a1.GPURecoveryPlan {
			reason := reasonWedged
			if rt == intelv1a1.RecoveryTypeReflash {
				reason = reasonSurvivability
			}

			return &intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{Name: planName},
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					DeviceID:         "0xabcd",
					XpuSmi:           intelv1a1.XpuSmiSpec{Image: "local/xpusmi:devel"},
				},
				Status: intelv1a1.GPURecoveryPlanStatus{
					Events: []intelv1a1.RecoveryEvent{{
						ID:           evtID,
						NodeName:     "node01",
						GPUBDF:       "0000:02:00.0",
						Reason:       reason,
						RecoveryType: intelv1a1.RecoveryTypeSpec{Type: rt},
						State:        intelv1a1.RecoveryEventStateWaitingApproval,
						LastUpdated:  ptr.To(metav1.Now()),
					}},
				},
			}
		}

		expectJobServiceAccount := func(jobName, want string) {
			job := &batch.Job{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: jobName, Namespace: "default"}, job)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(context.Background(), job)
			})

			Expect(job.Spec.Template.Spec.ServiceAccountName).To(Equal(want))
		}

		It("should create the SCC quadruple on reconcile", func() {
			r := newOpenShiftTestReconciler()
			key := types.NamespacedName{Name: "plan-openshift-scc"}
			sccName, roleName, bindingName, saName := sccNames(key.Name)

			createPlanForOwnerRef(&intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{Name: key.Name},
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					DeviceID:         "0xabcd",
				},
			})

			// The first reconcile only adds the finalizer; the second runs the phases.
			for range 2 {
				_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
				Expect(err).NotTo(HaveOccurred())
			}

			scc, err := getSCC(sccName)
			Expect(err).NotTo(HaveOccurred())
			Expect(scc.Object["allowPrivilegedContainer"]).To(BeTrue())

			role := &rbac.ClusterRole{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: roleName}, role)).To(Succeed())
			Expect(role.Rules[0].Verbs).To(ContainElement("use"))
			Expect(role.Rules[0].ResourceNames).To(ContainElement(sccName),
				"the Role must grant use of this plan's own SCC, not of every SCC in the cluster")

			binding := &rbac.ClusterRoleBinding{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: bindingName}, binding)).To(Succeed())
			Expect(binding.Subjects).To(ConsistOf(rbac.Subject{
				Kind: "ServiceAccount", Name: saName, Namespace: "default",
			}))

			Expect(k8sClient.Get(ctx,
				types.NamespacedName{Name: saName, Namespace: "default"}, &core.ServiceAccount{})).To(Succeed())
		})

		It("should not create any SCC resources on a plain Kubernetes cluster", func() {
			r := newTestReconciler()
			key := types.NamespacedName{Name: "plan-vanilla-k8s"}
			sccName, _, _, saName := sccNames(key.Name)

			createPlanForOwnerRef(&intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{Name: key.Name},
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					DeviceID:         "0xabcd",
				},
			})

			for range 2 {
				_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
				Expect(err).NotTo(HaveOccurred())
			}

			_, err := getSCC(sccName)
			Expect(err).To(Satisfy(errors.IsNotFound))
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: saName, Namespace: "default"},
				&core.ServiceAccount{})).To(Satisfy(errors.IsNotFound),
				"a cluster without security.openshift.io must not get OpenShift-only objects")
		})

		// Every reconcile calls it, so it has to be a no-op once the objects are there.
		It("should be idempotent across reconciles", func() {
			r := newOpenShiftTestReconciler()
			sccNames("plan-scc-idem")

			for range 3 {
				Expect(r.ensureOpenShiftResources(ctx, "plan-scc-idem")).To(Succeed())
			}
		})

		// SCC admission picks the constraint from the pod's ServiceAccount. A Job created without
		// one falls back to restricted and is refused for being privileged.
		It("should run reset Job pods under the SCC ServiceAccount", func() {
			r := newOpenShiftTestReconciler()
			_, _, _, saName := sccNames("plan-scc-job")

			p := planWithEvent("plan-scc-job", "evt-scc-job", intelv1a1.RecoveryTypeSBR)
			createPlanForOwnerRef(p)

			Expect(r.createRecoveryJob(ctx, p, &p.Status.Events[0])).To(Succeed())
			expectJobServiceAccount("recovery-evt-scc-job-0", saName)
		})

		// The reflash Job is built from the other template and by another function, so the SA has
		// to be asserted on both: prepareRecoveryJob is what they share, and it is where it is set.
		It("should run reflash Job pods under the SCC ServiceAccount too", func() {
			r := newOpenShiftTestReconciler()
			_, _, _, saName := sccNames("plan-scc-reflash")

			p := planWithEvent("plan-scc-reflash", "evt-scc-reflash", intelv1a1.RecoveryTypeReflash)
			p.Spec.Firmware = &intelv1a1.FirmwareSpec{
				Source: intelv1a1.FirmwareSource{
					ContainerSource: &intelv1a1.ContainerFirmwareSource{Name: "registry/fw:v1"},
				},
				File: "gfx.bin",
			}
			createPlanForOwnerRef(p)

			Expect(r.createRecoveryJob(ctx, p, &p.Status.Events[0])).To(Succeed())
			expectJobServiceAccount("recovery-evt-scc-reflash-0", saName)
		})

		It("should not set a ServiceAccount on a plain Kubernetes cluster", func() {
			r := newTestReconciler()

			p := planWithEvent("plan-no-sa", "evt-no-sa", intelv1a1.RecoveryTypeSBR)
			createPlanForOwnerRef(p)

			Expect(r.createRecoveryJob(ctx, p, &p.Status.Events[0])).To(Succeed())
			expectJobServiceAccount("recovery-evt-no-sa-0", "")
		})

		// The quadruple is cluster-scoped (bar the SA) and not owned by the plan, so nothing
		// garbage-collects it. A stale SCC left behind keeps granting privileges to a
		// ServiceAccount name a later, unrelated plan of the same name would reuse.
		It("should delete the SCC quadruple when the plan is deleted", func() {
			r := newOpenShiftTestReconciler()
			key := types.NamespacedName{Name: "plan-scc-delete"}
			sccName, roleName, bindingName, saName := sccNames(key.Name)

			p := &intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{Name: key.Name, Finalizers: []string{recoveryPlanFinalizer}},
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					DeviceID:         "0xabcd",
				},
			}
			Expect(k8sClient.Create(ctx, p)).To(Succeed())

			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			_, err = getSCC(sccName)
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Delete(ctx, p)).To(Succeed())

			// The deletion path runs first and removes the finalizer, so the object is gone once
			// this reconcile returns.
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			_, err = getSCC(sccName)
			Expect(err).To(Satisfy(errors.IsNotFound))
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: roleName},
				&rbac.ClusterRole{})).To(Satisfy(errors.IsNotFound))
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: bindingName},
				&rbac.ClusterRoleBinding{})).To(Satisfy(errors.IsNotFound))
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: saName, Namespace: "default"},
				&core.ServiceAccount{})).To(Satisfy(errors.IsNotFound))
		})

		// Two plans in one cluster must not share an SCC: deleting one would otherwise strip the
		// other's recovery Jobs of the grounds they were admitted on, mid-reset.
		It("should name the SCC quadruple per plan", func() {
			sccA, roleA, bindingA, saA := buildOpenShiftNames("plan-a", recoveryResourcePart)
			sccB, roleB, bindingB, saB := buildOpenShiftNames("plan-b", recoveryResourcePart)

			Expect([]string{sccA, roleA, bindingA, saA}).NotTo(ContainElements(sccB, roleB, bindingB, saB))
		})
	})
})
