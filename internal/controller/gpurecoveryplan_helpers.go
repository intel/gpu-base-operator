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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	batch "k8s.io/api/batch/v1"
	core "k8s.io/api/core/v1"
	resv1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	intelv1a1 "github.com/intel/gpu-base-operator/api/v1alpha1"
)

// deviceKey identifies a GPU by the Kubernetes node it lives on and its PCI BDF address.
type deviceKey struct{ node, bdf string }

// deviceNeed is what a device's taints currently call for: the recovery type to run, plus the
// human-readable cause.
type deviceNeed struct {
	rt     intelv1a1.RecoveryType
	reason string
}

// deviceAttributeString returns the string value of the named attribute from a device's
// attribute map. Returns an empty string if the attribute is absent or not a string type.
func deviceAttributeString(attrs map[resv1.QualifiedName]resv1.DeviceAttribute, name string) string {
	if attrs == nil {
		return ""
	}

	attr, ok := attrs[resv1.QualifiedName(name)]
	if !ok || attr.StringValue == nil {
		return ""
	}

	return *attr.StringValue
}

// taintToDeviceNeed maps a device taint key to the recovery it calls for and the reason
// recorded on the resulting event. Returns false if the taint key is not a recovery-related one.
func taintToDeviceNeed(taintKey string, defaultReset intelv1a1.RecoveryType) (deviceNeed, bool) {
	switch taintKey {
	case deviceTaintKeyReset:
		return deviceNeed{rt: resetTypeOrDefault(defaultReset), reason: reasonWedged}, true
	case deviceTaintKeyXpumdReflash, deviceTaintKeyReflash:
		return deviceNeed{rt: intelv1a1.RecoveryTypeReflash, reason: reasonSurvivability}, true
	default:
		return deviceNeed{}, false
	}
}

// resetTypeOrDefault resolves spec.defaultResetType, falling back to SBR when it is unset.
//
// The field is required by the CRD, so an empty value means an object that never went through
// the API server. Falling back matters because an empty type produces a malformed event ID: SBR
// rather than slot or amc, because guessing between those two is guessing at the platform.
func resetTypeOrDefault(rt intelv1a1.RecoveryType) intelv1a1.RecoveryType {
	if rt == "" {
		klog.Warningf("GPURecoveryPlan has no spec.defaultResetType; falling back to %s",
			intelv1a1.RecoveryTypeSBR)

		return intelv1a1.RecoveryTypeSBR
	}

	return rt
}

// higherPriorityNeed returns the more severe of two recovery needs.
func higherPriorityNeed(a, b deviceNeed) deviceNeed {
	if recoveryTypePriority(b.rt) > recoveryTypePriority(a.rt) {
		return b
	}

	return a
}

// recoveryTypePriority ranks recovery types for higherPriorityNeed and escalateEvent. Only reflash
// outranks a reset; the three resets rank equally.
func recoveryTypePriority(rt intelv1a1.RecoveryType) int {
	switch rt {
	case intelv1a1.RecoveryTypeReflash:
		return 2
	case intelv1a1.RecoveryTypeSBR, intelv1a1.RecoveryTypeSlot, intelv1a1.RecoveryTypeAMC:
		return 1
	default:
		return 0
	}
}

