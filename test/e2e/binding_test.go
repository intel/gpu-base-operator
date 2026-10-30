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

// Specs that move a GPU between its DRM driver (xe or i915) and vfio: by hand for the device
// plugin, and through the DRA driver's managed binding for DRA and KubeVirt.

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	core "k8s.io/api/core/v1"
	resv1 "k8s.io/api/resource/v1"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/intel/gpu-base-operator/config/deployments"
	"github.com/intel/gpu-base-operator/internal/controller/common"
	"github.com/intel/gpu-base-operator/test/utils"
)

const (
	// vfioBDFEnv names the GPU the binding specs move to vfio, by PCI address. It is required: while
	// the GPU is on vfio the host cannot use it, so it is never picked for the user. Must be a
	// dedicated GPU on xe or i915, and not the boot VGA device (see recoveryAllowBootVGAEnv).
	vfioBDFEnv = "E2E_VFIO_BDF"

	// kubevirtVersionEnv overrides the KubeVirt release the KubeVirt spec installs.
	kubevirtVersionEnv = "E2E_KUBEVIRT_VERSION"
	// The first KubeVirt release with DRA GPUs (GPUsWithDRA) that the DRA driver supports.
	kubevirtDefaultVersion = "v1.8.3"
	kubevirtNamespace      = "kubevirt"
	kubevirtRegistry       = "quay.io/kubevirt"
	kubevirtContainerDisk  = "cirros-container-disk-demo"
	kubevirtReleaseURL     = "https://github.com/kubevirt/kubevirt/releases/download/%s/kubevirt-operator.yaml"

	// Mirrors of the operator's internal names (internal/controller/clusterpolicy).
	vfioDeviceClass = "gpu-vfio.intel.com"
	draDaemonSet    = "gpu-policy-gpu-dra"

	// draDriverAttr is the ResourceSlice device attribute with the kernel driver the GPU is bound to.
	draDriverAttr = "driver"

	// draDeviceMetadataDir is where the DRA framework mounts device metadata (KEP-5304) into a pod.
	draDeviceMetadataDir = "/var/run/kubernetes.io/dra-device-attributes"

	bindingPodNs    = "default"
	bindingPod      = "binding-gpupod"
	bindingClaim    = "binding-gpu-claim"
	bindingDPPod    = "binding-dp-gpupod"
	bindingVMI      = "binding-vmi"
	bindingVMIClaim = "binding-vmi-claim"

	// The DRA driver binds a vfio claim's GPU to the first of these that is loaded, xe-vfio-pci
	// only for a GPU on xe.
	vfioPCIDriver   = "vfio-pci"
	xeVfioPCIDriver = "xe-vfio-pci"
)

var vfioDrivers = []string{vfioPCIDriver, xeVfioPCIDriver}

// bindingTarget is the GPU the binding specs move between drivers.
type bindingTarget struct {
	gpuFault
	// origDriver is the DRM driver the GPU was on when the suite started, and is put back on.
	origDriver string
	iommuGroup string
	// vfioDrivers are the vfio drivers loaded on the host.
	vfioDrivers []string
	// skipReason, if set, is why the node cannot move the GPU to vfio.
	skipReason string
}

// newBindingTarget checks the GPU at bdf on node. A GPU the suite must not touch fails the spec;
// a host that cannot bind it to vfio only sets skipReason.
func newBindingTarget(node, bdf string) *bindingTarget {
	t := &bindingTarget{gpuFault: gpuFault{node: node, bdf: bdf, allowBootVGA: os.Getenv(recoveryAllowBootVGAEnv) == "true"}}

	t.origDriver = t.driver()
	Expect(t.origDriver).To(BeElementOf("xe", "i915"), "%s=%s on node %s must be bound to xe or i915", vfioBDFEnv, bdf, node)
	t.checkNotBootVGA()

	out, _ := t.nodeSh(fmt.Sprintf(`basename "$(readlink /sys/bus/pci/devices/%s/iommu_group)" 2>/dev/null || true`, bdf))
	t.iommuGroup = strings.TrimSpace(out)

	out, _ = t.nodeSh("ls /sys/bus/pci/drivers")
	for _, d := range vfioDrivers {
		if slices.Contains(strings.Fields(out), d) {
			t.vfioDrivers = append(t.vfioDrivers, d)
		}
	}

	_, devVfioErr := t.nodeSh("mountpoint -q /dev/vfio")

	switch {
	case devVfioErr != nil:
		t.skipReason = fmt.Sprintf("node %s does not have the host's /dev/vfio, so it never sees the GPU's vfio "+
			"device; recreate the cluster with test/e2e/kind-config.yaml ('make setup-test-e2e')", node)
	case t.iommuGroup == "":
		t.skipReason = fmt.Sprintf("%s has no IOMMU group; boot the host with the IOMMU on (intel_iommu=on)", bdf)
	case len(t.vfioDrivers) == 0:
		t.skipReason = "no vfio driver is loaded on the host; load one with 'modprobe vfio-pci'"
	}

	logf("Binding target: node %s, GPU %s on %s, IOMMU group %q, vfio drivers %v",
		node, bdf, t.origDriver, t.iommuGroup, t.vfioDrivers)

	return t
}

