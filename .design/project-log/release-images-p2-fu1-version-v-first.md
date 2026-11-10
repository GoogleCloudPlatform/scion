# release-images P2 follow-up: VERSION from the v* tag (ptone/scion#6625)

Follow-up to Phase 2 (#4622). Branch `ri/p2fu1-version-v-first`, based on
upstream `main` `87ac072` (which contains the #4622 merge `271277c`).

## What changed

- **`image-build/scripts/build-images.sh`**: a new helper,
  `exact_tag_version <repo>`, prints the tag HEAD is exactly on, or nothing.
  It uses the same rule as `hack/version.sh`: first
  `git describe --tags --exact-match --match 'v*'`, then any exact tag, then
  `|| true`, so off-tag `VERSION` is still empty and the binaries still report
  `dev`. `VERSION` comes from that helper. The HEAD-on-tag warning
  (`warn_if_release_tag_not_used`) reads that same `VERSION`, so it now names
  the v* tag too. The comment that said "Same convention as hack/version.sh"
  now states the v*-first rule.
- **`image-build/scripts/test-build-images.sh`**: four new fixture repos and
  a section "VERSION: a v* tag wins over a nightly tag on the same commit":
  - `BOTH`: a lightweight nightly tag and a lightweight v* tag on HEAD.
  - `ANNOTATED`: an annotated nightly tag and a lightweight v* tag on HEAD.
  - `NIGHTLY`: only a nightly tag on HEAD.
  - `PAST`: the v* tag is one commit behind HEAD.

  The tests check the banner, the `VERSION` build-arg, the cloud-build
  `_VERSION` substitution and the warning text. The suite already runs under
  `make test-image-scripts`.

## Findings worth keeping

- **Exact match picks the nightly tag over the v* tag.** On one commit,
  `git describe --tags --exact-match` names the first lightweight tag in
  refname order, so `nightly-*` beats `v*`. It also prefers an annotated tag
  over a lightweight one, so an annotated nightly tag beats a lightweight v*
  tag regardless of order. `--match 'v*'` first fixes both cases (the
  `ANNOTATED` fixture covers the second).
- **Prove-it.** At `87ac072` the new suite fails 8 checks: every `BOTH` and
  `ANNOTATED` case. A mutation that drops the any-tag fallback fails the 3
  `NIGHTLY` checks. `PAST` guards that `--match` did not drop `--exact-match`.
- **The P1 driver still detects the tree.** `build-release-images.sh`
  `tree_is_stamped` greps `build-images.sh` for `describe --tags
  --exact-match`, and the helper still contains that string. The suite's
  `driver: tree_is_stamped` check still passes.
- **A redundancy left for later.** The driver's own `check_tree` uses a plain
  `describe --tags --exact-match`. It clones with `--no-tags` and requires the
  name to equal `<vX>`, so it is still correct, but its "another tag on the
  same commit" guard is now partly redundant. This change leaves it alone.
