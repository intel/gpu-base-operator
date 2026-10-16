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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	batch "k8s.io/api/batch/v1"
	core "k8s.io/api/core/v1"
	resv1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	api "github.com/intel/gpu-base-operator/api/v1alpha1"
	"github.com/intel/gpu-base-operator/internal/controller/common"
	"github.com/intel/gpu-base-operator/test/utils"
)

const (
	// Images built by 'make docker-build-e2e'.
	fwUpdaterMockImage = "ghcr.io/intel/gpu-fwupdater-mock:devel"
	fwContentE2EImage  = "ghcr.io/intel/gpu-firmware-update-files-e2e:devel"

	fwRegistryImage     = "registry:2"
	fwRegistryNamespace = "fwupdate-e2e-registry"
	// fwRegistryNodePort is where the registry is reached from the host (to push), from the node's
	// containerd (to pull) and from the operator (to verify), all through the node's IP. That IP is
	// RFC1918, which go-containerregistry talks plain HTTP to, so the registry needs no TLS.
	fwRegistryNodePort = 30500

	fwNodeLabel  = "fwupdate-e2e"
	fwTaint      = "gpufirmware-update-e2e"
	fwTestdata   = "test/e2e/testdata/fwfiles"
	fwSlowPod    = "fwupdate-slow-gpupod"
	fwSlowClaim  = "fwupdate-slow-gpu-claim"
	fwDPPod      = "fwupdate-dp-gpupod"
	draTestPod   = "config/test/dra"
	draTestPodNs = "default"

	// update.sh prints this when no GPU on the node has an MEI interface. Without one the updater
	// never reaches xpu-smi, so the specs that need the mock to run skip instead of failing.
	noMatchingDevicesMsg = "No matching devices found"

	// fwPCIIDEnv selects which GPU the specs update, by PCI device ID ("0xe20b" or "e20b"). Without
	// it the first GPU in the ResourceSlice is used, which on a node that also has an integrated GPU
	// may be one without MEI, skipping every spec that needs the updater to run.
	fwPCIIDEnv = "E2E_FWUPDATE_PCI_ID"
)

// fwDevice is one GPU as its DRA ResourceSlice describes it.
type fwDevice struct {
	pciID string
	bdf   string
}

// fwTarget is the node the firmware update specs run against and what they flash it with.
type fwTarget struct {
	nodeName     string
	pciDeviceID  string
	contentImage string
	checksums    map[string]string
}

// newFwUpdate returns a CR for the target node, flashing the given testdata files with their
// checksums from a digest-pinned content image.
func (t *fwTarget) newFwUpdate(crName, method string, files map[string]string) *api.GPUFirmwareUpdate {
	fu := &api.GPUFirmwareUpdate{
		TypeMeta:   metav1.TypeMeta{APIVersion: api.GroupVersion.String(), Kind: "GPUFirmwareUpdate"},
		ObjectMeta: metav1.ObjectMeta{Name: crName},
		Spec: api.GPUFirmwareUpdateSpec{
			UpdateMethod: method,
			UpdaterImage: fwUpdaterMockImage,
			PCIDeviceID:  t.pciDeviceID,
			UpdateTaint:  fwTaint,
			NodeSelector: map[string]string{fwNodeLabel: "true"},
			Content:      api.GPUFirmwareContent{ContainerImage: t.contentImage},
		},
	}

	// Sorted, so the files are flashed and logged in a stable order.
	fileNames := make([]string, 0, len(files))
	for f := range files {
		fileNames = append(fileNames, f)
	}

	slices.Sort(fileNames)

	for _, f := range fileNames {
		fu.Spec.Content.Files = append(fu.Spec.Content.Files, api.GPUFirmwareFile{
			FileName: f, Type: files[f], Checksum: t.checksums[f],
		})
	}

	return fu
}

// skipWithoutMEI skips the spec if update.sh found no GPU to update, which happens on nodes
// whose GPUs have no MEI interface (integrated GPUs, for one).
func (t *fwTarget) skipWithoutMEI(logs string) {
	if strings.Contains(logs, noMatchingDevicesMsg) {
		Skip(fmt.Sprintf("no GPU with PCI ID %s and an MEI interface on node %s: update.sh never reached xpu-smi",
			t.pciDeviceID, t.nodeName))
	}
}

