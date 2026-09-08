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
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	core "k8s.io/api/core/v1"
	resv1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha "github.com/intel/gpu-base-operator/api/v1alpha1"
)

// XPUMD device refresh.
//
// The xpumd device refresh controller watches the xpumd pods on each node and compares the GPUs
// they were given against the GPUs the node currently has. If a pod is missing a GPU that is
// published and usable, the pod is deleted so the DaemonSet controller replaces it with one that
// gets the node's current GPUs.
// Controller's behavior is controlled by the ClusterPolicy's spec.xpu.restartOnDeviceRecovery field,
// which can be set to Always, OnRecoveredDevice, or Disabled. OnRecoveredDevice is when a device is known
// to have been in unusable state and then comes back. Always triggers a Pod restart every time a device
// rebinds, even if it was never unusable. Disabled disables the controller entirely.

type XpumDeviceRefreshReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Opts   ControllerOpts

	// mu guards the three per-node guard maps below.
	mu sync.Mutex

	// lastRestart records when this controller last deleted the xpum pod on a node, to space out
	// restarts of the same node.
	lastRestart map[string]time.Time

	// restartAttempts counts consecutive restarts of a node that did not resolve its divergence.
	// Reset as soon as a pass finds the node converged.
	restartAttempts map[string]int

	// lostDevices tracks, per node, the devices a pod's record says it holds that the slices have
	// since called unusable — tainted for recovery, bound to vfio, or with no driver bound at all.
	lostDevices map[string]map[string]bool
}

const (
	// xpumDevicesAnnotation records the GPUs whose device nodes its container was given.
	//	gpu.intel.com/xpum-devices: 0000-04-00-0-0xe20b,0000-05-00-0-0xe20b
	xpumDevicesAnnotation = "gpu.intel.com/xpum-devices"

	// deviceAttrDriver is the ResourceSlice device attribute name for the kernel driver
	deviceAttrDriver = "driver"

	// resourceSliceNodeNameIndex is the field index deviceStates selects on, so that a pass costs
	// one node's slices rather than the cluster's. Slice churn is frequent and reconciles are
	// per-node, so this is a hot path.
	resourceSliceNodeNameIndex = "spec.nodeName"

	xpumdMonitorableDriverXe   = "xe"
	xpumdMonitorableDriverI915 = "i915"

	// xpumRestartCooldown is the minimum spacing between two restarts on the same Node.
	xpumRestartCooldown = 2 * time.Minute

	maxConcurrentXpumRestarts = 3

	maxXpumRestartAttempts = 3

	restartReasonRecovered = "recovered-device"
	restartReasonRebind    = "rebind"
)

// deviceState is what the ResourceSlices say about one GPU right now. Presence in the map built by
// deviceStates means the device is published for the node; absence means nothing at all is known
// about it, which is deliberately not the same as bad news.
type deviceState struct {
	// recovering is set when the device carries a taint the operator recovers from, i.e. it is
	// unusable now and will re-enumerate when it is fixed.
	recovering bool

	// monitorable is set when the device has a driver bound whose devices xpumd can read. Clear
	// for a card handed to vfio for passthrough and for one with no driver bound at all.
	monitorable bool
}

// usable reports whether a published device is one xpumd could monitor if its container had the
// device node.
func (s deviceState) usable() bool {
	return s.monitorable && !s.recovering
}

type restartOutcome int

const (
	restartDone restartOutcome = iota
	restartDeferred
	restartAbandoned
)

// The ClusterPolicy is read on every pass for spec.xpu.restartOnDeviceRecovery, and the slices are
// what say whether a GPU is usable at all.
// +kubebuilder:rbac:groups=intel.com,resources=clusterpolicies,verbs=get;list;watch
// +kubebuilder:rbac:groups=resource.k8s.io,resources=resourceslices,verbs=get;list;watch

// Patching of Pods is limited to the operator's namespace. "patch" right is needed, but it's
// requested in the namespaced role, not here.
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=resource.k8s.io,resources=resourceclaims,verbs=get;list;watch

