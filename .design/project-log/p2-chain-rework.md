# Omni Image Build: Harness Chaining Rework

**Date:** 2026-08-25
**Branch:** `scion/p2-chain-rework`
**Author:** dev-p2-chain

## Summary

Reworked the omni image build to chain harness Dockerfiles instead of
hand-transcribing their install steps into a single monolithic Dockerfile.
This eliminates version drift: when a harness Dockerfile bumps a version or
adds a tool, the omni image picks it up automatically.

## Chain Order

```
scion-base -> scion-claude -> scion-codex -> scion-opencode -> scion-antigravity -> scion-grok-build -> scion-omni
```

Only 5 harnesses are in the omni chain: claude, codex, opencode, antigravity,
grok-build. Other harnesses (copilot, gemini-cli, hermes) are not included.

## Changes

### `image-build/scripts/build-images.sh`
- Added `omni` to the `THICK_BUILD` conditional (omni implies thick base)
- Added `OMNI_BUILD` flag, gated on `TARGET == "omni"`, mirroring the
  `THICK_BUILD` pattern

### `image-build/scripts/lib/targets.sh`
- `resolve_targets omni` now emits the full chain:
  `scion-claude scion-codex scion-opencode scion-antigravity scion-grok-build scion-omni`
- Added `_OMNI_CHAIN` array and `_omni_chain_parent()` helper defining the
  chain order
- `step_parent` and `step_build_args` use the chain when `OMNI_BUILD=true`:
  each harness builds on the previous one instead of branching from scion-base
- When `OMNI_BUILD=false` (all other targets), harness parents remain
  `scion-base` — no change to existing build paths

### `image-build/omni/Dockerfile`
- Replaced the monolithic Dockerfile (which duplicated every harness's install
  steps) with a minimal hub-only layer that sets USER root, creates the hub
  state directory, and pins the final CMD

### `image-build/scripts/builders/cloud-build.sh`
- Gated the omni target in `cloud_build_config_for_target` to error with a
  message directing users to `--builder local-docker`. The chain requires
  building 6 sequential images, which the static cloud-build YAML model does
  not support efficiently. Omni builds are periodic and local-only is
  acceptable for now.

## Verification

- **Dry-run verified:** `--target omni --builder local-docker --tag test --dry-run`
  correctly produces the chain with proper BASE_IMAGE threading
- **Non-omni paths verified:** `--target common` and `--target thick` dry-runs
  confirmed unaffected — harnesses still branch from scion-base
- **Cloud-build gate verified:** `--target omni --builder cloud-build` correctly
  errors with a message to use local-docker
- **Static analysis:** All COPY references in harness Dockerfiles resolve
  against their context directories. USER state is safe through the chain
  (claude sets root; codex and opencode inherit root; antigravity and
  grok-build explicitly set root)

**Note:** Actual Docker build could not be verified in the sandbox environment
(no container runtime available). The dry-run output confirms all step
ordering, parent resolution, and BASE_IMAGE build-arg threading is correct.

## Known Considerations

- **USER state:** grok-build ends as root. The omni Dockerfile explicitly pins
  `USER root`. sciontool init handles the user switch at runtime.
- **CMD conflicts:** Each harness sets its own CMD. The omni Dockerfile's CMD
  is the last layer, so it wins correctly.
- **Context directories:** Each harness step uses its own context dir
  (`harnesses/<name>/`), so COPY paths resolve correctly.
