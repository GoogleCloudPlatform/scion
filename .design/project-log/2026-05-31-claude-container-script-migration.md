# Claude harness: builtin → container-script migration

**Date**: 2026-05-31
**Issue**: #100
**PR**: #109

## What was done

Migrated the Claude harness from the compiled-in builtin provisioner to the
container-script provisioning model. This is the first step in retiring the
builtin provisioner path — OpenCode and Codex were already on container-script.

### Changes

1. **config.yaml**: Changed `provisioner.type` from `builtin` to `container-script`,
   added provisioner command/timeout/lifecycle config, added MCP capabilities block.

2. **provision.py**: New container-side script (~350 lines) implementing:
   - Auth resolution matching the compiled harness's 4-way precedence
   - API key pre-approval fingerprint (mirrors `ApplyAuthSettings`)
   - Project workspace path setup (mirrors `provisionClaudeJSON`)
   - MCP server translation using shared `scion_harness.py` helper
   - Auth env var overlay output

3. **Parity tests**: 11 tests covering seed verification, parity with compiled
   harness, bundle staging, reconciliation, and Python script integration.

## Observations

- The container-script provisioner's `ResolveAuth` returns method `"container-script"`
  and passes ALL candidate credentials through, deferring final selection to the
  in-container script. This is different from the builtin's `ResolveAuth` which
  picks a single winner immediately.

- The `scion_harness.apply_mcp_servers_simple()` helper works well for Claude's
  MCP schema since it's nearly 1:1 with the universal format (unlike OpenCode
  which needs custom translation to local/remote types).

- The compiled `ClaudeCode` harness is kept intact as fallback. `resolve.go`'s
  priority order checks container-script first, so the new path is used for
  fresh installs. Existing installations on `type: builtin` continue using
  the compiled harness until they run `scion harness-config upgrade claude --activate-script`.

## Remaining work

- Gemini harness migration (tracked in #100)
- Once all harnesses migrate: retire `newBuiltin()` and compiled provisioning methods
