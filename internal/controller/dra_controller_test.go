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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apps "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	rbac "k8s.io/api/rbac/v1"
	resv1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha "github.com/intel/gpu-base-operator/api/v1alpha1"
	"github.com/intel/gpu-base-operator/config/deployments"
)

var _ = Describe("ClusterPolicy Controller for DRA", func() {

	Context("When reconciling DRA", func() {
		defaultNamespace := "foobar-dra"
		const resourceName = "test-resource-dra"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name: resourceName,
		}
		clusterpolicy := &v1alpha.ClusterPolicy{}

		BeforeEach(func() {
			ns := &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: defaultNamespace,
				},
			}

			Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		})

		AfterEach(func() {
			resource := &v1alpha.ClusterPolicy{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance ClusterPolicy")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})

		It("should successfully reconcile the resource", func() {
			By("creating the custom resource for the Kind ClusterPolicy")
			err := k8sClient.Get(ctx, typeNamespacedName, clusterpolicy)
			if err != nil && errors.IsNotFound(err) {
				resource := &v1alpha.ClusterPolicy{
					ObjectMeta: metav1.ObjectMeta{
						Name: resourceName,
					},
					Spec: v1alpha.ClusterPolicySpec{
						ResourceRegistration: "dra",
						DynamicResourceAllocationSpec: v1alpha.DynamicResourceAllocationSpec{
							Image: "dra-image:v1.2.3",
						},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}

			By("Reconciling the created resource")
			controllerReconciler := &ClusterPolicyReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Opts: ControllerOpts{
					Namespace:    defaultNamespace,
					DRAEnable:    true,
					RequeueDelay: time.Millisecond * 50,
				},
			}

			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})

			Expect(err).NotTo(HaveOccurred())

			daemonSets := apps.DaemonSetList{}
			err = k8sClient.List(ctx, &daemonSets, client.InNamespace(defaultNamespace))
			Expect(err).NotTo(HaveOccurred())

			Expect(daemonSets.Items).To(HaveLen(1))

			for _, ds := range daemonSets.Items {
				switch ds.Name {
				case "test-resource-dra-gpu-dra":
					Expect(ds.Spec.Template.Spec.Containers[0].Image).To(Equal("dra-image:v1.2.3"))
				default:
					Fail("Unexpected DaemonSet found: " + ds.Name)
				}
			}
		})
	})

	Context("When reconciling DRA and XPUM", func() {
		defaultNamespace := "foobar-dra-xpum"
		const resourceName = "test-resource-dra-xpum"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name: resourceName,
		}
		clusterpolicy := &v1alpha.ClusterPolicy{}

		BeforeEach(func() {
			ns := &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: defaultNamespace,
					Labels: map[string]string{
						"resource.kubernetes.io/admin-access": "true",
					},
				},
			}

			Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		})

		AfterEach(func() {
			resource := &v1alpha.ClusterPolicy{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance ClusterPolicy")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})

		It("should successfully reconcile the resource", func() {
			By("creating the custom resource for the Kind ClusterPolicy")
			err := k8sClient.Get(ctx, typeNamespacedName, clusterpolicy)
			if err != nil && errors.IsNotFound(err) {
				resource := &v1alpha.ClusterPolicy{
					ObjectMeta: metav1.ObjectMeta{
						Name: resourceName,
					},
					Spec: v1alpha.ClusterPolicySpec{
						ResourceRegistration: "dra",
						ResourceMonitoring:   true,
						UseNFDLabeling:       true,
						DynamicResourceAllocationSpec: v1alpha.DynamicResourceAllocationSpec{
							Image: "dra-image:v1.2.3",
						},
						XpuManagerSpec: v1alpha.XpuManagerSpec{
							Image:    "xpumanager:v3.2.1",
							LogLevel: 3,
						},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}

			By("Reconciling the created resource")
			controllerReconciler := &ClusterPolicyReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Opts: ControllerOpts{
					Namespace:    defaultNamespace,
					DRAEnable:    true,
					RequeueDelay: time.Millisecond * 50,
				},
			}

			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})

			Expect(err).NotTo(HaveOccurred())

			daemonSets := apps.DaemonSetList{}
			err = k8sClient.List(ctx, &daemonSets, client.InNamespace(defaultNamespace))
			Expect(err).NotTo(HaveOccurred())

			Expect(daemonSets.Items).To(HaveLen(2))

			for _, ds := range daemonSets.Items {
				switch ds.Name {
				case "test-resource-dra-xpum-gpu-dra":
					Expect(ds.Spec.Template.Spec.Containers[0].Image).To(Equal("dra-image:v1.2.3"))
					Expect(ds.Spec.Template.Spec.NodeSelector).To(HaveKeyWithValue("intel.feature.node.kubernetes.io/gpu", "true"))
					Expect(ds.Spec.Template.Spec.NodeSelector).To(HaveKeyWithValue("kubernetes.io/arch", "amd64"))
				case "test-resource-dra-xpum-xpu-manager":
					Expect(ds.Spec.Template.Spec.Containers[0].Image).To(Equal("xpumanager:v3.2.1"))
					Expect(ds.Spec.Template.Spec.NodeSelector).To(HaveKeyWithValue("intel.feature.node.kubernetes.io/gpu", "true"))
					Expect(ds.Spec.Template.Spec.NodeSelector).To(HaveKeyWithValue("kubernetes.io/arch", "amd64"))
				default:
					Fail("Unexpected DaemonSet found: " + ds.Name)
				}
			}

			dcList := &resv1.DeviceClassList{}
			err = k8sClient.List(ctx, dcList)
			Expect(err).NotTo(HaveOccurred())
			Expect(dcList.Items).To(HaveLen(2))
			for _, dc := range dcList.Items {
				switch dc.Name {
				case "gpu.intel.com":
				case "gpu-vfio.intel.com":
					Expect(dc.Spec.Selectors).To(HaveLen(2))
				default:
					Fail("Unexpected DeviceClass found: " + dc.Name)
				}
			}

			Expect(k8sClient.Get(ctx, typeNamespacedName, clusterpolicy)).To(Succeed(), "Failed to get ClusterPolicy after reconciliation")

			clusterpolicy.Spec.DynamicResourceAllocationSpec.ManageBinding = true
			Expect(k8sClient.Update(ctx, clusterpolicy)).To(Succeed(), "Failed to update ClusterPolicy with ManageBinding change")

			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			err = k8sClient.List(ctx, dcList)
			Expect(err).NotTo(HaveOccurred())
			Expect(dcList.Items).To(HaveLen(2))

			for _, dc := range dcList.Items {
				switch dc.Name {
				case "gpu.intel.com":
				case "gpu-vfio.intel.com":
					Expect(dc.Spec.Selectors).To(HaveLen(1))
				default:
					Fail("Unexpected DeviceClass found: " + dc.Name)
				}
			}
		})
	})

	Context("When reconciling DRA on OpenShift", func() {
		defaultNamespace := "foobar-dra-ocp"
		const resourceName = "test-resource-dra-ocp"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{Name: resourceName}

		BeforeEach(func() {
			Expect(k8sClient.Create(ctx, &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: defaultNamespace},
			})).To(Succeed())
		})

		AfterEach(func() {
			resource := &v1alpha.ClusterPolicy{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())

			deleteOpenShiftSCCResources(ctx, k8sClient,
				resourceName+"-gpu-dra-scc",
				resourceName+"-gpu-dra-scc-role",
				resourceName+"-gpu-dra-scc-binding",
				"", "")
		})

		It("creates DRA SCC resources and cleans them up on DRA removal", func() {
			By("creating the ClusterPolicy with DRA mode")
			Expect(k8sClient.Create(ctx, &v1alpha.ClusterPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName},
				Spec: v1alpha.ClusterPolicySpec{
					ResourceRegistration: "dra",
					DynamicResourceAllocationSpec: v1alpha.DynamicResourceAllocationSpec{
						Image: "intel/gpu-dra-driver:test",
					},
				},
			})).To(Succeed())

			reconciler := &ClusterPolicyReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Opts: ControllerOpts{
					Namespace:    defaultNamespace,
					DRAEnable:    true,
					OpenShift:    true,
					RequeueDelay: time.Millisecond * 50,
				},
			}

			By("reconcile creates DRA SCC resources")
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("DRA SCC is created")
			draSCC := &unstructured.Unstructured{}
			draSCC.SetAPIVersion(sccAPIVersion)
			draSCC.SetKind(sccKind)
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: resourceName + "-gpu-dra-scc"}, draSCC)).To(Succeed())

			By("DRA SCC ClusterRole is created")
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: resourceName + "-gpu-dra-scc-role"}, &rbac.ClusterRole{})).To(Succeed())

			By("DRA SCC ClusterRoleBinding is created")
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: resourceName + "-gpu-dra-scc-binding"}, &rbac.ClusterRoleBinding{})).To(Succeed())

			By("second reconcile is idempotent")
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("creating a ResourceClaim exercises anyAllocatedResourceClaims loop")
			resourceClaim := &resv1.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-rc-coverage",
					Namespace: defaultNamespace,
				},
				Spec: resv1.ResourceClaimSpec{
					Devices: resv1.DeviceClaim{
						Requests: []resv1.DeviceRequest{
							{Name: "gpu", Exactly: &resv1.ExactDeviceRequest{DeviceClassName: gpuDeviceClass}},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, resourceClaim)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, resourceClaim) })

			By("switching to DP mode removes DRA SCC resources")
			cp := &v1alpha.ClusterPolicy{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, cp)).To(Succeed())
			cp.Spec.ResourceRegistration = "dp"
			cp.Spec.DevicePluginSpec = v1alpha.DevicePluginSpec{
				PluginImage: "intel/intel-gpu-plugin:test",
			}
			Expect(k8sClient.Update(ctx, cp)).To(Succeed())

			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			draSCCCheck := &unstructured.Unstructured{}
			draSCCCheck.SetAPIVersion(sccAPIVersion)
			draSCCCheck.SetKind(sccKind)
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: resourceName + "-gpu-dra-scc"}, draSCCCheck)).
				To(Satisfy(errors.IsNotFound))
		})
	})

	Context("DaemonSet object update", func() {
		It("with tolerations, nodeSelector and pullsecret", func() {
			cp := &v1alpha.ClusterPolicy{
				Spec: v1alpha.ClusterPolicySpec{
					LogLevel: 2,
					DynamicResourceAllocationSpec: v1alpha.DynamicResourceAllocationSpec{
						LogLevel:       3,
						PodHealthCheck: true,
					},
					Tolerations: []v1.Toleration{
						{
							Key:      "my-toleration",
							Operator: v1.TolerationOpExists,
							Effect:   v1.TaintEffectNoExecute,
						},
					},
					PullSecret: &v1.LocalObjectReference{
						Name: "my-pull-secret",
					},
					NodeSelector: map[string]string{
						"foo": "bar",
					},
				},
			}
			controller := &DRAReconciler{}

			ds := deployments.DynamicResourceAllocationDaemonset()
			controller.updateDaemonSetObject(ds, cp)

			Expect(ds.Spec.Template.Spec.NodeSelector).To(HaveKeyWithValue("foo", "bar"))

			Expect(ds.Spec.Template.Spec.Tolerations[0].Key).To(Equal("my-toleration"))
			Expect(ds.Spec.Template.Spec.Tolerations[0].Operator).To(Equal(v1.TolerationOpExists))
			Expect(ds.Spec.Template.Spec.Tolerations[0].Effect).To(Equal(v1.TaintEffectNoExecute))

			Expect(ds.Spec.Template.Spec.ImagePullSecrets[0].Name).To(Equal("my-pull-secret"))

			Expect(ds.Spec.Template.Spec.Containers[0].Args).To(ContainElement("--healthcheck-port=51516"))
			Expect(ds.Spec.Template.Spec.Containers[0].Ports).To(ContainElement(v1.ContainerPort{
				Name:          "healthcheck",
				ContainerPort: 51516,
			}))

			Expect(ds.Spec.Template.Spec.Containers[0].StartupProbe).ToNot(BeNil())
			Expect(ds.Spec.Template.Spec.Containers[0].StartupProbe.GRPC).ToNot(BeNil())
			Expect(ds.Spec.Template.Spec.Containers[0].StartupProbe.GRPC.Port).To(Equal(int32(51516)))

			Expect(ds.Spec.Template.Spec.Containers[0].LivenessProbe).ToNot(BeNil())
			Expect(ds.Spec.Template.Spec.Containers[0].LivenessProbe.GRPC).ToNot(BeNil())
			Expect(ds.Spec.Template.Spec.Containers[0].LivenessProbe.GRPC.Port).To(Equal(int32(51516)))

			// Update with no changes
			controller.updateDaemonSetObject(ds, cp)
			Expect(ds.Spec.Template.Spec.Containers[0].StartupProbe).ToNot(BeNil())
			Expect(ds.Spec.Template.Spec.Containers[0].LivenessProbe).ToNot(BeNil())

			cp.Spec.PullSecret = nil
			cp.Spec.Tolerations = nil
			cp.Spec.DynamicResourceAllocationSpec.PodHealthCheck = false
			controller.updateDaemonSetObject(ds, cp)

			Expect(ds.Spec.Template.Spec.Tolerations).To(BeEmpty())
			Expect(ds.Spec.Template.Spec.ImagePullSecrets).To(BeEmpty())
			Expect(ds.Spec.Template.Spec.Containers[0].Args).To(ContainElement("--healthcheck-port=-1"))
			Expect(ds.Spec.Template.Spec.Containers[0].StartupProbe).To(BeNil())
			Expect(ds.Spec.Template.Spec.Containers[0].LivenessProbe).To(BeNil())
		})

		It("preserves kubelet-plugin container security context from YAML", func() {
			cp := &v1alpha.ClusterPolicy{
				Spec: v1alpha.ClusterPolicySpec{
					DynamicResourceAllocationSpec: v1alpha.DynamicResourceAllocationSpec{
						Image: "intel/gpu-dra-driver:test",
					},
				},
			}
			controller := &DRAReconciler{}

			ds := deployments.DynamicResourceAllocationDaemonset()
			controller.updateDaemonSetObject(ds, cp)

			var kubeletPlugin *v1.Container
			for i := range ds.Spec.Template.Spec.Containers {
				if ds.Spec.Template.Spec.Containers[i].Name == "kubelet-plugin" {
					kubeletPlugin = &ds.Spec.Template.Spec.Containers[i]
					break
				}
			}
			Expect(kubeletPlugin).NotTo(BeNil())
			Expect(kubeletPlugin.SecurityContext).NotTo(BeNil())
			Expect(kubeletPlugin.SecurityContext.AllowPrivilegeEscalation).NotTo(BeNil())
			Expect(*kubeletPlugin.SecurityContext.AllowPrivilegeEscalation).To(BeFalse())
		})
	})

	Context("DRA arguments handling", func() {
		It("with health monitoring", func() {
			cp := &v1alpha.ClusterPolicy{
				Spec: v1alpha.ClusterPolicySpec{
					LogLevel: 2,
					DynamicResourceAllocationSpec: v1alpha.DynamicResourceAllocationSpec{
						LogLevel:       3,
						PodHealthCheck: false,
					},
					HealthinessSpec: &v1alpha.HealthinessSpec{
						CheckIntervalSeconds:       67,
						CoreTemperatureThreshold:   42,
						MemoryTemperatureThreshold: 45,
					},
				},
			}
			controller := &DRAReconciler{}

			args := controller.generateArgs(cp)

			Expect(args).To(HaveLen(4))
			Expect(args).To(ContainElement("-v=3"))
			Expect(args).To(ContainElement("--health-monitoring=true"))
			Expect(args).To(ContainElement("--healthcheck-port=-1"))
			Expect(args).To(ContainElement("--manage-binding=false"))
		})

		It("with device taint", func() {
			cp := &v1alpha.ClusterPolicy{
				Spec: v1alpha.ClusterPolicySpec{
					LogLevel: 2,
					DynamicResourceAllocationSpec: v1alpha.DynamicResourceAllocationSpec{
						LogLevel:       3,
						PodHealthCheck: true,
						DeviceTaints:   true,
					},
					HealthinessSpec: &v1alpha.HealthinessSpec{
						CheckIntervalSeconds:       67,
						CoreTemperatureThreshold:   42,
						MemoryTemperatureThreshold: 45,
					},
				},
			}
			controller := &DRAReconciler{}

			args := controller.generateArgs(cp)

			Expect(args).To(HaveLen(5))
			Expect(args).To(ContainElement("-v=3"))
			Expect(args).To(ContainElement("--health-monitoring=true"))
			Expect(args).To(ContainElement("--healthcheck-port=51516"))
			Expect(args).To(ContainElement("--ignore-health-warning=false"))
			Expect(args).To(ContainElement("--manage-binding=false"))
		})

		It("with driver bind management", func() {
			cp := &v1alpha.ClusterPolicy{
				Spec: v1alpha.ClusterPolicySpec{
					LogLevel: 2,
					DynamicResourceAllocationSpec: v1alpha.DynamicResourceAllocationSpec{
						LogLevel:       3,
						PodHealthCheck: true,
						ManageBinding:  true,
					},
				},
			}
			controller := &DRAReconciler{}

			args := controller.generateArgs(cp)

			Expect(args).To(HaveLen(3))
			Expect(args).To(ContainElement("-v=3"))
			Expect(args).To(ContainElement("--healthcheck-port=51516"))
			Expect(args).To(ContainElement("--manage-binding=true"))
		})
	})
})
