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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	intelv1a1 "github.com/intel/gpu-base-operator/api/v1alpha1"
)

// How an event gets its recovery type: derived from the taint and the plan's default,
// then escalated when a worse taint appears.
var _ = Describe("GPURecoveryPlan Controller: recovery type selection", func() {
	ctx := context.Background()

	Context("Helper: taintToDeviceNeed", func() {
		It("should map a reset taint key to the plan's default reset type", func() {
			need, ok := taintToDeviceNeed(deviceTaintKeyReset, intelv1a1.RecoveryTypeSlot)
			Expect(ok).To(BeTrue())
			Expect(need.rt).To(Equal(intelv1a1.RecoveryTypeSlot))
			Expect(need.reason).To(Equal(reasonWedged))

			// The DRA driver cannot say which reset mechanism applies, so on a platform
			// without PCIe hot-plug the only thing that can select AMC is this field.
			need, ok = taintToDeviceNeed(deviceTaintKeyReset, intelv1a1.RecoveryTypeAMC)
			Expect(ok).To(BeTrue())
			Expect(need.rt).To(Equal(intelv1a1.RecoveryTypeAMC))
			Expect(need.reason).To(Equal(reasonWedged),
				"the mechanism changed, not the fault; the reason must still read gpu-wedged")
		})

		// The field is mandatory, so an empty value means an object that never reached the API
		// server. Falling back matters because an empty type produces a malformed event ID. SBR
		// rather than slot or amc: guessing between those two is guessing at the platform.
		It("should fall back to SBR when the plan carries no default reset type", func() {
			need, ok := taintToDeviceNeed(deviceTaintKeyReset, "")
			Expect(ok).To(BeTrue())
			Expect(need.rt).To(Equal(intelv1a1.RecoveryTypeSBR))
		})

		DescribeTable("should map a survivability taint key to Reflash",
			func(taintKey string) {
				need, ok := taintToDeviceNeed(taintKey, intelv1a1.RecoveryTypeSlot)
				Expect(ok).To(BeTrue())
				Expect(need.rt).To(Equal(intelv1a1.RecoveryTypeReflash))
				Expect(need.reason).To(Equal(reasonSurvivability))
			},
			Entry("applied by the driver at enumeration", deviceTaintKeyReflash),
			Entry("applied on xpumd's behalf at runtime", deviceTaintKeyXpumdReflash),
		)

		// A reflash need comes from the device's own state, not from the platform's reset
		// mechanism, so the default must not reach it.
		It("should not let the default reset type influence a reflash taint", func() {
			need, ok := taintToDeviceNeed(deviceTaintKeyXpumdReflash, intelv1a1.RecoveryTypeAMC)
			Expect(ok).To(BeTrue())
			Expect(need.rt).To(Equal(intelv1a1.RecoveryTypeReflash))
		})

		It("should return false for an unrelated taint key", func() {
			_, ok := taintToDeviceNeed("some.other/taint", intelv1a1.RecoveryTypeSlot)
			Expect(ok).To(BeFalse())
		})

		It("should prioritize survivability over wedged when both taints are present", func() {
			wedged := deviceNeed{rt: intelv1a1.RecoveryTypeSBR, reason: reasonWedged}
			surv := deviceNeed{rt: intelv1a1.RecoveryTypeReflash, reason: reasonSurvivability}

			// The reason must travel with the type, or the event would report the reflash
			// alongside "gpu-wedged".
			Expect(higherPriorityNeed(wedged, surv)).To(Equal(surv))
			Expect(higherPriorityNeed(surv, wedged)).To(Equal(surv))
		})

		It("should prioritize any known over empty", func() {
			known := deviceNeed{rt: intelv1a1.RecoveryTypeSBR, reason: reasonWedged}
			empty := deviceNeed{}

			Expect(higherPriorityNeed(known, empty)).To(Equal(known))
			Expect(higherPriorityNeed(empty, known)).To(Equal(known))
		})
	})

	// Which reset works is a property of the platform (hot-plug capable slots → the slot power
	// cycle, otherwise AMC), and the DRA driver only reports "needs a reset".
	// spec.defaultResetType is the only thing that can tell the two apart.
	Context("spec.defaultResetType", func() {
		const (
			drtSlice = "drt-slice"
			drtNode  = "node-drt"
			drtBDF   = "0000:04:00.0"
			drtDevID = "0xabcd"
		)

		putDrtSlice := func(taintKeys ...string) {
			putTaintedSlice(ctx, drtSlice, drtNode, drtDevID, drtBDF, taintKeys...)
		}

		newDrtPlan := func(name string, rt intelv1a1.RecoveryType) *intelv1a1.GPURecoveryPlan {
			return &intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{Name: name},
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DeviceID:         drtDevID,
					DefaultResetType: rt,
				},
			}
		}

		DescribeTable("should type a new reset event from the plan's default",
			func(rt intelv1a1.RecoveryType) {
				r := newTestReconciler()
				p := newDrtPlan("plan-drt-"+string(rt), rt)

				putDrtSlice(deviceTaintKeyReset)
				Expect(r.syncRecoveryEventsFromSlices(ctx, p)).To(Succeed())

				Expect(p.Status.Events).To(HaveLen(1))
				evt := p.Status.Events[0]
				Expect(evt.RecoveryType.Type).To(Equal(rt))
				Expect(evt.ID).To(ContainSubstring("-"+string(rt)+"-"),
					"the type is part of the ID, so an admin approving by name can see what will run")
			},
			Entry("slot, where the PCIe slots support hot-plug", intelv1a1.RecoveryTypeSlot),
			Entry("amc, where they do not", intelv1a1.RecoveryTypeAMC),
		)

		// The field is a default for events created afterwards, not a retroactive rewrite. An
		// event that is already waiting for approval keeps its type, because changing it
		// regenerates the ID and thereby invalidates any approval naming the old one — a
		// correction to the plan must not silently discard an approval an admin already gave.
		It("should leave a waiting-approval slot event alone when the default becomes amc", func() {
			r := newTestReconciler()
			p := newDrtPlan("plan-drt-slot-to-amc", intelv1a1.RecoveryTypeSlot)

			putDrtSlice(deviceTaintKeyReset)
			Expect(r.syncRecoveryEventsFromSlices(ctx, p)).To(Succeed())
			Expect(p.Status.Events).To(HaveLen(1))
			origID := p.Status.Events[0].ID

			p.Spec.DefaultResetType = intelv1a1.RecoveryTypeAMC
			Expect(r.syncRecoveryEventsFromSlices(ctx, p)).To(Succeed())

			Expect(p.Status.Events).To(HaveLen(1))
			Expect(p.Status.Events[0].RecoveryType.Type).To(Equal(intelv1a1.RecoveryTypeSlot))
			Expect(p.Status.Events[0].ID).To(Equal(origID))
		})

		// The mirror image of the spec above, and the one that constrains recoveryTypePriority:
		// escalateEvent re-types any event whose need outranks it, so if the resets were ranked
		// against each other a default change would silently re-type waiting events and void their
		// approvals. The resets are alternatives selected by platform, not a severity ladder.
		It("should leave a waiting-approval amc event alone when the default becomes slot", func() {
			r := newTestReconciler()
			p := newDrtPlan("plan-drt-amc-to-slot", intelv1a1.RecoveryTypeAMC)

			putDrtSlice(deviceTaintKeyReset)
			Expect(r.syncRecoveryEventsFromSlices(ctx, p)).To(Succeed())
			Expect(p.Status.Events).To(HaveLen(1))
			origID := p.Status.Events[0].ID

			p.Spec.DefaultResetType = intelv1a1.RecoveryTypeSlot
			Expect(r.syncRecoveryEventsFromSlices(ctx, p)).To(Succeed())

			Expect(p.Status.Events).To(HaveLen(1))
			Expect(p.Status.Events[0].RecoveryType.Type).To(Equal(intelv1a1.RecoveryTypeAMC),
				"a default that only applies to new events must not re-type one that is already waiting")
			Expect(p.Status.Events[0].ID).To(Equal(origID),
				"a regenerated ID would invalidate an approval the admin already granted")
		})

		// Reflash still has to outrank a reset, whichever reset the platform uses: a card in
		// survivability mode also reports wedged, and no reset revives it.
		It("should still escalate a reset event to reflash on an amc plan", func() {
			r := newTestReconciler()
			p := newDrtPlan("plan-drt-amc-escalate", intelv1a1.RecoveryTypeAMC)

			putDrtSlice(deviceTaintKeyReset)
			Expect(r.syncRecoveryEventsFromSlices(ctx, p)).To(Succeed())
			Expect(p.Status.Events[0].RecoveryType.Type).To(Equal(intelv1a1.RecoveryTypeAMC))

			putDrtSlice(deviceTaintKeyReset, deviceTaintKeyXpumdReflash)
			Expect(r.syncRecoveryEventsFromSlices(ctx, p)).To(Succeed())

			Expect(p.Status.Events).To(HaveLen(1))
			Expect(p.Status.Events[0].RecoveryType.Type).To(Equal(intelv1a1.RecoveryTypeReflash))
			Expect(p.Status.Events[0].Reason).To(Equal(reasonSurvivability))
		})

		It("should rank the three resets equally and only reflash above them", func() {
			base := recoveryTypePriority(intelv1a1.RecoveryTypeSBR)
			Expect(recoveryTypePriority(intelv1a1.RecoveryTypeSlot)).To(Equal(base))
			Expect(recoveryTypePriority(intelv1a1.RecoveryTypeAMC)).To(Equal(base))
			Expect(recoveryTypePriority(intelv1a1.RecoveryTypeReflash)).To(BeNumerically(">", base))
			Expect(recoveryTypePriority(intelv1a1.RecoveryType("flr"))).To(BeNumerically("<", base),
				"a type outside the enum must not outrank a real one")
		})
	})

	Context("Helper: escalateEvent", func() {
		// waitingEvent builds a single-device event of the given type, waiting for approval, with
		// approval bookkeeping and two failed attempts already present so escalation can be seen
		// to clear the approval and keep the attempt history.
		waitingEvent := func(rt intelv1a1.RecoveryType) *intelv1a1.RecoveryEvent {
			now := metav1.Now()

			return &intelv1a1.RecoveryEvent{
				ID:                generateEventID("node01", "0000:02:00.0", rt),
				NodeName:          "node01",
				GPUBDF:            "0000:02:00.0",
				Reason:            reasonWedged,
				RecoveryType:      intelv1a1.RecoveryTypeSpec{Type: rt},
				State:             intelv1a1.RecoveryEventStateWaitingApproval,
				PastJobs:          []string{"recovery-old-0", "recovery-old-1"},
				ApprovalID:        "app-old",
				ApprovalMatchedAt: ptr.To(now),
				LastUpdated:       ptr.To(now),
			}
		}

		It("should escalate a waiting-approval slot event to reflash", func() {
			p := &intelv1a1.GPURecoveryPlan{ObjectMeta: metav1.ObjectMeta{Name: "plan-esc"}}
			evt := waitingEvent(intelv1a1.RecoveryTypeSlot)

			escalateEvent(p, evt, deviceNeed{rt: intelv1a1.RecoveryTypeReflash, reason: reasonSurvivability})

			Expect(evt.RecoveryType.Type).To(Equal(intelv1a1.RecoveryTypeReflash))
			Expect(evt.ID).To(Equal("evt-node01-reflash-0000-02-00-0"))
			Expect(evt.Reason).To(Equal(reasonSurvivability),
				"status.events[].reason is what an admin reads before approving; it must track the escalation")
			Expect(p.Status.Messages).To(ContainElement(ContainSubstring("escalated")))
		})

		// The new ID is what stops a reset approval from silently authorising a reflash, so it is
		// asserted as behaviour rather than left implicit in the ID format.
		It("should invalidate an approval naming the pre-escalation event", func() {
			p := &intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{Name: "plan-esc-approval"},
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					Approvals: []intelv1a1.RecoveryApproval{
						{ID: "app-slot", EventID: "evt-node01-slot-0000-02-00-0"},
					},
				},
			}
			evt := waitingEvent(intelv1a1.RecoveryTypeSlot)

			// The approval names this event before escalation...
			Expect(evt.ID).To(Equal(p.Spec.Approvals[0].EventID))

			escalateEvent(p, evt, deviceNeed{rt: intelv1a1.RecoveryTypeReflash, reason: reasonSurvivability})

			// ...and must not after, or approving a reset would run a reflash.
			Expect(evt.ID).NotTo(Equal(p.Spec.Approvals[0].EventID),
				"a slot-reset approval must not carry over to the escalated reflash event")
		})

		It("should reset the approval state, keeping the attempt history", func() {
			p := &intelv1a1.GPURecoveryPlan{ObjectMeta: metav1.ObjectMeta{Name: "plan-esc-reset"}}
			evt := waitingEvent(intelv1a1.RecoveryTypeSlot)

			escalateEvent(p, evt, deviceNeed{rt: intelv1a1.RecoveryTypeReflash, reason: reasonSurvivability})

			Expect(evt.State).To(Equal(intelv1a1.RecoveryEventStateWaitingApproval))
			Expect(evt.ApprovalID).To(BeEmpty())
			Expect(evt.ApprovalMatchedAt).To(BeNil())
			Expect(evt.PastJobs).To(ConsistOf("recovery-old-0", "recovery-old-1"),
				"the Jobs of the earlier attempts stay listed for diagnostics")
		})

		// A reset that could not pull xpu-smi says nothing about the firmware image the escalated
		// reflash also needs. Carrying the recorded generation across would suppress the check on an
		// image that has never been looked at, and the reflash would run — or not — on a guess.
		It("should re-check the images of the escalated operation", func() {
			p := &intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{Name: "plan-esc-imgcheck", Generation: 7},
			}
			evt := waitingEvent(intelv1a1.RecoveryTypeSlot)
			evt.ImageVerifyGeneration = 7

			escalateEvent(p, evt, deviceNeed{rt: intelv1a1.RecoveryTypeReflash, reason: reasonSurvivability})

			Expect(evt.ImageVerifyGeneration).To(BeZero())
			// Clearing the hold must not take the escalation message with it.
			Expect(evt.StateMessage).To(ContainSubstring("escalated"))
		})

		// The ID changing is invisible on the event itself, and it is the reason an approval an
		// admin already granted has stopped applying. Nothing else says so.
		It("should say on the event why the previous approval no longer applies", func() {
			p := &intelv1a1.GPURecoveryPlan{ObjectMeta: metav1.ObjectMeta{Name: "plan-esc-msg"}}
			evt := waitingEvent(intelv1a1.RecoveryTypeSlot)

			escalateEvent(p, evt, deviceNeed{rt: intelv1a1.RecoveryTypeReflash, reason: reasonSurvivability})

			Expect(evt.StateMessage).To(SatisfyAll(
				ContainSubstring(string(intelv1a1.RecoveryTypeSlot)),
				ContainSubstring(string(intelv1a1.RecoveryTypeReflash)),
			))
		})

		It("should not downgrade reflash to a reset", func() {
			p := &intelv1a1.GPURecoveryPlan{ObjectMeta: metav1.ObjectMeta{Name: "plan-esc-down"}}
			evt := waitingEvent(intelv1a1.RecoveryTypeReflash)
			before := evt.DeepCopy()

			escalateEvent(p, evt, deviceNeed{rt: intelv1a1.RecoveryTypeSlot, reason: reasonWedged})

			Expect(evt).To(Equal(before), "escalation is one-way; act on the worst condition")
		})

		It("should be a no-op when the type is unchanged", func() {
			p := &intelv1a1.GPURecoveryPlan{ObjectMeta: metav1.ObjectMeta{Name: "plan-esc-same"}}
			evt := waitingEvent(intelv1a1.RecoveryTypeSlot)
			before := evt.DeepCopy()

			escalateEvent(p, evt, deviceNeed{rt: intelv1a1.RecoveryTypeSlot, reason: reasonWedged})

			Expect(evt).To(Equal(before))
			Expect(p.Status.Messages).To(BeEmpty(), "a steady-state reconcile must not log an escalation")
		})

		// Escalation must converge: repeated reconciles of the same escalated device must not
		// keep rewriting the event, or every reconcile would emit a status write and a message.
		It("should be idempotent across repeated reconciles", func() {
			p := &intelv1a1.GPURecoveryPlan{ObjectMeta: metav1.ObjectMeta{Name: "plan-esc-idem"}}
			evt := waitingEvent(intelv1a1.RecoveryTypeSlot)

			escalateEvent(p, evt, deviceNeed{rt: intelv1a1.RecoveryTypeReflash, reason: reasonSurvivability})
			afterFirst := evt.DeepCopy()
			msgCount := len(p.Status.Messages)

			escalateEvent(p, evt, deviceNeed{rt: intelv1a1.RecoveryTypeReflash, reason: reasonSurvivability})

			Expect(evt).To(Equal(afterFirst))
			Expect(p.Status.Messages).To(HaveLen(msgCount))
		})
	})
})
