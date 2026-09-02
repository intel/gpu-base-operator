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
	batch "k8s.io/api/batch/v1"
	core "k8s.io/api/core/v1"
	resv1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	intelv1a1 "github.com/intel/gpu-base-operator/api/v1alpha1"
)

// makeTestJob builds a minimal batch.Job with the given condition pre-set, suitable for creating
// in the envtest API server to drive syncJobStatuses and the deletion paths.
func makeTestJob(name, ns string, labels map[string]string, condType batch.JobConditionType) *batch.Job {
	return &batch.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    labels,
		},
		Spec: batch.JobSpec{
			Template: core.PodTemplateSpec{
				Spec: core.PodSpec{
					RestartPolicy: core.RestartPolicyNever,
					Containers: []core.Container{
						{Name: "c", Image: "busybox"},
					},
				},
			},
		},
		Status: batch.JobStatus{
			Conditions: []batch.JobCondition{
				{Type: condType, Status: core.ConditionTrue},
			},
		},
	}
}

// trackingQueue is a minimal workqueue that only records what was Added, so a real
// controller-runtime EventHandler can be driven in a test and its output inspected.
type trackingQueue struct {
	workqueue.TypedRateLimitingInterface[reconcile.Request]

	added []reconcile.Request
}

func (q *trackingQueue) Add(item reconcile.Request) {
	q.added = append(q.added, item)
}

// failingStatusClient makes writes fail on demand so the error paths in persistPlan can be
// driven. Status().Update() always fails; Update() fails only when failUpdate is set, so a spec
// write can still be observed to land after a failed status write.
type failingStatusClient struct {
	client.Client

	failUpdate bool
}

func (c *failingStatusClient) Status() client.SubResourceWriter {
	return &failingSubResourceWriter{SubResourceWriter: c.Client.Status()}
}

func (c *failingStatusClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if c.failUpdate {
		return fmt.Errorf("synthetic spec update failure")
	}

	return c.Client.Update(ctx, obj, opts...)
}

type failingSubResourceWriter struct {
	client.SubResourceWriter
}

func (w *failingSubResourceWriter) Update(
	ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption,
) error {
	return fmt.Errorf("synthetic status update failure")
}

// recordingClient notes the order in which the status and spec sub-writes reach the API server.
// persistPlan must write status before spec: the spec write (consuming a one-shot approval)
// triggers an immediate new reconcile, and if that reconcile read a stale status it would still
// see the event as waiting-approval and could act twice.
type recordingClient struct {
	client.Client

	writes *[]string
}

func (c *recordingClient) Status() client.SubResourceWriter {
	return &recordingSubResourceWriter{SubResourceWriter: c.Client.Status(), writes: c.writes}
}

func (c *recordingClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	*c.writes = append(*c.writes, "spec")

	return c.Client.Update(ctx, obj, opts...)
}

type recordingSubResourceWriter struct {
	client.SubResourceWriter

	writes *[]string
}

func (w *recordingSubResourceWriter) Update(
	ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption,
) error {
	*w.writes = append(*w.writes, "status")

	return w.SubResourceWriter.Update(ctx, obj, opts...)
}

