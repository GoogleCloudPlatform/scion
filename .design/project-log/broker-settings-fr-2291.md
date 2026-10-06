# broker-settings FR-2291: restrict agent-written secrets to profile scope (ptone/scion#2291)

PR: ptone/scion#2415, branch `scion/fr-2291-capture-scope`.
Design: `/scion-volumes/scratchpad/projects/broker-settings/fr-2291/design.md` (FINAL). Option B
(blanket rule) per ptone's ruling, 2026-09-30T11:36Z.

## What this adds

A new Layer-1 opsettings section `agent_secrets` with one key, `user_scope_only` (bool, default
`false`/permissive), following the exact P1b (`quotas`) pattern:
`pkg/config/opsettings/{sections,registry,koanf}.go`, the hand-written schema entry in both
`registry.go`'s `compileSchemas()` and `pkg/config/schemas/settings-v1.schema.json`, the
`VersionedSettings.AgentSecrets` file-mode field in `pkg/config/settings_v1.go`, and the
`GlobalConfig.AgentSecretsUserScopeOnly` top-level-key parsing in `pkg/config/hub_config.go`'s
`loadServerFromSettingsFile`.

`pkg/hub/operational_settings.go` carries it through `Layer1Snapshot`, `buildSnapshotFromKoanf`
(postgres), `BuildLayer1SnapshotFromFile` (file mode), and `ApplySnapshot` — unconditional
assignment, not skip-on-nil, because (like `EnforceBrokerQuotas`) nil is a real, meaningful value
here (the permissive default), not "leave the current value alone." `pkg/hub/server.go` adds
`ServerConfig.AgentSecretsUserScopeOnly` and `(*Server).agentSecretsUserScopeOnly()` (nil → false,
permissive default).

**Enforcement** (the actual security boundary, Option B): `pkg/hub/handlers_env_secrets.go`'s
`handleAgentSecrets` rejects every PUT whose resolved scope is `project` — including an empty
scope, which defaults to project — with `403 secret_scope_restricted` (new code in
`pkg/hub/errors.go`), when the setting is on. The check sits immediately after scope resolution
and before allowProgeny/decode/type/conflict handling and the `GetMeta` existence probe, so it
can't be bypassed by `force` and the request never reaches the secret backend. (Value/Encoding
validation runs earlier and fails closed on its own terms regardless.) This is
the *only* agent-writable project-scope secret route (design F1), so this one ~15-line check
covers harness auth capture and ad-hoc `sciontool secret set` alike, with no sciontool flag,
capture marker, or harness change — the design's whole rationale for choosing Option B over the
rejected marker-based Option A (which the agent process itself could always bypass by omitting the
marker).

