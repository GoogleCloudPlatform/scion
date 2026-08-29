# Phase 4 Frontend — Template & Create Integration

**Date**: 2026-08-29
**Author**: dev-phase4-frontend
**Branch**: `scion/dev-phase4-frontend-work` (based on `scion/ma-ui-em`)

## Summary

Added message mode selectors to the agent create and configure pages, and
displayed the mode badge on the template detail page. This completes the
Phase 4 deliverables of the messaging authorization UI.

## Changes

### 1. Template Type Extension (`web/src/shared/types.ts`)
- Added `TemplateConfig` interface mirroring the Go `store.TemplateConfig` struct
  with fields: `harness`, `image`, `configDir`, `env`, `detached`, `commandArgs`,
  `model`, and `messageMode`.
- Extended the `Template` interface with an optional `config?: TemplateConfig` field.

### 2. Agent Create Page (`web/src/components/pages/agent-create.ts`)
- Added `@state() private messageMode = ''` state property.
- Added a "Message Mode" `<sl-select>` in the Auth & Security tab, after the
  Agent Role field. Options include "Default (inherit from parent)" plus the
  four modes (Project, Branch, Lineage, Sealed) with icons from
  `MESSAGE_MODE_DISPLAY`.
- Inline warning displayed when "Sealed" (none) mode is selected.
- Hint text explains default inheritance behavior.
- `messageMode` is included in the POST body when a non-default value is selected.

### 3. Agent Configure Page (`web/src/components/pages/agent-configure.ts`)
- Added `@state() private messageMode = ''` state property, populated from
  `this.agent.messageMode` in `populateForm()`.
- For created-phase agents: added an editable `<sl-select>` with the same
  options as the create page, after the Agent Role section.
- For non-created agents: renders a read-only `<scion-message-mode-badge>`
  with a hint that mode changes use the agent detail page.
- `messageMode` is included in the PATCH body for both Save and Start actions.

### 4. Template Detail Page (`web/src/components/pages/template-detail.ts`)
- Imported `message-mode-badge.js`.
- When `template.config?.messageMode` is set, renders a
  `<scion-message-mode-badge>` in the `template-meta-row` metadata section
  alongside Scope, Status, and Hash.

## Verification

- `npx tsc --noEmit` passes with zero errors.

## Patterns Followed

- `agentRole` field pattern in agent-create.ts for state + select + POST body.
- `agentRole` disabled-select pattern in agent-configure.ts for read-only display.
- `harness-badge` pattern in template-detail.ts for inline metadata display.
- `MESSAGE_MODE_DISPLAY` constants from `web/src/shared/message-mode.ts`.
- `<scion-message-mode-badge>` component from `web/src/components/shared/message-mode-badge.ts`.
