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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	core "k8s.io/api/core/v1"
	resv1 "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// The primitives in drain.go are exercised against a fake client rather than envtest: every one
// of them is a pure function of what the API returns, and the fake client is the only way to
// provoke the List/Get/Update failures the error paths exist for.
//
// One thing the fake client does not model is the PodDisruptionBudget check behind the eviction
// subresource — it deletes the pod unconditionally. The eviction specs below therefore pin that
// evictPod goes through the subresource at all (rather than deleting outright, which would ignore
// PDBs), not what the API server does with a budget that forbids it.

func drainScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	Expect(core.AddToScheme(s)).To(Succeed())
	Expect(resv1.AddToScheme(s)).To(Succeed())

	return s
}

// drainClientBuilder is a fake client builder carrying the same spec.nodeName pod index that
// SetupDrainIndexes registers on a real manager's cache, so podsOnNode behaves here as it does in
// the cluster — and the index function itself is covered by every spec that lists pods.
func drainClientBuilder() *fake.ClientBuilder {
	return fake.NewClientBuilder().WithScheme(drainScheme()).
		WithIndex(&core.Pod{}, podNodeNameIndex, indexPodByNodeName)
}

// drainTestNode is a node carrying the given taints.
func drainTestNode(name string, taints ...core.Taint) *core.Node {
	return &core.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       core.NodeSpec{Taints: taints},
	}
}

// drainTestPod is a running pod on a node, with no GPU of any kind.
func drainTestPod(name, namespace, nodeName string) *core.Pod {
	return &core.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: core.PodSpec{
			NodeName:   nodeName,
			Containers: []core.Container{{Name: "c", Image: "busybox"}},
		},
		Status: core.PodStatus{Phase: core.PodRunning},
	}
}

// withGPUResource adds an extended-resource GPU request, the device plugin's way of holding a GPU.
func withGPUResource(pod *core.Pod, name core.ResourceName) *core.Pod {
	count, err := resource.ParseQuantity("1")
	Expect(err).NotTo(HaveOccurred())

	pod.Spec.Containers[0].Resources.Limits = core.ResourceList{name: count}

	return pod
}

// withClaim references a ResourceClaim by name, DRA's way of holding a GPU.
func withClaim(pod *core.Pod, claimName string) *core.Pod {
	pod.Spec.ResourceClaims = []core.PodResourceClaim{
		{Name: "gpu", ResourceClaimName: &claimName},
	}

	return pod
}

// withClaimTemplate references a ResourceClaimTemplate, the other DRA shape.
func withClaimTemplate(pod *core.Pod, templateName string) *core.Pod {
	pod.Spec.ResourceClaims = []core.PodResourceClaim{
		{Name: "gpu", ResourceClaimTemplateName: &templateName},
	}

	return pod
}

// gpuClaim is a ResourceClaim requesting a device from the given device class.
func gpuClaim(name, namespace, deviceClass string) *resv1.ResourceClaim {
	return &resv1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: resv1.ResourceClaimSpec{
			Devices: resv1.DeviceClaim{
				Requests: []resv1.DeviceRequest{
					{
						Name:    "req",
						Exactly: &resv1.ExactDeviceRequest{DeviceClassName: deviceClass},
					},
				},
			},
		},
	}
}

