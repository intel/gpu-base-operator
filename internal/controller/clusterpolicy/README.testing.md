# Testing GPU Base Operator Kueue with fake devices

GPU Base Operator Kueue behavior can be tested with the help of made up
device entries for both Device Plugins and Dynamic Resource Allocation (DRA).
These devices exist only as resource entries, no actual devices are created
nor emulated. Since this is an excersise in resource entries, both Device
Plugin and DRA fake devices can exist simultaneously in the same test cluster.

## Setting up fake devices for Device Plugins

Clone the Device Plugin repository, build images and tag them
```
git clone https://github.com/intel/intel-device-plugins-for-kubernetes
cd intel-device-plugins-for-kubernetes
make intel-gpu-fakedev
make intel-gpu-plugin
```

Tag and export the built images
```
docker tag docker.io/intel/intel-gpu-fakedev:devel registry.local/intel-gpu-fakedev:devel
docker tag docker.io/intel/intel-gpu-plugin:devel registry.local/intel-gpu-plugin:devel

docker image save registry.local/intel-gpu-fakedev:devel -o intel-gpu-fakedev.tar
docker image save registry.local/intel-gpu-plugin:devel -o intel-gpu-plugin.tar
```
Upload the .tar images to the test cluster nodes or its registry, if there is
one. If uploaded manually to the intended cluster nodes, images are imported
into containerd with
```
ctr n=k8s.io image import intel-gpu-fakedev.tar
ctr n=k8s.io image import intel-gpu-plugin.tar
```

Do notice that in OpenShift this import command takes the form
`podman image load -i intel-gpu-fakedev.tar` to achieve the same thing.

Apply the following yaml with `kubectl apply -f ...` to install fake Device
Plugin resources into the cluster. The 'IfNotPresent' image pull policy prevents
the pull attempt over the netowrk as long as the images are found locally.

```
apiVersion: v1
data:
  fakedev-config.json: "{\n\t\"Info\": \"8x 4 GiB DG1 [Iris Xe MAX Graphics] GPUs\",\n\t\"DevCount\":
    8,\n\t\"DevMemSize\": 4294967296,\n\t\"Capabilities\": {\n\t\t\"platform\": \"fake_DG1\"\n\t}\n}\n"
kind: ConfigMap
metadata:
  name: intel-gpu-fakedev-config
---
apiVersion: apps/v1
kind: DaemonSet
metadata:
  labels:
    app: intel-gpu-plugin
  name: intel-gpu-plugin
spec:
  selector:
    matchLabels:
      app: intel-gpu-plugin
  template:
    metadata:
      labels:
        app: intel-gpu-plugin
    spec:
      containers:
      - args:
        - -v=4
        - -prefix=/tmp
        env:
        - name: NODE_NAME
          valueFrom:
            fieldRef:
              fieldPath: spec.nodeName
        - name: HOST_IP
          valueFrom:
            fieldRef:
              fieldPath: status.hostIP
        image: registry.local/intel-gpu-plugin:devel
        imagePullPolicy: IfNotPresent
        name: intel-gpu-plugin
        resources:
          limits:
            cpu: 100m
            memory: 90Mi
          requests:
            cpu: 40m
            memory: 45Mi
        securityContext:
          allowPrivilegeEscalation: false
          capabilities:
            drop:
            - ALL
          readOnlyRootFilesystem: true
          seLinuxOptions:
            type: container_device_plugin_t
          seccompProfile:
            type: RuntimeDefault
        volumeMounts:
        - mountPath: /var/lib/kubelet/device-plugins
          name: kubeletsockets
        - mountPath: /var/run/cdi
          name: cdipath
        - mountPath: /tmp/dev
          name: devfs
        - mountPath: /tmp/sys
          name: sysfsdrm
      initContainers:
      - args:
        - -json
        - /config/fakedev-config.json
        image: registry.local/intel-gpu-fakedev:devel
        imagePullPolicy: IfNotPresent
        name: intel-gpu-initcontainer
        volumeMounts:
        - mountPath: /tmp/dev
          name: devfs
        - mountPath: /tmp/sys
          name: sysfsdrm
        - mountPath: /config/
          name: fakedevconfig
        workingDir: /tmp/
      nodeSelector:
        kubernetes.io/arch: amd64
      volumes:
      - hostPath:
          path: /var/lib/kubelet/device-plugins
        name: kubeletsockets
      - hostPath:
          path: /var/run/cdi
          type: DirectoryOrCreate
        name: cdipath
      - emptyDir: {}
        name: devfs
      - emptyDir: {}
        name: sysfsdrm
      - configMap:
          defaultMode: 288
          name: intel-gpu-fakedev-config
          optional: false
        name: fakedevconfig
  updateStrategy:
    rollingUpdate:
      maxSurge: 0
      maxUnavailable: 1
    type: RollingUpdate
```

