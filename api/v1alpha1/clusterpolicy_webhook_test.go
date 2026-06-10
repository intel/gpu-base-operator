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

package v1alpha1

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/distribution/reference"

	v1 "k8s.io/api/core/v1"
)

const (
	draName = "dra"
	dpName  = "dp"

	invalidImageRef = "not valid:image:ref"
)

// validCP returns a minimal valid ClusterPolicy for use as a base in tests.
func validCP() *ClusterPolicy {
	return &ClusterPolicy{
		Spec: ClusterPolicySpec{
			ResourceRegistration: dpName,
		},
	}
}

var _ = Describe("ClusterPolicy Defaulter Webhook", func() {
	var (
		obj       *ClusterPolicy
		defaulter ClusterPolicyCustomDefaulter
	)

	BeforeEach(func() {
		obj = validCP()
		defaulter = ClusterPolicyCustomDefaulter{}
	})

	Context("Default", func() {
		It("sets all default images when none are specified", func() {
			Expect(defaulter.Default(ctx, obj)).To(Succeed())
			Expect(obj.Spec.DevicePluginSpec.PluginImage).To(Equal(DefaultDPImage))
			Expect(obj.Spec.DynamicResourceAllocationSpec.Image).To(Equal(DefaultDRAImage))
			Expect(obj.Spec.XpuManagerSpec.Image).To(Equal(DefaultXPUImage))
			Expect(obj.Spec.XpuManagerSpec.MonitoringResource).To(Equal("monitoring"))
		})

		It("does not overwrite dp.plugin when already set", func() {
			custom := "my.registry/gpu-plugin:v1"
			obj.Spec.DevicePluginSpec.PluginImage = custom
			Expect(defaulter.Default(ctx, obj)).To(Succeed())
			Expect(obj.Spec.DevicePluginSpec.PluginImage).To(Equal(custom))
		})

		It("does not overwrite dra.image when already set", func() {
			custom := "my.registry/gpu-dra:v1"
			obj.Spec.DynamicResourceAllocationSpec.Image = custom
			Expect(defaulter.Default(ctx, obj)).To(Succeed())
			Expect(obj.Spec.DynamicResourceAllocationSpec.Image).To(Equal(custom))
		})

		It("does not overwrite xpu.image when already set", func() {
			custom := "my.registry/xpumd:v1"
			obj.Spec.XpuManagerSpec.Image = custom
			Expect(defaulter.Default(ctx, obj)).To(Succeed())
			Expect(obj.Spec.XpuManagerSpec.Image).To(Equal(custom))
		})

		It("sets xpu.monitoringResource to 'monitoring' when not specified", func() {
			Expect(defaulter.Default(ctx, obj)).To(Succeed())
			Expect(obj.Spec.XpuManagerSpec.MonitoringResource).To(Equal("monitoring"))
		})

		It("does not overwrite xpu.monitoringResource when already set", func() {
			custom := "i915_monitoring"
			obj.Spec.XpuManagerSpec.MonitoringResource = custom
			Expect(defaulter.Default(ctx, obj)).To(Succeed())
			Expect(obj.Spec.XpuManagerSpec.MonitoringResource).To(Equal(custom))
		})

		It("sets defaults idempotently when called twice", func() {
			Expect(defaulter.Default(ctx, obj)).To(Succeed())
			Expect(defaulter.Default(ctx, obj)).To(Succeed())
			Expect(obj.Spec.DevicePluginSpec.PluginImage).To(Equal(DefaultDPImage))
			Expect(obj.Spec.XpuManagerSpec.Image).To(Equal(DefaultXPUImage))
		})

		It("default images are valid image references", func() {
			Expect(defaulter.Default(ctx, obj)).To(Succeed())
			for _, img := range []string{
				obj.Spec.DevicePluginSpec.PluginImage,
				obj.Spec.DynamicResourceAllocationSpec.Image,
				obj.Spec.XpuManagerSpec.Image,
			} {
				_, err := reference.ParseNormalizedNamed(img)
				Expect(err).NotTo(HaveOccurred(), "default image %q is not a valid reference", img)
			}
		})
	})
})