// generateEventID returns a deterministic, human-readable event ID in the form
// "evt-<node>-<recoverytype>-<bdf-slug>".
//
// The BDF "0000:02:00.0" is sanitized to "0000-02-00-0": colons and dots are replaced with
// hyphens.
func generateEventID(nodeName, bdf string, rt intelv1a1.RecoveryType) string {
	id := fmt.Sprintf("evt-%s-%s-%s", nodeSegment(nodeName), rt, sanitizeSegment(bdf))

	// Two independent checks, because neither rule implies the other:
	//
	//   - Character set: metadata.name must be a DNS-1123 subdomain, which is stricter than a
	//     label value (uppercase and "_" pass IsValidLabelValue), so checking the name rule
	//     covers both.
	//   - Length: IsDNS1123Subdomain permits 253 characters, but the ID is also used as a label
	//     value on the recovery Job, capped at 63. The BDF is a free-form ResourceSlice attribute
	//     with no length bound of its own.
	//
	// idBudget is what is left once recoveryJobName has added its prefix and attempt suffix, so
	// an ID that fits here fits there too.
	const idBudget = maxRecoveryNameLen - len("recovery-") - len("-99")

	if errs := validation.IsDNS1123Subdomain(id); len(errs) > 0 || len(id) > idBudget {
		// Reaching here means a sanitizer let something through. Fall back to a fully-hashed
		// identity rather than attempting a repair: an unreadable but valid ID recovers the
		// GPU, a readable invalid one wedges it in a retry loop, since every attempt to name
		// the Job after it is rejected by the API server.
		//
		// rt is a closed enum of lowercase literals so it cannot be the invalid part and stays
		// outside the hash, as does the "evt-" prefix, preserving escalateEvent's contract that
		// a type change yields a new ID.
		fallback := fmt.Sprintf("evt-%s-%s", rt, hashSegment(nodeName+"/"+bdf))

		reason := strings.Join(errs, "; ")
		if reason == "" {
			reason = fmt.Sprintf("%d bytes exceeds the %d-byte budget", len(id), idBudget)
		}

		klog.Warningf("generated event ID %q for node %s BDF %s is unusable (%s); using %q instead",
			id, nodeName, bdf, reason, fallback)

		return fallback
	}

	return id
}

// recoveryJobName returns the Job name for the given event ID and attempt index.
func recoveryJobName(eventID string, attempt int) string {
	name := fmt.Sprintf("recovery-%s-%d", eventID, attempt)
	if len(name) <= maxRecoveryNameLen && len(validation.IsDNS1123Subdomain(name)) == 0 {
		return name
	}

	shortened := fmt.Sprintf("recovery-%s-%d", hashSegment(eventID), attempt)

	klog.Warningf("recovery Job name %q (%d bytes) exceeds the %d-byte limit or is not a valid resource name; using %q instead",
		name, len(name), maxRecoveryNameLen, shortened)

	return shortened
}

// nodeSegment renders a node name for use inside an event ID, bounded to nodeSegmentMax.
//
// Names within budget pass through sanitized but otherwise intact, so the common
// "evt-node03-sbr-02-00-0" is unchanged. Longer ones — an EKS-style
// "ip-10-0-134-22.us-west-2.compute.internal" is 41 characters — keep a readable prefix and gain a
// hash suffix. The full node name stays available in status.events[].nodeName and on the Job.
func nodeSegment(nodeName string) string {
	safe := sanitizeSegment(nodeName)
	if len(safe) <= nodeSegmentMax {
		return safe
	}

	// Hash the original name, not the truncated form: two nodes sharing a long prefix
	// ("worker-aaaa...-01" and "worker-aaaa...-02") must not collapse onto one ID.
	return strings.Trim(safe[:nodeSegmentMax-idHashLen-1], "-") + "-" + hashSegment(nodeName)
}

// sanitizeSegment folds an arbitrary string into something usable inside a Kubernetes
// resource name: lowercased, with every character outside [a-z0-9-] replaced by "-" and the
// result trimmed to start and end alphanumeric.
func sanitizeSegment(s string) string {
	var b strings.Builder

	b.Grow(len(s))

	for _, r := range strings.ToLower(s) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}

	return strings.Trim(b.String(), "-")
}

// hashSegment returns the first idHashLen hex characters of the SHA-256 of s.
func hashSegment(s string) string {
	sum := sha256.Sum256([]byte(s))

	return hex.EncodeToString(sum[:])[:idHashLen]
}

// recoveryTypeToArgs returns the xpu-smi command-line arguments that carry out the given reset
// against the given BDF. Returns nil for a type xpu-smi has no reset for, reflash above all: it
// writes firmware rather than resetting the device, so it is a different Job entirely.
func recoveryTypeToArgs(bdf string, rt intelv1a1.RecoveryType) []string {
	switch rt {
	case intelv1a1.RecoveryTypeSBR:
		return []string{"config", "-d", bdf, "--reset"}
	case intelv1a1.RecoveryTypeSlot:
		return []string{"config", "-d", bdf, "--coldreset"}
	case intelv1a1.RecoveryTypeAMC:
		return []string{"amc", "--gpureset", "-d", bdf, "-y"}
	default:
		return nil
	}
}