// requireBindingTarget skips the spec unless there is a target that can be bound to vfio, and to
// vfioDriver if it is set.
func requireBindingTarget(t *bindingTarget, vfioDriver string) {
	switch {
	case t == nil:
		Skip(vfioBDFEnv + " is not set; the binding specs need a dedicated GPU they may take from the host")
	case t.skipReason != "":
		Skip(t.skipReason)
	case vfioDriver != "" && !slices.Contains(t.vfioDrivers, vfioDriver):
		Skip(fmt.Sprintf("%s is not loaded on the host; load it with 'modprobe %s'", vfioDriver, vfioDriver))
	}
}

// rebindScript moves the GPU to driver the way the DRA driver does: unbind, driver_override, bind.
// Moving it back to its original driver clears driver_override instead.
func (t *bindingTarget) rebindScript(driver string) string {
	override := driver
	if driver == t.origDriver {
		override = ""
	}

	return fmt.Sprintf(`set -e
DEV=/sys/bus/pci/devices/%[1]s
if [ -e "$DEV/driver" ]; then echo %[1]s > "$DEV/driver/unbind"; fi
echo "%[2]s" > "$DEV/driver_override"
echo %[1]s > /sys/bus/pci/drivers/%[3]s/bind`, t.bdf, override, driver)
}

// bindTo moves the GPU to driver on the host, behind the back of whatever is using it.
func (t *bindingTarget) bindTo(driver string) {
	GinkgoHelper()

	By(fmt.Sprintf("binding GPU %s to %s", t.bdf, driver))

	out, err := t.nodeSh(t.rebindScript(driver))
	Expect(err).NotTo(HaveOccurred(), "binding %s to %s:\n%s", t.bdf, driver, out)
	Expect(t.driver()).To(Equal(driver))
}

// restore puts the GPU back on its original driver. It never fails the spec; the caller waits for
// the ResourceSlice to show the driver instead.
func (t *bindingTarget) restore() {
	if t.driver() == t.origDriver {
		return
	}

	out, err := t.nodeSh(t.rebindScript(t.origDriver))
	logf("restoring GPU %s to %s (err: %v):\n%s", t.bdf, t.origDriver, err, out)
}

// waitForSliceDriver waits for the DRA driver to list the GPU as bound to one of drivers.
func (t *bindingTarget) waitForSliceDriver(drivers ...string) {
	GinkgoHelper()

	Eventually(func(g Gomega) {
		g.Expect(sliceDriver(g, t.bdf)).To(BeElementOf(drivers))
	}).WithTimeout(2*time.Minute).Should(Succeed(), "the DRA driver does not list %s as bound to %v", t.bdf, drivers)
}

// expectOnVfio checks that the DRA driver moved the GPU to a vfio driver, and that it says so.
func (t *bindingTarget) expectOnVfio() {
	GinkgoHelper()

	By("validating that the GPU is bound to vfio on the host and in the ResourceSlice")
	Expect(t.driver()).To(BeElementOf(t.vfioDrivers))
	t.waitForSliceDriver(t.driver())
	Expect(deviceTaintKeys(Default, t.gpuFault)).To(BeEmpty())
}

// expectOnDRM checks that the GPU is back on its DRM driver, and that the pod got its DRM devices.
func (t *bindingTarget) expectOnDRM(pod string) {
	GinkgoHelper()

	By("validating that the GPU is bound to " + t.origDriver + " on the host and in the ResourceSlice")
	Expect(t.driver()).To(Equal(t.origDriver))
	t.waitForSliceDriver(t.origDriver)
	Expect(deviceTaintKeys(Default, t.gpuFault)).To(BeEmpty())

	By("validating that the pod has a render node")
	Expect(podExec(pod, "ls /dev/dri")).To(ContainSubstring("renderD"))
}

