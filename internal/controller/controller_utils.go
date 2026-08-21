/*
Copyright 2025 Intel Corporation. All Rights Reserved.

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

package controller

import (
	"context"

	v1alpha "github.com/intel/gpu-base-operator/api/v1alpha1"
	core "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func generateNodeSelector(cp *v1alpha.ClusterPolicy, opts ControllerOpts) map[string]string {
	ns := map[string]string{
		"kubernetes.io/arch": "amd64",
	}

	if len(cp.Spec.NodeSelector) > 0 {
		for k, v := range cp.Spec.NodeSelector {
			ns[k] = v
		}
	}

	if cp.Spec.UseNFDLabeling {
		ns["intel.feature.node.kubernetes.io/gpu"] = trueValue
	}

	if opts.KMMModuleReadyLabel != "" && cp.Spec.KernelModule != nil {
		ns[opts.KMMModuleReadyLabel] = ""
	}

	return ns
}

func generateTolerations(cp *v1alpha.ClusterPolicy) []core.Toleration {
	tolerations := []core.Toleration{}

	if len(cp.Spec.Tolerations) > 0 {
		tolerations = cp.Spec.Tolerations
	}

	return tolerations
}

func isClusterPolicyBeingDeleted(cp *v1alpha.ClusterPolicy) bool {
	// CP is nil, which means the CR was deleted, so we should remove everything.
	if cp == nil {
		return true
	}

	// CP is marked for deletion, so we should remove everything.
	if !cp.DeletionTimestamp.IsZero() {
		return true
	}

	return false
}

func shouldRemoveDRA(cp *v1alpha.ClusterPolicy) bool {
	if isClusterPolicyBeingDeleted(cp) {
		return true
	}

	// DRA not selected
	if cp.Spec.ResourceRegistration != resourceModeDRA {
		return true
	}

	return false
}

func shouldRemoveDevicePlugin(cp *v1alpha.ClusterPolicy) bool {
	if isClusterPolicyBeingDeleted(cp) {
		return true
	}

	// DP not selected
	if cp.Spec.ResourceRegistration != resourceModeDP {
		return true
	}

	return false
}

func shouldRemoveXpumd(cp *v1alpha.ClusterPolicy) bool {
	if isClusterPolicyBeingDeleted(cp) {
		return true
	}

	// No resource monitoring, no xpumd
	if !cp.Spec.ResourceMonitoring {
		return true
	}

	return false
}

func anyAllocatedResourceClaims(ctx context.Context, c client.Client, driverName string) bool {
	var rcList resourcev1.ResourceClaimList

	if err := c.List(ctx, &rcList); err != nil {
		klog.Error(err, "unable to list ResourceClaims, assuming allocated claims exist")
		return true
	}

	for _, claim := range rcList.Items {
		alloc := claim.Status.Allocation
		if alloc == nil || len(alloc.Devices.Results) == 0 {
			continue
		}

		for _, dev := range alloc.Devices.Results {
			if dev.Driver == driverName {
				klog.Infof("Found allocated ResourceClaim with GPU device: %s", claim.Name)
				return true
			}
		}
	}

	return false
}

// Convert the integer based log level to a string based log level for the OTel config.
func logLevelForXpum(cp *v1alpha.ClusterPolicy) string {
	v := cp.Spec.XpuManagerSpec.LogLevel
	v = max(cp.Spec.LogLevel, v)

	switch v {
	case 0:
		return "error"
	case 1:
		return "warn"
	case 2:
		return "info"
	default:
		return "debug"
	}
}
