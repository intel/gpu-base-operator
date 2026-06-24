# Intel GPU base operator policy Helm chart

Helm chart is for installing the Intel GPU base operator policy. The operator has to be installed before the policy. See [the operator chart](../gpu-base-operator/README.md).


## Helm install
```
helm install --namespace "intel-gpu-operator" --version 0.2.1 gpu-policy \
  oci://ghcr.io/intel/intel-gpu-base-operator-policy-chart
```

## Helm upgrade
```
helm upgrade --namespace "intel-gpu-operator" --version 0.2.1 gpu-policy \
  oci://ghcr.io/intel/intel-gpu-base-operator-policy-chart
```

## Helm uninstall
```
helm uninstall --namespace "intel-gpu-operator" gpu-policy --wait
```

## Configuration
See [Customizing the Chart Before Installing](https://helm.sh/docs/intro/using_helm/#customizing-the-chart-before-installing).

| Key | Default Value | Description |
|---|---|---|
| resourceRegistration | dp | Resource registration mode (dp or dra). |
| useNFDLabeling | false | Enable Node Feature Discovery labeling. |
| resourceMonitoring | true | Enable resource monitoring. |
| enableKueue | false | Set up Kueue queues for node resources. |
| prometheusIntegration | false | Integrate metrics into Prometheus. |
| logLevel | 1 | Global log level. |
| health.coreTemperatureThreshold | 88 | Core temperature threshold for health checks (°C). |
| health.memoryTemperatureThreshold | 99 | Memory temperature threshold for health checks (°C). |
| health.checkIntervalSeconds | 12 | Interval for health checks (seconds). |
| dp.plugin | intel/intel-gpu-plugin:0.36.0 | DP plugin image. |
| dp.logLevel | 2 | DP log level. |
| dp.byPathMode | single | DP by-path mounting mode |
| dp.allowIDs | [] | Allowed PCI Device IDs |
| dp.denyIDs | [] | Denied PCI Device IDs |
| dra.image | ghcr.io/intel/intel-resource-drivers-for-kubernetes/intel-gpu-resource-driver:v0.11.0 | DRA driver image. |
| dra.logLevel | 2 | DRA log level. |
| dra.podHealthCheck | true | Health check for DRA Pod. |
| dra.manageBinding | false | Allow DRA plugin to manage device binding between xe/i915 and vfio drivers. Needed for dynamic switching between normal and KubeVirt workloads. |
| xpu.image | ghcr.io/intel/xpumanager/xpumd:v2.0.0 | XPU manager image. |
| xpu.logLevel | 2 | XPU manager log level. |
| xpu.monitoringResource | monitoring | Monitoring resource for XPUMD with device plugin. |
| xpu.configMapOverride | "" | Override the default XPUM configuration ConfigMap name. |
| kueue.equalResources | [] | List of ClusterQueue configurations. |
| pullSecret | null | Image pull secret. |
| nodeSelector | {} | Node selector for scheduling pods. |
| tolerations | [] | Tolerations for scheduling pods. |
