# release-images Phase 3b: applying the `image_tag` setting

**Date:** 2026-11-05
**Branch:** ri/p3b-image-tag-apply (ptone/scion#6625, design: release-matched image builds §1c, §6, §7, §9 Phase 3)
**Base:** upstream main 9728c7dc (contains Phase 1 and Phase 3a)

## What changed

Phase 3a added the `image_tag` key, the resolver and the rewrite helpers.
This change applies the tag, behind `hub.image_tag_setting` (off by default).

- **Hub resolution** (`pkg/hub/image_tag.go`): hub env `SCION_IMAGE_TAG` >
  hub DB profile (a managed `profiles` row) > settings-file profile >
  settings-file top level. `version` becomes the hub's `pkg/version`
  (`latest` for a dev build). The tag is empty while the experiment is off.
  An agent resolves with its profile, else its broker's registered default
  profile, else at hub scope.
- **Carried to the broker:** `api.HubAgentDefaults.ImageTag` (wire
  `imageTag`, omitempty), set by `withDispatchImageTag` on the create,
  start (resume included) and restart builders.
- **Applied:**
  - Runtime Broker `AgentManager.Start` (`pkg/agent/run.go`): the tag goes on
    the resolved image before the local-image check (so a local
    `scion-<h>:<tag>` wins) and before the registry rewrite (`RewriteImageRef`).
  - Hub dispatch of the explicit image (`dispatchImageForBroker`): `RewriteImageRef`.
  - Reincarnation's stored image (`buildPatchedAppliedConfig`, new `imageTagResolver`).
  - Hub-scope harness-config image check, image status and pull, the bootstrap
    image status (`hubScopeImageRefs`), and `POST /api/v1/system/images/pull`
    (`runtime.HarnessImages(harnesses, registry, tag)`).
- **Not applied, deliberately:**
  - Maintenance builds and `POST /api/v1/system/images/build` keep `latest`
    (design §7).
  - `scion build` produces images with its own `--tag`.
  - `computeReincarnationPlan` stays registry-only, so a move from `latest` to
    the tag shows as an image change.
  - Echo detection (`dropEchoedInlineImage`, `recordExplicitEdits`) compares
    stored strings.
  - Registry provenance and setup status are display only.
- **Refusal and warning:** a hub settings save that sets or changes a
  profile's `image_tag` while the experiment is off is refused with 412
  `experiment_disabled` (`details.experiment`). This covers both the database
  save and the settings-file save; an echo or a clear passes. Round 1 used
  409, which P3a's log anticipated; review L1 aligned it with the hub's other
  experiment refusals. At startup the hub warns when a value is set in the env, the
  settings file or a saved hub profile while the experiment is off.
- **R6-F1 log:** `image_tag resolved` once at startup (`LogImageTag`, from
  `cmd/server_foreground.go`), and `image_tag changed` from `ApplySnapshot`
  whenever the hub-scope value, source, experiment state or tag changes.
  Each carries the value, the source (`env`, `hub_settings_profile`,
  `settings_file_profile`, `settings_file`, `unset`), the experiment state,
  the tag, and the profiles that set their own value.
- **Docs:** `settings-precedence.md` ("Image tag (experimental)"),
  `experiments.md`, `orchestrator-settings.md`, the schema descriptions, the
  experiment description, `scion config set` help and the missing-registry
  hint. Every "not yet applied / follow-up change" caveat from 3a is gone.

## Hub → broker boundary (R292)

- **(a) Ship together / safe default.** A co-located broker ships with the
  hub. A remote broker that predates the field ignores `imageTag`: dispatch
  bodies are decoded leniently (only the keys endpoint uses
  `DisallowUnknownFields`), so the images it resolves keep today's tag, while
  an explicit image still arrives tagged by the hub. An absent or empty field
  means no rewrite.
- **(b) Every path.** Create (`buildCreateRequestFor`, also used by
  provision and finalize-env), start (`dispatchAgentStart`, resume is the same
  path with `resume=true`) and restart (`DispatchAgentRestart`) all set it;
  the broker applies it in `AgentManager.Start`, which create, start and
  restart all reach. Provision does not resolve the run image.
- **(c) No hazards.** No `TODO` on the producer or consumer path:
  `hub_agent_defaults_wire.go`, `dispatch_image.go`, `image_registry.go`,
  `httpdispatcher.go`, `api/types.go`, `runtimebroker/start_context.go`,
  `agent/run.go`.

## Decisions and deviations from the design

- **The hub rewrites, and also sends the tag.** The brief preferred a hub-only
  rewrite, but template and harness-config images are resolved only on the
  broker, so the hub cannot hand it a final ref. The hub rewrites what it does
  resolve (the explicit image), and the broker applies the same tag to what it
  resolves. Both rewrites are idempotent.
- **No hub DB top-level layer (known gap, deviation from §1c).** Phase 3a put
  `image_tag` only in the hub `profiles` section; there is no endpoints or
  admin top-level field. A managed top-level value would need the
  `image_registry_persist.go` machinery (seeded-row filtering, provenance).
  ri-em accepted the gap for 3b; a possible follow-up.
- **Seeded profile copies: FIXED in P3b (round 2), no follow-up needed.** A seeded `profiles` row
  carries the settings file's profile `image_tag`. Before the fix, GET showed
  it, and saving that echo turned the row managed, after which the copy ranked
  as hub DB profile and shadowed a later settings-file edit. The fix mirrors
  `image_registry`:
  - `profilesFilteredBase` strips the copy (`stripProfileFileCopies`), which
    covers both the admin PUT and the setup runtime save;
  - GET shows managed values only (`showManagedProfileImageSettings`);
  - the superseded-keys comparison treats a managed profile without
    `image_tag` as inheriting the file value (`withoutInheritedProfileKeys`).
  Test: `TestImageTagPersistence_SeededCopiesStayFileValues`,
  `TestComputeSupersededKeys_ImageTagInherited`.
- **No profile on the co-located broker.** The broker picks its default
  profile from live settings the hub does not see, so for `image_registry`
  the hub leaves that case to the broker. For `image_tag` the hub uses the
  broker row's `DefaultProfile`, else hub scope, because the broker has no tag
  layers of its own.
- **`SCION_IMAGE_TAG` in deploy scripts.** The brief placed it in the
  single-node `deploy.sh`. It is actually in `scripts/cloudrun/deploy.sh:21`
  (and the Terraform HA runbook), where it selects the hub's own image
  (`hub:${SCION_IMAGE_TAG:-latest}`) and is never passed to the hub process.
  The docs say so: setting it for the script does not set the hub's
  `image_tag`, and the reverse is also true.

## Tests

- `pkg/config/image_tag_apply_test.go`: the application table (latest and
  untagged take the tag; an explicit tag, a digest and a FQ ref are kept;
  registry plus tag; `version` vs the dev fallback); the file tier reader;
  the source labels.
- `pkg/runtime/imagepull_test.go`: `TestHarnessImageMap_TagAppliesToEveryEntry`,
  `TestHarnessImages_Tag`.
- `pkg/api/hub_agent_defaults_image_tag_test.go`: `IsEmpty`, the nil-safe
  accessor, and omitempty on the wire.
- `pkg/agent/run_image_tag_test.go`: the broker's final image across tiers,
  byte-identical output with no tag, and the tagged local-image check.
- `pkg/hub/image_tag_test.go`: layers, the experiment gate and `version`,
  broker default profile, the startup and change log lines with source, the
  invalid-value warning, the 412 and echo rules.
- `pkg/hub/image_tag_dispatch_test.go` (sqlite):
  - create, start, resume and restart wires with the experiment on and off;
  - explicit tags are kept;
  - the provider is wired;
  - the reincarnation store and plan.

## Learnings

- The settings apply hook (`ApplySnapshot`) runs before
  `SetOperationalSettings` at startup, so a change log there would fire
  against an empty settings view. The change log stays silent until the
  explicit startup line has run.
- Comment lines break gofmt's struct-field alignment blocks, but they do not
  break key/value alignment in composite literals with short keys.

## Round 2 (review ri-p3b-rev-1)

- R1: the log state now includes every profile that sets `image_tag`, each as
  value, source and tag (`imageTagState.Profiles`). So a profile-only change
  logs `image_tag changed`, and the `hub_settings_profile` and
  `settings_file_profile` sources appear.
- R2: fixed by the seeded-copy commit above.
- L1, L2: the refusal is 412 `experiment_disabled` on both save paths.
- L3: one tag resolution per dispatch, shared by the explicit image and
  `HubAgentDefaults`.
- L4: an unprofiled agent resolves with `config.EffectiveDefaultProfile`; a
  flat broker resolves at hub scope; the co-located broker uses its default
  profile.
- L5: tests for the system image pull handler (tag on and off), the profile
  log, `ApplySnapshot` calling the change log, and flat and co-located
  brokers.
- N1: the `hub_settings` source label is kept, commented as reserved for a
  future hub top-level layer. Dropping it would map the hub DB source to
  `unset`.
- FYI-3 (round-2 review, no action): no test pins one tag resolution per
  dispatch. The structure enforces it: each dispatch builder computes
  `imageTag` once and passes it to both consumers.
- F2 (no action): the harness-config image status that the bootstrap check
  stores is stale after an `image_tag` or experiment change until it is
  re-checked. The same was already true for `image_registry` changes.
