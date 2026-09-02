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
	"reflect"
	"time"
	"unicode/utf8"

	resv1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	intelv1a1 "github.com/intel/gpu-base-operator/api/v1alpha1"
)

type GPURecoveryPlanReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Opts   ControllerOpts
}

// +kubebuilder:rbac:groups=intel.com,resources=gpurecoveryplans,verbs=get;list;watch
// +kubebuilder:rbac:groups=intel.com,resources=gpurecoveryplans/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=resource.k8s.io,resources=resourceslices,verbs=get;list;watch

// Reconcile is the main reconciliation loop for GPURecoveryPlan.
//
// The loop is triggered either by a change to a GPURecoveryPlan or by a ResourceSlice event
// routed through resourceSliceToPlans.
//
// Detection only, for now: it reflects the GPUs the DRA driver has tainted into status.events
// and derives status.state from them. Nothing is acted upon — every event it creates waits for
// an admin approval that no later phase yet consumes.
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

	// Reflect the current cluster GPU state into status.events.
	if err := r.syncRecoveryEventsFromSlices(ctx, plan); err != nil {
		return ctrl.Result{}, fmt.Errorf("syncRecoveryEventsFromSlices: %w", err)
	}

	// Derive status.state from the resulting event states.
	r.updatePlanState(plan)

	return ctrl.Result{}, nil
}

// persistPlan writes back whatever the reconcile phases changed on plan's status. It is called
// from Reconcile's defer and returns the error encountered, so the caller can fail the reconcile
// and get a retry with backoff.
func (r *GPURecoveryPlanReconciler) persistPlan(ctx context.Context, key types.NamespacedName, orig, plan *intelv1a1.GPURecoveryPlan) error {
	if reflect.DeepEqual(orig.Status, plan.Status) {
		return nil
	}

	wantStatus := plan.Status.DeepCopy()

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := r.Get(ctx, key, plan); err != nil {
			return err
		}

		plan.Status = *wantStatus.DeepCopy()

		return r.Status().Update(ctx, plan)
	})
	if err != nil {
		klog.Errorf("GPURecoveryPlan %s: failed to update status: %v", plan.Name, err)

		return fmt.Errorf("updating status: %w", err)
	}

	return nil
}

// syncRecoveryEventsFromSlices scans all ResourceSlices for GPU devices that match this
// plan's spec.deviceId and carry recovery-related device taints, then reconciles
// status.events against what it found: events whose taint has cleared are removed, and newly
// tainted devices get an event (or have their existing one escalated).
//
// The taint keys it recognises are deviceTaintKeyReset, deviceTaintKeyReflash and
// deviceTaintKeyXpumdReflash; see taintToDeviceNeed, which resolves the first against
// plan.Spec.DefaultResetType.
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

	// Remove events whose taint has cleared. This runs before the add loop so that resolved
	// events free up room under maxStatusEvents in the same pass.
	r.removeResolvedEvents(plan, activeKeys)

	// Add new events for newly tainted devices.
	r.addNewEvents(plan, activeKeys)

	// status.state is deliberately NOT set here: Reconcile derives it once, after every phase
	// that can move an event has run.

	return nil
}

// findEventForDevice returns the index into status.events of the existing event for the
// given node+BDF, or -1 if there is none.
//
// This enforces a single-event-per-device invariant: at most one recovery may run against a GPU at a
// time, so a new event is only created once the previous one has been removed (the taint cleared) and
// the first match is the only match. A device whose taints escalate is handled by escalateEvent
// rather than by adding a second event.
func (r *GPURecoveryPlanReconciler) findEventForDevice(plan *intelv1a1.GPURecoveryPlan, nodeName, bdf string) int {
	for i := range plan.Status.Events {
		if plan.Status.Events[i].NodeName == nodeName && plan.Status.Events[i].GPUBDF == bdf {
			return i
		}
	}

	return -1
}

// addNewEvents creates a RecoveryEvent for every tainted device that does not have one
// yet, up to maxStatusEvents entries in status.events.
func (r *GPURecoveryPlanReconciler) addNewEvents(plan *intelv1a1.GPURecoveryPlan, active map[deviceKey]deviceNeed) {
	skipped := 0

	for dk, need := range active {
		// A device that already has an event does not get a second one, but its taints may
		// since have escalated to a more severe recovery type.
		if i := r.findEventForDevice(plan, dk.node, dk.bdf); i >= 0 {
			r.escalateEvent(plan, &plan.Status.Events[i], need)

			continue
		}

		if len(plan.Status.Events) >= maxStatusEvents {
			skipped++

			continue
		}

		r.addRecoveryEvent(plan, dk.node, dk.bdf, need)
	}

	if skipped > 0 {
		msg := fmt.Sprintf("status.events is at its %d-entry limit: %d newly detected device(s) not recorded",
			maxStatusEvents, skipped)

		r.appendMessage(plan, msg)
		klog.Warningf("GPURecoveryPlan %s: %s", plan.Name, msg)
	}
}

