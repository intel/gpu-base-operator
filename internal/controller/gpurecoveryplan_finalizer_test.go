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
	"time"

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

// The finalizer and what it protects: a plan deletion must not kill a pod that is
// mid-reset, and the Jobs it left behind must not outlive the plan.
var _ = Describe("GPURecoveryPlan Controller: finalizer and deletion", func() {
	ctx := context.Background()

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
			job := makeTestJob(name, map[string]string{
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
			job := makeTestJob("recovery-terminating-0", map[string]string{
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
})
