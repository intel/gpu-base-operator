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
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	batch "k8s.io/api/batch/v1"
	core "k8s.io/api/core/v1"
	resv1 "k8s.io/api/resource/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	intelv1a1 "github.com/intel/gpu-base-operator/api/v1alpha1"
	"github.com/intel/gpu-base-operator/config/deployments"
)

type GPURecoveryPlanReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Opts   ControllerOpts
}

// +kubebuilder:rbac:groups=intel.com,resources=gpurecoveryplans,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=intel.com,resources=gpurecoveryplans/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=intel.com,resources=gpurecoveryplans/finalizers,verbs=update
// +kubebuilder:rbac:groups=resource.k8s.io,resources=resourceslices,verbs=get;list;watch

// Node labels decide whether a selector approval covers the node an event is on.
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch

// Reconcile is the main reconciliation loop for GPURecoveryPlan.
//
// The loop is triggered by a change to a GPURecoveryPlan (an admin adding an approval, or the
// operator's own status write), by a recovery Job the plan owns reaching a new state, or by a
// ResourceSlice event routed through resourceSliceToPlans.
//
// The phases run in a fixed order, each reading what the one before it wrote: detection mirrors
// the tainted GPUs into status.events, approvals turn the approved ones into recovery Jobs, and
// the Job sync reports what those Jobs did. status.state is derived once, at the end, from the
// event states all of them have settled on.
func (r *GPURecoveryPlanReconciler) Reconcile(ctx context.Context, req ctrl.Request) (retRes ctrl.Result, retErr error) {
	klog.V(2).Infof("Reconciling GPURecoveryPlan %s", req.Name)

	plan := &intelv1a1.GPURecoveryPlan{}

	if err := r.Get(ctx, req.NamespacedName, plan); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	orig := plan.DeepCopy()

	defer func() {
		if err := r.persistPlan(ctx, req.NamespacedName, orig, plan); err != nil {
			// Surface the write failure to the caller so the work is retried with backoff.
			// Silently swallowing it would leave the cluster's GPU state undetectable from the
			// CR, which is the only place it is reported.
			if retErr == nil {
				retErr = err
				retRes = ctrl.Result{}
			}
		}
	}()

	// Finalizer management.
	if done, err := r.handleFinalizer(ctx, plan); err != nil || done {
		// Deletion is blocked on an in-flight recovery Job: poll rather than fail. The status
		// update in the deferred block still runs, so the "waiting for N active Job(s)" message
		// reaches the CR before it disappears.
		if errors.Is(err, requeueReconcileErr{}) {
			return ctrl.Result{RequeueAfter: r.Opts.RequeueDelay}, nil
		}

		return ctrl.Result{}, err
	}

	// Reflect the current cluster GPU state into status.events.
	if err := r.syncRecoveryEventsFromSlices(ctx, plan); err != nil {
		return ctrl.Result{}, fmt.Errorf("syncRecoveryEventsFromSlices: %w", err)
	}

	// Start the recovery of every event an admin has approved.
	r.processApprovals(ctx, plan)

	// Update event states from the outcomes of the Jobs they are running.
	if err := r.syncJobStatuses(ctx, plan); err != nil {
		return ctrl.Result{}, fmt.Errorf("syncJobStatuses: %w", err)
	}

	// Drop consumed approvals no event refers to any more.
	pruneConsumedApprovals(plan)

	// Derive status.state from the resulting event states.
	updatePlanState(plan)

	// Requeue while a Job is in flight. A reconcile is also triggered by Job changes.
	if hasActiveJobs(plan) {
		return ctrl.Result{RequeueAfter: r.Opts.RequeueDelay}, nil
	}

	return ctrl.Result{}, nil
}

