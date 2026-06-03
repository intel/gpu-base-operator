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
	"sort"

	"github.com/google/go-cmp/cmp"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	v1alpha "github.com/intel/gpu-base-operator/api/v1alpha1"
	prometheusv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	core "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	kueuev1beta2 "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	nfdcrd "sigs.k8s.io/node-feature-discovery/api/nfd/v1alpha1"
)

var (
	testNodeName string = "test-node-01"
	i915         string = "i915"
	xe           string = "xe"
)

func kueueCRDObjects() (*apiextensionsv1.CustomResourceDefinition, *apiextensionsv1.CustomResourceDefinition, *apiextensionsv1.CustomResourceDefinition) {
	kueueCQ := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: kueueClusterQueueCrd,
		},
	}
	kueueRF := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: kueueResourceFlavorCrd,
		},
	}
	kueueLQ := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: kueueLocalQueueCrd,
		},
	}

	return kueueCQ, kueueRF, kueueLQ
}

func prometheusCRDObject() *apiextensionsv1.CustomResourceDefinition {
	prometheusCRD := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: serviceMonitorCrd,
		},
	}

	return prometheusCRD
}

func gpuNode() *core.Node {
	cpuQuantity := resource.NewQuantity(8, "")
	memQuantity := resource.NewQuantity(16000000000, "")
	gpuQuantity := resource.NewQuantity(8, "")

	return &core.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: testNodeName,
		},
		Status: core.NodeStatus{
			Capacity: core.ResourceList{
				"cpu":      *cpuQuantity,
				"memory":   *memQuantity,
				xeResource: *gpuQuantity,
			},
			Allocatable: core.ResourceList{
				"cpu":      *cpuQuantity,
				"memory":   *memQuantity,
				xeResource: *gpuQuantity,
			},
		},
	}
}

var _ = Describe("Misc", func() {

	Context("NFR creation", func() {
		checkValues := func(matchSet *nfdcrd.MatchExpressionSet) {
			for k, v := range *matchSet {
				switch k {
				case "vendor":
					Expect(v.Value).To(Equal(nfdcrd.MatchValue{"8086"}))
				case "class":
					Expect(v.Value).To(Equal(nfdcrd.MatchValue{"0300", "0380"}))
				default:
					Fail("unexpected match expression key: " + k)
				}
			}
		}

		It("with defaults", func() {
			spec := &v1alpha.ClusterPolicy{
				Spec: v1alpha.ClusterPolicySpec{},
			}

			nfr := createNfdRule(spec, "")
			Expect(nfr).NotTo(BeNil())

			Expect(nfr.Spec.Rules).To(HaveLen(1))
			rule := nfr.Spec.Rules[0]

			checkValues(rule.MatchFeatures[0].MatchExpressions)
		})
	})
})

func getTestCpuMemQuantity() (*resource.Quantity, *resource.Quantity) {
	cpuQuantity := resource.NewQuantity(8, "")
	memQuantity := resource.NewQuantity(16*10^9, "")
	// logging causes string representation to be created and cached
	_ = cpuQuantity.String()
	_ = memQuantity.String()

	return cpuQuantity, memQuantity
}

func getTestGpuQuantity() (*resource.Quantity, *resource.Quantity) {
	gpui915Quantity := resource.NewQuantity(5, "")
	gpuXeQuantity := resource.NewQuantity(7, "")
	// logging causes string representation to be created and cached
	_ = gpui915Quantity.String()
	_ = gpuXeQuantity.String()

	return gpui915Quantity, gpuXeQuantity
}

