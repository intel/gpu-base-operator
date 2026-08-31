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

package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"

	core "k8s.io/api/core/v1"
	policy "k8s.io/api/policy/v1"
	resv1 "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// This file holds the node taint / pod drain primitives used when a controller has to clear a
// node before touching its GPUs. They are free functions taking an explicit client rather than
// reconciler methods, so more than one controller can call them; each keeps its own CR status
// bookkeeping and ctrl.Result handling, which is the part that genuinely differs.
//
// GPUFirmwareUpdate is the only caller today. The GPU recovery controller, which lands
// separately, is the reason the primitives are free functions instead of methods: it drains for a
// PCIe reset, and that is deliberately NOT the same drain as a firmware update's. What differs is
// only *which* pods have to leave the node:
//
//   - A firmware write does not reset the bus, so only GPU pods are at risk. GPUFirmwareUpdate
//     selects exactly those, with gpuPodsOnNode.
//   - An SBR or slot reset can disturb the host itself, which would take an unrelated pod down
//     just as hard as a GPU one. A reset therefore has to empty the node of everything evictable,
//     with podsBlockingDrain.
//
// How they leave is shared: both go through the Eviction API (evictPods), so a workload owner's
// PodDisruptionBudget is honoured rather than silently broken by a node the operator decided to
// clear.
//
// Sharing the primitives and not the policy is the point: neither drain imposes its rules on the
// other, and the parts that are genuinely identical — is this taint already there, which pods are
// on this node, does this pod hold an Intel GPU — exist once.

// +kubebuilder:rbac:groups="",resources=pods/eviction,verbs=create

// drainAction is what a full-node drain should do about a particular pod.
type drainAction int

const (
	// mirrorPodAnnotation marks a static pod's API-server mirror. Such a pod is owned by the
	// kubelet's local manifest rather than by the API server: deleting or evicting the mirror
	// does not stop the container and the kubelet recreates it immediately.
	mirrorPodAnnotation = "kubernetes.io/config.mirror"

	// podNodeNameIndex is the field index podsOnNode selects on. Pods are cached cluster-wide (see
	// ctrl.Options.Cache.ByObject in cmd/main.go).
	podNodeNameIndex = "spec.nodeName"

	// drainIgnore means the pod neither needs evicting nor blocks the drain.
	drainIgnore drainAction = iota
	drainEvict
	drainAwait
)

// ensureNodeTaint adds taint to the named node if an identical taint is not already present.
// Returns added=false when the taint was already there, so the caller can tell a fresh taint
// from a repeat pass without re-reading the node.
//
// "Identical" means key, value and effect all match: the value carries the owning CR's name,
// so two CRs tainting the same node do not clobber each other's entry.
func ensureNodeTaint(ctx context.Context, c client.Client, nodeName string, taint core.Taint) (added bool, err error) {
	node := &core.Node{}

	if err := c.Get(ctx, client.ObjectKey{Name: nodeName}, node); err != nil {
		return false, fmt.Errorf("failed to get node %s: %w", nodeName, err)
	}

	for i := range node.Spec.Taints {
		if taintsEqual(node.Spec.Taints[i], taint) {
			return false, nil
		}
	}

	node.Spec.Taints = append(node.Spec.Taints, taint)

	if err := c.Update(ctx, node); err != nil {
		return false, fmt.Errorf("failed to update node %s with taint %s: %w", nodeName, taint.Key, err)
	}

	return true, nil
}

// removeNodeTaint drops taint from the named node. Returns removed=false when the node did
// not carry it, which is not an error: an admin may have removed it by hand, and the caller
// is only asking for the taint to be gone.
func removeNodeTaint(ctx context.Context, c client.Client, nodeName string, taint core.Taint) (removed bool, err error) {
	node := &core.Node{}

	if err := c.Get(ctx, client.ObjectKey{Name: nodeName}, node); err != nil {
		return false, fmt.Errorf("failed to get node %s: %w", nodeName, err)
	}

	kept := make([]core.Taint, 0, len(node.Spec.Taints))

	for i := range node.Spec.Taints {
		if taintsEqual(node.Spec.Taints[i], taint) {
			removed = true

			continue
		}

		kept = append(kept, node.Spec.Taints[i])
	}

	if !removed {
		return false, nil
	}

	node.Spec.Taints = kept

	if err := c.Update(ctx, node); err != nil {
		return false, fmt.Errorf("failed to remove taint %s from node %s: %w", taint.Key, nodeName, err)
	}

	return true, nil
}

// taintsEqual compares the three fields that identify a taint. TimeAdded is deliberately
// excluded: it is set by the API server on NoExecute taints, so comparing whole structs would
// make a taint we added ourselves look like a different one on the next pass.
func taintsEqual(a, b core.Taint) bool {
	return a.Key == b.Key && a.Value == b.Value && a.Effect == b.Effect
}

