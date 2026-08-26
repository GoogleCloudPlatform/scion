# Fix: Omni Dockerfile CMD missing --enable-web and --enable-runtime-broker

**Date:** 2026-08-26
**Task:** Fix CMD in omni Dockerfile so Cloud Run Instances actually serve traffic
**Branch:** `scion/dev-fix-omni-cmd`

## Problem

The omni image's CMD was:
```dockerfile
CMD ["scion", "server", "start", "--foreground", "--host", "0.0.0.0", "--enable-hub"]
```

This started the hub in **standalone mode**, which listens on port **9810**.
Cloud Run routes traffic to port **8080**, so nothing answered — every deployed
Instance was dead on arrival.

Additionally, without `--enable-runtime-broker`, there was no broker process and
no PTY support, meaning agents could not be launched or attached to.

## Fix

Changed CMD to:
```dockerfile
CMD ["scion", "server", "start", "--foreground", "--host", "0.0.0.0", "--enable-hub", "--enable-web", "--enable-runtime-broker"]
```

- **`--enable-web`** activates combined mode: the hub API is mounted on the web
  server, which listens on port 8080 (where Cloud Run routes traffic).
- **`--enable-runtime-broker`** starts the runtime broker for agent lifecycle
  management and PTY support.

See `cmd/server.go` lines 87–92 and 241–274 for flag definitions;
`cmd/server_foreground.go` lines 335–363 for the combined-mode mounting logic.

## Files Modified

- **`image-build/omni/Dockerfile`** — Added `--enable-web` and
  `--enable-runtime-broker` to the CMD. Updated the comment above CMD to
  describe all three components and explain why each flag is needed.

## Note

The previously published image tag
`dev-de79f5b3d2a75b24bd9d4c7de4e470c7881ead2a` remains broken. A new image
must be built and published from this fix for Instances to function correctly.

## Testing

A Dockerfile-content assertion test was considered (parse the Dockerfile, assert
CMD contains the three flags). This was rejected because:
1. The Dockerfile is not programmatically consumed by Go code — it is a build
   artifact consumed by Docker/Cloud Build.
2. A test that reads the Dockerfile as a text file is fragile (depends on exact
   formatting) and provides little value over code review.
3. The real verification is that a deployed Instance responds on port 8080,
   which requires an integration test with actual image build/deploy.
