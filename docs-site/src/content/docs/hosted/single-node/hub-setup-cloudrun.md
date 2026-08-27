---
title: Deploy on Cloud Run (Sandbox)
description: Deploy a Single-node Hub as a Cloud Run instance using the Sandbox tier.
---

The **Single-node Cloud Run sandbox tier** provides a streamlined, one-command deployment that spins up a full Scion Hub and local runtime within a single Cloud Run Instance.

This deployment method acts as a sandbox, provisioning a `run.app` URL where the Hub and all associated agents run within the boundaries of that single instance. This provides a fast, low-cost environment with a shared filesystem, ideal for testing, small team usage, and rapid prototyping without the overhead of full HA architecture.

## How It Works

- **Single Instance**: Both the Hub control plane and the agent runtime execute inside the same Cloud Run Instance.
- **Embedded SQLite**: The Hub uses the `sqlite` driver for metadata and state storage, avoiding the need for an external Cloud SQL database.
- **Shared Filesystem**: Workspace directories and agent state are stored locally on the instance, providing instant volume mounting.

*(Note: Because this tier is non-HA, state and worktrees will be lost or reset if the Cloud Run Instance scales down or is forcefully redeployed unless a persistent network volume is attached. Use for ephemeral workspaces or configure durable external storage.)*

## Deployment

Deploying the sandbox tier involves a single command through the provided setup tooling (details specific to your orchestration script):

```bash
# Example deployment command (refer to your internal tooling or scripts directory)
make deploy-cloudrun-sandbox
```

Once deployed, the command will output the `run.app` URL for the Hub.

## Comparison with HA Cloud Run

This single-node tier is distinct from the **HA Cloud Run** deployment:

* **Sandbox Tier**: One instance, embedded DB, internal agent execution.
* **HA Tier (Cloud Run Instances Runtime)**: The Hub scales across multiple instances with external Postgres; agents are dispatched as *individual* Cloud Run services with NFS and Secret Manager injection.

To step up to the fully distributed model, see the [Deploy on GCP (HA)](/scion/hosted/ha/setup-gcp/) guide.