// nodesWithTaint returns the names of all nodes currently carrying taint. Reconciling taints
// against the cluster rather than against a list kept in CR status is what makes a taint
// outlive a lost status write: it is still found, and still cleaned up.
func nodesWithTaint(ctx context.Context, c client.Client, taint core.Taint) ([]string, error) {
	nodeList := &core.NodeList{}

	if err := c.List(ctx, nodeList); err != nil {
		return nil, fmt.Errorf("failed to list nodes: %w", err)
	}

	var tainted []string

	for i := range nodeList.Items {
		for j := range nodeList.Items[i].Spec.Taints {
			if taintsEqual(nodeList.Items[i].Spec.Taints[j], taint) {
				tainted = append(tainted, nodeList.Items[i].Name)

				break
			}
		}
	}

	return tainted, nil
}

// indexPodByNodeName is the index function behind podNodeNameIndex. An unscheduled pod is on no
// node and is left out of the index entirely, so it cannot match a node name.
func indexPodByNodeName(obj client.Object) []string {
	pod, ok := obj.(*core.Pod)
	if !ok || pod.Spec.NodeName == "" {
		return nil
	}

	return []string{pod.Spec.NodeName}
}

// SetupDrainIndices registers the cache indexes the drain primitives need. Call it once per
// manager, from main, before any controller that drains is set up.
func SetupDrainIndices(ctx context.Context, mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(ctx, &core.Pod{}, podNodeNameIndex, indexPodByNodeName); err != nil {
		return fmt.Errorf("failed to register the %s pod index: %w", podNodeNameIndex, err)
	}

	return nil
}

// podsOnNode returns every cached pod assigned to the named node, across all namespaces. The
// filtering happens in the cache index, so the cost is proportional to the pods on that one node
// rather than to the size of the cluster.
func podsOnNode(ctx context.Context, c client.Client, nodeName string) ([]*core.Pod, error) {
	var pods core.PodList

	listOpts := []client.ListOption{
		client.InNamespace(core.NamespaceAll),
		client.MatchingFields{podNodeNameIndex: nodeName},
	}

	if err := c.List(ctx, &pods, listOpts...); err != nil {
		return nil, fmt.Errorf("failed to list pods on node %s: %w", nodeName, err)
	}

	onNode := make([]*core.Pod, 0, len(pods.Items))

	for i := range pods.Items {
		onNode = append(onNode, &pods.Items[i])
	}

	return onNode, nil
}

// podClaimsIntelGPU reports whether any of a pod's resource claims (direct or via template)
// requests a device from the Intel GPU DRA device class.
func podClaimsIntelGPU(ctx context.Context, c client.Client, claims []core.PodResourceClaim, namespace string) (bool, error) {
	for _, rc := range claims {
		if rc.ResourceClaimName != nil {
			resClaim := resv1.ResourceClaim{}

			if err := c.Get(ctx, client.ObjectKey{Name: *rc.ResourceClaimName, Namespace: namespace}, &resClaim); err != nil {
				return false, fmt.Errorf("failed to get ResourceClaim %s: %w", *rc.ResourceClaimName, err)
			}

			if requestsIntelGPU(resClaim.Spec.Devices.Requests) {
				return true, nil
			}
		}

		if rc.ResourceClaimTemplateName != nil {
			resClaimTmpl := resv1.ResourceClaimTemplate{}

			if err := c.Get(ctx, client.ObjectKey{Name: *rc.ResourceClaimTemplateName, Namespace: namespace}, &resClaimTmpl); err != nil {
				return false, fmt.Errorf("failed to get ResourceClaimTemplate %s: %w", *rc.ResourceClaimTemplateName, err)
			}

			if requestsIntelGPU(resClaimTmpl.Spec.Spec.Devices.Requests) {
				return true, nil
			}
		}
	}

	return false, nil
}

// requestsIntelGPU reports whether any device request — including the alternatives in a
// firstAvailable list — names the Intel GPU device class.
func requestsIntelGPU(requests []resv1.DeviceRequest) bool {
	for _, req := range requests {
		if req.Exactly != nil && req.Exactly.DeviceClassName == gpuDraDeviceClass {
			return true
		}

		for _, fa := range req.FirstAvailable {
			if fa.DeviceClassName == gpuDraDeviceClass {
				return true
			}
		}
	}

	return false
}

