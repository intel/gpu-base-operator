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
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	api "github.com/intel/gpu-base-operator/api/v1alpha1"
	"github.com/intel/gpu-base-operator/test/utils"
)

func kustomizeToClusterpolicy(dir string) api.ClusterPolicy {
	cmd := exec.Command("kustomize", "build", dir)
	original, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to build ClusterPolicy kustomize")

	cp := api.ClusterPolicy{}

	err = yaml.Unmarshal([]byte(original), &cp)
	Expect(err).NotTo(HaveOccurred(), "Failed to unmarshal ClusterPolicy YAML")

	return cp
}

func applyClusterPolicyToCluster(cp *api.ClusterPolicy, targetFile string) error {
	updated, err := yaml.Marshal(&cp)
	if err != nil {
		return err
	}

	err = os.WriteFile(targetFile, updated, 0644)
	if err != nil {
		return err
	}

	cmd := exec.Command("kubectl", "apply", "-f", targetFile)
	_, err = utils.Run(cmd)
	if err != nil {
		return err
	}

	return nil
}

func waitForNamedPodToBecomeRunning(name, ns string, g Gomega) {
	// Get the name of the controller-manager pod
	cmd := exec.Command("kubectl", "get",
		"pods", name,
		"-o", "go-template={{ .status.phase }}",
		"-n", ns,
	)

	output, err := utils.Run(cmd)
	g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve pod "+name)
	g.Expect(output).To(Equal("Running"), "Incorrect pod status")
}

func waitForNamedPodToBecomeComplete(name, ns string, g Gomega) {
	// Get the name of the controller-manager pod
	cmd := exec.Command("kubectl", "get",
		"pods", name,
		"-o", "go-template={{ .status.phase }}",
		"-n", ns,
	)

	output, err := utils.Run(cmd)
	g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve pod "+name)
	g.Expect(output).To(Equal("Succeeded"), "Incorrect pod status")
}

func waitForPodsToBecomeRunning(label string, ns string, g Gomega) {
	// Get the name of the controller-manager pod
	cmd := exec.Command("kubectl", "get",
		"pods", "-l", label,
		"-o", "go-template={{ range .items }}"+
			"{{ if not .metadata.deletionTimestamp }}"+
			"{{ .metadata.name }}"+
			"{{ \"\\n\" }}{{ end }}{{ end }}",
		"-n", ns,
	)

	podOutput, err := utils.Run(cmd)
	g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve pods for label "+label)
	podNames := utils.GetNonEmptyLines(podOutput)
	g.Expect(len(podNames)).To(BeNumerically(">=", 1), "expected >=1 pods")

	for _, podName := range podNames {
		// Validate the pod's status
		cmd = exec.Command("kubectl", "get",
			"pods", podName, "-o", "jsonpath={.status.phase}",
			"-n", ns,
		)
		output, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(output).To(Equal("Running"), "Incorrect pod status")
	}
}

func waitForDRAResourceSlices(g Gomega) {
	cmd := exec.Command("kubectl", "get",
		"resourceslices",
		"-o", "go-template={{ range .items }}"+
			"{{ .spec.driver }}"+
			"{{ \"\\n\" }}{{ end }}",
	)
	output, err := utils.Run(cmd)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(output).To(ContainSubstring("gpu.intel.com"), "Expected resource slice gpu.intel.com not found on any node")
}

func waitForPodsToGoAway(label, ns string, g Gomega) {
	// Get the name of the controller-manager pod
	cmd := exec.Command("kubectl", "get",
		"pods", "-l", label,
		"-o", "go-template={{ range .items }}"+
			"{{ .metadata.name }}"+
			"{{ \"\\n\" }}{{ end }}",
		"-n", ns,
	)

	podOutput, err := utils.Run(cmd)
	g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve pods")
	podNames := utils.GetNonEmptyLines(podOutput)
	g.Expect(podNames).To(BeEmpty(), "expected 0 pods")
}

func isDsAvailable(label, ns string) (bool, error) {
	// Get the name of the controller-manager pod
	cmd := exec.Command("kubectl", "get",
		"daemonset", "-l", label,
		"-o", "go-template={{ range .items }}"+
			"{{ if not .metadata.deletionTimestamp }}"+
			"{{ .metadata.name }}"+
			"{{ \"\\n\" }}{{ end }}{{ end }}",
		"-n", ns,
	)
	dsOutput, err := utils.Run(cmd)
	if err != nil {
		return false, err
	}
	dsNames := utils.GetNonEmptyLines(dsOutput)

	return len(dsNames) > 0, nil
}

