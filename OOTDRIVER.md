# Out-of-Tree Xe Driver

Intel GPU Base operator leverages [Kernel Module Management](https://github.com/kubernetes-sigs/kernel-module-management) (KMM) operator to install out-of-tree (OoT) driver to nodes. The ClusterPolicy CRD has a simplified version of the KMM's Module CRD included under `kernelModule` field in ClusterPolicy.

The OoT Xe driver project is located [here](https://github.com/intel-gpu/xekmd-backports/tree/releases/main).

## Prerequisites

* Kernel module management operator must be installed to the cluster
  * Intel GPU base operator won't try to deploy the Module CR if it cannot find KMM
  * KMM is detected once at operator startup by looking for the `kmm.sigs.x-k8s.io` API group. If KMM is installed after the operator, restart the operator deployment. Until then the `kernelModule` field is ignored and the ClusterPolicy reports `KMM is not installed in the cluster.` in `status.errors`.
* Container registry for storing kernel driver containers
  * Use harbor, docker's registry, or OpenShift's ImageStream
  * The registry must be writable: no prebuilt driver containers are published, so the images are always built in-cluster
* On OpenShift, install the operator with `openshift.enabled=true`
  * This creates the `<release>-module-loader-scc` SecurityContextConstraints (privileged, `SYS_MODULE`) that the module loader Pods need.

## Supported OSs

Currently the OoT KMD only supports Ubuntu 26.04.

## What the operator creates

When `kernelModule` is set in the ClusterPolicy, the operator creates a single KMM `Module` (`kmm.sigs.x-k8s.io/v1beta1`) named `<clusterpolicy-name>-gpu` in the operator's namespace. The Module is owned by the ClusterPolicy, so it is removed automatically when the ClusterPolicy is deleted.

A few ClusterPolicy fields are shared with the Module:

* `spec.pullSecret` is used as the Module's `imageRepoSecret`
* `spec.nodeSelector`, `spec.useNFDLabeling` and `spec.tolerations` are used for selecting and tolerating the target nodes
* The in-tree driver (`kernelModule.moduleName`, `xe` by default) is always added to the Module's `inTreeModulesToRemove` so the in-tree driver is unloaded before the OoT driver is loaded.

## Usage

Currently base operator only supports KMM's in-cluster builds. There are no prebuilt kernel mode driver (KMD) containers available. To leverage the OoT KMD, one has to have access to a registry that is used to store the KMD containers.

To leverage base operator's KMM integration, fill in `kernelModule` object in the ClusterPolicy CR. There is an incomplete sample for Ubuntu 26.04 [here](config/samples/dra-kmm-ubuntu26.04/). Incomplete means that the container registry, pull secret and nodeSelector are not properly set and need to be filled.

### pullSecret

Typically container registries are access controlled. Use the `pullSecret` field with a secret name that contains credentials to be able to push the built KMD container(s).

```
spec:
  pullSecret:
    name: <<REGISTRY_SECRET>>
```

The secret must be in the operator's namespace. With in-cluster builds the credentials need push access to the registry, not just pull.

Creating a pull secret from an existing docker config.json:

```
kubectl create secret generic my-registry \
  --namespace intel-gpu-base-operator \
  --type=kubernetes.io/dockerconfigjson \
  --from-file=.dockerconfigjson="$HOME/.docker/config.json"
```

### nodeSelector

Node selector is used to target nodes with Intel GPU hardware. For example, label nodes with `gpu-hw-wo-driver: true` and then use that as the nodeSelector.

Node selector:
```
spec:
  nodeSelector:
    gpu-hw-wo-driver: "true"
```

In addition to the labels given in `spec.nodeSelector`, the operator always requires `kubernetes.io/arch: amd64`.

*Note:* It's also possible to use `spec.useNFDLabeling: true` which allows targeting nodes with Intel GPU PCI devices. It should be noted that if there are heterogeneous nodes in the cluster, it's best to label nodes manually.

With `useNFDLabeling: true` the node selectors differ between the components:

* The KMM Module targets `intel.feature.node.kubernetes.io/gpu-pci: "true"`, which only requires an Intel GPU PCI device to be present. The module loader Pods can therefore be scheduled on nodes that do not yet have a working GPU driver.
* Device plugin, DRA and XPU Manager target `intel.feature.node.kubernetes.io/gpu: "true"`, which requires a working driver.

The `gpu-pci` label comes from the NFD rule in [config/deployments/nfd/node-feature-rules-gpu.yaml](config/deployments/nfd/node-feature-rules-gpu.yaml), so NFD and the GPU rule must be deployed for NFD based labeling to work.

Additionally, whenever `kernelModule` is set, the device plugin, DRA and XPU Manager node selectors get the KMM readiness label `kmm.node.kubernetes.io/<operator-namespace>.<clusterpolicy-name>-gpu.ready`. This way the GPU components are only started on nodes where the OoT driver has actually been loaded.

### containerImage

Container image field under the kernelMappings indicates the registry where the built KMD container will be pushed to (and pulled from).

```
spec:
  kernelModule:
    kernelMappings:
      - regexp: '7.0.0-.*-generic'
        containerImage: "<<CONTAINER_REGISTRY>>:v7.1.4.6_260728.7-$KERNEL_FULL_VERSION"
```

Note: `$KERNEL_FULL_VERSION` is replaced by KMM to reflect the kernel version running on the nodes. Other KMM template variables (for example `$MOD_NAME` and the `${VAR}` form) can be used as well.

Each `kernelMappings` entry needs:

* `regexp`: a regular expression matched against the node's kernel version. Anchored patterns (`^7\.0\.0-.*-generic$`) give exact matches.
* `containerImage`, `build` or both. The image reference must include an explicit tag or digest.

Changing `containerImage` is the recommended way to upgrade the driver. KMM rolls the new image out to all selected nodes at once, which briefly disrupts GPU workloads on those nodes while the module is reloaded.

### build

`build` enables KMM's in-cluster build. KMM builds and pushes the image if it is not found in the registry.

```
spec:
  kernelModule:
    kernelMappings:
      - regexp: '7.0.0-.*-generic'
        containerImage: "<<CONTAINER_REGISTRY>>:v7.1.4.6_260728.7-$KERNEL_FULL_VERSION"
        build:
          dockerfileConfigMap:
            name: xe-build-u26.04
          buildArgs:
            - name: XE_TAG
              value: xebr_v7.1.4.6_260728.7
```

* `dockerfileConfigMap` must point to a ConfigMap in the operator's namespace with the Dockerfile stored under the `dockerfile` key. See [build-configmap.yaml](config/samples/dra-kmm-ubuntu26.04/build-configmap.yaml) for a working Ubuntu 26.04 example.
* `buildArgs` are passed to the image builder as build arguments.
* `secrets` are build time secrets, for example credentials for a private source repository. They are not used for registry authentication, use `spec.pullSecret` for that.

The build Pods use the same node selector as the module itself, because building the driver requires the kernel headers and toolchain matching the kernel of the target nodes.

### firmwarePath

`firmwarePath` is the path inside the driver container that holds the firmware files. KMM copies the files from that path to the host's firmware search path before loading the module.

```
spec:
  kernelModule:
    firmwarePath: /firmware
```

Note: on containerd based clusters setting the firmware path currently fails, see the known issues below.

### modulesLoadingOrder

`modulesLoadingOrder` describes a softdep style loading order for drivers that consist of several modules. KMM loads the modules in the given order and unloads them in reverse order.

```
spec:
  kernelModule:
    modulesLoadingOrder:
      - "xe"
      - "drm_gpuvm"
      - "drm_buddy"
```

The list must have at least two entries and the first entry must be the same as `kernelModule.moduleName` (`xe` by default).

### registryTLS

`registryTLS` is meant for self-hosted registries that use plain HTTP or a certificate that is not trusted by the cluster.

```
spec:
  kernelModule:
    registryTLS:
      insecure: false
      insecureSkipTLSVerify: true
```

The setting can also be given per kernel mapping, in which case it overrides the value given under `kernelModule`.

### version and ordered upgrades

`kernelModule.version` opts into KMM's [ordered upgrade](https://kmm.sigs.k8s.io/documentation/ordered_upgrade), which is meant for low-disruption, node-by-node driver rollouts.

When `version` is set, KMM loads the driver on a node only after a cluster admin has labeled that node with the matching version label. Nodes without the label are left untouched, so nothing happens until the labels are added:

```
kubectl label node <NODE> \
  kmm.node.kubernetes.io/version-module.intel-gpu-base-operator.gpu-policy-gpu=v7.1.4.6_260728.7
```

The label format is `kmm.node.kubernetes.io/version-module.<operator-namespace>.<clusterpolicy-name>-gpu=<version>`. This lets the admin drain GPU workloads from a node before the driver is reloaded on it. Because of Kubernetes limitations on label names, the combined length of <operator-namespace>.<clusterpolicy-name>-gpu must not exceed 39 characters.

Most deployments should leave `version` unset and upgrade by changing `containerImage` instead.

Note: KMM's Module webhook does not allow adding or removing the version in place. When `version` is changed from empty to set, or from set to empty, the operator deletes and recreates the Module, which unloads the driver in the process. The ClusterPolicy reports `Recreating` in `status.kmmStatus` while this happens.

## Verification

When the ClusterPolicy is deployed, the status of the kernel module installation can be observed from the ClusterPolicy's status section. The `KMM` column shows how many of the targeted nodes have the module loaded.

```
$ kubectl get clusterpolicy
NAME         DP    DRA   XPU   KMM   AGE
gpu-policy   N/A   1/1   1/1   1/1   10m
```

```
$ kubectl get clusterpolicy gpu-policy -o yaml
...
status:
  draStatus: 1/1
  kmmStatus: 1/1
  xpuManagerStatus: 1/1
  devicePluginStatus: N/A
```

`kmmStatus` values:

| Value | Meaning |
|-|-|
| `<available>/<desired>` | Number of nodes where the module loader is available vs. targeted |
| `N/A` | `kernelModule` is not set in the ClusterPolicy |
| `Removing` | The Module is being deleted |
| `Recreating` | The Module is being recreated due to a `version` change |

While the driver is not yet loaded on every targeted node, the reason is reported under `status.errors`:

```
status:
  kmmStatus: 0/1
  errors:
  - module loader not fully available (0/1) for modules.kmm.sigs.x-k8s.io/gpu-policy-gpu
```

Digging deeper, when the status doesn't reach the desired count:

```
# Module CR status and events
kubectl -n intel-gpu-base-operator get modules
kubectl -n intel-gpu-base-operator describe module gpu-policy-gpu

# Build and module loader Pod logs
kubectl -n intel-gpu-base-operator get pods
kubectl -n intel-gpu-base-operator logs <POD>

# KMM's node labels: build/loader progress and readiness
kubectl get node <NODE> -o json | grep kmm.node.kubernetes.io

# On the node itself: confirm the loaded driver is the OoT one
modinfo xe | head
```

## Removing the OoT driver

Removing the `kernelModule` block from the ClusterPolicy, or deleting the ClusterPolicy, makes the operator delete the Module. KMM then unloads the OoT driver from the nodes.

On DRA clusters the operator refuses to delete or recreate the Module while there are allocated GPU ResourceClaims, so that the driver is not pulled out from under running GPU workloads. The reconcile is retried and the reason is reported in the ClusterPolicy status:

```
status:
  errors:
  - allocated GPU ResourceClaims blocking modules.kmm.sigs.x-k8s.io/gpu-policy-gpu deletion
```

Remove the workloads holding the claims to let the removal proceed. Note that unloading can still fail if devices are bound to the driver, see the known issues below.

## Known issues

* containerd: KMM's worker image is using USER 201 to set firmware load path. Even if the container is running with `securityContext: privileged: true`, the worker Pod fails to set the path. This will block KMD install.
  * KMM issue: https://github.com/kubernetes-sigs/kernel-module-management/issues/1337
* Device using a kernel driver blocks module removal: Xe driver cannot be removed if there are devices using it. The devices have to unbind first.
  * KMM has an enhancement to fix this: https://github.com/kubernetes-sigs/kernel-module-management/blob/main/docs/enhancements/0005-modprobed-config.md
  * By blacklisting the in-tree KMD, it's possible to load OoT driver once. But removing the OoT driver then fails due to the same reason.
* There are no prebuilt KMD containers, so a registry that the cluster can push to is always required.
* Only Ubuntu 26.04 is supported, see [Supported OSs](#supported-oss).
