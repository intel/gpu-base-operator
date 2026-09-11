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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batch "k8s.io/api/batch/v1"
	core "k8s.io/api/core/v1"
	resv1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	intelv1a1 "github.com/intel/gpu-base-operator/api/v1alpha1"
)

// makeTestJob builds a minimal batch.Job with the given condition pre-set, suitable for creating
// in the envtest API server to drive syncJobStatuses and the deletion paths. The namespace is the
// operator's own, which is where every recovery Job is created.
func makeTestJob(name string, labels map[string]string, condType batch.JobConditionType) *batch.Job {
	return &batch.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Labels:    labels,
		},
		Spec: batch.JobSpec{
			Template: core.PodTemplateSpec{
				Spec: core.PodSpec{
					RestartPolicy: core.RestartPolicyNever,
					Containers: []core.Container{
						{Name: "c", Image: "busybox"},
					},
				},
			},
		},
		Status: batch.JobStatus{
			Conditions: []batch.JobCondition{
				{Type: condType, Status: core.ConditionTrue},
			},
		},
	}
}

// trackingQueue is a minimal workqueue that only records what was Added, so a real
// controller-runtime EventHandler can be driven in a test and its output inspected.
type trackingQueue struct {
	workqueue.TypedRateLimitingInterface[reconcile.Request]

	added []reconcile.Request
}

func (q *trackingQueue) Add(item reconcile.Request) {
	q.added = append(q.added, item)
}

// failingStatusClient makes writes fail on demand so the error paths in persistPlan can be
// driven. Status().Update() always fails; Update() fails only when failUpdate is set, so a spec
// write can still be observed to land after a failed status write.
type failingStatusClient struct {
	client.Client

	failUpdate bool
}

func (c *failingStatusClient) Status() client.SubResourceWriter {
	return &failingSubResourceWriter{SubResourceWriter: c.Client.Status()}
}

func (c *failingStatusClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if c.failUpdate {
		return fmt.Errorf("synthetic spec update failure")
	}

	return c.Client.Update(ctx, obj, opts...)
}

type failingSubResourceWriter struct {
	client.SubResourceWriter
}

func (w *failingSubResourceWriter) Update(
	ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption,
) error {
	return fmt.Errorf("synthetic status update failure")
}

// jobRejectingClient refuses to create a batch Job, the way a cluster whose Job admission webhook
// has no endpoint does.
//
// That is not a contrived failure: on a single-node cluster the drain itself causes it. Evicting the
// node's pods takes the webhook's own pod with them, and from then on every Job creation is refused —
// on a node the drain has already emptied, so nothing about the drain looks wrong.
type jobRejectingClient struct {
	client.Client
}

func (c *jobRejectingClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if _, isJob := obj.(*batch.Job); isJob {
		return fmt.Errorf(`Internal error occurred: failed calling webhook "mjob.kb.io": ` +
			`no endpoints available for service "kueue-webhook-service"`)
	}

	return c.Client.Create(ctx, obj, opts...)
}

// nodeWriteRejectingClient refuses to write a Node, so the drain cannot even cordon what it is
// clearing — an RBAC change or a broken API path, from the drain's point of view.
type nodeWriteRejectingClient struct {
	client.Client
}

func (c *nodeWriteRejectingClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if _, isNode := obj.(*core.Node); isNode {
		return fmt.Errorf("nodes is forbidden: synthetic node write failure")
	}

	return c.Client.Update(ctx, obj, opts...)
}

// recordingClient notes the order in which the status and spec sub-writes reach the API server.
// persistPlan must write status before spec: the spec write (consuming a one-shot approval)
// triggers an immediate new reconcile, and if that reconcile read a stale status it would still
// see the event as waiting-approval and could act twice.
type recordingClient struct {
	client.Client

	writes *[]string
}

func (c *recordingClient) Status() client.SubResourceWriter {
	return &recordingSubResourceWriter{SubResourceWriter: c.Client.Status(), writes: c.writes}
}

func (c *recordingClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	*c.writes = append(*c.writes, "spec")

	return c.Client.Update(ctx, obj, opts...)
}

type recordingSubResourceWriter struct {
	client.SubResourceWriter

	writes *[]string
}

func (w *recordingSubResourceWriter) Update(
	ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption,
) error {
	*w.writes = append(*w.writes, "status")

	return w.SubResourceWriter.Update(ctx, obj, opts...)
}

// createPlanForOwnerRef persists an in-memory plan fixture so it gains a UID, then copies the
// server-assigned metadata back onto the fixture. Recovery Jobs carry a controller reference to
// the plan and the API server rejects an ownerReference with an empty UID, so any spec that drives
// Job creation needs a plan that really exists — which mirrors production, where Reconcile only
// ever works on an object it just Got. Status is preserved: the fixtures set status.events
// directly and rely on it.
func createPlanForOwnerRef(p *intelv1a1.GPURecoveryPlan) {
	status := p.Status

	toCreate := p.DeepCopy()
	Expect(k8sClient.Create(context.Background(), toCreate)).To(Succeed())

	DeferCleanup(func() {
		_ = k8sClient.Delete(context.Background(), toCreate)
	})

	p.ObjectMeta = toCreate.ObjectMeta
	p.Status = status
}

