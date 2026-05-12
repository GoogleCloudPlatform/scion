# Fix Issue #64: make install PATH problems

**Date:** 2026-05-12
**PR:** #65
**Branch:** scion/fix-issue-64

## Summary

Updated the Makefile `install` target to address issue #64 where the default install location (`~/.local/bin`) was often not in users' PATH.

## Changes

- Changed default install location from `~/.local/bin` to `/usr/local/bin` via configurable `PREFIX` variable
- Added post-install PATH check that warns users if the install directory is not in `$PATH`
- Added verification hint (`scion --version`) in post-install output
- Switched from `mkdir -p` to `install -d` for standard directory creation

## Usage

- `make install` — installs to `/usr/local/bin` (may need `sudo`)
- `make install PREFIX=~/.local` — installs to `~/.local/bin` for user-local

## Observations

- Pre-existing CI issues exist on `main`: Go formatting failures across 100+ files and a missing `entc.MigrateGroveToProjectData` symbol in `cmd/server_foreground.go`. These are unrelated to this change.
