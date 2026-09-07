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
	"strings"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
)

var (
	kubeconfig  string
	kubeContext string
)

var gpuRecoveryPlanGVR = schema.GroupVersionResource{
	Group:    "intel.com",
	Version:  "v1alpha1",
	Resource: "gpurecoveryplans",
}

var rootCmd = &cobra.Command{
	Use:   "kubectl-gpurecovery",
	Short: "Manage GPU recovery plans and events",
	Long:  "A kubectl plugin for inspecting and approving GPURecoveryPlan events.",
}

func init() {
	rootCmd.PersistentFlags().StringVar(&kubeconfig, "kubeconfig", "",
		"Path to kubeconfig (defaults to KUBECONFIG env then ~/.kube/config)")
	rootCmd.PersistentFlags().StringVar(&kubeContext, "context", "",
		"Kubernetes context to use")

	rootCmd.AddCommand(eventsCmd)
	rootCmd.AddCommand(messagesCmd)
	rootCmd.AddCommand(approvalsCmd)
	rootCmd.AddCommand(approveCmd)
	rootCmd.AddCommand(confirmCmd)
	rootCmd.AddCommand(removeCmd)
	rootCmd.AddCommand(completionCmd)
}

func newDynamicClient() (dynamic.Interface, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		loadingRules.ExplicitPath = kubeconfig
	}

	overrides := &clientcmd.ConfigOverrides{}
	if kubeContext != "" {
		overrides.CurrentContext = kubeContext
	}

	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		loadingRules, overrides,
	).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("building kubeconfig: %w", err)
	}

	return dynamic.NewForConfig(cfg)
}

// completePlanNames returns a ValidArgsFunction that completes GPURecoveryPlan names.
func completePlanNames(_ *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	cl, err := newDynamicClient()
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}

	list, err := cl.Resource(gpuRecoveryPlanGVR).List(context.Background(), listOpts())
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}

	var names []string

	for _, item := range list.Items {
		if name := item.GetName(); strings.HasPrefix(name, toComplete) {
			names = append(names, name)
		}
	}

	return names, cobra.ShellCompDirectiveNoFileComp
}

// completeEventIDs is a ValidArgsFunction that completes the IDs of events an approval would
// act on (see approvableEventStates) from the plan given as args[0].
func completeEventIDs(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) == 0 {
		return completePlanNames(cmd, args, toComplete)
	}

	if len(args) > 1 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	return completeEventIDsInPlan(args[0], toComplete, approvableEventStates)
}

// completeEventIDsForFlag completes an --event flag value with every event ID in the plan named
// by args[0], regardless of state: a flag that filters history is not restricted to the events
// something can still be done about.
func completeEventIDsForFlag(_ *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) == 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	return completeEventIDsInPlan(args[0], toComplete, nil)
}

// completeEventIDsInPlan lists the status.events[].id values in planName that start with
// toComplete. A nil states map accepts every state.
func completeEventIDsInPlan(planName, toComplete string, states map[string]bool) ([]string, cobra.ShellCompDirective) {
	cl, err := newDynamicClient()
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}

	plan, err := getPlan(cl, planName)
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}

	var ids []string

	for _, raw := range nestedSlice(plan.Object, "status", "events") {
		ev, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}

		if states != nil {
			if state, _ := ev["state"].(string); !states[state] {
				continue
			}
		}

		if id, _ := ev["id"].(string); strings.HasPrefix(id, toComplete) {
			ids = append(ids, id)
		}
	}

	return ids, cobra.ShellCompDirectiveNoFileComp
}

// completeApprovalIDs returns a ValidArgsFunction that completes approval IDs
// from the plan given as args[0].
func completeApprovalIDs(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) == 0 {
		return completePlanNames(cmd, args, toComplete)
	}

	if len(args) > 1 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	cl, err := newDynamicClient()
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}

	plan, err := getPlan(cl, args[0])
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}

	approvals := nestedSlice(plan.Object, "spec", "approvals")
	var ids []string

	for _, raw := range approvals {
		ap, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}

		if id, _ := ap["id"].(string); strings.HasPrefix(id, toComplete) {
			ids = append(ids, id)
		}
	}

	return ids, cobra.ShellCompDirectiveNoFileComp
}

var completionCmd = &cobra.Command{
	Use:       "completion [bash|zsh|fish|powershell]",
	Short:     "Generate shell completion script",
	ValidArgs: []string{"bash", "zsh", "fish", "powershell"},
	Args:      cobra.ExactArgs(1),
	RunE: func(_ *cobra.Command, args []string) error {
		switch args[0] {
		case "bash":
			return rootCmd.GenBashCompletion(os.Stdout)
		case "zsh":
			return rootCmd.GenZshCompletion(os.Stdout)
		case "fish":
			return rootCmd.GenFishCompletion(os.Stdout, true)
		case "powershell":
			return rootCmd.GenPowerShellCompletionWithDesc(os.Stdout)
		default:
			return fmt.Errorf("unsupported shell: %s", args[0])
		}
	},
}