// Reconcile checks one node's xpum pods against the GPUs the node currently has, and replaces a pod
// that cannot reach one of them.
func (r *XpumDeviceRefreshReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	node := req.Name

	// Check what, if anything, the ClusterPolicy asks this controller to do.
	mode, err := r.restartMode(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}

	if mode == v1alpha.XpumRestartDisabled {
		r.forgetNode(node)

		return ctrl.Result{}, nil
	}

	states, err := r.deviceStates(ctx, node)
	if err != nil {
		return ctrl.Result{}, err
	}

	if len(states) == 0 {
		// No Intel GPUs published for this node: nothing to compare against, and nothing to record.
		r.forgetNode(node)

		return ctrl.Result{}, nil
	}

	pods, err := r.xpumPodsOnNode(ctx, node)
	if err != nil {
		return ctrl.Result{}, err
	}

	requeue := false

	for _, pod := range pods {
		podRequeue, err := r.refreshPod(ctx, pod, node, states, mode)
		if err != nil {
			return ctrl.Result{}, err
		}

		requeue = requeue || podRequeue
	}

	if requeue {
		return ctrl.Result{RequeueAfter: r.Opts.RequeueDelay}, nil
	}

	return ctrl.Result{}, nil
}

// refreshPod handles one xpum pod: adopt it if it has no record yet, otherwise compare the record
// against the node's current devices and restart the pod if it is missing one. Returns true if the
// pass should be retried, which happens when it is waiting for something (a claim allocation, a
// restart slot) rather than failing.
func (r *XpumDeviceRefreshReconciler) refreshPod(ctx context.Context, pod *core.Pod, node string,
	states map[string]deviceState, mode v1alpha.XpumRestartMode) (bool, error) {
	// Only Running pods are considered.
	if pod.Status.Phase != core.PodRunning {
		return false, nil
	}

	// Retrieve the devices annotated on the Pod.
	record, adopted := pod.Annotations[xpumDevicesAnnotation]

	// If the pod has no record yet, adopt it.
	if !adopted {
		return r.adoptPod(ctx, pod, states)
	}

	held := parseDeviceRecord(record)

	missing := missingDevices(states, held)

	// Track rebinds whatever the mode, so that a rebind resolved under OnRecoveredDevice is consumed here
	// rather than left to fire the first time somebody switches to Always.
	rebound := r.trackRebinds(node, held, states)

	reason := ""

	switch {
	case len(missing) > 0:
		reason = restartReasonRecovered

		klog.V(2).Infof("xpum pod %s/%s was never given usable GPUs %s on node %s",
			pod.Namespace, pod.Name, strings.Join(missing, ","), node)
	case len(rebound) > 0 && mode == v1alpha.XpumRestartAlways:
		reason = restartReasonRebind

		klog.V(2).Infof("xpum pod %s/%s holds GPUs %s that re-enumerated on node %s",
			pod.Namespace, pod.Name, strings.Join(rebound, ","), node)
	case len(rebound) > 0:
		klog.V(2).Infof("xpum pod %s/%s holds GPUs %s that re-enumerated on node %s; leaving them to its own rescan",
			pod.Namespace, pod.Name, strings.Join(rebound, ","), node)

		// Declining to act on it still consumes the edge, so it is not left waiting for whoever
		// switches this node to Always later.
		r.clearRebinds(node, rebound)
	}

	// No reason found, do not restart the pod.
	if reason == "" {
		// Converged: forget any restart attempts spent getting here, so an unrelated divergence
		// later starts from a full budget.
		r.resetAttempts(node)

		return false, nil
	}

	// Reason found, try to restart the pod.
	outcome, err := r.restartPod(ctx, pod, node, reason)
	if err != nil {
		return false, err
	}

	// Only now is a rebind consumed: the cooldown and the concurrency cap can push the restart to a
	// later pass, which recomputes `rebound` and needs the edge to still be there.
	if reason == restartReasonRebind && outcome != restartDeferred {
		r.clearRebinds(node, rebound)
	}

	return outcome == restartDeferred, nil
}

