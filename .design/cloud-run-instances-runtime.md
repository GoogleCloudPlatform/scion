# Cloud Run Instances Runtime Design Doc

## Section 1: Executive Summary

This document proposes the integration of Google Cloud Run Instances as a new runtime backend for the Scion orchestration system. Cloud Run Instances is a new GCP alpha resource that provisions a continuously running container in a specified region, fully managed via the Cloud Run API without the need to maintain underlying nodes or a Kubernetes cluster. Integrating this runtime enables Scion agents to run serverless-ly on GCP infrastructure, simplifying operational overhead while leveraging native GCP integrations like NFS (Filestore) and Cloud Logging.

## Section 2: Cloud Run Instances — Resource Model

Cloud Run Instances represent a persistent group of containers running in a region, differing from Cloud Run Services (which are request-driven and scale to zero) and GKE Pods (which require node pools and a Kubernetes control plane).

**Key characteristics and lifecycle mapping:**
- **Lifecycle Operations:** The resource lifecycle maps to GCP API calls: `CreateInstance` (a long-running operation that creates and starts the instance), `StartInstance`, `StopInstance`, and `DeleteInstance`.
- **Workspace Mounting (NFS):** Support for NFS volumes is native but does not support `subPath` isolation natively like Kubernetes. Isolation must be enforced via the server-side NFS path (e.g., `projects/<pid>/workspace`), aligning with the existing Scion NFS workspace design (`.design/nfs-workspace.md` §5.4).
- **Image Restrictions:** Due to current alpha limitations, only `docker.io` images are accepted.
- **Execution Limits:** Instances have an 8-hour task timeout. With `restartPolicy: ALWAYS`, containers will automatically restart within this window, though this imposes an interruption for long-running agents.
- **Update Behavior:** Updates to an instance (e.g., changing labels or container specs) are destructive, requiring the instance to be stopped and recreated. Stopping an instance takes approximately 75 seconds due to SIGTERM/SIGKILL behavior.
- **Access:** Direct `exec` into the container is not supported natively in the same way as `kubectl exec`. Instead, SSH/exec access is facilitated via an IAP (Identity-Aware Proxy) tunnel, requiring the container to have a shell.

## Section 3: Approach Options

To integrate Cloud Run Instances into Scion, we evaluate three primary implementation approaches:

### Option A — Shell out to the `run-instance-cli` binary
- **Description:** Wrap the provided CLI reference implementation as a subprocess from within the Runtime Broker.
- **Pros:** Fast to prototype, no direct SDK dependency management in Scion, and the behavior is already tested in the CLI.
- **Cons:** Binary management and distribution overhead, poor error handling (relying on exit codes and stderr parsing), lack of robust streaming capabilities, and subprocess overhead.