func getTestNode() core.Node {
	cpuQuantity, memQuantity := getTestCpuMemQuantity()
	gpui915Quantity, gpuXeQuantity := getTestGpuQuantity()

	return core.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: testNodeName,
		},
		Status: core.NodeStatus{
			Allocatable: core.ResourceList{
				"foo":                           *resource.NewQuantity(1000, ""),
				"bar":                           *resource.NewQuantity(2000, ""),
				core.ResourceCPU:                *cpuQuantity,
				core.ResourceMemory:             *memQuantity,
				gpui915ResourceName:             *gpui915Quantity,
				"gpu.intel.com/i915_monitoring": *gpui915Quantity,
				gpuXeResourceName:               *gpuXeQuantity,
				"gpu.intel.com/xe_monitoring":   *gpuXeQuantity,
			},
		},
	}
}

func sortClusterQueue(clusterQueue *kueuev1beta2.ClusterQueue) {
	coveredResources := clusterQueue.Spec.ResourceGroups[0].CoveredResources
	sort.Slice(coveredResources, func(i, j int) bool {
		return string(coveredResources[i]) < string(coveredResources[j])
	})

	resources := clusterQueue.Spec.ResourceGroups[0].Flavors[0].Resources
	sort.Slice(resources, func(i, j int) bool {
		return resources[i].Name < resources[j].Name
	})
}