// adoptPod writes the initial device record for a pod that does not have one.
func (r *XpumDeviceRefreshReconciler) adoptPod(ctx context.Context, pod *core.Pod, states map[string]deviceState) (bool, error) {
	allocated, ready, err := r.allocatedDeviceNames(ctx, pod)
	if err != nil {
		return false, err
	}

	if !ready {
		// The claim is not allocated yet, or its status has not reached the cache. Try again later.
		klog.V(2).Infof("xpum pod %s/%s has no allocated monitoring claim yet; deferring adoption", pod.Namespace, pod.Name)

		return true, nil
	}

	held := make([]string, 0, len(allocated))

	for name := range allocated {
		state, published := states[name]

		if !published || state.usable() {
			held = append(held, name)
		}
	}

	formatted := formatDeviceRecord(held)

	if err := r.patchDeviceRecord(ctx, pod, formatted); err != nil {
		return false, err
	}

	klog.V(2).Infof("adopted xpum pod %s/%s with devices %q", pod.Namespace, pod.Name, formatted)

	return false, nil
}

// restartPod deletes a node's xpum pod so the DaemonSet controller replaces it with one that gets
// the node's currently usable GPUs.
func (r *XpumDeviceRefreshReconciler) restartPod(ctx context.Context, pod *core.Pod, node, reason string) (restartOutcome, error) {
	r.mu.Lock()

	attempts := r.restartAttempts[node]
	last := r.lastRestart[node]

	r.mu.Unlock()

	if attempts >= maxXpumRestartAttempts {
		// Report and stop. Restarting again would not fix whatever is keeping the device out of
		// the container, and the gauge stays up so the condition is still visible.
		klog.Errorf("xpum pod on node %s still cannot use GPUs that are published and untainted after %d restarts; giving up on this node",
			node, attempts)

		return restartAbandoned, nil
	}

	if since := time.Since(last); !last.IsZero() && since < xpumRestartCooldown {
		// Logged so that a deferred restart is distinguishable from a device this controller never
		// noticed.
		klog.V(2).Infof("deferring xpum restart on node %s (%s): last restart was %s ago, cooldown is %s",
			node, reason, since.Truncate(time.Second), xpumRestartCooldown)

		return restartDeferred, nil
	}

	restarting, err := r.restartingXpumPods(ctx)
	if err != nil {
		return restartDeferred, err
	}

	if restarting >= maxConcurrentXpumRestarts {
		klog.V(2).Infof("deferring xpum restart on node %s (%s): %d xpum pods are already restarting",
			node, reason, restarting)

		return restartDeferred, nil
	}

	if err := r.Delete(ctx, pod); err != nil {
		return restartDeferred, fmt.Errorf("failed to delete xpum pod %s/%s: %w", pod.Namespace, pod.Name, err)
	}

	r.mu.Lock()
	r.lastRestart[node] = time.Now()
	r.restartAttempts[node] = attempts + 1
	r.mu.Unlock()

	klog.Infof("restarted xpum pod %s/%s: node %s has GPUs its container cannot monitor (%s)",
		pod.Namespace, pod.Name, node, reason)

	return restartDone, nil
}

// missingDevices returns the devices that are usable on the node now and were not among the ones the
// container was given, sorted so that log lines for an unchanged condition are unchanged.
func missingDevices(states map[string]deviceState, held map[string]bool) []string {
	missing := make([]string, 0, len(states))

	for name, state := range states {
		if state.usable() && !held[name] {
			missing = append(missing, name)
		}
	}

	sort.Strings(missing)

	return missing
}

// trackRebinds notes which of the devices a container holds the slices currently call unusable, and
// reports the ones that were unusable on an earlier pass and are usable again — a card that
// re-enumerated behind a device node the container already has.
//
// The edge is any published-and-unusable state, not the recovery taint alone: a KMD unbind shows up
// as the driver attribute going "xe" → "" → "xe" with no taint anywhere, and a device handed to vfio
// and taken back has the same shape. A device vanishing from the slices is deliberately not an edge
// — see the type comment.
func (r *XpumDeviceRefreshReconciler) trackRebinds(node string, held map[string]bool, states map[string]deviceState) []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	lost := r.lostDevices[node]
	rebound := make([]string, 0, len(lost))

	for name := range held {
		state, published := states[name]
		if !published {
			continue
		}

		switch {
		case !state.usable():
			if lost == nil {
				lost = map[string]bool{}
				r.lostDevices[node] = lost
			}

			lost[name] = true
		case lost[name]:
			rebound = append(rebound, name)
		}
	}

	sort.Strings(rebound)

	return rebound
}

