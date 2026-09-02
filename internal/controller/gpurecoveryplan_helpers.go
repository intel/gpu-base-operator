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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	resv1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/klog/v2"

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
