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
