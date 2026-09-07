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

	"github.com/spf13/cobra"
)

var removeCmd = &cobra.Command{
	Use:   "remove <plan> <approval-id>",
	Short: "Remove an approval from a plan",
	Long: `Remove a specific approval entry by its ID.

Use 'kubectl gpurecovery approvals <plan>' to list approval IDs.`,
	Example:           `  kubectl gpurecovery remove b580-recovery-plan app-a4af`,
	Args:              cobra.ExactArgs(2),
	ValidArgsFunction: completeApprovalIDs,
	RunE: func(_ *cobra.Command, args []string) error {
		planName := args[0]
		approvalID := args[1]

		cl, err := newDynamicClient()
		if err != nil {
			return err
		}

		if err := removeApprovalByID(cl, planName, approvalID); err != nil {
			return fmt.Errorf("remove approval: %w", err)
		}

		fmt.Printf("Removed approval %s from plan %s.\n", approvalID, planName)

		return nil
	},
}