var _ = Describe("Device plugin with GPU rebinding", Ordered, Label("deviceplugin", "binding"), func() {
	var (
		t        *bindingTarget
		resource core.ResourceName
	)

	BeforeAll(func() {
		bdf := os.Getenv(vfioBDFEnv)
		if bdf == "" {
			Skip(vfioBDFEnv + " is not set; the rebinding spec needs a dedicated GPU it may take from the host")
		}

		target := newBindingTarget(firstNodeName(), bdf)
		requireBindingTarget(target, vfioPCIDriver)

		target.prepareNode()
		t = target
		resource = core.ResourceName("gpu.intel.com/" + t.origDriver)

		deployOperator()

		By("deploying the GPU device plugin")
		tmpDir, err := os.MkdirTemp("/tmp/", "operator-binding-dp*")
		Expect(err).NotTo(HaveOccurred())
		defer os.RemoveAll(tmpDir)

		cp := devicePluginClusterPolicy()
		cp.Spec.UseNFDLabeling = false
		cp.Spec.ResourceMonitoring = false

		Expect(applyClusterPolicyToCluster(&cp, filepath.Join(tmpDir, "clusterpolicy.yaml"))).To(Succeed())
		Eventually(waitForDevicePluginPodsToBecomeRunning).Should(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(nodeAllocatable(g, t.node, resource)).To(BeNumerically(">", 0))
		}).Should(Succeed())
	})

	AfterAll(func() {
		// Skipped in BeforeAll before anything was deployed.
		if t == nil {
			return
		}

		_, _ = utils.Run(exec.Command("kubectl", "delete", "clusterpolicy", "gpu-policy", "--ignore-not-found"))
		Eventually(func(g Gomega) { waitForPodsToGoAway("app=intel-gpu-plugin", namespace, g) }).Should(Succeed())

		t.unprepareNode()

		By("cleaning up operator deployment")
		_, _ = utils.Run(exec.Command("kubectl", "delete", "-k", "config/default"))
	})

	AfterEach(func() {
		if t == nil {
			return
		}

		if CurrentSpecReport().Failed() {
			dumpBindingDebugInfo(t, "app=intel-gpu-plugin")
		}

		_, _ = utils.Run(exec.Command("kubectl", "delete", "pod", bindingDPPod, "-n", bindingPodNs,
			"--ignore-not-found", "--wait", "--timeout", "2m"))

		t.restore()
	})

	SetDefaultEventuallyTimeout(3 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)

	It("stops advertising a GPU moved to vfio-pci, and advertises it again once it is back", func() {
		var before int64

		Eventually(func(g Gomega) { before = nodeAllocatable(g, t.node, resource) }).Should(Succeed())
		logf("node %s advertises %d %s", t.node, before, resource)

		t.bindTo(vfioPCIDriver)

		By("validating that the device plugin stops advertising the GPU")
		Eventually(func(g Gomega) {
			g.Expect(nodeAllocatable(g, t.node, resource)).To(Equal(before - 1))
		}).WithTimeout(40*time.Second).Should(Succeed(), "the device plugin still advertises %s on %s", t.bdf, vfioPCIDriver)

		t.bindTo(t.origDriver)

		By("validating that the device plugin advertises the GPU again")
		Eventually(func(g Gomega) {
			g.Expect(nodeAllocatable(g, t.node, resource)).To(Equal(before))
		}).WithTimeout(40*time.Second).Should(Succeed(), "the device plugin does not advertise %s back on %s", t.bdf, t.origDriver)

		By(fmt.Sprintf("launching a pod requesting all %d GPUs", before))
		Expect(applyManifest(dpAllGPUsPodManifest(resource, before))).To(Succeed())
		Eventually(func(g Gomega) { waitForNamedPodToBecomeRunning(bindingDPPod, bindingPodNs, g) }).Should(Succeed())

		By("validating that the pod has a render node for every GPU, the rebound one included")
		renderNodes := utils.GetNonEmptyLines(podExec(bindingDPPod, "ls /dev/dri | grep renderD || true"))
		Expect(renderNodes).To(HaveLen(int(before)))
	})
})

