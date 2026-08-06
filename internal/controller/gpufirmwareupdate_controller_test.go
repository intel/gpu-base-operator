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
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"

	batch "k8s.io/api/batch/v1"
	core "k8s.io/api/core/v1"
	resv1 "k8s.io/api/resource/v1"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	intelcomv1alpha1 "github.com/intel/gpu-base-operator/api/v1alpha1"
)

var (
	resourceName = "fwupdate-happypath"
)

func baseCr() *intelcomv1alpha1.GPUFirmwareUpdate {
	return &intelcomv1alpha1.GPUFirmwareUpdate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      resourceName,
			Namespace: "default",
		},
		Spec: intelcomv1alpha1.GPUFirmwareUpdateSpec{
			NodeSelector: map[string]string{
				"gpu-update": "true",
			},
			UpdaterImage:    "intel/xpumanager:v1.3.4",
			ImagePullSecret: "my-registry-secret",
			UpdateMethod:    "canary",
			UpdateTaint:     "gpu-update-in-progress",
			Tolerations: []core.Toleration{
				{
					Key:    "some-other-toleration",
					Effect: core.TaintEffectNoSchedule,
					Value:  "foobar",
				},
			},
			Content: intelcomv1alpha1.GPUFirmwareContent{
				ContainerImage: "firmware-update:1.0.0",
				Files: []intelcomv1alpha1.GPUFirmwareFile{
					{Type: "GFX", FileName: "gfx_firmware.bin"},
				},
			},
		},
		Status: intelcomv1alpha1.GPUFirmwareUpdateStatus{
			State: "",
		},
	}
}

func twoNodes() []*core.Node {
	return []*core.Node{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node-1",
				Labels: map[string]string{
					"gpu-update": "true",
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node-2",
				Labels: map[string]string{
					"gpu-update": "true",
				},
			},
		},
	}
}

func pod1() *core.Pod {
	count, _ := resource.ParseQuantity("1")

	return &core.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod-1",
			Namespace: "default",
		},
		Spec: core.PodSpec{
			NodeName: "node-1",
			Containers: []core.Container{
				{
					Name: "container-1",
					Resources: core.ResourceRequirements{
						Limits: core.ResourceList{
							xeResource: count,
						},
					},
				},
			},
		},
	}
}

func pod2() *core.Pod {
	return &core.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod-2",
			Namespace: "default",
		},
		Spec: core.PodSpec{
			NodeName:   "node-2",
			Containers: []core.Container{{Name: "container-2"}},
		},
	}
}

func pod3() *core.Pod {
	claimTemplate := "resource-claim-template-gpu"
	return &core.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod-3",
			Namespace: "default",
		},
		Spec: core.PodSpec{
			NodeName: "node-2",
			Containers: []core.Container{
				{
					Name: "container-3",
				},
			},
			ResourceClaims: []core.PodResourceClaim{
				{
					Name:                      "gpu-resourceclaim",
					ResourceClaimTemplateName: &claimTemplate,
				},
			},
		},
	}
}

func pod4() *core.Pod {
	claim := "resource-claim-gpu"
	return &core.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod-4",
			Namespace: "default",
		},
		Spec: core.PodSpec{
			NodeName: "node-2",
			Containers: []core.Container{
				{
					Name: "container-4",
				},
			},
			ResourceClaims: []core.PodResourceClaim{
				{
					Name:              "gpu-resourceclaim",
					ResourceClaimName: &claim,
				},
			},
		},
	}
}

func claimTemplateForPod3() *resv1.ResourceClaimTemplate {
	return &resv1.ResourceClaimTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "resource-claim-template-gpu",
			Namespace: "default",
		},
		Spec: resv1.ResourceClaimTemplateSpec{
			Spec: resv1.ResourceClaimSpec{
				Devices: resv1.DeviceClaim{
					Requests: []resv1.DeviceRequest{
						{
							Name: "intel-gpu",
							Exactly: &resv1.ExactDeviceRequest{
								DeviceClassName: "gpu.intel.com",
							},
						},
					},
				},
			},
		},
	}
}

func claimForPod4() *resv1.ResourceClaim {
	return &resv1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "resource-claim-gpu",
			Namespace: "default",
		},
		Spec: resv1.ResourceClaimSpec{
			Devices: resv1.DeviceClaim{
				Requests: []resv1.DeviceRequest{
					{
						Name: "intel-gpu",
						Exactly: &resv1.ExactDeviceRequest{
							DeviceClassName: "gpu.intel.com",
						},
					},
				},
			},
		},
	}
}

func createPodsForJobs(fakeClient client.Client, jobs *batch.JobList) {
	for i := range jobs.Items {
		// dummy Pod for the Job
		pod := core.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("fw-update-pod-%s", jobs.Items[i].Name),
				Namespace: "default",
				Labels: map[string]string{
					"node":              fmt.Sprintf("node-%d", i+1),
					"gpufirmwareupdate": resourceName,
					"job-name":          jobs.Items[i].Name,
				},
			},
			Spec: core.PodSpec{
				Containers: []core.Container{
					{
						Name:  "foo",
						Image: "fw-updater:latest",
					},
				},
			},
		}

		// This will always succeed in the test context
		_ = fakeClient.Create(ctx, &pod)
	}
}

type MockLogRetriever struct{}

