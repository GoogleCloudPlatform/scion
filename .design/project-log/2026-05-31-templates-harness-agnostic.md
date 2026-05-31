# Templates Harness-Agnostic Cleanup

**Date:** 2026-05-31
**Issue:** #98 (also relates to #42)

## Summary

Made templates strictly harness-agnostic by deprecating harness-specific fields
(`image`, `model` as concrete name, `auth_selectedType`) in `scion-agent.yaml`
and introducing model size aliases (`small`, `medium`, `large`) for portable
model selection.

## Changes

### Model Size Aliases
- Added `ModelAliases map[string]string` to `HarnessConfigEntry` (settings_v1.go),
  `HarnessConfigData` (hubclient/types.go, store/models.go)
- Added `model_aliases` to settings-v1 schema
- Added `ResolveModelAlias()` and `KnownModelAliases` to templates.go
- Wired alias resolution into `ProvisionAgent()` — resolves after harness-config
  merge, before the model string is persisted
- Seeded default aliases in all harness embeds:
  - Claude: small=haiku, medium=sonnet, large=opus
  - Gemini: small=gemini-flash-lite, medium=gemini-flash, large=gemini-pro
  - OpenCode: small=claude-sonnet, medium=claude-sonnet, large=claude-opus
  - Codex: small=gpt-4.1-mini, medium=gpt-4.1, large=o3

### Deprecation Warnings
- Added `WarnDeprecatedTemplateFields()` — warns when templates use `image`,
  `auth_selectedType`, or concrete (non-alias) model names
- Wired warnings into `ProvisionAgent()` after config merge
- Marked `image` and `auth_selectedType` as deprecated in agent-v1.schema.json
  with `x-deprecated-by` annotations

### Documentation
- Updated templates.md with a "Model Size Aliases" section
- Clarified that image/model/auth belong in harness-config, not templates
- Updated harness-config customization examples

### Backward Compatibility
- Concrete model names still pass through unchanged
- `image` and `auth_selectedType` in templates still work as overrides
- No breaking changes to API types or persisted config

## Observations

- The `docs-writer` template in `.scion/templates/` uses `model: "gemini-3.1-pro-preview"` —
  a concrete name that ties it to Gemini. With this change, it could use `model: large`
  instead to become portable. Left unmodified per project policy (`.scion/` managed manually).
- Pre-existing test failures in TestIsInsideProject, TestIsHubContext etc. are
  environment-dependent and unrelated to this change.
