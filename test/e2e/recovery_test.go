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
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	batch "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	api "github.com/intel/gpu-base-operator/api/v1alpha1"
	"github.com/intel/gpu-base-operator/internal/controller/common"
	"github.com/intel/gpu-base-operator/test/utils"
)

const (
	// recoveryBDFEnv names the GPU the recovery specs break, by PCI address. It is required: the
	// specs really wedge the GPU, so it is never picked for the user. Must be a dedicated discrete
	// GPU on xe, and not the boot VGA device.
	recoveryBDFEnv = "E2E_RECOVERY_BDF"
	// recoveryBDF2Env optionally names a second such GPU on the same node, for the specs about
	// sibling GPUs and one recovery per node.
	recoveryBDF2Env = "E2E_RECOVERY_BDF2"

	// Mirrors of the operator's internal names (internal/controller/gpurecoveryplan).
	recoveryNodeTaint = "gpurecoveryplan.intel.com/recovery"
	recoveryPlanLabel = "gpurecoveryplan.intel.com/plan"

	recoveryPodNs    = "default"
	recoveryGPUPod   = "recovery-gpupod"
	recoveryGPUClaim = "recovery-gpu-claim"
	recoveryHoldPod  = "recovery-holdpod"
	recoveryHoldPDB  = "recovery-holdpod-pdb"

	draPluginLabel = "app=intel-gpu-resource-driver-kubelet-plugin"
	xpumdLabel     = "app=intel-xpumanager"

	// The mock's output, test/fake-xpu-smi.
	mockReflashDone = "fake-xpu-smi: firmware update complete (simulated)"
)

// recoveryTarget is the GPU the recovery specs break, and what the plans point at.
type recoveryTarget struct {
	fault   gpuFault
	sibling *gpuFault
	pciID   string
	// siblingPCIID is the sibling's PCI ID. One plan only covers both GPUs if it is the same.
	siblingPCIID string

	xpuSmiImage string
	fwImage     string
	badImage    string
}

func (t *recoveryTarget) node() string { return t.fault.node }

func (t *recoveryTarget) faults() []gpuFault {
	if t.sibling == nil {
		return []gpuFault{t.fault}
	}

	return []gpuFault{t.fault, *t.sibling}
}

// newRecoveryPlan returns a plan for the target's GPU model with no approvals and no firmware.
//
// The drain covers the whole node, and Kind has one node. Without the skips it would evict
// coredns, the storage provisioner, cert-manager and the registry, which the recovery taint then
// keeps from coming back, so the Job could never pull its image.
func (t *recoveryTarget) newRecoveryPlan(name string) *api.GPURecoveryPlan {
	return &api.GPURecoveryPlan{
		TypeMeta:   metav1.TypeMeta{APIVersion: api.GroupVersion.String(), Kind: "GPURecoveryPlan"},
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: api.GPURecoveryPlanSpec{
			DeviceID:         t.pciID,
			DefaultResetType: api.RecoveryTypeSBR,
			XpuSmi:           api.XpuSmiSpec{Image: t.xpuSmiImage, PullPolicy: "IfNotPresent"},
			Drain: api.DrainSpec{
				TimeoutSeconds:   120,
				NamespacesToSkip: []string{"kube-system", "local-path-storage", "cert-manager", registryNamespace},
			},
		},
	}
}

func (t *recoveryTarget) firmware(file string) *api.FirmwareSpec {
	return &api.FirmwareSpec{
		Source: api.FirmwareSource{ContainerSource: &api.ContainerFirmwareSource{Name: t.fwImage}},
		File:   file,
	}
}

// wedge wedges a GPU and waits for the DRA driver to taint it.
func (t *recoveryTarget) wedge(f gpuFault) {
	f.wedge()

	By("waiting for the DRA driver to taint the wedged device")
	waitForDeviceTaint(f, common.DeviceTaintKeyReset, 30*time.Second)
}

// enterSurvivability puts a GPU into survivability mode and waits for the DRA driver to taint it.
//
// The driver may only notice survivability mode when it enumerates the devices, as on a boot. If
// the taint does not show up by itself, its pod on the node is restarted, which is what a boot
// would do.
func (t *recoveryTarget) enterSurvivability(f gpuFault) {
	f.enterSurvivability()

	By("waiting for the DRA driver to taint the device in survivability mode")

	if pollUntil(90*time.Second, func() bool {
		keys, _ := lenientDeviceTaintKeys(f)
		return slices.Contains(keys, common.DeviceTaintKeyReflash)
	}) {
		_, _ = fmt.Fprintf(GinkgoWriter, "DRA driver tainted %s on its own\n", f.bdf)
		return
	}

	By("restarting the DRA driver on the node, as a boot would")
	_, err := utils.Run(exec.Command("kubectl", "delete", "pod", "-n", namespace, "-l", draPluginLabel,
		"--field-selector", "spec.nodeName="+f.node))
	Expect(err).NotTo(HaveOccurred())
	Eventually(waitForDRAPodsToBecomeRunning).Should(Succeed())

	waitForDeviceTaint(f, common.DeviceTaintKeyReflash, 3*time.Minute)
	_, _ = fmt.Fprintf(GinkgoWriter, "DRA driver tainted %s after a restart\n", f.bdf)
}

