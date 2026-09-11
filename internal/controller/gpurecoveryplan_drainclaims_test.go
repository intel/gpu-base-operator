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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	core "k8s.io/api/core/v1"
	resv1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	intelv1a1 "github.com/intel/gpu-base-operator/api/v1alpha1"
)

// ResourceClaims against the GPU being reset: a claim the drain can still release is
// a reason to wait, and one it never could is not.
var _ = Describe("GPURecoveryPlan Controller: node drain and ResourceClaims", func() {
	ctx := context.Background()

	BeforeEach(func() {
		ensureDrainWorkloadNS(ctx)
	})

	It("should hold the reset while a ResourceClaim still reserves the GPU", func() {
		makeDrainNode(ctx)
		makeDrainSlice(ctx, "slice-drain-claim", deviceTaintKeyReset)
		key := makeDrainPlan(ctx, "plan-drain-claim", intelv1a1.RecoveryTypeSlot)

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

		updated := fetchPlan(ctx, key)
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
		makeDrainNode(ctx)
		makeDrainSlice(ctx, "slice-drain-unreserved", deviceTaintKeyReset)
		key := makeDrainPlan(ctx, "plan-drain-unreserved", intelv1a1.RecoveryTypeSlot)

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

		updated := fetchPlan(ctx, key)
		Expect(updated.Status.Events).To(HaveLen(1))
		Expect(updated.Status.Events[0].State).To(Equal(intelv1a1.RecoveryEventStateInProgress),
			"an unreserved claim holds no device; messages: %v", updated.Status.Messages)
		Expect(updated.Status.Events[0].ClaimsBlockingReset).To(BeEmpty())
	})

	It("should ignore a ResourceClaim holding a different GPU", func() {
		makeDrainNode(ctx)
		makeDrainSlice(ctx, "slice-drain-other", deviceTaintKeyReset)
		key := makeDrainPlan(ctx, "plan-drain-other", intelv1a1.RecoveryTypeSlot)

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

		updated := fetchPlan(ctx, key)
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

			updated := fetchPlan(ctx, key)
			Expect(updated.Status.Events).To(HaveLen(1))
			Expect(updated.Status.Events[0].State).To(Equal(intelv1a1.RecoveryEventStateInProgress),
				because+"; messages: %v", updated.Status.Messages)
			Expect(updated.Status.Events[0].ClaimsBlockingReset).To(BeEmpty())
		}

		// adminAccess is monitoring access: DRA hands out the device without reserving it, so
		// the same GPU stays allocatable to workloads and the claim proves nothing about
		// whether anyone is computing on it.
		It("should not wait for an admin-access claim", func() {
			makeDrainNode(ctx)
			makeDrainSlice(ctx, "slice-claim-admin", deviceTaintKeyReset)
			key := makeDrainPlan(ctx, "plan-claim-admin", intelv1a1.RecoveryTypeSlot)

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
			holder := makeWorkloadPod(ctx, "admin-holder")
			makeHoldingClaim("admin-claim", drainWorkloadNS, true, podConsumer(holder))

			_, err := reconcilePlan(ctx, key.Name)
			Expect(err).NotTo(HaveOccurred())

			updated := fetchPlan(ctx, key)
			evt := updated.Status.Events[0]
			Expect(evt.ClaimsBlockingReset).To(BeEmpty(),
				"an admin-access claim does not reserve the device; messages: %v", updated.Status.Messages)
			Expect(evt.PodsBlockingDrain).To(ContainElement(drainWorkloadNS+"/admin-holder"),
				"the holder pod is still an ordinary drain blocker")
		})

		// The operator's own namespace: nothing in it is ever evicted, so nothing in it can
		// ever release a claim.
		It("should not wait for a claim held by a pod in the operator namespace", func() {
			makeDrainNode(ctx)
			makeDrainSlice(ctx, "slice-claim-operator", deviceTaintKeyReset)
			key := makeDrainPlan(ctx, "plan-claim-operator", intelv1a1.RecoveryTypeSlot)

			// "default" is the operator namespace here, per newTestReconciler.
			holder := makeUnevictablePod("xpumd-operator-ns", "default")
			makeHoldingClaim("operator-ns-claim", "default", false, podConsumer(holder))

			expectReset(key, "a claim in the operator namespace can never be released by draining")
		})

		// Same deadlock without the operator namespace: any GPU-using DaemonSet anywhere
		// reaches it, because a DaemonSet pod is never evicted.
		It("should not wait for a claim held by a DaemonSet pod elsewhere", func() {
			makeDrainNode(ctx)
			makeDrainSlice(ctx, "slice-claim-ds", deviceTaintKeyReset)
			key := makeDrainPlan(ctx, "plan-claim-ds", intelv1a1.RecoveryTypeSlot)

			holder := makeUnevictablePod("monitor-ds", drainWorkloadNS)
			makeHoldingClaim("ds-claim", drainWorkloadNS, false, podConsumer(holder))

			expectReset(key, "a DaemonSet pod is never evicted, so its claim is never released")
		})

		// spec.drain.namespacesToSkip opens the same hole by configuration, which is why it is
		// documented as "do not list namespaces that run GPU workloads": the drain's skip set
		// and the set of claims that cannot block are the same set, on purpose.
		It("should not wait for a claim held by a pod in a skipped namespace", func() {
			makeDrainNode(ctx)
			makeDrainSlice(ctx, "slice-claim-skipns", deviceTaintKeyReset)
			key := makeDrainPlan(ctx, "plan-claim-skipns", intelv1a1.RecoveryTypeSlot)

			p := fetchPlan(ctx, key)
			p.Spec.Drain.NamespacesToSkip = []string{drainWorkloadNS}
			Expect(k8sClient.Update(ctx, p)).To(Succeed())

			// An ordinary pod: only the skip list makes it unevictable, so this also pins that
			// the claim check reads the same list the drain does.
			holder := makeWorkloadPod(ctx, "skipped-holder")
			makeHoldingClaim("skipns-claim", drainWorkloadNS, false, podConsumer(holder))

			expectReset(key, "a pod the drain skips by namespace cannot release its claim either")
		})

		// The other half of the rule: nothing above may weaken the check for a claim the drain
		// *can* release.
		It("should still wait for a claim held by an evictable pod", func() {
			makeDrainNode(ctx)
			makeDrainSlice(ctx, "slice-claim-evictable", deviceTaintKeyReset)
			key := makeDrainPlan(ctx, "plan-claim-evictable", intelv1a1.RecoveryTypeSlot)

			holder := makeWorkloadPod(ctx, "evictable-holder")
			makeHoldingClaim("evictable-claim", drainWorkloadNS, false, podConsumer(holder))

			_, err := reconcilePlan(ctx, key.Name)
			Expect(err).NotTo(HaveOccurred())

			updated := fetchPlan(ctx, key)
			evt := updated.Status.Events[0]
			Expect(evt.State).To(Equal(intelv1a1.RecoveryEventStateDraining),
				"an evictable holder is exactly what the in-use check exists for; messages: %v",
				updated.Status.Messages)
			Expect(evt.ClaimsBlockingReset).To(ContainElement(drainWorkloadNS + "/evictable-claim"))
		})

		// A mixed claim must block. Anything else lets one unevictable co-consumer hide a live
		// workload holding the same device.
		It("should still wait for a claim held by both an unevictable and an evictable pod", func() {
			makeDrainNode(ctx)
			makeDrainSlice(ctx, "slice-claim-mixed", deviceTaintKeyReset)
			key := makeDrainPlan(ctx, "plan-claim-mixed", intelv1a1.RecoveryTypeSlot)

			infra := makeUnevictablePod("mixed-ds", drainWorkloadNS)
			workload := makeWorkloadPod(ctx, "mixed-workload")
			makeHoldingClaim("mixed-claim", drainWorkloadNS, false,
				podConsumer(infra), podConsumer(workload))

			_, err := reconcilePlan(ctx, key.Name)
			Expect(err).NotTo(HaveOccurred())

			updated := fetchPlan(ctx, key)
			Expect(updated.Status.Events[0].ClaimsBlockingReset).To(
				ContainElement(drainWorkloadNS+"/mixed-claim"),
				"one unevictable consumer must not excuse the whole claim; messages: %v",
				updated.Status.Messages)
		})

		// A reservation whose pod is gone is the window the claim check was added for: the pod
		// object disappears before the kubelet has finished unpreparing the device. It must
		// keep blocking, with the drain deadline as the only backstop.
		It("should still wait for a reservation whose pod no longer exists", func() {
			makeDrainNode(ctx)
			makeDrainSlice(ctx, "slice-claim-ghost", deviceTaintKeyReset)
			key := makeDrainPlan(ctx, "plan-claim-ghost", intelv1a1.RecoveryTypeSlot)

			makeHoldingClaim("ghost-claim", drainWorkloadNS, false,
				resv1.ResourceClaimConsumerReference{
					Resource: "pods",
					Name:     "long-gone",
					UID:      "55555555-5555-5555-5555-555555555555",
				})

			_, err := reconcilePlan(ctx, key.Name)
			Expect(err).NotTo(HaveOccurred())

			updated := fetchPlan(ctx, key)
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
			makeDrainNode(ctx)
			makeDrainSlice(ctx, "slice-claim-uid", deviceTaintKeyReset)
			key := makeDrainPlan(ctx, "plan-claim-uid", intelv1a1.RecoveryTypeSlot)

			live := makeUnevictablePod("reused-name", drainWorkloadNS)
			stale := podConsumer(live)
			stale.UID = "66666666-6666-6666-6666-666666666666"
			makeHoldingClaim("uid-claim", drainWorkloadNS, false, stale)

			_, err := reconcilePlan(ctx, key.Name)
			Expect(err).NotTo(HaveOccurred())

			updated := fetchPlan(ctx, key)
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
			makeDrainNode(ctx)
			makeDrainSlice(ctx, "slice-claim-done", deviceTaintKeyReset)
			key := makeDrainPlan(ctx, "plan-claim-done", intelv1a1.RecoveryTypeSlot)

			holder := makeWorkloadPod(ctx, "finished-holder")
			holder.Status.Phase = core.PodSucceeded
			Expect(k8sClient.Status().Update(ctx, holder)).To(Succeed())

			makeHoldingClaim("done-claim", drainWorkloadNS, false, podConsumer(holder))

			_, err := reconcilePlan(ctx, key.Name)
			Expect(err).NotTo(HaveOccurred())

			updated := fetchPlan(ctx, key)
			evt := updated.Status.Events[0]
			Expect(evt.PodsBlockingDrain).To(BeEmpty(),
				"a Succeeded pod is not a drain blocker; messages: %v", updated.Status.Messages)
			Expect(evt.ClaimsBlockingReset).To(ContainElement(drainWorkloadNS+"/done-claim"),
				"a reservation that has not been released yet must still hold the reset")
		})

		// A consumer the operator cannot classify must count as a real holder. The DRA API
		// allows non-pod consumers, and guessing they are harmless would reset under one.
		It("should still wait for a claim reserved by a non-pod consumer", func() {
			makeDrainNode(ctx)
			makeDrainSlice(ctx, "slice-claim-nonpod", deviceTaintKeyReset)
			key := makeDrainPlan(ctx, "plan-claim-nonpod", intelv1a1.RecoveryTypeSlot)

			makeHoldingClaim("nonpod-claim", drainWorkloadNS, false,
				resv1.ResourceClaimConsumerReference{
					APIGroup: "example.com",
					Resource: "widgets",
					Name:     "some-widget",
					UID:      "77777777-7777-7777-7777-777777777777",
				})

			_, err := reconcilePlan(ctx, key.Name)
			Expect(err).NotTo(HaveOccurred())

			updated := fetchPlan(ctx, key)
			Expect(updated.Status.Events[0].ClaimsBlockingReset).To(
				ContainElement(drainWorkloadNS+"/nonpod-claim"),
				"an unclassifiable consumer must not be assumed harmless; messages: %v",
				updated.Status.Messages)
		})
	})
})