func (mlr *MockLogRetriever) RetrieveLogsForPodContainer(ctx context.Context, pod *core.Pod, containerName string) ([]string, error) {
	return []string{
		"Logs logs",
		"##COMPLETED##",
	}, nil
}

// fakeContentImageVerifier is a test double for ContentImageVerifier.
type fakeContentImageVerifier struct {
	err error
}

func (f *fakeContentImageVerifier) Verify(_ context.Context, _ *intelcomv1alpha1.GPUFirmwareUpdateSpec) error {
	return f.err
}

var _ = Describe("GPUFirmwareUpdate Controller", func() {
	Context("When running happy path for firmware update", func() {
		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default",
		}

		resource := baseCr()
		nodes := twoNodes()
		objs := []client.Object{pod1(), pod2(), pod3(), pod4(), claimTemplateForPod3(), claimForPod4()}

		var fakeClient client.Client
		var controllerReconciler *GPUFirmwareUpdateReconciler

		BeforeEach(func() {
			By("Creating fake client with test objects")

			scheme := runtime.NewScheme()
			Expect(intelcomv1alpha1.AddToScheme(scheme)).To(Succeed())
			Expect(core.AddToScheme(scheme)).To(Succeed())
			Expect(batch.AddToScheme(scheme)).To(Succeed())
			Expect(resv1.AddToScheme(scheme)).To(Succeed())

			// Create a fake client with a mock Update function
			fakeClient = fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(objs...).
				WithObjects(nodes[0], nodes[1]).
				WithIndex(&core.Pod{}, "spec.nodeName", podIndexerFunc).
				WithStatusSubresource(&intelcomv1alpha1.GPUFirmwareUpdate{}).
				Build()

			controllerReconciler = &GPUFirmwareUpdateReconciler{
				Client:    fakeClient,
				Scheme:    scheme,
				logRet:    &MockLogRetriever{},
				imgVerify: &fakeContentImageVerifier{},
				Opts: ControllerOpts{
					Namespace: "default",
				},
			}
		})

		AfterEach(func() {
			podList := &core.PodList{}
			matcher := client.MatchingLabels{"gpufirmwareupdate": resourceName}

			Expect(fakeClient.List(ctx, podList, client.InNamespace("default"), matcher)).To(Succeed())
			for _, pod := range podList.Items {
				Expect(fakeClient.Delete(ctx, &pod)).To(Succeed())
			}

			res := &intelcomv1alpha1.GPUFirmwareUpdate{}
			err := fakeClient.Get(ctx, typeNamespacedName, res)
			if err == nil {
				Expect(fakeClient.Delete(ctx, res)).To(Succeed())
			} else if !apierrors.IsNotFound(err) {
				Fail("unable to get GPUFirmwareUpdate for cleanup")
			}
		})

		reconcileAndGet := func(fakeClient client.Client, controllerReconciler *GPUFirmwareUpdateReconciler, resource *intelcomv1alpha1.GPUFirmwareUpdate) (reconcile.Result, error) {
			ret, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(fakeClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())
			return ret, err
		}

		It("it should complete canary flow: node-1 canary then auto-promote to node-2", func() {
			resource = baseCr()
			// HoldAfterCanary is false by default — promotion is automatic.

			Expect(fakeClient.Create(ctx, resource)).To(Succeed())

			// ── PHASE 1: CANARY (node-1) ────────────────────────────────────

			By("Initial reconcile selects only node-1 as canary, stores node-2 as pending")
			ret, err := reconcileAndGet(fakeClient, controllerReconciler, resource)
			Expect(err).NotTo(HaveOccurred())
			Expect(ret.RequeueAfter).To(BeNumerically(">", 0))

			Expect(resource.Status.State).To(Equal(stateDraining))
			Expect(resource.Status.NodeInfos.All).To(ConsistOf("node-1"))
			Expect(resource.Status.NodeInfos.Pending).To(ConsistOf("node-2"))
			Expect(resource.Status.NodeInfos.Draining).To(ConsistOf("node-1"))
			Expect(resource.Finalizers).To(ContainElement(gpuFwUpdateFinalizer))

			node := &core.Node{}
			Expect(fakeClient.Get(ctx, types.NamespacedName{Name: "node-1"}, node)).To(Succeed())
			Expect(node.Spec.Taints).To(HaveLen(1))
			Expect(node.Spec.Taints[0].Key).To(Equal("gpu-update-in-progress"))

			Expect(fakeClient.Get(ctx, types.NamespacedName{Name: "node-2"}, node)).To(Succeed())
			Expect(node.Spec.Taints).To(BeEmpty(), "node-2 must not be tainted during canary phase")

			Expect(apierrors.IsNotFound(fakeClient.Get(ctx, types.NamespacedName{Name: "pod-1", Namespace: "default"}, &core.Pod{}))).
				To(BeTrue(), "GPU pod on canary node should be evicted")
			Expect(fakeClient.Get(ctx, types.NamespacedName{Name: "pod-2", Namespace: "default"}, &core.Pod{})).
				To(Succeed(), "non-GPU pod should not be evicted")

			By("Draining reconcile: pod-1 gone, transition to updating")
			ret, err = reconcileAndGet(fakeClient, controllerReconciler, resource)
			Expect(err).NotTo(HaveOccurred())
			Expect(resource.Status.State).To(Equal(stateUpdating))

			By("Updating reconcile: create one Job for node-1 only")
			ret, err = reconcileAndGet(fakeClient, controllerReconciler, resource)
			Expect(err).NotTo(HaveOccurred())
			Expect(resource.Status.NodeInfos.Updating).To(ConsistOf("node-1"))

			jobs := &batch.JobList{}
			Expect(fakeClient.List(ctx, jobs)).To(Succeed())
			Expect(jobs.Items).To(HaveLen(1))
			Expect(jobs.Items[0].Spec.Template.Spec.NodeSelector).To(HaveKeyWithValue("kubernetes.io/hostname", "node-1"))
			Expect(jobs.Items[0].Spec.Template.Spec.Tolerations).To(HaveLen(2))
			Expect(jobs.Items[0].Spec.Template.Spec.Tolerations).To(ContainElement(core.Toleration{
				Key:    "gpu-update-in-progress",
				Effect: core.TaintEffectNoSchedule,
			}))
			Expect(jobs.Items[0].Spec.Template.Spec.Tolerations).To(ContainElement(core.Toleration{
				Key:    "some-other-toleration",
				Value:  "foobar",
				Effect: core.TaintEffectNoSchedule,
			}))
			Expect(jobs.Items[0].Spec.Template.Spec.Containers[0].Image).To(Equal("intel/xpumanager:v1.3.4"))
			Expect(jobs.Items[0].Spec.Template.Spec.ImagePullSecrets).To(Equal([]core.LocalObjectReference{
				{Name: "my-registry-secret"},
			}))

			createPodsForJobs(fakeClient, jobs)

			By("Updating reconcile in progress: job still running")
			ret, err = reconcileAndGet(fakeClient, controllerReconciler, resource)
			Expect(err).NotTo(HaveOccurred())
			Expect(resource.Status.State).To(Equal(stateUpdating))
			Expect(resource.Status.NodeInfos.Updating).To(ConsistOf("node-1"))

			By("Mark canary job succeeded → expect canary_done with pending=[node-2]")
			jobs.Items[0].Status.Succeeded = 1
			Expect(fakeClient.Status().Update(ctx, &jobs.Items[0])).To(Succeed())

			ret, err = reconcileAndGet(fakeClient, controllerReconciler, resource)
			Expect(err).NotTo(HaveOccurred())
			Expect(ret.RequeueAfter).To(BeNumerically(">", 0))
			Expect(resource.Status.State).To(Equal(stateCanaryDone))
			Expect(resource.Status.NodeInfos.Completed).To(ConsistOf("node-1"))
			Expect(resource.Status.NodeInfos.Pending).To(ConsistOf("node-2"))

			// ── PHASE 2: FULL ROLLOUT (node-2) ──────────────────────────────

			By("canary_done reconcile: auto-promote, taint node-2, evict its GPU pods")
			ret, err = reconcileAndGet(fakeClient, controllerReconciler, resource)
			Expect(err).NotTo(HaveOccurred())
			Expect(resource.Status.State).To(Equal(stateDraining))
			Expect(resource.Status.NodeInfos.All).To(ConsistOf("node-1", "node-2"))
			Expect(resource.Status.NodeInfos.Pending).To(BeEmpty())
			Expect(resource.Status.NodeInfos.Draining).To(ConsistOf("node-2"))

			Expect(fakeClient.Get(ctx, types.NamespacedName{Name: "node-2"}, node)).To(Succeed())
			Expect(node.Spec.Taints).To(HaveLen(1))
			Expect(node.Spec.Taints[0].Key).To(Equal("gpu-update-in-progress"))

			Expect(apierrors.IsNotFound(fakeClient.Get(ctx, types.NamespacedName{Name: "pod-3", Namespace: "default"}, &core.Pod{}))).
				To(BeTrue(), "DRA pod-3 on node-2 should be evicted")
			Expect(apierrors.IsNotFound(fakeClient.Get(ctx, types.NamespacedName{Name: "pod-4", Namespace: "default"}, &core.Pod{}))).
				To(BeTrue(), "DRA pod-4 on node-2 should be evicted")

			By("Draining reconcile: node-2 pods gone, transition to updating")
			ret, err = reconcileAndGet(fakeClient, controllerReconciler, resource)
			Expect(err).NotTo(HaveOccurred())
			Expect(resource.Status.State).To(Equal(stateUpdating))

			By("Updating reconcile: create one new Job for node-2 only (node-1 already completed)")
			ret, err = reconcileAndGet(fakeClient, controllerReconciler, resource)
			Expect(err).NotTo(HaveOccurred())
			Expect(resource.Status.NodeInfos.Updating).To(ConsistOf("node-2"))

			Expect(fakeClient.List(ctx, jobs)).To(Succeed())
			Expect(jobs.Items).To(HaveLen(2), "total Jobs across both phases")
			var node2Job *batch.Job
			for i := range jobs.Items {
				if jobs.Items[i].Labels["node"] == "node-2" {
					node2Job = &jobs.Items[i]
				}
			}
			Expect(node2Job).NotTo(BeNil())
			Expect(node2Job.Spec.Template.Spec.NodeSelector).To(HaveKeyWithValue("kubernetes.io/hostname", "node-2"))

			// Create a pod so log retrieval can work for node-2's job.
			phase2Pods := &batch.JobList{}
			Expect(fakeClient.List(ctx, phase2Pods)).To(Succeed())
			node2JobList := &batch.JobList{Items: []batch.Job{*node2Job}}
			createPodsForJobs(fakeClient, node2JobList)

			By("Updating reconcile in progress: only node-2 job still running")
			ret, err = reconcileAndGet(fakeClient, controllerReconciler, resource)
			Expect(err).NotTo(HaveOccurred())
			Expect(resource.Status.State).To(Equal(stateUpdating))
			Expect(resource.Status.NodeInfos.Updating).To(ConsistOf("node-2"))

			By("Mark node-2 job succeeded → both nodes completed, expect cleanup")
			node2Job.Status.Succeeded = 1
			Expect(fakeClient.Status().Update(ctx, node2Job)).To(Succeed())

			ret, err = reconcileAndGet(fakeClient, controllerReconciler, resource)
			Expect(err).NotTo(HaveOccurred())
			Expect(ret.RequeueAfter).To(BeNumerically(">", 0))
			Expect(resource.Status.State).To(Equal(stateCleanup))
			Expect(resource.Status.NodeInfos.Completed).To(ConsistOf("node-1", "node-2"))

			// ── CLEANUP ──────────────────────────────────────────────────────

			By("Cleanup reconcile: both nodes untainted, state=completed")
			ret, err = reconcileAndGet(fakeClient, controllerReconciler, resource)
			Expect(err).NotTo(HaveOccurred())
			Expect(resource.Status.State).To(Equal(stateComplete))

			for _, nodeName := range []string{"node-1", "node-2"} {
				Expect(fakeClient.Get(ctx, types.NamespacedName{Name: nodeName}, node)).To(Succeed())
				Expect(node.Spec.Taints).To(BeEmpty(), "node %s taint should be removed", nodeName)
			}

			By("Final reconcile: finalizer still held until CR is deleted")
			ret, err = reconcileAndGet(fakeClient, controllerReconciler, resource)
			Expect(err).NotTo(HaveOccurred())
			Expect(resource.Status.State).To(Equal(stateComplete))
			Expect(resource.Finalizers).To(HaveLen(1))

			Expect(fakeClient.Delete(ctx, resource)).To(Succeed())

			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			Expect(apierrors.IsNotFound(fakeClient.Get(ctx, typeNamespacedName, resource))).To(BeTrue())

			Expect(fakeClient.List(ctx, jobs)).To(Succeed())
			Expect(jobs.Items).To(BeEmpty())
		})

		It("it should complete canary flow with holdAfterCanary gate", func() {
			resource = baseCr()
			resource.Spec.HoldAfterCanary = true

			Expect(fakeClient.Create(ctx, resource)).To(Succeed())

			By("Run canary phase through to canary_done")
			// Initial reconcile: select node-1, store node-2 as pending.
			_, err := reconcileAndGet(fakeClient, controllerReconciler, resource)
			Expect(err).NotTo(HaveOccurred())
			Expect(resource.Status.State).To(Equal(stateDraining))

			// Drain.
			_, err = reconcileAndGet(fakeClient, controllerReconciler, resource)
			Expect(err).NotTo(HaveOccurred())
			Expect(resource.Status.State).To(Equal(stateUpdating))

			// Create job.
			_, err = reconcileAndGet(fakeClient, controllerReconciler, resource)
			Expect(err).NotTo(HaveOccurred())
			jobs := &batch.JobList{}
			Expect(fakeClient.List(ctx, jobs)).To(Succeed())
			createPodsForJobs(fakeClient, jobs)

			// Mark job succeeded.
			jobs.Items[0].Status.Succeeded = 1
			Expect(fakeClient.Status().Update(ctx, &jobs.Items[0])).To(Succeed())

			_, err = reconcileAndGet(fakeClient, controllerReconciler, resource)
			Expect(err).NotTo(HaveOccurred())
			Expect(resource.Status.State).To(Equal(stateCanaryDone))

			By("canary_done reconcile without approval: controller stays in canary_done")
			ret, err := reconcileAndGet(fakeClient, controllerReconciler, resource)
			Expect(err).NotTo(HaveOccurred())
			Expect(ret.RequeueAfter).To(BeNumerically(">", 0))
			Expect(resource.Status.State).To(Equal(stateCanaryDone))

			By("Set holdAfterCanary=false and reconcile: auto-promotes to full rollout")
			resource.Spec.HoldAfterCanary = false
			Expect(fakeClient.Update(ctx, resource)).To(Succeed())

			_, err = reconcileAndGet(fakeClient, controllerReconciler, resource)
			Expect(err).NotTo(HaveOccurred())
			Expect(resource.Status.State).To(Equal(stateDraining))
			Expect(resource.Status.NodeInfos.All).To(ConsistOf("node-1", "node-2"))
			Expect(resource.Status.NodeInfos.Pending).To(BeEmpty())

			node := &core.Node{}
			Expect(fakeClient.Get(ctx, types.NamespacedName{Name: "node-2"}, node)).To(Succeed())
			Expect(node.Spec.Taints).To(HaveLen(1))
		})

		It("it should complete AMC update flow", func() {
			resource = baseCr()
			resource.Spec.UpdateMethod = "direct"
			resource.Spec.Content.Files = []intelcomv1alpha1.GPUFirmwareFile{
				{Type: "AMC", FileName: "amc_firmware.bin"},
			}

			Expect(fakeClient.Create(ctx, resource)).To(Succeed())

			By("Initial reconcile: both nodes tainted and drained for AMC update")
			ret, err := reconcileAndGet(fakeClient, controllerReconciler, resource)
			Expect(err).NotTo(HaveOccurred())
			Expect(ret.RequeueAfter).To(BeNumerically(">", 0))
			Expect(resource.Status.State).To(Equal(stateDraining))
			Expect(resource.Status.NodeInfos.All).To(HaveLen(2))
			Expect(resource.Status.NodeInfos.Pending).To(BeEmpty())

			By("Draining reconcile: all pods evicted, move to updating")
			ret, err = reconcileAndGet(fakeClient, controllerReconciler, resource)
			Expect(err).NotTo(HaveOccurred())
			Expect(resource.Status.State).To(Equal(stateUpdating))

			By("Updating reconcile: create Jobs")
			ret, err = reconcileAndGet(fakeClient, controllerReconciler, resource)
			Expect(err).NotTo(HaveOccurred())
			Expect(resource.Status.State).To(Equal(stateUpdating))
			Expect(resource.Status.NodeInfos.Updating).To(HaveLen(2))

			jobs := &batch.JobList{}
			Expect(fakeClient.List(ctx, jobs)).To(Succeed())
			Expect(jobs.Items).To(HaveLen(2))

			createPodsForJobs(fakeClient, jobs)

			By("Mark all jobs succeeded → expect cleanup")
			for i := range jobs.Items {
				jobs.Items[i].Status.Succeeded = 1
				Expect(fakeClient.Status().Update(ctx, &jobs.Items[i])).To(Succeed())
			}

			ret, err = reconcileAndGet(fakeClient, controllerReconciler, resource)
			Expect(err).NotTo(HaveOccurred())
			Expect(resource.Status.State).To(Equal(stateCleanup))
			Expect(resource.Status.NodeInfos.Completed).To(HaveLen(2))

			By("Cleanup reconcile: both nodes untainted, state=completed")
			ret, err = reconcileAndGet(fakeClient, controllerReconciler, resource)
			Expect(err).NotTo(HaveOccurred())
			Expect(resource.Status.State).To(Equal(stateComplete))

			for _, nodeName := range []string{"node-1", "node-2"} {
				node := &core.Node{}
				Expect(fakeClient.Get(ctx, types.NamespacedName{Name: nodeName}, node)).To(Succeed())
				Expect(node.Spec.Taints).To(BeEmpty(), "node %s should be untainted after AMC update", nodeName)
			}
		})

		It("it should complete full flow with two nodes updating", func() {
			resource = baseCr()
			resource.Spec.UpdateMethod = "direct"

			Expect(fakeClient.Create(ctx, resource)).To(Succeed())

			// INITIAL RECONCILE

			ret, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})

			Expect(err).NotTo(HaveOccurred())
			Expect(ret.RequeueAfter).To(BeNumerically(">", 0))

			err = fakeClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			Expect(resource.Status.State).To(Equal(stateDraining))
			Expect(resource.Status.NodeInfos.All).To(HaveLen(2))
			Expect(resource.Status.NodeInfos.Pending).To(BeEmpty(), "direct mode has no pending nodes")
			Expect(resource.Status.NodeInfos.Draining).To(HaveLen(2))

			nodeNames := []string{"node-1", "node-2"}
			for _, nodeName := range nodeNames {
				node := &core.Node{}
				err = fakeClient.Get(ctx, types.NamespacedName{Name: nodeName}, node)
				Expect(err).NotTo(HaveOccurred())
				Expect(node.Spec.Taints).To(HaveLen(1))
				Expect(node.Spec.Taints[0].Key).To(Equal("gpu-update-in-progress"))
			}

			err = fakeClient.Get(ctx, types.NamespacedName{Name: "pod-1", Namespace: "default"}, &core.Pod{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "Pod-1 on the draining node should be evicted")

			err = fakeClient.Get(ctx, types.NamespacedName{Name: "pod-3", Namespace: "default"}, &core.Pod{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "Pod-3 on the draining node should be evicted")

			err = fakeClient.Get(ctx, types.NamespacedName{Name: "pod-4", Namespace: "default"}, &core.Pod{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "Pod-4 on the draining node should be evicted")

			// DRAINING IN PROGRESS RECONCILE

			ret, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})

			Expect(err).NotTo(HaveOccurred())
			Expect(ret.RequeueAfter).To(BeNumerically(">", 0))

			err = fakeClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			Expect(resource.Status.State).To(Equal(stateUpdating))

			// DRAINING COMPLETE AND MOVING TO UPDATING

			ret, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})

			Expect(err).NotTo(HaveOccurred())
			Expect(ret.RequeueAfter).To(BeNumerically(">", 0))

			err = fakeClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())
			Expect(resource.Status.State).To(Equal(stateUpdating))
			Expect(resource.Status.NodeInfos.Updating).To(HaveLen(2))
			Expect(resource.Status.NodeInfos.Draining).To(BeEmpty())

			jobs := batch.JobList{}
			err = fakeClient.List(ctx, &jobs)
			Expect(err).NotTo(HaveOccurred())
			Expect(jobs.Items).To(HaveLen(2))

			for index := range jobs.Items {
				job := &jobs.Items[index]

				nodeName := job.Spec.Template.Spec.NodeSelector["kubernetes.io/hostname"]
				Expect(nodeName).To(BeElementOf(nodeNames))
				Expect(job.Name).To(ContainSubstring(nodeName))
				Expect(job.Spec.Template.Spec.Containers).To(HaveLen(1))
				Expect(job.Spec.Template.Spec.Containers[0].Image).To(Equal("intel/xpumanager:v1.3.4"))
				Expect(job.Spec.Template.Spec.ImagePullSecrets).To(Equal([]core.LocalObjectReference{
					{Name: "my-registry-secret"},
				}))
			}

			createPodsForJobs(fakeClient, &jobs)

			// RECONCILE UPDATING IN PROGRESS

			ret, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			err = fakeClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())
			Expect(resource.Status.State).To(Equal(stateUpdating))
			Expect(resource.Status.NodeInfos.Updating).To(HaveLen(2))
			Expect(resource.Status.NodeInfos.Draining).To(BeEmpty())

			// RECONCILE JOB COMPLETION

			for index := range jobs.Items {
				job := &jobs.Items[index]

				job.Status.Succeeded = 1

				err = fakeClient.Status().Update(ctx, job)
				Expect(err).NotTo(HaveOccurred())
			}

			ret, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(ret.RequeueAfter).To(BeNumerically(">", 0))

			err = fakeClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())
			Expect(resource.Status.State).To(Equal(stateCleanup))

			// RECONCILE FINALIZE

			ret, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			err = fakeClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())
			Expect(resource.Status.State).To(Equal(stateComplete))

			for _, nodeName := range nodeNames {
				node := &core.Node{}

				err = fakeClient.Get(ctx, types.NamespacedName{Name: nodeName}, node)
				Expect(err).NotTo(HaveOccurred())
				Expect(node.Spec.Taints).To(BeEmpty())
			}

			// RECONCILE CLEANUP

			ret, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			err = fakeClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())
			Expect(resource.Status.State).To(Equal(stateComplete))
			Expect(resource.Finalizers).To(HaveLen(1))

			err = fakeClient.Delete(ctx, resource)
			Expect(err).NotTo(HaveOccurred())

			ret, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			err = fakeClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).To(HaveOccurred())

			err = fakeClient.List(ctx, &jobs)
			Expect(err).NotTo(HaveOccurred())

			Expect(jobs.Items).To(BeEmpty())
		})

	})
})

