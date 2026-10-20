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

// Helpers shared by the specs that run Jobs against a real GPU: firmware update and recovery.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	batch "k8s.io/api/batch/v1"
	core "k8s.io/api/core/v1"
	resv1 "k8s.io/api/resource/v1"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/intel/gpu-base-operator/internal/controller/common"
	"github.com/intel/gpu-base-operator/test/utils"
)

const (
	// Built by 'make docker-build-e2e'.
	fwUpdaterMockImage = "ghcr.io/intel/gpu-fwupdater-mock:devel"

	registryImage     = "registry:2"
	registryNamespace = "fwupdate-e2e-registry"
	// registryNodePort is where the registry is reached from the host (to push), from the node's
	// containerd (to pull) and from the operator (to verify), all through the node's IP. That IP is
	// RFC1918, which go-containerregistry talks plain HTTP to, so the registry needs no TLS.
	registryNodePort = 30500
)

// gpuDevice is one GPU as its DRA ResourceSlice describes it.
type gpuDevice struct {
	pciID string
	bdf   string
}

// deployOperatorWithRegistry loads the mock updater and registry images on Kind and deploys the
// operator.
func deployOperatorWithRegistry() {
	By("loading the mock updater and registry images on Kind")
	Expect(utils.LoadImageToKindClusterWithName(fwUpdaterMockImage)).To(Succeed(),
		"Failed to load the mock updater image; build it with 'make docker-build-e2e'")

	_, err := utils.Run(exec.Command("docker", "pull", registryImage))
	Expect(err).NotTo(HaveOccurred())
	Expect(utils.LoadImageToKindClusterWithName(registryImage)).To(Succeed())

	By("deploying the operator")
	_, err = utils.Run(exec.Command("kubectl", "apply", "-k", "config/default/"))
	Expect(err).NotTo(HaveOccurred(), "Failed to create operator deployment")

	_, err = utils.Run(exec.Command("kubectl", "wait", "deployment.apps/intel-gpu-base-operator-controller-manager",
		"--for", "condition=Available", "--namespace", namespace, "--timeout", "1m"))
	Expect(err).NotTo(HaveOccurred(), "Controller deployment did not become available in time")
}

// startRegistry runs the in-cluster registry the operator verifies Job images against.
func startRegistry() {
	By("starting an in-cluster registry")
	Expect(applyManifest(registryManifest())).To(Succeed())

	_, err := utils.Run(exec.Command("kubectl", "rollout", "status", "deployment/registry",
		"-n", registryNamespace, "--timeout", "2m"))
	Expect(err).NotTo(HaveOccurred())
}

// stopRegistry undoes startRegistry. It waits for the namespace to go, so that the next
// container's startRegistry does not apply into a terminating namespace.
func stopRegistry() {
	_, _ = utils.Run(exec.Command("kubectl", "delete", "namespace", registryNamespace, "--wait=false"))

	Eventually(func(g Gomega) { waitUntilNamespaceGone(g, registryNamespace) }).
		WithTimeout(2 * time.Minute).Should(Succeed())
}

// pushToNodeRegistry pushes a local image to the in-cluster registry as <repo>:e2e, pulls it into
// the node's containerd and returns its digest-pinned reference.
//
// The pull is what lets the Job pods start: their images are IfNotPresent, so with the image
// already in containerd under exactly this reference the kubelet never pulls it, and containerd
// needs no config to treat the registry as plain HTTP.
func pushToNodeRegistry(nodeName, localImage, repo string) string {
	target := fmt.Sprintf("%s:%d/%s:e2e", nodeInternalIP(nodeName), registryNodePort, repo)

	var (
		ref string
		err error
	)

	Eventually(func() error {
		ref, err = pushImageToRegistry(localImage, target)
		return err
	}).WithTimeout(time.Minute).Should(Succeed(), "Failed to push %s; build it with 'make docker-build-e2e'", localImage)

	_, err = utils.Run(exec.Command("docker", "exec", nodeName,
		"ctr", "-n", "k8s.io", "images", "pull", "--plain-http", ref))
	Expect(err).NotTo(HaveOccurred())

	return ref
}

// gpuDevicesFromResourceSlices returns the node of the first Intel GPU ResourceSlice and the GPUs it
// lists, as the DRA driver reports them.
func gpuDevicesFromResourceSlices(g Gomega) (string, []gpuDevice) {
	for _, slice := range gpuResourceSlices(g) {
		devices := []gpuDevice{}

		for _, dev := range slice.Spec.Devices {
			pciID := deviceAttrString(dev.Attributes, common.DeviceAttrDeviceID)
			bdf := deviceAttrString(dev.Attributes, common.DeviceAttrBDF)

			if pciID != "" && bdf != "" {
				devices = append(devices, gpuDevice{pciID: normalizePCIID(pciID), bdf: bdf})
			}
		}

		if len(devices) > 0 {
			return *slice.Spec.NodeName, devices
		}
	}

	g.Expect(false).To(BeTrue(), "no %s ResourceSlice with %s and %s attributes",
		common.GPUDeviceClass, common.DeviceAttrDeviceID, common.DeviceAttrBDF)

	return "", nil
}

// gpuResourceSlices returns the Intel GPU ResourceSlices that name a node.
func gpuResourceSlices(g Gomega) []resv1.ResourceSlice {
	output, err := utils.Run(exec.Command("kubectl", "get", "resourceslices", "-o", "json"))
	g.Expect(err).NotTo(HaveOccurred())

	sliceList := resv1.ResourceSliceList{}
	g.Expect(json.Unmarshal([]byte(output), &sliceList)).To(Succeed())

	found := []resv1.ResourceSlice{}

	for _, slice := range sliceList.Items {
		if slice.Spec.Driver == common.GPUDeviceClass && slice.Spec.NodeName != nil {
			found = append(found, slice)
		}
	}

	return found
}