// persistPlan writes back whatever the reconcile phases changed on plan: status first, then spec.
// It is called from Reconcile's defer and returns the first error encountered, so the caller can
// fail the reconcile and get a retry with backoff.
//
// Write ordering: status must land BEFORE spec, because the spec write (consuming a one-shot
// approval) triggers an immediate new reconcile. If that reconcile saw the old status it would
// still read the event as waiting-approval and could act on it twice.
func (r *GPURecoveryPlanReconciler) persistPlan(ctx context.Context, key types.NamespacedName, orig, plan *intelv1a1.GPURecoveryPlan) error {
	statusChanged := !reflect.DeepEqual(orig.Status, plan.Status)
	specChanged := !reflect.DeepEqual(orig.Spec, plan.Spec)

	if !statusChanged && !specChanged {
		return nil
	}

	wantStatus := plan.Status.DeepCopy()
	wantSpec := plan.Spec.DeepCopy()

	var firstErr error

	if statusChanged {
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			if err := r.Get(ctx, key, plan); err != nil {
				return err
			}

			plan.Status = *wantStatus.DeepCopy()

			return r.Status().Update(ctx, plan)
		})
		if err != nil {
			klog.Errorf("GPURecoveryPlan %s: failed to update status: %v", plan.Name, err)

			firstErr = fmt.Errorf("updating status: %w", err)
		}
	}

	// Attempted even when the status write failed: an approval that has already produced a Job
	// must be marked consumed, or the next pass creates a second Job for the same GPU. The
	// reconcile still fails, so the lost status is rewritten on the retry.
	if specChanged {
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			if err := r.Get(ctx, key, plan); err != nil {
				return err
			}

			plan.Spec = *wantSpec.DeepCopy()

			return r.Update(ctx, plan)
		})
		if err != nil {
			klog.Errorf("GPURecoveryPlan %s: failed to update spec: %v", plan.Name, err)

			if firstErr == nil {
				firstErr = fmt.Errorf("updating spec: %w", err)
			}
		}
	}

	return firstErr
}

// handleFinalizer keeps the finalizer on a live plan and carries out the plan's own teardown when
// it is deleted. Returns done=true when the caller must stop reconciling: either the plan is on
// its way out, or the finalizer was just added and the resulting Update has already queued
// another pass.
func (r *GPURecoveryPlanReconciler) handleFinalizer(ctx context.Context, plan *intelv1a1.GPURecoveryPlan) (done bool, err error) {
	if !plan.DeletionTimestamp.IsZero() {
		// A recovery Job may be mid-flight through a PCIe reset. Letting the CR go now would
		// delete the Job's owner and, with it, a reset nobody is watching any more.
		running, err := runningRecoveryJobs(r.Client, ctx, r.Opts.Namespace, plan)
		if err != nil {
			return true, fmt.Errorf("listing recovery jobs during deletion: %w", err)
		}

		if len(running) > 0 {
			klog.Infof("GPURecoveryPlan %s: deletion blocked, waiting for %d active recovery Job(s): %s",
				plan.Name, len(running), strings.Join(running, ", "))
			appendMessage(plan, fmt.Sprintf("Deletion waiting for %d active recovery Job(s): %s",
				len(running), strings.Join(running, ", ")))

			// Requeue-not-an-error: Reconcile returns this with a nil error.
			return true, requeueReconcileErr{fmt.Errorf("waiting for %d active recovery job(s)", len(running))}
		}

		// Delete the Jobs explicitly rather than leaving them to the garbage collector, so their
		// pods are gone by the time the CR is.
		r.deleteAllJobs(ctx, plan)

		controllerutil.RemoveFinalizer(plan, recoveryPlanFinalizer)

		if err := r.Update(ctx, plan); err != nil {
			return true, fmt.Errorf("removing finalizer: %w", err)
		}

		return true, nil
	}

	if !controllerutil.ContainsFinalizer(plan, recoveryPlanFinalizer) {
		controllerutil.AddFinalizer(plan, recoveryPlanFinalizer)

		if err := r.Update(ctx, plan); err != nil {
			return true, fmt.Errorf("adding finalizer: %w", err)
		}

		// The Update triggers a new reconcile; nothing further to do in this cycle.
		return true, nil
	}

	return false, nil
}

