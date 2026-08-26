# Omni Image: Embed Web Assets in Scion Binary

**Date:** 2026-08-26
**Author:** dev-omni-web-assets
**Branch:** scion/dev-omni-web-assets (based on scion/dev-rebase-1294)

## Problem

The omni image inherits its `scion` binary from scion-base, which builds with
`-tags no_embed_web`. This activates `web/embed_stub.go`, setting
`AssetsEmbedded = false` and providing an empty `embed.FS`. The result: the omni
image cannot serve the web UI, even though it is a hub server that needs it.

## Changes

### 1. Omni Dockerfile: Hub-builder stage (`image-build/omni/Dockerfile`)

Added a multi-stage build that rebuilds the `scion` binary with embedded web
assets:

- **hub-builder stage**: Copies `web/` directory, runs `npm install && npm run
  build` to produce `web/dist/client/`, then builds the Go binary WITHOUT the
  `no_embed_web` tag so `web/embed.go` picks up the assets via `//go:embed`.
- **Final stage**: Replaces only the `scion` binary via
  `COPY --from=hub-builder`. `sciontool` is left untouched (it's PID 1 via
  ENTRYPOINT and under active investigation).
- Uses `-buildvcs=false` since `.git/` is in `.dockerignore`.
- Passes `GIT_COMMIT` and `BUILD_TIME` via ldflags for version info.

### 2. targets.sh: Pass GIT_COMMIT to omni build (`image-build/scripts/lib/targets.sh`)

Added `GIT_COMMIT=${COMMIT_SHA}` to the `scion-omni)` case in
`step_build_args`, matching the existing pattern in the `scion-base)` case.
Without this, `scion version` in the omni image would show empty commit info.

## Verification

- `make fmt-check` passes
- Dockerfile is syntactically valid
- Builder stage correctly builds web frontend then Go binary without `no_embed_web`
- Only `scion` binary is replaced in final stage (not `sciontool`)
- `GIT_COMMIT` is properly threaded from orchestrator through build args to ldflags