func waitForNodeResources(resource string, g Gomega) {
	cmd := exec.Command("kubectl", "get",
		"nodes",
		"-o", "go-template={{ range .items }}"+
			"{{ .status.allocatable }}"+
			"{{ \"\\n\" }}{{ end }}",
	)
	output, err := utils.Run(cmd)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(output).To(ContainSubstring(resource), "Expected resource "+resource+" not found on any node")
}

func waitServiceMonitorToAppear(name, ns string, g Gomega) {
	// Get the name of the controller-manager pod
	cmd := exec.Command("kubectl", "get",
		"servicemonitors",
		"-o", "go-template={{ range .items }}"+
			"{{ .metadata.name }}"+
			"{{ \"\\n\" }}{{ end }}",
		"-n", ns,
	)

	smOutput, err := utils.Run(cmd)
	g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve servicemonitors")
	monitorNames := utils.GetNonEmptyLines(smOutput)
	g.Expect(monitorNames).To(ContainElement(name), "expected servicemonitor "+name+" to be present")
}

func waitServicesAppear(name, ns string, g Gomega) {
	// Get the name of the controller-manager pod
	cmd := exec.Command("kubectl", "get",
		"services",
		"-o", "go-template={{ range .items }}"+
			"{{ .metadata.name }}"+
			"{{ \"\\n\" }}{{ end }}",
		"-n", ns,
	)

	smOutput, err := utils.Run(cmd)
	g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve services")
	serviceNames := utils.GetNonEmptyLines(smOutput)
	g.Expect(serviceNames).To(ContainElement(name), "expected service "+name+" to be present")
}

func waitUntilResourceSlicesAreGone(g Gomega) {
	cmd := exec.Command("kubectl", "get", "resourceslices.resource.k8s.io", "--no-headers")
	output, err := utils.Run(cmd)
	if err != nil {
		return
	}

	lines := utils.GetNonEmptyLines(output)
	g.Expect(lines).To(ContainElement("No resources found"), "Expected no ResourceSlices to be present")
}

func waitUntilNamespaceGone(g Gomega, namespace string) {
	cmd := exec.Command("kubectl", "get", "namespace", namespace, "--ignore-not-found", "-o", "name")
	output, err := utils.Run(cmd)
	if err != nil {
		return
	}

	g.Expect(err).NotTo(HaveOccurred(), "Failed to query namespace")
	g.Expect(strings.TrimSpace(output)).To(BeEmpty(), "expected namespace "+namespace+" to be gone")
}

// removeNFDLabels removes all node-feature-discovery labels from every node.
// It is called as cleanup after tests that deploy NFD, because NFD labels
// persist on nodes even after the NFD workloads are deleted.
func removeNFDLabels() {
	nfdPrefixes := []string{
		"feature.node.kubernetes.io/",
		"nfd.node.kubernetes.io/",
	}

	cmd := exec.Command("kubectl", "get", "nodes",
		"-o", "go-template={{ range .items }}{{ .metadata.name }}{{ \"\\n\" }}{{ end }}")

	nodeOutput, err := utils.Run(cmd)
	if err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "warning: failed to list nodes for NFD label cleanup: %v\n", err)
		return
	}

	for _, node := range utils.GetNonEmptyLines(nodeOutput) {
		cmd = exec.Command("kubectl", "get", "node", node, "-o", "json")

		jsonOutput, err := utils.Run(cmd)
		if err != nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "warning: failed to get node %q for NFD label cleanup: %v\n", node, err)
			continue
		}

		var nodeObj struct {
			Metadata struct {
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
		}

		if err = json.Unmarshal([]byte(jsonOutput), &nodeObj); err != nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "warning: failed to parse node %q JSON: %v\n", node, err)
			continue
		}

		var toRemove []string

		for k := range nodeObj.Metadata.Labels {
			for _, prefix := range nfdPrefixes {
				if strings.HasPrefix(k, prefix) {
					toRemove = append(toRemove, k+"-")
					break
				}
			}
		}

		if len(toRemove) == 0 {
			continue
		}

		args := append([]string{"label", "node", node}, toRemove...)
		cmd = exec.Command("kubectl", args...)

		if _, err = utils.Run(cmd); err != nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "warning: failed to remove NFD labels from node %q: %v\n", node, err)
		}
	}
}