// syncRecoveryEventsFromSlices scans all ResourceSlices for GPU devices that match this
// plan's spec.deviceId and carry recovery-related device taints, then reconciles
// status.events against what it found: failed events whose taint persists are re-queued for
// another attempt, events whose taint has cleared are removed, and newly tainted devices get an
// event (or have their existing one escalated).
func (r *GPURecoveryPlanReconciler) syncRecoveryEventsFromSlices(ctx context.Context, plan *intelv1a1.GPURecoveryPlan) error {
	sliceList := &resv1.ResourceSliceList{}

	if err := r.List(ctx, sliceList); err != nil {
		return fmt.Errorf("listing ResourceSlices: %w", err)
	}

	// activeKeys tracks (nodeName, bdf) pairs currently carrying a recovery taint, mapped to
	// the recovery they call for. Used below to remove events whose taint has cleared.
	activeKeys := make(map[deviceKey]deviceNeed)

	for i := range sliceList.Items {
		slice := &sliceList.Items[i]

		if slice.Spec.Driver != gpuDeviceClass {
			continue
		}

		// Ignore invalid slices.
		if slice.Spec.NodeName == nil {
			klog.Warningf("ResourceSlice %s has no spec.nodeName, skipping", slice.Name)

			continue
		}

		nodeName := *slice.Spec.NodeName

		for _, dev := range slice.Spec.Devices {
			devID := deviceAttributeString(dev.Attributes, deviceAttrDeviceID)
			if devID != plan.Spec.DeviceID {
				continue
			}

			bdf := deviceAttributeString(dev.Attributes, deviceAttrBDF)
			if bdf == "" {
				klog.Warningf("ResourceSlice %s device %s has no %s attribute, skipping",
					slice.Name, dev.Name, deviceAttrBDF)

				continue
			}

			key := deviceKey{node: nodeName, bdf: bdf}

			for _, taint := range dev.Taints {
				need, ok := taintToDeviceNeed(taint.Key, plan.Spec.DefaultResetType)
				if !ok {
					continue
				}

				curr := activeKeys[key]
				activeKeys[key] = higherPriorityNeed(curr, need)
			}
		}
	}

	// Send failed events round again while their taint persists and their retry budget lasts.
	requeueFailedEvents(plan, activeKeys)

	// Remove events whose taint has cleared. This runs before the add loop so that resolved
	// events free up room under maxStatusEvents in the same pass.
	r.removeResolvedEvents(ctx, plan, activeKeys)

	// Add new events for newly tainted devices.
	addNewEvents(plan, activeKeys)

	// status.state is deliberately NOT set here: Reconcile derives it once, after every phase
	// that can move an event has run.

	return nil
}

