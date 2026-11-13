# Flat Runtime Brokers P4.3: Managed-Agent Selection & Hosted Profile Defaults Disposition

Status: **implemented** (ptone/scion#3284, parent ptone/scion#3281, top-level ptone/scion#2926). This document extends `.design/flat-runtime-brokers-contract.md` (§8–10) and `.design/flat-runtime-brokers/contract-appendix-p3.md` (`FC6`, `FC8`, `FC9`, `FC10`) and governs the P6 cleanup slices (ptone/scion#3292 and ptone/scion#3293).

## 1. Scope & Maintainer Ruling

Per binding maintainer direction (`ptone`, 2026-10-13 on ptone/scion#3284):

> *"Per maintainer direction, a Hub is required; local no-Hub profile selection is out of scope."*

Every Scion agent runs through a Hub (Workstation combo server, Single-node hosted, or HA hosted). Standalone no-Hub local preset selection is retired and out of scope. P4.3 therefore establishes:

1. An explicit, first-class **managed-agent selection contract** on Hub agent creation (`managedAgent: true` on `CreateAgentRequest`) decoupled from Runtime Broker profiles, plus a **bounded legacy adapter** at the Hub request entry boundary for callers still sending `profile: "managed-agents"` during the flat Runtime Broker transition.
2. An exhaustive **disposition inventory** for every remaining persisted or configured hosted profile default surface across user preferences, Runtime Broker records, project annotations/settings, historical agent records, and host `settings.yaml` files.

---

## 2. Explicit Managed-Agent Selection Contract & Bounded Legacy Adapter

### 2.1 Wire Contract

- **Hub & `hubclient` API (`CreateAgentRequest`):**
  - Primary selector: `ManagedAgent bool` (`json:"managedAgent,omitempty"`) on `hub.CreateAgentRequest` (`pkg/hub/handlers_agents_core.go`) and `hubclient.CreateAgentRequest` (`pkg/hubclient/agents.go`).
  - Routes covered: both `POST /api/v1/agents` and `POST /api/v1/projects/{projectId}/agents` (shared `createAgentInProject` entry point).
  - Existing-agent collision guard: execution modes never cross-adopt or cross-start — sending `managedAgent: true` (or normalized `profile: "managed-agents"`) over a pre-existing container agent, or sending a container create/resume over a pre-existing `managed:*` agent, returns `409 conflict` before any broker/pin mutation runs.
- **CLI & UI callers during P4/P5 coexistence:**
  - Existing CLI callers sending `--profile managed-agents` continue working transparently through the Hub entry-boundary normalization below (including against flat-broker and brokerless projects). Dedicated CLI flag wiring (`--managed-agent` on `scion start` / `scion create`) switches `cmd/` callers onto `ManagedAgent: true` prior to P6.1 (`ptone/scion#3292`) removing `--profile` and `ManagedAgentsProfile`.

### 2.2 Entry-Boundary Normalization & Contradiction Validation

`normalizeManagedAgentCreateRequest` (`pkg/hub/handlers_managed_agents.go`) runs at the top of `createAgentInProject` **before** any Runtime Broker lookup, `flatCreatePlacement` check, project default resolution, quota reservation, or store write:

1. **Bounded legacy adapter (`ManagedAgentsProfile = "managed-agents"`):**
   - If `req.Profile == "managed-agents"`, `normalizeManagedAgentCreateRequest` sets `req.ManagedAgent = true` and clears `req.Profile = ""`.
   - Sending `{managedAgent: true, profile: "managed-agents"}` normalizes identically to `{managedAgent: true, profile: ""}`.
   - No downstream create helper, `flatCreatePlacement` validator, project default resolver, or persisted `AppliedConfig`/`CreateInputs` record ever sees `req.Profile == "managed-agents"`.
   - `ManagedAgentsProfile` is retained solely for this entry-boundary normalization during migration and is scheduled for removal in P6.1 (ptone/scion#3292).
2. **Contradictory request rejection (`400 Bad Request`, `validation_error`):**
   - `req.ManagedAgent == true` with non-empty `req.Profile` (after normalizing `"managed-agents"`) fails with `details.field = "profile"`.
   - `req.ManagedAgent == true` with non-empty `req.RuntimeBrokerID` fails with `details.field = "runtimeBrokerId"`.
   - `req.ManagedAgent == true` with non-empty `req.ExpectedRuntimeTargetID` fails with `details.field = "expectedRuntimeTargetId"`.

### 2.3 Brokerless & Flat-Broker Project Invariants

When `req.ManagedAgent == true` (whether passed directly or normalized from `profile: "managed-agents"`):

- **No Runtime Broker resolution or placement:** `resolveRuntimeBroker`, `checkBrokerDispatchAccess`, `requireEmptyPerAgentBrokerCapability`, `flatCreatePlacement`, and workspace-bootstrap upload URL generation are bypassed completely. Managed-agent creation succeeds on projects with **zero** linked Runtime Brokers as well as projects whose default or only Runtime Brokers are flat instances (with `hub.flat_runtime_brokers` enabled or disabled).
- **Unpinned, brokerless Hub record:** `Agent.RuntimeBrokerID` is `""`, `PinnedRuntimeBrokerID` / `RuntimeTargetID` / `RuntimeTargetType` remain empty (`PinnedPlacement{}`), and `Agent.Runtime` is recorded as `managed:<provider>` (e.g. `managed:google`).
- **Quota accounting:** Per-project (`max_agents_per_project`) quota is enforced and released on rollback/delete; no `max_agents_per_broker` reservation is acquired because no Runtime Broker is used.
- **Project profile defaults skipped:** Neither `scion.io/active-profile` (`applyProjectDefaults`) nor `scion.io/default-gcp-identity-service-account-id-by-profile` (`profileDefaultSAUnlessPinned`) is applied, and `flatDefaultProfileDroppedWarning` is not emitted for managed agents.

---

## 3. Hosted Profile Defaults Inventory & P6 Disposition

Every remaining hosted profile surface has one of three P6 dispositions:

- **Migrated:** Replaced by flat Runtime Broker instance settings or project/broker-scoped replacements prior to P6 removal.
- **Rejected:** Requests supplying the legacy profile field are refused with an explicit `400`/`422` validation error once compatibility closes in P6.1 (ptone/scion#3292).
- **Retained read-only for history:** Stored JSON/column data on pre-P6 records remains readable without deserialization failure, is never written on new operations, and never drives runtime dispatch.

### 3.1 Summary Matrix

| Surface | Code & Storage Locations | P4 / P5 Coexistence Behavior | P6 Disposition (ptone/scion#3292 / ptone/scion#3293) |
|---|---|---|---|
| **Managed-agent create sentinel** (`CreateAgentRequest.Profile == "managed-agents"`, CLI `--profile managed-agents`) | `pkg/hub/handlers_managed_agents.go` (`ManagedAgentsProfile`, `normalizeManagedAgentCreateRequest`), `pkg/hub/handlers_agents_core.go`, `pkg/hubclient/agents.go` | Normalized at Hub entry into `ManagedAgent: true` + `Profile: ""` (P4.3). Primary Hub/`hubclient` contract is `managedAgent: true`. | **Rejected / deleted in P6.1** (ptone/scion#3292): wire CLI `--managed-agent` onto `ManagedAgent: true` and remove `CreateAgentRequest.Profile`, CLI `--profile` on `start`/`create`, and `ManagedAgentsProfile`. |
| **User preference `defaultProfile`** | `PATCH /api/v1/users/me` (`pkg/hub/handlers_users_core.go`), `store.UserPreferences.DefaultProfile` (`pkg/store/models.go`), `pkg/store/entadapter/user_store.go` (`users.preferences` JSON) | Stored/returned on `users/me` JSON; Hub server-side create paths do not read `UserPreferences.DefaultProfile` (only legacy UI read it). Ignored for flat brokers and managed agents. | **Rejected on write / ignored on read:** P6.1 (ptone/scion#3292) removes `defaultProfile` from `UpdateUserPreferences` / API response (or rejects non-empty writes with `400`); P6.2 (ptone/scion#3293) ignores any stale `"defaultProfile"` JSON key in existing `users.preferences` rows (non-destructive; no SQL column drop). |
| **Runtime Broker `DefaultProfile` & `Profiles`** | Join/heartbeat/API (`pkg/hub/brokerauth.go`, `pkg/hub/handlers_runtime_brokers.go`, `pkg/hubclient/runtime_brokers.go`, `pkg/hubclient/types.go`), `store.RuntimeBroker.{DefaultProfile,Profiles}` (`pkg/store/models.go`), `pkg/store/entadapter/project_store.go` (`runtime_brokers.default_profile`, `runtime_brokers.profiles`) | Flat brokers (`broker.IsFlat()`) already force `Profiles = nil` and `DefaultProfile = ""` on join, heartbeat, and store write (`FC4`, `FC9`). Legacy brokers continue reporting profiles through P5 compatibility (`FC6` `ProfileMap`). | **Deleted from wire/API in P6.1; schema retired in P6.2:** P6.1 (ptone/scion#3292) deletes hosted `Profiles`/`DefaultProfile` from join/heartbeat/broker APIs, `resolveAgentRuntimeProfileType`, and `reincarnate_move` profile checks. P6.2 (ptone/scion#3293) stops reading/writing the Ent fields; if `runtime_brokers.default_profile` or `runtime_brokers.profiles` columns are dropped via migration, `pkg/version.DowngradeFloor` **must** be bumped to the P6 release (`below_downgrade_floor`). |
| **Project active profile (`scion.io/active-profile` / `activeProfile`)** | `PUT/GET /api/v1/projects/{id}/settings` (`pkg/hub/project_settings_handlers.go`), `applyProjectDefaults` (`pkg/hub/project_settings_handlers.go`, `pkg/hub/handlers_agent_create_helpers.go`) (`projects.annotations` JSON) | Legacy broker creates still read `scion.io/active-profile` when `req.Profile == ""`. Flat broker creates drop it and emit `flatDefaultProfileDroppedWarning` (`FC8`). Managed-agent creates skip it silently (P4.3). | **Migrated + rejected on write:** Workload defaults move to `server.broker.instances[].agent_defaults` (`FC1`, `scion runtime-broker convert`) and project `defaultRuntimeBrokerId`. P6.1 (ptone/scion#3292) rejects `activeProfile` writes on `PUT /api/v1/projects/{id}/settings` and deletes `applyProjectDefaults` profile injection. P6.2 (ptone/scion#3293) ignores/prunes residual `scion.io/active-profile` annotation keys (JSON key only; no SQL column drop). |
| **Project per-profile GCP SA map (`scion.io/default-gcp-identity-service-account-id-by-profile`)** | `pkg/hub/default_gcp_identity.go` (`projectProfileDefaultSA`, `validateProjectProfileDefaultSAs`), `pkg/hub/project_settings_handlers.go`, `pkg/hub/flat_runtime_brokers.go` (`profileDefaultSAUnlessPinned`) (`projects.annotations` JSON) | Legacy broker creates consult `by-profile` SA defaults. Flat pinned agents skip `by-profile` lookup via `profileDefaultSAUnlessPinned` (`FC10`). Managed agents skip `by-profile` lookup (P4.3). | **Migrated + rejected on write:** Replaced by node-local Kubernetes SA mappings (`FC10`) and per-broker/project default GCP SA identity (tracked before flag removal in ptone/scion#4895). P6.1 (ptone/scion#3292) rejects `defaultGCPIdentityServiceAccountIDByProfile` writes on project settings and removes `projectProfileDefaultSA`. P6.2 (ptone/scion#3293) ignores residual annotation keys in `projects.annotations`. |
| **Historical agent config (`AppliedConfig.Profile`, `CreateInputs.Profile`)** | `store.AgentAppliedConfig.Profile`, `store.AgentCreateInputs.Profile` (`pkg/store/models.go`), `pkg/hub/reincarnate_config.go` (`agents.applied_config` JSON) | Flat creates and managed creates persist `Profile: ""` (`FC9`, P4.3). Legacy broker creates record the resolved profile. Reincarnate skips profile re-derivation for pinned agents (`reincarnate_config.go`). | **Retained read-only for history:** Keep JSON tags on `AgentAppliedConfig.Profile` and `AgentCreateInputs.Profile` (marked deprecated/historical in doc comments in P6.2 ptone/scion#3293) so pre-P6 agent audit/history rows deserialize cleanly; never populated on new creates and never consulted by dispatch or reincarnate. |
| **Host / contributor `settings.yaml` (`active_profile`, `profiles`, `runtimes`)** | `pkg/config/settings_v1.go`, `pkg/config/profile_conversion.go`, `pkg/brokerconvert/`, `cmd/runtime_broker.go` (`scion runtime-broker convert`) | Operators convert legacy host configs to `server.broker.instances[]` via `scion runtime-broker convert --plan|--apply|--rollback` (`FC1`, `FC6`). Local no-Hub execution is retired (ptone/scion#3284 ruling). | **Migrated then rejected:** After P5.4 (ptone/scion#3290) compatibility closes, P6.1/P6.2 (ptone/scion#3292 / ptone/scion#3293) remove legacy profile-resolving broker startup and reject unconverted `profiles`/`active_profile`/`runtimes` broker runtime configs with a clear message pointing to `server.broker.instances` / `scion runtime-broker convert`. |

### 3.2 Database Migration & `DowngradeFloor` Rule for P6.2 (`ptone/scion#3293`)

- Annotation-backed and JSON-backed fields (`users.preferences`, `projects.annotations`, `agents.applied_config`) require **no destructive SQL schema migration**: ignoring or omitting obsolete JSON keys on read/write is backward-compatible with older binaries and does not require a `pkg/version.DowngradeFloor` bump.
- Relational columns on `runtime_brokers` (`default_profile`, `profiles`):
  - Leaving the nullable/defaulted columns unused in SQL while removing API/domain references is non-destructive.
  - Dropping `runtime_brokers.default_profile` or `runtime_brokers.profiles` in `pkg/ent/entc/client.go` is a **destructive DB migration** and **must** bump `pkg/version.DowngradeFloor` to the first release containing that drop so running Hubs refuse downgrade targets older than the floor (`below_downgrade_floor`).
