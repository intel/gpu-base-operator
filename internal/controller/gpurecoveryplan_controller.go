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
	"slices"
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
	"k8s.io/utils/ptr"
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

	// imgVerify is the pre-flight registry check run before a recovery Job is created. An
	// interface so tests can answer for a registry they do not have.
	imgVerify ContentImageVerifier
}

// +kubebuilder:rbac:groups=intel.com,resources=gpurecoveryplans,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=intel.com,resources=gpurecoveryplans/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=intel.com,resources=gpurecoveryplans/finalizers,verbs=update
// +kubebuilder:rbac:groups=resource.k8s.io,resources=resourceslices,verbs=get;list;watch

// Node labels decide whether a selector approval covers the node an event is on, and the drain
// taints the node it is clearing (update, because a taint is written through node.spec).
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;update

// The drain reads the pods on a node and the claims that still reserve the GPU being reset.
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=resource.k8s.io,resources=resourceclaims,verbs=get;list;watch

// On OpenShift the recovery Jobs run under an SCC of their own; the operator has to be able to
// create it and to grant its use.
// +kubebuilder:rbac:groups=security.openshift.io,resources=securitycontextconstraints,verbs=create;delete;get;list;watch;use;update

// Reconcile is the main reconciliation loop for GPURecoveryPlan.
//
// The loop is triggered by a change to a GPURecoveryPlan (an admin adding an approval, or the
// operator's own status write), by a recovery Job the plan owns reaching a new state, or by a
// ResourceSlice event routed through resourceSliceToPlans.
//
// The phases run in a fixed order, each reading what the one before it wrote: detection mirrors
// the tainted GPUs into status.events, approvals send the approved ones into a node drain, the
// drain phase creates the recovery Job for every node that has come clear, and the Job sync
// reports what those Jobs did. status.state is derived once, at the end, from the event states all
// of them have settled on.
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

	// Apply SCCs if in OpenShift
	if r.Opts.OpenShift {
		if err := r.ensureOpenShiftResources(ctx, plan.Name); err != nil {
			appendMessage(plan, fmt.Sprintf("Failed to ensure OpenShift SCC resources: %v", err))

			return ctrl.Result{}, fmt.Errorf("ensureOpenShiftResources: %w", err)
		}
	}

	// Move every event an admin has approved forward: into a node drain for a reset, or straight
	// into a recovery Job for anything that does not need the node emptied.
	r.processApprovals(ctx, plan)

	// Advance the events that are draining, creating the recovery Job for each node that is clear.
	if err := r.processDrains(ctx, plan); err != nil {
		return ctrl.Result{}, fmt.Errorf("processDrains: %w", err)
	}

	// Update event states from the outcomes of the Jobs they are running.
	if err := r.syncJobStatuses(ctx, plan); err != nil {
		return ctrl.Result{}, fmt.Errorf("syncJobStatuses: %w", err)
	}

	// Drop consumed approvals no event refers to any more.
	pruneConsumedApprovals(plan)

	// Lift the drain taint from every node that no longer needs to be held. Runs after the phases
	// that decide what happens to each event, so it sees their final states.
	r.reconcileDrainTaints(ctx, plan)

	// Derive status.state from the resulting event states.
	updatePlanState(plan)

	// Requeue while a drain or a Job is in flight. A reconcile is also triggered by Job changes.
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

		// No event survives the CR, so no node may be left unschedulable on its behalf. This is
		// the last chance to do it: once the finalizer is gone nothing reconciles the plan again,
		// and the taint carries the plan's own name, which nothing else knows to look for.
		r.releaseAllDrainTaints(ctx, plan)

		// Remove any SCCs as the plan is going away.
		if r.Opts.OpenShift {
			r.cleanupOpenShiftResources(ctx, plan.Name)
		}

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

			// The attribute is a free-form string the DRA driver writes, and it reaches the shell
			// command line of a privileged root container. A device the operator cannot name a
			// PCI address for is one it cannot recover either way, so the shape is required here
			// rather than escaped later.
			if !validDeviceBDF(bdf) {
				klog.Warningf("ResourceSlice %s device %s has %s %q, which is not a PCI address, skipping",
					slice.Name, dev.Name, deviceAttrBDF, bdf)

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
	// blockedIDs contains the approvals that have an event that cannot start yet
	blockedIDs := make(map[string]bool)

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

		// blocked is admitted alongside waiting-approval
		if evt.State != intelv1a1.RecoveryEventStateWaitingApproval &&
			evt.State != intelv1a1.RecoveryEventStateBlocked &&
			evt.State != intelv1a1.RecoveryEventStateMissingFirmware {
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

		// Apply any override before the recovery type decides whether the node has to be drained.
		r.applyOverride(plan, evt, approval)

		// Prevent multiple events from running on the same node at once.
		if blocker, busy := nodeBusyWith(plan, evt); busy {
			blockEvent(plan, evt, blocker)

			blockedIDs[approval.ID] = true

			continue
		}

		// Every image the Job is about to pull has to resolve in its registry first.
		if !ensureImagesUsable(r.imgVerify, r.Opts.SecretName, ctx, plan, evt) {
			continue
		}

		// A reset needs the node emptied first, which spans several reconciles; its Job is created
		// by processDrains once the node is clear. Anything that resets nothing goes straight to
		// the Job.
		if needsDrain(plan, evt) {
			beginDrain(plan, evt)
		} else if err := r.createRecoveryJob(ctx, plan, evt); err != nil {
			// State unchanged — the event keeps its approval and is retried on the next pass; only
			// the reason it has not started yet is recorded.
			setEventState(evt, evt.State, "the recovery Job could not be created: %v", err)

			klog.Errorf("GPURecoveryPlan %s: failed to create job for event %s: %v", plan.Name, evt.ID, err)
			appendMessage(plan, fmt.Sprintf("Event %s: failed to create recovery job: %v", evt.ID, err))

			continue
		}

		// Only consume a one-shot approval once the event has actually left waiting-approval —
		// into draining, or straight to in-progress. An event parked in missing-firmware never
		// started, so its approval stays and fires again once spec.firmware is filled in.
		if !approval.Persistent &&
			(evt.State == intelv1a1.RecoveryEventStateInProgress ||
				evt.State == intelv1a1.RecoveryEventStateDraining) {
			consumedIDs[approval.ID] = true
		}
	}

	for id := range consumedIDs {
		if blockedIDs[id] {
			klog.V(2).Infof("GPURecoveryPlan %s: approval %s not consumed yet; it still has a blocked event",
				plan.Name, id)

			continue
		}

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

// processDrains advances every event sitting in the draining state: it taints the node, evicts what
// is on it, and creates the recovery Job once the node is clear and the GPU is released.
func (r *GPURecoveryPlanReconciler) processDrains(ctx context.Context, plan *intelv1a1.GPURecoveryPlan) error { // nolint:unparam
	for i := range plan.Status.Events {
		evt := &plan.Status.Events[i]

		if evt.State != intelv1a1.RecoveryEventStateDraining {
			continue
		}

		// stall indicates any drain issue. Empty means the pass moved the event on.
		stall := ""

		ready, err := r.drainNodeForEvent(ctx, plan, evt)

		switch {
		case err != nil:
			stall = fmt.Sprintf("the drain of node %s is not progressing: %v", evt.NodeName, err)

			// The event stays in draining and the deadline keeps running; the message is what
			// distinguishes a drain that is waiting from one that cannot proceed at all.
			setEventState(evt, intelv1a1.RecoveryEventStateDraining, "%s", stall)

			// Reported and carried on to the next event: one unreachable node must not stop the
			// other GPUs in the cluster from being recovered.
			klog.Errorf("GPURecoveryPlan %s: event %s drain of node %s failed: %v",
				plan.Name, evt.ID, evt.NodeName, err)
			appendMessage(plan, fmt.Sprintf("Event %s: drain of node %s failed: %v",
				evt.ID, evt.NodeName, err))

		case !ready:
			// Waiting on the pods and claims drainNodeForEvent has just recorded on the event.
			stall = drainBlockerDetail(evt.PodsBlockingDrain, evt.ClaimsBlockingReset)

		default:
			if err := r.createRecoveryJob(ctx, plan, evt); err != nil {
				stall = fmt.Sprintf("node %s is drained but the recovery Job could not be created: %v",
					evt.NodeName, err)

				// Stays in draining: the node is already empty and tainted, so the next pass finds
				// it clear again and only has to retry the Job — until the deadline below gives up,
				// which it must, because whatever is refusing the Job may never come back on a node
				// this drain has emptied.
				setEventState(evt, intelv1a1.RecoveryEventStateDraining, "%s", stall)

				klog.Errorf("GPURecoveryPlan %s: failed to create job for event %s: %v",
					plan.Name, evt.ID, err)
				appendMessage(plan, fmt.Sprintf("Event %s: failed to create recovery job: %v", evt.ID, err))
			}
		}

		if stall != "" && drainDeadlineExceeded(plan, evt) {
			failDrain(plan, evt, stall)
		}
	}

	return nil
}

// drainNodeForEvent performs one pass of the drain for a single event and reports whether the reset
// may now proceed.
func (r *GPURecoveryPlanReconciler) drainNodeForEvent(ctx context.Context, plan *intelv1a1.GPURecoveryPlan,
	evt *intelv1a1.RecoveryEvent) (bool, error) {
	if _, err := ensureNodeTaint(ctx, r.Client, evt.NodeName, recoveryTaint(plan.Name)); err != nil {
		return false, err
	}

	toEvict, toAwait, err := podsBlockingDrain(
		ctx, r.Client, evt.NodeName, r.Opts.Namespace, plan.Spec.Drain.NamespacesToSkip)
	if err != nil {
		return false, err
	}

	if err := evictPods(ctx, r.Client, toEvict); err != nil {
		// Not fatal to the drain: a pod that could not be evicted is still in the blocking list
		// below, the next pass asks again, and the drain deadline is the backstop.
		klog.Warningf("GPURecoveryPlan %s: event %s could not evict every pod on node %s: %v",
			plan.Name, evt.ID, evt.NodeName, err)
	}

	blocking := make([]string, 0, len(toEvict)+len(toAwait))

	for _, pod := range append(toEvict, toAwait...) {
		blocking = append(blocking, pod.Namespace+"/"+pod.Name)
	}

	// Sorted so a status write only happens when the set of blockers actually changes; List order
	// would otherwise reshuffle the reported list and churn the CR on every poll.
	slices.Sort(blocking)

	claims, err := r.claimsHoldingDevice(ctx, plan, evt)
	if err != nil {
		return false, err
	}

	evt.PodsBlockingDrain = capStrings(blocking, maxPodsBlockingDrainReported)
	evt.ClaimsBlockingReset = capStrings(claims, maxPodsBlockingDrainReported)

	// This pass got all the way through, so clear any message a previous one left about a drain.
	setEventState(evt, intelv1a1.RecoveryEventStateDraining, "")

	if len(blocking) == 0 && len(claims) == 0 {
		klog.Infof("GPURecoveryPlan %s: event %s node %s drained; proceeding with %s",
			plan.Name, evt.ID, evt.NodeName, evt.RecoveryType.Type)

		return true, nil
	}

	klog.V(2).Infof("GPURecoveryPlan %s: event %s waiting on %d pod(s) and %d claim(s) on node %s",
		plan.Name, evt.ID, len(blocking), len(claims), evt.NodeName)

	return false, nil
}

// reconcileDrainTaints removes this plan's drain taint from every node that no longer needs it.
func (r *GPURecoveryPlanReconciler) reconcileDrainTaints(ctx context.Context, plan *intelv1a1.GPURecoveryPlan) {
	wanted := make(map[string]struct{})

	for i := range plan.Status.Events {
		switch plan.Status.Events[i].State {
		case intelv1a1.RecoveryEventStateDraining, intelv1a1.RecoveryEventStateInProgress:
			// Included without re-checking needsDrain. A recovery that never drained has no taint
			// on its node from this plan, so listing it costs nothing, whereas re-deriving the
			// answer would let a mid-flight flip of spec.drain.enable untaint a node while its
			// reset is still running.
			wanted[plan.Status.Events[i].NodeName] = struct{}{}

		case intelv1a1.RecoveryEventStateBlocked:
			// A reset queued behind another recovery keeps the node tainted
			if needsDrain(plan, &plan.Status.Events[i]) {
				wanted[plan.Status.Events[i].NodeName] = struct{}{}
			}
		}

		// An event held back by a failed image check is deliberately absent, unlike a blocked one. A
		// block clears by itself, usually within minutes, so holding the node is cheap. An unpullable
		// image waits on a person and may wait indefinitely, and a NoSchedule taint parked on a working
		// node for the lifetime of a config typo takes real capacity out of the cluster. Such an event
		// reads as plain waiting-approval here: no taint until an approval actually starts a recovery.
	}

	r.untaintNodesExcept(ctx, plan, wanted)
}

// releaseAllDrainTaints removes this plan's drain taint from every node. Called on plan deletion.
func (r *GPURecoveryPlanReconciler) releaseAllDrainTaints(ctx context.Context, plan *intelv1a1.GPURecoveryPlan) {
	r.untaintNodesExcept(ctx, plan, nil)
}

// untaintNodesExcept drops this plan's drain taint from every tainted node not named in keep.
func (r *GPURecoveryPlanReconciler) untaintNodesExcept(ctx context.Context, plan *intelv1a1.GPURecoveryPlan,
	keep map[string]struct{}) {
	taint := recoveryTaint(plan.Name)

	tainted, err := nodesWithTaint(ctx, r.Client, taint)
	if err != nil {
		klog.Errorf("GPURecoveryPlan %s: failed to list nodes carrying the drain taint: %v", plan.Name, err)

		return
	}

	for _, nodeName := range tainted {
		if _, keepIt := keep[nodeName]; keepIt {
			continue
		}

		if _, err := removeNodeTaint(ctx, r.Client, nodeName, taint); err != nil {
			klog.Errorf("GPURecoveryPlan %s: failed to remove drain taint from node %s: %v", plan.Name, nodeName, err)
			appendMessage(plan, fmt.Sprintf("Failed to remove drain taint from node %s: %v", nodeName, err))

			continue
		}

		klog.Infof("GPURecoveryPlan %s: removed drain taint from node %s", plan.Name, nodeName)
	}
}

// claimsHoldingDevice returns the ResourceClaims that still reserve the GPU this event targets, as
// "namespace/name" strings.
func (r *GPURecoveryPlanReconciler) claimsHoldingDevice(ctx context.Context, plan *intelv1a1.GPURecoveryPlan,
	evt *intelv1a1.RecoveryEvent) ([]string, error) {
	devices, err := r.devicesForBDF(ctx, evt.NodeName, evt.GPUBDF)
	if err != nil {
		return nil, err
	}

	if len(devices) == 0 {
		return nil, nil
	}

	claimList := &resv1.ResourceClaimList{}

	if err := r.List(ctx, claimList); err != nil {
		return nil, fmt.Errorf("listing ResourceClaims: %w", err)
	}

	holding := make([]string, 0, len(claimList.Items))

	for i := range claimList.Items {
		claim := &claimList.Items[i]

		// An allocated claim nobody has reserved holds no device: the scheduler allocated it and
		// then the pod went away. Only reservedFor proves a live consumer.
		if claim.Status.Allocation == nil || len(claim.Status.ReservedFor) == 0 {
			continue
		}

		if !claimHoldsDevice(claim, devices) {
			continue
		}

		undrainable, holder, err := r.claimHeldOnlyByUndrainablePods(ctx, plan, claim)
		if err != nil {
			return nil, err
		}

		if undrainable {
			klog.V(2).Infof("GPURecoveryPlan %s: event %s not waiting for claim %s/%s: %s",
				plan.Name, evt.ID, claim.Namespace, claim.Name, holder)

			continue
		}

		holding = append(holding, claim.Namespace+"/"+claim.Name)
	}

	slices.Sort(holding)

	return holding, nil
}

// claimHeldOnlyByUndrainablePods reports whether every consumer reserving a claim is a pod the
// drain will never evict.
func (r *GPURecoveryPlanReconciler) claimHeldOnlyByUndrainablePods(ctx context.Context,
	plan *intelv1a1.GPURecoveryPlan, claim *resv1.ResourceClaim) (bool, string, error) {
	holder := ""

	for _, consumer := range claim.Status.ReservedFor {
		if consumer.APIGroup != "" || consumer.Resource != "pods" {
			return false, "", nil
		}

		// A ResourceClaim is only usable by pods in its own namespace, so the consumer name is
		// resolved there.
		pod := &core.Pod{}

		if err := r.Get(ctx, types.NamespacedName{Namespace: claim.Namespace, Name: consumer.Name}, pod); err != nil {
			if k8serrors.IsNotFound(err) {
				return false, "", nil
			}

			return false, "", fmt.Errorf("failed to get pod %s/%s reserving claim %s: %w",
				claim.Namespace, consumer.Name, claim.Name, err)
		}

		// The UID is what makes it the same pod: a StatefulSet replacement reuses the name, and
		// classifying the reservation by the new pod's owner references would answer a question
		// about a pod that no longer exists.
		if pod.UID != consumer.UID {
			return false, "", nil
		}

		reason, never := drainNeverEvicts(pod, r.Opts.Namespace, plan.Spec.Drain.NamespacesToSkip)
		if !never {
			return false, "", nil
		}

		if holder == "" {
			holder = fmt.Sprintf("reserved by pod %s/%s, which the drain leaves in place (%s)",
				pod.Namespace, pod.Name, reason)
		}
	}

	return holder != "", holder, nil
}

// devicesForBDF returns the DRA pool/device identities on a node whose published pciAddress matches
// bdf. Normally one, but a device can appear in more than one slice of a pool.
func (r *GPURecoveryPlanReconciler) devicesForBDF(ctx context.Context,
	nodeName, bdf string) (map[poolDevice]struct{}, error) {
	sliceList := &resv1.ResourceSliceList{}

	if err := r.List(ctx, sliceList); err != nil {
		return nil, fmt.Errorf("listing ResourceSlices: %w", err)
	}

	devices := make(map[poolDevice]struct{})

	for i := range sliceList.Items {
		slice := &sliceList.Items[i]

		if slice.Spec.NodeName == nil || *slice.Spec.NodeName != nodeName {
			continue
		}

		for _, dev := range slice.Spec.Devices {
			if deviceAttributeString(dev.Attributes, deviceAttrBDF) != bdf {
				continue
			}

			devices[poolDevice{pool: slice.Spec.Pool.Name, device: dev.Name}] = struct{}{}
		}
	}

	return devices, nil
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

	// Bound how long the recovery may run. Without a deadline a Job whose pod never gets anywhere —
	// an xpu-smi that hangs on a card that has stopped answering at all — holds the node's drain
	// taint and the event's in-progress state indefinitely, and nothing else in the reconcile is
	// watching a clock once the Job exists. Overwrites the template's own value, which is the same
	// number as the CRD default and is here for objects that bypassed defaulting.
	job.Spec.ActiveDeadlineSeconds = ptr.To(recoveryJobTimeout(plan, evt))

	// Pin the pod to the node hosting the affected GPU.
	job.Spec.Template.Spec.NodeName = evt.NodeName

	// Tolerate every taint.
	job.Spec.Template.Spec.Tolerations = append(
		[]core.Toleration{{Operator: core.TolerationOpExists}},
		plan.Spec.Tolerations...,
	)

	if r.Opts.SecretName != "" {
		job.Spec.Template.Spec.ImagePullSecrets = []core.LocalObjectReference{{Name: r.Opts.SecretName}}
	}

	// On OpenShift the pod has to run under the ServiceAccount bound to the recovery SCC
	if r.Opts.OpenShift {
		_, _, _, saName := buildOpenShiftNames(plan.Name, recoveryResourcePart)
		job.Spec.Template.Spec.ServiceAccountName = saName
	}

	return jobName
}

// ensureOpenShiftResources creates the SCC, ClusterRole, ClusterRoleBinding and ServiceAccount that
// let recovery Job pods run privileged on OpenShift.
func (r *GPURecoveryPlanReconciler) ensureOpenShiftResources(ctx context.Context, planName string) error {
	sccName, roleName, bindingName, saName := buildOpenShiftNames(planName, recoveryResourcePart)

	if err := createServiceAccount(ctx, r.Client, saName, r.Opts.Namespace); err != nil {
		return fmt.Errorf("failed to ensure recovery ServiceAccount: %w", err)
	}

	if err := ensureSCC(ctx, r.Client, buildRecoverySCC(sccName)); err != nil {
		return fmt.Errorf("failed to ensure recovery SCC: %w", err)
	}

	if err := createSCCRole(ctx, r.Client, roleName, sccName); err != nil {
		return fmt.Errorf("failed to ensure recovery SCC ClusterRole: %w", err)
	}

	if err := createSCCRoleBinding(ctx, r.Client, bindingName, roleName, saName, r.Opts.Namespace); err != nil {
		return fmt.Errorf("failed to ensure recovery SCC ClusterRoleBinding: %w", err)
	}

	return nil
}

// cleanupOpenShiftResources removes the SCC quadruple when the plan is deleted. The objects are
// cluster-scoped — or, for the ServiceAccount, in the operator namespace — and not owned by the
// plan, so they are not garbage-collected with it.
func (r *GPURecoveryPlanReconciler) cleanupOpenShiftResources(ctx context.Context, planName string) {
	sccName, roleName, bindingName, saName := buildOpenShiftNames(planName, recoveryResourcePart)
	deleteOpenShiftSCCResources(ctx, r.Client, sccName, roleName, bindingName, saName, r.Opts.Namespace)
}

// createRecoveryJob creates the Job that carries out the event's recovery and moves the event to
// in-progress.
func (r *GPURecoveryPlanReconciler) createRecoveryJob(ctx context.Context, plan *intelv1a1.GPURecoveryPlan, evt *intelv1a1.RecoveryEvent) error {
	if evt.RecoveryType.IsReflash() {
		return r.createReflashJob(ctx, plan, evt)
	}

	return r.createResetJob(ctx, plan, evt)
}

// createResetJob creates the PCIe-reset Job for a reset event and moves the event to in-progress.
func (r *GPURecoveryPlanReconciler) createResetJob(ctx context.Context, plan *intelv1a1.GPURecoveryPlan, evt *intelv1a1.RecoveryEvent) error {
	rt := evt.RecoveryType.Type

	cmd := buildResetCommand(evt.GPUBDF, rt)
	if cmd == "" {
		klog.Warningf("GPURecoveryPlan %s: unsupported recovery type %s for event %s; skipping", plan.Name, rt, evt.ID)

		return nil
	}

	job := deployments.XpuManagerResetJob()
	jobName := r.prepareRecoveryJob(job, plan, evt)

	// Inject the xpu-smi image and the reset command from the plan and the event. The template's
	// command is /bin/sh -c, as the reflash template's is, so the reset is one argument: a command
	// line, not an argv. Going through a shell is what lets xpu-smi be found on PATH, rather than
	// the operator having to know where the image the plan names keeps its binary.
	if c := containerByName(job.Spec.Template.Spec.Containers, resetJobContainer); c != nil {
		applyXpuSmiImage(c, plan)
		c.Args = []string{cmd}
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

// createReflashJob creates the firmware-reflash Job for a reflash event and moves the event to
// in-progress.
func (r *GPURecoveryPlanReconciler) createReflashJob(ctx context.Context, plan *intelv1a1.GPURecoveryPlan, evt *intelv1a1.RecoveryEvent) error {
	fw := plan.Spec.Firmware
	if fw == nil || fw.File == "" {
		// The state names the problem; the message names which part of spec.firmware is missing.
		r.parkForFirmware(plan, evt, "spec.firmware is not configured, so a reflash cannot be attempted",
			"spec.firmware not configured")

		return nil
	}

	if fw.Source.ContainerSource == nil {
		r.parkForFirmware(plan, evt,
			"spec.firmware.source.containerSource is not set; a reflash copies the firmware from a "+
				"container image, and volumeSource is not supported yet",
			"spec.firmware.source.containerSource not configured")

		return nil
	}

	job := deployments.XpuManagerFWUpdateJob()
	jobName := r.prepareRecoveryJob(job, plan, evt)

	// The firmware comes out of its own image, which the initContainer copies into the shared
	// emptyDir the updater then flashes from.
	if c := containerByName(job.Spec.Template.Spec.InitContainers, reflashCopyContainer); c != nil {
		c.Image = fw.Source.ContainerSource.Name
	}

	if c := containerByName(job.Spec.Template.Spec.Containers, reflashJobContainer); c != nil {
		applyXpuSmiImage(c, plan)

		// The template's command is /bin/sh -c, so the flash is one argument: a command line, not an
		// argv. Overwriting args rather than command keeps the shell, which the template needs
		// anyway, and leaves xpu-smi to be found on PATH inside whichever image the plan names.
		c.Args = []string{buildFDOFlashCommand(evt.GPUBDF, fw.File)}
	}

	if err := r.Create(ctx, job); err != nil {
		if !k8serrors.IsAlreadyExists(err) {
			return fmt.Errorf("creating reflash Job %s: %w", jobName, err)
		}

		// Adopted for the same reason createResetJob adopts: the name embeds the event ID and the
		// attempt index, so this is the very Job this attempt wanted.
		klog.V(2).Infof("GPURecoveryPlan %s: reflash Job %s already exists", plan.Name, jobName)
	}

	evt.JobName = jobName

	// No message, as in createResetJob: the Job named on the event is where the detail is.
	setEventState(evt, intelv1a1.RecoveryEventStateInProgress, "")

	appendMessage(plan, fmt.Sprintf("Event %s: FDO reflash Job %s created (node: %s, bdf: %s, file: %s)",
		evt.ID, jobName, evt.NodeName, evt.GPUBDF, fw.File))

	klog.Infof("GPURecoveryPlan %s: created reflash Job %s for event %s (node: %s, bdf: %s, file: %s)",
		plan.Name, jobName, evt.ID, evt.NodeName, evt.GPUBDF, fw.File)

	return nil
}

// parkForFirmware moves a reflash event to missing-firmware, recording on the event the sentence that
// says which part of spec.firmware is missing and on the plan the shorter form of the same.
func (r *GPURecoveryPlanReconciler) parkForFirmware(plan *intelv1a1.GPURecoveryPlan,
	evt *intelv1a1.RecoveryEvent, stateMsg, planMsg string) {
	setEventState(evt, intelv1a1.RecoveryEventStateMissingFirmware, "%s", stateMsg)

	klog.Warningf("GPURecoveryPlan %s: event %s parked in %s: %s",
		plan.Name, evt.ID, intelv1a1.RecoveryEventStateMissingFirmware, stateMsg)
	appendMessage(plan, fmt.Sprintf("Event %s: firmware reflash pending — %s", evt.ID, planMsg))
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

	// The API reader, not the cached client: the pull secret lives in the operator namespace but is
	// read before any Job exists, and the manager cache is not started yet at Setup time.
	r.imgVerify = newContentImageVerifier(mgr.GetAPIReader(), opts.Namespace)

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