var _ = Describe("GPU recovery", Ordered, Label("recovery"), func() {
	var t *recoveryTarget

	BeforeAll(func() {
		bdf := os.Getenv(recoveryBDFEnv)
		if bdf == "" {
			Skip(recoveryBDFEnv + " is not set; the recovery specs need a dedicated GPU they are allowed to break")
		}

		if !deviceTaintsEnabled() {
			Skip("the DRADeviceTaints feature gate is off, so the DRA driver cannot taint a GPU that needs recovery; " +
				"create the cluster with test/e2e/kind-config.yaml ('make setup-test-e2e')")
		}

		deployOperatorWithRegistry()

		Expect(utils.LoadImageToKindClusterWithName(gpuHangImage)).To(Succeed(),
			"Failed to load the gpu-hang image; build it with 'make docker-build-e2e'")

		By("deploying GPU DRA with resource monitoring, which wedge detection needs")
		tmpDir, err := os.MkdirTemp("/tmp/", "operator-recovery*")
		Expect(err).NotTo(HaveOccurred())
		defer os.RemoveAll(tmpDir)

		cp := dynamicResourceAllocationClusterPolicy()
		cp.Spec.UseNFDLabeling = false
		cp.Spec.ResourceMonitoring = true

		Expect(applyClusterPolicyToCluster(&cp, filepath.Join(tmpDir, "clusterpolicy.yaml"))).To(Succeed())
		Eventually(waitForDRAPodsToBecomeRunning).Should(Succeed())
		Eventually(waitForXpumPodsToBecomeRunning).Should(Succeed())

		By("finding the target GPU in the DRA ResourceSlice")
		target := &recoveryTarget{}

		var node string

		Eventually(func(g Gomega) {
			node, target.pciID = gpuInResourceSlices(g, bdf)
		}).Should(Succeed(), "%s=%s is not in any %s ResourceSlice", recoveryBDFEnv, bdf, common.GPUDeviceClass)

		allowBootVGA := os.Getenv(recoveryAllowBootVGAEnv) == "true"

		target.fault = gpuFault{node: node, bdf: bdf, allowBootVGA: allowBootVGA}
		target.fault.checkTarget()

		if bdf2 := os.Getenv(recoveryBDF2Env); bdf2 != "" {
			var node2 string

			Eventually(func(g Gomega) {
				node2, target.siblingPCIID = gpuInResourceSlices(g, bdf2)
			}).Should(Succeed(), "%s=%s is not in any %s ResourceSlice", recoveryBDF2Env, bdf2, common.GPUDeviceClass)
			Expect(node2).To(Equal(node), "%s must be on the same node as %s", recoveryBDF2Env, recoveryBDFEnv)

			target.sibling = &gpuFault{node: node, bdf: bdf2, allowBootVGA: allowBootVGA}
			target.sibling.checkTarget()
		}

		target.fault.prepareNode()

		startRegistry()

		By("pushing the mock xpu-smi and the firmware content images to the registry")
		target.xpuSmiImage = pushToNodeRegistry(node, fwUpdaterMockImage, "e2e/xpu-smi")
		target.fwImage = pushToNodeRegistry(node, fwContentE2EImage, "e2e/fwfiles")
		target.badImage = fmt.Sprintf("%s:%d/e2e/does-not-exist:e2e", nodeInternalIP(node), registryNodePort)

		_, _ = fmt.Fprintf(GinkgoWriter, "Recovery target: node %s, GPU %s (%s), sibling %+v\n",
			node, bdf, target.pciID, target.sibling)

		for _, f := range target.faults() {
			waitForGPUHealthy(f)
		}

		t = target
	})

	AfterAll(func() {
		// Skipped or failed in BeforeAll before the target was set up.
		if t == nil {
			return
		}

		stopRegistry()

		_, _ = utils.Run(exec.Command("kubectl", "delete", "clusterpolicy", "gpu-policy"))
		Eventually(waitForDRAPodsToGoAway).Should(Succeed())

		t.fault.unprepareNode()

		By("cleaning up operator deployment")
		_, _ = utils.Run(exec.Command("kubectl", "delete", "-k", "config/default"))
	})

	AfterEach(func() {
		if t == nil {
			return
		}

		failed := CurrentSpecReport().Failed()
		if failed {
			dumpRecoveryDebugInfo(t)
		}

		By("removing recovery plans and Jobs")
		deleteAllRecoveryPlans()
		_, _ = utils.Run(exec.Command("kubectl", "taint", "node", t.node(), recoveryNodeTaint+"-"))

		By("removing test pods")
		for _, args := range [][]string{
			{"pod", recoveryGPUPod, recoveryHoldPod, "--grace-period=0", "--force"},
			{"resourceclaimtemplate", recoveryGPUClaim},
			{"poddisruptionbudget", recoveryHoldPDB},
		} {
			_, _ = utils.Run(exec.Command("kubectl", append([]string{"delete", "-n", recoveryPodNs, "--ignore-not-found"}, args...)...))
		}

		// One broken spec must not cascade into the rest: every GPU is healthy again before the next.
		By("restoring the GPUs")
		for _, f := range t.faults() {
			keys, listed := lenientDeviceTaintKeys(f)
			f.restore(failed || !listed || len(recoveryTaints(keys)) > 0)
			waitForGPUHealthy(f)
		}
	})

	SetDefaultEventuallyTimeout(5 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)

	It("drains the node and resets a wedged GPU once the event is approved", Label("wedged"), func() {
		By("launching a pod holding the target GPU")
		Expect(applyManifest(pinnedGPUPodManifest(t.fault.bdf, 5))).To(Succeed())
		Eventually(func(g Gomega) { waitForNamedPodToBecomeRunning(recoveryGPUPod, recoveryPodNs, g) }).Should(Succeed())

		// The DRA driver's wedged taint is NoExecute, so it evicts the GPU pod itself, long before the
		// approval. This pod uses no GPU, so only the drain removes it, and its grace period keeps the
		// event in draining for long enough to be seen.
		By("launching a pod the drain has to evict")
		Expect(applyManifest(holdPodManifest(5, false))).To(Succeed())
		Eventually(func(g Gomega) { waitForNamedPodToBecomeRunning(recoveryHoldPod, recoveryPodNs, g) }).Should(Succeed())

		plan := t.newRecoveryPlan("recovery-wedged")
		Expect(applyRecoveryPlan(plan)).To(Succeed())

		t.wedge(t.fault)

		By("waiting for the recovery event")
		evt := waitForEventState(plan.Name, t.fault.bdf, api.RecoveryEventStateWaitingApproval)
		Expect(evt.Reason).To(Equal("gpu-wedged"))
		Expect(evt.RecoveryType.Type).To(Equal(api.RecoveryTypeSBR))
		Expect(evt.NodeName).To(Equal(t.node()))
		Expect(evt.GPUBDF).To(Equal(t.fault.bdf))

		By("validating that nothing happens without an approval")
		Consistently(func(g Gomega) {
			g.Expect(eventFor(getRecoveryPlan(plan.Name, g), t.fault.bdf).State).To(Equal(api.RecoveryEventStateWaitingApproval))
			g.Expect(recoveryJobNames(plan.Name)).To(BeEmpty())
			g.Expect(nodeHasTaint(t.node(), recoveryNodeTaint)).To(BeFalse())
		}).WithTimeout(10 * time.Second).WithPolling(2 * time.Second).Should(Succeed())

		approve(plan.Name, api.RecoveryApproval{EventID: evt.ID, Comment: "e2e"})

		tr := followEvents(plan.Name, t.node(), t.fault.bdf)[t.fault.bdf]
		expectRecovered(t, t.fault, plan.Name, tr)

		Expect(tr.states).To(ContainElements(api.RecoveryEventStateDraining, api.RecoveryEventStateInProgress))
		Expect(tr.sawNodeTaint).To(BeTrue(), "the node was never tainted for the drain")
		Expect(podExists(recoveryHoldPod, recoveryPodNs)).To(BeFalse(), "the drain did not evict %s", recoveryHoldPod)
		Expect(podExists(recoveryGPUPod, recoveryPodNs)).To(BeFalse(), "the GPU pod survived the wedge and the drain")

		Expect(tr.logs(tr.lastJob())).To(SatisfyAll(
			ContainSubstring("performing SBR (Secondary Bus Reset) on %s", t.fault.bdf),
			ContainSubstring("rebind complete: %s via driver xe", t.fault.bdf)))

		By("validating that the approval was spent")
		for _, a := range getRecoveryPlan(plan.Name, Default).Spec.Approvals {
			if a.ID == tr.last.ApprovalID {
				Expect(a.Consumed).To(BeTrue(), "approval %s", a.ID)
			}
		}

		if t.sibling != nil {
			Expect(recoveryTaints(deviceTaintKeys(Default, *t.sibling))).To(BeEmpty(),
				"sibling GPU %s needs recovery too", t.sibling.bdf)
		}

		By("validating that the GPU takes workloads again")
		Expect(applyManifest(pinnedGPUPodManifest(t.fault.bdf, 5))).To(Succeed())
		Eventually(func(g Gomega) { waitForNamedPodToBecomeRunning(recoveryGPUPod, recoveryPodNs, g) }).Should(Succeed())
	})

	It("reflashes a GPU in survivability mode once firmware is configured", Label("survivability"), func() {
		plan := t.newRecoveryPlan("recovery-survivability")
		Expect(applyRecoveryPlan(plan)).To(Succeed())

		t.enterSurvivability(t.fault)

		By("waiting for the recovery event")
		evt := waitForEventState(plan.Name, t.fault.bdf, api.RecoveryEventStateWaitingApproval)
		Expect(evt.Reason).To(Equal("survivability-mode"))
		Expect(evt.RecoveryType.Type).To(Equal(api.RecoveryTypeReflash))

		approve(plan.Name, api.RecoveryApproval{EventID: evt.ID})

		By("validating that the reflash waits for firmware")
		waitForEventState(plan.Name, t.fault.bdf, api.RecoveryEventStateMissingFirmware)
		Expect(recoveryJobNames(plan.Name)).To(BeEmpty())

		By("configuring the firmware")
		Expect(patchRecoveryPlan(plan.Name, map[string]any{"spec": map[string]any{"firmware": t.firmware("fdo.bin")}})).To(Succeed())

		tr := followEvents(plan.Name, t.node(), t.fault.bdf)[t.fault.bdf]
		expectRecovered(t, t.fault, plan.Name, tr)

		By("validating that the reflash skipped the drain")
		Expect(tr.states).To(ContainElement(api.RecoveryEventStateInProgress))
		Expect(tr.states).NotTo(ContainElement(api.RecoveryEventStateDraining))
		Expect(tr.sawNodeTaint).To(BeFalse(), "a reflash must not taint the node")

		job := tr.jobs[tr.lastJob()]
		Expect(job).NotTo(BeNil(), "Job %s was gone before it could be read", tr.lastJob())
		Expect(job.Spec.Template.Spec.InitContainers).To(ContainElement(SatisfyAll(
			HaveField("Name", "fw-copy"), HaveField("Image", t.fwImage))))

		Expect(tr.logs(tr.lastJob())).To(SatisfyAll(
			ContainSubstring(mockReflashDone),
			ContainSubstring("cleared survivability mode")))

		By("validating that the GPU is out of survivability mode")
		Expect(t.fault.inSurvivability()).To(BeFalse())
		// The mock creates the configfs group if it has to, so a missing one means it never wrote 0.
		Expect(t.fault.configfsSurvivabilityMode()).To(Equal("0"), "the reflash did not clear configfs survivability_mode")
		Expect(t.fault.nodeSh(fmt.Sprintf("ls /sys/bus/pci/devices/%s/drm | grep -q ^card", t.fault.bdf))).Error().
			NotTo(HaveOccurred(), "the GPU has no DRM node")
	})

	It("holds an approved event while its xpu-smi image cannot be pulled, touching nothing", Label("wedged"), func() {
		plan := t.newRecoveryPlan("recovery-bad-image")
		plan.Spec.XpuSmi.Image = t.badImage
		Expect(applyRecoveryPlan(plan)).To(Succeed())

		t.wedge(t.fault)

		evt := waitForEventState(plan.Name, t.fault.bdf, api.RecoveryEventStateWaitingApproval)
		approve(plan.Name, api.RecoveryApproval{EventID: evt.ID})

		By("waiting for the pre-flight check to hold the event")
		Eventually(func(g Gomega) {
			e := eventFor(getRecoveryPlan(plan.Name, g), t.fault.bdf)
			g.Expect(e).NotTo(BeNil())
			g.Expect(e.StateMessage).To(ContainSubstring("spec.xpuSmi.image"))
		}).Should(Succeed())

		By("validating that the node and the GPU are left alone, with the approval still in place")
		Consistently(func(g Gomega) {
			p := getRecoveryPlan(plan.Name, g)
			g.Expect(eventFor(p, t.fault.bdf).State).To(Equal(api.RecoveryEventStateWaitingApproval))
			g.Expect(p.Spec.Approvals).To(ContainElement(SatisfyAll(
				HaveField("EventID", evt.ID), HaveField("Consumed", false))))
			g.Expect(recoveryJobNames(plan.Name)).To(BeEmpty())
			g.Expect(nodeHasTaint(t.node(), recoveryNodeTaint)).To(BeFalse())
		}).WithTimeout(15 * time.Second).WithPolling(2 * time.Second).Should(Succeed())

		By("fixing the image")
		Expect(patchRecoveryPlan(plan.Name, map[string]any{"spec": map[string]any{"xpuSmi": map[string]any{"image": t.xpuSmiImage}}})).
			To(Succeed())

		tr := followEvents(plan.Name, t.node(), t.fault.bdf)[t.fault.bdf]
		Expect(tr.id).To(Equal(evt.ID), "the held event itself must proceed")
		expectRecovered(t, t.fault, plan.Name, tr)
	})

	It("fails a reflash the updater rejects, and retries it once re-approved", Label("long", "survivability"), func() {
		plan := t.newRecoveryPlan("recovery-reflash-retry")
		plan.Spec.Firmware = t.firmware("fdo-fail.bin")
		Expect(applyRecoveryPlan(plan)).To(Succeed())

		t.enterSurvivability(t.fault)

		evt := waitForEventState(plan.Name, t.fault.bdf, api.RecoveryEventStateWaitingApproval)
		approve(plan.Name, api.RecoveryApproval{EventID: evt.ID})

		tr := followEvents(plan.Name, t.node(), t.fault.bdf)[t.fault.bdf]
		failedJob := tr.lastJob()

		Expect(tr.last.State).To(Equal(api.RecoveryEventStateFailed), "states seen: %v", tr.states)
		Expect(tr.last.StateMessage).To(ContainSubstring(failedJob))
		Expect(jobFailed(failedJob)).To(BeTrue())
		Expect(tr.logs(failedJob)).To(ContainSubstring("FAKE_XPU_SMI_FAIL found in /update/fdo-fail.bin"))

		By("validating that the failed reflash left the GPU bound and in survivability mode")
		Expect(t.fault.driver()).To(Equal("xe"))
		Expect(t.fault.inSurvivability()).To(BeTrue())

		By("switching to good firmware and re-approving the event")
		Expect(patchRecoveryPlan(plan.Name, map[string]any{"spec": map[string]any{"firmware": map[string]any{"file": "fdo.bin"}}})).
			To(Succeed())
		approve(plan.Name, api.RecoveryApproval{EventID: evt.ID, Comment: "retry with fdo.bin"})

		// Out of failed first, which followEvents would take for the end of the event.
		waitForEventState(plan.Name, t.fault.bdf, api.RecoveryEventStateInProgress)

		tr = followEvents(plan.Name, t.node(), t.fault.bdf)[t.fault.bdf]
		expectRecovered(t, t.fault, plan.Name, tr)

		Expect(tr.jobNames).To(HaveLen(2), "one Job per attempt")
		Expect(tr.jobNames[0]).To(Equal(failedJob))
		Expect(tr.logs(tr.lastJob())).To(ContainSubstring(mockReflashDone))
	})

	It("runs the reset an approval overrides the suggested one with", Label("long", "wedged"), func() {
		plan := t.newRecoveryPlan("recovery-override")
		Expect(applyRecoveryPlan(plan)).To(Succeed())

		t.wedge(t.fault)

		evt := waitForEventState(plan.Name, t.fault.bdf, api.RecoveryEventStateWaitingApproval)
		approve(plan.Name, api.RecoveryApproval{
			EventID:  evt.ID,
			Override: &api.RecoveryOverride{RecoveryType: api.RecoveryTypeSlot},
		})

		tr := followEvents(plan.Name, t.node(), t.fault.bdf)[t.fault.bdf]
		expectRecovered(t, t.fault, plan.Name, tr)

		Expect(tr.last.RecoveryType.Type).To(Equal(api.RecoveryTypeSlot))
		Expect(tr.last.RecoveryType.SuggestedType).To(Equal(api.RecoveryTypeSBR))

		job := tr.jobs[tr.lastJob()]
		Expect(job).NotTo(BeNil(), "Job %s was gone before it could be read", tr.lastJob())
		Expect(job.Spec.Template.Spec.Containers[0].Args).To(ContainElement(ContainSubstring("--coldreset --force-reset-gpus")))
		Expect(tr.logs(tr.lastJob())).To(ContainSubstring("performing cold reset (PCIe slot power cycle) on %s", t.fault.bdf))
	})

	It("recovers without being asked under a persistent selector approval", Label("long", "wedged"), func() {
		plan := t.newRecoveryPlan("recovery-persistent")
		plan.Spec.Approvals = []api.RecoveryApproval{{
			Selector:   &api.ApprovalSelector{RecoveryType: api.RecoveryTypeSBR, NodeName: t.node()},
			Persistent: true,
		}}
		Expect(applyRecoveryPlan(plan)).To(Succeed())

		t.wedge(t.fault)

		tr := followEvents(plan.Name, t.node(), t.fault.bdf)[t.fault.bdf]
		expectRecovered(t, t.fault, plan.Name, tr)

		By("validating that the approval stays in place")
		Expect(getRecoveryPlan(plan.Name, Default).Spec.Approvals).To(ConsistOf(SatisfyAll(
			HaveField("Persistent", true), HaveField("Consumed", false))))
	})

	It("releases the node when the plan is deleted mid-drain", Label("long", "wedged"), func() {
		By("launching a pod that takes a minute to terminate")
		Expect(applyManifest(holdPodManifest(60, false))).To(Succeed())
		Eventually(func(g Gomega) { waitForNamedPodToBecomeRunning(recoveryHoldPod, recoveryPodNs, g) }).Should(Succeed())

		plan := t.newRecoveryPlan("recovery-delete-mid-drain")
		Expect(applyRecoveryPlan(plan)).To(Succeed())

		t.wedge(t.fault)

		evt := waitForEventState(plan.Name, t.fault.bdf, api.RecoveryEventStateWaitingApproval)
		approve(plan.Name, api.RecoveryApproval{EventID: evt.ID})

		waitForEventState(plan.Name, t.fault.bdf, api.RecoveryEventStateDraining)
		Expect(nodeHasTaint(t.node(), recoveryNodeTaint)).To(BeTrue())

		By("deleting the plan mid-drain")
		_, err := utils.Run(exec.Command("kubectl", "delete", "gpurecoveryplan", plan.Name, "--timeout", "1m"))
		Expect(err).NotTo(HaveOccurred())

		Expect(nodeHasTaint(t.node(), recoveryNodeTaint)).To(BeFalse())
		Expect(recoveryJobNames(plan.Name)).To(BeEmpty())
	})

	It("fails the event when the drain times out", Label("long", "wedged"), func() {
		By("launching a pod a PodDisruptionBudget keeps from being evicted")
		Expect(applyManifest(holdPodManifest(5, true))).To(Succeed())
		Eventually(func(g Gomega) { waitForNamedPodToBecomeRunning(recoveryHoldPod, recoveryPodNs, g) }).Should(Succeed())

		plan := t.newRecoveryPlan("recovery-drain-timeout")
		plan.Spec.Drain.TimeoutSeconds = 30
		Expect(applyRecoveryPlan(plan)).To(Succeed())

		t.wedge(t.fault)

		evt := waitForEventState(plan.Name, t.fault.bdf, api.RecoveryEventStateWaitingApproval)
		approve(plan.Name, api.RecoveryApproval{EventID: evt.ID})

		tr := followEvents(plan.Name, t.node(), t.fault.bdf)[t.fault.bdf]

		Expect(tr.last.State).To(Equal(api.RecoveryEventStateFailed), "states seen: %v", tr.states)
		Expect(tr.last.PodsBlockingDrain).To(ContainElement(recoveryPodNs + "/" + recoveryHoldPod))
		Expect(tr.jobNames).To(BeEmpty())

		Eventually(func() bool { return nodeHasTaint(t.node(), recoveryNodeTaint) }).
			WithTimeout(time.Minute).Should(BeFalse())
		Expect(podExists(recoveryHoldPod, recoveryPodNs)).To(BeTrue())
	})

	It("restarts a recovery whose drain timed out once the event is approved again", Label("long", "wedged"), func() {
		By("launching a pod a PodDisruptionBudget keeps from being evicted")
		Expect(applyManifest(holdPodManifest(5, true))).To(Succeed())
		Eventually(func(g Gomega) { waitForNamedPodToBecomeRunning(recoveryHoldPod, recoveryPodNs, g) }).Should(Succeed())

		plan := t.newRecoveryPlan("recovery-drain-retry")
		plan.Spec.Drain.TimeoutSeconds = 30
		Expect(applyRecoveryPlan(plan)).To(Succeed())

		t.wedge(t.fault)

		evt := waitForEventState(plan.Name, t.fault.bdf, api.RecoveryEventStateWaitingApproval)
		approve(plan.Name, api.RecoveryApproval{EventID: evt.ID, Comment: "first attempt"})

		tr := followEvents(plan.Name, t.node(), t.fault.bdf)[t.fault.bdf]
		Expect(tr.last.State).To(Equal(api.RecoveryEventStateFailed), "states seen: %v", tr.states)
		Expect(tr.last.StateMessage).To(ContainSubstring("timed out"))
		Expect(tr.jobNames).To(BeEmpty(), "a drain that timed out must not reset")

		By("validating that a failed event stays failed without a new approval")
		Consistently(func(g Gomega) {
			g.Expect(eventFor(getRecoveryPlan(plan.Name, g), t.fault.bdf).State).To(Equal(api.RecoveryEventStateFailed))
			g.Expect(nodeHasTaint(t.node(), recoveryNodeTaint)).To(BeFalse())
		}).WithTimeout(15 * time.Second).WithPolling(2 * time.Second).Should(Succeed())

		By("removing the PodDisruptionBudget and approving the event again")
		_, err := utils.Run(exec.Command("kubectl", "delete", "poddisruptionbudget", recoveryHoldPDB, "-n", recoveryPodNs))
		Expect(err).NotTo(HaveOccurred())

		approve(plan.Name, api.RecoveryApproval{EventID: evt.ID, Comment: "retry without the PDB"})

		// Out of failed first, which followEvents would take for the end of the event.
		Eventually(func(g Gomega) {
			g.Expect(eventFor(getRecoveryPlan(plan.Name, g), t.fault.bdf).State).NotTo(Equal(api.RecoveryEventStateFailed))
		}).WithTimeout(time.Minute).Should(Succeed())

		tr = followEvents(plan.Name, t.node(), t.fault.bdf)[t.fault.bdf]
		Expect(tr.id).To(Equal(evt.ID), "the failed event itself must be the one retried")
		expectRecovered(t, t.fault, plan.Name, tr)

		Expect(tr.jobNames).To(HaveLen(1), "the first attempt never got to a Job")
		Expect(podExists(recoveryHoldPod, recoveryPodNs)).To(BeFalse(), "the retried drain did not evict %s", recoveryHoldPod)
	})

	It("runs one recovery per node at a time", Label("long", "wedged"), func() {
		if t.sibling == nil {
			Skip(recoveryBDF2Env + " is not set")
		}

		if t.siblingPCIID != t.pciID {
			Skip(fmt.Sprintf("%s is a %s and %s a %s; one plan only covers GPUs of the same model",
				t.fault.bdf, t.pciID, t.sibling.bdf, t.siblingPCIID))
		}

		plan := t.newRecoveryPlan("recovery-one-per-node")
		Expect(applyRecoveryPlan(plan)).To(Succeed())

		t.wedge(t.fault)
		t.wedge(*t.sibling)

		waitForEventState(plan.Name, t.fault.bdf, api.RecoveryEventStateWaitingApproval)
		waitForEventState(plan.Name, t.sibling.bdf, api.RecoveryEventStateWaitingApproval)

		approve(plan.Name, api.RecoveryApproval{
			Selector: &api.ApprovalSelector{RecoveryType: api.RecoveryTypeSBR, NodeName: t.node()},
		})

		traces := followEvents(plan.Name, t.node(), t.fault.bdf, t.sibling.bdf)
		expectRecovered(t, t.fault, plan.Name, traces[t.fault.bdf])
		expectRecovered(t, *t.sibling, plan.Name, traces[t.sibling.bdf])

		Expect(slices.Concat(traces[t.fault.bdf].states, traces[t.sibling.bdf].states)).
			To(ContainElement(api.RecoveryEventStateBlocked), "neither event waited for the other")
	})
})

