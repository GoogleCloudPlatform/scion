# Changes between `d59f9451` and `HEAD`

## Features

1. Full OpenCode Harness Support (event bridge via `scion-plugin.js`, `opencode` hook dialect, `config.yaml` capability updates, `provision.py` enhancements, heartbeat mechanism)
2. `OPENCODE_API_KEY` auth support across the auth pipeline (auto-detection, `AuthConfig`, `ResolveAuth`, `isAuthEnvKey`, `RequiredAuthEnvKeys`)
3. `"none"` auth type for harness authentication (CLI flags, schema, capabilities, auth secrets/keys validation)
4. `--source` CLI flag for specifying git source branch/tag/commit for agent workspaces (propagated through `StartOptions`, `ScionConfig`, Hub API types, `CreateWorktree`, runtime broker)
5. Default branch detection via `git ls-remote --symref` in the Hub (`pkg/hub/default_branch.go`) with remote probing and fallback chain
6. `manifest.json` written to object storage during resource bootstrap and hash-match paths (with `writeManifestToStorage` helper)
7. Harness config sync status display (`in-sync`, `local-outdated`, `hub-only`, `storage-stale`) with content hash comparison
8. `manifest.json` upload to signed URL during `scion harness-config sync`
9. `--force-harness-configs` server flag and `RefreshDefaultTemplates` that preserves user customizations by default
10. `disable_local_auth` settings field to skip local auth sources in broker-like scenarios
11. Docker network mode configuration via `settings.yaml` (`runtimes.<name>.docker.network`) mapped to `--network` flag
12. `scion exec` command for executing arbitrary commands inside agent containers (local and Hub modes)
13. WebSocket `MaxMessageSize` increased from 64KB to 10MB in `wsprotocol` and hub control channel
14. Hub `session-end` handler forwards `assistant_text` as outbound `"assistant-reply"` message
15. `internal/testgit` test helper package with `TestMain` setup in 7 packages to suppress macOS keychain prompts during tests
16. `opencode.jsonc` support in `mapEmbedFileToHomePath` and provision script
17. New scripts: `buildkitd.toml`, `local-scion-server.sh`, `proxy-host-to-docker.sh`, `test/Makefile`, `test/multi-agent-collaboration-tests.md`

## Bugfixes

1. Restored missing `StageCaptureAuthAssets` call in `pkg/agent/provision.go` that was inadvertently disabled by broken indentation
2. Fixed shebang lines (`#!/bin/bash` -> `#!/usr/bin/env bash`) in 3 image-build shell scripts for portability
3. Fixed inline config `Source` field not being propagated in `create.go` and `common.go` CLI paths
4. Removed redundant `GIT_ASKPASS=echo` env var in `remote_templates.go` (duplicated with `GIT_TERMINAL_PROMPT=0`)
5. Fixed `AuthSelectedType` not falling back to `Auth.DefaultType` when empty in `LoadHarnessConfigDir`
6. Consolidated duplicate default-branch fallback logic in `populateAgentConfig` into a single `resolveDefaultBranch` call
7. Resource validation now produces aggregate/summary issues instead of per-file noise when storage is empty or has mismatches
8. Server startup changed from `UpdateDefaultTemplates(true, ...)` to `RefreshDefaultTemplates(..., force=false)` to avoid clobbering user harness-config customizations
