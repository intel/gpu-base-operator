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
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

var approvalsCmd = &cobra.Command{
	Use:   "approvals <plan>",
	Short: "List approvals configured on a plan",
	Long: `List the approval entries in a plan's spec.

OVERRIDE is the recovery type the approval substitutes for the operator's suggestion;
CONSUMED marks a non-persistent approval the operator has already acted upon, kept in the
list as an audit trail.`,
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completePlanNames,
	RunE: func(_ *cobra.Command, args []string) error {
		cl, err := newDynamicClient()
		if err != nil {
			return err
		}

		plan, err := getPlan(cl, args[0])
		if err != nil {
			return err
		}

		approvals := nestedSlice(plan.Object, "spec", "approvals")
		if len(approvals) == 0 {
			fmt.Println("No approvals.")
			return nil
		}

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tKIND\tTARGET\tOVERRIDE\tPERSISTENT\tCONSUMED\tCOMMENT") //nolint:errcheck
		fmt.Fprintln(w, "──\t────\t──────\t────────\t──────────\t────────\t───────") //nolint:errcheck

		for _, raw := range approvals {
			ap, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}

			id := nestedString(ap, "id")
			kind, target, persistent := approvalSummary(ap)
			consumed, _ := ap["consumed"].(bool)

			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%v\t%v\t%s\n", //nolint:errcheck
				id, kind, target,
				orDash(nestedString(ap, "override", "recoveryType")),
				persistent, consumed,
				orDash(nestedString(ap, "comment")))
		}

		return w.Flush()
	},
}

func approvalSummary(ap map[string]interface{}) (kind, target string, persistent bool) {
	persistent, _ = ap["persistent"].(bool)

	if eventID, _ := ap["eventId"].(string); eventID != "" {
		return "singular", eventID, persistent
	}

	sel, ok := ap["selector"].(map[string]interface{})
	if !ok {
		return "unknown", dash, persistent
	}

	var parts []string

	if rt, _ := sel["recoveryType"].(string); rt != "" {
		parts = append(parts, "type="+rt)
	}

	if node, _ := sel["nodeName"].(string); node != "" {
		parts = append(parts, "node="+node)
	}

	if labels := selectorLabels(sel); labels != "" {
		parts = append(parts, "labels="+labels)
	}

	if len(parts) == 0 {
		return "selector", "(any)", persistent
	}

	return "selector", strings.Join(parts, " "), persistent
}

// selectorLabels renders selector.nodeSelector as "k=v,k=v", sorted so the output is stable
// across invocations (map iteration order is not).
func selectorLabels(sel map[string]interface{}) string {
	raw, ok := sel["nodeSelector"].(map[string]interface{})
	if !ok || len(raw) == 0 {
		return ""
	}

	pairs := make([]string, 0, len(raw))

	for k, v := range raw {
		s, _ := v.(string)
		pairs = append(pairs, k+"="+s)
	}

	sort.Strings(pairs)

	return strings.Join(pairs, ",")
}
