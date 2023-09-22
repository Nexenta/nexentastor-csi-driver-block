# NexentaStor CSI Driver over iSCSI
[![Conventional Commits](https://img.shields.io/badge/Conventional%20Commits-1.0.0-yellow.svg)](https://conventionalcommits.org)

NexentaStor product page: [https://nexenta.com/products/nexentastor](https://nexenta.com/products/nexentastor).

This is a **development branch**, for the most recent stable version see "Supported versions".

## Overview
The NexentaStor Container Storage Interface (CSI) Driver provides a CSI interface used by Container Orchestrators (CO) to manage the lifecycle of NexentaStor volumes over iSCSI protocol.

## Supported kubernetes versions matrix

|                   | NexentaStor 5.3+|
|-------------------|----------------|
| Kubernetes >=1.17 | [1.0.0](https://github.com/Nexenta/nexentastor-csi-driver-block/tree/1.0.0) |
| Kubernetes >=1.17 | [1.1.0](https://github.com/Nexenta/nexentastor-csi-driver-block/tree/1.1.0) |
| Kubernetes >=1.17 | [1.2.0](https://github.com/Nexenta/nexentastor-csi-driver-block/tree/1.2.0) |
| Kubernetes >=1.19 | [1.3.0](https://github.com/Nexenta/nexentastor-csi-driver-block/tree/1.3.0) |
| Kubernetes >=1.19 | [master](https://github.com/Nexenta/nexentastor-csi-driver-block) |

Releases can be found here - https://github.com/Nexenta/nexentastor-csi-driver-block/releases

## Feature List
|Feature|Feature Status|CSI Driver Version|CSI Spec Version|Kubernetes Version|
|--- |--- |--- |--- |--- |
|Static Provisioning|GA|>= v1.0.0|>= v1.0.0|>=1.13|
|Dynamic Provisioning|GA|>= v1.0.0|>= v1.0.0|>=1.13|
|RW mode|GA|>= v1.0.0|>= v1.0.0|>=1.13|
|RO mode|GA|>= v1.0.0|>= v1.0.0|>=1.13|
|Creating and deleting snapshot|GA|>= v1.0.0|>= v1.0.0|>=1.17|
|Provision volume from snapshot|GA|>= v1.0.0|>= v1.0.0|>=1.17|
|Provision volume from another volume|GA|>= v1.1.0|>= v1.0.0|>=1.17|
|List snapshots of a volume|Beta|>= v1.0.0|>= v1.0.0|>=1.17|
|Expand volume|GA|>= v1.1.0|>= v1.1.0|>=1.16|
|Topology|Beta|>= v1.1.0|>= v1.0.0|>=1.17|
|Raw block device|GA|>= v1.0.0|>= v1.0.0|>=1.14|
|StorageClass Secrets|Beta|>= v1.0.0|>=1.0.0|>=1.13|


## Requirements

- Kubernetes cluster must allow privileged pods, this flag must be set for the API server and the kubelet
  ([instructions](https://github.com/kubernetes-csi/docs/blob/735f1ef4adfcb157afce47c64d750b71012c8151/book/src/Setup.md#enable-privileged-pods)):
  ```
  --allow-privileged=true
  ```
- Required the API server and the kubelet feature gates
  ([instructions](https://github.com/kubernetes-csi/docs/blob/735f1ef4adfcb157afce47c64d750b71012c8151/book/src/Setup.md#enabling-features)):
  ```
  --feature-gates=VolumeSnapshotDataSource=true,VolumePVCDataSource=true,ExpandInUsePersistentVolumes=true,ExpandCSIVolumes=true,ExpandPersistentVolumes=true,Topology=true,CSINodeInfo=true
  ```
  If you are planning on using topology, the following feature-gates are required
  ```
  ServiceTopology=true,CSINodeInfo=true
  ```
- Mount propagation must be enabled, the Docker daemon for the cluster must allow shared mounts
  ([instructions](https://github.com/kubernetes-csi/docs/blob/735f1ef4adfcb157afce47c64d750b71012c8151/book/src/Setup.md#enabling-mount-propagation))
  ```bash
  apt install -y open-iscsi
  ```

## Installation

1. Create NexentaStor volumeGroup for the driver, example: `csiDriverPool/csiDriverVolumeGroup`.
   By default, the driver will create filesystems in this volumeGroup and mount them to use as Kubernetes volumes.
2. Clone driver repository
   ```bash
   git clone https://github.com/Nexenta/nexentastor-csi-driver-block.git
   cd nexentastor-csi-driver-block
   git checkout master
   ```
3. Edit `deploy/kubernetes/nexentastor-csi-driver-block-config.yaml` file. Driver configuration example:
   ```yaml
   nexentastor_map:
     nstor-box1:
       restIp: https://10.3.199.252:8443,https://10.3.199.253:8443 # [required] NexentaStor REST API endpoint(s)
       username: admin                                             # [required] NexentaStor REST API username
       password: Nexenta@1                                         # [required] NexentaStor REST API password
       defaultDataIp: 10.3.1.1                                     # default NexentaStor data IP or HA VIP
       defaultVolumeGroup: csiDriverPool/csiVolumeGroup            # default volume group for driver's volumes [pool/volumeGroup]
       defaultTargetGroup: tg1                                     # [required if dynamicTargetLunAllocation = false] NexentaStor iSCSI target group name
       defaultTarget: iqn.2005-07.com.nexenta:01:test              # [required if dynamicTargetLunAllocation = false] NexentaStor iSCSI target
       defaultHostGroup: all                                       # [required] NexentaStor host group

     nstor-slow:
       restIp: https://10.3.4.4:8443,https://10.3.4.5:8443         # [required] NexentaStor REST API endpoint(s)
       username: admin                                             # [required] NexentaStor REST API username
       password: Nexenta@1                                         # [required] NexentaStor REST API password
       defaultDataIp: 10.3.1.2                                     # default NexentaStor data IP or HA VIP
       defaultVolumeGroup: csiDriverPool/csiVolumeGroup2           # default volume group for driver's volumes [pool/volumeGroup]
       defaultTargetGroup: tg1                                     # [required] NexentaStor iSCSI target group name
       defaultTarget: iqn.2005-07.com.nexenta:01:test              # [required] NexentaStor iSCSI target
       defaultHostGroup: all                                       # [required] NexentaStor host group

   ```
   **Note**: keyword nexentastor_map followed by cluster name of your choice MUST be used even if you are only using 1 NexentaStor cluster.

## Configuration options

   | Name                  | Description                                                     | Required   | Example                                                      |
   |-----------------------|-----------------------------------------------------------------|------------|--------------------------------------------------------------|
   | `restIp`              | NexentaStor REST API endpoint(s); `,` to separate cluster nodes | yes        | `https://10.3.3.4:8443`                                      |
   | `username`            | NexentaStor REST API username                                   | yes        | `admin`                                                      |
   | `password`            | NexentaStor REST API password                                   | yes        | `p@ssword`                                                   |
   | `defaultVolumeGroup`  | parent volumeGroup for driver's filesystemes [pool/volumeGroup] | yes        | `csiDriverPool/csiDriverVolumeGroup`                             |
   | `defaultHostGroup`    | NexentaStor host group to map volumes                           | no         | `all`   |
   | `mountPointPermissions` | Permissions to be set on volume's mount point | no            | `0750`     |
   | `defaultTarget`       | NexentaStor iSCSI target iqn                                    | yes if dynamicTargetLunAllocation = false | `iqn.2005-07.com.nexenta:01:csiTarget1`|
   | `defaultTargetGroup`  | NexentaStor target group name                                   | yes if dynamicTargetLunAllocation = false | `CSI-tg1`   |
   | `sparseVolume`         | Defines whether sparse(thin provisioning) should be used. Default `true` | no       | `true`   |
   | `defaultDataIp`       | NexentaStor data IP or HA VIP for mounting shares               | yes for PV | `20.20.20.21`                                                |
   | `dynamicTargetLunAllocation` | If true driver will automatically manage iSCSI target and targetgroup creation. Config values for target and group will be ignored if dynamicTargetLunAllocation = true | yes         | `true` |
   | `numOfLunsPerTarget`  | Maximum number of luns that can be assigned to each target with dynamicTargetLunAllocation | no         | `256`                                                       |
   | `useChapAuth`         | CHAP authentication for iSCSI targets         | no             | `true`     |
   | `chapUser`            | Username for CHAP authentication                        | no             | `admin`    |
   | `chapSecret`          | Password/secret for CHAP authentication. Minimun length is 12 symbols   | yes when useChapSecret is `true` | `verysecretpassword`                                                       |
   | `debug`               | print more logs (default: false)                                | no         | `true`                                                       |
   | `zone`                | Zone to match topology.kubernetes.io/zone.                      | no         | `us-west`                                                       |
   |`insecureSkipVerify`| TLS certificates check will be skipped when `true` (default: 'true')| no | `false` |
   |`iSCSITimeout`| Maximum time for iSCSI device discovery (default: '300')| no | `200` |

   **Note**: if parameter `defaultVolumeGroup`/`defaultDataIp` is not specified in driver configuration,
   then parameter `volumeGroup`/`dataIp` must be specified in _StorageClass_ configuration.

   **Note**: all default parameters (`default*`) may be overwritten in specific _StorageClass_ configuration.

4. Create Kubernetes secret from the file:
   ```bash
   kubectl create secret generic nexentastor-csi-driver-block-config --from-file=deploy/kubernetes/nexentastor-csi-driver-block-config.yaml
   ```
5. Register driver to Kubernetes:
   ```bash
   kubectl apply -f deploy/kubernetes/nexentastor-csi-driver-block.yaml
   ```

6. For snapshotting capabilities additional CRDs must be installed once per cluster and external-snapshotter deployed:
  ``` bash
   kubectl apply -f deploy/kubernetes/snapshots/crds.yaml
   kubectl apply -f deploy/kubernetes/snapshots/snapshotter.yaml
  ```

Configuring multiple controller volume replicas
We can configure this by changing the deploy/kubernetes/nexentastor-csi-driver-block.yaml:

change the following line in controller service config
```
kind: StatefulSet
apiVersion: apps/v1
metadata:
name: nexentastor-block-csi-controller
spec:
serviceName: nexentastor-block-csi-controller-service
replicas: 1  # Change this to 2 or more.
```

NexentaStor CSI driver's pods should be running after installation:

```bash
$ kubectl get pods
nexentastor-block-csi-controller-0   4/4     Running   0          23h
nexentastor-block-csi-controller-1   4/4     Running   0          23h
nexentastor-block-csi-node-6cmsj     2/2     Running   0          23h
nexentastor-block-csi-node-wcrgk     2/2     Running   0          23h
nexentastor-block-csi-node-xtmgv     2/2     Running   0          23h
```

## Storage class parameters
Storage classes provide the capability to define parameters per storageClass instead of using config values.
This is very useful to provide flexibility while using the same driver.
For example, we can use one storageClass to create thin provisioned and another for thick. Or use different iSCSI targets or volume groups.
A couple of possible use cases:

provide iSCSI target and group, thin provissioning (sparseVolume)
```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: nexentastor-block-csi-driver-static-target-tg
provisioner: nexentastor-block-csi-driver.nexenta.com
allowVolumeExpansion: true
parameters:
  dynamicTargetLunAllocation: false
  target: iqn.2005-07.com.nexenta:01:csiTarget1
  targetGroup: CSI-tg1
  sparseVolume: true
``` 

let the driver take care of targets and groups, thick provisioning
```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: nexentastor-block-csi-driver-dynamicTargetLunAllocation
provisioner: nexentastor-block-csi-driver.nexenta.com
allowVolumeExpansion: true
parameters:
  dynamicTargetLunAllocation: true
  numOfLunsPerTarget: "12"
  iSCSITargetPrefix: csi-
  sparseVolume: false
``` 

List of all valid parameters:
- volumeGroup
- configName
- sparseVolume
- dataIP
- target
- hostGroup
- iSCSIPort
- targetGroup
- iSCSITargetPrefix
- dynamicTargetLunAllocation
- numOfLunsPerTarget
- useChapAuth
- chapUser
- chapSecret
- mountPointPermissions


## Usage

### Dynamically provisioned volumes

For dynamic volume provisioning, the administrator needs to set up a _StorageClass_ pointing to the driver.
In this case Kubernetes generates volume name automatically (for example `pvc-ns-cfc67950-fe3c-11e8-a3ca-005056b857f8`).
Default driver configuration may be overwritten in `parameters` section:

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: nexentastor-csi-driver-block-sc-nginx-dynamic
provisioner: nexentastor-block-csi-driver.nexenta.com
mountOptions:                        # list of options for `mount -o ...` command
#  - noatime                         #
#- matchLabelExpressions:            # use to following lines to configure topology by zones
#  - key: topology.kubernetes.io/zone
#    values:
#    - us-east
parameters:
  #configName: nstor-slow            # specify exact NexentaStor appliance that you want to use to provision volumes.
  #volumeGroup: customPool/customvolumeGroup # to overwrite "defaultVolumeGroup" config property [pool/volumeGroup]
  #dataIp: 20.20.20.253              # to overwrite "defaultDataIp" config property
```

#### Parameters

| Name           | Description                                            | Example                                               |
|----------------|--------------------------------------------------------|-------------------------------------------------------|
| `volumeGroup`      | parent volumeGroup for driver's filesystems [pool/volumeGroup] | `customPool/customvolumeGroup`                            |
| `dataIp`       | NexentaStor data IP or HA VIP for mounting shares      | `20.20.20.253`                                        |
| `configName`   | name of NexentaStor appliance from config file         | `nstor-ssd`                                        |

#### Example

Run Nginx pod with dynamically provisioned volume:

```bash
kubectl apply -f examples/kubernetes/nginx-dynamic-volume.yaml

# to delete this pod:
kubectl delete -f examples/kubernetes/nginx-dynamic-volume.yaml
```

### Pre-provisioned volumes

The driver can use already existing NexentaStor filesystem,
in this case, _StorageClass_, _PersistentVolume_ and _PersistentVolumeClaim_ should be configured.

#### _StorageClass_ configuration

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: nexentastor-csi-driver-block-sc-nginx-persistent
provisioner: nexentastor-block-csi-driver.nexenta.com
mountOptions:                        # list of options for `mount -o ...` command
#  - noatime                         #
parameters:
  #volumeGroup: customPool/customvolumeGroup # to overwrite "defaultVolumeGroup" config property [pool/volumeGroup]
  #dataIp: 20.20.20.253              # to overwrite "defaultDataIp" config property
```

#### _PersistentVolume_ configuration

```yaml
apiVersion: v1
kind: PersistentVolume
metadata:
  name: nexentastor-csi-driver-block-pv-nginx-persistent
  labels:
    name: nexentastor-csi-driver-block-pv-nginx-persistent
spec:
  storageClassName: nexentastor-csi-driver-block-sc-nginx-persistent
  accessModes:
    - ReadWriteMany
  capacity:
    storage: 1Gi
  csi:
    driver: nexentastor-csi-driver.nexenta.com
    volumeHandle: nstor-ssd:csiDriverPool/csiDriverVolumeGroup/nginx-persistent
  #mountOptions:  # list of options for `mount` command
  #  - noatime    #
```

CSI Parameters:

| Name           | Description                                                       | Example                              |
|----------------|-------------------------------------------------------------------|--------------------------------------|
| `driver`       | installed driver name "nexentastor-csi-driver.nexenta.com"        | `nexentastor-csi-driver.nexenta.com` |
| `volumeHandle` | NS appliance name from config and path to existing NexentaStor filesystem [configName:pool/volumeGroup/filesystem] | `nstor-ssd:PoolA/volumeGroupA/nginx`               |

#### _PersistentVolumeClaim_ (pointed to created _PersistentVolume_)

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: nexentastor-csi-driver-block-pvc-nginx-persistent
spec:
  storageClassName: nexentastor-csi-driver-block-sc-nginx-persistent
  accessModes:
    - ReadWriteMany
  resources:
    requests:
      storage: 1Gi
  selector:
    matchLabels:
      # to create 1-1 relationship for pod - persistent volume use unique labels
      name: nexentastor-csi-driver-block-sc-nginx-persistent
```

#### Example

Run nginx server using PersistentVolume.

**Note:** Pre-configured filesystem should exist on the NexentaStor:
`csiDriverPool/csiDriverVolumeGroup/nginx-persistent`.

```bash
kubectl apply -f examples/kubernetes/nginx-persistent-volume.yaml

# to delete this pod:
kubectl delete -f examples/kubernetes/nginx-persistent-volume.yaml
```

### Cloned volumes

We can create a clone of an existing csi volume.
To do so, we need to create a _PersistentVolumeClaim_ with _dataSource_ spec pointing to an existing PVC that we want to clone.
In this case Kubernetes generates volume name automatically (for example `pvc-ns-cfc67950-fe3c-11e8-a3ca-005056b857f8`).

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: nexentastor-csi-driver-block-pvc-nginx-dynamic-clone
spec:
  storageClassName: nexentastor-csi-driver-block-sc-nginx-dynamic
  dataSource:
    kind: PersistentVolumeClaim
    apiGroup: ""
    name: nexentastor-csi-driver-block-sc-nginx-dynamic # pvc name
  accessModes:
    - ReadWriteMany
  resources:
    requests:
      storage: 1Gi
```

#### Example

Run Nginx pod with dynamically provisioned volume:

```bash
kubectl apply -f examples/kubernetes/nginx-clone-volume.yaml

# to delete this pod:
kubectl delete -f examples/kubernetes/nginx-clone-volume.yaml
```

## Snapshots

**Note**: this feature is an
[alpha feature](https://kubernetes-csi.github.io/docs/snapshot-restore-feature.html#status).

```bash
# create snapshot class
kubectl apply -f examples/kubernetes/snapshot-class.yaml

# take a snapshot
kubectl apply -f examples/kubernetes/take-snapshot.yaml

# deploy nginx pod with volume restored from a snapshot
kubectl apply -f examples/kubernetes/nginx-snapshot-volume.yaml

# snapshot classes
kubectl get volumesnapshotclasses.snapshot.storage.k8s.io

# snapshot list
kubectl get volumesnapshots.snapshot.storage.k8s.io

# snapshot content list
kubectl get volumesnapshotcontents.snapshot.storage.k8s.io
```

## CHAP authentication

To use iSCSI CHAP authentication, configure your iSCSI client's initiator username and password on each kubernetes node. Example for Ubuntu 18.04:
- Open iscsid config and set username(optional) and password for session auth (note that minimum lentgh for password is 12):
```bash
vi /etc/iscsi/iscsid.conf

node.session.auth.username = admin
node.session.auth.password = supersecretpassword
systemctl restart iscsid.service
```

Now that your client is configured, add according values to driver's config or storageClass (see examples/nginx-dynamic-volume-chap):
```bash
    useChapAuth: true
    chapUser: admin
    chapSecret: supersecretpassword
```

## Checking TLS certificates
Default driver behavior is to skip certificate checks for all Rest API calls.
v1.4.4 Release introduces new config parameter `insecureSkipVerify`=<true>.
When `InsecureSkipVerify` is set to false, the driver will enforce certificate checking.
To allow adding certificates, nexentastor-csi-driver-block.yaml has additional volumes added to nexentastor-block-csi-controller deployment and nexentastor-block-csi-node daemonset.
```bash
            - name: certs-dir
              mountPropagation: HostToContainer
              mountPath: /usr/local/share/ca-certificates
        - name: certs-dir
          hostPath:
            path: /etc/ssl/  # change this to your tls certificates folder
            type: Directory
```
`/etc/ssl` folder is the default certificates location for Ubuntu. Change this according to your
OS configuration.
If you only want to propagate a specific set of certificates instead of the whole cert folder 
from the host, you can put them in any folder on the host and set in the yaml file accordingly.
Note that this should be done on every node of the kubernetes cluster.

## Uninstall

Using the same files as for installation:

```bash
# delete driver
kubectl delete -f deploy/kubernetes/nexentastor-csi-driver-block.yaml

# delete secret
kubectl delete secret nexentastor-csi-driver-block-config
```

## Troubleshooting

- Show installed drivers:
  ```bash
  kubectl get csidrivers
  kubectl describe csidrivers
  ```
- Error:
  ```
  MountVolume.MountDevice failed for volume "pvc-ns-<...>" :
  driver name nexentastor-csi-driver.nexenta.com not found in the list of registered CSI drivers
  ```
  Make sure _kubelet_ configured with `--root-dir=/var/lib/kubelet`, otherwise update paths in the driver yaml file
  ([all requirements](https://github.com/kubernetes-csi/docs/blob/387dce893e59c1fcf3f4192cbea254440b6f0f07/book/src/Setup.md#enabling-features)).
- "VolumeSnapshotDataSource" feature gate is disabled:
  ```bash
  vim /var/lib/kubelet/config.yaml
  # ```
  # featureGates:
  #   VolumeSnapshotDataSource: true
  # ```
  vim /etc/kubernetes/manifests/kube-apiserver.yaml
  # ```
  #     - --feature-gates=VolumeSnapshotDataSource=true
  # ```
  ```
- Driver logs
  ```bash
  kubectl logs --all-containers $(kubectl get pods | grep nexentastor-block-csi-controller | awk '{print $1}') -f
  kubectl logs --all-containers $(kubectl get pods | grep nexentastor-block-csi-node | awk '{print $1}') -f
  ```
- Show termination message in case driver failed to run:
  ```bash
  kubectl get pod nexentastor-csi-block-controller-0 -o go-template="{{range .status.containerStatuses}}{{.lastState.terminated.message}}{{end}}"
  ```
- Configure Docker to trust insecure registries:
  ```bash
  # add `{"insecure-registries":["10.3.199.92:5000"]}` to:
  vim /etc/docker/daemon.json
  service docker restart
  ```

## Development

Commits should follow [Conventional Commits Spec](https://conventionalcommits.org).
Commit messages which include `feat:` and `fix:` prefixes will be included in CHANGELOG automatically.

### Build

```bash
# print variables and help
make

# build go app on local machine
make build

# build container (+ using build container)
make container-build

# update deps
~/go/bin/dep ensure
```

### Run

Without installation to k8s cluster only version command works:

```bash
./bin/nexentastor-csi-driver-block --version
```

### Publish

```bash
# push the latest built container to the local registry (see `Makefile`)
make container-push-local

# push the latest built container to hub.docker.com
make container-push-remote
```

### Tests

`test-all-*` instructions run:
- unit tests
- CSI sanity tests from https://github.com/kubernetes-csi/csi-test
- End-to-end driver tests with real K8s and NS appliances.

See [Makefile](Makefile) for more examples.

```bash
# Test options to be set before run tests:
# - NOCOLORS=true            # to run w/o colors
# - TEST_K8S_IP=10.3.199.250 # e2e k8s tests

# run all tests using local registry (`REGISTRY_LOCAL` in `Makefile`)
TEST_K8S_IP=10.3.199.250 make test-all-local-image
# run all tests using hub.docker.com registry (`REGISTRY` in `Makefile`)
TEST_K8S_IP=10.3.199.250 make test-all-remote-image

# run tests in container:
# - RSA keys from host's ~/.ssh directory will be used by container.
#   Make sure all remote hosts used in tests have host's RSA key added as trusted
#   (ssh-copy-id -i ~/.ssh/id_rsa.pub user@host)
#
# run all tests using local registry (`REGISTRY_LOCAL` in `Makefile`)
TEST_K8S_IP=10.3.199.250 make test-all-local-image-container
# run all tests using hub.docker.com registry (`REGISTRY` in `Makefile`)
TEST_K8S_IP=10.3.199.250 make test-all-remote-image-container
```

End-to-end K8s test parameters:

```bash
# Tests install driver to k8s and run nginx pod with mounted volume
# "export NOCOLORS=true" to run w/o colors
go test tests/e2e/driver_test.go -v -count 1 \
    --k8sConnectionString="root@10.3.199.250" \
    --k8sDeploymentFile="../../deploy/kubernetes/nexentastor-csi-driver-block.yaml" \
    --k8sSecretFile="./_configs/driver-config-single-default.yaml"
```

All development happens in `master` branch,
when it's time to publish a new version,
new git tag should be created.

1. Build and test the new version using local registry:
   ```bash
   # build development version:
   make container-build
   # publish to local registry
   make container-push-local
   # test plugin using local registry
   TEST_K8S_IP=10.3.199.250 make test-all-local-image-container
   ```

2. To release a new version run command:
   ```bash
   VERSION=X.X.X make release
   ```
   This script does following:
   - generates new `CHANGELOG.md`
   - builds driver container 'nexentastor-csi-driver-block'
   - Login to hub.docker.com will be requested
   - publishes driver version 'nexenta/nexentastor-csi-driver-block:X.X.X' to hub.docker.com
   - creates new Git tag 'vX.X.X' and pushes to the repository.

3. Update Github [releases](https://github.com/Nexenta/nexentastor-csi-driver-block/releases).
