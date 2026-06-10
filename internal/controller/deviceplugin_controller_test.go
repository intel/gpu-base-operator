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
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha "github.com/intel/gpu-base-operator/api/v1alpha1"
	"github.com/intel/gpu-base-operator/config/deployments"
)

var _ = Describe("ClusterPolicy Controller for Device Plugin", func() {

	Context("When reconciling Device Plugin and XPUM", func() {
		defaultNamespace := "foobar"
		const resourceName = "test-resource"

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
						ResourceRegistration: "dp",
						ResourceMonitoring:   true,
						UseNFDLabeling:       true,
						DevicePluginSpec: v1alpha.DevicePluginSpec{
							PluginImage: "intel/intel-gpu-plugin:0.32.0",
						},
						XpuManagerSpec: v1alpha.XpuManagerSpec{
							Image:              "intel/xpumanager:v1.2.27",
							MonitoringResource: "monitoring",
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
					Namespace: defaultNamespace,
					DRAEnable: true,
				},
			}

			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Checking that the DaemonSet has been created")
			dpDs := apps.DaemonSetList{}
			err = k8sClient.List(ctx, &dpDs, client.InNamespace(defaultNamespace))
			Expect(err).NotTo(HaveOccurred())

			Expect(dpDs.Items).To(HaveLen(2))
			Expect(dpDs.Items[0].Name).To(Equal("test-resource-device-plugin"))
			Expect(dpDs.Items[0].Spec.Template.Spec.Containers[0].Args).To(ContainElement("-enable-monitoring"))
			Expect(dpDs.Items[0].Spec.Template.Spec.NodeSelector).To(HaveKeyWithValue("intel.feature.node.kubernetes.io/gpu", "true"))
			Expect(dpDs.Items[0].Spec.Template.Spec.NodeSelector).To(HaveKeyWithValue("kubernetes.io/arch", "amd64"))
			Expect(dpDs.Items[1].Name).To(Equal("test-resource-xpu-manager"))
			Expect(dpDs.Items[1].Spec.Template.Spec.NodeSelector).To(HaveKeyWithValue("intel.feature.node.kubernetes.io/gpu", "true"))

			resource := &v1alpha.ClusterPolicy{}
			err = k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			resource.Spec.UseNFDLabeling = false
			resource.Spec.NodeSelector = map[string]string{"foo": "bar"}

			Expect(k8sClient.Update(ctx, resource)).To(Succeed())

			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			err = k8sClient.List(ctx, &dpDs, client.InNamespace(defaultNamespace))
			Expect(err).NotTo(HaveOccurred())

			Expect(dpDs.Items).To(HaveLen(2))
			Expect(dpDs.Items[0].Spec.Template.Spec.NodeSelector).To(HaveKeyWithValue("foo", "bar"))
			Expect(dpDs.Items[1].Spec.Template.Spec.NodeSelector).To(HaveKeyWithValue("foo", "bar"))

			err = k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			resource.Spec.ResourceMonitoring = false
			resource.Spec.DevicePluginSpec.LogLevel = 4

			Expect(k8sClient.Update(ctx, resource)).To(Succeed())

			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			err = k8sClient.List(ctx, &dpDs, client.InNamespace(defaultNamespace))
			Expect(err).NotTo(HaveOccurred())

			Expect(dpDs.Items).To(HaveLen(1))
			Expect(dpDs.Items[0].Name).To(Equal("test-resource-device-plugin"))
			Expect(dpDs.Items[0].Spec.Template.Spec.Containers[0].Args).To(ContainElement("-v=4"))
			Expect(dpDs.Items[0].Spec.Template.Spec.Containers[0].Args).NotTo(ContainElement("-enable-monitoring"))
		})
	})

	Context("When reconciling DP and XPUM", func() {
		defaultNamespace := "foobar-xpum"
		const resourceName = "test-resource-xpum"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name: resourceName,
		}
		clusterpolicy := &v1alpha.ClusterPolicy{
			ObjectMeta: metav1.ObjectMeta{
				Name: resourceName,
			},
			Spec: v1alpha.ClusterPolicySpec{
				ResourceRegistration: "dp",
				ResourceMonitoring:   true,
				DevicePluginSpec: v1alpha.DevicePluginSpec{
					PluginImage: "intel/intel-gpu-plugin:0.32.0",
				},
				XpuManagerSpec: v1alpha.XpuManagerSpec{
					Image:              "xpum-image:v1.2.3",
					LogLevel:           3,
					MonitoringResource: "monitoring",
				},
			},
		}

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
				Expect(k8sClient.Create(ctx, clusterpolicy)).To(Succeed())
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
				case "test-resource-xpum-device-plugin":
				case "test-resource-xpum-xpu-manager":
				default:
					Fail("Unexpected DaemonSet found: " + ds.Name)
				}
			}
		})
	})

	Context("When reconciling from DP to DRA", func() {
		defaultNamespace := "foobar-dp-to-dra"
		const resourceName = "test-resource-dp-to-dra"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name: resourceName,
		}
		clusterpolicy := &v1alpha.ClusterPolicy{
			ObjectMeta: metav1.ObjectMeta{
				Name: resourceName,
			},
			Spec: v1alpha.ClusterPolicySpec{
				ResourceRegistration: "dp",
				ResourceMonitoring:   false,
				DevicePluginSpec: v1alpha.DevicePluginSpec{
					PluginImage: "intel/intel-gpu-plugin:0.32.0",
				},
				DynamicResourceAllocationSpec: v1alpha.DynamicResourceAllocationSpec{
					Image: "intel/gpu-dra-driver:1.2.3",
				},
			},
		}

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
				Expect(k8sClient.Create(ctx, clusterpolicy)).To(Succeed())
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
				case "test-resource-dp-to-dra-device-plugin":
				default:
					Fail("Unexpected DaemonSet found: " + ds.Name)
				}
			}

			err = k8sClient.Get(ctx, typeNamespacedName, clusterpolicy)
			Expect(err).NotTo(HaveOccurred())

			clusterpolicy.Spec.ResourceRegistration = "dra"
			Expect(k8sClient.Update(ctx, clusterpolicy)).To(Succeed())

			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			err = k8sClient.List(ctx, &daemonSets, client.InNamespace(defaultNamespace))
			Expect(err).NotTo(HaveOccurred())

			Expect(daemonSets.Items).To(HaveLen(1))

			for _, ds := range daemonSets.Items {
				switch ds.Name {
				case "test-resource-dp-to-dra-gpu-dra":
				default:
					Fail("Unexpected DaemonSet found: " + ds.Name)
				}
			}
		})
	})

})

