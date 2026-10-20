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

// Fault injection for the recovery specs: puts a real GPU into a state that needs recovery, and
// brings it back when a spec could not. See test/gpu-hang/README.md.

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"time"

	core "k8s.io/api/core/v1"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/intel/gpu-base-operator/internal/controller/common"
	"github.com/intel/gpu-base-operator/test/utils"
)

const (
	// Built by 'make docker-build-e2e'.
	gpuHangImage = "ghcr.io/intel/gpu-hang-e2e:devel"

	gpuHangNamespace = "default"

	// gpu-hang exits with IGT's skip code when the kernel or the GPU lacks what a mode needs.
	gpuHangExitSkip = 77

	// recoveryAllowBootVGAEnv=true lets the target GPUs be the boot VGA device, for a node whose
	// only discrete GPU is it. Nothing that matters may run on its display.
	recoveryAllowBootVGAEnv = "E2E_RECOVERY_ALLOW_BOOT_VGA"
)

// gpuFault breaks and restores one GPU on a Kind node.
type gpuFault struct {
	node string
	bdf  string
	// allowBootVGA lets the GPU be the boot VGA device, which is refused by default.
	allowBootVGA bool
}

// nodeSh runs a shell script on the Kind node, which sees the host's sysfs.
func (f gpuFault) nodeSh(script string) (string, error) {
	return utils.Run(exec.Command("docker", "exec", f.node, "sh", "-c", script))
}

// driver returns the name of the driver the GPU is bound to, or "" when it is unbound.
func (f gpuFault) driver() string {
	out, _ := f.nodeSh(fmt.Sprintf(`basename "$(readlink /sys/bus/pci/devices/%s/driver)" 2>/dev/null || true`, f.bdf))
	out = strings.TrimSpace(out)

	if out == "." || out == "driver" {
		return ""
	}

	return out
}

// inSurvivability reports whether xe probed the GPU into survivability mode.
func (f gpuFault) inSurvivability() bool {
	_, err := f.nodeSh(fmt.Sprintf("test -e /sys/bus/pci/devices/%s/survivability_mode", f.bdf))
	return err == nil
}

// configfsSurvivabilityMode returns the device's xe configfs survivability_mode, or "" when the
// device has no configfs group.
func (f gpuFault) configfsSurvivabilityMode() string {
	out, _ := f.nodeSh(fmt.Sprintf("cat /sys/kernel/config/xe/%s/survivability_mode 2>/dev/null || true", f.bdf))
	return strings.TrimSpace(out)
}

// checkTarget refuses a GPU the suite must not break: one not on xe, or, unless allowBootVGA is
// set, the boot VGA device, which on a mixed node may be the iGPU driving the console.
func (f gpuFault) checkTarget() {
	Expect(f.driver()).To(Equal("xe"), "%s on node %s must be bound to xe", f.bdf, f.node)

	if f.allowBootVGA {
		return
	}

	out, err := f.nodeSh(fmt.Sprintf("cat /sys/bus/pci/devices/%s/boot_vga 2>/dev/null || echo 0", f.bdf))
	Expect(err).NotTo(HaveOccurred())
	Expect(strings.TrimSpace(out)).NotTo(Equal("1"),
		"%s is the boot VGA device; pick a dedicated test GPU, or set %s=true if it is one", f.bdf, recoveryAllowBootVGAEnv)
}

// prepareNode makes the Kind node's sysfs writable and mounts debugfs and configfs on it.
//
// Kind's entrypoint remounts the node container's /sys read-only, and a pod's /sys hostPath is a
// bind mount of it, which keeps that flag. Recovery Jobs and gpu-hang write the PCI driver's
// bind and unbind files, so they need it writable. On a cluster whose kubelet runs on the host,
// such as k3s, /sys is writable already.
//
// The node container also gets a sysfs without debugfs and configfs mounted. Pods reach both
// through their /sys hostPath, which is bound recursively when the pod starts.
//
// So this has to happen before the gpu-hang and recovery Job pods are created. Otherwise their
// /sys is read-only, gpu-hang finds no wedged_mode, and the reflash Job's configfs write fails.
// Pods already running keep the /sys they started with, which is fine: they only read it.
func (f gpuFault) prepareNode() {
	_, err := f.nodeSh(`mount -o remount,rw /sys
mountpoint -q /sys/kernel/debug || mount -t debugfs none /sys/kernel/debug
mountpoint -q /sys/kernel/config || mount -t configfs none /sys/kernel/config`)
	Expect(err).NotTo(HaveOccurred(), "preparing sysfs on node %s", f.node)
}