// clearRebinds forgets rebind edges that have been dealt with, either by a restart or by a mode that
// deliberately leaves them to xpumd's own rescan. Kept separate from trackRebinds so that a restart
// held back by the cooldown or the concurrency cap still has its edge on the next pass.
func (r *XpumDeviceRefreshReconciler) clearRebinds(node string, names []string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	lost := r.lostDevices[node]
	if lost == nil {
		return
	}

	for _, name := range names {
		delete(lost, name)
	}

	if len(lost) == 0 {
		delete(r.lostDevices, node)
	}
}

// parseDeviceRecord reads the annotation value into the set of devices the container holds.
func parseDeviceRecord(value string) map[string]bool {
	held := map[string]bool{}

	for _, entry := range strings.Split(value, ",") {
		if entry == "" {
			continue
		}

		held[entry] = true
	}

	return held
}

// formatDeviceRecord renders a device set sorted, so that the same set always produces the same
// annotation value.
func formatDeviceRecord(names []string) string {
	sorted := make([]string, len(names))
	copy(sorted, names)
	sort.Strings(sorted)

	return strings.Join(sorted, ",")
}

func (r *XpumDeviceRefreshReconciler) patchDeviceRecord(ctx context.Context, pod *core.Pod, value string) error {
	patch := client.MergeFrom(pod.DeepCopy())

	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}

	pod.Annotations[xpumDevicesAnnotation] = value

	if err := r.Patch(ctx, pod, patch); err != nil {
		return fmt.Errorf("failed to record devices on xpum pod %s/%s: %w", pod.Namespace, pod.Name, err)
	}

	return nil
}

// restartMode resolves spec.xpu.restartOnDeviceRecovery from the first ClusterPolicy, and folds in
// the two conditions that make this controller inert regardless of what the field says:
// monitoring switched off (no xpum pods to restart) and resource registration other than DRA
// (no ResourceSlices, so no way to tell whether a device is usable, and no claim for the pod either).
func (r *XpumDeviceRefreshReconciler) restartMode(ctx context.Context) (v1alpha.XpumRestartMode, error) {
	cpList := &v1alpha.ClusterPolicyList{}
	if err := r.List(ctx, cpList); err != nil {
		return v1alpha.XpumRestartDisabled, fmt.Errorf("failed to list ClusterPolicies: %w", err)
	}

	var cp *v1alpha.ClusterPolicy

	for i := range cpList.Items {
		// Skip a CR that is being deleted.
		if cpList.Items[i].DeletionTimestamp != nil {
			continue
		}

		cp = &cpList.Items[i]

		break
	}

	// No active CPs found, nothing to do.
	if cp == nil {
		return v1alpha.XpumRestartDisabled, nil
	}

	// No xpumd (monitoring) on the cluster, nothing to do.
	if !cp.Spec.ResourceMonitoring {
		return v1alpha.XpumRestartDisabled, nil
	}

	// No DRA, nothing to do.
	if !r.Opts.DRAEnable || cp.Spec.ResourceRegistration != resourceModeDRA {
		return v1alpha.XpumRestartDisabled, nil
	}

	switch mode := cp.Spec.XpuManagerSpec.RestartOnDeviceRecovery; mode {
	case v1alpha.XpumRestartOnRecoveredDevice, v1alpha.XpumRestartAlways, v1alpha.XpumRestartDisabled:
		return mode, nil
	case "":
		// An object that bypassed API-server defaulting, which in practice is a unit test.
		return v1alpha.XpumRestartOnRecoveredDevice, nil
	default:
		klog.Warningf("unknown spec.xpu.restartOnDeviceRecovery %q on ClusterPolicy %s; treating it as %s",
			mode, cp.Name, v1alpha.XpumRestartOnRecoveredDevice)

		return v1alpha.XpumRestartOnRecoveredDevice, nil
	}
}