func deviceAttrString(attrs map[resv1.QualifiedName]resv1.DeviceAttribute, attr string) string {
	if a, ok := attrs[resv1.QualifiedName(attr)]; ok && a.StringValue != nil {
		return *a.StringValue
	}

	return ""
}

// normalizePCIID turns the driver's PCI ID into the '0xabcd' form the CRDs require.
func normalizePCIID(id string) string {
	id = strings.ToLower(id)
	if !strings.HasPrefix(id, "0x") {
		id = "0x" + id
	}

	return id
}

func nodeInternalIP(node string) string {
	output, err := utils.Run(exec.Command("kubectl", "get", "node", node,
		"-o", `jsonpath={.status.addresses[?(@.type=="InternalIP")].address}`))
	Expect(err).NotTo(HaveOccurred())

	ip := strings.TrimSpace(output)
	Expect(ip).NotTo(BeEmpty(), "node %s has no InternalIP", node)

	return ip
}

// pushImageToRegistry pushes a local docker image to target (a tagged reference) and returns its
// digest-pinned reference.
func pushImageToRegistry(localImage, target string) (string, error) {
	tmp, err := os.CreateTemp("", "e2e-image-*.tar")
	if err != nil {
		return "", err
	}

	defer os.Remove(tmp.Name())

	_ = tmp.Close()

	if _, err := utils.Run(exec.Command("docker", "save", "-o", tmp.Name(), localImage)); err != nil {
		return "", err
	}

	img, err := tarball.ImageFromPath(tmp.Name(), nil)
	if err != nil {
		return "", err
	}

	ref, err := name.ParseReference(target)
	if err != nil {
		return "", err
	}

	// The registry is always on the Kind network. A host HTTP proxy cannot reach it and would answer
	// the push with an error page, so it is bypassed regardless of NO_PROXY.
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return "", fmt.Errorf("unexpected default transport type %T", http.DefaultTransport)
	}

	transport = transport.Clone()
	transport.Proxy = nil

	if err := remote.Write(ref, img, remote.WithTransport(transport)); err != nil {
		return "", err
	}

	digest, err := img.Digest()
	if err != nil {
		return "", err
	}

	return ref.Context().Digest(digest.String()).String(), nil
}

func applyManifest(manifest string) error {
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	_, err := utils.Run(cmd)

	return err
}

func mustRun(args ...string) string {
	output, _ := utils.Run(exec.Command(args[0], args[1:]...))
	return output
}

func jobFailed(jobName string) bool {
	output, err := utils.Run(exec.Command("kubectl", "get", "job", jobName, "-n", namespace, "-o", "json"))
	Expect(err).NotTo(HaveOccurred())

	job := batch.Job{}
	Expect(json.Unmarshal([]byte(output), &job)).To(Succeed())

	for _, cond := range job.Status.Conditions {
		if cond.Type == batch.JobFailed && cond.Status == core.ConditionTrue {
			return true
		}
	}

	return false
}

func nodeHasTaint(node, key string) bool {
	output, err := utils.Run(exec.Command("kubectl", "get", "node", node, "-o", "jsonpath={.spec.taints[*].key}"))
	Expect(err).NotTo(HaveOccurred())

	return slices.Contains(strings.Fields(output), key)
}

func podExists(podName, ns string) bool {
	output, err := utils.Run(exec.Command("kubectl", "get", "pod", podName, "-n", ns, "--ignore-not-found", "-o", "name"))
	Expect(err).NotTo(HaveOccurred())

	return strings.TrimSpace(output) != ""
}

func registryManifest() string {
	return fmt.Sprintf(`apiVersion: v1
kind: Namespace
metadata:
  name: %[1]s
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: registry
  namespace: %[1]s
spec:
  selector:
    matchLabels:
      app: registry
  template:
    metadata:
      labels:
        app: registry
    spec:
      containers:
      - name: registry
        image: %[2]s
        imagePullPolicy: IfNotPresent
        ports:
        - containerPort: 5000
---
apiVersion: v1
kind: Service
metadata:
  name: registry
  namespace: %[1]s
spec:
  type: NodePort
  selector:
    app: registry
  ports:
  - port: 5000
    targetPort: 5000
    nodePort: %[3]d
`, registryNamespace, registryImage, registryNodePort)
}

// deviceTaintKeys returns the keys of the taints on the GPU's ResourceSlice device. A GPU missing
// from the slices fails g, since a GPU the DRA driver does not list cannot be called healthy.
func deviceTaintKeys(g Gomega, f gpuFault) []string {
	for _, slice := range gpuResourceSlices(g) {
		if *slice.Spec.NodeName != f.node {
			continue
		}

		for _, dev := range slice.Spec.Devices {
			if deviceAttrString(dev.Attributes, common.DeviceAttrBDF) != f.bdf {
				continue
			}

			keys := []string{}
			for _, taint := range dev.Taints {
				keys = append(keys, taint.Key)
			}

			return keys
		}
	}

	g.Expect(false).To(BeTrue(), "no GPU %s on node %s in the ResourceSlices", f.bdf, f.node)

	return nil
}

// lenientDeviceTaintKeys is deviceTaintKeys for callers that must not fail: it reports whether
// the GPU was listed at all instead.
func lenientDeviceTaintKeys(f gpuFault) ([]string, bool) {
	listed := true
	g := NewGomega(func(string, ...int) { listed = false })

	keys := deviceTaintKeys(g, f)

	return keys, listed
}
