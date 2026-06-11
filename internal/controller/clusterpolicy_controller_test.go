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
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apps "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/yaml"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha "github.com/intel/gpu-base-operator/api/v1alpha1"
	"github.com/intel/gpu-base-operator/config/deployments"
	nfdcrd "sigs.k8s.io/node-feature-discovery/api/nfd/v1alpha1"
)

var _ = Describe("ClusterPolicy Controller", func() {

	Context("When reconciling dra and xpum", func() {
		defaultNamespace := "foobar-dra-xpum-xpum"
		const resourceName = "test-resource-dra-xpum-xpum"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name: resourceName,
		}
		clusterpolicy := &v1alpha.ClusterPolicy{
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
					Image:    "xpum-image:v1.2.3",
					LogLevel: 3,
				},
			},
		}

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

			ret, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(ret.RequeueAfter).To(BeZero())

			// Re-reconcile to make sure that no-op reconciliation doesn't fail.
			ret, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(ret.RequeueAfter).To(BeZero())

			daemonSets := apps.DaemonSetList{}
			err = k8sClient.List(ctx, &daemonSets, client.InNamespace(defaultNamespace))
			Expect(err).NotTo(HaveOccurred())

			Expect(daemonSets.Items).To(HaveLen(2))

			for _, ds := range daemonSets.Items {
				switch ds.Name {
				case "test-resource-dra-xpum-xpum-gpu-dra":
					Expect(ds.Spec.Template.Spec.Containers[0].Image).To(Equal("dra-image:v1.2.3"))
				case "test-resource-dra-xpum-xpum-xpu-manager":
					Expect(ds.Spec.Template.Spec.Containers[0].Image).To(Equal("xpum-image:v1.2.3"))
				default:
					Fail("Unexpected DaemonSet found: " + ds.Name)
				}
			}

			nfr := nfdcrd.NodeFeatureRule{}
			err = k8sClient.Get(ctx, types.NamespacedName{Name: "intel-gpu-devices"}, &nfr)
			Expect(err).NotTo(HaveOccurred())

			err = k8sClient.Get(ctx, typeNamespacedName, clusterpolicy)
			Expect(err).NotTo(HaveOccurred())

			err = k8sClient.Update(ctx, clusterpolicy)
			Expect(err).NotTo(HaveOccurred())

			ret, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(ret.RequeueAfter).To(BeZero())

			err = k8sClient.List(ctx, &daemonSets, client.InNamespace(defaultNamespace))
			Expect(err).NotTo(HaveOccurred())

			Expect(daemonSets.Items).To(HaveLen(2))

			configmaps := v1.ConfigMapList{}
			err = k8sClient.List(ctx, &configmaps, client.InNamespace(defaultNamespace))
			Expect(err).NotTo(HaveOccurred())

			Expect(configmaps.Items).To(HaveLen(1))
			Expect(configmaps.Items[0].Name).To(Equal("test-resource-dra-xpum-xpum-xpumanager-otel-config"))

			clusterpolicy.Spec.HealthinessSpec = &v1alpha.HealthinessSpec{
				CheckIntervalSeconds: 17,
			}
			Expect(k8sClient.Update(ctx, clusterpolicy)).To(Succeed())

			ret, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(ret.RequeueAfter).To(BeZero())

			// Create XPUM config override

			xpumConfig := deployments.XpuManagerOTelConfig()
			data, err := yaml.Marshal(xpumConfig)
			Expect(err).NotTo(HaveOccurred())

			xpumCm := v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-xpum-cm",
					Namespace: defaultNamespace,
				},
				Data: map[string]string{
					"config.yaml": string(data),
				},
			}
			Expect(k8sClient.Create(ctx, &xpumCm)).To(Succeed())

			clusterpolicy.Spec.XpuManagerSpec.ConfigMapOverride = "test-xpum-cm"
			Expect(k8sClient.Update(ctx, clusterpolicy)).To(Succeed())

			ret, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(ret.RequeueAfter).To(BeZero())

			err = k8sClient.List(ctx, &daemonSets, client.InNamespace(defaultNamespace))
			Expect(err).NotTo(HaveOccurred())

			Expect(daemonSets.Items).To(HaveLen(2))

			for _, ds := range daemonSets.Items {
				switch ds.Name {
				case "test-resource-dra-xpum-xpum-gpu-dra":
				case "test-resource-dra-xpum-xpum-xpu-manager":
					checked := false
					for _, v := range ds.Spec.Template.Spec.Volumes {
						if v.Name == "config" {
							Expect(v.VolumeSource.ConfigMap).NotTo(BeNil())
							Expect(v.VolumeSource.ConfigMap.Name).To(Equal("test-xpum-cm"))
							checked = true
						}
					}
					Expect(checked).To(BeTrue())
				default:
					Fail("Unexpected DaemonSet found: " + ds.Name)
				}
			}

			err = k8sClient.Delete(ctx, clusterpolicy)
			Expect(err).NotTo(HaveOccurred())

			// With the finalizer in place the ClusterPolicy is not immediately removed —
			// it receives a DeletionTimestamp and waits for the controller to finish cleanup.
			err = k8sClient.Get(ctx, typeNamespacedName, clusterpolicy)
			Expect(err).NotTo(HaveOccurred())
			Expect(clusterpolicy.DeletionTimestamp).NotTo(BeNil())

			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			// After the reconcile the finalizer has been removed and the CR is gone.
			err = k8sClient.Get(ctx, typeNamespacedName, clusterpolicy)
			Expect(errors.IsNotFound(err)).To(BeTrue())

			err = k8sClient.List(ctx, &daemonSets, client.InNamespace(defaultNamespace))
			Expect(err).NotTo(HaveOccurred())
			// Both DaemonSets (DRA and XPUM) are explicitly deleted by the reconciler
			// during the finalizer cleanup — we no longer rely on the K8s GC.
			Expect(daemonSets.Items).To(BeEmpty())
		})
	})
})