// deployOperatorForFwUpdate loads the firmware update images on Kind and deploys the operator.
func deployOperatorForFwUpdate() {
	By("loading the firmware update images on Kind")
	Expect(utils.LoadImageToKindClusterWithName(fwUpdaterMockImage)).To(Succeed(),
		"Failed to load the mock updater image; build it with 'make docker-build-e2e'")

	_, err := utils.Run(exec.Command("docker", "pull", fwRegistryImage))
	Expect(err).NotTo(HaveOccurred())
	Expect(utils.LoadImageToKindClusterWithName(fwRegistryImage)).To(Succeed())

	By("deploying the operator")
	_, err = utils.Run(exec.Command("kubectl", "apply", "-k", "config/default/"))
	Expect(err).NotTo(HaveOccurred(), "Failed to create operator deployment")

	_, err = utils.Run(exec.Command("kubectl", "wait", "deployment.apps/intel-gpu-base-operator-controller-manager",
		"--for", "condition=Available", "--namespace", namespace, "--timeout", "1m"))
	Expect(err).NotTo(HaveOccurred(), "Controller deployment did not become available in time")
}

// prepareFwTarget makes the content image available to the operator and the node, and labels the
// node so the specs' CRs select it.
func prepareFwTarget(nodeName, pciDeviceID string) *fwTarget {
	t := &fwTarget{nodeName: nodeName, pciDeviceID: pciDeviceID}

	By("starting an in-cluster registry for the firmware content image")
	Expect(applyManifest(fwRegistryManifest())).To(Succeed())

	_, err := utils.Run(exec.Command("kubectl", "rollout", "status", "deployment/registry",
		"-n", fwRegistryNamespace, "--timeout", "2m"))
	Expect(err).NotTo(HaveOccurred())

	By("pushing the firmware content image to the registry")
	repo := fmt.Sprintf("%s:%d/e2e/fwfiles", nodeInternalIP(nodeName), fwRegistryNodePort)

	Eventually(func() error {
		t.contentImage, err = pushImageToRegistry(fwContentE2EImage, repo+":e2e")
		return err
	}).WithTimeout(time.Minute).Should(Succeed(), "Failed to push the content image; build it with 'make docker-build-e2e'")

	// The fw-copy initContainer is IfNotPresent, so with the image already in containerd under
	// exactly this reference the kubelet never pulls it — and containerd needs no config to
	// treat the registry as plain HTTP.
	_, err = utils.Run(exec.Command("docker", "exec", nodeName,
		"ctr", "-n", "k8s.io", "images", "pull", "--plain-http", t.contentImage))
	Expect(err).NotTo(HaveOccurred())

	t.checksums = testdataChecksums()

	By("labelling the node for the firmware update")
	_, err = utils.Run(exec.Command("kubectl", "label", "node", nodeName, fwNodeLabel+"=true", "--overwrite"))
	Expect(err).NotTo(HaveOccurred())

	return t
}

// cleanupFwTarget undoes prepareFwTarget. It waits for the registry namespace to go, so that the
// next container's prepareFwTarget does not apply into a terminating namespace.
func cleanupFwTarget(nodeName string) {
	_, _ = utils.Run(exec.Command("kubectl", "label", "node", nodeName, fwNodeLabel+"-"))
	_, _ = utils.Run(exec.Command("kubectl", "delete", "namespace", fwRegistryNamespace, "--wait=false"))

	Eventually(func(g Gomega) { waitUntilNamespaceGone(g, fwRegistryNamespace) }).
		WithTimeout(2 * time.Minute).Should(Succeed())
}

// cleanupAfterFwSpec removes what a spec may leave behind, dumping debug info first if it failed.
func cleanupAfterFwSpec(nodeName string) {
	if CurrentSpecReport().Failed() {
		dumpFwUpdateDebugInfo()
	}

	By("removing firmware update CRs")
	deleteAllFwUpdates()

	// A failed spec can leave the taint behind; the next spec must start from an untainted node.
	_, _ = utils.Run(exec.Command("kubectl", "taint", "node", nodeName, fwTaint+"-"))
}