// processApprovals starts the recovery of every event an admin has authorised: one in
// waiting-approval that spec.approvals covers, or a permanently failed one an admin has named
// explicitly.
func (r *GPURecoveryPlanReconciler) processApprovals(ctx context.Context, plan *intelv1a1.GPURecoveryPlan) {
	// consumedIDs collects the one-shot approvals used this cycle. Marking them is deferred until
	// after the loop so that a single selector approval matches every event currently waiting —
	// three GPUs all needing an sbr, say — rather than being spent on the first one reached.
	consumedIDs := make(map[string]bool)

	for i := range plan.Status.Events {
		evt := &plan.Status.Events[i]

		// Re-approval path: an event that has spent its retry budget can be restarted by an admin
		// adding an approval that names it.
		if evt.State == intelv1a1.RecoveryEventStateFailed {
			approval, ok := r.findExplicitApprovalForEvent(plan, evt)
			if !ok {
				continue
			}

			now := setEventState(evt, intelv1a1.RecoveryEventStateWaitingApproval,
				"manually re-approved via approval %s after exhausting its retries; retry budget reset",
				approval.ID)
			evt.RetryCount = 0
			evt.ApprovalID = approval.ID
			evt.ApprovalMatchedAt = &now

			appendMessage(plan, fmt.Sprintf("Event %s manually re-approved via approval %s; retry budget reset",
				evt.ID, approval.ID))
			klog.Infof("GPURecoveryPlan %s: event %s re-approved via %s, retry budget reset",
				plan.Name, evt.ID, approval.ID)

			// State is waiting-approval now; fall through so the Job is created in this same cycle.
		}

		if evt.State != intelv1a1.RecoveryEventStateWaitingApproval {
			continue
		}

		approval, ok := r.findMatchingApproval(ctx, plan, evt)
		if !ok {
			klog.V(2).Infof("GPURecoveryPlan %s: no matching approval for event %s", plan.Name, evt.ID)

			continue
		}

		// Record which approval authorised this event, and when.
		if evt.ApprovalID != approval.ID {
			now := metav1.NewTime(time.Now())
			evt.ApprovalID = approval.ID
			evt.ApprovalMatchedAt = &now
			evt.LastUpdated = &now

			appendMessage(plan, fmt.Sprintf("Event %s matched approval %s", evt.ID, approval.ID))
		}

		// Apply any override before creating the Job.
		r.applyOverride(plan, evt, approval)

		if err := r.createRecoveryJob(ctx, plan, evt); err != nil {
			klog.Errorf("GPURecoveryPlan %s: failed to create job for event %s: %v", plan.Name, evt.ID, err)
			appendMessage(plan, fmt.Sprintf("Event %s: failed to create recovery job: %v", evt.ID, err))

			// State unchanged — the event keeps its approval and is retried on the next pass; only
			// the reason it has not started yet is recorded.
			setEventState(evt, evt.State, "the recovery Job could not be created: %v", err)

			continue
		}

		// Only consume a one-shot approval once the event has actually left waiting-approval.
		if !approval.Persistent && evt.State == intelv1a1.RecoveryEventStateInProgress {
			consumedIDs[approval.ID] = true
		}
	}

	for id := range consumedIDs {
		setApprovalConsumed(plan, id)
	}
}

// findMatchingApproval returns the first spec.approvals entry that authorises the given event.
// An approval matches when:
//   - it names the event through eventId (a single approval), or
//   - its selector matches the event's recovery type, node name and node labels (a group
//     approval). Every field set on the selector must match; unset fields mean "any".
func (r *GPURecoveryPlanReconciler) findMatchingApproval(ctx context.Context, plan *intelv1a1.GPURecoveryPlan,
	evt *intelv1a1.RecoveryEvent) (intelv1a1.RecoveryApproval, bool) {
	evtType := evt.RecoveryType.Type
	if evtType == "" {
		klog.Warningf("GPURecoveryPlan %s: event %s has no recovery type; skipping approval matching",
			plan.Name, evt.ID)

		return intelv1a1.RecoveryApproval{}, false
	}

	nodeCache := newNodeLabelCache(ctx, r)

	for _, a := range plan.Spec.Approvals {
		if a.Consumed {
			continue
		}

		// A single approval, naming one event.
		if a.EventID == evt.ID {
			return a, true
		}

		// A group approval, describing a set of events.
		if a.Selector != nil {
			if a.Selector.RecoveryType != "" && a.Selector.RecoveryType != evtType {
				continue
			}

			if a.Selector.NodeName != "" && a.Selector.NodeName != evt.NodeName {
				continue
			}

			if !nodeSelectorMatches(a.Selector.NodeSelector, evt.NodeName, nodeCache) {
				continue
			}

			return a, true
		}
	}

	return intelv1a1.RecoveryApproval{}, false
}

// findExplicitApprovalForEvent returns an unconsumed approval that names the event through eventId.
func (r *GPURecoveryPlanReconciler) findExplicitApprovalForEvent(plan *intelv1a1.GPURecoveryPlan, evt *intelv1a1.RecoveryEvent) (intelv1a1.RecoveryApproval, bool) {
	for _, a := range plan.Spec.Approvals {
		if a.EventID == evt.ID && !a.Consumed {
			return a, true
		}
	}

	return intelv1a1.RecoveryApproval{}, false
}

