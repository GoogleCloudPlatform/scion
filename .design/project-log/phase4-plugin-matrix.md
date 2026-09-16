# Phase 4: Complete Plugin Matrix + Wizard Polish

**Date**: 2026-09-15
**Issue**: ptone/scion#1575
**Branch**: scion/sn-vm-plugin-matrix (based on scion/sn-vm-iap-proxy)

## Changes

### 1. Release Workflow: Plugin Build Matrix
Added a new `build-plugins` job to `.github/workflows/build-release.yml` with matrix
entries for all four chat plugins (Telegram, Discord, Slack, Teams), each built for
linux/amd64 and linux/arm64 (8 matrix entries total). Each plugin is built from its
own Go module directory under `extras/`. The `release` job now depends on both `build`
and `build-plugins`.

### 2. Deploy Script: Chat Plugin Selection
Added an interactive wizard prompt in `deploy.sh` for selecting chat integrations
(Telegram, Discord, Slack, Teams, or None). Supports comma-separated multi-selection.
Selected plugins are downloaded from GitHub Releases and installed to
`/home/scion/.scion/plugins/broker/` during VM setup (Phase 3), using the correct
architecture suffix.

### 3. Deploy Script: Teardown Flow (--delete)
Added `--delete` flag that tears down all GCP resources created by a previous deploy:
Cloud Run IAP proxy service, GCE VM, and service account. Includes a confirmation
prompt before deletion and graceful handling of already-deleted resources.

### 4. Deploy Script: Disk Size Prompt
Added a disk size selection prompt (200 GB default, 500 GB, or custom) in the wizard.
The selected size is used in the `gcloud compute instances create` command's
`--boot-disk-size` flag.

## Files Modified
- `.github/workflows/build-release.yml` - added `build-plugins` job with 8 matrix entries
- `scripts/single-node-vm/deploy.sh` - added plugin selection, teardown, disk sizing
