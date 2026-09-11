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
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	intelv1a1 "github.com/intel/gpu-base-operator/api/v1alpha1"
)

// Approvals: which approval matches which event, what an override may change, and
// when an approval is consumed, pruned, or refused.
var _ = Describe("GPURecoveryPlan Controller: approvals", func() {
	ctx := context.Background()

	Context("Helper: findMatchingApproval", func() {
		var r *GPURecoveryPlanReconciler

		BeforeEach(func() {
			r = newTestReconciler()
		})

		evt := &intelv1a1.RecoveryEvent{
			ID:           "evt-aabb",
			NodeName:     "node05",
			GPUBDF:       "0000:02:00.0",
			RecoveryType: intelv1a1.RecoveryTypeSpec{Type: intelv1a1.RecoveryTypeSBR},
			State:        intelv1a1.RecoveryEventStateWaitingApproval,
		}

		It("should match a singular eventId approval", func() {
			p := &intelv1a1.GPURecoveryPlan{
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					Approvals: []intelv1a1.RecoveryApproval{
						{ID: "app-1234", EventID: "evt-aabb"},
					},
				},
			}
			approval, ok := r.findMatchingApproval(ctx, p, evt)
			Expect(ok).To(BeTrue())
			Expect(approval.ID).To(Equal("app-1234"))
		})

		It("should match a selector approval with matching recoveryType", func() {
			p := &intelv1a1.GPURecoveryPlan{
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					Approvals: []intelv1a1.RecoveryApproval{
						{
							ID:       "app-5678",
							Selector: &intelv1a1.ApprovalSelector{RecoveryType: intelv1a1.RecoveryTypeSBR},
						},
					},
				},
			}
			approval, ok := r.findMatchingApproval(ctx, p, evt)
			Expect(ok).To(BeTrue())
			Expect(approval.ID).To(Equal("app-5678"))
		})

		It("should not match a selector with a different recoveryType", func() {
			p := &intelv1a1.GPURecoveryPlan{
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					Approvals: []intelv1a1.RecoveryApproval{
						{
							Selector: &intelv1a1.ApprovalSelector{RecoveryType: intelv1a1.RecoveryTypeSlot},
						},
					},
				},
			}
			_, ok := r.findMatchingApproval(ctx, p, evt)
			Expect(ok).To(BeFalse())
		})

		It("should not match a selector with a different nodeName", func() {
			p := &intelv1a1.GPURecoveryPlan{
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					Approvals: []intelv1a1.RecoveryApproval{
						{Selector: &intelv1a1.ApprovalSelector{NodeName: "node99"}},
					},
				},
			}
			_, ok := r.findMatchingApproval(ctx, p, evt)
			Expect(ok).To(BeFalse())
		})

		It("should not match a consumed eventId approval", func() {
			p := &intelv1a1.GPURecoveryPlan{
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					Approvals: []intelv1a1.RecoveryApproval{
						{EventID: "evt-aabb", Consumed: true},
					},
				},
			}
			_, ok := r.findMatchingApproval(ctx, p, evt)
			Expect(ok).To(BeFalse())
		})

		It("should not match a consumed selector approval", func() {
			p := &intelv1a1.GPURecoveryPlan{
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					Approvals: []intelv1a1.RecoveryApproval{
						{
							Selector: &intelv1a1.ApprovalSelector{RecoveryType: intelv1a1.RecoveryTypeSBR},
							Consumed: true,
						},
					},
				},
			}
			_, ok := r.findMatchingApproval(ctx, p, evt)
			Expect(ok).To(BeFalse())
		})

		It("should refuse to match an event with no recovery type", func() {
			p := &intelv1a1.GPURecoveryPlan{
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					Approvals: []intelv1a1.RecoveryApproval{
						{ID: "app-any", Selector: &intelv1a1.ApprovalSelector{}},
					},
				},
			}

			_, ok := r.findMatchingApproval(ctx, p, &intelv1a1.RecoveryEvent{
				ID: "evt-typeless", NodeName: "node05", GPUBDF: "0000:02:00.0",
			})
			Expect(ok).To(BeFalse())
		})

		Context("nodeSelector matching", func() {
			// These specs create real Nodes in envtest so selector.nodeSelector is matched against
			// actual Node labels.
			const (
				labelledNode = "node-rack04"
				plainNode    = "node-plain"
			)

			nodeSelectorApproval := func(sel map[string]string) *intelv1a1.GPURecoveryPlan {
				return &intelv1a1.GPURecoveryPlan{
					Spec: intelv1a1.GPURecoveryPlanSpec{
						DefaultResetType: intelv1a1.RecoveryTypeSlot,
						Approvals: []intelv1a1.RecoveryApproval{
							{ID: "app-sel", Selector: &intelv1a1.ApprovalSelector{NodeSelector: sel}},
						},
					},
				}
			}

			eventForNode := func(nodeName string) *intelv1a1.RecoveryEvent {
				return &intelv1a1.RecoveryEvent{
					ID:           "evt-" + nodeName,
					NodeName:     nodeName,
					GPUBDF:       "0000:02:00.0",
					RecoveryType: intelv1a1.RecoveryTypeSpec{Type: intelv1a1.RecoveryTypeSBR},
					State:        intelv1a1.RecoveryEventStateWaitingApproval,
				}
			}

			BeforeEach(func() {
				for name, nodeLabels := range map[string]map[string]string{
					labelledNode: {"rack": "rack-04-32", "gpu": "true"},
					plainNode:    nil,
				} {
					node := &core.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: nodeLabels}}
					if err := k8sClient.Create(ctx, node); err != nil {
						Expect(errors.IsAlreadyExists(err)).To(BeTrue())
					}
				}
			})

			AfterEach(func() {
				for _, name := range []string{labelledNode, plainNode} {
					node := &core.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
					Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, node))).To(Succeed())
				}
			})

			It("should match when the node carries all selector labels", func() {
				p := nodeSelectorApproval(map[string]string{"rack": "rack-04-32"})

				approval, ok := r.findMatchingApproval(ctx, p, eventForNode(labelledNode))
				Expect(ok).To(BeTrue())
				Expect(approval.ID).To(Equal("app-sel"))
			})

			It("should match when every label in a multi-label selector is present", func() {
				p := nodeSelectorApproval(map[string]string{"rack": "rack-04-32", "gpu": "true"})

				_, ok := r.findMatchingApproval(ctx, p, eventForNode(labelledNode))
				Expect(ok).To(BeTrue())
			})

			It("should NOT match an event on a node without the selector labels", func() {
				// Without node-label matching this returns true, silently widening a rack-scoped
				// approval cluster-wide.
				p := nodeSelectorApproval(map[string]string{"rack": "rack-04-32"})

				_, ok := r.findMatchingApproval(ctx, p, eventForNode(plainNode))
				Expect(ok).To(BeFalse())
			})

			It("should NOT match when the label value differs", func() {
				p := nodeSelectorApproval(map[string]string{"rack": "rack-99-01"})

				_, ok := r.findMatchingApproval(ctx, p, eventForNode(labelledNode))
				Expect(ok).To(BeFalse())
			})

			It("should NOT match when only some of the selector labels are present", func() {
				p := nodeSelectorApproval(map[string]string{"rack": "rack-04-32", "zone": "west"})

				_, ok := r.findMatchingApproval(ctx, p, eventForNode(labelledNode))
				Expect(ok).To(BeFalse())
			})

			It("should fail closed when the node does not exist", func() {
				// A node that cannot be read must never be treated as matching, otherwise a
				// transient API error would widen the approval's scope.
				p := nodeSelectorApproval(map[string]string{"rack": "rack-04-32"})

				_, ok := r.findMatchingApproval(ctx, p, eventForNode("node-does-not-exist"))
				Expect(ok).To(BeFalse())
			})

			It("should still honour an empty nodeSelector as match-anything", func() {
				p := nodeSelectorApproval(nil)

				_, ok := r.findMatchingApproval(ctx, p, eventForNode(plainNode))
				Expect(ok).To(BeTrue())
			})

			It("should require both nodeSelector and recoveryType to match", func() {
				p := &intelv1a1.GPURecoveryPlan{
					Spec: intelv1a1.GPURecoveryPlanSpec{
						DefaultResetType: intelv1a1.RecoveryTypeSlot,
						Approvals: []intelv1a1.RecoveryApproval{
							{
								ID: "app-both",
								Selector: &intelv1a1.ApprovalSelector{
									RecoveryType: intelv1a1.RecoveryTypeSlot,
									NodeSelector: map[string]string{"rack": "rack-04-32"},
								},
							},
						},
					},
				}

				// Labels match but the recovery type (SBR) does not.
				_, ok := r.findMatchingApproval(ctx, p, eventForNode(labelledNode))
				Expect(ok).To(BeFalse())
			})
		})
	})

	Context("Helper: applyOverride", func() {
		var r *GPURecoveryPlanReconciler

		BeforeEach(func() {
			r = newTestReconciler()
		})

		It("should up-level a defaulted SBR event to the admin-requested type and record SuggestedType", func() {
			p := &intelv1a1.GPURecoveryPlan{ObjectMeta: metav1.ObjectMeta{Name: "plan-override"}}
			evt := &intelv1a1.RecoveryEvent{
				ID:           "evt-override-1",
				RecoveryType: intelv1a1.RecoveryTypeSpec{Type: intelv1a1.RecoveryTypeSBR},
			}
			approval := intelv1a1.RecoveryApproval{
				ID:       "app-override",
				Override: &intelv1a1.RecoveryOverride{RecoveryType: intelv1a1.RecoveryTypeSlot},
			}

			r.applyOverride(p, evt, approval)

			Expect(evt.RecoveryType.Type).To(Equal(intelv1a1.RecoveryTypeSlot))
			Expect(evt.RecoveryType.SuggestedType).To(Equal(intelv1a1.RecoveryTypeSBR))
			Expect(buildResetCommand("0000:02:00.0", evt.RecoveryType.Type)).To(
				ContainSubstring("--coldreset"), "an override must change the command actually run")
		})

		It("should be a no-op when the approval has no override", func() {
			p := &intelv1a1.GPURecoveryPlan{}
			evt := &intelv1a1.RecoveryEvent{
				RecoveryType: intelv1a1.RecoveryTypeSpec{Type: intelv1a1.RecoveryTypeSBR},
			}

			r.applyOverride(p, evt, intelv1a1.RecoveryApproval{ID: "app-no-override"})

			Expect(evt.RecoveryType.Type).To(Equal(intelv1a1.RecoveryTypeSBR))
			Expect(evt.RecoveryType.SuggestedType).To(BeEmpty())
		})

		It("should ignore an override on a reflash event", func() {
			p := &intelv1a1.GPURecoveryPlan{}
			evt := &intelv1a1.RecoveryEvent{
				RecoveryType: intelv1a1.RecoveryTypeSpec{Type: intelv1a1.RecoveryTypeReflash},
			}
			approval := intelv1a1.RecoveryApproval{
				Override: &intelv1a1.RecoveryOverride{RecoveryType: intelv1a1.RecoveryTypeSBR},
			}

			r.applyOverride(p, evt, approval)

			Expect(evt.RecoveryType.Type).To(Equal(intelv1a1.RecoveryTypeReflash))
			Expect(evt.RecoveryType.SuggestedType).To(BeEmpty())
		})

		It("should ignore an override that requests reflash for a reset event", func() {
			p := &intelv1a1.GPURecoveryPlan{}
			evt := &intelv1a1.RecoveryEvent{
				RecoveryType: intelv1a1.RecoveryTypeSpec{Type: intelv1a1.RecoveryTypeSBR},
			}
			approval := intelv1a1.RecoveryApproval{
				Override: &intelv1a1.RecoveryOverride{RecoveryType: intelv1a1.RecoveryTypeReflash},
			}

			r.applyOverride(p, evt, approval)

			Expect(evt.RecoveryType.Type).To(Equal(intelv1a1.RecoveryTypeSBR))
			Expect(evt.RecoveryType.SuggestedType).To(BeEmpty())
		})

		// SuggestedType is the audit trail of what the operator itself picked, so a second
		// override must not overwrite it with the first override's choice.
		It("should record SuggestedType only once across repeated overrides", func() {
			p := &intelv1a1.GPURecoveryPlan{}
			evt := &intelv1a1.RecoveryEvent{
				RecoveryType: intelv1a1.RecoveryTypeSpec{Type: intelv1a1.RecoveryTypeSBR},
			}

			r.applyOverride(p, evt, intelv1a1.RecoveryApproval{
				Override: &intelv1a1.RecoveryOverride{RecoveryType: intelv1a1.RecoveryTypeSlot},
			})
			r.applyOverride(p, evt, intelv1a1.RecoveryApproval{
				Override: &intelv1a1.RecoveryOverride{RecoveryType: intelv1a1.RecoveryTypeAMC},
			})

			Expect(evt.RecoveryType.Type).To(Equal(intelv1a1.RecoveryTypeAMC))
			Expect(evt.RecoveryType.SuggestedType).To(Equal(intelv1a1.RecoveryTypeSBR),
				"suggestedType must keep the type the operator detected, not the previous override")
		})
	})

	Context("processApprovals: approval consumption", func() {
		It("should set Consumed=true on the approval after a non-persistent selector approval fires", func() {
			r := newTestReconciler()

			p := &intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{Name: "plan-consume-test"},
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					DeviceID:         "0xabcd",
					XpuSmi:           intelv1a1.XpuSmiSpec{Image: "registry/xpu-smi:latest"},
					// The subject here is which approval fires and when it is spent, so the drain
					// is switched off: with it on, an approved event stops at draining and the
					// answer would depend on Node and Pod fixtures that say nothing about
					// approvals. The drain's own effect on consumption is covered with the drain.
					Drain: intelv1a1.DrainSpec{Enable: ptr.To(false)},
					Approvals: []intelv1a1.RecoveryApproval{
						{
							ID:         "sel-nonpersist",
							Selector:   &intelv1a1.ApprovalSelector{RecoveryType: intelv1a1.RecoveryTypeSBR},
							Persistent: false,
						},
					},
				},
				Status: intelv1a1.GPURecoveryPlanStatus{
					Events: []intelv1a1.RecoveryEvent{
						{
							ID:           "evt-consume-1",
							NodeName:     "node01",
							GPUBDF:       "0000:02:00.0",
							RecoveryType: intelv1a1.RecoveryTypeSpec{Type: intelv1a1.RecoveryTypeSBR},
							State:        intelv1a1.RecoveryEventStateWaitingApproval,
						},
					},
				},
			}

			// Recovery Jobs are owned by the plan, and an owner reference needs a UID — so the plan
			// must exist in the API server, as it always does in production (Reconcile only ever
			// operates on an object it just Got).
			createPlanForOwnerRef(p)

			r.processApprovals(ctx, p)

			Expect(p.Spec.Approvals[0].Consumed).To(BeTrue(),
				"non-persistent selector approval must be marked consumed after firing")

			// A second call with a new event must not match the now-consumed approval.
			p.Status.Events = append(p.Status.Events, intelv1a1.RecoveryEvent{
				ID:           "evt-consume-2",
				NodeName:     "node01",
				GPUBDF:       "0000:03:00.0",
				RecoveryType: intelv1a1.RecoveryTypeSpec{Type: intelv1a1.RecoveryTypeSBR},
				State:        intelv1a1.RecoveryEventStateWaitingApproval,
			})

			r.processApprovals(ctx, p)

			var secondEvt *intelv1a1.RecoveryEvent

			for i := range p.Status.Events {
				if p.Status.Events[i].ID == "evt-consume-2" {
					secondEvt = &p.Status.Events[i]

					break
				}
			}

			Expect(secondEvt).NotTo(BeNil())
			Expect(secondEvt.State).To(Equal(intelv1a1.RecoveryEventStateWaitingApproval),
				"second event must remain waiting-approval because the approval was already consumed")
		})

		// Consumption is deferred to the end of the loop so one approval covers everything
		// currently waiting. An admin approving "any sbr" for a node with three wedged GPUs means
		// all three, not whichever event happens to be first in status.events.
		It("should let one non-persistent approval fire for every event waiting in the same pass", func() {
			r := newTestReconciler()

			p := &intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{Name: "plan-consume-batch"},
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					DeviceID:         "0xabcd",
					XpuSmi:           intelv1a1.XpuSmiSpec{Image: "registry/xpu-smi:latest"},
					Drain:            intelv1a1.DrainSpec{Enable: ptr.To(false)},
					Approvals: []intelv1a1.RecoveryApproval{
						{
							ID:       "sel-batch",
							Selector: &intelv1a1.ApprovalSelector{RecoveryType: intelv1a1.RecoveryTypeSBR},
						},
					},
				},
				Status: intelv1a1.GPURecoveryPlanStatus{
					Events: []intelv1a1.RecoveryEvent{
						{
							ID: "evt-batch-1", NodeName: "node01", GPUBDF: "0000:02:00.0",
							RecoveryType: intelv1a1.RecoveryTypeSpec{Type: intelv1a1.RecoveryTypeSBR},
							State:        intelv1a1.RecoveryEventStateWaitingApproval,
						},
						{
							// On a second node, not a second GPU of node01: only one recovery runs
							// per node, so same-node siblings would come out blocked and the subject
							// here would become the node gate rather than the approval. Its effect on
							// batching is asserted with the gate itself.
							ID: "evt-batch-2", NodeName: "node02", GPUBDF: "0000:03:00.0",
							RecoveryType: intelv1a1.RecoveryTypeSpec{Type: intelv1a1.RecoveryTypeSBR},
							State:        intelv1a1.RecoveryEventStateWaitingApproval,
						},
					},
				},
			}

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

			r.processApprovals(ctx, p)

			for i := range p.Status.Events {
				Expect(p.Status.Events[i].State).To(Equal(intelv1a1.RecoveryEventStateInProgress),
					"event %s should have started under the same approval", p.Status.Events[i].ID)
				Expect(p.Status.Events[i].ApprovalID).To(Equal("sel-batch"))
			}

			Expect(p.Spec.Approvals[0].Consumed).To(BeTrue())
		})

		It("should not consume a persistent approval", func() {
			r := newTestReconciler()

			p := &intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{Name: "plan-consume-persistent"},
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					DeviceID:         "0xabcd",
					XpuSmi: intelv1a1.XpuSmiSpec{
						Image: "local/xpusmi:devel",
					},
					Drain: intelv1a1.DrainSpec{Enable: ptr.To(false)},
					Approvals: []intelv1a1.RecoveryApproval{
						{
							ID:         "sel-persistent",
							Selector:   &intelv1a1.ApprovalSelector{RecoveryType: intelv1a1.RecoveryTypeSBR},
							Persistent: true,
						},
					},
				},
				Status: intelv1a1.GPURecoveryPlanStatus{
					Events: []intelv1a1.RecoveryEvent{
						{
							ID: "evt-persistent-1", NodeName: "node01", GPUBDF: "0000:02:00.0",
							RecoveryType: intelv1a1.RecoveryTypeSpec{Type: intelv1a1.RecoveryTypeSBR},
							State:        intelv1a1.RecoveryEventStateWaitingApproval,
						},
					},
				},
			}

			createPlanForOwnerRef(p)

			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, &batch.Job{ObjectMeta: metav1.ObjectMeta{
					Name: "recovery-evt-persistent-1-0", Namespace: "default",
				}})
			})

			r.processApprovals(ctx, p)

			Expect(p.Status.Events[0].State).To(Equal(intelv1a1.RecoveryEventStateInProgress))
			Expect(p.Spec.Approvals[0].Consumed).To(BeFalse(),
				"a persistent approval is standing policy and must survive firing")
		})

		// A reflash the operator cannot build a Job for must leave its approval unspent: consuming it
		// would spend the admin's decision on nothing, and they would have to approve again once
		// spec.firmware told the operator what to flash.
		It("should park a reflash with no firmware configured without consuming the approval", func() {
			r := newTestReconciler()

			p := &intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{Name: "plan-reflash-park"},
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					DeviceID:         "0xabcd",
					Approvals: []intelv1a1.RecoveryApproval{
						{ID: "app-reflash", EventID: "evt-reflash-park"},
					},
				},
				Status: intelv1a1.GPURecoveryPlanStatus{
					Events: []intelv1a1.RecoveryEvent{
						{
							ID: "evt-reflash-park", NodeName: "node01", GPUBDF: "0000:02:00.0",
							RecoveryType: intelv1a1.RecoveryTypeSpec{Type: intelv1a1.RecoveryTypeReflash},
							Reason:       reasonSurvivability,
							State:        intelv1a1.RecoveryEventStateWaitingApproval,
						},
					},
				},
			}

			createPlanForOwnerRef(p)

			r.processApprovals(ctx, p)

			evt := p.Status.Events[0]
			Expect(evt.State).To(Equal(intelv1a1.RecoveryEventStateMissingFirmware))
			Expect(evt.JobName).To(BeEmpty())
			Expect(evt.StateMessage).To(ContainSubstring("spec.firmware"))
			Expect(p.Spec.Approvals[0].Consumed).To(BeFalse(),
				"an approval that produced no Job must stay available")

			job := &batch.Job{}
			err := k8sClient.Get(ctx, types.NamespacedName{
				Name: "recovery-evt-reflash-park-0", Namespace: "default",
			}, job)
			Expect(errors.IsNotFound(err)).To(BeTrue(),
				"a parked reflash must not leave a Job behind")
		})

		// The other half of the above: once spec.firmware says what to flash, the retained approval
		// carries the reflash through on the next pass without a second admin action.
		It("should consume the approval once the reflash Job is created", func() {
			r := newTestReconciler()

			p := &intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{Name: "plan-reflash-resume"},
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					DeviceID:         "0xabcd",
					XpuSmi:           intelv1a1.XpuSmiSpec{Image: "registry/xpu-smi:latest"},
					Firmware: &intelv1a1.FirmwareSpec{
						Source: intelv1a1.FirmwareSource{
							ContainerSource: &intelv1a1.ContainerFirmwareSource{Name: "registry/fw:v2"},
						},
						File: "gfx.bin",
					},
					Approvals: []intelv1a1.RecoveryApproval{
						{ID: "app-reflash-resume", EventID: "evt-reflash-resume"},
					},
				},
				Status: intelv1a1.GPURecoveryPlanStatus{
					Events: []intelv1a1.RecoveryEvent{
						{
							ID: "evt-reflash-resume", NodeName: "node01", GPUBDF: "0000:02:00.0",
							RecoveryType: intelv1a1.RecoveryTypeSpec{Type: intelv1a1.RecoveryTypeReflash},
							Reason:       reasonSurvivability,
							// Where the spec above left the event: parked, approval still in place.
							State: intelv1a1.RecoveryEventStateMissingFirmware,
						},
					},
				},
			}

			createPlanForOwnerRef(p)

			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, &batch.Job{ObjectMeta: metav1.ObjectMeta{
					Name: "recovery-evt-reflash-resume-0", Namespace: "default",
				}})
			})

			r.processApprovals(ctx, p)

			evt := p.Status.Events[0]
			Expect(evt.State).To(Equal(intelv1a1.RecoveryEventStateInProgress),
				"missing-firmware must be re-examined once the plan is corrected; messages: %v", p.Status.Messages)
			Expect(evt.JobName).To(Equal("recovery-evt-reflash-resume-0"))
			Expect(p.Spec.Approvals[0].Consumed).To(BeTrue(),
				"the retained approval is spent by the reflash it eventually authorised")
		})
	})

	Context("Re-approval of permanently failed events", func() {
		It("should restart a failed event when an explicit EventID approval is added", func() {
			r := newTestReconciler()
			now := metav1.Now()

			p := &intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{Name: "plan-reapprove"},
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					DeviceID:         "0xabcd",
					XpuSmi:           intelv1a1.XpuSmiSpec{Image: "registry/xpu-smi:latest"},
					// The drain is off so the re-approved event lands straight in in-progress;
					// what is under test is the approval and the attempt history, not the drain.
					Drain: intelv1a1.DrainSpec{Enable: ptr.To(false)},
					Approvals: []intelv1a1.RecoveryApproval{
						{ID: "reapp-001", EventID: "evt-exhausted"},
					},
				},
				Status: intelv1a1.GPURecoveryPlanStatus{
					Events: []intelv1a1.RecoveryEvent{
						{
							ID:           "evt-exhausted",
							NodeName:     "node02",
							GPUBDF:       "0000:03:00.0",
							RecoveryType: intelv1a1.RecoveryTypeSpec{Type: intelv1a1.RecoveryTypeSBR},
							State:        intelv1a1.RecoveryEventStateFailed,
							// Two attempts already failed.
							PastJobs:    []string{"recovery-evt-exhausted-0", "recovery-evt-exhausted-1"},
							LastUpdated: &now,
							ApprovalID:  "old-approval",
						},
					},
				},
			}

			createPlanForOwnerRef(p)

			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, &batch.Job{ObjectMeta: metav1.ObjectMeta{
					Name: "recovery-evt-exhausted-2", Namespace: "default",
				}})
			})

			r.processApprovals(ctx, p)

			evt := &p.Status.Events[0]
			Expect(evt.ApprovalID).To(Equal("reapp-001"))
			Expect(evt.JobName).To(Equal("recovery-evt-exhausted-2"),
				"the re-approved attempt gets a Job of its own, named after its attempt index")
			// Re-approval falls through into the same pass, so the Job starts immediately rather
			// than waiting for another reconcile.
			Expect(evt.State).To(Equal(intelv1a1.RecoveryEventStateInProgress))
			Expect(p.Spec.Approvals[0].Consumed).To(BeTrue())
		})

		It("should not restart a failed event via a selector approval", func() {
			r := newTestReconciler()
			now := metav1.Now()

			p := &intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{Name: "plan-no-selector-restart"},
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					DeviceID:         "0xabcd",
					XpuSmi:           intelv1a1.XpuSmiSpec{Image: "registry/xpu-smi:latest"},
					Approvals: []intelv1a1.RecoveryApproval{
						{
							ID:         "sel-approval",
							Selector:   &intelv1a1.ApprovalSelector{RecoveryType: intelv1a1.RecoveryTypeSBR},
							Persistent: true,
						},
					},
				},
				Status: intelv1a1.GPURecoveryPlanStatus{
					Events: []intelv1a1.RecoveryEvent{
						{
							ID:           "evt-perm-failed",
							NodeName:     "node03",
							GPUBDF:       "0000:04:00.0",
							RecoveryType: intelv1a1.RecoveryTypeSpec{Type: intelv1a1.RecoveryTypeSBR},
							State:        intelv1a1.RecoveryEventStateFailed,
							// Two attempts already failed.
							PastJobs:    []string{"recovery-evt-perm-failed-0", "recovery-evt-perm-failed-1"},
							LastUpdated: &now,
						},
					},
				},
			}

			r.processApprovals(ctx, p)

			// State must remain failed — a standing approval must not keep retrying a GPU whose
			// recovery it has already authorised once and watched fail.
			Expect(p.Status.Events[0].State).To(Equal(intelv1a1.RecoveryEventStateFailed))
		})

		It("findExplicitApprovalForEvent should only match EventID approvals", func() {
			r := newTestReconciler()

			evt := &intelv1a1.RecoveryEvent{ID: "evt-xyz", State: intelv1a1.RecoveryEventStateFailed}

			// Should match an explicit EventID approval.
			p := &intelv1a1.GPURecoveryPlan{
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					Approvals: []intelv1a1.RecoveryApproval{
						{ID: "explicit-1", EventID: "evt-xyz"},
					},
				},
			}
			approval, ok := r.findExplicitApprovalForEvent(p, evt)
			Expect(ok).To(BeTrue())
			Expect(approval.ID).To(Equal("explicit-1"))

			// Should NOT match a consumed EventID approval.
			pConsumed := &intelv1a1.GPURecoveryPlan{
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					Approvals: []intelv1a1.RecoveryApproval{
						{ID: "explicit-2", EventID: "evt-xyz", Consumed: true},
					},
				},
			}
			_, ok = r.findExplicitApprovalForEvent(pConsumed, evt)
			Expect(ok).To(BeFalse())

			// Should NOT match a selector-only approval.
			pSelector := &intelv1a1.GPURecoveryPlan{
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					Approvals: []intelv1a1.RecoveryApproval{
						{
							ID:       "sel-1",
							Selector: &intelv1a1.ApprovalSelector{RecoveryType: intelv1a1.RecoveryTypeSBR},
						},
					},
				},
			}
			_, ok = r.findExplicitApprovalForEvent(pSelector, evt)
			Expect(ok).To(BeFalse())
		})
	})

	Context("pruneConsumedApprovals", func() {
		It("should remove a consumed non-persistent approval when no event references it", func() {
			p := &intelv1a1.GPURecoveryPlan{
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					Approvals: []intelv1a1.RecoveryApproval{
						{
							ID:       "consumed-gone",
							Selector: &intelv1a1.ApprovalSelector{RecoveryType: intelv1a1.RecoveryTypeSBR},
							Consumed: true,
						},
						{ID: "active", EventID: "evt-1"},
					},
				},
				// No events reference "consumed-gone".
				Status: intelv1a1.GPURecoveryPlanStatus{
					Events: []intelv1a1.RecoveryEvent{{ID: "evt-1", ApprovalID: "active"}},
				},
			}

			pruneConsumedApprovals(p)

			Expect(p.Spec.Approvals).To(HaveLen(1))
			Expect(p.Spec.Approvals[0].ID).To(Equal("active"))
		})

		It("should keep a consumed non-persistent approval while an event still references it", func() {
			p := &intelv1a1.GPURecoveryPlan{
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					Approvals: []intelv1a1.RecoveryApproval{
						{
							ID:       "sel-used",
							Selector: &intelv1a1.ApprovalSelector{RecoveryType: intelv1a1.RecoveryTypeSBR},
							Consumed: true,
						},
					},
				},
				Status: intelv1a1.GPURecoveryPlanStatus{
					Events: []intelv1a1.RecoveryEvent{
						{ID: "evt-active", ApprovalID: "sel-used", State: intelv1a1.RecoveryEventStateInProgress},
					},
				},
			}

			pruneConsumedApprovals(p)

			Expect(p.Spec.Approvals).To(HaveLen(1),
				"consumed approval must be kept while its event is still active")
		})

		It("should never prune a persistent approval", func() {
			p := &intelv1a1.GPURecoveryPlan{
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					Approvals: []intelv1a1.RecoveryApproval{
						{
							ID:         "persistent-sel",
							Selector:   &intelv1a1.ApprovalSelector{RecoveryType: intelv1a1.RecoveryTypeSBR},
							Persistent: true,
							// Consumed is deliberately true: persistent approvals are never pruned
							// whatever else is set on them.
							Consumed: true,
						},
					},
				},
				Status: intelv1a1.GPURecoveryPlanStatus{}, // no events
			}

			pruneConsumedApprovals(p)

			Expect(p.Spec.Approvals).To(HaveLen(1), "persistent approvals must never be pruned")
		})

		It("should keep unconsumed approvals regardless of event references", func() {
			p := &intelv1a1.GPURecoveryPlan{
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					Approvals: []intelv1a1.RecoveryApproval{
						{ID: "pending", EventID: "evt-future"},
					},
				},
				Status: intelv1a1.GPURecoveryPlanStatus{}, // no events yet
			}

			pruneConsumedApprovals(p)

			Expect(p.Spec.Approvals).To(HaveLen(1), "unconsumed approvals must not be pruned")
		})
	})
})