// eventTrace is what followEvents saw of one GPU's recovery event.
type eventTrace struct {
	id     string
	states []api.RecoveryEventState
	// last is the event as last seen, before it was removed.
	last api.RecoveryEvent
	// gone means the event was removed: its device is no longer tainted.
	gone bool

	// jobNames lists the event's Jobs in the order they were seen, which is attempt order.
	jobNames []string
	// jobs holds each Job as first read; nil if it was deleted before it could be.
	jobs    map[string]*batch.Job
	jobLogs map[string]*jobLogs

	sawNodeTaint bool
}

func (tr *eventTrace) lastJob() string {
	if len(tr.jobNames) == 0 {
		return ""
	}

	return tr.jobNames[len(tr.jobNames)-1]
}

func (tr *eventTrace) logs(job string) string {
	l, ok := tr.jobLogs[job]
	Expect(ok).To(BeTrue(), "no logs were followed for Job %q", job)

	return l.wait()
}

// jobLogs follows a recovery Job's logs from the moment it is seen. The operator deletes the Job
// once the event is resolved, which can be within a second of the Job finishing, so they cannot be
// read afterwards.
type jobLogs struct {
	done chan struct{}
	out  string
}

func followJobLogs(job string) *jobLogs {
	l := &jobLogs{done: make(chan struct{})}

	go func() {
		defer GinkgoRecover()
		defer close(l.done)

		deadline := time.Now().Add(5 * time.Minute)

		// The pod may not exist yet, so the first attempts can fail. A finished container's logs
		// are returned in full, and -f returns once a running one exits.
		for time.Now().Before(deadline) {
			out, err := utils.Run(exec.Command("kubectl", "logs", "-f", "-n", namespace, "job/"+job,
				"--all-containers", "--prefix", "--pod-running-timeout=2m"))
			if err == nil {
				l.out = out
				return
			}

			time.Sleep(2 * time.Second)
		}
	}()

	return l
}