// newTestReconciler builds a GPURecoveryPlanReconciler wired to the shared test client.
//
// The image verifier is a fake that approves everything, because there is no registry here and the
// pre-flight check gates every recovery Job: a real verifier would fail every spec below on an image
// reference that is only ever a string in a fixture. The specs that are about the check itself supply
// their own fake through newTestReconcilerVerifying.
func newTestReconciler() *GPURecoveryPlanReconciler {
	return newTestReconcilerVerifying(&fakeContentImageVerifier{})
}

// newTestReconcilerVerifying builds a reconciler whose pre-flight image check answers as given.
func newTestReconcilerVerifying(v ContentImageVerifier) *GPURecoveryPlanReconciler {
	return &GPURecoveryPlanReconciler{
		Client: k8sClient,
		Scheme: k8sClient.Scheme(),
		Opts: ControllerOpts{
			Namespace:    "default",
			RequeueDelay: 2 * time.Second,
		},
		imgVerify: v,
	}
}

// newOpenShiftTestReconciler is newTestReconciler with OpenShift detection forced on, so the SCC
// paths can be driven without a real OpenShift cluster. envtest loads the SCC CRD from
// internal/controller/testdata/scc-crd.yaml, so the objects are really created and read back.
func newOpenShiftTestReconciler() *GPURecoveryPlanReconciler {
	r := newTestReconciler()
	r.Opts.OpenShift = true

	return r
}

// reconcilePlan runs a single reconcile cycle for the named GPURecoveryPlan.
func reconcilePlan(ctx context.Context, name string) (reconcile.Result, error) {
	return newTestReconciler().Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: name},
	})
}

// reconcilePlanVerifying runs a single reconcile cycle with the given image verifier in place.
func reconcilePlanVerifying(ctx context.Context, name string, v ContentImageVerifier) (reconcile.Result, error) {
	return newTestReconcilerVerifying(v).Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: name},
	})
}

// putTaintedSlice creates or replaces a single-device ResourceSlice carrying the given taint
// keys, and registers its deletion. Detection reads taints off real ResourceSlices, so the
// specs drive it through the API server rather than through a hand-built object.
func putTaintedSlice(ctx context.Context, sliceName, nodeName, devID, bdf string, taintKeys ...string) {
	taints := make([]resv1.DeviceTaint, 0, len(taintKeys))
	for _, k := range taintKeys {
		taints = append(taints, resv1.DeviceTaint{Key: k, Effect: resv1.DeviceTaintEffectNoSchedule})
	}

	slice := &resv1.ResourceSlice{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: sliceName}, slice); err == nil {
		slice.Spec.Devices[0].Taints = taints
		Expect(k8sClient.Update(ctx, slice)).To(Succeed())

		return
	}

	Expect(k8sClient.Create(ctx, &resv1.ResourceSlice{
		ObjectMeta: metav1.ObjectMeta{Name: sliceName},
		Spec: resv1.ResourceSliceSpec{
			Driver:   "gpu.intel.com",
			NodeName: ptr.To(nodeName),
			Pool:     resv1.ResourcePool{Name: sliceName + "-pool", ResourceSliceCount: 1},
			Devices: []resv1.Device{
				{
					Name: "dev-" + sanitizeSegment(bdf),
					Attributes: map[resv1.QualifiedName]resv1.DeviceAttribute{
						deviceAttrDeviceID: {StringValue: ptr.To(devID)},
						deviceAttrBDF:      {StringValue: ptr.To(bdf)},
					},
					Taints: taints,
				},
			},
		},
	})).To(Succeed())

	DeferCleanup(func() {
		stale := &resv1.ResourceSlice{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: sliceName}, stale); err == nil {
			Expect(k8sClient.Delete(ctx, stale)).To(Succeed())
		}
	})
}

// Fixtures shared by the drain specs. They live at package scope because the drain and the
// ResourceClaim specs are in separate files and build the same node, GPU and plan.
const (
	drainNode = "drain-node-1"
	drainBDF  = "0000:31:00.0"
	drainPool = "drain-pool"

	// Workload pods must not live in the operator namespace ("default" here, per
	// newTestReconciler): classifyPodForDrain skips that namespace wholesale, so a fixture
	// pod placed there would be ignored and every drain assertion below would pass for the
	// wrong reason.
	drainWorkloadNS = "drain-workloads"
)

