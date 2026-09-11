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
	resv1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	intelv1a1 "github.com/intel/gpu-base-operator/api/v1alpha1"
	"github.com/intel/gpu-base-operator/config/deployments"
)

// The recovery Job the operator builds: its command, its deadlines, its owner
// reference, and the name it ends up with.
var _ = Describe("GPURecoveryPlan Controller: recovery Job construction", func() {
	ctx := context.Background()

	Context("Helper: buildResetCommand", func() {
		const bdf = "0000:02:00.0"

		// Every reset type in the enum must map to a distinct xpu-smi invocation, or an admin's
		// override between two of them would look like a change while running the same command.
		It("should give every reset type its own distinct command", func() {
			resetTypes := []intelv1a1.RecoveryType{
				intelv1a1.RecoveryTypeSBR,
				intelv1a1.RecoveryTypeSlot,
				intelv1a1.RecoveryTypeAMC,
			}

			seen := map[string]intelv1a1.RecoveryType{}

			for _, rt := range resetTypes {
				cmd := buildResetCommand(bdf, rt)
				Expect(cmd).NotTo(BeEmpty(), "reset type %q must map to a command", rt)

				Expect(seen).NotTo(HaveKey(cmd),
					"reset types %q and %q share the command %q, so an override between them is a no-op",
					seen[cmd], rt, cmd)

				seen[cmd] = rt
			}
		})

		// The binary is named, not pathed: the plan can point spec.xpuSmi.image at any image that
		// has xpu-smi on PATH, which is not the same set of images as those that keep it in
		// /usr/local/bin.
		It("should invoke xpu-smi by name rather than by path", func() {
			Expect(buildResetCommand(bdf, intelv1a1.RecoveryTypeSlot)).To(HavePrefix("xpu-smi "))
		})

		It("should address the BDF the event names", func() {
			Expect(buildResetCommand("0000:af:00.0", intelv1a1.RecoveryTypeSBR)).
				To(ContainSubstring("0000:af:00.0"))
		})

		It("should return no command for reflash, which is not an xpu-smi reset", func() {
			Expect(buildResetCommand(bdf, intelv1a1.RecoveryTypeReflash)).To(BeEmpty())
		})

		It("should return no command for a type outside the enum", func() {
			Expect(buildResetCommand(bdf, intelv1a1.RecoveryType("flr"))).To(BeEmpty())
		})
	})

	// The deadline is the clock on the Job itself: an xpu-smi that hangs on a card that has stopped
	// answering would otherwise hold the node's drain taint and the event's state indefinitely. The
	// operator keeps its own clock over it (jobVerdictOverdue), because a deadline enforced by the
	// Job controller is no help on an event whose Job the Job controller never answers for.
	Context("Recovery Job timeouts", func() {
		timeoutFor := func(rt intelv1a1.RecoveryType, t intelv1a1.RecoveryTimeoutsSpec) int64 {
			plan := &intelv1a1.GPURecoveryPlan{Spec: intelv1a1.GPURecoveryPlanSpec{Timeouts: t}}
			evt := &intelv1a1.RecoveryEvent{RecoveryType: intelv1a1.RecoveryTypeSpec{Type: rt}}

			return recoveryJobTimeout(plan, evt)
		}

		DescribeTable("should take the deadline for the kind of recovery being run",
			func(rt intelv1a1.RecoveryType, t intelv1a1.RecoveryTimeoutsSpec, want int64) {
				Expect(timeoutFor(rt, t)).To(Equal(want))
			},
			Entry("a reset from resetSeconds", intelv1a1.RecoveryTypeSlot,
				intelv1a1.RecoveryTimeoutsSpec{ResetSeconds: 45, ReflashSeconds: 900}, int64(45)),
			Entry("a reflash from reflashSeconds", intelv1a1.RecoveryTypeReflash,
				intelv1a1.RecoveryTimeoutsSpec{ResetSeconds: 45, ReflashSeconds: 900}, int64(900)),
			// Cross-wiring the two is the mistake worth pinning: a reflash cut off after a reset's
			// deadline leaves the card part-written, which is worse than the state it started in.
			Entry("a reflash when only resetSeconds is set", intelv1a1.RecoveryTypeReflash,
				intelv1a1.RecoveryTimeoutsSpec{ResetSeconds: 45}, defaultReflashJobTimeout),
			Entry("a reset when only reflashSeconds is set", intelv1a1.RecoveryTypeSlot,
				intelv1a1.RecoveryTimeoutsSpec{ReflashSeconds: 900}, defaultResetJobTimeout),
			// spec.timeouts is defaulted by the CRD, so an empty one means an object that never
			// reached the API server. A Job with no deadline at all is the one outcome that must not
			// happen: activeDeadlineSeconds is what ends a hung recovery.
			Entry("a reset with no timeouts at all", intelv1a1.RecoveryTypeSlot,
				intelv1a1.RecoveryTimeoutsSpec{}, defaultResetJobTimeout),
			Entry("a reflash with no timeouts at all", intelv1a1.RecoveryTypeReflash,
				intelv1a1.RecoveryTimeoutsSpec{}, defaultReflashJobTimeout),
		)

		// The fallbacks exist to reproduce what the object would have been given had it been
		// defaulted, so three places have to agree: these constants, the CRD defaults, and the
		// templates' own activeDeadlineSeconds. The CRD side is checked by the webhook suite; this is
		// the templates.
		DescribeTable("should fall back to the deadline the Job template carries",
			func(tmpl *batch.Job, want int64) {
				Expect(tmpl.Spec.ActiveDeadlineSeconds).To(HaveValue(Equal(want)))
			},
			Entry("reset", deployments.XpuManagerResetJob(), defaultResetJobTimeout),
			Entry("reflash", deployments.XpuManagerFWUpdateJob(), defaultReflashJobTimeout),
		)

		DescribeTable("should put the plan's deadline on the Job it creates",
			func(planName, evtID string, rt intelv1a1.RecoveryType, want int64) {
				r := newTestReconciler()

				p := &intelv1a1.GPURecoveryPlan{
					ObjectMeta: metav1.ObjectMeta{Name: planName},
					Spec: intelv1a1.GPURecoveryPlanSpec{
						DefaultResetType: intelv1a1.RecoveryTypeSlot,
						DeviceID:         "0xabcd",
						XpuSmi:           intelv1a1.XpuSmiSpec{Image: "local/xpusmi:devel"},
						Timeouts:         intelv1a1.RecoveryTimeoutsSpec{ResetSeconds: 42, ReflashSeconds: 1200},
						Firmware: &intelv1a1.FirmwareSpec{
							Source: intelv1a1.FirmwareSource{
								ContainerSource: &intelv1a1.ContainerFirmwareSource{Name: "registry/fw:v2"},
							},
							File: "gfx.bin",
						},
					},
					Status: intelv1a1.GPURecoveryPlanStatus{
						Events: []intelv1a1.RecoveryEvent{{
							ID:           evtID,
							NodeName:     "node01",
							GPUBDF:       "0000:02:00.0",
							RecoveryType: intelv1a1.RecoveryTypeSpec{Type: rt},
							State:        intelv1a1.RecoveryEventStateWaitingApproval,
							LastUpdated:  ptr.To(metav1.Now()),
						}},
					},
				}

				createPlanForOwnerRef(p)

				Expect(r.createRecoveryJob(ctx, p, &p.Status.Events[0])).To(Succeed())

				job := &batch.Job{}
				Expect(k8sClient.Get(ctx, types.NamespacedName{
					Name: recoveryJobName(evtID, 0), Namespace: "default",
				}, job)).To(Succeed())

				DeferCleanup(func() {
					_ = k8sClient.Delete(ctx, job)
				})

				Expect(job.Spec.ActiveDeadlineSeconds).To(HaveValue(Equal(want)),
					"the template's own deadline must not survive the plan's")
			},
			Entry("a reset Job", "plan-deadline-reset", "evt-deadline-reset",
				intelv1a1.RecoveryTypeSlot, int64(42)),
			Entry("a reflash Job", "plan-deadline-reflash", "evt-deadline-reflash",
				intelv1a1.RecoveryTypeReflash, int64(1200)),
		)

		// One pod per approval: an admin authorised one attempt at this card, and the Job
		// controller's default backoff limit of 6 would quietly turn that into seven. The templates'
		// podFailurePolicy only names the container running xpu-smi, so a pod that fails anywhere
		// else (the reflash's fw-copy initContainer, when the firmware file is not in the image)
		// falls through to the backoff limit: seven privileged pods on a broken GPU, and the event
		// hears nothing until activeDeadlineSeconds.
		DescribeTable("should leave the Job controller no pod retries of its own",
			func(planName, evtID string, rt intelv1a1.RecoveryType) {
				r := newTestReconciler()

				p := &intelv1a1.GPURecoveryPlan{
					ObjectMeta: metav1.ObjectMeta{Name: planName},
					Spec: intelv1a1.GPURecoveryPlanSpec{
						DefaultResetType: intelv1a1.RecoveryTypeSlot,
						DeviceID:         "0xabcd",
						XpuSmi:           intelv1a1.XpuSmiSpec{Image: "local/xpusmi:devel"},
						Firmware: &intelv1a1.FirmwareSpec{
							Source: intelv1a1.FirmwareSource{
								ContainerSource: &intelv1a1.ContainerFirmwareSource{Name: "registry/fw:v2"},
							},
							File: "gfx.bin",
						},
					},
					Status: intelv1a1.GPURecoveryPlanStatus{
						Events: []intelv1a1.RecoveryEvent{{
							ID:           evtID,
							NodeName:     "node01",
							GPUBDF:       "0000:02:00.0",
							RecoveryType: intelv1a1.RecoveryTypeSpec{Type: rt},
							State:        intelv1a1.RecoveryEventStateWaitingApproval,
							LastUpdated:  ptr.To(metav1.Now()),
						}},
					},
				}

				createPlanForOwnerRef(p)

				Expect(r.createRecoveryJob(ctx, p, &p.Status.Events[0])).To(Succeed())

				job := &batch.Job{}
				Expect(k8sClient.Get(ctx, types.NamespacedName{
					Name: recoveryJobName(evtID, 0), Namespace: "default",
				}, job)).To(Succeed())

				DeferCleanup(func() {
					_ = k8sClient.Delete(ctx, job)
				})

				Expect(job.Spec.BackoffLimit).To(HaveValue(BeNumerically("==", 0)))
				Expect(job.Spec.Template.Spec.RestartPolicy).To(Equal(core.RestartPolicyNever),
					"a backoff limit of 0 only means one pod if the pod itself is not restarted")
			},
			Entry("a reset Job", "plan-backoff-reset", "evt-backoff-reset", intelv1a1.RecoveryTypeSlot),
			Entry("a reflash Job", "plan-backoff-reflash", "evt-backoff-reflash", intelv1a1.RecoveryTypeReflash),
		)
	})

	// Recovery Jobs must be owned by the plan. Without an owner reference the Owns(&batch.Job{})
	// watch in SetupWithManager never fires, so every state transition waits out a full
	// RequeueDelay, and any Job that deleteAllJobs misses leaks.
	Context("Owner references on recovery Jobs", func() {
		// planWithEvent returns a plan (created in the API server, so it has a UID for the owner
		// reference) plus a pending event of the given recovery type.
		planWithEvent := func(planName, evtID string, rt intelv1a1.RecoveryType) *intelv1a1.GPURecoveryPlan {
			p := &intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{Name: planName},
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					DeviceID:         "0xabcd",
					XpuSmi: intelv1a1.XpuSmiSpec{
						Image: "local/xpusmi:devel",
					},
				},
				Status: intelv1a1.GPURecoveryPlanStatus{
					Events: []intelv1a1.RecoveryEvent{
						{
							ID:           evtID,
							NodeName:     "node01",
							GPUBDF:       "0000:02:00.0",
							RecoveryType: intelv1a1.RecoveryTypeSpec{Type: rt},
							State:        intelv1a1.RecoveryEventStateWaitingApproval,
							LastUpdated:  ptr.To(metav1.Now()),
						},
					},
				},
			}

			createPlanForOwnerRef(p)

			return p
		}

		// expectOwned asserts the Job carries exactly one controller reference pointing at the
		// plan, with the fields the garbage collector and the owner handler both require.
		expectOwned := func(jobName string, p *intelv1a1.GPURecoveryPlan) *batch.Job {
			job := &batch.Job{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: jobName, Namespace: "default",
			}, job)).To(Succeed())

			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, job)
			})

			Expect(job.OwnerReferences).To(HaveLen(1))

			ref := job.OwnerReferences[0]
			Expect(ref.Kind).To(Equal("GPURecoveryPlan"))
			Expect(ref.APIVersion).To(Equal(intelv1a1.GroupVersion.String()))
			Expect(ref.Name).To(Equal(p.Name))
			// A UID mismatch makes the GC treat the reference as dangling and delete the Job.
			Expect(ref.UID).To(Equal(p.UID))
			Expect(ref.Controller).To(HaveValue(BeTrue()))

			return job
		}

		It("should own a reset Job", func() {
			r := newTestReconciler()
			p := planWithEvent("plan-own-reset", "evt-own-reset", intelv1a1.RecoveryTypeSBR)

			Expect(r.createRecoveryJob(ctx, p, &p.Status.Events[0])).To(Succeed())

			job := expectOwned("recovery-evt-own-reset-0", p)
			Expect(job.Labels).To(HaveKeyWithValue(recoveryJobLabelPlan, "plan-own-reset"))
			Expect(job.Labels).To(HaveKeyWithValue(recoveryJobLabelEvent, "evt-own-reset"))
			Expect(p.Status.Events[0].State).To(Equal(intelv1a1.RecoveryEventStateInProgress))
		})

		// The reset writes to the GPU's PCIe config space through sysfs on one specific node, and
		// nothing else places the pod there: NodeName is set directly, which bypasses the
		// scheduler, so the blanket toleration is what keeps the taint manager from evicting it
		// mid-reset off a node that is already fenced off as broken.
		It("should pin the Job to the event's node and tolerate its taints", func() {
			r := newTestReconciler()
			p := planWithEvent("plan-own-pinned", "evt-own-pinned", intelv1a1.RecoveryTypeSlot)
			p.Spec.XpuSmi = intelv1a1.XpuSmiSpec{Image: "registry/xpu-smi:v1", PullPolicy: "Always"}
			p.Spec.Tolerations = []core.Toleration{{Key: "extra", Operator: core.TolerationOpExists}}

			Expect(r.createRecoveryJob(ctx, p, &p.Status.Events[0])).To(Succeed())

			job := expectOwned("recovery-evt-own-pinned-0", p)

			Expect(job.Spec.Template.Spec.NodeName).To(Equal("node01"))
			Expect(job.Spec.Template.Spec.Tolerations).To(ContainElement(
				core.Toleration{Operator: core.TolerationOpExists}))
			Expect(job.Spec.Template.Spec.Tolerations).To(ContainElement(
				core.Toleration{Key: "extra", Operator: core.TolerationOpExists}))

			resetter := containerByName(job.Spec.Template.Spec.Containers, resetJobContainer)
			Expect(resetter).NotTo(BeNil())
			Expect(resetter.Image).To(Equal("registry/xpu-smi:v1"))
			Expect(resetter.ImagePullPolicy).To(Equal(core.PullAlways))
			// The BDF has to reach the command line, not just the event. One argument, because the
			// template runs /bin/sh -c: see the reflash Job's specs for what splitting it costs.
			Expect(resetter.Command).To(Equal([]string{"/bin/sh", "-c"}))
			Expect(resetter.Args).To(Equal([]string{"xpu-smi config -d 0000:02:00.0 --coldreset"}))
		})

		It("should give the Job the operator's own pull secret", func() {
			r := newTestReconciler()
			r.Opts.SecretName = "operator-pull-secret"

			p := planWithEvent("plan-own-secret", "evt-own-secret", intelv1a1.RecoveryTypeSBR)

			Expect(r.createRecoveryJob(ctx, p, &p.Status.Events[0])).To(Succeed())

			job := expectOwned("recovery-evt-own-secret-0", p)
			Expect(job.Spec.Template.Spec.ImagePullSecrets).To(ConsistOf(
				core.LocalObjectReference{Name: "operator-pull-secret"}))
		})

		// The reference existing is not the same as it being usable. This drives the real
		// controller-runtime owner handler to prove the emitted request is what Reconcile expects:
		// a cluster-scoped owner must yield Request{Name: plan} with NO namespace, otherwise the
		// lookup would target "default/plan-..." and silently never match.
		It("should enqueue a namespace-less request for the plan on a Job event", func() {
			r := newTestReconciler()
			p := planWithEvent("plan-own-enqueue", "evt-own-enqueue", intelv1a1.RecoveryTypeSBR)

			Expect(r.createRecoveryJob(ctx, p, &p.Status.Events[0])).To(Succeed())

			job := expectOwned("recovery-evt-own-enqueue-0", p)

			h := handler.EnqueueRequestForOwner(k8sClient.Scheme(), k8sClient.RESTMapper(),
				&intelv1a1.GPURecoveryPlan{}, handler.OnlyControllerOwner())

			q := &trackingQueue{}
			h.Update(ctx, event.UpdateEvent{ObjectOld: job, ObjectNew: job}, q)

			Expect(q.added).To(ConsistOf(reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "plan-own-enqueue"},
			}))
		})

		// An earlier pass may have created the Job and lost its status write. The name embeds the
		// event ID and attempt index, so the existing Job is the very one this attempt wanted:
		// adopting it is right, and failing would strand the event in waiting-approval forever.
		It("should adopt an existing Job of the same name", func() {
			r := newTestReconciler()
			p := planWithEvent("plan-own-adopt", "evt-own-adopt", intelv1a1.RecoveryTypeSBR)

			Expect(r.createRecoveryJob(ctx, p, &p.Status.Events[0])).To(Succeed())
			expectOwned("recovery-evt-own-adopt-0", p)

			// Second attempt at the same event, still on attempt 0.
			p.Status.Events[0].State = intelv1a1.RecoveryEventStateWaitingApproval
			p.Status.Events[0].JobName = ""

			Expect(r.createRecoveryJob(ctx, p, &p.Status.Events[0])).To(Succeed())

			Expect(p.Status.Events[0].State).To(Equal(intelv1a1.RecoveryEventStateInProgress))
			Expect(p.Status.Events[0].JobName).To(Equal("recovery-evt-own-adopt-0"))
		})

		// The attempt index in the name is what keeps a re-approved attempt from colliding with the Job that
		// already failed — and that Job is still there, kept for diagnostics.
		It("should name a re-approved attempt after its attempt index", func() {
			r := newTestReconciler()
			p := planWithEvent("plan-own-retry", "evt-own-retry", intelv1a1.RecoveryTypeSBR)
			p.Status.Events[0].PastJobs = []string{"recovery-evt-own-retry-0"}

			Expect(r.createRecoveryJob(ctx, p, &p.Status.Events[0])).To(Succeed())

			expectOwned("recovery-evt-own-retry-1", p)
			Expect(p.Status.Events[0].JobName).To(Equal("recovery-evt-own-retry-1"))
		})
	})

	// Both createResetJob and createReflashJob hand their container exactly one argument, so both
	// templates have to keep a shell as their command. This is a contract between Go and YAML that
	// nothing else checks: a template switched back to running the binary directly would exec a whole
	// command line as argv[0] and fail inside a pod with a "no such file" naming the entire string.
	//
	// Going through a shell is also what keeps the path to xpu-smi out of the operator. The plan names
	// an image; where that image keeps its binary is its own business, so the command line invokes
	// xpu-smi by name and lets PATH resolve it.
	Context("Recovery Job templates: the shell contract", func() {
		DescribeTable("should run xpu-smi through a shell, one command line at a time",
			func(job *batch.Job, containerName string) {
				c := containerByName(job.Spec.Template.Spec.Containers, containerName)
				Expect(c).NotTo(BeNil())
				Expect(c.Command).To(Equal([]string{"/bin/sh", "-c"}))
				Expect(c.Args).To(HaveLen(1),
					"the template's own args stand in for what the operator writes, so they must be one command line too")
			},
			Entry("reset", deployments.XpuManagerResetJob(), resetJobContainer),
			Entry("reflash", deployments.XpuManagerFWUpdateJob(), reflashJobContainer),
		)
	})

	Context("Reconcile: long node names still produce a creatable Job", func() {
		It("should create the recovery Job for a node name well over the limit", func() {
			r := newTestReconciler()
			key := types.NamespacedName{Name: "plan-long-node"}

			// 62 characters on its own — longer than the whole Job-name budget, and the kind of
			// name a real cloud provider hands out.
			const longNode = "ip-10-0-134-22.us-west-2.compute.internal.example-cluster.prod"

			// The Node has to exist for the pre-reset drain to taint it. Nothing here is about
			// draining — the node carries no pods, so the drain converges on its first pass — but
			// without the object the drain errors and the event never reaches a Job, which would
			// fail this spec for an unrelated reason.
			node := &core.Node{ObjectMeta: metav1.ObjectMeta{Name: longNode}}
			Expect(k8sClient.Create(ctx, node)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, node)
			})

			slice := &resv1.ResourceSlice{
				ObjectMeta: metav1.ObjectMeta{Name: "slice-long-node"},
				Spec: resv1.ResourceSliceSpec{
					Driver:   "gpu.intel.com",
					NodeName: ptr.To(longNode),
					Pool:     resv1.ResourcePool{Name: "pool-long-node", ResourceSliceCount: 1},
					Devices: []resv1.Device{{
						Name: "dev-0000-af-00-0",
						Attributes: map[resv1.QualifiedName]resv1.DeviceAttribute{
							deviceAttrDeviceID: {StringValue: ptr.To("0xbeef")},
							// Uppercase hex, as lspci prints it: this must be sanitized rather
							// than rejected.
							deviceAttrBDF: {StringValue: ptr.To("0000:AF:00.0")},
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

			p := &intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{Name: key.Name, Finalizers: []string{recoveryPlanFinalizer}},
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					DeviceID:         "0xbeef",
					XpuSmi:           intelv1a1.XpuSmiSpec{Image: "registry/xpu-smi:latest"},
					Approvals: []intelv1a1.RecoveryApproval{{
						ID:       "app-any-reset",
						Selector: &intelv1a1.ApprovalSelector{RecoveryType: intelv1a1.RecoveryTypeSlot},
					}},
				},
			}
			Expect(k8sClient.Create(ctx, p)).To(Succeed())
			DeferCleanup(func() {
				fresh := &intelv1a1.GPURecoveryPlan{}
				if err := k8sClient.Get(ctx, key, fresh); err == nil {
					fresh.Finalizers = nil
					_ = k8sClient.Update(ctx, fresh)
					_ = k8sClient.Delete(ctx, fresh)
				}
			})

			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			updated := &intelv1a1.GPURecoveryPlan{}
			Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
			Expect(updated.Status.Events).To(HaveLen(1))

			evt := updated.Status.Events[0]

			// in-progress with a jobName is the proof: had the create been rejected, the event
			// would still be waiting-approval with no Job, and the only trace would be a status
			// message.
			Expect(evt.State).To(Equal(intelv1a1.RecoveryEventStateInProgress),
				"event should be in-progress; messages: %v", updated.Status.Messages)
			Expect(evt.JobName).NotTo(BeEmpty())

			job := &batch.Job{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: evt.JobName, Namespace: "default"}, job)).
				To(Succeed(), "the Job the event claims to own must actually exist")

			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, job)
			})

			// The label selector deleteEventJobs uses is subject to the same 63-byte cap as the
			// name, so read it back off the created object rather than trusting it.
			Expect(job.Labels[recoveryJobLabelEvent]).To(Equal(evt.ID))
		})
	})
})
