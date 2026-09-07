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
	"context"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

var plansCmd = &cobra.Command{
	Use:   "plans",
	Short: "List all GPURecoveryPlan resources in the cluster",
	Long: `List the GPURecoveryPlans in the cluster.

STATE is status.state: "error" means at least one event needs human intervention,
"active" that a recovery is in flight or waiting for an approval, "idle" that there is
nothing to do. WAITING counts the events that an approval would start.`,
	Args: cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		cl, err := newDynamicClient()
		if err != nil {
			return err
		}

		list, err := cl.Resource(gpuRecoveryPlanGVR).List(context.Background(), listOpts())
		if err != nil {
			return fmt.Errorf("listing GPURecoveryPlans: %w", err)
		}

		if len(list.Items) == 0 {
			fmt.Println("No GPURecoveryPlans found.")
			return nil
		}

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tDEVICE-ID\tRESET\tSTATE\tEVENTS\tWAITING\tAPPROVALS\tAGE") //nolint:errcheck
		fmt.Fprintln(w, "────\t─────────\t─────\t─────\t──────\t───────\t─────────\t───") //nolint:errcheck

		for i := range list.Items {
			item := &list.Items[i]

			events := nestedSlice(item.Object, "status", "events")
			approvals := nestedSlice(item.Object, "spec", "approvals")

			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%d\t%d\t%s\n", //nolint:errcheck
				item.GetName(),
				orDash(nestedString(item.Object, "spec", "deviceId")),
				orDash(nestedString(item.Object, "spec", "defaultResetType")),
				orDash(nestedString(item.Object, "status", "state")),
				len(events),
				countWaitingEvents(events),
				len(approvals),
				compactDuration(time.Since(item.GetCreationTimestamp().Time)))
		}

		return w.Flush()
	},
}

// countWaitingEvents counts the events an approval would act on, i.e. the ones the operator
// considers approvable (see approvableEventStates).
func countWaitingEvents(events []interface{}) int {
	n := 0

	for _, raw := range events {
		ev, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}

		if state, _ := ev["state"].(string); approvableEventStates[state] {
			n++
		}
	}

	return n
}

func init() {
	rootCmd.AddCommand(plansCmd)
}
