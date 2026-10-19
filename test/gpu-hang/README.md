# gpu-hang — break an xe GPU for the GPU recovery e2e tests

`gpu-hang` puts an xe GPU into a state that needs recovery, so the e2e tests can check
the whole recovery path, from the DRA driver's slice taint to the GPURecoveryPlan. It
has two modes:

| `--mode` | State | Slice taint | Recovery |
|---|---|---|---|
| `wedged` (default) | wedged | `health-xpumd-gpu.wedged` (via xpumd) | `sbr`, a rebind |
| `survivability` | boot survivability mode | `health-Survivability` | `reflash` |

## Utilizing IGT

`gpu-hang` tool calls [IGT](https://gitlab.freedesktop.org/drm/igt-gpu-tools)'s functionality
to trigger a `wedged` mode into an Intel GPU. While it only uses minor part of the
functionality provided by IGT, the `gpu-hang` has all the dependencies of the tool.

Some of the libraries the application depends on are GPLv2+ lisenced.

## Wedged mode

It follows IGT's `xe_wedged@wedged-at-any-timeout` (`tests/intel/xe_wedged.c`). With the
device's debugfs `wedged_mode` set to `2` ("upon-any-hang-no-reset"), it submits a
non-preemptible spin batch that never ends, and keeps the fd open until the job times
out. xe then declares the device wedged instead of resetting it. Unlike the IGT test,
`gpu-hang` does **not** rebind the device afterwards. The GPU stays wedged until
something rebinds it: the recovery Job (`fake-xpu-smi config --reset`), or you.

It needs only debugfs. The fault-injection alternative (`basic-wedged`) also needs
`CONFIG_FAULT_INJECTION_DEBUG_FS`, which isn't enabled by default.

## Survivability mode

It follows IGT's `xe_configfs@survivability-mode` (`tests/intel/xe_configfs.c`):

1. Unbind the device from xe.
2. `mkdir /sys/kernel/config/xe/<BDF>` (if it isn't there), and write `1` to
   `survivability_mode` in it.
3. Bind the device back.

xe reads configfs when it probes the device, so it comes back in survivability mode: no
DRM node, and a `survivability_mode` attribute on the PCI device. That's what the tool
checks for. It's the state a card with broken firmware boots into.

It needs a discrete GPU with graphics IP 20.1 or later (BMG), configfs (the tool mounts
it if it isn't mounted), and a kernel with xe's configfs support. It doesn't work on a
VF.

The configfs value is **left at `1`**, so any later rebind goes back into survivability
mode, as a broken card would. The reflash clears it (`fake-xpu-smi updatefw -t FDO`
writes `0` while the device is unbound), and so do the manual steps below. If a step
fails after the unbind, the tool binds the device back before it exits, so the GPU never
goes missing from the node.

> **Warning:** this really wedges the GPU. Only use it on a dedicated discrete test GPU.
> It refuses the boot VGA device unless you pass `--force`. If a rebind doesn't bring
> the GPU back, you'll need to reboot.

## Build

The tool is built as one of IGT's tools, against a pinned IGT commit (`IGT_REF` in
`build/gpu-hang/Dockerfile`):

```sh
make gpu-hang-build                       # ghcr.io/intel/gpu-hang-e2e:$(TAG)
make gpu-hang-build GPU_HANG_IMG=gpu-hang-e2e:dev
```

## Usage

```
gpu-hang -b <BDF> [-m wedged|survivability] [-f] [-t <seconds>] [-w]

  -b, --bdf=BDF          PCI address of the xe GPU (required)
  -m, --mode=MODE        wedged (default) or survivability
  -f, --force            Allow using the boot VGA device
  wedged mode only:
  -t, --timeout=SECONDS  How long to wait for the hang to be detected (default 120)
  -w, --set-wedged-mode  Write wedged_mode=2 before the hang. Restored if the
                         device does not end up wedged; a rebind resets it anyway
```

IGT's common options (`--debug`, `--help`, …) work as well.

Both modes first check that the BDF is bound to `xe` and isn't the boot VGA device. If
the device is already in survivability mode, `--mode=survivability` exits 0 and
`--mode=wedged` fails: a device in survivability mode has no DRM node to submit work on.

In wedged mode, it then does the following, in order:

1. It opens the device's `/dev/dri/cardN`. If the device is already wedged, it reports
   that and exits 0.
2. It reads `wedged_mode`, and writes `2` first if `-w` is given. If the value isn't
   `2`, it warns: xe would reset the GT rather than wedge the device. If `wedged_mode`
   doesn't exist, it skips with IGT exit code 77.
3. It binds a spin batch, submits it on the device's first engine, and waits for it to
   start.
4. It waits up to `--timeout` for the job to be terminated.
5. It checks for up to 10s that ioctls fail with `ECANCELED`, which is the definition
   of wedged that `xe_wedged` uses.

| Exit | Meaning |
|---|---|
| 0 | The device is in the requested state, either from this run or already before it. |
| 77 | Skipped. Wedged: there is no `wedged_mode` in debugfs. Survivability: the GPU doesn't support it, it's a VF, or there's no xe configfs. |
| 79 | Invalid options, e.g. an unknown `--mode`. |
| 98 | Failed: wrong driver, or boot VGA. Wedged: `wedged_mode=2` was refused (on a VF, or on an SR-IOV PF without a debug kernel), no hang was detected in time, or the hang was handled without wedging the device. Survivability: the unbind, the configfs write or the bind failed, or the device came back without entering survivability mode. |

The log reports how long the hang took to detect (`Spinner terminated after …s`). That's
the first part of the recovery e2e's detection latency.

## Trying it out on a host

You don't need Kind. Run it as a privileged container directly on the GPU host
(`<bdf>` is e.g. `0000:03:00.0`):

```sh
BDF=<bdf>
DBG=$(grep -l "dev=$BDF" /sys/kernel/debug/dri/*/name | xargs dirname)

cat $DBG/wedged_mode                      # 1 by default

docker run --rm --privileged -v /sys:/sys -v /dev/dri:/dev/dri \
  gpu-hang-e2e:dev -b $BDF -w

sudo dmesg | grep -i wedged               # "... has declared device ... as wedged"
```

While it's wedged, xpumd should report the device as wedged, and the DRA
driver should publish `health-xpumd-gpu.wedged` on it.

To recover, rebind the device the same way the recovery Job does:

```sh
echo $BDF | sudo tee /sys/bus/pci/drivers/xe/unbind
echo $BDF | sudo tee /sys/bus/pci/drivers/xe/bind
cat $DBG/wedged_mode                      # expect 1 again after the rebind
```

The debugfs dir can move to a new `dri/<N>` after the rebind, so re-run the `grep` if
`$DBG` no longer exists.

Survivability mode:

```sh
docker run --rm --privileged -v /sys:/sys -v /dev/dri:/dev/dri \
  gpu-hang-e2e:dev -b $BDF -m survivability

ls /sys/bus/pci/devices/$BDF/survivability_mode   # present
ls /sys/bus/pci/devices/$BDF/drm                  # no card/render node
```

The DRA driver should then publish `health-Survivability` on the device. That's Phase
0.4 of the plan, which also asks whether it does so on the rebind, or only after its pod
restarts.

To recover, do what the mock reflash does: clear the value while the device is unbound.

```sh
echo $BDF | sudo tee /sys/bus/pci/drivers/xe/unbind
echo 0 | sudo tee /sys/kernel/config/xe/$BDF/survivability_mode
sudo rmdir /sys/kernel/config/xe/$BDF
echo $BDF | sudo tee /sys/bus/pci/drivers/xe/bind
```

## In the e2e suite

The image runs as a privileged pod pinned to the Kind node, with the host's `/dev/dri`
and `/sys` available. The pod exits once the device is in the requested state, so the
test waits for the pod to succeed rather than killing it. `/sys/kernel/config` is a
separate mount under `/sys`. CRI implementations bind host paths recursively, so it
comes along, but if the configfs `mkdir` fails inside the pod, check that first (see
`test/fake-xpu-smi/README.md`).