var _ = Describe("Device Plugin", func() {

	Context("Arguments creation", func() {
		It("with defaults", func() {
			spec := &v1alpha.ClusterPolicy{
				Spec: v1alpha.ClusterPolicySpec{
					DevicePluginSpec: v1alpha.DevicePluginSpec{},
				},
			}
			args := dpArgs(spec)
			Expect(args).To(BeEmpty())
		})

		It("Resource monitoring enabled", func() {
			spec := &v1alpha.ClusterPolicy{
				Spec: v1alpha.ClusterPolicySpec{
					ResourceMonitoring: true,
					DevicePluginSpec:   v1alpha.DevicePluginSpec{},
				},
			}
			args := dpArgs(spec)
			Expect(args).To(ContainElement("-enable-monitoring"))
			Expect(args).To(ContainElement("-xpumd-endpoint=/run/xpumd/intelxpuinfo.sock"))
		})

		It("Log level set", func() {
			spec := &v1alpha.ClusterPolicy{
				Spec: v1alpha.ClusterPolicySpec{
					LogLevel: 2,
					DevicePluginSpec: v1alpha.DevicePluginSpec{
						LogLevel: 3,
					},
				},
			}
			args := dpArgs(spec)
			Expect(args).To(ContainElement("-v=3"))
		})

		It("ByPath set", func() {
			spec := &v1alpha.ClusterPolicy{
				Spec: v1alpha.ClusterPolicySpec{
					DevicePluginSpec: v1alpha.DevicePluginSpec{
						ByPathMode: "all",
					},
				},
			}
			args := dpArgs(spec)
			Expect(args).To(ContainElement("-bypath=all"))
		})

		It("Allow IDs set", func() {
			spec := &v1alpha.ClusterPolicy{
				Spec: v1alpha.ClusterPolicySpec{
					DevicePluginSpec: v1alpha.DevicePluginSpec{
						AllowIDs: []string{"0xabcd", "0xdefa"},
					},
				},
			}
			args := dpArgs(spec)
			Expect(args).To(ContainElement("-allow-ids=0xabcd,0xdefa"))
		})

		It("Deny IDs set", func() {
			spec := &v1alpha.ClusterPolicy{
				Spec: v1alpha.ClusterPolicySpec{
					DevicePluginSpec: v1alpha.DevicePluginSpec{
						DenyIDs: []string{"0x0xyz", "0x0123"},
					},
				},
			}
			args := dpArgs(spec)
			Expect(args).To(ContainElement("-deny-ids=0x0xyz,0x0123"))
		})

		It("All options set", func() {
			spec := &v1alpha.ClusterPolicy{
				Spec: v1alpha.ClusterPolicySpec{
					ResourceMonitoring: true,
					LogLevel:           1,
					DevicePluginSpec: v1alpha.DevicePluginSpec{
						LogLevel: 3,
						AllowIDs: []string{"0x1id1"},
						DenyIDs:  []string{"0x2id2"},
					},
				},
			}
			args := dpArgs(spec)
			Expect(args).To(HaveLen(5))
			Expect(args).To(ContainElement("-enable-monitoring"))
			Expect(args).To(ContainElement("-v=3"))
			Expect(args).To(ContainElement("-xpumd-endpoint=/run/xpumd/intelxpuinfo.sock"))
			Expect(args).To(ContainElement("-allow-ids=0x1id1"))
			Expect(args).To(ContainElement("-deny-ids=0x2id2"))
		})
	})

	Context("DaemonSet object update", func() {
		It("with tolerations and pullsecret", func() {
			cp := &v1alpha.ClusterPolicy{
				Spec: v1alpha.ClusterPolicySpec{
					LogLevel: 2,
					DevicePluginSpec: v1alpha.DevicePluginSpec{
						LogLevel: 3,
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
				},
			}
			controller := &DevicePluginReconciler{}

			ds := deployments.DevicePluginDaemonset()
			controller.updateDaemonSetObject(ds, cp)

			Expect(ds.Spec.Template.Spec.Tolerations[0].Key).To(Equal("my-toleration"))
			Expect(ds.Spec.Template.Spec.Tolerations[0].Operator).To(Equal(v1.TolerationOpExists))
			Expect(ds.Spec.Template.Spec.Tolerations[0].Effect).To(Equal(v1.TaintEffectNoExecute))

			Expect(ds.Spec.Template.Spec.ImagePullSecrets[0].Name).To(Equal("my-pull-secret"))

			cp.Spec.PullSecret = nil
			cp.Spec.Tolerations = nil
			controller.updateDaemonSetObject(ds, cp)

			Expect(ds.Spec.Template.Spec.Tolerations).To(BeEmpty())
			Expect(ds.Spec.Template.Spec.ImagePullSecrets).To(BeEmpty())
		})

		It("preserves pod spec and container security context from YAML", func() {
			cp := &v1alpha.ClusterPolicy{
				Spec: v1alpha.ClusterPolicySpec{
					DevicePluginSpec: v1alpha.DevicePluginSpec{
						PluginImage: "intel/intel-gpu-plugin:test",
					},
				},
			}
			controller := &DevicePluginReconciler{}

			ds := deployments.DevicePluginDaemonset()
			controller.updateDaemonSetObject(ds, cp)

			By("automountServiceAccountToken is false")
			Expect(ds.Spec.Template.Spec.AutomountServiceAccountToken).NotTo(BeNil())
			Expect(*ds.Spec.Template.Spec.AutomountServiceAccountToken).To(BeFalse())
		})

		It("adds runxpumd volume and mount when ResourceMonitoring is true", func() {
			cp := &v1alpha.ClusterPolicy{
				Spec: v1alpha.ClusterPolicySpec{
					ResourceMonitoring: true,
					DevicePluginSpec: v1alpha.DevicePluginSpec{
						PluginImage: "intel/intel-gpu-plugin:test",
					},
				},
			}
			controller := &DevicePluginReconciler{}

			ds := deployments.DevicePluginDaemonset()
			controller.updateDaemonSetObject(ds, cp)

			volNames := []string{}
			for _, v := range ds.Spec.Template.Spec.Volumes {
				volNames = append(volNames, v.Name)
			}
			Expect(volNames).To(ContainElement(xpumdVolumeName))

			mountNames := []string{}
			for _, vm := range ds.Spec.Template.Spec.Containers[0].VolumeMounts {
				mountNames = append(mountNames, vm.Name)
			}
			Expect(mountNames).To(ContainElement(xpumdVolumeName))
		})

		It("does not duplicate runxpumd volume when updateDaemonSetObject is called twice", func() {
			cp := &v1alpha.ClusterPolicy{
				Spec: v1alpha.ClusterPolicySpec{
					ResourceMonitoring: true,
					DevicePluginSpec: v1alpha.DevicePluginSpec{
						PluginImage: "intel/intel-gpu-plugin:test",
					},
				},
			}
			controller := &DevicePluginReconciler{}

			ds := deployments.DevicePluginDaemonset()
			controller.updateDaemonSetObject(ds, cp)
			controller.updateDaemonSetObject(ds, cp)

			count := 0
			for _, v := range ds.Spec.Template.Spec.Volumes {
				if v.Name == xpumdVolumeName {
					count++
				}
			}
			Expect(count).To(Equal(1))
		})

		It("removes runxpumd volume and mount when ResourceMonitoring is disabled", func() {
			cp := &v1alpha.ClusterPolicy{
				Spec: v1alpha.ClusterPolicySpec{
					ResourceMonitoring: true,
					DevicePluginSpec: v1alpha.DevicePluginSpec{
						PluginImage: "intel/intel-gpu-plugin:test",
					},
				},
			}
			controller := &DevicePluginReconciler{}

			ds := deployments.DevicePluginDaemonset()
			controller.updateDaemonSetObject(ds, cp)

			cp.Spec.ResourceMonitoring = false
			controller.updateDaemonSetObject(ds, cp)

			for _, v := range ds.Spec.Template.Spec.Volumes {
				Expect(v.Name).NotTo(Equal(xpumdVolumeName))
			}
			for _, vm := range ds.Spec.Template.Spec.Containers[0].VolumeMounts {
				Expect(vm.Name).NotTo(Equal(xpumdVolumeName))
			}
		})

		It("removeXpumdMounts is a no-op when runxpumd is not present", func() {
			cp := &v1alpha.ClusterPolicy{
				Spec: v1alpha.ClusterPolicySpec{
					ResourceMonitoring: false,
					DevicePluginSpec: v1alpha.DevicePluginSpec{
						PluginImage: "intel/intel-gpu-plugin:test",
					},
				},
			}
			controller := &DevicePluginReconciler{}

			ds := deployments.DevicePluginDaemonset()
			initialVolCount := len(ds.Spec.Template.Spec.Volumes)
			initialMountCount := len(ds.Spec.Template.Spec.Containers[0].VolumeMounts)

			Expect(func() { controller.updateDaemonSetObject(ds, cp) }).NotTo(Panic())
			Expect(ds.Spec.Template.Spec.Volumes).To(HaveLen(initialVolCount))
			Expect(ds.Spec.Template.Spec.Containers[0].VolumeMounts).To(HaveLen(initialMountCount))
		})
	})
})
