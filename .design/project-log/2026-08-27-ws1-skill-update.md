# WS-1: scion-messaging Skill Update for S4

**Date:** 2026-08-27  
**Scope:** S5 WS-1 — Docs  
**Branch:** `scion/ca-msg-em5`  
**File:** `resources/platform_skills/scion-messaging/SKILL.md`

## Summary

Updated the `scion-messaging` platform skill to reflect the S4 messaging refactor changes. This skill teaches agents how to use `scion message` and related commands.

## Changes Made

### Recipient Types
- Added `@<agent-name>` as a fully supported conversation reference form
- Documented `@<email>` with its agent-container-only limitation
- Added explicit warning that `conv:<id>` and `#<thread>` are reserved but not available in the CLI (DEF-5)
- Updated broadcast anti-pattern to reference `scion broadcast` subcommand

### Channel and Thread Targeting
- Marked `--channel` and `--thread-id` as deprecated, pointing to `@<agent-name>` instead

### Special Message Flags
- Reorganized into active flags (`--wake`, `--interrupt`, `--attach`, `--visibility`) and a deprecated flags table
- `--raw` → `scion keys`
- `--broadcast`/`--all` → `scion broadcast`
- `--notify` → `scion notifications subscribe`
- `--in`/`--at` → `scion schedule message`
- `--plain`, `--channel`, `--thread-id`, `--cc` noted as deprecated

### New Subcommands Section
- Documented `scion broadcast` with flags and usage guidance
- Documented `scion keys` for raw keystroke injection

### Other
- Updated self-callback heartbeat pattern to prefer `scion schedule message`
- Updated verification checklist to include new recipient forms and subcommand usage
