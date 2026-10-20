# E2E tests for the operator

E2E tests run on a Kind cluster that is set up by the make target. In the case of an error, the cluster stays intact, and can be removed by calling: `kind delete clusters intel-gpu-base-operator-test-e2e`

The node where the e2e tests are run has to have one or more Intel GPUs with suitable KMD installed. While the operator prioritizes the newer `xe` KMD, it also supports the `i915`.

## Dependencies

Runtime dependencies:

* make - to trigger e2e run
* go - to compile and run the e2e tests
* kubectl - to deploy the operator and overall access to the cluster
* helm - to deploy the operator
* kind - to setup a temporary cluster
* jq - to parse json data in tests

Helm and Kind can be installed using `make`:
```
mkdir -p $(pwd)/bin
export PATH=$PATH:$(pwd)/bin
make helm
make kind
```

### Collateral images

Test run automatically pre-pulls the images used in the run to prevent timeouts during the test execution.
```
  STEP: loading the manager(Operator) image on Kind @ 12/03/25 10:33:33.238
  STEP: loading collateral images on Kind @ 12/03/25 10:33:33.349
  STEP: loading image intel/intel-gpu-plugin:0.34.0 to kind cluster @ 12/03/25 10:33:33.35
  STEP: loading image intel/intel-gpu-levelzero:0.34.0 to kind cluster @ 12/03/25 10:33:35.087
  STEP: loading image intel/xpumanager:v1.2.27 to kind cluster @ 12/03/25 10:33:36.941
  STEP: loading image ghcr.io/intel/intel-resource-drivers-for-kubernetes/intel-gpu-resource-driver:v0.9.0 to kind cluster @ 12/03/25 10:33:38.787
```

## How to run

Build the latest operator and the firmware update test images:

```
make operator-build docker-build-e2e
```
> Test run automatically imports the operator image to Kind from docker.

### Firmware update specs

The `fwupdate` specs run `GPUFirmwareUpdate`s against the node with the mock updater image
(`make fwupdater-mock-build`, see `test/fake-xpu-smi/README.md`), so no firmware is flashed. The
firmware files are dummies from `test/e2e/testdata/fwfiles`, built into a content image by
`make fwfiles-e2e-build`. The suite pushes that image to a registry it runs inside the cluster
(NodePort 30500 on the node), because the operator verifies the content image against a registry
before it starts.

The GPU to update is taken from the DRA ResourceSlice. `scripts/update.sh` only updates GPUs that
have an MEI interface, so on a node with none (integrated GPUs, for example) the specs that need the
mock to run are skipped. By default the first GPU in the ResourceSlice is used; on a node with
several GPUs, pick one by PCI device ID with `E2E_FWUPDATE_PCI_ID` (e.g. `E2E_FWUPDATE_PCI_ID=0xe20b`
for a B580). The suite fails if no GPU on the node has that ID. Run only these specs with
`--ginkgo.label-filter fwupdate`.

`E2E_FWUPDATE_PCI_ID` also enables a device plugin variant ("Firmware update with device plugin"),
which deploys the device plugin instead of DRA and drains a pod holding the `gpu.intel.com/xe` or
`gpu.intel.com/i915` extended resource. With the variable unset it is skipped, since without a
ResourceSlice there is nothing to pick the GPU from.

### GPU recovery specs

The `recovery` specs (`test/e2e/recovery_test.go`) **really break a GPU**, then check that a
`GPURecoveryPlan` brings it back:

- **Wedged** (`--ginkgo.label-filter wedged`): `gpu-hang` sets `wedged_mode=2` and hangs the GPU.
  xpumd reports it, the DRA driver taints the device `health-xpumd-gpu.wedged`, and the operator
  drains the node and runs an SBR (or slot) reset.
- **Survivability** (`--ginkgo.label-filter survivability`): `gpu-hang` puts the GPU into boot
  survivability mode through xe's configfs. The DRA driver taints the device `health-Survivability`,
  and the operator runs a reflash. If the driver doesn't notice the mode by itself within 90s, the
  spec restarts its pod on the node, as a boot would.

