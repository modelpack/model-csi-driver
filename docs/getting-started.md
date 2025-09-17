# Getting Started with Model CSI Driver

Model CSI Driver is a Kubernetes CSI driver for serving OCI model artifacts, which are bundled based on [Model Spec](https://github.com/modelpack/model-spec). This guide will help you deploy and use the Model CSI Driver in your Kubernetes cluster.

## Overview

The Model CSI Driver simplifies and accelerates model deployment in Kubernetes by:

- Seamlessly mount model artifacts as volumes into pod
- Compatible with older Kubernetes versions
- Natively supports P2P-accelerated distribution

## Prerequisites

Before getting started, ensure you have:

- `kubectl` configured to access your Kubernetes cluster
- Helm v3.x (recommended for installation)

## Installation

### Helm Installation

1. Create custom configuration:

```yaml
# values-custom.yaml
config:
  # Root working directory for model storage and metadata,
  # must be writable and have enough disk space
  rootDir: /var/lib/model-csi
  # Configuration for private registry auth
  registryAuths:
    # Registry host:port
    registry.example.com:
      # Based64 encoded username:password
      auth: dXNlcm5hbWU6cGFzc3dvcmQ=
      # Registry server scheme, http or https
      serverscheme: https
image:
  # Model csi driver daemonset image
  repository: ghcr.io/modelpack/model-csi-driver
  pullPolicy: IfNotPresent
  tag: latest
```

2. Install the driver using Helm:
```bash
helm upgrade --install model-csi-driver \
    oci://ghcr.io/modelpack/charts/model-csi-driver \
    --namespace model-csi \
    --create-namespace \
    -f values-custom.yaml
```

3. Verify the installation:
```bash
kubectl get pods -n model-csi
```

## Basic Usage

### Create Model Artifact with modctl

Follow the [guide](https://github.com/modelpack/modctl/blob/main/docs/getting-started.md) to build and push a model artifact to an OCI distribution-compatible registry.

### Create a Pod with Model Volume

The Model CSI Driver uses inline volume directly in pod spec, here's a basic example:

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
      mountPath: /model
      readOnly: true
  volumes:
  - name: model-volume
    csi:
      driver: model.csi.modelpack.org
      volumeAttributes:
        modelRef: "registry.example.com/models/qwen3-0.6b:latest"
```

## Troubleshooting

### Pod stuck in Pending or ContainerCreating
  ```bash
  # Describe a pod with issues
  kubectl describe pod <pod-name>

  # Check model csi driver logs
  kubectl logs -c model-csi-driver -n model-csi
  ```
