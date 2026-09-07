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
	"strings"

	"github.com/spf13/cobra"
)

var (
	approveOverride string
	approveComment  string
)

var approveCmd = &cobra.Command{
	Use:   "approve <plan> <event-id>",
	Short: "Approve a single recovery event by its ID",
	Long: `Approve a specific recovery event.

The event ID is copied from 'kubectl gpurecovery events <plan>'.
The operator generates an approval ID automatically.

A failed event is terminal — nothing retries it on its own. Approving it again this way is what
starts another attempt; only an approval naming the event can do that, never a group approval.

Use --override to run a different reset than the one the plan's defaultResetType picked, e.g.
an SBR on a card the platform's normal reset does not revive. The operator records the
original suggestion in status.events[].recoveryType.suggestedType.`,
	Example: `  kubectl gpurecovery approve b580-recovery-plan evt-node03-slot-02-00-0

  # Run an SBR on this one event instead of the plan's default reset
  kubectl gpurecovery approve b580-recovery-plan evt-node03-slot-02-00-0 --override sbr`,
	Args:              cobra.ExactArgs(2),
	ValidArgsFunction: completeEventIDs,
	RunE: func(_ *cobra.Command, args []string) error {
		planName := args[0]
		eventID := args[1]

		if err := validateOverride(approveOverride); err != nil {
			return err
		}

		cl, err := newDynamicClient()
		if err != nil {
			return err
		}

		approval := map[string]interface{}{
			"eventId": eventID,
		}

		if approveOverride != "" {
			approval["override"] = map[string]interface{}{
				"recoveryType": approveOverride,
			}
		}

		if approveComment != "" {
			approval["comment"] = approveComment
		}

		if err := addApproval(cl, planName, approval); err != nil {
			return fmt.Errorf("approve event: %w", err)
		}

		if approveOverride != "" {
			fmt.Printf("Approved event %s on plan %s, overriding the recovery type to %s.\n",
				eventID, planName, approveOverride)
		} else {
			fmt.Printf("Approved event %s on plan %s.\n", eventID, planName)
		}

		return nil
	},
}

func init() {
	approveCmd.Flags().StringVar(&approveOverride, "override", "",
		fmt.Sprintf("Run this reset instead of the plan's suggestion (one of %s)",
			strings.Join(overridableRecoveryTypes, ", ")))
	approveCmd.Flags().StringVar(&approveComment, "comment", "",
		"Note recorded on the approval explaining why it was granted")

	// nolint:errcheck
	approveCmd.RegisterFlagCompletionFunc("override", completeStaticValues(overridableRecoveryTypes))
}
