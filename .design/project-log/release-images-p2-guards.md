# release-images Phase 2: `build-images.sh` guards (ptone/scion#6625)

Design: release-matched image builds (approved; Layer 1, §1b and §9 Phase 2).
Branch `ri/p2-build-images-guards`, stacked on `ri/p1-release-images-driver`
at `454956b`. P1 is not on upstream `main` yet.

## What changed

- **`image-build/scripts/build-images.sh`**
  - **Base guard** (`check_required_base`). `scion-base` and `common` need
    `core-base:<tag>`, or `thick-prep:<tag>` when `THICK_BUILD=true`.
    `harnesses`, `hub`, `omni` and single images need `scion-base:<tag>`.
    `all`, `core-base`, `thick-prep` and `thick` need no base. This is the
    driver's pre-flight mapping. If the base is found neither in the selected
    builder's local store nor in the registry, the script exits 1 before
    `builder_prepare`, so nothing is built. `--allow-stale-base` builds anyway
    and prints a warning. `--dry-run` only warns.
  - **Tag warning** (`warn_if_release_tag_not_used`). It fires when HEAD is
    exactly on a tag and `--tag` was not passed. An explicit `--tag latest`
    silences it, so `scripts/single-node-vm/deploy.sh`, which builds a tag
    checkout with `--tag latest`, gets no new noise. The default stays
    `latest`.
  - **Commit-time `BUILD_TIME`.** `SOURCE_DATE_EPOCH` is set from
    `git log -1 --format=%ct` unless the caller already set it (it must be an
    integer). It is exported, so buildx and BuildKit stamp image timestamps
    with it, and formatted to `BUILD_TIME` with GNU `date -d @` or, on macOS,
    BSD `date -r`. The banner prints `Version:` and `Built at:`.
- **`lib/targets.sh`**: `BUILD_TIME=` build-arg for `scion-base` and
  `scion-omni`. These are the only two Dockerfiles that compile Go and declare
  `ARG BUILD_TIME`.
- **`builders/cloud-build.sh`**: passes `_BUILD_TIME` when it is non-empty and
  the config references it. This follows the `_VERSION` rule.
- **`cloudbuild{,-common,-scion-base,-thick,-omni}.yaml`**: add
  `--build-arg BUILD_TIME=$_BUILD_TIME` to every scion-base and omni build
  step, and set the default `_BUILD_TIME: ''` (empty means the Dockerfile
  falls back to `date`).
- **`scion-base/Dockerfile`**: `ARG VERSION` plus
  `LABEL org.opencontainers.image.version="${VERSION}"` in the final stage,
  next to the revision label.
- **`image-build/scripts/test-build-images.sh`** (new), wired into
  `make test-image-scripts`.
- **`image-build/README.md`**: Tagging (the tag warning), "Base guard and
  `--allow-stale-base`", "Build time and version label", and the
  `docker inspect` line for the version label.

## Decisions and why

- **The version label goes in the Dockerfile, not in builder `--label`
  flags.** The revision label already lives in the final stage of
  scion-base, and every image that stacks on it (harnesses, hub, omni)
  inherits it. Every Cloud Build config already passes `VERSION` to
  scion-base, so the label needs no yaml change. A builder `--label` would
  have missed cloud-build, or would have needed an edit to every harness step
  in every yaml. It would also have split the label from its revision pair.
  core-base and thick-prep carry no scion code and have no revision label, so
  they get no version label either. `cloudbuild-hub-gke.yaml` builds the
  repo-root Dockerfile, which `build-images.sh` does not drive and which has
  no revision label, so it is unchanged (adjacent cleanup).
- **Where BUILD_TIME was derived.** The `date` fallbacks are in
  `scion-base/Dockerfile` and `omni/Dockerfile`. They are kept for direct
  `docker build`s, and the orchestrator now always supplies the value. The
  builders never derived it. `hack/version.sh` and
  `.github/workflows/build-release.yml` stamp release binaries, not images,
  so they are out of scope.
- **The tests go in a new file.** `test-build-release-images.sh` drives a
  stub `build-images.sh`. These tests have to run the real one against a
  fixture repo with a fixed commit date, so their harness is different.
  `IMAGE_BUILD_UNDER_TEST=<dir>` runs the suite against a mutated copy of
  `image-build/`.