var _ = Describe("Firmware update", Ordered, Label("fwupdate"), func() {
	var (
		t       *fwTarget
		devices []fwDevice
	)

	BeforeAll(func() {
		deployOperatorForFwUpdate()

		By("deploying GPU DRA")
		tmpDir, err := os.MkdirTemp("/tmp/", "operator-fwupdate*")
		Expect(err).NotTo(HaveOccurred())
		defer os.RemoveAll(tmpDir)

		cp := dynamicResourceAllocationClusterPolicy()
		cp.Spec.UseNFDLabeling = false
		cp.Spec.ResourceMonitoring = false

		Expect(applyClusterPolicyToCluster(&cp, filepath.Join(tmpDir, "clusterpolicy.yaml"))).To(Succeed())
		Eventually(waitForDRAPodsToBecomeRunning).Should(Succeed())

		By("reading the GPUs from the DRA ResourceSlice")
		var nodeName string

		Eventually(func(g Gomega) {
			nodeName, devices = gpuDevicesFromResourceSlices(g)
		}).Should(Succeed())

		pciDeviceID := selectFwPCIID(devices)
		_, _ = fmt.Fprintf(GinkgoWriter, "Node %s, GPUs %+v, updating PCI ID %s\n", nodeName, devices, pciDeviceID)

		t = prepareFwTarget(nodeName, pciDeviceID)
	})

	AfterAll(func() {
		if t != nil {
			cleanupFwTarget(t.nodeName)
		}

		_, _ = utils.Run(exec.Command("kubectl", "delete", "clusterpolicy", "gpu-policy"))
		Eventually(waitForDRAPodsToGoAway).Should(Succeed())

		By("cleaning up operator deployment")
		_, _ = utils.Run(exec.Command("kubectl", "delete", "-k", "config/default"))
	})

	AfterEach(func() {
		// BeforeAll skipped or failed before the target was set up.
		if t == nil {
			return
		}

		cleanupAfterFwSpec(t.nodeName)

		By("removing test pods")
		_, _ = utils.Run(exec.Command("kubectl", "delete", "pod", fwSlowPod, "-n", draTestPodNs,
			"--ignore-not-found", "--grace-period=0", "--force"))
		_, _ = utils.Run(exec.Command("kubectl", "delete", "resourceclaimtemplate", fwSlowClaim, "-n", draTestPodNs,
			"--ignore-not-found"))
		_, _ = utils.Run(exec.Command("kubectl", "delete", "-k", draTestPod, "--ignore-not-found"))
	})

	SetDefaultEventuallyTimeout(3 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)

	It("rejects a content image whose checksum does not match, before touching the node", func() {
		fu := t.newFwUpdate("fwupdate-bad-checksum", "direct", map[string]string{"gfx.bin": "GFX"})
		fu.Spec.Content.Files[0].Checksum = "sha256:" + strings.Repeat("0", 64)

		Expect(applyFwUpdate(fu)).To(Succeed())

		By("waiting for the pre-flight check to fail the update")
		var state *api.GPUFirmwareUpdate

		Eventually(func(g Gomega) {
			state = getFwUpdate(fu.Name, g)
			g.Expect(state.Status.State).To(Equal("error"))
		}).Should(Succeed())

		// A mismatch, specifically: an unreachable registry also ends in "error", for the wrong reason.
		Expect(strings.Join(state.Status.Messages, "\n")).To(SatisfyAll(
			ContainSubstring("checksum mismatch"), ContainSubstring("gfx.bin")))

		By("validating that the node was never tainted and no Job was created")
		Expect(state.Status.NodeInfos.All).To(BeEmpty())
		Expect(nodeHasTaint(t.nodeName, fwTaint)).To(BeFalse())
		Expect(fwUpdateJobNames(fu.Name)).To(BeEmpty())
	})

	It("drains the DRA workload and updates the node directly", func() {
		By("launching a pod with a DRA GPU claim")
		Expect(launchTestPod(draTestPod)).To(Succeed())
		Eventually(verifyTestPodRunning).Should(Succeed())

		fu := t.newFwUpdate("fwupdate-direct", "direct", map[string]string{
			"gfx.bin":      "GFX",
			"gfx_data.bin": "GFX_DATA",
		})
		Expect(applyFwUpdate(fu)).To(Succeed())

		By("validating that the node is tainted and the GPU pod evicted")
		Eventually(func(g Gomega) {
			g.Expect(nodeHasTaint(t.nodeName, fwTaint)).To(BeTrue())
			g.Expect(podExists("gpupod", draTestPodNs)).To(BeFalse())
		}).Should(Succeed())

		By("following the update to its end")
		final, states := waitForFwUpdateToFinish(fu.Name)
		logs := fwUpdateJobLogs(final)
		t.skipWithoutMEI(logs)

		expectDirectUpdateCompleted(t, fu, final, states, logs)

		By("validating that update.sh flashed every file on every GPU DRA reports")
		for _, dev := range devices {
			if dev.pciID != t.pciDeviceID {
				continue
			}

			for _, f := range fu.Spec.Content.Files {
				Expect(logs).To(ContainSubstring("updated %s on %s with /update/%s", f.Type, dev.bdf, f.FileName))
			}
		}

		By("validating that the node is untainted and takes GPU workloads again")
		Expect(nodeHasTaint(t.nodeName, fwTaint)).To(BeFalse())

		Expect(launchTestPod(draTestPod)).To(Succeed())
		Eventually(verifyTestPodRunning).Should(Succeed())
	})

	It("reports the node as failed when the updater fails", func() {
		fu := t.newFwUpdate("fwupdate-fail", "direct", map[string]string{"fail.bin": "GFX_DATA"})
		Expect(applyFwUpdate(fu)).To(Succeed())

		final, states := waitForFwUpdateToFinish(fu.Name)
		logs := fwUpdateJobLogs(final)
		t.skipWithoutMEI(logs)

		Expect(final.Status.State).To(Equal("error"), "states seen: %v", states)
		Expect(final.Status.NodeInfos.Error).To(ConsistOf(t.nodeName))
		Expect(final.Status.NodeInfos.Completed).To(BeEmpty())

		// The failure must come from the mock, not from update.sh giving up before reaching it.
		Expect(logs).To(ContainSubstring("FAKE_XPU_SMI_FAIL found in /update/fail.bin"))
		Expect(logs).NotTo(ContainSubstring("##COMPLETED##"))

		Expect(final.Status.NodeInfos.Jobs).To(HaveLen(1))
		Expect(jobFailed(final.Status.NodeInfos.Jobs[0])).To(BeTrue())

		Expect(nodeHasTaint(t.nodeName, fwTaint)).To(BeFalse())
	})

	It("protects the spec while draining and can be aborted", Label("long"), func() {
		By("launching a GPU pod that takes a minute to terminate")
		Expect(applyManifest(slowGPUPodManifest())).To(Succeed())
		Eventually(func(g Gomega) { waitForNamedPodToBecomeRunning(fwSlowPod, draTestPodNs, g) }).Should(Succeed())

		fu := t.newFwUpdate("fwupdate-abort", "direct", map[string]string{"gfx.bin": "GFX"})
		Expect(applyFwUpdate(fu)).To(Succeed())

		Eventually(func(g Gomega) {
			g.Expect(getFwUpdate(fu.Name, g).Status.State).To(Equal("draining"))
		}).Should(Succeed())

		By("validating that the webhook rejects structural changes while draining")
		_, err := patchFwUpdate(fu.Name, `{"spec":{"updaterImage":"example.com/other:latest"}}`)
		Expect(err).To(HaveOccurred())

		_, err = patchFwUpdate(fu.Name, `{"spec":{"updateTaint":"some-other-taint"}}`)
		Expect(err).To(HaveOccurred())

		_, err = patchFwUpdate(fu.Name, `{"spec":{"holdAfterCanary":true}}`)
		Expect(err).NotTo(HaveOccurred(), "holdAfterCanary must stay mutable")

		Expect(getFwUpdate(fu.Name, Default).Status.State).To(Equal("draining"),
			"the slow pod should still be holding the drain")

		By("deleting the CR mid-drain")
		_, err = utils.Run(exec.Command("kubectl", "delete", "gpufirmwareupdate", fu.Name, "--timeout", "1m"))
		Expect(err).NotTo(HaveOccurred())

		Expect(nodeHasTaint(t.nodeName, fwTaint)).To(BeFalse())
		Expect(fwUpdateJobNames(fu.Name)).To(BeEmpty())
	})

	It("finishes a canary update without holding when the canary is the only node", Label("long"), func() {
		fu := t.newFwUpdate("fwupdate-canary", "canary", map[string]string{"gfx.bin": "GFX"})
		fu.Spec.HoldAfterCanary = true
		Expect(applyFwUpdate(fu)).To(Succeed())

		final, states := waitForFwUpdateToFinish(fu.Name)
		t.skipWithoutMEI(fwUpdateJobLogs(final))

		Expect(final.Status.State).To(Equal("completed"), "states seen: %v", states)
		Expect(states).NotTo(ContainElement("canary_done"))
		Expect(final.Status.NodeInfos.Pending).To(BeEmpty())
		Expect(final.Status.NodeInfos.Completed).To(ConsistOf(t.nodeName))
		Expect(nodeHasTaint(t.nodeName, fwTaint)).To(BeFalse())
	})
})

