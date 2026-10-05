# Flat Runtime Brokers: P1 contract appendix

Status: Stage A of ptone/scion#3267 (P1.1). **Revision 2** answers review round 1 (B1–B3, N1–N14, T1–T5); the change log is at the end. Parent phase: ptone/scion#3266. Delivery tracker: ptone/scion#2926.

This appendix fixes the names and rules that P1.2 (ptone/scion#3268), P1.3 (ptone/scion#3269) and later phases build on. Changing a frozen name after acceptance requires another review of this document. Where this document differs from the original illustrative design (for example its `schema_version: "2"` and top-level `runtime_brokers` list), this document wins.

Reconciled against upstream main **`28eb4f0a5827854837d18b28b18e26688d1641cd`**, fetched 2026-10-05. That is also the planning SHA and the current fork main. The original evaluation used `b8735e7`; section 1 records what changed in between.

Terms follow `GLOSSARY.md`. Always write **Runtime Broker**, never bare "broker", in user-facing text.
- **Flat Runtime Broker**: a Runtime Broker identity whose Hub row has a stored runtime target. It serves exactly one target.
- **Runtime Broker instance**: the in-process object that hosts one flat identity.
- **Legacy Runtime Broker**: today's profile-resolving Runtime Broker. Its row has no stored target.

**Central invariant.** Nothing in P1 retargets an existing Runtime Broker ID or moves an agent's pinned placement:
- No automatic writer converts a legacy row or agent into a flat one.
- No automatic writer moves a pinned agent.
- No automatic writer adopts a flat row by name.

Sections 6, 7 and 8 list every writer and the guard that holds this.

## 1. Source-map reconciliation (b8735e7 → 28eb4f0)

The git history here is shallow, so the comparison is by tree content. Of roughly 1.6k files changed, 129 are on the paths this work touches (`pkg/config`, `pkg/ent/schema`, `pkg/store`, `pkg/runtimebroker`, Hub create/dispatch/heartbeat, registration commands), at about +29k/-1.4k lines. The table lists the changes that affect this plan.

| Area | Current main | Effect on the plan |
|---|---|---|
| Run identity | `agents.run_id`, written only by `SetAgentRunID` and `CompareAndSwapAgentRunID`. Also the `scion.run_id` label, the `launch_*` columns, async launch, `beginSyncStart` and `startsInFlight` | Pinned placement uses its own columns and never touches the run ID. A mismatch is rejected before any of these are written (section 9). |
| Start claims | `start_claim_*` columns and `ClaimAgentStart(target)`. The Hub wiring is in flight (ptone/scion#3081, ptone/scion#3091, GoogleCloudPlatform/scion#2544). `start_claim_target` holds the *inventory target key* | The opaque runtime target ID is a different value and is never written there (section 11). |
| Run intent | `run_intent` and `run_intent_at`, plus `run_intent_marked_at` in ptone/scion#3081 to detect writes by older code | Same pattern used here: separate columns with dedicated setters (section 8). |
| Recovery | The `BrokerTargetInventory` table, recovery observations, and the reaper's "fresh complete inventory of its target" | The inventory key (`auxiliaryRuntimeIdentity`) is unchanged in P1. |
| Observed target | `applied_config.runtimeTarget` and `runtimeTargetCandidate`, promoted by `nextRuntimeTarget` and cleared by `forgetRuntimeTarget` | Semantics unchanged. The pin uses a different name (section 8). |
| Existing-agent routing | `recorded_runtime.go` (ptone/scion#2748), with the `?runtime=` query parameter | A flat instance has one runtime and either matches it or refuses. No new routing. |
| Profile fallback | `resolveManagerForOpts`/`ResolveRuntime` fall back silently, including `ResolveRuntime("")` falling back to the settings `active_profile`. `buildStartContext` writes markers before it resolves anything | A flat instance bypasses that resolution entirely (section 10). |
| Registration and orphan writers | Embedded `registerGlobalProjectAndBroker` adopts a row by name, rewrites `Profiles`/`DefaultProfile` on every start, then runs **`FindOrphanedAgents` → `ReassignAgentsToBroker` → `ReassignProjectBroker` → `MarkBrokerOffline`**, which bulk-moves every non-terminal agent on an offline or missing Runtime Broker. brokerauth `FindExistingBroker` matches by name first. The deprecated `RegisterProject` with `broker` matches by ID, then name, and overwrites name/slug/profiles. Heartbeats refresh profiles and default profile | Every one of these writers gets a flat guard (sections 6 and 8). The orphan writers get P1.1 store guards. |
| Store write semantics | `UpdateAgent` and `UpdateRuntimeBroker` overwrite all known columns, including the `applied_config` and `runtimes` JSON | New data goes in new columns that those updates leave out (section 8). |
| Server config path | The server reads `GlobalConfig.RuntimeBroker` (`RuntimeBrokerConfig`, camelCase) through `LoadGlobalConfig` → `loadServerFromSettingsFile` → `ConvertV1ServerToGlobalConfig`. The legacy `server.yaml` path still exists. A `server` section that fails to unmarshal is dropped silently | Section 2 freezes both struct shapes, the conversion, the `server.yaml` rule and a strict loader. |
| Migrations | ent auto-migrate (`WithDropColumn(false)`, `WithDropIndex(false)`) followed by Go backfills. Postgres tests run under `-tags integration` with `SCION_TEST_POSTGRES_URL` | Additive nullable columns, no backfill (section 12). |
| Quota | Per Runtime Broker only (`max_agents_per_broker`, `BrokerSettings.MaxAgents`) | Each flat Runtime Broker is its own scope. Aggregate limits are a P3/P5 question. |
| Conduit | `conduit_session.exec_scope`, `endpoint_incarnation` and `connection_epoch` exist. Nothing computes a Runtime Broker exec scope. Gated by `hub.conduit` | Section 13 records the fencing contract that flat dispatch preserves. |
| Settings | Storage and eviction keys on runtime/profile entries. `schema_version` is not checked at load. Unknown keys are dropped silently | Schema stays `"1"` with an additive key and a strict loader (section 2). |
| Experiments | Server and web layers, `Server.experimentEnabled`, `requireExperiment` (no production callers), `dispatchExperimentNames` | `hub.flat_runtime_brokers` is a server-layer experiment (section 14). |

In-flight work this was checked against:
- ptone/scion#3081 and ptone/scion#3091 (start claims, queued stops, recovery)
- ptone/scion#3180 (per-dir shared-dir backends in `settings_v1.go` and the schema)
- GoogleCloudPlatform/scion#2544 (start-claim runner)
- GoogleCloudPlatform/scion#2480 (metadata mode)
- GoogleCloudPlatform/scion#2479 and GoogleCloudPlatform/scion#2483 (Conduit)
- the workspace-recreation wire contract for GoogleCloudPlatform/scion#1931 (start/restart carry inputs through `StartExtras`)

P1.1 only adds to those. It does not edit `cmd/server_foreground.go`, `cmd/server_broker.go`, `pkg/hub/httpdispatcher.go`, `pkg/runtimebroker/handlers.go` or `pkg/runtime/k8s_runtime.go`.

## 2. Settings: schema version, key placement and the server path

- **The schema version stays `"1"`.** The flat configuration is one optional key. A v2 schema would duplicate the entire v1 schema and change nothing at load, because `schema_version` is not checked there.
- **Settings-file key: `server.broker.instances`**, a list. It does **not** use the singular top-level `runtime` key (`V1RuntimeDefaultsConfig`, the global behaviour defaults), `runtimes` or `profiles`. Process-wide listener and Hub-connection settings (`server.broker.port`, `host`, `hub_endpoint`, …) stay where they are and are shared by every hosted instance in P2.
- **An empty list (`instances: []`) means legacy**, the same as absent.

### Frozen Go shapes

| Layer | Field | Type | Tags |
|---|---|---|---|
| Settings file (`pkg/config/settings_v1.go`) | `V1BrokerConfig.Instances` | `[]V1RuntimeBrokerInstanceConfig` | `instances` (json/yaml/koanf, omitempty) |
| | `V1RuntimeBrokerInstanceConfig` | `Key string`, `Name string`, `RuntimeTarget *V1RuntimeTargetConfig` | `key`, `name`, `runtime_target` |
| | `V1RuntimeTargetConfig` | `Type`, `DisplayName`, `Context`, `Namespace` (string) | `type`, `display_name`, `context`, `namespace` |
| Server config (`pkg/config/hub_config.go`) | `RuntimeBrokerConfig.Instances` | `[]RuntimeBrokerInstanceConfig` | `instances` (camelCase family, like the rest of this struct) |
| | `RuntimeBrokerInstanceConfig` | `Key`, `Name`, `RuntimeTarget *RuntimeTargetConfig` | `key`, `name`, `runtimeTarget` |
| | `RuntimeTargetConfig` | `Type`, `DisplayName`, `Context`, `Namespace` | `type`, `displayName`, `context`, `namespace` |

- `ConvertV1ServerToGlobalConfig` copies `V1BrokerConfig.Instances` into `RuntimeBrokerConfig.Instances`, and `ConvertGlobalToV1ServerConfig` copies them back. Both are deep copies, field for field.
- `context` and `namespace` are Kubernetes-only. They are defined but not implemented in P1.

### Where it is read

- "Global only" means **read through `LoadGlobalConfig`**: the settings.yaml whose `server` key `loadGlobalConfigFromSettings` uses. That is the global directory, or the `--config` directory only when the global settings have no `server` key.
- Project-level settings are never consulted. A `server.broker.instances` in a project settings file has no effect on the server (tested).
- **Legacy `server.yaml`: not supported.** If `loadGlobalConfigLegacy` produces a non-empty `RuntimeBroker.Instances`, `LoadGlobalConfig` returns an error: "runtimeBroker.instances is only supported under server.broker.instances in settings.yaml".
- **Strict loader (P1.1):** `config.LoadRuntimeBrokerInstances(configPath string) ([]V1RuntimeBrokerInstanceConfig, error)`.
  - It resolves the same settings.yaml as `loadGlobalConfigFromSettings`.
  - It decodes the raw `server.broker.instances` node with unknown fields disallowed and runs `ValidateRuntimeBrokerInstances`.
  - A type error or unknown key is an explicit error. This closes the gap where `loadServerFromSettingsFile` silently drops a `server` section that fails to unmarshal.
  - P1.2 startup calls it and refuses to start on an error. It also refuses if the result differs from `GlobalConfig.RuntimeBroker.Instances`, which would mean the silent-fallback path was taken.

### One-entry Docker example (invented values)

```yaml
schema_version: "1"
server:
  broker:
    enabled: true
    instances:
      - key: local-docker
        name: example-docker
        runtime_target:
          type: docker
          display_name: Local Docker
```

- `key` is required. It is the immutable local instance key (section 3).
- `name` is **required in P1**. It is the Runtime Broker name registered with the Hub: a mutable label, not identity. Requiring it avoids a default that changes silently when `key` changes (see section 3).
- `runtime_target.type` is required. P1 accepts `docker` only. `kubernetes` is defined and rejected as not implemented yet. Any other value is unsupported.
- `display_name` is optional, non-sensitive, and shown in the UI. It defaults to the type.
- There are no per-instance `agent_defaults` in P1. P3.1 adds them additively.

### Validation

`ValidateRuntimeBrokerInstances(instances) []ValidationError`. Each error names its settings path. The JSON schema (`$defs/runtimeBrokerInstance` with `additionalProperties: false`) mirrors these rules for `scion config validate`.

| Condition | Error (path, message gist) |
|---|---|
| Duplicate `key` | `server.broker.instances[i].key`: duplicate instance key "k" (also at index j). Reported even when the count rule also fails |
| More than one entry | `server.broker.instances`: only one Runtime Broker instance is supported in this release |
| `key` missing or not matching `^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$` | invalid instance key |
| `name` empty | name is required |
| `runtime_target` missing or `type` empty | runtime_target.type is required |
| `type: kubernetes` | Kubernetes Runtime Broker instances are not implemented yet |
| Any other `type` | unsupported runtime target type "x" (supported: docker) |
| Docker entry with `context`/`namespace` | field not valid for runtime target type docker |

Other rules:
- `server.broker.broker_id` (and every other legacy ID source) keeps its current meaning: the legacy Runtime Broker identity. It never seeds a flat ID.
- When `instances` is non-empty, the P1 process hosts only the flat instance and does not run the legacy embedded registration or the orphan reassignment (section 6). It logs that the legacy identity is not hosted.
- The co-located `SettingsOverlay` never reads or writes `server.broker.instances` (tested).
- `active_profile`, `profiles` and `runtimes` are left untouched for the local CLI. A flat instance never consults them.
- Relation to ptone/scion#3180: different `$defs` and functions. Any conflict is only textual adjacency.
- **Rollback:** an older binary that rewrites global settings.yaml (e.g. through the admin settings handlers that call `LoadModifySaveVersionedSettings`) drops `server.broker.instances`. The identity directory (section 4) survives. Re-adding the entry with the same `key` restores the same identity (tested).

## 3. Immutable local instance key

- `key` names the instance's state directory and is recorded in the identity file. It is the local identity anchor.
- Changing `key` means a different instance. The old directory is simply not loaded, and there is no rename path.
- Because `name` is required and is not derived from `key`, an operator who changes only `key` keeps the same `name`. The new identity's registration is then refused by the name/slug guard (section 6) instead of silently creating a duplicate. An operator who changes both deliberately gets a new, separate Runtime Broker.
- The key is local only and is not sent to the Hub in P1.

## 4. Stable opaque IDs and local persistence

- **Runtime Broker ID**: a UUID minted locally at first boot. It is the existing `runtime_brokers.id`. It is never derived from settings, hostname or name.
- **Runtime target ID**: an independent random UUID, also minted at first boot. It is opaque and unique per instance; a target ID never belongs to two Runtime Broker IDs (enforced by a unique index, section 8).
- **Identity file:** `<GetGlobalDir()>/runtime-brokers/<key>/identity.json`, normally `~/.scion/runtime-brokers/<key>/identity.json`.
  - Directory mode 0700, file mode 0600.
  - Written before any registration attempt.
- Instance-scoped Hub credentials (P1.2) go in `<...>/runtime-brokers/<key>/hub-credentials/<hub>.json`, using the `brokercredentials` format. P1.1 reserves the path only.

```json
{
  "schemaVersion": 1,
  "instanceKey": "local-docker",
  "runtimeBrokerId": "6f1c…",
  "runtimeTarget": { "id": "0b9e…", "type": "docker" },
  "executionScope": { "type": "docker", "docker": { "daemonId": "…", "endpoint": "unix:///var/run/docker.sock" } },
  "createdAt": "2026-10-05T00:00:00Z"
}
```

### Package and creation protocol

The code lives in a new package, `pkg/brokeridentity`, with no runtime or Hub dependency. The caller probes the Docker scope (P1.2) and passes it in.

`LoadOrCreate(dir, key, targetType, observedScope, legacyBrokerIDs)`:
1. `mkdir -p` the directory (0700), then take an exclusive `flock` on `<dir>/.lock` for the whole operation.
2. If `identity.json` exists, load and verify it (table below).
3. Otherwise, decide whether this is first boot. **It is first boot only if the directory holds nothing except `.lock` and leftover temp files matching `.identity.json.tmp-*`**, which are removed. Any other entry means "other state exists".
4. On first boot:
   - mint both IDs;
   - write to a temp file and fsync it;
   - **`os.Link` it to `identity.json`**, which fails if the target exists, so a file is never replaced;
   - remove the temp file, fsync the directory, then re-read `identity.json` and return what is on disk.
   - If the link fails with EEXIST (a racer outside the lock, e.g. a different filesystem lock domain), load the winner's file instead.

`legacyBrokerIDs` holds every legacy identity source the caller can see:
- `server.broker.broker_id`
- legacy `hub.brokerId`
- `GlobalConfig.RuntimeBroker.BrokerID`
- the `brokerId` in `broker-credentials.json` and in each `hub-credentials/*.json`

If the identity's `runtimeBrokerId` equals any of them, that is an error (`ErrIdentityCollidesWithLegacy`).

These fields never change once written: `instanceKey`, `runtimeBrokerId`, `runtimeTarget.id`, `runtimeTarget.type`, and the identity part of `executionScope` (section 5). The file has no mutable fields in P1.

| Local state | Result |
|---|---|
| Directory missing, or holding only `.lock`/temp files | First boot: mint, record the observed scope, write |
| `identity.json` missing and any other entry present | `ErrIdentityMissing`: "identity state for instance \"k\" is missing but other state exists; restore it or remove the directory to register a new Runtime Broker". Never re-minted |
| Unreadable file, invalid JSON, unknown `schemaVersion`, or empty required field | `ErrIdentityCorrupt`. Never re-minted |
| `instanceKey` differs from the directory key | `ErrIdentityKeyMismatch` |
| Recorded type differs from the configured `runtime_target.type` | `ErrRuntimeTargetTypeChanged` |
| `runtimeBrokerId` equals a legacy ID | `ErrIdentityCollidesWithLegacy` |

Removing the whole directory is the operator's explicit request for a new identity. It produces a new Runtime Broker ID and never takes over the old registration or its agents. If `name` is unchanged, the Hub refuses the new registration (section 6).

## 5. Target identity per runtime

Target identity is the runtime target ID **bound to a normalized execution-scope record**. Names, display names, context aliases, file paths and credentials are never identity.

### Docker (P1)

- The probe goes through **the same CLI and command the `DockerRuntime` uses** (`DockerRuntime.Command`, normally `docker`). That way the active `docker context` / `DOCKER_CONTEXT` / `DOCKER_HOST` is honoured exactly as the runtime honours it.
  - `daemonId` = `<cmd> info --format '{{.ID}}'`
  - `endpoint` = `<cmd> context inspect --format '{{.Endpoints.docker.Host}}'`, normalized: lower-case scheme, cleaned `unix://` path, explicit port for `tcp://`. It is informational.
  - The runtime's `Host` setting is not used, because nothing in `DockerRuntime` reads it.
- **Identity field: `daemonId`.** An empty or failed `daemonId` probe is an explicit error (`ErrExecutionScopeUnidentified`) at first boot and at every start. The endpoint is never used as identity, because endpoints are aliases.
- `brokeridentity.VerifyScope(recorded, observed)` runs at every start (P1.2):
  - same `daemonId`: proceed; an endpoint change is accepted and logged;
  - different `daemonId` (another node or a reinstalled daemon): `ErrExecutionScopeChanged`, naming both values. The instance neither starts nor registers, and existing placement is not re-pointed.
- Docker has no credentials to rotate.

### Kubernetes (defined, not implemented)

- Record: `{ "type": "kubernetes", "kubernetes": { "clusterUid": <kube-system namespace metadata.uid>, "namespace": <resolved namespace>, "apiServer": <normalized URL, informational> } }`.
- **Identity fields: `clusterUid` + `namespace`.**
- Context name, kubeconfig path, user/auth entries and tokens are excluded. Rotating credentials or renaming a context keeps the target. Pointing the alias at another cluster or namespace is refused like a daemon change.
- An `apiServer` change with the same `clusterUid` is accepted and logged.
- P1.1 freezes the record shape and ships a normalization/compare unit test only.

A scope change always means a new target, which needs a new instance key and therefore a new Runtime Broker ID.

## 6. Registration descriptor and every Runtime Broker row writer

### Wire shape

```json
"runtimeTarget": { "id": "0b9e…", "type": "docker", "displayName": "Local Docker" }
```

- Go type: **`api.RuntimeTargetDescriptor { ID string "id"; Type string "type"; DisplayName string "displayName,omitempty" }`** in `pkg/api/runtime_target.go`. It is shared by store, Hub, hubclient and Runtime Broker.
- It is carried on:
  - Hub `CreateBrokerRegistrationRequest` and `BrokerJoinRequest` (`pkg/hub/brokerauth.go`);
  - hubclient `CreateBrokerRequest` and `JoinBrokerRequest` (`pkg/hubclient/runtime_brokers.go`);
  - the Runtime Broker API object as `runtimeTarget`.
- The scope record, endpoint, kubeconfig path and credentials are never sent.
- A flat registration sends no `profiles` and no `defaultProfile`.

### Shared rules

All writers apply these through **one Hub implementation**, `(*hub.Server).registerFlatRuntimeBroker` (P1.2). The embedded path calls it through an exported wrapper `(*hub.Server).RegisterEmbeddedFlatRuntimeBroker`, so the experiment snapshot is the Hub's own `experimentEnabled`.

- **R1 Experiment.**
  - With `hub.flat_runtime_brokers` off, a new flat registration (no row with this ID) gets 412 `experiment_disabled`.
  - Re-registration of an **existing flat** row with the same target is accepted, so its agents are not stranded.
  - A malformed experiment snapshot fails closed, which means off.
- **R2 Identity match.** A flat registration is matched **by Runtime Broker ID only**. If its `name` or `slug` equals that of any other Runtime Broker row, it gets 409 `runtime_broker_name_conflict`.
  - The check is check-then-act with no unique constraint. Existing rows may already share names or slugs, so a unique index is out of scope. The race is accepted explicitly: two concurrent first registrations with the same name are an operator error that only the first-boot window can hit, and the identity file lock (section 4) serializes a single host.
- **R3 Target set only on creation.** The target columns are written only when this flat registration **creates** the row (`CreateRuntimeBroker` with `RuntimeTarget`).
  - Existing row with the same target ID and type: accepted; the display name may update.
  - Existing row with a different target: 409 `runtime_target_changed`.
  - Existing **legacy** row (no stored target) with this ID: 409 `runtime_broker_not_flat`. Converting a legacy row is P5 work and is never done implicitly.
- **R4 Legacy writes to flat rows.** A legacy registration (no `runtimeTarget`) against a flat row gets 409 `runtime_target_changed`. No writer stores `profiles` or `defaultProfile` on a flat row: the store rejects it (section 8) and Hub handlers drop them before writing.
- **R5 No orphan adoption.** A flat registration never runs, and is never the target of, orphan reassignment (B1 guards, section 8).

### Every writer

| Writer | Code (28eb4f0) | Flat rule | Phase |
|---|---|---|---|
| brokerauth create | `createBrokerRegistration`, `FindExistingBroker` (name first) | With `runtimeTarget`: R1–R3, ID-only match, no name adoption. Without: legacy behaviour, except that `FindExistingBroker` **skips flat rows** for name matching and an ID match on a flat row gets R4 | P1.2 |
| brokerauth join | `CompleteBrokerJoin` | Descriptor must equal the stored target (else 409 `runtime_target_changed`). `profiles`/`defaultProfile` are rejected for flat rows | P1.2 |
| Embedded legacy registration | `cmd/server_broker.go registerGlobalProjectAndBroker` | Not run when `instances` is non-empty. When it does run (legacy process), its name lookup **skips flat rows**. An ID match on a flat row is a startup error naming R4, and profiles are never written onto it | P1.2 |
| Embedded flat registration | new, through `RegisterEmbeddedFlatRuntimeBroker` | R1–R5. No orphan reassignment, no profile write | P1.2 |
| Deprecated `RegisterProject` with `broker` | `pkg/hub/handlers_projects_core.go` (ID then name lookup, name/slug/profile overwrite) | A flat row found by ID gets 409 `runtime_target_changed`. A flat row found only by name is skipped (no adoption): the request then fails with 409 `runtime_broker_name_conflict` instead of creating a duplicate name. Never writes profiles to a flat row | P1.2 |
| Heartbeat refresh | `pkg/hub/handlers_runtime_brokers.go` (capabilities, workspace storage, default profile, profile attach) | For a flat row, `DefaultProfile`/`ProfileAttach` from a heartbeat are dropped with a warning log. Capabilities and workspace storage refresh as today | P1.2/P1.3 |
| Any `UpdateRuntimeBroker` caller | `pkg/store/entadapter/project_store.go` | The store guard rejects non-empty `Profiles`/`DefaultProfile` on a flat row (`store.ErrFlatRuntimeBrokerProfiles`). It never writes target columns | **P1.1** |

The registration and control channel are not Conduit-fenced. Section 13 puts no constraint on this descriptor.

## 7. Agent movers and existing-agent paths

| Writer | Code (28eb4f0) | Flat rule | Phase |
|---|---|---|---|
| Orphan discovery | `AgentStore.FindOrphanedAgents` | Excludes agents with non-NULL `pinned_runtime_broker_id` and agents whose current Runtime Broker row has a stored target. Returns nothing when `currentBrokerID` is a flat row | **P1.1** |
| Orphan reassignment | `AgentStore.ReassignAgentsToBroker` | Refuses a destination row with a stored target (`store.ErrFlatRuntimeBrokerReassign`). Its update also filters out pinned agents (`pinned_runtime_broker_id IS NULL`) | **P1.1** |
| Project default repoint | `AgentStore.ReassignProjectBroker` | No-op (0, nil) when either the old or the new row is flat | **P1.1** |
| Mark old Runtime Broker offline | `MarkBrokerOffline` from the orphan path | Not reached for flat rows, because their agents are never in the orphan set | — |
| Reincarnate move | `handlers_agent_reincarnate.go`, `reincarnate_move.go` | Moving a pinned agent, or moving any agent onto a flat Runtime Broker, gets 409 `runtime_target_move_unsupported` in P1. A future explicit move re-pins in the same write as the `runtime_broker_id` change | P1.2 |
| Create-on-existing | `handleExistingAgent` (resumes/starts on `existingAgent.RuntimeBrokerID`) | A lifecycle operation: flat checks apply to the Runtime Broker actually dispatched to, compared with the agent's pin. Not refused when the experiment is off | P1.2 |

**Stale pin.** A pin is valid only while `pinned_runtime_broker_id == runtime_broker_id`. With the guards above, no current-binary automatic writer can create a stale pin. Only an older binary can (for example an older Hub's move or orphan reassignment).
- On a stale pin, the Hub **refuses** start/restart/create-on-existing dispatch with 409 `runtime_target_pin_stale`, details `{agentId, pinnedRuntimeBrokerId, runtimeBrokerId}`.
- Stop and delete still go to the current `runtime_broker_id`, so the agent can be cleaned up.
- There is no automatic re-pin. Repair is an explicit operator action (P5 tooling, or delete and recreate).
- `SetAgentPinnedRuntimeTarget` exists for that explicit move. No start path calls it.

## 8. Persistence: columns, writers and old-writer safety

New nullable columns. No existing column changes meaning.

**`runtime_brokers`** (`pkg/ent/schema/runtimebroker.go`):
- `runtime_target_id`: string, Optional, Nillable, **unique index**. Multiple NULLs are allowed on SQLite and Postgres. NULL means legacy.
- `runtime_target_type`: string, Optional, Default "".
- `runtime_target_display_name`: string, Optional, Default "".

Store model: `RuntimeBroker.RuntimeTarget *api.RuntimeTargetDescriptor`, JSON `runtimeTarget,omitempty`.

**`agents`** (`pkg/ent/schema/agent.go`):
- `pinned_runtime_broker_id`: string, Optional, Nillable.
- `pinned_runtime_target_id`: string, Optional, Nillable.
- `pinned_runtime_target_type`: string, Optional, Default "".

Store model: `Agent.PinnedRuntimeBrokerID`, `PinnedRuntimeTargetID` and `PinnedRuntimeTargetType`, all `json:"-"` like the run/launch columns, so they can't be patched through the API. P1.3 exposes them read-only as `"pinnedRuntimeTarget": {"id","type","runtimeBrokerId"}`. That name avoids `appliedConfig.runtimeTarget`, the observed heartbeat key.

**Writers (P1.1 store surface):**
- `CreateAgent` sets the pinned columns when the model carries them.
- `SetAgentPinnedRuntimeTarget(ctx, agentID, expected, next PinnedPlacement)` compare-and-sets all three columns plus `runtime_broker_id` together. It is used only by a future explicit move and does not bump `state_version`.
- `CreateRuntimeBroker` writes the target columns from `RuntimeTarget`.
- `SetRuntimeBrokerTarget(ctx, brokerID, desc)`:
  - stored NULL: `store.ErrRuntimeBrokerNotFlat`;
  - same ID and type: updates the display name only;
  - otherwise: `store.ErrRuntimeTargetChanged`.
- Guard in `UpdateRuntimeBroker`: inside its existing CAS loop, a row with a stored target rejects non-empty `Profiles`/`DefaultProfile` with `store.ErrFlatRuntimeBrokerProfiles`.
- Orphan guards as in section 7.
- `buildAgentUpdate` (`UpdateAgent`) and `UpdateRuntimeBroker` **never write** the new columns, and nothing clears them. Only row deletion removes them.

**Old and legacy writers:**
- An older binary doesn't know the new columns, so its full-row updates leave them intact.
- Current-binary full-row updates also omit them, so a stale in-memory model can't erase a pin or a target.
- Tests cover `UpdateAgent` and `UpdateRuntimeBroker`, including rewrites of `applied_config` and `runtimes`.
- Remaining risk, accepted and documented: the store guards and orphan exclusions don't exist in **older** binaries. An older Hub's orphan reassignment can move pinned agents, which makes their pin stale and refused (section 7) rather than retargeted. It can also rewrite profiles onto a flat row; the current Hub drops them on the next flat re-registration via R4 and the heartbeat rule. This matters only during a mixed-version window. P5 owns that window.
- Reincarnation snapshots and `applied_config.profile`/`createInputs.profile` stay readable and unchanged. A flat create leaves both empty.

## 9. Expected-target request field and the error codes

### `expectedRuntimeTargetId` (string, optional on the wire)

- **Hub public create API** (`POST /api/v1/projects/{id}/agents` and the hubclient create request): optional. Clients normally send only `runtimeBrokerId`. A client that showed a specific target sends it as a staleness guard.
  - Field present and resolved Runtime Broker legacy: 409 `runtime_target_mismatch` with an empty `actualRuntimeTargetId`.
  - Field present and experiment off: 412 `experiment_disabled`.
- **Hub → Runtime Broker create** (`RemoteCreateAgentRequest` and Runtime Broker `CreateAgentRequest`): a top-level key, not inside `config`. The Hub always sets it to the pinned target ID when dispatching to a flat Runtime Broker, and never sets it toward a legacy one.
  - A current-binary **legacy** Runtime Broker that receives a non-empty value rejects it with 409 `runtime_target_mismatch` (empty actual); it never ignores it. Older binaries ignore unknown keys, but a current Hub never sends the key to a legacy row.
- **Hub → Runtime Broker start/restart:** `StartExtras.ExpectedRuntimeTargetID`, written by `applyStartExtras` as the top-level key `expectedRuntimeTargetId`, so both transports stay in parity. It is set from the agent's valid pin (P1.3).

### Pure check helper (P1.1)

`api.CheckExpectedRuntimeTarget(runtimeBrokerID, actual, expected string) *api.RuntimeTargetMismatch` lives in `pkg/api/runtime_target.go`.
- It returns nil when `expected == ""` or `expected == actual`.
- Otherwise it returns the details for the envelope.
- `api.RuntimeTargetMismatch.Message()` produces the frozen message: "Runtime Broker <runtimeBrokerId> serves runtime target <actual>, but the request expected <expected>". When `actual` is empty, the target text reads "no runtime target".
- The Hub and the Runtime Broker both build their envelopes from it.

### Error codes (all new; constants added by P1.2 in the packages listed)

| Code | HTTP | Constant | Package(s) | Details keys | When |
|---|---|---|---|---|---|
| `runtime_target_mismatch` | 409 | `ErrCodeRuntimeTargetMismatch` | hub, runtimebroker | `runtimeBrokerId`, `expectedRuntimeTargetId`, `actualRuntimeTargetId` | Expected target differs (sections 9, 10) |
| `runtime_profile_unsupported` | 422 | `ErrCodeRuntimeProfileUnsupported` | hub, runtimebroker | `runtimeBrokerId`, `profile` | Explicit profile toward a flat target |
| `runtime_target_required` | 412 | `ErrCodeRuntimeTargetRequired` | runtimebroker | `runtimeBrokerId` | Flat instance create without the field |
| `runtime_target_changed` | 409 | `ErrCodeRuntimeTargetChanged` | hub | `runtimeBrokerId`, `storedRuntimeTargetId`, `reportedRuntimeTargetId` | Registration R3/R4 |
| `runtime_broker_not_flat` | 409 | `ErrCodeRuntimeBrokerNotFlat` | hub | `runtimeBrokerId` | Flat registration against a legacy row (R3) |
| `runtime_broker_name_conflict` | 409 | `ErrCodeRuntimeBrokerNameConflict` | hub | `name`, `slug`, `existingRuntimeBrokerId` | R2 |
| `runtime_target_move_unsupported` | 409 | `ErrCodeRuntimeTargetMoveUnsupported` | hub | `agentId`, `runtimeBrokerId` | Move of a pinned agent or onto a flat row |
| `runtime_target_pin_stale` | 409 | `ErrCodeRuntimeTargetPinStale` | hub | `agentId`, `pinnedRuntimeBrokerId`, `runtimeBrokerId` | Stale pin (section 7) |
| `experiment_disabled` | 412 | `ErrCodeExperimentDisabled` | hub | `experiment` | Flat registration/create with `hub.flat_runtime_brokers` off |

Notes on the codes:
- `experiment_disabled` is deliberately 412 with a code, not `requireExperiment`'s 404 `route`. These are existing endpoints, and the caller has to learn why a flat request was refused.
- **Start markers:** on the Runtime Broker → Hub hop, a rejection envelope *without* `startAttempted:true` already means "did not act" (`brokerStartAttempted` only treats `true` as meaningful). Flat rejections therefore never set the marker. The run ID is still echoed where today's start-failure responses echo it. The **Hub public** envelopes never contain start markers, following the existing practice of not exposing them to clients.
- **Relay:** today `dispatchCreateErrorResponse` maps an unclassified Runtime Broker 409 to 502 `runtime_error`. P1.2 adds an explicit relay case modelled on `relaySkillResolutionError`. It passes through status, code, message and the frozen details keys for `runtime_target_mismatch`, `runtime_profile_unsupported` and `runtime_target_required`, with start markers stripped.

### Before any side effects

- **Hub create:** the checks run after the Runtime Broker is resolved and before any write. Writes include:
  - the provider auto-link (`AddProjectProvider`) and the project default-broker `UpdateProject` inside `resolveRuntimeBroker`;
  - `handleExistingAgent`;
  - quota reservation;
  - `commitAgentCreate`;
  - run intent and start claim;
  - `beginRun`/`SetAgentRunID`;
  - agent token and dispatch.

  P1.2 splits resolution from linking. The order is: resolve (pure), then `checkBrokerDispatchAccess` (unchanged, still before any link), then the flat checks, then link.
- **Runtime Broker create:** after decode and validation, before `beginCreateAttempt` (no attempt record, launch registry entry, NFS mount, project markers/dirs or auxiliary-manager cache). The check is deterministic, so a `requestId` replay gets the same answer.
- **Runtime Broker start/restart:** before `beginSyncStart`, `startsInFlight` or any runtime call.

## 10. Legacy negotiation and profile handling

**Hub-side default-profile application points.** Each one is skipped for a flat target, and the agent keeps `AppliedConfig.Profile` and `CreateInputs.Profile` empty:

| Point | Code (28eb4f0) | Flat behaviour |
|---|---|---|
| Project defaults | `project_settings_handlers.go applyProjectDefaults` (`scion.io/active-profile`) | Not applied |
| Hub-default passthrough pin | `handlers_agents_core.go` (writes `AppliedConfig.Profile` and `CreateInputs.Profile`) | Not applied. The gate `hubDefaultPassthroughAllowed` evaluates the runtime type from `RuntimeTarget.Type` instead of broker profiles or `DefaultProfile` |
| Reincarnation re-derivation | `handlers_agent_reincarnate.go` and `default_gcp_identity.go` (re-derive the project active profile) | Not applied for a pinned agent. The GCP-identity gate uses the target type |
| Dispatcher write-back of a reported profile | `httpdispatcher.go` (writes a Runtime Broker-reported profile onto the agent) | Not written for a pinned agent. A flat instance reports none anyway |

When a project or Hub default profile is dropped because the target is flat, the Hub adds a dispatch warning through the existing `addDispatchWarnings` mechanism: "default Runtime Broker Profile \"<p>\" was not applied: Runtime Broker <id> serves a single runtime target". Placement can also be selected implicitly (project default Runtime Broker, single provider), so dropping the default silently would hide a change.

| Situation | Behaviour |
|---|---|
| Experiment off, legacy Runtime Brokers | Unchanged: current schema, profiles, fallbacks and APIs. New columns stay NULL |
| Experiment off, new create targeting a flat row | 412 `experiment_disabled` before side effects |
| Experiment off, lifecycle on agents already pinned to a flat row (start, stop, restart, delete, logs, attach, create-on-existing resume) | Works |
| Experiment on, create on a legacy Runtime Broker | Unchanged legacy path. No pin, and no `expectedRuntimeTargetId` sent |
| Experiment on, flat target, **explicit** non-empty `profile` in the request | Hub: 422 `runtime_profile_unsupported`, before side effects |
| Experiment on, flat target, profile only from defaults | Not applied. Dispatch warning (above) |
| Flat instance receives create with non-empty `config.profile` (e.g. an older Hub) | 422 `runtime_profile_unsupported`, before side effects |
| Flat instance receives create without `expectedRuntimeTargetId` | 412 `runtime_target_required`: "this Runtime Broker serves a single runtime target and requires expectedRuntimeTargetId; upgrade the Hub or enable hub.flat_runtime_brokers" |
| Flat instance create, start or restart with an **empty** profile | The flat instance **bypasses `resolveManagerForOpts`/`ResolveRuntime` entirely**. It never consults the saved agent profile or the settings `active_profile`, and always uses its single manager |
| Flat instance start/restart | The start wire has no profile field. The profile today comes from the saved agent profile or settings, and the flat instance ignores both (row above). A body that fails to decode is rejected with 400 `invalid_request`, not treated as "no `expectedRuntimeTargetId`". A start without the field is accepted, since it can only use the one target |
| Rolled-back legacy binary re-registers a flat identity without a descriptor | 409 `runtime_target_changed` (R4) |

Managed agents (`ManagedAgentsProfile`) don't use a Runtime Broker and are unaffected.

## 11. Interaction with run ID, start claims, run intent and recovery

| New element | Interaction |
|---|---|
| Pinned placement columns | Set in the same `CreateAgent` transaction as the row, before run intent, start claim, `beginRun` and launch. Never cleared by `forgetRuntimeTarget`, run-ID writes, launch end, start-claim settlement or reaper actions. Never used as a start-claim target |
| `runtime_brokers.runtime_target_*` | Written only by `CreateRuntimeBroker` and `SetRuntimeBrokerTarget`. Heartbeat write-backs can't roll it back. Independent of `BrokerTargetInventory` |
| `expectedRuntimeTargetId` and the flat refusals | Checked before claim/intent/run-ID writes on the Hub and before any launch bookkeeping on the Runtime Broker. No start marker is set, so a claimed start (once ptone/scion#3081 wires claims) settles as "did not act" through `isConfirmedStartNotActedOnError`. A create rolls back with nothing to compensate |
| Stale-pin refusal | Happens before claim, intent and run ID. Same settlement |
| Runtime target ID vs inventory target key | Distinct. `start_claim_target`, `InventoryTarget.id`, `BrokerTargetInventory` and `applied_config.runtimeTarget`/`runtimeTargetCandidate` keep the inventory key. No mapping in P1 |
| Run ID | Unchanged: minted per create/start/restart, sent as `runId`, labelled `scion.run_id` |
| Queued stops and recovery (ptone/scion#3091) | Unchanged. A flat Runtime Broker has one inventory target |
| Orphan guards (section 7) | Pinned agents are never reassigned, so claims, intents and recovery observations stay on their pinned Runtime Broker |
| Quota | `max_agents_per_broker` applies per flat Runtime Broker ID |

## 12. Additive migration plan (SQLite and Postgres)

- The ent changes are the six optional columns in section 8 plus the unique index on `runtime_brokers.runtime_target_id`.
  - They are applied by the existing `entc.AutoMigrate` on both dialects.
  - There is no backfill and no new step in `CompositeStore.Migrate`. NULL already means legacy, and the index is created empty.
- Generated code: `go generate ./pkg/ent`, committed. `make ent-check` must stay clean.
- **SQLite** runs in normal CI:
  - build a database at the pre-change shape (new columns and index dropped through raw SQL);
  - insert legacy agents (`applied_config.profile`, `runtimeTarget`/`runtimeTargetCandidate`, `createInputs.profile`) and legacy Runtime Brokers (`runtimes` profiles JSON, `default_profile`);
  - re-run `AutoMigrate`;
  - assert every legacy value round-trips unchanged and the new columns read NULL/empty;
  - assert two brokers with NULL `runtime_target_id` coexist under the unique index.
- **Postgres** runs the same test under `-tags integration`, active only with `SCION_TEST_POSTGRES_URL`. Without it, the test **skips with a message**.
  - The group C prefixes are **added to the `-run` regex of `make test-launch-store-postgres`** (`TestFlatPlacementColumns_`, `TestUpdateAgent_PreservesPinnedPlacement`, `TestUpdateAgent_LegacyColumnSetPreservesPin`, `TestUpdateRuntimeBroker_`, `TestSetRuntimeBrokerTarget_`, `TestSetAgentPinnedRuntimeTarget_`, `TestFindOrphanedAgents_`, `TestReassignAgentsToBroker_`, `TestReassignProjectBroker_`, `TestCreateAgent_PinnedPlacement`). That target fails if anything skips there.
  - Real Postgres evidence is gathered in P1.4.
- **Rollback:** older binaries ignore the extra columns and the index. Settings rollback is covered in section 2. Dropping the columns is a later, separately gated release.
- `RuntimeTargetCandidate` semantics are unchanged (tested).

## 13. Conduit run-ID fencing preserved by flat dispatch

Flat dispatch does not change the Conduit fencing contract:
- The Hub admits an agent Conduit session only if the Hello launch ID equals the agent's current `run_id`. An empty `run_id` matches nothing.
- Every dispatch path (create, start and restart, on flat and legacy Runtime Brokers) sets `SCION_LAUNCH_ID` and the `scion.run_id` label from the same value.
- Reusing a running container returns that container's run ID.
- `run_id` is never cleared, and pin writes don't touch it.
- `startAttempted` and the run ID in start-failure responses survive unchanged. The flat refusals only add new codes; they never remove those keys from responses that carry them today.
- Registration and the control channel are not Conduit-fenced, so there is no constraint on the registration descriptor.

GoogleCloudPlatform/scion#2479 and GoogleCloudPlatform/scion#2483 are in flight in `cmd/server_foreground.go`, `pkg/hub/httpdispatcher.go` and `pkg/runtimebroker/handlers.go`. P1.1 stays out of those files. P1.2 rebases onto them before its review.

## 14. Experiment registration

- **Name: `hub.flat_runtime_brokers`**, exported as `experiments.FlatRuntimeBrokers`.
- **Fields:**
  - Title: "Flat Runtime Brokers"
  - Description: "Lets a Runtime Broker serve exactly one runtime target with a stable identity: the hub accepts single-target Runtime Broker registrations, pins new agents to that target and rejects mismatched dispatches. Existing profile-based Runtime Brokers are unchanged."
  - Layers `[LayerServer]`, Default `false`, Stage `alpha`, Issue `ptone/scion#3267`, Owner `runtime-broker`, ReviewBy `2027-03-31`.
- **Enforcement:** in Hub code with `s.experimentEnabled(experiments.FlatRuntimeBrokers)`, at registration (R1, including the embedded wrapper) and at agent create (section 10).
  - It is not a UI-only flag and is not exposed through `/api/v1/settings/public`.
  - It is not added to `dispatchExperimentNames`. A Runtime Broker's flat behaviour comes from its own configuration.
- With the experiment off, every existing schema, API and behaviour works as before (tested).

## 15. Frozen tests

Stage B commits these names. Groups A–E (including the new pure mismatch tests) run for real in P1.1.

Group F tests are the dispatch half:
- They compile against real types and helpers in P1.1, with no build tags.
- Each calls `t.Skip(pendingFlatDispatch)`, where `const pendingFlatDispatch = "pending ptone/scion#3268: flat dispatch not wired yet"` is defined once per package, so deleting the constant forces every skip to be removed.
- P1.2 removes the constant and must make the tests pass unchanged, or bring a reviewed amendment. The P1.2 review checks that the constant is gone.

**A. Settings (`pkg/config`)**
- `TestRuntimeBrokerInstances_OneDockerEntryRoundTrip`: settings save/load/save stays byte-stable.
- `TestRuntimeBrokerInstances_GlobalConfigRoundTrip`: settings.yaml → `LoadGlobalConfig` → `RuntimeBrokerConfig.Instances` → `ConvertGlobalToV1ServerConfig` gives the same entry.
- `TestRuntimeBrokerInstances_StrictLoaderRejectsTypeErrorAndUnknownKey`: covers the silent-fallback case.
- `TestRuntimeBrokerInstances_LegacyServerYAMLRejected`
- `TestRuntimeBrokerInstances_ProjectSettingsIgnoredByServer`
- `TestRuntimeBrokerInstances_EmptyListIsLegacy`
- `TestRuntimeBrokerInstances_DuplicateKeyRejected`
- `TestRuntimeBrokerInstances_MultipleEntriesRejected`
- `TestRuntimeBrokerInstances_InvalidKeyRejected`
- `TestRuntimeBrokerInstances_NameRequired`
- `TestRuntimeBrokerInstances_TargetTypeRequired`
- `TestRuntimeBrokerInstances_KubernetesNotImplemented`
- `TestRuntimeBrokerInstances_UnsupportedTypeRejected`
- `TestRuntimeBrokerInstances_DockerRejectsKubernetesFields`
- `TestRuntimeBrokerInstances_SchemaMatchesValidator`
- `TestRuntimeBrokerInstances_OverlayDoesNotTouchInstances`
- `TestRuntimeBrokerInstances_LegacyConfigUnchanged`

**B. Local identity (`pkg/brokeridentity`)**
- `TestIdentity_FirstBootMintsAndPersistsAcrossRestart`
- `TestIdentity_EmptyDirectoryIsFirstBoot`
- `TestIdentity_TempLeftoversAreFirstBoot`
- `TestIdentity_ConcurrentFirstBootYieldsOneIdentity`
- `TestIdentity_MissingWithLeftoverStateIsError`
- `TestIdentity_CorruptFileIsError`
- `TestIdentity_UnknownSchemaVersionIsError`
- `TestIdentity_KeyMismatchIsError`
- `TestIdentity_TargetTypeChangeIsError`
- `TestIdentity_CollidesWithAnyLegacyBrokerIDIsError`
- `TestIdentity_ReaddedEntryRestoresSameIDs`: the settings-rollback case.
- `TestVerifyScope_DockerDaemonChangeRefused`
- `TestVerifyScope_DockerEndpointChangeSameDaemonAccepted`
- `TestVerifyScope_DockerEmptyDaemonIDIsError`
- `TestVerifyScope_KubernetesIgnoresContextAliasAndCredentials`
- `TestVerifyScope_KubernetesClusterOrNamespaceChangeRefused`

**C. Store and migrations (`pkg/store/entadapter`, `pkg/store/enttest`)**
- `TestFlatPlacementColumns_AdditiveUpgrade_SQLite`
- `TestFlatPlacementColumns_AdditiveUpgrade_Postgres`
- `TestCreateAgent_PinnedPlacementRoundTrip`
- `TestUpdateAgent_PreservesPinnedPlacement`
- `TestUpdateAgent_LegacyColumnSetPreservesPin`
- `TestUpdateRuntimeBroker_PreservesRuntimeTarget`
- `TestUpdateRuntimeBroker_RejectsProfilesOnFlatRow`
- `TestCreateRuntimeBroker_RuntimeTargetRoundTrip`
- `TestRuntimeTargetID_UniqueIndex`
- `TestSetRuntimeBrokerTarget_LegacyRowNotFlat`
- `TestSetRuntimeBrokerTarget_SameTargetUpdatesDisplayName`
- `TestSetRuntimeBrokerTarget_DifferentTargetRejected`
- `TestSetAgentPinnedRuntimeTarget_CompareAndSet`
- `TestRuntimeTargetCandidate_DoesNotTouchPin`
- `TestPinnedPlacement_StaleWhenBrokerMoved`
- `TestFindOrphanedAgents_ExcludesPinnedAndFlat`
- `TestFindOrphanedAgents_FlatCurrentBrokerAdoptsNothing`
- `TestReassignAgentsToBroker_NeverTargetsFlatBroker`
- `TestReassignAgentsToBroker_SkipsPinnedAgents`
- `TestReassignProjectBroker_NeverRepointsToOrFromFlat`

**D. Experiment (`pkg/experiments`, `pkg/hub`)**
- `TestFlatRuntimeBrokersExperimentRegistered`
- `TestFlatRuntimeBrokersExperiment_DefaultOffInHub`

**E. Wire types and pure checks (`pkg/api`, `pkg/hubclient`)**
- `TestRuntimeTargetDescriptor_JSONShape`
- `TestExpectedRuntimeTargetID_JSONKey`
- `TestCheckExpectedRuntimeTarget_MatchEmptyAndMismatch`
- `TestRuntimeTargetMismatch_MessageAndDetails`: frozen message and details keys, including an empty actual.

**F. Dispatch half (skipped until P1.2)**

Hub (`pkg/hub/flat_runtime_broker_contract_test.go`):
- `TestFlatCreate_ExpectedTargetMismatchRejectedBeforeSideEffects`: no agent row, provider link, project default update, quota reservation, run intent, start claim, run ID or dispatch.
- `TestFlatCreate_ExpectedTargetTowardLegacyBrokerRejected`
- `TestFlatCreate_ExplicitProfileRejected`
- `TestFlatCreate_DefaultProfileNotAppliedWithWarning`
- `TestFlatCreate_PassthroughGateUsesTargetType`
- `TestFlatCreate_PinsPlacementAndSendsExpectedTarget`
- `TestFlatCreate_ExperimentOffRejected`
- `TestFlatCreate_ExperimentOffExistingPinnedAgentLifecycleWorks`: includes create-on-existing resume.
- `TestFlatCreate_RuntimeBrokerRejectionRelayed`: 409/422/412 relayed with code and details, start markers stripped, create rolled back.
- `TestFlatCreate_AccessCheckBeforeLink`
- `TestFlatStart_StalePinRefused`
- `TestFlatReincarnate_MovePinnedAgentRefused`
- `TestFlatReincarnate_ProfileNotRederived`
- `TestFlatRegistration_TargetChangeRejected`
- `TestFlatRegistration_LegacyRowNotConverted`
- `TestFlatRegistration_LegacyReRegistrationOfFlatIDRejected`
- `TestFlatRegistration_NameOrSlugCollisionNotAdopted`
- `TestFlatRegistration_ExperimentOffRejectsNewAllowsExisting`
- `TestFlatRegistration_EmbeddedPathUsesSharedRules`
- `TestLegacyEmbeddedRegistration_SkipsFlatRowByName`
- `TestDeprecatedRegisterProject_DoesNotAdoptFlatRow`
- `TestHeartbeat_DropsProfilesForFlatRow`
- `TestLegacyCreate_ExperimentOffUnchanged`

Runtime Broker (`pkg/runtimebroker/flat_runtime_broker_contract_test.go`):
- `TestFlatInstanceCreate_ExpectedTargetMismatchBeforeAttempt`: no attempt file, launch registry entry, project dirs/markers or runtime call.
- `TestFlatInstanceCreate_MissingExpectedTargetRejected`
- `TestFlatInstanceCreate_NonEmptyProfileRejected`
- `TestFlatInstanceCreate_EmptyProfileIgnoresSettingsActiveProfile`
- `TestFlatInstanceStart_ExpectedTargetMismatchRejected`
- `TestFlatInstanceStart_UndecodableBodyRejected`
- `TestFlatInstanceStart_WithoutExpectedTargetUsesOnlyTarget`
- `TestFlatInstanceStart_IgnoresSavedProfile`
- `TestFlatInstanceStart_MismatchKeepsRunIDFencing`
- `TestLegacyInstanceCreate_NonEmptyExpectedTargetRejected`

## 16. Scope

### Non-goals for P1.1

- No Runtime Broker startup, registration, dispatch, heartbeat or UI wiring (P1.2/P1.3).
- No Kubernetes implementation, other runtime types or target-health polling.
- No fleet conversion and no change to existing agents' data.

### P1.1 code surface

- `pkg/config`: `settings_v1.go`, `hub_config.go` (`RuntimeBrokerConfig`, conversion, legacy rejection, strict loader), schema, validator.
- `pkg/brokeridentity`: new.
- `pkg/api/runtime_target.go`: descriptor and pure check.
- `pkg/ent/schema` (runtimebroker, agent) plus generated code.
- `pkg/store`: models, interface, and entadapter setters and guards, including the orphan guards in `agent_store.go` and the `UpdateRuntimeBroker` guard in `project_store.go`.
- `pkg/experiments/registry.go`.
- Additive fields on the hubclient registration/create request types and on the Hub `CreateBrokerRegistrationRequest`/`BrokerJoinRequest` structs (the type definitions only, no handler logic).
- The Makefile regex for `test-launch-store-postgres`.
- New test files.

## Change log

- **r2 (review round 1):**
  - Added the central invariant.
  - B1: orphan guards in the store (P1.1), stale pins refused rather than re-pinned, moves refused in P1.
  - B2: `RuntimeBrokerConfig` shape and conversions, the `server.yaml` rule, the strict loader.
  - B3: inventory of every Runtime Broker row writer, a single shared registration implementation, a target set only on row creation, `runtime_broker_not_flat`, and the `UpdateRuntimeBroker` profile guard.
  - N1: explicit relay case. N2: start-marker semantics, with Hub public envelopes stripped. N3: full code table. N4: every default-profile point, the passthrough gate by target type, and the dispatch warning. N5: existing-agent and move paths. N6: the flat instance bypasses resolution for an empty profile, and an undecodable start body is rejected. N7: CLI-based Docker probe, with an empty daemon ID as an error. N8: lock plus `link` creation and a precise first-boot rule against every legacy ID source. N9: `name` required, slug included, race accepted with a reason. N10: expected target toward legacy. N11: unique index. N12: Makefile regex. N13: settings rollback. N14: tests added.
  - T1: Title/Description frozen. T2: hubclient types named. T3: `UpdateProject` and the access-check order. T4: an empty list means legacy. T5: pure mismatch helper and tests in P1.1.
- **r1:** initial version (ab334be).
