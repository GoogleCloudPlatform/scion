# P2: Omni Image Build Infrastructure

**Date:** 2026-08-25
**Task:** Create omni-image Dockerfile and build wiring
**Branch:** `scion/p2-omni-image`

## Summary

Created the omni-image: a single container image that combines the hub server
and five harnesses (antigravity, claude, codex, opencode, grok) for Cloud Run
Instances single-node deployment. Cloud Run Sandboxes use `--rootfs /`
(inheriting the launcher's filesystem), so a sandbox cannot have a different
image from its launcher — one image must contain both the hub and every harness.

## Files Created

- **`image-build/omni/Dockerfile`** — Multi-harness image built on scion-base.
  Dedupes the three npm-based harnesses (claude, codex, opencode) into a single
  `npm install -g` layer. Installs antigravity via binary download and grok via
  its official install script. Includes all supporting files (claude firewall
  script, grok config). Hub layer ensures state directory exists. Default CMD
  runs the hub server.

- **`image-build/cloudbuild-omni.yaml`** — Cloud Build configuration for the
  omni target. Follows the hub cloudbuild pattern with amd64-only platform
  (Cloud Run Instances are amd64). 60-minute timeout to accommodate npm installs
  and binary downloads.

## Files Modified

- **`image-build/scripts/lib/targets.sh`** — Added `scion-omni` step ID and
  `omni` target. Wired up step_dockerfile (image-build/omni/Dockerfile),
  step_context_dir (repo root for harness file access), step_build_args
  (BASE_IMAGE=scion-base), and step_parent (scion-base).

- **`image-build/scripts/builders/cloud-build.sh`** — Added omni target to
  cloudbuild config mapping.

- **`image-build/README.md`** — Added omni to both the Targets table and the
  Cloud Build Configs table.

## Design Decisions

1. **Deduped npm installs** — claude (@anthropic-ai/claude-code), codex
   (@openai/codex), and opencode (opencode-ai) are installed in a single
   `npm install -g` layer to reduce image layers and build time.

2. **Repo root as build context** — The omni Dockerfile needs to COPY files from
   harness bundles (claude/init-firewall.sh, grok-build/home/.grok/config.toml),
   so the build context must be the repository root.

3. **amd64-only** — Cloud Run Instances are amd64, so no arm64 build is needed,
   reducing build time.

4. **No provision.py** — Per the architect's note, provision.py is staged at
   runtime by container_script_harness.go, not baked into the image.

5. **Faithful replay of harness install steps** — All install steps from each
   harness Dockerfile are reproduced (including claude's git-delta, zsh setup,
   firewall script) to ensure identical runtime behavior.

## Verification

- Shell syntax checks pass for targets.sh and cloud-build.sh
- YAML structure validated for cloudbuild-omni.yaml
- All COPY source files verified to exist
- Dockerfile follows scion-base patterns (USER root -> installs -> USER scion)
