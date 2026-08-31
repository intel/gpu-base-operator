# GPU Recovery

In some cases the Intel GPU needs to go through a low-level recovery operations which can be of the following kinds:

* **Inoperative state** — every operation on the GPU fails, but the device is still visible to the `xe` driver.
  A harder, PCIe-level reset is required.
* **Corrupted firmware** — card boots up into FDO (survivability) mode where only the PCIe link is
  active and needs to be reflashed with an up to date firmware image.

Recovering from either case is potentially disruptive: a PCIe reset can disturb other devices on the
same host, or wedge the host itself. The operator therefore never resets a GPU on its own — it
detects the need, reports it, and waits for a cluster admin to approve the operation.

Recovery is driven by the cluster-scoped `GPURecoveryPlan` CRD.

## How it works

```
GPU fault → Xe KMD → xpumd → GPU DRA driver → ResourceSlice device taint
                                                        ↓
                                    operator detects taint, adds a recovery event
                                                        ↓
                                    admin approves it (spec.approvals)
                                                        ↓
                            node drain (resets only) → recovery Job runs xpu-smi
                                                        ↓
                                    taint clears → event removed, plan back to idle
```

The operator watches `ResourceSlice` device taints published by the GPU DRA driver:

| Taint key | Meaning | Recovery |
|---|---|---|
| `health-xpumd-gpu.wedged` | GPU wedged at runtime | reset (`spec.defaultResetType`) |
| `health-Survivability` | card booted into survivability/FDO mode | `reflash` |
| `health-xpumd-gpu.survivability` | card fell into survivability at runtime | `reflash` |

Each affected GPU gets one entry in `status.events` with a deterministic ID
(`evt-<node>-<type>-<bdf>`). A device whose condition worsens (reset → reflash) is *escalated in
place*, which regenerates the event ID so an approval for the lighter operation cannot silently
authorise the heavier one.

Recovery Jobs run `xpu-smi` from the image in `spec.xpuSmi`, privileged, pinned to the affected node,
and are retained after completion for post-mortem diagnostics (they are removed when the event is).

### Reset types

| Type | Command | Notes |
|---|---|---|
| `slot` | `xpu-smi config -d <bdf> --coldreset` | PCIe slot power cycle; requires PCIe hot-plug support |
| `amc` | `xpu-smi amc --gpuReset -d <bdf>` | Out-of-band reset through the card's AMC |
| `sbr` | `xpu-smi config -d <bdf> --reset` | Secondary Bus Reset; per-card backup, via an approval override only |
| `reflash` | `xpu-smi updatefw -d <bdf> -t FDO -f <file>` | Flash the known good firmware onto a card in FDO mode |

These are **not** a severity ladder. Exactly one of `slot` and `amc` works on a given platform —
slot where the PCIe slots do hot-plug, AMC where they do not — and the DRA driver can only say "this
device needs a reset", not which mechanism applies. That is why `spec.defaultResetType` is mandatory
with no default: a reset the platform cannot perform **exits 0**, so a wrong value produces a clean
run to `succeeded` over a GPU that was never touched.

### Event states

| State | Meaning |
|---|---|
| `waiting-approval` | Detected, waiting for `spec.approvals`. Also where an event lands when its pre-flight image check failed |
| `missing-firmware` | Reflash needed but `spec.firmware` is absent, empty or volume-sourced |
| `blocked` | Approved, but another recovery is already running on this node. The approval is kept, so it resumes on its own |
| `draining` | Node is being drained before a reset |
| `in-progress` | Recovery Job is running |
| `succeeded` / `failed` | Job finished. A failure inside `spec.maxRetries` is re-queued for approval |

`status.events[].stateMessage` explains any state that is not self-explanatory (which recovery holds
the node, which image cannot be pulled, what a stalled drain was waiting on). An empty value means
there is nothing to add.

The plan-level `status.state` is `idle`, `active`, or `error`. `error` means an admin is needed: an
event out of retries, or one blocked on missing firmware.

## Safety mechanisms

* **Admin approval** for every recovery, singular or by selector (see below).
* **Node drain before a reset** (`spec.drain`). The whole node is drained, not just GPU pods, because
  an SBR or slot reset can wedge the host. A reflash never drains — it writes firmware without
  driving the bus. The drain uses the Eviction API, so PodDisruptionBudgets are honoured, and is
  bounded by `spec.drain.timeoutSeconds`. The operator's own namespace, DaemonSet pods, static pods
  and already-terminated pods are always skipped; `spec.drain.namespacesToSkip` adds to that.
* **Job deadlines** (`spec.timeouts`). Each recovery Job carries an `activeDeadlineSeconds`:
  `spec.timeouts.resetSeconds` (default 300) for the resets, `spec.timeouts.reflashSeconds`
  (default 600) for a reflash, which also covers copying the firmware out of the known good firmware
  image. They are configurable because how long the hardware takes is a property of the platform, and
  a deadline that expires early kills the Job mid-operation and fails the event over a card that was
  recovering — so raise them where a reset or flash is known to be slow.
* **In-use check.** A reset waits until every `ResourceClaim` reserving the GPU is released. Claims
  the drain can never release are excluded (`adminAccess` claims, and claims held only by pods the
  drain leaves in place) — waiting for those would deadlock the recovery.