var _ = Describe("GPUFirmwareUpdate Controller Errors", func() {
	Context("beginUpdate fails", func() {
		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default",
		}

		resource := baseCr()

		var fakeClient client.Client
		var controllerReconciler *GPUFirmwareUpdateReconciler

		BeforeEach(func() {
			By("Creating fake client with test objects")

			scheme := runtime.NewScheme()
			Expect(intelcomv1alpha1.AddToScheme(scheme)).To(Succeed())
			Expect(core.AddToScheme(scheme)).To(Succeed())
			Expect(batch.AddToScheme(scheme)).To(Succeed())
			Expect(resv1.AddToScheme(scheme)).To(Succeed())

			// Create a fake client with a mock Update function
			fakeClient = fake.NewClientBuilder().
				WithScheme(scheme).
				WithIndex(&core.Pod{}, "spec.nodeName", podIndexerFunc).
				WithStatusSubresource(&intelcomv1alpha1.GPUFirmwareUpdate{}).
				Build()

			controllerReconciler = &GPUFirmwareUpdateReconciler{
				Client:    fakeClient,
				Scheme:    scheme,
				logRet:    &MockLogRetriever{},
				imgVerify: &fakeContentImageVerifier{},
				Opts: ControllerOpts{
					Namespace: "default",
				},
			}
		})

		AfterEach(func() {
			podList := &core.PodList{}
			matcher := client.MatchingLabels{"gpufirmwareupdate": resourceName}

			Expect(fakeClient.List(ctx, podList, client.InNamespace("default"), matcher)).To(Succeed())
			for _, pod := range podList.Items {
				Expect(fakeClient.Delete(ctx, &pod)).To(Succeed())
			}

			res := &intelcomv1alpha1.GPUFirmwareUpdate{}
			err := fakeClient.Get(ctx, typeNamespacedName, res)
			if err == nil {
				Expect(fakeClient.Delete(ctx, res)).To(Succeed())
			} else if !apierrors.IsNotFound(err) {
				Fail("unable to get GPUFirmwareUpdate for cleanup")
			}
		})

		It("should detect missing updater image and set status", func() {
			resource = baseCr()

			resource.Spec.Content.ContainerImage = ""

			Expect(fakeClient.Create(ctx, resource)).To(Succeed())

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})

			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("content container image must be specified"))

			err = fakeClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			Expect(resource.Status.State).To(Equal(stateError))
		})

		It("should detect invalid fw type and set status", func() {
			resource = baseCr()

			resource.Spec.Content.Files = []intelcomv1alpha1.GPUFirmwareFile{
				{Type: "FOOBAR", FileName: "firmware.bin"},
			}

			Expect(fakeClient.Create(ctx, resource)).To(Succeed())

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})

			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("unsupported firmware type"))

			err = fakeClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			Expect(resource.Status.State).To(Equal(stateError))
		})

		It("should bail out properly when no nodes are available", func() {
			resource = baseCr()

			Expect(fakeClient.Create(ctx, resource)).To(Succeed())

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})

			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("no suitable nodes found for update"))

			err = fakeClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			Expect(resource.Status.State).To(Equal(stateError))
		})

		It("should bail out properly when node get fails", func() {
			resource = baseCr()

			oldScheme := fakeClient.Scheme()
			nodes := twoNodes()

			fakeClient = fake.NewClientBuilder().
				WithScheme(oldScheme).
				WithIndex(&core.Pod{}, "spec.nodeName", podIndexerFunc).
				WithObjects(nodes[0], nodes[1]).
				WithStatusSubresource(&intelcomv1alpha1.GPUFirmwareUpdate{}).
				WithInterceptorFuncs(
					interceptor.Funcs{
						Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
							if _, isNode := obj.(*core.Node); isNode {
								return errors.New("oh no, error!")
							}

							return c.Get(ctx, key, obj, opts...)
						},
					},
				).Build()
			controllerReconciler.Client = fakeClient

			Expect(fakeClient.Create(ctx, resource)).To(Succeed())

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})

			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("oh no, error!"))

			err = fakeClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			Expect(resource.Status.State).To(Equal(stateError))
		})
	})
})

