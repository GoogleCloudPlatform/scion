# WS-3: CLI Reference Updated for Messaging Refactor

**Agent:** dev-ws3-cli
**Branch:** `scion/ca-msg-em5`
**Date:** 2026-08-27

## Summary

Updated `docs-site/src/content/docs/reference/cli.md` to document the S4 messaging-model
refactor: new `scion broadcast` and `scion keys` subcommands, updated `scion message` with
conversation-reference recipients, and deprecated-flag migration paths.

## Changes

### Section restructure

The previous flat "Agent Lifecycle" section covered everything from `scion start` through
`scion reset-auth`. This update splits it into three `##`-level groups:

- **Agent Lifecycle** — `start`, `stop`, `suspend`, `resume`, `attach`
- **Messaging** — `message`, `broadcast`, `keys`, `messages`
- **Agent Management** — `logs`, `list`, `delete`, `sync`, `reset-auth`

### `scion message` — updated

- **Recipients** now document five forms: bare `<agent-name>`, `agent:<name>`, `user:<name>`,
  `group[a,b,...]`, and `@<agent-name>` (conversation reference).
- **Retained flags** reduced to `-i`, `-w`, `-a/--attach`, `--visibility`.
- **Deprecated flags** listed with migration targets (e.g. `--broadcast` → `scion broadcast`,
  `--raw` → `scion keys`, `--in`/`--at` → `scion schedule message`).
- **Caveat block** added: `@<agent-name>` is fully available; `@<email>` is agent-container-only;
  `conv:<uuid>` and `#<thread-name>` are CLI-gated (parse but do not deliver).

### `scion broadcast` — new

Documents the replacement for `scion message --broadcast` with flags `--all`, `--interrupt`,
`--wake`, and `--visibility`.

### `scion keys` — new

Documents the replacement for `scion message --raw`: sends raw keystrokes via tmux send-keys.
Includes examples for `Escape`, `C-c`, and arrow-key sequences.

## Caveats applied

- No fenced code examples use `conv:<id>` or `#<thread>` as working recipients — these appear
  only in the gated-caveat admonition block.
- All fenced `scion ...` examples use valid cobra command syntax for parse-checking.
