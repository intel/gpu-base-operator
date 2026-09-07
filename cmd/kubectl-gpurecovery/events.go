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
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

var eventsOutput string

// stateMessageWidth caps the MESSAGE column in the default (non-wide) listing so one long
// registry or API error does not push the rest of the table off screen.
const stateMessageWidth = 90

// The listing's columns, and the rule drawn under them. Kept out of the printing code because the
// wide variant's header is too long to fit a line there.
const (
	eventsHeader     = "ID\tSTATE\tTYPE\tNODE\tBDF\tATTEMPTS\tUPDATED\tMESSAGE"
	eventsHeaderRule = "──\t─────\t────\t────\t───\t────────\t───────\t───────"

	eventsWideHeader     = "ID\tSTATE\tTYPE\tNODE\tBDF\tATTEMPTS\tUPDATED\tAPPROVAL\tJOB\tBLOCKED-BY\tMESSAGE"
	eventsWideHeaderRule = "──\t─────\t────\t────\t───\t────────\t───────\t────────\t───\t──────────\t───────"
)

var eventsCmd = &cobra.Command{
	Use:   "events <plan>",
	Short: "List recovery events for a plan",
	Long: `List the recovery events recorded in a plan's status.

MESSAGE is status.events[].stateMessage: why the event is in the state it is in, where the
state alone does not say. It is empty whenever the state speaks for itself.

ATTEMPTS is how many recovery Jobs the event has run, counted from status.events[].pastJobs
plus the Job in flight. Nothing retries a failed event on its own, so anything above one is an
event an admin approved again.

-o wide additionally shows the approval that authorised the event, its recovery Job, and
anything currently holding the recovery back (pods blocking the drain, ResourceClaims still
reserving the GPU), and does not truncate MESSAGE.

-o yaml prints status.events as it comes from the API, with the fields no column shows.`,
	Example: `  kubectl gpurecovery events b580-recovery-plan
  kubectl gpurecovery events b580-recovery-plan -o wide
  kubectl gpurecovery events b580-recovery-plan -o yaml`,
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completePlanNames,
	RunE: func(_ *cobra.Command, args []string) error {
		format, err := parseOutputFormat(eventsOutput)
		if err != nil {
			return err
		}

		cl, err := newDynamicClient()
		if err != nil {
			return err
		}

		plan, err := getPlan(cl, args[0])
		if err != nil {
			return err
		}

		events := nestedSlice(plan.Object, "status", "events")

		// A machine-readable format reports "no events" as an empty list, not as prose on
		// stdout, so that piping it into a parser works on an idle plan too.
		if format == outputYAML {
			if events == nil {
				events = []interface{}{}
			}

			return printYAML(os.Stdout, events)
		}

		if len(events) == 0 {
			fmt.Println("No events.")
			return nil
		}

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)

		if format == outputWide {
			fmt.Fprintln(w, eventsWideHeader)     //nolint:errcheck
			fmt.Fprintln(w, eventsWideHeaderRule) //nolint:errcheck
		} else {
			fmt.Fprintln(w, eventsHeader)     //nolint:errcheck
			fmt.Fprintln(w, eventsHeaderRule) //nolint:errcheck
		}

		for _, raw := range events {
			ev, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}

			id := nestedString(ev, "id")
			state := nestedString(ev, "state")
			node := nestedString(ev, "nodeName")
			bdf := nestedString(ev, "gpuBDF")
			rt := recoveryTypeSummary(ev)
			attempts := attemptCount(ev)
			updated := formatTime(nestedString(ev, "lastUpdated"))
			message := nestedString(ev, "stateMessage")

			if format != outputWide {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\n", //nolint:errcheck
					id, state, rt, node, bdf, attempts, updated,
					orDash(truncate(message, stateMessageWidth)))

				continue
			}

			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\t%s\n", //nolint:errcheck
				id, state, rt, node, bdf, attempts, updated,
				orDash(nestedString(ev, "approvalId")),
				orDash(nestedString(ev, "jobName")),
				joinOrDash(blockers(ev)),
				orDash(message))
		}

		return w.Flush()
	},
}

// recoveryTypeSummary renders status.events[].recoveryType: the type being executed, plus the
// operator's original suggestion in parentheses when an approval override replaced it.
func recoveryTypeSummary(ev map[string]interface{}) string {
	rt, ok := ev["recoveryType"].(map[string]interface{})
	if !ok {
		return dash
	}

	t, _ := rt["type"].(string)
	if t == "" {
		return dash
	}

	if suggested, _ := rt["suggestedType"].(string); suggested != "" && suggested != t {
		return fmt.Sprintf("%s (was %s)", t, suggested)
	}

	return t
}

// attemptCount reports how many recovery Jobs the event has run. The event carries no counter:
// pastJobs holds one entry per attempt that has reached a verdict, and jobName the attempt still
// in flight, which the operator only moves into pastJobs once its Job concludes.
func attemptCount(ev map[string]interface{}) int {
	n := len(nestedSlice(ev, "pastJobs"))

	if nestedString(ev, "jobName") != "" {
		n++
	}

	return n
}

// blockers collects what is currently holding the recovery back: pods the drain is still
// waiting on and ResourceClaims that still reserve the GPU.
func blockers(ev map[string]interface{}) []string {
	var out []string

	for _, field := range []string{"podsBlockingDrain", "claimsBlockingReset"} {
		for _, raw := range nestedSlice(ev, field) {
			if s, ok := raw.(string); ok && s != "" {
				out = append(out, s)
			}
		}
	}

	return out
}

func init() {
	addOutputFlag(eventsCmd, &eventsOutput,
		"Output format: 'wide' adds the approval, Job and blocking pods/claims columns "+
			"and does not truncate MESSAGE, 'yaml' prints the raw status.events entries")
}