// nodeSelectorMatches reports whether the named node carries all labels in sel. An empty
// selector matches anything, mirroring how the rest of the approval selector treats unset
// fields.
func nodeSelectorMatches(sel map[string]string, nodeName string, cache *nodeLabelCache) bool {
	if len(sel) == 0 {
		return true
	}

	nodeLabels, ok := cache.get(nodeName)
	if !ok {
		return false
	}

	return labels.SelectorFromSet(sel).Matches(labels.Set(nodeLabels))
}

// nodeLabelCache memoises Node label lookups for the duration of a single findMatchingApproval
// call, where many events across a handful of nodes may each be tested against several label
// selectors. A failed lookup is recorded too, so it is not retried within the same call.
type nodeLabelCache struct {
	r      *GPURecoveryPlanReconciler
	ctx    context.Context
	labels map[string]map[string]string
	failed map[string]struct{}
}

func newNodeLabelCache(ctx context.Context, r *GPURecoveryPlanReconciler) *nodeLabelCache {
	return &nodeLabelCache{
		r:      r,
		ctx:    ctx,
		labels: make(map[string]map[string]string),
		failed: make(map[string]struct{}),
	}
}

// get returns the labels of the named Node. The second return value is false when the Node could
// not be read, in which case the caller must not treat the selector as matched: a standing
// approval scoped to a set of nodes must not authorise a reset on a node whose membership of that
// set could not be confirmed.
func (c *nodeLabelCache) get(nodeName string) (map[string]string, bool) {
	if l, ok := c.labels[nodeName]; ok {
		return l, true
	}

	if _, failed := c.failed[nodeName]; failed {
		return nil, false
	}

	node := &core.Node{}
	if err := c.r.Get(c.ctx, types.NamespacedName{Name: nodeName}, node); err != nil {
		klog.Errorf("failed to get node %s for approval label matching: %v", nodeName, err)
		c.failed[nodeName] = struct{}{}

		return nil, false
	}

	c.labels[nodeName] = node.Labels

	return node.Labels, true
}

// jobIsTerminal reports whether a Job has finished, successfully or not. Only Complete and Failed
// are terminal; a Job with no conditions yet is still running.
func jobIsTerminal(job *batch.Job) bool {
	for _, cond := range job.Status.Conditions {
		if cond.Status != core.ConditionTrue {
			continue
		}

		if cond.Type == batch.JobComplete || cond.Type == batch.JobFailed {
			return true
		}
	}

	return false
}

// jobFailureDetail renders a failed Job's condition as the reason it failed. The Reason is the
// machine-readable verdict (BackoffLimitExceeded, DeadlineExceeded), the Message is the Job
// controller's sentence about it; either can be empty depending on how the Job failed, so this
// reports whichever are there rather than composing an empty parenthetical.
func jobFailureDetail(cond batch.JobCondition) string {
	switch {
	case cond.Reason != "" && cond.Message != "":
		return fmt.Sprintf("%s (%s)", cond.Reason, cond.Message)
	case cond.Reason != "":
		return cond.Reason
	case cond.Message != "":
		return cond.Message
	default:
		return "no reason reported by the Job controller"
	}
}

// setEventState records the state an event is moving to together with the sentence that explains it,
// and stamps LastUpdated. Every state write goes through it, so status.events[].stateMessage always
// describes the state next to it.
// nolint:unparam
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

// findEventForDevice returns the index into status.events of the existing event for the
// given node+BDF, or -1 if there is none.
func findEventForDevice(plan *intelv1a1.GPURecoveryPlan, nodeName, bdf string) int {
	for i := range plan.Status.Events {
		if plan.Status.Events[i].NodeName == nodeName && plan.Status.Events[i].GPUBDF == bdf {
			return i
		}
	}

	return -1
}

// addRecoveryEvent appends a new RecoveryEvent in waiting-approval state to the plan status.
func addRecoveryEvent(plan *intelv1a1.GPURecoveryPlan, nodeName, bdf string, need deviceNeed) string {
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

	return id
}