### Option B — Native Go SDK (`cloud.google.com/go/run/apiv2`)
- **Description:** Implement the `Runtime` interface directly against the Go client library (the same SDK used by the CLI).
- **Pros:** Provides first-class streaming, strong type safety, robust error handling, and aligns with the standard Go approach (and Scion's existing Kubernetes integration).
- **Cons:** Introduces a dependency on an alpha SDK, which may require specific module management (e.g., the CLI uses `GOWORK=off` to resolve public modules).

### Option C — REST/gRPC direct
- **Description:** Call the Cloud Run API directly via HTTP or gRPC without leveraging the Go SDK.
- **Pros:** No SDK dependency to manage, maximum control over the network requests.
- **Cons:** Extremely high implementation effort, lacks auto-generated types and helper functions, and increases operational risk due to manual wire-protocol handling.

**Recommendation:** **Option B** is the recommended approach. It aligns with Scion's existing Go-native integrations (e.g., the Kubernetes runtime) and provides the necessary reliability, type safety, and streaming capabilities required for a production-grade Runtime Broker.

## Section 4: Proposed Architecture

### 4.1 Runtime Broker Role
The Runtime Broker in Scion proxies Hub intents into execution. For Kubernetes, the broker requires cluster access; for Docker, it requires a local `dockerd`. For Cloud Run Instances, the "runtime" is the GCP control plane. The Runtime Broker will still run (as a Scion service), but its primary role will be translating `RunConfig` specs into Cloud Run API requests using GCP credentials (e.g., Workload Identity or Service Account). This simplifies the broker's footprint, as it acts purely as an API client to GCP.

### 4.2 Agent Lifecycle Mapping
Each Scion agent lifecycle event maps to Cloud Run Instance API calls:
- **Agent start:** Maps to `CreateInstance` (which starts the container inherently). If the instance already exists but is stopped, it maps to `StartInstance`.
- **Agent stop:** Maps to `StopInstance` (noting the ~75s delay).
- **Agent delete:** Maps to `DeleteInstance`.
- **Agent restart:** Maps to `StopInstance` followed by `StartInstance` (or a full recreation if configuration changed, since updates are destructive).

### 4.3 Workspace Mounting
Per `.design/nfs-workspace.md` §5.4, Cloud Run Instances do not support `subPath`. Therefore, the Hub will generate `RunConfig` with the NFS volume specification, and the `Runtime` implementation will configure the instance's Volume mapping directly to the server-side path (e.g., `projects/<pid>/workspace`). `CreateAgentConfig` and `RunConfig` will carry `WorkspaceBackendName = "nfs"`, `NFSUID`, `NFSGID`, and the exact NFS server IP and remote share path.

### 4.4 Home Dir & Secrets
Unlike Kubernetes, where Scion uses `kubectl cp` to synchronize the home directory and secrets post-start, Cloud Run Instances lack an out-of-band file copy mechanism.
**Alternatives:**
1.  **NFS Mounts:** Leverage the existing NFS infrastructure to mount a shared directory for home and secrets.
2.  **Secret Manager:** Inject secrets using native GCP Secret Manager integration in the Cloud Run spec (if supported) or as environment variables.
3.  **Init Script:** Use a custom entrypoint script that pulls secrets/files from GCP Storage or Secret Manager on startup.

### 4.5 Agent Logs
Agent logs cannot be streamed via an equivalent of `kubectl logs --follow`. Instead, Cloud Run Instances automatically forward standard output/error to Cloud Logging. The Scion `GetLogs` implementation will need to query the Cloud Logging API (`logging.googleapis.com`) filtering by the specific instance's resource labels or name. 

### 4.6 Exec / Attach
Scion's `Attach` and `Exec` commands currently rely on `docker exec` and `kubectl exec`. For Cloud Run Instances, this requires establishing an IAP SSH tunnel. The implementation will need to either vendor the IAP SSH logic (like `run-instance-cli ssh-native`) or use the local system's `ssh` binary configured for IAP. This impacts the user experience as the container *must* have a shell available (distroless images will fail) and the calling identity needs `roles/iap.tunnelResourceAccessor`.

### 4.7 Image Registry
The alpha limitation restricts images to `docker.io`. Since Scion agents often rely on custom or Google Artifact Registry (GAR) images, this is a significant bottleneck. Handling this could involve mirroring GAR images to Docker Hub, pushing directly to Docker Hub during image build, or relying on a mitigation strategy until the feature hits GA.

### 4.8 Config Schema
A new config block will be required in `V1RuntimeConfig` (e.g., `CloudRunInstancesConfig`) to hold runtime-specific settings:
- `ProjectID`: GCP Project ID.
- `Location`: GCP region (e.g., `us-central1`).
- `ServiceAccount`: The service account attached to the instance.
- `Network` & `Subnetwork`: VPC settings for NFS access.
- `NFSShare`: Details for the Filestore instance if not fully derived from Hub settings.

## Section 5: Migration & Rollout Path

To allow Scion operators to opt into the Cloud Run Instances runtime:
- The `factory.go` will be updated to register a new `"cloudrun"` runtime.
- Hub configuration will allow specifying `"cloudrun"` as the runtime type, alongside its specific configuration block.
- It can coexist with Docker/K8s runtimes on the same Hub, provided the Hub can route different agents (or all agents) to the appropriate Broker based on agent/project configuration.

## Section 6: Open Questions

1.  **Image Registry Alpha Limitation:**
    *Resolution:* Building and image storage are outside the strict scope of the `Runtime` interface, which focuses on container CRUD operations. Scion will add Cloud Build support for building and pushing images (e.g. to GAR/Docker Hub), but the runtime implementation will simply accept the provided image URI.
2.  **Home Dir & Secrets Synchronization:**
    *Resolution:* Use an NFS mount.
3.  **Exec / Attach Dependency:**
    *Resolution:* Port the Go IAP solution from run-instances-cli/cmd/ssh_native.go, but encapsulate it behind an interface so the implementation can be swapped later.
4.  **Logging Streaming:**
    *Resolution:* Use GCP Cloud Logging. Streaming support can be added where it is part of the explicit semantic.