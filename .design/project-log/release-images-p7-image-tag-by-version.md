# release-images Phase 7: a release build pulls its images by version tag

**Date:** 2026-11-08
**Issue:** ptone/scion#6625 (decision: "a versioned scion should pull its matching images by version tag")
**Base:** upstream main 323b539e (contains P5-a #4655 and P2-FU3)

## What changed

Phase 3 made `image_tag` an opt-in setting behind `hub.image_tag_setting`
(off by default); unset meant no rewrite, so every build pulled `:latest`,
which in `ghcr.io/homebrew-scion` is an old v0.2 image. This change gives
the **unset** value a default.

- **One resolution point:** `config.EffectiveImageTag(setting, version)`.
  - unset: the release version (`config.ReleaseImageTag`: `vX.Y.Z`,
    `vX.Y.Z-preview.N`, `vX.Y.Z-rc.N`), else `""` (no rewrite);
  - `version`: the release version, else `latest`;
  - any other value: the literal (explicit always wins).

  `ReleaseImageTag` is the v-branch of `hack/version.sh`'s release-tag rule
  without the nightly form, so `""`, `dev`, `nightly-*`, Go pseudo-versions
  and `-dirty` / `+meta` suffixes keep `:latest`.
- **Hub gate** (`pkg/hub/image_tag.go`, `imageTagStateOf`): a set value
  needs `hub.image_tag_setting` (unchanged); an unset value needs the new
  `hub.image_tag_version_default` (server layer, alpha, **on by default**).
  A set value while `hub.image_tag_setting` is off applies nothing and also
  suppresses the default, so `image_tag: latest` is a working opt-out
  without enabling the setting experiment. `dispatchImageTagFor` skips its
  broker lookup only when both experiments are off.
- **Nothing writes the default into settings**: it is computed at
  resolution time on every read. Every image path already went through
  `imageTagStateOf` (dispatch, harness-config checks and pulls, the
  system image pull, reincarnation); the Runtime Broker only applies the
  wire tag. The workstation server runs the same Hub code, so a Homebrew
  (solo) user gets the default and can turn it off locally.
- **Rulings** (release-manager): R1 default-true experiment gating only the
  unset default; R2 dirty builds documented, no build-script change
  (`hack/version.sh` stamps a tagged dirty tree with the tag; noted in the
  `pkg/config/image_tag.go` comment and `docs/release-process.md`); R3
  `rc.N` form, and `version` on a nightly resolves to `latest`.
- **Homebrew formula caveat:** the ":latest is an old v0.2 image" caveat is
  removed in the same change (`scripts/release/update-homebrew-formula.sh`,
  its golden fixture and its test); the caveat keeps the release-tagged
  image example and says this version pulls by its own tag.
- **Docs:** install.md (Homebrew paragraph plus a note: `--image NAME:latest`
  and bare `scion-*` names are retagged, `image_tag: latest` opts out, how to
  disable the experiment), settings-precedence "Image tag", custom-images,
  experiments reference, orchestrator-settings, image-build/README.md,
  docs/release-process.md, the settings schema descriptions and CLI help.

## Tests

- `pkg/config`: `TestEffectiveImageTag` (one row per case: release,
  preview, rc; dev, empty, dirty, nightly, pseudo; explicit latest and
  older tag; `version`), `TestReleaseImageTag`.
- `pkg/experiments`: `TestImageTagVersionDefaultExperimentRegistered`.
- `pkg/hub`: `TestImageTag_VersionDefault`,
  `TestImageTag_ExplicitValueWinsOverVersionDefault`,
  `TestLogImageTag_VersionDefault`; `test_admin_policy.go` classifies the
  new experiment.

## Notes for the next change

- Remote Runtime Brokers of another version still receive the hub's tag
  (the hub alone decides, by design).
- Maintenance image builds keep `latest`; digest, non-latest and fully
  qualified references are left alone.
