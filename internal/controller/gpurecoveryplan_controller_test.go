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
	resv1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	intelv1a1 "github.com/intel/gpu-base-operator/api/v1alpha1"
)

// failingStatusClient makes the status write fail on demand so the error path in persistPlan can
// be driven.
type failingStatusClient struct {
	client.Client
}

func (c *failingStatusClient) Status() client.SubResourceWriter {
	return &failingSubResourceWriter{SubResourceWriter: c.Client.Status()}
}

type failingSubResourceWriter struct {
	client.SubResourceWriter
}

func (w *failingSubResourceWriter) Update(
	ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption,
) error {
	return fmt.Errorf("synthetic status update failure")
}

// newTestReconciler builds a GPURecoveryPlanReconciler wired to the shared test client.
func newTestReconciler() *GPURecoveryPlanReconciler {
	return &GPURecoveryPlanReconciler{
		Client: k8sClient,
		Scheme: k8sClient.Scheme(),
		Opts: ControllerOpts{
			Namespace:    "default",
			RequeueDelay: 2 * time.Second,
		},
	}
}

// reconcilePlan runs a single reconcile cycle for the named GPURecoveryPlan.
func reconcilePlan(ctx context.Context, name string) (reconcile.Result, error) {
	return newTestReconciler().Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: name},
	})
}

// putTaintedSlice creates or replaces a single-device ResourceSlice carrying the given taint
// keys, and registers its deletion. Detection reads taints off real ResourceSlices, so the
// specs drive it through the API server rather than through a hand-built object.
func putTaintedSlice(ctx context.Context, sliceName, nodeName, devID, bdf string, taintKeys ...string) {
	taints := make([]resv1.DeviceTaint, 0, len(taintKeys))
	for _, k := range taintKeys {
		taints = append(taints, resv1.DeviceTaint{Key: k, Effect: resv1.DeviceTaintEffectNoSchedule})
	}

	slice := &resv1.ResourceSlice{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: sliceName}, slice); err == nil {
		slice.Spec.Devices[0].Taints = taints
		Expect(k8sClient.Update(ctx, slice)).To(Succeed())

		return
	}

	Expect(k8sClient.Create(ctx, &resv1.ResourceSlice{
		ObjectMeta: metav1.ObjectMeta{Name: sliceName},
		Spec: resv1.ResourceSliceSpec{
			Driver:   "gpu.intel.com",
			NodeName: ptr.To(nodeName),
			Pool:     resv1.ResourcePool{Name: sliceName + "-pool", ResourceSliceCount: 1},
			Devices: []resv1.Device{
				{
					Name: "dev-" + sanitizeSegment(bdf),
					Attributes: map[resv1.QualifiedName]resv1.DeviceAttribute{
						deviceAttrDeviceID: {StringValue: ptr.To(devID)},
						deviceAttrBDF:      {StringValue: ptr.To(bdf)},
					},
					Taints: taints,
				},
			},
		},
	})).To(Succeed())

	DeferCleanup(func() {
		stale := &resv1.ResourceSlice{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: sliceName}, stale); err == nil {
			Expect(k8sClient.Delete(ctx, stale)).To(Succeed())
		}
	})
}