func (l *jobLogs) wait() string {
	select {
	case <-l.done:
		return l.out
	case <-time.After(3 * time.Minute):
		return "(timed out waiting for the Job's logs)"
	}
}

// followEvents polls a plan until the event of each listed GPU is either removed (its device
// recovered) or failed, recording every state it was seen in and reading each Job it ran.
//
// Polling can miss short states, succeeded above all: an event is removed as soon as its device
// is untainted, which can be within a second of its Job finishing. Only assert on states that last
// as long as a drain or a Job does.
func followEvents(planName, node string, bdfs ...string) map[string]*eventTrace {
	traces := map[string]*eventTrace{}
	for _, bdf := range bdfs {
		traces[bdf] = &eventTrace{jobs: map[string]*batch.Job{}, jobLogs: map[string]*jobLogs{}}
	}

	Eventually(func(g Gomega) {
		plan := getRecoveryPlan(planName, g)
		tainted := nodeHasTaint(node, recoveryNodeTaint)

		for _, bdf := range bdfs {
			tr := traces[bdf]
			if tr.gone || tr.last.State == api.RecoveryEventStateFailed {
				continue
			}

			tr.sawNodeTaint = tr.sawNodeTaint || tainted

			evt := eventFor(plan, bdf)
			if evt == nil {
				// Not created yet, or already removed.
				tr.gone = tr.id != ""
				continue
			}

			tr.id = evt.ID
			tr.last = *evt.DeepCopy()

			if len(tr.states) == 0 || tr.states[len(tr.states)-1] != evt.State {
				tr.states = append(tr.states, evt.State)
			}

			for _, job := range append(slices.Clone(evt.PastJobs), evt.JobName) {
				if job == "" || slices.Contains(tr.jobNames, job) {
					continue
				}

				tr.jobNames = append(tr.jobNames, job)
				tr.jobs[job] = getJob(job)
				tr.jobLogs[job] = followJobLogs(job)
			}
		}

		for bdf, tr := range traces {
			g.Expect(tr.gone || tr.last.State == api.RecoveryEventStateFailed).To(BeTrue(),
				"event for %s is still %v", bdf, tr.states)
		}
	}).WithTimeout(10 * time.Minute).Should(Succeed())

	for bdf, tr := range traces {
		_, _ = fmt.Fprintf(GinkgoWriter, "Recovery event %s for %s: states %v, Jobs %v\n", tr.id, bdf, tr.states, tr.jobNames)
	}

	return traces
}