- **The base lookup depends on the builder.** cloud-build cannot see the
  local store, so only the registry counts there. Probes: `docker buildx
  imagetools inspect`, then `gcloud artifacts docker images describe` if
  docker is missing. A Cloud Build config that builds the base itself is not
  checked. `cloudbuild-omni.yaml` rebuilds from thick-prep, so `omni` on
  cloud-build needs no `scion-base:<tag>`. The P1 driver still requires one.
  local-podman uses podman's store and `podman manifest inspect`.

## Findings worth keeping

- **`scion-base` and `omni` already declared `ARG BUILD_TIME`, but nothing
  passed it.** Every image build stamped the build host clock.
- **The tag warning uses the same `git describe --tags --exact-match` as
  VERSION.** On a commit with several lightweight tags (see the P1 log), it
  can suggest the nightly name. That matches what the binaries are stamped
  with, and the driver avoids the ambiguity with `--no-tags`.
- **The P1 driver contract still holds.** `tree_is_stamped` (`describe --tags
  --exact-match` plus `^ARG VERSION`) and `tree_has_step_targets` still match,
  and the new suite checks this too. No flag the driver forwards changed.
  `--allow-stale-base` is new, and the driver does not forward it.
- `check-harness-coverage.sh` needs PyYAML, which was not installed in this
  sandbox.

## Round 2 (review ri-p2-rev-1)

- **R1-F1: `--push` ignores the local store.** A `--push` or multi-platform
  build runs on the docker-container buildx instance, which pulls
  `BASE_IMAGE` from the registry. A local hit used to satisfy the guard
  anyway, which kept the #2250 class open. The local store now counts only
  for local, non-push per-image builds, the same rule as
  `warn_if_scion_base_not_in_run`.
- **R1-F2: lookup errors are not "not found".** Each lookup is classified as
  found, a definite not-found (`not found`, `NOT_FOUND`, `No such image`,
  `image not known`, `manifest unknown`), or an error. The base is missing
  only when at least one lookup answers not-found and none finds it. If every
  lookup errors, the build stops with "could not be checked (<tool>:
  <error>)" and a fix hint; `--allow-stale-base` opts out. cloud-build asks
  `gcloud artifacts docker images describe` first, since builder_check
  guarantees gcloud and it carries the identity the build itself uses, and
  then asks docker. Choice: fail closed with a distinct message, not fail
  open. Reasons: the guard exists to stop surprises, the message names the
  failing tool and its error, and there is an explicit opt-out. Risk to
  watch: a scheduled cloud-build run whose gcloud identity can submit builds
  but cannot read Artifact Registry would now stop. Its message points to
  credentials or `--allow-stale-base`.
- **R1-F3:** in per-image mode, single steps use `step_parent`, so
  `scion-omni` under `OMNI_BUILD` needs `scion-grok-build`. Target mode
  follows the static configs and ignores an exported
  `THICK_BUILD`/`OMNI_BUILD`: scion-base and common need core-base, single
  harnesses need scion-base.
- **R1-F4:** a test runs build-images.sh with a BSD-style `date` (no `-d`,
  `-r <epoch>`).
- **R1-F5:** the README scopes the SOURCE_DATE_EPOCH image timestamps to
  local-docker. Cloud Build gets only BUILD_TIME. CREATED shows the commit
  age.
- **F6:** the review's m16 (rc=2 fails open on real builds) is equivalent,
  because builder_check guarantees a lookup tool. The rc=2 path is now
  reachable through lookup errors, and a test covers it.

## Round 3 (EM finding EM-P2-1)

- **`gcloud artifacts docker images describe` needs more than Artifact
  Registry read.** It also lists Container Analysis occurrences. ri-em ran it
  as the service account behind the scheduled cloud-build rebuild, and it
  failed with "Permission 'containeranalysis.occurrences.list' denied". The
  round-2 guard would have classified that as a lookup error and stopped the
  scheduled `--builder cloud-build --target common` run, since that host has
  no docker to fall back on.