var _ = Describe("GPURecoveryPlan Controller", func() {
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
	})

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
					MaxRetries:       3,
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

	Context("persistPlan: status write-back", func() {
		newPersistPlan := func(name string) (*GPURecoveryPlanReconciler, *intelv1a1.GPURecoveryPlan) {
			p := &intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{Name: name},
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					DeviceID:         "0xabcd",
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

		It("should do nothing when the status did not change", func() {
			r, p := newPersistPlan("plan-persist-noop")
			key := types.NamespacedName{Name: p.Name}

			r.Client = &failingStatusClient{Client: r.Client}

			// orig == p, so there is nothing to write and the failing path is not hit.
			Expect(r.persistPlan(ctx, key, p.DeepCopy(), p)).To(Succeed())
		})
	})

	Context("Reconcile: write failures are surfaced", func() {
		It("should return an error when persistPlan cannot write", func() {
			// A write error that is only logged produces a successful reconcile and no retry, so
			// a detected GPU would stay invisible in the CR until something else triggered a
			// reconcile. Only Status().Update is made to fail, and the phases dirty status
			// (state "" -> idle), so the error can only originate in persistPlan.
			p := &intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{Name: "plan-reconcile-writefail"},
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot, DeviceID: "0xabcd",
				},
			}
			Expect(k8sClient.Create(ctx, p)).To(Succeed())

			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, p)
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
			r := newTestReconciler()
			p := &intelv1a1.GPURecoveryPlan{}

			for i := 0; i < maxStatusMessages+10; i++ {
				r.appendMessage(p, "msg")
			}

			Expect(p.Status.Messages).To(HaveLen(maxStatusMessages))
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
					DefaultResetType: intelv1a1.RecoveryTypeSlot, DeviceID: "0xabcd", MaxRetries: 3,
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
					DefaultResetType: intelv1a1.RecoveryTypeSlot, DeviceID: "0xabcd", MaxRetries: 3,
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

	Context("Helper: escalateEvent", func() {
		var r *GPURecoveryPlanReconciler

		BeforeEach(func() {
			r = newTestReconciler()
		})

		// waitingEvent builds a single-device event of the given type, waiting for approval, with
		// approval bookkeeping and a spent retry budget already present so escalation can be seen
		// to clear them.
		waitingEvent := func(rt intelv1a1.RecoveryType) *intelv1a1.RecoveryEvent {
			now := metav1.Now()

			return &intelv1a1.RecoveryEvent{
				ID:                generateEventID("node01", "0000:02:00.0", rt),
				NodeName:          "node01",
				GPUBDF:            "0000:02:00.0",
				Reason:            reasonWedged,
				RecoveryType:      intelv1a1.RecoveryTypeSpec{Type: rt},
				State:             intelv1a1.RecoveryEventStateWaitingApproval,
				RetryCount:        2,
				ApprovalID:        "app-old",
				ApprovalMatchedAt: ptr.To(now),
				LastUpdated:       ptr.To(now),
			}
		}

		It("should escalate a waiting-approval slot event to reflash", func() {
			p := &intelv1a1.GPURecoveryPlan{ObjectMeta: metav1.ObjectMeta{Name: "plan-esc"}}
			evt := waitingEvent(intelv1a1.RecoveryTypeSlot)

			r.escalateEvent(p, evt, deviceNeed{rt: intelv1a1.RecoveryTypeReflash, reason: reasonSurvivability})

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

			r.escalateEvent(p, evt, deviceNeed{rt: intelv1a1.RecoveryTypeReflash, reason: reasonSurvivability})

			// ...and must not after, or approving a reset would run a reflash.
			Expect(evt.ID).NotTo(Equal(p.Spec.Approvals[0].EventID),
				"a slot-reset approval must not carry over to the escalated reflash event")
		})

		It("should reset the approval state and retry budget", func() {
			p := &intelv1a1.GPURecoveryPlan{ObjectMeta: metav1.ObjectMeta{Name: "plan-esc-reset"}}
			evt := waitingEvent(intelv1a1.RecoveryTypeSlot)

			r.escalateEvent(p, evt, deviceNeed{rt: intelv1a1.RecoveryTypeReflash, reason: reasonSurvivability})

			Expect(evt.State).To(Equal(intelv1a1.RecoveryEventStateWaitingApproval))
			Expect(evt.RetryCount).To(BeZero(), "the escalated operation gets its own retry budget")
			Expect(evt.ApprovalID).To(BeEmpty())
			Expect(evt.ApprovalMatchedAt).To(BeNil())
		})

		// The ID changing is invisible on the event itself, and it is the reason an approval an
		// admin already granted has stopped applying. Nothing else says so.
		It("should say on the event why the previous approval no longer applies", func() {
			p := &intelv1a1.GPURecoveryPlan{ObjectMeta: metav1.ObjectMeta{Name: "plan-esc-msg"}}
			evt := waitingEvent(intelv1a1.RecoveryTypeSlot)

			r.escalateEvent(p, evt, deviceNeed{rt: intelv1a1.RecoveryTypeReflash, reason: reasonSurvivability})

			Expect(evt.StateMessage).To(SatisfyAll(
				ContainSubstring(string(intelv1a1.RecoveryTypeSlot)),
				ContainSubstring(string(intelv1a1.RecoveryTypeReflash)),
			))
		})

		It("should not downgrade reflash to a reset", func() {
			p := &intelv1a1.GPURecoveryPlan{ObjectMeta: metav1.ObjectMeta{Name: "plan-esc-down"}}
			evt := waitingEvent(intelv1a1.RecoveryTypeReflash)
			before := evt.DeepCopy()

			r.escalateEvent(p, evt, deviceNeed{rt: intelv1a1.RecoveryTypeSlot, reason: reasonWedged})

			Expect(evt).To(Equal(before), "escalation is one-way; act on the worst condition")
		})

		It("should be a no-op when the type is unchanged", func() {
			p := &intelv1a1.GPURecoveryPlan{ObjectMeta: metav1.ObjectMeta{Name: "plan-esc-same"}}
			evt := waitingEvent(intelv1a1.RecoveryTypeSlot)
			before := evt.DeepCopy()

			r.escalateEvent(p, evt, deviceNeed{rt: intelv1a1.RecoveryTypeSlot, reason: reasonWedged})

			Expect(evt).To(Equal(before))
			Expect(p.Status.Messages).To(BeEmpty(), "a steady-state reconcile must not log an escalation")
		})

		// Escalation must converge: repeated reconciles of the same escalated device must not
		// keep rewriting the event, or every reconcile would emit a status write and a message.
		It("should be idempotent across repeated reconciles", func() {
			p := &intelv1a1.GPURecoveryPlan{ObjectMeta: metav1.ObjectMeta{Name: "plan-esc-idem"}}
			evt := waitingEvent(intelv1a1.RecoveryTypeSlot)

			r.escalateEvent(p, evt, deviceNeed{rt: intelv1a1.RecoveryTypeReflash, reason: reasonSurvivability})
			afterFirst := evt.DeepCopy()
			msgCount := len(p.Status.Messages)

			r.escalateEvent(p, evt, deviceNeed{rt: intelv1a1.RecoveryTypeReflash, reason: reasonSurvivability})

			Expect(evt).To(Equal(afterFirst))
			Expect(p.Status.Messages).To(HaveLen(msgCount))
		})
	})

	Context("Helper: findEventForDevice", func() {
		It("should find an event regardless of its recovery type", func() {
			r := newTestReconciler()
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

			Expect(r.findEventForDevice(p, "node01", "0000:02:00.0")).To(Equal(0))
			Expect(r.findEventForDevice(p, "node02", "0000:03:00.0")).To(Equal(1))
			Expect(r.findEventForDevice(p, "node03", "0000:02:00.0")).To(Equal(-1))
			Expect(r.findEventForDevice(p, "node01", "0000:09:00.0")).To(Equal(-1))
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
			r := newTestReconciler()
			p := &intelv1a1.GPURecoveryPlan{}

			r.addNewEvents(p, activeSet(3))

			Expect(p.Status.Events).To(HaveLen(3))
		})

		// status.events[].reason is the only place the triggering condition is recorded: the
		// taint lives on the ResourceSlice and is gone by the time anyone reads the event.
		It("should record the cause the recovery was derived from", func() {
			r := newTestReconciler()
			p := &intelv1a1.GPURecoveryPlan{}

			r.addNewEvents(p, map[deviceKey]deviceNeed{
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
			r := newTestReconciler()
			p := &intelv1a1.GPURecoveryPlan{}

			r.addNewEvents(p, activeSet(maxStatusEvents+10))

			Expect(p.Status.Events).To(HaveLen(maxStatusEvents))
		})

		It("should report the refusal in status.messages rather than dropping silently", func() {
			r := newTestReconciler()
			p := &intelv1a1.GPURecoveryPlan{}

			r.addNewEvents(p, activeSet(maxStatusEvents+10))

			Expect(p.Status.Messages).NotTo(BeEmpty())
			Expect(p.Status.Messages[len(p.Status.Messages)-1]).To(ContainSubstring("not recorded"))
		})

		// status.events is live state, not a log: an event carries the approval an admin gave it.
		// Evicting entries FIFO-style to make room would discard that.
		It("should keep the existing events when the cap is reached", func() {
			r := newTestReconciler()
			p := &intelv1a1.GPURecoveryPlan{}

			r.addNewEvents(p, activeSet(maxStatusEvents))
			oldest := p.Status.Events[0]

			r.addNewEvents(p, map[deviceKey]deviceNeed{
				{node: "brand-new-node", bdf: "0000:03:00.0"}: {
					rt: intelv1a1.RecoveryTypeSlot, reason: reasonWedged,
				},
			})

			Expect(p.Status.Events).To(HaveLen(maxStatusEvents))
			Expect(p.Status.Events[0]).To(Equal(oldest))
		})

		It("should not add a second event for a device that already has one", func() {
			r := newTestReconciler()
			p := &intelv1a1.GPURecoveryPlan{}
			active := activeSet(2)

			r.addNewEvents(p, active)
			r.addNewEvents(p, active)

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

			r.removeResolvedEvents(p, map[deviceKey]deviceNeed{})

			Expect(p.Status.Events).To(BeEmpty())
			Expect(p.Status.Messages).To(ContainElement(ContainSubstring("evt-gone")))
		})

		It("should keep an event whose taint is still active", func() {
			p := planWithEvents(intelv1a1.RecoveryEvent{
				ID: "evt-stays", NodeName: "node01", GPUBDF: "0000:02:00.0",
				State: intelv1a1.RecoveryEventStateWaitingApproval,
			})

			r.removeResolvedEvents(p, map[deviceKey]deviceNeed{
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

			r.removeResolvedEvents(p, map[deviceKey]deviceNeed{
				{node: "node01", bdf: "0000:02:00.0"}: {rt: intelv1a1.RecoveryTypeSlot, reason: reasonWedged},
			})

			Expect(p.Status.Events).To(BeEmpty())
		})
	})

	Context("Event ID generation", func() {
		It("should produce readable IDs embedding node, type and BDF", func() {
			id := generateEventID("rocebuntu2", "0000:02:00.0", intelv1a1.RecoveryTypeSBR)
			Expect(id).To(Equal("evt-rocebuntu2-sbr-0000-02-00-0"))
		})

		It("should keep a non-zero domain in the BDF slug", func() {
			id := generateEventID("node01", "0001:03:00.0", intelv1a1.RecoveryTypeSlot)
			Expect(id).To(Equal("evt-node01-slot-0001-03-00-0"))
		})

		It("should produce the same ID for the same inputs (deterministic)", func() {
			id1 := generateEventID("node01", "0000:05:00.0", intelv1a1.RecoveryTypeSlot)
			id2 := generateEventID("node01", "0000:05:00.0", intelv1a1.RecoveryTypeSlot)
			Expect(id1).To(Equal(id2))
		})

		// The ID becomes both a Job name (DNS-1123 subdomain) and a label value, and neither
		// rule is a superset of the other: uppercase and "_" are legal in a label value but
		// rejected in a name, so these assert the stricter of the two.
		DescribeTable("should always yield a valid, in-budget Job name",
			func(nodeName, bdf string, rt intelv1a1.RecoveryType) {
				id := generateEventID(nodeName, bdf, rt)

				Expect(validation.IsDNS1123Subdomain(id)).To(BeEmpty(),
					"event ID %q is not a valid resource name", id)
				Expect(validation.IsValidLabelValue(id)).To(BeEmpty(),
					"event ID %q is not a valid label value", id)

				// maxRetries has no upper bound, so check the widest attempt index the
				// budget was sized for rather than just the first attempt.
				for _, attempt := range []int{0, 9, 99} {
					name := recoveryJobName(id, attempt)

					Expect(len(name)).To(BeNumerically("<=", maxRecoveryNameLen),
						"Job name %q is %d bytes", name, len(name))
					Expect(validation.IsDNS1123Subdomain(name)).To(BeEmpty(),
						"Job name %q is not a valid resource name", name)
					Expect(validation.IsValidLabelValue(name)).To(BeEmpty(),
						"Job name %q is not a valid label value", name)
				}
			},
			Entry("short name", "node03", "0000:02:00.0", intelv1a1.RecoveryTypeSBR),
			Entry("EKS-style FQDN", "ip-10-0-134-22.us-west-2.compute.internal", "0000:02:00.0", intelv1a1.RecoveryTypeReflash),
			Entry("OpenShift-style FQDN", "worker-03.cluster.example.com", "0000:af:00.0", intelv1a1.RecoveryTypeReflash),
			Entry("at the node budget", strings.Repeat("n", nodeSegmentMax), "0001:02:00.0", intelv1a1.RecoveryTypeReflash),
			Entry("one over the node budget", strings.Repeat("n", nodeSegmentMax+1), "0001:02:00.0", intelv1a1.RecoveryTypeReflash),
			Entry("absurdly long name", strings.Repeat("very-long-node-name.", 20), "0001:02:00.0", intelv1a1.RecoveryTypeReflash),
			// lspci prints PCI addresses in uppercase hex, so this is a likely spelling
			// rather than a pathological one — and the DRA driver's pciAddress attribute
			// is a free-form string, not a validated field.
			Entry("uppercase hex BDF", "node03", "0000:AF:00.0", intelv1a1.RecoveryTypeSBR),
			Entry("BDF with unexpected separators", "node03", "0000/02_00,0", intelv1a1.RecoveryTypeSBR),
			// The truncation point is nodeSegmentMax-idHashLen-1, so placing a hyphen at the
			// last kept index makes the cut land exactly on a separator — the case that
			// needs trimming before the hash suffix is appended, since a DNS-1123 subdomain
			// may not contain "--" at a label boundary or end on a hyphen.
			Entry("node name that truncates onto a separator",
				strings.Repeat("b", nodeSegmentMax-idHashLen-2)+"-more-tail", "0000:02:00.0", intelv1a1.RecoveryTypeReflash),
			Entry("empty BDF", "node03", "", intelv1a1.RecoveryTypeSBR),
			// pciAddress is a free-form ResourceSlice attribute written by the DRA driver,
			// so nothing bounds its length. Sanitizing it yields a valid *character set*
			// at any length, which is exactly how a charset-only guard lets an
			// over-long ID through.
			Entry("absurdly long BDF", "node03", strings.Repeat("0000:02:00.0/", 12), intelv1a1.RecoveryTypeReflash),
			// A BDF ending in a separator sanitizes to a trailing hyphen, which is the one
			// position a DNS-1123 subdomain forbids outright. Unlike the node segment — where
			// the hash suffix always follows and hides it — the BDF is the last part of the
			// ID, so nothing covers for it.
			Entry("BDF with a trailing separator", "node03", "0000:02:00.0:", intelv1a1.RecoveryTypeSBR),
			Entry("BDF with a leading separator", "node03", ":02:00.0", intelv1a1.RecoveryTypeSBR),
			Entry("node name with leading and trailing dots", ".node03.", "0000:02:00.0", intelv1a1.RecoveryTypeSBR),
			Entry("long node and long BDF together",
				strings.Repeat("node.", 20), strings.Repeat("0000:02:00.0/", 12), intelv1a1.RecoveryTypeReflash),
		)

		// The ID is not only a Job-name component: addRecoveryEvent stores it in
		// status.events[].id and it is assigned verbatim to a label on the recovery Job, whose
		// values cap at 63 bytes. A guard that checked only the character set would pass a
		// 170-byte ID here — IsDNS1123Subdomain permits 253 — and the label assignment would
		// then be rejected.
		DescribeTable("should yield an ID usable as a label value in its own right",
			func(nodeName, bdf string) {
				id := generateEventID(nodeName, bdf, intelv1a1.RecoveryTypeReflash)

				Expect(validation.IsValidLabelValue(id)).To(BeEmpty(),
					"event ID %q (%d bytes) is not a valid label value", id, len(id))
			},
			Entry("long BDF", "node03", strings.Repeat("0000:02:00.0/", 12)),
			Entry("long node", strings.Repeat("node.", 20), "0000:02:00.0"),
			Entry("both long", strings.Repeat("node.", 20), strings.Repeat("0000:02:00.0/", 12)),
		)

		// nodeSegmentMax is a hand-computed budget, and the specs above only prove that the
		// *result* is valid — recoveryJobName's hash fallback would silently absorb an
		// oversized constant, turning every long-node ID unreadable while still passing.
		// This pins the arithmetic directly so the constant cannot drift away from the limit
		// it was derived from without saying so.
		It("should size nodeSegmentMax so the worst-case name needs no fallback", func() {
			worst := len("recovery-") + len("evt-") + nodeSegmentMax +
				len("-reflash") + len("-0001-02-00-0") + len("-99")

			Expect(worst).To(BeNumerically("<=", maxRecoveryNameLen),
				"nodeSegmentMax=%d makes the worst-case Job name %d bytes, over the %d-byte limit",
				nodeSegmentMax, worst, maxRecoveryNameLen)

			// And the readable path is actually taken at the budget: a node segment exactly
			// at nodeSegmentMax must survive into the Job name intact, not be hashed away.
			id := generateEventID(strings.Repeat("n", nodeSegmentMax), "0001:02:00.0",
				intelv1a1.RecoveryTypeReflash)
			Expect(recoveryJobName(id, 99)).To(ContainSubstring(strings.Repeat("n", nodeSegmentMax)),
				"the worst-case name should fit without recoveryJobName hashing it")
		})

		It("should lowercase an uppercase BDF rather than rejecting it", func() {
			// Specifically pins the sanitize-rather-than-hash behaviour: the readable form
			// survives, so this must not fall through to the hash branch.
			id := generateEventID("node03", "0000:AF:00.0", intelv1a1.RecoveryTypeSBR)
			Expect(id).To(Equal("evt-node03-sbr-0000-af-00-0"))
		})

		// Validity alone is a weak assertion here: the guard's whole-ID hash fallback also
		// produces a valid ID, so a version that did no bounding at all — collapsing every
		// long node to an opaque "evt-sbr-09553f" — would satisfy every check above. These
		// pin the behaviour that motivates truncate-plus-hash over hash-everything: the
		// operator can still tell which node an event belongs to at a glance.
		DescribeTable("should keep a readable node prefix rather than hashing the whole ID",
			func(nodeName, wantPrefix string) {
				id := generateEventID(nodeName, "0000:02:00.0", intelv1a1.RecoveryTypeSBR)

				Expect(id).To(HavePrefix(wantPrefix),
					"ID %q should keep a readable prefix of node %q", id, nodeName)
				Expect(id).To(ContainSubstring("-sbr-"),
					"ID %q should still carry the recovery type", id)
			},
			Entry("EKS-style FQDN", "ip-10-0-134-22.us-west-2.compute.internal", "evt-ip-10-0-134-22"),
			Entry("OpenShift-style FQDN", "worker-03.cluster.example.com", "evt-worker-03"),
			Entry("long single-token name", strings.Repeat("worker-aaaa", 4)+"-01", "evt-worker-aaaa"),
		)

		It("should not collide for long node names sharing a prefix", func() {
			// The hash covers the *original* name, not the truncated form. If it hashed the
			// truncation instead, these two would alias onto one ID and break the
			// one-event-per-device invariant findEventForDevice relies on.
			prefix := strings.Repeat("worker-aaaa", 4)
			id1 := generateEventID(prefix+"-01", "0000:02:00.0", intelv1a1.RecoveryTypeSBR)
			id2 := generateEventID(prefix+"-02", "0000:02:00.0", intelv1a1.RecoveryTypeSBR)

			Expect(id1).NotTo(Equal(id2))
		})

		It("should stay deterministic for long node names", func() {
			long := strings.Repeat("long-node-name.", 6)
			Expect(generateEventID(long, "0000:02:00.0", intelv1a1.RecoveryTypeSBR)).
				To(Equal(generateEventID(long, "0000:02:00.0", intelv1a1.RecoveryTypeSBR)))
		})

		// escalateEvent depends on a type change producing a new ID: that is what invalidates
		// an approval naming the old one, so a reset approval cannot silently authorise the
		// reflash it escalated into. The property has to survive every path an ID can take,
		// including the whole-ID hash fallback — which is why the recovery type is
		// interpolated outside the hash rather than fed into it.
		DescribeTable("should change the ID on escalation on every naming path",
			func(nodeName, bdf string) {
				slot := generateEventID(nodeName, bdf, intelv1a1.RecoveryTypeSlot)
				reflash := generateEventID(nodeName, bdf, intelv1a1.RecoveryTypeReflash)

				Expect(slot).NotTo(Equal(reflash),
					"slot ID %q and reflash ID %q must differ or a stale approval stays valid", slot, reflash)
			},
			Entry("readable path", "node03", "0000:02:00.0"),
			Entry("truncated node path", strings.Repeat("long-node-name.", 6), "0000:02:00.0"),
			// Long BDF pushes the ID over budget, so this exercises the whole-ID fallback.
			Entry("whole-ID fallback path", "node03", strings.Repeat("0000:02:00.0/", 12)),
		)

		It("should preserve separator runs so they cannot alias", func() {
			// "a--b" is a valid subdomain, so collapsing runs would only lose information
			// and risk two distinct nodes mapping to one ID.
			id1 := generateEventID("node-01", "0000:02:00.0", intelv1a1.RecoveryTypeSBR)
			id2 := generateEventID("node--01", "0000:02:00.0", intelv1a1.RecoveryTypeSBR)

			Expect(id1).NotTo(Equal(id2))
		})
	})

	// status.events[].stateMessage exists because the states are coarse — several different
	// situations park an event in the same one — and an admin cannot be expected to reconstruct
	// which is which from a 50-entry ring shared by every event.
	Context("Helper: setEventState", func() {
		eventIn := func(state intelv1a1.RecoveryEventState) *intelv1a1.RecoveryEvent {
			return &intelv1a1.RecoveryEvent{
				ID:           "evt-msg-001",
				NodeName:     "node01",
				GPUBDF:       "0000:02:00.0",
				RecoveryType: intelv1a1.RecoveryTypeSpec{Type: intelv1a1.RecoveryTypeSlot},
				State:        state,
				LastUpdated:  ptr.To(metav1.NewTime(time.Now().Add(-time.Hour))),
			}
		}

		It("should drop the previous explanation on a transition that has none of its own", func() {
			evt := eventIn(intelv1a1.RecoveryEventStateWaitingApproval)
			evt.StateMessage = "escalated from slot to reflash (survivability-mode)"

			setEventState(evt, intelv1a1.RecoveryEventStateWaitingApproval, "")

			// The whole value of the field is that it describes the state next to it. A message
			// about a transition that has since been superseded is worse than no message at all.
			Expect(evt.StateMessage).To(BeEmpty())
		})

		It("should not re-stamp LastUpdated when neither the state nor the message changes", func() {
			evt := eventIn(intelv1a1.RecoveryEventStateWaitingApproval)
			setEventState(evt, intelv1a1.RecoveryEventStateWaitingApproval, "held: %v", "some reason")
			first := *evt.LastUpdated

			setEventState(evt, intelv1a1.RecoveryEventStateWaitingApproval, "held: %v", "some reason")

			// Conditions are re-detected on every pass. Stamping each time would write the object
			// once per reconcile for as long as the condition lasts, and make lastUpdated useless
			// as "when did this event last actually change".
			Expect(*evt.LastUpdated).To(Equal(first))
		})

		It("should truncate on a rune boundary so the status write stays valid UTF-8", func() {
			evt := eventIn(intelv1a1.RecoveryEventStateWaitingApproval)

			setEventState(evt, intelv1a1.RecoveryEventStateWaitingApproval, "%s", strings.Repeat("dénï — ", 60))

			Expect(len(evt.StateMessage)).To(BeNumerically("<=", maxStateMessageLen))
			Expect(utf8.ValidString(evt.StateMessage)).To(BeTrue(),
				"the API server rejects a status write containing invalid UTF-8")
		})
	})

	Context("Helper: updatePlanState", func() {
		var r *GPURecoveryPlanReconciler

		BeforeEach(func() {
			r = newTestReconciler()
		})

		// planWith builds a plan with maxRetries=3 and the given events, so the retry-budget
		// comparison in updatePlanState has a real threshold to test against.
		planWith := func(events ...intelv1a1.RecoveryEvent) *intelv1a1.GPURecoveryPlan {
			return &intelv1a1.GPURecoveryPlan{
				Spec: intelv1a1.GPURecoveryPlanSpec{DefaultResetType: intelv1a1.RecoveryTypeSlot, MaxRetries: 3},
				Status: intelv1a1.GPURecoveryPlanStatus{
					Events: events,
				},
			}
		}
		evt := func(state intelv1a1.RecoveryEventState, retries int32) intelv1a1.RecoveryEvent {
			return intelv1a1.RecoveryEvent{State: state, RetryCount: retries}
		}

		It("should report idle with no events", func() {
			p := planWith()
			r.updatePlanState(p)
			Expect(p.Status.State).To(Equal(intelv1a1.PlanStateIdle))
		})

		DescribeTable("should report active while an event is on its way somewhere",
			func(state intelv1a1.RecoveryEventState) {
				p := planWith(evt(state, 0))
				r.updatePlanState(p)
				Expect(p.Status.State).To(Equal(intelv1a1.PlanStateActive))
			},
			Entry("waiting for an approval", intelv1a1.RecoveryEventStateWaitingApproval),
			// blocked is active, not stuck: it clears on its own once the node frees up.
			Entry("blocked behind another recovery", intelv1a1.RecoveryEventStateBlocked),
			Entry("draining its node", intelv1a1.RecoveryEventStateDraining),
			Entry("running its recovery", intelv1a1.RecoveryEventStateInProgress),
		)

		It("should report idle once every event has settled", func() {
			p := planWith(evt(intelv1a1.RecoveryEventStateSucceeded, 1))
			r.updatePlanState(p)
			Expect(p.Status.State).To(Equal(intelv1a1.PlanStateIdle))
		})

		It("should report error for an event that exhausted its retry budget", func() {
			p := planWith(evt(intelv1a1.RecoveryEventStateFailed, 3))
			r.updatePlanState(p)
			Expect(p.Status.State).To(Equal(intelv1a1.PlanStateError),
				"an exhausted event never retries on its own; it needs an explicit re-approval")
		})

		// A failure inside the budget is transient: the event is put back to waiting-approval on
		// a later pass, so surfacing "error" would be a false alarm.
		It("should not report error for a failure still inside its retry budget", func() {
			p := planWith(evt(intelv1a1.RecoveryEventStateFailed, 1))
			r.updatePlanState(p)
			Expect(p.Status.State).NotTo(Equal(intelv1a1.PlanStateError))
		})

		It("should report error when a reflash is blocked on missing firmware", func() {
			p := planWith(evt(intelv1a1.RecoveryEventStateMissingFirmware, 0))
			r.updatePlanState(p)
			Expect(p.Status.State).To(Equal(intelv1a1.PlanStateError),
				"nothing clears this but an operator filling in spec.firmware")
		})

		// The whole point of the field is answering "does this need me?" — one healthy event in
		// flight must not mask a stuck one.
		It("should let error outrank active", func() {
			p := planWith(
				evt(intelv1a1.RecoveryEventStateInProgress, 0),
				evt(intelv1a1.RecoveryEventStateFailed, 3),
			)
			r.updatePlanState(p)
			Expect(p.Status.State).To(Equal(intelv1a1.PlanStateError))
		})
	})
})
