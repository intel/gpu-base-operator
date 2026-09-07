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
	messagesTail  int
	messagesEvent string
)

var messagesCmd = &cobra.Command{
	Use:   "messages <plan>",
	Short: "Show a plan's recent status messages",
	Long: `Print status.messages: the plan's audit trail of what the operator did and why —
approvals matching, recovery types overridden, re-approvals, failures.

It is a 50-entry ring shared by every event in the plan, printed oldest first, so ordinary
progress rotates older lines out. To find out why one event is stuck, read its MESSAGE in
'kubectl gpurecovery events <plan>' instead; this is the history around it.

Lines are free-form text, not structured records, so --event filters by matching the event ID
as a substring.`,
	Example: `  kubectl gpurecovery messages b580-recovery-plan

  # Just the last ten lines
  kubectl gpurecovery messages b580-recovery-plan --tail 10

  # Only the lines naming one event
  kubectl gpurecovery messages b580-recovery-plan --event evt-node03-slot-02-00-0`,
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completePlanNames,
	RunE: func(_ *cobra.Command, args []string) error {
		if messagesTail < 0 {
			return fmt.Errorf("--tail must not be negative")
		}

		cl, err := newDynamicClient()
		if err != nil {
			return err
		}

		plan, err := getPlan(cl, args[0])
		if err != nil {
			return err
		}

		messages := planMessages(plan.Object, messagesEvent, messagesTail)
		if len(messages) == 0 {
			if messagesEvent != "" {
				fmt.Printf("No messages mentioning %s.\n", messagesEvent)
			} else {
				fmt.Println("No messages.")
			}

			return nil
		}

		for _, msg := range messages {
			fmt.Println(msg)
		}

		return nil
	},
}

// planMessages extracts status.messages, optionally keeping only the lines mentioning eventID
// and only the last tail entries. A tail of 0 means all of them. Filtering happens before the
// tail, so --tail counts the lines actually printed.
func planMessages(plan map[string]interface{}, eventID string, tail int) []string {
	raw := nestedSlice(plan, "status", "messages")
	out := make([]string, 0, len(raw))

	for _, item := range raw {
		msg, ok := item.(string)
		if !ok {
			continue
		}

		if eventID != "" && !strings.Contains(msg, eventID) {
			continue
		}

		out = append(out, msg)
	}

	if tail > 0 && len(out) > tail {
		out = out[len(out)-tail:]
	}

	return out
}

func init() {
	messagesCmd.Flags().IntVar(&messagesTail, "tail", 0,
		"Show only the last N messages (0 shows all)")
	messagesCmd.Flags().StringVar(&messagesEvent, "event", "",
		"Show only messages mentioning this event ID")

	// nolint:errcheck
	messagesCmd.RegisterFlagCompletionFunc("event", completeEventIDsForFlag)
}
