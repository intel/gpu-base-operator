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

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	api "github.com/intel/gpu-base-operator/api/v1alpha1"
	"github.com/intel/gpu-base-operator/test/utils"
)

const (
	// namespace where the project is deployed in
	namespace = "intel-gpu-base-operator"

	// Helm chart install names
	helmOperatorName = "gpu-operator"
	helmPolicyName   = "gpu-policy"

	// Helm chart paths
	helmOperatorChartPath = "charts/gpu-base-operator"
	helmPolicyChartPath   = "charts/gpu-base-operator-policy"
)

func devicePluginClusterPolicy() api.ClusterPolicy {
	return kustomizeToClusterpolicy("config/samples/deviceplugin/")
}

func dynamicResourceAllocationClusterPolicy() api.ClusterPolicy {
	return kustomizeToClusterpolicy("config/samples/dra/")
}

func waitForDRAPodsToBecomeRunning(g Gomega) {
	waitForPodsToBecomeRunning("app=intel-gpu-resource-driver-kubelet-plugin", namespace, g)
}

func waitForXPUMPodsToGoAway(g Gomega) {
	waitForPodsToGoAway("app=intel-xpumanager", namespace, g)
}

func waitForDRAPodsToGoAway(g Gomega) {
	waitForPodsToGoAway("app=intel-gpu-resource-driver-kubelet-plugin", namespace, g)
}

func waitForXpumPodsToBecomeRunning(g Gomega) {
	waitForPodsToBecomeRunning("app=intel-xpumanager", namespace, g)
}

func waitForDevicePluginPodsToBecomeRunning(g Gomega) {
	waitForPodsToBecomeRunning("app=intel-gpu-plugin", namespace, g)
}

func waitForPrometheusToBeReady(g Gomega) {
	waitForPodsToBecomeRunning("prometheus=prometheus", "default", g)
}

func waitForDevicePluginNodeResources(g Gomega) {
	waitForNodeResources("gpu.intel.com/", g)
}

func waitForPrometheusServiceMonitors(g Gomega) {
	waitServiceMonitorToAppear("intel-xpumanager", namespace, g)
}

func waitForServices(g Gomega) {
	waitServicesAppear("intel-xpumanager", namespace, g)
}

func waitForTestSSLPodToBecomeComplete(g Gomega) {
	waitForNamedPodToBecomeComplete("testssl-tester", namespace, g)
}

func waitForTestNMAPPodToBecomeComplete(g Gomega) {
	waitForNamedPodToBecomeComplete("nmap-tester", namespace, g)
}

func waitForPrometheusToScrapeXpumanager(g Gomega) {
	podName := "prometheus-prometheus-0"
	jqQuery := "'.data.activeTargets[].labels.service'"
	prometheusUrl := "http://localhost:9090/api/v1/targets"
	cmd := exec.Command("sh", "-c", "kubectl exec "+podName+" -- wget --quiet -O- "+prometheusUrl+" | jq "+jqQuery)

	output, err := utils.Run(cmd)
	g.Expect(err).NotTo(HaveOccurred())

	output = strings.TrimSpace(output)
	g.Expect(output).To(ContainSubstring("intel-xpumanager"), "Prometheus is not scraping the xpumanager service")
}

func waitForNFRsToClear(g Gomega) {
	cmd := exec.Command("kubectl", "get",
		"nodefeaturerules",
		"-o", "go-template={{ range .items }}"+
			"{{ . }}"+
			"{{ \"\\n\" }}{{ end }}",
	)
	output, err := utils.Run(cmd)
	g.Expect(err).NotTo(HaveOccurred())

	output = strings.TrimSpace(output)
	g.Expect(output).To(BeEmpty(), "NodeFeatureRules are still present in the cluster")
}

func isXpumDsAvailable() (bool, error) {
	return isDsAvailable("app=intel-xpumanager", namespace)
}

func launchTestPod(deploymentPath string) error {
	cmd := exec.Command("kubectl", "apply", "-k", deploymentPath)
	_, err := utils.Run(cmd)
	return err
}