func waitForKueueObjectsToClear(g Gomega) {
	cmd := exec.Command("kubectl", "get", "crds", "--no-headers")
	output, err := utils.Run(cmd)
	if err != nil {
		return
	}

	if !strings.Contains(output, "kueue.x-k8s.io") {
		// No Kueue CRDs found, so we can assume all Kueue objects are gone as well
		return
	}

	typesToCheck := []string{
		"clusterqueues.kueue.x-k8s.io",
		"resourceflavors.kueue.x-k8s.io",
	}

	for _, t := range typesToCheck {
		cmd := exec.Command("kubectl", "get", t, "--no-headers")
		output, err := utils.Run(cmd)
		if err != nil {
			return
		}

		lines := utils.GetNonEmptyLines(output)
		g.Expect(lines).To(ContainElement("No resources found"), "Expected no "+t+" to be present")
	}
}

func isNFDInstalled() bool {
	cmd := exec.Command("kubectl", "get", "crds", "nodefeaturerules.nfd.k8s-sigs.io", "--no-headers", "--ignore-not-found")
	output, err := utils.Run(cmd)
	if err != nil {
		return false
	}

	return strings.TrimSpace(output) != ""
}

func createTestNMAPPod(namespace, endPoint string) (string, error) {
	tmpPath, err := os.MkdirTemp("", "testnmappod")
	if err != nil {
		return "", err
	}

	defer os.RemoveAll(tmpPath)

	yamlPath := filepath.Join(tmpPath, "pod.yaml")

	podName := "nmap-tester"
	podSpec := &v1.Pod{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Pod",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: namespace,
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Args: []string{
						"nmap", endPoint, "-p", "1-65535", "--open", "-oG", "-",
					},
					Name:            "nmap",
					Image:           "instrumentisto/nmap",
					ImagePullPolicy: "IfNotPresent",
				},
			},
			RestartPolicy: v1.RestartPolicyNever,
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

func createTestSSLPod(namespace, endpoint string) (string, error) {
	tmpPath, err := os.MkdirTemp("", "testsslpod")
	if err != nil {
		return "", err
	}

	defer os.RemoveAll(tmpPath)

	yamlPath := filepath.Join(tmpPath, "pod.yaml")

	podName := "testssl-tester"
	podSpec := &v1.Pod{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Pod",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: namespace,
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Args: []string{
						"--color=0",
						"--openssl=/usr/bin/openssl",
						"--mapping",
						"iana",
						"-s",
						"-f",
						"-p",
						"-P",
						"-U",
						endpoint},
					Name:            "testssl-container",
					Image:           "drwetter/testssl.sh:3.2",
					ImagePullPolicy: "IfNotPresent",
				},
			},
			RestartPolicy: v1.RestartPolicyNever,
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

func deletePod(namespace, podName string) error {
	cmd := exec.Command("kubectl", "delete", "pod", podName, "-n", namespace)
	_, err := utils.Run(cmd)
	return err
}

func getServiceClusterIP(name, ns string) string {
	cmd := exec.Command("kubectl", "get",
		"service", name,
		"-o", "go-template={{ .spec.clusterIP }}",
		"-n", ns,
	)

	output, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to retrieve service "+name)
	return strings.TrimSpace(output)
}

func getControllerPodIP(ns string) string {
	cmd := exec.Command("kubectl", "get",
		"pods", "-l", "control-plane=gpu-operator-controller-manager",
		"-o", "go-template={{ range .items }}"+
			"{{ if not .metadata.deletionTimestamp }}"+
			"{{ .status.podIP }}"+
			"{{ \"\\n\" }}{{ end }}{{ end }}",
		"-n", ns,
	)

	output, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to retrieve controller pod IP")

	// Get the first IP from the output. There shouldn't ever be more than one.
	outarr := strings.SplitN(output, "\n", 2)
	Expect(outarr).To(HaveLen(2), "Expected exactly one controller manager pod IP")

	return strings.TrimSpace(outarr[0])
}
