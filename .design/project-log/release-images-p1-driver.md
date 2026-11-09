# release-images Phase 1: release image driver script (ptone/scion#6625)

Design: release-matched image builds (approved; Layer 1, §1a and §9 Phase 1).
Branch `ri/p1-build-release-images`, based on local `main` `c54bc5e`.

## What changed

- **`image-build/scripts/build-release-images.sh`** (new). A standalone driver
  that builds the image set from a release tag's own specs: resolve the
  version (argument, or the `version` field of `scion version --format json`),
  verify the tag with `git ls-remote`, clone it (or reuse a matching clone),
  run the tag's `build-images.sh --target all --tag <vX> <passthrough>`, then
  print a summary and write `<src>/build-manifest.json`. It sources nothing,
  needs only git and docker or podman, and avoids bash 4 constructs so it runs
  under bash 3.2.
- **`image-build/scripts/test-build-release-images.sh`** (new). A pure-bash
  test: a temp git repo cloned over `file://` plays the upstream. Its stub
  `build-images.sh` records argv and the version its tree would stamp. docker,
  podman and scion are stubs on a temp PATH. It includes an in-harness
  negative control, and `DRIVER_UNDER_TEST=<path>` runs the suite against a
  mutated driver.
- **`Makefile`**: `test-image-scripts`, wired into `check-custom` (and so into
  `make ci`).
- **`image-build/README.md`**: the section "Build images for a release".

## Findings worth keeping

- **Several tags can share one commit, and `git describe` picks the wrong
  one.** Upstream release tags are lightweight. `v0.3.0-preview.5` is the same
  commit as `nightly-20261030`, and `v0.3.0-preview.1/2/3` share one commit.
  A plain `git clone --depth 1 --branch v0.3.0-preview.5` also fetches
  `nightly-20261030` (tag following), and `git describe --tags --exact-match`
  then prints `nightly-20261030`: git keeps the first name in ref order when
  both are lightweight. The tag's `build-images.sh` stamps `VERSION` that way,
  so the binaries would report the nightly name. The driver therefore clones
  with `--no-tags`, fetches exactly `refs/tags/<vX>`, and requires describe to
  print `<vX>` both after a fresh clone and before it reuses a clone. In
  `--ref` mode it fetches by SHA with `--no-tags` and requires describe to
  fail, so the binaries report `dev` even when the commit is tagged.
- **Base flag contract.** `v0.3.0-preview.1` understands `--builder --registry
  --target --tag --platform --push --dry-run`. `--continue-on-error` arrived
  later, so the driver does not forward it.
- **`SCION_IMAGE_REGISTRY`.** Every supported `build-images.sh` falls back to
  this variable when `--registry` is not passed. The driver mirrors the
  fallback so that its pre-flight, `:latest` guard and summary use the same
  refs the build produces.
- **Step list.** The driver reads the built images from the `Steps:` banner
  that every supported `build-images.sh` prints, rather than sourcing the
  tag's `lib/targets.sh`.
- **GitHub Actions does not run `make check-custom`.** `.github/workflows/ci.yml`
  calls each check script directly, so `test-image-scripts` runs under
  `make ci` but not yet in GitHub CI. Adding a step there is a separate,
  shared-infrastructure change.

## Decisions

- `--channel-tag preview|stable` is parsed and validated, then exits with
  "not yet implemented" (Phase 5). An error is safer than a silent no-op for a
  flag whose name promises a registry change.
- An existing `--src-dir` that does not match is an error. The driver never
  deletes or re-clones over it; fresh clones are built in a mktemp sibling and
  moved into place.
- `--force-latest` without `--also-tag latest` is a usage error.
- `--also-tag latest` with pushed podman images uses `podman tag` and
  `podman push`; with docker it uses `docker buildx imagetools create`.

## Verification

- `bash image-build/scripts/test-build-release-images.sh`: 115 checks pass.
- shellcheck 0.11.0 is clean on both scripts.
- Mutated drivers fail the suite: without `--no-tags` (43 failures), without
  `--no-tags` and without the describe check (5 failures, including the
  stamped `nightly-20200101`), with the base pre-flight downgraded to a
  warning (4), and with a `source` line added (1).
- No bash 3.2 binary was available, so 3.2 compatibility was checked
  statically only (the test greps for banned constructs).
- The real-tag acceptance run (`v0.3.0-preview.5`, `scion-claude`) needs a
  docker host and is scheduled separately.

## Round 2 (review ri-p1-rev-1: REQUEST CHANGES)

- **Test isolation (F1).** The test used the *directories* of git, bash and
  the other tools as PATH, so a real docker in `/usr/bin` (CI runners,
  docker-ce hosts) made it exit 2. It now builds a private toolbin of
  per-tool symlinks and puts only that plus the stubs on the driver's PATH. A
  self-check re-runs the whole suite with fake docker, podman, skopeo and
  scion in the same directory as the real tools.