var _ = Describe("ClusterPolicy Webhook", func() {
	var (
		obj       *ClusterPolicy
		validator ClusterPolicyCustomValidator
	)

	BeforeEach(func() {
		obj = validCP()
		validator = ClusterPolicyCustomValidator{}
	})

	Context("ValidateCreate", func() {
		It("accepts a minimal valid dp ClusterPolicy", func() {
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		It("accepts a minimal valid dra ClusterPolicy", func() {
			obj.Spec.ResourceRegistration = draName
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		Context("dp.allowIDs / dp.denyIDs", func() {
			It("accepts valid allowIDs", func() {
				obj.Spec.DevicePluginSpec.AllowIDs = []string{"0x56c0", "0x56c1"}
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).NotTo(HaveOccurred())
			})

			It("accepts valid denyIDs", func() {
				obj.Spec.DevicePluginSpec.DenyIDs = []string{"0x1234"}
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).NotTo(HaveOccurred())
			})

			It("rejects allowIDs and denyIDs set together", func() {
				obj.Spec.DevicePluginSpec.AllowIDs = []string{"0x56c0"}
				obj.Spec.DevicePluginSpec.DenyIDs = []string{"0x1234"}
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("allowIDs and dp.denyIDs cannot both be set"))
			})

			It("rejects malformed PCI IDs in allowIDs", func() {
				for _, badID := range []string{"1234", "0X56C0", "0xGGGG", "0x56c", "0x56c01", ""} {
					obj.Spec.DevicePluginSpec.AllowIDs = []string{badID}
					obj.Spec.DevicePluginSpec.DenyIDs = nil
					_, err := validator.ValidateCreate(ctx, obj)
					Expect(err).To(HaveOccurred(), "expected error for allowID %q", badID)
				}
			})

			It("rejects malformed PCI IDs in denyIDs", func() {
				for _, badID := range []string{"1234", "0X56C0", "0xGGGG", "0x56c", "0x56c01"} {
					obj.Spec.DevicePluginSpec.DenyIDs = []string{badID}
					obj.Spec.DevicePluginSpec.AllowIDs = nil
					_, err := validator.ValidateCreate(ctx, obj)
					Expect(err).To(HaveOccurred(), "expected error for denyID %q", badID)
				}
			})
		})

		Context("image reference validation", func() {
			It("accepts valid image references", func() {
				obj.Spec.DynamicResourceAllocationSpec.Image = "registry.example.com/intel/gpu-dra:latest"
				obj.Spec.DevicePluginSpec.PluginImage = "intel/gpu-plugin:v1.0"
				obj.Spec.XpuManagerSpec.Image = "intel/xpumanager:latest"
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).NotTo(HaveOccurred())
			})

			It("accepts empty image fields", func() {
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).NotTo(HaveOccurred())
			})

			It("rejects invalid dra.image", func() {
				obj.Spec.DynamicResourceAllocationSpec.Image = invalidImageRef
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("dra.image"))
			})

			It("rejects invalid dp.plugin", func() {
				obj.Spec.DevicePluginSpec.PluginImage = invalidImageRef
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("dp.plugin"))
			})

			It("rejects invalid xpu.image", func() {
				obj.Spec.XpuManagerSpec.Image = invalidImageRef
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("xpu.image"))
			})

			It("reports all invalid image fields together", func() {
				obj.Spec.DevicePluginSpec.PluginImage = "bad:img:1"
				obj.Spec.XpuManagerSpec.Image = "bad:img:2"
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("dp.plugin"))
				Expect(err.Error()).To(ContainSubstring("xpu.image"))
			})
		})

		Context("pullSecret validation", func() {
			It("accepts a valid pullSecret name", func() {
				obj.Spec.PullSecret = &v1.LocalObjectReference{Name: "my-pull-secret"}
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).NotTo(HaveOccurred())
			})

			It("accepts no pullSecret", func() {
				obj.Spec.PullSecret = nil
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).NotTo(HaveOccurred())
			})

			It("rejects an empty pullSecret name", func() {
				obj.Spec.PullSecret = &v1.LocalObjectReference{Name: ""}
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("pullSecret.name"))
			})

			It("rejects a pullSecret name with invalid characters", func() {
				obj.Spec.PullSecret = &v1.LocalObjectReference{Name: "inv@lid name!"}
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("pullSecret.name"))
			})
		})

		Context("xpu.configMapOverride validation", func() {
			It("accepts a valid configMapOverride name", func() {
				obj.Spec.XpuManagerSpec.ConfigMapOverride = "my-otel-config"
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).NotTo(HaveOccurred())
			})

			It("accepts empty configMapOverride", func() {
				obj.Spec.XpuManagerSpec.ConfigMapOverride = ""
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).NotTo(HaveOccurred())
			})

			It("rejects an invalid configMapOverride name", func() {
				obj.Spec.XpuManagerSpec.ConfigMapOverride = "Invalid_Name!"
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("xpu.configMapOverride"))
			})
		})

		Context("Kueue spec validation", func() {
			It("accepts a valid Kueue spec", func() {
				obj.Spec.EnableKueue = true
				obj.Spec.Kueue = &KueueQueueSpec{
					EqualResources: []ClusterQueueSpec{
						{
							Name: "cluster-queue-a",
							LocalQueues: []LocalQueueSpec{
								{Name: "lq-1", Namespace: "team-a"},
								{Name: "lq-2", Namespace: "team-b"},
							},
						},
						{
							Name: "cluster-queue-b",
							LocalQueues: []LocalQueueSpec{
								{Name: "lq-1", Namespace: "team-c"},
							},
						},
					},
				}
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).NotTo(HaveOccurred())
			})

			It("accepts nil Kueue", func() {
				obj.Spec.Kueue = nil
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).NotTo(HaveOccurred())
			})

			It("rejects an empty clusterQueue name", func() {
				obj.Spec.Kueue = &KueueQueueSpec{
					EqualResources: []ClusterQueueSpec{
						{Name: "", LocalQueues: []LocalQueueSpec{{Name: "lq", Namespace: "ns"}}},
					},
				}
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("equalResources[0].name"))
			})

			It("rejects a clusterQueue name with invalid characters", func() {
				obj.Spec.Kueue = &KueueQueueSpec{
					EqualResources: []ClusterQueueSpec{
						{Name: "Invalid_CQ!", LocalQueues: nil},
					},
				}
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("equalResources[0].name"))
			})

			It("rejects duplicate clusterQueue names", func() {
				obj.Spec.Kueue = &KueueQueueSpec{
					EqualResources: []ClusterQueueSpec{
						{Name: "cq-a", LocalQueues: nil},
						{Name: "cq-a", LocalQueues: nil},
					},
				}
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("duplicate clusterQueue name"))
			})

			It("rejects an empty localQueue name", func() {
				obj.Spec.Kueue = &KueueQueueSpec{
					EqualResources: []ClusterQueueSpec{
						{Name: "cq", LocalQueues: []LocalQueueSpec{{Name: "", Namespace: "ns"}}},
					},
				}
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("localQueues[0].name"))
			})

			It("rejects an empty localQueue namespace", func() {
				obj.Spec.Kueue = &KueueQueueSpec{
					EqualResources: []ClusterQueueSpec{
						{Name: "cq", LocalQueues: []LocalQueueSpec{{Name: "lq", Namespace: ""}}},
					},
				}
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("localQueues[0].namespace"))
			})

			It("rejects duplicate localQueue namespace/name pairs within a clusterQueue", func() {
				obj.Spec.Kueue = &KueueQueueSpec{
					EqualResources: []ClusterQueueSpec{
						{
							Name: "cq",
							LocalQueues: []LocalQueueSpec{
								{Name: "lq", Namespace: "ns"},
								{Name: "lq", Namespace: "ns"},
							},
						},
					},
				}
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("duplicate localQueue"))
			})

			It("allows the same localQueue name in different clusterQueues", func() {
				obj.Spec.Kueue = &KueueQueueSpec{
					EqualResources: []ClusterQueueSpec{
						{Name: "cq-a", LocalQueues: []LocalQueueSpec{{Name: "lq", Namespace: "ns"}}},
						{Name: "cq-b", LocalQueues: []LocalQueueSpec{{Name: "lq", Namespace: "ns"}}},
					},
				}
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).NotTo(HaveOccurred())
			})
		})

		Context("spec warning for Levelzero", func() {
			It("emits a warning when levelzero image is set in DP mode", func() {
				obj.Spec.ResourceRegistration = dpName
				obj.Spec.DevicePluginSpec.PluginImage = "some-plugin:latest"
				obj.Spec.DevicePluginSpec.LevelzeroImage = "some-image:latest"
				warnings, err := validator.ValidateCreate(ctx, obj)
				Expect(err).NotTo(HaveOccurred())
				Expect(warnings).To(Not(BeEmpty()))
			})

			It("does not emit a warning when levelzero image is empty in DP mode", func() {
				obj.Spec.ResourceRegistration = dpName
				obj.Spec.DevicePluginSpec.PluginImage = "some-plugin:latest"
				obj.Spec.DevicePluginSpec.LevelzeroImage = ""
				warnings, err := validator.ValidateCreate(ctx, obj)
				Expect(err).NotTo(HaveOccurred())
				Expect(warnings).To(BeEmpty())
			})
		})
	})

	Context("ValidateUpdate", func() {
		It("applies the same validation as create", func() {
			old := validCP()
			obj.Spec.DevicePluginSpec.AllowIDs = []string{"0x56c0"}
			obj.Spec.DevicePluginSpec.DenyIDs = []string{"0x1234"}
			_, err := validator.ValidateUpdate(ctx, old, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("allowIDs and dp.denyIDs cannot both be set"))
		})

		It("accepts a valid update", func() {
			old := validCP()
			obj.Spec.ResourceRegistration = draName
			obj.Spec.DynamicResourceAllocationSpec.Image = "intel/gpu-dra:v2.0"
			_, err := validator.ValidateUpdate(ctx, old, obj)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("ValidateDelete", func() {
		It("always accepts delete", func() {
			_, err := validator.ValidateDelete(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