var _ = Describe("draPodReadinessPredicate", func() {
	var pred = draPodReadinessPredicate()

	draLabels := map[string]string{appLabel: draValue}

	draReadyPod := func(ready bool) *v1.Pod {
		status := v1.ConditionFalse
		if ready {
			status = v1.ConditionTrue
		}
		return &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Labels: draLabels},
			Status: v1.PodStatus{
				Conditions: []v1.PodCondition{
					{Type: v1.PodReady, Status: status},
				},
			},
		}
	}

	Context("CreateFunc", func() {
		It("passes DRA pods", func() {
			Expect(pred.Create(event.CreateEvent{Object: draReadyPod(false)})).To(BeTrue())
		})

		It("filters out non-DRA pods", func() {
			pod := &v1.Pod{}
			Expect(pred.Create(event.CreateEvent{Object: pod})).To(BeFalse())
		})
	})

	Context("DeleteFunc", func() {
		It("passes DRA pods", func() {
			Expect(pred.Delete(event.DeleteEvent{Object: draReadyPod(true)})).To(BeTrue())
		})

		It("filters out non-DRA pods", func() {
			Expect(pred.Delete(event.DeleteEvent{Object: &v1.Pod{}})).To(BeFalse())
		})
	})

	Context("GenericFunc", func() {
		It("always filters events", func() {
			Expect(pred.Generic(event.GenericEvent{Object: draReadyPod(true)})).To(BeFalse())
		})
	})

	Context("UpdateFunc", func() {
		It("passes when DRA pod Ready condition changes from false to true", func() {
			e := event.UpdateEvent{ObjectOld: draReadyPod(false), ObjectNew: draReadyPod(true)}
			Expect(pred.Update(e)).To(BeTrue())
		})

		It("passes when DRA pod Ready condition changes from true to false", func() {
			e := event.UpdateEvent{ObjectOld: draReadyPod(true), ObjectNew: draReadyPod(false)}
			Expect(pred.Update(e)).To(BeTrue())
		})

		It("filters when DRA pod Ready condition is unchanged (both ready)", func() {
			e := event.UpdateEvent{ObjectOld: draReadyPod(true), ObjectNew: draReadyPod(true)}
			Expect(pred.Update(e)).To(BeFalse())
		})

		It("filters when DRA pod Ready condition is unchanged (both not ready)", func() {
			e := event.UpdateEvent{ObjectOld: draReadyPod(false), ObjectNew: draReadyPod(false)}
			Expect(pred.Update(e)).To(BeFalse())
		})

		It("filters non-DRA pods", func() {
			nonDRA := &v1.Pod{}
			e := event.UpdateEvent{ObjectOld: nonDRA, ObjectNew: nonDRA}
			Expect(pred.Update(e)).To(BeFalse())
		})

		It("filters when objects are not pods", func() {
			notAPod := &v1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Labels: draLabels}}
			e := event.UpdateEvent{ObjectOld: notAPod, ObjectNew: notAPod}
			Expect(pred.Update(e)).To(BeFalse())
		})
	})
})

var _ = Describe("draPodToClusterPolicy", func() {
	newScheme := func() *runtime.Scheme {
		s := runtime.NewScheme()
		Expect(v1alpha.AddToScheme(s)).To(Succeed())
		return s
	}

	It("returns one request per existing ClusterPolicy", func() {
		cp1 := &v1alpha.ClusterPolicy{ObjectMeta: metav1.ObjectMeta{Name: "policy-one"}}
		cp2 := &v1alpha.ClusterPolicy{ObjectMeta: metav1.ObjectMeta{Name: "policy-two"}}

		r := &ClusterPolicyReconciler{
			Client: fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(cp1, cp2).Build(),
		}

		reqs := r.draPodToClusterPolicy(context.Background(), nil)
		Expect(reqs).To(HaveLen(2))

		names := []string{reqs[0].Name, reqs[1].Name}
		Expect(names).To(ConsistOf("policy-one", "policy-two"))
	})

	It("returns an empty slice when no ClusterPolicies exist", func() {
		r := &ClusterPolicyReconciler{
			Client: fake.NewClientBuilder().WithScheme(newScheme()).Build(),
		}

		reqs := r.draPodToClusterPolicy(context.Background(), nil)
		Expect(reqs).To(BeEmpty())
	})

	It("returns nil when the List call fails", func() {
		r := &ClusterPolicyReconciler{
			Client: fake.NewClientBuilder().WithScheme(newScheme()).WithInterceptorFuncs(interceptor.Funcs{
				List: func(_ context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
					return fmt.Errorf("list error")
				},
			}).Build(),
		}

		reqs := r.draPodToClusterPolicy(context.Background(), nil)
		Expect(reqs).To(BeNil())
	})
})