// The device plugin variant needs a PCI ID: without a ResourceSlice there is nothing to pick a GPU
// from, and the specs only add the extended-resource path of the drain to what the DRA ones cover.
var _ = Describe("Firmware update with device plugin", Ordered, Label("fwupdate", "deviceplugin"), func() {
	var (
		t    *fwTarget
		bdfs []string
	)

	BeforeAll(func() {
		pciDeviceID, ok := os.LookupEnv(fwPCIIDEnv)
		if !ok || pciDeviceID == "" {
			Skip(fwPCIIDEnv + " is not set")
		}

		pciDeviceID = normalizePCIID(pciDeviceID)

		deployOperatorForFwUpdate()

		By("deploying the GPU device plugin")
		tmpDir, err := os.MkdirTemp("/tmp/", "operator-fwupdate-dp*")
		Expect(err).NotTo(HaveOccurred())
		defer os.RemoveAll(tmpDir)

		cp := devicePluginClusterPolicy()
		cp.Spec.UseNFDLabeling = false
		cp.Spec.ResourceMonitoring = false

		Expect(applyClusterPolicyToCluster(&cp, filepath.Join(tmpDir, "clusterpolicy.yaml"))).To(Succeed())
		Eventually(waitForDevicePluginPodsToBecomeRunning).Should(Succeed())

		By("finding the node advertising the device plugin's GPU resource")
		var nodeName string

		Eventually(func(g Gomega) {
			nodeName, _ = gpuNodeFromDevicePlugin(g)
		}).Should(Succeed())

		bdfs = nodeBDFsForPCIID(nodeName, pciDeviceID)
		Expect(bdfs).NotTo(BeEmpty(), "%s=%s matches no Intel PCI device on node %s", fwPCIIDEnv, pciDeviceID, nodeName)

		_, _ = fmt.Fprintf(GinkgoWriter, "Node %s, GPUs %v, updating PCI ID %s\n", nodeName, bdfs, pciDeviceID)

		t = prepareFwTarget(nodeName, pciDeviceID)
	})

	AfterAll(func() {
		// Skipped in BeforeAll: nothing was deployed.
		if t == nil {
			return
		}

		cleanupFwTarget(t.nodeName)

		_, _ = utils.Run(exec.Command("kubectl", "delete", "clusterpolicy", "gpu-policy"))
		Eventually(func(g Gomega) { waitForPodsToGoAway("app=intel-gpu-plugin", namespace, g) }).Should(Succeed())

		By("cleaning up operator deployment")
		_, _ = utils.Run(exec.Command("kubectl", "delete", "-k", "config/default"))
	})

	AfterEach(func() {
		// BeforeAll skipped or failed before the target was set up.
		if t == nil {
			return
		}

		cleanupAfterFwSpec(t.nodeName)

		By("removing test pods")
		_, _ = utils.Run(exec.Command("kubectl", "delete", "pod", fwDPPod, "-n", draTestPodNs,
			"--ignore-not-found", "--grace-period=0", "--force"))
	})

	SetDefaultEventuallyTimeout(3 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)

	It("drains the device plugin workload and updates the node directly", func() {
		_, resource := gpuNodeFromDevicePlugin(Default)

		By("launching a pod requesting " + resource)
		Expect(applyManifest(dpGPUPodManifest(resource))).To(Succeed())
		Eventually(func(g Gomega) { waitForNamedPodToBecomeRunning(fwDPPod, draTestPodNs, g) }).Should(Succeed())

		fu := t.newFwUpdate("fwupdate-dp-direct", "direct", map[string]string{
			"gfx.bin":      "GFX",
			"gfx_data.bin": "GFX_DATA",
		})
		Expect(applyFwUpdate(fu)).To(Succeed())

		By("validating that the node is tainted and the GPU pod evicted")
		Eventually(func(g Gomega) {
			g.Expect(nodeHasTaint(t.nodeName, fwTaint)).To(BeTrue())
			g.Expect(podExists(fwDPPod, draTestPodNs)).To(BeFalse())
		}).Should(Succeed())

		By("following the update to its end")
		final, states := waitForFwUpdateToFinish(fu.Name)
		logs := fwUpdateJobLogs(final)
		t.skipWithoutMEI(logs)

		expectDirectUpdateCompleted(t, fu, final, states, logs)

		By("validating that update.sh flashed every file on every GPU with the PCI ID")
		for _, bdf := range bdfs {
			for _, f := range fu.Spec.Content.Files {
				Expect(logs).To(ContainSubstring("updated %s on %s with /update/%s", f.Type, bdf, f.FileName))
			}
		}

		By("validating that the node is untainted and takes GPU workloads again")
		Expect(nodeHasTaint(t.nodeName, fwTaint)).To(BeFalse())

		Expect(applyManifest(dpGPUPodManifest(resource))).To(Succeed())
		Eventually(func(g Gomega) { waitForNamedPodToBecomeRunning(fwDPPod, draTestPodNs, g) }).Should(Succeed())
	})
})

