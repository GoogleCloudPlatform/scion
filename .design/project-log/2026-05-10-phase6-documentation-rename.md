# Project Log - Phase 6: Grove to Project Rename (Documentation)

## Task Description
Rename 'grove' to 'project' across all user-facing documentation as part of Phase 6 of the grove-to-project rename effort.

## Work Completed
- Scanned documentation directories (`docs-site/src/content/docs/`, `docs-repo/`) and `README.md` for 'grove' occurrences.
- Renamed `docs-site/src/content/docs/hub-user/git-groves.md` to `docs-site/src/content/docs/hub-user/git-projects.md`.
- Updated content in 31 files using a regular expression script to handle various case sensitivities (`grove`, `Grove`, `GROVE`, `groves`, `Groves`, `GROVES`).
- Updated `docs-site/astro.config.mjs` sidebar configuration to point to the new `git-projects.md` file and updated the label.
- Verified that no 'grove' occurrences remain in the target documentation, respecting the exclusion of `release-notes.md`, `changelog/`, and `.design/`.

## Findings and Observations
- Most occurrences were in `docs-site/src/content/docs/contributing/architecture.md` and `docs-site/src/content/docs/hub-user/git-projects.md` (formerly `git-groves.md`).
- The `scion` CLI help output now shows `--project` instead of `--grove` (verified by building from source), confirming that the binary itself has already been updated in previous phases.
- Documentation links were mostly relative or handled by the bulk replacement.

## Verification
- `grep -ri "grove"` in target directories returned no results (excluding release notes).
- `make build` and `./build/scion --help` shows `--project` flag.
- Sidebar in `astro.config.mjs` was manually verified.