// applyOverride re-aims evt's reset type at approval.Override.RecoveryType, if set. The DRA driver
// cannot tell which reset mechanism a platform needs (see taintToDeviceNeed), so this is where an
// admin's choice of a different reset is honoured.
func (r *GPURecoveryPlanReconciler) applyOverride(plan *intelv1a1.GPURecoveryPlan, evt *intelv1a1.RecoveryEvent, approval intelv1a1.RecoveryApproval) {
	if approval.Override == nil {
		return
	}

	if evt.RecoveryType.IsReflash() {
		klog.Warningf("GPURecoveryPlan %s: approval %s specifies an override but event %s is not a reset-type event; ignoring",
			plan.Name, approval.ID, evt.ID)

		return
	}

	newType := approval.Override.RecoveryType
	if newType == intelv1a1.RecoveryTypeReflash {
		klog.Warningf("GPURecoveryPlan %s: approval %s cannot override reset event %s to reflash; ignoring",
			plan.Name, approval.ID, evt.ID)

		return
	}

	if evt.RecoveryType.Type == newType {
		return
	}

	if evt.RecoveryType.SuggestedType == "" {
		evt.RecoveryType.SuggestedType = evt.RecoveryType.Type
	}

	klog.Infof("GPURecoveryPlan %s: event %s reset type overridden %s -> %s via approval %s",
		plan.Name, evt.ID, evt.RecoveryType.Type, newType, approval.ID)
	appendMessage(plan, fmt.Sprintf("Event %s: reset type overridden from %s to %s via approval %s",
		evt.ID, evt.RecoveryType.SuggestedType, newType, approval.ID))

	evt.RecoveryType.Type = newType
}

// prepareRecoveryJob applies the naming, labelling, ownership, node-pinning and pull settings
// every recovery Job needs, and returns the Job name.
func (r *GPURecoveryPlanReconciler) prepareRecoveryJob(job *batch.Job, plan *intelv1a1.GPURecoveryPlan, evt *intelv1a1.RecoveryEvent) string {
	// The attempt index (how many Jobs this event has already run) goes in the name, so each retry
	// gets a name of its own and every attempt stays readable until the event is removed.
	jobName := recoveryJobName(evt.ID, len(evt.PastJobs))
	job.Name = jobName
	job.Namespace = r.Opts.Namespace

	if job.Labels == nil {
		job.Labels = make(map[string]string)
	}

	job.Labels[recoveryJobLabelPlan] = plan.Name
	job.Labels[recoveryJobLabelEvent] = evt.ID

	// Own the Job so that (a) its status changes wake this controller through the
	// Owns(&batch.Job{}) watch instead of waiting out a full RequeueDelay, and (b) any Job that
	// deleteAllJobs misses is garbage-collected with the plan rather than leaking.
	if err := ctrl.SetControllerReference(plan, job, r.Scheme); err != nil {
		warning := fmt.Sprintf("Event %s: failed to set controller reference on Job %s: %v", evt.ID, jobName, err)
		appendMessage(plan, warning)
		klog.Warning(warning)
	}

	// Pin the pod to the node hosting the affected GPU.
	job.Spec.Template.Spec.NodeName = evt.NodeName

	// Tolerate every taint.
	job.Spec.Template.Spec.Tolerations = append(
		[]core.Toleration{{Operator: core.TolerationOpExists}},
		plan.Spec.Tolerations...,
	)

	if plan.Spec.XpuSmi.PullPolicy != "" {
		job.Spec.Template.Spec.Containers[0].ImagePullPolicy = core.PullPolicy(plan.Spec.XpuSmi.PullPolicy)
	}

	if r.Opts.SecretName != "" {
		job.Spec.Template.Spec.ImagePullSecrets = []core.LocalObjectReference{{Name: r.Opts.SecretName}}
	}

	return jobName
}