// deviceStates returns every Intel GPU published for a node, with what the slices currently say
// about it.
func (r *XpumDeviceRefreshReconciler) deviceStates(ctx context.Context, node string) (map[string]deviceState, error) {
	slices := &resv1.ResourceSliceList{}
	if err := r.List(ctx, slices, client.MatchingFields{resourceSliceNodeNameIndex: node}); err != nil {
		return nil, fmt.Errorf("failed to list ResourceSlices for node %s: %w", node, err)
	}

	states := map[string]deviceState{}

	for i := range slices.Items {
		slice := &slices.Items[i]

		if slice.Spec.Driver != gpuDeviceClass {
			continue
		}

		for j := range slice.Spec.Devices {
			dev := &slice.Spec.Devices[j]

			prev, seen := states[dev.Name]

			state := deviceState{
				recovering:  deviceNeedsRecovery(dev),
				monitorable: xpumdMonitorableDriver(dev),
			}

			// Merge with the previous state if this device has already been seen.
			if seen {
				state.recovering = state.recovering || prev.recovering
				state.monitorable = state.monitorable && prev.monitorable
			}

			states[dev.Name] = state
		}
	}

	return states, nil
}

// deviceNeedsRecovery reports whether a device carries a taint the operator recovers from, i.e. it
// is unusable now and will re-enumerate once it is fixed.
func deviceNeedsRecovery(dev *resv1.Device) bool {
	for i := range dev.Taints {
		if _, ok := taintToDeviceNeed(dev.Taints[i].Key, v1alpha.RecoveryTypeSBR); ok {
			return true
		}
	}

	return false
}

// xpumdMonitorableDriver reports whether a device has a driver bound that gives xpumd something to
// monitor.
func xpumdMonitorableDriver(dev *resv1.Device) bool {
	attr, published := dev.Attributes[resv1.QualifiedName(deviceAttrDriver)]
	if !published || attr.StringValue == nil {
		return true
	}

	switch strings.ToLower(*attr.StringValue) {
	case xpumdMonitorableDriverXe, xpumdMonitorableDriverI915:
		return true
	default:
		return false
	}
}

// allocatedDeviceNames returns the device names allocated to a pod's monitoring claim, and whether
// the allocation could be read at all.
func (r *XpumDeviceRefreshReconciler) allocatedDeviceNames(ctx context.Context, pod *core.Pod) (map[string]bool, bool, error) {
	names := map[string]bool{}
	found := false

	for _, status := range pod.Status.ResourceClaimStatuses {
		if status.ResourceClaimName == nil {
			continue
		}

		claim := &resv1.ResourceClaim{}

		key := client.ObjectKey{Name: *status.ResourceClaimName, Namespace: pod.Namespace}
		if err := r.Get(ctx, key, claim); err != nil {
			return nil, false, fmt.Errorf("failed to get ResourceClaim %s: %w", key, err)
		}

		if claim.Status.Allocation == nil {
			continue
		}

		found = true

		for _, result := range claim.Status.Allocation.Devices.Results {
			if result.Driver != gpuDeviceClass {
				continue
			}

			names[result.Device] = true
		}
	}

	return names, found, nil
}

// xpumPodsOnNode returns the xpum pods scheduled on a node.
func (r *XpumDeviceRefreshReconciler) xpumPodsOnNode(ctx context.Context, node string) ([]*core.Pod, error) {
	pods, err := r.listXpumPods(ctx)
	if err != nil {
		return nil, err
	}

	onNode := make([]*core.Pod, 0, 1)

	for _, pod := range pods {
		if pod.Spec.NodeName == node && pod.DeletionTimestamp == nil {
			onNode = append(onNode, pod)
		}
	}

	return onNode, nil
}

func (r *XpumDeviceRefreshReconciler) listXpumPods(ctx context.Context) ([]*core.Pod, error) {
	podList := &core.PodList{}

	err := r.List(ctx, podList, client.InNamespace(r.Opts.Namespace), client.MatchingLabels{xpuLabel: xpuValue})
	if err != nil {
		return nil, fmt.Errorf("failed to list xpum pods: %w", err)
	}

	pods := make([]*core.Pod, 0, len(podList.Items))
	for i := range podList.Items {
		pods = append(pods, &podList.Items[i])
	}

	return pods, nil
}