- **Version stamping floor (F2).** v0.3.0-preview.1 to .3 (commit 253ba544)
  derive no VERSION and have no `ARG VERSION`, so their binaries report
  `dev`. Stamping starts at v0.3.0-preview.4. The driver detects this in the
  cloned tree, warns, prints it in the summary and writes
  `"version_stamped": false`. `--help` and the README say "v0.3.0-preview.4 or
  later". EM decision: the base-FLAG floor stays at preview.1 (the flags work
  there); only the stamping floor is preview.4.
- **Option injection (F3, F5).** `--repo '--upload-pack=<cmd>'` ran the
  command. Values for `--repo`, `--ref`, `--src-dir` and the version that
  start with `-` are now rejected, and every `ls-remote`, `clone` and `fetch`
  puts `--` before its positionals.
- **Test gaps (F4).** Added cases for the `SCION_IMAGE_REGISTRY` fallback,
  bad versions, the cloud-build target-mode `Steps:` banner, the podman push
  of `:latest`, the dry-run pre-flight warning and the dirty-clone refusal.
  26 driver mutants are now run against the suite (see gates).
- **Pre-flight without docker (F6).** A dry run only warns. For
  `--builder cloud-build` the check still needs docker or podman (declined):
  the base may exist only in the registry, checking it needs a registry
  client, and the design lists docker or podman as the script's dependency.
  The error message says so and points to `--target all`.
- **Podman digest (F7).** Pushed podman images record the registry digest:
  `skopeo inspect` when skopeo is installed, otherwise
  `podman push --digestfile` on the already pushed ref.
- **Staged manifest (F8).** A run in a `--src-dir` whose `build-manifest.json`
  has the same version and commit merges into it (one image per line, so no
  jq), and the file ends up listing every staged image. Another commit starts
  fresh.
- **README (F9)** and **json_str (F10).** json_str now takes its pattern and
  replacement from quoted variables, also escapes tab, CR and newline, and is
  tested by extracting the function from the driver. It has still not been run
  under bash 3.2 itself.

## Follow-ups

- **GitHub CI step for the script test (EM-1, withdrawn from this branch).**
  `.github/workflows/ci.yml` does not run `make check-custom`, so
  `test-image-scripts` runs under `make ci` but not in GitHub Actions. The
  step (after "Check Harness Coverage in Cloud Build Configs", same rc
  handling) goes in a separate change: this branch must keep a `.github` diff
  of 0, and agent tokens generally cannot push workflow files. The F1 toolbin
  makes the test runnable on an ubuntu runner, where docker is in `/usr/bin`.

## Round 3 (review ri-p1-rev-2: REQUEST CHANGES, one Medium)

- **Untested behaviours (R2-F1).** Added tests that a dry run with
  `--also-tag latest` (local, and with `--force-latest` and a registry) makes
  no tag or `imagetools create` call and writes no manifest; that
  `--platform all` without `--push` counts as pushed (registry digests,
  `:latest` via `imagetools create`); that the podman digest comes from a
  skopeo stub when skopeo is on PATH and from `--digestfile` otherwise; and
  that a podman base that exists only in the registry passes the pre-flight.
  The reviewer's four surviving mutants are now killed.
- **Merge key (R2-F2).** The manifest records `"pushed"`. A staged run merges
  only when version, commit, builder and push mode all match, so every
  digest in a file is of one kind.
- **Single-image target floor (R2-F3).** `resolve_targets` falls back to step
  IDs only from v0.3.0-preview.4 on. The driver greps the cloned
  `lib/targets.sh` for that fallback (`for _id in "${ALL_STEP_IDS[@]}"`) and
  stops before the pre-flight with a message naming the floor. A grep was
  chosen over parsing build-images.sh's "unknown --target" error, because
  the pre-flight would otherwise die first with a misleading "stacks on
  scion-base" message.
- **Canaries (R2-F3, R2-F4).** The test runs the driver's own
  `tree_is_stamped` and `tree_has_step_targets` against this repository's
  image-build tree, so a refactor on main fails `make test-image-scripts`
  instead of mislabelling future releases.
- **Passthrough '-' values (R2-F5)** are rejected like the driver's own
  options. **The Steps banner (R2-F6)** is split with `read -r -a`, so it is
  not glob-expanded.
- **Reuse (FYI-a)** now also refuses untracked files; only the driver's own
  `build-manifest.json` is allowed. Ignored files are not listed.
- **`--src-dir` race (FYI-b).** The clone is moved under its unique mktemp
  name. If another run created `--src-dir` in the meantime, `mv` nests it;
  the driver detects the nested copy by that name, removes only it and stops.
  This is portable (no GNU `mv -T`).
- Mutation run: 38 of 40 driver mutants killed. The two survivors (M16 and
  M24) are the equivalent ones that the second reviewer confirmed.

## Round 4 (review ri-p1-rev-3: REQUEST CHANGES, one Medium)