// createRecoveryJob creates the Job that carries out the event's recovery and moves the event to
// in-progress.
func (r *GPURecoveryPlanReconciler) createRecoveryJob(ctx context.Context, plan *intelv1a1.GPURecoveryPlan, evt *intelv1a1.RecoveryEvent) error {
	if evt.RecoveryType.IsReflash() {
		// A reflash writes firmware over a card that is already in survivability mode rather than
		// resetting the PCIe bus, so it is a different Job built from different inputs — a firmware
		// image and a file within it — which this version does not assemble yet. The event keeps
		// its state and its approval, so it starts as soon as the operator can carry it out.
		setEventState(evt, evt.State, "a firmware reflash is not carried out by this version of the operator")

		klog.Warningf("GPURecoveryPlan %s: event %s calls for a firmware reflash, which is not implemented; leaving it in %s",
			plan.Name, evt.ID, evt.State)

		return nil
	}

	return r.createResetJob(ctx, plan, evt)
}

// createResetJob creates the PCIe-reset Job for a reset event and moves the event to in-progress.
func (r *GPURecoveryPlanReconciler) createResetJob(ctx context.Context, plan *intelv1a1.GPURecoveryPlan, evt *intelv1a1.RecoveryEvent) error {
	rt := evt.RecoveryType.Type

	args := recoveryTypeToArgs(evt.GPUBDF, rt)
	if args == nil {
		klog.Warningf("GPURecoveryPlan %s: unsupported recovery type %s for event %s; skipping", plan.Name, rt, evt.ID)

		return nil
	}

	job := deployments.XpuManagerResetJob()
	jobName := r.prepareRecoveryJob(job, plan, evt)

	// Inject the xpu-smi image and the reset command from the plan and the event.
	for i := range job.Spec.Template.Spec.Containers {
		if job.Spec.Template.Spec.Containers[i].Name == "resetter" {
			if plan.Spec.XpuSmi.Image != "" {
				job.Spec.Template.Spec.Containers[i].Image = plan.Spec.XpuSmi.Image
			}

			job.Spec.Template.Spec.Containers[i].Args = args

			break
		}
	}

	if err := r.Create(ctx, job); err != nil {
		if !k8serrors.IsAlreadyExists(err) {
			return fmt.Errorf("creating recovery Job %s: %w", jobName, err)
		}

		// The Job is already there: an earlier pass created it and lost its status write. Adopting
		// it is right — the name embeds the event ID and the attempt index, so this is the very
		// Job this attempt wanted.
		klog.V(2).Infof("GPURecoveryPlan %s: Job %s already exists", plan.Name, jobName)
	}

	evt.JobName = jobName

	setEventState(evt, intelv1a1.RecoveryEventStateInProgress, "")

	appendMessage(plan, fmt.Sprintf("Event %s: recovery Job %s created (type: %s, node: %s, bdf: %s)",
		evt.ID, jobName, rt, evt.NodeName, evt.GPUBDF))

	klog.Infof("GPURecoveryPlan %s: created recovery Job %s for event %s (type: %s, node: %s, bdf: %s)",
		plan.Name, jobName, evt.ID, rt, evt.NodeName, evt.GPUBDF)

	return nil
}