var _ = Describe("DRA device binding", Ordered, Label("dra", "binding"), func() {
	var t *bindingTarget

	BeforeAll(func() {
		node := firstNodeName()

		// The specs that need no GPU of their own still run without one.
		if bdf := os.Getenv(vfioBDFEnv); bdf != "" {
			t = newBindingTarget(node, bdf)

			// The DRA driver's /sys is a bind mount of the node's, so it has to be writable before
			// the driver starts.
			if t.skipReason == "" {
				t.prepareNode()
			}
		}

		deployOperator()

		By("deploying GPU DRA with managed binding")
		tmpDir, err := os.MkdirTemp("/tmp/", "operator-binding-dra*")
		Expect(err).NotTo(HaveOccurred())
		defer os.RemoveAll(tmpDir)

		cp := dynamicResourceAllocationClusterPolicy()
		cp.Spec.UseNFDLabeling = false
		cp.Spec.ResourceMonitoring = false
		cp.Spec.DynamicResourceAllocationSpec.ManageBinding = true

		Expect(applyClusterPolicyToCluster(&cp, filepath.Join(tmpDir, "clusterpolicy.yaml"))).To(Succeed())
		Eventually(waitForDRAPodsToBecomeRunning).Should(Succeed())
		Eventually(waitForDRAResourceSlices).Should(Succeed())

		if t != nil && t.skipReason == "" {
			t.waitForSliceDriver(t.origDriver)
		}
	})

	AfterAll(func() {
		_, _ = utils.Run(exec.Command("kubectl", "delete", "clusterpolicy", "gpu-policy", "--ignore-not-found"))
		Eventually(waitForDRAPodsToGoAway).Should(Succeed())

		if t != nil && t.skipReason == "" {
			t.unprepareNode()
		}

		By("cleaning up operator deployment")
		_, _ = utils.Run(exec.Command("kubectl", "delete", "-k", "config/default"))
	})

	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			dumpBindingDebugInfo(t, draPluginLabel)
		}

		deleteBindingWorkloads()

		// The DRA driver leaves a GPU on vfio when its claim goes; only the next DRM claim would move
		// it back. Every spec starts with the GPU on its DRM driver.
		if t != nil && t.skipReason == "" {
			t.restore()
			t.waitForSliceDriver(t.origDriver)
		}
	})

	SetDefaultEventuallyTimeout(3 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)

	It("keeps each device class to its drivers unless binding is managed", func() {
		DeferCleanup(func() {
			// The pending vfio pod would take any GPU, the boot VGA device included, once binding is
			// managed again, so it goes first.
			deleteBindingWorkloads()
			setManageBinding(true)
		})

		setManageBinding(false)

		By("validating that the device classes select by driver")
		expectDeviceClassSelectors(false)

		if gpusOnVfio() {
			logf("a GPU is already on vfio; not checking that a vfio claim stays unallocated")
		} else {
			By("validating that a vfio claim is not allocated while every GPU is on its DRM driver")
			Expect(applyManifest(bindingPodManifest(bindingPod, bindingClaim, vfioDeviceClass, ""))).To(Succeed())
			Consistently(func(g Gomega) {
				phase, nodeName := podPhaseAndNode(g, bindingPod)
				g.Expect(phase).To(Equal(string(core.PodPending)))
				g.Expect(nodeName).To(BeEmpty(), "the vfio claim's pod was scheduled")
			}).WithTimeout(15 * time.Second).Should(Succeed())

			deleteBindingWorkloads()
		}

		setManageBinding(true)

		By("validating that the device classes no longer select by driver")
		expectDeviceClassSelectors(true)
	})

	It("binds a GPU to vfio for a "+vfioDeviceClass+" claim", func() {
		requireBindingTarget(t, "")

		By("launching a pod with a " + vfioDeviceClass + " claim for the GPU")
		Expect(applyManifest(bindingPodManifest(bindingPod, bindingClaim, vfioDeviceClass, t.bdf))).To(Succeed())
		Eventually(func(g Gomega) { waitForNamedPodToBecomeRunning(bindingPod, bindingPodNs, g) }).Should(Succeed())

		t.expectOnVfio()

		By("validating that the pod has the GPU's vfio devices and no DRM devices")
		Expect(podExec(bindingPod, "ls /dev/vfio")).To(ContainSubstring(t.iommuGroup))
		Expect(podExec(bindingPod, "ls /dev/dri 2>/dev/null || true")).NotTo(ContainSubstring("renderD"))

		By("validating that the pod's device metadata names the GPU, as KubeVirt reads it")
		// The file is <driver>-metadata.json, under resourceclaimtemplates/<podClaimName>/<requestName>/
		// for a claim from a template (k8s.io/dynamic-resource-allocation/api/metadata).
		metadata := podExec(bindingPod, fmt.Sprintf("cat %s/resourceclaimtemplates/gpu/gpu/%s-metadata.json || true",
			draDeviceMetadataDir, common.GPUDeviceClass))
		Expect(metadata).To(ContainSubstring(t.bdf), "metadata files in the pod:\n%s",
			podExec(bindingPod, "find "+draDeviceMetadataDir+" || true"))
	})

	It("moves a GPU from xe to vfio and back between workloads", Label("long"), func() {
		requireBindingTarget(t, "")

		runDRMWorkload := func() {
			By("launching a pod with a " + common.GPUDeviceClass + " claim for the GPU")
			Expect(applyManifest(bindingPodManifest(bindingPod, bindingClaim, common.GPUDeviceClass, t.bdf))).To(Succeed())
			Eventually(func(g Gomega) { waitForNamedPodToBecomeRunning(bindingPod, bindingPodNs, g) }).Should(Succeed())

			t.expectOnDRM(bindingPod)
			deleteBindingWorkloads()
		}

		runDRMWorkload()

		By("launching a pod with a " + vfioDeviceClass + " claim for the GPU")
		Expect(applyManifest(bindingPodManifest(bindingPod, bindingClaim, vfioDeviceClass, t.bdf))).To(Succeed())
		Eventually(func(g Gomega) { waitForNamedPodToBecomeRunning(bindingPod, bindingPodNs, g) }).Should(Succeed())

		t.expectOnVfio()
		Expect(podExec(bindingPod, "ls /dev/vfio")).To(ContainSubstring(t.iommuGroup))
		deleteBindingWorkloads()

		// The DRA driver leaves the GPU on vfio, so it is the DRM claim's prepare, not the suite, that
		// moves it back. Were that ever to change, the last workload would not exercise the rebind.
		logf("%s is on %q once its vfio claim is gone", t.bdf, t.driver())

		runDRMWorkload()
	})

	It("starts a KubeVirt VM with the GPU from a "+vfioDeviceClass+" claim", Label("long", "kubevirt"), func() {
		requireBindingTarget(t, "")

		if _, err := t.nodeSh("test -c /dev/kvm"); err != nil {
			Skip("node " + t.node + " has no /dev/kvm")
		}

		version := os.Getenv(kubevirtVersionEnv)
		if version == "" {
			version = kubevirtDefaultVersion
		}

		pullKubeVirtImages(version)

		installKubeVirt(version)
		DeferCleanup(uninstallKubeVirt, version)

		By("starting a VM with the GPU")
		Expect(applyManifest(bindingVMIManifest(t.bdf, version))).To(Succeed())

		Eventually(func(g Gomega) {
			out, err := utils.Run(exec.Command("kubectl", "get", "vmi", bindingVMI, "-n", bindingPodNs,
				"-o", "jsonpath={.status.phase}"))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(Equal("Running"))
		}).WithTimeout(10*time.Minute).Should(Succeed(), "VM %s did not start", bindingVMI)

		t.expectOnVfio()

		By("validating that QEMU was given the GPU through vfio")
		Expect(qemuCmdlines(t)).To(SatisfyAll(ContainSubstring("vfio-pci"), ContainSubstring(shortBDF(t.bdf))))
	})
})