- **Cloud Build is asynchronous (R3-F1).** The cloud-build builder of every
  supported tag (v0.3.0-preview.1 `builders/cloud-build.sh:130`, preview.5
  `:229`) runs `gcloud builds submit --async` and returns before the images
  exist. The driver now treats cloud-build as submit-only:
  - `--also-tag latest` with `--builder cloud-build` is a usage error (rc 2)
    before the clone;
  - no digest lookup happens, so an older build of the same tag in the
    registry is never reported;
  - the summary says the build was only submitted;
  - the manifest is still written, with `"submitted": true`,
    `"pushed": false` and empty digests. That keeps the record of what was
    submitted (refs, commit), and gives Phase 5 a seam to fill in digests
    after waiting for the build.
  - A submitted manifest is never merged with, in either direction. Today
    the builder key alone already separates them (cloud-build is exactly
    the submitted case), so the two submit checks are defence in depth.
  - The test stub now models the async submit, and the old test that
    asserted `:latest` moves after a cloud-build "build" is replaced.
- **`git status` failure (R3-F2)** now stops reuse with a message instead of
  passing as clean.
- **Race test (R3-F3).** The fake "other run" directory holds an executable
  `build-images.sh`. (Correction from round 5: in round 4 the directory was
  not a git checkout, so a driver that continued died earlier, at
  `git rev-parse`, and the rc check was what caught it. Round 5 makes it a
  git checkout, so "the other run's tree is never built" is now the
  assertion that fails.)
- **Temp manifest (R3-F4)** is removed by the EXIT trap. A refused reuse
  lists the offending paths (at most 10).
- **Usual race (R3-F5).** When `--src-dir` is filled by another run before
  this one replaces it, the run gets the "created by another run; re-run to
  reuse it" message.
- **Group targets (R3-F6)** are pinned by a test on the pre-preview.4
  fixture.
- **Ignored files are not checked on reuse.** At the supported tags, no
  compile input is git-ignored: scion-base COPYs only `cmd/`, `harnesses/`,
  `internal/`, `pkg/`, `proto/`, `resources/`, `third_party/` and `web/*.go`,
  and the builders write nothing into the tree. Before tightening the check
  to `--ignored`, confirm that no builder writes ignored files.

## Round 5 (review ri-p1-rev-4: APPROVE, six Low/Nit taken as fixes)

- **README (R4-F1)** lists `submitted` in the manifest shape and says that a
  submitted run has empty digests.
- **Race test (R4-F2).** The fake other-run directory is a git checkout with
  a commit, so a driver that continues after the race reaches its
  `build-images.sh`, and "never built" is what fails (see the correction
  under round 4).
- **Inherited environment (R4-F3).** `THICK_BUILD=true` (scion-base on
  thick-prep) or `OMNI_BUILD=true` (chained harnesses) in the caller's
  environment changed the parents that the tag's scripts use, while the
  driver's pre-flight checked core-base/scion-base. The driver now runs
  build-images.sh under `env -u THICK_BUILD -u OMNI_BUILD`.
  - Audit of every `${VAR:-}` read in build-images.sh, lib/targets.sh and
    builders/ at v0.3.0-preview.1 and .5:
    - REGISTRY, PUSH, DRY_RUN, PLATFORMS, SHORT_SHA, COMMIT_SHA and VERSION
      are reassigned by build-images.sh before use.
    - SCION_IMAGE_REGISTRY is the documented fallback, which the driver
      mirrors.
    - GCLOUD_PROJECT (cloud-build), NPM_REGISTRY, PIP_INDEX_URL,
      NPM_CONFIG_FILE and PIP_CONFIG_FILE (package mirrors and credentials)
      and TMPDIR change neither targets nor bases.
    - So only THICK_BUILD and OMNI_BUILD are cleared.
  - Tested with both exported.
- **Truncation (R4-F4).** The ">10 files" message is tested with 11 untracked
  files.
- **Merge comment (R4-F5)** reworded. **`--help` (R4-F6)**: every line is now
  80 columns or fewer.
- **Review package.** This checkout is shallow (its boundary is c54bc5ec), so
  a bundle without a prerequisite cannot be built from it, and a full-history
  fetch would need a named GO. Round 5 therefore ships the range bundle, tree
  archives of the head and of the base, and the diff.

## Round 6 (review ri-p1-rev-5: APPROVE, two findings taken as fixes; test-only)

- **Failing build (R5-F1).** No test made the tag's build-images.sh fail, so
  dropping `|| die "build-images.sh failed"` or `pipefail` went unnoticed.
  Either change would let a failed build move `:latest` and record stale
  digests. The stub now has `STUB_FAIL` (it prints the Steps banner, then
  exits 3). A test with `--also-tag latest --force-latest -- --registry R
  --push` and older remote images present asserts rc 1, the message, no
  `imagetools create`, no digest lookup, no manifest and no temp manifest.
  The driver needed no change.
- **Truncation (R5-F2).** The 11-file case asserts that exactly 10 names are
  listed.
- **Gates are offline only** from this round on (ri-em's 06:50Z rule): no
  network git and no real-tag dry runs without a named GO.
