# Harness-Config Decoupling: Top-Level Bundle Directory & Opt-In Install

**Status:** Draft plan — 2026-06-06
**Owner:** harness-refactor agent (for ptone@google.com)
**Related:** [`decoupled-harness-implementation.md`](./decoupled-harness-implementation.md) (the container-script provisioning work this builds on)

## Motivation

Today every harness ships compiled into the scion binary and is **installed by
default**. `harness.All()` returns `{gemini, claude, opencode, codex}`, and
`scion init` / `scion server` startup seed each one's embedded config into
`~/.scion/harness-configs/<name>/` from `pkg/harness/<name>/embeds/`.

We want to move to a model where **harnesses and their configs are not all
installed by default**. The first step is to:

1. Establish a **new top-level harness-config directory at the repo root** that
   holds harness bundles as plain on-disk artifacts (not Go embeds).
2. **Refactor OpenCode and Codex** out of `pkg/harness/*/embeds/` into that
   directory.
3. **Port the Antigravity harness config** (from
   [`ptone/scion-antigravity`](https://github.com/ptone/scion-antigravity)) into
   that directory.

The container-script migration (`decoupled-harness-implementation.md`, Phases
0–5) already did the hard part: Codex and OpenCode are fully declarative
(`config.yaml` + `provision.py`) and run their provisioning inside the agent
container. This plan is the **packaging/distribution** follow-on — it changes
*where the bundles live* and *whether they are installed automatically*, not how
they provision.

## Current State (verified)

| Concern | Where it lives today |
|---|---|
| Default-install set | `pkg/harness/harness.go::All()` → gemini, claude, opencode, codex |
| Default-install call sites | `cmd/project.go` (`scion init`), `cmd/templates.go` (`templates update-default`), `cmd/server_foreground.go` (`scion server`) — all call `harness.All()` |
| Seeding from embeds | `pkg/config/harness_config.go::SeedHarnessConfig()` walks `h.GetHarnessEmbedsFS()` |
| OpenCode bundle (embedded) | `pkg/harness/opencode/embeds/{config.yaml,opencode.json,provision.py}` + `pkg/harness/opencode/embeds.go` (`//go:embed`) |
| Codex bundle (embedded) | `pkg/harness/codex/embeds/{config.yaml,config.toml,scion_notify.sh,bashrc,provision.py}` + `pkg/harness/codex/embeds.go` |
| Built-in Go fallbacks | `pkg/harness/opencode.go`, `pkg/harness/codex.go` (+ `codex_config.go`), selected by `harness.New()` / `harness.Resolve()` |
| Opt-in install (already exists!) | `cmd/harness_config_install.go` → `scion harness-config install <source>` supports local dir, `github.com/...` shorthand, `file://`, `:gcs:`, and `.tgz`/`.zip` archives |
| Image builds | `image-build/{opencode,codex,claude,gemini}/Dockerfile`; DAG in `image-build/scripts/lib/targets.sh`; `image-build/cloudbuild-harnesses.yaml` |
| Antigravity source layout | `antigravity/{config.yaml,provision.py,dialect.yaml,skills/}` + root `Dockerfile` + `cloudbuild.yaml` |

Key insight: **the opt-in install command already exists.** The work is mostly
about (a) relocating the bundles, (b) shrinking the default set, and (c) deciding
the fate of the Go embeds and built-in fallbacks.

## Target State

```
<repo root>/
  harnesses/                      # NEW top-level harness-config directory
    opencode/
      config.yaml
      provision.py
      Dockerfile                  # moved from image-build/opencode/
      cloudbuild.yaml             # per-bundle image build
      home/
        .config/opencode/opencode.json
      README.md
    codex/
      config.yaml
      provision.py
      Dockerfile                  # moved from image-build/codex/
      cloudbuild.yaml
      home/
        .codex/config.toml
        .codex/scion_notify.sh
        .bashrc
      README.md
    antigravity/
      config.yaml
      provision.py
      dialect.yaml
      Dockerfile                  # ported from ptone/scion-antigravity
      cloudbuild.yaml
      skills/.gitkeep
      home/
        .gemini/...
      README.md
```

Note: the bundle root now carries non-harness-config files (`Dockerfile`,
`cloudbuild.yaml`). `scion harness-config install` copies the whole directory, so
these get copied into `~/.scion/harness-configs/<name>/` too. That is harmless
(they're ignored at provision time) but the install/seed allowlist and
`ComputeHarnessConfigRevision` should be reviewed so image-build files don't
perturb the config revision hash — see Phase D.4.

- `harness.All()` (default-install set) shrinks to **`{gemini, claude}`** (TBD —
  see Decision 2).
- OpenCode / Codex / Antigravity become **opt-in**, installed with:
  ```
  scion harness-config install harnesses/opencode      # from a repo checkout
  scion harness-config install github.com/GoogleCloudPlatform/scion/tree/main/harnesses/codex
  ```
- The `harnesses/` bundles are the **single source of truth** for these configs.
  No duplicate copies under `pkg/harness/*/embeds/`.

## Decisions (locked — ptone, 2026-06-06)

1. **Directory name: `harnesses/`** at the repo root.
2. **Default-install set shrinks to `{claude, gemini}`.** OpenCode, Codex, and
   Antigravity become opt-in bundles.
3. **Drop the Go entirely.** Remove both the embeds
   (`pkg/harness/{opencode,codex}/embeds*`) **and** the built-in Go
   implementations (`opencode.go`, `codex.go`, `codex_config.go`). The
   `harnesses/` bundles become the sole source; OpenCode/Codex resolve purely as
   container-script harnesses from an installed bundle. No built-in fallback is
   retained. (This is more aggressive than the prior design's "keep fallback one
   release" guidance — the parity oracle goes away, so the relocated bundles must
   be locked down with golden/install tests first; see Phase A.4 and Risks.)
4. **Co-locate `Dockerfile` + cloudbuild file inside each bundle.** Each
   `harnesses/<name>/` is self-contained (config + provisioner + image build),
   matching the antigravity repo layout. The centralized `image-build/{opencode,
   codex}/` dirs are removed and the build DAG/cloudbuild wiring is repointed at
   the bundle dirs.
5. **Keep first-party bundles in this repo** under `harnesses/` for now (no split
   into separate repos this phase).

## Implementation Plan

Decisions locked above. Steps are ordered to keep the tree green at each commit;
the destructive Go removal (Phase D) lands only after the relocated bundles are
proven (Phase A.4).

### Phase A — Establish `harnesses/` and relocate the OpenCode/Codex bundles

1. Create top-level `harnesses/` with `opencode/` and `codex/` subdirs.
2. Move the embedded bundle files into the new layout, converting the implicit
   `mapEmbedFileToHomePath` placement into an **explicit `home/**`** layout
   (the prior design's preferred end state, §"File Seeding and Packaging
   Changes"):
   - OpenCode: `opencode.json` → `harnesses/opencode/home/.config/opencode/opencode.json`; `config.yaml`, `provision.py` at bundle root.
   - Codex: `config.toml` → `home/.codex/config.toml`; `scion_notify.sh` → `home/.codex/scion_notify.sh`; `bashrc` → `home/.bashrc`; `config.yaml`, `provision.py` at root.
3. Move the image build into each bundle (Decision 4): `image-build/opencode/Dockerfile`
   → `harnesses/opencode/Dockerfile`, same for codex; add a per-bundle
   `cloudbuild.yaml` (extract the opencode/codex steps from
   `image-build/cloudbuild-harnesses.yaml`, threading `BASE_IMAGE` from
   `scion-base`).
4. **Lock down behavior before deleting the Go oracle.** Capture golden output
   from the existing built-in + container-script paths (command construction,
   seeded file layout, provision staging) as fixtures, and add a CI smoke test:
   `scion harness-config install harnesses/<name> --name <name>-test` →
   `scion harness-config show <name>-test` → assert config parses and a dry
   provision stages the expected bundle. This replaces the parity oracle that
   Decision 3 removes.
5. Add a `README.md` per bundle (purpose, `install` command, auth modes, image
   build) — mirror the antigravity repo's README.

### Phase B — Port Antigravity

1. Copy `antigravity/{config.yaml,provision.py,dialect.yaml,skills/}` plus the
   root `Dockerfile` and `cloudbuild.yaml` from `ptone/scion-antigravity` into
   `harnesses/antigravity/` (Decision 4 keeps image build in-bundle).
2. Reconcile `config.yaml` against the current `HarnessConfigEntry` schema and
   `ValidateHarnessConfig`. The antigravity config exercises fields a relocated
   first-party bundle may not have: the top-level `mcp:` global-config mapping
   block, `dialect.yaml`, and `oauth-token` / `vertex-ai` auth types (the latter
   with an empty `vertex-ai: {}` body). Confirm the in-repo schema accepts all of
   them; add schema support for any rejected field before merging.
3. The antigravity image needs keyring packages (`gnome-keyring`, `libsecret`)
   not in `scion-base` — its `Dockerfile`/`cloudbuild.yaml` already encode the
   `core-base → scion-base → antigravity` chain; verify they reference the
   in-repo base image tags rather than the external repo's registry.
4. Confirm `ContainerScriptHarness.Provision` stages `dialect.yaml` (it does,
   `container_script_harness.go:342`).

### Phase C — Shrink the default-install set

1. Change `harness.All()` to return `{GeminiCLI, ClaudeCode}` (Decision 2).
2. Audit the three call sites (`cmd/project.go`, `cmd/templates.go`,
   `cmd/server_foreground.go`) — confirm none assume opencode/codex presence.
3. Update tests that assert the 4-harness default (e.g.
   `pkg/config/init_test.go`, `templates_test.go`).

### Phase D — Drop the Go (embeds + built-in implementations)

Decision 3 — remove entirely, no fallback. Land this after Phase A.4 proves the
relocated bundles.

1. Delete `pkg/harness/opencode/` (embeds + `embeds.go`), `pkg/harness/codex/`
   (embeds + `embeds.go`).
2. Delete `pkg/harness/opencode.go`, `pkg/harness/codex.go`,
   `pkg/harness/codex_config.go`, and their `_test.go` + `*_parity_test.go`
   files (the parity tests compared against the now-removed built-in oracle;
   their coverage moves to the Phase A.4 install/golden tests).
3. Remove the `codex`/`opencode` cases from `harness.New()` and
   `harness.newBuiltin()` so resolution flows: container-script (installed
   bundle) → declarative-generic. With no bundle installed, `--harness codex`
   falls to `Generic` — acceptable now that they're opt-in (surface a clear
   "not installed; run scion harness-config install" hint where practical).
4. Review the install/seed allowlist and `ComputeHarnessConfigRevision` so the
   newly co-located `Dockerfile`/`cloudbuild.yaml` in each bundle don't break
   provisioning or destabilize the revision hash (either exclude them, or accept
   them as part of the hash deliberately).
5. `scion harness-config reset codex` currently restores *embedded* defaults via
   `harness.New` — with embeds gone it must change. Repoint `reset` to fail
   clearly with "reinstall from bundle: scion harness-config install
   harnesses/codex" guidance (and update its tests).
6. Remove `image-build/opencode/` and `image-build/codex/` and repoint the build
   DAG (`image-build/scripts/lib/targets.sh`) + `cloudbuild-harnesses.yaml` at
   the bundle dirs (or split codex/opencode out of the centralized `harnesses`
   target entirely, since their images are now bundle-local).

### Phase E — Discoverability & docs ✓

1. [x] Add `harnesses/README.md` indexing available bundles + install commands.
2. [x] Update `image-build/README.md` (image hierarchy no longer lists
   opencode/codex centrally), top-level `README.md`, and
   `decoupled-harness-implementation.md` cross-references.
3. [x] Verified web UI harness fallback lists in `agent-create.ts` and
   `project-settings.ts` — they enumerate known/installable harnesses (incl.
   opt-in ones), not the default-install set; left as-is with clarifying
   comments.
4. `scion harness-config list --available` deferred — out of scope for this PR;
   noted as follow-up in `harnesses/README.md`.

### Phase F — Migration for existing installs

Existing machines already have `~/.scion/harness-configs/{opencode,codex}/`
seeded. Shrinking defaults and dropping embeds must **not** delete a user's
installed config.

1. `scion init`/upgrade must leave existing installed configs untouched
   (additive-only upgrade is already the contract —
   `decoupled-harness-implementation.md` §"Existing Installation Upgrade Plan").
2. Existing codex/opencode configs keep resolving as container-script harnesses
   from their on-disk dir (they already declare `provisioner.type:
   container-script`), so removing the Go built-in does not break them — **but**
   any legacy config still on `provisioner.type: builtin` would break. Add an
   upgrade check that flags/auto-activates such configs (`--activate-script`)
   before the built-in is removed.
3. Document that fresh installs no longer get opencode/codex automatically, plus
   the one-line `harness-config install` to restore them. No agent-home
   rewrites; already-created agents keep working.

## Risks & Open Questions

- **No more parity oracle (Decision 3).** Removing the built-in Go
  implementations deletes the reference behavior the parity tests checked
  against. Phase A.4 golden + install tests must land *first* and be trusted.
- **Legacy `provisioner.type: builtin` configs break** once the Go built-in is
  gone (Phase F.2). Needs an upgrade/auto-activate safety net.
- **`reset` semantics change** (Phase D.5) — agree on the replacement
  (reinstall-from-bundle hint).
- **Image-build files inside config bundles** (Decision 4) mean
  `harness-config install`/sync copies `Dockerfile`/`cloudbuild.yaml` into the
  installed config dir and into Hub artifacts. Confirm that's acceptable and
  doesn't perturb `ComputeHarnessConfigRevision` (Phase D.4).
- **Hub-distributed configs**: brokers install on demand so are unaffected, but
  the Hub's own seed/catalog may assume the 4-harness set — audit
  `pkg/runtimebroker` + Hub harness-config endpoints.
- **Antigravity schema gaps**: the ported `config.yaml` may use fields the
  in-repo validator hasn't accepted from a first-party bundle (MCP mapping
  block, empty `vertex-ai` type). Phase B.2 must validate before merging.
- **Web UI / templates** that list harnesses (`web/`, `cmd/templates.go`
  template harness-configs) may hard-code the 4 names — grep before shipping.

## Out of Scope (for this phase)

- Migrating Claude/Gemini to container-script bundles (that's
  `decoupled-harness-implementation.md` Phase 6).
- Splitting first-party bundles into standalone repos (Decision 5, deferred).
- A full remote harness catalog / marketplace.
