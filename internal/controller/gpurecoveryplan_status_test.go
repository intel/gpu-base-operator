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
	"strings"
	"time"
	"unicode/utf8"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	intelv1a1 "github.com/intel/gpu-base-operator/api/v1alpha1"
)

// Status and spec write-back: what reaches the API server at the end of a pass, and
// what the plan and its events report while it is happening.
var _ = Describe("GPURecoveryPlan Controller: status persistence", func() {
	ctx := context.Background()

	Context("persistPlan: status and spec write-back", func() {
		// The plan carries one consumable approval, so the spec half of persistPlan has something
		// real to write: consuming an approval is the only spec change the operator ever makes.
		newPersistPlan := func(name string) (*GPURecoveryPlanReconciler, *intelv1a1.GPURecoveryPlan) {
			p := &intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{Name: name},
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					DeviceID:         "0xabcd",
					Approvals: []intelv1a1.RecoveryApproval{
						{ID: "app-persist", EventID: "evt-persist-1"},
					},
				},
			}
			Expect(k8sClient.Create(ctx, p)).To(Succeed())

			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, p)
			})

			return newTestReconciler(), p
		}

		It("should write status changes back to the API server", func() {
			r, p := newPersistPlan("plan-persist-status")
			key := types.NamespacedName{Name: p.Name}
			orig := p.DeepCopy()

			p.Status.Messages = []string{"hello"}

			Expect(r.persistPlan(ctx, key, orig, p)).To(Succeed())

			got := &intelv1a1.GPURecoveryPlan{}
			Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
			Expect(got.Status.Messages).To(Equal([]string{"hello"}))
		})

		It("should report a failed status write rather than swallowing it", func() {
			r, p := newPersistPlan("plan-persist-fails")
			key := types.NamespacedName{Name: p.Name}
			orig := p.DeepCopy()

			r.Client = &failingStatusClient{Client: r.Client}

			p.Status.Messages = []string{"dropped"}

			err := r.persistPlan(ctx, key, orig, p)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("updating status"))
		})

		It("should write both status and spec changes back to the API server", func() {
			r, p := newPersistPlan("plan-persist-both")
			key := types.NamespacedName{Name: p.Name}
			orig := p.DeepCopy()

			p.Status.Messages = []string{"hello"}
			p.Spec.Approvals[0].Consumed = true

			Expect(r.persistPlan(ctx, key, orig, p)).To(Succeed())

			got := &intelv1a1.GPURecoveryPlan{}
			Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
			Expect(got.Status.Messages).To(Equal([]string{"hello"}))
			Expect(got.Spec.Approvals[0].Consumed).To(BeTrue())
		})

		It("should still write the spec when the status write fails", func() {
			// Returning early on a status-update error would silently drop consumed=true. That
			// lets the approval match again and reset the same GPU a second time — status is
			// recomputable on the next pass, consumed is not.
			r, p := newPersistPlan("plan-persist-status-fails")
			key := types.NamespacedName{Name: p.Name}
			orig := p.DeepCopy()

			r.Client = &failingStatusClient{Client: r.Client}

			p.Status.Messages = []string{"dropped"}
			p.Spec.Approvals[0].Consumed = true

			err := r.persistPlan(ctx, key, orig, p)
			Expect(err).To(HaveOccurred(), "the status failure must be reported, not swallowed")
			Expect(err.Error()).To(ContainSubstring("updating status"))

			got := &intelv1a1.GPURecoveryPlan{}
			Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
			Expect(got.Spec.Approvals[0].Consumed).To(BeTrue(),
				"consumed=true must survive a failed status write")
		})

		It("should write status before spec", func() {
			// The spec write consumes the approval and immediately triggers another reconcile; that
			// reconcile must not observe a status still describing the event as waiting-approval,
			// or it can act on it a second time.
			r, p := newPersistPlan("plan-persist-order")
			key := types.NamespacedName{Name: p.Name}
			orig := p.DeepCopy()

			writes := []string{}
			r.Client = &recordingClient{Client: r.Client, writes: &writes}

			p.Status.Messages = []string{"hello"}
			p.Spec.Approvals[0].Consumed = true

			Expect(r.persistPlan(ctx, key, orig, p)).To(Succeed())
			Expect(writes).To(Equal([]string{"status", "spec"}))
		})

		It("should recover from a conflict by retrying against the current object", func() {
			// Simulates the routine case: something else updated the plan between our
			// Get and our write, so our cached resourceVersion is stale.
			r, p := newPersistPlan("plan-persist-conflict")
			key := types.NamespacedName{Name: p.Name}
			orig := p.DeepCopy()

			// Bump the object's resourceVersion behind our back.
			other := &intelv1a1.GPURecoveryPlan{}
			Expect(k8sClient.Get(ctx, key, other)).To(Succeed())
			other.Labels = map[string]string{"touched": "yes"}
			Expect(k8sClient.Update(ctx, other)).To(Succeed())

			// p still holds the pre-update resourceVersion, so a naive write conflicts.
			p.Status.Messages = []string{"after-conflict"}

			Expect(r.persistPlan(ctx, key, orig, p)).To(Succeed())

			got := &intelv1a1.GPURecoveryPlan{}
			Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
			Expect(got.Status.Messages).To(Equal([]string{"after-conflict"}))
			Expect(got.Labels).To(HaveKeyWithValue("touched", "yes"),
				"the concurrent change must be preserved, not clobbered")
		})

		It("should do nothing when neither status nor spec changed", func() {
			r, p := newPersistPlan("plan-persist-noop")
			key := types.NamespacedName{Name: p.Name}

			r.Client = &failingStatusClient{Client: r.Client, failUpdate: true}

			// orig == p, so there is nothing to write and neither failing path is hit.
			Expect(r.persistPlan(ctx, key, p.DeepCopy(), p)).To(Succeed())
		})

		// The pass that produced this used to un-consume the approval it had just spent: it was
		// woken by the previous pass's status write, read the spec from a cache that had not caught
		// up with the previous pass's spec write, pruned an unrelated spent approval, and wrote its
		// whole stale spec back — with consumed=false on the approval that had already started a
		// recovery Job. The approval then matched again and reset the same GPU a second time.
		It("must not revert a consumed approval when its own copy of the spec is stale", func() {
			r, p := newPersistPlan("plan-persist-stale")
			key := types.NamespacedName{Name: p.Name}

			// Two approvals: app-persist, which the pass below still believes is unconsumed, and
			// app-spent, which it prunes.
			p.Spec.Approvals = append(p.Spec.Approvals, intelv1a1.RecoveryApproval{
				ID: "app-spent", EventID: "evt-persist-0", Consumed: true,
			})
			Expect(k8sClient.Update(ctx, p)).To(Succeed())

			orig := p.DeepCopy()

			// What the previous pass wrote and this one has not seen.
			ahead := &intelv1a1.GPURecoveryPlan{}
			Expect(k8sClient.Get(ctx, key, ahead)).To(Succeed())
			ahead.Spec.Approvals[0].Consumed = true
			Expect(k8sClient.Update(ctx, ahead)).To(Succeed())

			// This pass prunes app-spent and nothing else.
			pruneConsumedApprovals(p)
			Expect(p.Spec.Approvals).To(HaveLen(1))

			Expect(r.persistPlan(ctx, key, orig, p)).To(Succeed())

			got := &intelv1a1.GPURecoveryPlan{}
			Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
			Expect(got.Spec.Approvals).To(HaveLen(1), "the spent approval must still be pruned")
			Expect(got.Spec.Approvals[0].ID).To(Equal("app-persist"))
			Expect(got.Spec.Approvals[0].Consumed).To(BeTrue(),
				"a consumed approval must not be un-consumed by a write from a stale copy")
		})

		It("must keep an approval added while the pass was running", func() {
			r, p := newPersistPlan("plan-persist-added")
			key := types.NamespacedName{Name: p.Name}
			orig := p.DeepCopy()

			// An admin approves another event after this pass read the plan.
			added := &intelv1a1.GPURecoveryPlan{}
			Expect(k8sClient.Get(ctx, key, added)).To(Succeed())
			added.Spec.Approvals = append(added.Spec.Approvals, intelv1a1.RecoveryApproval{
				ID: "app-late", EventID: "evt-persist-2",
			})
			Expect(k8sClient.Update(ctx, added)).To(Succeed())

			// The pass consumes the approval it matched, knowing nothing of the new one.
			setApprovalConsumed(p, "app-persist")

			Expect(r.persistPlan(ctx, key, orig, p)).To(Succeed())

			got := &intelv1a1.GPURecoveryPlan{}
			Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
			Expect(got.Spec.Approvals).To(HaveLen(2), "the approval added mid-pass must survive")
			Expect(got.Spec.Approvals[0].Consumed).To(BeTrue())
			Expect(got.Spec.Approvals[1].ID).To(Equal("app-late"))
			Expect(got.Spec.Approvals[1].Consumed).To(BeFalse())
		})
	})

	Context("Reconcile: write failures are surfaced", func() {
		It("should return an error when persistPlan cannot write", func() {
			// A write error that is only logged produces a successful reconcile and no retry, so
			// a detected GPU would stay invisible in the CR until something else triggered a
			// reconcile. Only Status().Update is made to fail, and the phases dirty status
			// (state "" -> idle), so the error can only originate in persistPlan.
			p := &intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "plan-reconcile-writefail",
					Finalizers: []string{recoveryPlanFinalizer},
				},
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot, DeviceID: "0xabcd",
				},
			}
			Expect(k8sClient.Create(ctx, p)).To(Succeed())

			DeferCleanup(func() {
				stale := &intelv1a1.GPURecoveryPlan{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: p.Name}, stale); err == nil {
					stale.Finalizers = nil
					_ = k8sClient.Update(ctx, stale)
					_ = k8sClient.Delete(ctx, stale)
				}
			})

			r := newTestReconciler()
			r.Client = &failingStatusClient{Client: r.Client}

			_, err := r.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: p.Name},
			})
			Expect(err).To(HaveOccurred(), "a failed write must fail the reconcile so it is retried")
			Expect(err.Error()).To(ContainSubstring("updating status"),
				"the error must come from persistPlan, not from an earlier phase")
		})
	})

	Context("Helper: appendMessage", func() {
		It("should cap messages at maxStatusMessages", func() {
			p := &intelv1a1.GPURecoveryPlan{}

			for i := 0; i < maxStatusMessages+10; i++ {
				appendMessage(p, fmt.Sprintf("msg - %d", i))
			}

			Expect(p.Status.Messages).To(HaveLen(maxStatusMessages))
		})
	})

	// status.events[].stateMessage exists because the states are coarse — several different
	// situations park an event in the same one — and an admin cannot be expected to reconstruct
	// which is which from a 50-entry ring shared by every event.
	Context("Helper: setEventState", func() {
		// Every spec here starts from waiting-approval, the state an event is in before anything
		// has happened to it. LastUpdated is stamped an hour back so a re-stamp is visible.
		waitingEvent := func() *intelv1a1.RecoveryEvent {
			return &intelv1a1.RecoveryEvent{
				ID:           "evt-msg-001",
				NodeName:     "node01",
				GPUBDF:       "0000:02:00.0",
				RecoveryType: intelv1a1.RecoveryTypeSpec{Type: intelv1a1.RecoveryTypeSlot},
				State:        intelv1a1.RecoveryEventStateWaitingApproval,
				LastUpdated:  ptr.To(metav1.NewTime(time.Now().Add(-time.Hour))),
			}
		}

		It("should drop the previous explanation on a transition that has none of its own", func() {
			evt := waitingEvent()
			evt.StateMessage = "escalated from slot to reflash (survivability-mode)"

			setEventState(evt, intelv1a1.RecoveryEventStateWaitingApproval, "")

			// The whole value of the field is that it describes the state next to it. A message
			// about a transition that has since been superseded is worse than no message at all.
			Expect(evt.StateMessage).To(BeEmpty())
		})

		It("should not re-stamp LastUpdated when neither the state nor the message changes", func() {
			evt := waitingEvent()
			setEventState(evt, intelv1a1.RecoveryEventStateWaitingApproval, "held: %v", "some reason")
			first := *evt.LastUpdated

			setEventState(evt, intelv1a1.RecoveryEventStateWaitingApproval, "held: %v", "some reason")

			// Conditions are re-detected on every pass. Stamping each time would write the object
			// once per reconcile for as long as the condition lasts, and make lastUpdated useless
			// as "when did this event last actually change".
			Expect(*evt.LastUpdated).To(Equal(first))
		})

		It("should truncate on a rune boundary so the status write stays valid UTF-8", func() {
			evt := waitingEvent()

			setEventState(evt, intelv1a1.RecoveryEventStateWaitingApproval, "%s", strings.Repeat("dénï — ", 60))

			Expect(len(evt.StateMessage)).To(BeNumerically("<=", maxStateMessageLen))
			Expect(utf8.ValidString(evt.StateMessage)).To(BeTrue(),
				"the API server rejects a status write containing invalid UTF-8")
		})

		// blocked is the one state an admin can do nothing about and has no other way to diagnose:
		// the event looks stalled, and only the message says it is queued rather than stuck.
		It("should name the recovery holding the node when an event is blocked", func() {
			p := &intelv1a1.GPURecoveryPlan{ObjectMeta: metav1.ObjectMeta{Name: "plan-block-msg"}}
			evt := waitingEvent()

			blockEvent(p, evt, "evt-holder-001")

			Expect(evt.State).To(Equal(intelv1a1.RecoveryEventStateBlocked))
			Expect(evt.StateMessage).To(ContainSubstring("evt-holder-001"))
			Expect(evt.StateMessage).To(ContainSubstring("node01"))
			Expect(evt.StateMessage).To(ContainSubstring("approval retained"))
			Expect(p.Status.Messages).To(HaveLen(1))
			Expect(p.Status.Messages[0]).To(ContainSubstring("evt-msg-001"))
		})
	})

	Context("Helper: updatePlanState", func() {
		// planWith builds a plan carrying the given events, which is all updatePlanState reads.
		planWith := func(events ...intelv1a1.RecoveryEvent) *intelv1a1.GPURecoveryPlan {
			return &intelv1a1.GPURecoveryPlan{
				Spec: intelv1a1.GPURecoveryPlanSpec{DefaultResetType: intelv1a1.RecoveryTypeSlot},
				Status: intelv1a1.GPURecoveryPlanStatus{
					Events: events,
				},
			}
		}
		// pastJobs stands in for the attempts the event has already made, one Job name each.
		evt := func(state intelv1a1.RecoveryEventState, pastJobs ...string) intelv1a1.RecoveryEvent {
			return intelv1a1.RecoveryEvent{State: state, PastJobs: pastJobs}
		}

		It("should report idle with no events", func() {
			p := planWith()
			updatePlanState(p)
			Expect(p.Status.State).To(Equal(intelv1a1.PlanStateIdle))
		})

		DescribeTable("should report active while an event is on its way somewhere",
			func(state intelv1a1.RecoveryEventState) {
				p := planWith(evt(state))
				updatePlanState(p)
				Expect(p.Status.State).To(Equal(intelv1a1.PlanStateActive))
			},
			Entry("waiting for an approval", intelv1a1.RecoveryEventStateWaitingApproval),
			// blocked is active, not stuck: it clears on its own once the node frees up.
			Entry("blocked behind another recovery", intelv1a1.RecoveryEventStateBlocked),
			Entry("draining its node", intelv1a1.RecoveryEventStateDraining),
			Entry("running its recovery", intelv1a1.RecoveryEventStateInProgress),
		)

		It("should report idle once every event has settled", func() {
			p := planWith(evt(intelv1a1.RecoveryEventStateSucceeded, "recovery-0"))
			updatePlanState(p)
			Expect(p.Status.State).To(Equal(intelv1a1.PlanStateIdle))
		})

		// A failed event never restarts on its own, so there is no such thing as a transient
		// failure here: the very first one is already waiting for a person. Reporting anything
		// else — least of all idle, the one word that means no GPU in the cluster needs
		// anything — would hide a broken card behind a state nobody looks twice at.
		DescribeTable("should report error for a failed event",
			func(pastJobs []string) {
				p := planWith(evt(intelv1a1.RecoveryEventStateFailed, pastJobs...))
				updatePlanState(p)
				Expect(p.Status.State).To(Equal(intelv1a1.PlanStateError))
			},
			Entry("on its first attempt", []string{"recovery-0"}),
			Entry("after an admin re-approved it once", []string{"recovery-0", "recovery-1"}),
		)

		It("should report error when a reflash is blocked on missing firmware", func() {
			p := planWith(evt(intelv1a1.RecoveryEventStateMissingFirmware))
			updatePlanState(p)
			Expect(p.Status.State).To(Equal(intelv1a1.PlanStateError),
				"nothing clears this but an operator filling in spec.firmware")
		})

		// An event whose images did not verify is parked in waiting-approval, which normally reads as
		// active. Here it is not: the approval is already there and the plan is what is wrong, so
		// nothing will move until an admin edits it. Reporting active would hide that behind a state
		// that means "working on it".
		It("should report error for an event held on an unpullable image", func() {
			p := planWith(evt(intelv1a1.RecoveryEventStateWaitingApproval))
			p.Generation = 4
			p.Status.Events[0].ImageVerifyGeneration = 4

			updatePlanState(p)

			Expect(p.Status.State).To(Equal(intelv1a1.PlanStateError))
		})

		// The recorded generation is only a verdict on the spec it was made against. Once the spec
		// moves on the check has not been made yet, so the event is genuinely waiting again.
		It("should report active again once the plan has moved past a held generation", func() {
			p := planWith(evt(intelv1a1.RecoveryEventStateWaitingApproval))
			p.Generation = 5
			p.Status.Events[0].ImageVerifyGeneration = 4

			updatePlanState(p)

			Expect(p.Status.State).To(Equal(intelv1a1.PlanStateActive))
		})

		// The whole point of the field is answering "does this need me?" — one healthy event in
		// flight must not mask a stuck one.
		It("should let error outrank active", func() {
			p := planWith(
				evt(intelv1a1.RecoveryEventStateInProgress),
				evt(intelv1a1.RecoveryEventStateFailed, "recovery-0", "recovery-1", "recovery-2"),
			)
			updatePlanState(p)
			Expect(p.Status.State).To(Equal(intelv1a1.PlanStateError))
		})
	})
})
