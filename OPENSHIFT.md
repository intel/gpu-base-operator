# Intel GPU Base Operator on OpenShift

This document covers what is different when running the operator on Red Hat OpenShift:
installing through OLM, the SecurityContextConstraints the operator manages, the optional
NFD, KMM and monitoring integrations, and disconnected (air-gapped) installs.

- [How the operator detects OpenShift](#how-the-operator-detects-openshift)
- [SecurityContextConstraints](#securitycontextconstraints)
- [Installing with OLM (bundle)](#installing-with-olm-bundle)
- [Installing with Helm](#installing-with-helm)
- [Node Feature Discovery](#node-feature-discovery)
- [Out-of-tree driver with KMM](#out-of-tree-driver-with-kmm)
- [Monitoring](#monitoring)
- [Disconnected installs](#disconnected-installs)
- [Building the bundle](#building-the-bundle)

## How the operator detects OpenShift

At startup the operator lists the API groups served by the cluster. If either
`route.openshift.io` or `security.openshift.io` is present, it treats the cluster as
OpenShift and creates SecurityContextConstraints (SCC) for the components it deploys.
This detection logic cannot be turned off.

The same startup check decides whether DRA (`resource.k8s.io`) and KMM (`kmm.sigs.x-k8s.io`)
are available. Detection happens only once: if you install KMM after the operator, restart
the operator Deployment.

## SecurityContextConstraints

On OpenShift the operator creates four objects for each component it deploys:

| Object | Name |
|---|---|
| SecurityContextConstraints | `<cr-name>-<component>-scc` |
| ClusterRole (`use` on that SCC) | `<cr-name>-<component>-scc-role` |
| ClusterRoleBinding | `<cr-name>-<component>-scc-binding` |
| ServiceAccount (in the operator namespace) | `<cr-name>-<component>-sa` |

`<cr-name>` is the name of the ClusterPolicy, GPUFirmwareUpdate or GPURecoveryPlan, and
`<component>` is one of:

| Component | Created by | Privileged | Host paths |
|---|---|---|---|
| `gpu-dp` (Device Plugin) | ClusterPolicy | No | Yes |
| `gpu-dra` (DRA driver) | ClusterPolicy | No | Yes |
| `xpu-manager` (XPU Manager) | ClusterPolicy | No | Yes |
| `gpu-fwupdate` (firmware update Jobs) | GPUFirmwareUpdate | Yes | Yes |
| `gpu-recovery` (recovery Jobs) | GPURecoveryPlan | Yes | Yes |

The operator deletes these objects when the owning CR is deleted or the component is
disabled. Review the privileged SCCs before you create GPUFirmwareUpdate or GPURecoveryPlan
CRs; see the [Security Configuration Guide](SECURITY-CONFIGURATION.md).

The KMM module loader is the exception: the operator does not create an SCC for it. OLM installs grant
it the built-in `privileged` SCC, and the Helm chart creates a dedicated SCC. See
[Out-of-tree driver with KMM](#out-of-tree-driver-with-kmm).

## Installing with OLM (bundle)

The operator can be packaged as an OLM bundle, but it is not yet available in registries.
The CSV supports the `AllNamespaces` install mode only: the operator runs in one namespace
and manages the whole cluster.

Webhook TLS certificates are provided by OLM, so cert-manager is not needed for OLM installs.

### Prepare the namespace

When you install from OperatorHub in the OpenShift console, the console suggests the
`intel-gpu-base-operator` namespace and creates it with the label DRA monitoring needs. You
can skip to [Install](#install).

For any other OLM install (`operator-sdk run bundle`, or a Subscription you create yourself),
OLM installs into an existing namespace and does not create one. Create the namespace and add
the label:

```sh
oc create namespace intel-gpu-base-operator
oc label namespace intel-gpu-base-operator resource.kubernetes.io/admin-access=true
```

The `admin-access` label is needed for XPU Manager's monitoring ResourceClaim when
`resourceRegistration: dra` is used; the API server rejects admin-access claims in namespaces
without it. The Helm chart adds it for you. If it is missing, the
ClusterPolicy reports it in `status.errors` together with the command that adds it, and
XPU Manager is not deployed. Once the label is added, the operator picks it up right away.

The Device Plugin, DRA driver and XPU Manager run in the operator namespace. They get access
through the SCCs above. If their pods are rejected with a Pod Security violation, label the
namespace privileged:

```sh
oc label namespace intel-gpu-base-operator pod-security.kubernetes.io/enforce=privileged
```

### Install

With `operator-sdk`:

```sh
operator-sdk run bundle <bundle location>:<version> \
  --namespace intel-gpu-base-operator \
  --security-context-config restricted
```

Always pass `--namespace`. Without it, `run bundle` uses the current namespace of your
kubeconfig context, which is often `default`. The command creates an OperatorGroup in the
namespace if there is none. `--security-context-config restricted` lets the temporary catalog
pod run under OpenShift's default restricted SCC.

For a permanent install, add the bundle to a catalog (`make catalog-build`) and create a
CatalogSource and a Subscription for the `intel-gpu-base-operator` package in that namespace.

### Uninstall

Delete the ClusterPolicy first and wait for it to go away, so the operator can tear down DRA
while it is still running. Then remove the operator:

```sh
oc delete clusterpolicy --all --wait
operator-sdk cleanup intel-gpu-base-operator --namespace intel-gpu-base-operator
```

## Installing with Helm

The Helm install described in the [README](README.md#helm-deployment) also works on OpenShift.
Two differences:

- cert-manager is required for webhook certificates. On OpenShift, install the
  *cert-manager Operator for Red Hat OpenShift* from OperatorHub.
- Set `openshift.enabled=true` on the operator chart. It only controls the KMM module loader
  SCC; the component SCCs are created by the operator either way.

```sh
helm install --create-namespace --namespace intel-gpu-base-operator gpu-operator \
  oci://ghcr.io/intel/intel-gpu-base-operator-chart --wait \
  --set openshift.enabled=true
```

Do not use the chart's `nfd.install=true` on OpenShift. Use the Red Hat NFD operator
instead, as described below.

## Node Feature Discovery

With `useNFDLabeling: true` the operator creates NodeFeatureRules that label nodes with Intel
GPUs. Use of these labels allows scheduling Pods to nodes with Intel GPUs. It is preferred to
start operator after NFD has been installed. On OpenShift, install the Red Hat
*Node Feature Discovery* operator and create an NFD instance:

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: openshift-nfd
  labels:
    openshift.io/cluster-monitoring: "true"
---
apiVersion: operators.coreos.com/v1
kind: OperatorGroup
metadata:
  name: openshift-nfd
  namespace: openshift-nfd
spec:
  targetNamespaces:
  - openshift-nfd
---
apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata:
  name: nfd
  namespace: openshift-nfd
spec:
  channel: stable
  installPlanApproval: Automatic
  name: nfd
  source: redhat-operators
  sourceNamespace: openshift-marketplace
```

Once the operator is running, create a `NodeFeatureDiscovery` CR in `openshift-nfd`. The
defaults from the OperatorHub form are enough. The GPU base operator creates rules which
leverage PCI device information, so pci device info has to be enabled in the NFD worker config.

## Out-of-tree driver with KMM

**NOTE**: OoT KMD in RHEL/OpenShift is not yet available.

To load the out-of-tree Xe driver, install the Red Hat *Kernel Module Management* operator and
follow [OOTDRIVER.md](OOTDRIVER.md). An OpenShift ImageStream can serve as the registry for
in-cluster driver builds.

KMM's module loader pods run as the `intel-gpu-base-operator-module-loader` ServiceAccount in
the operator namespace. The operator's KMM Module names this ServiceAccount explicitly, so
KMM's own privileged ServiceAccount is not used. On OpenShift the module loader therefore
needs its own permission to run privileged pods:

- **OLM:** nothing to do. The bundle grants the ServiceAccount `use` of OpenShift's built-in
  `privileged` SCC, through a Role and RoleBinding in the install namespace. This is the setup
  the [KMM documentation](https://github.com/rh-ecosystem-edge/kernel-module-management/blob/main/docs/mkdocs/documentation/deploy_kmod.md#security-and-permissions)
  describes for Modules outside KMM's own namespace.
- **Helm:** `openshift.enabled=true` creates a dedicated SCC for the ServiceAccount.

To check the grant, look for the RoleBinding in the operator namespace:

```sh
oc get rolebinding -n intel-gpu-base-operator -o wide | grep module-loader
```

## Monitoring

With `resourceMonitoring: true` and `prometheusIntegration: true`, the operator creates a
ServiceMonitor for XPU Manager in the operator namespace. On OpenShift, enable
[monitoring for user-defined projects](https://docs.redhat.com/en/documentation/openshift_container_platform/latest/html/monitoring/configuring-user-workload-monitoring)
so that this ServiceMonitor gets scraped:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: cluster-monitoring-config
  namespace: openshift-monitoring
data:
  config.yaml: |
    enableUserWorkload: true
```

## Disconnected installs

The CSV is annotated `features.operators.openshift.io/disconnected: "true"`. All images it
deploys are pinned by digest and listed in `spec.relatedImages`, so `oc-mirror` copies them
with the bundle:

| relatedImages name | Image |
|---|---|
| `gpu-plugin` | GPU Device Plugin |
| `gpu-dra` | GPU DRA driver |
| `xpumanager` | XPU Manager |

The operator image itself is the CSV's deployment image.

Mirror the operator with oc-mirror, then apply the generated `ImageDigestMirrorSet` and
CatalogSource. For example, with oc-mirror v2:

```yaml
apiVersion: mirror.openshift.io/v2alpha1
kind: ImageSetConfiguration
mirror:
  operators:
  - catalog: <catalog image containing the intel-gpu-base-operator bundle>
    packages:
    - name: intel-gpu-base-operator
```

The `ImageDigestMirrorSet` redirects the digest-pinned default images to the mirror. Leave the
image fields in the ClusterPolicy empty so these defaults are used.

### Images you mirror yourself

Some images are not part of the bundle because they are supplied in your CRs:

- GPUFirmwareUpdate: `spec.updaterImage` and `spec.content.containerImage`
- GPURecoveryPlan: `spec.xpuSmi.image` and the firmware image used for reflashing
- ClusterPolicy `kernelModule`: the driver images built or pulled by KMM

Mirror these images with `oc-mirror` (`additionalImages`) or `oc image mirror`.

**Point these CRs at the mirror registry directly.** Before creating any Jobs, the operator
checks the GPUFirmwareUpdate content image, and the GPURecoveryPlan images, against their
registry. The operator makes this request itself, so `ImageDigestMirrorSet` does not apply to
it, and a reference to the original registry fails in a disconnected cluster. For
GPURecoveryPlan you can also set `spec.skipImageVerification: true`. GPUFirmwareUpdate has no
such option. Use `spec.imagePullSecret` if the mirror needs credentials.

## Building the bundle

```sh
make docker-build docker-push IMG=<registry>/intel-gpu-base-operator:<tag>
make bundle USE_IMAGE_DIGESTS=true IMG=<registry>/intel-gpu-base-operator:<tag>
make bundle-build BUNDLE_IMG=<registry>/intel-gpu-base-operator-bundle:<tag>
podman push <registry>/intel-gpu-base-operator-bundle:<tag>
```

`USE_IMAGE_DIGESTS=true` resolves `IMG` to a digest, so it must already be pushed. The build
fails if any image in the CSV is not pinned by digest. Without it, `make bundle` only prints a
warning, and the resulting bundle cannot be used in a disconnected cluster.

The default images for the Device Plugin, DRA driver and XPU Manager come from the
`RELATED_IMAGE_*` env vars in `config/manager/manager.yaml`. When you change one of them,
update the matching `Default*Image` constant in `api/v1alpha1/clusterpolicy_webhook.go` and
`operandImages` in `charts/gpu-base-operator/values.yaml` too.
