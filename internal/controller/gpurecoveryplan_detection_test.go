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
	resv1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	intelv1a1 "github.com/intel/gpu-base-operator/api/v1alpha1"
)

// Detection: what the operator makes of the taints the DRA driver publishes on
// ResourceSlices — which devices become events, and when those events go away again.
var _ = Describe("GPURecoveryPlan Controller: detection", func() {
	ctx := context.Background()

	const planName = "test-recovery-plan"

	plan := &intelv1a1.GPURecoveryPlan{}
	planKey := types.NamespacedName{Name: planName}

	BeforeEach(func() {
		By("creating the GPURecoveryPlan")
		err := k8sClient.Get(ctx, planKey, plan)
		if errors.IsNotFound(err) {
			resource := &intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{
					Name: planName,
					// Created with the finalizer already on it, as a plan is from its first
					// reconcile onwards. Without it the first reconcile does nothing but add the
					// finalizer, and every spec below would need a throwaway pass before the one
					// it is actually testing.
					Finalizers: []string{recoveryPlanFinalizer},
				},
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DeviceID:         "0x1234",
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
		}
	})

	AfterEach(func() {
		By("deleting the GPURecoveryPlan")
		resource := &intelv1a1.GPURecoveryPlan{}
		if err := k8sClient.Get(ctx, planKey, resource); err == nil {
			// Drop the finalizer first: nothing runs the controller during cleanup, so the object
			// would otherwise sit in Terminating forever and the next spec's Create would fail.
			resource.Finalizers = nil
			Expect(k8sClient.Update(ctx, resource)).To(Succeed())
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		}
	})

	// The whole point of the detection phase: a GPU the DRA driver has tainted shows up in the
	// CR, with everything an admin needs to decide on it, and the plan says it needs attention.
	Context("Reconcile: detection end to end", func() {
		It("should record an event for a tainted GPU and report the plan as active", func() {
			putTaintedSlice(ctx, "slice-e2e", "node-e2e", "0x1234", "0000:02:00.0", deviceTaintKeyReset)

			_, err := reconcilePlan(ctx, planName)
			Expect(err).NotTo(HaveOccurred())

			updated := &intelv1a1.GPURecoveryPlan{}
			Expect(k8sClient.Get(ctx, planKey, updated)).To(Succeed())
			Expect(updated.Status.Events).To(HaveLen(1))

			evt := updated.Status.Events[0]
			Expect(evt.NodeName).To(Equal("node-e2e"))
			Expect(evt.GPUBDF).To(Equal("0000:02:00.0"))
			Expect(evt.Reason).To(Equal(reasonWedged))
			Expect(evt.RecoveryType.Type).To(Equal(intelv1a1.RecoveryTypeSlot))
			Expect(evt.State).To(Equal(intelv1a1.RecoveryEventStateWaitingApproval))
			Expect(evt.LastUpdated).NotTo(BeNil())

			Expect(updated.Status.State).To(Equal(intelv1a1.PlanStateActive))
		})

		It("should ignore a GPU whose device ID the plan does not target", func() {
			putTaintedSlice(ctx, "slice-other-dev", "node-other", "0x9999", "0000:02:00.0", deviceTaintKeyReset)

			_, err := reconcilePlan(ctx, planName)
			Expect(err).NotTo(HaveOccurred())

			updated := &intelv1a1.GPURecoveryPlan{}
			Expect(k8sClient.Get(ctx, planKey, updated)).To(Succeed())
			Expect(updated.Status.Events).To(BeEmpty())
			Expect(updated.Status.State).To(Equal(intelv1a1.PlanStateIdle))
		})

		It("should ignore a taint that is not one the operator recovers from", func() {
			putTaintedSlice(ctx, "slice-other-taint", "node-untainted", "0x1234", "0000:02:00.0",
				"health-SomethingElse")

			_, err := reconcilePlan(ctx, planName)
			Expect(err).NotTo(HaveOccurred())

			updated := &intelv1a1.GPURecoveryPlan{}
			Expect(k8sClient.Get(ctx, planKey, updated)).To(Succeed())
			Expect(updated.Status.Events).To(BeEmpty())
		})

		// Detection is a mirror of the cluster, not a log: an event exists for exactly as long as
		// the taint behind it does. Nothing has been done to the GPU yet, so a cleared taint means
		// something else healed it and the plan must stop asking for approval to reset a healthy card.
		It("should drop the event once the taint clears", func() {
			putTaintedSlice(ctx, "slice-clearing", "node-clearing", "0x1234", "0000:02:00.0",
				deviceTaintKeyReset)

			_, err := reconcilePlan(ctx, planName)
			Expect(err).NotTo(HaveOccurred())

			updated := &intelv1a1.GPURecoveryPlan{}
			Expect(k8sClient.Get(ctx, planKey, updated)).To(Succeed())
			Expect(updated.Status.Events).To(HaveLen(1))

			putTaintedSlice(ctx, "slice-clearing", "node-clearing", "0x1234", "0000:02:00.0")

			_, err = reconcilePlan(ctx, planName)
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, planKey, updated)).To(Succeed())
			Expect(updated.Status.Events).To(BeEmpty())
			Expect(updated.Status.State).To(Equal(intelv1a1.PlanStateIdle))
			Expect(updated.Status.Messages).To(ContainElement(ContainSubstring("taint resolved")))
		})
	})

	Context("Reconcile loop – no ResourceSlices", func() {
		It("should succeed with no events and set state to idle", func() {
			_, err := reconcilePlan(ctx, planName)
			Expect(err).NotTo(HaveOccurred())

			updated := &intelv1a1.GPURecoveryPlan{}
			Expect(k8sClient.Get(ctx, planKey, updated)).To(Succeed())
			Expect(updated.Status.State).To(Equal(intelv1a1.PlanStateIdle))
		})

		It("should not requeue when nothing needs recovering", func() {
			result, err := reconcilePlan(ctx, planName)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero())
		})
	})

	Context("Reconcile with non-existent plan", func() {
		It("should return no error for a missing plan", func() {
			_, err := reconcilePlan(ctx, "does-not-exist")
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("ResourceSlice → plan mapping", func() {
		const sliceName = "test-slice"

		AfterEach(func() {
			slice := &resv1.ResourceSlice{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: sliceName}, slice); err == nil {
				Expect(k8sClient.Delete(ctx, slice)).To(Succeed())
			}
		})

		It("should map a matching ResourceSlice to the plan", func() {
			slice := &resv1.ResourceSlice{
				ObjectMeta: metav1.ObjectMeta{Name: sliceName},
				Spec: resv1.ResourceSliceSpec{
					Driver:   "gpu.intel.com",
					NodeName: ptr.To("node01"),
					Pool:     resv1.ResourcePool{Name: "pool01", ResourceSliceCount: 1},
					Devices: []resv1.Device{
						{
							Name: "dev-0000-02-00-0",
							Attributes: map[resv1.QualifiedName]resv1.DeviceAttribute{
								deviceAttrDeviceID: {StringValue: ptr.To("0x1234")},
								deviceAttrBDF:      {StringValue: ptr.To("0000:02:00.0")},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, slice)).To(Succeed())

			r := newTestReconciler()
			reqs := r.resourceSliceToPlans(ctx, slice)
			Expect(reqs).To(HaveLen(1))
			Expect(reqs[0].Name).To(Equal(planName))

			// A cluster-scoped owner must yield a namespace-less request, or the Get in Reconcile
			// would target "default/test-recovery-plan" and silently never match.
			Expect(reqs[0].Namespace).To(BeEmpty())
		})

		It("should return no requests for a slice with a different deviceId", func() {
			slice := &resv1.ResourceSlice{
				ObjectMeta: metav1.ObjectMeta{Name: sliceName},
				Spec: resv1.ResourceSliceSpec{
					Driver:   "gpu.intel.com",
					NodeName: ptr.To("node02"),
					Pool:     resv1.ResourcePool{Name: "pool02", ResourceSliceCount: 1},
					Devices: []resv1.Device{
						{
							Name: "dev-0000-03-00-0",
							Attributes: map[resv1.QualifiedName]resv1.DeviceAttribute{
								deviceAttrDeviceID: {StringValue: ptr.To("0x9999")},
								deviceAttrBDF:      {StringValue: ptr.To("0000:03:00.0")},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, slice)).To(Succeed())

			r := newTestReconciler()
			reqs := r.resourceSliceToPlans(ctx, slice)
			Expect(reqs).To(BeEmpty())
		})

		It("should return no requests for a slice with no device attributes", func() {
			slice := &resv1.ResourceSlice{
				ObjectMeta: metav1.ObjectMeta{Name: sliceName},
				Spec: resv1.ResourceSliceSpec{
					Driver:   "gpu.intel.com",
					NodeName: ptr.To("node03"),
					Pool:     resv1.ResourcePool{Name: "pool03", ResourceSliceCount: 1},
					Devices:  []resv1.Device{{Name: "dev-bare"}},
				},
			}
			Expect(k8sClient.Create(ctx, slice)).To(Succeed())

			r := newTestReconciler()
			reqs := r.resourceSliceToPlans(ctx, slice)
			Expect(reqs).To(BeEmpty())
		})
	})

	// A device with no pciAddress cannot be named in an event, let alone recovered, so it is
	// skipped rather than allowed to produce an event whose GPUBDF is empty — that would collide
	// with every other attribute-less device on the node under findEventForDevice.
	Context("syncRecoveryEventsFromSlices: devices without a BDF", func() {
		It("should skip a tainted device that has no pciAddress attribute", func() {
			slice := &resv1.ResourceSlice{
				ObjectMeta: metav1.ObjectMeta{Name: "slice-no-bdf"},
				Spec: resv1.ResourceSliceSpec{
					Driver:   "gpu.intel.com",
					NodeName: ptr.To("node-no-bdf"),
					Pool:     resv1.ResourcePool{Name: "pool-no-bdf", ResourceSliceCount: 1},
					Devices: []resv1.Device{{
						Name: "dev-no-bdf",
						Attributes: map[resv1.QualifiedName]resv1.DeviceAttribute{
							deviceAttrDeviceID: {StringValue: ptr.To("0xabcd")},
						},
						Taints: []resv1.DeviceTaint{
							{Key: deviceTaintKeyReset, Effect: resv1.DeviceTaintEffectNoSchedule},
						},
					}},
				},
			}
			Expect(k8sClient.Create(ctx, slice)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, slice)
			})

			r := newTestReconciler()
			p := &intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{Name: "plan-no-bdf"},
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DeviceID: "0xabcd", DefaultResetType: intelv1a1.RecoveryTypeSlot,
				},
			}

			Expect(r.syncRecoveryEventsFromSlices(ctx, p)).To(Succeed())
			Expect(p.Status.Events).To(BeEmpty())
		})

		// A BDF that is not a PCI address is dropped at the same point, and for a sharper reason:
		// it would otherwise be interpolated into the command line of a privileged root container.
		// Detection is the only place the value enters the plan, so this is what lets both command
		// builders interpolate it without escaping.
		It("should skip a tainted device whose pciAddress is not a PCI address", func() {
			slice := &resv1.ResourceSlice{
				ObjectMeta: metav1.ObjectMeta{Name: "slice-bad-bdf"},
				Spec: resv1.ResourceSliceSpec{
					Driver:   "gpu.intel.com",
					NodeName: ptr.To("node-bad-bdf"),
					Pool:     resv1.ResourcePool{Name: "pool-bad-bdf", ResourceSliceCount: 1},
					Devices: []resv1.Device{{
						Name: "dev-bad-bdf",
						Attributes: map[resv1.QualifiedName]resv1.DeviceAttribute{
							deviceAttrDeviceID: {StringValue: ptr.To("0xabcd")},
							deviceAttrBDF:      {StringValue: ptr.To("0000:02:00.0; touch /tmp/pwned")},
						},
						Taints: []resv1.DeviceTaint{
							{Key: deviceTaintKeyReset, Effect: resv1.DeviceTaintEffectNoSchedule},
						},
					}},
				},
			}
			Expect(k8sClient.Create(ctx, slice)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, slice)
			})

			r := newTestReconciler()
			p := &intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{Name: "plan-bad-bdf"},
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DeviceID: "0xabcd", DefaultResetType: intelv1a1.RecoveryTypeSlot,
				},
			}

			Expect(r.syncRecoveryEventsFromSlices(ctx, p)).To(Succeed())
			Expect(p.Status.Events).To(BeEmpty())
		})
	})

	// The DRA driver adds the survivability taint alongside the wedged one on a GPU that already
	// has a pending reset event. Driven through syncRecoveryEventsFromSlices with real
	// ResourceSlices rather than by calling escalateEvent directly, because the failure mode is
	// the detection loop skipping such devices outright — a unit test of the escalation helper
	// alone would not catch it.
	Context("syncRecoveryEventsFromSlices: taint escalation", func() {
		const (
			escSlice = "esc-slice"
			escPlan  = "plan-escalation"
			escNode  = "node01"
			escBDF   = "0000:02:00.0"
		)

		putSlice := func(taintKeys ...string) {
			putTaintedSlice(ctx, escSlice, escNode, "0xabcd", escBDF, taintKeys...)
		}

		It("should escalate an existing reset event when the survivability taint appears", func() {
			r := newTestReconciler()
			p := &intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{Name: escPlan},
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot, DeviceID: "0xabcd",
				},
			}

			// Round 1: wedged only → one event of the plan's default reset type.
			putSlice(deviceTaintKeyReset)
			Expect(r.syncRecoveryEventsFromSlices(ctx, p)).To(Succeed())
			Expect(p.Status.Events).To(HaveLen(1))
			Expect(p.Status.Events[0].RecoveryType.Type).To(Equal(intelv1a1.RecoveryTypeSlot))
			Expect(p.Status.Events[0].Reason).To(Equal(reasonWedged))

			// Round 2: the driver adds survivability alongside wedged.
			putSlice(deviceTaintKeyReset, deviceTaintKeyXpumdReflash)
			Expect(r.syncRecoveryEventsFromSlices(ctx, p)).To(Succeed())

			Expect(p.Status.Events).To(HaveLen(1),
				"escalation must happen in place; two events for one GPU would race two recoveries on it")
			Expect(p.Status.Events[0].RecoveryType.Type).To(Equal(intelv1a1.RecoveryTypeReflash))
			Expect(p.Status.Events[0].State).To(Equal(intelv1a1.RecoveryEventStateWaitingApproval))
			Expect(p.Status.Events[0].Reason).To(Equal(reasonSurvivability),
				"the cause must be restated too; a reflash reported as gpu-wedged misleads whoever approves it")
		})

		It("should leave a steady-state event untouched", func() {
			r := newTestReconciler()
			p := &intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{Name: escPlan + "-steady"},
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot, DeviceID: "0xabcd",
				},
			}

			putSlice(deviceTaintKeyReset)
			Expect(r.syncRecoveryEventsFromSlices(ctx, p)).To(Succeed())
			afterFirst := p.DeepCopy()

			Expect(r.syncRecoveryEventsFromSlices(ctx, p)).To(Succeed())
			Expect(p.Status).To(Equal(afterFirst.Status),
				"an unchanged taint set must not rewrite status on every reconcile")
		})
	})

	Context("Helper: findEventForDevice", func() {
		It("should find an event regardless of its recovery type", func() {
			p := &intelv1a1.GPURecoveryPlan{
				Status: intelv1a1.GPURecoveryPlanStatus{
					Events: []intelv1a1.RecoveryEvent{
						{NodeName: "node01", GPUBDF: "0000:02:00.0",
							RecoveryType: intelv1a1.RecoveryTypeSpec{Type: intelv1a1.RecoveryTypeSlot}},
						{NodeName: "node02", GPUBDF: "0000:03:00.0",
							RecoveryType: intelv1a1.RecoveryTypeSpec{Type: intelv1a1.RecoveryTypeReflash}},
					},
				},
			}

			Expect(findEventForDevice(p, "node01", "0000:02:00.0")).To(Equal(0))
			Expect(findEventForDevice(p, "node02", "0000:03:00.0")).To(Equal(1))
			Expect(findEventForDevice(p, "node03", "0000:02:00.0")).To(Equal(-1))
			Expect(findEventForDevice(p, "node01", "0000:09:00.0")).To(Equal(-1))
		})
	})

	Context("Helper: addNewEvents", func() {
		// activeSet builds an activeKeys map of n distinct tainted devices.
		activeSet := func(n int) map[deviceKey]deviceNeed {
			active := make(map[deviceKey]deviceNeed, n)
			for i := 0; i < n; i++ {
				active[deviceKey{node: fmt.Sprintf("node%04d", i), bdf: "0000:02:00.0"}] =
					deviceNeed{rt: intelv1a1.RecoveryTypeSlot, reason: reasonWedged}
			}

			return active
		}

		It("should add an event per newly tainted device", func() {
			p := &intelv1a1.GPURecoveryPlan{}

			addNewEvents(p, activeSet(3))

			Expect(p.Status.Events).To(HaveLen(3))
		})

		// status.events[].reason is the only place the triggering condition is recorded: the
		// taint lives on the ResourceSlice and is gone by the time anyone reads the event.
		It("should record the cause the recovery was derived from", func() {
			p := &intelv1a1.GPURecoveryPlan{}

			addNewEvents(p, map[deviceKey]deviceNeed{
				{node: "node-w", bdf: "0000:02:00.0"}: {
					rt: intelv1a1.RecoveryTypeSlot, reason: reasonWedged,
				},
				{node: "node-s", bdf: "0000:03:00.0"}: {
					rt: intelv1a1.RecoveryTypeReflash, reason: reasonSurvivability,
				},
			})

			byNode := map[string]intelv1a1.RecoveryEvent{}
			for _, evt := range p.Status.Events {
				byNode[evt.NodeName] = evt
			}

			Expect(byNode["node-w"].Reason).To(Equal(reasonWedged))
			Expect(byNode["node-s"].Reason).To(Equal(reasonSurvivability))
		})

		It("should cap events at maxStatusEvents", func() {
			p := &intelv1a1.GPURecoveryPlan{}

			addNewEvents(p, activeSet(maxStatusEvents+10))

			Expect(p.Status.Events).To(HaveLen(maxStatusEvents))
		})

		It("should report the refusal in status.messages rather than dropping silently", func() {
			p := &intelv1a1.GPURecoveryPlan{}

			addNewEvents(p, activeSet(maxStatusEvents+10))

			Expect(p.Status.Messages).NotTo(BeEmpty())
			Expect(p.Status.Messages[len(p.Status.Messages)-1]).To(ContainSubstring("not recorded"))
		})

		// status.events is live state, not a log: an event carries the approval an admin gave it.
		// Evicting entries FIFO-style to make room would discard that.
		It("should keep the existing events when the cap is reached", func() {
			p := &intelv1a1.GPURecoveryPlan{}

			addNewEvents(p, activeSet(maxStatusEvents))
			oldest := p.Status.Events[0]

			addNewEvents(p, map[deviceKey]deviceNeed{
				{node: "brand-new-node", bdf: "0000:03:00.0"}: {
					rt: intelv1a1.RecoveryTypeSlot, reason: reasonWedged,
				},
			})

			Expect(p.Status.Events).To(HaveLen(maxStatusEvents))
			Expect(p.Status.Events[0]).To(Equal(oldest))
		})

		It("should not add a second event for a device that already has one", func() {
			p := &intelv1a1.GPURecoveryPlan{}
			active := activeSet(2)

			addNewEvents(p, active)
			addNewEvents(p, active)

			Expect(p.Status.Events).To(HaveLen(2))
		})
	})

	Context("Helper: removeResolvedEvents", func() {
		var r *GPURecoveryPlanReconciler

		BeforeEach(func() {
			r = newTestReconciler()
		})

		planWithEvents := func(events ...intelv1a1.RecoveryEvent) *intelv1a1.GPURecoveryPlan {
			return &intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{Name: "plan-resolved"},
				Status:     intelv1a1.GPURecoveryPlanStatus{Events: events},
			}
		}

		It("should drop an event whose taint has cleared and say so", func() {
			p := planWithEvents(intelv1a1.RecoveryEvent{
				ID: "evt-gone", NodeName: "node01", GPUBDF: "0000:02:00.0",
				State: intelv1a1.RecoveryEventStateWaitingApproval,
			})

			r.removeResolvedEvents(ctx, p, map[deviceKey]deviceNeed{})

			Expect(p.Status.Events).To(BeEmpty())
			Expect(p.Status.Messages).To(ContainElement(ContainSubstring("evt-gone")))
		})

		It("should keep an event whose taint is still active", func() {
			p := planWithEvents(intelv1a1.RecoveryEvent{
				ID: "evt-stays", NodeName: "node01", GPUBDF: "0000:02:00.0",
				State: intelv1a1.RecoveryEventStateWaitingApproval,
			})

			r.removeResolvedEvents(ctx, p, map[deviceKey]deviceNeed{
				{node: "node01", bdf: "0000:02:00.0"}: {rt: intelv1a1.RecoveryTypeSlot, reason: reasonWedged},
			})

			Expect(p.Status.Events).To(HaveLen(1))
			Expect(p.Status.Messages).To(BeEmpty())
		})

		// The key is (node, BDF): the same BDF on a different node is a different GPU, and a
		// match on the address alone would drop an event that is still needed.
		It("should not match a device on the address alone", func() {
			p := planWithEvents(intelv1a1.RecoveryEvent{
				ID: "evt-other-node", NodeName: "node02", GPUBDF: "0000:02:00.0",
				State: intelv1a1.RecoveryEventStateWaitingApproval,
			})

			r.removeResolvedEvents(ctx, p, map[deviceKey]deviceNeed{
				{node: "node01", bdf: "0000:02:00.0"}: {rt: intelv1a1.RecoveryTypeSlot, reason: reasonWedged},
			})

			Expect(p.Status.Events).To(BeEmpty())
		})
	})
})
