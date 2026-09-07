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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/util/retry"
)

func listOpts() metav1.ListOptions { return metav1.ListOptions{} }

func getPlan(cl dynamic.Interface, name string) (*unstructured.Unstructured, error) {
	plan, err := cl.Resource(gpuRecoveryPlanGVR).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get plan %q: %w", name, err)
	}

	return plan, nil
}

// addApproval appends an approval entry to spec.approvals and updates the plan.
// approval must be a JSON-serialisable map matching RecoveryApproval fields.
func addApproval(cl dynamic.Interface, planName string, approval map[string]interface{}) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		plan, err := getPlan(cl, planName)
		if err != nil {
			return err
		}

		approvals := nestedSlice(plan.Object, "spec", "approvals")
		approvals = append(approvals, approval)

		if err := unstructured.SetNestedSlice(plan.Object, approvals, "spec", "approvals"); err != nil {
			return fmt.Errorf("setting approvals: %w", err)
		}

		_, err = cl.Resource(gpuRecoveryPlanGVR).Update(
			context.Background(), plan, metav1.UpdateOptions{},
		)

		return err
	})
}

// removeApprovalByID removes the approval with the given ID from spec.approvals.
func removeApprovalByID(cl dynamic.Interface, planName, approvalID string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		plan, err := getPlan(cl, planName)
		if err != nil {
			return err
		}

		approvals := nestedSlice(plan.Object, "spec", "approvals")

		kept := make([]interface{}, 0, len(approvals))
		found := false

		for _, raw := range approvals {
			ap, ok := raw.(map[string]interface{})
			if !ok {
				kept = append(kept, raw)
				continue
			}

			if id, _ := ap["id"].(string); id == approvalID {
				found = true
				continue
			}

			kept = append(kept, raw)
		}

		if !found {
			return fmt.Errorf("approval %q not found in plan %q", approvalID, planName)
		}

		if err := unstructured.SetNestedSlice(plan.Object, kept, "spec", "approvals"); err != nil {
			return fmt.Errorf("setting approvals: %w", err)
		}

		_, err = cl.Resource(gpuRecoveryPlanGVR).Update(
			context.Background(), plan, metav1.UpdateOptions{},
		)
		return err
	})
}

// nestedSlice safely extracts a []interface{} from a nested map path.
// Returns nil (empty slice) if the path doesn't exist.
func nestedSlice(obj map[string]interface{}, fields ...string) []interface{} {
	s, _, _ := unstructured.NestedSlice(obj, fields...)
	return s
}

// nestedString safely extracts a string from a nested map path.
func nestedString(obj map[string]interface{}, fields ...string) string {
	s, _, _ := unstructured.NestedString(obj, fields...)
	return s
}