// addRecoveryEvent appends a new RecoveryEvent in waiting-approval state to the plan status.
func (r *GPURecoveryPlanReconciler) addRecoveryEvent(plan *intelv1a1.GPURecoveryPlan, nodeName, bdf string, need deviceNeed) {
	id := generateEventID(nodeName, bdf, need.rt)

	evt := intelv1a1.RecoveryEvent{
		ID:           id,
		NodeName:     nodeName,
		GPUBDF:       bdf,
		Reason:       need.reason,
		RecoveryType: intelv1a1.RecoveryTypeSpec{Type: need.rt},
		RetryCount:   0,
	}

	// No message: reason, nodeName, gpuBDF and recoveryType already say everything about a new
	// event, and restating them would only train an admin to ignore the field.
	setEventState(&evt, intelv1a1.RecoveryEventStateWaitingApproval, "")

	plan.Status.Events = append(plan.Status.Events, evt)

	r.appendMessage(plan, fmt.Sprintf("New recovery event %s for %s on %s (reason: %s, type: %s)",
		id, bdf, nodeName, need.reason, need.rt))

	klog.Infof("GPURecoveryPlan %s: added recovery event %s for device %s on node %s (reason: %s)",
		plan.Name, id, bdf, nodeName, need.reason)
}

// escalateEvent up-levels an existing event in place when the device's taints now call for a more
// severe recovery than the event was created for. In practice that is the wedged -> survivability
// transition (reset -> reflash): the DRA driver applies the survivability taint alongside the wedged
// one, so a GPU that was merely stuck can turn out to need a firmware reflash while its reset event
// is still pending.
//
// Escalation is in-place rather than a second event because only one recovery may run against a GPU
// at a time; two approvable events for one device would let a reset and a reflash Job race on the
// same hardware.
//
// Escalation is one-way: the reverse (survivability clears, wedged remains) does not downgrade,
// mirroring higherPriorityNeed's "act on the worst condition" rule. Being monotonic also bounds it —
// reflash is the top of the ordering, so a device escalates at most once per event lifetime and
// flapping taints cannot drive a loop.
func (r *GPURecoveryPlanReconciler) escalateEvent(plan *intelv1a1.GPURecoveryPlan, evt *intelv1a1.RecoveryEvent, need deviceNeed) {
	if recoveryTypePriority(need.rt) <= recoveryTypePriority(evt.RecoveryType.Type) {
		return
	}

	oldID := evt.ID
	oldType := evt.RecoveryType.Type

	// The ID embeds the recovery type, so it has to be regenerated. That also invalidates
	// any spec.approvals entry naming the old ID, which is the point: an admin who approved
	// a slot reset has not approved a firmware reflash, and re-using the ID would silently
	// promote the narrower approval to the more destructive operation. A selector approval
	// for the new type still matches, since that is an explicit standing decision.
	evt.ID = generateEventID(evt.NodeName, evt.GPUBDF, need.rt)
	evt.RecoveryType = intelv1a1.RecoveryTypeSpec{Type: need.rt}

	// The cause changed too — the device is in survivability mode now, not merely wedged.
	evt.Reason = need.reason

	// Back to square one: unapproved, with a fresh retry budget, because the escalated operation is
	// not the one the previous attempts were spending that budget on.
	evt.RetryCount = 0
	evt.ApprovalID = ""
	evt.ApprovalMatchedAt = nil

	// Back in waiting-approval with a different ID than the admin last saw, which needs saying: an
	// approval that was granted has stopped applying, and nothing else on the event explains why.
	setEventState(evt, intelv1a1.RecoveryEventStateWaitingApproval,
		"escalated from %s to %s (%s); the approval for the previous type no longer applies",
		oldType, need.rt, need.reason)

	klog.Infof("GPURecoveryPlan %s: escalated event %s -> %s for %s/%s (%s -> %s); awaiting approval",
		plan.Name, oldID, evt.ID, evt.NodeName, evt.GPUBDF, oldType, need.rt)
	r.appendMessage(plan, fmt.Sprintf("Event %s escalated to %s on %s/%s (was %s, now %s); previous approval no longer applies",
		oldID, evt.ID, evt.NodeName, evt.GPUBDF, oldType, need.rt))
}

