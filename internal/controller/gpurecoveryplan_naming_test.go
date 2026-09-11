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
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/util/validation"

	intelv1a1 "github.com/intel/gpu-base-operator/api/v1alpha1"
)

// Event IDs and the names derived from them: every ID has to survive being used as a
// Job name and as a label value, on node names far longer than either allows.
var _ = Describe("GPURecoveryPlan Controller: event IDs and naming", func() {
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

				// An event can be re-approved any number of times, so check the widest attempt
				// index the name budget was sized for rather than just the first attempt.
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

	// Both command lines interpolate the BDF into a string /bin/sh parses, so the shape of the
	// ResourceSlice attribute they come from is what stands between a free-form driver-written
	// string and a privileged root shell.
	Context("Helper: validDeviceBDF", func() {
		DescribeTable("should accept the addresses the kernel prints",
			func(bdf string) {
				Expect(validDeviceBDF(bdf)).To(BeTrue(), "%q is a PCI address", bdf)
			},
			Entry("domain zero", "0000:02:00.0"),
			Entry("non-zero domain", "0001:af:00.0"),
			// lspci prints PCI addresses in uppercase hex, so this is a likely spelling rather
			// than a pathological one.
			Entry("uppercase hex", "0000:AF:00.0"),
			Entry("highest function", "0000:00:1f.7"),
		)

		DescribeTable("should reject anything else",
			func(bdf string) {
				Expect(validDeviceBDF(bdf)).To(BeFalse(), "%q is not a PCI address", bdf)
			},
			Entry("empty", ""),
			Entry("no domain", "02:00.0"),
			Entry("no function", "0000:02:00"),
			Entry("function out of range", "0000:02:00.8"),
			Entry("non-hex digits", "0000:0g:00.0"),
			Entry("unexpected separators", "0000/02_00,0"),
			Entry("trailing separator", "0000:02:00.0:"),
			Entry("leading separator", ":02:00.0"),
			Entry("trailing newline", "0000:02:00.0\n"),
			// The one that matters: a shell metacharacter must not reach a command line run as
			// root in a privileged container.
			Entry("shell command substitution", "0000:02:00.0; touch /tmp/pwned"),
			Entry("shell pipeline", "0000:02:00.0 | sh"),
			Entry("backticks", "`id`"),
		)
	})
})
