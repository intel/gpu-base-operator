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

package main

import (
	"bytes"
	"testing"
)

// The fixtures below are the JSON shape of GPURecoveryPlan as the API server serves it. They
// exist because the plugin reads the CRD through the dynamic client: a renamed or moved status
// field is not a compile error here, it silently prints "-".

func TestRecoveryTypeSummary(t *testing.T) {
	for _, tc := range []struct {
		name string
		ev   map[string]interface{}
		want string
	}{
		{
			name: "reset",
			ev:   map[string]interface{}{"recoveryType": map[string]interface{}{"type": "slot"}},
			want: "slot",
		},
		{
			name: "reflash",
			ev:   map[string]interface{}{"recoveryType": map[string]interface{}{"type": "reflash"}},
			want: "reflash",
		},
		{
			name: "overridden reports the operator's original suggestion",
			ev: map[string]interface{}{"recoveryType": map[string]interface{}{
				"type": "sbr", "suggestedType": "slot",
			}},
			want: "sbr (was slot)",
		},
		{
			name: "suggestedType equal to type is not an override",
			ev: map[string]interface{}{"recoveryType": map[string]interface{}{
				"type": "amc", "suggestedType": "amc",
			}},
			want: "amc",
		},
		{
			name: "missing recoveryType",
			ev:   map[string]interface{}{},
			want: dash,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := recoveryTypeSummary(tc.ev); got != tc.want {
				t.Errorf("recoveryTypeSummary() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAttemptCount(t *testing.T) {
	for _, tc := range []struct {
		name string
		ev   map[string]interface{}
		want int
	}{
		{
			name: "no Job has run yet",
			ev:   map[string]interface{}{},
			want: 0,
		},
		{
			name: "first attempt in flight is counted before it concludes",
			ev:   map[string]interface{}{"jobName": "recovery-evt-a-0"},
			want: 1,
		},
		{
			// The operator clears jobName as it appends to pastJobs, so a concluded attempt is
			// counted once, not twice.
			name: "concluded attempt",
			ev:   map[string]interface{}{"pastJobs": []interface{}{"recovery-evt-a-0"}},
			want: 1,
		},
		{
			name: "re-approved event running its second attempt",
			ev: map[string]interface{}{
				"pastJobs": []interface{}{"recovery-evt-a-0"},
				"jobName":  "recovery-evt-a-1",
			},
			want: 2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := attemptCount(tc.ev); got != tc.want {
				t.Errorf("attemptCount() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestBlockers(t *testing.T) {
	ev := map[string]interface{}{
		"podsBlockingDrain":   []interface{}{"default/trainer-0"},
		"claimsBlockingReset": []interface{}{"default/claim-a", "default/claim-b"},
	}

	want := "default/trainer-0,default/claim-a,default/claim-b"
	if got := joinOrDash(blockers(ev)); got != want {
		t.Errorf("blockers() = %q, want %q", got, want)
	}

	if got := joinOrDash(blockers(map[string]interface{}{})); got != dash {
		t.Errorf("blockers() on an unblocked event = %q, want %q", got, dash)
	}
}

func TestApprovalSummary(t *testing.T) {
	for _, tc := range []struct {
		name           string
		ap             map[string]interface{}
		wantKind       string
		wantTarget     string
		wantPersistent bool
	}{
		{
			name:       "event approval",
			ap:         map[string]interface{}{"eventId": "evt-node03-slot-02-00-0"},
			wantKind:   "singular",
			wantTarget: "evt-node03-slot-02-00-0",
		},
		{
			name: "selector with every field",
			ap: map[string]interface{}{
				"persistent": true,
				"selector": map[string]interface{}{
					"recoveryType": "slot",
					"nodeName":     "node07",
					"nodeSelector": map[string]interface{}{"rack": "a7", "zone": "b"},
				},
			},
			wantKind:       "selector",
			wantTarget:     "type=slot node=node07 labels=rack=a7,zone=b",
			wantPersistent: true,
		},
		{
			name:       "empty selector matches anything",
			ap:         map[string]interface{}{"selector": map[string]interface{}{}},
			wantKind:   "selector",
			wantTarget: "(any)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kind, target, persistent := approvalSummary(tc.ap)
			if kind != tc.wantKind || target != tc.wantTarget || persistent != tc.wantPersistent {
				t.Errorf("approvalSummary() = (%q, %q, %v), want (%q, %q, %v)",
					kind, target, persistent, tc.wantKind, tc.wantTarget, tc.wantPersistent)
			}
		})
	}
}

func TestCountWaitingEvents(t *testing.T) {
	events := []interface{}{
		map[string]interface{}{"state": "waiting-approval"},
		map[string]interface{}{"state": "missing-firmware"},
		map[string]interface{}{"state": "failed"},
		map[string]interface{}{"state": "blocked"},
		map[string]interface{}{"state": "draining"},
		map[string]interface{}{"state": "in-progress"},
		map[string]interface{}{"state": "succeeded"},
	}

	if got := countWaitingEvents(events); got != 3 {
		t.Errorf("countWaitingEvents() = %d, want 3", got)
	}
}

func TestPlanMessages(t *testing.T) {
	plan := map[string]interface{}{
		"status": map[string]interface{}{
			"messages": []interface{}{
				"Event evt-node03-slot-02-00-0 detected",
				"Event evt-node07-amc-04-00-0 detected",
				"Event evt-node03-slot-02-00-0: reset type overridden from slot to sbr via approval app-a4af",
				"Event evt-node03-slot-02-00-0 succeeded",
			},
		},
	}

	all := planMessages(plan, "", 0)
	if len(all) != 4 {
		t.Fatalf("planMessages() returned %d messages, want 4", len(all))
	}

	if all[0] != "Event evt-node03-slot-02-00-0 detected" {
		t.Errorf("planMessages() is not oldest-first: first line is %q", all[0])
	}

	if got := planMessages(plan, "", 2); len(got) != 2 || got[1] != all[3] {
		t.Errorf("planMessages(tail=2) = %v, want the last two lines", got)
	}

	if got := planMessages(plan, "evt-node07-amc-04-00-0", 0); len(got) != 1 {
		t.Errorf("planMessages(event=...) = %v, want the one matching line", got)
	}

	// The filter runs before the tail, so --tail counts printed lines rather than scanned ones.
	if got := planMessages(plan, "evt-node03-slot-02-00-0", 2); len(got) != 2 || got[1] != all[3] {
		t.Errorf("planMessages(event=..., tail=2) = %v, want the last two matching lines", got)
	}

	if got := planMessages(map[string]interface{}{}, "", 0); len(got) != 0 {
		t.Errorf("planMessages() on a plan with no status = %v, want empty", got)
	}
}

func TestParseOutputFormat(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    outputFormat
		wantErr bool
	}{
		{in: "", want: outputTable},
		{in: "wide", want: outputWide},
		{in: "yaml", want: outputYAML},
		// kubectl accepts these; this plugin does not, and says so rather than falling back
		// to the table and printing something the caller cannot parse.
		{in: "json", wantErr: true},
		{in: "jsonpath={.id}", wantErr: true},
		{in: "YAML", wantErr: true},
		{in: "w", wantErr: true},
	} {
		got, err := parseOutputFormat(tc.in)

		if tc.wantErr {
			if err == nil {
				t.Errorf("parseOutputFormat(%q) = %q, want an error", tc.in, got)
			}

			continue
		}

		if err != nil {
			t.Errorf("parseOutputFormat(%q) = %v, want %q", tc.in, err, tc.want)
			continue
		}

		if got != tc.want {
			t.Errorf("parseOutputFormat(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestPrintYAML(t *testing.T) {
	events := []interface{}{
		map[string]interface{}{
			"id":           "evt-node03-slot-02-00-0",
			"state":        "waiting-approval",
			"pastJobs":     []interface{}{"recovery-evt-node03-slot-02-00-0-0"},
			"recoveryType": map[string]interface{}{"type": "slot"},
		},
	}

	var buf bytes.Buffer
	if err := printYAML(&buf, events); err != nil {
		t.Fatalf("printYAML() = %v", err)
	}

	want := `- id: evt-node03-slot-02-00-0
  pastJobs:
  - recovery-evt-node03-slot-02-00-0-0
  recoveryType:
    type: slot
  state: waiting-approval
`

	if buf.String() != want {
		t.Errorf("printYAML() =\n%s\nwant\n%s", buf.String(), want)
	}

	// An idle plan has no events; a parser reading the output must see an empty list rather
	// than the "null" a nil slice would marshal to.
	buf.Reset()

	if err := printYAML(&buf, []interface{}{}); err != nil {
		t.Fatalf("printYAML() on no events = %v", err)
	}

	if buf.String() != "[]\n" {
		t.Errorf("printYAML() on no events = %q, want %q", buf.String(), "[]\n")
	}
}

func TestValidateOverride(t *testing.T) {
	for _, override := range []string{"", "sbr", "slot", "amc"} {
		if err := validateOverride(override); err != nil {
			t.Errorf("validateOverride(%q) = %v, want nil", override, err)
		}
	}

	for _, override := range []string{"reflash", "flr", "SBR"} {
		if err := validateOverride(override); err == nil {
			t.Errorf("validateOverride(%q) = nil, want an error", override)
		}
	}
}
