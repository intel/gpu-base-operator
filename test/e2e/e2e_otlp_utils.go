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

package e2e

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"

	. "github.com/onsi/gomega"

	api "github.com/intel/gpu-base-operator/api/v1alpha1"
	"github.com/intel/gpu-base-operator/test/utils"
)

const (
	otlpGatewayNamespace = "default"
	// OTLP gateway that receives the xpumd exports
	otlpGatewayDeployment = "config/test/otlp-gateway/cleartext"
	// OTLP gateway that requires TLS and a bearer token
	otlpTLSGatewayDeployment = "config/test/otlp-gateway/tls"

	otlpTLSGatewayHost         = "otlp-gateway.default.svc"
	otlpGatewayEndpoint        = "http://" + otlpTLSGatewayHost + ":4318"
	otlpGatewayGRPCEndpoint    = "http://" + otlpTLSGatewayHost + ":4317"
	otlpTLSGatewayEndpoint     = "https://" + otlpTLSGatewayHost + ":4318"
	otlpTLSGatewayGRPCEndpoint = "https://" + otlpTLSGatewayHost + ":4317"

	// objects in the operator namespace that the ClusterPolicy refers to
	otlpCAConfigMap  = "otlp-ca"
	otlpAuthSecret   = "otlp-auth"
	otlpAuthTokenKey = "authorization"

	// host directory watched by the xpumd crashlog receiver
	crashlogDir = "/var/log/crashlog"
)

func otlpXpumClusterPolicy(crashLog bool) api.ClusterPolicy {
	cp := dynamicResourceAllocationClusterPolicy()

	cp.Spec.UseNFDLabeling = false
	cp.Spec.ResourceMonitoring = true
	cp.Spec.XpuManagerSpec.CrashLog = crashLog
	cp.Spec.XpuManagerSpec.OTLP = &api.OTLPSpec{
		HTTP: &api.OTLPExporterSpec{Endpoint: otlpGatewayEndpoint},
	}

	return cp
}

func otlpTLSXpumClusterPolicy(grpc bool) api.ClusterPolicy {
	cp := dynamicResourceAllocationClusterPolicy()

	exporter := &api.OTLPExporterSpec{
		Endpoint: otlpTLSGatewayEndpoint,
		Headers: []api.OTLPHeader{{
			Name: "Authorization",
			ValueFrom: corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: otlpAuthSecret},
				Key:                  otlpAuthTokenKey,
			},
		}},
		TLS: &api.OTLPTLSSpec{
			CA: &corev1.ConfigMapKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: otlpCAConfigMap},
				Key:                  "ca.crt",
			},
		},
	}

	cp.Spec.UseNFDLabeling = false
	cp.Spec.ResourceMonitoring = true
	cp.Spec.XpuManagerSpec.OTLP = &api.OTLPSpec{HTTP: exporter}

	if grpc {
		exporter.Endpoint = otlpTLSGatewayGRPCEndpoint
		cp.Spec.XpuManagerSpec.OTLP = &api.OTLPSpec{GRPC: exporter}
	}

	return cp
}

// otlpTLSGatewayOverlay is the kustomize overlay that adds the bearer token Secrets to the
// OTLP TLS gateway deployment. Kustomize only accepts relative resource paths, hence the %s.
const otlpTLSGatewayOverlay = `resources:
- %s
generatorOptions:
  disableNameSuffixHash: true
secretGenerator:
- name: otlp-gateway-auth
  namespace: ` + otlpGatewayNamespace + `
  files:
  - token
- name: ` + otlpAuthSecret + `
  namespace: ` + namespace + `
  files:
  - ` + otlpAuthTokenKey + `
`

// deployOTLPTLSGateway deploys the OTLP gateway that requires TLS and a bearer token. The
// gateway certificate is issued by cert-manager, and the random bearer token is added, for
// both the gateway and as the Authorization header for the operator, by a kustomize overlay
// written into overlayDir. Finally, the CA certificate is copied to the operator namespace.
func deployOTLPTLSGateway(overlayDir string) {
	tokenBytes := make([]byte, 32)
	_, err := rand.Read(tokenBytes)
	Expect(err).NotTo(HaveOccurred())

	token := hex.EncodeToString(tokenBytes)

	projectDir, err := utils.GetProjectDir()
	Expect(err).NotTo(HaveOccurred())

	base, err := filepath.Rel(overlayDir, filepath.Join(projectDir, otlpTLSGatewayDeployment))
	Expect(err).NotTo(HaveOccurred())

	files := map[string]string{
		"kustomization.yaml": fmt.Sprintf(otlpTLSGatewayOverlay, base),
		"token":              token,
		otlpAuthTokenKey:     "Bearer " + token,
	}

	for name, content := range files {
		err := os.WriteFile(filepath.Join(overlayDir, name), []byte(content), 0600)
		Expect(err).NotTo(HaveOccurred(), "Failed to write the OTLP TLS gateway overlay")
	}

	cmd := exec.Command("kubectl", "apply", "-k", overlayDir)
	_, err = utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to deploy the OTLP gateway")

	cmd = exec.Command("kubectl", "wait", "certificate/otlp-gateway-tls", "-n", otlpGatewayNamespace,
		"--for=condition=Ready", "--timeout=2m")
	_, err = utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "cert-manager did not issue the OTLP gateway certificate")

	cmd = exec.Command("kubectl", "get", "secret", "otlp-gateway-tls", "-n", otlpGatewayNamespace,
		"-o", `jsonpath={.data.ca\.crt}`)
	output, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to get the OTLP gateway CA certificate")

	caCert, err := base64.StdEncoding.DecodeString(strings.TrimSpace(output))
	Expect(err).NotTo(HaveOccurred())
	Expect(caCert).NotTo(BeEmpty(), "OTLP gateway certificate Secret has no ca.crt")

	caPath := filepath.Join(overlayDir, "ca.crt")
	Expect(os.WriteFile(caPath, caCert, 0600)).To(Succeed())

	cmd = exec.Command("kubectl", "create", "configmap", otlpCAConfigMap, "-n", namespace,
		"--from-file=ca.crt="+caPath)
	_, err = utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to create the OTLP CA ConfigMap")
}