// gpuPodsOnNode returns the pods on a node that hold an Intel GPU, either through the device
// plugin's extended resources or through a DRA claim.
func gpuPodsOnNode(ctx context.Context, c client.Client, nodeName string) ([]*core.Pod, error) {
	pods, err := podsOnNode(ctx, c, nodeName)
	if err != nil {
		return nil, err
	}

	gpuPods := []*core.Pod{}

	for _, pod := range pods {
		include := false

		// Extended GPU resources requested by any container.
		for _, cnt := range pod.Spec.Containers {
			if _, found := cnt.Resources.Limits[xeResource]; found {
				include = true
			} else if _, found := cnt.Resources.Limits[i915Resource]; found {
				include = true
			}
		}

		if !include && len(pod.Spec.ResourceClaims) > 0 {
			isGPU, err := podClaimsIntelGPU(ctx, c, pod.Spec.ResourceClaims, pod.Namespace)
			if err != nil {
				return nil, fmt.Errorf("failed to check ResourceClaims for pod %s: %w", pod.Name, err)
			}

			include = isGPU
		}

		if include {
			gpuPods = append(gpuPods, pod)
		}
	}

	return gpuPods, nil
}

// drainNeverEvicts reports whether a full-node drain leaves this pod on the node *permanently*,
// and why.
func drainNeverEvicts(pod *core.Pod, operatorNamespace string, skipNamespaces []string) (string, bool) {
	if pod.Namespace == operatorNamespace {
		return "operator namespace", true
	}

	if slices.Contains(skipNamespaces, pod.Namespace) {
		return "namespace in namespacesToSkip", true
	}

	if _, isMirror := pod.Annotations[mirrorPodAnnotation]; isMirror {
		return "static pod", true
	}

	for i := range pod.OwnerReferences {
		if pod.OwnerReferences[i].Kind == "DaemonSet" {
			return "DaemonSet pod", true
		}
	}

	return "", false
}

// classifyPodForDrain decides how a full-node drain should treat one pod, returning the
// action and a short human-readable reason for it.
func classifyPodForDrain(pod *core.Pod, operatorNamespace string, skipNamespaces []string) (drainAction, string) {
	if reason, never := drainNeverEvicts(pod, operatorNamespace, skipNamespaces); never {
		return drainIgnore, reason
	}

	if pod.Status.Phase == core.PodSucceeded || pod.Status.Phase == core.PodFailed {
		return drainIgnore, fmt.Sprintf("already %s", pod.Status.Phase)
	}

	// Checked after the skip rules so that a terminating DaemonSet or static pod is not
	// waited on: those are recreated by design and would block forever.
	if pod.DeletionTimestamp != nil {
		return drainAwait, "already terminating"
	}

	return drainEvict, "evictable"
}

// podsBlockingDrain splits the pods on a node into those a drain must evict and those it
// merely has to wait for. A node is drained when both lists are empty.
func podsBlockingDrain(ctx context.Context, c client.Client, nodeName, operatorNamespace string,
	skipNamespaces []string) (toEvict, toAwait []*core.Pod, err error) {
	pods, err := podsOnNode(ctx, c, nodeName)
	if err != nil {
		return nil, nil, err
	}

	for _, pod := range pods {
		action, reason := classifyPodForDrain(pod, operatorNamespace, skipNamespaces)

		switch action {
		case drainEvict:
			toEvict = append(toEvict, pod)
		case drainAwait:
			toAwait = append(toAwait, pod)
		case drainIgnore:
			klog.V(2).Infof("drain of node %s ignoring pod %s/%s (%s)", nodeName, pod.Namespace, pod.Name, reason)
		}
	}

	return toEvict, toAwait, nil
}

// evictPods evicts every pod in pods, and is the step both drains share. It is safe to call on
// every pass: a pod already on its way out is skipped rather than evicted a second time, so a
// drain that is simply taking its time costs no API calls at all.
func evictPods(ctx context.Context, c client.Client, pods []*core.Pod) error {
	var errs []error

	for _, pod := range pods {
		if pod.DeletionTimestamp != nil {
			continue
		}

		err := evictPod(ctx, c, pod)

		switch {
		case err == nil:
			klog.Infof("Evicted pod %s/%s from node %s", pod.Namespace, pod.Name, pod.Spec.NodeName)
		case apierrors.IsNotFound(err):
			// The pod went away between the List and the eviction, which is the outcome we
			// wanted. Losing that race must not fail the drain.
			klog.V(2).Infof("Pod %s/%s already gone", pod.Namespace, pod.Name)
		case apierrors.IsTooManyRequests(err):
			klog.V(2).Infof("Eviction of pod %s/%s deferred by a PodDisruptionBudget, will retry: %v",
				pod.Namespace, pod.Name, err)
		default:
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// evictPod requests eviction of a pod through the policy/v1 eviction subresource rather than
// deleting it outright, so that PodDisruptionBudgets are honoured and the workload owner's
// availability guarantees are not silently broken by a node drain.
func evictPod(ctx context.Context, c client.Client, pod *core.Pod) error {
	eviction := &policy.Eviction{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pod.Name,
			Namespace: pod.Namespace,
		},
	}

	if err := c.SubResource("eviction").Create(ctx, pod, eviction); err != nil {
		return fmt.Errorf("failed to evict pod %s/%s: %w", pod.Namespace, pod.Name, err)
	}

	return nil
}