// syncJobStatuses polls the Job of every in-progress event and moves the event to succeeded or
// failed once the Job has finished.
func (r *GPURecoveryPlanReconciler) syncJobStatuses(ctx context.Context, plan *intelv1a1.GPURecoveryPlan) error { // nolint:unparam
	for i := range plan.Status.Events {
		evt := &plan.Status.Events[i]
		if evt.State != intelv1a1.RecoveryEventStateInProgress || evt.JobName == "" {
			continue
		}

		job := &batch.Job{}

		if err := r.Get(ctx, types.NamespacedName{Name: evt.JobName, Namespace: r.Opts.Namespace}, job); err != nil {
			klog.Warningf("GPURecoveryPlan %s: failed to get Job %s for event %s: %v",
				plan.Name, evt.JobName, evt.ID, err)

			continue
		}

		for _, cond := range job.Status.Conditions {
			if cond.Status != core.ConditionTrue {
				continue
			}

			switch cond.Type {
			case batch.JobComplete:
				klog.Infof("GPURecoveryPlan %s: Job %s succeeded for event %s", plan.Name, evt.JobName, evt.ID)
				appendMessage(plan, fmt.Sprintf("Event %s: recovery Job %s succeeded — pods retained until taint clears",
					evt.ID, evt.JobName))

				evt.PastJobs = append(evt.PastJobs, evt.JobName)
				evt.JobName = ""

				// No message: the state is the whole story, and the Job that produced it is the
				// last entry in pastJobs.
				setEventState(evt, intelv1a1.RecoveryEventStateSucceeded, "")

			case batch.JobFailed:
				klog.Warningf("GPURecoveryPlan %s: Job %s failed for event %s", plan.Name, evt.JobName, evt.ID)
				appendMessage(plan, fmt.Sprintf("Event %s: recovery Job %s failed (retries: %d) — pods retained until taint clears",
					evt.ID, evt.JobName, evt.RetryCount))

				failedJob := evt.JobName

				evt.PastJobs = append(evt.PastJobs, evt.JobName)
				evt.JobName = ""
				evt.RetryCount++

				// Record which attempt this was, since that says whether the operator will try
				// again, plus the Job's own verdict: BackoffLimitExceeded and DeadlineExceeded are
				// different problems, and the pod is gone once the event is removed.
				setEventState(evt, intelv1a1.RecoveryEventStateFailed,
					"recovery Job %s failed on attempt %d of %d: %s",
					failedJob, evt.RetryCount, plan.Spec.MaxRetries, jobFailureDetail(cond))
			}
		}
	}

	return nil
}

// deleteEventJobs deletes the event's current Job, if any, and every Job it has already run.
func (r *GPURecoveryPlanReconciler) deleteEventJobs(ctx context.Context, planName string, evt intelv1a1.RecoveryEvent) {
	if evt.JobName != "" {
		r.deleteJobByName(ctx, planName, evt.JobName)
	}

	for _, name := range evt.PastJobs {
		r.deleteJobByName(ctx, planName, name)
	}
}

// deleteAllJobs deletes every Job belonging to the plan.
func (r *GPURecoveryPlanReconciler) deleteAllJobs(ctx context.Context, plan *intelv1a1.GPURecoveryPlan) {
	jobList := &batch.JobList{}

	if err := r.List(ctx, jobList,
		client.InNamespace(r.Opts.Namespace),
		client.MatchingLabels{recoveryJobLabelPlan: plan.Name},
	); err != nil {
		klog.Warningf("GPURecoveryPlan %s: failed to list recovery Jobs for deletion: %v", plan.Name, err)
	}

	for i := range jobList.Items {
		r.deleteJobByName(ctx, plan.Name, jobList.Items[i].Name)
	}

	for _, evt := range plan.Status.Events {
		r.deleteEventJobs(ctx, plan.Name, evt)
	}
}

// deleteJobByName deletes a single Job by name, cascading to its pods through Background
// propagation.
func (r *GPURecoveryPlanReconciler) deleteJobByName(ctx context.Context, planName, jobName string) {
	job := &batch.Job{}

	if err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: r.Opts.Namespace}, job); err != nil {
		if !k8serrors.IsNotFound(err) {
			klog.Warningf("GPURecoveryPlan %s: failed to get Job %s for deletion: %v", planName, jobName, err)
		}

		return
	}

	bg := metav1.DeletePropagationBackground

	if err := r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &bg}); err != nil {
		if !k8serrors.IsNotFound(err) {
			klog.Warningf("GPURecoveryPlan %s: failed to delete Job %s: %v", planName, jobName, err)
		}

		return
	}

	klog.Infof("GPURecoveryPlan %s: deleted Job %s", planName, jobName)
}

