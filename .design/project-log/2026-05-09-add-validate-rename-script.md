# Project Log: Add Rename Validation Script

**Date:** 2026-05-09
**Task:** Create `hack/validate-rename.sh` to track 'grove' to 'project' rename progress.

## Overview
Implemented a bash script that searches for the term "grove" (case-insensitive) across the codebase while excluding specific directories and files that are either irrelevant or expected to contain the term (e.g., design docs about the rename itself).

## Changes
- Created `hack/validate-rename.sh`.
- Added functionality to report match counts per file, sorted descending.
- Added a grand total count.
- Implemented `--strict` mode which exits non-zero if any matches are found.
- Ensured the script is executable.

## Exclusions
The script excludes:
- `.design/grove-to-project-rename.md`
- `.scratch/`
- `/scion-volumes/scratchpad/`
- `go.sum`
- `changelog/`
- `.git/`

## Observations
The current codebase has over 44,000 occurrences of "grove" (including duplicates in `.scion/agents/...` workspaces). This indicates a significant amount of work remains for the full rename.

## Verification Results
- Script correctly identifies occurrences in files like `pkg/hub/handlers.go`.
- Exclusions are working as expected.
- `--strict` mode correctly exits with code 1 when matches exist.
