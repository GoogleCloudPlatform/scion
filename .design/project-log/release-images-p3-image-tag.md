# release-images Phase 3a: the `image_tag` setting (key, resolver, experiment)

**Date:** 2026-11-05
**Branch:** ri/p3-image-tag-config (ptone/scion#6625, design: release-matched image builds §1c)

## What changed

Phase 3 is split in two pull requests. This first one (3a) adds the setting and
has no runtime effect: nothing reads it to rewrite an image yet.

- `pkg/config/image_tag.go`: `EnvImageTag` (`SCION_IMAGE_TAG`), read only by
  `ImageTagFromEnv` and excluded from the settings env mappers; `ImageTagLayers`
  (the `image_registry` layer type) and `ResolveImageTagLayers`;
  `ValidateImageTag` (empty, `version`, or the docker tag grammar
  `[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}`); `EffectiveImageTag` (`version` becomes
  the given binary version, `latest` for `dev`, empty, or a version that is not
  a valid tag); `RewriteImageTag` and `RewriteImageRef`.
- Rewrite rule: only a short-form `scion-*` reference whose tag is `latest` or
  absent takes the tag. An explicit other tag, a digest, a fully qualified
  reference and a non-`scion-*` image are left alone. `RewriteImageRef` applies
  the tag first, then the registry, and also retags the registry rewrite's own
  output (`<registry>/<name>`), so it is stable when applied twice. With no
  tag, it is byte-identical to `RewriteImageRegistry` (tested on a grid of
  images and registries).
- `image_tag` on `VersionedSettings` and `V1ProfileConfig`, in the settings
  schema (with a pattern), in `scion config set/get/unset` (an invalid value,
  including one with surrounding whitespace, is refused by the canonical
  pre-write check `ValidateSettingValue`, before any read, lock or legacy-file
  migration), and in `VersionedSettings.ImageTagLayers`
  (file profile and top; no hub database layer, which the hub adds in 3b).
- The hub profiles settings section schema lists `image_tag` (with the same
  pattern, `config.ImageTagSettingPattern`), because that schema must match
  `V1ProfileConfig` field for field. Nothing reads the stored value until 3b.
- Flat instances have no `image_tag`; the profile converter reports a profile
  value as a `legacy_field_no_effect` notice instead of dropping it silently.
- Experiment `hub.image_tag_setting` (server layer, off by default), its
  registration test, and its test-admin policy entry (every registered
  experiment needs one, or `TestTestAdminPolicy_Complete` fails).
- The missing-registry hint links the README section "Build images for a
  release" (`image-build/README.md#build-images-for-a-release`), which the
  build-script phase adds.
- Docs: the experiments reference, `settings-precedence.md` ("Image tag
  (experimental)") and the settings reference rows.

## Decisions and deviations from the design

- **Env layer:** `SCION_IMAGE_TAG` is read from the hub process only. For
  `image_registry`, a Runtime Broker's own environment beats every setting;
  for `image_tag` it is not a layer. The hub resolves the tag (and expands
  `version` with its own version) and sends the result with each dispatch, so
  no new broker capability field is needed and the hub alone decides.
- **Where the tag is applied (3b):** most images are resolved by the Runtime
  Broker from the template or harness config, so the hub cannot hand the
  broker a final reference. 3b sends the resolved tag as an optional dispatch
  field. An older broker ignores the field and runs unchanged references.
- **Experiment off (the refusal and warning land in 3b):** the setting is
  ignored and references are unchanged. A hub settings save that carries
  `image_tag` is refused (409, naming the experiment); a settings-file or CLI
  value cannot see the hub's experiment state, so it is accepted and the hub
  warns once at startup.
- **Out of scope:** flat instance `agent_defaults.image_tag`, and showing the
  tag in doctor or runtime output.

## Learnings

- Registering an experiment requires an entry in the test-admin policy map in
  `pkg/hub`, so even a config-only change touches one hub file.
- The profile converter's finding codes are frozen; a new field without a flat
  counterpart reuses `legacy_field_no_effect`.

## Follow-ups

- Landing order: this change links
  `image-build/README.md#build-images-for-a-release`, which the build-script
  phase adds. Land it only after that README section is on `main`.
- 3b reinstates, with the code, the docs and experiment-description claims
  that the hub refuses an `image_tag` save while the experiment is off and
  warns at startup about a settings-file value. It also removes every
  "not yet applied" / "follow-up change" caveat:
  - the `scion config set` help text (`cmd/config.go`);
  - the missing-registry hint (`cmd/image_registry_presence.go`);
  - the experiment description and comment (`pkg/experiments/registry.go`);
  - `experiments.md`, `settings-precedence.md`, and the two `image_tag` rows
    of `orchestrator-settings.md`;
  - the top-level and profile `image_tag` descriptions in
    `settings-v1.schema.json`;
  - the package comment in `pkg/config/image_tag.go`.
- Declined in review: moving the closing caveat of the "Image tag
  (experimental)" section of `settings-precedence.md` up next to its heading.
  The closing caveat and the 3b list above already cover that section, and
  moving the text adds churn; 3b rewrites the section anyway.
