# Kubernetes NFS Shared Workspace Test Manifests

These manifests are developer convenience resources used for testing and validating NFS-based shared workspace coordination under Kubernetes (such as GKE). They correspond to the architecture and design guidelines described in `.design/nfs-workspace.md`.

## Contents

- `scion-nfs-pv.yaml`: Configures the PersistentVolume (PV) and PersistentVolumeClaim (PVC) targeting a shared NFS storage server.
- `nm2-test-pod-a.yaml`: Scenario A. A single pod that mounts the PVC at a project-specific subpath, provisions a Git workspace using an init container, and performs permission and filesystem isolation checks.
- `nm2-test-pod-b1.yaml` / `nm2-test-pod-b2.yaml`: Scenario B. Concurrent pods sharing the same PVC on different subpaths, validating parallel provisioning and runtime isolation.
- `nm2-test-pod-e.yaml`: Scenario E. An advanced multi-container pod template verifying volume mounts, mount boundaries, and runtime execution behavior.

## How to Use

Apply the volume configurations followed by the test scenarios to verify your cluster's NFS volume mount and isolation mechanics:

```bash
# Setup PV and PVC
kubectl apply -f scion-nfs-pv.yaml

# Run test scenario A
kubectl apply -f nm2-test-pod-a.yaml
kubectl get pod nm2-test-agent-a -n scion-agents -w
kubectl logs nm2-test-agent-a -n scion-agents
```