func removeTestPod(deploymentPath string) error {
	cmd := exec.Command("kubectl", "delete", "-k", deploymentPath)
	_, err := utils.Run(cmd)
	return err
}

func verifyTestPodRunning(g Gomega) {
	waitForNamedPodToBecomeRunning("gpupod", "default", g)
}

func removeNfdAllTogether() {
	By("removing NFD components")
	cmd := exec.Command("kubectl", "delete", "-k", "config/nfd")
	_, _ = utils.Run(cmd)

	By("removing NFD labels from nodes")
	removeNFDLabels()
}

var controllerPodName string

var _ = Describe("Kubectl apply", Ordered, func() {
	BeforeAll(func() {
		By("deploying the operator as a whole")
		cmd := exec.Command("kubectl", "apply", "-k", "config/default/")
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create operator deployment")

		By("waiting for the operator's controller-manager pod to be available")
		cmd = exec.Command("kubectl", "wait", "deployment.apps/intel-gpu-base-operator-controller-manager",
			"--for", "condition=Available",
			"--namespace", namespace,
			"--timeout", "1m",
		)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Controller deployment did not become available in time")
	})

	AfterAll(func() {
		By("cleaning up operator deployment")
		cmd := exec.Command("kubectl", "delete", "-k", "config/default")
		_, _ = utils.Run(cmd)
	})

	// After each test, check for failures and collect logs, events,
	// and pod descriptions for debugging.
	AfterEach(func() {
		specReport := CurrentSpecReport()
		if specReport.Failed() {
			By("fetching controller manager pod logs")
			cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
			controllerLogs, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Controller logs:\n %s", controllerLogs)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Controller logs: %s", err)
			}

			By("fetching Kubernetes events")
			cmd = exec.Command("kubectl", "get", "events", "-n", namespace, "--sort-by=.lastTimestamp")
			eventsOutput, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Kubernetes events:\n%s", eventsOutput)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Kubernetes events: %s", err)
			}

			By("fetching controller manager pod description")
			cmd = exec.Command("kubectl", "describe", "pod", controllerPodName, "-n", namespace)
			podDescription, err := utils.Run(cmd)
			if err == nil {
				fmt.Println("Pod description:\n", podDescription)
			} else {
				fmt.Println("Failed to describe controller pod")
			}
		}
	})

	AfterEach(func() {
		removeNfdAllTogether()

		By("remove gpu resource slices after each test")
		cmd := exec.Command("kubectl", "delete", "resourceslices", "--all", "--wait")
		_, _ = utils.Run(cmd)

		Eventually(waitUntilResourceSlicesAreGone).Should(Succeed())
	})

	SetDefaultEventuallyTimeout(2 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)

	Context("Operator installation", Label("core"), func() {
		It("should run successfully", func() {
			By("validating that the controller-manager pod is running as expected")
			verifyControllerUp := func(g Gomega) {
				// Get the name of the controller-manager pod
				cmd := exec.Command("kubectl", "get",
					"pods", "-l", "control-plane=controller-manager",
					"-o", "go-template={{ range .items }}"+
						"{{ if not .metadata.deletionTimestamp }}"+
						"{{ .metadata.name }}"+
						"{{ \"\\n\" }}{{ end }}{{ end }}",
					"-n", namespace,
				)

				podOutput, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve controller-manager pod information")
				podNames := utils.GetNonEmptyLines(podOutput)
				g.Expect(podNames).To(HaveLen(1), "expected 1 controller pod running")
				controllerPodName = podNames[0]
				g.Expect(controllerPodName).To(ContainSubstring("controller-manager"))

				// Validate the pod's status
				cmd = exec.Command("kubectl", "get",
					"pods", controllerPodName, "-o", "jsonpath={.status.phase}",
					"-n", namespace,
				)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"), "Incorrect controller-manager pod status")
			}
			Eventually(verifyControllerUp).Should(Succeed())
		})
	})

	Context("ClusterPolicy to", Label("deviceplugin"), func() {
		AfterEach(func() {
			cmd := exec.Command("kubectl", "delete", "clusterpolicy", "gpu-policy")
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
		})

		It("deploy Device Plugin without NFD and XPUM", func() {
			tmpDir, err := os.MkdirTemp("/tmp/", "operator-deviceplugin*")
			if err != nil {
				Fail(fmt.Sprintf("couldn't create temporary directory: %+v", err))
			}
			defer os.RemoveAll(tmpDir)

			cp := devicePluginClusterPolicy()

			cp.Spec.UseNFDLabeling = false
			cp.Spec.ResourceMonitoring = false

			err = applyClusterPolicyToCluster(&cp, filepath.Join(tmpDir, "clusterpolicy.yaml"))
			Expect(err).NotTo(HaveOccurred(), "Failed to apply ClusterPolicy")

			By("validating that the device plugin daemonset is running as expected")
			Eventually(waitForDevicePluginPodsToBecomeRunning).Should(Succeed())

			By("validating that no xpumanager daemonset exists")
			found, err := isXpumDsAvailable()
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeFalse())
		})

		It("deploy Device Plugin with XPUM", Label("xe", "xpum"), func() {
			tmpDir, err := os.MkdirTemp("/tmp/", "operator-deviceplugin*")
			if err != nil {
				Fail(fmt.Sprintf("couldn't create temporary directory: %+v", err))
			}
			defer os.RemoveAll(tmpDir)

			cp := devicePluginClusterPolicy()

			cp.Spec.UseNFDLabeling = false
			cp.Spec.XpuManagerSpec.MonitoringResource = "monitoring"

			err = applyClusterPolicyToCluster(&cp, filepath.Join(tmpDir, "clusterpolicy.yaml"))
			Expect(err).NotTo(HaveOccurred(), "Failed to apply ClusterPolicy")

			By("validating that the device plugin daemonset is running as expected")
			Eventually(waitForDevicePluginPodsToBecomeRunning).Should(Succeed())

			By("validate that the GPU resources are registered on the node")
			Eventually(waitForDevicePluginNodeResources).Should(Succeed())

			By("validating that the xpumd pods are running as expected")
			Eventually(waitForXpumPodsToBecomeRunning).Should(Succeed())

			By("launching test pod requesting xe resource")
			podDeployment := "config/test/dp-xe"
			err = launchTestPod(podDeployment)
			Expect(err).NotTo(HaveOccurred())

			defer removeTestPod(podDeployment)

			By("validating that gpu xe test pod is running as expected")
			Eventually(verifyTestPodRunning).Should(Succeed())
		})

		It("deploy Device Plugin with NFD without XPUM", Label("nfd"), func() {
			tmpDir, err := os.MkdirTemp("/tmp/", "operator-deviceplugin*")
			if err != nil {
				Fail(fmt.Sprintf("couldn't create temporary directory: %+v", err))
			}
			defer os.RemoveAll(tmpDir)

			cmd := exec.Command("kubectl", "apply", "-k", "config/nfd")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to apply NFD configuration")

			cp := devicePluginClusterPolicy()
			cp.Spec.UseNFDLabeling = true
			cp.Spec.ResourceMonitoring = false

			err = applyClusterPolicyToCluster(&cp, filepath.Join(tmpDir, "clusterpolicy.yaml"))
			Expect(err).NotTo(HaveOccurred(), "Failed to apply ClusterPolicy")

			By("validating that the device plugin daemonset is running as expected")
			Eventually(waitForDevicePluginPodsToBecomeRunning).Should(Succeed())

			By("validate that the GPU resources are registered on the node")
			Eventually(waitForDevicePluginNodeResources).Should(Succeed())

			By("validating that no xpumanager daemonset exists")
			found, err := isXpumDsAvailable()
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeFalse())
		})
	})

	Context("ClusterPolicy", Label("dra"), func() {
		AfterEach(func() {
			cmd := exec.Command("kubectl", "delete", "clusterpolicy", "gpu-policy")
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
		})

		It("deploy GPU DRA without NFD and XPUM", func() {
			tmpDir, err := os.MkdirTemp("/tmp/", "operator-deviceplugin*")
			if err != nil {
				Fail(fmt.Sprintf("couldn't create temporary directory: %+v", err))
			}
			defer os.RemoveAll(tmpDir)

			cp := dynamicResourceAllocationClusterPolicy()

			cp.Spec.UseNFDLabeling = false
			cp.Spec.ResourceMonitoring = false

			err = applyClusterPolicyToCluster(&cp, filepath.Join(tmpDir, "clusterpolicy.yaml"))
			Expect(err).NotTo(HaveOccurred(), "Failed to apply ClusterPolicy")

			By("validating that the DRA daemonset is running as expected")
			Eventually(waitForDRAPodsToBecomeRunning).Should(Succeed())

			By("validating that no xpumanager daemonset exists")
			found, err := isXpumDsAvailable()
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeFalse())

			By("launching test pod requesting DRA resource claim")
			podDeployment := "config/test/dra"
			err = launchTestPod(podDeployment)
			Expect(err).NotTo(HaveOccurred())

			defer removeTestPod(podDeployment)

			By("validating that DRA test pod is running as expected")
			Eventually(verifyTestPodRunning).Should(Succeed())
		})

		It("deploy GPU DRA and XPUM", Label("xpum"), func() {
			tmpDir, err := os.MkdirTemp("/tmp/", "operator-deviceplugin*")
			if err != nil {
				Fail(fmt.Sprintf("couldn't create temporary directory: %+v", err))
			}
			defer os.RemoveAll(tmpDir)

			cp := dynamicResourceAllocationClusterPolicy()

			cp.Spec.UseNFDLabeling = false
			cp.Spec.ResourceMonitoring = true

			err = applyClusterPolicyToCluster(&cp, filepath.Join(tmpDir, "clusterpolicy.yaml"))
			Expect(err).NotTo(HaveOccurred(), "Failed to apply ClusterPolicy")

			By("validating that the DRA daemonset is running as expected")
			Eventually(waitForDRAPodsToBecomeRunning).Should(Succeed())

			By("validating that the DRA resource slices are created")
			Eventually(waitForDRAResourceSlices).Should(Succeed())

			By("validating that xpumanager pod is running as expected")
			Eventually(waitForXpumPodsToBecomeRunning, 3*time.Minute).Should(Succeed())
		})

		It("deploy GPU DRA with XPUM and NFD", Label("xpum", "nfd"), func() {
			tmpDir, err := os.MkdirTemp("/tmp/", "operator-deviceplugin*")
			if err != nil {
				Fail(fmt.Sprintf("couldn't create temporary directory: %+v", err))
			}
			defer os.RemoveAll(tmpDir)

			cmd := exec.Command("kubectl", "apply", "-k", "config/nfd")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to apply NFD configuration")

			cp := dynamicResourceAllocationClusterPolicy()

			cp.Spec.UseNFDLabeling = true
			cp.Spec.ResourceMonitoring = true

			err = applyClusterPolicyToCluster(&cp, filepath.Join(tmpDir, "clusterpolicy.yaml"))
			Expect(err).NotTo(HaveOccurred(), "Failed to apply ClusterPolicy")

			By("validating that the DRA daemonset is running as expected")
			Eventually(waitForDRAPodsToBecomeRunning).Should(Succeed())

			By("validating that the DRA resource slices are created")
			Eventually(waitForDRAResourceSlices).Should(Succeed())

			By("validating that xpumanager pod is running as expected")
			Eventually(waitForXpumPodsToBecomeRunning, 3*time.Minute).Should(Succeed())
		})

		It("deploy GPU DRA with XPUM, NFD and Health", Label("xpum", "nfd", "health"), func() {
			tmpDir, err := os.MkdirTemp("/tmp/", "operator-deviceplugin*")
			if err != nil {
				Fail(fmt.Sprintf("couldn't create temporary directory: %+v", err))
			}
			defer os.RemoveAll(tmpDir)

			cmd := exec.Command("kubectl", "apply", "-k", "config/nfd")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to apply NFD configuration")

			cp := dynamicResourceAllocationClusterPolicy()

			cp.Spec.UseNFDLabeling = true
			cp.Spec.ResourceMonitoring = true
			cp.Spec.HealthinessSpec.CheckIntervalSeconds = 10
			cp.Spec.HealthinessSpec.CoreTemperatureThreshold = 99
			cp.Spec.HealthinessSpec.MemoryTemperatureThreshold = 99

			err = applyClusterPolicyToCluster(&cp, filepath.Join(tmpDir, "clusterpolicy.yaml"))
			Expect(err).NotTo(HaveOccurred(), "Failed to apply ClusterPolicy")

			By("validating that the DRA daemonset is running as expected")
			Eventually(waitForDRAPodsToBecomeRunning).Should(Succeed())

			By("validating that the DRA resource slices are created")
			Eventually(waitForDRAResourceSlices).Should(Succeed())

			By("validating that xpumanager pod is running as expected")
			Eventually(waitForXpumPodsToBecomeRunning, 3*time.Minute).Should(Succeed())
		})
	})
})

