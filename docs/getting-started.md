# Getting Started with Model CSI Driver

Model CSI Driver is a Kubernetes CSI driver for serving OCI model artifacts, which are bundled based on [Model Spec](https://github.com/modelpack/model-spec). This guide will help you deploy and use the Model CSI Driver in your Kubernetes cluster.

## Overview

The Model CSI Driver enables efficient deployment of model in Kubernetes by:

- Easily mount model artifact as volume into pod
- Compatibility with older Kubernetes versions
- Natively supports P2P accelerated distribution

## Prerequisites

Before getting started, ensure you have:

- Kubernetes cluster (v1.20+)
- `kubectl` configured to access your cluster
- Helm v3.x (recommended for installation)
- Container runtime with CSI support (containerd, CRI-O)

## Installation

### Helm Installation

1. Clone the repository:
```bash
git clone https://github.com/your-org/model-csi-driver.git
cd model-csi-driver
```

2. Create custom configuration:

```yaml
# values-custom.yaml
config:
  # Root working directory for model storage and metadata,
  # must be writable and have enough disk space for model storage
  rootDir: /var/lib/model-csi
  registryAuths:
    # Registry host:port
    registry.example.com:
      # Based64 encoded username:password
      auth: dXNlcm5hbWU6cGFzc3dvcmQ=
      # Registry server scheme, http or https
      serverscheme: https
```

3. Install the driver using Helm:
```bash
helm install model-csi-driver ./charts/model-csi-driver \
    --namespace model-csi \
    --create-namespace \
    -f values-custom.yaml
```

4. Verify the installation:
```bash
kubectl get pods -n model-csi
```

## Basic Usage

### Create a Pod with Model Volume

The Model CSI Driver uses inline volumes directly in pod specifications. Here's a basic example:

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
        modelRef: "registry.example.com/models/bert-base:latest"
```

## Troubleshooting

### Common Issues

**Pod stuck in Pending or ContainerCreating**
  ```bash
  # Describe a pod with issues
  kubectl describe pod <pod-name>

  # Check model csi driver logs
  kubectl logs -c model-csi-driver -n model-csi
  ```