// expectRecovered checks that an event's recovery ran its last Job to success and left the node
// and the GPU healthy.
func expectRecovered(t *recoveryTarget, f gpuFault, planName string, tr *eventTrace) {
	GinkgoHelper()

	Expect(tr.gone).To(BeTrue(), "event %s was not resolved, states seen: %v, %s", tr.id, tr.states, tr.last.StateMessage)
	Expect(tr.jobNames).NotTo(BeEmpty(), "event %s ran no Job", tr.id)

	// The event, and with it the Job, may be removed before a poll sees succeeded; the plan's
	// messages keep the verdict.
	messages := strings.Join(getRecoveryPlan(planName, Default).Status.Messages, "\n")
	Expect(messages).To(ContainSubstring("Event %s: recovery Job %s succeeded", tr.id, tr.lastJob()),
		func() string { return "Job logs:\n" + tr.logs(tr.lastJob()) })

	Expect(nodeHasTaint(t.node(), recoveryNodeTaint)).To(BeFalse())
	Expect(f.driver()).To(Equal("xe"))
	Expect(recoveryTaints(deviceTaintKeys(Default, f))).To(BeEmpty())
}

// waitForEventState waits for the GPU's event in the plan to reach the given state and returns it.
func waitForEventState(planName, bdf string, state api.RecoveryEventState) *api.RecoveryEvent {
	GinkgoHelper()

	var evt *api.RecoveryEvent

	Eventually(func(g Gomega) {
		evt = eventFor(getRecoveryPlan(planName, g), bdf)
		g.Expect(evt).NotTo(BeNil(), "no event for %s yet", bdf)
		g.Expect(evt.State).To(Equal(state), "event %s: %s", evt.ID, evt.StateMessage)
	}).Should(Succeed())

	return evt
}