## Setting up fake devices for DRA

DRA fake devices need the 'intel-device-faker' image to be built. The
'intel-gpu-resource-driver' image is already available for download.

```
git clone https://github.com/intel/intel-resource-drivers-for-kubernetes.git
cd intel-resource-drivers-for-kubernetes
make device-faker-container-build
```

Tag and export the image, upload it to cluster nodes or cluster registry.

```
docker image tag registry.local/intel-device-faker:v0.5.0 registry.local/intel-device-faker:devel
docker save registry.local/intel-device-faker:devel -o intel-device-faker.tar
```

To import the image on nodes running containerd do
```
ctr n=k8s.io image import intel-device-faker.tar
```
Do notice that in OpenShift this import command takes the form
`podman image load -i intel-device-faker.tar` to achieve the same thing.

Apply the following yaml with `kubectl apply -f ...` to install fake DRA
resource slices into the cluster. The `IfNotPresent` pull policy prevents
downloads over the network when the images exist locally.

```
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: intel-gpu-resource-driver-kubelet-plugin
  namespace: intel-gpu-resource-driver
spec:
  template:
    spec:
      initContainers:
      - name: device-faker
        # 'Always' policy makes it a sideCar container with longer lifecycle,
        # allowing it to be terminated last
        # https://kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle/#termination-with-sidecars
        # which allows proper fake root cleanup.
        restartPolicy: Always
        # TODO: switch to CI image when public CI is in place
        image: registry.local/intel-device-faker:v0.5.0
        imagePullPolicy: IfNotPresent
        command: ["/device-faker", "gpu", "-t", "/opt/templates/gpu-template.json", "-r", "-d", "/tmp/gpu-fake-root", "-c", "-p"]
        volumeMounts:
        - name: gpu-fake-root
          mountPath: /tmp/gpu-fake-root
        securityContext:
          readOnlyRootFilesystem: false
          allowPrivilegeEscalation: false
          capabilities:
            drop: [ "ALL" ]
            add:  [ "MKNOD" ]
      containers:
      - name: kubelet-plugin
        command: ["/kubelet-gpu-plugin", "-v", "5"]
        # TODO: change to :devel when public CI is in place
        image: ghcr.io/intel/intel-resource-drivers-for-kubernetes/intel-gpu-resource-driver:devel
        # TODO: pull policy is needed when :devel is used instead of :latest
        imagePullPolicy: Always
        env:
        - name: SYSFS_ROOT
          value: "/tmp/gpu-fake-root/sysfs"
        - name: DEVFS_ROOT
          value: "/tmp/gpu-fake-root/dev"
        # Host dir for system's dynamic CDI dir. Containerd & CRI-O default value.
        - name: CDI_ROOT
          value: "/var/run/cdi"
        volumeMounts:
        - name: gpu-fake-root
          mountPath: /tmp/gpu-fake-root/sysfs
          subPath: sysfs
        - name: gpu-fake-root
          mountPath: /tmp/gpu-fake-root/dev
          subPath: dev
        # Host dir for system's dynamic CDI dir. Containerd & CRI-O default value.
        - name: dynamic-cdi
          mountPath: /var/run/cdi
      volumes:
      - name: gpu-fake-root
        hostPath:
          path: /tmp/gpu-fake-root
      # Host dir for system's dynamic CDI dir. Containerd & CRI-O default value.
      - name: dynamic-cdi
        hostPath:
          path: /var/run/cdi
```