- **The probe is now `gcloud artifacts docker tags list <registry>/<image>
  --format='value(tag)'`**, which needs only Artifact Registry read
  (`probe_gcloud_tag`). The tag must equal a listed line exactly, as a bare
  name or as the last element of the tag's resource name (`.../tags/<tag>`).
  `--filter='tags:<tag>'` was rejected because it matches substrings: `:v1`
  would be satisfied by `:v10`, and `:latest` by `:latest-old`.
- **Classification.** rc 0 with no exact match, including an empty list, is a
  definite not-found. rc != 0 goes through the shared classifier: NOT_FOUND
  is not-found, anything else (such as PERMISSION_DENIED) is a named lookup
  error. The image and tag are split at the last ':', so a registry with a
  port works.

## Round 4 (review ri-p2-rev-2; coordinator-2's required property)

Requirement: a lookup that fails because of a permission, API, network or
tool problem must not stop a pipeline that works today, under any identity.

- **R2-F1: warn and go ahead on a lookup error.** Only a DEFINITE not-found
  stops the build. Otherwise the guard prints "could not be checked (<tool>:
  <error>)" as a warning and the build proceeds, as it did before the guard
  existed. Lookups can fail where the build succeeds, because they run under
  a different identity or path:
  - cloud-build lookups run as the submitter, while the build runs as the
    Cloud Build service account;
  - imagetools uses client-side config, while a non-push docker build pulls
    through the daemon (insecure registries, mirrors, a daemon-side proxy);
  - an old gcloud may lack the `artifacts` group, or the AR API may be
    disabled in the quota project.

  `--strict-base` is an opt-in hard gate for CI. `--allow-stale-base` now
  skips the check entirely. build-release-images.sh forwards neither flag
  (§11 base flag contract). That is acceptable because the default no longer
  blocks on lookup errors, and the driver keeps its own pre-flight.
- **R2-F2(a): per-tool not-found patterns.** docker store `No such image`,
  podman store `image not known`, imagetools/podman manifest `manifest
  unknown` or a trailing `: not found`, gcloud `^ERROR: ... NOT_FOUND`, or an
  exact-match miss in the tag list. Credential and tool failures are checked
  first and are always errors (`executable file not found`, `command not
  found`, `credential`, `unauthorized`, `denied`, `forbidden`,
  `permission`). This covers docker's credential-helper messages, both
  `exec: "docker-credential-gcloud": executable file not found in $PATH` and
  the shell form `docker-credential-gcloud: not found`. A network "host not
  found" stays an error.
- **R2-F2(b):** when a registry is set, only a registry lookup's not-found
  is definite. A local miss plus a registry error is a lookup error.
- **R2-F3:** the detail shows the first ERROR or fatal line, not a leading
  gcloud WARNING.
- **R2-F4:** empty tag-list lines are skipped, so `--tag ''` never matches.
- **Probe timeout: declined.** `timeout` is not portable (macOS lacks it).
  With R2-F1, a hung lookup is visible to the operator, who can interrupt it,
  and the build itself would hang the same way.
- The tag list grows with each :<short-sha> tag, but retention bounds it and
  the match is exact, so no action is needed.

## Round 5 (review ri-p2-rev-3: APPROVE; cleanup pass)

