# release-images P2 follow-up: VERSION accepts only release tag forms (ptone/scion#6625)

Second follow-up to Phase 2 (#4622). Branch `ri/p2fu2-tag-validate`, stacked
on the P2-FU1 head `5b89d46` (`ri/p2fu1-version-v-first`, on upstream `main`
`87ac072`).

## What changed

- **One release-tag rule, in two scripts.** `hack/version.sh` and
  `image-build/scripts/build-images.sh` carry the same block,
  `release_tag_or_empty`, between `# BEGIN release-tag rule` and
  `# END release-tag rule`. It accepts `vX.Y.Z`, `vX.Y.Z-preview.N`,
  `vX.Y.Z-rc.N` (the pattern of `scripts/release/update-homebrew-formula.sh`)
  and `nightly-YYYYMMDD`.
  - Any other tag is treated as a non-release tag, the same as being off-tag:
    `VERSION` stays empty and the binaries report `dev`.
  - One warning goes to stderr. It names the tag only when the tag uses
    nothing but `[A-Za-z0-9._-]`; otherwise it says "a tag with unexpected
    characters".
  - Neither script fails on such a tag, so a local tag cannot break
    `make build`.
  - `exact_tag_version` keeps the v*-first lookup and passes its result
    through the rule. `hack/version.sh` does the same after its describe
    chain.
- **Release callers fail instead of building "dev" images.**
  - `image-build/scripts/build-release-images.sh`: in tag mode, on a tree
    that stamps versions, `tree_stamps_version` runs the tree's own
    `hack/version.sh`. If it does not print `Version=<tag>`, the driver stops
    before the build starts.
  - `.github/workflows/publish-homebrew.yml`, step "Verify checkout is the
    release tag": after the existing `git describe` check, it runs
    `hack/version.sh` and fails unless it stamps exactly `TAG`. The gate's
    shape check accepts leading zeros (for example `v1.2.3-preview.04`),
    which the rule rejects.
- **Tests** (both suites run under `make test-image-scripts`):
  - `test-build-images.sh`:
    - The two shared blocks must be identical.
    - Accepted forms (`v1.2.3`, `-preview.4`, `-rc.1`, `v10.0.0`,
      `nightly-20261106`) are stamped: banner, `VERSION` build-arg,
      cloud-build `_VERSION` and `hack/version.sh` output.
    - Rejected forms (a tag with a comma, `v1.2.3;x`, `v1.2`, `v1.2.3-beta.1`,
      `v01.2.3`, `nightly-2026110`, `latest`) give exit 0, the off-tag
      banner, no `VERSION` build-arg, no `_VERSION=` and no
      `hack/version.sh` `Version=`, plus the expected warning text.
    - The publish-homebrew step is extracted from the workflow and run as
      written: it passes for `v1.2.3-preview.4` and fails for
      `v1.2.3-preview.04`.
  - `test-build-release-images.sh`: the fixture upstream carries the real
    `hack/version.sh`. A stamped side-branch tree tagged `v1.2.0-beta.1` and
    `v1.2.0-preview.01` makes the driver exit 1, and the build stub never
    runs.

## Release callers checked

| Caller | Check that the stamped version equals the release tag |
|---|---|
| `image-build/scripts/build-release-images.sh` | Added: `tree_stamps_version`, a hard failure in tag mode. |
| `.github/workflows/publish-homebrew.yml` (images job) | Added to "Verify checkout is the release tag". |
| `.github/workflows/build-release.yml` | Already there: "Verify binary version" fails unless `scion version` reports the tag. Its `VERSION` is the tag itself, not the helper. |
| `scripts/single-node-vm/deploy.sh` | Covered by P4 (deploy.sh calls build-release-images.sh); not changed here; interim gap until P4 lands. |
| `.github/workflows/build-images.yml`, `image-build/scripts/trigger-cloudbuild.sh`, the hub's image build (`pkg/hub/system_handlers.go`), the `Makefile` | Not release paths: manual or local builds with no release tag to match. |

## Findings worth keeping

- **Prove-it.** Against the `5b89d46` versions of the changed scripts:
  - the driver suite fails the 6 new checks (and the 6 again in its
    isolation re-run);
  - `test-build-images.sh` fails 61 new checks;
  - the `5b89d46` publish-homebrew step passes `v1.2.3-preview.04`;
  - an older `hack/version.sh` next to the new `build-images.sh` fails the
    rule-identity check.
- **Older trees still work with the new driver.** Every tree from
  v0.3.0-preview.4 on has `hack/version.sh`, which accepts the package as
  `$1`, and its describe rule matches that tree's `build-images.sh`.
  Unstamped trees skip the check and print the existing warning.
- **The rule is a choice about the tag that was found, not a search.** When
  the v* lookup finds a malformed v* tag (say `v1.2`) on a commit that also
  carries a valid nightly tag, the result is non-release; the nightly is not
  tried. This keeps one lookup and one decision.