// expectDirectUpdateCompleted checks a single-node direct update ran to completion.
func expectDirectUpdateCompleted(t *fwTarget, fu, final *api.GPUFirmwareUpdate, states []string, logs string) {
	GinkgoHelper()

	Expect(final.Status.State).To(Equal("completed"), "states seen: %v, job logs:\n%s", states, logs)
	Expect(states).To(ContainElement("updating"))
	Expect(final.Status.NodeInfos.All).To(ConsistOf(t.nodeName))
	Expect(final.Status.NodeInfos.Completed).To(ConsistOf(t.nodeName))
	Expect(final.Status.NodeInfos.Error).To(BeEmpty())
	Expect(final.Status.NodeInfos.Jobs).To(HaveLen(1), "one Job for %s", fu.Name)
	Expect(logs).To(ContainSubstring("##COMPLETED##"))
}

// gpuNodeFromDevicePlugin returns the first node with a non-zero allocatable Intel GPU extended
// resource, and that resource's name.
func gpuNodeFromDevicePlugin(g Gomega) (string, string) {
	output, err := utils.Run(exec.Command("kubectl", "get", "nodes", "-o", "json"))
	g.Expect(err).NotTo(HaveOccurred())

	nodes := core.NodeList{}
	g.Expect(json.Unmarshal([]byte(output), &nodes)).To(Succeed())

	for _, node := range nodes.Items {
		for _, res := range []core.ResourceName{common.XeResource, common.I915Resource} {
			if q, ok := node.Status.Allocatable[res]; ok && !q.IsZero() {
				return node.Name, string(res)
			}
		}
	}

	g.Expect(false).To(BeTrue(), "no node advertises %s or %s", common.XeResource, common.I915Resource)

	return "", ""
}

