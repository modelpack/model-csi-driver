# Getting Started with Model CSI Driver

Model CSI Driver is a Kubernetes CSI driver for serving OCI model artifacts packaged according to the [Model Spec](https://github.com/modelpack/model-spec). It enables model delivery through CSI volumes and supports both direct image pulls and P2P-accelerated distribution.

## Overview

Model CSI Driver is designed for clusters that need to mount model artifacts into pods without building model data into application images.

Key capabilities:

- Mount OCI model artifacts as CSI volumes
- Support a simple static inline mount flow for direct consumption
- Support a dynamic in-pod mount flow through a local Unix domain socket API
- Integrate with P2P distribution for large model delivery

## Prerequisites

Prepare the following before deployment:

- A Kubernetes cluster with kubectl access
- Helm 3.x
- Access to an OCI registry that stores model artifacts

To build and push a model artifact, follow the `modctl` guide at https://github.com/modelpack/modctl/blob/main/docs/getting-started.md.

## Installation

Install the driver with Helm. The example below keeps only the configuration that is typically customized in real deployments.

```yaml
# values-custom.yaml
config:
  serviceName: model.csi.modelpack.org
  rootDir: /var/lib/model-csi
  dynamicCsiEndpoint: unix:///var/run/model-csi/csi.sock
  metricsAddr: tcp://127.0.0.1:5244
  registryAuths:
    registry.example.com:
      auth: dXNlcm5hbWU6cGFzc3dvcmQ=
      serverscheme: https

image:
  repository: ghcr.io/modelpack/model-csi-driver
  pullPolicy: IfNotPresent
  tag: latest
```

Notes:

- **serviceName** must stay aligned with the CSI driver name used in pod specs unless you intentionally deploy a custom name.
- **rootDir** must be writable on every node and have enough local disk capacity for pulled model data.
- **dynamicCsiEndpoint** keeps the legacy shared dynamic CSI socket available for backward compatibility.
- **metricsAddr** controls the Prometheus metrics listener.
- **registryAuths** uses base64-encoded username:password values.

Deploy the chart:

```bash
helm upgrade --install model-csi-driver \
  oci://ghcr.io/modelpack/charts/model-csi-driver \
  --namespace model-csi \
  --create-namespace \
  -f values-custom.yaml
```

Verify the daemonset:

```bash
kubectl get pods -n model-csi
```

## Static Inline Mount

Use a static inline mount when the model reference is known at pod creation time. The model is pulled and mounted during pod startup, and the local data is reclaimed when the pod is removed.

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: model-inference-pod
spec:
  containers:
    - name: inference-server
      image: ubuntu:24.04
      command: ["sleep", "infinity"]
      volumeMounts:
        - name: model-volume
          mountPath: /home/admin/model
          readOnly: true
  volumes:
    - name: model-volume
      csi:
        driver: model.csi.modelpack.org
        volumeAttributes:
          model.csi.modelpack.org/type: image
          model.csi.modelpack.org/reference: example.com/model/llama:v1.0.0
```

Use this mode for the simplest deployment path, keep in mind that kubelet applies a mount timeout of about 2 minutes for inline volumes, so for large models, combine the driver with a P2P cache service (for example [Dragonfly](https://github.com/dragonflyoss/dragonfly)) to avoid startup failures, we'll provide detailed information about integrating Dragonfly later.

## Dynamic Inline Mount

Use a dynamic inline mount when the pod should decide at runtime which model to mount. In this mode, the CSI volume only exposes the driver working directory. Models are then mounted and unmounted inside the pod through the local Unix domain socket API.

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: dynamic-model-pod
spec:
  containers:
    - name: main
      image: ubuntu:24.04
      command: ["sleep", "infinity"]
      volumeMounts:
        - name: model-volume
          mountPath: /home/admin/model-csi
  volumes:
    - name: model-volume
      csi:
        driver: model.csi.modelpack.org
```

After the pod starts, use the mounted directory as the root for local model operations.

## UDS HTTP API

The dynamic mount flow is managed through a REST-style HTTP API exposed on a Unix domain socket.

### Discover the socket path

If the CSI root is mounted at volume_dir, the socket path is:

```text
unix://$volume_dir/csi/csi.sock
```

### Discover the volume name

```bash
volume_name=$(jq -r .volume_name "$volume_dir/status.json")
```

### Response semantics

- 2xx indicates success
- 4xx indicates an invalid client request
- 5xx indicates an internal server failure

Error responses use the following shape:

```json
{
  "code": "INVALID_ARGUMENT",
  "message": "..."
}
```

During daemonset rollout or restart, the socket file may be recreated. Clients should retry when the socket path does not exist or when the request fails with connection refused.

### Create a model mount

```bash
curl --unix-socket "$volume_dir/csi/csi.sock" \
  -H "Content-Type: application/json" \
  -X POST http://localhost/api/v1/volumes/$volume_name/mounts \
  -d '{
    "mount_id": "demo-mount",
    "reference": "example.com/model/llama:v1.0.0"
  }'
```

The same request can include file filtering parameters when the pod only needs part of the model contents:

```bash
curl --unix-socket "$volume_dir/csi/csi.sock" \
  -H "Content-Type: application/json" \
  -X POST http://localhost/api/v1/volumes/$volume_name/mounts \
  -d '{
    "mount_id": "bootstrap-only",
    "reference": "example.com/model/llama:v1.0.0",
    "exclude_file_patterns": [
      "model.safetensors.index.json",
      "!tiktoken.model"
    ]
  }'
```

Example response:

```json
{
  "volume_name": "csi-xxx",
  "mount_id": "demo-mount",
  "reference": "example.com/model/llama:v1.0.0",
  "state": "PULL_SUCCEEDED"
}
```

Notes:

- mount_id may contain letters, numbers, underscores, and hyphens
- The mounted model becomes available at $volume_dir/models/$mount_id/model
- This is a synchronous operation that pulls and mounts the model before returning
- For large models, use a sufficiently large HTTP client timeout

### Get a model mount

```bash
curl --unix-socket "$volume_dir/csi/csi.sock" \
  -X GET http://localhost/api/v1/volumes/$volume_name/mounts/$mount_id
```

Example response:

```json
{
  "volume_name": "csi-xxx",
  "mount_id": "demo-mount",
  "reference": "example.com/model/llama:v1.0.0",
  "state": "PULLING",
  "progress": {
    "total": 5,
    "items": [
      {
        "digest": "sha256:0c75d49a2c25846123b238a2e7bfa2d78f6b3d62069f3ce68364e3024d1a76da",
        "path": "/tokenizer.json",
        "size": 7849472,
        "started_at": "2025-06-10T20:19:12.797873473+08:00",
        "finished_at": "2025-06-10T20:19:15.046158731+08:00"
      },
      {
        "digest": "sha256:70c80fe937f84ce03629c7b397038a1566cac5aeabad92b5344384aa8f13f44c",
        "path": "/configuration.json",
        "size": 2048,
        "started_at": "2025-06-10T20:19:12.79806982+08:00"
      }
    ]
  }
}
```

Possible state values are `PULLING`, `PULL_SUCCEEDED`, and `PULL_FAILED`.

### List model mounts

```bash
curl --unix-socket "$volume_dir/csi/csi.sock" \
  -X GET http://localhost/api/v1/volumes/$volume_name/mounts
```

### Delete a model mount

```bash
curl --unix-socket "$volume_dir/csi/csi.sock" \
  -X DELETE http://localhost/api/v1/volumes/$volume_name/mounts/$mount_id
```

## Supported Volume Attributes

The driver recognizes the following CSI volume attributes:

| Attribute | Required | Description |
| --- | --- | --- |
| model.csi.modelpack.org/reference | Yes for static inline mounts | OCI reference of the model artifact to mount. |
| model.csi.modelpack.org/exclude-file-patterns | No | JSON array of path patterns to exclude during static inline mounts. |

For dynamic inline mounts, the pod-level CSI volume typically omits these attributes. The model reference is supplied later through the UDS API.

## Troubleshooting

### Pod stays in Pending or ContainerCreating

```bash
kubectl describe pod <pod-name>
kubectl logs -n model-csi -c model-csi-driver <model-csi-driver-pod>
```