// setManageBinding sets the ClusterPolicy's manageBinding and waits for the DRA driver to run with it.
func setManageBinding(manage bool) {
	GinkgoHelper()

	By(fmt.Sprintf("setting manageBinding to %t", manage))
	_, err := utils.Run(exec.Command("kubectl", "patch", "clusterpolicy", "gpu-policy", "--type=merge",
		"-p", fmt.Sprintf(`{"spec":{"dra":{"manageBinding":%t}}}`, manage)))
	Expect(err).NotTo(HaveOccurred())

	arg := fmt.Sprintf("--manage-binding=%t", manage)

	Eventually(func(g Gomega) {
		out, err := utils.Run(exec.Command("kubectl", "get", "daemonset", draDaemonSet, "-n", namespace,
			"-o", "jsonpath={.spec.template.spec.containers[0].args}"))
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(out).To(ContainSubstring(arg))
	}).Should(Succeed())

	_, err = utils.Run(exec.Command("kubectl", "rollout", "status", "daemonset/"+draDaemonSet, "-n", namespace, "--timeout", "3m"))
	Expect(err).NotTo(HaveOccurred())
	Eventually(waitForDRAPodsToBecomeRunning).Should(Succeed())
}

// expectDeviceClassSelectors waits for both device classes to have the selectors the operator
// gives them for manageBinding.
func expectDeviceClassSelectors(manageBinding bool) {
	GinkgoHelper()

	for _, want := range []*resv1.DeviceClass{
		deployments.DynamicResourceAllocationDeviceClass(!manageBinding),
		deployments.DynamicResourceAllocationDeviceClassVfio(!manageBinding),
	} {
		Eventually(func(g Gomega) {
			g.Expect(deviceClassExpressions(g, want.Name)).To(Equal(celExpressions(want.Spec.Selectors)))
		}).Should(Succeed(), "device class %s", want.Name)
	}
}

