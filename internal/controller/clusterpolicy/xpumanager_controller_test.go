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

package clusterpolicy

import (
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	v1alpha "github.com/intel/gpu-base-operator/api/v1alpha1"
	"github.com/intel/gpu-base-operator/config/deployments"
	"github.com/intel/gpu-base-operator/internal/controller/common"
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
			Opts: common.ControllerOpts{
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

		It("sets and clears XPU Manager affinity", func() {
			cp := newCp()
			cp.Spec.XpuManagerSpec.Affinity = &core.Affinity{
				NodeAffinity: &core.NodeAffinity{
					RequiredDuringSchedulingIgnoredDuringExecution: &core.NodeSelector{
						NodeSelectorTerms: []core.NodeSelectorTerm{{
							MatchExpressions: []core.NodeSelectorRequirement{{
								Key:      "gpu.intel.com/xpumd-deny",
								Operator: core.NodeSelectorOpDoesNotExist,
							}},
						}},
					},
				},
			}

			ds := deployments.XpuManagerDaemonset()
			controller.updateDaemonSetObject(ds, cp, "", "test-otel-cm", "hash-a")
			Expect(ds.Spec.Template.Spec.Affinity).To(Equal(cp.Spec.XpuManagerSpec.Affinity))

			cp.Spec.XpuManagerSpec.Affinity = nil
			controller.updateDaemonSetObject(ds, cp, "", "test-otel-cm", "hash-a")
			Expect(ds.Spec.Template.Spec.Affinity).To(BeNil())
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

		It("mounts crashlog and tracefs only when enabled", func() {
			cp := newCp()

			volumeNames := func(ds *apps.DaemonSet) []string {
				names := []string{}
				for _, v := range ds.Spec.Template.Spec.Volumes {
					names = append(names, v.Name)
				}

				return names
			}
			mountNames := func(ds *apps.DaemonSet) []string {
				names := []string{}
				for _, m := range ds.Spec.Template.Spec.Containers[0].VolumeMounts {
					names = append(names, m.Name)
				}

				return names
			}

			ds := deployments.XpuManagerDaemonset()
			controller.updateDaemonSetObject(ds, cp, "", "test-otel-cm", "hash-a")
			Expect(volumeNames(ds)).NotTo(ContainElements(crashlogVolume, tracefsVolume))
			Expect(mountNames(ds)).NotTo(ContainElements(crashlogVolume, tracefsVolume))

			cp.Spec.XpuManagerSpec.CrashLog = true
			cp.Spec.XpuManagerSpec.InfoLogs = true
			controller.updateDaemonSetObject(ds, cp, "", "test-otel-cm", "hash-a")
			// Reconciling twice must not duplicate the entries.
			controller.updateDaemonSetObject(ds, cp, "", "test-otel-cm", "hash-a")
			Expect(volumeNames(ds)).To(ContainElements(crashlogVolume, tracefsVolume))
			Expect(mountNames(ds)).To(ContainElements(crashlogVolume, tracefsVolume))
			Expect(volumeNames(ds)).To(HaveLen(len(deployments.XpuManagerDaemonset().Spec.Template.Spec.Volumes) + 2))

			for _, m := range ds.Spec.Template.Spec.Containers[0].VolumeMounts {
				switch m.Name {
				case crashlogVolume:
					Expect(m.MountPath).To(Equal(crashlogHostDir))
					Expect(m.ReadOnly).To(BeTrue())
				case tracefsVolume:
					Expect(m.MountPath).To(Equal(tracefsHostDir))
					Expect(m.ReadOnly).To(BeFalse())
				}
			}

			cp.Spec.XpuManagerSpec.CrashLog = false
			cp.Spec.XpuManagerSpec.InfoLogs = false
			controller.updateDaemonSetObject(ds, cp, "", "test-otel-cm", "hash-a")
			Expect(volumeNames(ds)).NotTo(ContainElements(crashlogVolume, tracefsVolume))
			Expect(mountNames(ds)).NotTo(ContainElements(crashlogVolume, tracefsVolume))
		})

		It("passes OTLP header Secrets as env and mounts the TLS files only when configured", func() {
			cp := newCp()

			findVolume := func(ds *apps.DaemonSet, name string) *core.Volume {
				for i := range ds.Spec.Template.Spec.Volumes {
					if ds.Spec.Template.Spec.Volumes[i].Name == name {
						return &ds.Spec.Template.Spec.Volumes[i]
					}
				}

				return nil
			}
			findMount := func(ds *apps.DaemonSet, name string) *core.VolumeMount {
				for i := range ds.Spec.Template.Spec.Containers[0].VolumeMounts {
					if ds.Spec.Template.Spec.Containers[0].VolumeMounts[i].Name == name {
						return &ds.Spec.Template.Spec.Containers[0].VolumeMounts[i]
					}
				}

				return nil
			}

			cp.Spec.XpuManagerSpec.OTLP = &v1alpha.OTLPSpec{
				GRPC: &v1alpha.OTLPExporterSpec{
					Endpoint: "https://gateway:4317",
					Headers: []v1alpha.OTLPHeader{
						{
							Name: "Authorization",
							ValueFrom: core.SecretKeySelector{
								LocalObjectReference: core.LocalObjectReference{Name: "otlp-auth"},
								Key:                  "token",
							},
						},
						{
							Name: "X-Tenant",
							ValueFrom: core.SecretKeySelector{
								LocalObjectReference: core.LocalObjectReference{Name: "otlp-auth"},
								Key:                  "tenant",
							},
						},
					},
					TLS: &v1alpha.OTLPTLSSpec{
						CA: &core.ConfigMapKeySelector{
							LocalObjectReference: core.LocalObjectReference{Name: "gateway-ca"},
							Key:                  "ca-bundle.crt",
						},
						ClientCertSecret: &core.LocalObjectReference{Name: "otlp-client"},
					},
				},
			}

			ds := deployments.XpuManagerDaemonset()
			controller.updateDaemonSetObject(ds, cp, "", "test-otel-cm", "hash-a")
			// Reconciling twice must not duplicate the entries.
			controller.updateDaemonSetObject(ds, cp, "", "test-otel-cm", "hash-a")

			env := ds.Spec.Template.Spec.Containers[0].Env
			Expect(env).To(HaveLen(2))
			Expect(env[0].Name).To(Equal(otlpHeaderEnvName(0)))
			Expect(env[0].Value).To(BeEmpty())
			Expect(env[0].ValueFrom.SecretKeyRef.Name).To(Equal("otlp-auth"))
			Expect(env[0].ValueFrom.SecretKeyRef.Key).To(Equal("token"))
			Expect(env[1].Name).To(Equal(otlpHeaderEnvName(1)))
			Expect(env[1].ValueFrom.SecretKeyRef.Key).To(Equal("tenant"))

			caVol := findVolume(ds, otlpCAVolume)
			Expect(caVol).NotTo(BeNil())
			Expect(caVol.ConfigMap).NotTo(BeNil())
			Expect(caVol.ConfigMap.Name).To(Equal("gateway-ca"))
			Expect(caVol.ConfigMap.Items).To(ConsistOf(core.KeyToPath{Key: "ca-bundle.crt", Path: otlpCAFile}))

			certVol := findVolume(ds, otlpClientCertVolume)
			Expect(certVol).NotTo(BeNil())
			Expect(certVol.Secret).NotTo(BeNil())
			Expect(certVol.Secret.SecretName).To(Equal("otlp-client"))
			Expect(*certVol.Secret.DefaultMode).To(BeEquivalentTo(0o400))

			Expect(findMount(ds, otlpCAVolume)).To(HaveField("MountPath", otlpCAMountDir))
			Expect(findMount(ds, otlpClientCertVolume)).To(HaveField("MountPath", otlpClientCertMountDir))
			Expect(findMount(ds, otlpClientCertVolume)).To(HaveField("ReadOnly", BeTrue()))

			cp.Spec.XpuManagerSpec.OTLP = nil
			controller.updateDaemonSetObject(ds, cp, "", "test-otel-cm", "hash-a")
			Expect(ds.Spec.Template.Spec.Containers[0].Env).To(BeEmpty())
			Expect(findVolume(ds, otlpCAVolume)).To(BeNil())
			Expect(findVolume(ds, otlpClientCertVolume)).To(BeNil())
			Expect(findMount(ds, otlpCAVolume)).To(BeNil())
			Expect(findMount(ds, otlpClientCertVolume)).To(BeNil())
		})
	})

	Context("OTel config generation", func() {
		controller := &XpuManagerReconciler{}

		buildConfig := func(cp *v1alpha.ClusterPolicy) *deployments.OTelConfig {
			data, err := controller.buildOTelConfigData(cp)
			Expect(err).NotTo(HaveOccurred())

			cfg := &deployments.OTelConfig{}
			Expect(yaml.Unmarshal([]byte(data), cfg)).To(Succeed())

			return cfg
		}

		It("keeps info logs and crashlog disabled by default", func() {
			cfg := buildConfig(&v1alpha.ClusterPolicy{})

			Expect(cfg.Receivers.IntelXPU.InfoLogs).NotTo(BeNil())
			Expect(cfg.Receivers.IntelXPU.InfoLogs.Enabled).To(BeFalse())
			Expect(cfg.Receivers.IntelCrashlog).To(BeNil())
			Expect(cfg.Service.Pipelines.Logs.Receivers).NotTo(ContainElement(crashlogReceiver))
		})

		It("enables info logs and crashlog when requested", func() {
			cp := &v1alpha.ClusterPolicy{}
			cp.Spec.XpuManagerSpec.InfoLogs = true
			cp.Spec.XpuManagerSpec.CrashLog = true

			cfg := buildConfig(cp)

			Expect(cfg.Receivers.IntelXPU.InfoLogs.Enabled).To(BeTrue())
			Expect(cfg.Receivers.IntelXPU.InfoLogs.InstanceName).To(Equal("xpumd"))
			Expect(cfg.Receivers.IntelCrashlog).NotTo(BeNil())
			Expect(cfg.Receivers.IntelCrashlog.Directory).To(Equal(crashlogHostDir))
			Expect(cfg.Service.Pipelines.Logs.Receivers).To(ContainElement(crashlogReceiver))
		})

		It("has no OTLP exporter by default", func() {
			cfg := buildConfig(&v1alpha.ClusterPolicy{})

			Expect(cfg.Exporters.OTLPHTTP).To(BeNil())
			Expect(cfg.Exporters.OTLPGRPC).To(BeNil())
			Expect(cfg.Service.Pipelines.Metrics.Exporters).NotTo(ContainElements(otlpHTTPExporter, otlpGRPCExporter))
		})

		It("adds an OTLP/HTTP exporter with secret headers and TLS files", func() {
			cp := &v1alpha.ClusterPolicy{}
			cp.Spec.XpuManagerSpec.OTLP = &v1alpha.OTLPSpec{
				HTTP: &v1alpha.OTLPExporterSpec{
					Endpoint: "https://gateway:4318",
					Headers: []v1alpha.OTLPHeader{{
						Name: "Authorization",
						ValueFrom: core.SecretKeySelector{
							LocalObjectReference: core.LocalObjectReference{Name: "otlp-auth"},
							Key:                  "token",
						},
					}},
					TLS: &v1alpha.OTLPTLSSpec{
						CA: &core.ConfigMapKeySelector{
							LocalObjectReference: core.LocalObjectReference{Name: "gateway-ca"},
							Key:                  "ca.crt",
						},
						ClientCertSecret: &core.LocalObjectReference{Name: "otlp-client"},
						ServerName:       "gateway.example.com",
					},
				},
			}

			data, err := controller.buildOTelConfigData(cp)
			Expect(err).NotTo(HaveOccurred())
			// Only the env reference may end up in the ConfigMap, never the Secret name or value.
			Expect(data).NotTo(ContainSubstring("otlp-auth"))

			cfg := buildConfig(cp)

			Expect(cfg.Exporters.OTLPGRPC).To(BeNil())
			Expect(cfg.Exporters.OTLPHTTP).To(Equal(&deployments.OTLPExporter{
				Endpoint: "https://gateway:4318",
				Headers:  map[string]string{"Authorization": "${env:" + otlpHeaderEnvName(0) + "}"},
				TLS: &deployments.OTLPExporterTLS{
					CAFile:             otlpCAMountDir + "/" + otlpCAFile,
					CertFile:           otlpClientCertMountDir + "/tls.crt",
					KeyFile:            otlpClientCertMountDir + "/tls.key",
					ServerNameOverride: "gateway.example.com",
				},
			}))
			Expect(cfg.Service.Pipelines.Metrics.Exporters).To(ContainElement(otlpHTTPExporter))
			Expect(cfg.Service.Pipelines.Logs.Exporters).To(ContainElement(otlpHTTPExporter))
		})

		It("disables TLS for a plain-text OTLP/gRPC endpoint", func() {
			cp := &v1alpha.ClusterPolicy{}
			cp.Spec.XpuManagerSpec.OTLP = &v1alpha.OTLPSpec{
				GRPC: &v1alpha.OTLPExporterSpec{Endpoint: "http://gateway:4317"},
			}

			cfg := buildConfig(cp)

			Expect(cfg.Exporters.OTLPHTTP).To(BeNil())
			Expect(cfg.Exporters.OTLPGRPC).To(Equal(&deployments.OTLPExporter{
				Endpoint: "http://gateway:4317",
				TLS:      &deployments.OTLPExporterTLS{Insecure: true},
			}))
			Expect(cfg.Service.Pipelines.Metrics.Exporters).To(ContainElement(otlpGRPCExporter))
			Expect(cfg.Service.Pipelines.Logs.Exporters).To(ContainElement(otlpGRPCExporter))
		})

		It("leaves TLS to the defaults for an https endpoint without TLS settings", func() {
			cp := &v1alpha.ClusterPolicy{}
			cp.Spec.XpuManagerSpec.OTLP = &v1alpha.OTLPSpec{
				GRPC: &v1alpha.OTLPExporterSpec{Endpoint: "https://gateway:4317"},
			}

			Expect(buildConfig(cp).Exporters.OTLPGRPC.TLS).To(BeNil())
		})
	})

	Context("OTLP CRD validation", func() {
		dryRunCreate := func(otlp *v1alpha.OTLPSpec) error {
			cp := &v1alpha.ClusterPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: "otlp-validation"},
				Spec: v1alpha.ClusterPolicySpec{
					ResourceRegistration: "dp",
					XpuManagerSpec:       v1alpha.XpuManagerSpec{OTLP: otlp},
				},
			}

			return k8sClient.Create(ctx, cp, client.DryRunAll)
		}

		It("accepts a single transport", func() {
			Expect(dryRunCreate(&v1alpha.OTLPSpec{
				HTTP: &v1alpha.OTLPExporterSpec{Endpoint: "https://gateway:4318"},
			})).To(Succeed())
			Expect(dryRunCreate(&v1alpha.OTLPSpec{
				GRPC: &v1alpha.OTLPExporterSpec{Endpoint: "http://gateway:4317"},
			})).To(Succeed())
		})

		It("rejects both or neither transport", func() {
			err := dryRunCreate(&v1alpha.OTLPSpec{
				HTTP: &v1alpha.OTLPExporterSpec{Endpoint: "https://gateway:4318"},
				GRPC: &v1alpha.OTLPExporterSpec{Endpoint: "https://gateway:4317"},
			})
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "unexpected error: %v", err)
			Expect(err.Error()).To(ContainSubstring("exactly one of http or grpc"))

			err = dryRunCreate(&v1alpha.OTLPSpec{})
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "unexpected error: %v", err)
		})

		It("rejects TLS settings for a plain-text endpoint", func() {
			err := dryRunCreate(&v1alpha.OTLPSpec{
				HTTP: &v1alpha.OTLPExporterSpec{
					Endpoint: "http://gateway:4318",
					TLS:      &v1alpha.OTLPTLSSpec{InsecureSkipVerify: true},
				},
			})
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "unexpected error: %v", err)
		})

		It("rejects an endpoint without http(s) scheme", func() {
			err := dryRunCreate(&v1alpha.OTLPSpec{
				GRPC: &v1alpha.OTLPExporterSpec{Endpoint: "gateway:4317"},
			})
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "unexpected error: %v", err)
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