// removeResolvedEvents removes events whose device taint has cleared: nothing has been done to
// the GPU yet, so a cleared taint means whatever healed it (a node reboot, an admin) has made
// the recovery unnecessary, and keeping the event would leave the plan asking for approval to
// reset a healthy card.
func (r *GPURecoveryPlanReconciler) removeResolvedEvents(
	plan *intelv1a1.GPURecoveryPlan,
	active map[deviceKey]deviceNeed,
) {
	kept := plan.Status.Events[:0]

	for _, evt := range plan.Status.Events {
		key := deviceKey{node: evt.NodeName, bdf: evt.GPUBDF}
		if _, stillActive := active[key]; stillActive {
			kept = append(kept, evt)

			continue
		}

		klog.Infof("GPURecoveryPlan %s: removing resolved event %s (taint cleared on %s/%s, state: %s)",
			plan.Name, evt.ID, evt.NodeName, evt.GPUBDF, evt.State)

		r.appendMessage(plan, fmt.Sprintf("Event %s cleared: taint resolved on %s/%s",
			evt.ID, evt.NodeName, evt.GPUBDF))
	}

	plan.Status.Events = kept
}

// setEventState records the state an event is moving to together with the sentence that explains it,
// and stamps LastUpdated. Every state write goes through it, so status.events[].stateMessage always
// describes the state next to it.
//
// Returns the timestamp the event now carries, so a caller that has another clock to set uses the
// same instant rather than reading the wall clock twice.
//
// nolint:unparam // detection only ever parks an event in waiting-approval; the states the
// recovery phases move it through are the reason this takes the state as a parameter.
func setEventState(evt *intelv1a1.RecoveryEvent, state intelv1a1.RecoveryEventState, format string, args ...any) metav1.Time {
	msg := ""
	if format != "" {
		msg = capString(fmt.Sprintf(format, args...), maxStateMessageLen)
	}

	if evt.State == state && evt.StateMessage == msg && evt.LastUpdated != nil {
		return *evt.LastUpdated
	}

	now := metav1.NewTime(time.Now())
	evt.State = state
	evt.StateMessage = msg
	evt.LastUpdated = &now

	return now
}

// capString truncates a single string for storage in status, marking it when anything was cut so a
// reader can tell a complete message from a clipped one. Counted in bytes rather than runes, since
// the limit exists to bound the size of the stored object.
func capString(s string, limit int) string {
	const marker = "..."

	if len(s) <= limit {
		return s
	}

	// Back off to a rune boundary so the result stays valid UTF-8; the API server would otherwise
	// reject the status write outright.
	cut := limit - len(marker)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}

	return s[:cut] + marker
}

// updatePlanState derives the overall plan state from the current events and sets status.state,
// the value shown in the "State" print column of `kubectl get gpurecoveryplan`.
func (r *GPURecoveryPlanReconciler) updatePlanState(plan *intelv1a1.GPURecoveryPlan) {
	anyActive := false
	anyStuck := false

	for _, evt := range plan.Status.Events {
		switch evt.State {
		case intelv1a1.RecoveryEventStateWaitingApproval,
			intelv1a1.RecoveryEventStateBlocked,
			intelv1a1.RecoveryEventStateDraining,
			intelv1a1.RecoveryEventStateInProgress:
			// blocked is active, not stuck: it clears on its own once the node frees up.
			anyActive = true

		case intelv1a1.RecoveryEventStateMissingFirmware:
			// Blocked on operator configuration, not on hardware or an admin decision:
			// the reflash cannot even be attempted until spec.firmware is filled in.
			anyStuck = true

		case intelv1a1.RecoveryEventStateFailed:
			// A failure within the retry budget is re-queued for another approval, so only an
			// event that has spent its budget needs an admin.
			if evt.RetryCount >= plan.Spec.MaxRetries {
				anyStuck = true
			}
		}
	}

	switch {
	case anyStuck:
		plan.Status.State = intelv1a1.PlanStateError
	case anyActive:
		plan.Status.State = intelv1a1.PlanStateActive
	default:
		plan.Status.State = intelv1a1.PlanStateIdle
	}
}

// appendMessage appends a message to status.messages, evicting the oldest entry if the
// cap (maxStatusMessages) has been reached.
func (r *GPURecoveryPlanReconciler) appendMessage(plan *intelv1a1.GPURecoveryPlan, msg string) {
	plan.Status.Messages = append(plan.Status.Messages, msg)

	for len(plan.Status.Messages) > maxStatusMessages {
		plan.Status.Messages = plan.Status.Messages[1:]
	}
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
		Named("gpurecoveryplan").
		Complete(r)
}