func deviceClassExpressions(g Gomega, name string) []string {
	out, err := utils.Run(exec.Command("kubectl", "get", "deviceclass", name, "-o", "json"))
	g.Expect(err).NotTo(HaveOccurred())

	dc := resv1.DeviceClass{}
	g.Expect(json.Unmarshal([]byte(out), &dc)).To(Succeed())

	return celExpressions(dc.Spec.Selectors)
}

func celExpressions(selectors []resv1.DeviceSelector) []string {
	exprs := []string{}

	for _, s := range selectors {
		if s.CEL != nil {
			exprs = append(exprs, s.CEL.Expression)
		}
	}

	return exprs
}

// gpusOnVfio reports whether the DRA driver lists any GPU as bound to a vfio driver.
func gpusOnVfio() bool {
	for _, slice := range gpuResourceSlices(Default) {
		for _, dev := range slice.Spec.Devices {
			if slices.Contains(vfioDrivers, deviceAttrString(dev.Attributes, draDriverAttr)) {
				return true
			}
		}
	}

	return false
}

// sliceDriver returns the driver the DRA driver lists the GPU at bdf as bound to.
func sliceDriver(g Gomega, bdf string) string {
	for _, slice := range gpuResourceSlices(g) {
		for _, dev := range slice.Spec.Devices {
			if deviceAttrString(dev.Attributes, common.DeviceAttrBDF) == bdf {
				return deviceAttrString(dev.Attributes, draDriverAttr)
			}
		}
	}

	g.Expect(false).To(BeTrue(), "no GPU %s in the ResourceSlices", bdf)

	return ""
}

func firstNodeName() string {
	out, err := utils.Run(exec.Command("kubectl", "get", "nodes", "-o", "jsonpath={.items[0].metadata.name}"))
	Expect(err).NotTo(HaveOccurred())

	return strings.TrimSpace(out)
}

func nodeAllocatable(g Gomega, node string, resource core.ResourceName) int64 {
	out, err := utils.Run(exec.Command("kubectl", "get", "node", node, "-o", "json"))
	g.Expect(err).NotTo(HaveOccurred())

	n := core.Node{}
	g.Expect(json.Unmarshal([]byte(out), &n)).To(Succeed())

	q := n.Status.Allocatable[resource]

	return q.Value()
}

func podPhaseAndNode(g Gomega, pod string) (string, string) {
	out, err := utils.Run(exec.Command("kubectl", "get", "pod", pod, "-n", bindingPodNs,
		"-o", "jsonpath={.status.phase} {.spec.nodeName}"))
	g.Expect(err).NotTo(HaveOccurred())

	phase, nodeName, _ := strings.Cut(strings.TrimSpace(out), " ")

	return phase, nodeName
}

func podExec(pod, script string) string {
	GinkgoHelper()

	out, err := utils.Run(exec.Command("kubectl", "exec", "-n", bindingPodNs, pod, "--", "sh", "-c", script))
	Expect(err).NotTo(HaveOccurred())

	return out
}

// deleteBindingWorkloads deletes the binding specs' pods, VM and claim templates, and waits for
// their claims to be gone, so that the GPU is free for the next workload.
func deleteBindingWorkloads() {
	_, _ = utils.Run(exec.Command("kubectl", "delete", "vmi", bindingVMI, "-n", bindingPodNs,
		"--ignore-not-found", "--wait", "--timeout", "2m"))

	// Not forced: a forced delete removes the pod before the kubelet has unprepared its claim, and the
	// next pod's claim for the same GPU then fails to prepare until that cleanup catches up. The pods
	// have a 1s grace period.
	for _, args := range [][]string{
		{"pod", bindingPod, bindingDPPod, "--wait", "--timeout", "2m"},
		{"resourceclaimtemplate", bindingClaim, bindingVMIClaim},
	} {
		_, _ = utils.Run(exec.Command("kubectl", append([]string{"delete", "-n", bindingPodNs, "--ignore-not-found"}, args...)...))
	}

	Eventually(func() []string {
		return utils.GetNonEmptyLines(mustRun("kubectl", "get", "resourceclaims", "-n", bindingPodNs, "-o", "name"))
	}).WithTimeout(2*time.Minute).Should(BeEmpty(), "the binding workloads' ResourceClaims are not gone")
}

