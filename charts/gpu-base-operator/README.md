# Intel GPU base operator Helm chart

Helm chart is for installing the Intel GPU base operator. Operator installation is a dependency for the [policy chart](../gpu-base-operator-policy/README.md). Once the operator is installed, the policy chart can configure the cluster in a certain way.

## Prerequisites
- [cert-manager](https://cert-manager.io/docs/installation/) [required — provisions TLS certificates for the admission webhook]
- [Node Feature Discovery NFD](https://kubernetes-sigs.github.io/node-feature-discovery/master/get-started/deployment-and-usage.html) [recommended, optional]
- [Prometheus](https://github.com/prometheus-community) [optional]

## Helm install
```
kubectl create ns intel-gpu-operator
# Required by DRA's admin access
kubectl label ns intel-gpu-operator resource.kubernetes.io/admin-access=true

helm install --namespace "intel-gpu-operator" --version 0.2.1 gpu-operator \
  oci://ghcr.io/intel/intel-gpu-base-operator-chart --wait
```

## Helm upgrade
```
helm upgrade --namespace "intel-gpu-operator" --version 0.2.1 gpu-operator \
  oci://ghcr.io/intel/intel-gpu-base-operator-chart --wait
```

## Helm uninstall
```
helm uninstall --namespace "intel-gpu-operator" gpu-operator
kubectl delete ns intel-gpu-operator
```

## Configuration
See [Customizing the Chart Before Installing](https://helm.sh/docs/intro/using_helm/#customizing-the-chart-before-installing).

| Value | Default Value | Description|
|---|---|---|
| `createNamespace`| true | Create the namespace during chart installation |
| `kueue.install`| false | Have operator install kueue when installing this Helm chart |
| `nfd.install`| false | Have operator install NFD when installing this Helm chart |
| `operator.image.repository`| ghcr.io/intel/intel-gpu-base-operator:devel | Operator container image (repository and tag) |
| `operator.image.pullPolicy` | IfNotPresent| Image pull policy |
| `operator.verbosity` | 2 | Operator logging verbosity level |
| `operator.resources.limits.cpu` | 500m | CPU limit for operator pod |
| `operator.resources.limits.memory` | 128Mi | Memory limit for operator pod |
| `operator.resources.requests.cpu` | 10m | CPU request for operator pod |
| `operator.resources.requests.memory` | 64Mi | Memory request for operator pod |
| `privateRegistry.url` | "" | Private registry URL |
| `privateRegistry.user` | "" | Private registry username |
| `privateRegistry.token` | "" | Private registry authentication token |

Private registry values may be used for internal/private registries that require authentication. They convert into a Kubernetes secret that is passed to the operator's deployment.