# Phase 5: Container Labels and Runtime Discovery Transition

Date: 2026-05-09
Agent: developer

## Overview

Implemented Phase 5 transition for container labels and runtime discovery. This involves emitting both `scion.project*` and `scion.grove*` labels/annotations for backward compatibility, and updating discovery logic to prefer the new project variants while falling back to grove variants.

## Changes

### Container Label Emission
- **pkg/agent/run.go**: Updated `RunConfig` generation to include:
  - Labels: `scion.project`, `scion.project_id` (alongside `scion.grove`, `scion.grove_id`).
  - Annotations: `scion.project_path` (alongside `scion.grove_path`).
- **cmd/server_dispatcher.go**: Updated `hubAgent` label setting to include `scion.project`.

### Discovery and Filtering Logic
- **pkg/agent/list.go**: Updated `List` to handle `scion.project` and `scion.project_path` in filters.
- **pkg/agent/manager.go**: Updated `MessageRaw` and `deliverImmediate` to use `scion.project_id` in filters.
- **pkg/runtimebroker/handlers.go**:
  - Updated `matchesAgent` to check both `scion.project_id` and `scion.grove_id`.
  - Updated `listAgents` to support `scion.project_id` filter.
  - Updated `agentKey` for deduping to use project ID.
  - Updated `resolveManagerForAgent` and `resolveRuntimeForAgent` to use `scion.project_id` in filters.
- **pkg/runtime/docker.go**:
  - Updated `List` to populate `Project`, `ProjectID`, and `ProjectPath` from both label variants.
  - Updated filtering logic to support fallback from project keys to grove keys.
- **pkg/runtime/k8s_runtime.go**:
  - Updated `List` to populate `Project`, `ProjectID`, and `ProjectPath` from both label variants.
  - Updated `List` selector translation to use grove variants for the K8s API call (ensuring old and new pods are found).
  - Updated `createSharedDirPVCs` to emit both project and grove labels on PVCs.

### Command Line Interface
- Updated `attach`, `delete`, `list`, `message`, `stop`, and `suspend` commands to use `scion.project` and `scion.project_path` when filtering agents.

### State and Provisioning
- **pkg/agent/provision.go**: Updated `ProvisionAgent` to populate `ProjectID` and `ProjectPath` in the `AgentInfo` written to `agent-info.json`.
- **pkg/agent/provision.go**: Updated `StopProjectContainers` to use `scion.project` for filtering.

## Verification Results

- Verified that all modified files contain the new label variants.
- Verified that discovery logic prefers project labels but successfully falls back to grove labels.
- Build and basic tests for modified packages pass.
