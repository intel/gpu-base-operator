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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batch "k8s.io/api/batch/v1"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	intelv1a1 "github.com/intel/gpu-base-operator/api/v1alpha1"
)

// Node-level exclusion: one recovery at a time per node, with everything else on
// that node held back rather than started alongside it.
var _ = Describe("GPURecoveryPlan Controller: node-level recovery exclusion", func() {
	ctx := context.Background()

	// Two recoveries must never overlap on one node: a PCIe reset fired while another card on the
	// same host is mid-write can leave that card unrecoverable. A second approved event therefore
	// parks in blocked, keeps the authorisation it already matched, and starts by itself once the
	// node frees up — no second decision from the admin.
	Context("Node-level recovery exclusion", func() {
		const (
			busyNode  = "busy-node-1"
			otherNode = "other-node-1"
			bdfA      = "0000:02:00.0"
			bdfB      = "0000:03:00.0"
			bdfC      = "0000:04:00.0"
		)

		var r *GPURecoveryPlanReconciler

		BeforeEach(func() {
			r = newTestReconciler()
		})

		// A slot reset: the recovery type is incidental to the gate, which serialises whatever the
		// node is asked to do. The one case where it matters — a reflash, which never drains and so
		// holds no taint — builds its event itself, in the taint context below.
		evtOn := func(id, node, bdf string, state intelv1a1.RecoveryEventState) intelv1a1.RecoveryEvent {
			return intelv1a1.RecoveryEvent{
				ID:           id,
				NodeName:     node,
				GPUBDF:       bdf,
				RecoveryType: intelv1a1.RecoveryTypeSpec{Type: intelv1a1.RecoveryTypeSlot},
				State:        state,
			}
		}

		// planWithEvents installs persistent selector approvals for every recovery type, so an
		// event is approved on sight and the gate is the only thing left deciding what happens to
		// it. Consumption is deliberately out of the picture here — it has its own spec below.
		//
		// The drain is off for the same reason: with it on an approved event stops at draining and
		// the assertions would depend on Node and Pod fixtures that say nothing about the gate. The
		// interaction with the drain is asserted separately, in the taint context.
		planWithEvents := func(name string, events ...intelv1a1.RecoveryEvent) *intelv1a1.GPURecoveryPlan {
			p := &intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{Name: name},
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					DeviceID:         "0x1234",
					XpuSmi:           intelv1a1.XpuSmiSpec{Image: "registry/xpu-smi:latest"},
					Drain:            intelv1a1.DrainSpec{Enable: ptr.To(false)},
					Approvals: []intelv1a1.RecoveryApproval{
						{
							ID:         "app-any-reset",
							Selector:   &intelv1a1.ApprovalSelector{RecoveryType: intelv1a1.RecoveryTypeSlot},
							Persistent: true,
						},
						{
							ID:         "app-any-sbr",
							Selector:   &intelv1a1.ApprovalSelector{RecoveryType: intelv1a1.RecoveryTypeSBR},
							Persistent: true,
						},
					},
				},
				Status: intelv1a1.GPURecoveryPlanStatus{Events: events},
			}

			// Recovery Jobs are owned by the plan, and an owner reference needs a UID.
			createPlanForOwnerRef(p)

			DeferCleanup(func() {
				for _, evt := range p.Status.Events {
					if evt.JobName != "" {
						_ = k8sClient.Delete(ctx, &batch.Job{ObjectMeta: metav1.ObjectMeta{
							Name: evt.JobName, Namespace: "default",
						}})
					}
				}
			})

			return p
		}

		eventByID := func(p *intelv1a1.GPURecoveryPlan, id string) *intelv1a1.RecoveryEvent {
			for i := range p.Status.Events {
				if p.Status.Events[i].ID == id {
					return &p.Status.Events[i]
				}
			}

			return nil
		}

		DescribeTable("should hold back an approved recovery while another owns the node",
			func(holderState intelv1a1.RecoveryEventState) {
				p := planWithEvents("plan-gate-"+strings.ReplaceAll(string(holderState), "-", ""),
					evtOn("evt-holder", busyNode, bdfA, holderState),
					evtOn("evt-queued", busyNode, bdfB, intelv1a1.RecoveryEventStateWaitingApproval),
				)

				r.processApprovals(ctx, p)

				queued := eventByID(p, "evt-queued")
				Expect(queued.State).To(Equal(intelv1a1.RecoveryEventStateBlocked))
				Expect(queued.StateMessage).To(ContainSubstring("evt-holder"))
				Expect(queued.JobName).To(BeEmpty(),
					"a blocked event must not have started a Job")

				// The authorisation is recorded even though nothing ran: that is what lets the event
				// resume without the admin approving it a second time.
				Expect(queued.ApprovalID).To(Equal("app-any-reset"))
				Expect(queued.ApprovalMatchedAt).NotTo(BeNil())

				Expect(eventByID(p, "evt-holder").State).To(Equal(holderState),
					"the recovery that owns the node must be left alone")
			},
			Entry("holder has a Job running", intelv1a1.RecoveryEventStateInProgress),
			// A drain is as much a hold on the node as a reset is: the pods are already going, and
			// starting a second recovery would reset a card while the first node is being emptied.
			Entry("holder is draining the node", intelv1a1.RecoveryEventStateDraining),
		)

		DescribeTable("should not block behind a sibling that is no longer running",
			func(siblingState intelv1a1.RecoveryEventState) {
				p := planWithEvents("plan-nogate-"+strings.ReplaceAll(string(siblingState), "-", ""),
					evtOn("evt-done", busyNode, bdfA, siblingState),
					evtOn("evt-next", busyNode, bdfB, intelv1a1.RecoveryEventStateWaitingApproval),
				)

				r.processApprovals(ctx, p)

				next := eventByID(p, "evt-next")
				Expect(next.State).To(Equal(intelv1a1.RecoveryEventStateInProgress),
					"a terminal sibling holds nothing; the node is free")
				Expect(next.JobName).NotTo(BeEmpty())
			},
			Entry("sibling succeeded", intelv1a1.RecoveryEventStateSucceeded),
			Entry("sibling failed", intelv1a1.RecoveryEventStateFailed),
		)

		It("should not block a recovery on a different node", func() {
			p := planWithEvents("plan-gate-other-node",
				evtOn("evt-busy", busyNode, bdfA, intelv1a1.RecoveryEventStateInProgress),
				evtOn("evt-elsewhere", otherNode, bdfA, intelv1a1.RecoveryEventStateWaitingApproval),
			)

			r.processApprovals(ctx, p)

			// The rule is per node, not per plan: serialising the whole cluster behind one recovery
			// would make a plan spanning a hundred nodes useless.
			elsewhere := eventByID(p, "evt-elsewhere")
			Expect(elsewhere.State).To(Equal(intelv1a1.RecoveryEventStateInProgress))
			Expect(elsewhere.JobName).NotTo(BeEmpty())
		})

		DescribeTable("nodeBusyWith should count only operations actually in flight",
			func(siblingState intelv1a1.RecoveryEventState, expectBusy bool) {
				p := &intelv1a1.GPURecoveryPlan{
					Status: intelv1a1.GPURecoveryPlanStatus{
						Events: []intelv1a1.RecoveryEvent{
							evtOn("evt-sibling", busyNode, bdfA, siblingState),
							evtOn("evt-subject", busyNode, bdfB, intelv1a1.RecoveryEventStateWaitingApproval),
						},
					},
				}

				blocker, busy := nodeBusyWith(p, &p.Status.Events[1])

				Expect(busy).To(Equal(expectBusy))

				if expectBusy {
					Expect(blocker).To(Equal("evt-sibling"),
						"the caller records the blocker in the state message, so it must be named")
				} else {
					Expect(blocker).To(BeEmpty())
				}
			},
			Entry("in-progress", intelv1a1.RecoveryEventStateInProgress, true),
			Entry("draining", intelv1a1.RecoveryEventStateDraining, true),
			Entry("waiting-approval", intelv1a1.RecoveryEventStateWaitingApproval, false),
			// Counting blocked would deadlock a queue of three: the second would hold the third,
			// and none of them would ever be the one to start.
			Entry("blocked", intelv1a1.RecoveryEventStateBlocked, false),
			Entry("missing-firmware", intelv1a1.RecoveryEventStateMissingFirmware, false),
			Entry("succeeded", intelv1a1.RecoveryEventStateSucceeded, false),
			Entry("failed", intelv1a1.RecoveryEventStateFailed, false),
		)

		// The self-check is what stops a running event from being re-examined as its own blocker,
		// which would park it in blocked behind itself and abandon the Job it already has.
		It("should not let an event block itself", func() {
			p := &intelv1a1.GPURecoveryPlan{
				Status: intelv1a1.GPURecoveryPlanStatus{
					Events: []intelv1a1.RecoveryEvent{
						evtOn("evt-only", busyNode, bdfA, intelv1a1.RecoveryEventStateInProgress),
					},
				},
			}

			_, busy := nodeBusyWith(p, &p.Status.Events[0])
			Expect(busy).To(BeFalse())
		})

		It("should start exactly one of several queued recoveries on a node", func() {
			p := planWithEvents("plan-gate-queue",
				evtOn("evt-q1", busyNode, bdfA, intelv1a1.RecoveryEventStateWaitingApproval),
				evtOn("evt-q2", busyNode, bdfB, intelv1a1.RecoveryEventStateWaitingApproval),
				evtOn("evt-q3", busyNode, bdfC, intelv1a1.RecoveryEventStateWaitingApproval),
			)

			r.processApprovals(ctx, p)

			started, blocked := 0, 0

			for _, evt := range p.Status.Events {
				switch evt.State {
				case intelv1a1.RecoveryEventStateInProgress:
					started++
				case intelv1a1.RecoveryEventStateBlocked:
					blocked++
				}
			}

			// All three are approved in the same pass, so the gate has to hold within a single call
			// and not merely across reconciles: nodeBusyWith reads the states the loop has already
			// written, not the states it started with.
			Expect(started).To(Equal(1), "exactly one recovery may run on a node")
			Expect(blocked).To(Equal(2))
		})

		It("should start a blocked recovery once the node frees up", func() {
			p := planWithEvents("plan-gate-resume",
				evtOn("evt-first", busyNode, bdfA, intelv1a1.RecoveryEventStateInProgress),
				evtOn("evt-second", busyNode, bdfB, intelv1a1.RecoveryEventStateWaitingApproval),
			)

			r.processApprovals(ctx, p)
			Expect(eventByID(p, "evt-second").State).To(Equal(intelv1a1.RecoveryEventStateBlocked))

			By("finishing the recovery that held the node")
			eventByID(p, "evt-first").State = intelv1a1.RecoveryEventStateSucceeded

			r.processApprovals(ctx, p)

			second := eventByID(p, "evt-second")
			Expect(second.State).To(Equal(intelv1a1.RecoveryEventStateInProgress),
				"a blocked event must resume on its own, without a new approval")
			Expect(second.JobName).NotTo(BeEmpty())

			// The message described the hold, which is over. Leaving it would have the event read
			// as queued behind a recovery that finished.
			Expect(second.StateMessage).NotTo(ContainSubstring("evt-first"))
		})

		// The gate sits after applyOverride so that an admin's choice of reset mechanism is decided
		// once, when the approval matches, and not re-derived on the pass that happens to find the
		// node free — by which time the approval carrying the override may have been pruned.
		It("should keep an overridden reset type on an event it holds back", func() {
			p := planWithEvents("plan-gate-override",
				evtOn("evt-busy", busyNode, bdfA, intelv1a1.RecoveryEventStateInProgress),
				evtOn("evt-override", busyNode, bdfB, intelv1a1.RecoveryEventStateWaitingApproval),
			)
			p.Spec.Approvals = []intelv1a1.RecoveryApproval{{
				ID:      "app-override",
				EventID: "evt-override",
				Override: &intelv1a1.RecoveryOverride{
					RecoveryType: intelv1a1.RecoveryTypeSBR,
				},
			}}

			r.processApprovals(ctx, p)

			blocked := eventByID(p, "evt-override")
			Expect(blocked.State).To(Equal(intelv1a1.RecoveryEventStateBlocked))
			Expect(blocked.RecoveryType.Type).To(Equal(intelv1a1.RecoveryTypeSBR))
			Expect(blocked.RecoveryType.SuggestedType).To(Equal(intelv1a1.RecoveryTypeSlot))
		})

		// status.messages is capped at maxStatusMessages, and a blocked event is re-examined on
		// every reconcile. One message per pass would rotate out everything worth reading well
		// before the hold ended.
		It("should report the hold once, not once per reconcile", func() {
			p := planWithEvents("plan-gate-message-once",
				evtOn("evt-busy", busyNode, bdfA, intelv1a1.RecoveryEventStateInProgress),
				evtOn("evt-held", busyNode, bdfB, intelv1a1.RecoveryEventStateWaitingApproval),
			)

			for range 5 {
				r.processApprovals(ctx, p)
			}

			held := 0

			for _, msg := range p.Status.Messages {
				if strings.Contains(msg, "held back") {
					held++
				}
			}

			Expect(held).To(Equal(1), "messages: %v", p.Status.Messages)
			Expect(eventByID(p, "evt-held").State).To(Equal(intelv1a1.RecoveryEventStateBlocked))
		})

		It("should requeue while an event is blocked", func() {
			p := &intelv1a1.GPURecoveryPlan{
				Status: intelv1a1.GPURecoveryPlanStatus{
					Events: []intelv1a1.RecoveryEvent{
						evtOn("evt-held", busyNode, bdfB, intelv1a1.RecoveryEventStateBlocked),
					},
				},
			}

			// The sibling that holds the node reaches succeeded in syncJobStatuses, a phase after
			// processApprovals has already re-blocked this event. So on the pass that frees the node
			// nothing else is in flight, and without this the freed recovery would sit until an
			// unrelated ResourceSlice or Job event happened along.
			Expect(hasActiveJobs(p)).To(BeTrue())
		})

		It("should report the plan as active while an event is blocked", func() {
			p := &intelv1a1.GPURecoveryPlan{
				Status: intelv1a1.GPURecoveryPlanStatus{
					Events: []intelv1a1.RecoveryEvent{
						evtOn("evt-held", busyNode, bdfB, intelv1a1.RecoveryEventStateBlocked),
					},
				},
			}

			updatePlanState(p)

			// Not error: nothing is wrong and nobody has to intervene. The hold clears by itself.
			Expect(p.Status.State).To(Equal(intelv1a1.PlanStateActive))
		})

		It("should drop a blocked event when its device taint clears", func() {
			p := planWithEvents("plan-gate-resolved",
				evtOn("evt-held", busyNode, bdfB, intelv1a1.RecoveryEventStateBlocked),
			)

			// Nothing was done to the GPU, so a cleared taint means the queued reset is no longer
			// wanted — running it later would reset a working card.
			r.removeResolvedEvents(ctx, p, map[deviceKey]deviceNeed{})

			Expect(p.Status.Events).To(BeEmpty())
		})

		// The taint is what stops the scheduler refilling the node in the gap between the holding
		// recovery finishing and this one's drain starting. Without it a queued reset opens by
		// evicting pods that were admitted while nothing held the node.
		Context("drain taint while blocked", func() {
			makeNode := func(name string) {
				node := &core.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
				Expect(k8sClient.Create(ctx, node)).To(Succeed())
				DeferCleanup(func() {
					fresh := &core.Node{}
					if err := k8sClient.Get(ctx, types.NamespacedName{Name: name}, fresh); err == nil {
						fresh.Spec.Taints = nil
						_ = k8sClient.Update(ctx, fresh)
						_ = k8sClient.Delete(ctx, fresh)
					}
				})
			}

			// taintNode puts the plan's drain taint on the node, as a drain would have, so the
			// specs observe whether reconcileDrainTaints keeps it or lifts it.
			taintNode := func(name, planName string) {
				node := &core.Node{}
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name}, node)).To(Succeed())
				node.Spec.Taints = append(node.Spec.Taints, recoveryTaint(planName))
				Expect(k8sClient.Update(ctx, node)).To(Succeed())
			}

			hasRecoveryTaint := func(name, planName string) bool {
				node := &core.Node{}
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name}, node)).To(Succeed())

				want := recoveryTaint(planName)
				for _, t := range node.Spec.Taints {
					if t.Key == want.Key && t.Value == want.Value {
						return true
					}
				}

				return false
			}

			blockedPlan := func(name, node string, rt intelv1a1.RecoveryType, drain bool) *intelv1a1.GPURecoveryPlan {
				return &intelv1a1.GPURecoveryPlan{
					ObjectMeta: metav1.ObjectMeta{Name: name},
					Spec: intelv1a1.GPURecoveryPlanSpec{
						DefaultResetType: intelv1a1.RecoveryTypeSlot,
						DeviceID:         "0x1234",
						Drain:            intelv1a1.DrainSpec{Enable: ptr.To(drain), TimeoutSeconds: 300},
					},
					Status: intelv1a1.GPURecoveryPlanStatus{
						Events: []intelv1a1.RecoveryEvent{{
							ID:           "evt-held",
							NodeName:     node,
							GPUBDF:       bdfB,
							RecoveryType: intelv1a1.RecoveryTypeSpec{Type: rt},
							State:        intelv1a1.RecoveryEventStateBlocked,
						}},
					},
				}
			}

			It("should keep the node tainted for a blocked reset", func() {
				const (
					nodeName = "gate-taint-keep"
					planName = "plan-gate-taint-keep"
				)

				makeNode(nodeName)
				taintNode(nodeName, planName)

				r.reconcileDrainTaints(ctx, blockedPlan(planName, nodeName, intelv1a1.RecoveryTypeSlot, true))

				Expect(hasRecoveryTaint(nodeName, planName)).To(BeTrue())
			})

			// A reflash never drains, so a taint held on its behalf is one nothing would ask for and
			// nothing on the reflash path would remove.
			It("should not keep the node tainted for a blocked reflash", func() {
				const (
					nodeName = "gate-taint-reflash"
					planName = "plan-gate-taint-reflash"
				)

				makeNode(nodeName)
				taintNode(nodeName, planName)

				r.reconcileDrainTaints(ctx, blockedPlan(planName, nodeName, intelv1a1.RecoveryTypeReflash, true))

				Expect(hasRecoveryTaint(nodeName, planName)).To(BeFalse())
			})

			It("should not keep the node tainted for a blocked reset when the drain is off", func() {
				const (
					nodeName = "gate-taint-nodrain"
					planName = "plan-gate-taint-nodrain"
				)

				makeNode(nodeName)
				taintNode(nodeName, planName)

				r.reconcileDrainTaints(ctx, blockedPlan(planName, nodeName, intelv1a1.RecoveryTypeSlot, false))

				Expect(hasRecoveryTaint(nodeName, planName)).To(BeFalse())
			})
		})

		// A one-shot approval covering several GPUs on one node is only partly spent when the first
		// of them starts. Consuming it there would leave its blocked siblings unmatchable —
		// findMatchingApproval skips consumed approvals — and they would sit in blocked forever,
		// contradicting the "approval retained" guarantee the state exists to give.
		Context("one-shot approvals with a blocked event", func() {
			It("should not consume an approval that still has a blocked event", func() {
				p := planWithEvents("plan-gate-oneshot",
					evtOn("evt-one", busyNode, bdfA, intelv1a1.RecoveryEventStateWaitingApproval),
					evtOn("evt-two", busyNode, bdfB, intelv1a1.RecoveryEventStateWaitingApproval),
				)

				// Replaces the persistent approvals the fixture installs: a persistent approval is
				// never consumed, so it could not show what this spec is about.
				p.Spec.Approvals = []intelv1a1.RecoveryApproval{{
					ID:       "app-oneshot",
					Selector: &intelv1a1.ApprovalSelector{RecoveryType: intelv1a1.RecoveryTypeSlot},
				}}

				r.processApprovals(ctx, p)

				Expect(eventByID(p, "evt-one").State).To(Equal(intelv1a1.RecoveryEventStateInProgress))
				Expect(eventByID(p, "evt-two").State).To(Equal(intelv1a1.RecoveryEventStateBlocked))
				Expect(p.Spec.Approvals[0].Consumed).To(BeFalse(),
					"the approval still owes the blocked event a recovery")

				By("finishing the first recovery")
				eventByID(p, "evt-one").State = intelv1a1.RecoveryEventStateSucceeded

				r.processApprovals(ctx, p)

				Expect(eventByID(p, "evt-two").State).To(Equal(intelv1a1.RecoveryEventStateInProgress),
					"the retained approval must start the second recovery")
				Expect(p.Spec.Approvals[0].Consumed).To(BeTrue(),
					"with nothing left queued, the approval is finally spent")
			})
		})

		It("should accept blocked in the CRD schema", func() {
			const planName = "plan-gate-crd"

			p := &intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{Name: planName},
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					DeviceID:         "0x1234",
				},
			}
			Expect(k8sClient.Create(ctx, p)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, p)
			})

			p.Status.Events = []intelv1a1.RecoveryEvent{{
				ID:           "evt-crd-blocked",
				NodeName:     busyNode,
				GPUBDF:       bdfA,
				RecoveryType: intelv1a1.RecoveryTypeSpec{Type: intelv1a1.RecoveryTypeSlot},
				State:        intelv1a1.RecoveryEventStateBlocked,
				LastUpdated:  ptr.To(metav1.NewTime(time.Now())),
			}}
			p.Status.State = intelv1a1.PlanStateActive

			// The enum is declared twice, on the type alias and on the field. A value the controller
			// writes but the schema rejects would fail every status write for the whole plan.
			Expect(k8sClient.Status().Update(ctx, p)).To(Succeed())

			fresh := &intelv1a1.GPURecoveryPlan{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: planName}, fresh)).To(Succeed())
			Expect(fresh.Status.Events[0].State).To(Equal(intelv1a1.RecoveryEventStateBlocked))
		})
	})
})
