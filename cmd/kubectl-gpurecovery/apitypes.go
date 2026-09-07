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

// This file mirrors the vocabulary of the GPURecoveryPlan CRD
// (api/v1alpha1/gpurecoveryplan_types.go). The plugin talks to the API through the dynamic
// client, so nothing here is checked by the compiler — keep it in step with the CRD enums.

// recoveryTypes are the values of the RecoveryType enum, i.e. what a
// status.events[].recoveryType.type or a selector's recoveryType can be.
var recoveryTypes = []string{"sbr", "slot", "amc", "reflash"}

// overridableRecoveryTypes are the values accepted in spec.approvals[].override.recoveryType.
var overridableRecoveryTypes = []string{"sbr", "slot", "amc"}

// approvableEventStates are the RecoveryEventState values in which an approval has an effect.
var approvableEventStates = map[string]bool{
	"waiting-approval": true,
	"missing-firmware": true,
	"failed":           true,
}

// validateOverride rejects an --override value the operator would silently ignore.
func validateOverride(override string) error {
	if override == "" || slices.Contains(overridableRecoveryTypes, override) {
		return nil
	}

	if override == "reflash" {
		return fmt.Errorf("--override reflash is not supported: the operator decides which events " +
			"need a firmware reflash and ignores an override to it")
	}

	return fmt.Errorf("invalid --override %q: must be one of %s",
		override, strings.Join(overridableRecoveryTypes, ", "))
}

// completeStaticValues builds a completion function over a fixed value list.
func completeStaticValues(values []string) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(_ *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		var matches []string

		for _, v := range values {
			if strings.HasPrefix(v, toComplete) {
				matches = append(matches, v)
			}
		}

		return matches, cobra.ShellCompDirectiveNoFileComp
	}
}
