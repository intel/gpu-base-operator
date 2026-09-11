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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batch "k8s.io/api/batch/v1"
	core "k8s.io/api/core/v1"
	policy "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	intelv1a1 "github.com/intel/gpu-base-operator/api/v1alpha1"
)

// The drain that runs before a reset: the node is cordoned and emptied of anything
// that could be computing on the GPU, and the reset waits until it is.
var _ = Describe("GPURecoveryPlan Controller: node drain", func() {
	ctx := context.Background()

	BeforeEach(func() {
		ensureDrainWorkloadNS(ctx)
	})

	It("should hold a reset in draining while a workload pod is still on the node", func() {
		makeDrainNode(ctx)
		makeDrainSlice(ctx, "slice-drain-hold", deviceTaintKeyReset)
		makeWorkloadPod(ctx, "drain-victim")
		key := makeDrainPlan(ctx, "plan-drain-hold", intelv1a1.RecoveryTypeSlot)

		res, err := reconcilePlan(ctx, key.Name)
		Expect(err).NotTo(HaveOccurred())

		// Nothing else re-triggers a reconcile when the last pod finally goes away, so a drain
		// that does not requeue stalls until an unrelated event happens along.
		Expect(res.RequeueAfter).To(BeNumerically(">", 0),
			"a draining event must requeue or the drain never progresses")

		updated := fetchPlan(ctx, key)
		Expect(updated.Status.Events).To(HaveLen(1))

		evt := updated.Status.Events[0]
		Expect(evt.State).To(Equal(intelv1a1.RecoveryEventStateDraining),
			"the reset must not start while a pod is still on the node; messages: %v", updated.Status.Messages)
		Expect(evt.JobName).To(BeEmpty(), "no Job may exist before the node is drained")
		Expect(evt.PodsBlockingDrain).To(ContainElement(drainWorkloadNS+"/drain-victim"),
			"an admin needs to see what the drain is waiting on")
		Expect(evt.DrainStartedAt).NotTo(BeNil(), "the deadline clock must start with the drain")

		// The taint is what stops the scheduler refilling the node behind the eviction.
		Expect(nodeTaints(ctx)).To(ContainElement(recoveryTaint(key.Name)))

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
		makeDrainNode(ctx)
		makeDrainSlice(ctx, "slice-drain-clear", deviceTaintKeyReset)
		key := makeDrainPlan(ctx, "plan-drain-clear", intelv1a1.RecoveryTypeSlot)

		// No pods on the node at all, so the drain converges immediately and the Job is
		// created in the same reconcile that started the drain.
		_, err := reconcilePlan(ctx, key.Name)
		Expect(err).NotTo(HaveOccurred())

		updated := fetchPlan(ctx, key)
		Expect(updated.Status.Events).To(HaveLen(1))

		evt := updated.Status.Events[0]
		Expect(evt.State).To(Equal(intelv1a1.RecoveryEventStateInProgress),
			"an empty node needs no waiting; messages: %v", updated.Status.Messages)
		Expect(evt.JobName).NotTo(BeEmpty())
		Expect(evt.PodsBlockingDrain).To(BeEmpty())

		// The taint stays for the duration of the Job: dropping it here would let the
		// scheduler refill the node with pods that then sit through the PCIe reset.
		Expect(nodeTaints(ctx)).To(ContainElement(recoveryTaint(key.Name)),
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
		makeDrainNode(ctx)
		makeDrainSlice(ctx, "slice-drain-reflash", deviceTaintKeyXpumdReflash)
		makeWorkloadPod(ctx, "reflash-bystander")
		key := makeDrainPlan(ctx, "plan-drain-reflash", intelv1a1.RecoveryTypeReflash)

		// Firmware configured, so the reflash really runs: what is under test is that the Job is
		// reached without a drain, not the missing-firmware parking that would also skip one.
		p := fetchPlan(ctx, key)
		p.Spec.Firmware = &intelv1a1.FirmwareSpec{
			Source: intelv1a1.FirmwareSource{
				ContainerSource: &intelv1a1.ContainerFirmwareSource{Name: "registry/fw:v2"},
			},
			File: "gfx.bin",
		}
		Expect(k8sClient.Update(ctx, p)).To(Succeed())

		_, err := reconcilePlan(ctx, key.Name)
		Expect(err).NotTo(HaveOccurred())

		updated := fetchPlan(ctx, key)
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

		Expect(nodeTaints(ctx)).NotTo(ContainElement(recoveryTaint(key.Name)),
			"a reflash must not cordon the node")

		pod := &core.Pod{}
		Expect(k8sClient.Get(ctx,
			types.NamespacedName{Name: "reflash-bystander", Namespace: drainWorkloadNS}, pod)).To(Succeed())
		Expect(pod.DeletionTimestamp).To(BeNil(), "a reflash must not evict unrelated pods")
	})

	It("should skip the drain when spec.drain.enable is false", func() {
		makeDrainNode(ctx)
		makeDrainSlice(ctx, "slice-drain-skip", deviceTaintKeyReset)
		makeWorkloadPod(ctx, "skip-bystander")
		key := makeDrainPlan(ctx, "plan-drain-skip", intelv1a1.RecoveryTypeSlot)

		p := fetchPlan(ctx, key)
		p.Spec.Drain.Enable = ptr.To(false)
		Expect(k8sClient.Update(ctx, p)).To(Succeed())

		_, err := reconcilePlan(ctx, key.Name)
		Expect(err).NotTo(HaveOccurred())

		updated := fetchPlan(ctx, key)
		Expect(updated.Status.Events).To(HaveLen(1))
		Expect(updated.Status.Events[0].State).To(Equal(intelv1a1.RecoveryEventStateInProgress),
			"a disabled drain must go straight to the Job; messages: %v", updated.Status.Messages)

		Expect(nodeTaints(ctx)).NotTo(ContainElement(recoveryTaint(key.Name)),
			"a disabled drain must not cordon the node either")

		pod := &core.Pod{}
		Expect(k8sClient.Get(ctx,
			types.NamespacedName{Name: "skip-bystander", Namespace: drainWorkloadNS}, pod)).To(Succeed())
		Expect(pod.DeletionTimestamp).To(BeNil(), "a disabled drain must not evict anything")
	})

	It("should leave DaemonSet and operator-namespace pods alone", func() {
		makeDrainNode(ctx)
		makeDrainSlice(ctx, "slice-drain-skips", deviceTaintKeyReset)
		key := makeDrainPlan(ctx, "plan-drain-skips", intelv1a1.RecoveryTypeSlot)

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

		updated := fetchPlan(ctx, key)
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

		makeDrainNode(ctx)
		makeDrainSlice(ctx, "slice-drain-nsskip", deviceTaintKeyReset)
		key := makeDrainPlan(ctx, "plan-drain-nsskip", intelv1a1.RecoveryTypeSlot)

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

		makeWorkloadPod(ctx, "nsskip-victim")

		p := fetchPlan(ctx, key)
		p.Spec.Drain.NamespacesToSkip = []string{skipNS}
		Expect(k8sClient.Update(ctx, p)).To(Succeed())

		_, err := reconcilePlan(ctx, key.Name)
		Expect(err).NotTo(HaveOccurred())

		updated := fetchPlan(ctx, key)
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
		makeDrainNode(ctx)
		makeDrainSlice(ctx, "slice-drain-pdb", deviceTaintKeyReset)
		key := makeDrainPlan(ctx, "plan-drain-pdb", intelv1a1.RecoveryTypeSlot)

		pod := makeWorkloadPod(ctx, "pdb-protected")
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

		updated := fetchPlan(ctx, key)
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
		makeDrainNode(ctx)
		makeDrainSlice(ctx, "slice-drain-terminating", deviceTaintKeyReset)
		key := makeDrainPlan(ctx, "plan-drain-terminating", intelv1a1.RecoveryTypeSlot)

		// A pod with a finalizer keeps its deletionTimestamp: envtest has no kubelet to confirm
		// the delete, so this is exactly the shape of a pod on its way out but not yet gone. A
		// drain that treated "terminating" as "gone" would fire the reset while the workload's
		// containers were still running on the GPU.
		pod := makeWorkloadPod(ctx, "slow-goodbye")
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

		updated := fetchPlan(ctx, key)
		Expect(updated.Status.Events).To(HaveLen(1))

		evt := updated.Status.Events[0]
		Expect(evt.State).To(Equal(intelv1a1.RecoveryEventStateDraining),
			"a terminating pod still occupies the node; messages: %v", updated.Status.Messages)
		Expect(evt.JobName).To(BeEmpty())
		Expect(evt.PodsBlockingDrain).To(ContainElement(drainWorkloadNS + "/slow-goodbye"))
	})

	It("should fail the event and untaint the node when the drain deadline passes", func() {
		makeDrainNode(ctx)
		makeDrainSlice(ctx, "slice-drain-timeout", deviceTaintKeyReset)
		makeWorkloadPod(ctx, "stuck-pod")
		key := makeDrainPlan(ctx, "plan-drain-timeout", intelv1a1.RecoveryTypeSlot)

		// Pass 1: the drain starts and stalls on the pod.
		_, err := reconcilePlan(ctx, key.Name)
		Expect(err).NotTo(HaveOccurred())

		updated := fetchPlan(ctx, key)
		Expect(updated.Status.Events[0].State).To(Equal(intelv1a1.RecoveryEventStateDraining))

		// Backdate the clock rather than sleeping out a real timeout.
		updated.Status.Events[0].DrainStartedAt = ptr.To(metav1.NewTime(time.Now().Add(-10 * time.Minute)))
		Expect(k8sClient.Status().Update(ctx, updated)).To(Succeed())

		// Pass 2: the deadline has passed.
		_, err = reconcilePlan(ctx, key.Name)
		Expect(err).NotTo(HaveOccurred())

		updated = fetchPlan(ctx, key)
		evt := updated.Status.Events[0]

		// Failing is the point: an unsatisfiable PodDisruptionBudget or a pod stuck on a
		// finalizer would otherwise park the event in draining forever with no diagnostic.
		Expect(evt.State).To(Equal(intelv1a1.RecoveryEventStateFailed),
			"a drain that cannot finish must fail rather than hang; messages: %v", updated.Status.Messages)
		Expect(evt.JobName).To(BeEmpty(), "the reset must not run after a failed drain")
		Expect(evt.PastJobs).To(BeEmpty(), "no Job ran, so there is nothing to keep for diagnostics")

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
		Expect(nodeTaints(ctx)).NotTo(ContainElement(recoveryTaint(key.Name)),
			"a failed drain must release the node")
	})

	// The two specs below cover the ways of staying in draining that do NOT go through the
	// blocking-pod path, which is where the deadline used to be checked. Both looped for ever:
	// the drain reported perfect progress — an empty node, no blockers — while nothing was timing
	// it, and the plan sat in draining with the node cordoned indefinitely.
	It("should fail the event when a drained node will not accept the recovery Job", func() {
		makeDrainNode(ctx)
		makeDrainSlice(ctx, "slice-drain-nojob", deviceTaintKeyReset)
		key := makeDrainPlan(ctx, "plan-drain-nojob", intelv1a1.RecoveryTypeSlot)

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

		updated := fetchPlan(ctx, key)
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

		updated = fetchPlan(ctx, key)
		evt = updated.Status.Events[0]

		Expect(evt.State).To(Equal(intelv1a1.RecoveryEventStateFailed),
			"an event that cannot start its Job must give up at the deadline, not retry for ever; messages: %v",
			updated.Status.Messages)
		Expect(evt.StateMessage).To(SatisfyAll(
			ContainSubstring("timed out"),
			ContainSubstring("could not be created"),
		), "the failure must name what stopped it, which is the Job and not a pod")

		Expect(nodeTaints(ctx)).NotTo(ContainElement(recoveryTaint(key.Name)),
			"a node emptied for a reset that never started must not stay cordoned")
	})

	It("should fail the event when the drain itself cannot be carried out", func() {
		makeDrainNode(ctx)
		makeDrainSlice(ctx, "slice-drain-nocordon", deviceTaintKeyReset)
		key := makeDrainPlan(ctx, "plan-drain-nocordon", intelv1a1.RecoveryTypeSlot)

		reconcileRejectingNodeWrites := func() {
			r := newTestReconciler()
			r.Client = &nodeWriteRejectingClient{Client: k8sClient}

			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred(),
				"one unreachable node must not fail the reconcile for every other GPU")
		}

		// Pass 1: the cordon fails, so the drain never gets as far as looking at pods.
		reconcileRejectingNodeWrites()

		updated := fetchPlan(ctx, key)
		Expect(updated.Status.Events).To(HaveLen(1))
		Expect(updated.Status.Events[0].State).To(Equal(intelv1a1.RecoveryEventStateDraining),
			"a transient node write failure is worth retrying; messages: %v", updated.Status.Messages)

		updated.Status.Events[0].DrainStartedAt = ptr.To(metav1.NewTime(time.Now().Add(-10 * time.Minute)))
		Expect(k8sClient.Status().Update(ctx, updated)).To(Succeed())

		// Pass 2: still failing, and now out of time.
		reconcileRejectingNodeWrites()

		evt := fetchPlan(ctx, key).Status.Events[0]
		Expect(evt.State).To(Equal(intelv1a1.RecoveryEventStateFailed),
			"a drain that cannot be performed at all must still hit the deadline")
		Expect(evt.StateMessage).To(SatisfyAll(
			ContainSubstring("timed out"),
			ContainSubstring("not progressing"),
		), "the failure must say the drain never ran, not that pods were in the way")
	})

	It("should release the drain taint when the plan is deleted", func() {
		makeDrainNode(ctx)
		makeDrainSlice(ctx, "slice-drain-delete", deviceTaintKeyReset)
		makeWorkloadPod(ctx, "delete-victim")
		key := makeDrainPlan(ctx, "plan-drain-delete", intelv1a1.RecoveryTypeSlot)

		_, err := reconcilePlan(ctx, key.Name)
		Expect(err).NotTo(HaveOccurred())
		Expect(nodeTaints(ctx)).To(ContainElement(recoveryTaint(key.Name)))

		Expect(k8sClient.Delete(ctx, fetchPlan(ctx, key))).To(Succeed())

		_, err = reconcilePlan(ctx, key.Name)
		Expect(err).NotTo(HaveOccurred())

		// Once the CR is gone nothing reconciles these taints, so a node missed here stays
		// unschedulable forever with no object left to explain why.
		Expect(nodeTaints(ctx)).NotTo(ContainElement(recoveryTaint(key.Name)),
			"deleting the plan must not leave nodes cordoned")
	})

	It("should not remove another plan's drain taint", func() {
		makeDrainNode(ctx)

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

		key := makeDrainPlan(ctx, "plan-drain-coexist", intelv1a1.RecoveryTypeSlot)

		_, err := reconcilePlan(ctx, key.Name)
		Expect(err).NotTo(HaveOccurred())

		Expect(fetchPlan(ctx, key).Status.Events).To(BeEmpty(),
			"fixture check: this plan must have no event on the node, or the node would be "+
				"in its own keep-set and the cleanup would never run")

		Expect(nodeTaints(ctx)).To(ContainElement(other),
			"a plan must only ever touch the taint carrying its own name; untainting a "+
				"node another plan is still draining would let pods land mid-reset")
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
})