var _ = Describe("GPU Firmware Update Job", func() {
	Context("Security context", func() {
		It("job preserves pod spec and container security context from YAML", func() {
			fu := baseCr()
			r := &GPUFirmwareUpdateReconciler{}

			job := r.createUpdateJobObjForNode("node-1", fu)

			By("automountServiceAccountToken is false")
			Expect(job.Spec.Template.Spec.AutomountServiceAccountToken).NotTo(BeNil())
			Expect(*job.Spec.Template.Spec.AutomountServiceAccountToken).To(BeFalse())

			By("fw-copy init container has allowPrivilegeEscalation=false, capabilities.drop=ALL, RuntimeDefault seccomp")
			var fwCopy *core.Container
			for i := range job.Spec.Template.Spec.InitContainers {
				if job.Spec.Template.Spec.InitContainers[i].Name == "fw-copy" {
					fwCopy = &job.Spec.Template.Spec.InitContainers[i]
					break
				}
			}
			Expect(fwCopy).NotTo(BeNil())
			Expect(fwCopy.SecurityContext).NotTo(BeNil())
			Expect(fwCopy.SecurityContext.AllowPrivilegeEscalation).NotTo(BeNil())
			Expect(*fwCopy.SecurityContext.AllowPrivilegeEscalation).To(BeFalse())
			Expect(fwCopy.SecurityContext.Capabilities).NotTo(BeNil())
			Expect(fwCopy.SecurityContext.Capabilities.Drop).To(ContainElement(core.Capability("ALL")))
			Expect(fwCopy.SecurityContext.SeccompProfile).NotTo(BeNil())
			Expect(fwCopy.SecurityContext.SeccompProfile.Type).To(Equal(core.SeccompProfileTypeRuntimeDefault))

			By("updater container has capabilities.drop=ALL and RuntimeDefault seccomp")
			var updater *core.Container
			for i := range job.Spec.Template.Spec.Containers {
				if job.Spec.Template.Spec.Containers[i].Name == "updater" {
					updater = &job.Spec.Template.Spec.Containers[i]
					break
				}
			}
			Expect(updater).NotTo(BeNil())
			Expect(updater.SecurityContext).NotTo(BeNil())
			Expect(updater.SecurityContext.SeccompProfile).NotTo(BeNil())
			Expect(updater.SecurityContext.SeccompProfile.Type).To(Equal(core.SeccompProfileTypeRuntimeDefault))
			Expect(updater.SecurityContext.Capabilities).NotTo(BeNil())
			Expect(updater.SecurityContext.Capabilities.Drop).To(ContainElement(core.Capability("ALL")))
		})
	})
})