// removeOTLPTLSGateway removes everything deployOTLPTLSGateway created, including the
// Secrets of the issued certificates, which cert-manager leaves behind.
func removeOTLPTLSGateway(overlayDir string) {
	cmds := [][]string{
		{"delete", "-k", overlayDir, "--ignore-not-found"},
		{"delete", "secret", "otlp-gateway-ca", "otlp-gateway-tls", "-n", otlpGatewayNamespace, "--ignore-not-found"},
		{"delete", "configmap", otlpCAConfigMap, "-n", namespace, "--ignore-not-found"},
	}

	for _, args := range cmds {
		_, _ = utils.Run(exec.Command("kubectl", args...))
	}
}

func waitForOTLPGatewayToBecomeReady(g Gomega) {
	waitForPodsToBecomeReady("app=otlp-gateway", otlpGatewayNamespace, g)
}

func getOTLPGatewayLogs() (string, error) {
	cmd := exec.Command("kubectl", "logs", "deployment/otlp-gateway", "-n", otlpGatewayNamespace)
	return utils.Run(cmd)
}

func waitForOTLPGatewayToReceiveXpumMetrics(g Gomega) {
	output, err := getOTLPGatewayLogs()
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(output).To(ContainSubstring("-> Name: hw.gpu.info"), "OTLP gateway has not received metrics from xpumd")
}

func waitForOTLPGatewayToReceiveCrashlog(fileName, content string) func(Gomega) {
	return func(g Gomega) {
		output, err := getOTLPGatewayLogs()
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(output).To(ContainSubstring("-> file.name: Str("+fileName+")"),
			"OTLP gateway has not received the crashlog record from xpumd")
		// The debug exporter prints the raw file contents base64 encoded.
		g.Expect(output).To(ContainSubstring("Body: Bytes("+base64.StdEncoding.EncodeToString([]byte(content))+")"),
			"crashlog record body does not match the crashlog file contents")
	}
}

func getXpumPodNodeName() string {
	cmd := exec.Command("kubectl", "get",
		"pods", "-l", "app=intel-xpumanager",
		"-o", "jsonpath={.items[0].spec.nodeName}",
		"-n", namespace,
	)

	output, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to retrieve xpumanager pod node")
	return strings.TrimSpace(output)
}

// writeCrashlogFile creates the file atomically, as the crashlog receiver expects.
func writeCrashlogFile(podName, fileName, content string) error {
	tmpFile := crashlogDir + "/." + fileName + ".tmp"
	cmd := exec.Command("kubectl", "exec", podName, "-n", otlpGatewayNamespace, "--",
		"sh", "-c", fmt.Sprintf("printf '%%s' '%s' > %s && mv %s %s/%s", content, tmpFile, tmpFile, crashlogDir, fileName))
	_, err := utils.Run(cmd)
	return err
}

func removeCrashlogFile(podName, fileName string) {
	cmd := exec.Command("kubectl", "exec", podName, "-n", otlpGatewayNamespace, "--",
		"rm", "-f", crashlogDir+"/"+fileName)
	_, _ = utils.Run(cmd)
}

// createCrashlogWriterPod creates a pod on the given node that has the node's crashlog
// directory mounted, so that dummy crashlog files can be written there with kubectl exec.
func createCrashlogWriterPod(namespace, nodeName, crashlogDir string) (string, error) {
	tmpPath, err := os.MkdirTemp("", "crashlogwriterpod")
	if err != nil {
		return "", err
	}

	defer os.RemoveAll(tmpPath)

	yamlPath := filepath.Join(tmpPath, "pod.yaml")

	podName := "crashlog-writer"
	podSpec := &corev1.Pod{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Pod",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: namespace,
		},
		Spec: corev1.PodSpec{
			NodeName: nodeName,
			// sleep runs as PID 1 and ignores SIGTERM, so skip the grace period on delete.
			TerminationGracePeriodSeconds: ptr.To(int64(0)),
			Containers: []corev1.Container{
				{
					Command:         []string{"sleep", "infinity"},
					Name:            "writer",
					Image:           "busybox:1.37",
					ImagePullPolicy: "IfNotPresent",
					VolumeMounts: []corev1.VolumeMount{
						{
							Name:      "crashlog",
							MountPath: crashlogDir,
						},
					},
				},
			},
			Volumes: []corev1.Volume{
				{
					Name: "crashlog",
					VolumeSource: corev1.VolumeSource{
						HostPath: &corev1.HostPathVolumeSource{
							Path: crashlogDir,
							Type: ptr.To(corev1.HostPathDirectoryOrCreate),
						},
					},
				},
			},
			RestartPolicy: corev1.RestartPolicyNever,
		},
	}

	yamlData, err := yaml.Marshal(podSpec)
	if err != nil {
		return "", err
	}

	err = os.WriteFile(yamlPath, yamlData, 0644)
	if err != nil {
		return "", err
	}

	cmd := exec.Command("kubectl", "apply", "-f", yamlPath)
	_, err = utils.Run(cmd)
	if err != nil {
		return "", err
	}

	return podName, nil
}