var _ = Describe("Misc Kueue", func() {

	Context("Cluster resource map with two dummy entries", func() {
		It("with two resources", func() {
			resourceMap := clusterResourceMap{
				"foo": &resource.Quantity{},
				"bar": &resource.Quantity{},
			}

			r := &MiscReconciler{}
			clusterQueue := r.createClusterQueue(resourceMap, &v1alpha.ClusterQueueSpec{Name: "test-cluster-queue"})
			Expect(clusterQueue).NotTo(BeNil())
			resourceGroups := clusterQueue.Spec.ResourceGroups
			Expect(resourceGroups).To(HaveLen(1))
			Expect(resourceGroups[0].CoveredResources).To(HaveLen(2))
			Expect(resourceGroups[0].Flavors).To(HaveLen(1))
			resources := resourceGroups[0].Flavors[0].Resources
			Expect(resources).To(HaveLen(2))

			expectedQuantity := resource.NewQuantity(1, "")

			addToResource(resourceMap, "foo", *resource.NewQuantity(1, ""))
			Expect(resourceMap).To(HaveLen(2))
			Expect(resourceMap["foo"]).To(Equal(expectedQuantity))

			addToResource(resourceMap, "bar", *resource.NewQuantity(1, ""))
			Expect(resourceMap).To(HaveLen(2))
			Expect(resourceMap["bar"]).To(Equal(expectedQuantity))

			expectedQuantity.Add(*resource.NewQuantity(1, ""))
			addToResource(resourceMap, "bar", *resource.NewQuantity(1, ""))
			Expect(resourceMap["bar"]).To(Equal(expectedQuantity))
		})
	})

	Context("Resources for a cluster node", func() {

		It("get device plugin resources", func() {
			r := MiscReconciler{}
			cpuQuantity, memQuantity := getTestCpuMemQuantity()
			gpui915Quantity, gpuXeQuantity := getTestGpuQuantity()
			node := getTestNode()
			cnMap := clusterNodeMap{}
			cnMap[node.Name] = &node

			resourceMap := r.getDevicePluginResources(cnMap)
			Expect(resourceMap).NotTo(BeNil())
			Expect(resourceMap["foo"]).To(BeNil())
			Expect(resourceMap["bar"]).To(BeNil())
			Expect(resourceMap[gpui915ResourceName]).To(Equal(gpui915Quantity))
			Expect(resourceMap["gpu.intel.com/i915_monitor"]).To(BeNil())
			Expect(resourceMap[gpuXeResourceName]).To(Equal(gpuXeQuantity))
			Expect(resourceMap["gpu.intel.com/xe_monitor"]).To(BeNil())
			Expect(resourceMap[string(core.ResourceCPU)]).NotTo(BeNil())
			Expect(resourceMap[string(core.ResourceCPU)]).To(Equal(cpuQuantity))
			Expect(resourceMap[string(core.ResourceMemory)]).NotTo(BeNil())
			Expect(resourceMap[string(core.ResourceMemory)]).To(Equal(memQuantity))
		})

		It("get DRA resource slice resources", func() {
			draQuantity := resource.NewQuantity(6, "")
			foo := "foo"
			bar := "bar"
			cnMap := make(clusterNodeMap)
			node := getTestNode()
			cnMap[node.Name] = &node
			resourceMap := make(clusterResourceMap)
			testOptsName := "test-opts-name"
			r := MiscReconciler{}
			r.Opts.ReqName = testOptsName

			resourceSlice := resourcev1.ResourceSlice{
				Spec: resourcev1.ResourceSliceSpec{
					Driver:   gpuResourceName,
					NodeName: &testNodeName,
					Devices: []resourcev1.Device{
						{
							Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
								resourcev1.QualifiedName("driver"): {
									StringValue: &i915,
								},
							},
							Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
								"millicores": {
									Value: *resource.NewQuantity(1300, ""),
								},
							},
						},
						{
							Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
								resourcev1.QualifiedName("driver"): {
									StringValue: &xe,
								},
							},
							Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
								"millicores": {
									Value: *resource.NewQuantity(6000, ""),
								},
							},
						},
						{
							Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
								resourcev1.QualifiedName("family"): {
									StringValue: &foo,
								},
							},
						},
						{
							Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
								"memory": {
									Value: *resource.NewQuantity(1300, ""),
								},
							},
						},
						{
							Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
								resourcev1.QualifiedName("driver"): {
									StringValue: &i915,
								},
							},
							Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
								"memory": {
									Value: *resource.NewQuantity(6000, ""),
								},
							},
						},
						{
							Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
								resourcev1.QualifiedName("pciRoot"): {
									StringValue: &bar,
								},
							},
						},
						{
							Name: "monitoring",
						},
					},
				},
			}

			nodeName := r.addResourceFromResourceSlice(&resourceSlice, resourceMap, cnMap)
			Expect(*nodeName).To(Equal(testNodeName))
			Expect(resourceMap[foo]).To(BeNil())
			Expect(resourceMap[draResourceName]).To(Equal(draQuantity))
			Expect(resourceMap[gpui915ResourceName]).To(BeNil())
			Expect(resourceMap[gpuXeResourceName]).To(BeNil())

			cpuQuantity, memQuantity := getTestCpuMemQuantity()
			addToResource(resourceMap, string(core.ResourceMemory), *memQuantity)
			addToResource(resourceMap, string(core.ResourceCPU), *cpuQuantity)

			clusterQueueSpec := v1alpha.ClusterQueueSpec{
				Name: "test-spec-name",
			}

			expectedClusterQueue := &kueuev1beta2.ClusterQueue{
				ObjectMeta: metav1.ObjectMeta{
					Name: clusterQueueSpec.Name,
					Labels: map[string]string{
						"app":   kueueAppLabel,
						"owner": testOptsName,
					},
				},
				Spec: kueuev1beta2.ClusterQueueSpec{
					NamespaceSelector: &metav1.LabelSelector{},
					ResourceGroups: []kueuev1beta2.ResourceGroup{
						{
							CoveredResources: []core.ResourceName{
								core.ResourceName(draResourceName), core.ResourceMemory, core.ResourceCPU,
							},
							Flavors: []kueuev1beta2.FlavorQuotas{
								{
									Name: kueueFlavorName,
									Resources: []kueuev1beta2.ResourceQuota{
										{Name: core.ResourceMemory, NominalQuota: *memQuantity},
										{Name: core.ResourceCPU, NominalQuota: *cpuQuantity},
										{Name: core.ResourceName(draResourceName), NominalQuota: *draQuantity},
									},
								},
							},
						},
					},
				},
			}
			sortClusterQueue(expectedClusterQueue)

			actualClusterQueue := r.createClusterQueue(resourceMap, &clusterQueueSpec)
			sortClusterQueue(actualClusterQueue)

			Expect(cmp.Equal(actualClusterQueue, expectedClusterQueue)).To(BeTrue())

		})

		It("split resources into queues", func() {
			r := MiscReconciler{}
			cpuQuantity, memQuantity := getTestCpuMemQuantity()
			gpui915Quantity, gpuXeQuantity := getTestGpuQuantity()

			resources := clusterResourceMap{
				string(core.ResourceCPU):    cpuQuantity,
				string(core.ResourceMemory): memQuantity,
				gpui915ResourceName:         gpui915Quantity,
				gpuXeResourceName:           gpuXeQuantity,
			}
			quantity1 := resource.NewQuantity(1, "")
			quantity2 := resource.NewQuantity(2, "")

			splitResources, err := r.divideResources(resources, 4)
			Expect(err).ToNot(HaveOccurred())
			Expect(splitResources).To(HaveLen(4))
			Expect(splitResources[0][gpui915ResourceName]).To(Equal(quantity2))
			Expect(splitResources[0][gpuXeResourceName]).To(Equal(quantity2))
			Expect(splitResources[1][gpui915ResourceName]).To(Equal(quantity1))
			Expect(splitResources[1][gpuXeResourceName]).To(Equal(quantity2))
			Expect(splitResources[2][gpui915ResourceName]).To(Equal(quantity1))
			Expect(splitResources[2][gpuXeResourceName]).To(Equal(quantity2))
			Expect(splitResources[3][gpui915ResourceName]).To(Equal(quantity1))
			Expect(splitResources[3][gpuXeResourceName]).To(Equal(quantity1))

			_, err = r.divideResources(resources, 6)
			Expect(err).To(HaveOccurred())
		})

		It("reconcile Kueue with one node", func() {
			s := runtime.NewScheme()
			Expect(core.AddToScheme(s)).To(Succeed())
			Expect(kueuev1beta2.AddToScheme(s)).To(Succeed())
			Expect(apiextensionsv1.AddToScheme(s)).To(Succeed())

			cp := &v1alpha.ClusterPolicy{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-cluster-policy",
				},
				Spec: v1alpha.ClusterPolicySpec{
					ResourceRegistration: "dp",
					EnableKueue:          true,

					Kueue: &v1alpha.KueueQueueSpec{
						EqualResources: []v1alpha.ClusterQueueSpec{
							{
								Name: "test-spec-name",
								LocalQueues: []v1alpha.LocalQueueSpec{
									{Name: "my-queue-1", Namespace: "default"},
									{Name: "my-queue-2", Namespace: "default"},
								},
							},
						},
					},
				},
			}
			ctx := context.Background()

			crd1, crd2, crd3 := kueueCRDObjects()
			node := gpuNode()

			r := MiscReconciler{}
			r.Client = fake.NewClientBuilder().WithScheme(s).WithObjects(crd1, crd2, crd3, node).Build()
			r.Opts = ControllerOpts{ReqName: "test-cluster-policy"}

			err := r.reconcileKueueObjects(ctx, cp)
			Expect(err).NotTo(HaveOccurred())

			flavor := &kueuev1beta2.ResourceFlavor{}
			err = r.Get(ctx, types.NamespacedName{Name: kueueFlavorName}, flavor)
			Expect(err).NotTo(HaveOccurred())

			By("verifying LocalQueues were created")
			localQueue1 := &kueuev1beta2.LocalQueue{}
			err = r.Get(ctx, types.NamespacedName{Name: "my-queue-1", Namespace: "default"}, localQueue1)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(localQueue1.Spec.ClusterQueue)).To(Equal("test-spec-name"))

			localQueue2 := &kueuev1beta2.LocalQueue{}
			err = r.Get(ctx, types.NamespacedName{Name: "my-queue-2", Namespace: "default"}, localQueue2)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(localQueue2.Spec.ClusterQueue)).To(Equal("test-spec-name"))

			cp.Spec.EnableKueue = false
			cp.Spec.Kueue = nil

			err = r.reconcileKueueObjects(ctx, cp)
			Expect(err).NotTo(HaveOccurred())

			err = r.Get(ctx, types.NamespacedName{Name: kueueFlavorName}, flavor)
			Expect(err).To(HaveOccurred())
		})
	})
})