* **One recovery per node at a time.** A second approved event on the same node is held in `blocked`
  rather than run concurrently: a PCIe reset during another card's firmware write can leave that card
  unrecoverable.
* **Pre-flight image verification.** `spec.xpuSmi.image` and the firmware image are resolved
  against their registries before a Job is created, so a mistyped reference does not produce a Job
  that reports `in-progress` from `ImagePullBackOff`. Checked once per spec generation; disable with
  `spec.skipImageVerification` where nodes hold pull credentials the operator cannot see.
* **Retry budget.** `spec.maxRetries` bounds automatic retries; an exhausted event needs an explicit
  per-event re-approval, so a standing group approval cannot loop a dying card forever.
* **Finalizer.** Deleting a plan blocks until every recovery Job is terminal, and releases all drain
  taints first.
* **The operator tolerates its own drain taint**, since it is the only thing that removes it.

## Example plan

```yaml
apiVersion: intel.com/v1alpha1
kind: GPURecoveryPlan
metadata:
  name: recoveryplan-bmg
spec:
  deviceId: "0xe20b"          # mandatory; one plan per GPU model
  defaultResetType: "slot"    # mandatory: "slot" or "amc"
  maxRetries: 3

  drain:
    enable: true
    timeoutSeconds: 300
    namespacesToSkip: ["kube-system"]

  timeouts:                   # how long a recovery Job itself may run
    resetSeconds: 300
    reflashSeconds: 600

  xpuSmi:
    image: "docker.io/intel/xpu-smi:devel"
    pullPolicy: "IfNotPresent"

  firmware:             # required for reflash recovery
    source:
      containerSource:
        name: "docker.io/intel/intel-gpu-fw-binaries:devel"
    file: "fdo_firmware.bin"
```

Heterogeneous clusters use one plan per device ID. A full, commented sample lives in
[`config/samples/recoveryplan/gpurecoveryplan.yaml`](config/samples/recoveryplan/gpurecoveryplan.yaml).

## Approving recovery

An approval either names one event or selects a group. Selector approvals are one-shot unless marked
`persistent: true`. `override` changes the recovery type that will actually run, and the original
suggestion is kept in `status.events[].recoveryType.suggestedType` for audit.

```yaml
spec:
  approvals:
    # one specific event
    - eventId: evt-node05-slot-02-00-0

    # escalate one card to a Secondary Bus Reset
    - eventId: evt-node03-slot-02-00-0
      override:
        recoveryType: sbr

    # all current reset events on nodes labelled rack=rack-04-32
    - selector:
        recoveryType: slot
        nodeSelector:
          rack: rack-04-32

    # standing order: auto-approve future slot resets (CI/test clusters)
    - selector:
        recoveryType: slot
      persistent: true
```

The operator fills in a missing `id`, and marks non-persistent approvals `consumed: true` once acted
upon; consumed entries stay as an audit trail and can be removed by hand.

### kubectl plugin

`kubectl-gpurecovery` wraps the patching, with tab completion for plan names, event IDs and approval
IDs:

```sh
make install-kubectl-plugin   # builds to bin/ and installs to ~/.local/bin
make setup-completion         # optional: shell completion

kubectl gpurecovery plans
kubectl gpurecovery events <plan>
kubectl gpurecovery messages <plan>
kubectl gpurecovery approvals <plan>
kubectl gpurecovery approve <plan> <event-id>
kubectl gpurecovery confirm <plan> <recovery-type> [--persistent]
kubectl gpurecovery remove <plan> <approval-id>
```

Plain `kubectl patch` works too, e.g.:

```sh
kubectl patch gpurecoveryplan <plan> --type=json \
  -p='[{"op":"add","path":"/spec/approvals/-","value":{"eventId":"<event-id>"}}]'
```

## Metrics

Exposed on the operator's own `/metrics` endpoint (there is deliberately no `status.stats` field —
the CR holds current state, the counters hold history):

| Metric | Labels |
|---|---|
| `gpu_recovery_events_total` | `plan`, `type`, `reason` |
| `gpu_recovery_attempts_total` | `plan`, `type` |
| `gpu_recovery_outcomes_total` | `plan`, `type`, `result` |
| `gpu_recovery_overrides_total` | `plan`, `suggested_type`, `chosen_type` |
| `gpu_recovery_events` (gauge) | `plan`, `node`, `type`, `state` |
| `gpu_recovery_plan_state` (gauge) | `plan`, `state` |

## Current limitations

* **Sibling devices are not protected.** Nothing stops a slot reset or SBR on one card from
  disturbing another card behind the same PCIe root port. Drain the node and check the topology
  before approving.
* **Job targeting is privileged + `nodeName`**, not a DRA claim with device tolerations and CEL
  expressions as originally designed. Still an open decision.
* **`spec.subDeviceId` / `spec.subVendorId` are validated but not used for matching** — the DRA
  driver does not publish those attributes yet.
* **`firmware.source.volumeSource` is not implemented.** A reflash event on a
  volume-only plan stays in `missing-firmware`; use `containerSource`.
* **Reset efficacy on BMG.** On some B580 cards a reset leaves the GPU non-working; end-to-end
  validation depends on driver/firmware fixes.
* **The `health-xpumd-gpu.wedged` taint key is not yet confirmed against a shipping DRA driver.**
  The survivability key and the `pciId` / `pciAddress` device attributes are.

