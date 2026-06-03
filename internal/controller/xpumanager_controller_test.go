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
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	core "k8s.io/api/core/v1"

	v1alpha "github.com/intel/gpu-base-operator/api/v1alpha1"
	"github.com/intel/gpu-base-operator/config/deployments"
)

const (
	memoryType = "memory"
	cpuType    = "cpu"
)

var _ = Describe("XPU manager daemonset", func() {

	Context("Daemonset updates", func() {
		controller := &XpuManagerReconciler{
			Client: k8sClient,
			Scheme: nil,
			Opts: ControllerOpts{
				Namespace: "test-xpumanager",
			},
		}

		newCp := func() *v1alpha.ClusterPolicy {
			return &v1alpha.ClusterPolicy{
				Spec: v1alpha.ClusterPolicySpec{
					ResourceRegistration: "dp",
					ResourceMonitoring:   true,
					XpuManagerSpec: v1alpha.XpuManagerSpec{
						Image:              "foobar:1.2.3",
						MonitoringResource: "xe_monitoring",
					},
				},
			}
		}

		It("from default monitoring to i915", func() {
			cp := newCp()

			ds := deployments.XpuManagerDaemonset()
			controller.updateDaemonSetObject(ds, cp, "", "test-otel-cm", "hash-a")
			Expect(ds.Spec.Template.Spec.Containers[0].Image).To(Equal("foobar:1.2.3"))
			Expect(ds.Spec.Template.Annotations[otelConfigHashKey]).To(Equal("hash-a"))
			for k, v := range ds.Spec.Template.Spec.Containers[0].Resources.Requests {
				switch k {
				case "gpu.intel.com/xe_monitoring":
					Expect(v.String()).To(Equal("1"))
				case cpuType:
				case memoryType:
				default:
					Fail(fmt.Sprintf("unexpected resource request key: %s (%v)", k, v))
				}
			}

			cp.Spec.XpuManagerSpec.MonitoringResource = "i915_monitoring"
			controller.updateDaemonSetObject(ds, cp, "", "test-otel-cm", "hash-b")
			Expect(ds.Spec.Template.Annotations[otelConfigHashKey]).To(Equal("hash-b"))
			for k, v := range ds.Spec.Template.Spec.Containers[0].Resources.Requests {
				switch k {
				case "gpu.intel.com/i915_monitoring":
					Expect(v.String()).To(Equal("1"))
				case cpuType:
				case memoryType:
				default:
					Fail(fmt.Sprintf("unexpected resource request key: %s (%v)", k, v))
				}
			}
		})

		It("with NFD labeling", func() {
			cp := newCp()

			cp.Spec.UseNFDLabeling = true

			ds := deployments.XpuManagerDaemonset()
			controller.updateDaemonSetObject(ds, cp, "", "test-otel-cm", "hash-a")

			Expect(ds.Spec.Template.Spec.NodeSelector).To(HaveKeyWithValue("kubernetes.io/arch", "amd64"))
			Expect(ds.Spec.Template.Spec.NodeSelector["intel.feature.node.kubernetes.io/gpu"]).To(Equal("true"))
		})

		It("with custom labeling to NFD", func() {
			cp := newCp()

			cp.Spec.ResourceRegistration = resourceModeDP
			cp.Spec.UseNFDLabeling = false
			cp.Spec.NodeSelector = map[string]string{"foo": "bar"}

			ds := deployments.XpuManagerDaemonset()
			controller.updateDaemonSetObject(ds, cp, "", "test-otel-cm", "hash-a")

			Expect(ds.Spec.Template.Spec.NodeSelector).To(HaveLen(2))
			Expect(ds.Spec.Template.Spec.NodeSelector["foo"]).To(Equal("bar"))
			Expect(ds.Spec.Template.Spec.NodeSelector["kubernetes.io/arch"]).To(Equal("amd64"))

			cp.Spec.UseNFDLabeling = true
			cp.Spec.NodeSelector = nil

			controller.updateDaemonSetObject(ds, cp, "", "test-otel-cm", "hash-a")

			Expect(ds.Spec.Template.Spec.NodeSelector).To(HaveLen(2))
			Expect(ds.Spec.Template.Spec.NodeSelector["kubernetes.io/arch"]).To(Equal("amd64"))
			Expect(ds.Spec.Template.Spec.NodeSelector["intel.feature.node.kubernetes.io/gpu"]).To(Equal("true"))
		})

		It("with custom nodeselector", func() {
			cp := newCp()
			cp.Spec.NodeSelector = map[string]string{
				"my-label": "yes-value",
			}

			ds := deployments.XpuManagerDaemonset()
			controller.updateDaemonSetObject(ds, cp, "", "test-otel-cm", "hash-a")

			Expect(ds.Spec.Template.Spec.NodeSelector).To(HaveLen(2))
			Expect(ds.Spec.Template.Spec.NodeSelector["kubernetes.io/arch"]).To(Equal("amd64"))
			Expect(ds.Spec.Template.Spec.NodeSelector["my-label"]).To(Equal("yes-value"))
		})

		It("with tolerations and pullsecret", func() {
			cp := newCp()
			cp.Spec.Tolerations = []core.Toleration{
				{
					Key:      "my-toleration",
					Operator: core.TolerationOpExists,
					Effect:   core.TaintEffectNoExecute,
				},
			}
			cp.Spec.PullSecret = &core.LocalObjectReference{
				Name: "my-pull-secret",
			}

			ds := deployments.XpuManagerDaemonset()
			controller.updateDaemonSetObject(ds, cp, "", "test-otel-cm", "hash-a")

			Expect(ds.Spec.Template.Spec.Tolerations[0].Key).To(Equal("my-toleration"))
			Expect(ds.Spec.Template.Spec.Tolerations[0].Operator).To(Equal(core.TolerationOpExists))
			Expect(ds.Spec.Template.Spec.Tolerations[0].Effect).To(Equal(core.TaintEffectNoExecute))

			Expect(ds.Spec.Template.Spec.ImagePullSecrets[0].Name).To(Equal("my-pull-secret"))

			cp.Spec.PullSecret = nil
			cp.Spec.Tolerations = nil
			controller.updateDaemonSetObject(ds, cp, "", "test-otel-cm", "hash-a")

			Expect(ds.Spec.Template.Spec.Tolerations).To(BeEmpty())
			Expect(ds.Spec.Template.Spec.ImagePullSecrets).To(BeEmpty())
		})

		It("preserves automountServiceAccountToken=false from YAML", func() {
			cp := newCp()

			ds := deployments.XpuManagerDaemonset()
			controller.updateDaemonSetObject(ds, cp, "", "test-otel-cm", "hash-a")

			Expect(ds.Spec.Template.Spec.AutomountServiceAccountToken).NotTo(BeNil())
			Expect(*ds.Spec.Template.Spec.AutomountServiceAccountToken).To(BeFalse())
		})
	})

	Context("Log level mapping", func() {
		It("maps LogLevel 1 to warn", func() {
			cp := &v1alpha.ClusterPolicy{
				Spec: v1alpha.ClusterPolicySpec{LogLevel: 1},
			}
			Expect(logLevelForXpum(cp)).To(Equal("warn"))
		})

		It("maps LogLevel 2 to info", func() {
			cp := &v1alpha.ClusterPolicy{
				Spec: v1alpha.ClusterPolicySpec{LogLevel: 2},
			}
			Expect(logLevelForXpum(cp)).To(Equal("info"))
		})
	})
})
