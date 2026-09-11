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
	"sigs.k8s.io/controller-runtime/pkg/client"

	intelv1a1 "github.com/intel/gpu-base-operator/api/v1alpha1"
)

// What the operator makes of a recovery Job once it is running: how it concludes,
// times out, or is left alone, and what that does to the event.
var _ = Describe("GPURecoveryPlan Controller: recovery Job outcomes", func() {
	ctx := context.Background()

	// Jobs are kept for as long as the event they belong to, so an admin looking at a GPU can still
	// read the pod that touched it. That is only true if the terminal-Job handling moves the name
	// into pastJobs rather than deleting the object.
	Context("Job outcomes: Jobs retained for diagnostics", func() {
		// putJob creates a Job and drives its status subresource to the given terminal condition.
		// K8s 1.36 requires startTime plus the interim condition before the terminal one.
		putJob := func(name, planName, evtID string, complete bool, reason string) {
			job := makeTestJob(name, map[string]string{
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
							// A just-started attempt. syncJobStatuses times an in-progress event
							// out against this, so leaving it unset would make the specs below
							// pass on a missing timestamp rather than on a recent one.
							LastUpdated: ptr.To(metav1.Now()),
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
			Expect(evt.PastJobs).To(ConsistOf("recovery-evt-fail-001-0"))
			Expect(evt.JobName).To(BeEmpty())

			// The Job outlives the event, but its pods do not outlive the plan's cleanup, so the
			// verdict is copied onto the event: which attempt it was, which Job ran it, and what
			// the Job controller made of it.
			Expect(evt.StateMessage).To(SatisfyAll(
				ContainSubstring("recovery-evt-fail-001-0"),
				ContainSubstring("attempt 1"),
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
			Expect(evt.PastJobs).To(ConsistOf("recovery-evt-ok-001-0"))
			Expect(evt.JobName).To(BeEmpty())

			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      "recovery-evt-ok-001-0",
				Namespace: "default",
			}, &batch.Job{})).To(Succeed(), "a succeeded Job must remain until the event is removed")
		})

		// A Job that has gone missing is not a failure of the recovery: failed is terminal, so
		// reporting one could park the event there while the reset it started is still running.
		It("should leave an event in-progress when its Job cannot be read", func() {
			r := newTestReconciler()
			p := planInProgress("plan-job-missing", "evt-missing-001", "recovery-evt-missing-001-0")

			Expect(r.syncJobStatuses(ctx, p)).To(Succeed())

			Expect(p.Status.Events[0].State).To(Equal(intelv1a1.RecoveryEventStateInProgress))
			Expect(p.Status.Events[0].PastJobs).To(BeEmpty(), "no attempt has concluded yet")
		})

		// ...but it cannot stay in-progress for ever either. in-progress is the state that holds the
		// node's recovery taint on and keeps the event out of every other phase, so an event whose
		// Job never reports back is a node the operator has cordoned and then forgotten about. Only
		// the Job controller writes the verdict, and it writes nothing at all if the Job was deleted
		// by hand, evicted with its namespace, or never admitted in the first place.
		It("should fail an event whose Job is gone and whose deadline has passed", func() {
			r := newTestReconciler()
			p := planInProgress("plan-job-lost", "evt-lost-001", "recovery-evt-lost-001-0")
			p.Status.Events[0].LastUpdated = ptr.To(metav1.NewTime(time.Now().Add(-20 * time.Minute)))

			Expect(r.syncJobStatuses(ctx, p)).To(Succeed())

			evt := &p.Status.Events[0]
			Expect(evt.State).To(Equal(intelv1a1.RecoveryEventStateFailed))
			Expect(evt.PastJobs).To(ConsistOf("recovery-evt-lost-001-0"),
				"the attempt is recorded, so a re-approved event's next Job gets a name of its own")
			Expect(evt.JobName).To(BeEmpty())
			Expect(evt.StateMessage).To(ContainSubstring("no longer exists"))
		})

		// The Job's own activeDeadlineSeconds is meant to end a hung recovery, and normally does. It
		// is not a guarantee: nothing enforces it if the Job controller is not running, and the pod
		// holding the GPU open outlives the event either way.
		It("should fail an event whose Job outlived its own deadline without concluding", func() {
			r := newTestReconciler()
			p := planInProgress("plan-job-hung", "evt-hung-001", "recovery-evt-hung-001-0")

			job := makeTestJob("recovery-evt-hung-001-0", map[string]string{
				recoveryJobLabelPlan:  p.Name,
				recoveryJobLabelEvent: "evt-hung-001",
			}, batch.JobComplete)
			job.Status = batch.JobStatus{}
			job.Spec.ActiveDeadlineSeconds = ptr.To(defaultResetJobTimeout)

			Expect(k8sClient.Create(ctx, job)).To(Succeed())

			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, job)
			})

			// Running, with no condition of any kind, since well before a reset's deadline.
			job.Status = batch.JobStatus{
				StartTime: ptr.To(metav1.NewTime(time.Now().Add(-20 * time.Minute))),
				Active:    1,
			}
			Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())

			Expect(r.syncJobStatuses(ctx, p)).To(Succeed())

			evt := &p.Status.Events[0]
			Expect(evt.State).To(Equal(intelv1a1.RecoveryEventStateFailed))
			Expect(evt.PastJobs).To(ConsistOf("recovery-evt-hung-001-0"))
			Expect(evt.StateMessage).To(ContainSubstring("no verdict"))

			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: "recovery-evt-hung-001-0", Namespace: "default",
			}, &batch.Job{})).To(Succeed(), "the Job is left for an admin to look at, as a failed one is")
		})

		It("should leave a Job that is merely still running alone", func() {
			r := newTestReconciler()
			p := planInProgress("plan-job-running", "evt-running-001", "recovery-evt-running-001-0")

			job := makeTestJob("recovery-evt-running-001-0", map[string]string{
				recoveryJobLabelPlan:  p.Name,
				recoveryJobLabelEvent: "evt-running-001",
			}, batch.JobComplete)
			job.Status = batch.JobStatus{}

			Expect(k8sClient.Create(ctx, job)).To(Succeed())

			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, job)
			})

			job.Status = batch.JobStatus{StartTime: ptr.To(metav1.Now()), Active: 1}
			Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())

			Expect(r.syncJobStatuses(ctx, p)).To(Succeed())

			Expect(p.Status.Events[0].State).To(Equal(intelv1a1.RecoveryEventStateInProgress))
			Expect(p.Status.Events[0].JobName).To(Equal("recovery-evt-running-001-0"))
			Expect(p.Status.Events[0].PastJobs).To(BeEmpty(), "the attempt is still in flight")
		})

		// The operator's clock has to sit outside the Job's, or it fires while a recovery that is
		// still within its deadline is running — a reflash interrupted part-written being the
		// outcome worth avoiding, which is also why the reflash deadline is the longer one.
		DescribeTable("should time an attempt out only past the Job's own deadline",
			func(rt intelv1a1.RecoveryType, age time.Duration, overdue bool) {
				p := planInProgress("plan-overdue", "evt-overdue", "job-overdue")
				evt := &p.Status.Events[0]
				evt.RecoveryType.Type = rt

				since := ptr.To(metav1.NewTime(time.Now().Add(-age)))
				Expect(jobVerdictOverdue(jobDeadlineSeconds(p, evt, nil), since)).To(Equal(overdue))
			},
			Entry("a reset inside its deadline", intelv1a1.RecoveryTypeSlot, 4*time.Minute, false),
			Entry("a reset inside the grace after it", intelv1a1.RecoveryTypeSlot, 5*time.Minute+30*time.Second, false),
			Entry("a reset past both", intelv1a1.RecoveryTypeSlot, 7*time.Minute, true),
			// The same age that is overdue for a reset is not for a reflash.
			Entry("a reflash at a reset's deadline", intelv1a1.RecoveryTypeReflash, 7*time.Minute, false),
			Entry("a reflash past its own", intelv1a1.RecoveryTypeReflash, 12*time.Minute, true),
		)

		// An event with no timestamp has no deadline to be past, and guessing one from time.Now()
		// would make the first reconcile that saw it start the clock at zero.
		It("should not time out an attempt with no timestamp to measure from", func() {
			p := planInProgress("plan-no-stamp", "evt-no-stamp", "job-no-stamp")
			Expect(jobVerdictOverdue(jobDeadlineSeconds(p, &p.Status.Events[0], nil), nil)).To(BeFalse())
		})

		// A Job runs under the activeDeadlineSeconds it was created with, and spec.timeouts is
		// editable while one is in flight, so the clock the operator keeps has to come off the Job
		// and not off the plan as it reads now.
		DescribeTable("should measure a Job against the deadline it was created with",
			func(job *batch.Job, want int64) {
				p := planInProgress("plan-deadline-src", "evt-deadline-src", "job-deadline-src")
				p.Spec.Timeouts.ResetSeconds = 60

				Expect(jobDeadlineSeconds(p, &p.Status.Events[0], job)).To(Equal(want))
			},
			Entry("the Job's own deadline, not the plan's",
				&batch.Job{Spec: batch.JobSpec{ActiveDeadlineSeconds: ptr.To(int64(1800))}}, int64(1800)),
			// A Job the operator created always has one; these cover a Job that has gone missing,
			// and one created by hand or by an operator old enough not to have set it.
			Entry("the plan's when there is no Job to read", nil, int64(60)),
			Entry("the plan's when the Job carries none", &batch.Job{}, int64(60)),
			Entry("the plan's when the Job carries a zero",
				&batch.Job{Spec: batch.JobSpec{ActiveDeadlineSeconds: ptr.To(int64(0))}}, int64(60)),
		)

		// The reason the deadline is read off the Job: nothing stops an admin from lowering
		// spec.timeouts while a recovery is running, and reading it back from the plan would fail an
		// event whose Job is still inside the deadline it was admitted with — for a reflash, with
		// the card part-written and the Job controller about to report success.
		It("should not fail a running Job that spec.timeouts was shortened under", func() {
			r := newTestReconciler()
			p := planInProgress("plan-timeout-cut", "evt-timeout-cut", "recovery-evt-timeout-cut-0")

			job := makeTestJob("recovery-evt-timeout-cut-0", map[string]string{
				recoveryJobLabelPlan:  p.Name,
				recoveryJobLabelEvent: "evt-timeout-cut",
			}, batch.JobComplete)
			job.Status = batch.JobStatus{}
			job.Spec.ActiveDeadlineSeconds = ptr.To(int64(3600))

			Expect(k8sClient.Create(ctx, job)).To(Succeed())

			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, job)
			})

			job.Status = batch.JobStatus{
				StartTime: ptr.To(metav1.NewTime(time.Now().Add(-20 * time.Minute))),
				Active:    1,
			}
			Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())

			// Cut to well under the Job's age after the Job was already admitted.
			p.Spec.Timeouts.ResetSeconds = 60

			Expect(r.syncJobStatuses(ctx, p)).To(Succeed())

			Expect(p.Status.Events[0].State).To(Equal(intelv1a1.RecoveryEventStateInProgress))
			Expect(p.Status.Events[0].PastJobs).To(BeEmpty(),
				"the Job is still inside the deadline it was created with")
		})

		It("should report an in-flight Job as an active Job", func() {
			Expect(hasActiveJobs(planInProgress("plan-active", "evt-a", "job-a"))).To(BeTrue())

			done := planInProgress("plan-done", "evt-b", "")
			done.Status.Events[0].State = intelv1a1.RecoveryEventStateSucceeded
			Expect(hasActiveJobs(done)).To(BeFalse())
		})
	})

	// A recovery Job that fails ends the event, and nothing in the operator restarts it. Retrying a
	// reset that did not bring the card back is unlikely to help and not free — it is another
	// privileged pod and another node drain on hardware that has already misbehaved — and the
	// failures that are not the hardware's (a renamed xpu-smi flag, a firmware file missing from the
	// image) are deterministic. Both want an admin, so failed is where the event waits for one.
	//
	// This is checked through Reconcile because the property is about the whole loop: the phase that
	// records the failure, the phases that could pick the event up again, and the approval matching
	// in between.
	Context("Reconcile: a failed recovery Job ends the event", func() {
		const (
			endPlan = "plan-failed-terminal"
			endNode = "node-failed-terminal"
			endBDF  = "0000:4b:00.0"
		)

		// failJob drives a Job to the state the Job controller leaves it in when its pod exits
		// non-zero: FailureTarget, then Failed, with the pod failure policy as the reason.
		failJob := func(name string) {
			job := &batch.Job{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, job)).To(Succeed())

			now := metav1.Now()
			job.Status = batch.JobStatus{
				StartTime: &now,
				Failed:    1,
				Conditions: []batch.JobCondition{
					{Type: batch.JobFailureTarget, Status: core.ConditionTrue, Reason: "PodFailurePolicy"},
					{
						Type: batch.JobFailed, Status: core.ConditionTrue, Reason: "PodFailurePolicy",
						Message: "Container updater for pod default/x failed with exit code 1 matching FailJob rule at index 0",
					},
				},
			}
			Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())
		}

		getPlan := func() *intelv1a1.GPURecoveryPlan {
			p := &intelv1a1.GPURecoveryPlan{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: endPlan}, p)).To(Succeed())

			return p
		}

		// reconcileUntilSettled runs the loop the way the manager does, where each status write
		// triggers another pass, so the specs assert on the state the loop comes to rest in rather
		// than on a chosen number of passes.
		reconcileUntilSettled := func() {
			for range 5 {
				_, err := reconcilePlan(ctx, endPlan)
				Expect(err).NotTo(HaveOccurred())
			}
		}

		// createPlan builds the plan with the given approvals and runs it up to a failed first
		// attempt, returning the event. A reflash, so the recovery goes straight from approval to
		// Job: the drain is a reset-only phase and would only add passes here.
		createPlan := func(approvals ...intelv1a1.RecoveryApproval) intelv1a1.RecoveryEvent {
			putTaintedSlice(ctx, "slice-failed-terminal", endNode, "0x1234", endBDF, deviceTaintKeyXpumdReflash)

			p := &intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{Name: endPlan, Finalizers: []string{recoveryPlanFinalizer}},
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DeviceID:         "0x1234",
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					XpuSmi:           intelv1a1.XpuSmiSpec{Image: "registry/xpu-smi:v1"},
					Firmware: &intelv1a1.FirmwareSpec{
						File: "gfx_fw.bin",
						Source: intelv1a1.FirmwareSource{
							ContainerSource: &intelv1a1.ContainerFirmwareSource{Name: "registry/fw:1"},
						},
					},
					Approvals: approvals,
				},
			}
			Expect(k8sClient.Create(ctx, p)).To(Succeed())

			// Both the plan name and the Job names are derived from the fixture, so they repeat
			// across the specs below: the cleanup has to wait for the objects to be gone, or the
			// next spec finds its first attempt already failed.
			DeferCleanup(func() {
				stale := &intelv1a1.GPURecoveryPlan{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: endPlan}, stale); err == nil {
					stale.Finalizers = nil
					Expect(k8sClient.Update(ctx, stale)).To(Succeed())
					Expect(k8sClient.Delete(ctx, stale)).To(Succeed())
				}

				jobs := &batch.JobList{}
				Expect(k8sClient.List(ctx, jobs, client.InNamespace("default"),
					client.MatchingLabels{recoveryJobLabelPlan: endPlan})).To(Succeed())

				// Background rather than the default: deleting a Job the default way has the
				// apiserver add an orphan finalizer for the garbage collector to clear, and
				// envtest runs no controller-manager, so the Job would sit there terminating.
				for i := range jobs.Items {
					Expect(k8sClient.Delete(ctx, &jobs.Items[i],
						client.PropagationPolicy(metav1.DeletePropagationBackground))).To(Succeed())
				}

				Eventually(func() int {
					remaining := &batch.JobList{}
					Expect(k8sClient.List(ctx, remaining, client.InNamespace("default"),
						client.MatchingLabels{recoveryJobLabelPlan: endPlan})).To(Succeed())

					return len(remaining.Items)
				}).Should(BeZero())

				Eventually(func() bool {
					return errors.IsNotFound(k8sClient.Get(ctx,
						types.NamespacedName{Name: endPlan}, &intelv1a1.GPURecoveryPlan{}))
				}).Should(BeTrue())
			})

			reconcileUntilSettled()

			evt := getPlan().Status.Events[0]
			Expect(evt.State).To(Equal(intelv1a1.RecoveryEventStateInProgress))
			Expect(evt.JobName).To(Equal(recoveryJobName(evt.ID, 0)))

			By("failing the first attempt the way a pod exiting 1 does")
			failJob(evt.JobName)
			reconcileUntilSettled()

			return evt
		}

		expectNoSecondJob := func(evtID string) {
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: recoveryJobName(evtID, 1), Namespace: "default",
			}, &batch.Job{})).To(Satisfy(errors.IsNotFound))
		}

		It("should leave the event failed and tell the admin the plan needs them", func() {
			first := createPlan(intelv1a1.RecoveryApproval{
				ID:       "app-one-shot",
				Selector: &intelv1a1.ApprovalSelector{RecoveryType: intelv1a1.RecoveryTypeReflash},
			})

			p := getPlan()
			evt := p.Status.Events[0]

			Expect(evt.State).To(Equal(intelv1a1.RecoveryEventStateFailed))
			Expect(evt.JobName).To(BeEmpty())
			Expect(evt.PastJobs).To(ConsistOf(first.JobName))
			Expect(evt.StateMessage).To(SatisfyAll(
				ContainSubstring("attempt 1 failed"),
				ContainSubstring("exit code 1"),
			))
			Expect(p.Status.State).To(Equal(intelv1a1.PlanStateError),
				"a failed recovery is the plan's own error; nothing here will move it")

			expectNoSecondJob(evt.ID)
		})

		// The device taint is still there, and a persistent approval goes on matching new events for
		// as long as it exists — but this event is not a new one. Auto-approving it again would be
		// the retry loop, arrived at from the other direction.
		It("should not restart the event under a persistent approval either", func() {
			createPlan(intelv1a1.RecoveryApproval{
				ID:         "app-persistent",
				Selector:   &intelv1a1.ApprovalSelector{RecoveryType: intelv1a1.RecoveryTypeReflash},
				Persistent: true,
			})

			evt := getPlan().Status.Events[0]
			Expect(evt.State).To(Equal(intelv1a1.RecoveryEventStateFailed))
			expectNoSecondJob(evt.ID)

			// Several more passes, since a persistent approval is never consumed: if anything were
			// going to pick the event up again, it would be here.
			reconcileUntilSettled()

			Expect(getPlan().Status.Events[0].State).To(Equal(intelv1a1.RecoveryEventStateFailed))
			expectNoSecondJob(evt.ID)
		})

		// The way back. An admin who has read the pod logs and wants another attempt says so about
		// this event specifically, and that is the only thing that starts one.
		It("should start another attempt once an admin names the event", func() {
			createPlan(intelv1a1.RecoveryApproval{
				ID:       "app-one-shot",
				Selector: &intelv1a1.ApprovalSelector{RecoveryType: intelv1a1.RecoveryTypeReflash},
			})

			evtID := getPlan().Status.Events[0].ID

			By("adding an approval naming the failed event")

			p := getPlan()
			p.Spec.Approvals = append(p.Spec.Approvals, intelv1a1.RecoveryApproval{
				ID: "app-second-look", EventID: evtID,
			})
			Expect(k8sClient.Update(ctx, p)).To(Succeed())

			reconcileUntilSettled()

			evt := getPlan().Status.Events[0]
			Expect(evt.State).To(Equal(intelv1a1.RecoveryEventStateInProgress))
			Expect(evt.JobName).To(Equal(recoveryJobName(evtID, 1)),
				"the second attempt gets a Job of its own, named after its attempt index")
			Expect(evt.ApprovalID).To(Equal("app-second-look"))
			Expect(evt.PastJobs).To(HaveLen(1), "the first attempt's Job stays listed for diagnostics")

			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: evt.JobName, Namespace: "default",
			}, &batch.Job{})).To(Succeed())
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