var _ = Describe("Misc Prometheus", func() {
	Context("Reconcile", func() {
		It("disabled prometheus", func() {
			s := runtime.NewScheme()
			Expect(core.AddToScheme(s)).To(Succeed())
			Expect(apiextensionsv1.AddToScheme(s)).To(Succeed())
			Expect(prometheusv1.AddToScheme(s)).To(Succeed())

			cp := &v1alpha.ClusterPolicy{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-cluster-policy",
				},
				Spec: v1alpha.ClusterPolicySpec{
					ResourceRegistration:  "dp",
					ResourceMonitoring:    true,
					PrometheusIntegration: false,
				},
			}
			ctx := context.Background()

			r := MiscReconciler{}
			r.Client = fake.NewClientBuilder().WithScheme(s).WithObjects(prometheusCRDObject()).Build()
			r.Opts = ControllerOpts{ReqName: "test-cluster-policy", Namespace: "default"}

			err := r.reconcilePrometheusComponents(ctx, cp)
			Expect(err).NotTo(HaveOccurred())

			sm := &prometheusv1.ServiceMonitor{}
			err = r.Get(ctx, types.NamespacedName{Name: "intel-xpumanager", Namespace: r.Opts.Namespace}, sm)
			Expect(err).To(HaveOccurred())
		})

		It("enable and disable prometheus", func() {
			s := runtime.NewScheme()
			Expect(core.AddToScheme(s)).To(Succeed())
			Expect(apiextensionsv1.AddToScheme(s)).To(Succeed())
			Expect(prometheusv1.AddToScheme(s)).To(Succeed())

			cp := &v1alpha.ClusterPolicy{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-cluster-policy",
				},
				Spec: v1alpha.ClusterPolicySpec{
					ResourceRegistration:  "dp",
					ResourceMonitoring:    true,
					PrometheusIntegration: true,
				},
			}
			ctx := context.Background()

			r := MiscReconciler{}
			r.Client = fake.NewClientBuilder().WithScheme(s).WithObjects(prometheusCRDObject()).Build()
			r.Opts = ControllerOpts{ReqName: "test-cluster-policy", Namespace: "default"}

			err := r.reconcilePrometheusComponents(ctx, cp)
			Expect(err).NotTo(HaveOccurred())

			sm := &prometheusv1.ServiceMonitor{}
			err = r.Get(ctx, types.NamespacedName{Name: "intel-xpumanager", Namespace: r.Opts.Namespace}, sm)
			Expect(err).NotTo(HaveOccurred())

			service := &core.Service{}
			err = r.Get(ctx, types.NamespacedName{Name: "intel-xpumanager", Namespace: r.Opts.Namespace}, service)
			Expect(err).NotTo(HaveOccurred())

			cp.Spec.PrometheusIntegration = false
			err = r.reconcilePrometheusComponents(ctx, cp)
			Expect(err).NotTo(HaveOccurred())

			err = r.Get(ctx, types.NamespacedName{Name: "intel-xpumanager", Namespace: r.Opts.Namespace}, sm)
			Expect(err).To(HaveOccurred())

			err = r.Get(ctx, types.NamespacedName{Name: "intel-xpumanager", Namespace: r.Opts.Namespace}, service)
			Expect(err).To(HaveOccurred())
		})

		It("enable and remove prometheus", func() {
			s := runtime.NewScheme()
			Expect(core.AddToScheme(s)).To(Succeed())
			Expect(apiextensionsv1.AddToScheme(s)).To(Succeed())
			Expect(prometheusv1.AddToScheme(s)).To(Succeed())

			cp := &v1alpha.ClusterPolicy{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-cluster-policy",
				},
				Spec: v1alpha.ClusterPolicySpec{
					ResourceRegistration:  "dp",
					ResourceMonitoring:    true,
					PrometheusIntegration: true,
				},
			}
			ctx := context.Background()

			r := MiscReconciler{}
			r.Client = fake.NewClientBuilder().WithScheme(s).WithObjects(prometheusCRDObject()).Build()
			r.Opts = ControllerOpts{ReqName: "test-cluster-policy", Namespace: "default"}

			_, err := r.Reconcile(ctx, cp)
			Expect(err).NotTo(HaveOccurred())

			sm := &prometheusv1.ServiceMonitor{}
			err = r.Get(ctx, types.NamespacedName{Name: "intel-xpumanager", Namespace: r.Opts.Namespace}, sm)
			Expect(err).NotTo(HaveOccurred())

			service := &core.Service{}
			err = r.Get(ctx, types.NamespacedName{Name: "intel-xpumanager", Namespace: r.Opts.Namespace}, service)
			Expect(err).NotTo(HaveOccurred())

			_, err = r.Reconcile(ctx, nil)
			Expect(err).NotTo(HaveOccurred())

			err = r.Get(ctx, types.NamespacedName{Name: "intel-xpumanager", Namespace: r.Opts.Namespace}, sm)
			Expect(err).To(HaveOccurred())

			err = r.Get(ctx, types.NamespacedName{Name: "intel-xpumanager", Namespace: r.Opts.Namespace}, service)
			Expect(err).To(HaveOccurred())
		})

		It("nil ClusterPolicy returns immediately", func() {
			s := runtime.NewScheme()
			Expect(core.AddToScheme(s)).To(Succeed())
			Expect(prometheusv1.AddToScheme(s)).To(Succeed())

			r := MiscReconciler{}
			r.Client = fake.NewClientBuilder().WithScheme(s).Build()
			r.Opts = ControllerOpts{ReqName: "test-cluster-policy", Namespace: "default"}

			err := r.reconcilePrometheusComponents(context.Background(), nil)
			Expect(err).NotTo(HaveOccurred())
		})

		It("removes prometheus resources when they are already absent", func() {
			s := runtime.NewScheme()
			Expect(core.AddToScheme(s)).To(Succeed())
			Expect(apiextensionsv1.AddToScheme(s)).To(Succeed())
			Expect(prometheusv1.AddToScheme(s)).To(Succeed())

			r := MiscReconciler{}
			r.Client = fake.NewClientBuilder().WithScheme(s).WithObjects(prometheusCRDObject()).Build()
			r.Opts = ControllerOpts{ReqName: "test-cluster-policy", Namespace: "default"}

			err := r.removePrometheusComponents(context.Background(), "test-cluster-policy")
			Expect(err).NotTo(HaveOccurred())
		})
	})
})