var _ = Describe("Helm", Ordered, Label("helm"), func() {
	Context("install", func() {
		operatorInstallArgsBase := []string{"install", "--create-namespace", "-n", namespace, helmOperatorName, helmOperatorChartPath, "--wait"}

		BeforeAll(func() {
			By("update helm dependencies")
			cmd := exec.Command("helm", "dep", "update", helmOperatorChartPath)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to update helm dependencies")
		})

		AfterEach(func() {
			By("remove clusterpolicy")
			cmd := exec.Command("helm", "uninstall", "-n", namespace, helmPolicyName, "--wait")
			utils.Run(cmd)

			// TODO: Find a better way to ensure that the xpumanager pods are gone
			By("wait for the xpumanager pods to vanish")
			Eventually(waitForXPUMPodsToGoAway, time.Second*60).Should(Succeed())

			// FIXME: Removal of the clusterpolicy should hold until DRA is fully removed.
			By("wait for the dra pods to vanish")
			Eventually(waitForDRAPodsToGoAway, time.Second*60).Should(Succeed())

			if isNFDInstalled() {
				By("ensure that the Node Feature Rules are gone")
				Eventually(waitForNFRsToClear, time.Second*15).Should(Succeed())
			}

			By("ensure kueue crds are removed")
			Eventually(waitForKueueObjectsToClear, time.Second*60).Should(Succeed())

			By("remove operator")
			cmd = exec.Command("helm", "uninstall", "-n", namespace, helmOperatorName, "--wait")
			utils.Run(cmd)

			By("remove gpu resource slices after each test")
			cmd = exec.Command("kubectl", "delete", "resourceslices", "--all")
			_, _ = utils.Run(cmd)

			Eventually(waitUntilResourceSlicesAreGone).Should(Succeed())

			removeNfdAllTogether()
		})

		It("DRA with defaults", Label("dra", "xpum", "nfd"), func() {
			By("install operator helm chart")
			cmd := exec.Command("helm", append(operatorInstallArgsBase, "--set", "nfd.install=true")...)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to deploy operator helm chart")

			By("install policy helm chart")
			cmd = exec.Command("helm", "install", "-n", namespace, helmPolicyName, helmPolicyChartPath,
				"--wait", "--set", "resourceRegistration=dra")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to deploy operator policy helm chart")

			By("validating that the DRA daemonset is running as expected")
			Eventually(waitForDRAPodsToBecomeRunning).Should(Succeed())

			By("validating that the DRA resource slices are created")
			Eventually(waitForDRAResourceSlices).Should(Succeed())

			By("validating that the xpumanager is running as expected")
			Eventually(waitForXpumPodsToBecomeRunning, 3*time.Minute).Should(Succeed())

			By("launching test pod requesting DRA resource claim")
			podDeployment := "config/test/dra"
			err = launchTestPod(podDeployment)
			Expect(err).NotTo(HaveOccurred())

			defer removeTestPod(podDeployment)

			By("validating that DRA test pod is running as expected")
			Eventually(verifyTestPodRunning).Should(Succeed())
		})

		It("DRA with prometheus integration", Label("dra", "xpum", "prometheus", "long", "nfd"), func() {
			By("install Prometheus")
			err := utils.InstallPrometheusOperator()
			Expect(err).NotTo(HaveOccurred(), "Failed to install Prometheus")
			defer func() {
				utils.UninstallPrometheusOperator()
			}()

			By("validating that Prometheus is running")
			Eventually(waitForPrometheusToBeReady).Should(Succeed())

			By("install operator helm chart")
			cmd := exec.Command("helm", append(operatorInstallArgsBase, "--set", "nfd.install=true")...)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to deploy operator helm chart")

			By("install policy helm chart")
			cmd = exec.Command("helm", "install", "-n", namespace, helmPolicyName, helmPolicyChartPath,
				"--wait", "--set", "resourceRegistration=dra", "--set", "prometheusIntegration=true",
				"--set", "useNFDLabeling=true")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to deploy operator policy helm chart")

			By("validating that the DRA daemonset is running as expected")
			Eventually(waitForDRAPodsToBecomeRunning).Should(Succeed())

			By("validating that the DRA resource slices are created")
			Eventually(waitForDRAResourceSlices).Should(Succeed())

			By("validating that the xpumanager is running as expected")
			Eventually(waitForXpumPodsToBecomeRunning, 3*time.Minute).Should(Succeed())

			By("verifying that Service is created")
			Eventually(waitForServices).Should(Succeed())

			By("verifying that Prometheus ServiceMonitor is created")
			Eventually(waitForPrometheusServiceMonitors).Should(Succeed())

			By("verify that xpu-manager is scraped by Prometheus")
			Eventually(waitForPrometheusToScrapeXpumanager).Should(Succeed())
		})

		It("check operator's TLS cipher selection", Label("helm", "tls", "long"), func() {
			By("install operator helm chart")
			cmd := exec.Command("helm", operatorInstallArgsBase...)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to deploy operator helm chart")

			By("deploy testssl.sh pod")

			serviceName := "intel-gpu-base-operator-webhook-service"
			serviceIP := getServiceClusterIP(serviceName, namespace)

			podName, err := createTestSSLPod(namespace, serviceIP)
			Expect(err).NotTo(HaveOccurred(), "Failed to deploy testssl.sh pod")

			defer func() {
				err := deletePod(namespace, podName)
				Expect(err).NotTo(HaveOccurred(), "Failed to delete testssl.sh pod")
			}()

			Eventually(waitForTestSSLPodToBecomeComplete).WithTimeout(5 * time.Minute).Should(Succeed())

			By("checking the TLS ciphers used by the operator's webhook")
			cmd = exec.Command("kubectl", "logs", podName, "-n", namespace)
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to get logs from testssl.sh pod")

			By(output)

			// TLS
			Expect(output).To(ContainSubstring("TLS 1.1    not offered"))
			Expect(output).To(ContainSubstring("TLS 1.3    not offered"))
			Expect(output).To(Not(ContainSubstring("TLS 1.2    not offered")))
			Expect(output).To(ContainSubstring("TLS 1.2    offered (OK)"))

			// No http/2
			Expect(output).To(ContainSubstring("ALPN/HTTP2 http/1.1 (offered)"))
		})

		It("check operator's open ports", Label("helm", "ports", "long"), func() {
			By("install operator helm chart")
			cmd := exec.Command("helm", operatorInstallArgsBase...)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to deploy operator helm chart")

			By("deploy nmap pod")
			nmapPod, err := createTestNMAPPod(namespace, getControllerPodIP(namespace))
			Expect(err).NotTo(HaveOccurred(), "Failed to deploy testnmap pod")

			defer func() {
				err := deletePod(namespace, nmapPod)
				Expect(err).NotTo(HaveOccurred(), "Failed to delete testnmap pod")
			}()

			Eventually(waitForTestNMAPPodToBecomeComplete).WithTimeout(2 * time.Minute).Should(Succeed())

			By("checking NMAP scan output")
			cmd = exec.Command("kubectl", "logs", nmapPod, "-n", namespace)
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to get logs from testnmap pod")

			By(output)

			allowedPorts := map[string]bool{
				"9443": true, // webhook
				"8081": true, // healthz
			}
			foundPorts := map[string]bool{}

			// Extract open ports from nmap output using regex
			// Host: 10.244.0.64 (<service>)	Ports:
			//   8081/open/tcp//blackice-icecap///, 9443/open/tcp//tungsten-https///	Ignored State: closed (65533)
			//   ^^^^                               ^^^^
			portRe := regexp.MustCompile(`(\d+)/open`)

			matches := portRe.FindAllStringSubmatch(string(output), -1)
			Expect(matches).NotTo(BeEmpty(), "No open ports found in nmap output")

			for _, match := range matches {
				port := match[1]
				Expect(port).To(BeKeyOf(allowedPorts), "unexpected open port: %s", port)

				foundPorts[port] = true
			}

			for port := range allowedPorts {
				Expect(foundPorts).To(HaveKey(port), "expected open port not found in nmap output: %s", port)
			}
		})

		It("device plugin with xpumd", Label("deviceplugin", "xpum"), func() {
			By("install operator helm chart")
			cmd := exec.Command("helm", operatorInstallArgsBase...)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to deploy operator helm chart")

			By("install policy helm chart")
			cmd = exec.Command("helm", "install", "-n", namespace, helmPolicyName, helmPolicyChartPath,
				"--wait", "--set", "resourceRegistration=dp", "--set", "useNFDLabeling=false", "--set", "xpu.monitoringResource=monitoring")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to deploy operator policy helm chart")

			By("validating that the xpumanager is running as expected")
			Eventually(waitForXpumPodsToBecomeRunning, 3*time.Minute).Should(Succeed())

			By("launching test pod requesting device plugin resources")
			podDeployment := "config/test/dp-xe"
			err = launchTestPod(podDeployment)
			Expect(err).NotTo(HaveOccurred())

			defer removeTestPod(podDeployment)

			By("validating that dp test pod is running as expected")
			Eventually(verifyTestPodRunning).Should(Succeed())
		})

		It("Kueue with DRA", Label("dra", "kueue", "nfd"), func() {
			By("install operator helm chart")
			cmd := exec.Command("helm", append(operatorInstallArgsBase, "--set", "kueue.install=true", "--set", "nfd.install=true")...)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to deploy operator helm chart")

			By("install policy helm chart")
			cmd = exec.Command("helm", "install", "-n", namespace, helmPolicyName, helmPolicyChartPath,
				"--wait", "--set", "resourceRegistration=dra", "--set", "enableKueue=true",
				"--set", "resourceMonitoring=false", "--set", "useNFDLabeling=true")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to deploy operator policy helm chart")

			By("validating that the DRA daemonset is running as expected")
			Eventually(waitForDRAPodsToBecomeRunning).Should(Succeed())

			By("validating that the DRA resource slices are created")
			Eventually(waitForDRAResourceSlices).Should(Succeed())

			By("launching test pod requesting DRA resource claim via kueue queue")
			podDeployment := "config/test/dra-kueue"
			err = launchTestPod(podDeployment)
			Expect(err).NotTo(HaveOccurred())

			defer removeTestPod(podDeployment)

			By("validating that DRA test pod is running as expected")
			Eventually(verifyTestPodRunning).Should(Succeed())
		})
	})
})