// removeResolvedEvents removes events whose device taint has cleared, and deletes the Jobs they
// ran. A cleared taint means the GPU no longer needs recovering: either the recovery worked, or
// something else (a node reboot, an admin) healed it, and keeping the event would leave the plan
// asking for approval to reset a healthy card.
func (r *GPURecoveryPlanReconciler) removeResolvedEvents(ctx context.Context, plan *intelv1a1.GPURecoveryPlan, active map[deviceKey]deviceNeed) {
	kept := plan.Status.Events[:0]

	for _, evt := range plan.Status.Events {
		key := deviceKey{node: evt.NodeName, bdf: evt.GPUBDF}
		if _, stillActive := active[key]; stillActive {
			kept = append(kept, evt)

			continue
		}

		if evt.State == intelv1a1.RecoveryEventStateInProgress {
			klog.V(2).Infof("GPURecoveryPlan %s: taint cleared on %s/%s but event %s still has Job %s in flight; keeping it",
				plan.Name, evt.NodeName, evt.GPUBDF, evt.ID, evt.JobName)

			kept = append(kept, evt)

			continue
		}

		klog.Infof("GPURecoveryPlan %s: removing resolved event %s (taint cleared on %s/%s, state: %s)",
			plan.Name, evt.ID, evt.NodeName, evt.GPUBDF, evt.State)

		appendMessage(plan, fmt.Sprintf("Event %s cleared: taint resolved on %s/%s",
			evt.ID, evt.NodeName, evt.GPUBDF))

		// The Jobs were kept alive for as long as the event was, so their pods could be read for
		// diagnostics. This is where that ends.
		r.deleteEventJobs(ctx, plan.Name, evt)
	}

	plan.Status.Events = kept
}

// resourceSliceToPlans maps a ResourceSlice event to reconcile requests for all
// GPURecoveryPlan objects whose spec.deviceId matches at least one device in the slice.
func (r *GPURecoveryPlanReconciler) resourceSliceToPlans(ctx context.Context, obj client.Object) []reconcile.Request {
	slice, ok := obj.(*resv1.ResourceSlice)
	if !ok {
		return nil
	}

	// Collect all device IDs present in this slice.
	sliceDeviceIDs := make(map[string]struct{})

	for _, dev := range slice.Spec.Devices {
		if id := deviceAttributeString(dev.Attributes, deviceAttrDeviceID); id != "" {
			sliceDeviceIDs[id] = struct{}{}
		}
	}

	if len(sliceDeviceIDs) == 0 {
		return nil
	}

	planList := &intelv1a1.GPURecoveryPlanList{}

	if err := r.List(ctx, planList); err != nil {
		klog.Errorf("resourceSliceToPlans: failed to list GPURecoveryPlans: %v", err)

		return nil
	}

	var reqs []reconcile.Request

	for _, plan := range planList.Items {
		if _, match := sliceDeviceIDs[plan.Spec.DeviceID]; match {
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: plan.Name},
			})
		}
	}

	klog.V(2).Infof("resourceSliceToPlans: ResourceSlice %s maps to %d plan(s)", slice.Name, len(reqs))

	return reqs
}

// SetupWithManager sets up the controller with the Manager.
func (r *GPURecoveryPlanReconciler) SetupWithManager(mgr ctrl.Manager, opts ControllerOpts) error {
	r.Opts = opts

	return ctrl.NewControllerManagedBy(mgr).
		For(&intelv1a1.GPURecoveryPlan{}).
		Watches(
			&resv1.ResourceSlice{},
			handler.EnqueueRequestsFromMapFunc(r.resourceSliceToPlans),
		).
		// Recovery Jobs are owned by the plan, so a Job reaching Complete or Failed wakes this
		// controller immediately instead of waiting out the RequeueAfter poll. The owner reference
		// is cross-scope (cluster-scoped plan, namespaced Job), which is why the request the
		// handler produces carries only the plan's name.
		Owns(&batch.Job{}).
		Named("gpurecoveryplan").
		Complete(r)
}