// nodeBDFsForPCIID returns the BDFs of the Intel PCI devices with the given ID on a Kind node, read
// from the host sysfs the node container sees — the same place update.sh looks.
func nodeBDFsForPCIID(nodeName, pciID string) []string {
	script := fmt.Sprintf(`for d in /sys/bus/pci/devices/*; do
  [ "$(cat "$d/vendor")" = 0x8086 ] && [ "$(cat "$d/device")" = %s ] && basename "$d"
done; true`, pciID)

	output, err := utils.Run(exec.Command("docker", "exec", nodeName, "sh", "-c", script))
	Expect(err).NotTo(HaveOccurred())

	return utils.GetNonEmptyLines(output)
}

// gpuDevicesFromResourceSlices returns the node of the first Intel GPU ResourceSlice and the GPUs it
// lists, as the DRA driver reports them.
func gpuDevicesFromResourceSlices(g Gomega) (string, []fwDevice) {
	output, err := utils.Run(exec.Command("kubectl", "get", "resourceslices", "-o", "json"))
	g.Expect(err).NotTo(HaveOccurred())

	sliceList := resv1.ResourceSliceList{}
	g.Expect(json.Unmarshal([]byte(output), &sliceList)).To(Succeed())

	for _, slice := range sliceList.Items {
		if slice.Spec.Driver != common.GPUDeviceClass || slice.Spec.NodeName == nil {
			continue
		}

		devices := []fwDevice{}

		for _, dev := range slice.Spec.Devices {
			pciID := deviceAttrString(dev.Attributes, common.DeviceAttrDeviceID)
			bdf := deviceAttrString(dev.Attributes, common.DeviceAttrBDF)

			if pciID != "" && bdf != "" {
				devices = append(devices, fwDevice{pciID: normalizePCIID(pciID), bdf: bdf})
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

// selectFwPCIID returns the PCI ID requested through fwPCIIDEnv, failing if no GPU has it, or the
// first GPU's when the variable is unset.
func selectFwPCIID(devices []fwDevice) string {
	want, ok := os.LookupEnv(fwPCIIDEnv)
	if !ok || want == "" {
		return devices[0].pciID
	}

	want = normalizePCIID(want)

	Expect(slices.ContainsFunc(devices, func(d fwDevice) bool { return d.pciID == want })).To(BeTrue(),
		"%s=%s matches no GPU on the node: %+v", fwPCIIDEnv, want, devices)

	return want
}

func deviceAttrString(attrs map[resv1.QualifiedName]resv1.DeviceAttribute, attr string) string {
	if a, ok := attrs[resv1.QualifiedName(attr)]; ok && a.StringValue != nil {
		return *a.StringValue
	}

	return ""
}

// normalizePCIID turns the driver's PCI ID into the '0xabcd' form spec.pciDeviceID requires.
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
	tmp, err := os.CreateTemp("", "fwcontent-*.tar")
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

// testdataChecksums returns the "sha256:..." checksum of every file in the content image's testdata.
func testdataChecksums() map[string]string {
	dir, err := utils.GetProjectDir()
	Expect(err).NotTo(HaveOccurred())

	entries, err := os.ReadDir(filepath.Join(dir, fwTestdata))
	Expect(err).NotTo(HaveOccurred())

	sums := map[string]string{}

	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(dir, fwTestdata, e.Name()))
		Expect(err).NotTo(HaveOccurred())

		sum := sha256.Sum256(data)
		sums[e.Name()] = "sha256:" + hex.EncodeToString(sum[:])
	}

	return sums
}

func applyManifest(manifest string) error {
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	_, err := utils.Run(cmd)

	return err
}

func applyFwUpdate(fu *api.GPUFirmwareUpdate) error {
	out, err := yaml.Marshal(fu)
	if err != nil {
		return err
	}

	return applyManifest(string(out))
}

func patchFwUpdate(crName, patch string) (string, error) {
	return utils.Run(exec.Command("kubectl", "patch", "gpufirmwareupdate", crName, "--type=merge", "-p", patch))
}

func getFwUpdate(crName string, g Gomega) *api.GPUFirmwareUpdate {
	output, err := utils.Run(exec.Command("kubectl", "get", "gpufirmwareupdate", crName, "-o", "json"))
	g.Expect(err).NotTo(HaveOccurred())

	fu := &api.GPUFirmwareUpdate{}
	g.Expect(json.Unmarshal([]byte(output), fu)).To(Succeed())

	return fu
}

// waitForFwUpdateToFinish polls the CR until it is completed or failed, returning it along with
// every state it was seen in. Polling can miss short states such as cleanup, so callers should
// only assert on states that last as long as a Job does.
func waitForFwUpdateToFinish(crName string) (*api.GPUFirmwareUpdate, []string) {
	var (
		fu     *api.GPUFirmwareUpdate
		states []string
	)

	Eventually(func(g Gomega) {
		fu = getFwUpdate(crName, g)

		if len(states) == 0 || states[len(states)-1] != fu.Status.State {
			states = append(states, fu.Status.State)
		}

		if fu.Status.State == "updating" {
			g.Expect(nodeHasTaint(fu.Status.NodeInfos.All[0], fu.Spec.UpdateTaint)).To(BeTrue())
		}

		g.Expect(fu.Status.State).To(BeElementOf("completed", "error"))
	}).Should(Succeed())

	_, _ = fmt.Fprintf(GinkgoWriter, "GPUFirmwareUpdate %s states: %v\n", crName, states)

	return fu, states
}

func fwUpdateJobNames(crName string) []string {
	output, err := utils.Run(exec.Command("kubectl", "get", "jobs", "-n", namespace,
		"-l", "gpufirmwareupdate="+crName, "-o", "go-template={{ range .items }}{{ .metadata.name }}{{ \"\\n\" }}{{ end }}"))
	Expect(err).NotTo(HaveOccurred())

	return utils.GetNonEmptyLines(output)
}

func fwUpdateJobLogs(fu *api.GPUFirmwareUpdate) string {
	logs := ""

	for _, job := range fu.Status.NodeInfos.Jobs {
		output, err := utils.Run(exec.Command("kubectl", "logs", "-n", namespace, "job/"+job, "-c", "updater"))
		Expect(err).NotTo(HaveOccurred(), "Failed to get logs of Job %s", job)

		logs += output
	}

	return logs
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

// deleteAllFwUpdates deletes every GPUFirmwareUpdate. A CR stopped mid-update keeps its finalizer
// until the update ends, so the finalizer is stripped rather than waited for; the caller removes
// the taint the operator would have.
func deleteAllFwUpdates() {
	listCmd := []string{"kubectl", "get", "gpufirmwareupdates",
		"-o", "go-template={{ range .items }}{{ .metadata.name }}{{ \"\\n\" }}{{ end }}"}

	_, _ = utils.Run(exec.Command("kubectl", "delete", "gpufirmwareupdates", "--all", "--wait=false"))

	for _, crName := range utils.GetNonEmptyLines(mustRun(listCmd...)) {
		_, _ = patchFwUpdate(crName, `{"metadata":{"finalizers":null}}`)
	}

	Eventually(func() []string {
		return utils.GetNonEmptyLines(mustRun(listCmd...))
	}).WithTimeout(time.Minute).Should(BeEmpty())

	_, _ = utils.Run(exec.Command("kubectl", "delete", "jobs", "-n", namespace, "-l", "gpufirmwareupdate", "--ignore-not-found"))
}

func mustRun(args ...string) string {
	output, _ := utils.Run(exec.Command(args[0], args[1:]...))
	return output
}

func dumpFwUpdateDebugInfo() {
	for _, args := range [][]string{
		{"kubectl", "get", "gpufirmwareupdates", "-o", "yaml"},
		{"kubectl", "get", "jobs,pods", "-n", namespace, "-l", "gpufirmwareupdate", "-o", "wide"},
		{"kubectl", "logs", "-n", namespace, "-l", "gpufirmwareupdate", "--all-containers", "--prefix", "--tail=-1"},
		{"kubectl", "logs", "-n", namespace, "-l", "control-plane=controller-manager", "--tail=200"},
		{"kubectl", "get", "events", "-n", namespace, "--sort-by=.lastTimestamp"},
	} {
		_, _ = fmt.Fprintf(GinkgoWriter, "--- %s\n%s\n", strings.Join(args, " "), mustRun(args...))
	}
}

func fwRegistryManifest() string {
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
`, fwRegistryNamespace, fwRegistryImage, fwRegistryNodePort)
}

// slowGPUPodManifest is a GPU pod that holds a drain for a minute: sleep is the container's PID 1,
// which the kernel does not deliver SIGTERM to without a handler, so it runs out its grace period.
func slowGPUPodManifest() string {
	return fmt.Sprintf(`apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  name: %[1]s
  namespace: %[3]s
spec:
  spec:
    devices:
      requests:
      - name: gpu
        exactly:
          deviceClassName: %[4]s
---
apiVersion: v1
kind: Pod
metadata:
  name: %[2]s
  namespace: %[3]s
spec:
  terminationGracePeriodSeconds: 60
  containers:
  - name: gpu
    image: ubuntu:24.04
    command: ["sleep", "infinity"]
    resources:
      claims:
      - name: gpu
  resourceClaims:
  - name: gpu
    resourceClaimTemplateName: %[1]s
`, fwSlowClaim, fwSlowPod, draTestPodNs, common.GPUDeviceClass)
}

// dpGPUPodManifest is a pod holding one GPU through the device plugin's extended resource.
func dpGPUPodManifest(resource string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: %[1]s
  namespace: %[2]s
spec:
  terminationGracePeriodSeconds: 5
  containers:
  - name: gpu
    image: ubuntu:24.04
    command: ["sleep", "infinity"]
    resources:
      limits:
        %[3]s: "1"
`, fwDPPod, draTestPodNs, resource)
}