func nfdCRDObject(scope apiextensionsv1.ResourceScope) *apiextensionsv1.CustomResourceDefinition {
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: nfdRuleCrd,
		},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Scope: scope,
		},
	}
}

var _ = Describe("Misc NFD", func() {

	cp := func(useNFD bool) *v1alpha.ClusterPolicy {
		return &v1alpha.ClusterPolicy{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-cluster-policy",
			},
			Spec: v1alpha.ClusterPolicySpec{
				UseNFDLabeling: useNFD,
			},
		}
	}

	Context("cluster-scoped NFD CRD", func() {
		It("creates NFR without namespace when UseNFDLabeling is true", func() {
			s := runtime.NewScheme()
			Expect(apiextensionsv1.AddToScheme(s)).To(Succeed())
			Expect(nfdcrd.AddToScheme(s)).To(Succeed())

			r := &MiscReconciler{}
			r.Client = fake.NewClientBuilder().WithScheme(s).
				WithObjects(nfdCRDObject(apiextensionsv1.ClusterScoped)).
				Build()
			r.Opts = ControllerOpts{ReqName: "test-cluster-policy", Namespace: "gpu-operator"}
			ctx := context.Background()

			err := r.reconcileNfdRules(ctx, cp(true))
			Expect(err).NotTo(HaveOccurred())

			nfr := &nfdcrd.NodeFeatureRule{}
			err = r.Get(ctx, types.NamespacedName{Name: nfdRuleName}, nfr)
			Expect(err).NotTo(HaveOccurred())
			Expect(nfr.Namespace).To(BeEmpty())
		})

		It("deletes NFR without namespace when UseNFDLabeling is false", func() {
			existingNfr := &nfdcrd.NodeFeatureRule{
				ObjectMeta: metav1.ObjectMeta{
					Name: nfdRuleName,
				},
			}

			s := runtime.NewScheme()
			Expect(apiextensionsv1.AddToScheme(s)).To(Succeed())
			Expect(nfdcrd.AddToScheme(s)).To(Succeed())

			r := &MiscReconciler{}
			r.Client = fake.NewClientBuilder().WithScheme(s).
				WithObjects(nfdCRDObject(apiextensionsv1.ClusterScoped)).
				WithObjects(existingNfr).
				Build()
			r.Opts = ControllerOpts{ReqName: "test-cluster-policy", Namespace: "gpu-operator"}
			ctx := context.Background()

			err := r.reconcileNfdRules(ctx, cp(false))
			Expect(err).NotTo(HaveOccurred())

			nfr := &nfdcrd.NodeFeatureRule{}
			err = r.Get(ctx, types.NamespacedName{Name: nfdRuleName}, nfr)
			Expect(err).To(HaveOccurred())
		})
	})

	Context("namespace-scoped NFD CRD", func() {
		It("creates NFR with operator namespace when UseNFDLabeling is true", func() {
			s := runtime.NewScheme()
			Expect(apiextensionsv1.AddToScheme(s)).To(Succeed())
			Expect(nfdcrd.AddToScheme(s)).To(Succeed())

			r := &MiscReconciler{}
			r.Client = fake.NewClientBuilder().WithScheme(s).
				WithObjects(nfdCRDObject(apiextensionsv1.NamespaceScoped)).
				Build()
			r.Opts = ControllerOpts{ReqName: "test-cluster-policy", Namespace: "gpu-operator"}
			ctx := context.Background()

			err := r.reconcileNfdRules(ctx, cp(true))
			Expect(err).NotTo(HaveOccurred())

			nfr := &nfdcrd.NodeFeatureRule{}
			err = r.Get(ctx, types.NamespacedName{Name: nfdRuleName, Namespace: r.Opts.Namespace}, nfr)
			Expect(err).NotTo(HaveOccurred())
			Expect(nfr.Namespace).To(Equal(r.Opts.Namespace))
		})

		It("deletes NFR with operator namespace when UseNFDLabeling is false", func() {
			existingNfr := &nfdcrd.NodeFeatureRule{
				ObjectMeta: metav1.ObjectMeta{
					Name:      nfdRuleName,
					Namespace: "gpu-operator",
				},
			}

			s := runtime.NewScheme()
			Expect(apiextensionsv1.AddToScheme(s)).To(Succeed())
			Expect(nfdcrd.AddToScheme(s)).To(Succeed())

			r := &MiscReconciler{}
			r.Client = fake.NewClientBuilder().WithScheme(s).
				WithObjects(nfdCRDObject(apiextensionsv1.NamespaceScoped)).
				WithObjects(existingNfr).
				Build()
			r.Opts = ControllerOpts{ReqName: "test-cluster-policy", Namespace: "gpu-operator"}
			ctx := context.Background()

			err := r.reconcileNfdRules(ctx, cp(false))
			Expect(err).NotTo(HaveOccurred())

			nfr := &nfdcrd.NodeFeatureRule{}
			err = r.Get(ctx, types.NamespacedName{Name: nfdRuleName, Namespace: "gpu-operator"}, nfr)
			Expect(err).To(HaveOccurred())
		})
	})
})