var _ = Describe("verifyChecksumsFromExport", func() {
	// makeTar returns a tar stream containing the given files under "fwupdate/".
	makeTar := func(files map[string][]byte) *bytes.Buffer {
		buf := &bytes.Buffer{}
		tw := tar.NewWriter(buf)

		for name, content := range files {
			hdr := &tar.Header{
				Name: "fwupdate/" + name,
				Mode: 0o600,
				Size: int64(len(content)),
			}
			ExpectWithOffset(1, tw.WriteHeader(hdr)).To(Succeed())
			_, err := tw.Write(content)
			ExpectWithOffset(1, err).NotTo(HaveOccurred())
		}

		ExpectWithOffset(1, tw.Close()).To(Succeed())

		return buf
	}

	// sha256Of returns the "sha256:<hex>" checksum of the given data.
	sha256Of := func(data []byte) string {
		h := sha256.New()
		h.Write(data)
		return fmt.Sprintf("sha256:%x", h.Sum(nil))
	}

	It("passes when all declared checksums match", func() {
		content := []byte("firmware binary data")
		files := []intelcomv1alpha1.GPUFirmwareFile{
			{Type: "GFX", FileName: "gfx.bin", Checksum: sha256Of(content)},
		}

		err := verifyChecksumsFromExport(makeTar(map[string][]byte{"gfx.bin": content}), files)
		Expect(err).NotTo(HaveOccurred())
	})

	It("passes when some files have no checksum (those are skipped)", func() {
		content := []byte("firmware binary data")
		files := []intelcomv1alpha1.GPUFirmwareFile{
			{Type: "GFX", FileName: "gfx.bin", Checksum: sha256Of(content)},
			{Type: "GFX_DATA", FileName: "gfxdata.bin"}, // no checksum — skipped
		}
		tarFiles := map[string][]byte{
			"gfx.bin":     content,
			"gfxdata.bin": []byte("other data"),
		}

		err := verifyChecksumsFromExport(makeTar(tarFiles), files)
		Expect(err).NotTo(HaveOccurred())
	})

	It("returns a mismatch error when checksum does not match", func() {
		files := []intelcomv1alpha1.GPUFirmwareFile{
			{Type: "GFX", FileName: "gfx.bin", Checksum: sha256Of([]byte("correct data"))},
		}

		err := verifyChecksumsFromExport(makeTar(map[string][]byte{"gfx.bin": []byte("wrong data")}), files)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("checksum mismatch"))
		Expect(err.Error()).To(ContainSubstring("gfx.bin"))
	})

	It("returns a distinct error when a declared file is absent from the image", func() {
		files := []intelcomv1alpha1.GPUFirmwareFile{
			{Type: "GFX", FileName: "missing.bin", Checksum: sha256Of([]byte("data"))},
		}

		err := verifyChecksumsFromExport(makeTar(map[string][]byte{"other.bin": []byte("data")}), files)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("not found"))
		Expect(err.Error()).To(ContainSubstring("missing.bin"))
	})

	It("ignores files outside the fwupdate/ directory", func() {
		content := []byte("firmware")
		files := []intelcomv1alpha1.GPUFirmwareFile{
			{Type: "GFX", FileName: "gfx.bin", Checksum: sha256Of(content)},
		}
		// Tar contains the file at the correct path AND a decoy at the wrong path.
		buf := &bytes.Buffer{}
		tw := tar.NewWriter(buf)
		for _, entry := range []struct {
			name string
			data []byte
		}{
			{"other/gfx.bin", []byte("wrong data at wrong path")},
			{"fwupdate/gfx.bin", content},
		} {
			hdr := &tar.Header{Name: entry.name, Mode: 0o600, Size: int64(len(entry.data))}
			Expect(tw.WriteHeader(hdr)).To(Succeed())
			_, err := tw.Write(entry.data)
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(tw.Close()).To(Succeed())

		err := verifyChecksumsFromExport(buf, files)
		Expect(err).NotTo(HaveOccurred())
	})
})