The admin UI (`web/src/components/pages/admin-server-config.ts`) gets a new "Agent Secrets" card
next to Quotas with an `<sl-switch>` labelled "Restrict agent-written secrets to profile scope,"
wired into the type, label map, load, and both DB-mode/file-mode payload builders — the same way
`quotas.enforce_broker_quotas` is. `pkg/hub/admin_settings.go` and `admin_settings_db.go` get the
matching `AgentSecrets`/`config.AgentSecretsSettings` field on `ServerConfigResponse` and
`ServerConfigUpdateRequest`, an `isZeroStruct`-based section-generic delete in
`applySettingsUpdates` (mirroring the pattern GoogleCloudPlatform/scion#2148 just applied to
`AutoExposePorts`, landed on `main` between this PR's phases 1 and 2), and an `"agent_secrets"`
case in `extractKoanfKeysFromRequest` / `buildSingleSectionDoc`. Section reset (`DELETE
/api/v1/admin/server-config/sections/agent_secrets`) needed **no changes** — it's already generic
via `opsettings.SectionByName`.

`pkg/hub/handlers_public_settings.go`'s `PublicSettingsResponse` gets
`AgentSecretsUserScopeOnly bool` (mirrors `AutoExposePortsEnabled`), so the unauthenticated
`/api/v1/settings/public` endpoint exposes the policy boolean to the web terminal without needing
admin rights — the same trust boundary `nativeChatEnabled`/`autoExposePortsEnabled` already sit on
(a 403 would reveal the same fact anyway).

**Web terminal capture dialog** (`terminal-pane.ts`): the Capture Auth button now calls
`openCaptureAuthScopeDialog()`, which opens the dialog immediately and fetches
`/api/v1/settings/public` fresh on every open — not cached — so an admin flipping the setting
mid-session takes effect the next time a user opens the dialog, including the race where the
dialog is already open with Project selected when the flip happens. While the fetch is in flight,
both radios and the Capture button are disabled. When the policy is on, the Project radio is
disabled (not hidden, with an explanatory hint) and the selection is forced to `user`, overriding
whatever was previously chosen. A fetch failure fails open (today's unrestricted dialog) — the
server enforces regardless, so nothing is actually at risk. `handleCaptureAuth`'s rejection
handling checks the exec output for `secret_scope_restricted` *before* the existing
`SECRET_CONFLICT_RE` check, so a policy rejection shows its own toast instead of silently falling
through to "Capture failed" or, worse, opening the force-update conflict dialog for something that
was never a secret-already-exists conflict.

## Docs

`docs-site/src/content/docs/reference/admin-settings.md`: added `agent_secrets.user_scope_only` to
the Layer-1 keys table and a new "Agent Secrets Card" bullet (item 5) alongside the existing
Quotas Card bullet (item 4), in the same style and level of detail.
`docs-site/src/content/docs/hosted/user/secrets.md`: a note after the `--scope` example explaining
that hub admins can restrict agents to user scope only, and what an agent sees when that happens.
`docs-site/src/content/docs/local/agent-credentials.md`: a note in the "Secret Scope Selection"
section covering the same restriction from the capture-dialog angle (Project disabled, retry with
`--scope user`).

## Stale issues (per design §12)

- **ptone/scion#1166** — delivered by ptone/scion#1222 (the Project/Profile capture dialog,
  `--scope`, and hub user-scope routing all already exist on `main`). Recommend closing.
- **ptone/scion#314** — partly delivered: Project and Profile scopes exist; the admin-only **Hub**
  scope option (this issue's third option) does not, and this PR does not add it (design §2
  non-goal, and design §4 Option A/C/D discussion for the rejected alternatives that *would* have
  touched that surface). Recommend retitling to track the Hub-scope option specifically, or
  closing as superseded if ptone doesn't want a Hub capture scope.

## Tests (design §10, items 1-10)

1. **Registration and default**: `TestRegistryHasAllSections` (+ `agent_secrets`),
   `TestOwningSection` (+ `agent_secrets.user_scope_only`), koanf round-trip
   (`TestAgentSecretsKoanfRoundTrip`, `TestAgentSecretsEmptyExtract`), schema accept/reject
   (`TestValidateValidDoc`/`TestValidateInvalidDoc` + `agent_secrets` cases), file-load tests
   (`TestLoadServerFromSettingsFile_AgentSecretsUserScopeOnly`/`_AgentSecretsAbsent`), and
   `TestServer_AgentSecretsUserScopeOnly_DefaultAndValues` for the accessor's nil/false/true cases.
2. **Snapshot and apply**: `TestBuildLayer1SnapshotFromFile_AgentSecrets`,
   `TestSnapshot_AgentSecretsFromDB`, `TestApplySnapshot_AgentSecretsUserScopeOnlyClearedResetsToPermissive`,
   `TestApplySnapshot_AgentSecretsUserScopeOnlyAppliedTracking`.
3. **Admin API**: file-mode round-trip incl. GET
   (`TestHandlePutServerConfig_AgentSecretsUserScopeOnly_PersistedAndAppliedWithoutRestart`), and
   DB-mode round-trip, cross-replica propagation, empty-PUT reset, non-boolean rejection, and
   section reset (`TestPutServerConfigDB_AgentSecrets_*`, `TestResetSection_AgentSecretsDeleteResetsToPermissive`)
   — all mirroring their `Quotas` counterparts. Plus `applySettingsUpdates` unit tests
   (nil-field-deletes-section, section-generic zero check, nil-request-preserves).
4. **Enforcement, full table**: new `pkg/hub/agent_secret_scope_policy_test.go` —
   `TestAgentSecretScopePolicy_Table` covers setting ∈ {nil, false, true} × scope ∈
   {"", project, user} × force ∈ {false, true} (18 cases) against a real httptest `*Server` with a
   `scopePolicyCountingBackend` wrapper proving the backend `Set`/`GetMeta` are never reached on a
   restricted write; `_InvalidEncodingStillForbidden` proves the check runs before the value is
   decoded; `_UserIdentityProjectWriteUnaffected` is the non-goal guard; `_LiveToggleViaApplySnapshot`
   proves the live flip with no restart.
5. **Public settings**: `TestPublicSettingsAgentSecretsUserScopeOnly` (nil/false/true tri-state,
   mirroring `TestPublicSettingsNativeChat`'s shape with the default direction inverted).
6-9. **Web dialog** (`terminal-pane.test.ts`, new "Capture Auth scope dialog" describe block):
   setting off (both radios enabled, default project, exec sends `--scope project`); setting on
   (Project disabled with the exact hint text, selection forced to user even when project was
   previously selected, exec sends `--scope user`); settings fetch failure (dialog shows today's
   unrestricted state); rejection (`secret_scope_restricted` in the exec output shows the policy
   toast, not "Capture failed", and never opens the conflict dialog).
10. **Admin page** (`admin-server-config.test.ts`, new "Agent Secrets card" describe block): the
    card's hint text renders; the switch loads unchecked when the section is absent and checked
    when `user_scope_only: true`; both payload builders (`buildLayer1Payload` for DB mode,
    `buildFilePayload` for file mode) send `agent_secrets.user_scope_only` on save; the
    env-override badge shows and the switch is hidden when the koanf key is env-pinned.

## Real-hub validation (no Docker/Postgres available by default in this sandbox)

Full transcript: `/scion-volumes/scratchpad/projects/broker-settings/notes/fr-2291-phase1-validation.md`.

**File mode**: built the binary from this branch, ran a real hub server process (SQLite, a known
`--session-secret`), seeded an `Agent` row directly via the real `entadapter`/`entc.OpenSQLite`
store code (bypassing only the Docker-dependent dispatch step, since no container runtime is
installed in this sandbox), and minted a real agent JWT against the server's actual derived
signing key. Captured genuine HTTP 403 `secret_scope_restricted` / 201 responses, in both
directions, by hand-editing `settings.yaml` and triggering the existing generic admin-PUT reload
path (phase 1 was validated before phase 2's dedicated `agent_secrets` admin fields existed).
Confirmed `force:true` doesn't bypass it, a token without `OriginUserID` gets the pre-existing,
distinct `forbidden` error (not swallowed by the new check), and a token with ancestry can still
write user scope while the restriction is on.

**DB mode (real Postgres)**: `apt-get install postgresql` succeeded in this sandbox (added after
an EM follow-up specifically asked for it, since the DB admin path is gated on `IsPostgres()`).
Installed Postgres 15, started the hub against it, confirmed `agent_secrets` seeds into the real
`hub_settings` table on boot, then round-tripped the *real* `PUT /api/v1/admin/server-config` and
`DELETE .../sections/agent_secrets` against the running process: setting off → 201, admin PUT on →
403 (same running process, no restart), section reset → 201 again (same running process, no
restart). Cross-checked directly against the Postgres tables (`hub_settings` row absent after
reset; the two allowed secrets present, the one rejected write absent).

## Verification

- `go build -buildvcs=false ./...`, `go vet ./pkg/config/... ./pkg/hub/...`: clean.
- `gofmt -l` on every changed `.go` file: clean.
- `golangci-lint run --new-from-rev=upstream-main ./pkg/config/... ./pkg/hub/...`: 0 issues.
- Targeted `go test -run 'AgentSecret|Quota|ApplySnapshot|Snapshot_|PublicSettings' ./pkg/hub/...`:
  pass (every test this PR added or touched).
- `go test ./pkg/config/...`: 25 pre-existing failures (all `auto_expose_ports` koanf-decode
  related), identical test set before and after this change — confirmed by diffing the failure
  list against this branch with the changes stashed out.
- `make test-fast`: 2 pre-existing failures unrelated to this PR
  (`pkg/runtimebroker`'s `TestEnvGather_*`/`TestBuildInfoProfiles_*` — confirmed identical on
  upstream main via a detached worktree; `pkg/sciontool/supervisor`'s
  `TestNativeTelemetryPolicyEffectiveChildEnv` — a `CLAUDE_CODE_ENABLE_TELEMETRY` env leak from the
  sandbox, reproduces the same way on upstream main with the var present and disappears when
  scrubbed).
- `make test-hub-sqlite`: 2 pre-existing failures, both already tracked —
  `TestCatalogHTTPEntryPoints_LiveMethodCheck` is GoogleCloudPlatform/scion#2146 (confirmed on
  upstream main before that fix merged; the EM confirmed #2146 landing should clear it — verify on
  the post-rebase CI run), `TestHandleAgentMessage_LogCapture_RawContentRedacted`'s control case is
  the known flake ptone/scion#2337 (not reproducible in isolation on either branch). Same 2 tests
  failed identically in the PR's own CI run.
- `npm run typecheck`, `npm run test -- admin-server-config` (39/39),
  `npm run test -- terminal-pane` (30/30): all pass.
- `npm run lint` (eslint) on the four changed web files, diffed before/after against each file's
  own baseline: 0 new errors; 1 new "Missing return type on function" warning per changed file (2
  total), matching the identical pre-existing pattern already present ~150+ times combined across
  these two files — not a new category of issue, and not fixed, to stay consistent with every
  sibling toggle handler already in the same files.
- Rebased onto upstream main twice during development (once before phase 2, once again per the
  EM's request before reporting phase 2/3/4 ready) — both rebases were clean, no conflicts.
