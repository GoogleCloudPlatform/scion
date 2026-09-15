# Phase 1: Release Pipeline - Telegram Plugin Build

**Date:** 2026-09-15
**Issue:** ptone/scion#1575 (Single-Node-VM deploy tier)

## What Changed

Modified `.github/workflows/build-release.yml` to build the Telegram chat plugin
binary alongside the existing scion binary during releases.

## Changes

### Matrix Expansion
- Added two new matrix entries for `scion-plugin-telegram` (linux/amd64 and linux/arm64).
- Added explicit `build_path`, `binary_name`, and `module_dir` fields to all matrix entries.
- Existing entries get `build_path: ./cmd/scion` and `binary_name: scion` explicitly.
- Plugin entries get `module_dir: extras/scion-telegram` to signal they are separate Go modules.

### Build Step
- Replaced the hardcoded `go build` command with a conditional that handles both cases:
  - For plugin builds (`MODULE_DIR` set): `cd` into the module directory and build without ldflags (plugins don't embed version info).
  - For main binary builds (`MODULE_DIR` empty): unchanged behavior with ldflags for version embedding.
- Tarball creation uses `GITHUB_WORKSPACE` for absolute paths since `cd` may change the working directory.

### Conditional Steps
- Added `if: ${{ !matrix.module_dir }}` to "Setup Node" and "Build Web UI" steps.
  Plugins don't need the web UI, so these steps are skipped for plugin builds.

### Release Job
- No changes needed. The release job already downloads all artifacts and moves them
  into the release directory, so plugin tarballs are automatically included.

## Output Artifacts (per release)
- `scion-linux-amd64.tar.gz` (unchanged)
- `scion-linux-arm64.tar.gz` (unchanged)
- `scion-darwin-amd64.tar.gz` (unchanged)
- `scion-darwin-arm64.tar.gz` (unchanged)
- `scion-plugin-telegram-linux-amd64.tar.gz` (new)
- `scion-plugin-telegram-linux-arm64.tar.gz` (new)