var _ = Describe("verifyContentImage via beginUpdate", func() {
	ctx := context.Background()

	typeNamespacedName := types.NamespacedName{
		Name:      resourceName,
		Namespace: "default",
	}

	var fakeClient client.Client
	var controllerReconciler *GPUFirmwareUpdateReconciler
	var resource *intelcomv1alpha1.GPUFirmwareUpdate
	var imgVerify *fakeContentImageVerifier

	BeforeEach(func() {
		scheme := runtime.NewScheme()
		Expect(intelcomv1alpha1.AddToScheme(scheme)).To(Succeed())
		Expect(core.AddToScheme(scheme)).To(Succeed())
		Expect(batch.AddToScheme(scheme)).To(Succeed())
		Expect(resv1.AddToScheme(scheme)).To(Succeed())

		imgVerify = &fakeContentImageVerifier{}

		fakeClient = fake.NewClientBuilder().
			WithScheme(scheme).
			WithIndex(&core.Pod{}, "spec.nodeName", podIndexerFunc).
			WithStatusSubresource(&intelcomv1alpha1.GPUFirmwareUpdate{}).
			Build()

		controllerReconciler = &GPUFirmwareUpdateReconciler{
			Client:    fakeClient,
			Scheme:    scheme,
			logRet:    &MockLogRetriever{},
			imgVerify: imgVerify,
			Opts:      ControllerOpts{Namespace: "default"},
		}

		resource = baseCr()
	})

	AfterEach(func() {
		res := &intelcomv1alpha1.GPUFirmwareUpdate{}
		err := fakeClient.Get(ctx, typeNamespacedName, res)
		if err == nil {
			Expect(fakeClient.Delete(ctx, res)).To(Succeed())
		}
	})

	It("proceeds past beginUpdate when verifier returns nil (no nodes available)", func() {
		imgVerify.err = nil
		Expect(fakeClient.Create(ctx, resource)).To(Succeed())

		_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
		Expect(err).To(HaveOccurred())

		Expect(fakeClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())
		// Image verification passed; failure is due to no nodes — state is error but not "content image verification failed".
		Expect(resource.Status.State).To(Equal(stateError))
		Expect(resource.Status.Messages).NotTo(ContainElement(ContainSubstring("content image verification failed")))
	})

	It("sets error state when verifier returns a checksum mismatch", func() {
		imgVerify.err = errors.New("checksum mismatch for \"gfx.bin\": expected sha256:aaa..., got sha256:bbb...")
		Expect(fakeClient.Create(ctx, resource)).To(Succeed())

		_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
		Expect(err).To(HaveOccurred())

		Expect(fakeClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())
		Expect(resource.Status.State).To(Equal(stateError))
		Expect(resource.Status.Messages).To(ContainElement(ContainSubstring("content image verification failed")))
	})

	It("sets error state when verifier returns an unreachable image error", func() {
		imgVerify.err = errors.New("content image \"registry/fw@sha256:abc\" is not reachable: connection refused")
		Expect(fakeClient.Create(ctx, resource)).To(Succeed())

		_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
		Expect(err).To(HaveOccurred())

		Expect(fakeClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())
		Expect(resource.Status.State).To(Equal(stateError))
		Expect(resource.Status.Messages).To(ContainElement(ContainSubstring("content image verification failed")))
	})
})