// createPlanForOwnerRef persists an in-memory plan fixture so it gains a UID, then copies the
// server-assigned metadata back onto the fixture. Recovery Jobs carry a controller reference to
// the plan and the API server rejects an ownerReference with an empty UID, so any spec that drives
// Job creation needs a plan that really exists — which mirrors production, where Reconcile only
// ever works on an object it just Got. Status is preserved: the fixtures set status.events
// directly and rely on it.
func createPlanForOwnerRef(p *intelv1a1.GPURecoveryPlan) {
	status := p.Status

	toCreate := p.DeepCopy()
	Expect(k8sClient.Create(context.Background(), toCreate)).To(Succeed())

	DeferCleanup(func() {
		_ = k8sClient.Delete(context.Background(), toCreate)
	})

	p.ObjectMeta = toCreate.ObjectMeta
	p.Status = status
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

	Context("Finalizer management", func() {
		It("should add the finalizer on first reconcile", func() {
			// The shared plan is created with the finalizer already on it, so this uses its own
			// object to exercise the path that puts it there.
			key := types.NamespacedName{Name: "plan-finalizer-add"}
			p := &intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{Name: key.Name},
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot, DeviceID: "0xabcd",
				},
			}
			Expect(k8sClient.Create(ctx, p)).To(Succeed())

			DeferCleanup(func() {
				stale := &intelv1a1.GPURecoveryPlan{}
				if err := k8sClient.Get(ctx, key, stale); err == nil {
					stale.Finalizers = nil
					_ = k8sClient.Update(ctx, stale)
					_ = k8sClient.Delete(ctx, stale)
				}
			})

			_, err := reconcilePlan(ctx, key.Name)
			Expect(err).NotTo(HaveOccurred())

			updated := &intelv1a1.GPURecoveryPlan{}
			Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
			Expect(updated.Finalizers).To(ContainElement(recoveryPlanFinalizer))
		})
	})

	// The finalizer exists to stop a plan deletion from killing a pod that is mid-reset. These
	// specs pin that behaviour: deletion must block while any Job is non-terminal, and must
	// complete once they all are.
	//
	// envtest runs no Job controller, so a Job created without conditions stays non-terminal
	// indefinitely — which is exactly the "in-flight" state being tested.
	Context("Finalizer: deletion blocks on in-flight recovery Jobs", func() {
		// Each spec uses its own plan name: a plan whose finalizer is cleared during cleanup is
		// garbage-collected asynchronously, so reusing one name races the next Create.
		var (
			delPlanName string
			delPlanKey  types.NamespacedName
		)

		// newTerminatingPlan creates a plan carrying the finalizer, deletes it so that a real
		// deletionTimestamp is set by the API server, and returns it still present in etcd.
		newTerminatingPlan := func(name string, events []intelv1a1.RecoveryEvent) *intelv1a1.GPURecoveryPlan {
			delPlanName = name
			delPlanKey = types.NamespacedName{Name: name}

			p := &intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{
					Name:       name,
					Finalizers: []string{recoveryPlanFinalizer},
				},
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot, DeviceID: "0xabcd",
				},
			}
			Expect(k8sClient.Create(ctx, p)).To(Succeed())

			DeferCleanup(func() {
				stale := &intelv1a1.GPURecoveryPlan{}
				if err := k8sClient.Get(ctx, delPlanKey, stale); err == nil {
					stale.Finalizers = nil
					_ = k8sClient.Update(ctx, stale)
				}
			})

			if len(events) > 0 {
				p.Status.Events = events
				Expect(k8sClient.Status().Update(ctx, p)).To(Succeed())
			}

			Expect(k8sClient.Delete(ctx, p)).To(Succeed())

			// The finalizer holds it alive; re-read to pick up the deletionTimestamp.
			Expect(k8sClient.Get(ctx, delPlanKey, p)).To(Succeed())
			Expect(p.DeletionTimestamp.IsZero()).To(BeFalse())

			return p
		}

		// createJob puts a Job for delPlanName in the API server. When terminal is true the status
		// subresource is driven to Failed; otherwise it is left condition-less and so counts as
		// still running.
		createJob := func(name string, terminal bool) *batch.Job {
			job := makeTestJob(name, "default", map[string]string{
				recoveryJobLabelPlan:  delPlanName,
				recoveryJobLabelEvent: "evt-del-001",
			}, batch.JobFailed)
			job.Status = batch.JobStatus{} // status is not settable on create

			Expect(k8sClient.Create(ctx, job)).To(Succeed())

			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, job)
			})

			if terminal {
				// K8s 1.36 requires startTime + FailureTarget=True before Failed=True.
				startTime := metav1.Now()
				job.Status = batch.JobStatus{
					StartTime: &startTime,
					Conditions: []batch.JobCondition{
						{Type: batch.JobFailureTarget, Status: core.ConditionTrue},
						{Type: batch.JobFailed, Status: core.ConditionTrue},
					},
				}
				Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())
			}

			return job
		}

		It("should keep the finalizer and requeue while a Job is still running", func() {
			p := newTerminatingPlan("plan-del-running", []intelv1a1.RecoveryEvent{
				{
					ID:           "evt-del-001",
					NodeName:     "node01",
					GPUBDF:       "0000:02:00.0",
					RecoveryType: intelv1a1.RecoveryTypeSpec{Type: intelv1a1.RecoveryTypeSBR},
					State:        intelv1a1.RecoveryEventStateInProgress,
					JobName:      "recovery-evt-del-001-0",
					LastUpdated:  ptr.To(metav1.Now()),
				},
			})
			Expect(p).NotTo(BeNil())

			createJob("recovery-evt-del-001-0", false)

			result, err := reconcilePlan(ctx, delPlanName)

			// requeue-not-an-error: the caller sees a nil error plus a RequeueAfter.
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(2 * time.Second))

			By("keeping the finalizer so the object is not garbage-collected")
			updated := &intelv1a1.GPURecoveryPlan{}
			Expect(k8sClient.Get(ctx, delPlanKey, updated)).To(Succeed())
			Expect(updated.Finalizers).To(ContainElement(recoveryPlanFinalizer))

			By("not deleting the in-flight Job")
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      "recovery-evt-del-001-0",
				Namespace: "default",
			}, &batch.Job{})).To(Succeed(), "an in-flight reset Job must not be killed by plan deletion")

			By("recording why deletion is waiting")
			Expect(updated.Status.Messages).To(ContainElement(
				ContainSubstring("Deletion waiting for 1 active recovery Job(s)")))
		})

		It("should remove the finalizer and delete Jobs once all are terminal", func() {
			p := newTerminatingPlan("plan-del-terminal", []intelv1a1.RecoveryEvent{
				{
					ID:           "evt-del-001",
					NodeName:     "node01",
					GPUBDF:       "0000:02:00.0",
					RecoveryType: intelv1a1.RecoveryTypeSpec{Type: intelv1a1.RecoveryTypeSBR},
					State:        intelv1a1.RecoveryEventStateFailed,
					PastJobs:     []string{"recovery-evt-del-002-0"},
					LastUpdated:  ptr.To(metav1.Now()),
				},
			})
			Expect(p).NotTo(BeNil())

			createJob("recovery-evt-del-002-0", true)

			_, err := reconcilePlan(ctx, delPlanName)
			Expect(err).NotTo(HaveOccurred())

			By("letting the object go away")
			Eventually(func() bool {
				return errors.IsNotFound(k8sClient.Get(ctx, delPlanKey, &intelv1a1.GPURecoveryPlan{}))
			}, 5*time.Second, 100*time.Millisecond).Should(BeTrue())

			By("cleaning up the terminal Job")
			Eventually(func() bool {
				return errors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{
					Name:      "recovery-evt-del-002-0",
					Namespace: "default",
				}, &batch.Job{}))
			}, 5*time.Second, 100*time.Millisecond).Should(BeTrue())
		})

		It("should delete a terminal labelled Job that no event references", func() {
			// Complements the blocking case: once status.events has been pruned, the label is the
			// only handle left on the Job, so cleanup must not rely on the events walk or the Job
			// would leak after the plan is gone.
			p := newTerminatingPlan("plan-del-orphan-cleanup", nil)
			Expect(p.Status.Events).To(BeEmpty())

			createJob("recovery-orphan-terminal-0", true)

			_, err := reconcilePlan(ctx, delPlanName)
			Expect(err).NotTo(HaveOccurred())

			Eventually(func() bool {
				return errors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{
					Name:      "recovery-orphan-terminal-0",
					Namespace: "default",
				}, &batch.Job{}))
			}, 5*time.Second, 100*time.Millisecond).Should(BeTrue(),
				"a Job with no event entry must still be cleaned up via its plan label")
		})

		It("should block on a labelled Job even when no event references it", func() {
			// A Job whose event entry was already pruned from status must still be waited for —
			// otherwise pruning status would silently unblock a live reset.
			p := newTerminatingPlan("plan-del-orphan", nil)
			Expect(p.Status.Events).To(BeEmpty())

			createJob("recovery-orphan-0", false)

			result, err := reconcilePlan(ctx, delPlanName)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(2 * time.Second))

			updated := &intelv1a1.GPURecoveryPlan{}
			Expect(k8sClient.Get(ctx, delPlanKey, updated)).To(Succeed())
			Expect(updated.Finalizers).To(ContainElement(recoveryPlanFinalizer))
		})

		It("should not wait for a Job that is already terminating", func() {
			r := newTestReconciler()
			delPlanName = "plan-del-terminating"
			p := &intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{Name: delPlanName},
			}

			// A Job finalizer keeps the object readable after Delete, so it is observable in the
			// Terminating state that runningRecoveryJobs must skip.
			job := makeTestJob("recovery-terminating-0", "default", map[string]string{
				recoveryJobLabelPlan: delPlanName,
			}, batch.JobFailed)
			job.Status = batch.JobStatus{}
			job.Finalizers = []string{"test.intel.com/hold"}

			Expect(k8sClient.Create(ctx, job)).To(Succeed())

			DeferCleanup(func() {
				stale := &batch.Job{}
				if err := k8sClient.Get(ctx, types.NamespacedName{
					Name: "recovery-terminating-0", Namespace: "default",
				}, stale); err == nil {
					stale.Finalizers = nil
					_ = k8sClient.Update(ctx, stale)
				}
			})

			By("confirming it counts as running before deletion")
			running, err := runningRecoveryJobs(r.Client, ctx, r.Opts.Namespace, p)
			Expect(err).NotTo(HaveOccurred())
			Expect(running).To(ConsistOf("recovery-terminating-0"))

			Expect(k8sClient.Delete(ctx, job)).To(Succeed())

			By("no longer counting it once a deletionTimestamp is set")
			Eventually(func() []string {
				running, err := runningRecoveryJobs(r.Client, ctx, r.Opts.Namespace, p)
				Expect(err).NotTo(HaveOccurred())

				return running
			}, 5*time.Second, 100*time.Millisecond).Should(BeEmpty())
		})
	})

	Context("Helper: jobIsTerminal", func() {
		cond := func(t batch.JobConditionType, s core.ConditionStatus) batch.JobCondition {
			return batch.JobCondition{Type: t, Status: s}
		}
		jobWith := func(conds ...batch.JobCondition) *batch.Job {
			return &batch.Job{Status: batch.JobStatus{Conditions: conds}}
		}

		It("should treat a Job with no conditions as running", func() {
			Expect(jobIsTerminal(jobWith())).To(BeFalse())
		})

		It("should treat Complete=True as terminal", func() {
			Expect(jobIsTerminal(jobWith(cond(batch.JobComplete, core.ConditionTrue)))).To(BeTrue())
		})

		It("should treat Failed=True as terminal", func() {
			Expect(jobIsTerminal(jobWith(cond(batch.JobFailed, core.ConditionTrue)))).To(BeTrue())
		})

		It("should not treat Complete=False as terminal", func() {
			Expect(jobIsTerminal(jobWith(cond(batch.JobComplete, core.ConditionFalse)))).To(BeFalse())
		})

		It("should not treat intermediate conditions as terminal", func() {
			// SuccessCriteriaMet/FailureTarget precede the real terminal condition; acting on them
			// would cut a Job's pod off before it has actually finished.
			Expect(jobIsTerminal(jobWith(
				cond(batch.JobSuspended, core.ConditionTrue),
				cond(batch.JobSuccessCriteriaMet, core.ConditionTrue),
				cond(batch.JobFailureTarget, core.ConditionTrue),
			))).To(BeFalse())
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
				appendMessage(p, "msg")
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

		It("should reset the approval state and retry budget", func() {
			p := &intelv1a1.GPURecoveryPlan{ObjectMeta: metav1.ObjectMeta{Name: "plan-esc-reset"}}
			evt := waitingEvent(intelv1a1.RecoveryTypeSlot)

			escalateEvent(p, evt, deviceNeed{rt: intelv1a1.RecoveryTypeReflash, reason: reasonSurvivability})

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
			updatePlanState(p)
			Expect(p.Status.State).To(Equal(intelv1a1.PlanStateIdle))
		})

		DescribeTable("should report active while an event is on its way somewhere",
			func(state intelv1a1.RecoveryEventState) {
				p := planWith(evt(state, 0))
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
			p := planWith(evt(intelv1a1.RecoveryEventStateSucceeded, 1))
			updatePlanState(p)
			Expect(p.Status.State).To(Equal(intelv1a1.PlanStateIdle))
		})

		It("should report error for an event that exhausted its retry budget", func() {
			p := planWith(evt(intelv1a1.RecoveryEventStateFailed, 3))
			updatePlanState(p)
			Expect(p.Status.State).To(Equal(intelv1a1.PlanStateError),
				"an exhausted event never retries on its own; it needs an explicit re-approval")
		})

		// A failure inside the budget is transient: the event is put back to waiting-approval on
		// a later pass, so surfacing "error" would be a false alarm.
		It("should not report error for a failure still inside its retry budget", func() {
			p := planWith(evt(intelv1a1.RecoveryEventStateFailed, 1))
			updatePlanState(p)
			Expect(p.Status.State).NotTo(Equal(intelv1a1.PlanStateError))
		})

		It("should report error when a reflash is blocked on missing firmware", func() {
			p := planWith(evt(intelv1a1.RecoveryEventStateMissingFirmware, 0))
			updatePlanState(p)
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
			updatePlanState(p)
			Expect(p.Status.State).To(Equal(intelv1a1.PlanStateError))
		})
	})

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

		// An event with no recovery type cannot be authorised: a selector with an empty
		// recoveryType means "any reset", and matching it against an event whose own type is
		// unknown would approve a reset nobody chose.
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
			Expect(recoveryTypeToArgs("0000:02:00.0", evt.RecoveryType.Type)).To(
				ContainElement("--coldreset"), "an override must change the command actually run")
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
					MaxRetries:       3,
					XpuSmi:           intelv1a1.XpuSmiSpec{Image: "registry/xpu-smi:latest"},
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
					MaxRetries:       3,
					XpuSmi:           intelv1a1.XpuSmiSpec{Image: "registry/xpu-smi:latest"},
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
							ID: "evt-batch-2", NodeName: "node01", GPUBDF: "0000:03:00.0",
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
					MaxRetries:       3,
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

		// A reflash cannot be carried out yet, so its approval must stay unspent: consuming it
		// would leave the admin's decision spent on nothing, and they would have to approve again
		// once the operator can do the work.
		It("should park an approved reflash without consuming the approval", func() {
			r := newTestReconciler()

			p := &intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{Name: "plan-reflash-park"},
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					DeviceID:         "0xabcd",
					MaxRetries:       3,
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
			Expect(evt.State).To(Equal(intelv1a1.RecoveryEventStateWaitingApproval))
			Expect(evt.JobName).To(BeEmpty())
			Expect(evt.StateMessage).To(ContainSubstring("reflash"))
			Expect(p.Spec.Approvals[0].Consumed).To(BeFalse(),
				"an approval that produced no Job must stay available")

			job := &batch.Job{}
			err := k8sClient.Get(ctx, types.NamespacedName{
				Name: "recovery-evt-reflash-park-0", Namespace: "default",
			}, job)
			Expect(errors.IsNotFound(err)).To(BeTrue(),
				"a reflash event must not be answered with a reset Job")
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
					MaxRetries:       2,
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
							RetryCount:   2, // exhausted
							LastUpdated:  &now,
							ApprovalID:   "old-approval",
						},
					},
				},
			}

			createPlanForOwnerRef(p)

			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, &batch.Job{ObjectMeta: metav1.ObjectMeta{
					Name: "recovery-evt-exhausted-0", Namespace: "default",
				}})
			})

			r.processApprovals(ctx, p)

			evt := &p.Status.Events[0]
			Expect(evt.RetryCount).To(BeZero(), "retry count must be reset on re-approval")
			Expect(evt.ApprovalID).To(Equal("reapp-001"))
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
					MaxRetries:       2,
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
							RetryCount:   2, // exhausted
							LastUpdated:  &now,
						},
					},
				},
			}

			r.processApprovals(ctx, p)

			// State must remain failed — a standing approval must not keep retrying a GPU that has
			// already failed its way out of the budget that same approval granted.
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

	Context("Helper: recoveryTypeToArgs", func() {
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
				args := recoveryTypeToArgs(bdf, rt)
				Expect(args).NotTo(BeEmpty(), "reset type %q must map to a command", rt)

				key := strings.Join(args, " ")
				Expect(seen).NotTo(HaveKey(key),
					"reset types %q and %q share the command %q, so an override between them is a no-op",
					seen[key], rt, key)

				seen[key] = rt
			}
		})

		It("should address the BDF the event names", func() {
			Expect(recoveryTypeToArgs("0000:af:00.0", intelv1a1.RecoveryTypeSBR)).
				To(ContainElement("0000:af:00.0"))
		})

		It("should return nil for reflash, which is not an xpu-smi reset", func() {
			Expect(recoveryTypeToArgs(bdf, intelv1a1.RecoveryTypeReflash)).To(BeNil())
		})

		It("should return nil for a type outside the enum", func() {
			Expect(recoveryTypeToArgs(bdf, intelv1a1.RecoveryType("flr"))).To(BeNil())
		})
	})

	// Jobs are kept for as long as the event they belong to, so an admin looking at a GPU can still
	// read the pod that touched it. That is only true if the terminal-Job handling moves the name
	// into pastJobs rather than deleting the object.
	Context("Job outcomes: Jobs retained for diagnostics", func() {
		// putJob creates a Job and drives its status subresource to the given terminal condition.
		// K8s 1.36 requires startTime plus the interim condition before the terminal one.
		putJob := func(name, planName, evtID string, complete bool, reason string) {
			job := makeTestJob(name, "default", map[string]string{
				recoveryJobLabelPlan:  planName,
				recoveryJobLabelEvent: evtID,
			}, batch.JobComplete)
			job.Status = batch.JobStatus{} // status is not settable on create

			Expect(k8sClient.Create(ctx, job)).To(Succeed())

			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, job)
			})

			startTime := metav1.Now()

			if complete {
				job.Status = batch.JobStatus{
					StartTime:      &startTime,
					CompletionTime: &startTime,
					Conditions: []batch.JobCondition{
						{Type: batch.JobSuccessCriteriaMet, Status: core.ConditionTrue},
						{Type: batch.JobComplete, Status: core.ConditionTrue},
					},
				}
			} else {
				job.Status = batch.JobStatus{
					StartTime: &startTime,
					Conditions: []batch.JobCondition{
						{Type: batch.JobFailureTarget, Status: core.ConditionTrue},
						{Type: batch.JobFailed, Status: core.ConditionTrue, Reason: reason},
					},
				}
			}

			Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())
		}

		planInProgress := func(planName, evtID, jobName string) *intelv1a1.GPURecoveryPlan {
			return &intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{Name: planName},
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					DeviceID:         "0xabcd",
					MaxRetries:       3,
				},
				Status: intelv1a1.GPURecoveryPlanStatus{
					Events: []intelv1a1.RecoveryEvent{
						{
							ID:           evtID,
							NodeName:     "node01",
							GPUBDF:       "0000:02:00.0",
							RecoveryType: intelv1a1.RecoveryTypeSpec{Type: intelv1a1.RecoveryTypeSBR},
							State:        intelv1a1.RecoveryEventStateInProgress,
							JobName:      jobName,
						},
					},
				},
			}
		}

		It("should move JobName to PastJobs and not delete the Job when a Job fails", func() {
			r := newTestReconciler()
			p := planInProgress("plan-fail-diag", "evt-fail-001", "recovery-evt-fail-001-0")

			putJob("recovery-evt-fail-001-0", p.Name, "evt-fail-001", false, "BackoffLimitExceeded")

			Expect(r.syncJobStatuses(ctx, p)).To(Succeed())

			evt := &p.Status.Events[0]
			Expect(evt.State).To(Equal(intelv1a1.RecoveryEventStateFailed))
			Expect(evt.PastJobs).To(ContainElement("recovery-evt-fail-001-0"))
			Expect(evt.JobName).To(BeEmpty())
			Expect(evt.RetryCount).To(BeNumerically("==", 1))

			// The Job outlives the event, but its pods do not outlive the plan's cleanup, so the
			// verdict is copied onto the event. The attempt count is what says whether the operator
			// will try again — retryCount alone does not, without also knowing spec.maxRetries.
			Expect(evt.StateMessage).To(SatisfyAll(
				ContainSubstring("recovery-evt-fail-001-0"),
				ContainSubstring("attempt 1 of 3"),
				ContainSubstring("BackoffLimitExceeded"),
			))

			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      "recovery-evt-fail-001-0",
				Namespace: "default",
			}, &batch.Job{})).To(Succeed(), "a failed Job must remain for diagnostics until the event is removed")
		})

		It("should move JobName to PastJobs and not delete the Job when a Job succeeds", func() {
			r := newTestReconciler()
			p := planInProgress("plan-success-retain", "evt-ok-001", "recovery-evt-ok-001-0")

			putJob("recovery-evt-ok-001-0", p.Name, "evt-ok-001", true, "")

			Expect(r.syncJobStatuses(ctx, p)).To(Succeed())

			evt := &p.Status.Events[0]
			Expect(evt.State).To(Equal(intelv1a1.RecoveryEventStateSucceeded))
			Expect(evt.PastJobs).To(ContainElement("recovery-evt-ok-001-0"))
			Expect(evt.JobName).To(BeEmpty())
			Expect(evt.RetryCount).To(BeZero())

			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      "recovery-evt-ok-001-0",
				Namespace: "default",
			}, &batch.Job{})).To(Succeed(), "a succeeded Job must remain until the event is removed")
		})

		// A Job that has gone missing is not a failure of the recovery: reporting one would burn a
		// retry and could park the event in failed while the reset it started is still running.
		It("should leave an event in-progress when its Job cannot be read", func() {
			r := newTestReconciler()
			p := planInProgress("plan-job-missing", "evt-missing-001", "recovery-evt-missing-001-0")

			Expect(r.syncJobStatuses(ctx, p)).To(Succeed())

			Expect(p.Status.Events[0].State).To(Equal(intelv1a1.RecoveryEventStateInProgress))
			Expect(p.Status.Events[0].RetryCount).To(BeZero())
		})

		It("should report an in-flight Job as an active Job", func() {
			Expect(hasActiveJobs(planInProgress("plan-active", "evt-a", "job-a"))).To(BeTrue())

			done := planInProgress("plan-done", "evt-b", "")
			done.Status.Events[0].State = intelv1a1.RecoveryEventStateSucceeded
			Expect(hasActiveJobs(done)).To(BeFalse())
		})
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
					MaxRetries:       3,
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

			resetter := findJobContainer(job, "resetter")
			Expect(resetter).NotTo(BeNil())
			Expect(resetter.Image).To(Equal("registry/xpu-smi:v1"))
			Expect(resetter.ImagePullPolicy).To(Equal(core.PullAlways))
			// The BDF has to reach the command line, not just the event.
			Expect(resetter.Args).To(Equal([]string{"config", "-d", "0000:02:00.0", "--coldreset"}))
		})

		// pullPolicy is defaulted by both the CRD and the webhook, so an empty one means an object
		// that never reached the API server. Keeping the template's IfNotPresent then matters:
		// leaving the field empty hands the choice to the kubelet, which picks Always for a
		// ":latest" image — a pull the broken node may not be able to make.
		It("should keep the template's pull policy when the plan states none", func() {
			r := newTestReconciler()
			p := planWithEvent("plan-own-nopolicy", "evt-own-nopolicy", intelv1a1.RecoveryTypeSBR)
			p.Spec.XpuSmi = intelv1a1.XpuSmiSpec{Image: "registry/xpu-smi:latest"}

			Expect(r.createRecoveryJob(ctx, p, &p.Status.Events[0])).To(Succeed())

			job := expectOwned("recovery-evt-own-nopolicy-0", p)

			resetter := findJobContainer(job, "resetter")
			Expect(resetter).NotTo(BeNil())
			Expect(resetter.ImagePullPolicy).To(Equal(core.PullIfNotPresent))
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

		// The attempt index in the name is what keeps a retry from colliding with the Job that
		// already failed — and that Job is still there, kept for diagnostics.
		It("should name a retry after its attempt index", func() {
			r := newTestReconciler()
			p := planWithEvent("plan-own-retry", "evt-own-retry", intelv1a1.RecoveryTypeSBR)
			p.Status.Events[0].PastJobs = []string{"recovery-evt-own-retry-0"}

			Expect(r.createRecoveryJob(ctx, p, &p.Status.Events[0])).To(Succeed())

			expectOwned("recovery-evt-own-retry-1", p)
			Expect(p.Status.Events[0].JobName).To(Equal("recovery-evt-own-retry-1"))
		})
	})

	Context("Reconcile: long node names still produce a creatable Job", func() {
		It("should create the recovery Job for a node name well over the limit", func() {
			r := newTestReconciler()
			key := types.NamespacedName{Name: "plan-long-node"}

			// 62 characters on its own — longer than the whole Job-name budget, and the kind of
			// name a real cloud provider hands out.
			const longNode = "ip-10-0-134-22.us-west-2.compute.internal.example-cluster.prod"

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
					MaxRetries:       3,
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

	// A GPU whose taint is still there after a failed attempt gets another go, up to
	// spec.maxRetries. The event ID is reused, so a standing group approval re-approves the same
	// event rather than fanning one broken GPU out into a flood of events.
	Context("Helper: requeueFailedEvents", func() {
		failedPlan := func(maxRetries int32, retryCount int32) *intelv1a1.GPURecoveryPlan {
			return &intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{Name: "plan-requeue"},
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					DeviceID:         "0xabcd",
					MaxRetries:       maxRetries,
				},
				Status: intelv1a1.GPURecoveryPlanStatus{
					Events: []intelv1a1.RecoveryEvent{
						{
							ID:           "evt-requeue",
							NodeName:     "node01",
							GPUBDF:       "0000:02:00.0",
							RecoveryType: intelv1a1.RecoveryTypeSpec{Type: intelv1a1.RecoveryTypeSBR},
							State:        intelv1a1.RecoveryEventStateFailed,
							RetryCount:   retryCount,
							PastJobs:     []string{"recovery-evt-requeue-0"},
							LastUpdated:  ptr.To(metav1.Now()),
						},
					},
				},
			}
		}

		stillTainted := map[deviceKey]deviceNeed{
			{node: "node01", bdf: "0000:02:00.0"}: {rt: intelv1a1.RecoveryTypeSBR, reason: reasonWedged},
		}

		It("should send a failed event back to waiting-approval while the taint persists", func() {
			p := failedPlan(3, 1)

			requeueFailedEvents(p, stillTainted)

			evt := p.Status.Events[0]
			Expect(evt.State).To(Equal(intelv1a1.RecoveryEventStateWaitingApproval))
			Expect(evt.ID).To(Equal("evt-requeue"), "the ID must be reused so a standing approval re-matches")
			Expect(evt.RetryCount).To(BeNumerically("==", 1), "the budget is spent by the failure, not by the re-queue")
			Expect(evt.PastJobs).To(ContainElement("recovery-evt-requeue-0"))
			Expect(evt.StateMessage).To(ContainSubstring("re-queued"))
			Expect(p.Status.Messages).To(ContainElement(ContainSubstring("re-queued for retry 1/3")))
		})

		It("should leave an event alone once its retry budget is spent", func() {
			p := failedPlan(2, 2)

			requeueFailedEvents(p, stillTainted)

			Expect(p.Status.Events[0].State).To(Equal(intelv1a1.RecoveryEventStateFailed))
			Expect(p.Status.Messages).To(BeEmpty())
		})

		// maxRetries: 0 turns automatic retrying off entirely, which has to hold on the very first
		// failure rather than allowing one free attempt.
		It("should not retry at all when maxRetries is zero", func() {
			p := failedPlan(0, 0)

			requeueFailedEvents(p, stillTainted)

			Expect(p.Status.Events[0].State).To(Equal(intelv1a1.RecoveryEventStateFailed))
		})

		It("should leave a failed event whose taint has cleared for removeResolvedEvents", func() {
			p := failedPlan(3, 1)

			requeueFailedEvents(p, map[deviceKey]deviceNeed{})

			Expect(p.Status.Events[0].State).To(Equal(intelv1a1.RecoveryEventStateFailed))
		})

		It("should ignore events in any other state", func() {
			p := failedPlan(3, 0)
			p.Status.Events[0].State = intelv1a1.RecoveryEventStateInProgress

			requeueFailedEvents(p, stillTainted)

			Expect(p.Status.Events[0].State).To(Equal(intelv1a1.RecoveryEventStateInProgress))
		})
	})

	// An event with a Job in flight is the only record of that Job. Both phases that would
	// otherwise rewrite or drop it have to leave it alone until syncJobStatuses has resolved it.
	Context("In-progress events are not disturbed", func() {
		var r *GPURecoveryPlanReconciler

		BeforeEach(func() {
			r = newTestReconciler()
		})

		It("should defer an escalation while a Job is in flight", func() {
			p := &intelv1a1.GPURecoveryPlan{ObjectMeta: metav1.ObjectMeta{Name: "plan-esc-inflight"}}
			evt := &intelv1a1.RecoveryEvent{
				ID:           "evt-esc-inflight",
				NodeName:     "node01",
				GPUBDF:       "0000:02:00.0",
				RecoveryType: intelv1a1.RecoveryTypeSpec{Type: intelv1a1.RecoveryTypeSlot},
				State:        intelv1a1.RecoveryEventStateInProgress,
				JobName:      "recovery-evt-esc-inflight-0",
			}

			escalateEvent(p, evt, deviceNeed{rt: intelv1a1.RecoveryTypeReflash, reason: reasonSurvivability})

			// A new ID would orphan the Job named after the old one.
			Expect(evt.ID).To(Equal("evt-esc-inflight"))
			Expect(evt.RecoveryType.Type).To(Equal(intelv1a1.RecoveryTypeSlot))
			Expect(evt.State).To(Equal(intelv1a1.RecoveryEventStateInProgress))
			Expect(evt.JobName).To(Equal("recovery-evt-esc-inflight-0"))
		})

		It("should keep an in-progress event whose taint has cleared", func() {
			p := &intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{Name: "plan-resolved-inflight"},
				Status: intelv1a1.GPURecoveryPlanStatus{
					Events: []intelv1a1.RecoveryEvent{{
						ID: "evt-inflight", NodeName: "node01", GPUBDF: "0000:02:00.0",
						State:   intelv1a1.RecoveryEventStateInProgress,
						JobName: "recovery-evt-inflight-0",
					}},
				},
			}

			// The taint clearing mid-reset is the normal case: the reset worked.
			r.removeResolvedEvents(ctx, p, map[deviceKey]deviceNeed{})

			Expect(p.Status.Events).To(HaveLen(1),
				"dropping the event now would leave its Job collected by nothing")
			Expect(p.Status.Messages).To(BeEmpty())
		})
	})
})

// findJobContainer returns the named container from a Job's pod template, or nil.
func findJobContainer(job *batch.Job, name string) *core.Container {
	for i := range job.Spec.Template.Spec.Containers {
		if job.Spec.Template.Spec.Containers[i].Name == name {
			return &job.Spec.Template.Spec.Containers[i]
		}
	}

	return nil
}