// kubevirtImages are the images the KubeVirt install and the VM run. virt-operator deploys the
// other components at its own tag, and virt-handler runs virt-launcher as an init container too.
func kubevirtImages(version string) []string {
	images := []string{}

	for _, name := range []string{"virt-operator", "virt-api", "virt-controller", "virt-handler", "virt-launcher",
		kubevirtContainerDisk} {
		images = append(images, fmt.Sprintf("%s/%s:%s", kubevirtRegistry, name, version))
	}

	return images
}

// pullKubeVirtImages loads the KubeVirt images on Kind, so that installing KubeVirt and starting the
// VM only wait for KubeVirt itself.
func pullKubeVirtImages(version string) {
	GinkgoHelper()

	for _, image := range kubevirtImages(version) {
		By("loading image " + image + " to kind cluster")
		_, err := utils.Run(exec.Command("docker", "pull", image))
		Expect(err).NotTo(HaveOccurred())
		Expect(utils.LoadImageToKindClusterWithName(image)).To(Succeed())
	}
}

func installKubeVirt(version string) {
	GinkgoHelper()

	By("installing KubeVirt " + version + " with GPUsWithDRA")
	_, err := utils.Run(exec.Command("kubectl", "apply", "-f", fmt.Sprintf(kubevirtReleaseURL, version)))
	Expect(err).NotTo(HaveOccurred())

	Expect(applyManifest(kubevirtCRManifest())).To(Succeed())

	_, err = utils.Run(exec.Command("kubectl", "wait", "kubevirt", "kubevirt", "-n", kubevirtNamespace,
		"--for", "condition=Available", "--timeout", "10m"))
	Expect(err).NotTo(HaveOccurred(), "KubeVirt did not become available")
}

// uninstallKubeVirt removes KubeVirt. The KubeVirt CR goes first: with the operator gone, nothing
// would remove what it deployed.
func uninstallKubeVirt(version string) {
	By("uninstalling KubeVirt")
	_, _ = utils.Run(exec.Command("kubectl", "delete", "vmi", "--all", "-A", "--wait"))
	_, _ = utils.Run(exec.Command("kubectl", "delete", "kubevirt", "kubevirt", "-n", kubevirtNamespace,
		"--ignore-not-found", "--wait", "--timeout", "5m"))
	_, _ = utils.Run(exec.Command("kubectl", "delete", "-f", fmt.Sprintf(kubevirtReleaseURL, version),
		"--ignore-not-found", "--wait", "--timeout", "5m"))
}

// qemuCmdlines returns the command lines of the QEMU processes on the node.
func qemuCmdlines(t *bindingTarget) string {
	out, err := t.nodeSh(`for p in /proc/[0-9]*; do
  c=$(tr '\0' ' ' < "$p/cmdline" 2>/dev/null) || continue
  case "$c" in *qemu*) echo "$c" ;; esac
done`)
	Expect(err).NotTo(HaveOccurred())

	return out
}

// shortBDF drops the PCI domain, which QEMU's host= may leave out.
func shortBDF(bdf string) string {
	if _, rest, ok := strings.Cut(bdf, ":"); ok && strings.Count(bdf, ":") == 2 {
		return rest
	}

	return bdf
}

