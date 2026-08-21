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
	stderrors "errors"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha "github.com/intel/gpu-base-operator/api/v1alpha1"
	kmmv1beta1 "github.com/kubernetes-sigs/kernel-module-management/api/v1beta1"
)

var _ = Describe("KMM Controller", func() {
	Context("When creating a Module with KernelModule spec", func() {
		const (
			namespace    = "kmm-oot-create"
			resourceName = "kmm-oot-create"
		)

		ctx := context.Background()
		typeNamespacedName := types.NamespacedName{Name: resourceName}

		BeforeEach(func() {
			Expect(k8sClient.Create(ctx, &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: namespace},
			})).To(Succeed())
		})

		AfterEach(func() {
			resource := &v1alpha.ClusterPolicy{}
			if err := k8sClient.Get(ctx, typeNamespacedName, resource); err == nil {
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			}
		})

		It("should create a KMM Module CR with moduleLoader", func() {
			cp := &v1alpha.ClusterPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName},
				Spec: v1alpha.ClusterPolicySpec{
					ResourceRegistration: "dra",
					KernelModule: &v1alpha.KernelModuleSpec{
						ModuleName: "xe",
						KernelMappings: []v1alpha.KernelMappingSpec{
							{Regexp: "^.+$", ContainerImage: "registry.example.com/xe-driver:1.0"},
						},
					},
					DynamicResourceAllocationSpec: v1alpha.DynamicResourceAllocationSpec{
						Image: "ghcr.io/intel/gpu-dra:v0.11.0",
					},
				},
			}
			Expect(k8sClient.Create(ctx, cp)).To(Succeed())

			reconciler := &ClusterPolicyReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Opts: ControllerOpts{
					Namespace:                      namespace,
					KMMEnable:                      true,
					ModuleLoaderServiceAccountName: "intel-gpu-module-loader",
				},
			}

			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("verifying KMM Module was created")
			mod := &kmmv1beta1.Module{}
			modKey := types.NamespacedName{Name: resourceName + kmmModuleSuffix, Namespace: namespace}
			Expect(k8sClient.Get(ctx, modKey, mod)).To(Succeed())

			By("verifying ModuleLoader is set with correct modprobe config")
			Expect(mod.Spec.ModuleLoader).NotTo(BeNil())
			Expect(mod.Spec.ModuleLoader.Container.Modprobe.ModuleName).To(Equal("xe"))
			Expect(mod.Spec.ModuleLoader.Container.ContainerImage).To(BeEmpty())
			Expect(mod.Spec.ModuleLoader.Container.InTreeModulesToRemove).To(Equal([]string{"xe"}))
			Expect(mod.Spec.ModuleLoader.Container.KernelMappings).To(HaveLen(1))
			Expect(mod.Spec.ModuleLoader.Container.KernelMappings[0].Regexp).To(Equal("^.+$"))
			Expect(mod.Spec.ModuleLoader.Container.KernelMappings[0].ContainerImage).To(Equal("registry.example.com/xe-driver:1.0"))

			By("verifying no DRA or DevicePlugin specs are set")
			Expect(mod.Spec.DRA).To(BeNil())
			Expect(mod.Spec.DevicePlugin).To(BeNil())

			By("verifying owner reference is set")
			Expect(mod.OwnerReferences).To(HaveLen(1))
			Expect(mod.OwnerReferences[0].Name).To(Equal(resourceName))
		})
	})

	Context("When KernelModule is nil", func() {
		const (
			namespace    = "kmm-no-km"
			resourceName = "kmm-no-km"
		)

		ctx := context.Background()
		typeNamespacedName := types.NamespacedName{Name: resourceName}

		BeforeEach(func() {
			Expect(k8sClient.Create(ctx, &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: namespace},
			})).To(Succeed())
		})

		AfterEach(func() {
			resource := &v1alpha.ClusterPolicy{}
			if err := k8sClient.Get(ctx, typeNamespacedName, resource); err == nil {
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			}
		})

		It("should not create a Module CR", func() {
			cp := &v1alpha.ClusterPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName},
				Spec: v1alpha.ClusterPolicySpec{
					ResourceRegistration: "dra",
					DynamicResourceAllocationSpec: v1alpha.DynamicResourceAllocationSpec{
						Image: "ghcr.io/intel/gpu-dra:v0.11.0",
					},
				},
			}
			Expect(k8sClient.Create(ctx, cp)).To(Succeed())

			reconciler := &ClusterPolicyReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Opts: ControllerOpts{
					Namespace:                      namespace,
					KMMEnable:                      true,
					ModuleLoaderServiceAccountName: "intel-gpu-module-loader",
				},
			}

			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			mod := &kmmv1beta1.Module{}
			modKey := types.NamespacedName{Name: resourceName + kmmModuleSuffix, Namespace: namespace}
			err = k8sClient.Get(ctx, modKey, mod)
			Expect(errors.IsNotFound(err)).To(BeTrue())
		})
	})

	Context("When KMM is not enabled", func() {
		const (
			namespace    = "kmm-not-enabled"
			resourceName = "kmm-not-enabled"
		)

		ctx := context.Background()
		typeNamespacedName := types.NamespacedName{Name: resourceName}

		BeforeEach(func() {
			Expect(k8sClient.Create(ctx, &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: namespace},
			})).To(Succeed())
		})

		AfterEach(func() {
			resource := &v1alpha.ClusterPolicy{}
			if err := k8sClient.Get(ctx, typeNamespacedName, resource); err == nil {
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			}
		})

		It("should set kmmNotEnabledMsg error and not create a Module", func() {
			cp := &v1alpha.ClusterPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName},
				Spec: v1alpha.ClusterPolicySpec{
					ResourceRegistration: "dra",
					KernelModule: &v1alpha.KernelModuleSpec{
						ModuleName: "xe",
						KernelMappings: []v1alpha.KernelMappingSpec{
							{Regexp: "^.+$", ContainerImage: "registry.example.com/xe-driver:1.0"},
						},
					},
					DynamicResourceAllocationSpec: v1alpha.DynamicResourceAllocationSpec{
						Image: "ghcr.io/intel/gpu-dra:v0.11.0",
					},
				},
			}
			Expect(k8sClient.Create(ctx, cp)).To(Succeed())

			reconciler := &ClusterPolicyReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Opts: ControllerOpts{
					Namespace:                      namespace,
					KMMEnable:                      false,
					ModuleLoaderServiceAccountName: "intel-gpu-module-loader",
				},
			}

			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("verifying status has kmmNotEnabledMsg")
			Expect(k8sClient.Get(ctx, typeNamespacedName, cp)).To(Succeed())
			Expect(cp.Status.Errors).To(ContainElement(kmmNotEnabledMsg))

			By("verifying no Module was created")
			mod := &kmmv1beta1.Module{}
			modKey := types.NamespacedName{Name: resourceName + kmmModuleSuffix, Namespace: namespace}
			err = k8sClient.Get(ctx, modKey, mod)
			Expect(errors.IsNotFound(err)).To(BeTrue())
		})
	})

	Context("When a status error condition resolves", func() {
		const (
			namespace    = "kmm-clear-err"
			resourceName = "kmm-clear-err"
		)

		ctx := context.Background()
		typeNamespacedName := types.NamespacedName{Name: resourceName}

		BeforeEach(func() {
			Expect(k8sClient.Create(ctx, &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: namespace},
			})).To(Succeed())
		})

		AfterEach(func() {
			resource := &v1alpha.ClusterPolicy{}
			if err := k8sClient.Get(ctx, typeNamespacedName, resource); err == nil {
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			}
		})

		It("clears a stale error on the next reconcile once the condition is gone", func() {
			cp := &v1alpha.ClusterPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName},
				Spec: v1alpha.ClusterPolicySpec{
					ResourceRegistration: "dra",
					KernelModule: &v1alpha.KernelModuleSpec{
						ModuleName: "xe",
						KernelMappings: []v1alpha.KernelMappingSpec{
							{Regexp: "^.+$", ContainerImage: "registry.example.com/xe-driver:1.0"},
						},
					},
					DynamicResourceAllocationSpec: v1alpha.DynamicResourceAllocationSpec{
						Image: "ghcr.io/intel/gpu-dra:v0.11.0",
					},
				},
			}
			Expect(k8sClient.Create(ctx, cp)).To(Succeed())

			// KMM disabled + KernelModule set => kmmNotEnabledMsg is recorded.
			reconciler := &ClusterPolicyReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Opts: ControllerOpts{
					Namespace:                      namespace,
					KMMEnable:                      false,
					ModuleLoaderServiceAccountName: "intel-gpu-module-loader",
				},
			}

			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("verifying the error is present")
			Expect(k8sClient.Get(ctx, typeNamespacedName, cp)).To(Succeed())
			Expect(cp.Status.Errors).To(ContainElement(kmmNotEnabledMsg))

			By("removing KernelModule so the condition no longer applies")
			cp.Spec.KernelModule = nil
			Expect(k8sClient.Update(ctx, cp)).To(Succeed())

			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("verifying the stale error was cleared")
			Expect(k8sClient.Get(ctx, typeNamespacedName, cp)).To(Succeed())
			Expect(cp.Status.Errors).NotTo(ContainElement(kmmNotEnabledMsg))
		})
	})

	Context("When KernelModule is removed from ClusterPolicy", func() {
		const (
			namespace    = "kmm-remove-km"
			resourceName = "kmm-remove-km"
		)

		ctx := context.Background()
		typeNamespacedName := types.NamespacedName{Name: resourceName}

		BeforeEach(func() {
			Expect(k8sClient.Create(ctx, &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: namespace},
			})).To(Succeed())
		})

		AfterEach(func() {
			resource := &v1alpha.ClusterPolicy{}
			if err := k8sClient.Get(ctx, typeNamespacedName, resource); err == nil {
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			}
		})

		It("should delete the Module when KernelModule is nilled out", func() {
			cp := &v1alpha.ClusterPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName},
				Spec: v1alpha.ClusterPolicySpec{
					ResourceRegistration: "dra",
					KernelModule: &v1alpha.KernelModuleSpec{
						ModuleName: "xe",
						KernelMappings: []v1alpha.KernelMappingSpec{
							{Regexp: "^.+$", ContainerImage: "registry.example.com/xe-driver:1.0"},
						},
					},
					DynamicResourceAllocationSpec: v1alpha.DynamicResourceAllocationSpec{
						Image: "ghcr.io/intel/gpu-dra:v0.11.0",
					},
				},
			}
			Expect(k8sClient.Create(ctx, cp)).To(Succeed())

			reconciler := &ClusterPolicyReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Opts: ControllerOpts{
					Namespace:                      namespace,
					KMMEnable:                      true,
					ModuleLoaderServiceAccountName: "intel-gpu-module-loader",
				},
			}

			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			modKey := types.NamespacedName{Name: resourceName + kmmModuleSuffix, Namespace: namespace}
			Expect(k8sClient.Get(ctx, modKey, &kmmv1beta1.Module{})).To(Succeed())

			By("removing KernelModule from ClusterPolicy")
			Expect(k8sClient.Get(ctx, typeNamespacedName, cp)).To(Succeed())
			cp.Spec.KernelModule = nil
			Expect(k8sClient.Update(ctx, cp)).To(Succeed())

			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("verifying Module was deleted")
			err = k8sClient.Get(ctx, modKey, &kmmv1beta1.Module{})
			Expect(errors.IsNotFound(err)).To(BeTrue())
		})
	})

	Context("When ClusterPolicy is deleted", func() {
		const (
			namespace    = "kmm-del-cp"
			resourceName = "kmm-del-cp"
		)

		ctx := context.Background()
		typeNamespacedName := types.NamespacedName{Name: resourceName}

		BeforeEach(func() {
			Expect(k8sClient.Create(ctx, &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: namespace},
			})).To(Succeed())
		})

		AfterEach(func() {
			resource := &v1alpha.ClusterPolicy{}
			if err := k8sClient.Get(ctx, typeNamespacedName, resource); err == nil {
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			}
		})

		It("should delete Module when ClusterPolicy is deleted", func() {
			cp := &v1alpha.ClusterPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName},
				Spec: v1alpha.ClusterPolicySpec{
					ResourceRegistration: "dra",
					KernelModule: &v1alpha.KernelModuleSpec{
						ModuleName: "xe",
						KernelMappings: []v1alpha.KernelMappingSpec{
							{Regexp: "^.+$", ContainerImage: "registry.example.com/xe-driver:1.0"},
						},
					},
					DynamicResourceAllocationSpec: v1alpha.DynamicResourceAllocationSpec{
						Image: "ghcr.io/intel/gpu-dra:v0.11.0",
					},
				},
			}
			Expect(k8sClient.Create(ctx, cp)).To(Succeed())

			reconciler := &ClusterPolicyReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Opts: ControllerOpts{
					Namespace:                      namespace,
					KMMEnable:                      true,
					ModuleLoaderServiceAccountName: "intel-gpu-module-loader",
				},
			}

			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			modKey := types.NamespacedName{Name: resourceName + kmmModuleSuffix, Namespace: namespace}
			Expect(k8sClient.Get(ctx, modKey, &kmmv1beta1.Module{})).To(Succeed())

			By("deleting the ClusterPolicy")
			Expect(k8sClient.Delete(ctx, cp)).To(Succeed())

			By("reconciling the deletion via KMM reconciler directly")
			kmmReconciler := &KMMReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Opts: ControllerOpts{
					ReqName:   resourceName,
					Namespace: namespace,
					KMMEnable: true,
				},
			}
			_, err = kmmReconciler.Reconcile(ctx, nil)
			Expect(err).NotTo(HaveOccurred())

			By("verifying Module was deleted")
			err = k8sClient.Get(ctx, modKey, &kmmv1beta1.Module{})
			Expect(errors.IsNotFound(err)).To(BeTrue())
		})
	})

	Context("When ClusterPolicy has tolerations and pull secret", func() {
		const (
			namespace    = "kmm-opts-create"
			resourceName = "kmm-opts-create"
		)

		ctx := context.Background()
		typeNamespacedName := types.NamespacedName{Name: resourceName}

		BeforeEach(func() {
			Expect(k8sClient.Create(ctx, &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: namespace},
			})).To(Succeed())
		})

		AfterEach(func() {
			resource := &v1alpha.ClusterPolicy{}
			if err := k8sClient.Get(ctx, typeNamespacedName, resource); err == nil {
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			}
		})

		It("should propagate tolerations and pull secret to Module", func() {
			cp := &v1alpha.ClusterPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName},
				Spec: v1alpha.ClusterPolicySpec{
					ResourceRegistration: "dra",
					KernelModule: &v1alpha.KernelModuleSpec{
						ModuleName: "xe",
						KernelMappings: []v1alpha.KernelMappingSpec{
							{Regexp: "^.+$", ContainerImage: "registry.example.com/xe-driver:1.0"},
						},
					},
					DynamicResourceAllocationSpec: v1alpha.DynamicResourceAllocationSpec{
						Image: "ghcr.io/intel/gpu-dra:v0.11.0",
					},
					Tolerations: []v1.Toleration{
						{
							Key:      "gpu-dedicated",
							Operator: v1.TolerationOpExists,
							Effect:   v1.TaintEffectNoSchedule,
						},
					},
					PullSecret: &v1.LocalObjectReference{
						Name: "my-registry-secret",
					},
				},
			}
			Expect(k8sClient.Create(ctx, cp)).To(Succeed())

			reconciler := &ClusterPolicyReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Opts: ControllerOpts{
					Namespace:                      namespace,
					KMMEnable:                      true,
					ModuleLoaderServiceAccountName: "intel-gpu-module-loader",
				},
			}

			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			mod := &kmmv1beta1.Module{}
			modKey := types.NamespacedName{Name: resourceName + kmmModuleSuffix, Namespace: namespace}
			Expect(k8sClient.Get(ctx, modKey, mod)).To(Succeed())

			By("verifying tolerations")
			Expect(mod.Spec.Tolerations).To(HaveLen(1))
			Expect(mod.Spec.Tolerations[0].Key).To(Equal("gpu-dedicated"))

			By("verifying pull secret")
			Expect(mod.Spec.ImageRepoSecret).NotTo(BeNil())
			Expect(mod.Spec.ImageRepoSecret.Name).To(Equal("my-registry-secret"))
		})
	})

	DescribeTable("expanded KernelModule fields", func(
		ns string,
		cpSpec v1alpha.ClusterPolicySpec,
		assertFn func(mod *kmmv1beta1.Module, cp *v1alpha.ClusterPolicy),
	) {
		ctx := context.Background()
		resName := ns

		Expect(k8sClient.Create(ctx, &v1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		})).To(Succeed())

		cp := &v1alpha.ClusterPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: resName},
			Spec:       cpSpec,
		}
		Expect(k8sClient.Create(ctx, cp)).To(Succeed())

		reconciler := &ClusterPolicyReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			Opts: ControllerOpts{
				Namespace:                      ns,
				KMMEnable:                      true,
				ModuleLoaderServiceAccountName: "intel-gpu-module-loader",
			},
		}
		_, err := reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: resName},
		})
		Expect(err).NotTo(HaveOccurred())

		mod := &kmmv1beta1.Module{}
		modKey := types.NamespacedName{Name: resName + kmmModuleSuffix, Namespace: ns}
		Expect(k8sClient.Get(ctx, modKey, mod)).To(Succeed())

		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: resName}, cp)).To(Succeed())

		assertFn(mod, cp)
	},
		Entry("should configure ModuleLoader with multiple kernel mappings",
			"kmm-exp-multi",
			v1alpha.ClusterPolicySpec{
				ResourceRegistration: "dra",
				KernelModule: &v1alpha.KernelModuleSpec{
					ModuleName: "xe",
					KernelMappings: []v1alpha.KernelMappingSpec{
						{Regexp: "^5\\.14\\.0-.*\\.el9", ContainerImage: "registry.example.com/xe-rhel9:1.0"},
						{Regexp: "^6\\.12\\..*", ContainerImage: "registry.example.com/xe-rhel10:1.0"},
					},
				},
				DynamicResourceAllocationSpec: v1alpha.DynamicResourceAllocationSpec{
					Image: "ghcr.io/intel/gpu-dra:v0.11.0",
				},
			},
			func(mod *kmmv1beta1.Module, _ *v1alpha.ClusterPolicy) {
				Expect(mod.Spec.ModuleLoader.Container.KernelMappings).To(HaveLen(2))
				Expect(mod.Spec.ModuleLoader.Container.KernelMappings[0].Regexp).To(Equal("^5\\.14\\.0-.*\\.el9"))
				Expect(mod.Spec.ModuleLoader.Container.KernelMappings[0].ContainerImage).To(Equal("registry.example.com/xe-rhel9:1.0"))
				Expect(mod.Spec.ModuleLoader.Container.KernelMappings[1].Regexp).To(Equal("^6\\.12\\..*"))
				Expect(mod.Spec.ModuleLoader.Container.KernelMappings[1].ContainerImage).To(Equal("registry.example.com/xe-rhel10:1.0"))
				Expect(mod.Spec.ModuleLoader.Container.ContainerImage).To(BeEmpty())
			},
		),
		Entry("should set Version on container spec",
			"kmm-exp-version",
			v1alpha.ClusterPolicySpec{
				ResourceRegistration: "dra",
				KernelModule: &v1alpha.KernelModuleSpec{
					ModuleName: "xe",
					Version:    "2.0",
					KernelMappings: []v1alpha.KernelMappingSpec{
						{Regexp: "^.+$", ContainerImage: "registry.example.com/xe-driver:2.0"},
					},
				},
				DynamicResourceAllocationSpec: v1alpha.DynamicResourceAllocationSpec{
					Image: "ghcr.io/intel/gpu-dra:v0.11.0",
				},
			},
			func(mod *kmmv1beta1.Module, _ *v1alpha.ClusterPolicy) {
				Expect(mod.Spec.ModuleLoader.Container.Version).To(Equal("2.0"))
			},
		),
		Entry("should configure Build on kernel mapping",
			"kmm-exp-build",
			v1alpha.ClusterPolicySpec{
				ResourceRegistration: "dra",
				KernelModule: &v1alpha.KernelModuleSpec{
					ModuleName: "xe",
					KernelMappings: []v1alpha.KernelMappingSpec{
						{
							Regexp: "^5\\.14\\..*",
							Build: &v1alpha.KernelModuleBuildSpec{
								DockerfileConfigMap: v1.LocalObjectReference{Name: "xe-dockerfile"},
								BuildArgs:           []v1alpha.BuildArg{{Name: "XE_TAG", Value: "v1.0"}},
								Secrets:             []v1.LocalObjectReference{{Name: "private-repo"}},
							},
						},
					},
				},
				DynamicResourceAllocationSpec: v1alpha.DynamicResourceAllocationSpec{
					Image: "ghcr.io/intel/gpu-dra:v0.11.0",
				},
			},
			func(mod *kmmv1beta1.Module, _ *v1alpha.ClusterPolicy) {
				kmmBuild := mod.Spec.ModuleLoader.Container.KernelMappings[0].Build
				Expect(kmmBuild).NotTo(BeNil())
				Expect(kmmBuild.DockerfileConfigMap).NotTo(BeNil())
				Expect(kmmBuild.DockerfileConfigMap.Name).To(Equal("xe-dockerfile"))
				Expect(kmmBuild.BuildArgs).To(HaveLen(1))
				Expect(kmmBuild.BuildArgs[0].Name).To(Equal("XE_TAG"))
				Expect(kmmBuild.BuildArgs[0].Value).To(Equal("v1.0"))
				Expect(kmmBuild.Secrets).To(HaveLen(1))
				Expect(kmmBuild.Secrets[0].Name).To(Equal("private-repo"))
				// The build must run on the same nodes the module targets.
				Expect(kmmBuild.Selector).To(Equal(mod.Spec.Selector))
			},
		),
		Entry("should propagate FirmwarePath to ModprobeSpec",
			"kmm-exp-firmware",
			v1alpha.ClusterPolicySpec{
				ResourceRegistration: "dra",
				KernelModule: &v1alpha.KernelModuleSpec{
					ModuleName:   "xe",
					FirmwarePath: "/opt/lib/firmware/xe",
					KernelMappings: []v1alpha.KernelMappingSpec{
						{Regexp: "^.+$", ContainerImage: "registry.example.com/xe-driver:1.0"},
					},
				},
				DynamicResourceAllocationSpec: v1alpha.DynamicResourceAllocationSpec{
					Image: "ghcr.io/intel/gpu-dra:v0.11.0",
				},
			},
			func(mod *kmmv1beta1.Module, _ *v1alpha.ClusterPolicy) {
				Expect(mod.Spec.ModuleLoader.Container.Modprobe.FirmwarePath).To(Equal("/opt/lib/firmware/xe"))
			},
		),
		Entry("should propagate ModulesLoadingOrder to ModprobeSpec",
			"kmm-exp-loadorder",
			v1alpha.ClusterPolicySpec{
				ResourceRegistration: "dra",
				KernelModule: &v1alpha.KernelModuleSpec{
					ModuleName:          "xe",
					ModulesLoadingOrder: []string{"xe", "drm_buddy", "drm_ttm_helper"},
					KernelMappings: []v1alpha.KernelMappingSpec{
						{Regexp: "^.+$", ContainerImage: "registry.example.com/xe-driver:1.0"},
					},
				},
				DynamicResourceAllocationSpec: v1alpha.DynamicResourceAllocationSpec{
					Image: "ghcr.io/intel/gpu-dra:v0.11.0",
				},
			},
			func(mod *kmmv1beta1.Module, _ *v1alpha.ClusterPolicy) {
				Expect(mod.Spec.ModuleLoader.Container.Modprobe.ModulesLoadingOrder).To(Equal([]string{"xe", "drm_buddy", "drm_ttm_helper"}))
			},
		),
		Entry("should map RegistryTLS to container-level TLSOptions",
			"kmm-exp-tls",
			v1alpha.ClusterPolicySpec{
				ResourceRegistration: "dra",
				KernelModule: &v1alpha.KernelModuleSpec{
					ModuleName:  "xe",
					RegistryTLS: &v1alpha.RegistryTLSSpec{Insecure: true, InsecureSkipTLSVerify: true},
					KernelMappings: []v1alpha.KernelMappingSpec{
						{Regexp: "^.+$", ContainerImage: "registry.example.com/xe-driver:1.0"},
					},
				},
				DynamicResourceAllocationSpec: v1alpha.DynamicResourceAllocationSpec{
					Image: "ghcr.io/intel/gpu-dra:v0.11.0",
				},
			},
			func(mod *kmmv1beta1.Module, _ *v1alpha.ClusterPolicy) {
				Expect(mod.Spec.ModuleLoader.Container.RegistryTLS.Insecure).To(BeTrue())
				Expect(mod.Spec.ModuleLoader.Container.RegistryTLS.InsecureSkipTLSVerify).To(BeTrue())
			},
		),
		Entry("should map per-mapping RegistryTLS to KernelMapping TLSOptions",
			"kmm-exp-tls-map",
			v1alpha.ClusterPolicySpec{
				ResourceRegistration: "dra",
				KernelModule: &v1alpha.KernelModuleSpec{
					ModuleName: "xe",
					KernelMappings: []v1alpha.KernelMappingSpec{
						{
							Regexp:         "^.+$",
							ContainerImage: "registry.example.com/xe-driver:1.0",
							RegistryTLS:    &v1alpha.RegistryTLSSpec{InsecureSkipTLSVerify: true},
						},
					},
				},
				DynamicResourceAllocationSpec: v1alpha.DynamicResourceAllocationSpec{
					Image: "ghcr.io/intel/gpu-dra:v0.11.0",
				},
			},
			func(mod *kmmv1beta1.Module, _ *v1alpha.ClusterPolicy) {
				Expect(mod.Spec.ModuleLoader.Container.KernelMappings[0].RegistryTLS).NotTo(BeNil())
				Expect(mod.Spec.ModuleLoader.Container.KernelMappings[0].RegistryTLS.InsecureSkipTLSVerify).To(BeTrue())
			},
		),
		Entry("should set container-level InTreeModulesToRemove to [ModuleName]",
			"kmm-exp-intree",
			v1alpha.ClusterPolicySpec{
				ResourceRegistration: "dra",
				KernelModule: &v1alpha.KernelModuleSpec{
					ModuleName: "xe",
					KernelMappings: []v1alpha.KernelMappingSpec{
						{Regexp: "^.+$", ContainerImage: "registry.example.com/xe-driver:1.0"},
					},
				},
				DynamicResourceAllocationSpec: v1alpha.DynamicResourceAllocationSpec{
					Image: "ghcr.io/intel/gpu-dra:v0.11.0",
				},
			},
			func(mod *kmmv1beta1.Module, _ *v1alpha.ClusterPolicy) {
				Expect(mod.Spec.ModuleLoader.Container.InTreeModulesToRemove).To(Equal([]string{"xe"}))
				Expect(mod.Spec.ModuleLoader.Container.KernelMappings[0].InTreeModulesToRemove).To(BeEmpty())
			},
		),
		Entry("should auto-prepend ModuleName to per-mapping InTreeModulesToRemove",
			"kmm-exp-dedup",
			v1alpha.ClusterPolicySpec{
				ResourceRegistration: "dra",
				KernelModule: &v1alpha.KernelModuleSpec{
					ModuleName: "xe",
					KernelMappings: []v1alpha.KernelMappingSpec{
						{
							Regexp:                "^.+$",
							ContainerImage:        "registry.example.com/xe-driver:1.0",
							InTreeModulesToRemove: []string{"i915"},
						},
					},
				},
				DynamicResourceAllocationSpec: v1alpha.DynamicResourceAllocationSpec{
					Image: "ghcr.io/intel/gpu-dra:v0.11.0",
				},
			},
			func(mod *kmmv1beta1.Module, _ *v1alpha.ClusterPolicy) {
				Expect(mod.Spec.ModuleLoader.Container.KernelMappings[0].InTreeModulesToRemove).To(Equal([]string{"xe", "i915"}))
			},
		),
		Entry("should set per-mapping InTreeModulesToRemove override",
			"kmm-exp-permapping",
			v1alpha.ClusterPolicySpec{
				ResourceRegistration: "dra",
				KernelModule: &v1alpha.KernelModuleSpec{
					ModuleName: "xe",
					KernelMappings: []v1alpha.KernelMappingSpec{
						{
							Regexp:                "^5\\.14\\..*",
							ContainerImage:        "registry.example.com/xe:1.0",
							InTreeModulesToRemove: []string{"old_xe"},
						},
					},
				},
				DynamicResourceAllocationSpec: v1alpha.DynamicResourceAllocationSpec{
					Image: "ghcr.io/intel/gpu-dra:v0.11.0",
				},
			},
			func(mod *kmmv1beta1.Module, _ *v1alpha.ClusterPolicy) {
				Expect(mod.Spec.ModuleLoader.Container.KernelMappings[0].InTreeModulesToRemove).To(Equal([]string{"xe", "old_xe"}))
				Expect(mod.Spec.ModuleLoader.Container.InTreeModulesToRemove).To(Equal([]string{"xe"}))
			},
		),
	)

	Context("KMM ready-label gating", func() {
		It("should not add KMM ready label to the Module selector", func() {
			ns := "kmm-label-gate"
			resName := ns
			ctx := context.Background()

			Expect(k8sClient.Create(ctx, &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: ns},
			})).To(Succeed())

			cp := &v1alpha.ClusterPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: resName},
				Spec: v1alpha.ClusterPolicySpec{
					ResourceRegistration: "dra",
					KernelModule: &v1alpha.KernelModuleSpec{
						ModuleName: "xe",
						KernelMappings: []v1alpha.KernelMappingSpec{
							{Regexp: "^.+$", ContainerImage: "registry.example.com/xe-driver:1.0"},
						},
					},
					DynamicResourceAllocationSpec: v1alpha.DynamicResourceAllocationSpec{
						Image: "ghcr.io/intel/gpu-dra:v0.11.0",
					},
				},
			}
			Expect(k8sClient.Create(ctx, cp)).To(Succeed())

			reconciler := &ClusterPolicyReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Opts: ControllerOpts{
					Namespace:                      ns,
					KMMEnable:                      true,
					ModuleLoaderServiceAccountName: "intel-gpu-module-loader",
				},
			}

			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: resName},
			})
			Expect(err).NotTo(HaveOccurred())

			mod := &kmmv1beta1.Module{}
			modKey := types.NamespacedName{Name: resName + kmmModuleSuffix, Namespace: ns}
			Expect(k8sClient.Get(ctx, modKey, mod)).To(Succeed())

			readyLabel := "kmm.node.kubernetes.io/" + ns + "." + resName + kmmModuleSuffix + ".ready"
			Expect(mod.Spec.Selector).NotTo(HaveKey(readyLabel),
				"KMM Module selector must not gate on its own ready label")
		})

		It("should add KMM ready label to generateNodeSelector when KernelModule is set", func() {
			readyLabel := "kmm.node.kubernetes.io/test-ns.test-cp-gpu.ready"
			cp := &v1alpha.ClusterPolicy{
				Spec: v1alpha.ClusterPolicySpec{
					KernelModule: &v1alpha.KernelModuleSpec{
						ModuleName: "xe",
						KernelMappings: []v1alpha.KernelMappingSpec{
							{Regexp: "^.+$", ContainerImage: "registry.example.com/xe:1.0"},
						},
					},
				},
			}
			opts := ControllerOpts{KMMModuleReadyLabel: readyLabel}
			ns := generateNodeSelector(cp, opts)
			Expect(ns).To(HaveKeyWithValue(readyLabel, ""))
		})

		It("should not add KMM ready label when KernelModule is nil", func() {
			readyLabel := "kmm.node.kubernetes.io/test-ns.test-cp-gpu.ready"
			cp := &v1alpha.ClusterPolicy{}
			opts := ControllerOpts{KMMModuleReadyLabel: readyLabel}
			ns := generateNodeSelector(cp, opts)
			Expect(ns).NotTo(HaveKey(readyLabel))
		})

		It("should not add KMM ready label when KMMModuleReadyLabel is empty", func() {
			cp := &v1alpha.ClusterPolicy{
				Spec: v1alpha.ClusterPolicySpec{
					KernelModule: &v1alpha.KernelModuleSpec{
						ModuleName: "xe",
						KernelMappings: []v1alpha.KernelMappingSpec{
							{Regexp: "^.+$", ContainerImage: "registry.example.com/xe:1.0"},
						},
					},
				},
			}
			opts := ControllerOpts{}
			ns := generateNodeSelector(cp, opts)
			for k := range ns {
				Expect(k).NotTo(ContainSubstring("kmm.node.kubernetes.io"))
			}
		})
	})

	Context("When toggling container.version between empty and non-empty", func() {
		newReconciler := func(ns string) *KMMReconciler {
			return &KMMReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Opts: ControllerOpts{
					ReqName:                        ns,
					Namespace:                      ns,
					KMMEnable:                      true,
					RequeueDelay:                   time.Millisecond,
					ModuleLoaderServiceAccountName: "module-loader",
				},
			}
		}

		cpWithVersion := func(name, version string) *v1alpha.ClusterPolicy {
			return &v1alpha.ClusterPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: name},
				Spec: v1alpha.ClusterPolicySpec{
					ResourceRegistration: "dra",
					KernelModule: &v1alpha.KernelModuleSpec{
						ModuleName: "xe",
						Version:    version,
						KernelMappings: []v1alpha.KernelMappingSpec{
							{Regexp: "^.+$", ContainerImage: "registry.example.com/xe-driver:1.0"},
						},
					},
				},
			}
		}

		It("recreates the Module when version is added to an existing (empty-version) Module", func() {
			ns := "kmm-ver-add"
			ctx := context.Background()
			Expect(k8sClient.Create(ctx, &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: ns},
			})).To(Succeed())

			cp := cpWithVersion(ns, "")
			Expect(k8sClient.Create(ctx, cp)).To(Succeed())

			reconciler := newReconciler(ns)
			modKey := types.NamespacedName{Name: ns + kmmModuleSuffix, Namespace: ns}

			_, err := reconciler.Reconcile(ctx, cp)
			Expect(err).NotTo(HaveOccurred())

			mod := &kmmv1beta1.Module{}
			Expect(k8sClient.Get(ctx, modKey, mod)).To(Succeed())
			Expect(mod.Spec.ModuleLoader.Container.Version).To(BeEmpty())
			originalUID := mod.UID

			// Add a version: the reconcile should delete the Module and requeue
			// rather than attempt an in-place patch KMM would reject.
			cp.Spec.KernelModule.Version = "1.0"
			res, err := reconciler.Reconcile(ctx, cp)
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(BeNumerically(">", 0))
			Expect(cp.Status.KMMStatus).To(Equal("Recreating"))
			Expect(errors.IsNotFound(k8sClient.Get(ctx, modKey, &kmmv1beta1.Module{}))).To(BeTrue())

			// The next reconcile recreates the Module with the new version.
			_, err = reconciler.Reconcile(ctx, cp)
			Expect(err).NotTo(HaveOccurred())
			recreated := &kmmv1beta1.Module{}
			Expect(k8sClient.Get(ctx, modKey, recreated)).To(Succeed())
			Expect(recreated.Spec.ModuleLoader.Container.Version).To(Equal("1.0"))
			Expect(recreated.UID).NotTo(Equal(originalUID))
		})

		It("recreates the Module when version is removed from an existing (versioned) Module", func() {
			ns := "kmm-ver-remove"
			ctx := context.Background()
			Expect(k8sClient.Create(ctx, &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: ns},
			})).To(Succeed())

			cp := cpWithVersion(ns, "1.0")
			Expect(k8sClient.Create(ctx, cp)).To(Succeed())

			reconciler := newReconciler(ns)
			modKey := types.NamespacedName{Name: ns + kmmModuleSuffix, Namespace: ns}

			_, err := reconciler.Reconcile(ctx, cp)
			Expect(err).NotTo(HaveOccurred())

			mod := &kmmv1beta1.Module{}
			Expect(k8sClient.Get(ctx, modKey, mod)).To(Succeed())
			Expect(mod.Spec.ModuleLoader.Container.Version).To(Equal("1.0"))
			originalUID := mod.UID

			cp.Spec.KernelModule.Version = ""
			res, err := reconciler.Reconcile(ctx, cp)
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(BeNumerically(">", 0))
			Expect(errors.IsNotFound(k8sClient.Get(ctx, modKey, &kmmv1beta1.Module{}))).To(BeTrue())

			_, err = reconciler.Reconcile(ctx, cp)
			Expect(err).NotTo(HaveOccurred())
			recreated := &kmmv1beta1.Module{}
			Expect(k8sClient.Get(ctx, modKey, recreated)).To(Succeed())
			Expect(recreated.Spec.ModuleLoader.Container.Version).To(BeEmpty())
			Expect(recreated.UID).NotTo(Equal(originalUID))
		})

		It("patches in place (no recreate) when version changes between two non-empty values", func() {
			ns := "kmm-ver-bump"
			ctx := context.Background()
			Expect(k8sClient.Create(ctx, &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: ns},
			})).To(Succeed())

			cp := cpWithVersion(ns, "1.0")
			Expect(k8sClient.Create(ctx, cp)).To(Succeed())

			reconciler := newReconciler(ns)
			modKey := types.NamespacedName{Name: ns + kmmModuleSuffix, Namespace: ns}

			_, err := reconciler.Reconcile(ctx, cp)
			Expect(err).NotTo(HaveOccurred())

			mod := &kmmv1beta1.Module{}
			Expect(k8sClient.Get(ctx, modKey, mod)).To(Succeed())
			originalUID := mod.UID

			cp.Spec.KernelModule.Version = "2.0"
			res, err := reconciler.Reconcile(ctx, cp)
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(BeZero())

			updated := &kmmv1beta1.Module{}
			Expect(k8sClient.Get(ctx, modKey, updated)).To(Succeed())
			Expect(updated.Spec.ModuleLoader.Container.Version).To(Equal("2.0"))
			Expect(updated.UID).To(Equal(originalUID), "Module should be patched in place, not recreated")
		})

		It("blocks the recreate while GPU ResourceClaims are allocated", func() {
			ns := "kmm-ver-inuse"
			ctx := context.Background()
			Expect(k8sClient.Create(ctx, &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: ns},
			})).To(Succeed())

			cp := cpWithVersion(ns, "")
			Expect(k8sClient.Create(ctx, cp)).To(Succeed())

			reconciler := newReconciler(ns)
			reconciler.Opts.DRAEnable = true
			modKey := types.NamespacedName{Name: ns + kmmModuleSuffix, Namespace: ns}

			_, err := reconciler.Reconcile(ctx, cp)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, modKey, &kmmv1beta1.Module{})).To(Succeed())

			By("allocating a GPU ResourceClaim")
			rc := &resourcev1.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "test-gpu-claim", Namespace: ns},
				Spec: resourcev1.ResourceClaimSpec{
					Devices: resourcev1.DeviceClaim{
						Requests: []resourcev1.DeviceRequest{
							{
								Name: "gpu",
								FirstAvailable: []resourcev1.DeviceSubRequest{
									{
										Name:            "gpu-sub",
										DeviceClassName: gpuDeviceClass,
										AllocationMode:  resourcev1.DeviceAllocationModeExactCount,
										Count:           1,
									},
								},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, rc)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, rc) })

			rc.Status.Allocation = &resourcev1.AllocationResult{
				Devices: resourcev1.DeviceAllocationResult{
					Results: []resourcev1.DeviceRequestAllocationResult{
						{Request: "gpu", Driver: gpuDeviceClass, Pool: "node-pool", Device: "gpu-0"},
					},
				},
			}
			Expect(k8sClient.Status().Update(ctx, rc)).To(Succeed())

			By("toggling version while the claim is allocated: recreate must be blocked")
			cp.Spec.KernelModule.Version = "1.0"
			res, err := reconciler.Reconcile(ctx, cp)
			Expect(stderrors.Is(err, requeueReconcileErr{})).To(BeTrue())
			Expect(res.RequeueAfter).To(BeNumerically(">", 0))
			Expect(cp.Status.Errors).To(ContainElement(ContainSubstring("blocking modules.kmm.sigs.x-k8s.io/" + ns + kmmModuleSuffix)))
			Expect(k8sClient.Get(ctx, modKey, &kmmv1beta1.Module{})).To(Succeed(), "Module must not be deleted while GPUs are in use")

			By("freeing the claim: the recreate proceeds")
			Expect(k8sClient.Delete(ctx, rc)).To(Succeed())
			_, err = reconciler.Reconcile(ctx, cp)
			Expect(err).NotTo(HaveOccurred())
			Expect(errors.IsNotFound(k8sClient.Get(ctx, modKey, &kmmv1beta1.Module{}))).To(BeTrue())
		})
	})

	Context("When deleting Module with allocated ResourceClaims", func() {
		const (
			namespace    = "kmm-rc-safety"
			resourceName = "kmm-rc-safety"
		)

		ctx := context.Background()
		typeNamespacedName := types.NamespacedName{Name: resourceName}

		BeforeEach(func() {
			Expect(k8sClient.Create(ctx, &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: namespace},
			})).To(Succeed())
		})

		AfterEach(func() {
			resource := &v1alpha.ClusterPolicy{}
			if err := k8sClient.Get(ctx, typeNamespacedName, resource); err == nil {
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			}

			var rcList resourcev1.ResourceClaimList
			if err := k8sClient.List(ctx, &rcList); err == nil {
				for i := range rcList.Items {
					_ = k8sClient.Delete(ctx, &rcList.Items[i])
				}
			}
		})

		It("should requeue instead of deleting the Module", func() {
			cp := &v1alpha.ClusterPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName},
				Spec: v1alpha.ClusterPolicySpec{
					ResourceRegistration: "dra",
					KernelModule: &v1alpha.KernelModuleSpec{
						ModuleName: "xe",
						KernelMappings: []v1alpha.KernelMappingSpec{
							{Regexp: "^.+$", ContainerImage: "registry.example.com/xe-driver:1.0"},
						},
					},
					DynamicResourceAllocationSpec: v1alpha.DynamicResourceAllocationSpec{
						Image: "ghcr.io/intel/gpu-dra:v0.11.0",
					},
				},
			}
			Expect(k8sClient.Create(ctx, cp)).To(Succeed())

			reconciler := &ClusterPolicyReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Opts: ControllerOpts{
					Namespace:                      namespace,
					KMMEnable:                      true,
					DRAEnable:                      true,
					RequeueDelay:                   5 * time.Second,
					ModuleLoaderServiceAccountName: "intel-gpu-module-loader",
				},
			}

			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			modKey := types.NamespacedName{Name: resourceName + kmmModuleSuffix, Namespace: namespace}
			Expect(k8sClient.Get(ctx, modKey, &kmmv1beta1.Module{})).To(Succeed())

			By("creating an allocated ResourceClaim with a GPU device")
			rc := &resourcev1.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-gpu-claim",
					Namespace: namespace,
				},
				Spec: resourcev1.ResourceClaimSpec{
					Devices: resourcev1.DeviceClaim{
						Requests: []resourcev1.DeviceRequest{
							{
								Name: "gpu",
								FirstAvailable: []resourcev1.DeviceSubRequest{
									{
										Name:            "gpu-sub",
										DeviceClassName: gpuDeviceClass,
										AllocationMode:  resourcev1.DeviceAllocationModeExactCount,
										Count:           1,
									},
								},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, rc)).To(Succeed())

			rc.Status.Allocation = &resourcev1.AllocationResult{
				Devices: resourcev1.DeviceAllocationResult{
					Results: []resourcev1.DeviceRequestAllocationResult{
						{
							Request: "gpu",
							Driver:  gpuDeviceClass,
							Pool:    "node-pool",
							Device:  "gpu-0",
						},
					},
				},
			}
			Expect(k8sClient.Status().Update(ctx, rc)).To(Succeed())

			By("removing KernelModule to trigger deletion")
			Expect(k8sClient.Get(ctx, typeNamespacedName, cp)).To(Succeed())
			cp.Spec.KernelModule = nil
			Expect(k8sClient.Update(ctx, cp)).To(Succeed())

			result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(5 * time.Second))

			By("verifying Module was NOT deleted")
			Expect(k8sClient.Get(ctx, modKey, &kmmv1beta1.Module{})).To(Succeed())

			By("removing the ResourceClaim and reconciling again")
			Expect(k8sClient.Delete(ctx, rc)).To(Succeed())

			result, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero())

			By("verifying Module was deleted")
			err = k8sClient.Get(ctx, modKey, &kmmv1beta1.Module{})
			Expect(errors.IsNotFound(err)).To(BeTrue())
		})
	})

	Context("When KernelModule is nil", func() {
		const (
			namespace    = "kmm-exp-nkmm"
			resourceName = "kmm-exp-nkmm"
		)

		ctx := context.Background()
		typeNamespacedName := types.NamespacedName{Name: resourceName}

		BeforeEach(func() {
			Expect(k8sClient.Create(ctx, &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: namespace},
			})).To(Succeed())
		})

		AfterEach(func() {
			resource := &v1alpha.ClusterPolicy{}
			if err := k8sClient.Get(ctx, typeNamespacedName, resource); err == nil {
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			}
		})

		It("should set KMMStatus to N/A", func() {
			cp := &v1alpha.ClusterPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName},
				Spec: v1alpha.ClusterPolicySpec{
					ResourceRegistration: "dra",
					DynamicResourceAllocationSpec: v1alpha.DynamicResourceAllocationSpec{
						Image: "ghcr.io/intel/gpu-dra:v0.11.0",
					},
				},
			}
			Expect(k8sClient.Create(ctx, cp)).To(Succeed())

			reconciler := &ClusterPolicyReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Opts: ControllerOpts{
					Namespace:                      namespace,
					KMMEnable:                      true,
					ModuleLoaderServiceAccountName: "intel-gpu-module-loader",
				},
			}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, typeNamespacedName, cp)).To(Succeed())
			Expect(cp.Status.KMMStatus).To(Equal("N/A"))
		})
	})
})