var _ = Describe("Drain primitives", func() {
	ctx := context.Background()

	noSchedule := func(key, value string) core.Taint {
		return core.Taint{Key: key, Value: value, Effect: core.TaintEffectNoSchedule}
	}

	Context("taintsEqual", func() {
		// The whole reason this helper exists instead of == on the struct.
		It("should ignore TimeAdded", func() {
			now := metav1.NewTime(time.Now())

			a := noSchedule("k", "v")
			b := noSchedule("k", "v")
			b.TimeAdded = &now

			Expect(taintsEqual(a, b)).To(BeTrue(),
				"the API server sets TimeAdded itself, so comparing it would make our own taint "+
					"look foreign on the next pass and it would be added a second time")
		})

		It("should distinguish taints by value", func() {
			// The value carries the owning CR's name, which is what keeps two CRs tainting the
			// same node from clobbering each other's entry.
			Expect(taintsEqual(noSchedule("k", "cr-a"), noSchedule("k", "cr-b"))).To(BeFalse())
		})

		It("should distinguish taints by effect", func() {
			a := noSchedule("k", "v")
			b := a
			b.Effect = core.TaintEffectNoExecute

			Expect(taintsEqual(a, b)).To(BeFalse())
		})
	})

	Context("ensureNodeTaint", func() {
		taint := noSchedule("gpu-update-in-progress", "fu-1")

		It("should add a taint the node does not have", func() {
			c := drainClientBuilder().
				WithObjects(drainTestNode("node-1")).Build()

			added, err := ensureNodeTaint(ctx, c, "node-1", taint)
			Expect(err).NotTo(HaveOccurred())
			Expect(added).To(BeTrue())

			node := &core.Node{}
			Expect(c.Get(ctx, types.NamespacedName{Name: "node-1"}, node)).To(Succeed())
			Expect(node.Spec.Taints).To(ConsistOf(taint))
		})

		It("should report added=false and not duplicate an existing taint", func() {
			// This is what makes the taint step idempotent across requeues: a repeat pass has to
			// be distinguishable from a first one without re-reading the node.
			c := drainClientBuilder().
				WithObjects(drainTestNode("node-1", taint)).Build()

			added, err := ensureNodeTaint(ctx, c, "node-1", taint)
			Expect(err).NotTo(HaveOccurred())
			Expect(added).To(BeFalse())

			node := &core.Node{}
			Expect(c.Get(ctx, types.NamespacedName{Name: "node-1"}, node)).To(Succeed())
			Expect(node.Spec.Taints).To(HaveLen(1))
		})

		It("should not re-add a taint that only differs in TimeAdded", func() {
			now := metav1.NewTime(time.Now())
			stamped := taint
			stamped.TimeAdded = &now

			c := drainClientBuilder().
				WithObjects(drainTestNode("node-1", stamped)).Build()

			added, err := ensureNodeTaint(ctx, c, "node-1", taint)
			Expect(err).NotTo(HaveOccurred())
			Expect(added).To(BeFalse())

			node := &core.Node{}
			Expect(c.Get(ctx, types.NamespacedName{Name: "node-1"}, node)).To(Succeed())
			Expect(node.Spec.Taints).To(HaveLen(1), "a stamped taint is still our taint")
		})

		It("should keep taints set by somebody else", func() {
			foreign := noSchedule("other.example.com/thing", "x")

			c := drainClientBuilder().
				WithObjects(drainTestNode("node-1", foreign)).Build()

			_, err := ensureNodeTaint(ctx, c, "node-1", taint)
			Expect(err).NotTo(HaveOccurred())

			node := &core.Node{}
			Expect(c.Get(ctx, types.NamespacedName{Name: "node-1"}, node)).To(Succeed())
			Expect(node.Spec.Taints).To(ConsistOf(foreign, taint))
		})

		It("should fail when the node is gone", func() {
			c := drainClientBuilder().Build()

			added, err := ensureNodeTaint(ctx, c, "no-such-node", taint)
			Expect(err).To(HaveOccurred())
			Expect(added).To(BeFalse())
		})
	})

	Context("removeNodeTaint", func() {
		taint := noSchedule("gpu-update-in-progress", "fu-1")

		It("should remove the taint and leave the others", func() {
			foreign := noSchedule("other.example.com/thing", "x")

			c := drainClientBuilder().
				WithObjects(drainTestNode("node-1", foreign, taint)).Build()

			removed, err := removeNodeTaint(ctx, c, "node-1", taint)
			Expect(err).NotTo(HaveOccurred())
			Expect(removed).To(BeTrue())

			node := &core.Node{}
			Expect(c.Get(ctx, types.NamespacedName{Name: "node-1"}, node)).To(Succeed())
			Expect(node.Spec.Taints).To(ConsistOf(foreign))
		})

		It("should remove a taint the API server stamped with TimeAdded", func() {
			// The bug this replaced: comparing whole structs made a stamped taint unfindable, so
			// the node was reported as having lost the taint while still carrying it.
			now := metav1.NewTime(time.Now())
			stamped := taint
			stamped.TimeAdded = &now

			c := drainClientBuilder().
				WithObjects(drainTestNode("node-1", stamped)).Build()

			removed, err := removeNodeTaint(ctx, c, "node-1", taint)
			Expect(err).NotTo(HaveOccurred())
			Expect(removed).To(BeTrue())

			node := &core.Node{}
			Expect(c.Get(ctx, types.NamespacedName{Name: "node-1"}, node)).To(Succeed())
			Expect(node.Spec.Taints).To(BeEmpty())
		})

		It("should report removed=false rather than an error when the taint is absent", func() {
			// An admin removing it by hand is not a failure: the caller only wants it gone.
			c := drainClientBuilder().
				WithObjects(drainTestNode("node-1")).Build()

			removed, err := removeNodeTaint(ctx, c, "node-1", taint)
			Expect(err).NotTo(HaveOccurred())
			Expect(removed).To(BeFalse())
		})

		It("should not write to the node when there is nothing to remove", func() {
			c := drainClientBuilder().
				WithObjects(drainTestNode("node-1")).
				WithInterceptorFuncs(interceptor.Funcs{
					Update: func(_ context.Context, _ client.WithWatch, _ client.Object, _ ...client.UpdateOption) error {
						Fail("removeNodeTaint must not update a node it changed nothing on")

						return nil
					},
				}).Build()

			_, err := removeNodeTaint(ctx, c, "node-1", taint)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("nodesWithTaint", func() {
		taint := noSchedule("gpurecovery", "plan-1")

		It("should find every node carrying the taint and no others", func() {
			// Asking the cluster rather than a list in CR status is what lets a taint survive a
			// lost status write and still be cleaned up.
			c := drainClientBuilder().WithObjects(
				drainTestNode("tainted-1", taint),
				drainTestNode("clean"),
				drainTestNode("tainted-2", noSchedule("unrelated", "y"), taint),
				drainTestNode("other-value", noSchedule("gpurecovery", "plan-2")),
			).Build()

			names, err := nodesWithTaint(ctx, c, taint)
			Expect(err).NotTo(HaveOccurred())
			Expect(names).To(ConsistOf("tainted-1", "tainted-2"))
		})

		It("should return nothing when no node carries it", func() {
			c := drainClientBuilder().
				WithObjects(drainTestNode("clean")).Build()

			names, err := nodesWithTaint(ctx, c, taint)
			Expect(err).NotTo(HaveOccurred())
			Expect(names).To(BeEmpty())
		})
	})

	Context("podsOnNode", func() {
		It("should return the pods on that node across all namespaces", func() {
			c := drainClientBuilder().WithObjects(
				drainTestPod("a", "default", "node-1"),
				drainTestPod("b", "kube-system", "node-1"),
				drainTestPod("c", "default", "node-2"),
			).Build()

			pods, err := podsOnNode(ctx, c, "node-1")
			Expect(err).NotTo(HaveOccurred())

			names := []string{}
			for _, pod := range pods {
				names = append(names, pod.Namespace+"/"+pod.Name)
			}

			Expect(names).To(ConsistOf("default/a", "kube-system/b"))
		})

		It("should skip unscheduled pods", func() {
			// A pod with an empty spec.nodeName is on no node, and must not match one.
			c := drainClientBuilder().WithObjects(
				drainTestPod("pending", "default", ""),
			).Build()

			pods, err := podsOnNode(ctx, c, "node-1")
			Expect(err).NotTo(HaveOccurred())
			Expect(pods).To(BeEmpty())
		})

		It("should return an empty list, not an error, for a node with no pods", func() {
			c := drainClientBuilder().Build()

			pods, err := podsOnNode(ctx, c, "node-1")
			Expect(err).NotTo(HaveOccurred())
			Expect(pods).To(BeEmpty())
		})
	})

	Context("requestsIntelGPU", func() {
		It("should match an exact request for the Intel GPU device class", func() {
			Expect(requestsIntelGPU([]resv1.DeviceRequest{
				{Exactly: &resv1.ExactDeviceRequest{DeviceClassName: gpuDraDeviceClass}},
			})).To(BeTrue())
		})

		It("should match an Intel GPU among firstAvailable alternatives", func() {
			// A firstAvailable list is a set of alternatives, any one of which may end up
			// allocated, so a single Intel GPU entry makes the pod a GPU pod.
			Expect(requestsIntelGPU([]resv1.DeviceRequest{
				{FirstAvailable: []resv1.DeviceSubRequest{
					{DeviceClassName: "other.example.com"},
					{DeviceClassName: gpuDraDeviceClass},
				}},
			})).To(BeTrue())
		})

		It("should not match another vendor's device class", func() {
			Expect(requestsIntelGPU([]resv1.DeviceRequest{
				{Exactly: &resv1.ExactDeviceRequest{DeviceClassName: "other.example.com"}},
			})).To(BeFalse())
		})

		It("should not match an empty request list", func() {
			Expect(requestsIntelGPU(nil)).To(BeFalse())
		})
	})

	Context("gpuPodsOnNode", func() {
		It("should include pods holding a GPU through either driver's extended resource", func() {
			c := drainClientBuilder().WithObjects(
				withGPUResource(drainTestPod("xe", "default", "node-1"), xeResource),
				withGPUResource(drainTestPod("i915", "default", "node-1"), i915Resource),
				drainTestPod("plain", "default", "node-1"),
			).Build()

			pods, err := gpuPodsOnNode(ctx, c, "node-1")
			Expect(err).NotTo(HaveOccurred())

			names := []string{}
			for _, pod := range pods {
				names = append(names, pod.Name)
			}

			Expect(names).To(ConsistOf("xe", "i915"))
		})

		It("should include a pod holding a GPU through a ResourceClaim", func() {
			c := drainClientBuilder().WithObjects(
				withClaim(drainTestPod("dra", "default", "node-1"), "claim-gpu"),
				gpuClaim("claim-gpu", "default", gpuDraDeviceClass),
			).Build()

			pods, err := gpuPodsOnNode(ctx, c, "node-1")
			Expect(err).NotTo(HaveOccurred())
			Expect(pods).To(HaveLen(1))
			Expect(pods[0].Name).To(Equal("dra"))
		})

		It("should include a pod holding a GPU through a ResourceClaimTemplate", func() {
			c := drainClientBuilder().WithObjects(
				withClaimTemplate(drainTestPod("dra-tmpl", "default", "node-1"), "tmpl-gpu"),
				&resv1.ResourceClaimTemplate{
					ObjectMeta: metav1.ObjectMeta{Name: "tmpl-gpu", Namespace: "default"},
					Spec: resv1.ResourceClaimTemplateSpec{
						Spec: gpuClaim("ignored", "default", gpuDraDeviceClass).Spec,
					},
				},
			).Build()

			pods, err := gpuPodsOnNode(ctx, c, "node-1")
			Expect(err).NotTo(HaveOccurred())
			Expect(pods).To(HaveLen(1))
			Expect(pods[0].Name).To(Equal("dra-tmpl"))
		})

		It("should exclude a pod whose claim is for another vendor's devices", func() {
			c := drainClientBuilder().WithObjects(
				withClaim(drainTestPod("other-vendor", "default", "node-1"), "claim-other"),
				gpuClaim("claim-other", "default", "other.example.com"),
			).Build()

			pods, err := gpuPodsOnNode(ctx, c, "node-1")
			Expect(err).NotTo(HaveOccurred())
			Expect(pods).To(BeEmpty())
		})

		It("should fail when a referenced claim cannot be read", func() {
			// Guessing "not a GPU pod" here would drop a GPU pod from the eviction list and let
			// a firmware update start underneath a running workload.
			c := drainClientBuilder().WithObjects(
				withClaim(drainTestPod("dangling", "default", "node-1"), "missing-claim"),
			).Build()

			_, err := gpuPodsOnNode(ctx, c, "node-1")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("dangling"))
		})
	})

	Context("classifyPodForDrain", func() {
		const operatorNS = "operator-ns"

		podIn := func(namespace string) *core.Pod {
			return drainTestPod("p", namespace, "node-1")
		}

		It("should evict a pod in a namespace nobody asked to skip", func() {
			action, reason := classifyPodForDrain(podIn("team-a"), operatorNS, []string{"kube-system"})
			Expect(action).To(Equal(drainEvict))
			Expect(reason).To(Equal("evictable"))
		})

		It("should ignore a pod in a listed namespace", func() {
			action, reason := classifyPodForDrain(podIn("kube-system"), operatorNS, []string{"cert-manager", "kube-system"})
			Expect(action).To(Equal(drainIgnore))
			Expect(reason).To(ContainSubstring("namespacesToSkip"),
				"the log line is how an admin finds out why a pod was left behind")
		})

		// The operator namespace skip is not a default an admin can replace by supplying a list of
		// their own: evicting there aborts the reconcile driving the drain and kills any Job the
		// operator is running on the node.
		It("should ignore the operator namespace even when a skip list is set", func() {
			action, reason := classifyPodForDrain(podIn(operatorNS), operatorNS, []string{"kube-system"})
			Expect(action).To(Equal(drainIgnore))
			Expect(reason).To(Equal("operator namespace"))
		})

		It("should ignore a DaemonSet pod", func() {
			// It gets an automatic NoSchedule toleration, so evicting it brings it straight back
			// and the drain would never converge.
			pod := podIn("team-a")
			pod.OwnerReferences = []metav1.OwnerReference{{Kind: "DaemonSet", Name: "ds"}}

			action, reason := classifyPodForDrain(pod, operatorNS, nil)
			Expect(action).To(Equal(drainIgnore))
			Expect(reason).To(Equal("DaemonSet pod"))
		})

		It("should still evict a pod owned by something other than a DaemonSet", func() {
			pod := podIn("team-a")
			pod.OwnerReferences = []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "rs"}}

			action, _ := classifyPodForDrain(pod, operatorNS, nil)
			Expect(action).To(Equal(drainEvict))
		})

		It("should ignore a static pod's mirror", func() {
			// Evicting the mirror stops nothing: the kubelet owns the manifest and recreates it.
			pod := podIn("kube-system")
			pod.Annotations = map[string]string{mirrorPodAnnotation: "abc"}

			action, reason := classifyPodForDrain(pod, operatorNS, nil)
			Expect(action).To(Equal(drainIgnore))
			Expect(reason).To(Equal("static pod"))
		})

		DescribeTable("should ignore a pod that has already finished",
			func(phase core.PodPhase) {
				pod := podIn("team-a")
				pod.Status.Phase = phase

				action, reason := classifyPodForDrain(pod, operatorNS, nil)
				Expect(action).To(Equal(drainIgnore))
				Expect(reason).To(ContainSubstring("already"))
			},
			Entry("Succeeded", core.PodSucceeded),
			Entry("Failed", core.PodFailed),
		)

		It("should await a terminating pod rather than evict it again", func() {
			pod := podIn("team-a")
			now := metav1.NewTime(time.Now())
			pod.DeletionTimestamp = &now

			action, reason := classifyPodForDrain(pod, operatorNS, nil)
			Expect(action).To(Equal(drainAwait))
			Expect(reason).To(Equal("already terminating"))
		})

		// Order matters: a terminating DaemonSet pod is recreated by design, so awaiting it would
		// block the drain forever. The permanent-exclusion rules have to be checked first.
		It("should ignore a terminating DaemonSet pod instead of awaiting it", func() {
			pod := podIn("team-a")
			now := metav1.NewTime(time.Now())
			pod.DeletionTimestamp = &now
			pod.OwnerReferences = []metav1.OwnerReference{{Kind: "DaemonSet", Name: "ds"}}

			action, _ := classifyPodForDrain(pod, operatorNS, nil)
			Expect(action).To(Equal(drainIgnore))
		})
	})

	Context("drainNeverEvicts", func() {
		const operatorNS = "operator-ns"

		// The deliberate difference from classifyPodForDrain: a Succeeded or terminating pod is
		// not evicted, but it does clear by itself, so a DRA claim held by one is worth waiting
		// for. Folding the two predicates together would make such a wait give up early.
		It("should not report a finished pod as a permanent occupant", func() {
			pod := drainTestPod("p", "team-a", "node-1")
			pod.Status.Phase = core.PodSucceeded

			_, never := drainNeverEvicts(pod, operatorNS, nil)
			Expect(never).To(BeFalse())
		})

		It("should not report a terminating pod as a permanent occupant", func() {
			pod := drainTestPod("p", "team-a", "node-1")
			now := metav1.NewTime(time.Now())
			pod.DeletionTimestamp = &now

			_, never := drainNeverEvicts(pod, operatorNS, nil)
			Expect(never).To(BeFalse())
		})

		It("should report a DaemonSet pod as a permanent occupant", func() {
			pod := drainTestPod("p", "team-a", "node-1")
			pod.OwnerReferences = []metav1.OwnerReference{{Kind: "DaemonSet", Name: "ds"}}

			_, never := drainNeverEvicts(pod, operatorNS, nil)
			Expect(never).To(BeTrue())
		})
	})

	Context("podsBlockingDrain", func() {
		const operatorNS = "operator-ns"

		It("should split the node's pods into evict, await and ignore", func() {
			terminating := drainTestPod("terminating", "team-a", "node-1")
			terminating.Finalizers = []string{"test.example.com/hold"}

			daemon := drainTestPod("daemon", "team-a", "node-1")
			daemon.OwnerReferences = []metav1.OwnerReference{{Kind: "DaemonSet", Name: "ds"}}

			c := drainClientBuilder().WithObjects(
				drainTestPod("workload", "team-a", "node-1"),
				drainTestPod("operator", operatorNS, "node-1"),
				drainTestPod("skipped", "kube-system", "node-1"),
				drainTestPod("elsewhere", "team-a", "node-2"),
				daemon,
				terminating,
			).Build()

			// The finalizer keeps the pod around with a deletionTimestamp, which is what a pod
			// mid-eviction looks like to a later pass.
			Expect(c.Delete(ctx, terminating)).To(Succeed())

			toEvict, toAwait, err := podsBlockingDrain(ctx, c, "node-1", operatorNS, []string{"kube-system"})
			Expect(err).NotTo(HaveOccurred())

			evictNames := []string{}
			for _, pod := range toEvict {
				evictNames = append(evictNames, pod.Name)
			}

			awaitNames := []string{}
			for _, pod := range toAwait {
				awaitNames = append(awaitNames, pod.Name)
			}

			Expect(evictNames).To(ConsistOf("workload"))
			Expect(awaitNames).To(ConsistOf("terminating"))
		})

		It("should report both lists empty for a node that is already drained", func() {
			// Both empty is the drained condition, so an all-ignored node must not read as busy.
			daemon := drainTestPod("daemon", "team-a", "node-1")
			daemon.OwnerReferences = []metav1.OwnerReference{{Kind: "DaemonSet", Name: "ds"}}

			c := drainClientBuilder().WithObjects(
				daemon,
				drainTestPod("operator", operatorNS, "node-1"),
			).Build()

			toEvict, toAwait, err := podsBlockingDrain(ctx, c, "node-1", operatorNS, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(toEvict).To(BeEmpty())
			Expect(toAwait).To(BeEmpty())
		})
	})

	Context("evictPods", func() {
		It("should evict every pod given to it", func() {
			c := drainClientBuilder().WithObjects(
				drainTestPod("a", "team-a", "node-1"),
				drainTestPod("b", "team-b", "node-1"),
			).Build()

			pods, err := podsOnNode(ctx, c, "node-1")
			Expect(err).NotTo(HaveOccurred())
			Expect(evictPods(ctx, c, pods)).To(Succeed())

			remaining := &core.PodList{}
			Expect(c.List(ctx, remaining)).To(Succeed())
			Expect(remaining.Items).To(BeEmpty())
		})

		It("should not evict a pod that is already terminating", func() {
			// A drain that is merely slow must cost nothing: the pod is on its way out and
			// re-evicting it would be a pointless API call on every poll.
			pod := drainTestPod("going", "team-a", "node-1")
			pod.Finalizers = []string{"test.example.com/hold"}

			c := drainClientBuilder().WithObjects(pod).
				WithInterceptorFuncs(interceptor.Funcs{
					SubResourceCreate: func(_ context.Context, _ client.Client, _ string,
						_ client.Object, _ client.Object, _ ...client.SubResourceCreateOption) error {
						Fail("evictPods must skip a pod that already has a deletionTimestamp")

						return nil
					},
				}).Build()

			Expect(c.Delete(ctx, pod)).To(Succeed())

			pods, err := podsOnNode(ctx, c, "node-1")
			Expect(err).NotTo(HaveOccurred())
			Expect(pods).To(HaveLen(1))
			Expect(evictPods(ctx, c, pods)).To(Succeed())
		})

		// The fake client does not implement PodDisruptionBudgets, so the 429 the API server
		// would send is injected. This is the case that made the polling retry necessary: the
		// first attempt is refused and a later one succeeds.
		It("should treat a PodDisruptionBudget refusal as 'not yet', not a failure", func() {
			c := drainClientBuilder().
				WithObjects(drainTestPod("protected", "team-a", "node-1")).
				WithInterceptorFuncs(interceptor.Funcs{
					SubResourceCreate: func(_ context.Context, _ client.Client, _ string,
						_ client.Object, _ client.Object, _ ...client.SubResourceCreateOption) error {
						return apierrors.NewTooManyRequests("cannot evict, disruption budget is exhausted", 10)
					},
				}).Build()

			pods, err := podsOnNode(ctx, c, "node-1")
			Expect(err).NotTo(HaveOccurred())

			Expect(evictPods(ctx, c, pods)).To(Succeed(),
				"a refusal from a PDB is retried on the next pass, so failing the drain here "+
					"would abort an update that was going to succeed")

			Expect(c.Get(ctx, types.NamespacedName{Name: "protected", Namespace: "team-a"}, &core.Pod{})).
				To(Succeed(), "the pod is still there, so the caller keeps the node in draining")
		})

		It("should not fail when the pod disappeared before it could be evicted", func() {
			// Lost race between the List and the eviction. The pod being gone is what we wanted.
			c := drainClientBuilder().Build()

			Expect(evictPods(ctx, c, []*core.Pod{drainTestPod("ghost", "team-a", "node-1")})).To(Succeed())
		})

		It("should return a real failure so a drain that can never work surfaces", func() {
			// Missing RBAC on pods/eviction looks like this. Swallowing it would leave the CR
			// sitting in draining forever with nothing to point at.
			c := drainClientBuilder().
				WithObjects(drainTestPod("victim", "team-a", "node-1")).
				WithInterceptorFuncs(interceptor.Funcs{
					SubResourceCreate: func(_ context.Context, _ client.Client, _ string,
						_ client.Object, _ client.Object, _ ...client.SubResourceCreateOption) error {
						return apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "victim",
							errors.New("no permission"))
					},
				}).Build()

			pods, err := podsOnNode(ctx, c, "node-1")
			Expect(err).NotTo(HaveOccurred())

			err = evictPods(ctx, c, pods)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("team-a/victim"))
		})

		It("should try every pod even when one of them fails", func() {
			// One broken pod must not shield the rest: the node has to end up clear.
			attempted := []string{}

			c := drainClientBuilder().WithObjects(
				drainTestPod("bad", "team-a", "node-1"),
				drainTestPod("good", "team-a", "node-1"),
			).WithInterceptorFuncs(interceptor.Funcs{
				SubResourceCreate: func(_ context.Context, _ client.Client, _ string,
					obj client.Object, _ client.Object, _ ...client.SubResourceCreateOption) error {
					attempted = append(attempted, obj.GetName())

					if obj.GetName() == "bad" {
						return apierrors.NewInternalError(errors.New("boom"))
					}

					return nil
				},
			}).Build()

			pods, err := podsOnNode(ctx, c, "node-1")
			Expect(err).NotTo(HaveOccurred())

			Expect(evictPods(ctx, c, pods)).To(HaveOccurred())
			Expect(attempted).To(ConsistOf("bad", "good"))
		})
	})

	Context("evictPod", func() {
		It("should evict through the eviction subresource", func() {
			pod := drainTestPod("victim", "team-a", "node-1")

			c := drainClientBuilder().WithObjects(pod).
				WithInterceptorFuncs(interceptor.Funcs{
					Delete: func(_ context.Context, _ client.WithWatch, _ client.Object, _ ...client.DeleteOption) error {
						Fail("evictPod must go through pods/eviction, not Delete, or PodDisruptionBudgets are bypassed")

						return nil
					},
				}).Build()

			Expect(evictPod(ctx, c, pod)).To(Succeed())

			err := c.Get(ctx, types.NamespacedName{Name: "victim", Namespace: "team-a"}, &core.Pod{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		})

		It("should wrap the failure with the pod's identity", func() {
			// The error travels into CR status, where "which pod" is the only useful part.
			c := drainClientBuilder().Build()

			err := evictPod(ctx, c, drainTestPod("victim", "team-a", "node-1"))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("team-a/victim"))
		})
	})
})
