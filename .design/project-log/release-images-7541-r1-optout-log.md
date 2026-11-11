# release-images 7541 round 1: the image_tag opt-out logs at Info

**Date:** 2026-11-08
**Issue:** ptone/scion#7541 (follow-ups deferred from Phase 7), items O1 and N2
**Base:** upstream main 643a4407 (the edits were made on Phase 7, 9068092d; the changed paths are blob-identical at both)

## What changed

- **O1, startup log level** (`pkg/hub/image_tag.go`,
  `warnImageTagWithoutExperiment`): with `hub.image_tag_setting` off, a set
  `image_tag` applies no tag and suppresses the version default, so
  `image_tag: latest` has exactly its documented effect: agent images keep
  `:latest`. The hub used to warn at every start that the value is ignored,
  which reads as a misconfiguration for the endorsed opt-out. When every set
  value (hub env, settings-file top level and profiles, saved hub profiles;
  trimmed) is `latest`, the hub logs the opt-out at Info instead. Any other
  value, alone or next to `latest`, keeps the warning unchanged.
  `config.ImageTagLatest` is exported so the hub does not repeat the
  literal.
- **N2, registry wording** (`pkg/experiments/registry.go`): the
  `hub.image_tag_setting` Description said `version` gives `latest` "for a
  development build"; it gives `latest` for any non-release build, nightly
  included, matching the experiments docs page. The const comment and the
  `config.ImageTagLatest` comment say the same.
- **Docs**: the experiments page and Settings precedence ("Image tag") say a
  value other than `latest` is reported with a startup warning, and `latest`
  is logged at Info.

## Tests

`TestLogImageTag_ExperimentOffLevel` (pkg/hub): four opt-out rows (file
top level, hub env, file profile, saved hub profile) assert the Info line
and no warning; three rows (a non-latest value, `latest` next to a
non-latest file profile, and `latest` next to a saved hub profile `v1`)
assert the warning and no opt-out line. Level and message are asserted
together.

## Notes

- Out of scope for this round: O2 (hub test rows through a broker or
  profile) and the upgrade-path test.
- `golangci-lint` over `./pkg/hub/...` type-checks the whole pkg/hub test
  package, so it runs in fork CI rather than in a local sandbox window.