func eventFor(plan *api.GPURecoveryPlan, bdf string) *api.RecoveryEvent {
	for i := range plan.Status.Events {
		if plan.Status.Events[i].GPUBDF == bdf {
			return &plan.Status.Events[i]
		}
	}

	return nil
}

// gpuInResourceSlices returns the node and the PCI ID of the GPU at bdf.
func gpuInResourceSlices(g Gomega, bdf string) (string, string) {
	for _, slice := range gpuResourceSlices(g) {
		for _, dev := range slice.Spec.Devices {
			if deviceAttrString(dev.Attributes, common.DeviceAttrBDF) == bdf {
				return *slice.Spec.NodeName, normalizePCIID(deviceAttrString(dev.Attributes, common.DeviceAttrDeviceID))
			}
		}
	}

	g.Expect(false).To(BeTrue(), "no GPU %s in the ResourceSlices", bdf)

	return "", ""
}

// recoveryTaints returns the taint keys the operator raises a recovery event for.
func recoveryTaints(keys []string) []string {
	return slices.DeleteFunc(slices.Clone(keys), func(k string) bool {
		return common.RecoveryForDeviceTaint(k) == common.DeviceRecoveryNone
	})
}

func waitForDeviceTaint(f gpuFault, key string, timeout time.Duration) {
	GinkgoHelper()

	Eventually(func(g Gomega) {
		g.Expect(deviceTaintKeys(g, f)).To(ContainElement(key))
	}).WithTimeout(timeout).Should(Succeed(), "the DRA driver did not taint %s with %s", f.bdf, key)

	// Whether the taint evicts the pods using the GPU (NoExecute) or only keeps new ones off it
	// (NoSchedule) decides what a drain still has to do.
	_, _ = fmt.Fprintf(GinkgoWriter, "%s taints: %s\n", f.bdf, mustRun("kubectl", "get", "resourceslices", "-o",
		fmt.Sprintf(`jsonpath={.items[*].spec.devices[?(@.attributes.pciAddress.string=="%s")].taints}`, f.bdf)))
}

