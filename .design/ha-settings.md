# Design: Hub Admin Settings — Seed/Managed Settings Model & Layer-Aware UI

- **Project:** ha-settings
- **Epic issue:** https://github.com/ptone/scion/issues/437
- **Author:** hs-arch (rev 1); hs-arch2 (rev 2–4, with user on thread 4199)
- **Status:** **REV 4 — FINAL.** All design questions resolved with user
  (Telegram thread 4199, 2026-07-12): `SCION_SEED_*` prefix + seeded/managed
  section lifecycle confirmed ("seed" as the marker semantic). Ready for
  implementation.
- **Predecessor:** settings-db (issue #268, PR #640 — merged). Prior design:
  `/scion-volumes/scratchpad/projects/settings-db/design.md`.
- **Scope (user, 2026-07-12):** everything below is in scope for this epic,
  including the backend precedence/model work — not just the original UI fix.

---

## 1. Problem & Goals

### Problem (two parts)

**Part A — the reported bug (#437).** The admin settings page
(`/admin/server-config`, `web/src/components/pages/admin-server-config.ts`) is
not layer-aware. In postgres (DB-tier) mode, `buildPayload()`
(`admin-server-config.ts:980-1137`) unconditionally re-emits Layer-0 scalars
(`server.mode`, `log_level`, `hub.port/host/timeouts`, `auth.dev_mode`,
`database.*`, `storage.*`, `secrets.*`). The PUT handler
(`handlePutServerConfigDB`, `admin_settings_db.go:277`) classifies via the
registry and returns **422 `layer0_rejected`** (`admin_settings_db.go:318`)
before any write — discarding the operator's valid Layer-1 edits along with the
Layer-0 fields the UI should never have sent. The page is effectively read-only
for everything in DB mode.

**Part B — the settings model (user direction, 2026-07-12).** The settings-db
precedence (`env > DB > file > defaults`) allows two mutable influences on an
operational value at runtime (shared DB + node-local env), reintroducing
per-node drift through the escape hatch. The user's direction: **one mutable
store**, with deployment-provided values acting as *seeds* (BIOS model: the
system tracks shipped defaults until an admin saves a setting). The paradox —
"env should be an overridable initial value" vs. the universal convention "env
beats config files" — is resolved by **naming** (a dedicated `SCION_SEED_*`
namespace whose name says initial-only) plus **behavior** (sections re-sync
from bootstrap material on every boot until first admin write).

### Goals / success criteria

1. **One universal precedence chain** (§3.1), identical in shape everywhere:
   `coded defaults → SCION_SEED_* → mutable store (yaml XOR DB) → SCION_SERVER_* (Layer-0 only)`.
2. **Seeded/managed lifecycle:** a DB-backed section tracks bootstrap material
   (re-synced each boot) until the first explicit admin write flips it to
   `managed`; from then the DB owns it. "Reset to bootstrap" reverts.
3. **`SCION_SEED_*` namespace** as the blessed way to provide initial
   operational values from deployment tooling; `SCION_SERVER_*` retains
   conventional env-wins for Layer-0; `SCION_SERVER_*` on Layer-1 keys enters a
   deprecation window (honored as seed input + WARN), then removal.
4. In DB mode the operator can edit and save Layer-1 settings from the UI with
   no 422; changes persist, propagate cluster-wide, no restart.
5. Layer-0 fields render read-only in DB mode with a clear badge; never
   submitted. File mode: env-pinned (`SCION_SERVER_*`) fields read-only; rest
   editable as today.
6. Managed sections whose bootstrap material differs show an informational
   **"superseded by database value"** notice (managed sections only — seeded
   sections track bootstrap by definition, nothing to warn about).
7. Maintenance/admin mode is cluster-consistent: the per-node
   `SCION_SERVER_ADMIN_MODE` force-win is removed (user decision); maintenance
   env values are ordinary seed inputs.
8. UI classification/provenance sourced from the backend (schema endpoint + GET
   metadata); structured errors (400/409/422) legible; Layer-1 booleans persist
   `false` and cleared fields clear (#391); provenance coverage complete (#389).
9. **Phases are EM-supervisable** (§7): commit-sized, explicit dependencies,
   per-phase acceptance, parallelizable after the backend contract lands.

## 2. Non-Goals

- **Storage re-architecture.** `hub_settings`, section registry, LISTEN/NOTIFY
  propagation, advisory-lock seeding stay as shipped in PR #640. This design
  adds one column and changes merge/seed/override semantics.
- **Full schema-driven form generation** — classify from the schema endpoint,
  don't generate the form.
- **notification_channels editor** — deferred; tracked in **ptone/scion#444**
  (with #387 secret-param splitting).
- **Maintenance-mode toggle on this page** — pure admin feature, stays on its
  existing surface (user decision).
- **Cross-node diagnostics** (#386), **settings audit log** (#390) — follow-ups.

## 3. Proposed Design

### 3.1 The precedence model (normative)

**Key classes** (existing `pkg/config/opsettings` registry):
- **Layer-0 (bootstrap-only):** needed before DB connect or restart-bound
  (database, listeners, auth stack, secrets/storage, mode/identity, logging,
  CORS, message broker, plugins). Never in the DB.
- **Layer-1 (operational):** registry sections (`access`, `lifecycle`,
  `maintenance`, `telemetry`, `agent_defaults`, `endpoints`, `github_app`,
  `notifications`).

**One bootstrap merge order, used everywhere:**

```
coded defaults → SCION_SEED_* → settings.yaml → SCION_SERVER_*
```

- `SCION_SEED_*`: new env namespace; path mapping identical to `SCION_SERVER_*`
  after the prefix (e.g. `SCION_SEED_SERVER_HUB_ADMINEMAILS` →
  `server.hub.admin_emails`). Provides initial values that the mutable store
  may override. Valid for any key; primarily useful for Layer-1.
- `settings.yaml`: bootstrap material in DB mode; **the mutable store** in
  no-DB mode (so the chain reads `defaults → SEED → mutable store → SERVER`
  there — exactly the universal shape).
- `SCION_SERVER_*`: conventional env-wins. **Layer-0: permanent** (deployment
  tooling must win on bootstrap keys). **Layer-1: deprecated** — during the
  transition window it is honored as a *seed input at the top of the bootstrap
  merge* (preserving today's "env wins over yaml" for existing deploys) with a
  startup WARN and a GET-response notice pointing at `SCION_SEED_*`; a later
  release ignores it for Layer-1 entirely.

**Runtime resolution:**

| Mode | Layer-0 | Layer-1 |
|---|---|---|
| No DB | bootstrap merge (restart-bound) | bootstrap merge; yaml editable via UI/API; `SCION_SERVER_*`-pinned keys read-only |
| DB (postgres) | bootstrap merge (restart-bound) | DB rows win; rowless sections fall back to the bootstrap merge |

### 3.2 The seeded/managed section lifecycle (the BIOS model)

New column on `hub_settings` (Ent schema `pkg/ent/schema/hubsetting.go`,
additive, rides declarative auto-migration):

```go
field.Enum("origin").Values("seeded", "managed").Default("seeded")
```

**Lifecycle:**

1. **Boot (every boot, not just first):** under the existing advisory lock,
   compute each section's document from the bootstrap merge. For sections with
   `origin = seeded` (or no row), upsert the computed doc **iff it differs**
   from the stored doc (skip-write on equality → no revision bump, no event
   churn). Sections with `origin = managed` are left untouched.
   - Since env can only change at process restart, this makes `SCION_SEED_*`
     behave *fully conventionally* for unmanaged sections: change the deploy
     config, restart/redeploy, the cluster picks it up.
   - Heterogeneous node envs converge to last-boot-wins in shared state — a
     consistent cluster, not per-node drift. Docs still say: keep env uniform
     via deployment tooling.
2. **First explicit admin write** (`PUT /admin/server-config` or
   `/admin/maintenance` touching the section): the upsert sets
   `origin = managed`. The DB now owns the section; bootstrap material is inert
   for it.
3. **Superseded notice:** for `managed` sections only, any bootstrap-material
   key whose merged value differs from the DB value is reported in the GET as
   `superseded_keys` and surfaced in the UI (§3.4).
4. **Reset to bootstrap:** per-section action = `DeleteHubSetting` (exists).
   The snapshot immediately falls back to the bootstrap merge for that section;
   next boot (or an immediate inline re-seed after delete) recreates the row as
   `seeded`. Exposed as a small per-section "Reset to bootstrap" button in the
   UI with a confirm dialog.

**Migration/backfill:** existing rows were written either by PR #640 seeding
(`updated_by = "seed"` — the sentinel already used, see `UpsertHubSetting`
call sites) or by admin PUTs. Backfill: `origin = seeded` where
`updated_by = "seed"`, else `managed`. The `_meta` row is exempt from the
lifecycle. Seed-writes continue to use `updated_by = "seed"`.

**Maintenance mode:** no special-casing remains. `SCION_SERVER_ADMIN_MODE` /
`SCION_SERVER_MAINTENANCE_MESSAGE` lose their bidirectional per-node force-win
(`operational_settings.go:755-765` removed — user decision: maintenance must be
cluster-consistent in HA). They participate as deprecated Layer-1
`SCION_SERVER_*` seed inputs (WARN), with `SCION_SEED_*` equivalents as the
go-forward. `PUT /api/v1/admin/maintenance` marks the `maintenance` section
managed, exactly like any other admin write. Release-note callout required
(removes a documented break-glass; remedy: admin API, or reset-to-bootstrap on
the maintenance row).

### 3.3 Backend changes (by file)

1. **`pkg/config` koanf load** — add the `SCION_SEED_*` env provider at the
   bottom of the merge (above defaults, below file); keep `SCION_SERVER_*` on
   top. Deprecation WARN when a `SCION_SERVER_*` var resolves to a Layer-1 key.
2. **`pkg/ent/schema/hubsetting.go`** — `origin` enum field (+ generated code);
   store surface (`UpsertHubSetting` gains an origin-aware variant or a
   `MarkManaged` semantic: admin path writes `managed`, seed path writes
   `seeded`).
3. **Seeding path** (`cmd/server_foreground.go` wiring +
   `pkg/hub/operational_settings.go`) — every-boot re-sync of
   seeded/rowless sections from the bootstrap merge under the advisory lock;
   skip-write on equality.
4. **`OperationalSettings.Snapshot()`** (`operational_settings.go:255-292`) —
   DB rows win; rowless sections fall back to the bootstrap merge. The current
   env-merged-last block is removed (env now lives inside the bootstrap merge).
5. **Maintenance force-win removal** (`operational_settings.go:740-765`) +
   test updates (`operational_settings_propagation_test.go`, `ha_e2e_test.go`).
6. **GET responses** (`admin_settings_db.go` / `admin_settings.go`):
   - `settings_tier`: `"db"` | `"file"`.
   - Per-section `origin` added to `section_metadata` (drives UI captions and
     the reset button).
   - DB mode: `superseded_keys: [{key, source: "seed_env"|"yaml"|"server_env"}]`
     for managed sections only.
   - File mode: `env_keys` — all keys pinned by `SCION_SERVER_*` (generalize
     `DetectEnvOverrides`, `pkg/config/opsettings/koanf.go:303`, which today
     filters to Layer-1).
   - `deprecated_env_keys` — `SCION_SERVER_*` vars hitting Layer-1 keys, for a
     UI nudge toward `SCION_SEED_*`.
7. **PUT handler** — sets `origin = managed` on written sections. `DELETE`
   /reset endpoint (or reuse existing delete path) per section.
8. **#391 correctness** — `*bool` round-trips persist `false`; explicit-empty
   clears via `fieldPresence` (`admin_settings_db.go:1002`).

### 3.4 UI: editability predicate and rendering

Classification from `GET /api/v1/admin/server-config/schema`
(`handleAdminServerConfigSchema`, `admin_settings_db.go:966`) → `layer1Keys`;
`STATIC_LAYER1_KEYS` fallback (derived from `KOANF_KEY_LABELS`,
`admin-server-config.ts:271`) if the fetch fails. Single predicate:

```ts
private readOnlyReason(koanfKey: string): 'bootstrap' | 'env' | null {
  if (this.settingsTier === 'db') {
    return this.layer1Keys.has(koanfKey) ? null : 'bootstrap'; // DB owns Layer-1
  }
  return this.envKeys.has(koanfKey) ? 'env' : null;            // file mode: SERVER_* pins
}
```

| State | Treatment |
|---|---|
| DB mode, Layer-0 | static value + grey **🔒 Managed via deployment configuration (settings.yaml / env)** |
| DB mode, Layer-1, section `seeded` | editable; caption: *"tracking deployment configuration — will re-sync on restart until edited"* |
| DB mode, Layer-1, section `managed` | editable; caption shows `source/revision/updated_by/updated_at` (exists: `renderSectionMeta`, line 1244) |
| DB mode, managed + differing bootstrap value | editable + blue ⓘ *"a deployment-provided value differs and is superseded by this database value"*; page-top panel lists `superseded_keys` |
| DB mode, `deprecated_env_keys` present | page-top notice: *"SCION_SERVER_* is deprecated for operational settings — use SCION_SEED_*"* |
| File mode, `SCION_SERVER_*`-pinned | static value + grey **🔒 Set via environment variable** |
| File mode, others | editable (today's behavior) |

Per-section **"Reset to bootstrap"** button (confirm dialog) → delete/reset →
reload. Editing any field in a seeded section and saving flips it to managed —
the seeded-caption doubles as the warning that this is a one-way door (until
reset).

UI organization is **option A** (per-field marking within existing functional
tabs) — confirmed; a tab legitimately mixes editable and read-only fields.

### 3.5 Payload construction

Invariant: **never submit a field rendered read-only** — both builders key off
`readOnlyReason()`.

- `buildLayer1Payload()` (DB mode) — only Layer-1 keys. Sections:
  `server.hub.{admin_emails, public_url, soft_delete_*, auto_suspend_stalled}`,
  `server.auth.{user_access_mode, authorized_domains}`, `agent_defaults`
  fields, `telemetry` (whole object), `server.github_app` non-secret fields
  (secrets stay on `/api/v1/github-app`), `server.notification_channels`
  round-tripped raw until #444. Booleans always sent (`*bool`, never
  `|| undefined`); cleared fields sent as explicit empty (#391).
- `buildFilePayload()` (file mode) — today's payload minus env-pinned keys;
  byte-identical to today when no `SCION_SERVER_*` vars are set.

Zero Layer-0 keys in a DB-mode PUT → the 422 becomes unreachable in normal use
(kept as a safety net, §3.6).

### 3.6 Structured error handling

Typed parser on the `error` discriminator in `handleSave()` replacing generic
`extractApiError()`:

| HTTP | body | UI |
|---|---|---|
| 200 | `{reload, ignored_keys?}` | success; ignored-keys notice (exists) |
| 400 | `validation_failed` + per-section paths | inline per-section errors; scroll to first |
| 409 | `revision_conflict` + conflicted sections | "changed since you loaded" banner + Reload |
| 422 | `layer0_rejected` + keys | safety-net notice; console log (should not occur) |

### 3.7 Optional: optimistic concurrency (CAS)

Opt in later by sending `expected_revisions` from `section_metadata`; 409 per
§3.6. Separate phase (#441); default stays last-writer-wins.

### 3.8 Data flow summary

```
bootstrap merge (everywhere): defaults → SCION_SEED_* → yaml → SCION_SERVER_*
                                                (Layer-1 SERVER_* = deprecated seed input + WARN)

boot (postgres): under advisory lock, for each section with origin=seeded or no row:
                   doc := extract(bootstrap merge); upsert iff changed (origin=seeded)
                 managed rows untouched
runtime (postgres): Snapshot = bootstrap merge under DB rows (DB wins)
admin PUT: validate → upsert(origin=managed) → event → peers Refresh()
reset: delete row → falls back to bootstrap → re-seeded next boot

load():   GET server-config        → values + settings_tier + section_metadata(origin)
                                     + superseded_keys | env_keys + deprecated_env_keys
          GET server-config/schema → layer1Keys (fallback STATIC_LAYER1_KEYS)
render(): readOnlyReason() → editable | 🔒bootstrap | 🔒env; captions by origin; ⓘ superseded
save():   PUT {only editable fields; presence-aware; booleans always} → typed 200/400/409/422
```

## 4. Alternatives Considered

1. **Keep env-wins precedence (settings-db as shipped), UI-only fix** (rev 1/2).
   *Rejected by user:* two mutable influences at runtime; per-node drift
   persists; UI must explain "editable but locally overridden".
2. **Pure runtime precedence inversion, seed-once** (rev 3). *Refined away:*
   left the paradox — a changed env var silently not applying violates the
   universal env convention, biting hardest during early deploy iteration.
3. **Prefix-only (`SCION_SEED_*`), seed-once, no managed tracking.** *Rejected
   (user chose the refinement):* the name explains initial-only, but changed
   SEED values still silently no-op after first boot; no clean "reset" story;
   one column buys behavior that matches the name.
4. **Re-seed on every boot unconditionally (no managed marker).** *Rejected:*
   admin edits would be overwritten at each restart — the DB would never truly
   own anything.
5. **Backend-only 422 tolerance** (extend `isZeroStruct` to scalars).
   *Rejected:* fragile heuristic; hides real Layer-0 write attempts; bootstrap
   fields stay visibly editable-but-inert.
6. **Option B UI (Operational vs Bootstrap groups).** *Rejected (user confirmed
   A):* cannot express runtime-dependent mixed editability within a tab.
7. **Per-node break-glass retained for maintenance.** *Rejected by user:*
   maintenance must be cluster-consistent in HA.
8. **Configurable precedence knob.** *Rejected:* configurable precedence is how
   config systems become haunted.
9. **Hardcoded client key set** — rejected as primary (drift), kept as fallback.
10. **Per-section save buttons** — deferred (pairs with CAS, #441).

## 5. Migration / Rollout

- **Schema:** one additive `origin` column (default `seeded`), declarative
  auto-migration. Backfill `managed` where `updated_by != "seed"`.
- **Behavior on upgrade (existing postgres deployments):** sections never
  admin-edited (`updated_by = "seed"`) become `seeded` → they re-sync from
  current bootstrap material at next boot. Their DB values equal the original
  file seed, so a re-sync simply refreshes them to current deploy config —
  the intended semantic. Admin-edited sections become `managed` → DB keeps
  winning; any still-set env var that differs now shows the superseded notice
  instead of silently winning per-node. **Release-note callouts:** (a) env no
  longer overrides managed operational settings at runtime (notably
  `user_access_mode` lockdowns), (b) `SCION_SERVER_ADMIN_MODE` force-win
  removed, (c) `SCION_SERVER_*` deprecated for Layer-1 → `SCION_SEED_*`.
- **No-DB hubs:** unchanged except `SCION_SERVER_*`-pinned fields render
  read-only (edits to them were already inert) and `SCION_SEED_*` becomes
  available as an overridable default below yaml.
- **Rolling deploy:** old+new replicas disagree on Layer-1 env precedence
  during the window — transient, converges at rollout completion (same stance
  as settings-db's mixed-version window). Client degrades safely: tier inferred
  from `section_metadata` if `settings_tier` absent; missing
  `env_keys`/`superseded_keys`/`origin` treated as empty/managed;
  `STATIC_LAYER1_KEYS` if the schema endpoint fails.
- **Docs (#440):** precedence chain, seeded/managed lifecycle + reset,
  `SCION_SEED_*` reference (per-key table), deprecation of `SCION_SERVER_*`
  for Layer-1, break-glass removal, env-first HA bootstrap guidance.

## 6. Open Questions

**None.** All resolved with the user on thread 4199 (2026-07-12):

| # | Question | Resolution |
|---|---|---|
| 1 | UI organization A vs B | **A** (per-field marking) |
| 2 | Precedence scope | Layer-1 only in DB; Layer-0 stays file/env, `SCION_SERVER_*` wins |
| 3 | Env as seed source | Yes — `SCION_SEED_*` namespace ("seed" semantic confirmed) |
| 4 | Seed lifecycle | Seeded/managed marker; re-sync unmanaged sections every boot; first admin write takes ownership; reset-to-bootstrap reverts |
| 5 | `SCION_SERVER_*` on Layer-1 | Deprecation window (seed input + WARN), then ignored |
| 6 | `SCION_SERVER_ADMIN_MODE` break-glass | Removed — maintenance is cluster-consistent |
| 7 | notification_channels editor | Deferred → ptone/scion#444 |
| 8 | Maintenance toggle on page | Out of scope (pure admin feature) |
| 9 | No-DB env ordering | `defaults → SEED → yaml → SERVER` — universal chain, no special case |

## 7. Implementation Phases (EM-supervisable)

Yes — structured for an EM: commit-sized phases with explicit dependencies,
per-phase acceptance, and a parallelization map. Backend contract lands first
(B1–B3, serial — same files); the four workstreams after it can run in
parallel across developers.

```
B1 → B2 → B3 ──┬── W1 → W2  (web core → notices/reset)
               ├── W3        (structured errors)
               ├── B4        (#391 correctness)
               └── T1 → T2   (test infra → component tests)
                                then D1 (docs), C1 (optional CAS)
```

**B1 — `SCION_SEED_*` provider + unified bootstrap merge (backend).** *(#446)*
Koanf: SEED provider below file, SERVER on top; Layer-1 SERVER deprecation WARN.
No DB changes. *Accept:* unit tests prove the merge order
`defaults → SEED → yaml → SERVER` incl. Layer-1 SERVER-as-top-of-bootstrap
during deprecation; WARN emitted.

**B2 — `origin` column + seeded/managed lifecycle (backend).** *(#446)*
Ent field + backfill (`updated_by != "seed"` → managed); every-boot re-sync of
seeded/rowless sections under the advisory lock with skip-write-on-equality;
PUT sets managed; reset path (delete → fallback → reseed next boot).
*Accept:* store/integration tests — re-sync updates a seeded row when bootstrap
changed, skips when equal (no revision bump/event), never touches managed rows;
concurrent-boot exactly-once; backfill correct.

**B3 — Runtime resolution + provenance API (backend).** *(#446)*
`Snapshot()` = bootstrap merge under DB rows; remove env-merged-last block;
remove maintenance force-win (update propagation/e2e tests); GET gains
`settings_tier`, `origin` in `section_metadata`, `superseded_keys` (managed
only), `env_keys` (file mode, generalized detection), `deprecated_env_keys`.
*Accept:* two-node test — managed section ignores env at runtime and reports
superseded; seeded section refreshes on restart; maintenance no longer
env-forced; response fields golden-tested. **This freezes the API contract for
W1–W3.**

**W1 — Layer-aware rendering + payload split (web) — the #437 fix.** *(#438)*
Schema fetch → `layer1Keys` + static fallback; `readOnlyReason()`; `field()`
wrapper / per-control disabled + badges; `buildLayer1Payload()` /
`buildFilePayload()`. *Accept:* acceptance criteria 3, 4, 6 (§8) manually
verified against a local postgres hub; no Layer-0/env-pinned key in any PUT.

**W2 — Origin captions, superseded/deprecation notices, reset button (web).**
*(#438)* Seeded/managed captions; ⓘ superseded badges + page panel;
`SCION_SEED_*` deprecation nudge; per-section reset with confirm. Closes #389
(audit: every field declares its koanf key). *Accept:* criteria 1, 5 (§8).

**W3 — Structured error handling (web).** *(#439)* Typed 400/409/422 parser;
inline per-section errors; conflict banner; 422 safety net. *Accept:*
criterion 8.

**B4 — Presence/boolean correctness (backend+web, #391).** `false` persists;
explicit-empty clears; round-trip tests. *Accept:* criterion 7.

**T1 — Component test infra (web, #388).** `@web/test-runner` or vitest +
`@open-wc/testing` wired into CI. **T2 — Component tests** covering criteria
1, 3, 4, 6, 8. *(T1 can start anytime; T2 needs W1–W3.)*

**D1 — Docs (#440).** Per §5. *(After B3 stabilizes; screenshots after W2.)*

**C1 (optional) — CAS (#441).** `expected_revisions` + 409 UX + per-section
save consideration.

Branch/PR guidance: B1–B3 as one reviewable backend PR train on the epic
branch (or stacked); W1–W3 independent PRs behind it. Design doc committed as
`.design/ha-settings.md` with the first PR. Rebase-onto-upstream at each phase
start and before compare URLs, per process.

## 8. Acceptance Criteria

1. **Seeded lifecycle:** fresh postgres hub, `SCION_SEED_SERVER_HUB_ADMINEMAILS=a@x`:
   value serves from DB (`origin: seeded`). Change the var to `b@y`, restart:
   value updates cluster-wide. Edit to `c@z` in the UI: section becomes
   `managed`; changing the var + restart no longer alters it; superseded notice
   lists the key. Reset-to-bootstrap → back to tracking (`b@y` after reseed).
2. **Bootstrap merge order:** with defaults + SEED + yaml + SERVER all setting
   the same Layer-1 key, effective bootstrap value is SERVER's (deprecation
   window, WARN emitted); without SERVER, yaml's; without yaml, SEED's.
3. **DB mode, Layer-1 edit persists (the #437 fix):** editing
   `image_registry`, `admin_emails`, a telemetry toggle → 200, no 422, value on
   reload with `source: "db"`, propagated to a second replica ≤2s.
4. **DB mode, Layer-0 read-only:** `server.mode`, `log_level`, `hub.port`,
   `database.*`, `secrets.*`, `storage.*`, dev-auth render read-only with the
   bootstrap badge and are absent from the PUT (network capture).
5. **Maintenance cluster-consistency:** `SCION_SERVER_ADMIN_MODE=true` on one
   node does not force that node into admin mode when the DB maintenance row
   says otherwise; `PUT /admin/maintenance` marks the section managed,
   propagates, survives restart.
6. **File mode:** no env vars → all fields editable, Save writes settings.yaml
   as today (existing tests pass). `SCION_SERVER_HUB_PORT` set → port field
   read-only + env badge, absent from PUT. `SCION_SEED_*` value appears as an
   editable default that a yaml write overrides.
7. **#391:** boolean on→off persists `false`; clearing `admin_emails` clears it.
8. **Structured errors:** schema violation → inline per-section error; conflict
   (CAS) → reload banner; scripted PUT with `server.mode` → 422 safety net.
9. **Re-sync hygiene:** restart with unchanged bootstrap material causes zero
   writes/revision bumps/events (verified by revision equality across restarts).
10. **Resilience / mixed versions:** schema-endpoint failure → static fallback;
    GET missing new fields (older hub) → safe degraded behavior.
11. **Component tests (#388)** cover 1 (UI aspects), 3, 4, 6, 8 and run in CI.

## 9. QA / Live-instance test plan (Cloud Run + IAP)

Against a live Cloud Run hub in postgres mode via IAP browser auth
(`/scion-volumes/contrib-repo/skills/iap-browser-auth/SKILL.md`); promote the
SA user to admin first. Playwright with `waitUntil: 'domcontentloaded'`.

1. **Load & tier:** page loads; `settings_tier: "db"`; bootstrap badges on
   Data & Storage / dev-auth / hub port; seeded/managed captions render.
2. **Seed lifecycle end-to-end:** set a `SCION_SEED_*` Layer-1 var on the
   service (new revision = restart) → value appears (`origin: seeded`); edit it
   in the UI → managed; redeploy with a different var value → UI value
   unchanged + superseded notice; reset-to-bootstrap → tracks var again.
3. **Layer-1 happy path:** edit `image_registry` → 200; reload shows
   `source: "db"`, `updated_by` = SA email.
4. **Layer-0 immutability + safety net:** bootstrap inputs read-only; scripted
   PUT with `server.mode` → 422.
5. **Propagation:** save on one replica; second replica reflects ≤2s
   (min-instances=2).
6. **Maintenance:** on/off round-trip via API; no env force-win.
7. **Screenshots** of each tab.

## 10. Files & surfaces touched (for the developer)

**Backend:**
- `pkg/config` (koanf load) — `SCION_SEED_*` provider; SERVER-on-Layer-1
  deprecation WARN.
- `pkg/ent/schema/hubsetting.go` (+ regen) — `origin` enum; backfill.
- `pkg/store` / `entadapter` — origin-aware upsert paths.
- `pkg/hub/operational_settings.go` — every-boot re-sync (advisory lock,
  skip-on-equal); `Snapshot()` merge (~255-292); maintenance force-win removal
  (~740-765); superseded computation.
- `pkg/config/opsettings/koanf.go` — generalized env detection (line 303),
  SEED/SERVER split, value-diff superseded helper.
- `pkg/hub/admin_settings_db.go` / `admin_settings.go` — response fields
  (`settings_tier`, `origin`, `superseded_keys`, `env_keys`,
  `deprecated_env_keys`); PUT sets managed; reset path; #391 presence fixes
  (`fieldPresence`, line 1002).
- Tests: `opsettings_test.go`, `operational_settings_propagation_test.go`,
  `ha_e2e_test.go`, handler tests, store tests.

**Web:**
- `web/src/components/pages/admin-server-config.ts` — tier/origin/provenance
  state, schema fetch + fallback, `readOnlyReason()`, `field()` /
  `renderManagedBadge(reason)`, seeded/managed captions, superseded +
  deprecation notices, reset button, `buildLayer1Payload()` /
  `buildFilePayload()`, typed error parser. CSS: `.managed-badge`
  (`.env`/`.bootstrap`), `.superseded-badge`, `.read-only`, `.ro-value`.
- `web/` test infra (#388) + `*.test.ts`.

**Docs:** admin settings page under `docs/` (precedence chain, lifecycle,
`SCION_SEED_*` reference, deprecations, HA guidance).