// restartingXpumPods counts xpum pods that are not currently Running, which is how many nodes are
// mid-restart whether this controller caused it or not.
func (r *XpumDeviceRefreshReconciler) restartingXpumPods(ctx context.Context) (int, error) {
	pods, err := r.listXpumPods(ctx)
	if err != nil {
		return 0, err
	}

	count := 0

	for _, pod := range pods {
		if pod.Status.Phase != core.PodRunning || pod.DeletionTimestamp != nil {
			count++
		}
	}

	return count, nil
}

// forgetNode drops a node's guard state and metric series. Called when the node has no GPUs to
// watch or the feature is off, so that a node leaving the cluster does not leave a gauge behind
// reading whatever it read last.
func (r *XpumDeviceRefreshReconciler) forgetNode(node string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	delete(r.lastRestart, node)
	delete(r.restartAttempts, node)
	delete(r.lostDevices, node)
}

func (r *XpumDeviceRefreshReconciler) resetAttempts(node string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	delete(r.restartAttempts, node)
}

// indexResourceSliceByNodeName is the index function behind resourceSliceNodeNameIndex. A slice with
// no node — one describing network-attached devices — is left out of the index entirely, so it
// cannot match a node name.
func indexResourceSliceByNodeName(obj client.Object) []string {
	slice, ok := obj.(*resv1.ResourceSlice)
	if !ok || slice.Spec.NodeName == nil || *slice.Spec.NodeName == "" {
		return nil
	}

	return []string{*slice.Spec.NodeName}
}

// resourceSliceToNode maps a ResourceSlice event to the node it describes.
func (r *XpumDeviceRefreshReconciler) resourceSliceToNode(_ context.Context, obj client.Object) []reconcile.Request {
	slice, ok := obj.(*resv1.ResourceSlice)
	if !ok {
		return nil
	}

	if slice.Spec.Driver != gpuDeviceClass || slice.Spec.NodeName == nil {
		return nil
	}

	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: *slice.Spec.NodeName}}}
}

// xpumPodToNode maps an xpum pod event to its node.
func (r *XpumDeviceRefreshReconciler) xpumPodToNode(_ context.Context, obj client.Object) []reconcile.Request {
	pod, ok := obj.(*core.Pod)
	if !ok {
		return nil
	}

	if pod.Namespace != r.Opts.Namespace || pod.Labels[xpuLabel] != xpuValue || pod.Spec.NodeName == "" {
		return nil
	}

	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: pod.Spec.NodeName}}}
}

// SetupWithManager registers the controller with the Manager.
//
// There is no For() object: requests are node names rather than objects, and both watches map into
// that keyspace. It is a top-level controller rather than a ClusterPolicy sub-reconciler on purpose
// — ResourceSlice churn is frequent, and fanning it into ClusterPolicy would re-run the device
// plugin, DRA and misc reconcilers and diff three DaemonSets every time a GPU's taints changed.
//
// The ClusterPolicy is read on every pass but not watched, so a mode change takes effect on the next
// slice or pod event for a node rather than immediately. Nothing durable depends on the mode, so
// there is nothing for the change to act on retroactively.
func (r *XpumDeviceRefreshReconciler) SetupWithManager(mgr ctrl.Manager, opts ControllerOpts) error {
	r.Opts = opts
	r.lastRestart = map[string]time.Time{}
	r.restartAttempts = map[string]int{}
	r.lostDevices = map[string]map[string]bool{}

	// The index is only used by this controller, so it is registered here rather than alongside the
	// shared drain indexes.
	err := mgr.GetFieldIndexer().IndexField(context.Background(), &resv1.ResourceSlice{},
		resourceSliceNodeNameIndex, indexResourceSliceByNodeName)
	if err != nil {
		return fmt.Errorf("failed to register the ResourceSlice %s index: %w", resourceSliceNodeNameIndex, err)
	}

	return ctrl.NewControllerManagedBy(mgr).
		Watches(
			&resv1.ResourceSlice{},
			handler.EnqueueRequestsFromMapFunc(r.resourceSliceToNode),
		).
		Watches(
			&core.Pod{},
			handler.EnqueueRequestsFromMapFunc(r.xpumPodToNode),
		).
		Named("xpumdevicerefresh").
		Complete(r)
}