// unprepareNode puts the Kind node's /sys back to read-only, as Kind has it. Kind keeps it
// read-only so that node components such as kube-proxy do not write host kernel parameters.
func (f gpuFault) unprepareNode() {
	_, _ = f.nodeSh("mount -o remount,ro /sys")
}

// wedge wedges the GPU (gpu-hang --mode=wedged): wedged_mode=2 and a batch that never ends.
//
// The wedge counts as done once gpu-hang exits 0, or once the DRA driver taints the device as
// wedged, whatever has become of gpu-hang by then. With an approval already in place the recovery
// starts on that taint and may get to gpu-hang first: the drain evicts it, or the reset unbinds the
// GPU it still has open. That is the recovery working, not the wedge failing.
func (f gpuFault) wedge() {
	By(fmt.Sprintf("wedging GPU %s", f.bdf))
	f.runGPUHang("wedged", func() bool {
		keys, _ := lenientDeviceTaintKeys(f)
		return slices.Contains(keys, common.DeviceTaintKeyReset)
	}, "--set-wedged-mode")
}

// enterSurvivability puts the GPU into boot survivability mode (gpu-hang --mode=survivability):
// unbind, configfs survivability_mode=1, bind.
func (f gpuFault) enterSurvivability() {
	By(fmt.Sprintf("putting GPU %s into survivability mode", f.bdf))
	f.runGPUHang("survivability", nil)
	Expect(f.inSurvivability()).To(BeTrue())
}

func (f gpuFault) gpuHangPodName(mode string) string {
	return "gpu-hang-" + mode + "-" + strings.NewReplacer(":", "-", ".", "-").Replace(f.bdf)
}

// runGPUHang runs gpu-hang in a privileged pod on the node and waits for it to finish. It skips the
// spec when gpu-hang does, because a kernel or GPU without the knob is not a test failure.
//
// faulted, if set, reports that the fault is in place whatever gpu-hang is doing. Once it does,
// the wait ends and gpu-hang's pod is no longer looked at: it may be running, gone or failed.
func (f gpuFault) runGPUHang(mode string, faulted func() bool, extraArgs ...string) {
	pod := f.gpuHangPodName(mode)

	_, _ = utils.Run(exec.Command("kubectl", "delete", "pod", pod, "-n", gpuHangNamespace, "--ignore-not-found"))
	if f.allowBootVGA {
		extraArgs = append(extraArgs, "--force")
	}

	Expect(applyManifest(gpuHangPodManifest(pod, f.node, f.bdf, mode, extraArgs))).To(Succeed())

	var (
		terminated *core.ContainerStateTerminated
		faultedBy  bool
	)

	Eventually(func(g Gomega) {
		output, err := utils.Run(exec.Command("kubectl", "get", "pod", pod, "-n", gpuHangNamespace, "-o", "json"))

		p := core.Pod{}
		if err == nil && json.Unmarshal([]byte(output), &p) == nil && len(p.Status.ContainerStatuses) > 0 {
			terminated = p.Status.ContainerStatuses[0].State.Terminated
		}

		// gpu-hang's own verdict is the better one, so it wins when both are in.
		if terminated != nil && (terminated.ExitCode == 0 || terminated.ExitCode == gpuHangExitSkip) {
			return
		}

		if faulted != nil && faulted() {
			faultedBy = true
			return
		}

		if err != nil && strings.Contains(output, "NotFound") {
			StopTrying(fmt.Sprintf("gpu-hang pod %s/%s was deleted before the fault was in place", gpuHangNamespace, pod)).Now()
		}

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(terminated).NotTo(BeNil(), "gpu-hang is still running")
	}).WithTimeout(4 * time.Minute).Should(Succeed())

	logs, _ := utils.Run(exec.Command("kubectl", "logs", pod, "-n", gpuHangNamespace))
	_, _ = fmt.Fprintf(GinkgoWriter, "gpu-hang --mode=%s on %s:\n%s\n", mode, f.bdf, logs)

	if faultedBy {
		state := "still running"
		if terminated != nil {
			state = fmt.Sprintf("exited %d", terminated.ExitCode)
		}

		_, _ = fmt.Fprintf(GinkgoWriter, "%s is faulted (--mode=%s); gpu-hang %s, not waiting for it\n", f.bdf, mode, state)
		_, _ = utils.Run(exec.Command("kubectl", "delete", "pod", pod, "-n", gpuHangNamespace, "--ignore-not-found", "--wait=false"))

		return
	}

	if terminated.ExitCode == gpuHangExitSkip {
		Skip(fmt.Sprintf("gpu-hang cannot use --mode=%s on %s: %s", mode, f.bdf, lastLine(logs)))
	}

	Expect(terminated.ExitCode).To(BeZero(), "gpu-hang --mode=%s failed on %s:\n%s", mode, f.bdf, logs)

	_, _ = utils.Run(exec.Command("kubectl", "delete", "pod", pod, "-n", gpuHangNamespace, "--ignore-not-found"))
}