func dumpBindingDebugInfo(t *bindingTarget, pluginLabel string) {
	cmds := [][]string{
		{"kubectl", "get", "resourceslices", "-o", "yaml"},
		{"kubectl", "get", "deviceclasses", "-o", "yaml"},
		{"kubectl", "get", "resourceclaims", "-A", "-o", "yaml"},
		{"kubectl", "logs", "-n", namespace, "-l", pluginLabel, "--all-containers", "--prefix", "--tail=200"},
		{"kubectl", "logs", "-n", namespace, "-l", "control-plane=controller-manager", "--tail=200"},
		{"kubectl", "describe", "pods", "-n", bindingPodNs},
		{"kubectl", "get", "vmi", "-A", "-o", "yaml"},
		{"kubectl", "get", "pods", "-A", "-o", "wide"},
		{"kubectl", "get", "events", "-A", "--sort-by=.lastTimestamp"},
	}

	if t != nil {
		cmds = append(cmds,
			[]string{"docker", "exec", t.node, "sh", "-c", fmt.Sprintf(
				"ls -l /sys/bus/pci/devices/%[1]s/driver; cat /sys/bus/pci/devices/%[1]s/driver_override", t.bdf)},
			[]string{"docker", "exec", t.node, "sh", "-c", "dmesg | tail -50"})
	}

	for _, args := range cmds {
		logf("--- %s\n%s", strings.Join(args, " "), mustRun(args...))
	}
}

// bindingClaimTemplateManifest is a claim template for one GPU of deviceClass: the one at bdf, or
// any if bdf is empty.
func bindingClaimTemplateManifest(claim, deviceClass, bdf string) string {
	selectors := ""
	if bdf != "" {
		selectors = fmt.Sprintf(`
          selectors:
          - cel:
              expression: device.attributes["%s"].%s == "%s"`, common.GPUDeviceClass, common.DeviceAttrBDF, bdf)
	}

	return fmt.Sprintf(`apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  name: %[1]s
  namespace: %[2]s
spec:
  spec:
    devices:
      requests:
      - name: gpu
        exactly:
          deviceClassName: %[3]s%[4]s
`, claim, bindingPodNs, deviceClass, selectors)
}

// bindingPodManifest is a pod holding one GPU of deviceClass through a claim template.
func bindingPodManifest(pod, claim, deviceClass, bdf string) string {
	return bindingClaimTemplateManifest(claim, deviceClass, bdf) + fmt.Sprintf(`---
apiVersion: v1
kind: Pod
metadata:
  name: %[1]s
  namespace: %[2]s
spec:
  terminationGracePeriodSeconds: 1
  containers:
  - name: gpu
    image: ubuntu:24.04
    command: ["sleep", "infinity"]
    resources:
      claims:
      - name: gpu
  resourceClaims:
  - name: gpu
    resourceClaimTemplateName: %[3]s
`, pod, bindingPodNs, claim)
}

// dpAllGPUsPodManifest is a pod holding count units of the device plugin's resource.
func dpAllGPUsPodManifest(resource core.ResourceName, count int64) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: %[1]s
  namespace: %[2]s
spec:
  terminationGracePeriodSeconds: 1
  containers:
  - name: gpu
    image: ubuntu:24.04
    command: ["sleep", "infinity"]
    resources:
      limits:
        %[3]s: "%[4]d"
`, bindingDPPod, bindingPodNs, resource, count)
}

// kubevirtCRManifest enables KubeVirt's DRA GPUs, which the DRA driver's KubeVirt support needs.
func kubevirtCRManifest() string {
	return fmt.Sprintf(`apiVersion: kubevirt.io/v1
kind: KubeVirt
metadata:
  name: kubevirt
  namespace: %s
spec:
  imagePullPolicy: IfNotPresent
  configuration:
    developerConfiguration:
      featureGates:
      - GPUsWithDRA
`, kubevirtNamespace)
}

// bindingVMIManifest is a CirrOS VM with the GPU at bdf from a vfio claim, as in the DRA driver's
// KubeVirt example (deployments/gpu/examples/kubevirt-vmi-inline-gpu.yaml).
func bindingVMIManifest(bdf, kubevirtVersion string) string {
	return bindingClaimTemplateManifest(bindingVMIClaim, vfioDeviceClass, bdf) + fmt.Sprintf(`---
apiVersion: kubevirt.io/v1
kind: VirtualMachineInstance
metadata:
  name: %[1]s
  namespace: %[2]s
spec:
  domain:
    devices:
      disks:
      - name: containerdisk
        disk:
          bus: virtio
      gpus:
      - name: gpu
        claimName: gpu
        requestName: gpu
    memory:
      guest: 1Gi
    resources: {}
  resourceClaims:
  - name: gpu
    resourceClaimTemplateName: %[3]s
  terminationGracePeriodSeconds: 0
  volumes:
  - name: containerdisk
    containerDisk:
      image: %[4]s/%[5]s:%[6]s
      imagePullPolicy: IfNotPresent
`, bindingVMI, bindingPodNs, bindingVMIClaim, kubevirtRegistry, kubevirtContainerDisk, kubevirtVersion)
}