// hasActiveJobs reports whether any event still has a Job in flight.
func hasActiveJobs(plan *intelv1a1.GPURecoveryPlan) bool {
	for _, evt := range plan.Status.Events {
		if evt.State == intelv1a1.RecoveryEventStateInProgress {
			return true
		}
	}

	return false
}

// updatePlanState derives the overall plan state from the current events and sets status.state,
// the value shown in the "State" print column of `kubectl get gpurecoveryplan`.
func updatePlanState(plan *intelv1a1.GPURecoveryPlan) {
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

// pruneConsumedApprovals removes consumed one-shot approvals that are not referred by status.events
func pruneConsumedApprovals(plan *intelv1a1.GPURecoveryPlan) {
	referenced := make(map[string]struct{}, len(plan.Status.Events))

	for _, evt := range plan.Status.Events {
		if evt.ApprovalID != "" {
			referenced[evt.ApprovalID] = struct{}{}
		}
	}

	kept := plan.Spec.Approvals[:0]

	for _, a := range plan.Spec.Approvals {
		if a.Consumed && !a.Persistent {
			if _, active := referenced[a.ID]; !active {
				klog.Infof("GPURecoveryPlan %s: pruning consumed approval %s (no active events reference it)",
					plan.Name, a.ID)

				continue
			}
		}

		kept = append(kept, a)
	}

	plan.Spec.Approvals = kept
}

// appendMessage appends a message to status.messages, evicting the oldest entry if the
// cap (maxStatusMessages) has been reached.
func appendMessage(plan *intelv1a1.GPURecoveryPlan, msg string) {
	plan.Status.Messages = append(plan.Status.Messages, msg)

	for len(plan.Status.Messages) > maxStatusMessages {
		plan.Status.Messages = plan.Status.Messages[1:]
	}
}

// requeueFailedEvents sends a failed event back to waiting-approval while its device taint is
// still there and its retry budget (spec.maxRetries) is not spent.
func requeueFailedEvents(plan *intelv1a1.GPURecoveryPlan, active map[deviceKey]deviceNeed) {
	maxRetries := plan.Spec.MaxRetries

	for i := range plan.Status.Events {
		evt := &plan.Status.Events[i]
		if evt.State != intelv1a1.RecoveryEventStateFailed {
			continue
		}

		key := deviceKey{node: evt.NodeName, bdf: evt.GPUBDF}
		if _, stillActive := active[key]; !stillActive {
			// The recovery worked, or something else fixed the GPU: removeResolvedEvents has it.
			continue
		}

		if evt.RetryCount >= maxRetries {
			klog.V(2).Infof("GPURecoveryPlan %s: event %s for %s/%s has reached max retries (%d); leaving as failed",
				plan.Name, evt.ID, evt.NodeName, evt.GPUBDF, maxRetries)

			continue
		}

		// The message is what distinguishes a re-queued event from one asking for its first
		// approval. It replaces the explanation of the failure, which described the state the
		// event is now leaving.
		setEventState(evt, intelv1a1.RecoveryEventStateWaitingApproval,
			"re-queued for retry %d of %d after the previous attempt failed; the device taint persists",
			evt.RetryCount, maxRetries)

		klog.Infof("GPURecoveryPlan %s: re-queuing failed event %s for %s/%s (retry %d/%d)",
			plan.Name, evt.ID, evt.NodeName, evt.GPUBDF, evt.RetryCount, maxRetries)
		appendMessage(plan, fmt.Sprintf("Event %s re-queued for retry %d/%d (taint persists on %s/%s)",
			evt.ID, evt.RetryCount, maxRetries, evt.NodeName, evt.GPUBDF))
	}
}

// addNewEvents creates a RecoveryEvent for every tainted device that does not have one
// yet, up to maxStatusEvents entries in status.events.
func addNewEvents(plan *intelv1a1.GPURecoveryPlan, active map[deviceKey]deviceNeed) {
	skipped := 0

	for dk, need := range active {
		// A device that already has an event does not get a second one, but its taints may
		// since have escalated to a more severe recovery type.
		if i := findEventForDevice(plan, dk.node, dk.bdf); i >= 0 {
			escalateEvent(plan, &plan.Status.Events[i], need)

			continue
		}

		if len(plan.Status.Events) >= maxStatusEvents {
			skipped++

			continue
		}

		eventId := addRecoveryEvent(plan, dk.node, dk.bdf, need)

		klog.Infof("GPURecoveryPlan %s: added recovery event %s for device %s on node %s (reason: %s)",
			plan.Name, eventId, dk.bdf, dk.node, need.reason)
	}

	if skipped > 0 {
		msg := fmt.Sprintf("status.events is at its %d-entry limit: %d newly detected device(s) not recorded",
			maxStatusEvents, skipped)

		appendMessage(plan, msg)
		klog.Warningf("GPURecoveryPlan %s: %s", plan.Name, msg)
	}
}

// escalateEvent up-levels an existing event in place when the device's taints now call for a more
// severe recovery than the event was created for.
func escalateEvent(plan *intelv1a1.GPURecoveryPlan, evt *intelv1a1.RecoveryEvent, need deviceNeed) {
	if recoveryTypePriority(need.rt) <= recoveryTypePriority(evt.RecoveryType.Type) {
		return
	}

	if evt.State == intelv1a1.RecoveryEventStateInProgress {
		klog.V(2).Infof("GPURecoveryPlan %s: device %s/%s now needs %s but event %s has a Job in flight; deferring escalation",
			plan.Name, evt.NodeName, evt.GPUBDF, need.rt, evt.ID)

		return
	}

	oldID := evt.ID
	oldType := evt.RecoveryType.Type

	// The ID embeds the recovery type, so it has to be regenerated.
	evt.ID = generateEventID(evt.NodeName, evt.GPUBDF, need.rt)
	evt.RecoveryType = intelv1a1.RecoveryTypeSpec{Type: need.rt}

	// The cause changed too — the device is in survivability mode now, not merely wedged.
	evt.Reason = need.reason

	// Reset event back to initial state.
	evt.RetryCount = 0
	evt.ApprovalID = ""
	evt.ApprovalMatchedAt = nil

	setEventState(evt, intelv1a1.RecoveryEventStateWaitingApproval,
		"escalated from %s to %s (%s); the approval for the previous type no longer applies",
		oldType, need.rt, need.reason)

	klog.Infof("GPURecoveryPlan %s: escalated event %s -> %s for %s/%s (%s -> %s); awaiting approval",
		plan.Name, oldID, evt.ID, evt.NodeName, evt.GPUBDF, oldType, need.rt)
	appendMessage(plan, fmt.Sprintf("Event %s escalated to %s on %s/%s (was %s, now %s); previous approval no longer applies",
		oldID, evt.ID, evt.NodeName, evt.GPUBDF, oldType, need.rt))
}

// runningRecoveryJobs returns the names of this plan's Jobs that have not reached a terminal
// condition. Jobs are found by the plan label rather than by walking status.events, so a Job whose
// event entry was already pruned still holds up deletion.
func runningRecoveryJobs(cli client.Reader, ctx context.Context, ns string, plan *intelv1a1.GPURecoveryPlan) ([]string, error) {
	jobList := &batch.JobList{}

	if err := cli.List(ctx, jobList,
		client.InNamespace(ns),
		client.MatchingLabels{recoveryJobLabelPlan: plan.Name},
	); err != nil {
		return nil, err
	}

	running := make([]string, 0, len(jobList.Items))

	for i := range jobList.Items {
		job := &jobList.Items[i]

		// A Job already being deleted is not something to wait for.
		if !job.DeletionTimestamp.IsZero() {
			continue
		}

		if !jobIsTerminal(job) {
			running = append(running, job.Name)
		}
	}

	return running, nil
}

// setApprovalConsumed marks the approval with the given ID as consumed
func setApprovalConsumed(plan *intelv1a1.GPURecoveryPlan, id string) {
	for i := range plan.Spec.Approvals {
		if plan.Spec.Approvals[i].ID == id {
			plan.Spec.Approvals[i].Consumed = true

			klog.Infof("GPURecoveryPlan %s: approval %s marked as consumed", plan.Name, id)

			return
		}
	}
}
