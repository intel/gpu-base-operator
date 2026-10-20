# fake-xpu-smi — Mock xpu-smi for GPU Recovery and Firmware Update Testing

`fake-xpu-smi` mimics the subset of the real `xpu-smi` CLI used by GPU recovery
Jobs and `GPUFirmwareUpdate` Jobs. It is the `xpu-smi` in the
`gpu-fwupdater-mock` image (`make fwupdater-mock-build`).

Instead of invoking xpumd control APIs, it now simulates recovery by performing
a PCI **unbind + bind** for the target BDF under sysfs.

## Build

```sh
cd test/fake-xpu-smi
go build -o fake-xpu-smi .
```

or, as the mock updater image:

```sh
make fwupdater-mock-build
```

## Usage

```
fake-xpu-smi config   -d <deviceId|BDF> --reset                      SBR reset
fake-xpu-smi config   -d <pciBdfAddress> --coldreset                  PCIe slot power cycle
fake-xpu-smi amc      --gpuReset -d <deviceId|BDF> [-y]              AMC GPU reset
fake-xpu-smi updatefw -d <deviceId|BDF> -t <type> -f <imageFilePath>  Firmware update (-d optional for AMC)
fake-xpu-smi list                                                     List devices
```

## Firmware update behavior

`updatefw` with any type other than `FDO` is a `GPUFirmwareUpdate` flash and
touches no device:

1. Check that `-t` is a known type, `-d` is given (except for `AMC`) and the `-f`
   file is readable — the file is the one the Job's `fw-copy` initContainer staged.
2. Sleep `FAKE_XPU_SMI_UPDATE_DELAY` (a Go duration, default `3s`).
3. Print `fake-xpu-smi: updated <type> on <BDF> with <file> (simulated)` and exit 0.

If the firmware file's contents include `FAKE_XPU_SMI_FAIL`, it exits 1 instead.
That is how a test makes an update Job fail: the CR controls which files are
flashed, but cannot set anything on the updater container.

## Recovery behavior

Every recovery path rebinds the target device:

1. Write BDF to `/sys/bus/pci/drivers/<driver>/unbind`
2. Wait (7s for reset flows; `updatefw -t FDO` first sleeps 30s, then rebinds)
3. Write BDF to `/sys/bus/pci/drivers/<driver>/bind`

The reset paths (`config --reset`, `config --coldreset`, `amc --gpuReset`) do only
that. `amc --gpureset` and `config --force-reset-gpus`, the spellings the recovery
Job uses, are accepted too.

A reflash fails the same way: if the FDO file contains `FAKE_XPU_SMI_FAIL`,
`updatefw -t FDO` exits 1 after its sleep and **before** the unbind. The device is
left bound and still in survivability mode, as a real failed flash would leave it.

### Reflash: clearing survivability mode

`updatefw -t FDO` additionally clears xe's survivability mode, between the unbind and the
bind:

1. `mkdir <sysfs-root>/kernel/config/xe/<BDF>`
2. Write `0` to `survivability_mode` in that directory

This mirrors how a device is put *into* survivability mode with the xe KMD in the
first place — unbind, write `1` to the same file, bind back — which is how a test
GPU is made to look like it needs a reflash.

Three things about it are easy to get wrong:

- **The `mkdir` is required, not defensive.** Configfs groups are created by
  `mkdir`; that is the interface. They do not survive a reboot, so on a freshly
  booted node `/sys/kernel/config/xe` exists with nothing under it and a write
  without the `mkdir` fails with `ENOENT`. On a node where someone has already put
  a device into survivability mode by hand, the directory is there — so the missing
  `mkdir` works right up until the node reboots.
- **It happens while the device is unbound.** xe reads these attributes when it
  probes the device, so writing to them with the driver bound changes nothing until
  something rebinds. Writing `0` is not what revives the card (the rebind is, and
  the value appears to make no observable difference either way) — it is written so
  the mock leaves the node as a real recovery would, rather than leaving a `1`
  behind for the next reboot to act on.
- **A failure here does not leave the GPU unbound.** The device is bound back
  first, and the failure is reported afterwards through a non-zero exit, which is
  what fails the recovery Job. Losing the card from the node would be worse than
  the problem being reported.

## Sysfs root selection

The sysfs root path is resolved in this order:

| Priority | Source | Example |
|----------|--------|---------|
| 1 | `XPU_SMI_SYSFS_ROOT` env variable | `/host-sys` |
| 2 | `SYSFS_ROOT` env variable | `/tmp/gpu-fake-root/sysfs` |
| 3 | `--sysfs-root` flag on subcommands | `--sysfs-root /sys` |
| 4 | Default | `/sys` |

## Running inside a Kubernetes Job

The container must be able to write PCI driver bind/unbind files on the node:

```yaml
securityContext:
  runAsUser: 0
  privileged: true
volumeMounts:
- name: host-sys
  mountPath: /sys
  readOnly: false
volumes:
- name: host-sys
  hostPath:
    path: /sys
    type: Directory
```

Both recovery Job templates (`config/deployments/xpum/xpum-reset-job.yaml`,
`xpum-fwupdate-job.yaml`) already mount `/sys` this way, writable and privileged.

Note that `/sys/kernel/config` is a separate configfs mount *under* `/sys`, so the
reflash path only works if the container's `/sys` came in recursively — the usual
case, since CRI implementations bind host paths with `rbind`. If
`/sys/kernel/config` looks empty inside the Job while the host has entries, that is
the thing to check first; the symptom is the `mkdir` failing rather than anything
about the GPU.

## Full mock flow

```
1. xpumd reports recovery need to DRA
2. DRA taints device in ResourceSlice
3. Operator detects taint → creates RecoveryEvent
4. Admin approval triggers recovery Job
5. Job runs fake-xpu-smi for target BDF
6. fake-xpu-smi unbinds + binds that BDF
   (reflash: also clears survivability_mode while unbound)
7. xpumd observes recovery and DRA clears taint
8. Operator marks RecoveryEvent as succeeded
```