The recovery Jobs run the mock xpu-smi, whose reset and reflash both rebind the device; that rebind
is what clears the fault (`test/fake-xpu-smi/README.md`). The faults come from `gpu-hang`
(`make gpu-hang-build`, see `test/gpu-hang/README.md`).

The target GPU must be named, never picked: set `E2E_RECOVERY_BDF` to its PCI address
(e.g. `E2E_RECOVERY_BDF=0000:03:00.0`). Without it the specs skip. The suite refuses a GPU that
isn't bound to `xe`, or that is the boot VGA device, unless `E2E_RECOVERY_ALLOW_BOOT_VGA=true` is
set. Set that only if the node's discrete GPU is its boot VGA device and nothing needs its display. Use a dedicated discrete GPU: survivability mode
needs BMG or newer, and if a GPU doesn't come back from a rebind, the node needs a reboot.
`E2E_RECOVERY_BDF2` optionally names a second GPU on the same node. It enables the spec that runs
two recoveries on one node, and is checked to stay untouched while the first GPU recovers.

The specs also need:

- The `DRADeviceTaints` feature gate, which is off by default before Kubernetes 1.37. Without it the
  API server drops the device taints. `make setup-test-e2e` creates the cluster with it
  (`test/e2e/kind-config.yaml`); on a cluster without it the specs skip.
- `wedged_mode` in xe's debugfs, and xe's configfs for survivability mode. The suite mounts debugfs
  and configfs on the Kind node. A spec whose knob is missing skips.
- A writable `/sys` on the node, for the PCI driver's `bind` and `unbind` files. Kind mounts the
  node's `/sys` read-only, and pods inherit that, so the suite remounts it read-write for the
  duration of the recovery specs and puts it back afterwards.

Every spec ends by bringing the GPU back if it is still broken, and waits for the DRA driver to list
it healthy before the next spec starts. On a failure it first dumps the plans, the operator, DRA
driver and xpumd logs, the ResourceSlices and `dmesg`.

### GPU binding specs

The binding specs (`test/e2e/binding_test.go`) move a GPU between its DRM driver (`xe` or `i915`)
and vfio, which takes it away from the host while it is there:

- **Device plugin** (`--ginkgo.label-filter 'deviceplugin && binding'`): the suite binds the GPU to `vfio-pci` itself,
  checks the device plugin stops advertising it, binds it back and checks it is advertised again and
  can be allocated.
- **DRA** (`--ginkgo.label-filter dra && binding`): with `manageBinding: false` the device classes select
  by driver and a `gpu-vfio.intel.com` claim stays unallocated; with `manageBinding: true` a claim
  for that class makes the DRA driver bind the GPU to `xe-vfio-pci` or `vfio-pci`. The `long` specs
  run an xe, a vfio and another xe workload on the same GPU, and start a KubeVirt VM with it.

The target GPU must be named, never picked: set `E2E_VFIO_BDF` to its PCI address. Without it only
the DRA device class spec runs. The suite refuses the boot VGA device unless
`E2E_RECOVERY_ALLOW_BOOT_VGA=true` is set. A spec skips if the GPU has no IOMMU group (boot with
`intel_iommu=on`) or no vfio driver is loaded on the host (`modprobe vfio-pci`; the driver never
loads it). As with the recovery specs, the suite remounts the Kind node's `/sys` read-write, before
the DRA driver starts, and every spec ends with the GPU back on its original driver.

The Kind node needs the host's `/dev/vfio` and `/dev/dri` mounted in, because a node container
only gets a copy of `/dev` from when it started, which never has the vfio device the bind creates.
`test/e2e/kind-config.yaml` does that; on a cluster created without it, the specs that bind skip.

The KubeVirt spec (`--ginkgo.label-filter kubevirt`) needs `/dev/kvm` on the node and network access
to GitHub and quay.io. It installs KubeVirt `v1.8.3` with the `GPUsWithDRA` feature gate
(`E2E_KUBEVIRT_VERSION` overrides the version), starts a CirrOS VMI with the GPU, checks QEMU got it
through vfio, and uninstalls KubeVirt.