// waitForGPUHealthy waits until the DRA driver lists the GPU with no taint that calls for recovery.
func waitForGPUHealthy(f gpuFault) {
	GinkgoHelper()

	Eventually(func(g Gomega) {
		g.Expect(recoveryTaints(deviceTaintKeys(g, f))).To(BeEmpty())
	}).WithTimeout(5*time.Minute).Should(Succeed(), "GPU %s on node %s did not become healthy", f.bdf, f.node)
}

// deviceTaintsEnabled reports whether the API server has the DRADeviceTaints feature gate on.
// Without it the taints the DRA driver publishes are dropped.
func deviceTaintsEnabled() bool {
	out, err := utils.Run(exec.Command("kubectl", "get", "--raw", "/metrics"))
	Expect(err).NotTo(HaveOccurred())

	return regexp.MustCompile(`(?m)^kubernetes_feature_enabled\{name="DRADeviceTaints".*\} 1$`).MatchString(out)
}

func pollUntil(timeout time.Duration, cond func() bool) bool {
	for deadline := time.Now().Add(timeout); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
		if cond() {
			return true
		}
	}

	return false
}

func applyRecoveryPlan(plan *api.GPURecoveryPlan) error {
	out, err := yaml.Marshal(plan)
	if err != nil {
		return err
	}

	return applyManifest(string(out))
}

func patchRecoveryPlan(planName string, patch map[string]any) error {
	data, err := json.Marshal(patch)
	if err != nil {
		return err
	}

	_, err = utils.Run(exec.Command("kubectl", "patch", "gpurecoveryplan", planName, "--type=merge", "-p", string(data)))

	return err
}