// ensureDrainWorkloadNS creates the namespace the drain fixtures put their workload pods in.
func ensureDrainWorkloadNS(ctx context.Context) {
	ns := &core.Namespace{ObjectMeta: metav1.ObjectMeta{Name: drainWorkloadNS}}
	if err := k8sClient.Create(ctx, ns); err != nil {
		Expect(errors.IsAlreadyExists(err)).To(BeTrue())
	}
}

// makeDrainNode creates the Node the drain operates on. Its taints are cleared before the
// delete so a leftover drain taint cannot follow the name into a later spec.
func makeDrainNode(ctx context.Context) {
	name := drainNode

	node := &core.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
	Expect(k8sClient.Create(ctx, node)).To(Succeed())
	DeferCleanup(func() {
		fresh := &core.Node{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: name}, fresh); err == nil {
			fresh.Spec.Taints = nil
			_ = k8sClient.Update(ctx, fresh)
			_ = k8sClient.Delete(ctx, fresh)
		}
	})
}

// makeDrainSlice publishes a tainted GPU on the node so syncRecoveryEventsFromSlices
// creates an event for it.
func makeDrainSlice(ctx context.Context, name, taintKey string) {
	slice := &resv1.ResourceSlice{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: resv1.ResourceSliceSpec{
			Driver:   gpuDeviceClass,
			NodeName: ptr.To(drainNode),
			Pool:     resv1.ResourcePool{Name: drainPool, ResourceSliceCount: 1},
			Devices: []resv1.Device{{
				Name: "dev-drain-0",
				Attributes: map[resv1.QualifiedName]resv1.DeviceAttribute{
					deviceAttrDeviceID: {StringValue: ptr.To("0x1234")},
					deviceAttrBDF:      {StringValue: ptr.To(drainBDF)},
				},
				Taints: []resv1.DeviceTaint{
					{Key: taintKey, Effect: resv1.DeviceTaintEffectNoSchedule},
				},
			}},
		},
	}
	Expect(k8sClient.Create(ctx, slice)).To(Succeed())
	DeferCleanup(func() {
		_ = k8sClient.Delete(ctx, slice)
	})
}

// makeDrainPlan creates a plan with a blanket approval for the given recovery type, so the
// event is approved on the first reconcile and the drain is what the spec is left
// observing. rt must match what the plan's defaultResetType produces for a reset taint.
func makeDrainPlan(ctx context.Context, name string, rt intelv1a1.RecoveryType) types.NamespacedName {
	p := &intelv1a1.GPURecoveryPlan{
		ObjectMeta: metav1.ObjectMeta{Name: name, Finalizers: []string{recoveryPlanFinalizer}},
		Spec: intelv1a1.GPURecoveryPlanSpec{
			DefaultResetType: intelv1a1.RecoveryTypeSlot,
			DeviceID:         "0x1234",
			XpuSmi:           intelv1a1.XpuSmiSpec{Image: "local/xpusmi:devel"},
			// Spelled out rather than left to CRD defaulting: these specs are about what
			// the drain does, so what it was asked to do belongs in the fixture.
			Drain: intelv1a1.DrainSpec{
				Enable:         ptr.To(true),
				TimeoutSeconds: 300,
			},
			Approvals: []intelv1a1.RecoveryApproval{{
				ID:       "app-drain",
				Selector: &intelv1a1.ApprovalSelector{RecoveryType: rt},
			}},
		},
	}
	Expect(k8sClient.Create(ctx, p)).To(Succeed())
	DeferCleanup(func() {
		fresh := &intelv1a1.GPURecoveryPlan{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: name}, fresh); err == nil {
			fresh.Finalizers = nil
			_ = k8sClient.Update(ctx, fresh)
			_ = k8sClient.Delete(ctx, fresh)
		}
	})

	return types.NamespacedName{Name: name}
}

// makeWorkloadPod puts an evictable pod on the node. A bare pod with no owner is the case
// a drain must actually evict.
func makeWorkloadPod(ctx context.Context, name string) *core.Pod {
	pod := &core.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: drainWorkloadNS},
		Spec: core.PodSpec{
			NodeName:   drainNode,
			Containers: []core.Container{{Name: "c", Image: "busybox"}},
		},
	}
	Expect(k8sClient.Create(ctx, pod)).To(Succeed())
	DeferCleanup(func() {
		_ = k8sClient.Delete(ctx, pod, client.GracePeriodSeconds(0))
	})

	return pod
}

// fetchPlan reads the named plan back, which is how every drain spec inspects what the pass did.
func fetchPlan(ctx context.Context, key types.NamespacedName) *intelv1a1.GPURecoveryPlan {
	updated := &intelv1a1.GPURecoveryPlan{}
	Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())

	return updated
}

// nodeTaints returns the drain node's taints, so a spec can say whether the drain taint is on it.
func nodeTaints(ctx context.Context) []core.Taint {
	node := &core.Node{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: drainNode}, node)).To(Succeed())

	return node.Spec.Taints
}
