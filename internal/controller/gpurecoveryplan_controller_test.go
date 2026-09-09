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
	"strings"
	"time"
	"unicode/utf8"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batch "k8s.io/api/batch/v1"
	core "k8s.io/api/core/v1"
	policy "k8s.io/api/policy/v1"
	rbac "k8s.io/api/rbac/v1"
	resv1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	intelv1a1 "github.com/intel/gpu-base-operator/api/v1alpha1"
	"github.com/intel/gpu-base-operator/config/deployments"
)

// makeTestJob builds a minimal batch.Job with the given condition pre-set, suitable for creating
// in the envtest API server to drive syncJobStatuses and the deletion paths.
func makeTestJob(name, ns string, labels map[string]string, condType batch.JobConditionType) *batch.Job {
	return &batch.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
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

var _ = Describe("GPURecoveryPlan Controller", func() {
	ctx := context.Background()

	const planName = "test-recovery-plan"

	plan := &intelv1a1.GPURecoveryPlan{}
	planKey := types.NamespacedName{Name: planName}

	BeforeEach(func() {
		By("creating the GPURecoveryPlan")
		err := k8sClient.Get(ctx, planKey, plan)
		if errors.IsNotFound(err) {
			resource := &intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{
					Name: planName,
					// Created with the finalizer already on it, as a plan is from its first
					// reconcile onwards. Without it the first reconcile does nothing but add the
					// finalizer, and every spec below would need a throwaway pass before the one
					// it is actually testing.
					Finalizers: []string{recoveryPlanFinalizer},
				},
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DeviceID:         "0x1234",
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
		}
	})

	AfterEach(func() {
		By("deleting the GPURecoveryPlan")
		resource := &intelv1a1.GPURecoveryPlan{}
		if err := k8sClient.Get(ctx, planKey, resource); err == nil {
			// Drop the finalizer first: nothing runs the controller during cleanup, so the object
			// would otherwise sit in Terminating forever and the next spec's Create would fail.
			resource.Finalizers = nil
			Expect(k8sClient.Update(ctx, resource)).To(Succeed())
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		}
	})

	// The whole point of the detection phase: a GPU the DRA driver has tainted shows up in the
	// CR, with everything an admin needs to decide on it, and the plan says it needs attention.
	Context("Reconcile: detection end to end", func() {
		It("should record an event for a tainted GPU and report the plan as active", func() {
			putTaintedSlice(ctx, "slice-e2e", "node-e2e", "0x1234", "0000:02:00.0", deviceTaintKeyReset)

			_, err := reconcilePlan(ctx, planName)
			Expect(err).NotTo(HaveOccurred())

			updated := &intelv1a1.GPURecoveryPlan{}
			Expect(k8sClient.Get(ctx, planKey, updated)).To(Succeed())
			Expect(updated.Status.Events).To(HaveLen(1))

			evt := updated.Status.Events[0]
			Expect(evt.NodeName).To(Equal("node-e2e"))
			Expect(evt.GPUBDF).To(Equal("0000:02:00.0"))
			Expect(evt.Reason).To(Equal(reasonWedged))
			Expect(evt.RecoveryType.Type).To(Equal(intelv1a1.RecoveryTypeSlot))
			Expect(evt.State).To(Equal(intelv1a1.RecoveryEventStateWaitingApproval))
			Expect(evt.LastUpdated).NotTo(BeNil())

			Expect(updated.Status.State).To(Equal(intelv1a1.PlanStateActive))
		})

		It("should ignore a GPU whose device ID the plan does not target", func() {
			putTaintedSlice(ctx, "slice-other-dev", "node-other", "0x9999", "0000:02:00.0", deviceTaintKeyReset)

			_, err := reconcilePlan(ctx, planName)
			Expect(err).NotTo(HaveOccurred())

			updated := &intelv1a1.GPURecoveryPlan{}
			Expect(k8sClient.Get(ctx, planKey, updated)).To(Succeed())
			Expect(updated.Status.Events).To(BeEmpty())
			Expect(updated.Status.State).To(Equal(intelv1a1.PlanStateIdle))
		})

		It("should ignore a taint that is not one the operator recovers from", func() {
			putTaintedSlice(ctx, "slice-other-taint", "node-untainted", "0x1234", "0000:02:00.0",
				"health-SomethingElse")

			_, err := reconcilePlan(ctx, planName)
			Expect(err).NotTo(HaveOccurred())

			updated := &intelv1a1.GPURecoveryPlan{}
			Expect(k8sClient.Get(ctx, planKey, updated)).To(Succeed())
			Expect(updated.Status.Events).To(BeEmpty())
		})

		// Detection is a mirror of the cluster, not a log: an event exists for exactly as long as
		// the taint behind it does. Nothing has been done to the GPU yet, so a cleared taint means
		// something else healed it and the plan must stop asking for approval to reset a healthy card.
		It("should drop the event once the taint clears", func() {
			putTaintedSlice(ctx, "slice-clearing", "node-clearing", "0x1234", "0000:02:00.0",
				deviceTaintKeyReset)

			_, err := reconcilePlan(ctx, planName)
			Expect(err).NotTo(HaveOccurred())

			updated := &intelv1a1.GPURecoveryPlan{}
			Expect(k8sClient.Get(ctx, planKey, updated)).To(Succeed())
			Expect(updated.Status.Events).To(HaveLen(1))

			putTaintedSlice(ctx, "slice-clearing", "node-clearing", "0x1234", "0000:02:00.0")

			_, err = reconcilePlan(ctx, planName)
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, planKey, updated)).To(Succeed())
			Expect(updated.Status.Events).To(BeEmpty())
			Expect(updated.Status.State).To(Equal(intelv1a1.PlanStateIdle))
			Expect(updated.Status.Messages).To(ContainElement(ContainSubstring("taint resolved")))
		})
	})

	Context("Reconcile loop – no ResourceSlices", func() {
		It("should succeed with no events and set state to idle", func() {
			_, err := reconcilePlan(ctx, planName)
			Expect(err).NotTo(HaveOccurred())

			updated := &intelv1a1.GPURecoveryPlan{}
			Expect(k8sClient.Get(ctx, planKey, updated)).To(Succeed())
			Expect(updated.Status.State).To(Equal(intelv1a1.PlanStateIdle))
		})

		It("should not requeue when nothing needs recovering", func() {
			result, err := reconcilePlan(ctx, planName)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero())
		})
	})

	Context("Finalizer management", func() {
		It("should add the finalizer on first reconcile", func() {
			// The shared plan is created with the finalizer already on it, so this uses its own
			// object to exercise the path that puts it there.
			key := types.NamespacedName{Name: "plan-finalizer-add"}
			p := &intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{Name: key.Name},
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot, DeviceID: "0xabcd",
				},
			}
			Expect(k8sClient.Create(ctx, p)).To(Succeed())

			DeferCleanup(func() {
				stale := &intelv1a1.GPURecoveryPlan{}
				if err := k8sClient.Get(ctx, key, stale); err == nil {
					stale.Finalizers = nil
					_ = k8sClient.Update(ctx, stale)
					_ = k8sClient.Delete(ctx, stale)
				}
			})

			_, err := reconcilePlan(ctx, key.Name)
			Expect(err).NotTo(HaveOccurred())

			updated := &intelv1a1.GPURecoveryPlan{}
			Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
			Expect(updated.Finalizers).To(ContainElement(recoveryPlanFinalizer))
		})
	})

	// The finalizer exists to stop a plan deletion from killing a pod that is mid-reset. These
	// specs pin that behaviour: deletion must block while any Job is non-terminal, and must
	// complete once they all are.
	//
	// envtest runs no Job controller, so a Job created without conditions stays non-terminal
	// indefinitely — which is exactly the "in-flight" state being tested.
	Context("Finalizer: deletion blocks on in-flight recovery Jobs", func() {
		// Each spec uses its own plan name: a plan whose finalizer is cleared during cleanup is
		// garbage-collected asynchronously, so reusing one name races the next Create.
		var (
			delPlanName string
			delPlanKey  types.NamespacedName
		)

		// newTerminatingPlan creates a plan carrying the finalizer, deletes it so that a real
		// deletionTimestamp is set by the API server, and returns it still present in etcd.
		newTerminatingPlan := func(name string, events []intelv1a1.RecoveryEvent) *intelv1a1.GPURecoveryPlan {
			delPlanName = name
			delPlanKey = types.NamespacedName{Name: name}

			p := &intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{
					Name:       name,
					Finalizers: []string{recoveryPlanFinalizer},
				},
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot, DeviceID: "0xabcd",
				},
			}
			Expect(k8sClient.Create(ctx, p)).To(Succeed())

			DeferCleanup(func() {
				stale := &intelv1a1.GPURecoveryPlan{}
				if err := k8sClient.Get(ctx, delPlanKey, stale); err == nil {
					stale.Finalizers = nil
					_ = k8sClient.Update(ctx, stale)
				}
			})

			if len(events) > 0 {
				p.Status.Events = events
				Expect(k8sClient.Status().Update(ctx, p)).To(Succeed())
			}

			Expect(k8sClient.Delete(ctx, p)).To(Succeed())

			// The finalizer holds it alive; re-read to pick up the deletionTimestamp.
			Expect(k8sClient.Get(ctx, delPlanKey, p)).To(Succeed())
			Expect(p.DeletionTimestamp.IsZero()).To(BeFalse())

			return p
		}

		// createJob puts a Job for delPlanName in the API server. When terminal is true the status
		// subresource is driven to Failed; otherwise it is left condition-less and so counts as
		// still running.
		createJob := func(name string, terminal bool) *batch.Job {
			job := makeTestJob(name, "default", map[string]string{
				recoveryJobLabelPlan:  delPlanName,
				recoveryJobLabelEvent: "evt-del-001",
			}, batch.JobFailed)
			job.Status = batch.JobStatus{} // status is not settable on create

			Expect(k8sClient.Create(ctx, job)).To(Succeed())

			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, job)
			})

			if terminal {
				// K8s 1.36 requires startTime + FailureTarget=True before Failed=True.
				startTime := metav1.Now()
				job.Status = batch.JobStatus{
					StartTime: &startTime,
					Conditions: []batch.JobCondition{
						{Type: batch.JobFailureTarget, Status: core.ConditionTrue},
						{Type: batch.JobFailed, Status: core.ConditionTrue},
					},
				}
				Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())
			}

			return job
		}

		It("should keep the finalizer and requeue while a Job is still running", func() {
			p := newTerminatingPlan("plan-del-running", []intelv1a1.RecoveryEvent{
				{
					ID:           "evt-del-001",
					NodeName:     "node01",
					GPUBDF:       "0000:02:00.0",
					RecoveryType: intelv1a1.RecoveryTypeSpec{Type: intelv1a1.RecoveryTypeSBR},
					State:        intelv1a1.RecoveryEventStateInProgress,
					JobName:      "recovery-evt-del-001-0",
					LastUpdated:  ptr.To(metav1.Now()),
				},
			})
			Expect(p).NotTo(BeNil())

			createJob("recovery-evt-del-001-0", false)

			result, err := reconcilePlan(ctx, delPlanName)

			// requeue-not-an-error: the caller sees a nil error plus a RequeueAfter.
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(2 * time.Second))

			By("keeping the finalizer so the object is not garbage-collected")
			updated := &intelv1a1.GPURecoveryPlan{}
			Expect(k8sClient.Get(ctx, delPlanKey, updated)).To(Succeed())
			Expect(updated.Finalizers).To(ContainElement(recoveryPlanFinalizer))

			By("not deleting the in-flight Job")
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      "recovery-evt-del-001-0",
				Namespace: "default",
			}, &batch.Job{})).To(Succeed(), "an in-flight reset Job must not be killed by plan deletion")

			By("recording why deletion is waiting")
			Expect(updated.Status.Messages).To(ContainElement(
				ContainSubstring("Deletion waiting for 1 active recovery Job(s)")))
		})

		It("should remove the finalizer and delete Jobs once all are terminal", func() {
			p := newTerminatingPlan("plan-del-terminal", []intelv1a1.RecoveryEvent{
				{
					ID:           "evt-del-001",
					NodeName:     "node01",
					GPUBDF:       "0000:02:00.0",
					RecoveryType: intelv1a1.RecoveryTypeSpec{Type: intelv1a1.RecoveryTypeSBR},
					State:        intelv1a1.RecoveryEventStateFailed,
					PastJobs:     []string{"recovery-evt-del-002-0"},
					LastUpdated:  ptr.To(metav1.Now()),
				},
			})
			Expect(p).NotTo(BeNil())

			createJob("recovery-evt-del-002-0", true)

			_, err := reconcilePlan(ctx, delPlanName)
			Expect(err).NotTo(HaveOccurred())

			By("letting the object go away")
			Eventually(func() bool {
				return errors.IsNotFound(k8sClient.Get(ctx, delPlanKey, &intelv1a1.GPURecoveryPlan{}))
			}, 5*time.Second, 100*time.Millisecond).Should(BeTrue())

			By("cleaning up the terminal Job")
			Eventually(func() bool {
				return errors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{
					Name:      "recovery-evt-del-002-0",
					Namespace: "default",
				}, &batch.Job{}))
			}, 5*time.Second, 100*time.Millisecond).Should(BeTrue())
		})

		It("should delete a terminal labelled Job that no event references", func() {
			// Complements the blocking case: once status.events has been pruned, the label is the
			// only handle left on the Job, so cleanup must not rely on the events walk or the Job
			// would leak after the plan is gone.
			p := newTerminatingPlan("plan-del-orphan-cleanup", nil)
			Expect(p.Status.Events).To(BeEmpty())

			createJob("recovery-orphan-terminal-0", true)

			_, err := reconcilePlan(ctx, delPlanName)
			Expect(err).NotTo(HaveOccurred())

			Eventually(func() bool {
				return errors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{
					Name:      "recovery-orphan-terminal-0",
					Namespace: "default",
				}, &batch.Job{}))
			}, 5*time.Second, 100*time.Millisecond).Should(BeTrue(),
				"a Job with no event entry must still be cleaned up via its plan label")
		})

		It("should block on a labelled Job even when no event references it", func() {
			// A Job whose event entry was already pruned from status must still be waited for —
			// otherwise pruning status would silently unblock a live reset.
			p := newTerminatingPlan("plan-del-orphan", nil)
			Expect(p.Status.Events).To(BeEmpty())

			createJob("recovery-orphan-0", false)

			result, err := reconcilePlan(ctx, delPlanName)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(2 * time.Second))

			updated := &intelv1a1.GPURecoveryPlan{}
			Expect(k8sClient.Get(ctx, delPlanKey, updated)).To(Succeed())
			Expect(updated.Finalizers).To(ContainElement(recoveryPlanFinalizer))
		})

		It("should not wait for a Job that is already terminating", func() {
			r := newTestReconciler()
			delPlanName = "plan-del-terminating"
			p := &intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{Name: delPlanName},
			}

			// A Job finalizer keeps the object readable after Delete, so it is observable in the
			// Terminating state that runningRecoveryJobs must skip.
			job := makeTestJob("recovery-terminating-0", "default", map[string]string{
				recoveryJobLabelPlan: delPlanName,
			}, batch.JobFailed)
			job.Status = batch.JobStatus{}
			job.Finalizers = []string{"test.intel.com/hold"}

			Expect(k8sClient.Create(ctx, job)).To(Succeed())

			DeferCleanup(func() {
				stale := &batch.Job{}
				if err := k8sClient.Get(ctx, types.NamespacedName{
					Name: "recovery-terminating-0", Namespace: "default",
				}, stale); err == nil {
					stale.Finalizers = nil
					_ = k8sClient.Update(ctx, stale)
				}
			})

			By("confirming it counts as running before deletion")
			running, err := runningRecoveryJobs(r.Client, ctx, r.Opts.Namespace, p)
			Expect(err).NotTo(HaveOccurred())
			Expect(running).To(ConsistOf("recovery-terminating-0"))

			Expect(k8sClient.Delete(ctx, job)).To(Succeed())

			By("no longer counting it once a deletionTimestamp is set")
			Eventually(func() []string {
				running, err := runningRecoveryJobs(r.Client, ctx, r.Opts.Namespace, p)
				Expect(err).NotTo(HaveOccurred())

				return running
			}, 5*time.Second, 100*time.Millisecond).Should(BeEmpty())
		})
	})

	Context("Helper: jobIsTerminal", func() {
		cond := func(t batch.JobConditionType, s core.ConditionStatus) batch.JobCondition {
			return batch.JobCondition{Type: t, Status: s}
		}
		jobWith := func(conds ...batch.JobCondition) *batch.Job {
			return &batch.Job{Status: batch.JobStatus{Conditions: conds}}
		}

		It("should treat a Job with no conditions as running", func() {
			Expect(jobIsTerminal(jobWith())).To(BeFalse())
		})

		It("should treat Complete=True as terminal", func() {
			Expect(jobIsTerminal(jobWith(cond(batch.JobComplete, core.ConditionTrue)))).To(BeTrue())
		})

		It("should treat Failed=True as terminal", func() {
			Expect(jobIsTerminal(jobWith(cond(batch.JobFailed, core.ConditionTrue)))).To(BeTrue())
		})

		It("should not treat Complete=False as terminal", func() {
			Expect(jobIsTerminal(jobWith(cond(batch.JobComplete, core.ConditionFalse)))).To(BeFalse())
		})

		It("should not treat intermediate conditions as terminal", func() {
			// SuccessCriteriaMet/FailureTarget precede the real terminal condition; acting on them
			// would cut a Job's pod off before it has actually finished.
			Expect(jobIsTerminal(jobWith(
				cond(batch.JobSuspended, core.ConditionTrue),
				cond(batch.JobSuccessCriteriaMet, core.ConditionTrue),
				cond(batch.JobFailureTarget, core.ConditionTrue),
			))).To(BeFalse())
		})
	})

	Context("Reconcile with non-existent plan", func() {
		It("should return no error for a missing plan", func() {
			_, err := reconcilePlan(ctx, "does-not-exist")
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("ResourceSlice → plan mapping", func() {
		const sliceName = "test-slice"

		AfterEach(func() {
			slice := &resv1.ResourceSlice{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: sliceName}, slice); err == nil {
				Expect(k8sClient.Delete(ctx, slice)).To(Succeed())
			}
		})

		It("should map a matching ResourceSlice to the plan", func() {
			slice := &resv1.ResourceSlice{
				ObjectMeta: metav1.ObjectMeta{Name: sliceName},
				Spec: resv1.ResourceSliceSpec{
					Driver:   "gpu.intel.com",
					NodeName: ptr.To("node01"),
					Pool:     resv1.ResourcePool{Name: "pool01", ResourceSliceCount: 1},
					Devices: []resv1.Device{
						{
							Name: "dev-0000-02-00-0",
							Attributes: map[resv1.QualifiedName]resv1.DeviceAttribute{
								deviceAttrDeviceID: {StringValue: ptr.To("0x1234")},
								deviceAttrBDF:      {StringValue: ptr.To("0000:02:00.0")},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, slice)).To(Succeed())

			r := newTestReconciler()
			reqs := r.resourceSliceToPlans(ctx, slice)
			Expect(reqs).To(HaveLen(1))
			Expect(reqs[0].Name).To(Equal(planName))

			// A cluster-scoped owner must yield a namespace-less request, or the Get in Reconcile
			// would target "default/test-recovery-plan" and silently never match.
			Expect(reqs[0].Namespace).To(BeEmpty())
		})

		It("should return no requests for a slice with a different deviceId", func() {
			slice := &resv1.ResourceSlice{
				ObjectMeta: metav1.ObjectMeta{Name: sliceName},
				Spec: resv1.ResourceSliceSpec{
					Driver:   "gpu.intel.com",
					NodeName: ptr.To("node02"),
					Pool:     resv1.ResourcePool{Name: "pool02", ResourceSliceCount: 1},
					Devices: []resv1.Device{
						{
							Name: "dev-0000-03-00-0",
							Attributes: map[resv1.QualifiedName]resv1.DeviceAttribute{
								deviceAttrDeviceID: {StringValue: ptr.To("0x9999")},
								deviceAttrBDF:      {StringValue: ptr.To("0000:03:00.0")},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, slice)).To(Succeed())

			r := newTestReconciler()
			reqs := r.resourceSliceToPlans(ctx, slice)
			Expect(reqs).To(BeEmpty())
		})

		It("should return no requests for a slice with no device attributes", func() {
			slice := &resv1.ResourceSlice{
				ObjectMeta: metav1.ObjectMeta{Name: sliceName},
				Spec: resv1.ResourceSliceSpec{
					Driver:   "gpu.intel.com",
					NodeName: ptr.To("node03"),
					Pool:     resv1.ResourcePool{Name: "pool03", ResourceSliceCount: 1},
					Devices:  []resv1.Device{{Name: "dev-bare"}},
				},
			}
			Expect(k8sClient.Create(ctx, slice)).To(Succeed())

			r := newTestReconciler()
			reqs := r.resourceSliceToPlans(ctx, slice)
			Expect(reqs).To(BeEmpty())
		})
	})

	// A device with no pciAddress cannot be named in an event, let alone recovered, so it is
	// skipped rather than allowed to produce an event whose GPUBDF is empty — that would collide
	// with every other attribute-less device on the node under findEventForDevice.
	Context("syncRecoveryEventsFromSlices: devices without a BDF", func() {
		It("should skip a tainted device that has no pciAddress attribute", func() {
			slice := &resv1.ResourceSlice{
				ObjectMeta: metav1.ObjectMeta{Name: "slice-no-bdf"},
				Spec: resv1.ResourceSliceSpec{
					Driver:   "gpu.intel.com",
					NodeName: ptr.To("node-no-bdf"),
					Pool:     resv1.ResourcePool{Name: "pool-no-bdf", ResourceSliceCount: 1},
					Devices: []resv1.Device{{
						Name: "dev-no-bdf",
						Attributes: map[resv1.QualifiedName]resv1.DeviceAttribute{
							deviceAttrDeviceID: {StringValue: ptr.To("0xabcd")},
						},
						Taints: []resv1.DeviceTaint{
							{Key: deviceTaintKeyReset, Effect: resv1.DeviceTaintEffectNoSchedule},
						},
					}},
				},
			}
			Expect(k8sClient.Create(ctx, slice)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, slice)
			})

			r := newTestReconciler()
			p := &intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{Name: "plan-no-bdf"},
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DeviceID: "0xabcd", DefaultResetType: intelv1a1.RecoveryTypeSlot,
				},
			}

			Expect(r.syncRecoveryEventsFromSlices(ctx, p)).To(Succeed())
			Expect(p.Status.Events).To(BeEmpty())
		})

		// A BDF that is not a PCI address is dropped at the same point, and for a sharper reason:
		// it would otherwise be interpolated into the command line of a privileged root container.
		// Detection is the only place the value enters the plan, so this is what lets both command
		// builders interpolate it without escaping.
		It("should skip a tainted device whose pciAddress is not a PCI address", func() {
			slice := &resv1.ResourceSlice{
				ObjectMeta: metav1.ObjectMeta{Name: "slice-bad-bdf"},
				Spec: resv1.ResourceSliceSpec{
					Driver:   "gpu.intel.com",
					NodeName: ptr.To("node-bad-bdf"),
					Pool:     resv1.ResourcePool{Name: "pool-bad-bdf", ResourceSliceCount: 1},
					Devices: []resv1.Device{{
						Name: "dev-bad-bdf",
						Attributes: map[resv1.QualifiedName]resv1.DeviceAttribute{
							deviceAttrDeviceID: {StringValue: ptr.To("0xabcd")},
							deviceAttrBDF:      {StringValue: ptr.To("0000:02:00.0; touch /tmp/pwned")},
						},
						Taints: []resv1.DeviceTaint{
							{Key: deviceTaintKeyReset, Effect: resv1.DeviceTaintEffectNoSchedule},
						},
					}},
				},
			}
			Expect(k8sClient.Create(ctx, slice)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, slice)
			})

			r := newTestReconciler()
			p := &intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{Name: "plan-bad-bdf"},
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DeviceID: "0xabcd", DefaultResetType: intelv1a1.RecoveryTypeSlot,
				},
			}

			Expect(r.syncRecoveryEventsFromSlices(ctx, p)).To(Succeed())
			Expect(p.Status.Events).To(BeEmpty())
		})
	})

	Context("Helper: taintToDeviceNeed", func() {
		It("should map a reset taint key to the plan's default reset type", func() {
			need, ok := taintToDeviceNeed(deviceTaintKeyReset, intelv1a1.RecoveryTypeSlot)
			Expect(ok).To(BeTrue())
			Expect(need.rt).To(Equal(intelv1a1.RecoveryTypeSlot))
			Expect(need.reason).To(Equal(reasonWedged))

			// The DRA driver cannot say which reset mechanism applies, so on a platform
			// without PCIe hot-plug the only thing that can select AMC is this field.
			need, ok = taintToDeviceNeed(deviceTaintKeyReset, intelv1a1.RecoveryTypeAMC)
			Expect(ok).To(BeTrue())
			Expect(need.rt).To(Equal(intelv1a1.RecoveryTypeAMC))
			Expect(need.reason).To(Equal(reasonWedged),
				"the mechanism changed, not the fault; the reason must still read gpu-wedged")
		})

		// The field is mandatory, so an empty value means an object that never reached the API
		// server. Falling back matters because an empty type produces a malformed event ID. SBR
		// rather than slot or amc: guessing between those two is guessing at the platform.
		It("should fall back to SBR when the plan carries no default reset type", func() {
			need, ok := taintToDeviceNeed(deviceTaintKeyReset, "")
			Expect(ok).To(BeTrue())
			Expect(need.rt).To(Equal(intelv1a1.RecoveryTypeSBR))
		})

		DescribeTable("should map a survivability taint key to Reflash",
			func(taintKey string) {
				need, ok := taintToDeviceNeed(taintKey, intelv1a1.RecoveryTypeSlot)
				Expect(ok).To(BeTrue())
				Expect(need.rt).To(Equal(intelv1a1.RecoveryTypeReflash))
				Expect(need.reason).To(Equal(reasonSurvivability))
			},
			Entry("applied by the driver at enumeration", deviceTaintKeyReflash),
			Entry("applied on xpumd's behalf at runtime", deviceTaintKeyXpumdReflash),
		)

		// A reflash need comes from the device's own state, not from the platform's reset
		// mechanism, so the default must not reach it.
		It("should not let the default reset type influence a reflash taint", func() {
			need, ok := taintToDeviceNeed(deviceTaintKeyXpumdReflash, intelv1a1.RecoveryTypeAMC)
			Expect(ok).To(BeTrue())
			Expect(need.rt).To(Equal(intelv1a1.RecoveryTypeReflash))
		})

		It("should return false for an unrelated taint key", func() {
			_, ok := taintToDeviceNeed("some.other/taint", intelv1a1.RecoveryTypeSlot)
			Expect(ok).To(BeFalse())
		})

		It("should prioritize survivability over wedged when both taints are present", func() {
			wedged := deviceNeed{rt: intelv1a1.RecoveryTypeSBR, reason: reasonWedged}
			surv := deviceNeed{rt: intelv1a1.RecoveryTypeReflash, reason: reasonSurvivability}

			// The reason must travel with the type, or the event would report the reflash
			// alongside "gpu-wedged".
			Expect(higherPriorityNeed(wedged, surv)).To(Equal(surv))
			Expect(higherPriorityNeed(surv, wedged)).To(Equal(surv))
		})

		It("should prioritize any known over empty", func() {
			known := deviceNeed{rt: intelv1a1.RecoveryTypeSBR, reason: reasonWedged}
			empty := deviceNeed{}

			Expect(higherPriorityNeed(known, empty)).To(Equal(known))
			Expect(higherPriorityNeed(empty, known)).To(Equal(known))
		})
	})

	// Which reset works is a property of the platform (hot-plug capable slots → the slot power
	// cycle, otherwise AMC), and the DRA driver only reports "needs a reset".
	// spec.defaultResetType is the only thing that can tell the two apart.
	Context("spec.defaultResetType", func() {
		const (
			drtSlice = "drt-slice"
			drtNode  = "node-drt"
			drtBDF   = "0000:04:00.0"
			drtDevID = "0xabcd"
		)

		putDrtSlice := func(taintKeys ...string) {
			putTaintedSlice(ctx, drtSlice, drtNode, drtDevID, drtBDF, taintKeys...)
		}

		newDrtPlan := func(name string, rt intelv1a1.RecoveryType) *intelv1a1.GPURecoveryPlan {
			return &intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{Name: name},
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DeviceID:         drtDevID,
					MaxRetries:       3,
					DefaultResetType: rt,
				},
			}
		}

		DescribeTable("should type a new reset event from the plan's default",
			func(rt intelv1a1.RecoveryType) {
				r := newTestReconciler()
				p := newDrtPlan("plan-drt-"+string(rt), rt)

				putDrtSlice(deviceTaintKeyReset)
				Expect(r.syncRecoveryEventsFromSlices(ctx, p)).To(Succeed())

				Expect(p.Status.Events).To(HaveLen(1))
				evt := p.Status.Events[0]
				Expect(evt.RecoveryType.Type).To(Equal(rt))
				Expect(evt.ID).To(ContainSubstring("-"+string(rt)+"-"),
					"the type is part of the ID, so an admin approving by name can see what will run")
			},
			Entry("slot, where the PCIe slots support hot-plug", intelv1a1.RecoveryTypeSlot),
			Entry("amc, where they do not", intelv1a1.RecoveryTypeAMC),
		)

		// The field is a default for events created afterwards, not a retroactive rewrite. An
		// event that is already waiting for approval keeps its type, because changing it
		// regenerates the ID and thereby invalidates any approval naming the old one — a
		// correction to the plan must not silently discard an approval an admin already gave.
		It("should leave a waiting-approval slot event alone when the default becomes amc", func() {
			r := newTestReconciler()
			p := newDrtPlan("plan-drt-slot-to-amc", intelv1a1.RecoveryTypeSlot)

			putDrtSlice(deviceTaintKeyReset)
			Expect(r.syncRecoveryEventsFromSlices(ctx, p)).To(Succeed())
			Expect(p.Status.Events).To(HaveLen(1))
			origID := p.Status.Events[0].ID

			p.Spec.DefaultResetType = intelv1a1.RecoveryTypeAMC
			Expect(r.syncRecoveryEventsFromSlices(ctx, p)).To(Succeed())

			Expect(p.Status.Events).To(HaveLen(1))
			Expect(p.Status.Events[0].RecoveryType.Type).To(Equal(intelv1a1.RecoveryTypeSlot))
			Expect(p.Status.Events[0].ID).To(Equal(origID))
		})

		// The mirror image of the spec above, and the one that constrains recoveryTypePriority:
		// escalateEvent re-types any event whose need outranks it, so if the resets were ranked
		// against each other a default change would silently re-type waiting events and void their
		// approvals. The resets are alternatives selected by platform, not a severity ladder.
		It("should leave a waiting-approval amc event alone when the default becomes slot", func() {
			r := newTestReconciler()
			p := newDrtPlan("plan-drt-amc-to-slot", intelv1a1.RecoveryTypeAMC)

			putDrtSlice(deviceTaintKeyReset)
			Expect(r.syncRecoveryEventsFromSlices(ctx, p)).To(Succeed())
			Expect(p.Status.Events).To(HaveLen(1))
			origID := p.Status.Events[0].ID

			p.Spec.DefaultResetType = intelv1a1.RecoveryTypeSlot
			Expect(r.syncRecoveryEventsFromSlices(ctx, p)).To(Succeed())

			Expect(p.Status.Events).To(HaveLen(1))
			Expect(p.Status.Events[0].RecoveryType.Type).To(Equal(intelv1a1.RecoveryTypeAMC),
				"a default that only applies to new events must not re-type one that is already waiting")
			Expect(p.Status.Events[0].ID).To(Equal(origID),
				"a regenerated ID would invalidate an approval the admin already granted")
		})

		// Reflash still has to outrank a reset, whichever reset the platform uses: a card in
		// survivability mode also reports wedged, and no reset revives it.
		It("should still escalate a reset event to reflash on an amc plan", func() {
			r := newTestReconciler()
			p := newDrtPlan("plan-drt-amc-escalate", intelv1a1.RecoveryTypeAMC)

			putDrtSlice(deviceTaintKeyReset)
			Expect(r.syncRecoveryEventsFromSlices(ctx, p)).To(Succeed())
			Expect(p.Status.Events[0].RecoveryType.Type).To(Equal(intelv1a1.RecoveryTypeAMC))

			putDrtSlice(deviceTaintKeyReset, deviceTaintKeyXpumdReflash)
			Expect(r.syncRecoveryEventsFromSlices(ctx, p)).To(Succeed())

			Expect(p.Status.Events).To(HaveLen(1))
			Expect(p.Status.Events[0].RecoveryType.Type).To(Equal(intelv1a1.RecoveryTypeReflash))
			Expect(p.Status.Events[0].Reason).To(Equal(reasonSurvivability))
		})

		It("should rank the three resets equally and only reflash above them", func() {
			base := recoveryTypePriority(intelv1a1.RecoveryTypeSBR)
			Expect(recoveryTypePriority(intelv1a1.RecoveryTypeSlot)).To(Equal(base))
			Expect(recoveryTypePriority(intelv1a1.RecoveryTypeAMC)).To(Equal(base))
			Expect(recoveryTypePriority(intelv1a1.RecoveryTypeReflash)).To(BeNumerically(">", base))
			Expect(recoveryTypePriority(intelv1a1.RecoveryType("flr"))).To(BeNumerically("<", base),
				"a type outside the enum must not outrank a real one")
		})
	})

	Context("persistPlan: status and spec write-back", func() {
		// The plan carries one consumable approval, so the spec half of persistPlan has something
		// real to write: consuming an approval is the only spec change the operator ever makes.
		newPersistPlan := func(name string) (*GPURecoveryPlanReconciler, *intelv1a1.GPURecoveryPlan) {
			p := &intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{Name: name},
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					DeviceID:         "0xabcd",
					Approvals: []intelv1a1.RecoveryApproval{
						{ID: "app-persist", EventID: "evt-persist-1"},
					},
				},
			}
			Expect(k8sClient.Create(ctx, p)).To(Succeed())

			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, p)
			})

			return newTestReconciler(), p
		}

		It("should write status changes back to the API server", func() {
			r, p := newPersistPlan("plan-persist-status")
			key := types.NamespacedName{Name: p.Name}
			orig := p.DeepCopy()

			p.Status.Messages = []string{"hello"}

			Expect(r.persistPlan(ctx, key, orig, p)).To(Succeed())

			got := &intelv1a1.GPURecoveryPlan{}
			Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
			Expect(got.Status.Messages).To(Equal([]string{"hello"}))
		})

		It("should report a failed status write rather than swallowing it", func() {
			r, p := newPersistPlan("plan-persist-fails")
			key := types.NamespacedName{Name: p.Name}
			orig := p.DeepCopy()

			r.Client = &failingStatusClient{Client: r.Client}

			p.Status.Messages = []string{"dropped"}

			err := r.persistPlan(ctx, key, orig, p)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("updating status"))
		})

		It("should write both status and spec changes back to the API server", func() {
			r, p := newPersistPlan("plan-persist-both")
			key := types.NamespacedName{Name: p.Name}
			orig := p.DeepCopy()

			p.Status.Messages = []string{"hello"}
			p.Spec.Approvals[0].Consumed = true

			Expect(r.persistPlan(ctx, key, orig, p)).To(Succeed())

			got := &intelv1a1.GPURecoveryPlan{}
			Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
			Expect(got.Status.Messages).To(Equal([]string{"hello"}))
			Expect(got.Spec.Approvals[0].Consumed).To(BeTrue())
		})

		It("should still write the spec when the status write fails", func() {
			// Returning early on a status-update error would silently drop consumed=true. That
			// lets the approval match again and reset the same GPU a second time — status is
			// recomputable on the next pass, consumed is not.
			r, p := newPersistPlan("plan-persist-status-fails")
			key := types.NamespacedName{Name: p.Name}
			orig := p.DeepCopy()

			r.Client = &failingStatusClient{Client: r.Client}

			p.Status.Messages = []string{"dropped"}
			p.Spec.Approvals[0].Consumed = true

			err := r.persistPlan(ctx, key, orig, p)
			Expect(err).To(HaveOccurred(), "the status failure must be reported, not swallowed")
			Expect(err.Error()).To(ContainSubstring("updating status"))

			got := &intelv1a1.GPURecoveryPlan{}
			Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
			Expect(got.Spec.Approvals[0].Consumed).To(BeTrue(),
				"consumed=true must survive a failed status write")
		})

		It("should write status before spec", func() {
			// The spec write consumes the approval and immediately triggers another reconcile; that
			// reconcile must not observe a status still describing the event as waiting-approval,
			// or it can act on it a second time.
			r, p := newPersistPlan("plan-persist-order")
			key := types.NamespacedName{Name: p.Name}
			orig := p.DeepCopy()

			writes := []string{}
			r.Client = &recordingClient{Client: r.Client, writes: &writes}

			p.Status.Messages = []string{"hello"}
			p.Spec.Approvals[0].Consumed = true

			Expect(r.persistPlan(ctx, key, orig, p)).To(Succeed())
			Expect(writes).To(Equal([]string{"status", "spec"}))
		})

		It("should recover from a conflict by retrying against the current object", func() {
			// Simulates the routine case: something else updated the plan between our
			// Get and our write, so our cached resourceVersion is stale.
			r, p := newPersistPlan("plan-persist-conflict")
			key := types.NamespacedName{Name: p.Name}
			orig := p.DeepCopy()

			// Bump the object's resourceVersion behind our back.
			other := &intelv1a1.GPURecoveryPlan{}
			Expect(k8sClient.Get(ctx, key, other)).To(Succeed())
			other.Labels = map[string]string{"touched": "yes"}
			Expect(k8sClient.Update(ctx, other)).To(Succeed())

			// p still holds the pre-update resourceVersion, so a naive write conflicts.
			p.Status.Messages = []string{"after-conflict"}

			Expect(r.persistPlan(ctx, key, orig, p)).To(Succeed())

			got := &intelv1a1.GPURecoveryPlan{}
			Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
			Expect(got.Status.Messages).To(Equal([]string{"after-conflict"}))
			Expect(got.Labels).To(HaveKeyWithValue("touched", "yes"),
				"the concurrent change must be preserved, not clobbered")
		})

		It("should do nothing when neither status nor spec changed", func() {
			r, p := newPersistPlan("plan-persist-noop")
			key := types.NamespacedName{Name: p.Name}

			r.Client = &failingStatusClient{Client: r.Client, failUpdate: true}

			// orig == p, so there is nothing to write and neither failing path is hit.
			Expect(r.persistPlan(ctx, key, p.DeepCopy(), p)).To(Succeed())
		})
	})

	Context("Reconcile: write failures are surfaced", func() {
		It("should return an error when persistPlan cannot write", func() {
			// A write error that is only logged produces a successful reconcile and no retry, so
			// a detected GPU would stay invisible in the CR until something else triggered a
			// reconcile. Only Status().Update is made to fail, and the phases dirty status
			// (state "" -> idle), so the error can only originate in persistPlan.
			p := &intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "plan-reconcile-writefail",
					Finalizers: []string{recoveryPlanFinalizer},
				},
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot, DeviceID: "0xabcd",
				},
			}
			Expect(k8sClient.Create(ctx, p)).To(Succeed())

			DeferCleanup(func() {
				stale := &intelv1a1.GPURecoveryPlan{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: p.Name}, stale); err == nil {
					stale.Finalizers = nil
					_ = k8sClient.Update(ctx, stale)
					_ = k8sClient.Delete(ctx, stale)
				}
			})

			r := newTestReconciler()
			r.Client = &failingStatusClient{Client: r.Client}

			_, err := r.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: p.Name},
			})
			Expect(err).To(HaveOccurred(), "a failed write must fail the reconcile so it is retried")
			Expect(err.Error()).To(ContainSubstring("updating status"),
				"the error must come from persistPlan, not from an earlier phase")
		})
	})

	Context("Helper: appendMessage", func() {
		It("should cap messages at maxStatusMessages", func() {
			p := &intelv1a1.GPURecoveryPlan{}

			for i := 0; i < maxStatusMessages+10; i++ {
				appendMessage(p, fmt.Sprintf("msg - %d", i))
			}

			Expect(p.Status.Messages).To(HaveLen(maxStatusMessages))
		})
	})

	// The DRA driver adds the survivability taint alongside the wedged one on a GPU that already
	// has a pending reset event. Driven through syncRecoveryEventsFromSlices with real
	// ResourceSlices rather than by calling escalateEvent directly, because the failure mode is
	// the detection loop skipping such devices outright — a unit test of the escalation helper
	// alone would not catch it.
	Context("syncRecoveryEventsFromSlices: taint escalation", func() {
		const (
			escSlice = "esc-slice"
			escPlan  = "plan-escalation"
			escNode  = "node01"
			escBDF   = "0000:02:00.0"
		)

		putSlice := func(taintKeys ...string) {
			putTaintedSlice(ctx, escSlice, escNode, "0xabcd", escBDF, taintKeys...)
		}

		It("should escalate an existing reset event when the survivability taint appears", func() {
			r := newTestReconciler()
			p := &intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{Name: escPlan},
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot, DeviceID: "0xabcd", MaxRetries: 3,
				},
			}

			// Round 1: wedged only → one event of the plan's default reset type.
			putSlice(deviceTaintKeyReset)
			Expect(r.syncRecoveryEventsFromSlices(ctx, p)).To(Succeed())
			Expect(p.Status.Events).To(HaveLen(1))
			Expect(p.Status.Events[0].RecoveryType.Type).To(Equal(intelv1a1.RecoveryTypeSlot))
			Expect(p.Status.Events[0].Reason).To(Equal(reasonWedged))

			// Round 2: the driver adds survivability alongside wedged.
			putSlice(deviceTaintKeyReset, deviceTaintKeyXpumdReflash)
			Expect(r.syncRecoveryEventsFromSlices(ctx, p)).To(Succeed())

			Expect(p.Status.Events).To(HaveLen(1),
				"escalation must happen in place; two events for one GPU would race two recoveries on it")
			Expect(p.Status.Events[0].RecoveryType.Type).To(Equal(intelv1a1.RecoveryTypeReflash))
			Expect(p.Status.Events[0].State).To(Equal(intelv1a1.RecoveryEventStateWaitingApproval))
			Expect(p.Status.Events[0].Reason).To(Equal(reasonSurvivability),
				"the cause must be restated too; a reflash reported as gpu-wedged misleads whoever approves it")
		})

		It("should leave a steady-state event untouched", func() {
			r := newTestReconciler()
			p := &intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{Name: escPlan + "-steady"},
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot, DeviceID: "0xabcd", MaxRetries: 3,
				},
			}

			putSlice(deviceTaintKeyReset)
			Expect(r.syncRecoveryEventsFromSlices(ctx, p)).To(Succeed())
			afterFirst := p.DeepCopy()

			Expect(r.syncRecoveryEventsFromSlices(ctx, p)).To(Succeed())
			Expect(p.Status).To(Equal(afterFirst.Status),
				"an unchanged taint set must not rewrite status on every reconcile")
		})
	})

	Context("Helper: escalateEvent", func() {
		// waitingEvent builds a single-device event of the given type, waiting for approval, with
		// approval bookkeeping and a spent retry budget already present so escalation can be seen
		// to clear them.
		waitingEvent := func(rt intelv1a1.RecoveryType) *intelv1a1.RecoveryEvent {
			now := metav1.Now()

			return &intelv1a1.RecoveryEvent{
				ID:                generateEventID("node01", "0000:02:00.0", rt),
				NodeName:          "node01",
				GPUBDF:            "0000:02:00.0",
				Reason:            reasonWedged,
				RecoveryType:      intelv1a1.RecoveryTypeSpec{Type: rt},
				State:             intelv1a1.RecoveryEventStateWaitingApproval,
				RetryCount:        2,
				ApprovalID:        "app-old",
				ApprovalMatchedAt: ptr.To(now),
				LastUpdated:       ptr.To(now),
			}
		}

		It("should escalate a waiting-approval slot event to reflash", func() {
			p := &intelv1a1.GPURecoveryPlan{ObjectMeta: metav1.ObjectMeta{Name: "plan-esc"}}
			evt := waitingEvent(intelv1a1.RecoveryTypeSlot)

			escalateEvent(p, evt, deviceNeed{rt: intelv1a1.RecoveryTypeReflash, reason: reasonSurvivability})

			Expect(evt.RecoveryType.Type).To(Equal(intelv1a1.RecoveryTypeReflash))
			Expect(evt.ID).To(Equal("evt-node01-reflash-0000-02-00-0"))
			Expect(evt.Reason).To(Equal(reasonSurvivability),
				"status.events[].reason is what an admin reads before approving; it must track the escalation")
			Expect(p.Status.Messages).To(ContainElement(ContainSubstring("escalated")))
		})

		// The new ID is what stops a reset approval from silently authorising a reflash, so it is
		// asserted as behaviour rather than left implicit in the ID format.
		It("should invalidate an approval naming the pre-escalation event", func() {
			p := &intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{Name: "plan-esc-approval"},
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					Approvals: []intelv1a1.RecoveryApproval{
						{ID: "app-slot", EventID: "evt-node01-slot-0000-02-00-0"},
					},
				},
			}
			evt := waitingEvent(intelv1a1.RecoveryTypeSlot)

			// The approval names this event before escalation...
			Expect(evt.ID).To(Equal(p.Spec.Approvals[0].EventID))

			escalateEvent(p, evt, deviceNeed{rt: intelv1a1.RecoveryTypeReflash, reason: reasonSurvivability})

			// ...and must not after, or approving a reset would run a reflash.
			Expect(evt.ID).NotTo(Equal(p.Spec.Approvals[0].EventID),
				"a slot-reset approval must not carry over to the escalated reflash event")
		})

		It("should reset the approval state and retry budget", func() {
			p := &intelv1a1.GPURecoveryPlan{ObjectMeta: metav1.ObjectMeta{Name: "plan-esc-reset"}}
			evt := waitingEvent(intelv1a1.RecoveryTypeSlot)

			escalateEvent(p, evt, deviceNeed{rt: intelv1a1.RecoveryTypeReflash, reason: reasonSurvivability})

			Expect(evt.State).To(Equal(intelv1a1.RecoveryEventStateWaitingApproval))
			Expect(evt.RetryCount).To(BeZero(), "the escalated operation gets its own retry budget")
			Expect(evt.ApprovalID).To(BeEmpty())
			Expect(evt.ApprovalMatchedAt).To(BeNil())
		})

		// A reset that could not pull xpu-smi says nothing about the firmware image the escalated
		// reflash also needs. Carrying the recorded generation across would suppress the check on an
		// image that has never been looked at, and the reflash would run — or not — on a guess.
		It("should re-check the images of the escalated operation", func() {
			p := &intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{Name: "plan-esc-imgcheck", Generation: 7},
			}
			evt := waitingEvent(intelv1a1.RecoveryTypeSlot)
			evt.ImageVerifyGeneration = 7

			escalateEvent(p, evt, deviceNeed{rt: intelv1a1.RecoveryTypeReflash, reason: reasonSurvivability})

			Expect(evt.ImageVerifyGeneration).To(BeZero())
			// Clearing the hold must not take the escalation message with it.
			Expect(evt.StateMessage).To(ContainSubstring("escalated"))
		})

		// The ID changing is invisible on the event itself, and it is the reason an approval an
		// admin already granted has stopped applying. Nothing else says so.
		It("should say on the event why the previous approval no longer applies", func() {
			p := &intelv1a1.GPURecoveryPlan{ObjectMeta: metav1.ObjectMeta{Name: "plan-esc-msg"}}
			evt := waitingEvent(intelv1a1.RecoveryTypeSlot)

			escalateEvent(p, evt, deviceNeed{rt: intelv1a1.RecoveryTypeReflash, reason: reasonSurvivability})

			Expect(evt.StateMessage).To(SatisfyAll(
				ContainSubstring(string(intelv1a1.RecoveryTypeSlot)),
				ContainSubstring(string(intelv1a1.RecoveryTypeReflash)),
			))
		})

		It("should not downgrade reflash to a reset", func() {
			p := &intelv1a1.GPURecoveryPlan{ObjectMeta: metav1.ObjectMeta{Name: "plan-esc-down"}}
			evt := waitingEvent(intelv1a1.RecoveryTypeReflash)
			before := evt.DeepCopy()

			escalateEvent(p, evt, deviceNeed{rt: intelv1a1.RecoveryTypeSlot, reason: reasonWedged})

			Expect(evt).To(Equal(before), "escalation is one-way; act on the worst condition")
		})

		It("should be a no-op when the type is unchanged", func() {
			p := &intelv1a1.GPURecoveryPlan{ObjectMeta: metav1.ObjectMeta{Name: "plan-esc-same"}}
			evt := waitingEvent(intelv1a1.RecoveryTypeSlot)
			before := evt.DeepCopy()

			escalateEvent(p, evt, deviceNeed{rt: intelv1a1.RecoveryTypeSlot, reason: reasonWedged})

			Expect(evt).To(Equal(before))
			Expect(p.Status.Messages).To(BeEmpty(), "a steady-state reconcile must not log an escalation")
		})

		// Escalation must converge: repeated reconciles of the same escalated device must not
		// keep rewriting the event, or every reconcile would emit a status write and a message.
		It("should be idempotent across repeated reconciles", func() {
			p := &intelv1a1.GPURecoveryPlan{ObjectMeta: metav1.ObjectMeta{Name: "plan-esc-idem"}}
			evt := waitingEvent(intelv1a1.RecoveryTypeSlot)

			escalateEvent(p, evt, deviceNeed{rt: intelv1a1.RecoveryTypeReflash, reason: reasonSurvivability})
			afterFirst := evt.DeepCopy()
			msgCount := len(p.Status.Messages)

			escalateEvent(p, evt, deviceNeed{rt: intelv1a1.RecoveryTypeReflash, reason: reasonSurvivability})

			Expect(evt).To(Equal(afterFirst))
			Expect(p.Status.Messages).To(HaveLen(msgCount))
		})
	})

	Context("Helper: findEventForDevice", func() {
		It("should find an event regardless of its recovery type", func() {
			p := &intelv1a1.GPURecoveryPlan{
				Status: intelv1a1.GPURecoveryPlanStatus{
					Events: []intelv1a1.RecoveryEvent{
						{NodeName: "node01", GPUBDF: "0000:02:00.0",
							RecoveryType: intelv1a1.RecoveryTypeSpec{Type: intelv1a1.RecoveryTypeSlot}},
						{NodeName: "node02", GPUBDF: "0000:03:00.0",
							RecoveryType: intelv1a1.RecoveryTypeSpec{Type: intelv1a1.RecoveryTypeReflash}},
					},
				},
			}

			Expect(findEventForDevice(p, "node01", "0000:02:00.0")).To(Equal(0))
			Expect(findEventForDevice(p, "node02", "0000:03:00.0")).To(Equal(1))
			Expect(findEventForDevice(p, "node03", "0000:02:00.0")).To(Equal(-1))
			Expect(findEventForDevice(p, "node01", "0000:09:00.0")).To(Equal(-1))
		})
	})

	Context("Helper: addNewEvents", func() {
		// activeSet builds an activeKeys map of n distinct tainted devices.
		activeSet := func(n int) map[deviceKey]deviceNeed {
			active := make(map[deviceKey]deviceNeed, n)
			for i := 0; i < n; i++ {
				active[deviceKey{node: fmt.Sprintf("node%04d", i), bdf: "0000:02:00.0"}] =
					deviceNeed{rt: intelv1a1.RecoveryTypeSlot, reason: reasonWedged}
			}

			return active
		}

		It("should add an event per newly tainted device", func() {
			p := &intelv1a1.GPURecoveryPlan{}

			addNewEvents(p, activeSet(3))

			Expect(p.Status.Events).To(HaveLen(3))
		})

		// status.events[].reason is the only place the triggering condition is recorded: the
		// taint lives on the ResourceSlice and is gone by the time anyone reads the event.
		It("should record the cause the recovery was derived from", func() {
			p := &intelv1a1.GPURecoveryPlan{}

			addNewEvents(p, map[deviceKey]deviceNeed{
				{node: "node-w", bdf: "0000:02:00.0"}: {
					rt: intelv1a1.RecoveryTypeSlot, reason: reasonWedged,
				},
				{node: "node-s", bdf: "0000:03:00.0"}: {
					rt: intelv1a1.RecoveryTypeReflash, reason: reasonSurvivability,
				},
			})

			byNode := map[string]intelv1a1.RecoveryEvent{}
			for _, evt := range p.Status.Events {
				byNode[evt.NodeName] = evt
			}

			Expect(byNode["node-w"].Reason).To(Equal(reasonWedged))
			Expect(byNode["node-s"].Reason).To(Equal(reasonSurvivability))
		})

		It("should cap events at maxStatusEvents", func() {
			p := &intelv1a1.GPURecoveryPlan{}

			addNewEvents(p, activeSet(maxStatusEvents+10))

			Expect(p.Status.Events).To(HaveLen(maxStatusEvents))
		})

		It("should report the refusal in status.messages rather than dropping silently", func() {
			p := &intelv1a1.GPURecoveryPlan{}

			addNewEvents(p, activeSet(maxStatusEvents+10))

			Expect(p.Status.Messages).NotTo(BeEmpty())
			Expect(p.Status.Messages[len(p.Status.Messages)-1]).To(ContainSubstring("not recorded"))
		})

		// status.events is live state, not a log: an event carries the approval an admin gave it.
		// Evicting entries FIFO-style to make room would discard that.
		It("should keep the existing events when the cap is reached", func() {
			p := &intelv1a1.GPURecoveryPlan{}

			addNewEvents(p, activeSet(maxStatusEvents))
			oldest := p.Status.Events[0]

			addNewEvents(p, map[deviceKey]deviceNeed{
				{node: "brand-new-node", bdf: "0000:03:00.0"}: {
					rt: intelv1a1.RecoveryTypeSlot, reason: reasonWedged,
				},
			})

			Expect(p.Status.Events).To(HaveLen(maxStatusEvents))
			Expect(p.Status.Events[0]).To(Equal(oldest))
		})

		It("should not add a second event for a device that already has one", func() {
			p := &intelv1a1.GPURecoveryPlan{}
			active := activeSet(2)

			addNewEvents(p, active)
			addNewEvents(p, active)

			Expect(p.Status.Events).To(HaveLen(2))
		})
	})

	Context("Helper: removeResolvedEvents", func() {
		var r *GPURecoveryPlanReconciler

		BeforeEach(func() {
			r = newTestReconciler()
		})

		planWithEvents := func(events ...intelv1a1.RecoveryEvent) *intelv1a1.GPURecoveryPlan {
			return &intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{Name: "plan-resolved"},
				Status:     intelv1a1.GPURecoveryPlanStatus{Events: events},
			}
		}

		It("should drop an event whose taint has cleared and say so", func() {
			p := planWithEvents(intelv1a1.RecoveryEvent{
				ID: "evt-gone", NodeName: "node01", GPUBDF: "0000:02:00.0",
				State: intelv1a1.RecoveryEventStateWaitingApproval,
			})

			r.removeResolvedEvents(ctx, p, map[deviceKey]deviceNeed{})

			Expect(p.Status.Events).To(BeEmpty())
			Expect(p.Status.Messages).To(ContainElement(ContainSubstring("evt-gone")))
		})

		It("should keep an event whose taint is still active", func() {
			p := planWithEvents(intelv1a1.RecoveryEvent{
				ID: "evt-stays", NodeName: "node01", GPUBDF: "0000:02:00.0",
				State: intelv1a1.RecoveryEventStateWaitingApproval,
			})

			r.removeResolvedEvents(ctx, p, map[deviceKey]deviceNeed{
				{node: "node01", bdf: "0000:02:00.0"}: {rt: intelv1a1.RecoveryTypeSlot, reason: reasonWedged},
			})

			Expect(p.Status.Events).To(HaveLen(1))
			Expect(p.Status.Messages).To(BeEmpty())
		})

		// The key is (node, BDF): the same BDF on a different node is a different GPU, and a
		// match on the address alone would drop an event that is still needed.
		It("should not match a device on the address alone", func() {
			p := planWithEvents(intelv1a1.RecoveryEvent{
				ID: "evt-other-node", NodeName: "node02", GPUBDF: "0000:02:00.0",
				State: intelv1a1.RecoveryEventStateWaitingApproval,
			})

			r.removeResolvedEvents(ctx, p, map[deviceKey]deviceNeed{
				{node: "node01", bdf: "0000:02:00.0"}: {rt: intelv1a1.RecoveryTypeSlot, reason: reasonWedged},
			})

			Expect(p.Status.Events).To(BeEmpty())
		})
	})

	Context("Event ID generation", func() {
		It("should produce readable IDs embedding node, type and BDF", func() {
			id := generateEventID("rocebuntu2", "0000:02:00.0", intelv1a1.RecoveryTypeSBR)
			Expect(id).To(Equal("evt-rocebuntu2-sbr-0000-02-00-0"))
		})

		It("should keep a non-zero domain in the BDF slug", func() {
			id := generateEventID("node01", "0001:03:00.0", intelv1a1.RecoveryTypeSlot)
			Expect(id).To(Equal("evt-node01-slot-0001-03-00-0"))
		})

		It("should produce the same ID for the same inputs (deterministic)", func() {
			id1 := generateEventID("node01", "0000:05:00.0", intelv1a1.RecoveryTypeSlot)
			id2 := generateEventID("node01", "0000:05:00.0", intelv1a1.RecoveryTypeSlot)
			Expect(id1).To(Equal(id2))
		})

		// The ID becomes both a Job name (DNS-1123 subdomain) and a label value, and neither
		// rule is a superset of the other: uppercase and "_" are legal in a label value but
		// rejected in a name, so these assert the stricter of the two.
		DescribeTable("should always yield a valid, in-budget Job name",
			func(nodeName, bdf string, rt intelv1a1.RecoveryType) {
				id := generateEventID(nodeName, bdf, rt)

				Expect(validation.IsDNS1123Subdomain(id)).To(BeEmpty(),
					"event ID %q is not a valid resource name", id)
				Expect(validation.IsValidLabelValue(id)).To(BeEmpty(),
					"event ID %q is not a valid label value", id)

				// maxRetries has no upper bound, so check the widest attempt index the
				// budget was sized for rather than just the first attempt.
				for _, attempt := range []int{0, 9, 99} {
					name := recoveryJobName(id, attempt)

					Expect(len(name)).To(BeNumerically("<=", maxRecoveryNameLen),
						"Job name %q is %d bytes", name, len(name))
					Expect(validation.IsDNS1123Subdomain(name)).To(BeEmpty(),
						"Job name %q is not a valid resource name", name)
					Expect(validation.IsValidLabelValue(name)).To(BeEmpty(),
						"Job name %q is not a valid label value", name)
				}
			},
			Entry("short name", "node03", "0000:02:00.0", intelv1a1.RecoveryTypeSBR),
			Entry("EKS-style FQDN", "ip-10-0-134-22.us-west-2.compute.internal", "0000:02:00.0", intelv1a1.RecoveryTypeReflash),
			Entry("OpenShift-style FQDN", "worker-03.cluster.example.com", "0000:af:00.0", intelv1a1.RecoveryTypeReflash),
			Entry("at the node budget", strings.Repeat("n", nodeSegmentMax), "0001:02:00.0", intelv1a1.RecoveryTypeReflash),
			Entry("one over the node budget", strings.Repeat("n", nodeSegmentMax+1), "0001:02:00.0", intelv1a1.RecoveryTypeReflash),
			Entry("absurdly long name", strings.Repeat("very-long-node-name.", 20), "0001:02:00.0", intelv1a1.RecoveryTypeReflash),
			// lspci prints PCI addresses in uppercase hex, so this is a likely spelling
			// rather than a pathological one — and the DRA driver's pciAddress attribute
			// is a free-form string, not a validated field.
			Entry("uppercase hex BDF", "node03", "0000:AF:00.0", intelv1a1.RecoveryTypeSBR),
			Entry("BDF with unexpected separators", "node03", "0000/02_00,0", intelv1a1.RecoveryTypeSBR),
			// The truncation point is nodeSegmentMax-idHashLen-1, so placing a hyphen at the
			// last kept index makes the cut land exactly on a separator — the case that
			// needs trimming before the hash suffix is appended, since a DNS-1123 subdomain
			// may not contain "--" at a label boundary or end on a hyphen.
			Entry("node name that truncates onto a separator",
				strings.Repeat("b", nodeSegmentMax-idHashLen-2)+"-more-tail", "0000:02:00.0", intelv1a1.RecoveryTypeReflash),
			Entry("empty BDF", "node03", "", intelv1a1.RecoveryTypeSBR),
			// pciAddress is a free-form ResourceSlice attribute written by the DRA driver,
			// so nothing bounds its length. Sanitizing it yields a valid *character set*
			// at any length, which is exactly how a charset-only guard lets an
			// over-long ID through.
			Entry("absurdly long BDF", "node03", strings.Repeat("0000:02:00.0/", 12), intelv1a1.RecoveryTypeReflash),
			// A BDF ending in a separator sanitizes to a trailing hyphen, which is the one
			// position a DNS-1123 subdomain forbids outright. Unlike the node segment — where
			// the hash suffix always follows and hides it — the BDF is the last part of the
			// ID, so nothing covers for it.
			Entry("BDF with a trailing separator", "node03", "0000:02:00.0:", intelv1a1.RecoveryTypeSBR),
			Entry("BDF with a leading separator", "node03", ":02:00.0", intelv1a1.RecoveryTypeSBR),
			Entry("node name with leading and trailing dots", ".node03.", "0000:02:00.0", intelv1a1.RecoveryTypeSBR),
			Entry("long node and long BDF together",
				strings.Repeat("node.", 20), strings.Repeat("0000:02:00.0/", 12), intelv1a1.RecoveryTypeReflash),
		)

		// The ID is not only a Job-name component: addRecoveryEvent stores it in
		// status.events[].id and it is assigned verbatim to a label on the recovery Job, whose
		// values cap at 63 bytes. A guard that checked only the character set would pass a
		// 170-byte ID here — IsDNS1123Subdomain permits 253 — and the label assignment would
		// then be rejected.
		DescribeTable("should yield an ID usable as a label value in its own right",
			func(nodeName, bdf string) {
				id := generateEventID(nodeName, bdf, intelv1a1.RecoveryTypeReflash)

				Expect(validation.IsValidLabelValue(id)).To(BeEmpty(),
					"event ID %q (%d bytes) is not a valid label value", id, len(id))
			},
			Entry("long BDF", "node03", strings.Repeat("0000:02:00.0/", 12)),
			Entry("long node", strings.Repeat("node.", 20), "0000:02:00.0"),
			Entry("both long", strings.Repeat("node.", 20), strings.Repeat("0000:02:00.0/", 12)),
		)

		// nodeSegmentMax is a hand-computed budget, and the specs above only prove that the
		// *result* is valid — recoveryJobName's hash fallback would silently absorb an
		// oversized constant, turning every long-node ID unreadable while still passing.
		// This pins the arithmetic directly so the constant cannot drift away from the limit
		// it was derived from without saying so.
		It("should size nodeSegmentMax so the worst-case name needs no fallback", func() {
			worst := len("recovery-") + len("evt-") + nodeSegmentMax +
				len("-reflash") + len("-0001-02-00-0") + len("-99")

			Expect(worst).To(BeNumerically("<=", maxRecoveryNameLen),
				"nodeSegmentMax=%d makes the worst-case Job name %d bytes, over the %d-byte limit",
				nodeSegmentMax, worst, maxRecoveryNameLen)

			// And the readable path is actually taken at the budget: a node segment exactly
			// at nodeSegmentMax must survive into the Job name intact, not be hashed away.
			id := generateEventID(strings.Repeat("n", nodeSegmentMax), "0001:02:00.0",
				intelv1a1.RecoveryTypeReflash)
			Expect(recoveryJobName(id, 99)).To(ContainSubstring(strings.Repeat("n", nodeSegmentMax)),
				"the worst-case name should fit without recoveryJobName hashing it")
		})

		It("should lowercase an uppercase BDF rather than rejecting it", func() {
			// Specifically pins the sanitize-rather-than-hash behaviour: the readable form
			// survives, so this must not fall through to the hash branch.
			id := generateEventID("node03", "0000:AF:00.0", intelv1a1.RecoveryTypeSBR)
			Expect(id).To(Equal("evt-node03-sbr-0000-af-00-0"))
		})

		// Validity alone is a weak assertion here: the guard's whole-ID hash fallback also
		// produces a valid ID, so a version that did no bounding at all — collapsing every
		// long node to an opaque "evt-sbr-09553f" — would satisfy every check above. These
		// pin the behaviour that motivates truncate-plus-hash over hash-everything: the
		// operator can still tell which node an event belongs to at a glance.
		DescribeTable("should keep a readable node prefix rather than hashing the whole ID",
			func(nodeName, wantPrefix string) {
				id := generateEventID(nodeName, "0000:02:00.0", intelv1a1.RecoveryTypeSBR)

				Expect(id).To(HavePrefix(wantPrefix),
					"ID %q should keep a readable prefix of node %q", id, nodeName)
				Expect(id).To(ContainSubstring("-sbr-"),
					"ID %q should still carry the recovery type", id)
			},
			Entry("EKS-style FQDN", "ip-10-0-134-22.us-west-2.compute.internal", "evt-ip-10-0-134-22"),
			Entry("OpenShift-style FQDN", "worker-03.cluster.example.com", "evt-worker-03"),
			Entry("long single-token name", strings.Repeat("worker-aaaa", 4)+"-01", "evt-worker-aaaa"),
		)

		It("should not collide for long node names sharing a prefix", func() {
			// The hash covers the *original* name, not the truncated form. If it hashed the
			// truncation instead, these two would alias onto one ID and break the
			// one-event-per-device invariant findEventForDevice relies on.
			prefix := strings.Repeat("worker-aaaa", 4)
			id1 := generateEventID(prefix+"-01", "0000:02:00.0", intelv1a1.RecoveryTypeSBR)
			id2 := generateEventID(prefix+"-02", "0000:02:00.0", intelv1a1.RecoveryTypeSBR)

			Expect(id1).NotTo(Equal(id2))
		})

		It("should stay deterministic for long node names", func() {
			long := strings.Repeat("long-node-name.", 6)
			Expect(generateEventID(long, "0000:02:00.0", intelv1a1.RecoveryTypeSBR)).
				To(Equal(generateEventID(long, "0000:02:00.0", intelv1a1.RecoveryTypeSBR)))
		})

		// escalateEvent depends on a type change producing a new ID: that is what invalidates
		// an approval naming the old one, so a reset approval cannot silently authorise the
		// reflash it escalated into. The property has to survive every path an ID can take,
		// including the whole-ID hash fallback — which is why the recovery type is
		// interpolated outside the hash rather than fed into it.
		DescribeTable("should change the ID on escalation on every naming path",
			func(nodeName, bdf string) {
				slot := generateEventID(nodeName, bdf, intelv1a1.RecoveryTypeSlot)
				reflash := generateEventID(nodeName, bdf, intelv1a1.RecoveryTypeReflash)

				Expect(slot).NotTo(Equal(reflash),
					"slot ID %q and reflash ID %q must differ or a stale approval stays valid", slot, reflash)
			},
			Entry("readable path", "node03", "0000:02:00.0"),
			Entry("truncated node path", strings.Repeat("long-node-name.", 6), "0000:02:00.0"),
			// Long BDF pushes the ID over budget, so this exercises the whole-ID fallback.
			Entry("whole-ID fallback path", "node03", strings.Repeat("0000:02:00.0/", 12)),
		)

		It("should preserve separator runs so they cannot alias", func() {
			// "a--b" is a valid subdomain, so collapsing runs would only lose information
			// and risk two distinct nodes mapping to one ID.
			id1 := generateEventID("node-01", "0000:02:00.0", intelv1a1.RecoveryTypeSBR)
			id2 := generateEventID("node--01", "0000:02:00.0", intelv1a1.RecoveryTypeSBR)

			Expect(id1).NotTo(Equal(id2))
		})
	})

	// status.events[].stateMessage exists because the states are coarse — several different
	// situations park an event in the same one — and an admin cannot be expected to reconstruct
	// which is which from a 50-entry ring shared by every event.
	Context("Helper: setEventState", func() {
		// Every spec here starts from waiting-approval, the state an event is in before anything
		// has happened to it. LastUpdated is stamped an hour back so a re-stamp is visible.
		waitingEvent := func() *intelv1a1.RecoveryEvent {
			return &intelv1a1.RecoveryEvent{
				ID:           "evt-msg-001",
				NodeName:     "node01",
				GPUBDF:       "0000:02:00.0",
				RecoveryType: intelv1a1.RecoveryTypeSpec{Type: intelv1a1.RecoveryTypeSlot},
				State:        intelv1a1.RecoveryEventStateWaitingApproval,
				LastUpdated:  ptr.To(metav1.NewTime(time.Now().Add(-time.Hour))),
			}
		}

		It("should drop the previous explanation on a transition that has none of its own", func() {
			evt := waitingEvent()
			evt.StateMessage = "escalated from slot to reflash (survivability-mode)"

			setEventState(evt, intelv1a1.RecoveryEventStateWaitingApproval, "")

			// The whole value of the field is that it describes the state next to it. A message
			// about a transition that has since been superseded is worse than no message at all.
			Expect(evt.StateMessage).To(BeEmpty())
		})

		It("should not re-stamp LastUpdated when neither the state nor the message changes", func() {
			evt := waitingEvent()
			setEventState(evt, intelv1a1.RecoveryEventStateWaitingApproval, "held: %v", "some reason")
			first := *evt.LastUpdated

			setEventState(evt, intelv1a1.RecoveryEventStateWaitingApproval, "held: %v", "some reason")

			// Conditions are re-detected on every pass. Stamping each time would write the object
			// once per reconcile for as long as the condition lasts, and make lastUpdated useless
			// as "when did this event last actually change".
			Expect(*evt.LastUpdated).To(Equal(first))
		})

		It("should truncate on a rune boundary so the status write stays valid UTF-8", func() {
			evt := waitingEvent()

			setEventState(evt, intelv1a1.RecoveryEventStateWaitingApproval, "%s", strings.Repeat("dénï — ", 60))

			Expect(len(evt.StateMessage)).To(BeNumerically("<=", maxStateMessageLen))
			Expect(utf8.ValidString(evt.StateMessage)).To(BeTrue(),
				"the API server rejects a status write containing invalid UTF-8")
		})

		// blocked is the one state an admin can do nothing about and has no other way to diagnose:
		// the event looks stalled, and only the message says it is queued rather than stuck.
		It("should name the recovery holding the node when an event is blocked", func() {
			p := &intelv1a1.GPURecoveryPlan{ObjectMeta: metav1.ObjectMeta{Name: "plan-block-msg"}}
			evt := waitingEvent()

			blockEvent(p, evt, "evt-holder-001")

			Expect(evt.State).To(Equal(intelv1a1.RecoveryEventStateBlocked))
			Expect(evt.StateMessage).To(ContainSubstring("evt-holder-001"))
			Expect(evt.StateMessage).To(ContainSubstring("node01"))
			Expect(evt.StateMessage).To(ContainSubstring("approval retained"))
			Expect(p.Status.Messages).To(HaveLen(1))
			Expect(p.Status.Messages[0]).To(ContainSubstring("evt-msg-001"))
		})
	})

	Context("Helper: updatePlanState", func() {
		// planWith builds a plan with maxRetries=3 and the given events, so the retry-budget
		// comparison in updatePlanState has a real threshold to test against.
		planWith := func(events ...intelv1a1.RecoveryEvent) *intelv1a1.GPURecoveryPlan {
			return &intelv1a1.GPURecoveryPlan{
				Spec: intelv1a1.GPURecoveryPlanSpec{DefaultResetType: intelv1a1.RecoveryTypeSlot, MaxRetries: 3},
				Status: intelv1a1.GPURecoveryPlanStatus{
					Events: events,
				},
			}
		}
		evt := func(state intelv1a1.RecoveryEventState, retries int32) intelv1a1.RecoveryEvent {
			return intelv1a1.RecoveryEvent{State: state, RetryCount: retries}
		}

		It("should report idle with no events", func() {
			p := planWith()
			updatePlanState(p)
			Expect(p.Status.State).To(Equal(intelv1a1.PlanStateIdle))
		})

		DescribeTable("should report active while an event is on its way somewhere",
			func(state intelv1a1.RecoveryEventState) {
				p := planWith(evt(state, 0))
				updatePlanState(p)
				Expect(p.Status.State).To(Equal(intelv1a1.PlanStateActive))
			},
			Entry("waiting for an approval", intelv1a1.RecoveryEventStateWaitingApproval),
			// blocked is active, not stuck: it clears on its own once the node frees up.
			Entry("blocked behind another recovery", intelv1a1.RecoveryEventStateBlocked),
			Entry("draining its node", intelv1a1.RecoveryEventStateDraining),
			Entry("running its recovery", intelv1a1.RecoveryEventStateInProgress),
		)

		It("should report idle once every event has settled", func() {
			p := planWith(evt(intelv1a1.RecoveryEventStateSucceeded, 1))
			updatePlanState(p)
			Expect(p.Status.State).To(Equal(intelv1a1.PlanStateIdle))
		})

		It("should report error for an event that exhausted its retry budget", func() {
			p := planWith(evt(intelv1a1.RecoveryEventStateFailed, 3))
			updatePlanState(p)
			Expect(p.Status.State).To(Equal(intelv1a1.PlanStateError),
				"an exhausted event never retries on its own; it needs an explicit re-approval")
		})

		// A failure inside the budget is transient: the event is put back to waiting-approval on
		// a later pass, so surfacing "error" would be a false alarm.
		It("should not report error for a failure still inside its retry budget", func() {
			p := planWith(evt(intelv1a1.RecoveryEventStateFailed, 1))
			updatePlanState(p)
			Expect(p.Status.State).NotTo(Equal(intelv1a1.PlanStateError))
		})

		It("should report error when a reflash is blocked on missing firmware", func() {
			p := planWith(evt(intelv1a1.RecoveryEventStateMissingFirmware, 0))
			updatePlanState(p)
			Expect(p.Status.State).To(Equal(intelv1a1.PlanStateError),
				"nothing clears this but an operator filling in spec.firmware")
		})

		// An event whose images did not verify is parked in waiting-approval, which normally reads as
		// active. Here it is not: the approval is already there and the plan is what is wrong, so
		// nothing will move until an admin edits it. Reporting active would hide that behind a state
		// that means "working on it".
		It("should report error for an event held on an unpullable image", func() {
			p := planWith(evt(intelv1a1.RecoveryEventStateWaitingApproval, 0))
			p.Generation = 4
			p.Status.Events[0].ImageVerifyGeneration = 4

			updatePlanState(p)

			Expect(p.Status.State).To(Equal(intelv1a1.PlanStateError))
		})

		// The recorded generation is only a verdict on the spec it was made against. Once the spec
		// moves on the check has not been made yet, so the event is genuinely waiting again.
		It("should report active again once the plan has moved past a held generation", func() {
			p := planWith(evt(intelv1a1.RecoveryEventStateWaitingApproval, 0))
			p.Generation = 5
			p.Status.Events[0].ImageVerifyGeneration = 4

			updatePlanState(p)

			Expect(p.Status.State).To(Equal(intelv1a1.PlanStateActive))
		})

		// The whole point of the field is answering "does this need me?" — one healthy event in
		// flight must not mask a stuck one.
		It("should let error outrank active", func() {
			p := planWith(
				evt(intelv1a1.RecoveryEventStateInProgress, 0),
				evt(intelv1a1.RecoveryEventStateFailed, 3),
			)
			updatePlanState(p)
			Expect(p.Status.State).To(Equal(intelv1a1.PlanStateError))
		})
	})

	Context("Helper: findMatchingApproval", func() {
		var r *GPURecoveryPlanReconciler

		BeforeEach(func() {
			r = newTestReconciler()
		})

		evt := &intelv1a1.RecoveryEvent{
			ID:           "evt-aabb",
			NodeName:     "node05",
			GPUBDF:       "0000:02:00.0",
			RecoveryType: intelv1a1.RecoveryTypeSpec{Type: intelv1a1.RecoveryTypeSBR},
			State:        intelv1a1.RecoveryEventStateWaitingApproval,
		}

		It("should match a singular eventId approval", func() {
			p := &intelv1a1.GPURecoveryPlan{
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					Approvals: []intelv1a1.RecoveryApproval{
						{ID: "app-1234", EventID: "evt-aabb"},
					},
				},
			}
			approval, ok := r.findMatchingApproval(ctx, p, evt)
			Expect(ok).To(BeTrue())
			Expect(approval.ID).To(Equal("app-1234"))
		})

		It("should match a selector approval with matching recoveryType", func() {
			p := &intelv1a1.GPURecoveryPlan{
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					Approvals: []intelv1a1.RecoveryApproval{
						{
							ID:       "app-5678",
							Selector: &intelv1a1.ApprovalSelector{RecoveryType: intelv1a1.RecoveryTypeSBR},
						},
					},
				},
			}
			approval, ok := r.findMatchingApproval(ctx, p, evt)
			Expect(ok).To(BeTrue())
			Expect(approval.ID).To(Equal("app-5678"))
		})

		It("should not match a selector with a different recoveryType", func() {
			p := &intelv1a1.GPURecoveryPlan{
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					Approvals: []intelv1a1.RecoveryApproval{
						{
							Selector: &intelv1a1.ApprovalSelector{RecoveryType: intelv1a1.RecoveryTypeSlot},
						},
					},
				},
			}
			_, ok := r.findMatchingApproval(ctx, p, evt)
			Expect(ok).To(BeFalse())
		})

		It("should not match a selector with a different nodeName", func() {
			p := &intelv1a1.GPURecoveryPlan{
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					Approvals: []intelv1a1.RecoveryApproval{
						{Selector: &intelv1a1.ApprovalSelector{NodeName: "node99"}},
					},
				},
			}
			_, ok := r.findMatchingApproval(ctx, p, evt)
			Expect(ok).To(BeFalse())
		})

		It("should not match a consumed eventId approval", func() {
			p := &intelv1a1.GPURecoveryPlan{
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					Approvals: []intelv1a1.RecoveryApproval{
						{EventID: "evt-aabb", Consumed: true},
					},
				},
			}
			_, ok := r.findMatchingApproval(ctx, p, evt)
			Expect(ok).To(BeFalse())
		})

		It("should not match a consumed selector approval", func() {
			p := &intelv1a1.GPURecoveryPlan{
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					Approvals: []intelv1a1.RecoveryApproval{
						{
							Selector: &intelv1a1.ApprovalSelector{RecoveryType: intelv1a1.RecoveryTypeSBR},
							Consumed: true,
						},
					},
				},
			}
			_, ok := r.findMatchingApproval(ctx, p, evt)
			Expect(ok).To(BeFalse())
		})

		// An event with no recovery type cannot be authorised: a selector with an empty
		// recoveryType means "any reset", and matching it against an event whose own type is
		// unknown would approve a reset nobody chose.
		It("should refuse to match an event with no recovery type", func() {
			p := &intelv1a1.GPURecoveryPlan{
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					Approvals: []intelv1a1.RecoveryApproval{
						{ID: "app-any", Selector: &intelv1a1.ApprovalSelector{}},
					},
				},
			}

			_, ok := r.findMatchingApproval(ctx, p, &intelv1a1.RecoveryEvent{
				ID: "evt-typeless", NodeName: "node05", GPUBDF: "0000:02:00.0",
			})
			Expect(ok).To(BeFalse())
		})

		Context("nodeSelector matching", func() {
			// These specs create real Nodes in envtest so selector.nodeSelector is matched against
			// actual Node labels.
			const (
				labelledNode = "node-rack04"
				plainNode    = "node-plain"
			)

			nodeSelectorApproval := func(sel map[string]string) *intelv1a1.GPURecoveryPlan {
				return &intelv1a1.GPURecoveryPlan{
					Spec: intelv1a1.GPURecoveryPlanSpec{
						DefaultResetType: intelv1a1.RecoveryTypeSlot,
						Approvals: []intelv1a1.RecoveryApproval{
							{ID: "app-sel", Selector: &intelv1a1.ApprovalSelector{NodeSelector: sel}},
						},
					},
				}
			}

			eventForNode := func(nodeName string) *intelv1a1.RecoveryEvent {
				return &intelv1a1.RecoveryEvent{
					ID:           "evt-" + nodeName,
					NodeName:     nodeName,
					GPUBDF:       "0000:02:00.0",
					RecoveryType: intelv1a1.RecoveryTypeSpec{Type: intelv1a1.RecoveryTypeSBR},
					State:        intelv1a1.RecoveryEventStateWaitingApproval,
				}
			}

			BeforeEach(func() {
				for name, nodeLabels := range map[string]map[string]string{
					labelledNode: {"rack": "rack-04-32", "gpu": "true"},
					plainNode:    nil,
				} {
					node := &core.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: nodeLabels}}
					if err := k8sClient.Create(ctx, node); err != nil {
						Expect(errors.IsAlreadyExists(err)).To(BeTrue())
					}
				}
			})

			AfterEach(func() {
				for _, name := range []string{labelledNode, plainNode} {
					node := &core.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
					Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, node))).To(Succeed())
				}
			})

			It("should match when the node carries all selector labels", func() {
				p := nodeSelectorApproval(map[string]string{"rack": "rack-04-32"})

				approval, ok := r.findMatchingApproval(ctx, p, eventForNode(labelledNode))
				Expect(ok).To(BeTrue())
				Expect(approval.ID).To(Equal("app-sel"))
			})

			It("should match when every label in a multi-label selector is present", func() {
				p := nodeSelectorApproval(map[string]string{"rack": "rack-04-32", "gpu": "true"})

				_, ok := r.findMatchingApproval(ctx, p, eventForNode(labelledNode))
				Expect(ok).To(BeTrue())
			})

			It("should NOT match an event on a node without the selector labels", func() {
				// Without node-label matching this returns true, silently widening a rack-scoped
				// approval cluster-wide.
				p := nodeSelectorApproval(map[string]string{"rack": "rack-04-32"})

				_, ok := r.findMatchingApproval(ctx, p, eventForNode(plainNode))
				Expect(ok).To(BeFalse())
			})

			It("should NOT match when the label value differs", func() {
				p := nodeSelectorApproval(map[string]string{"rack": "rack-99-01"})

				_, ok := r.findMatchingApproval(ctx, p, eventForNode(labelledNode))
				Expect(ok).To(BeFalse())
			})

			It("should NOT match when only some of the selector labels are present", func() {
				p := nodeSelectorApproval(map[string]string{"rack": "rack-04-32", "zone": "west"})

				_, ok := r.findMatchingApproval(ctx, p, eventForNode(labelledNode))
				Expect(ok).To(BeFalse())
			})

			It("should fail closed when the node does not exist", func() {
				// A node that cannot be read must never be treated as matching, otherwise a
				// transient API error would widen the approval's scope.
				p := nodeSelectorApproval(map[string]string{"rack": "rack-04-32"})

				_, ok := r.findMatchingApproval(ctx, p, eventForNode("node-does-not-exist"))
				Expect(ok).To(BeFalse())
			})

			It("should still honour an empty nodeSelector as match-anything", func() {
				p := nodeSelectorApproval(nil)

				_, ok := r.findMatchingApproval(ctx, p, eventForNode(plainNode))
				Expect(ok).To(BeTrue())
			})

			It("should require both nodeSelector and recoveryType to match", func() {
				p := &intelv1a1.GPURecoveryPlan{
					Spec: intelv1a1.GPURecoveryPlanSpec{
						DefaultResetType: intelv1a1.RecoveryTypeSlot,
						Approvals: []intelv1a1.RecoveryApproval{
							{
								ID: "app-both",
								Selector: &intelv1a1.ApprovalSelector{
									RecoveryType: intelv1a1.RecoveryTypeSlot,
									NodeSelector: map[string]string{"rack": "rack-04-32"},
								},
							},
						},
					},
				}

				// Labels match but the recovery type (SBR) does not.
				_, ok := r.findMatchingApproval(ctx, p, eventForNode(labelledNode))
				Expect(ok).To(BeFalse())
			})
		})
	})

	Context("Helper: applyOverride", func() {
		var r *GPURecoveryPlanReconciler

		BeforeEach(func() {
			r = newTestReconciler()
		})

		It("should up-level a defaulted SBR event to the admin-requested type and record SuggestedType", func() {
			p := &intelv1a1.GPURecoveryPlan{ObjectMeta: metav1.ObjectMeta{Name: "plan-override"}}
			evt := &intelv1a1.RecoveryEvent{
				ID:           "evt-override-1",
				RecoveryType: intelv1a1.RecoveryTypeSpec{Type: intelv1a1.RecoveryTypeSBR},
			}
			approval := intelv1a1.RecoveryApproval{
				ID:       "app-override",
				Override: &intelv1a1.RecoveryOverride{RecoveryType: intelv1a1.RecoveryTypeSlot},
			}

			r.applyOverride(p, evt, approval)

			Expect(evt.RecoveryType.Type).To(Equal(intelv1a1.RecoveryTypeSlot))
			Expect(evt.RecoveryType.SuggestedType).To(Equal(intelv1a1.RecoveryTypeSBR))
			Expect(buildResetCommand("0000:02:00.0", evt.RecoveryType.Type)).To(
				ContainSubstring("--coldreset"), "an override must change the command actually run")
		})

		It("should be a no-op when the approval has no override", func() {
			p := &intelv1a1.GPURecoveryPlan{}
			evt := &intelv1a1.RecoveryEvent{
				RecoveryType: intelv1a1.RecoveryTypeSpec{Type: intelv1a1.RecoveryTypeSBR},
			}

			r.applyOverride(p, evt, intelv1a1.RecoveryApproval{ID: "app-no-override"})

			Expect(evt.RecoveryType.Type).To(Equal(intelv1a1.RecoveryTypeSBR))
			Expect(evt.RecoveryType.SuggestedType).To(BeEmpty())
		})

		It("should ignore an override on a reflash event", func() {
			p := &intelv1a1.GPURecoveryPlan{}
			evt := &intelv1a1.RecoveryEvent{
				RecoveryType: intelv1a1.RecoveryTypeSpec{Type: intelv1a1.RecoveryTypeReflash},
			}
			approval := intelv1a1.RecoveryApproval{
				Override: &intelv1a1.RecoveryOverride{RecoveryType: intelv1a1.RecoveryTypeSBR},
			}

			r.applyOverride(p, evt, approval)

			Expect(evt.RecoveryType.Type).To(Equal(intelv1a1.RecoveryTypeReflash))
			Expect(evt.RecoveryType.SuggestedType).To(BeEmpty())
		})

		It("should ignore an override that requests reflash for a reset event", func() {
			p := &intelv1a1.GPURecoveryPlan{}
			evt := &intelv1a1.RecoveryEvent{
				RecoveryType: intelv1a1.RecoveryTypeSpec{Type: intelv1a1.RecoveryTypeSBR},
			}
			approval := intelv1a1.RecoveryApproval{
				Override: &intelv1a1.RecoveryOverride{RecoveryType: intelv1a1.RecoveryTypeReflash},
			}

			r.applyOverride(p, evt, approval)

			Expect(evt.RecoveryType.Type).To(Equal(intelv1a1.RecoveryTypeSBR))
			Expect(evt.RecoveryType.SuggestedType).To(BeEmpty())
		})

		// SuggestedType is the audit trail of what the operator itself picked, so a second
		// override must not overwrite it with the first override's choice.
		It("should record SuggestedType only once across repeated overrides", func() {
			p := &intelv1a1.GPURecoveryPlan{}
			evt := &intelv1a1.RecoveryEvent{
				RecoveryType: intelv1a1.RecoveryTypeSpec{Type: intelv1a1.RecoveryTypeSBR},
			}

			r.applyOverride(p, evt, intelv1a1.RecoveryApproval{
				Override: &intelv1a1.RecoveryOverride{RecoveryType: intelv1a1.RecoveryTypeSlot},
			})
			r.applyOverride(p, evt, intelv1a1.RecoveryApproval{
				Override: &intelv1a1.RecoveryOverride{RecoveryType: intelv1a1.RecoveryTypeAMC},
			})

			Expect(evt.RecoveryType.Type).To(Equal(intelv1a1.RecoveryTypeAMC))
			Expect(evt.RecoveryType.SuggestedType).To(Equal(intelv1a1.RecoveryTypeSBR),
				"suggestedType must keep the type the operator detected, not the previous override")
		})
	})

	Context("processApprovals: approval consumption", func() {
		It("should set Consumed=true on the approval after a non-persistent selector approval fires", func() {
			r := newTestReconciler()

			p := &intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{Name: "plan-consume-test"},
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					DeviceID:         "0xabcd",
					MaxRetries:       3,
					XpuSmi:           intelv1a1.XpuSmiSpec{Image: "registry/xpu-smi:latest"},
					// The subject here is which approval fires and when it is spent, so the drain
					// is switched off: with it on, an approved event stops at draining and the
					// answer would depend on Node and Pod fixtures that say nothing about
					// approvals. The drain's own effect on consumption is covered with the drain.
					Drain: intelv1a1.DrainSpec{Enable: ptr.To(false)},
					Approvals: []intelv1a1.RecoveryApproval{
						{
							ID:         "sel-nonpersist",
							Selector:   &intelv1a1.ApprovalSelector{RecoveryType: intelv1a1.RecoveryTypeSBR},
							Persistent: false,
						},
					},
				},
				Status: intelv1a1.GPURecoveryPlanStatus{
					Events: []intelv1a1.RecoveryEvent{
						{
							ID:           "evt-consume-1",
							NodeName:     "node01",
							GPUBDF:       "0000:02:00.0",
							RecoveryType: intelv1a1.RecoveryTypeSpec{Type: intelv1a1.RecoveryTypeSBR},
							State:        intelv1a1.RecoveryEventStateWaitingApproval,
						},
					},
				},
			}

			// Recovery Jobs are owned by the plan, and an owner reference needs a UID — so the plan
			// must exist in the API server, as it always does in production (Reconcile only ever
			// operates on an object it just Got).
			createPlanForOwnerRef(p)

			r.processApprovals(ctx, p)

			Expect(p.Spec.Approvals[0].Consumed).To(BeTrue(),
				"non-persistent selector approval must be marked consumed after firing")

			// A second call with a new event must not match the now-consumed approval.
			p.Status.Events = append(p.Status.Events, intelv1a1.RecoveryEvent{
				ID:           "evt-consume-2",
				NodeName:     "node01",
				GPUBDF:       "0000:03:00.0",
				RecoveryType: intelv1a1.RecoveryTypeSpec{Type: intelv1a1.RecoveryTypeSBR},
				State:        intelv1a1.RecoveryEventStateWaitingApproval,
			})

			r.processApprovals(ctx, p)

			var secondEvt *intelv1a1.RecoveryEvent

			for i := range p.Status.Events {
				if p.Status.Events[i].ID == "evt-consume-2" {
					secondEvt = &p.Status.Events[i]

					break
				}
			}

			Expect(secondEvt).NotTo(BeNil())
			Expect(secondEvt.State).To(Equal(intelv1a1.RecoveryEventStateWaitingApproval),
				"second event must remain waiting-approval because the approval was already consumed")
		})

		// Consumption is deferred to the end of the loop so one approval covers everything
		// currently waiting. An admin approving "any sbr" for a node with three wedged GPUs means
		// all three, not whichever event happens to be first in status.events.
		It("should let one non-persistent approval fire for every event waiting in the same pass", func() {
			r := newTestReconciler()

			p := &intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{Name: "plan-consume-batch"},
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					DeviceID:         "0xabcd",
					MaxRetries:       3,
					XpuSmi:           intelv1a1.XpuSmiSpec{Image: "registry/xpu-smi:latest"},
					Drain:            intelv1a1.DrainSpec{Enable: ptr.To(false)},
					Approvals: []intelv1a1.RecoveryApproval{
						{
							ID:       "sel-batch",
							Selector: &intelv1a1.ApprovalSelector{RecoveryType: intelv1a1.RecoveryTypeSBR},
						},
					},
				},
				Status: intelv1a1.GPURecoveryPlanStatus{
					Events: []intelv1a1.RecoveryEvent{
						{
							ID: "evt-batch-1", NodeName: "node01", GPUBDF: "0000:02:00.0",
							RecoveryType: intelv1a1.RecoveryTypeSpec{Type: intelv1a1.RecoveryTypeSBR},
							State:        intelv1a1.RecoveryEventStateWaitingApproval,
						},
						{
							// On a second node, not a second GPU of node01: only one recovery runs
							// per node, so same-node siblings would come out blocked and the subject
							// here would become the node gate rather than the approval. Its effect on
							// batching is asserted with the gate itself.
							ID: "evt-batch-2", NodeName: "node02", GPUBDF: "0000:03:00.0",
							RecoveryType: intelv1a1.RecoveryTypeSpec{Type: intelv1a1.RecoveryTypeSBR},
							State:        intelv1a1.RecoveryEventStateWaitingApproval,
						},
					},
				},
			}

			createPlanForOwnerRef(p)

			DeferCleanup(func() {
				for _, evt := range p.Status.Events {
					if evt.JobName != "" {
						_ = k8sClient.Delete(ctx, &batch.Job{ObjectMeta: metav1.ObjectMeta{
							Name: evt.JobName, Namespace: "default",
						}})
					}
				}
			})

			r.processApprovals(ctx, p)

			for i := range p.Status.Events {
				Expect(p.Status.Events[i].State).To(Equal(intelv1a1.RecoveryEventStateInProgress),
					"event %s should have started under the same approval", p.Status.Events[i].ID)
				Expect(p.Status.Events[i].ApprovalID).To(Equal("sel-batch"))
			}

			Expect(p.Spec.Approvals[0].Consumed).To(BeTrue())
		})

		It("should not consume a persistent approval", func() {
			r := newTestReconciler()

			p := &intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{Name: "plan-consume-persistent"},
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					DeviceID:         "0xabcd",
					MaxRetries:       3,
					XpuSmi: intelv1a1.XpuSmiSpec{
						Image: "local/xpusmi:devel",
					},
					Drain: intelv1a1.DrainSpec{Enable: ptr.To(false)},
					Approvals: []intelv1a1.RecoveryApproval{
						{
							ID:         "sel-persistent",
							Selector:   &intelv1a1.ApprovalSelector{RecoveryType: intelv1a1.RecoveryTypeSBR},
							Persistent: true,
						},
					},
				},
				Status: intelv1a1.GPURecoveryPlanStatus{
					Events: []intelv1a1.RecoveryEvent{
						{
							ID: "evt-persistent-1", NodeName: "node01", GPUBDF: "0000:02:00.0",
							RecoveryType: intelv1a1.RecoveryTypeSpec{Type: intelv1a1.RecoveryTypeSBR},
							State:        intelv1a1.RecoveryEventStateWaitingApproval,
						},
					},
				},
			}

			createPlanForOwnerRef(p)

			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, &batch.Job{ObjectMeta: metav1.ObjectMeta{
					Name: "recovery-evt-persistent-1-0", Namespace: "default",
				}})
			})

			r.processApprovals(ctx, p)

			Expect(p.Status.Events[0].State).To(Equal(intelv1a1.RecoveryEventStateInProgress))
			Expect(p.Spec.Approvals[0].Consumed).To(BeFalse(),
				"a persistent approval is standing policy and must survive firing")
		})

		// A reflash the operator cannot build a Job for must leave its approval unspent: consuming it
		// would spend the admin's decision on nothing, and they would have to approve again once
		// spec.firmware told the operator what to flash.
		It("should park a reflash with no firmware configured without consuming the approval", func() {
			r := newTestReconciler()

			p := &intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{Name: "plan-reflash-park"},
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					DeviceID:         "0xabcd",
					MaxRetries:       3,
					Approvals: []intelv1a1.RecoveryApproval{
						{ID: "app-reflash", EventID: "evt-reflash-park"},
					},
				},
				Status: intelv1a1.GPURecoveryPlanStatus{
					Events: []intelv1a1.RecoveryEvent{
						{
							ID: "evt-reflash-park", NodeName: "node01", GPUBDF: "0000:02:00.0",
							RecoveryType: intelv1a1.RecoveryTypeSpec{Type: intelv1a1.RecoveryTypeReflash},
							Reason:       reasonSurvivability,
							State:        intelv1a1.RecoveryEventStateWaitingApproval,
						},
					},
				},
			}

			createPlanForOwnerRef(p)

			r.processApprovals(ctx, p)

			evt := p.Status.Events[0]
			Expect(evt.State).To(Equal(intelv1a1.RecoveryEventStateMissingFirmware))
			Expect(evt.JobName).To(BeEmpty())
			Expect(evt.StateMessage).To(ContainSubstring("spec.firmware"))
			Expect(p.Spec.Approvals[0].Consumed).To(BeFalse(),
				"an approval that produced no Job must stay available")

			job := &batch.Job{}
			err := k8sClient.Get(ctx, types.NamespacedName{
				Name: "recovery-evt-reflash-park-0", Namespace: "default",
			}, job)
			Expect(errors.IsNotFound(err)).To(BeTrue(),
				"a parked reflash must not leave a Job behind")
		})

		// The other half of the above: once spec.firmware says what to flash, the retained approval
		// carries the reflash through on the next pass without a second admin action.
		It("should consume the approval once the reflash Job is created", func() {
			r := newTestReconciler()

			p := &intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{Name: "plan-reflash-resume"},
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					DeviceID:         "0xabcd",
					MaxRetries:       3,
					XpuSmi:           intelv1a1.XpuSmiSpec{Image: "registry/xpu-smi:latest"},
					Firmware: &intelv1a1.FirmwareSpec{
						Source: intelv1a1.FirmwareSource{
							ContainerSource: &intelv1a1.ContainerFirmwareSource{Name: "registry/fw:v2"},
						},
						File: "gfx.bin",
					},
					Approvals: []intelv1a1.RecoveryApproval{
						{ID: "app-reflash-resume", EventID: "evt-reflash-resume"},
					},
				},
				Status: intelv1a1.GPURecoveryPlanStatus{
					Events: []intelv1a1.RecoveryEvent{
						{
							ID: "evt-reflash-resume", NodeName: "node01", GPUBDF: "0000:02:00.0",
							RecoveryType: intelv1a1.RecoveryTypeSpec{Type: intelv1a1.RecoveryTypeReflash},
							Reason:       reasonSurvivability,
							// Where the spec above left the event: parked, approval still in place.
							State: intelv1a1.RecoveryEventStateMissingFirmware,
						},
					},
				},
			}

			createPlanForOwnerRef(p)

			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, &batch.Job{ObjectMeta: metav1.ObjectMeta{
					Name: "recovery-evt-reflash-resume-0", Namespace: "default",
				}})
			})

			r.processApprovals(ctx, p)

			evt := p.Status.Events[0]
			Expect(evt.State).To(Equal(intelv1a1.RecoveryEventStateInProgress),
				"missing-firmware must be re-examined once the plan is corrected; messages: %v", p.Status.Messages)
			Expect(evt.JobName).To(Equal("recovery-evt-reflash-resume-0"))
			Expect(p.Spec.Approvals[0].Consumed).To(BeTrue(),
				"the retained approval is spent by the reflash it eventually authorised")
		})
	})

	Context("Re-approval of permanently failed events", func() {
		It("should restart a failed event when an explicit EventID approval is added", func() {
			r := newTestReconciler()
			now := metav1.Now()

			p := &intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{Name: "plan-reapprove"},
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					DeviceID:         "0xabcd",
					MaxRetries:       2,
					XpuSmi:           intelv1a1.XpuSmiSpec{Image: "registry/xpu-smi:latest"},
					// The drain is off so the re-approved event lands straight in in-progress;
					// what is under test is the retry counter and the approval, not the drain.
					Drain: intelv1a1.DrainSpec{Enable: ptr.To(false)},
					Approvals: []intelv1a1.RecoveryApproval{
						{ID: "reapp-001", EventID: "evt-exhausted"},
					},
				},
				Status: intelv1a1.GPURecoveryPlanStatus{
					Events: []intelv1a1.RecoveryEvent{
						{
							ID:           "evt-exhausted",
							NodeName:     "node02",
							GPUBDF:       "0000:03:00.0",
							RecoveryType: intelv1a1.RecoveryTypeSpec{Type: intelv1a1.RecoveryTypeSBR},
							State:        intelv1a1.RecoveryEventStateFailed,
							RetryCount:   2, // exhausted
							LastUpdated:  &now,
							ApprovalID:   "old-approval",
						},
					},
				},
			}

			createPlanForOwnerRef(p)

			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, &batch.Job{ObjectMeta: metav1.ObjectMeta{
					Name: "recovery-evt-exhausted-0", Namespace: "default",
				}})
			})

			r.processApprovals(ctx, p)

			evt := &p.Status.Events[0]
			Expect(evt.RetryCount).To(BeZero(), "retry count must be reset on re-approval")
			Expect(evt.ApprovalID).To(Equal("reapp-001"))
			// Re-approval falls through into the same pass, so the Job starts immediately rather
			// than waiting for another reconcile.
			Expect(evt.State).To(Equal(intelv1a1.RecoveryEventStateInProgress))
			Expect(p.Spec.Approvals[0].Consumed).To(BeTrue())
		})

		It("should not restart a failed event via a selector approval", func() {
			r := newTestReconciler()
			now := metav1.Now()

			p := &intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{Name: "plan-no-selector-restart"},
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					DeviceID:         "0xabcd",
					MaxRetries:       2,
					XpuSmi:           intelv1a1.XpuSmiSpec{Image: "registry/xpu-smi:latest"},
					Approvals: []intelv1a1.RecoveryApproval{
						{
							ID:         "sel-approval",
							Selector:   &intelv1a1.ApprovalSelector{RecoveryType: intelv1a1.RecoveryTypeSBR},
							Persistent: true,
						},
					},
				},
				Status: intelv1a1.GPURecoveryPlanStatus{
					Events: []intelv1a1.RecoveryEvent{
						{
							ID:           "evt-perm-failed",
							NodeName:     "node03",
							GPUBDF:       "0000:04:00.0",
							RecoveryType: intelv1a1.RecoveryTypeSpec{Type: intelv1a1.RecoveryTypeSBR},
							State:        intelv1a1.RecoveryEventStateFailed,
							RetryCount:   2, // exhausted
							LastUpdated:  &now,
						},
					},
				},
			}

			r.processApprovals(ctx, p)

			// State must remain failed — a standing approval must not keep retrying a GPU that has
			// already failed its way out of the budget that same approval granted.
			Expect(p.Status.Events[0].State).To(Equal(intelv1a1.RecoveryEventStateFailed))
		})

		It("findExplicitApprovalForEvent should only match EventID approvals", func() {
			r := newTestReconciler()

			evt := &intelv1a1.RecoveryEvent{ID: "evt-xyz", State: intelv1a1.RecoveryEventStateFailed}

			// Should match an explicit EventID approval.
			p := &intelv1a1.GPURecoveryPlan{
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					Approvals: []intelv1a1.RecoveryApproval{
						{ID: "explicit-1", EventID: "evt-xyz"},
					},
				},
			}
			approval, ok := r.findExplicitApprovalForEvent(p, evt)
			Expect(ok).To(BeTrue())
			Expect(approval.ID).To(Equal("explicit-1"))

			// Should NOT match a consumed EventID approval.
			pConsumed := &intelv1a1.GPURecoveryPlan{
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					Approvals: []intelv1a1.RecoveryApproval{
						{ID: "explicit-2", EventID: "evt-xyz", Consumed: true},
					},
				},
			}
			_, ok = r.findExplicitApprovalForEvent(pConsumed, evt)
			Expect(ok).To(BeFalse())

			// Should NOT match a selector-only approval.
			pSelector := &intelv1a1.GPURecoveryPlan{
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					Approvals: []intelv1a1.RecoveryApproval{
						{
							ID:       "sel-1",
							Selector: &intelv1a1.ApprovalSelector{RecoveryType: intelv1a1.RecoveryTypeSBR},
						},
					},
				},
			}
			_, ok = r.findExplicitApprovalForEvent(pSelector, evt)
			Expect(ok).To(BeFalse())
		})
	})

	Context("pruneConsumedApprovals", func() {
		It("should remove a consumed non-persistent approval when no event references it", func() {
			p := &intelv1a1.GPURecoveryPlan{
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					Approvals: []intelv1a1.RecoveryApproval{
						{
							ID:       "consumed-gone",
							Selector: &intelv1a1.ApprovalSelector{RecoveryType: intelv1a1.RecoveryTypeSBR},
							Consumed: true,
						},
						{ID: "active", EventID: "evt-1"},
					},
				},
				// No events reference "consumed-gone".
				Status: intelv1a1.GPURecoveryPlanStatus{
					Events: []intelv1a1.RecoveryEvent{{ID: "evt-1", ApprovalID: "active"}},
				},
			}

			pruneConsumedApprovals(p)

			Expect(p.Spec.Approvals).To(HaveLen(1))
			Expect(p.Spec.Approvals[0].ID).To(Equal("active"))
		})

		It("should keep a consumed non-persistent approval while an event still references it", func() {
			p := &intelv1a1.GPURecoveryPlan{
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					Approvals: []intelv1a1.RecoveryApproval{
						{
							ID:       "sel-used",
							Selector: &intelv1a1.ApprovalSelector{RecoveryType: intelv1a1.RecoveryTypeSBR},
							Consumed: true,
						},
					},
				},
				Status: intelv1a1.GPURecoveryPlanStatus{
					Events: []intelv1a1.RecoveryEvent{
						{ID: "evt-active", ApprovalID: "sel-used", State: intelv1a1.RecoveryEventStateInProgress},
					},
				},
			}

			pruneConsumedApprovals(p)

			Expect(p.Spec.Approvals).To(HaveLen(1),
				"consumed approval must be kept while its event is still active")
		})

		It("should never prune a persistent approval", func() {
			p := &intelv1a1.GPURecoveryPlan{
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					Approvals: []intelv1a1.RecoveryApproval{
						{
							ID:         "persistent-sel",
							Selector:   &intelv1a1.ApprovalSelector{RecoveryType: intelv1a1.RecoveryTypeSBR},
							Persistent: true,
							// Consumed is deliberately true: persistent approvals are never pruned
							// whatever else is set on them.
							Consumed: true,
						},
					},
				},
				Status: intelv1a1.GPURecoveryPlanStatus{}, // no events
			}

			pruneConsumedApprovals(p)

			Expect(p.Spec.Approvals).To(HaveLen(1), "persistent approvals must never be pruned")
		})

		It("should keep unconsumed approvals regardless of event references", func() {
			p := &intelv1a1.GPURecoveryPlan{
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					Approvals: []intelv1a1.RecoveryApproval{
						{ID: "pending", EventID: "evt-future"},
					},
				},
				Status: intelv1a1.GPURecoveryPlanStatus{}, // no events yet
			}

			pruneConsumedApprovals(p)

			Expect(p.Spec.Approvals).To(HaveLen(1), "unconsumed approvals must not be pruned")
		})
	})

	Context("Helper: buildResetCommand", func() {
		const bdf = "0000:02:00.0"

		// Every reset type in the enum must map to a distinct xpu-smi invocation, or an admin's
		// override between two of them would look like a change while running the same command.
		It("should give every reset type its own distinct command", func() {
			resetTypes := []intelv1a1.RecoveryType{
				intelv1a1.RecoveryTypeSBR,
				intelv1a1.RecoveryTypeSlot,
				intelv1a1.RecoveryTypeAMC,
			}

			seen := map[string]intelv1a1.RecoveryType{}

			for _, rt := range resetTypes {
				cmd := buildResetCommand(bdf, rt)
				Expect(cmd).NotTo(BeEmpty(), "reset type %q must map to a command", rt)

				Expect(seen).NotTo(HaveKey(cmd),
					"reset types %q and %q share the command %q, so an override between them is a no-op",
					seen[cmd], rt, cmd)

				seen[cmd] = rt
			}
		})

		// The binary is named, not pathed: the plan can point spec.xpuSmi.image at any image that
		// has xpu-smi on PATH, which is not the same set of images as those that keep it in
		// /usr/local/bin.
		It("should invoke xpu-smi by name rather than by path", func() {
			Expect(buildResetCommand(bdf, intelv1a1.RecoveryTypeSlot)).To(HavePrefix("xpu-smi "))
		})

		It("should address the BDF the event names", func() {
			Expect(buildResetCommand("0000:af:00.0", intelv1a1.RecoveryTypeSBR)).
				To(ContainSubstring("0000:af:00.0"))
		})

		It("should return no command for reflash, which is not an xpu-smi reset", func() {
			Expect(buildResetCommand(bdf, intelv1a1.RecoveryTypeReflash)).To(BeEmpty())
		})

		It("should return no command for a type outside the enum", func() {
			Expect(buildResetCommand(bdf, intelv1a1.RecoveryType("flr"))).To(BeEmpty())
		})
	})

	// Both command lines interpolate the BDF into a string /bin/sh parses, so the shape of the
	// ResourceSlice attribute they come from is what stands between a free-form driver-written
	// string and a privileged root shell.
	Context("Helper: validDeviceBDF", func() {
		DescribeTable("should accept the addresses the kernel prints",
			func(bdf string) {
				Expect(validDeviceBDF(bdf)).To(BeTrue(), "%q is a PCI address", bdf)
			},
			Entry("domain zero", "0000:02:00.0"),
			Entry("non-zero domain", "0001:af:00.0"),
			// lspci prints PCI addresses in uppercase hex, so this is a likely spelling rather
			// than a pathological one.
			Entry("uppercase hex", "0000:AF:00.0"),
			Entry("highest function", "0000:00:1f.7"),
		)

		DescribeTable("should reject anything else",
			func(bdf string) {
				Expect(validDeviceBDF(bdf)).To(BeFalse(), "%q is not a PCI address", bdf)
			},
			Entry("empty", ""),
			Entry("no domain", "02:00.0"),
			Entry("no function", "0000:02:00"),
			Entry("function out of range", "0000:02:00.8"),
			Entry("non-hex digits", "0000:0g:00.0"),
			Entry("unexpected separators", "0000/02_00,0"),
			Entry("trailing separator", "0000:02:00.0:"),
			Entry("leading separator", ":02:00.0"),
			Entry("trailing newline", "0000:02:00.0\n"),
			// The one that matters: a shell metacharacter must not reach a command line run as
			// root in a privileged container.
			Entry("shell command substitution", "0000:02:00.0; touch /tmp/pwned"),
			Entry("shell pipeline", "0000:02:00.0 | sh"),
			Entry("backticks", "`id`"),
		)
	})

	// The deadline is the only clock running once a recovery Job exists: nothing in the reconcile
	// gives up on an in-progress event, so an xpu-smi that hangs on a card that has stopped answering
	// would hold the node's drain taint and the event's state indefinitely.
	Context("Recovery Job timeouts", func() {
		timeoutFor := func(rt intelv1a1.RecoveryType, t intelv1a1.RecoveryTimeoutsSpec) int64 {
			plan := &intelv1a1.GPURecoveryPlan{Spec: intelv1a1.GPURecoveryPlanSpec{Timeouts: t}}
			evt := &intelv1a1.RecoveryEvent{RecoveryType: intelv1a1.RecoveryTypeSpec{Type: rt}}

			return recoveryJobTimeout(plan, evt)
		}

		DescribeTable("should take the deadline for the kind of recovery being run",
			func(rt intelv1a1.RecoveryType, t intelv1a1.RecoveryTimeoutsSpec, want int64) {
				Expect(timeoutFor(rt, t)).To(Equal(want))
			},
			Entry("a reset from resetSeconds", intelv1a1.RecoveryTypeSlot,
				intelv1a1.RecoveryTimeoutsSpec{ResetSeconds: 45, ReflashSeconds: 900}, int64(45)),
			Entry("a reflash from reflashSeconds", intelv1a1.RecoveryTypeReflash,
				intelv1a1.RecoveryTimeoutsSpec{ResetSeconds: 45, ReflashSeconds: 900}, int64(900)),
			// Cross-wiring the two is the mistake worth pinning: a reflash cut off after a reset's
			// deadline leaves the card part-written, which is worse than the state it started in.
			Entry("a reflash when only resetSeconds is set", intelv1a1.RecoveryTypeReflash,
				intelv1a1.RecoveryTimeoutsSpec{ResetSeconds: 45}, defaultReflashJobTimeout),
			Entry("a reset when only reflashSeconds is set", intelv1a1.RecoveryTypeSlot,
				intelv1a1.RecoveryTimeoutsSpec{ReflashSeconds: 900}, defaultResetJobTimeout),
			// spec.timeouts is defaulted by the CRD, so an empty one means an object that never
			// reached the API server. A Job with no deadline at all is the one outcome that must not
			// happen: activeDeadlineSeconds is what ends a hung recovery.
			Entry("a reset with no timeouts at all", intelv1a1.RecoveryTypeSlot,
				intelv1a1.RecoveryTimeoutsSpec{}, defaultResetJobTimeout),
			Entry("a reflash with no timeouts at all", intelv1a1.RecoveryTypeReflash,
				intelv1a1.RecoveryTimeoutsSpec{}, defaultReflashJobTimeout),
		)

		// The fallbacks exist to reproduce what the object would have been given had it been
		// defaulted, so three places have to agree: these constants, the CRD defaults, and the
		// templates' own activeDeadlineSeconds. The CRD side is checked by the webhook suite; this is
		// the templates.
		DescribeTable("should fall back to the deadline the Job template carries",
			func(tmpl *batch.Job, want int64) {
				Expect(tmpl.Spec.ActiveDeadlineSeconds).To(HaveValue(Equal(want)))
			},
			Entry("reset", deployments.XpuManagerResetJob(), defaultResetJobTimeout),
			Entry("reflash", deployments.XpuManagerFWUpdateJob(), defaultReflashJobTimeout),
		)

		DescribeTable("should put the plan's deadline on the Job it creates",
			func(planName, evtID string, rt intelv1a1.RecoveryType, want int64) {
				r := newTestReconciler()

				p := &intelv1a1.GPURecoveryPlan{
					ObjectMeta: metav1.ObjectMeta{Name: planName},
					Spec: intelv1a1.GPURecoveryPlanSpec{
						DefaultResetType: intelv1a1.RecoveryTypeSlot,
						DeviceID:         "0xabcd",
						MaxRetries:       3,
						XpuSmi:           intelv1a1.XpuSmiSpec{Image: "local/xpusmi:devel"},
						Timeouts:         intelv1a1.RecoveryTimeoutsSpec{ResetSeconds: 42, ReflashSeconds: 1200},
						Firmware: &intelv1a1.FirmwareSpec{
							Source: intelv1a1.FirmwareSource{
								ContainerSource: &intelv1a1.ContainerFirmwareSource{Name: "registry/fw:v2"},
							},
							File: "gfx.bin",
						},
					},
					Status: intelv1a1.GPURecoveryPlanStatus{
						Events: []intelv1a1.RecoveryEvent{{
							ID:           evtID,
							NodeName:     "node01",
							GPUBDF:       "0000:02:00.0",
							RecoveryType: intelv1a1.RecoveryTypeSpec{Type: rt},
							State:        intelv1a1.RecoveryEventStateWaitingApproval,
							LastUpdated:  ptr.To(metav1.Now()),
						}},
					},
				}

				createPlanForOwnerRef(p)

				Expect(r.createRecoveryJob(ctx, p, &p.Status.Events[0])).To(Succeed())

				job := &batch.Job{}
				Expect(k8sClient.Get(ctx, types.NamespacedName{
					Name: recoveryJobName(evtID, 0), Namespace: "default",
				}, job)).To(Succeed())

				DeferCleanup(func() {
					_ = k8sClient.Delete(ctx, job)
				})

				Expect(job.Spec.ActiveDeadlineSeconds).To(HaveValue(Equal(want)),
					"the template's own deadline must not survive the plan's")
			},
			Entry("a reset Job", "plan-deadline-reset", "evt-deadline-reset",
				intelv1a1.RecoveryTypeSlot, int64(42)),
			Entry("a reflash Job", "plan-deadline-reflash", "evt-deadline-reflash",
				intelv1a1.RecoveryTypeReflash, int64(1200)),
		)
	})

	// Jobs are kept for as long as the event they belong to, so an admin looking at a GPU can still
	// read the pod that touched it. That is only true if the terminal-Job handling moves the name
	// into pastJobs rather than deleting the object.
	Context("Job outcomes: Jobs retained for diagnostics", func() {
		// putJob creates a Job and drives its status subresource to the given terminal condition.
		// K8s 1.36 requires startTime plus the interim condition before the terminal one.
		putJob := func(name, planName, evtID string, complete bool, reason string) {
			job := makeTestJob(name, "default", map[string]string{
				recoveryJobLabelPlan:  planName,
				recoveryJobLabelEvent: evtID,
			}, batch.JobComplete)
			job.Status = batch.JobStatus{} // status is not settable on create

			Expect(k8sClient.Create(ctx, job)).To(Succeed())

			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, job)
			})

			startTime := metav1.Now()

			if complete {
				job.Status = batch.JobStatus{
					StartTime:      &startTime,
					CompletionTime: &startTime,
					Conditions: []batch.JobCondition{
						{Type: batch.JobSuccessCriteriaMet, Status: core.ConditionTrue},
						{Type: batch.JobComplete, Status: core.ConditionTrue},
					},
				}
			} else {
				job.Status = batch.JobStatus{
					StartTime: &startTime,
					Conditions: []batch.JobCondition{
						{Type: batch.JobFailureTarget, Status: core.ConditionTrue},
						{Type: batch.JobFailed, Status: core.ConditionTrue, Reason: reason},
					},
				}
			}

			Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())
		}

		planInProgress := func(planName, evtID, jobName string) *intelv1a1.GPURecoveryPlan {
			return &intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{Name: planName},
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					DeviceID:         "0xabcd",
					MaxRetries:       3,
				},
				Status: intelv1a1.GPURecoveryPlanStatus{
					Events: []intelv1a1.RecoveryEvent{
						{
							ID:           evtID,
							NodeName:     "node01",
							GPUBDF:       "0000:02:00.0",
							RecoveryType: intelv1a1.RecoveryTypeSpec{Type: intelv1a1.RecoveryTypeSBR},
							State:        intelv1a1.RecoveryEventStateInProgress,
							JobName:      jobName,
						},
					},
				},
			}
		}

		It("should move JobName to PastJobs and not delete the Job when a Job fails", func() {
			r := newTestReconciler()
			p := planInProgress("plan-fail-diag", "evt-fail-001", "recovery-evt-fail-001-0")

			putJob("recovery-evt-fail-001-0", p.Name, "evt-fail-001", false, "BackoffLimitExceeded")

			Expect(r.syncJobStatuses(ctx, p)).To(Succeed())

			evt := &p.Status.Events[0]
			Expect(evt.State).To(Equal(intelv1a1.RecoveryEventStateFailed))
			Expect(evt.PastJobs).To(ContainElement("recovery-evt-fail-001-0"))
			Expect(evt.JobName).To(BeEmpty())
			Expect(evt.RetryCount).To(BeNumerically("==", 1))

			// The Job outlives the event, but its pods do not outlive the plan's cleanup, so the
			// verdict is copied onto the event. The attempt count is what says whether the operator
			// will try again — retryCount alone does not, without also knowing spec.maxRetries.
			Expect(evt.StateMessage).To(SatisfyAll(
				ContainSubstring("recovery-evt-fail-001-0"),
				ContainSubstring("attempt 1 of 3"),
				ContainSubstring("BackoffLimitExceeded"),
			))

			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      "recovery-evt-fail-001-0",
				Namespace: "default",
			}, &batch.Job{})).To(Succeed(), "a failed Job must remain for diagnostics until the event is removed")
		})

		It("should move JobName to PastJobs and not delete the Job when a Job succeeds", func() {
			r := newTestReconciler()
			p := planInProgress("plan-success-retain", "evt-ok-001", "recovery-evt-ok-001-0")

			putJob("recovery-evt-ok-001-0", p.Name, "evt-ok-001", true, "")

			Expect(r.syncJobStatuses(ctx, p)).To(Succeed())

			evt := &p.Status.Events[0]
			Expect(evt.State).To(Equal(intelv1a1.RecoveryEventStateSucceeded))
			Expect(evt.PastJobs).To(ContainElement("recovery-evt-ok-001-0"))
			Expect(evt.JobName).To(BeEmpty())
			Expect(evt.RetryCount).To(BeZero())

			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      "recovery-evt-ok-001-0",
				Namespace: "default",
			}, &batch.Job{})).To(Succeed(), "a succeeded Job must remain until the event is removed")
		})

		// A Job that has gone missing is not a failure of the recovery: reporting one would burn a
		// retry and could park the event in failed while the reset it started is still running.
		It("should leave an event in-progress when its Job cannot be read", func() {
			r := newTestReconciler()
			p := planInProgress("plan-job-missing", "evt-missing-001", "recovery-evt-missing-001-0")

			Expect(r.syncJobStatuses(ctx, p)).To(Succeed())

			Expect(p.Status.Events[0].State).To(Equal(intelv1a1.RecoveryEventStateInProgress))
			Expect(p.Status.Events[0].RetryCount).To(BeZero())
		})

		It("should report an in-flight Job as an active Job", func() {
			Expect(hasActiveJobs(planInProgress("plan-active", "evt-a", "job-a"))).To(BeTrue())

			done := planInProgress("plan-done", "evt-b", "")
			done.Status.Events[0].State = intelv1a1.RecoveryEventStateSucceeded
			Expect(hasActiveJobs(done)).To(BeFalse())
		})
	})

	// Recovery Jobs must be owned by the plan. Without an owner reference the Owns(&batch.Job{})
	// watch in SetupWithManager never fires, so every state transition waits out a full
	// RequeueDelay, and any Job that deleteAllJobs misses leaks.
	Context("Owner references on recovery Jobs", func() {
		// planWithEvent returns a plan (created in the API server, so it has a UID for the owner
		// reference) plus a pending event of the given recovery type.
		planWithEvent := func(planName, evtID string, rt intelv1a1.RecoveryType) *intelv1a1.GPURecoveryPlan {
			p := &intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{Name: planName},
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					DeviceID:         "0xabcd",
					MaxRetries:       3,
					XpuSmi: intelv1a1.XpuSmiSpec{
						Image: "local/xpusmi:devel",
					},
				},
				Status: intelv1a1.GPURecoveryPlanStatus{
					Events: []intelv1a1.RecoveryEvent{
						{
							ID:           evtID,
							NodeName:     "node01",
							GPUBDF:       "0000:02:00.0",
							RecoveryType: intelv1a1.RecoveryTypeSpec{Type: rt},
							State:        intelv1a1.RecoveryEventStateWaitingApproval,
							LastUpdated:  ptr.To(metav1.Now()),
						},
					},
				},
			}

			createPlanForOwnerRef(p)

			return p
		}

		// expectOwned asserts the Job carries exactly one controller reference pointing at the
		// plan, with the fields the garbage collector and the owner handler both require.
		expectOwned := func(jobName string, p *intelv1a1.GPURecoveryPlan) *batch.Job {
			job := &batch.Job{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: jobName, Namespace: "default",
			}, job)).To(Succeed())

			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, job)
			})

			Expect(job.OwnerReferences).To(HaveLen(1))

			ref := job.OwnerReferences[0]
			Expect(ref.Kind).To(Equal("GPURecoveryPlan"))
			Expect(ref.APIVersion).To(Equal(intelv1a1.GroupVersion.String()))
			Expect(ref.Name).To(Equal(p.Name))
			// A UID mismatch makes the GC treat the reference as dangling and delete the Job.
			Expect(ref.UID).To(Equal(p.UID))
			Expect(ref.Controller).To(HaveValue(BeTrue()))

			return job
		}

		It("should own a reset Job", func() {
			r := newTestReconciler()
			p := planWithEvent("plan-own-reset", "evt-own-reset", intelv1a1.RecoveryTypeSBR)

			Expect(r.createRecoveryJob(ctx, p, &p.Status.Events[0])).To(Succeed())

			job := expectOwned("recovery-evt-own-reset-0", p)
			Expect(job.Labels).To(HaveKeyWithValue(recoveryJobLabelPlan, "plan-own-reset"))
			Expect(job.Labels).To(HaveKeyWithValue(recoveryJobLabelEvent, "evt-own-reset"))
			Expect(p.Status.Events[0].State).To(Equal(intelv1a1.RecoveryEventStateInProgress))
		})

		// The reset writes to the GPU's PCIe config space through sysfs on one specific node, and
		// nothing else places the pod there: NodeName is set directly, which bypasses the
		// scheduler, so the blanket toleration is what keeps the taint manager from evicting it
		// mid-reset off a node that is already fenced off as broken.
		It("should pin the Job to the event's node and tolerate its taints", func() {
			r := newTestReconciler()
			p := planWithEvent("plan-own-pinned", "evt-own-pinned", intelv1a1.RecoveryTypeSlot)
			p.Spec.XpuSmi = intelv1a1.XpuSmiSpec{Image: "registry/xpu-smi:v1", PullPolicy: "Always"}
			p.Spec.Tolerations = []core.Toleration{{Key: "extra", Operator: core.TolerationOpExists}}

			Expect(r.createRecoveryJob(ctx, p, &p.Status.Events[0])).To(Succeed())

			job := expectOwned("recovery-evt-own-pinned-0", p)

			Expect(job.Spec.Template.Spec.NodeName).To(Equal("node01"))
			Expect(job.Spec.Template.Spec.Tolerations).To(ContainElement(
				core.Toleration{Operator: core.TolerationOpExists}))
			Expect(job.Spec.Template.Spec.Tolerations).To(ContainElement(
				core.Toleration{Key: "extra", Operator: core.TolerationOpExists}))

			resetter := containerByName(job.Spec.Template.Spec.Containers, resetJobContainer)
			Expect(resetter).NotTo(BeNil())
			Expect(resetter.Image).To(Equal("registry/xpu-smi:v1"))
			Expect(resetter.ImagePullPolicy).To(Equal(core.PullAlways))
			// The BDF has to reach the command line, not just the event. One argument, because the
			// template runs /bin/sh -c: see the reflash Job's specs for what splitting it costs.
			Expect(resetter.Command).To(Equal([]string{"/bin/sh", "-c"}))
			Expect(resetter.Args).To(Equal([]string{"xpu-smi config -d 0000:02:00.0 --coldreset"}))
		})

		It("should give the Job the operator's own pull secret", func() {
			r := newTestReconciler()
			r.Opts.SecretName = "operator-pull-secret"

			p := planWithEvent("plan-own-secret", "evt-own-secret", intelv1a1.RecoveryTypeSBR)

			Expect(r.createRecoveryJob(ctx, p, &p.Status.Events[0])).To(Succeed())

			job := expectOwned("recovery-evt-own-secret-0", p)
			Expect(job.Spec.Template.Spec.ImagePullSecrets).To(ConsistOf(
				core.LocalObjectReference{Name: "operator-pull-secret"}))
		})

		// The reference existing is not the same as it being usable. This drives the real
		// controller-runtime owner handler to prove the emitted request is what Reconcile expects:
		// a cluster-scoped owner must yield Request{Name: plan} with NO namespace, otherwise the
		// lookup would target "default/plan-..." and silently never match.
		It("should enqueue a namespace-less request for the plan on a Job event", func() {
			r := newTestReconciler()
			p := planWithEvent("plan-own-enqueue", "evt-own-enqueue", intelv1a1.RecoveryTypeSBR)

			Expect(r.createRecoveryJob(ctx, p, &p.Status.Events[0])).To(Succeed())

			job := expectOwned("recovery-evt-own-enqueue-0", p)

			h := handler.EnqueueRequestForOwner(k8sClient.Scheme(), k8sClient.RESTMapper(),
				&intelv1a1.GPURecoveryPlan{}, handler.OnlyControllerOwner())

			q := &trackingQueue{}
			h.Update(ctx, event.UpdateEvent{ObjectOld: job, ObjectNew: job}, q)

			Expect(q.added).To(ConsistOf(reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "plan-own-enqueue"},
			}))
		})

		// An earlier pass may have created the Job and lost its status write. The name embeds the
		// event ID and attempt index, so the existing Job is the very one this attempt wanted:
		// adopting it is right, and failing would strand the event in waiting-approval forever.
		It("should adopt an existing Job of the same name", func() {
			r := newTestReconciler()
			p := planWithEvent("plan-own-adopt", "evt-own-adopt", intelv1a1.RecoveryTypeSBR)

			Expect(r.createRecoveryJob(ctx, p, &p.Status.Events[0])).To(Succeed())
			expectOwned("recovery-evt-own-adopt-0", p)

			// Second attempt at the same event, still on attempt 0.
			p.Status.Events[0].State = intelv1a1.RecoveryEventStateWaitingApproval
			p.Status.Events[0].JobName = ""

			Expect(r.createRecoveryJob(ctx, p, &p.Status.Events[0])).To(Succeed())

			Expect(p.Status.Events[0].State).To(Equal(intelv1a1.RecoveryEventStateInProgress))
			Expect(p.Status.Events[0].JobName).To(Equal("recovery-evt-own-adopt-0"))
		})

		// The attempt index in the name is what keeps a retry from colliding with the Job that
		// already failed — and that Job is still there, kept for diagnostics.
		It("should name a retry after its attempt index", func() {
			r := newTestReconciler()
			p := planWithEvent("plan-own-retry", "evt-own-retry", intelv1a1.RecoveryTypeSBR)
			p.Status.Events[0].PastJobs = []string{"recovery-evt-own-retry-0"}

			Expect(r.createRecoveryJob(ctx, p, &p.Status.Events[0])).To(Succeed())

			expectOwned("recovery-evt-own-retry-1", p)
			Expect(p.Status.Events[0].JobName).To(Equal("recovery-evt-own-retry-1"))
		})
	})

	// Both createResetJob and createReflashJob hand their container exactly one argument, so both
	// templates have to keep a shell as their command. This is a contract between Go and YAML that
	// nothing else checks: a template switched back to running the binary directly would exec a whole
	// command line as argv[0] and fail inside a pod with a "no such file" naming the entire string.
	//
	// Going through a shell is also what keeps the path to xpu-smi out of the operator. The plan names
	// an image; where that image keeps its binary is its own business, so the command line invokes
	// xpu-smi by name and lets PATH resolve it.
	Context("Recovery Job templates: the shell contract", func() {
		DescribeTable("should run xpu-smi through a shell, one command line at a time",
			func(job *batch.Job, containerName string) {
				c := containerByName(job.Spec.Template.Spec.Containers, containerName)
				Expect(c).NotTo(BeNil())
				Expect(c.Command).To(Equal([]string{"/bin/sh", "-c"}))
				Expect(c.Args).To(HaveLen(1),
					"the template's own args stand in for what the operator writes, so they must be one command line too")
			},
			Entry("reset", deployments.XpuManagerResetJob(), resetJobContainer),
			Entry("reflash", deployments.XpuManagerFWUpdateJob(), reflashJobContainer),
		)
	})

	// A card in survivability mode has firmware that no reset can fix, so the recovery is to write a
	// known-good image over it. That is a different Job from a reset — a firmware image, an
	// initContainer that stages it, and a shell command line rather than an argv — and every part the
	// operator fills in fails silently inside a pod if it is filled in wrongly.
	Context("Reflash Jobs", func() {
		const (
			reflashBDF  = "0000:4b:00.0"
			reflashFile = "gfx_fw.bin"
			fwImage     = "registry.example.com/intel/gpu-fw:2026.1"
		)

		// reflashPlan creates a plan (in the API server, so the Job's owner reference has a UID)
		// carrying one reflash event ready to be acted on, so createRecoveryJob is the whole of what
		// these specs drive.
		reflashPlan := func(planName, evtID string, fw *intelv1a1.FirmwareSpec) *intelv1a1.GPURecoveryPlan {
			p := &intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{Name: planName},
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					DeviceID:         "0xabcd",
					MaxRetries:       3,
					XpuSmi:           intelv1a1.XpuSmiSpec{Image: "registry/xpu-smi:latest"},
					Firmware:         fw,
				},
				Status: intelv1a1.GPURecoveryPlanStatus{
					Events: []intelv1a1.RecoveryEvent{{
						ID:           evtID,
						NodeName:     "node07",
						GPUBDF:       reflashBDF,
						RecoveryType: intelv1a1.RecoveryTypeSpec{Type: intelv1a1.RecoveryTypeReflash},
						Reason:       reasonSurvivability,
						State:        intelv1a1.RecoveryEventStateWaitingApproval,
						LastUpdated:  ptr.To(metav1.Now()),
					}},
				},
			}

			createPlanForOwnerRef(p)

			return p
		}

		containerFirmware := func() *intelv1a1.FirmwareSpec {
			return &intelv1a1.FirmwareSpec{
				Source: intelv1a1.FirmwareSource{
					ContainerSource: &intelv1a1.ContainerFirmwareSource{Name: fwImage},
				},
				File: reflashFile,
			}
		}

		getJob := func(name string) *batch.Job {
			job := &batch.Job{}
			Expect(k8sClient.Get(ctx,
				types.NamespacedName{Name: name, Namespace: "default"}, job)).To(Succeed())

			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, job)
			})

			return job
		}

		expectNoJob := func(name string) {
			err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, &batch.Job{})
			Expect(errors.IsNotFound(err)).To(BeTrue(), "a parked reflash must not leave a Job behind")
		}

		It("should build the flash Job from the firmware-update template", func() {
			r := newTestReconciler()
			p := reflashPlan("plan-reflash-build", "evt-reflash-build", containerFirmware())
			p.Spec.XpuSmi = intelv1a1.XpuSmiSpec{Image: "registry/xpu-smi:v1", PullPolicy: "Always"}

			Expect(r.createRecoveryJob(ctx, p, &p.Status.Events[0])).To(Succeed())

			evt := p.Status.Events[0]
			Expect(evt.State).To(Equal(intelv1a1.RecoveryEventStateInProgress))
			Expect(evt.JobName).To(Equal("recovery-evt-reflash-build-0"))

			job := getJob(evt.JobName)
			Expect(job.Labels).To(HaveKeyWithValue(recoveryJobLabelPlan, "plan-reflash-build"))
			Expect(job.Labels).To(HaveKeyWithValue(recoveryJobLabelEvent, "evt-reflash-build"))
			Expect(job.Spec.Template.Spec.NodeName).To(Equal("node07"))

			// The firmware comes out of its own image, which is the initContainer's whole job.
			copyC := containerByName(job.Spec.Template.Spec.InitContainers, reflashCopyContainer)
			Expect(copyC).NotTo(BeNil())
			Expect(copyC.Image).To(Equal(fwImage))

			flashC := containerByName(job.Spec.Template.Spec.Containers, reflashJobContainer)
			Expect(flashC).NotTo(BeNil())
			Expect(flashC.Image).To(Equal("registry/xpu-smi:v1"))
			Expect(flashC.ImagePullPolicy).To(Equal(core.PullAlways))

			// The template runs /bin/sh -c, so the flash has to stay one command line: replacing the
			// command with an argv, as a reset does, would leave the shell nothing to run. The length
			// is the assertion that matters — sh -c takes its command from the first operand and turns
			// the rest into $0, $1, …, so a flash split across arguments runs a bare "xpu-smi" and
			// silently flashes nothing.
			Expect(flashC.Command).To(Equal([]string{"/bin/sh", "-c"}))
			Expect(flashC.Args).To(HaveLen(1))
			Expect(flashC.Args[0]).To(Equal(buildFDOFlashCommand(reflashBDF, reflashFile)))

			// An admin who has to check what was flashed onto which card reads this, not the pod log.
			Expect(p.Status.Messages).To(ContainElement(SatisfyAll(
				ContainSubstring(reflashBDF),
				ContainSubstring(reflashFile),
				ContainSubstring(evt.JobName),
			)))
		})

		// The two halves of the reflash are wired together by convention, not by anything either side
		// checks: the initContainer copies one directory into the staging volume, and xpu-smi is
		// handed a path under it. If the template's directories and the operator's constants drift
		// apart the Job still starts and fails minutes later, from inside a pod, on a missing file.
		It("should flash from the directory the initContainer stages into", func() {
			tmpl := deployments.XpuManagerFWUpdateJob()

			copyC := containerByName(tmpl.Spec.Template.Spec.InitContainers, reflashCopyContainer)
			Expect(copyC).NotTo(BeNil())
			Expect(copyC.Args).To(HaveLen(1))
			Expect(copyC.Args[0]).To(SatisfyAll(
				ContainSubstring(firmwareImageDir),
				ContainSubstring(reflashStagingDir),
			), "the copy must read where firmwareImagePath looks and write where the flash reads")

			// mountedAt returns the name of the volume the container sees at the given path, so the
			// two containers can be shown to be talking about the same emptyDir.
			mountedAt := func(c *core.Container, path string) string {
				for i := range c.VolumeMounts {
					if c.VolumeMounts[i].MountPath == path {
						return c.VolumeMounts[i].Name
					}
				}

				return ""
			}

			flashC := containerByName(tmpl.Spec.Template.Spec.Containers, reflashJobContainer)
			Expect(flashC).NotTo(BeNil())
			Expect(mountedAt(copyC, reflashStagingDir)).NotTo(BeEmpty())
			Expect(mountedAt(flashC, reflashStagingDir)).To(Equal(mountedAt(copyC, reflashStagingDir)))

			Expect(buildFDOFlashCommand(reflashBDF, reflashFile)).To(
				ContainSubstring(reflashStagingDir + "/" + reflashFile))
			Expect(firmwareImagePath(reflashFile)).To(Equal(firmwareImageDir + "/" + reflashFile))
		})

		// -y because there is no terminal to answer the prompt on, and --force because the card is in
		// survivability mode: xpu-smi otherwise declines to write an image it judges no newer than
		// what is on the device, and what is on the device is exactly what has to go.
		It("should force an unattended FDO flash", func() {
			Expect(buildFDOFlashCommand("0000:02:00.0", "fw.bin")).To(
				Equal("xpu-smi updatefw -d 0000:02:00.0 -t FDO -f /update/fw.bin -y --force"))
		})

		DescribeTable("should park in missing-firmware rather than build an unusable Job",
			func(planName, evtID string, fw *intelv1a1.FirmwareSpec, wantMsg string) {
				r := newTestReconciler()
				p := reflashPlan(planName, evtID, fw)

				// Not an error: nothing has gone wrong in the cluster, the plan is simply not
				// finished. Returning one would put the whole reconcile into backoff and pile the
				// same message into status.errors on every retry.
				Expect(r.createRecoveryJob(ctx, p, &p.Status.Events[0])).To(Succeed())

				evt := p.Status.Events[0]
				Expect(evt.State).To(Equal(intelv1a1.RecoveryEventStateMissingFirmware))
				Expect(evt.JobName).To(BeEmpty())
				Expect(evt.StateMessage).To(ContainSubstring(wantMsg))
				Expect(p.Status.Messages).To(ContainElement(ContainSubstring(wantMsg)))

				expectNoJob(recoveryJobName(evtID, 0))
			},
			Entry("no firmware at all", "plan-reflash-nofw", "evt-reflash-nofw",
				nil, "spec.firmware"),
			Entry("a source but no file", "plan-reflash-nofile", "evt-reflash-nofile",
				&intelv1a1.FirmwareSpec{
					Source: intelv1a1.FirmwareSource{
						ContainerSource: &intelv1a1.ContainerFirmwareSource{Name: fwImage},
					},
				}, "spec.firmware"),
			// volumeSource is accepted by the CRD but not acted on: the reflash Job copies firmware
			// out of a container image. Parking says so rather than building a Job whose
			// initContainer would copy from an image that holds no firmware.
			Entry("a volume source, which is not implemented", "plan-reflash-vol", "evt-reflash-vol",
				&intelv1a1.FirmwareSpec{
					Source: intelv1a1.FirmwareSource{
						VolumeSource: &intelv1a1.VolumeFirmwareSource{Name: "fw-pvc"},
					},
					File: reflashFile,
				}, "containerSource"),
		)
	})

	// Every image a recovery Job pulls is checked against its registry first. A Job pinned to an
	// image that does not resolve is not a fast failure: it reports in-progress while its pod sits in
	// ImagePullBackOff until activeDeadlineSeconds expires, so a mistyped reference reads as minutes
	// of recovery followed by a failure that names the Job rather than the typo.
	Context("Pre-flight image verification", func() {
		const (
			imgBDF  = "0000:5e:00.0"
			imgFile = "fdo.bin"
			imgFW   = "registry.example.com/fw:1.0"
			imgSmi  = "registry.example.com/xpu-smi:1.0"
		)

		// imgPlan creates a plan whose single event is approved by a blanket EventID approval and
		// whose drain is off, so processApprovals runs the gate and then goes straight to the Job.
		imgPlan := func(planName, evtID string, rt intelv1a1.RecoveryType) *intelv1a1.GPURecoveryPlan {
			p := &intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{Name: planName},
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					DeviceID:         "0xabcd",
					MaxRetries:       3,
					Drain:            intelv1a1.DrainSpec{Enable: ptr.To(false)},
					XpuSmi:           intelv1a1.XpuSmiSpec{Image: imgSmi},
					Approvals:        []intelv1a1.RecoveryApproval{{ID: "app-" + evtID, EventID: evtID}},
				},
				Status: intelv1a1.GPURecoveryPlanStatus{
					Events: []intelv1a1.RecoveryEvent{{
						ID:           evtID,
						NodeName:     "node11",
						GPUBDF:       imgBDF,
						RecoveryType: intelv1a1.RecoveryTypeSpec{Type: rt},
						State:        intelv1a1.RecoveryEventStateWaitingApproval,
						LastUpdated:  ptr.To(metav1.Now()),
					}},
				},
			}

			if rt == intelv1a1.RecoveryTypeReflash {
				p.Spec.Firmware = &intelv1a1.FirmwareSpec{
					Source: intelv1a1.FirmwareSource{
						ContainerSource: &intelv1a1.ContainerFirmwareSource{Name: imgFW},
					},
					File: imgFile,
				}
			}

			createPlanForOwnerRef(p)

			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, &batch.Job{ObjectMeta: metav1.ObjectMeta{
					Name: recoveryJobName(evtID, 0), Namespace: "default",
				}})
			})

			return p
		}

		It("should check the xpu-smi image before creating a reset Job", func() {
			fake := &fakeContentImageVerifier{}
			r := newTestReconcilerVerifying(fake)
			r.Opts.SecretName = "operator-pull-secret"

			p := imgPlan("plan-img-reset", "evt-img-reset", intelv1a1.RecoveryTypeSlot)

			r.processApprovals(ctx, p)

			Expect(p.Status.Events[0].State).To(Equal(intelv1a1.RecoveryEventStateInProgress))

			// No Files: nothing inside the xpu-smi image is the operator's business, and a content
			// check would stream hundreds of megabytes to learn nothing. The operator's own pull
			// secret has to be there, or a private registry answers "unauthorized" for an image the
			// kubelet would have pulled perfectly well.
			Expect(fake.requests).To(ConsistOf(ImageVerifyRequest{
				Image:      imgSmi,
				PullSecret: "operator-pull-secret",
			}))
		})

		It("should check the firmware image for its file as well as its existence", func() {
			fake := &fakeContentImageVerifier{}
			r := newTestReconcilerVerifying(fake)

			p := imgPlan("plan-img-reflash", "evt-img-reflash", intelv1a1.RecoveryTypeReflash)

			r.processApprovals(ctx, p)

			Expect(p.Status.Events[0].State).To(Equal(intelv1a1.RecoveryEventStateInProgress))
			Expect(fake.requests).To(HaveLen(2))

			// An image that exists but does not carry spec.firmware.file fails the same way a missing
			// image does, only later and inside the initContainer, where the diagnostic is a shell
			// error in a pod log. No checksum is asked for: the plan does not declare one.
			Expect(fake.requests[1]).To(Equal(ImageVerifyRequest{
				Image: imgFW,
				Files: []ImageFile{{Name: firmwareImagePath(imgFile)}},
			}))
		})

		// The xpu-smi image and the firmware image can live on different registries, only one of
		// which serves a certificate the operator cannot chase. Folding the two opt-outs together
		// would silently widen whichever one the admin did not ask for.
		It("should carry each image's own TLS opt-out", func() {
			fake := &fakeContentImageVerifier{}
			r := newTestReconcilerVerifying(fake)

			p := imgPlan("plan-img-tls", "evt-img-tls", intelv1a1.RecoveryTypeReflash)
			p.Spec.Firmware.Source.ContainerSource.InsecureSkipTLSVerify = true

			r.processApprovals(ctx, p)

			Expect(fake.requests).To(HaveLen(2))
			Expect(fake.requests[0].InsecureSkipTLSVerify).To(BeFalse())
			Expect(fake.requests[1].InsecureSkipTLSVerify).To(BeTrue())
		})

		// Pull policy Never means the kubelet never contacts a registry: the image is on the node
		// already, put there by something outside Kubernetes. Verifying it against a registry that
		// may not even hold it would park a recovery whose image is sitting right there, leaving the
		// card broken.
		It("should not check an image the kubelet will not pull", func() {
			fake := &fakeContentImageVerifier{err: fmt.Errorf("not in any registry")}
			r := newTestReconcilerVerifying(fake)

			p := imgPlan("plan-img-never", "evt-img-never", intelv1a1.RecoveryTypeSlot)
			p.Spec.XpuSmi.PullPolicy = string(core.PullNever)

			r.processApprovals(ctx, p)

			Expect(fake.requests).To(BeEmpty())
			Expect(p.Status.Events[0].State).To(Equal(intelv1a1.RecoveryEventStateInProgress))
		})

		// The pull policy is spec.xpuSmi's; the firmware image is pulled by the initContainer at
		// whatever the template says. A preloaded xpu-smi therefore says nothing about the firmware.
		It("should still check the firmware image when xpu-smi is preloaded", func() {
			fake := &fakeContentImageVerifier{}
			r := newTestReconcilerVerifying(fake)

			p := imgPlan("plan-img-never-fw", "evt-img-never-fw", intelv1a1.RecoveryTypeReflash)
			p.Spec.XpuSmi.PullPolicy = string(core.PullNever)

			r.processApprovals(ctx, p)

			Expect(fake.requests).To(HaveLen(1))
			Expect(fake.requests[0].Image).To(Equal(imgFW))
		})

		It("should skip the check entirely when the plan asks it to", func() {
			fake := &fakeContentImageVerifier{err: fmt.Errorf("registry unreachable")}
			r := newTestReconcilerVerifying(fake)

			p := imgPlan("plan-img-skip", "evt-img-skip", intelv1a1.RecoveryTypeSlot)
			p.Spec.SkipImageVerification = true

			r.processApprovals(ctx, p)

			Expect(fake.requests).To(BeEmpty())
			Expect(p.Status.Events[0].State).To(Equal(intelv1a1.RecoveryEventStateInProgress),
				"an air-gapped cluster must be able to opt out and still recover its GPUs")
		})

		It("should hold the recovery and keep the approval when an image cannot be pulled", func() {
			fake := &fakeContentImageVerifier{err: fmt.Errorf("MANIFEST_UNKNOWN")}
			r := newTestReconcilerVerifying(fake)

			p := imgPlan("plan-img-hold", "evt-img-hold", intelv1a1.RecoveryTypeSlot)

			r.processApprovals(ctx, p)

			evt := p.Status.Events[0]
			Expect(evt.State).To(Equal(intelv1a1.RecoveryEventStateWaitingApproval))
			Expect(evt.JobName).To(BeEmpty())

			// The state says "waiting for an approval" while an approval is sitting right there, so
			// the message is the only thing that tells an admin what is actually wrong, and it has to
			// name the field to correct as well as the failure.
			Expect(evt.StateMessage).To(SatisfyAll(
				ContainSubstring("spec.xpuSmi.image"),
				ContainSubstring(imgSmi),
				ContainSubstring("MANIFEST_UNKNOWN"),
			))
			Expect(evt.ImageVerifyGeneration).To(Equal(p.Generation))

			Expect(p.Spec.Approvals[0].Consumed).To(BeFalse(),
				"correcting the reference must be enough; the admin must not have to approve twice")
			Expect(p.Status.Messages).To(ContainElement(ContainSubstring("spec.xpuSmi.image")))

			err := k8sClient.Get(ctx, types.NamespacedName{
				Name: recoveryJobName("evt-img-hold", 0), Namespace: "default",
			}, &batch.Job{})
			Expect(errors.IsNotFound(err)).To(BeTrue(), "no Job may exist for a held recovery")
		})

		// What failed is a value in the plan, and only an admin edit can change that answer — which
		// is also what advances metadata.generation. Retrying on the reconcile cadence would re-ask a
		// question the spec has already settled, and append a message every time.
		It("should not re-check or re-report until the plan changes", func() {
			fake := &fakeContentImageVerifier{err: fmt.Errorf("MANIFEST_UNKNOWN")}
			r := newTestReconcilerVerifying(fake)

			p := imgPlan("plan-img-once", "evt-img-once", intelv1a1.RecoveryTypeSlot)

			r.processApprovals(ctx, p)
			afterFirst := len(p.Status.Messages)

			r.processApprovals(ctx, p)
			r.processApprovals(ctx, p)

			Expect(fake.requests).To(HaveLen(1), "one registry round trip per spec version")
			Expect(p.Status.Messages).To(HaveLen(afterFirst))
		})

		It("should resume the recovery once the plan is corrected", func() {
			failing := &fakeContentImageVerifier{err: fmt.Errorf("MANIFEST_UNKNOWN")}
			p := imgPlan("plan-img-resume", "evt-img-resume", intelv1a1.RecoveryTypeSlot)

			newTestReconcilerVerifying(failing).processApprovals(ctx, p)
			Expect(p.Status.Events[0].ImageVerifyGeneration).To(Equal(p.Generation))

			// What an admin fixing the reference does: a spec write, which the API server answers
			// with a new generation. The approval is still there, unconsumed.
			p.Spec.XpuSmi.Image = "registry.example.com/xpu-smi:1.1"
			p.Generation++

			passing := &fakeContentImageVerifier{}
			newTestReconcilerVerifying(passing).processApprovals(ctx, p)

			evt := p.Status.Events[0]
			Expect(evt.State).To(Equal(intelv1a1.RecoveryEventStateInProgress),
				"messages: %v", p.Status.Messages)
			Expect(evt.ImageVerifyGeneration).To(BeZero(),
				"a cleared hold must not make the next failure look like an old one")
			Expect(evt.StateMessage).To(BeEmpty(),
				"a stale explanation on a running recovery points at a problem that is gone")
			Expect(passing.requests).To(HaveLen(1))
			Expect(passing.requests[0].Image).To(Equal("registry.example.com/xpu-smi:1.1"))

			// The transition out of a hold is worth a line: the plan's own history is where an admin
			// checks whether their edit was the one that worked.
			Expect(p.Status.Messages).To(ContainElement(ContainSubstring("resuming")))
			Expect(p.Spec.Approvals[0].Consumed).To(BeTrue())
		})

		// A block clears by itself within minutes, so holding the node through one is cheap. An
		// unpullable image waits on a person and may wait indefinitely, and a NoSchedule taint parked
		// on a working node for the lifetime of a config typo takes real capacity out of the cluster.
		It("should not cordon the node while a recovery is held on an image", func() {
			const heldNode = "img-held-node"

			node := &core.Node{ObjectMeta: metav1.ObjectMeta{Name: heldNode}}
			Expect(k8sClient.Create(ctx, node)).To(Succeed())
			DeferCleanup(func() {
				fresh := &core.Node{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: heldNode}, fresh); err == nil {
					fresh.Spec.Taints = nil
					_ = k8sClient.Update(ctx, fresh)
					_ = k8sClient.Delete(ctx, fresh)
				}
			})

			putTaintedSlice(ctx, "slice-img-held", heldNode, "0x1234", imgBDF, deviceTaintKeyReset)

			p := &intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{
					Name: "plan-img-nocordon", Finalizers: []string{recoveryPlanFinalizer},
				},
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					DeviceID:         "0x1234",
					MaxRetries:       3,
					// The drain is on, which is what makes this worth asserting: a reset would
					// normally taint the node on its way to the Job.
					Drain:  intelv1a1.DrainSpec{Enable: ptr.To(true), TimeoutSeconds: 300},
					XpuSmi: intelv1a1.XpuSmiSpec{Image: imgSmi},
					Approvals: []intelv1a1.RecoveryApproval{{
						ID:       "app-img-nocordon",
						Selector: &intelv1a1.ApprovalSelector{RecoveryType: intelv1a1.RecoveryTypeSlot},
					}},
				},
			}
			Expect(k8sClient.Create(ctx, p)).To(Succeed())
			DeferCleanup(func() {
				fresh := &intelv1a1.GPURecoveryPlan{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: p.Name}, fresh); err == nil {
					fresh.Finalizers = nil
					_ = k8sClient.Update(ctx, fresh)
					_ = k8sClient.Delete(ctx, fresh)
				}
			})

			_, err := reconcilePlanVerifying(ctx, p.Name,
				&fakeContentImageVerifier{err: fmt.Errorf("MANIFEST_UNKNOWN")})
			Expect(err).NotTo(HaveOccurred())

			updated := &intelv1a1.GPURecoveryPlan{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: p.Name}, updated)).To(Succeed())
			Expect(updated.Status.Events).To(HaveLen(1))
			Expect(updated.Status.Events[0].State).To(Equal(intelv1a1.RecoveryEventStateWaitingApproval))
			Expect(updated.Status.Events[0].DrainStartedAt).To(BeNil())

			// The plan must say it needs attention: waiting-approval alone would read as normal.
			Expect(updated.Status.State).To(Equal(intelv1a1.PlanStateError))

			// Only the operator's own taint is asserted on: envtest runs no kubelet, so the Node
			// carries node.kubernetes.io/not-ready of its own accord.
			fresh := &core.Node{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: heldNode}, fresh)).To(Succeed())
			Expect(fresh.Spec.Taints).NotTo(ContainElement(HaveField("Key", recoveryTaintKey)),
				"a recovery that cannot start must not take the node out of service")
		})
	})

	// Recovery Jobs are privileged, run as root and mount hostPath /sys. On OpenShift the default
	// restricted SCC rejects that at admission, so without an SCC of their own the recovery never
	// happens — and the event is already marked in-progress against a pod that never runs.
	Context("OpenShift SCC handling", func() {
		// sccNames returns the SCC/Role/Binding/SA quadruple for a plan and registers cleanup for
		// all four: they are cluster-scoped (bar the SA) and outlive the plan otherwise.
		sccNames := func(planName string) (string, string, string, string) {
			sccName, roleName, bindingName, saName := buildOpenShiftNames(planName, recoveryResourcePart)

			DeferCleanup(func() {
				deleteOpenShiftSCCResources(context.Background(), k8sClient,
					sccName, roleName, bindingName, saName, "default")
			})

			return sccName, roleName, bindingName, saName
		}

		getSCC := func(sccName string) (*unstructured.Unstructured, error) {
			scc := &unstructured.Unstructured{}
			scc.SetAPIVersion(sccAPIVersion)
			scc.SetKind(sccKind)

			return scc, k8sClient.Get(ctx, types.NamespacedName{Name: sccName}, scc)
		}

		// planWithEvent builds a plan carrying one event in waiting-approval, ready for a direct
		// createRecoveryJob call.
		planWithEvent := func(planName, evtID string, rt intelv1a1.RecoveryType) *intelv1a1.GPURecoveryPlan {
			reason := reasonWedged
			if rt == intelv1a1.RecoveryTypeReflash {
				reason = reasonSurvivability
			}

			return &intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{Name: planName},
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					DeviceID:         "0xabcd",
					MaxRetries:       3,
					XpuSmi:           intelv1a1.XpuSmiSpec{Image: "local/xpusmi:devel"},
				},
				Status: intelv1a1.GPURecoveryPlanStatus{
					Events: []intelv1a1.RecoveryEvent{{
						ID:           evtID,
						NodeName:     "node01",
						GPUBDF:       "0000:02:00.0",
						Reason:       reason,
						RecoveryType: intelv1a1.RecoveryTypeSpec{Type: rt},
						State:        intelv1a1.RecoveryEventStateWaitingApproval,
						LastUpdated:  ptr.To(metav1.Now()),
					}},
				},
			}
		}

		expectJobServiceAccount := func(jobName, want string) {
			job := &batch.Job{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: jobName, Namespace: "default"}, job)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(context.Background(), job)
			})

			Expect(job.Spec.Template.Spec.ServiceAccountName).To(Equal(want))
		}

		It("should create the SCC quadruple on reconcile", func() {
			r := newOpenShiftTestReconciler()
			key := types.NamespacedName{Name: "plan-openshift-scc"}
			sccName, roleName, bindingName, saName := sccNames(key.Name)

			createPlanForOwnerRef(&intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{Name: key.Name},
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					DeviceID:         "0xabcd",
					MaxRetries:       3,
				},
			})

			// The first reconcile only adds the finalizer; the second runs the phases.
			for range 2 {
				_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
				Expect(err).NotTo(HaveOccurred())
			}

			scc, err := getSCC(sccName)
			Expect(err).NotTo(HaveOccurred())
			Expect(scc.Object["allowPrivilegedContainer"]).To(BeTrue())

			role := &rbac.ClusterRole{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: roleName}, role)).To(Succeed())
			Expect(role.Rules[0].Verbs).To(ContainElement("use"))
			Expect(role.Rules[0].ResourceNames).To(ContainElement(sccName),
				"the Role must grant use of this plan's own SCC, not of every SCC in the cluster")

			binding := &rbac.ClusterRoleBinding{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: bindingName}, binding)).To(Succeed())
			Expect(binding.Subjects).To(ConsistOf(rbac.Subject{
				Kind: "ServiceAccount", Name: saName, Namespace: "default",
			}))

			Expect(k8sClient.Get(ctx,
				types.NamespacedName{Name: saName, Namespace: "default"}, &core.ServiceAccount{})).To(Succeed())
		})

		It("should not create any SCC resources on a plain Kubernetes cluster", func() {
			r := newTestReconciler()
			key := types.NamespacedName{Name: "plan-vanilla-k8s"}
			sccName, _, _, saName := sccNames(key.Name)

			createPlanForOwnerRef(&intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{Name: key.Name},
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					DeviceID:         "0xabcd",
					MaxRetries:       3,
				},
			})

			for range 2 {
				_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
				Expect(err).NotTo(HaveOccurred())
			}

			_, err := getSCC(sccName)
			Expect(err).To(Satisfy(errors.IsNotFound))
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: saName, Namespace: "default"},
				&core.ServiceAccount{})).To(Satisfy(errors.IsNotFound),
				"a cluster without security.openshift.io must not get OpenShift-only objects")
		})

		// Every reconcile calls it, so it has to be a no-op once the objects are there.
		It("should be idempotent across reconciles", func() {
			r := newOpenShiftTestReconciler()
			sccNames("plan-scc-idem")

			for range 3 {
				Expect(r.ensureOpenShiftResources(ctx, "plan-scc-idem")).To(Succeed())
			}
		})

		// SCC admission picks the constraint from the pod's ServiceAccount. A Job created without
		// one falls back to restricted and is refused for being privileged.
		It("should run reset Job pods under the SCC ServiceAccount", func() {
			r := newOpenShiftTestReconciler()
			_, _, _, saName := sccNames("plan-scc-job")

			p := planWithEvent("plan-scc-job", "evt-scc-job", intelv1a1.RecoveryTypeSBR)
			createPlanForOwnerRef(p)

			Expect(r.createRecoveryJob(ctx, p, &p.Status.Events[0])).To(Succeed())
			expectJobServiceAccount("recovery-evt-scc-job-0", saName)
		})

		// The reflash Job is built from the other template and by another function, so the SA has
		// to be asserted on both: prepareRecoveryJob is what they share, and it is where it is set.
		It("should run reflash Job pods under the SCC ServiceAccount too", func() {
			r := newOpenShiftTestReconciler()
			_, _, _, saName := sccNames("plan-scc-reflash")

			p := planWithEvent("plan-scc-reflash", "evt-scc-reflash", intelv1a1.RecoveryTypeReflash)
			p.Spec.Firmware = &intelv1a1.FirmwareSpec{
				Source: intelv1a1.FirmwareSource{
					ContainerSource: &intelv1a1.ContainerFirmwareSource{Name: "registry/fw:v1"},
				},
				File: "gfx.bin",
			}
			createPlanForOwnerRef(p)

			Expect(r.createRecoveryJob(ctx, p, &p.Status.Events[0])).To(Succeed())
			expectJobServiceAccount("recovery-evt-scc-reflash-0", saName)
		})

		It("should not set a ServiceAccount on a plain Kubernetes cluster", func() {
			r := newTestReconciler()

			p := planWithEvent("plan-no-sa", "evt-no-sa", intelv1a1.RecoveryTypeSBR)
			createPlanForOwnerRef(p)

			Expect(r.createRecoveryJob(ctx, p, &p.Status.Events[0])).To(Succeed())
			expectJobServiceAccount("recovery-evt-no-sa-0", "")
		})

		// The quadruple is cluster-scoped (bar the SA) and not owned by the plan, so nothing
		// garbage-collects it. A stale SCC left behind keeps granting privileges to a
		// ServiceAccount name a later, unrelated plan of the same name would reuse.
		It("should delete the SCC quadruple when the plan is deleted", func() {
			r := newOpenShiftTestReconciler()
			key := types.NamespacedName{Name: "plan-scc-delete"}
			sccName, roleName, bindingName, saName := sccNames(key.Name)

			p := &intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{Name: key.Name, Finalizers: []string{recoveryPlanFinalizer}},
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					DeviceID:         "0xabcd",
					MaxRetries:       3,
				},
			}
			Expect(k8sClient.Create(ctx, p)).To(Succeed())

			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			_, err = getSCC(sccName)
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Delete(ctx, p)).To(Succeed())

			// The deletion path runs first and removes the finalizer, so the object is gone once
			// this reconcile returns.
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			_, err = getSCC(sccName)
			Expect(err).To(Satisfy(errors.IsNotFound))
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: roleName},
				&rbac.ClusterRole{})).To(Satisfy(errors.IsNotFound))
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: bindingName},
				&rbac.ClusterRoleBinding{})).To(Satisfy(errors.IsNotFound))
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: saName, Namespace: "default"},
				&core.ServiceAccount{})).To(Satisfy(errors.IsNotFound))
		})

		// Two plans in one cluster must not share an SCC: deleting one would otherwise strip the
		// other's recovery Jobs of the grounds they were admitted on, mid-reset.
		It("should name the SCC quadruple per plan", func() {
			sccA, roleA, bindingA, saA := buildOpenShiftNames("plan-a", recoveryResourcePart)
			sccB, roleB, bindingB, saB := buildOpenShiftNames("plan-b", recoveryResourcePart)

			Expect([]string{sccA, roleA, bindingA, saA}).NotTo(ContainElements(sccB, roleB, bindingB, saB))
		})
	})

	Context("Reconcile: long node names still produce a creatable Job", func() {
		It("should create the recovery Job for a node name well over the limit", func() {
			r := newTestReconciler()
			key := types.NamespacedName{Name: "plan-long-node"}

			// 62 characters on its own — longer than the whole Job-name budget, and the kind of
			// name a real cloud provider hands out.
			const longNode = "ip-10-0-134-22.us-west-2.compute.internal.example-cluster.prod"

			// The Node has to exist for the pre-reset drain to taint it. Nothing here is about
			// draining — the node carries no pods, so the drain converges on its first pass — but
			// without the object the drain errors and the event never reaches a Job, which would
			// fail this spec for an unrelated reason.
			node := &core.Node{ObjectMeta: metav1.ObjectMeta{Name: longNode}}
			Expect(k8sClient.Create(ctx, node)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, node)
			})

			slice := &resv1.ResourceSlice{
				ObjectMeta: metav1.ObjectMeta{Name: "slice-long-node"},
				Spec: resv1.ResourceSliceSpec{
					Driver:   "gpu.intel.com",
					NodeName: ptr.To(longNode),
					Pool:     resv1.ResourcePool{Name: "pool-long-node", ResourceSliceCount: 1},
					Devices: []resv1.Device{{
						Name: "dev-0000-af-00-0",
						Attributes: map[resv1.QualifiedName]resv1.DeviceAttribute{
							deviceAttrDeviceID: {StringValue: ptr.To("0xbeef")},
							// Uppercase hex, as lspci prints it: this must be sanitized rather
							// than rejected.
							deviceAttrBDF: {StringValue: ptr.To("0000:AF:00.0")},
						},
						Taints: []resv1.DeviceTaint{
							{Key: deviceTaintKeyReset, Effect: resv1.DeviceTaintEffectNoSchedule},
						},
					}},
				},
			}
			Expect(k8sClient.Create(ctx, slice)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, slice)
			})

			p := &intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{Name: key.Name, Finalizers: []string{recoveryPlanFinalizer}},
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					DeviceID:         "0xbeef",
					MaxRetries:       3,
					XpuSmi:           intelv1a1.XpuSmiSpec{Image: "registry/xpu-smi:latest"},
					Approvals: []intelv1a1.RecoveryApproval{{
						ID:       "app-any-reset",
						Selector: &intelv1a1.ApprovalSelector{RecoveryType: intelv1a1.RecoveryTypeSlot},
					}},
				},
			}
			Expect(k8sClient.Create(ctx, p)).To(Succeed())
			DeferCleanup(func() {
				fresh := &intelv1a1.GPURecoveryPlan{}
				if err := k8sClient.Get(ctx, key, fresh); err == nil {
					fresh.Finalizers = nil
					_ = k8sClient.Update(ctx, fresh)
					_ = k8sClient.Delete(ctx, fresh)
				}
			})

			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			updated := &intelv1a1.GPURecoveryPlan{}
			Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
			Expect(updated.Status.Events).To(HaveLen(1))

			evt := updated.Status.Events[0]

			// in-progress with a jobName is the proof: had the create been rejected, the event
			// would still be waiting-approval with no Job, and the only trace would be a status
			// message.
			Expect(evt.State).To(Equal(intelv1a1.RecoveryEventStateInProgress),
				"event should be in-progress; messages: %v", updated.Status.Messages)
			Expect(evt.JobName).NotTo(BeEmpty())

			job := &batch.Job{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: evt.JobName, Namespace: "default"}, job)).
				To(Succeed(), "the Job the event claims to own must actually exist")

			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, job)
			})

			// The label selector deleteEventJobs uses is subject to the same 63-byte cap as the
			// name, so read it back off the created object rather than trusting it.
			Expect(job.Labels[recoveryJobLabelEvent]).To(Equal(evt.ID))
		})
	})

	// A GPU whose taint is still there after a failed attempt gets another go, up to
	// spec.maxRetries. The event ID is reused, so a standing group approval re-approves the same
	// event rather than fanning one broken GPU out into a flood of events.
	Context("Helper: requeueFailedEvents", func() {
		failedPlan := func(maxRetries int32, retryCount int32) *intelv1a1.GPURecoveryPlan {
			return &intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{Name: "plan-requeue"},
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					DeviceID:         "0xabcd",
					MaxRetries:       maxRetries,
				},
				Status: intelv1a1.GPURecoveryPlanStatus{
					Events: []intelv1a1.RecoveryEvent{
						{
							ID:           "evt-requeue",
							NodeName:     "node01",
							GPUBDF:       "0000:02:00.0",
							RecoveryType: intelv1a1.RecoveryTypeSpec{Type: intelv1a1.RecoveryTypeSBR},
							State:        intelv1a1.RecoveryEventStateFailed,
							RetryCount:   retryCount,
							PastJobs:     []string{"recovery-evt-requeue-0"},
							LastUpdated:  ptr.To(metav1.Now()),
						},
					},
				},
			}
		}

		stillTainted := map[deviceKey]deviceNeed{
			{node: "node01", bdf: "0000:02:00.0"}: {rt: intelv1a1.RecoveryTypeSBR, reason: reasonWedged},
		}

		It("should send a failed event back to waiting-approval while the taint persists", func() {
			p := failedPlan(3, 1)

			requeueFailedEvents(p, stillTainted)

			evt := p.Status.Events[0]
			Expect(evt.State).To(Equal(intelv1a1.RecoveryEventStateWaitingApproval))
			Expect(evt.ID).To(Equal("evt-requeue"), "the ID must be reused so a standing approval re-matches")
			Expect(evt.RetryCount).To(BeNumerically("==", 1), "the budget is spent by the failure, not by the re-queue")
			Expect(evt.PastJobs).To(ContainElement("recovery-evt-requeue-0"))
			Expect(evt.StateMessage).To(ContainSubstring("re-queued"))
			Expect(p.Status.Messages).To(ContainElement(ContainSubstring("re-queued for retry 1/3")))
		})

		It("should leave an event alone once its retry budget is spent", func() {
			p := failedPlan(2, 2)

			requeueFailedEvents(p, stillTainted)

			Expect(p.Status.Events[0].State).To(Equal(intelv1a1.RecoveryEventStateFailed))
			Expect(p.Status.Messages).To(BeEmpty())
		})

		// maxRetries: 0 turns automatic retrying off entirely, which has to hold on the very first
		// failure rather than allowing one free attempt.
		It("should not retry at all when maxRetries is zero", func() {
			p := failedPlan(0, 0)

			requeueFailedEvents(p, stillTainted)

			Expect(p.Status.Events[0].State).To(Equal(intelv1a1.RecoveryEventStateFailed))
		})

		It("should leave a failed event whose taint has cleared for removeResolvedEvents", func() {
			p := failedPlan(3, 1)

			requeueFailedEvents(p, map[deviceKey]deviceNeed{})

			Expect(p.Status.Events[0].State).To(Equal(intelv1a1.RecoveryEventStateFailed))
		})

		It("should ignore events in any other state", func() {
			p := failedPlan(3, 0)
			p.Status.Events[0].State = intelv1a1.RecoveryEventStateInProgress

			requeueFailedEvents(p, stillTainted)

			Expect(p.Status.Events[0].State).To(Equal(intelv1a1.RecoveryEventStateInProgress))
		})
	})

	// An event with a Job in flight is the only record of that Job. Both phases that would
	// otherwise rewrite or drop it have to leave it alone until syncJobStatuses has resolved it.
	Context("In-progress events are not disturbed", func() {
		var r *GPURecoveryPlanReconciler

		BeforeEach(func() {
			r = newTestReconciler()
		})

		It("should defer an escalation while a Job is in flight", func() {
			p := &intelv1a1.GPURecoveryPlan{ObjectMeta: metav1.ObjectMeta{Name: "plan-esc-inflight"}}
			evt := &intelv1a1.RecoveryEvent{
				ID:           "evt-esc-inflight",
				NodeName:     "node01",
				GPUBDF:       "0000:02:00.0",
				RecoveryType: intelv1a1.RecoveryTypeSpec{Type: intelv1a1.RecoveryTypeSlot},
				State:        intelv1a1.RecoveryEventStateInProgress,
				JobName:      "recovery-evt-esc-inflight-0",
			}

			escalateEvent(p, evt, deviceNeed{rt: intelv1a1.RecoveryTypeReflash, reason: reasonSurvivability})

			// A new ID would orphan the Job named after the old one.
			Expect(evt.ID).To(Equal("evt-esc-inflight"))
			Expect(evt.RecoveryType.Type).To(Equal(intelv1a1.RecoveryTypeSlot))
			Expect(evt.State).To(Equal(intelv1a1.RecoveryEventStateInProgress))
			Expect(evt.JobName).To(Equal("recovery-evt-esc-inflight-0"))
		})

		It("should keep an in-progress event whose taint has cleared", func() {
			p := &intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{Name: "plan-resolved-inflight"},
				Status: intelv1a1.GPURecoveryPlanStatus{
					Events: []intelv1a1.RecoveryEvent{{
						ID: "evt-inflight", NodeName: "node01", GPUBDF: "0000:02:00.0",
						State:   intelv1a1.RecoveryEventStateInProgress,
						JobName: "recovery-evt-inflight-0",
					}},
				},
			}

			// The taint clearing mid-reset is the normal case: the reset worked.
			r.removeResolvedEvents(ctx, p, map[deviceKey]deviceNeed{})

			Expect(p.Status.Events).To(HaveLen(1),
				"dropping the event now would leave its Job collected by nothing")
			Expect(p.Status.Messages).To(BeEmpty())
		})
	})

	// An SBR or slot reset acts on the PCIe bus and can wedge the host, so the node is emptied
	// before the reset runs and the reset waits for the GPU to actually be released. Before this,
	// a reset fired the moment an approval matched — underneath running workloads, including ones
	// still holding the device through a ResourceClaim.
	//
	// The primitives themselves (taints, eviction, pod classification) are covered against a fake
	// client in drain_test.go; what these specs pin is the recovery state machine built on them.
	Context("Reconcile: node drain before a reset", func() {
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

		BeforeEach(func() {
			ns := &core.Namespace{ObjectMeta: metav1.ObjectMeta{Name: drainWorkloadNS}}
			if err := k8sClient.Create(ctx, ns); err != nil {
				Expect(errors.IsAlreadyExists(err)).To(BeTrue())
			}
		})

		// makeDrainNode creates the Node the drain operates on. Its taints are cleared before the
		// delete so a leftover drain taint cannot follow the name into a later spec.
		makeDrainNode := func(name string) *core.Node {
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

			return node
		}

		// makeDrainSlice publishes a tainted GPU on the node so syncRecoveryEventsFromSlices
		// creates an event for it.
		makeDrainSlice := func(name, nodeName, bdf, taintKey string) {
			slice := &resv1.ResourceSlice{
				ObjectMeta: metav1.ObjectMeta{Name: name},
				Spec: resv1.ResourceSliceSpec{
					Driver:   gpuDeviceClass,
					NodeName: ptr.To(nodeName),
					Pool:     resv1.ResourcePool{Name: drainPool, ResourceSliceCount: 1},
					Devices: []resv1.Device{{
						Name: "dev-drain-0",
						Attributes: map[resv1.QualifiedName]resv1.DeviceAttribute{
							deviceAttrDeviceID: {StringValue: ptr.To("0x1234")},
							deviceAttrBDF:      {StringValue: ptr.To(bdf)},
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
		makeDrainPlan := func(name string, rt intelv1a1.RecoveryType) types.NamespacedName {
			p := &intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{Name: name, Finalizers: []string{recoveryPlanFinalizer}},
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					DeviceID:         "0x1234",
					MaxRetries:       3,
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
		makeWorkloadPod := func(name, nodeName string) *core.Pod {
			pod := &core.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: drainWorkloadNS},
				Spec: core.PodSpec{
					NodeName:   nodeName,
					Containers: []core.Container{{Name: "c", Image: "busybox"}},
				},
			}
			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, pod, client.GracePeriodSeconds(0))
			})

			return pod
		}

		fetch := func(key types.NamespacedName) *intelv1a1.GPURecoveryPlan {
			updated := &intelv1a1.GPURecoveryPlan{}
			Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())

			return updated
		}

		nodeTaints := func(name string) []core.Taint {
			node := &core.Node{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name}, node)).To(Succeed())

			return node.Spec.Taints
		}

		It("should hold a reset in draining while a workload pod is still on the node", func() {
			makeDrainNode(drainNode)
			makeDrainSlice("slice-drain-hold", drainNode, drainBDF, deviceTaintKeyReset)
			makeWorkloadPod("drain-victim", drainNode)
			key := makeDrainPlan("plan-drain-hold", intelv1a1.RecoveryTypeSlot)

			res, err := reconcilePlan(ctx, key.Name)
			Expect(err).NotTo(HaveOccurred())

			// Nothing else re-triggers a reconcile when the last pod finally goes away, so a drain
			// that does not requeue stalls until an unrelated event happens along.
			Expect(res.RequeueAfter).To(BeNumerically(">", 0),
				"a draining event must requeue or the drain never progresses")

			updated := fetch(key)
			Expect(updated.Status.Events).To(HaveLen(1))

			evt := updated.Status.Events[0]
			Expect(evt.State).To(Equal(intelv1a1.RecoveryEventStateDraining),
				"the reset must not start while a pod is still on the node; messages: %v", updated.Status.Messages)
			Expect(evt.JobName).To(BeEmpty(), "no Job may exist before the node is drained")
			Expect(evt.PodsBlockingDrain).To(ContainElement(drainWorkloadNS+"/drain-victim"),
				"an admin needs to see what the drain is waiting on")
			Expect(evt.DrainStartedAt).NotTo(BeNil(), "the deadline clock must start with the drain")

			// The taint is what stops the scheduler refilling the node behind the eviction.
			Expect(nodeTaints(drainNode)).To(ContainElement(recoveryTaint(key.Name)))

			// The drain must actually request the eviction, not merely report the pod as blocking.
			// Reporting alone waits for something else to remove the pod, which nothing will do —
			// the event would sit in draining until its deadline expired. envtest runs no kubelet
			// to confirm the delete, so the pod lingers with a deletionTimestamp rather than
			// disappearing.
			victim := &core.Pod{}
			Expect(k8sClient.Get(ctx,
				types.NamespacedName{Name: "drain-victim", Namespace: drainWorkloadNS}, victim)).To(Succeed())
			Expect(victim.DeletionTimestamp).NotTo(BeNil(),
				"the drain must evict the pod, not just name it in status")

			// A one-shot approval is spent once its event leaves waiting-approval, which is now
			// draining rather than in-progress. Leaving it unspent would let it authorise a second
			// event later on.
			Expect(updated.Spec.Approvals[0].Consumed).To(BeTrue(),
				"reaching draining is the approval being acted on")

			// status.state must read as active: a draining plan is working, not idle.
			Expect(updated.Status.State).To(Equal(intelv1a1.PlanStateActive))
		})

		It("should create the reset Job once the node is clear", func() {
			makeDrainNode(drainNode)
			makeDrainSlice("slice-drain-clear", drainNode, drainBDF, deviceTaintKeyReset)
			key := makeDrainPlan("plan-drain-clear", intelv1a1.RecoveryTypeSlot)

			// No pods on the node at all, so the drain converges immediately and the Job is
			// created in the same reconcile that started the drain.
			_, err := reconcilePlan(ctx, key.Name)
			Expect(err).NotTo(HaveOccurred())

			updated := fetch(key)
			Expect(updated.Status.Events).To(HaveLen(1))

			evt := updated.Status.Events[0]
			Expect(evt.State).To(Equal(intelv1a1.RecoveryEventStateInProgress),
				"an empty node needs no waiting; messages: %v", updated.Status.Messages)
			Expect(evt.JobName).NotTo(BeEmpty())
			Expect(evt.PodsBlockingDrain).To(BeEmpty())

			// The taint stays for the duration of the Job: dropping it here would let the
			// scheduler refill the node with pods that then sit through the PCIe reset.
			Expect(nodeTaints(drainNode)).To(ContainElement(recoveryTaint(key.Name)),
				"the node must stay unschedulable while the reset runs")

			job := &batch.Job{}
			Expect(k8sClient.Get(ctx,
				types.NamespacedName{Name: evt.JobName, Namespace: "default"}, job)).To(Succeed())

			// Recovery pods bypass the scheduler via NodeName, so a NoSchedule taint would not
			// have stopped them — but the taint manager evicts a pod that does not tolerate
			// NoExecute however it was placed, which would kill the pod mid-reset.
			Expect(job.Spec.Template.Spec.Tolerations).To(ContainElement(
				core.Toleration{Operator: core.TolerationOpExists}),
				"a recovery Job must tolerate the taints on the broken node it has to run on")
		})

		It("should not drain for a reflash", func() {
			makeDrainNode(drainNode)
			makeDrainSlice("slice-drain-reflash", drainNode, drainBDF, deviceTaintKeyXpumdReflash)
			makeWorkloadPod("reflash-bystander", drainNode)
			key := makeDrainPlan("plan-drain-reflash", intelv1a1.RecoveryTypeReflash)

			// Firmware configured, so the reflash really runs: what is under test is that the Job is
			// reached without a drain, not the missing-firmware parking that would also skip one.
			p := fetch(key)
			p.Spec.Firmware = &intelv1a1.FirmwareSpec{
				Source: intelv1a1.FirmwareSource{
					ContainerSource: &intelv1a1.ContainerFirmwareSource{Name: "registry/fw:v2"},
				},
				File: "gfx.bin",
			}
			Expect(k8sClient.Update(ctx, p)).To(Succeed())

			_, err := reconcilePlan(ctx, key.Name)
			Expect(err).NotTo(HaveOccurred())

			updated := fetch(key)
			Expect(updated.Status.Events).To(HaveLen(1))

			// A reflash writes firmware to a device already in survivability mode, without
			// resetting the bus. There is nothing on the node for a drain to protect, so evicting
			// unrelated workloads would be pure disruption.
			evt := updated.Status.Events[0]
			Expect(evt.State).To(Equal(intelv1a1.RecoveryEventStateInProgress),
				"a reflash must not enter draining; messages: %v", updated.Status.Messages)
			Expect(evt.JobName).NotTo(BeEmpty())
			Expect(evt.DrainStartedAt).To(BeNil())

			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, &batch.Job{ObjectMeta: metav1.ObjectMeta{
					Name: evt.JobName, Namespace: "default",
				}})
			})

			Expect(nodeTaints(drainNode)).NotTo(ContainElement(recoveryTaint(key.Name)),
				"a reflash must not cordon the node")

			pod := &core.Pod{}
			Expect(k8sClient.Get(ctx,
				types.NamespacedName{Name: "reflash-bystander", Namespace: drainWorkloadNS}, pod)).To(Succeed())
			Expect(pod.DeletionTimestamp).To(BeNil(), "a reflash must not evict unrelated pods")
		})

		It("should skip the drain when spec.drain.enable is false", func() {
			makeDrainNode(drainNode)
			makeDrainSlice("slice-drain-skip", drainNode, drainBDF, deviceTaintKeyReset)
			makeWorkloadPod("skip-bystander", drainNode)
			key := makeDrainPlan("plan-drain-skip", intelv1a1.RecoveryTypeSlot)

			p := fetch(key)
			p.Spec.Drain.Enable = ptr.To(false)
			Expect(k8sClient.Update(ctx, p)).To(Succeed())

			_, err := reconcilePlan(ctx, key.Name)
			Expect(err).NotTo(HaveOccurred())

			updated := fetch(key)
			Expect(updated.Status.Events).To(HaveLen(1))
			Expect(updated.Status.Events[0].State).To(Equal(intelv1a1.RecoveryEventStateInProgress),
				"a disabled drain must go straight to the Job; messages: %v", updated.Status.Messages)

			Expect(nodeTaints(drainNode)).NotTo(ContainElement(recoveryTaint(key.Name)),
				"a disabled drain must not cordon the node either")

			pod := &core.Pod{}
			Expect(k8sClient.Get(ctx,
				types.NamespacedName{Name: "skip-bystander", Namespace: drainWorkloadNS}, pod)).To(Succeed())
			Expect(pod.DeletionTimestamp).To(BeNil(), "a disabled drain must not evict anything")
		})

		It("should leave DaemonSet and operator-namespace pods alone", func() {
			makeDrainNode(drainNode)
			makeDrainSlice("slice-drain-skips", drainNode, drainBDF, deviceTaintKeyReset)
			key := makeDrainPlan("plan-drain-skips", intelv1a1.RecoveryTypeSlot)

			// A DaemonSet pod carries an automatic NoSchedule toleration, so evicting it brings it
			// straight back and the drain would never converge. Placed in the workload namespace,
			// not the operator one, so the DaemonSet rule is what is under test rather than the
			// namespace skip.
			dsPod := &core.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "ds-pod",
					Namespace: drainWorkloadNS,
					OwnerReferences: []metav1.OwnerReference{{
						APIVersion: "apps/v1",
						Kind:       "DaemonSet",
						Name:       "some-ds",
						UID:        "11111111-1111-1111-1111-111111111111",
					}},
				},
				Spec: core.PodSpec{
					NodeName:   drainNode,
					Containers: []core.Container{{Name: "c", Image: "busybox"}},
				},
			}
			Expect(k8sClient.Create(ctx, dsPod)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, dsPod, client.GracePeriodSeconds(0))
			})

			// An ordinary, evictable pod in the operator's own namespace ("default", per
			// newTestReconciler). Evicting there is self-destruction: the namespace holds the
			// operator pod, whose eviction aborts the very reconcile driving the drain, and the
			// recovery Jobs, which run *on* the node being drained.
			opPod := &core.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "operator-ns-pod", Namespace: "default"},
				Spec: core.PodSpec{
					NodeName:   drainNode,
					Containers: []core.Container{{Name: "c", Image: "busybox"}},
				},
			}
			Expect(k8sClient.Create(ctx, opPod)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, opPod, client.GracePeriodSeconds(0))
			})

			// A pod that has already reached a terminal phase holds no devices and cannot be
			// evicted meaningfully — the eviction API accepts the call and nothing changes, so
			// treating it as a blocker means the drain waits out its full deadline. Completed Job
			// pods linger on nodes as a matter of course, so this is the likeliest of the three
			// skip rules to fire in practice.
			donePod := &core.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "succeeded-pod", Namespace: drainWorkloadNS},
				Spec: core.PodSpec{
					NodeName:      drainNode,
					RestartPolicy: core.RestartPolicyNever,
					Containers:    []core.Container{{Name: "c", Image: "busybox"}},
				},
			}
			Expect(k8sClient.Create(ctx, donePod)).To(Succeed())
			donePod.Status.Phase = core.PodSucceeded
			Expect(k8sClient.Status().Update(ctx, donePod)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, donePod, client.GracePeriodSeconds(0))
			})

			_, err := reconcilePlan(ctx, key.Name)
			Expect(err).NotTo(HaveOccurred())

			updated := fetch(key)
			Expect(updated.Status.Events).To(HaveLen(1))
			Expect(updated.Status.Events[0].State).To(Equal(intelv1a1.RecoveryEventStateInProgress),
				"pods a drain must ignore cannot keep it from converging; messages: %v", updated.Status.Messages)
			Expect(updated.Status.Events[0].PodsBlockingDrain).To(BeEmpty(),
				"an ignored pod must not be reported as blocking either")

			pod := &core.Pod{}
			Expect(k8sClient.Get(ctx,
				types.NamespacedName{Name: "ds-pod", Namespace: drainWorkloadNS}, pod)).To(Succeed())
			Expect(pod.DeletionTimestamp).To(BeNil(), "a DaemonSet pod must not be evicted")

			Expect(k8sClient.Get(ctx,
				types.NamespacedName{Name: "operator-ns-pod", Namespace: "default"}, pod)).To(Succeed())
			Expect(pod.DeletionTimestamp).To(BeNil(), "the operator's own namespace must not be drained")

			Expect(k8sClient.Get(ctx,
				types.NamespacedName{Name: "succeeded-pod", Namespace: drainWorkloadNS}, pod)).To(Succeed())
			Expect(pod.Status.Phase).To(Equal(core.PodSucceeded),
				"fixture check: the phase must survive, or this asserts nothing")
		})

		// spec.drain.namespacesToSkip: cluster infrastructure an admin is content to leave running
		// through a reset — cert-manager, kube-system and the like — rather than evict off every
		// node a GPU is recovered on.
		//
		// Both halves are asserted in one spec on purpose. Ignoring the field altogether and
		// treating every namespace as skipped are both green against half of it: the first evicts
		// the infra pod, the second lets the reset start with an ordinary workload still on the
		// node.
		It("should leave a namespace in spec.drain.namespacesToSkip alone", func() {
			const skipNS = "drain-infra"

			ns := &core.Namespace{ObjectMeta: metav1.ObjectMeta{Name: skipNS}}
			if err := k8sClient.Create(ctx, ns); err != nil {
				Expect(errors.IsAlreadyExists(err)).To(BeTrue())
			}

			makeDrainNode(drainNode)
			makeDrainSlice("slice-drain-nsskip", drainNode, drainBDF, deviceTaintKeyReset)
			key := makeDrainPlan("plan-drain-nsskip", intelv1a1.RecoveryTypeSlot)

			infraPod := &core.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "infra-pod", Namespace: skipNS},
				Spec: core.PodSpec{
					NodeName:   drainNode,
					Containers: []core.Container{{Name: "c", Image: "busybox"}},
				},
			}
			Expect(k8sClient.Create(ctx, infraPod)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, infraPod, client.GracePeriodSeconds(0))
			})

			makeWorkloadPod("nsskip-victim", drainNode)

			p := fetch(key)
			p.Spec.Drain.NamespacesToSkip = []string{skipNS}
			Expect(k8sClient.Update(ctx, p)).To(Succeed())

			_, err := reconcilePlan(ctx, key.Name)
			Expect(err).NotTo(HaveOccurred())

			updated := fetch(key)
			Expect(updated.Status.Events).To(HaveLen(1))

			evt := updated.Status.Events[0]
			Expect(evt.State).To(Equal(intelv1a1.RecoveryEventStateDraining),
				"the pod outside the skipped namespace must still hold the reset back; messages: %v",
				updated.Status.Messages)
			Expect(evt.PodsBlockingDrain).To(ConsistOf(drainWorkloadNS+"/nsskip-victim"),
				"a skipped namespace is not something the drain is waiting on")

			survivor := &core.Pod{}
			Expect(k8sClient.Get(ctx,
				types.NamespacedName{Name: "infra-pod", Namespace: skipNS}, survivor)).To(Succeed())
			Expect(survivor.DeletionTimestamp).To(BeNil(),
				"a pod in a skipped namespace must not be evicted")

			victim := &core.Pod{}
			Expect(k8sClient.Get(ctx,
				types.NamespacedName{Name: "nsskip-victim", Namespace: drainWorkloadNS}, victim)).To(Succeed())
			Expect(victim.DeletionTimestamp).NotTo(BeNil(),
				"skipping one namespace must not turn the whole drain off")
		})

		It("should honour a PodDisruptionBudget that forbids the eviction", func() {
			makeDrainNode(drainNode)
			makeDrainSlice("slice-drain-pdb", drainNode, drainBDF, deviceTaintKeyReset)
			key := makeDrainPlan("plan-drain-pdb", intelv1a1.RecoveryTypeSlot)

			pod := makeWorkloadPod("pdb-protected", drainNode)
			pod.Labels = map[string]string{"app": "pdb-guarded"}
			Expect(k8sClient.Update(ctx, pod)).To(Succeed())

			// The pod must be Running and Ready for the budget to apply at all: the eviction API
			// deliberately lets an unhealthy pod go without consulting any PDB, on the grounds that
			// evicting something already broken costs no availability. Without this the eviction
			// succeeds and the spec proves nothing.
			pod.Status = core.PodStatus{
				Phase:      core.PodRunning,
				Conditions: []core.PodCondition{{Type: core.PodReady, Status: core.ConditionTrue}},
			}
			Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

			// A budget that permits no disruption at all. This is the whole reason the drain goes
			// through the eviction subresource rather than deleting pods outright: a plain Delete
			// ignores budgets and would silently break the availability guarantee the workload
			// owner asked for.
			pdb := &policy.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{Name: "no-disruptions", Namespace: drainWorkloadNS},
				Spec: policy.PodDisruptionBudgetSpec{
					MinAvailable: ptr.To(intstr.FromInt32(1)),
					Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": "pdb-guarded"}},
				},
			}
			Expect(k8sClient.Create(ctx, pdb)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, pdb)
			})

			// The status has to be written by hand: envtest runs no disruption controller, and the
			// eviction handler in the API server waits — with backoff, for over a minute — for a
			// budget whose observedGeneration is behind its spec, on the assumption that a
			// controller is about to catch up. A real cluster always has that status computed, so
			// filling it in is the faithful fixture as well as the fast one.
			pdb.Status = policy.PodDisruptionBudgetStatus{
				ObservedGeneration: pdb.Generation,
				DisruptionsAllowed: 0,
				CurrentHealthy:     1,
				DesiredHealthy:     1,
				ExpectedPods:       1,
			}
			Expect(k8sClient.Status().Update(ctx, pdb)).To(Succeed())

			_, err := reconcilePlan(ctx, key.Name)
			Expect(err).NotTo(HaveOccurred())

			updated := fetch(key)
			Expect(updated.Status.Events).To(HaveLen(1))

			evt := updated.Status.Events[0]

			// A rejected eviction is a "not yet", not a failure: the event keeps waiting and the
			// drain deadline is what eventually gives up on it.
			Expect(evt.State).To(Equal(intelv1a1.RecoveryEventStateDraining),
				"a budget-blocked eviction must leave the event draining; messages: %v", updated.Status.Messages)
			Expect(evt.PodsBlockingDrain).To(ContainElement(drainWorkloadNS + "/pdb-protected"))

			survivor := &core.Pod{}
			Expect(k8sClient.Get(ctx,
				types.NamespacedName{Name: "pdb-protected", Namespace: drainWorkloadNS}, survivor)).To(Succeed())
			Expect(survivor.DeletionTimestamp).To(BeNil(),
				"a pod a PodDisruptionBudget protects must survive the drain")
		})

		It("should keep waiting for a pod that is already terminating", func() {
			makeDrainNode(drainNode)
			makeDrainSlice("slice-drain-terminating", drainNode, drainBDF, deviceTaintKeyReset)
			key := makeDrainPlan("plan-drain-terminating", intelv1a1.RecoveryTypeSlot)

			// A pod with a finalizer keeps its deletionTimestamp: envtest has no kubelet to confirm
			// the delete, so this is exactly the shape of a pod on its way out but not yet gone. A
			// drain that treated "terminating" as "gone" would fire the reset while the workload's
			// containers were still running on the GPU.
			pod := makeWorkloadPod("slow-goodbye", drainNode)
			pod.Finalizers = []string{"test.intel.com/hold"}
			Expect(k8sClient.Update(ctx, pod)).To(Succeed())
			DeferCleanup(func() {
				fresh := &core.Pod{}
				if err := k8sClient.Get(ctx,
					types.NamespacedName{Name: "slow-goodbye", Namespace: drainWorkloadNS}, fresh); err == nil {
					fresh.Finalizers = nil
					_ = k8sClient.Update(ctx, fresh)
				}
			})

			Expect(k8sClient.Delete(ctx, pod)).To(Succeed())

			fresh := &core.Pod{}
			Expect(k8sClient.Get(ctx,
				types.NamespacedName{Name: "slow-goodbye", Namespace: drainWorkloadNS}, fresh)).To(Succeed())
			Expect(fresh.DeletionTimestamp).NotTo(BeNil(), "fixture check: the pod must be terminating")

			_, err := reconcilePlan(ctx, key.Name)
			Expect(err).NotTo(HaveOccurred())

			updated := fetch(key)
			Expect(updated.Status.Events).To(HaveLen(1))

			evt := updated.Status.Events[0]
			Expect(evt.State).To(Equal(intelv1a1.RecoveryEventStateDraining),
				"a terminating pod still occupies the node; messages: %v", updated.Status.Messages)
			Expect(evt.JobName).To(BeEmpty())
			Expect(evt.PodsBlockingDrain).To(ContainElement(drainWorkloadNS + "/slow-goodbye"))
		})

		It("should fail the event and untaint the node when the drain deadline passes", func() {
			makeDrainNode(drainNode)
			makeDrainSlice("slice-drain-timeout", drainNode, drainBDF, deviceTaintKeyReset)
			makeWorkloadPod("stuck-pod", drainNode)
			key := makeDrainPlan("plan-drain-timeout", intelv1a1.RecoveryTypeSlot)

			// Pass 1: the drain starts and stalls on the pod.
			_, err := reconcilePlan(ctx, key.Name)
			Expect(err).NotTo(HaveOccurred())

			updated := fetch(key)
			Expect(updated.Status.Events[0].State).To(Equal(intelv1a1.RecoveryEventStateDraining))

			// Backdate the clock rather than sleeping out a real timeout.
			updated.Status.Events[0].DrainStartedAt = ptr.To(metav1.NewTime(time.Now().Add(-10 * time.Minute)))
			Expect(k8sClient.Status().Update(ctx, updated)).To(Succeed())

			// Pass 2: the deadline has passed.
			_, err = reconcilePlan(ctx, key.Name)
			Expect(err).NotTo(HaveOccurred())

			updated = fetch(key)
			evt := updated.Status.Events[0]

			// Failing is the point: an unsatisfiable PodDisruptionBudget or a pod stuck on a
			// finalizer would otherwise park the event in draining forever with no diagnostic.
			Expect(evt.State).To(Equal(intelv1a1.RecoveryEventStateFailed),
				"a drain that cannot finish must fail rather than hang; messages: %v", updated.Status.Messages)
			Expect(evt.JobName).To(BeEmpty(), "the reset must not run after a failed drain")
			Expect(evt.RetryCount).To(BeNumerically(">", 0))

			// failed covers two different situations, and this is the one where the GPU was never
			// touched: what has to change is the workload on the node, not anything about the plan
			// or the device. The event has to say which it is.
			Expect(evt.StateMessage).To(SatisfyAll(
				ContainSubstring(drainNode),
				ContainSubstring("timed out"),
				ContainSubstring("reset not attempted"),
			), "a drain timeout must be distinguishable from a reset that ran and failed")

			// The node must not be left cordoned: keeping a whole node out of service on account
			// of one un-recovered GPU is the worse outcome.
			Expect(nodeTaints(drainNode)).NotTo(ContainElement(recoveryTaint(key.Name)),
				"a failed drain must release the node")
		})

		// The two specs below cover the ways of staying in draining that do NOT go through the
		// blocking-pod path, which is where the deadline used to be checked. Both looped for ever:
		// the drain reported perfect progress — an empty node, no blockers — while nothing was timing
		// it, and the plan sat in draining with the node cordoned indefinitely.
		It("should fail the event when a drained node will not accept the recovery Job", func() {
			makeDrainNode(drainNode)
			makeDrainSlice("slice-drain-nojob", drainNode, drainBDF, deviceTaintKeyReset)
			key := makeDrainPlan("plan-drain-nojob", intelv1a1.RecoveryTypeSlot)

			reconcileRejectingJobs := func() {
				r := newTestReconciler()
				r.Client = &jobRejectingClient{Client: k8sClient}

				_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
				Expect(err).NotTo(HaveOccurred(),
					"a Job that cannot be created is the event's problem, not the reconcile's")
			}

			// Pass 1: nothing is on the node, so the drain converges at once and the rejected Job is
			// the only thing keeping the event in draining.
			reconcileRejectingJobs()

			updated := fetch(key)
			Expect(updated.Status.Events).To(HaveLen(1))

			evt := updated.Status.Events[0]
			Expect(evt.State).To(Equal(intelv1a1.RecoveryEventStateDraining),
				"the Job is worth retrying, so the event waits; messages: %v", updated.Status.Messages)
			Expect(evt.JobName).To(BeEmpty())
			Expect(evt.PodsBlockingDrain).To(BeEmpty(),
				"fixture check: the node must be clear, or this spec would run through the blocker path")

			// Backdate the clock rather than sleeping out a real timeout.
			updated.Status.Events[0].DrainStartedAt = ptr.To(metav1.NewTime(time.Now().Add(-10 * time.Minute)))
			Expect(k8sClient.Status().Update(ctx, updated)).To(Succeed())

			// Pass 2: the deadline has passed with the Job still un-creatable.
			reconcileRejectingJobs()

			updated = fetch(key)
			evt = updated.Status.Events[0]

			Expect(evt.State).To(Equal(intelv1a1.RecoveryEventStateFailed),
				"an event that cannot start its Job must give up at the deadline, not retry for ever; messages: %v",
				updated.Status.Messages)
			Expect(evt.StateMessage).To(SatisfyAll(
				ContainSubstring("timed out"),
				ContainSubstring("could not be created"),
			), "the failure must name what stopped it, which is the Job and not a pod")
			Expect(evt.RetryCount).To(BeNumerically(">", 0))

			Expect(nodeTaints(drainNode)).NotTo(ContainElement(recoveryTaint(key.Name)),
				"a node emptied for a reset that never started must not stay cordoned")
		})

		It("should fail the event when the drain itself cannot be carried out", func() {
			makeDrainNode(drainNode)
			makeDrainSlice("slice-drain-nocordon", drainNode, drainBDF, deviceTaintKeyReset)
			key := makeDrainPlan("plan-drain-nocordon", intelv1a1.RecoveryTypeSlot)

			reconcileRejectingNodeWrites := func() {
				r := newTestReconciler()
				r.Client = &nodeWriteRejectingClient{Client: k8sClient}

				_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
				Expect(err).NotTo(HaveOccurred(),
					"one unreachable node must not fail the reconcile for every other GPU")
			}

			// Pass 1: the cordon fails, so the drain never gets as far as looking at pods.
			reconcileRejectingNodeWrites()

			updated := fetch(key)
			Expect(updated.Status.Events).To(HaveLen(1))
			Expect(updated.Status.Events[0].State).To(Equal(intelv1a1.RecoveryEventStateDraining),
				"a transient node write failure is worth retrying; messages: %v", updated.Status.Messages)

			updated.Status.Events[0].DrainStartedAt = ptr.To(metav1.NewTime(time.Now().Add(-10 * time.Minute)))
			Expect(k8sClient.Status().Update(ctx, updated)).To(Succeed())

			// Pass 2: still failing, and now out of time.
			reconcileRejectingNodeWrites()

			evt := fetch(key).Status.Events[0]
			Expect(evt.State).To(Equal(intelv1a1.RecoveryEventStateFailed),
				"a drain that cannot be performed at all must still hit the deadline")
			Expect(evt.StateMessage).To(SatisfyAll(
				ContainSubstring("timed out"),
				ContainSubstring("not progressing"),
			), "the failure must say the drain never ran, not that pods were in the way")
		})

		It("should hold the reset while a ResourceClaim still reserves the GPU", func() {
			makeDrainNode(drainNode)
			makeDrainSlice("slice-drain-claim", drainNode, drainBDF, deviceTaintKeyReset)
			key := makeDrainPlan("plan-drain-claim", intelv1a1.RecoveryTypeSlot)

			// A claim can outlive the pod that created it, so an empty node is not proof the device
			// is free. reservedFor is what proves a live consumer.
			claim := &resv1.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "holding-claim", Namespace: "default"},
				Spec: resv1.ResourceClaimSpec{
					Devices: resv1.DeviceClaim{
						Requests: []resv1.DeviceRequest{{
							Name:    "gpu",
							Exactly: &resv1.ExactDeviceRequest{DeviceClassName: gpuDeviceClass},
						}},
					},
				},
			}
			Expect(k8sClient.Create(ctx, claim)).To(Succeed())
			DeferCleanup(func() {
				fresh := &resv1.ResourceClaim{}
				if err := k8sClient.Get(ctx,
					types.NamespacedName{Name: "holding-claim", Namespace: "default"}, fresh); err == nil {
					fresh.Status = resv1.ResourceClaimStatus{}
					_ = k8sClient.Status().Update(ctx, fresh)
					_ = k8sClient.Delete(ctx, fresh)
				}
			})

			claim.Status = resv1.ResourceClaimStatus{
				Allocation: &resv1.AllocationResult{
					Devices: resv1.DeviceAllocationResult{
						Results: []resv1.DeviceRequestAllocationResult{{
							Request: "gpu",
							Driver:  gpuDeviceClass,
							Pool:    drainPool,
							Device:  "dev-drain-0",
						}},
					},
				},
				ReservedFor: []resv1.ResourceClaimConsumerReference{{
					Resource: "pods",
					Name:     "claim-holder",
					UID:      "22222222-2222-2222-2222-222222222222",
				}},
			}
			Expect(k8sClient.Status().Update(ctx, claim)).To(Succeed())

			_, err := reconcilePlan(ctx, key.Name)
			Expect(err).NotTo(HaveOccurred())

			updated := fetch(key)
			Expect(updated.Status.Events).To(HaveLen(1))

			evt := updated.Status.Events[0]
			Expect(evt.State).To(Equal(intelv1a1.RecoveryEventStateDraining),
				"resetting under a live allocation pulls the hardware out from under it; messages: %v",
				updated.Status.Messages)
			Expect(evt.JobName).To(BeEmpty())
			Expect(evt.ClaimsBlockingReset).To(ContainElement("default/holding-claim"),
				"the claim holding the device must be named in status")
		})

		It("should proceed when an allocated ResourceClaim has no consumer", func() {
			makeDrainNode(drainNode)
			makeDrainSlice("slice-drain-unreserved", drainNode, drainBDF, deviceTaintKeyReset)
			key := makeDrainPlan("plan-drain-unreserved", intelv1a1.RecoveryTypeSlot)

			claim := &resv1.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "stale-claim", Namespace: "default"},
				Spec: resv1.ResourceClaimSpec{
					Devices: resv1.DeviceClaim{
						Requests: []resv1.DeviceRequest{{
							Name:    "gpu",
							Exactly: &resv1.ExactDeviceRequest{DeviceClassName: gpuDeviceClass},
						}},
					},
				},
			}
			Expect(k8sClient.Create(ctx, claim)).To(Succeed())
			DeferCleanup(func() {
				fresh := &resv1.ResourceClaim{}
				if err := k8sClient.Get(ctx,
					types.NamespacedName{Name: "stale-claim", Namespace: "default"}, fresh); err == nil {
					fresh.Status = resv1.ResourceClaimStatus{}
					_ = k8sClient.Status().Update(ctx, fresh)
					_ = k8sClient.Delete(ctx, fresh)
				}
			})

			// Allocated but reservedFor is empty: the scheduler allocated the device and the pod
			// then went away. Nothing holds it, so blocking here would deadlock the recovery on a
			// claim that will never be released by anyone.
			claim.Status = resv1.ResourceClaimStatus{
				Allocation: &resv1.AllocationResult{
					Devices: resv1.DeviceAllocationResult{
						Results: []resv1.DeviceRequestAllocationResult{{
							Request: "gpu",
							Driver:  gpuDeviceClass,
							Pool:    drainPool,
							Device:  "dev-drain-0",
						}},
					},
				},
			}
			Expect(k8sClient.Status().Update(ctx, claim)).To(Succeed())

			_, err := reconcilePlan(ctx, key.Name)
			Expect(err).NotTo(HaveOccurred())

			updated := fetch(key)
			Expect(updated.Status.Events).To(HaveLen(1))
			Expect(updated.Status.Events[0].State).To(Equal(intelv1a1.RecoveryEventStateInProgress),
				"an unreserved claim holds no device; messages: %v", updated.Status.Messages)
			Expect(updated.Status.Events[0].ClaimsBlockingReset).To(BeEmpty())
		})

		It("should ignore a ResourceClaim holding a different GPU", func() {
			makeDrainNode(drainNode)
			makeDrainSlice("slice-drain-other", drainNode, drainBDF, deviceTaintKeyReset)
			key := makeDrainPlan("plan-drain-other", intelv1a1.RecoveryTypeSlot)

			claim := &resv1.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "other-gpu-claim", Namespace: "default"},
				Spec: resv1.ResourceClaimSpec{
					Devices: resv1.DeviceClaim{
						Requests: []resv1.DeviceRequest{{
							Name:    "gpu",
							Exactly: &resv1.ExactDeviceRequest{DeviceClassName: gpuDeviceClass},
						}},
					},
				},
			}
			Expect(k8sClient.Create(ctx, claim)).To(Succeed())
			DeferCleanup(func() {
				fresh := &resv1.ResourceClaim{}
				if err := k8sClient.Get(ctx,
					types.NamespacedName{Name: "other-gpu-claim", Namespace: "default"}, fresh); err == nil {
					fresh.Status = resv1.ResourceClaimStatus{}
					_ = k8sClient.Status().Update(ctx, fresh)
					_ = k8sClient.Delete(ctx, fresh)
				}
			})

			// Reserved, but for a different device in the same pool. A node with eight GPUs has
			// seven of these in normal operation; blocking on them would mean a reset could only
			// ever run on a completely idle node.
			claim.Status = resv1.ResourceClaimStatus{
				Allocation: &resv1.AllocationResult{
					Devices: resv1.DeviceAllocationResult{
						Results: []resv1.DeviceRequestAllocationResult{{
							Request: "gpu",
							Driver:  gpuDeviceClass,
							Pool:    drainPool,
							Device:  "dev-drain-99",
						}},
					},
				},
				ReservedFor: []resv1.ResourceClaimConsumerReference{{
					Resource: "pods",
					Name:     "other-holder",
					UID:      "33333333-3333-3333-3333-333333333333",
				}},
			}
			Expect(k8sClient.Status().Update(ctx, claim)).To(Succeed())

			_, err := reconcilePlan(ctx, key.Name)
			Expect(err).NotTo(HaveOccurred())

			updated := fetch(key)
			Expect(updated.Status.Events).To(HaveLen(1))
			Expect(updated.Status.Events[0].State).To(Equal(intelv1a1.RecoveryEventStateInProgress),
				"a claim on another GPU must not block this reset; messages: %v", updated.Status.Messages)
			Expect(updated.Status.Events[0].ClaimsBlockingReset).To(BeEmpty())
		})

		// A claim the drain cannot release must not be treated as a blocker: the event would sit
		// in draining for the full timeout, fail, spend a retry, and repeat — on a GPU nobody could
		// have freed. XPU Manager is the case that matters in practice, holding an admin claim on
		// every GPU it monitors from a DaemonSet in the operator's own namespace, which the drain
		// skips twice over.
		Context("Claims the drain can never release", func() {
			// makeHoldingClaim allocates the event's GPU to a claim and reserves it for the given
			// consumers. Status is cleared before deletion because an allocated claim would
			// otherwise be seen by later specs in the same suite.
			makeHoldingClaim := func(
				name, namespace string,
				adminAccess bool,
				consumers ...resv1.ResourceClaimConsumerReference,
			) {
				claim := &resv1.ResourceClaim{
					ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
					Spec: resv1.ResourceClaimSpec{
						Devices: resv1.DeviceClaim{
							Requests: []resv1.DeviceRequest{{
								Name:    "gpu",
								Exactly: &resv1.ExactDeviceRequest{DeviceClassName: gpuDeviceClass},
							}},
						},
					},
				}
				Expect(k8sClient.Create(ctx, claim)).To(Succeed())
				DeferCleanup(func() {
					fresh := &resv1.ResourceClaim{}
					if err := k8sClient.Get(ctx,
						types.NamespacedName{Name: name, Namespace: namespace}, fresh); err == nil {
						fresh.Status = resv1.ResourceClaimStatus{}
						_ = k8sClient.Status().Update(ctx, fresh)
						_ = k8sClient.Delete(ctx, fresh)
					}
				})

				result := resv1.DeviceRequestAllocationResult{
					Request: "gpu",
					Driver:  gpuDeviceClass,
					Pool:    drainPool,
					Device:  "dev-drain-0",
				}
				if adminAccess {
					result.AdminAccess = ptr.To(true)
				}

				claim.Status = resv1.ResourceClaimStatus{
					Allocation: &resv1.AllocationResult{
						Devices: resv1.DeviceAllocationResult{
							Results: []resv1.DeviceRequestAllocationResult{result},
						},
					},
					ReservedFor: consumers,
				}
				Expect(k8sClient.Status().Update(ctx, claim)).To(Succeed())
			}

			// makeUnevictablePod puts a pod the drain will never evict on the node: a DaemonSet
			// pod, in the namespace given. This is XPU Manager's shape.
			makeUnevictablePod := func(name, namespace string) *core.Pod {
				pod := &core.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      name,
						Namespace: namespace,
						OwnerReferences: []metav1.OwnerReference{{
							APIVersion: "apps/v1",
							Kind:       "DaemonSet",
							Name:       "xpumanager",
							UID:        "44444444-4444-4444-4444-444444444444",
						}},
					},
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

			podConsumer := func(pod *core.Pod) resv1.ResourceClaimConsumerReference {
				return resv1.ResourceClaimConsumerReference{Resource: "pods", Name: pod.Name, UID: pod.UID}
			}

			// expectReset asserts the reset went ahead, i.e. the claim was not treated as a hold.
			expectReset := func(key types.NamespacedName, because string) {
				_, err := reconcilePlan(ctx, key.Name)
				Expect(err).NotTo(HaveOccurred())

				updated := fetch(key)
				Expect(updated.Status.Events).To(HaveLen(1))
				Expect(updated.Status.Events[0].State).To(Equal(intelv1a1.RecoveryEventStateInProgress),
					because+"; messages: %v", updated.Status.Messages)
				Expect(updated.Status.Events[0].ClaimsBlockingReset).To(BeEmpty())
			}

			// adminAccess is monitoring access: DRA hands out the device without reserving it, so
			// the same GPU stays allocatable to workloads and the claim proves nothing about
			// whether anyone is computing on it.
			It("should not wait for an admin-access claim", func() {
				makeDrainNode(drainNode)
				makeDrainSlice("slice-claim-admin", drainNode, drainBDF, deviceTaintKeyReset)
				key := makeDrainPlan("plan-claim-admin", intelv1a1.RecoveryTypeSlot)

				// The API server refuses an admin-access allocation unless the claim's namespace
				// carries this label, so a cluster using admin claims has already opted its
				// monitoring namespace in — including the operator's own.
				ns := &core.Namespace{}
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: drainWorkloadNS}, ns)).To(Succeed())
				metav1.SetMetaDataLabel(&ns.ObjectMeta, "resource.kubernetes.io/admin-access", "true")
				Expect(k8sClient.Update(ctx, ns)).To(Succeed())

				// Deliberately an evictable pod in a workload namespace: the exclusion must come
				// from the admin flag alone, not from where the consumer happens to run. Same
				// fixture as "should still wait for a claim held by an evictable pod" below, with
				// adminAccess the only difference — so the pod still blocks the drain and the claim
				// must not appear alongside it.
				holder := makeWorkloadPod("admin-holder", drainNode)
				makeHoldingClaim("admin-claim", drainWorkloadNS, true, podConsumer(holder))

				_, err := reconcilePlan(ctx, key.Name)
				Expect(err).NotTo(HaveOccurred())

				updated := fetch(key)
				evt := updated.Status.Events[0]
				Expect(evt.ClaimsBlockingReset).To(BeEmpty(),
					"an admin-access claim does not reserve the device; messages: %v", updated.Status.Messages)
				Expect(evt.PodsBlockingDrain).To(ContainElement(drainWorkloadNS+"/admin-holder"),
					"the holder pod is still an ordinary drain blocker")
			})

			// The operator's own namespace: nothing in it is ever evicted, so nothing in it can
			// ever release a claim.
			It("should not wait for a claim held by a pod in the operator namespace", func() {
				makeDrainNode(drainNode)
				makeDrainSlice("slice-claim-operator", drainNode, drainBDF, deviceTaintKeyReset)
				key := makeDrainPlan("plan-claim-operator", intelv1a1.RecoveryTypeSlot)

				// "default" is the operator namespace here, per newTestReconciler.
				holder := makeUnevictablePod("xpumd-operator-ns", "default")
				makeHoldingClaim("operator-ns-claim", "default", false, podConsumer(holder))

				expectReset(key, "a claim in the operator namespace can never be released by draining")
			})

			// Same deadlock without the operator namespace: any GPU-using DaemonSet anywhere
			// reaches it, because a DaemonSet pod is never evicted.
			It("should not wait for a claim held by a DaemonSet pod elsewhere", func() {
				makeDrainNode(drainNode)
				makeDrainSlice("slice-claim-ds", drainNode, drainBDF, deviceTaintKeyReset)
				key := makeDrainPlan("plan-claim-ds", intelv1a1.RecoveryTypeSlot)

				holder := makeUnevictablePod("monitor-ds", drainWorkloadNS)
				makeHoldingClaim("ds-claim", drainWorkloadNS, false, podConsumer(holder))

				expectReset(key, "a DaemonSet pod is never evicted, so its claim is never released")
			})

			// spec.drain.namespacesToSkip opens the same hole by configuration, which is why it is
			// documented as "do not list namespaces that run GPU workloads": the drain's skip set
			// and the set of claims that cannot block are the same set, on purpose.
			It("should not wait for a claim held by a pod in a skipped namespace", func() {
				makeDrainNode(drainNode)
				makeDrainSlice("slice-claim-skipns", drainNode, drainBDF, deviceTaintKeyReset)
				key := makeDrainPlan("plan-claim-skipns", intelv1a1.RecoveryTypeSlot)

				p := fetch(key)
				p.Spec.Drain.NamespacesToSkip = []string{drainWorkloadNS}
				Expect(k8sClient.Update(ctx, p)).To(Succeed())

				// An ordinary pod: only the skip list makes it unevictable, so this also pins that
				// the claim check reads the same list the drain does.
				holder := makeWorkloadPod("skipped-holder", drainNode)
				makeHoldingClaim("skipns-claim", drainWorkloadNS, false, podConsumer(holder))

				expectReset(key, "a pod the drain skips by namespace cannot release its claim either")
			})

			// The other half of the rule: nothing above may weaken the check for a claim the drain
			// *can* release.
			It("should still wait for a claim held by an evictable pod", func() {
				makeDrainNode(drainNode)
				makeDrainSlice("slice-claim-evictable", drainNode, drainBDF, deviceTaintKeyReset)
				key := makeDrainPlan("plan-claim-evictable", intelv1a1.RecoveryTypeSlot)

				holder := makeWorkloadPod("evictable-holder", drainNode)
				makeHoldingClaim("evictable-claim", drainWorkloadNS, false, podConsumer(holder))

				_, err := reconcilePlan(ctx, key.Name)
				Expect(err).NotTo(HaveOccurred())

				updated := fetch(key)
				evt := updated.Status.Events[0]
				Expect(evt.State).To(Equal(intelv1a1.RecoveryEventStateDraining),
					"an evictable holder is exactly what the in-use check exists for; messages: %v",
					updated.Status.Messages)
				Expect(evt.ClaimsBlockingReset).To(ContainElement(drainWorkloadNS + "/evictable-claim"))
			})

			// A mixed claim must block. Anything else lets one unevictable co-consumer hide a live
			// workload holding the same device.
			It("should still wait for a claim held by both an unevictable and an evictable pod", func() {
				makeDrainNode(drainNode)
				makeDrainSlice("slice-claim-mixed", drainNode, drainBDF, deviceTaintKeyReset)
				key := makeDrainPlan("plan-claim-mixed", intelv1a1.RecoveryTypeSlot)

				infra := makeUnevictablePod("mixed-ds", drainWorkloadNS)
				workload := makeWorkloadPod("mixed-workload", drainNode)
				makeHoldingClaim("mixed-claim", drainWorkloadNS, false,
					podConsumer(infra), podConsumer(workload))

				_, err := reconcilePlan(ctx, key.Name)
				Expect(err).NotTo(HaveOccurred())

				updated := fetch(key)
				Expect(updated.Status.Events[0].ClaimsBlockingReset).To(
					ContainElement(drainWorkloadNS+"/mixed-claim"),
					"one unevictable consumer must not excuse the whole claim; messages: %v",
					updated.Status.Messages)
			})

			// A reservation whose pod is gone is the window the claim check was added for: the pod
			// object disappears before the kubelet has finished unpreparing the device. It must
			// keep blocking, with the drain deadline as the only backstop.
			It("should still wait for a reservation whose pod no longer exists", func() {
				makeDrainNode(drainNode)
				makeDrainSlice("slice-claim-ghost", drainNode, drainBDF, deviceTaintKeyReset)
				key := makeDrainPlan("plan-claim-ghost", intelv1a1.RecoveryTypeSlot)

				makeHoldingClaim("ghost-claim", drainWorkloadNS, false,
					resv1.ResourceClaimConsumerReference{
						Resource: "pods",
						Name:     "long-gone",
						UID:      "55555555-5555-5555-5555-555555555555",
					})

				_, err := reconcilePlan(ctx, key.Name)
				Expect(err).NotTo(HaveOccurred())

				updated := fetch(key)
				Expect(updated.Status.Events[0].State).To(Equal(intelv1a1.RecoveryEventStateDraining),
					"a claim outliving its pod must still hold the reset; messages: %v", updated.Status.Messages)
				Expect(updated.Status.Events[0].ClaimsBlockingReset).To(
					ContainElement(drainWorkloadNS + "/ghost-claim"))
			})

			// A pod recreated under the same name is a different consumer. Classifying the
			// reservation by whatever pod currently answers to that name would answer a question
			// about a pod that no longer exists — and here it would answer it wrongly, since the
			// live pod is one the drain leaves alone.
			It("should still wait for a reservation whose pod was replaced under the same name", func() {
				makeDrainNode(drainNode)
				makeDrainSlice("slice-claim-uid", drainNode, drainBDF, deviceTaintKeyReset)
				key := makeDrainPlan("plan-claim-uid", intelv1a1.RecoveryTypeSlot)

				live := makeUnevictablePod("reused-name", drainWorkloadNS)
				stale := podConsumer(live)
				stale.UID = "66666666-6666-6666-6666-666666666666"
				makeHoldingClaim("uid-claim", drainWorkloadNS, false, stale)

				_, err := reconcilePlan(ctx, key.Name)
				Expect(err).NotTo(HaveOccurred())

				updated := fetch(key)
				Expect(updated.Status.Events[0].ClaimsBlockingReset).To(
					ContainElement(drainWorkloadNS+"/uid-claim"),
					"a stale UID must not be classified by the pod that replaced it; messages: %v",
					updated.Status.Messages)
			})

			// The drain declines to evict a Succeeded pod too, but that is a state which clears by
			// itself rather than a pod that will never move — so its claim is worth waiting for.
			// This pins the deliberate difference between drainNeverEvicts and
			// classifyPodForDrain: folding the two together would stop the reset waiting here.
			It("should still wait for a claim held by a completed pod", func() {
				makeDrainNode(drainNode)
				makeDrainSlice("slice-claim-done", drainNode, drainBDF, deviceTaintKeyReset)
				key := makeDrainPlan("plan-claim-done", intelv1a1.RecoveryTypeSlot)

				holder := makeWorkloadPod("finished-holder", drainNode)
				holder.Status.Phase = core.PodSucceeded
				Expect(k8sClient.Status().Update(ctx, holder)).To(Succeed())

				makeHoldingClaim("done-claim", drainWorkloadNS, false, podConsumer(holder))

				_, err := reconcilePlan(ctx, key.Name)
				Expect(err).NotTo(HaveOccurred())

				updated := fetch(key)
				evt := updated.Status.Events[0]
				Expect(evt.PodsBlockingDrain).To(BeEmpty(),
					"a Succeeded pod is not a drain blocker; messages: %v", updated.Status.Messages)
				Expect(evt.ClaimsBlockingReset).To(ContainElement(drainWorkloadNS+"/done-claim"),
					"a reservation that has not been released yet must still hold the reset")
			})

			// A consumer the operator cannot classify must count as a real holder. The DRA API
			// allows non-pod consumers, and guessing they are harmless would reset under one.
			It("should still wait for a claim reserved by a non-pod consumer", func() {
				makeDrainNode(drainNode)
				makeDrainSlice("slice-claim-nonpod", drainNode, drainBDF, deviceTaintKeyReset)
				key := makeDrainPlan("plan-claim-nonpod", intelv1a1.RecoveryTypeSlot)

				makeHoldingClaim("nonpod-claim", drainWorkloadNS, false,
					resv1.ResourceClaimConsumerReference{
						APIGroup: "example.com",
						Resource: "widgets",
						Name:     "some-widget",
						UID:      "77777777-7777-7777-7777-777777777777",
					})

				_, err := reconcilePlan(ctx, key.Name)
				Expect(err).NotTo(HaveOccurred())

				updated := fetch(key)
				Expect(updated.Status.Events[0].ClaimsBlockingReset).To(
					ContainElement(drainWorkloadNS+"/nonpod-claim"),
					"an unclassifiable consumer must not be assumed harmless; messages: %v",
					updated.Status.Messages)
			})
		})

		It("should release the drain taint when the plan is deleted", func() {
			makeDrainNode(drainNode)
			makeDrainSlice("slice-drain-delete", drainNode, drainBDF, deviceTaintKeyReset)
			makeWorkloadPod("delete-victim", drainNode)
			key := makeDrainPlan("plan-drain-delete", intelv1a1.RecoveryTypeSlot)

			_, err := reconcilePlan(ctx, key.Name)
			Expect(err).NotTo(HaveOccurred())
			Expect(nodeTaints(drainNode)).To(ContainElement(recoveryTaint(key.Name)))

			Expect(k8sClient.Delete(ctx, fetch(key))).To(Succeed())

			_, err = reconcilePlan(ctx, key.Name)
			Expect(err).NotTo(HaveOccurred())

			// Once the CR is gone nothing reconciles these taints, so a node missed here stays
			// unschedulable forever with no object left to explain why.
			Expect(nodeTaints(drainNode)).NotTo(ContainElement(recoveryTaint(key.Name)),
				"deleting the plan must not leave nodes cordoned")
		})

		It("should not remove another plan's drain taint", func() {
			makeDrainNode(drainNode)

			// The taint value carries the owning plan's name precisely so two plans can drain the
			// same node without clobbering each other. Two plans on one node is a real
			// configuration: one plan per device ID, and a node can host more than one GPU model.
			// Here the other plan is mid-drain and this one has nothing to do on the node at all —
			// no slice publishes a GPU there, so it has no events.
			other := recoveryTaint("some-other-plan")

			node := &core.Node{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: drainNode}, node)).To(Succeed())
			node.Spec.Taints = append(node.Spec.Taints, other)
			Expect(k8sClient.Update(ctx, node)).To(Succeed())

			key := makeDrainPlan("plan-drain-coexist", intelv1a1.RecoveryTypeSlot)

			_, err := reconcilePlan(ctx, key.Name)
			Expect(err).NotTo(HaveOccurred())

			Expect(fetch(key).Status.Events).To(BeEmpty(),
				"fixture check: this plan must have no event on the node, or the node would be "+
					"in its own keep-set and the cleanup would never run")

			Expect(nodeTaints(drainNode)).To(ContainElement(other),
				"a plan must only ever touch the taint carrying its own name; untainting a "+
					"node another plan is still draining would let pods land mid-reset")
		})
	})

	// drainDeadlineExceeded's two defensive branches are exercised directly rather than through
	// Reconcile: neither is reachable via the API server. The CRD defaults drain.timeoutSeconds to
	// 300 and forbids values below 1, and a nil DrainStartedAt only arises from a status write that
	// was lost. Unreachable-by-construction is a reason to test the function, not a reason to leave
	// the branch unpinned.
	Context("Helper: drainDeadlineExceeded defensive branches", func() {
		It("should treat a zero timeout as the default rather than as 'wait forever'", func() {
			plan := &intelv1a1.GPURecoveryPlan{}
			plan.Spec.Drain.TimeoutSeconds = 0

			long := metav1.NewTime(time.Now().Add(-2 * defaultDrainTimeout))
			evt := &intelv1a1.RecoveryEvent{DrainStartedAt: &long}

			Expect(drainDeadlineExceeded(plan, evt)).To(BeTrue(),
				"an object that bypassed API-server defaulting unmarshals as 0; treating "+
					"that as no timeout is the exact hang the deadline exists to prevent")

			recent := metav1.NewTime(time.Now().Add(-1 * time.Second))
			evt.DrainStartedAt = &recent

			Expect(drainDeadlineExceeded(plan, evt)).To(BeFalse(),
				"the fallback must be the default timeout, not zero")
		})

		It("should re-stamp a missing DrainStartedAt instead of failing the event", func() {
			plan := &intelv1a1.GPURecoveryPlan{}
			plan.Spec.Drain.TimeoutSeconds = 300

			evt := &intelv1a1.RecoveryEvent{DrainStartedAt: nil}

			Expect(drainDeadlineExceeded(plan, evt)).To(BeFalse(),
				"a lost status write should cost one more drain interval, not a recovery")
			Expect(evt.DrainStartedAt).NotTo(BeNil(),
				"the clock must be restarted, or every later pass re-enters this branch and "+
					"the deadline can never be reached at all")
		})
	})

	// needsDrain's fail-safe reading of a nil spec.drain.enable. The CRD defaults the field to
	// true, so nil only arises from an object that bypassed API-server defaulting — and reading nil
	// as "off" would silently drive a PCIe reset into a running workload, which is the one outcome
	// the drain exists to prevent.
	Context("Helper: needsDrain", func() {
		resetEvent := &intelv1a1.RecoveryEvent{
			RecoveryType: intelv1a1.RecoveryTypeSpec{Type: intelv1a1.RecoveryTypeSlot},
		}

		planWithEnable := func(enable *bool) *intelv1a1.GPURecoveryPlan {
			return &intelv1a1.GPURecoveryPlan{
				Spec: intelv1a1.GPURecoveryPlanSpec{
					Drain: intelv1a1.DrainSpec{Enable: enable},
				},
			}
		}

		It("should drain when enable is unset", func() {
			Expect(needsDrain(planWithEnable(nil), resetEvent)).To(BeTrue())
		})

		It("should drain when enable is true", func() {
			Expect(needsDrain(planWithEnable(ptr.To(true)), resetEvent)).To(BeTrue())
		})

		It("should not drain when enable is false", func() {
			Expect(needsDrain(planWithEnable(ptr.To(false)), resetEvent)).To(BeFalse())
		})

		It("should not drain for a reflash whatever enable says", func() {
			reflash := &intelv1a1.RecoveryEvent{
				RecoveryType: intelv1a1.RecoveryTypeSpec{Type: intelv1a1.RecoveryTypeReflash},
			}
			Expect(needsDrain(planWithEnable(ptr.To(true)), reflash)).To(BeFalse())
		})
	})

	// Two recoveries must never overlap on one node: a PCIe reset fired while another card on the
	// same host is mid-write can leave that card unrecoverable. A second approved event therefore
	// parks in blocked, keeps the authorisation it already matched, and starts by itself once the
	// node frees up — no second decision from the admin.
	Context("Node-level recovery exclusion", func() {
		const (
			busyNode  = "busy-node-1"
			otherNode = "other-node-1"
			bdfA      = "0000:02:00.0"
			bdfB      = "0000:03:00.0"
			bdfC      = "0000:04:00.0"
		)

		var r *GPURecoveryPlanReconciler

		BeforeEach(func() {
			r = newTestReconciler()
		})

		// A slot reset: the recovery type is incidental to the gate, which serialises whatever the
		// node is asked to do. The one case where it matters — a reflash, which never drains and so
		// holds no taint — builds its event itself, in the taint context below.
		evtOn := func(id, node, bdf string, state intelv1a1.RecoveryEventState) intelv1a1.RecoveryEvent {
			return intelv1a1.RecoveryEvent{
				ID:           id,
				NodeName:     node,
				GPUBDF:       bdf,
				RecoveryType: intelv1a1.RecoveryTypeSpec{Type: intelv1a1.RecoveryTypeSlot},
				State:        state,
			}
		}

		// planWithEvents installs persistent selector approvals for every recovery type, so an
		// event is approved on sight and the gate is the only thing left deciding what happens to
		// it. Consumption is deliberately out of the picture here — it has its own spec below.
		//
		// The drain is off for the same reason: with it on an approved event stops at draining and
		// the assertions would depend on Node and Pod fixtures that say nothing about the gate. The
		// interaction with the drain is asserted separately, in the taint context.
		planWithEvents := func(name string, events ...intelv1a1.RecoveryEvent) *intelv1a1.GPURecoveryPlan {
			p := &intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{Name: name},
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					DeviceID:         "0x1234",
					MaxRetries:       3,
					XpuSmi:           intelv1a1.XpuSmiSpec{Image: "registry/xpu-smi:latest"},
					Drain:            intelv1a1.DrainSpec{Enable: ptr.To(false)},
					Approvals: []intelv1a1.RecoveryApproval{
						{
							ID:         "app-any-reset",
							Selector:   &intelv1a1.ApprovalSelector{RecoveryType: intelv1a1.RecoveryTypeSlot},
							Persistent: true,
						},
						{
							ID:         "app-any-sbr",
							Selector:   &intelv1a1.ApprovalSelector{RecoveryType: intelv1a1.RecoveryTypeSBR},
							Persistent: true,
						},
					},
				},
				Status: intelv1a1.GPURecoveryPlanStatus{Events: events},
			}

			// Recovery Jobs are owned by the plan, and an owner reference needs a UID.
			createPlanForOwnerRef(p)

			DeferCleanup(func() {
				for _, evt := range p.Status.Events {
					if evt.JobName != "" {
						_ = k8sClient.Delete(ctx, &batch.Job{ObjectMeta: metav1.ObjectMeta{
							Name: evt.JobName, Namespace: "default",
						}})
					}
				}
			})

			return p
		}

		eventByID := func(p *intelv1a1.GPURecoveryPlan, id string) *intelv1a1.RecoveryEvent {
			for i := range p.Status.Events {
				if p.Status.Events[i].ID == id {
					return &p.Status.Events[i]
				}
			}

			return nil
		}

		DescribeTable("should hold back an approved recovery while another owns the node",
			func(holderState intelv1a1.RecoveryEventState) {
				p := planWithEvents("plan-gate-"+strings.ReplaceAll(string(holderState), "-", ""),
					evtOn("evt-holder", busyNode, bdfA, holderState),
					evtOn("evt-queued", busyNode, bdfB, intelv1a1.RecoveryEventStateWaitingApproval),
				)

				r.processApprovals(ctx, p)

				queued := eventByID(p, "evt-queued")
				Expect(queued.State).To(Equal(intelv1a1.RecoveryEventStateBlocked))
				Expect(queued.StateMessage).To(ContainSubstring("evt-holder"))
				Expect(queued.JobName).To(BeEmpty(),
					"a blocked event must not have started a Job")

				// The authorisation is recorded even though nothing ran: that is what lets the event
				// resume without the admin approving it a second time.
				Expect(queued.ApprovalID).To(Equal("app-any-reset"))
				Expect(queued.ApprovalMatchedAt).NotTo(BeNil())

				Expect(eventByID(p, "evt-holder").State).To(Equal(holderState),
					"the recovery that owns the node must be left alone")
			},
			Entry("holder has a Job running", intelv1a1.RecoveryEventStateInProgress),
			// A drain is as much a hold on the node as a reset is: the pods are already going, and
			// starting a second recovery would reset a card while the first node is being emptied.
			Entry("holder is draining the node", intelv1a1.RecoveryEventStateDraining),
		)

		DescribeTable("should not block behind a sibling that is no longer running",
			func(siblingState intelv1a1.RecoveryEventState) {
				p := planWithEvents("plan-nogate-"+strings.ReplaceAll(string(siblingState), "-", ""),
					evtOn("evt-done", busyNode, bdfA, siblingState),
					evtOn("evt-next", busyNode, bdfB, intelv1a1.RecoveryEventStateWaitingApproval),
				)

				r.processApprovals(ctx, p)

				next := eventByID(p, "evt-next")
				Expect(next.State).To(Equal(intelv1a1.RecoveryEventStateInProgress),
					"a terminal sibling holds nothing; the node is free")
				Expect(next.JobName).NotTo(BeEmpty())
			},
			Entry("sibling succeeded", intelv1a1.RecoveryEventStateSucceeded),
			Entry("sibling failed", intelv1a1.RecoveryEventStateFailed),
		)

		It("should not block a recovery on a different node", func() {
			p := planWithEvents("plan-gate-other-node",
				evtOn("evt-busy", busyNode, bdfA, intelv1a1.RecoveryEventStateInProgress),
				evtOn("evt-elsewhere", otherNode, bdfA, intelv1a1.RecoveryEventStateWaitingApproval),
			)

			r.processApprovals(ctx, p)

			// The rule is per node, not per plan: serialising the whole cluster behind one recovery
			// would make a plan spanning a hundred nodes useless.
			elsewhere := eventByID(p, "evt-elsewhere")
			Expect(elsewhere.State).To(Equal(intelv1a1.RecoveryEventStateInProgress))
			Expect(elsewhere.JobName).NotTo(BeEmpty())
		})

		DescribeTable("nodeBusyWith should count only operations actually in flight",
			func(siblingState intelv1a1.RecoveryEventState, expectBusy bool) {
				p := &intelv1a1.GPURecoveryPlan{
					Status: intelv1a1.GPURecoveryPlanStatus{
						Events: []intelv1a1.RecoveryEvent{
							evtOn("evt-sibling", busyNode, bdfA, siblingState),
							evtOn("evt-subject", busyNode, bdfB, intelv1a1.RecoveryEventStateWaitingApproval),
						},
					},
				}

				blocker, busy := nodeBusyWith(p, &p.Status.Events[1])

				Expect(busy).To(Equal(expectBusy))

				if expectBusy {
					Expect(blocker).To(Equal("evt-sibling"),
						"the caller records the blocker in the state message, so it must be named")
				} else {
					Expect(blocker).To(BeEmpty())
				}
			},
			Entry("in-progress", intelv1a1.RecoveryEventStateInProgress, true),
			Entry("draining", intelv1a1.RecoveryEventStateDraining, true),
			Entry("waiting-approval", intelv1a1.RecoveryEventStateWaitingApproval, false),
			// Counting blocked would deadlock a queue of three: the second would hold the third,
			// and none of them would ever be the one to start.
			Entry("blocked", intelv1a1.RecoveryEventStateBlocked, false),
			Entry("missing-firmware", intelv1a1.RecoveryEventStateMissingFirmware, false),
			Entry("succeeded", intelv1a1.RecoveryEventStateSucceeded, false),
			Entry("failed", intelv1a1.RecoveryEventStateFailed, false),
		)

		// The self-check is what stops a running event from being re-examined as its own blocker,
		// which would park it in blocked behind itself and abandon the Job it already has.
		It("should not let an event block itself", func() {
			p := &intelv1a1.GPURecoveryPlan{
				Status: intelv1a1.GPURecoveryPlanStatus{
					Events: []intelv1a1.RecoveryEvent{
						evtOn("evt-only", busyNode, bdfA, intelv1a1.RecoveryEventStateInProgress),
					},
				},
			}

			_, busy := nodeBusyWith(p, &p.Status.Events[0])
			Expect(busy).To(BeFalse())
		})

		It("should start exactly one of several queued recoveries on a node", func() {
			p := planWithEvents("plan-gate-queue",
				evtOn("evt-q1", busyNode, bdfA, intelv1a1.RecoveryEventStateWaitingApproval),
				evtOn("evt-q2", busyNode, bdfB, intelv1a1.RecoveryEventStateWaitingApproval),
				evtOn("evt-q3", busyNode, bdfC, intelv1a1.RecoveryEventStateWaitingApproval),
			)

			r.processApprovals(ctx, p)

			started, blocked := 0, 0

			for _, evt := range p.Status.Events {
				switch evt.State {
				case intelv1a1.RecoveryEventStateInProgress:
					started++
				case intelv1a1.RecoveryEventStateBlocked:
					blocked++
				}
			}

			// All three are approved in the same pass, so the gate has to hold within a single call
			// and not merely across reconciles: nodeBusyWith reads the states the loop has already
			// written, not the states it started with.
			Expect(started).To(Equal(1), "exactly one recovery may run on a node")
			Expect(blocked).To(Equal(2))
		})

		It("should start a blocked recovery once the node frees up", func() {
			p := planWithEvents("plan-gate-resume",
				evtOn("evt-first", busyNode, bdfA, intelv1a1.RecoveryEventStateInProgress),
				evtOn("evt-second", busyNode, bdfB, intelv1a1.RecoveryEventStateWaitingApproval),
			)

			r.processApprovals(ctx, p)
			Expect(eventByID(p, "evt-second").State).To(Equal(intelv1a1.RecoveryEventStateBlocked))

			By("finishing the recovery that held the node")
			eventByID(p, "evt-first").State = intelv1a1.RecoveryEventStateSucceeded

			r.processApprovals(ctx, p)

			second := eventByID(p, "evt-second")
			Expect(second.State).To(Equal(intelv1a1.RecoveryEventStateInProgress),
				"a blocked event must resume on its own, without a new approval")
			Expect(second.JobName).NotTo(BeEmpty())

			// The message described the hold, which is over. Leaving it would have the event read
			// as queued behind a recovery that finished.
			Expect(second.StateMessage).NotTo(ContainSubstring("evt-first"))
		})

		// The gate sits after applyOverride so that an admin's choice of reset mechanism is decided
		// once, when the approval matches, and not re-derived on the pass that happens to find the
		// node free — by which time the approval carrying the override may have been pruned.
		It("should keep an overridden reset type on an event it holds back", func() {
			p := planWithEvents("plan-gate-override",
				evtOn("evt-busy", busyNode, bdfA, intelv1a1.RecoveryEventStateInProgress),
				evtOn("evt-override", busyNode, bdfB, intelv1a1.RecoveryEventStateWaitingApproval),
			)
			p.Spec.Approvals = []intelv1a1.RecoveryApproval{{
				ID:      "app-override",
				EventID: "evt-override",
				Override: &intelv1a1.RecoveryOverride{
					RecoveryType: intelv1a1.RecoveryTypeSBR,
				},
			}}

			r.processApprovals(ctx, p)

			blocked := eventByID(p, "evt-override")
			Expect(blocked.State).To(Equal(intelv1a1.RecoveryEventStateBlocked))
			Expect(blocked.RecoveryType.Type).To(Equal(intelv1a1.RecoveryTypeSBR))
			Expect(blocked.RecoveryType.SuggestedType).To(Equal(intelv1a1.RecoveryTypeSlot))
		})

		// status.messages is capped at maxStatusMessages, and a blocked event is re-examined on
		// every reconcile. One message per pass would rotate out everything worth reading well
		// before the hold ended.
		It("should report the hold once, not once per reconcile", func() {
			p := planWithEvents("plan-gate-message-once",
				evtOn("evt-busy", busyNode, bdfA, intelv1a1.RecoveryEventStateInProgress),
				evtOn("evt-held", busyNode, bdfB, intelv1a1.RecoveryEventStateWaitingApproval),
			)

			for range 5 {
				r.processApprovals(ctx, p)
			}

			held := 0

			for _, msg := range p.Status.Messages {
				if strings.Contains(msg, "held back") {
					held++
				}
			}

			Expect(held).To(Equal(1), "messages: %v", p.Status.Messages)
			Expect(eventByID(p, "evt-held").State).To(Equal(intelv1a1.RecoveryEventStateBlocked))
		})

		It("should requeue while an event is blocked", func() {
			p := &intelv1a1.GPURecoveryPlan{
				Status: intelv1a1.GPURecoveryPlanStatus{
					Events: []intelv1a1.RecoveryEvent{
						evtOn("evt-held", busyNode, bdfB, intelv1a1.RecoveryEventStateBlocked),
					},
				},
			}

			// The sibling that holds the node reaches succeeded in syncJobStatuses, a phase after
			// processApprovals has already re-blocked this event. So on the pass that frees the node
			// nothing else is in flight, and without this the freed recovery would sit until an
			// unrelated ResourceSlice or Job event happened along.
			Expect(hasActiveJobs(p)).To(BeTrue())
		})

		It("should report the plan as active while an event is blocked", func() {
			p := &intelv1a1.GPURecoveryPlan{
				Spec: intelv1a1.GPURecoveryPlanSpec{MaxRetries: 3},
				Status: intelv1a1.GPURecoveryPlanStatus{
					Events: []intelv1a1.RecoveryEvent{
						evtOn("evt-held", busyNode, bdfB, intelv1a1.RecoveryEventStateBlocked),
					},
				},
			}

			updatePlanState(p)

			// Not error: nothing is wrong and nobody has to intervene. The hold clears by itself.
			Expect(p.Status.State).To(Equal(intelv1a1.PlanStateActive))
		})

		It("should drop a blocked event when its device taint clears", func() {
			p := planWithEvents("plan-gate-resolved",
				evtOn("evt-held", busyNode, bdfB, intelv1a1.RecoveryEventStateBlocked),
			)

			// Nothing was done to the GPU, so a cleared taint means the queued reset is no longer
			// wanted — running it later would reset a working card.
			r.removeResolvedEvents(ctx, p, map[deviceKey]deviceNeed{})

			Expect(p.Status.Events).To(BeEmpty())
		})

		// The taint is what stops the scheduler refilling the node in the gap between the holding
		// recovery finishing and this one's drain starting. Without it a queued reset opens by
		// evicting pods that were admitted while nothing held the node.
		Context("drain taint while blocked", func() {
			makeNode := func(name string) {
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

			// taintNode puts the plan's drain taint on the node, as a drain would have, so the
			// specs observe whether reconcileDrainTaints keeps it or lifts it.
			taintNode := func(name, planName string) {
				node := &core.Node{}
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name}, node)).To(Succeed())
				node.Spec.Taints = append(node.Spec.Taints, recoveryTaint(planName))
				Expect(k8sClient.Update(ctx, node)).To(Succeed())
			}

			hasRecoveryTaint := func(name, planName string) bool {
				node := &core.Node{}
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name}, node)).To(Succeed())

				want := recoveryTaint(planName)
				for _, t := range node.Spec.Taints {
					if t.Key == want.Key && t.Value == want.Value {
						return true
					}
				}

				return false
			}

			blockedPlan := func(name, node string, rt intelv1a1.RecoveryType, drain bool) *intelv1a1.GPURecoveryPlan {
				return &intelv1a1.GPURecoveryPlan{
					ObjectMeta: metav1.ObjectMeta{Name: name},
					Spec: intelv1a1.GPURecoveryPlanSpec{
						DefaultResetType: intelv1a1.RecoveryTypeSlot,
						DeviceID:         "0x1234",
						Drain:            intelv1a1.DrainSpec{Enable: ptr.To(drain), TimeoutSeconds: 300},
					},
					Status: intelv1a1.GPURecoveryPlanStatus{
						Events: []intelv1a1.RecoveryEvent{{
							ID:           "evt-held",
							NodeName:     node,
							GPUBDF:       bdfB,
							RecoveryType: intelv1a1.RecoveryTypeSpec{Type: rt},
							State:        intelv1a1.RecoveryEventStateBlocked,
						}},
					},
				}
			}

			It("should keep the node tainted for a blocked reset", func() {
				const (
					nodeName = "gate-taint-keep"
					planName = "plan-gate-taint-keep"
				)

				makeNode(nodeName)
				taintNode(nodeName, planName)

				r.reconcileDrainTaints(ctx, blockedPlan(planName, nodeName, intelv1a1.RecoveryTypeSlot, true))

				Expect(hasRecoveryTaint(nodeName, planName)).To(BeTrue())
			})

			// A reflash never drains, so a taint held on its behalf is one nothing would ask for and
			// nothing on the reflash path would remove.
			It("should not keep the node tainted for a blocked reflash", func() {
				const (
					nodeName = "gate-taint-reflash"
					planName = "plan-gate-taint-reflash"
				)

				makeNode(nodeName)
				taintNode(nodeName, planName)

				r.reconcileDrainTaints(ctx, blockedPlan(planName, nodeName, intelv1a1.RecoveryTypeReflash, true))

				Expect(hasRecoveryTaint(nodeName, planName)).To(BeFalse())
			})

			It("should not keep the node tainted for a blocked reset when the drain is off", func() {
				const (
					nodeName = "gate-taint-nodrain"
					planName = "plan-gate-taint-nodrain"
				)

				makeNode(nodeName)
				taintNode(nodeName, planName)

				r.reconcileDrainTaints(ctx, blockedPlan(planName, nodeName, intelv1a1.RecoveryTypeSlot, false))

				Expect(hasRecoveryTaint(nodeName, planName)).To(BeFalse())
			})
		})

		// A one-shot approval covering several GPUs on one node is only partly spent when the first
		// of them starts. Consuming it there would leave its blocked siblings unmatchable —
		// findMatchingApproval skips consumed approvals — and they would sit in blocked forever,
		// contradicting the "approval retained" guarantee the state exists to give.
		Context("one-shot approvals with a blocked event", func() {
			It("should not consume an approval that still has a blocked event", func() {
				p := planWithEvents("plan-gate-oneshot",
					evtOn("evt-one", busyNode, bdfA, intelv1a1.RecoveryEventStateWaitingApproval),
					evtOn("evt-two", busyNode, bdfB, intelv1a1.RecoveryEventStateWaitingApproval),
				)

				// Replaces the persistent approvals the fixture installs: a persistent approval is
				// never consumed, so it could not show what this spec is about.
				p.Spec.Approvals = []intelv1a1.RecoveryApproval{{
					ID:       "app-oneshot",
					Selector: &intelv1a1.ApprovalSelector{RecoveryType: intelv1a1.RecoveryTypeSlot},
				}}

				r.processApprovals(ctx, p)

				Expect(eventByID(p, "evt-one").State).To(Equal(intelv1a1.RecoveryEventStateInProgress))
				Expect(eventByID(p, "evt-two").State).To(Equal(intelv1a1.RecoveryEventStateBlocked))
				Expect(p.Spec.Approvals[0].Consumed).To(BeFalse(),
					"the approval still owes the blocked event a recovery")

				By("finishing the first recovery")
				eventByID(p, "evt-one").State = intelv1a1.RecoveryEventStateSucceeded

				r.processApprovals(ctx, p)

				Expect(eventByID(p, "evt-two").State).To(Equal(intelv1a1.RecoveryEventStateInProgress),
					"the retained approval must start the second recovery")
				Expect(p.Spec.Approvals[0].Consumed).To(BeTrue(),
					"with nothing left queued, the approval is finally spent")
			})
		})

		It("should accept blocked in the CRD schema", func() {
			const planName = "plan-gate-crd"

			p := &intelv1a1.GPURecoveryPlan{
				ObjectMeta: metav1.ObjectMeta{Name: planName},
				Spec: intelv1a1.GPURecoveryPlanSpec{
					DefaultResetType: intelv1a1.RecoveryTypeSlot,
					DeviceID:         "0x1234",
				},
			}
			Expect(k8sClient.Create(ctx, p)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, p)
			})

			p.Status.Events = []intelv1a1.RecoveryEvent{{
				ID:           "evt-crd-blocked",
				NodeName:     busyNode,
				GPUBDF:       bdfA,
				RecoveryType: intelv1a1.RecoveryTypeSpec{Type: intelv1a1.RecoveryTypeSlot},
				State:        intelv1a1.RecoveryEventStateBlocked,
				LastUpdated:  ptr.To(metav1.NewTime(time.Now())),
			}}
			p.Status.State = intelv1a1.PlanStateActive

			// The enum is declared twice, on the type alias and on the field. A value the controller
			// writes but the schema rejects would fail every status write for the whole plan.
			Expect(k8sClient.Status().Update(ctx, p)).To(Succeed())

			fresh := &intelv1a1.GPURecoveryPlan{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: planName}, fresh)).To(Succeed())
			Expect(fresh.Status.Events[0].State).To(Equal(intelv1a1.RecoveryEventStateBlocked))
		})
	})
})
