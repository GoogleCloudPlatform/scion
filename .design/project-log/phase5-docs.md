# Phase 5: Documentation + Starter-Hub Rebrand

**Date:** 2026-09-15
**Issue:** ptone/scion#1575 (Single-Node-VM deploy tier)
**Branch:** scion/sn-vm-docs (based on scion/sn-vm-plugin-matrix)

## What was done

### 1. Single-Node VM Deployment Guide
Created `docs/deploy/single-node-vm.md` with comprehensive documentation
covering the full deploy.sh workflow:
- Overview of the tier and its key properties
- Prerequisites (GCP project, gcloud CLI, required APIs)
- Quick Start (running deploy.sh with interactive prompts)
- Architecture diagram (User -> Cloud Run IAP Proxy -> GCE VM)
- What gets created (GCP resources and on-VM files)
- Configuration (settings.yaml two-stage write, hub.env)
- Chat plugins (selection during deploy, available options)
- Access patterns (web via IAP, agents via localhost, CLI via IAP, SSH via gcloud)
- Teardown (deploy.sh --delete)
- Troubleshooting (health check failures, IAP issues, cloud-init, re-runs)

### 2. Choosing a Deployment Mode
Created `docs/deploy/choosing-a-mode.md` comparing the three deployment tiers:
- Single-Node VM: released binaries, local storage, IAP auth
- Cloud Run Instance: container-based, managed lifecycle, IAP auth
- Developer Hub (formerly Starter Hub): builds from source, GCS, Secret Manager

### 3. Starter-Hub Rebrand to Developer Hub
Updated display name from "Starter Hub" to "Developer Hub" in:
- `scripts/starter-hub/README.md` (heading and description)
- `docs-site/src/content/docs/glossary.md`
- `docs-site/src/content/docs/choosing-a-mode.md`
- `docs-site/src/content/docs/hosted/single-node/overview.md`
- `docs-site/src/content/docs/hosted/single-node/hub-setup-gce.md`
- `docs-site/src/content/docs/hosted/single-node/hub-server.md`

Directory `scripts/starter-hub/` was NOT renamed (intentional, backward compat).

### 4. docs/README.md
Added a "Deployment Guides" section linking the new docs.

## Files changed
- `docs/deploy/single-node-vm.md` (new)
- `docs/deploy/choosing-a-mode.md` (new)
- `docs/README.md` (updated)
- `scripts/starter-hub/README.md` (updated)
- `docs-site/src/content/docs/glossary.md` (updated)
- `docs-site/src/content/docs/choosing-a-mode.md` (updated)
- `docs-site/src/content/docs/hosted/single-node/overview.md` (updated)
- `docs-site/src/content/docs/hosted/single-node/hub-setup-gce.md` (updated)
- `docs-site/src/content/docs/hosted/single-node/hub-server.md` (updated)
- `.design/project-log/phase5-docs.md` (new)
