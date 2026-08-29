# Phase 5: Messaging Authorization Documentation

**Date**: 2026-08-29
**Agent**: dev-phase5-docs
**Branch**: scion/msg-authz
**Status**: Complete

## Summary

Documented the message mode system implemented in Phases 1-4 of the messaging
authorization project.

## Deliverables

### Primary documentation: `docs/messaging-authorization.md`

Created comprehensive reference covering:

- **Mode overview**: Four modes (none, lineage, branch, project) with the
  default being project (preserves pre-mode behavior).
- **Decision tables**: User-to-agent, agent-to-agent, and system-to-agent
  authorization rules.
- **System-plane vs message-plane dividing line**: Message-plane = free text
  from principals; system-plane = hub-generated operational notices (bypass
  all mode checks).
- **Piercing rules**: Super-admin pierces all (including none); project owner
  and ancestry users pierce lineage/branch; user-identity-only (never
  inherited by agents).
- **Mixed modes in a branch**: Allowed; mixtures only remove edges (fail-safe).
- **Quarantine guide**: mode=none as kill-switch, cascade for branch-wide
  quarantine, unquarantine procedure.
- **API reference**: set_message_mode endpoints, request/response format,
  authorization matrix (D7 restrictions).
- **Permission reference**: agent.message (scope, UAT: agent:message, seeded
  to project member) and agent.set_message_mode (resource, no UAT scope, no
  agent scope, excluded from admin).
- **Design decisions summary**: D1-D10 with key rationale.

### Cross-references

- Added "Related: Message Mode Authorization" section to
  `.design/messages-evolution.md` linking to the new doc.
- Added reference link in `docs/README.md`.

### Code-level documentation

Updated godoc comments in:
- `pkg/hub/authorize_message.go`: Expanded to describe the decision table
  evaluation order and link to documentation.
- `pkg/hub/handlers_agent_message_mode.go`: Expanded to list denied/allowed
  callers with D7 rationale.
- `pkg/store/models.go`: MessageMode constants now reference D4/D5/D6/D10
  design decisions and link to documentation.

## Verification

- `go build ./...` passes.
- Changes are documentation-only (markdown files and godoc comments); no
  behavioral changes to compiled code.
