# Design: System Project

## Status

Approved and implemented on branch `scion/system-project`.

## Overview

The system project is a built-in, optional Scion project that gives Hub admins a
dedicated helper-assistant workspace for configuring and managing Scion. It is
separate from the existing global project: global remains a default workspace,
while the system project is a management surface where agents can act with
elevated Hub permissions inherited from their creating admin user.

The system project is represented as a normal `Project` record with reserved
identity and labels:

| Field | Value |
| --- | --- |
| Slug | `system` |
| Name | `System` |
| Visibility | `private` |
| Labels | `scion.io/system=true`, `scion.io/system-project=true` |

## Goals

1. Create an optional built-in `system` project at Hub startup.
2. Give system-project agents assistant-mode CLI access instead of restricted
   agent-mode CLI access.
3. Allow system-project agents created by Hub admins to evaluate and enforce
   Hub authorization the same way at runtime and via policy evaluation.
4. Keep the feature compatible with co-located and HA deployments by supporting
   a configurable workspace path.

## Non-Goals

1. Removing or replacing the global project.
2. Adding a dedicated system-project UI.
3. Creating a shared, global system agent.
4. Granting elevated permissions to non-admin-created agents.

## Enablement

The feature is disabled by default and controlled by Hub server config:

```yaml
server:
  system_project:
    enabled: true
    workspace_path: /optional/shared/path
```

Equivalent CLI flags are available:

```bash
scion server start --enable-system-project
scion server start --enable-system-project --system-project-workspace-path /mnt/nfs/scion/system-project
```

When disabled, startup is a no-op and existing system-project records are not
deleted.

## Startup Registration

When enabled, the foreground server startup path calls `registerSystemProject`
after the co-located broker identity is known. Registration is idempotent:

1. Resolve the workspace path.
2. Create the workspace directory tree.
3. Look up project slug `system`.
4. Create the project if missing.
5. Backfill missing system labels, default broker ID, shared dir, and provider
   mapping if the project already exists.
6. Ensure the project members group and policy bindings exist.

The system project reuses the existing co-located runtime broker rather than
creating a dedicated broker.

## Workspace Layout

By default, the workspace lives under the Scion global directory:

```text
~/.scion/system-project/
├── shared/
│   ├── journal.md
│   ├── notes/
│   └── runbooks/
├── agents/
└── config/
```

`server.system_project.workspace_path` overrides this for shared-storage or HA
deployments, for example an NFS mount.

## Reserved Slugs

The slugs `global` and `system` are reserved. User-driven project creation and
project registration reject reserved slugs unless the caller is a Hub admin.
This prevents a regular user from pre-empting the `system` slug before startup
registration and then gaining elevated semantics from the reserved identity.

## Access Control

The system project uses standard project membership:

1. Hub admins have access through the existing admin bypass.
2. Named users can be added to `project:system:members`.
3. The project remains private and should not appear to unauthorized users.

For elevated Hub API access, system-project agents use ancestry-based admin
delegation. If an agent belongs to the system project and its origin user
(`Ancestry[0]`) is currently a Hub admin, authorization grants admin-equivalent
access with reason `system project admin delegation`.

The admin role is checked at evaluation time, so removing the origin user's admin
role revokes delegated access.

The policy evaluation endpoint must populate the same agent ancestry used by
runtime enforcement. Otherwise `/api/v1/policies/evaluate` can disagree with
actual access checks for system-project agents.

## CLI Mode

Normal agents receive `SCION_CLI_MODE=agent`. System-project agents receive
`SCION_CLI_MODE=assistant`.

Assistant mode exposes broader management commands while still blocking
security-sensitive or interactive commands such as Hub auth flows, token
management, config migration, direct directory-changing helpers, reconnect, and
cleanup.

The Hub dispatcher applies this mode when creating, starting, and restarting
agents so a restarted system-project agent keeps assistant mode.

## Capability Reporting

Capability precomputation follows the same system-project delegation rule as
direct authorization checks. UI and API capability responses therefore report
the same access that runtime enforcement will allow.

## Implementation Map

| Concern | Location |
| --- | --- |
| Server flags and constants | `cmd/server.go` |
| Startup registration | `cmd/server_foreground.go`, `cmd/server_broker.go` |
| Daemon flag forwarding | `cmd/server_daemon.go` |
| Config model and V1 settings | `pkg/config/hub_config.go`, `pkg/config/settings_v1.go` |
| Workspace path helper | `pkg/config/paths.go` |
| System labels and reserved slugs | `pkg/projectcompat/labels.go` |
| CLI mode dispatch | `pkg/hub/httpdispatcher.go` |
| Authorization delegation | `pkg/hub/authz.go` |
| Capability precompute | `pkg/hub/capabilities.go` |
| Policy evaluate ancestry | `pkg/hub/handlers_policies.go` |
| Reserved slug enforcement | `pkg/hub/handlers.go` |

## Verification

Focused coverage includes:

1. Disabled registration is a no-op.
2. Enabled registration creates and backfills the `system` project, provider,
   workspace tree, shared dir, labels, and members group.
3. System-project create/start/restart dispatch sets `SCION_CLI_MODE=assistant`.
4. Normal project dispatch preserves/defaults agent CLI mode correctly.
5. System-project agents with admin ancestry receive delegated access.
6. Delegation is revoked when the origin user is no longer an admin.
7. `/api/v1/policies/evaluate` uses stored agent ancestry.
8. Non-admin users cannot create or register reserved slugs.