- **R3-F1:** local-podman cases for a store miss, a store lookup error, the
  same with --strict-base, a local miss plus a registry error, a registry
  not-found, and --push with a credential error. The podman stub now prints
  podman's own messages: `Error: <ref>: image not known` and `Error: reading
  manifest <tag> in <repo>: manifest unknown`.
- **R3-F2:** the store-miss rule is now `store_missing && -z REGISTRY`. The
  `-z BASE_CHECK_DETAIL` clause was dead and would have contradicted "a
  local miss alone proves nothing about the registry".
- **R3-F3:** tests cover a gcloud crash that says "host not found" without
  NOT_FOUND (a lookup error) and a lookup error under --dry-run without
  --strict-base (goes ahead).
- **R3-F4, gcloud `tags list` rc 0 rule:** exact match means found. Zero
  bytes of output is a definite not-found, because an image with no tags
  lists nothing. Output with no non-blank line is a lookup error, an output
  shape the script does not understand. Non-blank lines with no exact match
  are a definite not-found. stdout now goes to a file, because `$(...)`
  strips trailing newlines and would hide the blank-only case, and a last
  line without a trailing newline is still read.
- **R3-F6 hardening, taken:** the classifier greps here-strings instead of
  `printf | grep`, so pipefail plus an early grep exit cannot skip the
  credential-first check.
- **FYI, no action.** R3-F5: `: not found$` would also match a hypothetical
  non-credential helper (`sh: 1: <tool>: not found`), and a registry that
  masks authorization failures as 404 would read as definite. Neither is
  known for AR, gcloud or Docker Hub; GHCR is unverified. An AR remote or
  virtual repository lists only cached tags, and Scion's registries are
  standard repositories.

## Round 6 (review ri-p2-rev-4)

- **R4-F1: gcloud with user output disabled.** With
  `core/user_output_enabled=false` (config or
  `CLOUDSDK_CORE_USER_OUTPUT_ENABLED=false`), gcloud prints nothing and
  exits 0, which read as "no tags" and so as definitely missing. The probe
  now passes `--user-output-enabled`, which overrides both the property and
  the environment variable.
- **R4-F4:** rc 0 with empty stdout but an `ERROR` line on stderr is now a
  lookup error.
- **R4-F2:** one trailing CR is stripped per tag line, so the CRLF output of
  a Windows gcloud still matches.
- **R4-F3:**
  - a glob-looking `--tag 'v*'` matches literally only;
  - whitespace-only output (` \n\t\n`) is the blank-only lookup error;
  - probe temp files are removed on every path, and a test checks that none
    leak.
- **The rc 0 rule now reads:** zero bytes of stdout and no stderr ERROR
  means a definite not-found; zero bytes with a stderr ERROR, or only
  whitespace lines, means a lookup error; an exact match on a line with the
  CR stripped means found; any other non-blank lines mean a definite
  not-found.

## Round 7 (review ri-p2-rev-5, plus coordinator-2's addendum)

- **R5-F1 + addendum: error-first precedence, made general.** The classifier
  reads `PROBE_ERROR_MARKERS`, one documented list covering:
  - credentials and permissions;
  - timeouts, network, DNS and TLS failures, connection refused or reset;
  - HTTP 429 and 5xx;
  - a crashed tool or Traceback, an unreachable docker or podman daemon;
  - multi-source "Mirrors also failed" messages.

  If any marker appears, the lookup is a lookup error (warn and go ahead),
  for every backend, even when not-found text appears too. Reason: podman
  with a registries.conf mirror reports "(Mirrors also failed: [<mirror>:
  manifest unknown]): <primary>: <timeout|503|429>", and the mirror's
  "manifest unknown" used to make that a definite not-found, which stopped
  the build.
- **Rule for a mirror message whose primary also says not found:** still a
  lookup error. A multi-source message is never a definite answer, because
  which source is authoritative cannot be told safely. The safe direction is
  warn and go ahead, which is pre-P2 behaviour: the build's own pull fails
  if the image is really missing.
- **R5-F2:** the gcloud lookup runs with
  `CLOUDSDK_CORE_SHOW_STRUCTURED_LOGS=never
  CLOUDSDK_CORE_CONSOLE_LOG_FORMAT=standard` (an env prefix on that call
  only) and `--verbosity=error`. Structured JSON logs, a timestamped console
  format or verbosity none in the user's config no longer hide NOT_FOUND.
- **R5-F3:**
  - a NOT_FOUND only in a WARNING line, with a different ERROR, is a lookup
    error (R12);
  - CR-only output is the blank-only lookup error (R14);
  - PERMISSION_DENIED next to NOT_FOUND is a lookup error (R20).
- **Tests:** a mixed (not-found plus error) message for each backend, both
  through the guard and as direct classifier calls; each marker beats
  "manifest unknown"; three podman mirror shapes (timeout, 503, 429) plus
  mirror-and-primary not-found; gcloud user configs (structured, detailed,
  quiet) that must still yield a definite NOT_FOUND.
- **FYI, no action.** R5-F4: buildx imagetools with buildkitd mirrors
  probably returns only the last host's error (containerd's resolver does
  not aggregate). Unverified, and any error marker in its output now wins
  anyway. R5-F5: the masked-404 and `sh: <tool>: not found` notes are
  unchanged and still theoretical.

## Round 8 (review ri-p2-rev-6; one bounded fix round ruled by coordinator-2)

- **R6-F1: `log.showSignature` stopped every build on a signed HEAD.** git
  printed "gpg: Signature made ..." / "No signature" on stdout before `%ct`,
  the integer check failed, and the script exited 1. The call is now
  `git --no-pager -C "$REPO_ROOT" log -1 --no-show-signature --no-color
  --format=%ct`. A git-derived value that is still not an integer warns
  ("could not read the commit time (git log printed '...'); using the build
  clock.") and falls back to the build clock (pre-P2 behaviour): no
  BUILD_TIME build-arg, and SOURCE_DATE_EPOCH not exported. The hard error
  stays only for an explicit SOURCE_DATE_EPOCH from the environment.
- **R6-F2:** `classify_probe_failure` gets the looked-up ref, and `mask_ref`
  masks it before the marker and not-found greps, as literal text via awk's
  index(): the full ref, the image path, `:<tag>`, and podman's `manifest
  <tag> `. A tag like `permission-fix` or `timeout-fix`, or a path like
  `credentials-prod`, therefore cannot pass for an error marker. A real
  marker outside the ref still wins, including for a tag literally named
  `timeout`, and a registry port is not taken for a tag.
- **R6-F3:**
  - Two redundant alternatives were removed from PROBE_ERROR_MARKERS: `5xx
    (phrase)`, already covered by the bare phrases, and `Gateway Timeout`,
    which contains `timeout`. Removing them changes nothing semantically.
  - `Cannot connect to (the )?(Docker|Podman)` now also matches podman's
    real "Cannot connect to Podman".
  - Each of the 32 remaining alternatives has an isolated test string.

### Git call sweep (every git call in the scripts P2 touched)

lib/targets.sh, builders/*.sh and the Makefile `test-image-scripts` target
make no git calls. The Makefile's other git lines (ent-check) are outside
P2. In build-images.sh:

| Line | Call | Output used | Verdict |
|---|---|---|---|
| 260 | `rev-parse --is-inside-work-tree` | exit code only | Robust. Output is discarded. |
| 261 | `rev-parse --short HEAD` | the :<short-sha> tag | Robust. Plumbing; no pager, color, signature or format key applies. `core.abbrev` may change the length, which is still a valid tag and unchanged since before P2. |
| 262 | `rev-parse HEAD` | GIT_COMMIT build-arg | Robust. Plumbing, full SHA. |
| 269 | `describe --tags --exact-match` | VERSION | Robust (P1 line, unchanged). describe prints the tag name only; showSignature, color, pager and formats do not apply. Multiple lightweight tags on a commit are covered in the P1 log. |
| 280 | `log -1 --format=%ct` | SOURCE_DATE_EPOCH | FIXED. showSignature → `--no-show-signature`; color.ui=always → `--no-color`; pager → `--no-pager` (and stdout is not a tty); `format.pretty`, `log.date` and `log.decorate` are overridden by `--format`; notes are not printed with a custom format; a non-integer anyway falls back with a warning. |

Applies to every call:
- Aliases: git does not allow an alias to shadow a built-in command.
- External diff and diff drivers: no call produces a diff.
- Locale (LC_ALL / LANG): the parsed outputs are an exit code, hex SHAs, a
  tag name and a decimal epoch, none of which are localised; errors go to
  stderr, which is discarded.
- `safe.directory` ownership refusal: the first call fails and no git
  metadata is used, unchanged since before P2.

Pinned by a test: a git shim that behaves like log.showSignature=true plus
color.ui=always unless the call opts out, which must still yield the commit
time. A shim printing garbage even with the flags must warn and fall back.

Mutation notes for round 8:
- 144 killed out of 144, including one drop per marker alternative (A01-A32
  plus the sub-alternatives) and the reviewer's M07/M11/M12/M16/M17/M18/M20.
- A full-ref masking step was removed because it was redundant: masking the
  image path and `:<tag>` already covers the full ref.
- One equivalent mutant is left out: treating a registry port as a tag
  (`${ref}` instead of `${ref##*/}` in mask_ref). That can only differ for a
  ref with a port and no tag, and there the `:<tag>` replacement masks the
  same text.