// approve replaces the plan's approvals.
func approve(planName string, approvals ...api.RecoveryApproval) {
	GinkgoHelper()

	By(fmt.Sprintf("approving: %+v", approvals))
	Expect(patchRecoveryPlan(planName, map[string]any{"spec": map[string]any{"approvals": approvals}})).To(Succeed())
}

func getRecoveryPlan(planName string, g Gomega) *api.GPURecoveryPlan {
	output, err := utils.Run(exec.Command("kubectl", "get", "gpurecoveryplan", planName, "-o", "json"))
	g.Expect(err).NotTo(HaveOccurred())

	plan := &api.GPURecoveryPlan{}
	g.Expect(json.Unmarshal([]byte(output), plan)).To(Succeed())

	return plan
}

// getJob returns the Job, or nil if it no longer exists.
func getJob(jobName string) *batch.Job {
	output, err := utils.Run(exec.Command("kubectl", "get", "job", jobName, "-n", namespace, "-o", "json"))
	if err != nil {
		return nil
	}

	job := &batch.Job{}
	if json.Unmarshal([]byte(output), job) != nil {
		return nil
	}

	return job
}

func recoveryJobNames(planName string) []string {
	output, err := utils.Run(exec.Command("kubectl", "get", "jobs", "-n", namespace,
		"-l", recoveryPlanLabel+"="+planName, "-o", "go-template={{ range .items }}{{ .metadata.name }}{{ \"\\n\" }}{{ end }}"))
	Expect(err).NotTo(HaveOccurred())

	return utils.GetNonEmptyLines(output)
}

// deleteAllRecoveryPlans deletes every GPURecoveryPlan. A plan with a Job in flight keeps its
// finalizer until the Job ends, so the finalizer is stripped rather than waited for; the caller
// removes the node taint the operator would have.
func deleteAllRecoveryPlans() {
	listCmd := []string{"kubectl", "get", "gpurecoveryplans",
		"-o", "go-template={{ range .items }}{{ .metadata.name }}{{ \"\\n\" }}{{ end }}"}

	_, _ = utils.Run(exec.Command("kubectl", "delete", "gpurecoveryplans", "--all", "--wait=false"))

	for _, planName := range utils.GetNonEmptyLines(mustRun(listCmd...)) {
		_ = patchRecoveryPlan(planName, map[string]any{"metadata": map[string]any{"finalizers": nil}})
	}

	Eventually(func() []string {
		return utils.GetNonEmptyLines(mustRun(listCmd...))
	}).WithTimeout(time.Minute).Should(BeEmpty())

	_, _ = utils.Run(exec.Command("kubectl", "delete", "jobs", "-n", namespace, "-l", recoveryPlanLabel, "--ignore-not-found"))
}

func dumpRecoveryDebugInfo(t *recoveryTarget) {
	for _, args := range [][]string{
		{"kubectl", "get", "gpurecoveryplans", "-o", "yaml"},
		{"kubectl", "get", "jobs,pods", "-n", namespace, "-l", recoveryPlanLabel, "-o", "wide"},
		{"kubectl", "logs", "-n", namespace, "-l", recoveryPlanLabel, "--all-containers", "--prefix", "--tail=-1"},
		{"kubectl", "logs", "-n", namespace, "-l", "control-plane=controller-manager", "--tail=200"},
		{"kubectl", "get", "resourceslices", "-o", "yaml"},
		{"kubectl", "logs", "-n", namespace, "-l", draPluginLabel, "--all-containers", "--prefix", "--tail=100"},
		{"kubectl", "logs", "-n", namespace, "-l", xpumdLabel, "--all-containers", "--prefix", "--tail=100"},
		{"kubectl", "get", "pods", "-A", "-o", "wide"},
		{"kubectl", "get", "events", "-A", "--sort-by=.lastTimestamp"},
		{"docker", "exec", t.node(), "sh", "-c", "dmesg | tail -50"},
	} {
		_, _ = fmt.Fprintf(GinkgoWriter, "--- %s\n%s\n", strings.Join(args, " "), mustRun(args...))
	}
}

// pinnedGPUPodManifest is a pod holding the GPU at bdf, picked by a CEL selector so that on a
// multi-GPU node it really holds the GPU the spec breaks.
func pinnedGPUPodManifest(bdf string, gracePeriod int) string {
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
          selectors:
          - cel:
              expression: device.attributes["%[4]s"].%[5]s == "%[6]s"
---
apiVersion: v1
kind: Pod
metadata:
  name: %[2]s
  namespace: %[3]s
spec:
  terminationGracePeriodSeconds: %[7]d
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
`, recoveryGPUClaim, recoveryGPUPod, recoveryPodNs, common.GPUDeviceClass, common.DeviceAttrBDF, bdf, gracePeriod)
}

// holdPodManifest is a pod, using no GPU, that holds up a whole-node drain: through its grace
// period, sleep being PID 1 with no SIGTERM handler, or through a PodDisruptionBudget refusing its
// eviction. It uses no GPU so that a device taint that evicts GPU pods cannot remove it first.
func holdPodManifest(gracePeriod int, withPDB bool) string {
	manifest := fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: %[1]s
  namespace: %[2]s
  labels:
    app: %[1]s
spec:
  terminationGracePeriodSeconds: %[3]d
  containers:
  - name: hold
    image: ubuntu:24.04
    command: ["sleep", "infinity"]
`, recoveryHoldPod, recoveryPodNs, gracePeriod)

	if withPDB {
		manifest += fmt.Sprintf(`---
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: %[1]s
  namespace: %[2]s
spec:
  minAvailable: 1
  selector:
    matchLabels:
      app: %[3]s
`, recoveryHoldPDB, recoveryPodNs, recoveryHoldPod)
	}

	return manifest
}