// restore brings the GPU back to a healthy state as a last resort. It never fails the spec: every
// step runs whatever the previous one did, and the caller waits for the ResourceSlice to show the
// GPU healthy instead.
//
// The device is rebound when anything says it is not healthy: rebind is set, it is unbound, in
// survivability mode, or has an xe configfs group. Only a rebind un-wedges a GPU, and only one with
// survivability_mode cleared first brings it out of survivability mode. wedged_mode is reset before
// that, so that a hang in a later spec cannot wedge the GPU if the rebind is skipped.
func (f gpuFault) restore(rebind bool) {
	_, _ = utils.Run(exec.Command("kubectl", "delete", "pod", "-n", gpuHangNamespace,
		f.gpuHangPodName("wedged"), f.gpuHangPodName("survivability"), "--ignore-not-found", "--grace-period=0", "--force"))

	script := fmt.Sprintf(`BDF=%s
DEV=/sys/bus/pci/devices/$BDF
CFG=/sys/kernel/config/xe/$BDF
mount -o remount,rw /sys
mountpoint -q /sys/kernel/debug || mount -t debugfs none /sys/kernel/debug
mountpoint -q /sys/kernel/config || mount -t configfs none /sys/kernel/config
# Writes to a wedged device's debugfs fail with EIO; the rebind below resets them anyway.
for n in /sys/kernel/debug/dri/*/name; do
  grep -q "dev=$BDF" "$n" 2>/dev/null || continue
  DBG=$(dirname "$n")
  echo 1 2>/dev/null > "$DBG/wedged_mode"
  if [ -e "$DBG/fail_gt_reset/probability" ]; then
    echo 0 2>/dev/null > "$DBG/fail_gt_reset/probability"
    echo 1 2>/dev/null > "$DBG/fail_gt_reset/times"
  fi
done
rebind=%t
[ -d "$CFG" ] && rebind=true
[ -e "$DEV/survivability_mode" ] && rebind=true
[ -e "$DEV/driver" ] || rebind=true
if [ "$rebind" = true ]; then
  echo "rebinding $BDF"
  [ -e "$DEV/driver" ] && echo "$BDF" > /sys/bus/pci/drivers/xe/unbind
  if [ -d "$CFG" ]; then
    echo 0 > "$CFG/survivability_mode"
    rmdir "$CFG"
  fi
  echo "$BDF" > /sys/bus/pci/drivers/xe/bind
fi
echo "driver: $(basename "$(readlink "$DEV/driver")")"
true`, f.bdf, rebind)

	out, err := f.nodeSh(script)
	_, _ = fmt.Fprintf(GinkgoWriter, "restoring GPU %s (err: %v):\n%s\n", f.bdf, err, out)
}

func lastLine(s string) string {
	lines := utils.GetNonEmptyLines(s)
	if len(lines) == 0 {
		return ""
	}

	return lines[len(lines)-1]
}

// gpuHangPodManifest is a privileged pod running gpu-hang on one node. It reaches the GPU through
// the node's /sys and /dev/dri, and tolerates every taint, since the node may be held by a drain.
func gpuHangPodManifest(pod, node, bdf, mode string, extraArgs []string) string {
	args := append([]string{"--bdf", bdf, "--mode", mode}, extraArgs...)

	quoted := make([]string, 0, len(args))
	for _, a := range args {
		quoted = append(quoted, fmt.Sprintf("%q", a))
	}

	return fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: %[1]s
  namespace: %[2]s
spec:
  nodeName: %[3]s
  restartPolicy: Never
  tolerations:
  - operator: Exists
  containers:
  - name: gpu-hang
    image: %[4]s
    imagePullPolicy: IfNotPresent
    args: [%[5]s]
    securityContext:
      privileged: true
      runAsUser: 0
    volumeMounts:
    - name: sys
      mountPath: /sys
    - name: dri
      mountPath: /dev/dri
  volumes:
  - name: sys
    hostPath:
      path: /sys
      type: Directory
  - name: dri
    hostPath:
      path: /dev/dri
      type: Directory
`, pod, gpuHangNamespace, node, gpuHangImage, strings.Join(quoted, ", "))
}
