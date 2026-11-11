# release-images Phase 4: adopters and docs (ptone/scion#6625)

Design: release-matched image builds (approved; §1a, §6 and §9 Phase 4).
Branch `ri/p4-adopters-docs`, based on upstream `main` `fbc42467`, which
contains the Phase 3b merge (#4619, `1fe62ca1`).

## What changed

- **`scripts/single-node-vm/deploy.sh`, Phase 3b.** The inline
  clone-and-build (`git clone --branch VERSION` into `/opt/scion-source`,
  then `build-images.sh --target core-base`, `--target scion-base` and a raw
  `docker build` of antigravity, then `docker tag` into `localhost/scion`) is
  replaced by the release image driver from the checkout `deploy.sh` runs
  from. The driver is copied to `/opt/scion-image-build/` through a private
  staging dir, installed root-owned and checked against the local SHA-256,
  then run once per image (`core-base`, `scion-base`, `scion-antigravity`)
  with `<VERSION> --src-dir /opt/scion-image-src/<VERSION> --also-tag latest
  --force-latest -- --builder local-docker --registry localhost/scion`.
  Nothing is pushed: `--registry` without `--push` only names the images
  locally, and `--force-latest` moves only the local `:latest`.
- **VERSION check.** Before the driver is staged or called, Phase 3b accepts
  only `vX.Y.Z`, `vX.Y.Z-preview.N`, `vX.Y.Z-rc.N` (the pattern of
  `scripts/release/update-homebrew-formula.sh`) or `nightly-YYYYMMDD`
  (`select-weekly-preview.sh`). The general check earlier in `deploy.sh`
  already refuses a leading `-`.
- **Marker.** It moved from `/opt/scion-source/.images-built.version` to
  `/opt/scion-image-build/images-built.version`, because `/opt/scion-source`
  is no longer created by Phase 3b. The old marker is still honoured for the
  skip, so images built by an older `deploy.sh` are reused, and a new build
  removes both.
- **IAP proxy.** Its build used the `/opt/scion-source` checkout Phase 3b
  left at `VERSION`. With `source: build` the proxy step now clones or moves
  that checkout to `VERSION` itself; with `source: registry` it keeps the old
  clone-if-missing behaviour. Small difference: before, a skipped image build
  left the checkout at the old version; now it always follows `VERSION`.
- **Review round 1.**
  - The early checks (source: build only) run right after the general
    VERSION check, before Phase 2 creates anything: the driver must be next to
    `deploy.sh`, and the release must not be older than v0.3.0-preview.4
    (`release_order` from `release-order.sh`; a nightly cannot be ordered
    against a semver tag and passes, since nightlies expire after 14 days).
    The strict tag-shape check stays in Phase 3b, before the driver is staged
    or called. An early shape check would have stopped every harness test
    that deploys `v1.0.0-test` (9 files, including the downgrade cases that
    need a pre-release), for no security gain.
  - The IAP proxy step fetches the tag only when it is not already local.
    On a re-run of the same VERSION, the checkout already has the tag (it
    was cloned at it, or fetched last time), so it is checked out without a
    network fetch; only a new VERSION fetches.
  - A failed `gcloud compute scp` of the driver removes the VM staging dir
    (best effort, after re-checking its name) before stopping.
  - `install.md` no longer says `ghcr.io/homebrew-scion` holds v0.2 images
    only, which stops being true once release-tagged images are published
    there. It now says images there carry the release tag and `:latest` is
    old, matching the formula caveat.
- **Review round 2.** The floor applies only when a build will actually
  run. Phase 3b checks it (with the driver) after the skip decision, before
  staging, in one helper, `release_image_build_preflight`, which the early
  check shares. The early check skips the floor when `deploy.sh` keeps the
  release a VM already runs, because that VM's images may already be built;
  a re-run on such a VM below the floor reaches the skip. An explicit
  `--version` below the floor, or a new install, is still refused before
  Phase 2, even if the VM happens to have the images: that cannot be known
  without a VM call there.
- **Hint text** (`cmd/image_registry_presence.go`): the missing-registry
  hint now names `image-build/scripts/build-release-images.sh <version>`
  next to the README section. `experiments.md` is unchanged: Phase 3b's
  `hub.image_tag_setting` entry is already complete.
- **Docs.** `install.md` "Build container images" (the false "no publicly
  hosted images" line, now accurate about `ghcr.io/homebrew-scion` being v0.2
  only, plus the build-from-tag flow); `custom-images.md` (the "pre-built
  images from the upstream registry" contradiction, a wrong default-image
  table, and a new "Images that match a release" section);
  `image-build/README.md` (deploy.sh adopter notes, and `image_tag` in the
  `--also-tag latest` section).
- **Tests.** `tests/test_release_images.sh` (new) and three stub cases in
  `tests/lib/gcloud`; the two Phase 3b tests in `test_install_layout.sh` now
  use a release version and the new command.

## Findings worth keeping

- **A failed background image build was never reported.** The build ran as
  `nohup bash -c '{ set -euo pipefail; ...; } > log; echo $? > exit'`. With
  `set -e` inside a `{ }` group, the first failing step ends the whole
  shell, so the exit file was never written and the poll waited out its
  45-minute timeout instead of printing "Image build failed". The group is
  now a subshell `( ... )`; `test_release_images_build_failure_writes_no_marker`
  covers it.
- **Testing remote commands.** The stub `gcloud` only logs `compute ssh`
  bodies. The useful pattern (from `test_install_layout.sh`) is to pull a
  body out of the log and run it for real with `/opt` and `HOME` rewritten
  into a temp dir, a pass-through `sudo`, and stubs for every external
  command that record and fail. That is how the driver's golden argv, the
  hash check and both markers are tested offline.
- **Version floor.** The driver's single-image target
  `scion-antigravity` needs v0.3.0-preview.4 or later, where the old raw
  `docker build` worked with any release. `deploy.sh` refuses an older
  release with its own floor message before the driver runs: before Phase
  2 for an explicit `--version` or a new install, or in Phase 3b when a
  kept older release needs a build.
