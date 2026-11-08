# release-images P5-a: --channel-tag for registry publishing, stage 2a checklist (ptone/scion#6625)

Phase 5, part a: code and docs only. No registry, Cloud Build or gcloud call was made. P5-b (the
first real publish) needs ptone's approval.

## What --channel-tag did at the base

It accepted only `preview` or `stable` (anything else exits 2), then exited 1 with "not yet
implemented" before any clone. Nothing else existed.

## What it does in this change

- **Refusals, all before the clone and the build:**
  - no registry (exit 2);
  - a build that is not pushed (exit 2);
  - `preview` on anything but `vX.Y.Z-preview.N` or `vX.Y.Z-rc.N`, and `stable` on anything but
    `vX.Y.Z` (exit 2). rc releases are on the preview channel (LATEST.json and the preview
    branch), so `:preview` follows them. release-manager decided this in round 1; the first cut
    had refused rc for both channels;
  - any nightly tag (exit 2);
  - `--ref` builds (exit 2);
  - `--also-tag latest` (or `--force-latest`) together with `--channel-tag` (exit 2): the flag's
    contract is that `:latest` is never touched (round 2);
  - a registry path outside `<host>[:<port>]/<path>` (exit 2). An image name from the step list
    outside the image-name grammar stops the run with nothing moved, so recovery commands are built
    only from checked values (round 2, sec's optional hardening);
  - with `--builder cloud-build`, a registry that is not exactly
    `<location>-docker.pkg.dev/<project>/<repository>[/...]`, matched by an anchored regex and not
    by substring (exit 2). `<location>` follows the Artifact Registry grammar: `us`, `europe`,
    `asia`, or a region shaped `<area>-<direction><1-2 digits>`. That refuses prefix tricks such as
    `evil-us-central1-docker.pkg.dev` (S1, round 1).
- **`:latest`** is never touched by `--channel-tag`. Tests assert it on every path.
- **Verified first, recoverable after.** The following all happen before the first channel tag
  moves:
  - every image's digest is checked (`sha256:` plus 64 lowercase hex);
  - each image's current `:<channel>` digest is read and recorded. Only a failed lookup whose
    stderr says "not found", "NOT_FOUND" or "manifest unknown" records "none"; any other failure,
    or a malformed digest, stops the run with nothing moved.
  Each moved tag is read back. A registry cannot move several tags atomically. If a move or
  read-back fails part-way, the run stops, lists the tags moved so far, and writes recovery
  commands (to the recorded digests, or `tags delete` for "none") to
  `<src-dir>/channel-recovery.XXXXXX`. The file is created by mktemp, so it never overwrites. The
  driver never runs the commands, and it writes no manifest in that case. A successful run writes
  no recovery file. Reusing a `--src-dir` tolerates these files. If the file cannot be created, the
  commands go to stderr. A podman channel run needs `skopeo` and reads every registry digest
  (`:vX`, the channel pre-read and the read-back) with it, never by pushing.
- **Pushed local builds.** Docker runs `imagetools create -t <img>:<channel> <img>@<digest of
  :vX>`; podman runs `tag` plus `push`. The digest is the registry digest of `:vX` read in this run.
- **Cloud Build** (decided by release-manager on the advice of sec-review-3: option 1 on top of 3):
  - The driver logs build-images.sh stderr too, and parses each build from gcloud's
    `Created [https://cloudbuild.googleapis.com/v1/projects/<p>/locations/<l>/builds/<uuid>].`
    line with an anchored regex. Any other `Created [https://cloudbuild...` line stops the run.
  - It polls `gcloud builds describe --format=value(status)`. Bounds: `--build-timeout`
    (default 14400s per build, the longest yaml timeout) and `--wait-timeout` (default 21600s in
    total). The poll interval is 30s, and the clock advances by the interval only, so real time is
    never shorter than stated and can be longer (a hanging gcloud call is not bounded; --help says
    so). Any end state other than SUCCESS stops the run, and so do three
    describe failures in a row.
  - Digests come from the builds' own results, never from a tag:
    - `results.images` (name, digest). Names this run did not build, such as `:<short-sha>`, are
      ignored.
    - `results.buildStepOutputs`: base64, checked against the alphabet. Each non-empty line must
      be `<ref> sha256:<64 hex>` for a ref this run built. An extra, unknown or duplicate line stops
      the run.
    - If both sources name an image, they must agree. Every image needs exactly one digest.
  - `:vX` read through `gcloud artifacts docker images describe` must equal the build-result
    digest.
  - Then `gcloud artifacts docker tags add <img>@<digest> <img>:<channel>` runs for each image, and
    each tag is read back. Every call is argv; nothing goes through a shell.
  - The manifest records `"submitted": false` and `"pushed": true` with the real digests.
- **Manifest:** a channel run adds `tag_digest` (what `:vX` pointed to when checked) and
  `channel_ref` to each image line. Other runs keep the existing shape.

## Known gap (found here; release-manager ruled: ship the strict form)

Cloud Build fills `results.images` only for a config's top-level `images:` list. Every
`image-build/cloudbuild*.yaml` except `cloudbuild-omni.yaml` pushes from inside a step with
`buildx --push` and lists no `images:`, and no step writes `$BUILDER_OUTPUT/output`. So with
the release trees at base 17ed440f, a cloud-build `--channel-tag` run stops at "no build-result
digest" and moves nothing. The follow-up (P5-c) is a yaml change: `buildx --metadata-file`, then each step
writes `<img>:<vX> <digest>` to `$BUILDER_OUTPUT/output`. The reader is in place here. It applies
only to tags cut after that change. Until then, a pushed local build (option 3) is the only path
that moves a channel tag.

## Docs

- `docs/release-process.md`:
  - a stage 2a section with the exact commands for preview and stable, the registry, the record at
    `published/<vX>.json`, "agent-run, never nightly", the recovery line (re-run, or run the
    commands in `<src-dir>/channel-recovery.XXXXXX`) and the known gap;
  - a release-notes-page step for rc and stable tags, to be done before the tag is pushed;
  - the Homebrew section names the "Verify checkout is the release tag" step.
- `install.md`:
  - the Homebrew paragraph qualifies the ghcr pull with `image_tag` or a local build. The
    dev-registry paragraph was dropped in round 1 (R3); P5-b adds it after the first real publish.
- `image-build/README.md` documents the flag, the recovery file and the podman read-back.

## Tests

`make test-image-scripts`: test-build-release-images.sh goes from 284 to 758 passing; the other two
suites are unchanged. The suite against the base driver gives 174 failures.
Seven mutants are each caught:
- the :vX check removed;
- the duplicate step-output check removed;
- preview accepting preview tags only;
- the round-1 `ar_digest` (`| head -n 1` under pipefail);
- any pre-read failure taken as "none";
- the `--also-tag latest` refusal removed;
- the image-name check removed.

Stubs added: gcloud, sleep, and digest aliases in the docker/podman stub (hex
digests with `STUB_HEX`). Cases:

- golden argv for build-images.sh and for the retag / `tags add` calls;
- every refusal, including rc refused for stable and near-miss rc/preview forms refused for both
  channels;
- rc accepted for preview;
- results.images success and buildStepOutputs success over two builds;
- no results, one image missing, a malformed ID (also with trailing junk), a malformed digest
  (short, uppercase, 63 and 65 hex);
- a malformed, unknown or duplicate step line, sources that disagree, and non-base64 output;
- `:vX` that differs from the build digest;
- a failing build (also CANCELLED, TIMEOUT, EXPIRED, INTERNAL_ERROR), both timeouts, and repeated
  describe failures;
- a results.images conflict across builds, step-output digests of 63 and 65 hex, CR and
  leading-space step lines, and gcloud or base64 missing;
- Artifact Registry prefix, suffix, userinfo, port and uppercase forms (refused);
- part-way failures (a tags add failure, a read-back that is absent or mismatches, a docker create
  failure, a podman push failure), each with the moved list and the exact recovery file content
  (a previous digest and "none"), and a re-run that reuses the clone;
- a failed :vX lookup, and a failed or malformed channel pre-read (a stop, nothing moved);
- dry runs.

shellcheck: not installed in the sandbox, so NOT RUN.

## Review

- Round 1, by ri-p5a-rev (REQUEST CHANGES, no Critical). The fixes:
  - R1: `ar_digest` is total, so failures reach the `<unavailable>` and moved-so-far messages
    instead of a silent exit.
  - R2: the docs describe a part-way failure honestly, and the recovery file was added on
    release-manager's request, with ri-em's guard on "absent" versus "failed".
  - R3: the install.md dev-registry paragraph was dropped.
  - S1: the Artifact Registry location grammar.
  - O1-O5 were applied. O6 was declined as code, and documented in the README.
- Round 2, by ri-p5a-rev-2 (APPROVE, no Critical or Required). Small items, taken:
  - O1: the oldest recovery file restores the pre-release state.
  - O2: if the recovery file cannot be created, the commands go to stderr.
  - O3: a podman channel run reads with skopeo only, with no push fallback.
  - O4: the explicit statement that the release tags' Cloud Build configs stop the cloud-build
    path (omni excepted).
  - N1: the README's recovery wording for docker and podman.
  - The `--also-tag latest` refusal.
  - sec's optional hardening (registry and image-name grammar).
  - Base kept at 17ed440f: upstream 17ed440f..2955993c touches none of these files.
- Round 3, by ri-p5a-rev-3 (REQUEST CHANGES, no security findings). The fixes:
  - R1: the SC2016 directive on the literal `$(id)` test value.
  - R2: the recovery-file rule: per tag, use the oldest file that names it.
  - O-a: the OCI separator set in the registry and image-name grammars, with a double-hyphen
    AR path test.
  - O-b: the mut7 fixture resolves the bad name, so removing the check fails the no-retag
    assertion.
  - N1: the recovery lines are asserted on stderr alone.
  - N2: the docs describe the property, not the phase name.
  - N3: a paragraph break in this log.
  - The commit message names the grammar checks and the omni exception.
