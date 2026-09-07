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
	"slices"
	"strings"

	"github.com/spf13/cobra"
)

var (
	confirmPersistent   bool
	confirmNodeName     string
	confirmNodeSelector map[string]string
	confirmOverride     string
	confirmComment      string
)

var confirmCmd = &cobra.Command{
	Use:   "confirm <plan> <recovery-type>",
	Short: "Approve all current events of a given recovery type (group approval)",
	Long: `Add a selector-based group approval for the given recovery type.

This approves all currently waiting events that match. The approval is
marked as consumed (consumed=true) once the operator processes it, unless
--persistent is given.

The recovery type is the type the events carry, which for resets is the plan's
spec.defaultResetType unless an event was overridden. It is not what the approval runs — use
--override for that.

A group approval cannot restart a failed event; that needs
'kubectl gpurecovery approve <plan> <event-id>'.`,
	Example: `  # One-shot: approve all current slot-reset events
  kubectl gpurecovery confirm b580-recovery-plan slot

  # Node-scoped one-shot
  kubectl gpurecovery confirm b580-recovery-plan slot --node node07

  # Label-scoped, and run an SBR instead of the suggested slot reset
  kubectl gpurecovery confirm b580-recovery-plan slot --node-selector rack=a7 --override sbr

  # Persistent: auto-approve all future reflash events
  kubectl gpurecovery confirm b580-recovery-plan reflash --persistent`,
	Args: cobra.ExactArgs(2),
	ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) == 0 {
			return completePlanNames(cmd, args, toComplete)
		}

		if len(args) == 1 {
			return completeStaticValues(recoveryTypes)(cmd, args, toComplete)
		}

		return nil, cobra.ShellCompDirectiveNoFileComp
	},
	RunE: func(_ *cobra.Command, args []string) error {
		planName := args[0]
		recoveryType := args[1]

		if !slices.Contains(recoveryTypes, recoveryType) {
			return fmt.Errorf("invalid recovery type %q: must be one of %s",
				recoveryType, strings.Join(recoveryTypes, ", "))
		}

		if err := validateOverride(confirmOverride); err != nil {
			return err
		}

		// The operator's own webhook rejects a nodeName+nodeSelector combination only if the
		// labels are malformed, so catch the redundant pairing here where the intent is clear.
		if confirmNodeName != "" && len(confirmNodeSelector) > 0 {
			return fmt.Errorf("--node and --node-selector are mutually exclusive")
		}

		cl, err := newDynamicClient()
		if err != nil {
			return err
		}

		selector := map[string]interface{}{
			"recoveryType": recoveryType,
		}

		if confirmNodeName != "" {
			selector["nodeName"] = confirmNodeName
		}

		if len(confirmNodeSelector) > 0 {
			labels := make(map[string]interface{}, len(confirmNodeSelector))
			for k, v := range confirmNodeSelector {
				labels[k] = v
			}

			selector["nodeSelector"] = labels
		}

		approval := map[string]interface{}{
			"selector": selector,
		}

		if confirmPersistent {
			approval["persistent"] = true
		}

		if confirmOverride != "" {
			approval["override"] = map[string]interface{}{
				"recoveryType": confirmOverride,
			}
		}

		if confirmComment != "" {
			approval["comment"] = confirmComment
		}

		if err := addApproval(cl, planName, approval); err != nil {
			return fmt.Errorf("confirm group: %w", err)
		}

		fmt.Printf("Added %s %s approval%s to plan %s%s.\n",
			approvalKind(confirmPersistent), recoveryType, scopeSuffix(), planName,
			overrideSuffix(confirmOverride))

		return nil
	},
}

func approvalKind(persistent bool) string {
	if persistent {
		return "persistent"
	}

	return "one-shot"
}

func scopeSuffix() string {
	switch {
	case confirmNodeName != "":
		return " on node " + confirmNodeName
	case len(confirmNodeSelector) > 0:
		return " on nodes matching " + selectorLabels(map[string]interface{}{
			"nodeSelector": toUnstructuredMap(confirmNodeSelector),
		})
	default:
		return ""
	}
}

func overrideSuffix(override string) string {
	if override == "" {
		return ""
	}

	return ", overriding the recovery type to " + override
}

func toUnstructuredMap(m map[string]string) map[string]interface{} {
	out := make(map[string]interface{}, len(m))
	for k, v := range m {
		out[k] = v
	}

	return out
}

func init() {
	confirmCmd.Flags().BoolVar(&confirmPersistent, "persistent", false,
		"Keep the approval alive to auto-approve future matching events")
	confirmCmd.Flags().StringVar(&confirmNodeName, "node", "",
		"Restrict approval to events on a specific node")
	confirmCmd.Flags().StringToStringVar(&confirmNodeSelector, "node-selector", nil,
		"Restrict approval to events on nodes carrying all these labels (key=value,key=value)")
	confirmCmd.Flags().StringVar(&confirmOverride, "override", "",
		fmt.Sprintf("Run this reset instead of the events' suggestion (one of %s)",
			strings.Join(overridableRecoveryTypes, ", ")))
	confirmCmd.Flags().StringVar(&confirmComment, "comment", "",
		"Note recorded on the approval explaining why it was granted")

	// nolint:errcheck
	confirmCmd.RegisterFlagCompletionFunc("override", completeStaticValues(overridableRecoveryTypes))
}
