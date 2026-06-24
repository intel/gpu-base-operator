# Intel GPU Base Operator for Kubernetes

[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/intel/gpu-base-operator/badge)](https://scorecard.dev/viewer/?uri=github.com/intel/gpu-base-operator)

CAUTION: This is an beta / non-production software, do not use on production clusters.

Intel GPU Base operator allows automatic deployment of GPU related components to enable use of Intel GPU hardware within the Kubernetes cluster.

## Description

![Architecture diagram](docs/architecture.svg)

Intel GPU Base operator deploys components to a cluster to make it possible to run Intel GPU workloads on the cluster nodes. It can configure the cluster with either Device Plugin or Dynamic Resource Allocation (DRA) based approach. In addition to registering GPU resources for the cluster, it can also deploy Intel XPU-Manager. If the cluster has Prometheus enabled, operator can deploy the required Kubernetes components to integrate GPU metrics to the Prometheus database.

Operator does its best to detect cluster features and to only deploy components that are supported. For example, GPU DRA driver is only deployed from certain Kubernetes version onwards.

When the operator is requested with device plugin, resource monitoring:
```
apiVersion: intel.com/v1alpha1
kind: ClusterPolicy
metadata:
  name: gpu-dp
spec:
  resourceRegistration: dp
  useNFDLabeling: true
  resourceMonitoring: true
```
the end result will look something like this:
```
$ kubectl get pods -n intel-gpu-base-operator
NAME                                                          READY   STATUS    RESTARTS   AGE
gpu-dp-device-plugin-5txt5                                    2/2     Running   0          2m4s
gpu-dp-xpu-manager-hdg6b                                      2/2     Running   0          2m4s
intel-gpu-base-operator-controller-manager-749cb69d6c-kqnzw   1/1     Running   0          124m
```

Device Plugin GPU resources can be seen like so:
```
$ kubectl get node node1 -o yaml | yq .status.allocatable
...
gpu.intel.com/i915: "1"
gpu.intel.com/i915_monitoring: "1"
...
```

When the operator is requested with DRA, resource monitoring and with NFD:
```
apiVersion: intel.com/v1alpha1
kind: ClusterPolicy
metadata:
  name: gpu-dra
spec:
  resourceRegistration: dra
  useNFDLabeling: true
  resourceMonitoring: true
```
the end result will look something like this:

```
$ kubectl get pods -n intel-gpu-base-operator
NAME                                                          READY   STATUS    RESTARTS   AGE
gpu-dra-gpu-dra-rqr94                                         1/1     Running   0          88s
gpu-dra-xpu-manager-cqsbj                                     2/2     Running   0          88s
intel-gpu-base-operator-controller-manager-749cb69d6c-kqnzw   1/1     Running   0          147m
```

DRA GPU resource slices can be seen like so:
```
$ kubectl get resourceslices.resource.k8s.io
NAME                                NODE            DRIVER          POOL            AGE
node1                               node1           gpu.intel.com   node1           111s
```

## Dependencies

|Dependency|Level|Purpose|Chart integration|
|---|---|---|---|
|Cert Manager|Mandatory|The operator uses webhooks which have hooks to cert-manager's TLS apply.|None|
|Node Feature Discovery|Recommended|Allows scheduling Pods only to Nodes with Intel GPU hardware.|Optionally installed|
|Kueue|Recommended|Enables advanced Pod scheduling mechanisms.|Optionally installed|
|Prometheus|Recommended|Accesses GPU metrics from all Nodes from one endpoint.|None|

## Getting Started

The latest release version is [0.2.1](https://github.com/intel/gpu-base-operator/releases/tag/v0.2.1). Instructions for deploying the operator via Helm are described below.

### Helm deployment

The _preferred_ installation method to the cluster via our Helm charts.

Helm deployment is split into two charts: operator and policy. The reason for this split is to allow the operator to run cleanup before it is removed from the cluster. DRA especially is problematic as Pods using its resources (e.g. XPU Manager) will get stuck at `Terminating` if the DRA plugin is removed from the cluster.

The basic installation is as follows:
```
kubectl create ns intel-gpu-operator
# Required by DRA's admin access
kubectl label ns intel-gpu-operator resource.kubernetes.io/admin-access=true

helm install --namespace "intel-gpu-operator" --version 0.2.1 gpu-operator \
  oci://ghcr.io/intel/intel-gpu-base-operator-chart --wait
helm install --namespace "intel-gpu-operator" --version 0.2.1 gpu-policy \
  oci://ghcr.io/intel/intel-gpu-base-operator-policy-chart --set resourceRegistration=dra
```

This installs the operator and a DRA-enabled deployment with Intel XPU Manager. Node Feature Discovery and Kueue may be installed with the operator chart; this depends on the `kueue.install` and `nfd.install` parameters.

#### Example: DRA without NFD

```
helm install --namespace "intel-gpu-operator" --version 0.2.1 gpu-operator \
  oci://ghcr.io/intel/intel-gpu-base-operator-chart --wait
helm install --namespace "intel-gpu-operator" --version 0.2.1 gpu-policy \
  oci://ghcr.io/intel/intel-gpu-base-operator-policy-chart --set resourceRegistration=dra
```

#### Example: DRA with NFD and Kueue

```
helm install --namespace "intel-gpu-operator" --version 0.2.1 gpu-operator \
  oci://ghcr.io/intel/intel-gpu-base-operator-chart --wait \
  --set nfd.install=true \
  --set kueue.install=true
helm install --namespace "intel-gpu-operator" --version 0.2.1 gpu-policy \
  oci://ghcr.io/intel/intel-gpu-base-operator-policy-chart \
  --set resourceRegistration=dra \
  --set useNFDLabeling=true \
  --set enableKueue=true
```

#### Example: Device Plugin with NFD

```
helm install --namespace "intel-gpu-operator" --version 0.2.1 gpu-operator \
  oci://ghcr.io/intel/intel-gpu-base-operator-chart --wait \
  --set nfd.install=true
helm install --namespace "intel-gpu-operator" --version 0.2.1 gpu-policy \
  oci://ghcr.io/intel/intel-gpu-base-operator-policy-chart \
  --set resourceRegistration=dp \
  --set useNFDLabeling=true
```

Uninstalling the charts:
```
helm uninstall --namespace "intel-gpu-operator" gpu-policy --wait
helm uninstall --namespace "intel-gpu-operator" gpu-operator
kubectl delete ns intel-gpu-operator
```

See more details for the charts in the [operator](charts/gpu-base-operator/README.md) and [policy](charts/gpu-base-operator-policy/README.md) READMEs.

### Custom Resource (CR) fields

CR fields control how the operator configures the cluster. See the [full struct](api/v1alpha1/clusterpolicy_types.go) for all options.

#### Core fields

|Field|Description|Default|
|---|---|---|
|`spec.resourceRegistration`|Resource registration mode: `dp` (Device Plugin) or `dra` (Dynamic Resource Allocation)|—|
|`spec.resourceMonitoring`|Deploy XPU-Manager for per-GPU telemetry and health monitoring|`false`|
|`spec.useNFDLabeling`|Deploy an NFD NodeFeatureRule to label Intel GPU nodes; requires NFD to be installed|`false`|
|`spec.prometheusIntegration`|Create a ServiceMonitor so Prometheus scrapes XPU-Manager GPU metrics; requires Prometheus Operator|`false`|
|`spec.enableKueue`|Create Kueue ResourceFlavor / ClusterQueue / LocalQueue resources for GPU workload queuing|`false`|
|`spec.logLevel`|Default log level for all deployed components (0–4)|`2`|

#### Device Plugin (`spec.dp`)

Used when `spec.resourceRegistration: dp`.

|Field|Description|Default|
|---|---|---|
|`spec.dp.allowIDs`|Only register GPUs whose PCI device ID is in this list. Format: `['0xabcd']`. Cannot be combined with `denyIDs`|`[]`|
|`spec.dp.denyIDs`|Exclude GPUs whose PCI device ID is in this list. Format: `['0xabcd']`. Cannot be combined with `allowIDs`|`[]`|
|`spec.dp.byPathMode`|Controls which DRI by-path symlinks are exposed to containers: `single`, `all`, or `none`|`single`|

#### Dynamic Resource Allocation (`spec.dra`)

Used when `spec.resourceRegistration: dra`.

|Field|Description|Default|
|---|---|---|
|`spec.dra.podHealthCheck`|Enable health check for DRA Pod|true|

#### Health monitoring (`spec.health`)

Applies to both DP and DRA unless noted. Thresholds that are exceeded mark the GPU as unhealthy.

|Field|Description|Default|
|---|---|---|
|`spec.health.coreTemperatureThreshold`|GPU core temperature limit in °C (1–130)|`100`|
|`spec.health.memoryTemperatureThreshold`|GPU memory temperature limit in °C (1–130)|`100`|
|`spec.health.checkIntervalSeconds`|How often health is evaluated in seconds (1–3600). Not supported by Device Plugin|`5`|

#### XPU-Manager (`spec.xpu`)

|Field|Description|Default|
|---|---|---|
|`spec.xpu.monitoringResource`|Set XPUMD resource for Device Plugin use.|`xe_monitoring`|
|`spec.xpu.configMapOverride`|Name of a ConfigMap in the operator namespace containing a custom OpenTelemetry Collector `config.yaml`|—|

#### Kueue (`spec.kueue`)

When `enableKueue: true`, the operator creates Kueue `ClusterQueue` and `LocalQueue` resources to enable GPU-aware job scheduling via [Kueue](https://kueue.sigs.k8s.io/).

The operator currently supports one configuration for Kueue: `equal division`. With equal division, the operator discovers all GPU resources available in the cluster and divides them **equally** across the ClusterQueues defined in `spec.kueue.equalResources`. It also automatically creates a single `ResourceFlavor` that covers all GPU resource types. Other configuration options and resource division schemas will be added in the future.

|Field|Description|
|---|---|
|`spec.kueue.equalResources`|List of ClusterQueues. GPU capacity is split evenly across all entries|
|`spec.kueue.equalResources[].name`|Name of the Kueue `ClusterQueue` to create|
|`spec.kueue.equalResources[].localQueues`|List of `LocalQueue` resources to create for this ClusterQueue|
|`spec.kueue.equalResources[].localQueues[].name`|Name of the `LocalQueue`|
|`spec.kueue.equalResources[].localQueues[].namespace`|Namespace in which the `LocalQueue` is created|

> **NOTE**: Kueue prevents Pods from consuming more resource than defined in `ClusterQueues`. Kueue does not prevent Pods in one `LocalQueues` from consuming all the resources in the `ClusterQueue`, thus leaving no resources for the other `LocalQueues`.

### Building the operator

To build the operator without containerization.

Prerequisites:
* golang (1.24+)
* make

```
go mod vendor
make build
```

To build the operator container.

Prerequisites for the docker-build:
* docker version 17.03+
* make

```
make docker-build
```

### Development builds

While developing the operator, it is sometimes required to re-generate the autogenerated code. This can be done by
```
make generate
```

If the operator receives (or loses) access rights to the cluster, the deployment files can be updated with:
```
make manifests
```

One can run the operator unit tests with:
```
make test
```

### Dependencies

Cert-manager:
```
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.19.4/cert-manager.yaml
```

Node Feature Discovery (optional):
```
kubectl apply -k config/nfd
```

Kueue (optional):
```
kubectl apply -k config/kueue
```

### Kubectl deployment

> NOTE: As the operator's images are not available anywhere yet, one has to build the container and store it in a registry somewhere.

To store the operator container image in the container-runtime image cache:
```
docker save ghcr.io/intel/intel-gpu-base-operator:devel | sudo ctr -n k8s.io image import -
```

To deploy the operator:

```
kubectl apply -k config/default
```

To undeploy the operator:

```
kubectl delete -k config/default
```

To deploy a Device Plugin CR:
```
kubectl apply -k config/samples/deviceplugin/
```

To deploy a DRA CR:
```
kubectl apply -k config/samples/dra/
```

> *WARNING*: If the operator is deleted when DRA CR is configured, the XPU-Manager Pod has the tendency to get stuck in `Terminating` phase. This is because DRA needs to tear down the claim for the Pod to get Terminated. If that happens, the quickest fix is to redeploy the operator and custom resource, and then remove the custom resource properly.

## Firmware update

The Intel GPU base operator supports updating the GPU firmware on the cluster nodes. The update is handled via a GPUFirmwareUpdate CRD. The update flow and details are explained in the [FW update documentation](FWUPDATE.md).

## Contributing

[Contributions](CONTRIBUTING.md) to this project are welcome as issues (bugs, enhancement requests) or via pull requests. Please review our [Code of Conduct](CODE_OF_CONDUCT.md) and our note on [security policy](SECURITY.md).

## License

Copyright 2025 Intel Corporation. All Rights Reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

##

Intel and the Intel logo are trademarks of Intel Corporation or its subsidiaries.
