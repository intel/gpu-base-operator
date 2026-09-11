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
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	intelv1a1 "github.com/intel/gpu-base-operator/api/v1alpha1"
)

// The pre-flight image check: no recovery starts on an image the kubelet could not
// pull, and a plan held on one must say so without spinning.
var _ = Describe("GPURecoveryPlan Controller: image pre-flight verification", func() {
	ctx := context.Background()

	// Every image a recovery Job pulls is checked against its registry first. A Job pinned to an
	// image that does not resolve is not a fast failure: it reports in-progress while its pod sits in
	// ImagePullBackOff until activeDeadlineSeconds expires, so a mistyped reference reads as minutes
	// of recovery followed by a failure that names the Job rather than the typo.
	Context("Pre-flight image verification", func() {
		const (
			imgBDF  = "0000:5e:00.0"
			imgFile = "fdo.bin"
			imgFW   = "registry.example.com/fw:1.0"
			imgSmi  = "registry.example.com/xpu-smi:1.0"
		)

		// imgPlan creates a plan whose single event is approved by a blanket EventID approval and
		// whose drain is off, so processApprovals runs the gate and then goes straight to the Job.
		imgPlan := func(planName, evtID string, rt intelv1a1.RecoveryType) *intelv1a1.GPURecoveryPlan {
			p := &intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{Name: planName},
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					DeviceID:         "0xabcd",
					Drain:            intelv1a1.DrainSpec{Enable: ptr.To(false)},
					XpuSmi:           intelv1a1.XpuSmiSpec{Image: imgSmi},
					Approvals:        []intelv1a1.RecoveryApproval{{ID: "app-" + evtID, EventID: evtID}},
				},
				Status: intelv1a1.GPURecoveryPlanStatus{
					Events: []intelv1a1.RecoveryEvent{{
						ID:           evtID,
						NodeName:     "node11",
						GPUBDF:       imgBDF,
						RecoveryType: intelv1a1.RecoveryTypeSpec{Type: rt},
						State:        intelv1a1.RecoveryEventStateWaitingApproval,
						LastUpdated:  ptr.To(metav1.Now()),
					}},
				},
			}

			if rt == intelv1a1.RecoveryTypeReflash {
				p.Spec.Firmware = &intelv1a1.FirmwareSpec{
					Source: intelv1a1.FirmwareSource{
						ContainerSource: &intelv1a1.ContainerFirmwareSource{Name: imgFW},
					},
					File: imgFile,
				}
			}

			createPlanForOwnerRef(p)

			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, &batch.Job{ObjectMeta: metav1.ObjectMeta{
					Name: recoveryJobName(evtID, 0), Namespace: "default",
				}})
			})

			return p
		}

		It("should check the xpu-smi image before creating a reset Job", func() {
			fake := &fakeContentImageVerifier{}
			r := newTestReconcilerVerifying(fake)
			r.Opts.SecretName = "operator-pull-secret"

			p := imgPlan("plan-img-reset", "evt-img-reset", intelv1a1.RecoveryTypeSlot)

			r.processApprovals(ctx, p)

			Expect(p.Status.Events[0].State).To(Equal(intelv1a1.RecoveryEventStateInProgress))

			// No Files: nothing inside the xpu-smi image is the operator's business, and a content
			// check would stream hundreds of megabytes to learn nothing. The operator's own pull
			// secret has to be there, or a private registry answers "unauthorized" for an image the
			// kubelet would have pulled perfectly well.
			Expect(fake.requests).To(ConsistOf(ImageVerifyRequest{
				Image:      imgSmi,
				PullSecret: "operator-pull-secret",
			}))
		})

		It("should check the firmware image for its file as well as its existence", func() {
			fake := &fakeContentImageVerifier{}
			r := newTestReconcilerVerifying(fake)

			p := imgPlan("plan-img-reflash", "evt-img-reflash", intelv1a1.RecoveryTypeReflash)

			r.processApprovals(ctx, p)

			Expect(p.Status.Events[0].State).To(Equal(intelv1a1.RecoveryEventStateInProgress))
			Expect(fake.requests).To(HaveLen(2))

			// An image that exists but does not carry spec.firmware.file fails the same way a missing
			// image does, only later and inside the initContainer, where the diagnostic is a shell
			// error in a pod log. No checksum is asked for: the plan does not declare one.
			Expect(fake.requests[1]).To(Equal(ImageVerifyRequest{
				Image: imgFW,
				Files: []ImageFile{{Name: firmwareImagePath(imgFile)}},
			}))
		})

		// The xpu-smi image and the firmware image can live on different registries, only one of
		// which serves a certificate the operator cannot chase. Folding the two opt-outs together
		// would silently widen whichever one the admin did not ask for.
		It("should carry each image's own TLS opt-out", func() {
			fake := &fakeContentImageVerifier{}
			r := newTestReconcilerVerifying(fake)

			p := imgPlan("plan-img-tls", "evt-img-tls", intelv1a1.RecoveryTypeReflash)
			p.Spec.Firmware.Source.ContainerSource.InsecureSkipTLSVerify = true

			r.processApprovals(ctx, p)

			Expect(fake.requests).To(HaveLen(2))
			Expect(fake.requests[0].InsecureSkipTLSVerify).To(BeFalse())
			Expect(fake.requests[1].InsecureSkipTLSVerify).To(BeTrue())
		})

		// Pull policy Never means the kubelet never contacts a registry: the image is on the node
		// already, put there by something outside Kubernetes. Verifying it against a registry that
		// may not even hold it would park a recovery whose image is sitting right there, leaving the
		// card broken.
		It("should not check an image the kubelet will not pull", func() {
			fake := &fakeContentImageVerifier{err: fmt.Errorf("not in any registry")}
			r := newTestReconcilerVerifying(fake)

			p := imgPlan("plan-img-never", "evt-img-never", intelv1a1.RecoveryTypeSlot)
			p.Spec.XpuSmi.PullPolicy = string(core.PullNever)

			r.processApprovals(ctx, p)

			Expect(fake.requests).To(BeEmpty())
			Expect(p.Status.Events[0].State).To(Equal(intelv1a1.RecoveryEventStateInProgress))
		})

		// The pull policy is spec.xpuSmi's; the firmware image is pulled by the initContainer at
		// whatever the template says. A preloaded xpu-smi therefore says nothing about the firmware.
		It("should still check the firmware image when xpu-smi is preloaded", func() {
			fake := &fakeContentImageVerifier{}
			r := newTestReconcilerVerifying(fake)

			p := imgPlan("plan-img-never-fw", "evt-img-never-fw", intelv1a1.RecoveryTypeReflash)
			p.Spec.XpuSmi.PullPolicy = string(core.PullNever)

			r.processApprovals(ctx, p)

			Expect(fake.requests).To(HaveLen(1))
			Expect(fake.requests[0].Image).To(Equal(imgFW))
		})

		It("should skip the check entirely when the plan asks it to", func() {
			fake := &fakeContentImageVerifier{err: fmt.Errorf("registry unreachable")}
			r := newTestReconcilerVerifying(fake)

			p := imgPlan("plan-img-skip", "evt-img-skip", intelv1a1.RecoveryTypeSlot)
			p.Spec.SkipImageVerification = true

			r.processApprovals(ctx, p)

			Expect(fake.requests).To(BeEmpty())
			Expect(p.Status.Events[0].State).To(Equal(intelv1a1.RecoveryEventStateInProgress),
				"an air-gapped cluster must be able to opt out and still recover its GPUs")
		})

		It("should hold the recovery and keep the approval when an image cannot be pulled", func() {
			fake := &fakeContentImageVerifier{err: fmt.Errorf("MANIFEST_UNKNOWN")}
			r := newTestReconcilerVerifying(fake)

			p := imgPlan("plan-img-hold", "evt-img-hold", intelv1a1.RecoveryTypeSlot)

			r.processApprovals(ctx, p)

			evt := p.Status.Events[0]
			Expect(evt.State).To(Equal(intelv1a1.RecoveryEventStateWaitingApproval))
			Expect(evt.JobName).To(BeEmpty())

			// The state says "waiting for an approval" while an approval is sitting right there, so
			// the message is the only thing that tells an admin what is actually wrong, and it has to
			// name the field to correct as well as the failure.
			Expect(evt.StateMessage).To(SatisfyAll(
				ContainSubstring("spec.xpuSmi.image"),
				ContainSubstring(imgSmi),
				ContainSubstring("MANIFEST_UNKNOWN"),
			))
			Expect(evt.ImageVerifyGeneration).To(Equal(p.Generation))

			Expect(p.Spec.Approvals[0].Consumed).To(BeFalse(),
				"correcting the reference must be enough; the admin must not have to approve twice")
			Expect(p.Status.Messages).To(ContainElement(ContainSubstring("spec.xpuSmi.image")))

			err := k8sClient.Get(ctx, types.NamespacedName{
				Name: recoveryJobName("evt-img-hold", 0), Namespace: "default",
			}, &batch.Job{})
			Expect(errors.IsNotFound(err)).To(BeTrue(), "no Job may exist for a held recovery")
		})

		// What failed is a value in the plan, and only an admin edit can change that answer — which
		// is also what advances metadata.generation. Retrying on the reconcile cadence would re-ask a
		// question the spec has already settled, and append a message every time.
		It("should not re-check or re-report until the plan changes", func() {
			fake := &fakeContentImageVerifier{err: fmt.Errorf("MANIFEST_UNKNOWN")}
			r := newTestReconcilerVerifying(fake)

			p := imgPlan("plan-img-once", "evt-img-once", intelv1a1.RecoveryTypeSlot)

			r.processApprovals(ctx, p)
			afterFirst := len(p.Status.Messages)

			r.processApprovals(ctx, p)
			r.processApprovals(ctx, p)

			Expect(fake.requests).To(HaveLen(1), "one registry round trip per spec version")
			Expect(p.Status.Messages).To(HaveLen(afterFirst))
		})

		It("should resume the recovery once the plan is corrected", func() {
			failing := &fakeContentImageVerifier{err: fmt.Errorf("MANIFEST_UNKNOWN")}
			p := imgPlan("plan-img-resume", "evt-img-resume", intelv1a1.RecoveryTypeSlot)

			newTestReconcilerVerifying(failing).processApprovals(ctx, p)
			Expect(p.Status.Events[0].ImageVerifyGeneration).To(Equal(p.Generation))

			// What an admin fixing the reference does: a spec write, which the API server answers
			// with a new generation. The approval is still there, unconsumed.
			p.Spec.XpuSmi.Image = "registry.example.com/xpu-smi:1.1"
			p.Generation++

			passing := &fakeContentImageVerifier{}
			newTestReconcilerVerifying(passing).processApprovals(ctx, p)

			evt := p.Status.Events[0]
			Expect(evt.State).To(Equal(intelv1a1.RecoveryEventStateInProgress),
				"messages: %v", p.Status.Messages)
			Expect(evt.ImageVerifyGeneration).To(BeZero(),
				"a cleared hold must not make the next failure look like an old one")
			Expect(evt.StateMessage).To(BeEmpty(),
				"a stale explanation on a running recovery points at a problem that is gone")
			Expect(passing.requests).To(HaveLen(1))
			Expect(passing.requests[0].Image).To(Equal("registry.example.com/xpu-smi:1.1"))

			// The transition out of a hold is worth a line: the plan's own history is where an admin
			// checks whether their edit was the one that worked.
			Expect(p.Status.Messages).To(ContainElement(ContainSubstring("resuming")))
			Expect(p.Spec.Approvals[0].Consumed).To(BeTrue())
		})

		// A block clears by itself within minutes, so holding the node through one is cheap. An
		// unpullable image waits on a person and may wait indefinitely, and a NoSchedule taint parked
		// on a working node for the lifetime of a config typo takes real capacity out of the cluster.
		It("should not cordon the node while a recovery is held on an image", func() {
			const heldNode = "img-held-node"

			node := &core.Node{ObjectMeta: metav1.ObjectMeta{Name: heldNode}}
			Expect(k8sClient.Create(ctx, node)).To(Succeed())
			DeferCleanup(func() {
				fresh := &core.Node{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: heldNode}, fresh); err == nil {
					fresh.Spec.Taints = nil
					_ = k8sClient.Update(ctx, fresh)
					_ = k8sClient.Delete(ctx, fresh)
				}
			})

			putTaintedSlice(ctx, "slice-img-held", heldNode, "0x1234", imgBDF, deviceTaintKeyReset)

			p := &intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{
					Name: "plan-img-nocordon", Finalizers: []string{recoveryPlanFinalizer},
				},
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					DeviceID:         "0x1234",
					// The drain is on, which is what makes this worth asserting: a reset would
					// normally taint the node on its way to the Job.
					Drain:  intelv1a1.DrainSpec{Enable: ptr.To(true), TimeoutSeconds: 300},
					XpuSmi: intelv1a1.XpuSmiSpec{Image: imgSmi},
					Approvals: []intelv1a1.RecoveryApproval{{
						ID:       "app-img-nocordon",
						Selector: &intelv1a1.ApprovalSelector{RecoveryType: intelv1a1.RecoveryTypeSlot},
					}},
				},
			}
			Expect(k8sClient.Create(ctx, p)).To(Succeed())
			DeferCleanup(func() {
				fresh := &intelv1a1.GPURecoveryPlan{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: p.Name}, fresh); err == nil {
					fresh.Finalizers = nil
					_ = k8sClient.Update(ctx, fresh)
					_ = k8sClient.Delete(ctx, fresh)
				}
			})

			_, err := reconcilePlanVerifying(ctx, p.Name,
				&fakeContentImageVerifier{err: fmt.Errorf("MANIFEST_UNKNOWN")})
			Expect(err).NotTo(HaveOccurred())

			updated := &intelv1a1.GPURecoveryPlan{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: p.Name}, updated)).To(Succeed())
			Expect(updated.Status.Events).To(HaveLen(1))
			Expect(updated.Status.Events[0].State).To(Equal(intelv1a1.RecoveryEventStateWaitingApproval))
			Expect(updated.Status.Events[0].DrainStartedAt).To(BeNil())

			// The plan must say it needs attention: waiting-approval alone would read as normal.
			Expect(updated.Status.State).To(Equal(intelv1a1.PlanStateError))

			// Only the operator's own taint is asserted on: envtest runs no kubelet, so the Node
			// carries node.kubernetes.io/not-ready of its own accord.
			fresh := &core.Node{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: heldNode}, fresh)).To(Succeed())
			Expect(fresh.Spec.Taints).NotTo(ContainElement(HaveField("Key", recoveryTaintKey)),
				"a recovery that cannot start must not take the node out of service")
		})
	})
})
