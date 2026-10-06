# Flat Runtime Brokers: P1 contract appendix

Status: Stage A of ptone/scion#3267 (P1.1). **Revision 7** answers review rounds 1–7 and the cross-lane guard-test conditions; the change log is at the end. Parent phase: ptone/scion#3266. Delivery tracker: ptone/scion#2926.

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
| Server config (`pkg/config/hub_config.go`) | `RuntimeBrokerConfig.Instances` | `[]RuntimeBrokerInstanceConfig` | `instances` (camelCase family; json, yaml **and** koanf tags, like the rest of `RuntimeBrokerConfig`) |
| | `RuntimeBrokerInstanceConfig` | `Key`, `Name`, `RuntimeTarget *RuntimeTargetConfig` | `key`, `name`, `runtimeTarget` (json/yaml/koanf) |
| | `RuntimeTargetConfig` | `Type`, `DisplayName`, `Context`, `Namespace` | `type`, `displayName`, `context`, `namespace` (json/yaml/koanf) |

- `ConvertV1ServerToGlobalConfig` copies `V1BrokerConfig.Instances` into `RuntimeBrokerConfig.Instances`, and `ConvertGlobalToV1ServerConfig` copies them back. Both are deep copies, field for field.
- `context` and `namespace` are Kubernetes-only. They are defined but not implemented in P1.

### Where it is read

- "Global only" means **read through `LoadGlobalConfig`**: the settings.yaml whose `server` key `loadGlobalConfigFromSettings` uses. That is the global directory, or the `--config` directory only when the global settings have no `server` key.
- Project-level settings are never consulted. A `server.broker.instances` in a project settings file has no effect on the server (tested).
- **Legacy `server.yaml`: not supported.** If `loadGlobalConfigLegacy` produces a non-empty `RuntimeBroker.Instances`, `LoadGlobalConfig` returns an error: "runtimeBroker.instances is only supported under server.broker.instances in settings.yaml".
- **Strict loader (P1.1):** `config.LoadRuntimeBrokerInstances(configPath string) ([]V1RuntimeBrokerInstanceConfig, error)`.
  - **File resolution:**
    - the global `settings.yaml` if it exists and has a raw `server` key, *whether or not that section unmarshals*;
    - otherwise the `--config` directory's `settings.yaml` under the same rule;
    - otherwise no instances.
  - A global `settings.yaml` that exists but cannot be parsed as YAML is an explicit error. It never falls through to the `--config` file.
  - It decodes the raw `server.broker.instances` node with unknown fields disallowed and runs `ValidateRuntimeBrokerInstances`.
  - A type error or unknown key is an explicit error. This closes the gap where `loadServerFromSettingsFile` silently drops a `server` section that fails to unmarshal.
  - P1.2 startup calls it and refuses to start on an error. It also maps the result field for field, exactly as `ConvertV1ServerToGlobalConfig` would, and refuses unless that is deep-equal to `GlobalConfig.RuntimeBroker.Instances`. A difference means the silent-fallback path was taken.
- **Admin server-config PUT.** `handleAdminServerConfig` takes one of three paths. The rule for each is frozen here:
  - **(a) File mode, P1.1** (no OperationalSettings: `handlePutServerConfig` → `applySettingsUpdates`, `pkg/hub/admin_settings.go`). `applySettingsUpdates` merges `server` only one level deep, and the web editor never sends `instances`.
    - Presence of `server.broker.instances` is read from the **raw request body** (as `rejectRemovedProfileTimezone` does), because the typed `V1ServerConfig` can't tell an absent key from `[]`.
    - Absent: the stored `server.broker.instances` is carried over unchanged.
    - Present (including `[]` or `null`): it is validated with `ValidateRuntimeBrokerInstances` before anything is written, then applied.
  - **(b) Workstation DB mode, P1.1** (`handlePutServerConfigDB`, workstation file leaves in `admin_settings_workstation.go`; an additive guard only). An omitted `instances` is already preserved. The frozen rule:
    - an explicit `instances` leaf is validated with `ValidateRuntimeBrokerInstances` in `validateServerConfigFileKeys` before anything is written;
    - `server.broker.instances` goes in a **separate keep list** (e.g. `nullPreservedBrokerPaths`), consulted only when building `Keep` for a `null` on `server`/`server.broker`. Only an explicit `instances` key removes it. It is **not** added to `hubOwnedBrokerPaths`: that list also drives `hubOwnedBrokerChanges` (which rejects non-echo changes) and is skipped by the write loop, so an explicit valid list would never be written.
    - Every other key's handling is unchanged.
  - **(c) Hosted DB mode, unchanged:** `server.broker.instances` is a bootstrap (file) key. Any change to it is rejected with today's 422 for unclassified/bootstrap changes, and an omitted key is not a change.
- **Startup safety net (P1.2):** if any `<global>/runtime-brokers/*/identity.json` exists but no instance is configured, startup logs a prominent warning naming the unhosted Runtime Broker IDs before hosting the legacy identity. It does not refuse, so a deliberate rollback to legacy hosting stays possible.

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
- When `instances` is non-empty, the P1 process must also run the Hub (co-located; otherwise startup fails closed with `flat_runtime_broker_remote_unsupported`, section 6 R10). It hosts only the flat instance, loads no legacy Runtime Broker credentials, and does not run the legacy embedded registration or the orphan reassignment (sections 6 and 7). It logs that the legacy identity is not hosted.
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
- Instance-scoped Hub credentials go in `<...>/runtime-brokers/<key>/hub-credentials/<hub>.json`, using the `brokercredentials` format. They are **written only by the P2.1 remote registration (R10), after both acknowledgements pass, and read only by P2.1 remote activation.** The P1 co-located instance uses the embedded registration's in-memory credentials. P1.1 reserves the path only.

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

**Frozen Go types** (`pkg/brokeridentity`):
- `type Identity struct { SchemaVersion int; InstanceKey string; RuntimeBrokerID string; RuntimeTarget api.RuntimeTargetDescriptor; ExecutionScope ExecutionScope; CreatedAt time.Time }`. Its JSON is the file shape shown above; the target's `displayName` is not stored.
- `type ExecutionScope struct { Type string; Docker *DockerScope; Kubernetes *KubernetesScope }`, `type DockerScope struct { DaemonID, Endpoint string }`, `type KubernetesScope struct { ClusterUID, Namespace, APIServer string }`, with JSON keys as in section 5.
- `func LoadOrCreate(dir, key, targetType string, observed ExecutionScope, legacyBrokerIDs []string) (*Identity, error)`
- `func VerifyScope(recorded, observed ExecutionScope) error`
- `type AckBinding struct { RuntimeBrokerID string; RuntimeTarget *api.RuntimeTargetDescriptor }`
- `type AckError struct { Code, Phase, RuntimeBrokerID string; Expected, Got AckBinding }`, with `Error()`. `RuntimeBrokerID` is the identity's ID. The §9 details keys for the two ack codes are the log and report fields of this struct (`runtimeBrokerId`, `phase`, `expected`, `got`).
- `func CheckActivationAck(id *Identity, phase, brokerID string, target *api.RuntimeTargetDescriptor) error`

`LoadOrCreate`:
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

A scope change always means a new target. That requires a new identity, either a new instance key or the directory removed (section 4), and therefore a new Runtime Broker ID.

## 6. Registration descriptor and every Runtime Broker row writer

### Wire shape

```json
"runtimeTarget": { "id": "0b9e…", "type": "docker", "displayName": "Local Docker" }
```

- Go type: **`api.RuntimeTargetDescriptor { ID string "id"; Type string "type"; DisplayName string "displayName,omitempty" }`** in `pkg/api/runtime_target.go`. It is shared by store, Hub, hubclient and Runtime Broker.
- It is carried on:
  - Hub `CreateBrokerRegistrationRequest` and `BrokerJoinRequest` (`pkg/hub/brokerauth.go`);
  - hubclient `CreateBrokerRequest` and `JoinBrokerRequest` (`pkg/hubclient/runtime_brokers.go`);
  - **the registration and join responses:** Hub `CreateBrokerRegistrationResponse` and `BrokerJoinResponse`, hubclient `CreateBrokerResponse` and `JoinBrokerResponse`, each gaining `RuntimeTarget *api.RuntimeTargetDescriptor` (`runtimeTarget,omitempty`). A Hub implementing this contract always echoes the **stored** descriptor for a flat row, and none for a legacy row;
  - the Runtime Broker API object as `runtimeTarget`: store `RuntimeBroker.RuntimeTarget` (P1.1) and the hubclient response type `hubclient.RuntimeBroker` (`pkg/hubclient/types.go`), which gains `RuntimeTarget *api.RuntimeTargetDescriptor` in P1.3.
- **Naming:** on Runtime Broker objects `runtimeTarget` is this opaque descriptor. Inside `appliedConfig` (`runtimeTarget`/`runtimeTargetCandidate`) and in the per-agent heartbeat entry (`hubclient.AgentHeartbeat.RuntimeTarget` and the Hub-side heartbeat agent type in `handlers_runtime_brokers.go`), `runtimeTarget` stays the inventory target key string. The glossary entry names all three. P1.1 adds `GLOSSARY.md` entries for **Runtime target ID** (opaque, stable, from the registration descriptor) and **Inventory target key** (the `auxiliaryRuntimeIdentity` string used by heartbeats, start claims and recovery), so the two aren't confused.
- The scope record, endpoint, kubeconfig path and credentials are never sent.
- A flat registration sends no `profiles` and no `defaultProfile`.

### Shared rules

Registration **authorization** is unchanged and is not part of these rules: the existing user-credential gate and `broker.create`, with the existing re-register gate (R6; section 17). The rules below are identity and placement correctness checks applied after that authorization. All flat registration goes through **one Hub implementation**, `(*hub.Server).registerFlatRuntimeBroker` (P1.2). The embedded path calls it through an exported wrapper, `(*hub.Server).RegisterEmbeddedFlatRuntimeBroker`, so the experiment snapshot is always the Hub's own `experimentEnabled`.

**Name comparison** in all rules below: `name` is compared case-insensitively (as `GetRuntimeBrokerByName` / `NameEqualFold` do); `slug` is compared exactly.

- **R1 Experiment.**
  - With `hub.flat_runtime_brokers` off, a new flat registration (no row with this ID) gets 412 `experiment_disabled`.
  - Re-registration of an **existing flat** row with the same target is accepted, so its agents are not stranded.
  - A malformed experiment snapshot fails closed, which means off.
- **R2 Identity match and names.**
  - A flat registration is matched **by Runtime Broker ID only**. A flat row is never matched by name.
  - The name/slug collision check applies **only when a flat registration creates a row**: if `name` or `slug` equals that of any other row, the creation gets 409 `runtime_broker_name_conflict`.
  - **Re-registration of an existing flat row matched by ID never fails on a name collision.** A collision that arose later (for example a row created by an older binary) is logged as a warning.
  - **The name and slug are set only at creation.** A later change of `name` in config is logged as a warning and not applied. A rename goes through the admin PATCH, which applies the collision rule.
  - The creation-time check is check-then-act with no unique constraint. Existing rows may already share names or slugs, so a unique index is out of scope. The race is accepted explicitly: two concurrent first registrations with the same name are an operator error that only the first-boot window can hit, and the identity file lock (section 4) serializes a single host.
- **R3 Target set only on creation.** The target columns are written only when this flat registration **creates** the row (`CreateRuntimeBroker` with `RuntimeTarget`).
  - Existing row with the same target ID and type: accepted; the display name may update.
  - Existing row with a different target: 409 `runtime_target_changed`.
  - Existing **legacy** row (no stored target) with this ID: 409 `runtime_broker_not_flat`. Converting a legacy row is P5 work and is never done implicitly.
- **R4 Legacy writes to flat rows.**
  - A legacy registration (no `runtimeTarget`) matched by ID to a flat row gets 409 `runtime_target_changed`.
  - **A legacy writer that would create a row refuses when its name or slug collides with a flat row**: 409 `runtime_broker_name_conflict` over HTTP, or a startup error on the embedded path. It never creates a duplicate next to a flat row.
  - `profiles`/`defaultProfile` never persist on a flat row. The store strips them (section 8), and registration writes them empty.
- **R5 No orphan adoption.** A flat registration never runs orphan reassignment and is never its target (section 7).
- **R6 Authorization match.** `createBrokerRegistration` authorizes a re-registration against the row returned by `FindExistingBroker`, then pins the mutation to that row. Both steps must use the same rule.
  - P1.2 gives `FindExistingBroker` the descriptor. With a descriptor, it matches by ID only and never matches a flat row by name. The handler uses that one result for both the authorization decision and the pinned mutation.
  - Flat re-registration keeps the existing re-register permission gate.
- **R8 Auto-provide and links (see section 17).** `autoProvide` on a flat registration is stored as sent. The flat contract does not extend auto-provide or automatic linking: **a flat row receives no automatic cross-project link.** When a new project is created, the auto-provide link (`handlers_env_secrets.go`, provider plus default) **skips flat rows, whatever the experiment state**. `autoProvide` never creates a link to a flat row. Whether a linked flat row may be dispatched to is decided by `canDispatchToBroker` (section 17). A link to a flat row is created **only by an explicit link action**: the existing project providers endpoint `POST /api/v1/projects/{id}/providers` (`handleProjectProviders`, which needs project-update permission), reached by `scion runtime-broker provide` or hubclient `Projects().AddProvider`. No automatic path links a flat row. That covers project-creation auto-provide, the create-time link in `resolveRuntimeBroker`, the co-located registration (R7) and the deprecated `RegisterProject` (which refuses flat rows). Whatever ptone/scion#3340 decides for explicit links applies to flat rows unchanged.
- **R9 Activation acknowledgement (reverse negotiation; architecture decision).** A Hub that predates this contract (today's Hub, a P1.1-only Hub, or an older replica during a rolling Hub upgrade) decodes registration leniently. It drops `runtimeTarget`, matches by name first, and either re-registers an existing legacy row (rewriting its auto-provide, GCP host fields and labels) or creates a legacy row with the flat ID. The architecture decision for this workstream is that **a flat instance refuses to activate against such a Hub; there is no automatic downgrade to legacy.** R9 governs the HTTP registration and join of a remote flat instance (built in P2.1). R10 applies the same acknowledgement semantics to the P1 embedded path and to every remote activation.
  - **Acknowledgement.** Registration must acknowledge both the requested Runtime Broker ID and the agreed `runtimeTarget` binding: the response `brokerId` equals the identity's `runtimeBrokerId`, **and** the response `runtimeTarget` equals the identity's target (ID and type). The join response is checked the same way. The pure check is `brokeridentity.CheckActivationAck(id, phase, brokerID, target)` (P1.1; types in section 4). It returns a typed `*brokeridentity.AckError` (`Phase` is `"register"`, `"join"`, `"embedded"` or `"activate"`; section 6 R10) with one of two **distinct** codes. **Evaluation order (frozen):** (1) a response `brokerId` different from the identity gives `runtime_target_binding_conflict`; (2) otherwise a nil `runtimeTarget` gives `runtime_target_ack_missing`; (3) otherwise a different target ID or type gives `runtime_target_binding_conflict`. An unaware Hub that adopts by name returns another row's ID **and** no `runtimeTarget`, so it hits (1). The name-adoption test fixtures carry no `runtimeTarget`.
    - **`runtime_target_ack_missing`** (unsupported protocol/feature): the response carries no `runtimeTarget`. Message: "the Hub did not acknowledge the runtime target binding for Runtime Broker <id>; it does not support flat Runtime Brokers. Finish the Hub rollout (every replica, with hub.flat_runtime_brokers on) before enabling this instance".
    - **`runtime_target_binding_conflict`** (binding conflict): the response `brokerId` or `runtimeTarget` differs from the identity. Message: "the Hub acknowledged a different binding for Runtime Broker <id> (expected <id>/<target>, got <id>/<target>); refusing to activate". An identity returned only through a name match is never adopted.
  - **Refusal.** If the registration check fails, the instance sends no join and saves no credentials. If the join check fails, it discards the returned secret. In both cases the instance is **not activated**: no control channel, no heartbeat, no HTTP routing to it, and no dispatch accepted. The error is reported as a startup error, or through `EmbeddedBrokerRegistrationFailed` on the embedded path. The configured and persisted identity is preserved: no new identity is minted and there is no fallback to a profile or the legacy identity.
  - **Not a zero-side-effect guarantee.** Refusing activation does not undo what an unaware Hub may already have done. It may have created a legacy row with the flat ID, or re-registered (and changed the metadata of) an existing legacy row matched by name. The refusal message says this may have happened and names the Runtime Broker ID and name for reconciliation. **Nothing deletes or rewrites such a row automatically**, because it may belong to an existing Runtime Broker. Recovery is an explicit administrator action:
    - (i) a legacy row with the flat ID (no secret, because join never ran): after the Hub rollout, re-registration gets 409 `runtime_broker_not_flat` (R3). An administrator deletes that row, then the instance restarts and registers with the same identity. There is no automatic R3 exception.
    - (ii) an existing legacy row adopted by name: an administrator restores its metadata, and the flat instance's `name` is changed if it collides (R2 at creation).
    - (iii) **join-phase leftovers** (mixed replicas: registration acknowledged by a capable replica, join served by an unaware one). The unaware join has already deleted the row's previous secret and stored a new one that the instance discards. It has also marked the row online/connected, with a fresh heartbeat time, version and capabilities. The row therefore shows online although the instance never activated, until the stale-Runtime Broker sweep marks it offline. The join refusal message says the secret was rotated and the row may show online. Recovery: rerun the instance's registration once every replica is capable, which rotates the secret again. Nothing is cleaned up automatically.
  - **All serving replicas.** Flat mode requires the complete Hub path implementing this contract, with `hub.flat_runtime_brokers` on, on **every** Hub replica that can serve the instance. One successful registration, join or preflight does not prove that the next request reaches a capable replica. Operators finish the Hub rollout before enabling flat instances. Dispatch keeps its own guard. A create from an **unaware** replica arrives without `expectedRuntimeTargetId` and the Runtime Broker refuses it (412 `runtime_target_required`); a mismatched expected target is refused (409). A **capable replica with the experiment off** refuses new creates itself (412 `experiment_disabled`, before dispatch). It still sends `expectedRuntimeTargetId` on lifecycle and create-on-existing dispatches to existing flat rows (section 9); the experiment never gates sending the field. An explicitly configured legacy Runtime Broker (no `instances`) stays supported during the compatibility window. It is never an automatic downgrade of flat config.
  - **Forward rule (P2):** where one process hosts several instances, an acknowledgement failure rejects only the affected instance. Other instances in the process keep running.
  - This is protocol compatibility at activation. It is unrelated to proactive Kubernetes credential monitoring, which remains out of scope.
- **R10 Where activation is validated (P1 co-located; P2.1 remote).** Placement decision for this workstream: P1 delivers the **co-located** flat instance only (the Hub runs in the same process and performs the embedded registration; `startRuntimeBroker` in `cmd/server_foreground.go` and `cmd/server_broker.go`). Remote flat hosting (a separate runtime host, or a CLI-registered Runtime Broker joining a Hub) is built in P2.1 to the contract frozen below, and is refused in P1. Earlier wording about a Hub "implementing this contract" means Hub support. It does not imply remote activation support in the P1 Runtime Broker.
  - **P1 embedded path (bound result required).**
    - `RegisterEmbeddedFlatRuntimeBroker(ctx, id *brokeridentity.Identity, inst config.V1RuntimeBrokerInstanceConfig) (*store.RuntimeBroker, error)` applies R1–R8 in-process and returns the **stored row**.
    - Activation proceeds only on a **bound** result: `brokeridentity.CheckActivationAck(id, "embedded", row.ID, row.RuntimeTarget)` returns nil (same codes and evaluation order as R9). Being the same binary does not excuse a conflicting or legacy row.
    - A conflicting stored target, a legacy row with the flat ID (R3 `runtime_broker_not_flat`) or a name collision (R2, no adoption) means the instance is not activated.
    - Every refusal (R1, R2, R3, R7, identity errors, ack codes) is reported through `EmbeddedBrokerRegistrationFailed`.
    - Credentials are the in-memory credentials of the embedded registration. A flat process **never loads the legacy multi-store (`hub-credentials/*.json`) or `broker-credentials.json`**, and has no legacy or profile fallback.
  - **P1 remote flat fails closed.**
    - `config.CheckRuntimeBrokerInstanceHosting(instances []V1RuntimeBrokerInstanceConfig, hubInProcess bool) error` (P1.1). It refuses a non-empty `instances` when the Hub is not in the same process. **`hubInProcess` is exactly the predicate that admits the embedded registration today, `colocatedBrokerRegisters(cfg, s)`** (`enableHub && cfg.RuntimeBroker.Enabled && !simulateRemoteBroker && s != nil`). So `--simulate-remote-broker` with `instances` is also refused. The refusal holds **whether or not any credentials (instance-scoped or legacy) are saved**. Error code `flat_runtime_broker_remote_unsupported`, message: "server.broker.instances requires the Hub in the same process in this release; remote flat Runtime Broker hosting arrives in P2 (ptone/scion#3271). Remove server.broker.instances to run this host as a legacy Runtime Broker".
    - P1.2 startup calls it before any registration, credential load or connection, and refuses to start the instance. There is no remote flat activation path in P1.
    - Legacy non-flat remote hosting is unchanged.
  - **P2.1 remote registration (frozen now).**
    - Entry point: `scion broker register --instance <key>`. It is user-authorized through the existing user-credential registration endpoint and `broker.create` (Runtime Broker HMAC self-registration stays **not admitted**). Its act function is `brokerregistration.RegisterInstance(ctx, client hubclient.Client, inst config.V1RuntimeBrokerInstanceConfig, id *brokeridentity.Identity, hubName, credDir string) (*brokercredentials.BrokerCredentials, error)` in a new P2.1 package `pkg/brokerregistration`.
    - Order:
      1. strict-load the instance (`LoadRuntimeBrokerInstances`);
      2. `LoadOrCreate` the identity and `VerifyScope`;
      3. `POST /api/v1/brokers` with `brokerId`, `name` and `runtimeTarget`, then `CheckActivationAck(id, "register", …)`;
      4. `POST /api/v1/brokers/join` with `runtimeTarget`, then `CheckActivationAck(id, "join", …)`;
      5. only then write `runtime-brokers/<key>/hub-credentials/<hub>.json` (0600, atomic).
    - Nothing is saved or used before both acknowledgements pass. Existing instance credentials don't short-circuit the command. It re-registers and re-joins as the owner, rotating the secret, and saves the result only after both checks pass. Refusals, leftovers and recovery are as in R9.
  - **P2.1 remote activation validation (REQUIRED, frozen now).**
    - On **every** remote activation (each instance start, even with saved credentials), before the control channel, heartbeat or any dispatch is accepted, the instance:
      1. loads only `runtime-brokers/<key>/hub-credentials/*.json`. None present: `flat_runtime_broker_not_registered`, not activated, no fallback;
      2. authenticates with them. An authentication failure means not activated, with today's auth error;
      3. obtains the Hub's **current** binding for its Runtime Broker ID;
      4. runs `CheckActivationAck(id, "activate", got.ID, got.RuntimeTarget)`. A missing `runtimeTarget` (an unaware Hub, a legacy row, or a capable Hub with the row unbound) gives `runtime_target_ack_missing`; a different ID or target gives `runtime_target_binding_conflict`.
    - Only a nil result activates the instance. The act function is `brokerregistration.ValidateActivation(ctx, client, id *brokeridentity.Identity, creds *brokercredentials.BrokerCredentials) error`.
    - **Allowed carriers** (P2.1 chooses one; it must implement these semantics and codes unchanged):
      - (a) an HMAC-authenticated self-read of the instance's own Runtime Broker row (`GET /api/v1/runtime-brokers/{id}`, returning `runtimeTarget`);
      - (b) the control-channel connection handshake, carrying the stored binding in its acknowledgement before the channel is used;
      - (c) the Conduit session handshake, if Runtime Brokers have moved onto Conduit by then.
    - This is startup compatibility validation, repeated at each activation. It is **not** periodic credential monitoring or target-health polling.
  - **Both phases:** every serving Hub replica must be upgraded. One successful activation does not prove the next replica is compatible, so the per-dispatch expected-target guards (412/409) stay in force.
- **R7 Embedded side duties.** The embedded flat path (`RegisterEmbeddedFlatRuntimeBroker`, called from startup in place of `registerGlobalProjectAndBroker`):
  - creates the global project if it is missing (as today), but **creates no provider link and sets no default Runtime Broker for the flat row** (section 17). A project reaches the flat instance only through an explicit link;
  - records `SetEmbeddedBrokerID(flatID)`, so the Hub-default passthrough gate identifies the flat instance as the embedded Runtime Broker;
  - activates only on a bound result (R10), and reports any refusal (412 `experiment_disabled` at first boot, `ErrExecutionScopeChanged`, an identity error, an R3 or R2 refusal, an ack code) through `EmbeddedBrokerRegistrationFailed`. It never falls back to the legacy identity.

### Every writer

| Writer | Code (28eb4f0) | Flat rule | Phase |
|---|---|---|---|
| brokerauth create | `createBrokerRegistration` (`handlers_brokers.go`), `FindExistingBroker` (name first) | With `runtimeTarget`: R1–R3 and R6. Without it (legacy): `FindExistingBroker`'s name match uses `GetLegacyRuntimeBrokerByName` (flat rows are never candidates); an ID match on a flat row gets R4; **creating a row whose name/slug collides with a flat row gets 409 `runtime_broker_name_conflict`** | P1.2 |
| brokerauth join | `CompleteBrokerJoin` | The descriptor check (the descriptor must equal the stored target; a flat row joined without a descriptor, or a legacy row joined with one, gets 409 `runtime_target_changed`) reads the row and runs **right after the token, broker-ID and expiry checks, before `DeleteBrokerSecret`/`CreateBrokerSecret`**, so a refused join leaves the existing secret untouched. For a flat row, `profiles`/`defaultProfile` are written empty. The response echoes the stored descriptor (R9) | P1.2 |
| Embedded legacy registration | `cmd/server_broker.go registerGlobalProjectAndBroker` | Not run when `instances` is non-empty. When it runs (legacy process), its name lookup uses the filtered store query `GetLegacyRuntimeBrokerByName` (case-insensitive name **and** `runtime_target_id IS NULL`, P1.1). It never calls `GetRuntimeBrokerByName` and then skips, because that method's `First()` could return a flat row and hide a legacy row of the same name. an ID match on a flat row is a startup error naming R4, and **creating a row whose name/slug collides with a flat row is a startup error** | P1.2 |
| Embedded flat registration | new, `RegisterEmbeddedFlatRuntimeBroker` | R1–R7. No orphan reassignment, no profile write | P1.2 |
| `RegisterProject` with `brokerId` (link to an existing Runtime Broker) | `pkg/hub/handlers_projects_core.go handleProjectRegister`, `req.BrokerID` branch (`AddProjectProvider`, default if none) | **Refuses a flat row** before any project is created or changed. A read-only `GetRuntimeBroker(req.BrokerID)` is added to the pre-mutation block, after the `project.create` and project-update gates and before "Create new project if not found". It refuses only a flat row; the existing lookup, its not-found response and the link stay where they are for legacy rows. Refusal: 409 `runtime_broker_link_path_unsupported` ("flat Runtime Brokers are linked only through POST /api/v1/projects/{id}/providers"). Legacy rows are unchanged. This keeps the providers endpoint the **one** explicit link path for flat rows | P1.2 |
| Deprecated `RegisterProject` with `broker` | `pkg/hub/handlers_projects_core.go` (ID then name lookup, name/slug/profile overwrite) | Its name lookup uses `GetLegacyRuntimeBrokerByName`, not `GetRuntimeBrokerByName`, so the result doesn't depend on row order. Decided **in the pre-mutation lookup and authorization block**, before any project is created or changed: a flat row found by ID gets 409 `runtime_target_changed`; a name/slug collision with a flat row gets 409 `runtime_broker_name_conflict` (no adoption, no duplicate). It never writes profiles to a flat row | P1.2 |
| Status/connection-only writers | `UpdateRuntimeBrokerHeartbeat`, `ClaimRuntimeBrokerConnection`, `ReleaseRuntimeBrokerConnection`, `ReleaseAndMarkBrokerOffline`, `ReapStaleBrokerAffinity`, `MarkStaleBrokersOffline`, `MarkBrokerOffline`, `SetRuntimeBrokerCreatedByIfEmpty` | Unaffected. They write only status, heartbeat, connection-affinity or created-by columns, never the target, name or profiles | — |
| Messaging-plugin Runtime Broker rows | `cmd/server_foreground.go runServerStart` (`CreateRuntimeBroker` for `plugin-<type>` with a deterministic ID, only when that ID is missing) | Unchanged. These are legacy rows created by ID with a reserved name pattern; no name lookup and no profiles. A flat registration whose name or slug collides with one gets R2's 409 at its own creation | — |
| Admin PATCH `updateRuntimeBroker` | `pkg/hub/handlers_runtime_brokers.go` (rename, labels; read-modify-write) | A rename that would make a flat row's name/slug collide with another row, or another row's with a flat row, gets 409 `runtime_broker_name_conflict`. Profiles are handled by the store strip | P1.2 |
| Heartbeat refresh | `pkg/hub/handlers_runtime_brokers.go` (capabilities, workspace storage, default profile, profile attach) | For a flat row, `DefaultProfile`/`ProfileAttach` from a heartbeat are dropped with a warning log. Capabilities and workspace storage refresh as today | P1.2/P1.3 |
| Any `UpdateRuntimeBroker` caller | `pkg/store/entadapter/project_store.go` | **Store strip:** on a row with a stored target, `Profiles`/`DefaultProfile` are written empty, whatever the model holds, and a warning is logged when non-empty values were dropped. Non-flat rows are unaffected. It never writes target columns | **P1.1** |

The registration and control channel are not Conduit-fenced. Section 13 puts no constraint on this descriptor.

## 7. Agent movers and existing-agent paths

| Writer | Code (28eb4f0) | Flat rule | Phase |
|---|---|---|---|
| Orphan discovery | `AgentStore.FindOrphanedAgents` | Excludes agents with non-NULL `pinned_runtime_broker_id` and agents whose current Runtime Broker row has a stored target. Returns nothing when `currentBrokerID` is a flat row. **A missing broker row counts as legacy**, so agents of a deleted legacy Runtime Broker stay orphan-eligible unless pinned (today's behaviour). Implementation: query the IDs of flat rows in Go and add them to the existing `excludeIDs` list, as the online-broker exclusion already does. No join or subquery: `runtime_brokers.id` is a UUID column and `agents.runtime_broker_id` a string, and Postgres would need a cast | **P1.1** |
| Orphan reassignment | `AgentStore.ReassignAgentsToBroker` | Refuses a destination row with a stored target (`store.ErrFlatRuntimeBrokerReassign`). Its update also filters out pinned agents (`pinned_runtime_broker_id IS NULL`) | **P1.1** |
| Project default repoint | `AgentStore.ReassignProjectBroker` | No-op (0, nil) when either the old or the new row is flat. **A missing old row counts as legacy** (today's behaviour is unchanged) | **P1.1** |
| Mark old Runtime Broker offline | `MarkBrokerOffline` from the orphan path | Not reached for flat rows, because their agents are never in the orphan set | — |
| Reincarnate move | `handlers_agent_reincarnate.go`, `reincarnate_move.go` | Today a non-dry-run move already returns 501 `not_implemented`. **That stays unchanged in P1**: a flat or pinned move still gets 501. A `--dry-run` move of a pinned agent, or onto a flat Runtime Broker, follows the existing convention. `evaluateMoveEligibility` gains a **first** check named `runtime_target` (constant `moveCheckRuntimeTarget`, ahead of `workspace_mode`), and its failure returns `writeMoveRefusal` with **409 `runtime_target_move_unsupported`**, the verdict listing `runtime_target` as failed and the rest as not evaluated. P1.2 adds it at the front of `moveCheckOrder` and updates the ordered check list in the public API reference (`docs-site/src/content/docs/reference/api.md`). When moves are implemented, the flat refusal (409 `runtime_target_move_unsupported`) must come before any move work, and a permitted explicit move re-pins in the same write as the `runtime_broker_id` change (`SetAgentPinnedRuntimeTarget`) | P1.2 |
| **Scheduled create** | `server.go dispatchAgentEventHandler` (Runtime Broker = `providers[0].BrokerID`, builds `store.Agent`, applies the Hub-default passthrough pin, then `createAgentWithIdentityKey` → `recordRunIntent` → `DispatchAgentCreate`) | The **same new-create rules** as an interactive create, through the shared helper in section 9 (`flatCreatePlacement`). The scheduler loads the `providers[0]` row with `GetRuntimeBroker`; a missing row keeps today's behaviour (treated as legacy, no pin). `flatCreatePlacement` runs right after that and **before `applyScheduledProjectDefaultGCPIdentity` and `deriveAgentConfig`**, which consume its flat/legacy decision. Its check is the experiment. A scheduled request carries no explicit profile, so there is no profile refusal. Both of its profile sources are defaults and both are skipped for a flat target: the Hub-default passthrough pin (`applyScheduledProjectDefaultGCPIdentity`) and the project `scion.io/active-profile` (`applyProjectDefaults` via `deriveAgentConfig`). There is no HTTP response, so the dropped-default warning goes to a structured warning log line carrying the schedule and agent IDs. On success the pin goes on the model and is written in the `CreateAgent` transaction. On refusal the handler returns the `*hub.RuntimeTargetRefusal` as its error before any write, so the scheduler records a failed run (existing failure path). Nothing is created, and there is no retry beyond the schedule's own next occurrence | P1.2 |
| Reincarnate (in place) | `handleReincarnateAgent` → `runReincarnationWorker` (stop → applied-config write → `DispatchAgentReprovision` → `DispatchAgentStart`) | `checkPinnedPlacement` runs in `handleReincarnateAgent` at the **in-place** empty-`RuntimeBrokerID` check (after the move branch has returned, so a dry-run move gets the plan answer, not `runtime_target_pin_stale`), **before** the reincarnation record and the worker, and is relayed as 409. A refused reincarnation stops nothing, reprovisions nothing and mints no credential. Reprovision of a validly pinned agent sends `expectedRuntimeTargetId` through `buildCreateRequest` | P1.2 |
| Create-on-existing | `handleExistingAgent` (resumes/starts on `existingAgent.RuntimeBrokerID`; has delete-and-recreate branches) | Uses the check order in section 9. Resuming or starting in place is a lifecycle operation, checked against `existingAgent.RuntimeBrokerID` and its pin, and not refused when the experiment is off. A delete-and-recreate branch is a new create, and the new-create checks run before the delete | P1.2 |

**Stale pin.** A pin is valid only while `pinned_runtime_broker_id == runtime_broker_id`. With the guards above, no current-binary automatic writer can create a stale pin. Only an older binary can (for example an older Hub's move or orphan reassignment).
- On a stale pin, the Hub **refuses** start/restart/create-on-existing dispatch with 409 `runtime_target_pin_stale`, details `{agentId, pinnedRuntimeBrokerId, runtimeBrokerId}`.
- **An agent on a flat row with a NULL pin is also stale.** It is refused the same way, with `pinnedRuntimeBrokerId: ""`. Section 9's rule for agents with an empty Runtime Broker means no current writer produces this state.
- **Pure pre-check (P1.2):** `(*hub.Server).checkPinnedPlacement(agent *store.Agent) error`.
  - It reads only the agent and its Runtime Broker row.
  - If the agent's `runtime_broker_id` row no longer exists, it returns nil and today's missing-Runtime-Broker handling applies, unchanged (e.g. 503 or not-found). A deleted row is not a placement refusal.
  - It returns nil, or a typed **`*hub.RuntimeTargetRefusal{Code, Status, Message, Details}`** (code `runtime_target_pin_stale`, status 409).
  - The same type carries the Hub's other flat refusals (`runtime_target_mismatch` on the lifecycle branch, `runtime_profile_unsupported`, `experiment_disabled`).
- **Where `checkPinnedPlacement` is called:**
  - **User restart** (`handlers_agent_lifecycle.go`, stop + start): before `beginStartDispatchHTTP`, `recordRunIntent` and **the stop leg**, next to the existing `requireEmptyPerAgentBrokerCapabilityForAgent` pre-check. A refused restart never stops the agent and leaves the run intent and reservation unchanged.
  - **Lifecycle start, wake-on-DM and create-on-existing:** before `beginStartDispatch`/`beginStartDispatchHTTP` and `recordRunIntent`. After the ptone/scion#3081 rebase (which P1.2 rebases onto), this means inside `startAgentCore`, **before the start claim**, the lifecycle op and the capacity hold. The user restart takes its own claim and capacity hold before its stop leg (outside `startAgentCore`), so its pre-check goes **before the restart's own claim and hold**.
  - **Reincarnation:** in `handleReincarnateAgent`, before the worker (section 7 table).
  - **Backstop:** also at the very top of `HTTPAgentDispatcher.DispatchAgentStart` and `DispatchAgentRestart`, before `launchGuardError`, `buildStartEnv` (agent credential mint) and `beginRun`. It is defence in depth for every caller, including the cross-node executor and the start-claim runner once wired.
  - **Cross-node execution** (`reconcile.go execDispatch*`, which runs a dispatch that another node's handler queued): the requesting node's own top-of-`DispatchAgentStart` backstop runs before anything is queued, so a stale pin is refused on the requesting node. If the executing node's backstop refuses (state changed in between), P1.2 adds `*hub.RuntimeTargetRefusal` to the `dispatchFailureResult` envelope (code, status, details), so the requesting node rebuilds the typed error instead of a generic 502.
  - Setting `StartExtras.ExpectedRuntimeTargetID` from a valid pin stays in the dispatcher (P1.2/P1.3).
- **Classification:** `isConfirmedStartNotActedOnError` returns true for `*hub.RuntimeTargetRefusal` (via `errors.As`). No compensating stop runs, no credential is minted, and a held claim settles as "did not act".
- **Relay:** every handler error switch that can receive the refusal (lifecycle start and restart, create-on-existing via `writeExistingAgentGuardError` or a sibling, and wake) writes its own Status, Code and Details. It is never mapped to 502 `runtime_error`.
- **Retry behaviour:** no component retries a `*hub.RuntimeTargetRefusal`, or a Runtime Broker 409 `runtime_target_mismatch` on start/restart. Both are classified as confirmed not-acted-on. The caller that recorded the intent settles it as a definite start failure, setting the agent message to the refusal message:
  - at 28eb4f0, the handler's existing start-failure handling;
  - after the ptone/scion#3081/ptone/scion#3091 rebase, `startAgentCore`'s outcome settlement. A provisioned agent then returns to rest through #3091's provisioned-start settlement.
  - The cross-node executor (`reconcile.go`) is not a retry loop: it executes one queued dispatch and returns the result. No hot retry loop exists or is added.
- Stop, delete and the other per-agent operations that don't change placement (message delivery, exec, logs, keys, reset-auth, check-prompt, attach) still go to the current `runtime_broker_id` exactly as today, so the agent can be cleaned up. They are unaffected by the pin.
- There is no automatic re-pin. Repair is an explicit operator action (P5 tooling, or delete and recreate).
- `SetAgentPinnedRuntimeTarget` has exactly two callers: the first placement of an existing agent with an empty Runtime Broker (section 9 step 4) and a future explicit move. No other start path calls it.

## 8. Persistence: columns, writers and old-writer safety

New nullable columns. No existing column changes meaning.

**`runtime_brokers`** (`pkg/ent/schema/runtimebroker.go`):
- `runtime_target_id`: string, Optional, Nillable, NULL means legacy. It is made unique by a **named index in `Indexes()`**, `index.Fields("runtime_target_id").Unique()`, not an inline column `UNIQUE`. Multiple NULLs are allowed on SQLite and Postgres.
- `runtime_target_type`: string, Optional, Default "".
- `runtime_target_display_name`: string, Optional, Default "".

Store model: `RuntimeBroker.RuntimeTarget *api.RuntimeTargetDescriptor`, JSON `runtimeTarget,omitempty`.

**`agents`** (`pkg/ent/schema/agent.go`):
- `pinned_runtime_broker_id`: string, Optional, Nillable.
- `pinned_runtime_target_id`: string, Optional, Nillable.
- `pinned_runtime_target_type`: string, Optional, Default "".

Store model: `Agent.PinnedRuntimeBrokerID`, `PinnedRuntimeTargetID` and `PinnedRuntimeTargetType`, all `json:"-"` like the run/launch columns, so they can't be patched through the API. P1.3 exposes them read-only as `"pinnedRuntimeTarget": {"id","type","runtimeBrokerId"}`. That name avoids `appliedConfig.runtimeTarget`, the observed heartbeat key.

**Frozen P1.1 store signatures** (`pkg/store`):
- `type PinnedPlacement struct { RuntimeBrokerID, RuntimeTargetID, RuntimeTargetType string }`. An empty `RuntimeTargetID` means a NULL pin (and NULL `pinned_runtime_broker_id`).
- `SetAgentPinnedRuntimeTarget(ctx context.Context, agentID string, expected, next PinnedPlacement) (*Agent, error)`.
  - The conditional update matches `runtime_broker_id = expected.RuntimeBrokerID`. The column is nullable, so `expected.RuntimeBrokerID == ""` matches **both `''` and NULL**. It also matches the pin columns against the rest of `expected`: NULL when `expected.RuntimeTargetID == ""`, otherwise `pinned_runtime_broker_id = expected.RuntimeBrokerID` and an equal target ID and type.
  - It writes `runtime_broker_id` and `pinned_runtime_broker_id` from `next.RuntimeBrokerID`, the target columns from `next`, and `state_version + 1`.
  - `next.RuntimeTargetID` must be non-empty (it never unpins); otherwise `store.ErrInvalidPinnedPlacement`.
  - A compare miss returns **`store.ErrPinnedPlacementChanged`**; a missing agent returns `store.ErrNotFound`.
- `SetRuntimeBrokerTarget(ctx context.Context, brokerID string, desc api.RuntimeTargetDescriptor) (*RuntimeBroker, error)`. Errors: `store.ErrRuntimeBrokerNotFlat`, `store.ErrRuntimeTargetChanged`, `store.ErrNotFound`.
- `GetLegacyRuntimeBrokerByName(ctx context.Context, name string) (*RuntimeBroker, error)`. Case-insensitive name and `runtime_target_id IS NULL`. When several legacy rows match, it returns the oldest by `created` (deterministic). None: `store.ErrNotFound`.

**Writers (P1.1 store surface):**
- `CreateAgent` sets the pinned columns when the model carries them.
- `SetAgentPinnedRuntimeTarget(ctx, agentID, expected, next PinnedPlacement)` compare-and-sets all three columns plus `runtime_broker_id` together, and **bumps `state_version` in the same conditional update**. `buildAgentUpdate` writes `runtime_broker_id` from the in-memory model, so without the bump a stale `UpdateAgent` would pass its CAS and write the old Runtime Broker back, leaving a stale pin. With the bump it gets a version conflict. (`run_id` can skip the bump only because `buildAgentUpdate` never writes it.) It **returns the updated `*store.Agent`** (new `state_version`, `runtime_broker_id` and pin), and callers replace their in-memory model with it. Callers: section 9 step 4 (first placement) and a future explicit move.
- `CreateRuntimeBroker` writes the target columns from `RuntimeTarget`. When `RuntimeTarget` is set it applies the same strip (empty `runtimes`/`default_profile`), so "profiles never persist on a flat row" is a store invariant.
- `SetRuntimeBrokerTarget(ctx, brokerID, desc)`:
  - stored NULL: `store.ErrRuntimeBrokerNotFlat`;
  - same ID and type: updates the display name only;
  - otherwise: `store.ErrRuntimeTargetChanged`.
- **Strip in `UpdateRuntimeBroker`:** inside its existing CAS loop, on a row whose stored `runtime_target_id` is non-NULL, `runtimes` and `default_profile` are written empty whatever the model holds, and a warning is logged when non-empty values were dropped.
  - This strips rather than rejects, so a leftover from an older Hub cannot make every later read-modify-write (heartbeat capability refresh, admin PATCH, re-registration) fail. The next current-binary write heals the row.
  - Rows without a stored target are **unaffected**: a legacy row's `Profiles` can still be replaced (for example by a single kubernetes profile), and an Endpoint-only update succeeds.
- Orphan guards as in section 7.
- `buildAgentUpdate` (`UpdateAgent`) and `UpdateRuntimeBroker` **never write** the new columns, and nothing clears them. Only row deletion removes them.

**Old and legacy writers:**
- An older binary doesn't know the new columns, so its full-row updates leave them intact.
- Current-binary full-row updates also omit them, so a stale in-memory model can't erase a pin or a target.
- Tests cover `UpdateAgent` and `UpdateRuntimeBroker`, including rewrites of `applied_config` and `runtimes`.
- Remaining risk, accepted and documented: the store guards and orphan exclusions don't exist in **older** binaries. An older Hub's orphan reassignment can move pinned agents, which makes their pin stale and refused (section 7) rather than retargeted. It can also rewrite profiles onto a flat row. The current binary's `UpdateRuntimeBroker` strip clears them on the next write of that row (heartbeat refresh, re-registration, admin PATCH), and no current writer fails because of them. This matters only during a mixed-version window. P5 owns that window.
- Reincarnation snapshots and `applied_config.profile`/`createInputs.profile` stay readable and unchanged. A flat create leaves both empty.

## 9. Expected-target request field and the error codes

### `expectedRuntimeTargetId` (string, optional on the wire)

- **Hub public create API** (`POST /api/v1/projects/{id}/agents` and the hubclient create request): optional. Clients normally send only `runtimeBrokerId`. A client that showed a specific target sends it as a staleness guard.
  - **New-create branch only** (section 9 step 4): field present and resolved Runtime Broker legacy → 409 `runtime_target_mismatch` with an empty `actualRuntimeTargetId`; field present and experiment off → 412 `experiment_disabled`.
  - **Lifecycle branch** (existing agent kept): the field is compared with the agent's valid pin (step 4 rule), and the experiment state plays no part. With the experiment off, a matching value against a pinned agent is accepted.
- **Hub → Runtime Broker create** (`RemoteCreateAgentRequest` and Runtime Broker `CreateAgentRequest`): a top-level key, not inside `config`. The Hub always sets it to the pinned target ID when dispatching to a flat Runtime Broker, and never sets it toward a legacy one. **`buildCreateRequest` (`httpdispatcher.go`) is the single place it is set**, from **any non-NULL pin** (not only a valid one). A stale pin therefore sends the old pinned target, and the receiving Runtime Broker answers 409 `runtime_target_mismatch` (a flat row with a different target, or a current legacy binary rejecting a non-empty value). An agent on a flat row with a NULL pin sends no field and gets 412 `runtime_target_required`. Both are safe refusals before side effects on the Runtime Broker. It is set so every create-shaped dispatch carries it: `DispatchAgentCreate`, `DispatchAgentCreateWithGather`, `DispatchAgentProvision`, `DispatchAgentReprovision` (reincarnation) and `DispatchFinalizeEnv`.
  - A current-binary **legacy** Runtime Broker that receives a non-empty value rejects it with 409 `runtime_target_mismatch` (empty actual); it never ignores it. Older binaries ignore unknown keys, but a current Hub never sends the key to a legacy row.
- **Hub → Runtime Broker start/restart:** `StartExtras.ExpectedRuntimeTargetID`, written by `applyStartExtras` as the top-level key `expectedRuntimeTargetId`, so both transports stay in parity. It is set from the agent's valid pin in `DispatchAgentStart`/`DispatchAgentRestart` (section 7, P1.2/P1.3).
- **The Hub sends `expectedRuntimeTargetId` to a flat row whatever the experiment state.** The experiment gates new flat registrations and new creates. It never removes the field from dispatches to an existing flat row, so experiment-off lifecycle operations that reach a Runtime Broker create (the create-on-existing provisioning branch) don't hit 412 `runtime_target_required`.

### Pure check helper (P1.1)

`api.CheckExpectedRuntimeTarget(runtimeBrokerID, actual, expected string) *api.RuntimeTargetMismatch` lives in `pkg/api/runtime_target.go`.
- It returns nil when `expected == ""` or `expected == actual`.
- Otherwise it returns the details for the envelope.
- `api.RuntimeTargetMismatch.Message()` produces the frozen message: "Runtime Broker <runtimeBrokerId> serves runtime target <actual>, but the request expected <expected>". When `actual` is empty, the message reads "Runtime Broker <runtimeBrokerId> serves no runtime target, but the request expected <expected>".
- The Hub and the Runtime Broker both build their envelopes from it.

### Error codes (all new; constants added in **P1.1** in the packages listed, used by P1.2)

| Code | HTTP | Constant | Package(s) | Details keys | When |
|---|---|---|---|---|---|
| `runtime_target_mismatch` | 409 | `ErrCodeRuntimeTargetMismatch` | api (aliased in hub and runtimebroker) | `runtimeBrokerId`, `expectedRuntimeTargetId`, `actualRuntimeTargetId` | Expected target differs (sections 9, 10) |
| `runtime_profile_unsupported` | 422 | `ErrCodeRuntimeProfileUnsupported` | api (aliased in hub and runtimebroker) | `runtimeBrokerId`, `profile` | Explicit profile toward a flat target |
| `runtime_target_required` | 412 | `ErrCodeRuntimeTargetRequired` | api (aliased in hub and runtimebroker) | `runtimeBrokerId` | Flat instance create without the field |
| `runtime_target_changed` | 409 | `ErrCodeRuntimeTargetChanged` | hub | `runtimeBrokerId`, `storedRuntimeTargetId`, `reportedRuntimeTargetId` | Registration R3/R4 |
| `runtime_broker_not_flat` | 409 | `ErrCodeRuntimeBrokerNotFlat` | hub | `runtimeBrokerId` | Flat registration against a legacy row (R3) |
| `runtime_broker_name_conflict` | 409 | `ErrCodeRuntimeBrokerNameConflict` | hub | `name`, `slug`, `existingRuntimeBrokerId` | R2 (flat creation), R4 (legacy creation colliding with a flat row), admin PATCH rename |
| `runtime_target_move_unsupported` | 409 | `ErrCodeRuntimeTargetMoveUnsupported` | hub | none: written by `writeMoveRefusal` with only its existing `verdict` detail (no new wire details) | Move eligibility: a dry-run move refused through `writeMoveRefusal` (verdict check `runtime_target`) in P1 (non-dry-run moves stay 501), and the same refusal once moves exist (section 7) |
| `runtime_target_pin_stale` | 409 | `ErrCodeRuntimeTargetPinStale` | hub | `agentId`, `pinnedRuntimeBrokerId`, `runtimeBrokerId` | Stale pin (section 7) |
| `runtime_target_ack_missing` | — (instance-side, not HTTP) | `ErrCodeRuntimeTargetAckMissing` | api | `runtimeBrokerId`, `phase` | The registration or join response (R9), the embedded bound result, or the P2.1 activation binding (R10) lacks `runtimeTarget`. Startup error; instance not activated |
| `runtime_target_binding_conflict` | — (instance-side, not HTTP) | `ErrCodeRuntimeTargetBindingConflict` | api | `runtimeBrokerId`, `phase`, `expected`, `got` | The registration or join response (R9), the embedded bound result, or the P2.1 activation binding (R10) carries a different Runtime Broker ID or target. Startup error; instance not activated |
| `flat_runtime_broker_remote_unsupported` | — (instance-side startup error) | `ErrCodeFlatRuntimeBrokerRemoteUnsupported` | api | `instanceKey` | P1: `server.broker.instances` without a Hub in the same process, with or without saved credentials (R10) |
| `flat_runtime_broker_not_registered` | — (instance-side startup error) | `ErrCodeFlatRuntimeBrokerNotRegistered` | api | `instanceKey`, `runtimeBrokerId` | P2.1: remote activation with no instance-scoped credentials (R10) |
| `runtime_broker_link_path_unsupported` | 409 | `ErrCodeRuntimeBrokerLinkPathUnsupported` | hub | `runtimeBrokerId` | `RegisterProject` with `brokerId` names a flat row; flat rows are linked only through the providers endpoint (section 6, section 17) |
| `runtime_broker_not_linked` | 422 | `ErrCodeRuntimeBrokerNotLinked` | hub | `runtimeBrokerId`, `projectId` | Create resolves to a flat row with no explicit link to the project; flat rows are never linked automatically (section 17) |
| `experiment_disabled` | 412 | `ErrCodeExperimentDisabled` | hub | `experiment` | Flat registration/create with `hub.flat_runtime_brokers` off |

Notes on the codes:
- `experiment_disabled` is deliberately 412 with a code, not `requireExperiment`'s 404 `route`. These are existing endpoints, and the caller has to learn why a flat request was refused.
- **Start markers:** on the Runtime Broker → Hub hop, a rejection envelope *without* `startAttempted:true` already means "did not act" (`brokerStartAttempted` only treats `true` as meaningful). Flat rejections therefore never set the marker. The run ID is still echoed where today's start-failure responses echo it. The **Hub public** envelopes never contain start markers, following the existing practice of not exposing them to clients.
- **Shared wire codes:** the three codes that cross the Hub/Runtime Broker hop (`runtime_target_mismatch`, `runtime_profile_unsupported`, `runtime_target_required`) are defined once in `pkg/api/runtime_target.go` and aliased in `pkg/hub/errors.go` and `pkg/runtimebroker/errors.go`, following the `wsprotocol.ErrCodeRuntimeAttachUnsupported` precedent. The Hub-only codes are defined in `pkg/hub/errors.go`.
- **Relay:** today `dispatchCreateErrorResponse` maps an unclassified Runtime Broker 409 to 502 `runtime_error`. P1.2 adds an explicit relay case modelled on `relaySkillResolutionError`. It passes through status, code, message and the frozen details keys for the three shared codes, with start markers stripped. **Start and restart responses use the same relay** (the lifecycle, create-on-existing and wake error switches), so a Runtime Broker 409/412/422 on those paths is never mapped to 502.

### Before any side effects

- **Hub create** (P1.2). Writes that must not happen before the checks:
  - for a **flat** row, any link: no provider link or project default is ever written for a flat row (the resolver answers 422 `runtime_broker_not_linked` instead). A legacy row's in-resolver link (`AddProjectProvider`, and `UpdateProject` for a default) is kept unchanged in step 1, as today;
  - `handleExistingAgent`, including its delete in the delete-and-recreate branches;
  - quota reservation;
  - `commitAgentCreate`;
  - run intent and start claim;
  - `beginRun`/`SetAgentRunID`;
  - agent token and dispatch.

  **Frozen order:**
  1. **Resolve** the Runtime Broker. **A flat row is never linked here.** An unlinked flat row can be reached only through an **explicit** `runtimeBrokerId`: the project default and Hub-default cases select only from the project's linked, online providers, so they keep today's fall-through. For an explicit flat row that is not linked to the project, the resolver evaluates **`canDispatchToBroker` on that row first**. A denial returns today's authorization response (authorization wins). Otherwise it answers 422 `runtime_broker_not_linked` ("Runtime Broker <id> is not linked to project <p>; link it explicitly (scion runtime-broker provide, or POST /api/v1/projects/{id}/providers) before creating agents on it") and writes nothing.
     **Legacy rows keep today's order unchanged**: the in-resolver project-update `CheckAccess`, the offline 503, and then today's create-time link inside the resolver, before step 2. An agent caller creating on an explicit, unlinked legacy row is denied by that project-update `CheckAccess` today: it gets 403 and no link is written. That is unchanged. A client-supplied `expectedRuntimeTargetId` toward a legacy row is **not** checked here: it stays in step 4's new-create branch, in the step-5 precedence, after dispatch authorization. So for an explicit unlinked legacy row, today's create-time link (step 1) may persist when a later check refuses, exactly as it does today for a step-2 403.
  2. **Access (the single authorization rule, section 17):** `checkBrokerDispatchAccess`, unchanged. The existing pure checks that follow it today stay here, unchanged and in their current order: `requireEmptyPerAgentBrokerCapability` (412), GCP passthrough authorization, and service-account assignment validation.
  3. **Read the existing agent** (`GetAgentBySlug`, a pure read moved up from its current position) and decide which `handleExistingAgent` branch applies, without executing it.
  4. **Branch:**
     - **Resume or start in place** (an existing agent with a non-empty `RuntimeBrokerID` kept): apply the *lifecycle* rules against `existingAgent.RuntimeBrokerID` and its pin: `checkPinnedPlacement`, the expected target taken from the pin, an explicit profile against a flat row refused (422), and **no experiment refusal**.
       - A client-supplied `expectedRuntimeTargetId` here is compared with the agent's **valid pin**. A mismatch, including an unpinned agent or an agent on a legacy row, gets 409 `runtime_target_mismatch`.
     - **Existing agent with an empty `RuntimeBrokerID`** (which today adopts the resolved Runtime Broker, `handleExistingAgent`): when the resolved Runtime Broker is flat, it takes the *new-create* checks (experiment, expected target, profile). On success its pin and `runtime_broker_id` are written together by `SetAgentPinnedRuntimeTarget` (expected: empty placement), which bumps `state_version`. The write comes after the checks and **before** `beginStartDispatchHTTP`/`recordRunIntent`, and the handler replaces `existingAgent` with the returned row. So the dispatcher sees the pin and sends `expectedRuntimeTargetId`, and the handler's post-dispatch `UpdateAgent`/`updateAgentAfterDispatch` carries the current `state_version` (no conflict). It never sits on a flat row unpinned. On `store.ErrPinnedPlacementChanged` (a concurrent placement), the handler answers 409 `conflict` (existing code), "agent placement changed concurrently; retry", before any dispatch. **The first placement stays persisted if the subsequent start fails** (quota refusal or start error): the agent now belongs to that flat Runtime Broker, exactly as if it had been created there. This differs from today's in-memory adoption, deliberately. When the resolved Runtime Broker is legacy, today's behaviour is unchanged.
     - **New create** (no existing agent, or a delete-and-recreate branch): apply the *new-create* checks against the resolved Runtime Broker, before any write including that delete.
  - **Shared helper:** the new-create checks and the pin are computed by one function, `(*hub.Server).flatCreatePlacement(ctx, broker *store.RuntimeBroker, explicitProfile, expectedRuntimeTargetID string) (store.PinnedPlacement, error)`. It returns the placement to set on the model (zero for a legacy row) or a `*hub.RuntimeTargetRefusal`. The same component decides whether default profiles apply (`flatTarget := broker.RuntimeTarget != nil`). `createAgentInProject`, the scheduler's `dispatchAgentEventHandler` and the empty-Runtime-Broker existing-agent case all call it, so the create paths cannot drift.
  5. The existing flow continues unchanged. Flat rows are never linked by this path, and legacy rows were linked in step 1, as today.
- **Check precedence** (the first failing check wins):
  - **Hub new create:** the existing step-2 checks first (so 412 `empty_per_agent` capability comes before 412 `experiment_disabled`), then experiment (412 `experiment_disabled`), then expected target (409 `runtime_target_mismatch`, including toward a legacy row), then explicit profile (422 `runtime_profile_unsupported`).
  - **Hub lifecycle branch:** stale pin (409 `runtime_target_pin_stale`), then client expected target (409 `runtime_target_mismatch`), then explicit profile (422).
  - **Runtime Broker:** decode (400), then target required (412, create only), then mismatch (409), then profile (422).
- **Runtime Broker create:** after decode and validation, before `beginCreateAttempt` (no attempt record, launch registry entry, NFS mount, project markers/dirs or auxiliary-manager cache). The check is deterministic, so a `requestId` replay gets the same answer.
- **Runtime Broker start/restart:** before `beginSyncStart` and any runtime call. The handler opens its in-memory `startsInFlight` entry at entry, before the body is decoded, and that order is **not** changed (it avoids reordering a handler that in-flight work also touches). The entry closes on return, so a refused start is visible in at most one heartbeat's in-flight list, which recovery already tolerates.

## 10. Legacy negotiation and profile handling

None of the refusals in this section is an authorization decision. Authorization runs first, through the single rule in section 17; the codes below are correctness and compatibility refusals.

**Hub-side default-profile application points.** Each one is skipped for a flat target, and the agent keeps `AppliedConfig.Profile` and `CreateInputs.Profile` empty:

| Point | Code (28eb4f0) | Flat behaviour |
|---|---|---|
| Project defaults | `project_settings_handlers.go applyProjectDefaults` (`scion.io/active-profile`) | Not applied |
| Hub-default passthrough pin | `handlers_agents_core.go` and the scheduler copy in `server.go` (`dispatchAgentEventHandler`, via `applyScheduledProjectDefaultGCPIdentity`/`hubDefaultPassthroughAllowed`); both write `AppliedConfig.Profile` and `CreateInputs.Profile` | Not applied. The gate `hubDefaultPassthroughAllowed` evaluates the runtime type from `RuntimeTarget.Type` instead of broker profiles or `DefaultProfile` |
| Reincarnation re-derivation | `handlers_agent_reincarnate.go` and `default_gcp_identity.go` (re-derive the project active profile) | Not applied for a pinned agent. The GCP-identity gate uses the target type |
| Runtime Broker-reported profile write-backs | `httpdispatcher.go` (create/start response profile), heartbeat backfill `agent.AppliedConfig.Profile = agentHB.Profile` (`handlers_runtime_brokers.go`), reincarnation echo `copyBrokerEcho` (`reincarnate_worker.go`) | General rule: **no Runtime Broker-reported profile is ever written onto a pinned agent.** A flat instance reports none anyway |

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
| Flat instance receives create without `expectedRuntimeTargetId` | 412 `runtime_target_required`: "this Runtime Broker serves a single runtime target and requires expectedRuntimeTargetId; upgrade the Hub" |
| Flat instance create, start or restart with an **empty** profile | The flat instance **bypasses `resolveManagerForOpts`/`ResolveRuntime` entirely**. It never consults the saved agent profile or the settings `active_profile`, and always uses its single manager |
| Flat instance start/restart | The start wire has no profile field. The profile today comes from the saved agent profile or settings, and the flat instance ignores both (row above). A body that fails to decode (invalid JSON or a type error) is rejected with 400 `invalid_request`, not treated as "no `expectedRuntimeTargetId`". **Unknown keys stay ignored**: no `DisallowUnknownFields`, per the start/restart version-skew contract for GoogleCloudPlatform/scion#1931. A start without the field is accepted, since it can only use the one target |
| Rolled-back legacy binary re-registers a flat identity without a descriptor | 409 `runtime_target_changed` (R4) |

Managed agents (`ManagedAgentsProfile`) don't use a Runtime Broker and are unaffected.

### Version pairings

Terms used in the table:
- **New Hub:** a Hub implementing this contract (P1.2 and later).
- **Old Hub:** a Hub that predates this contract: today's Hub, a P1.1-only Hub, or an older replica during a rolling upgrade. It does not consult or enforce `hub.flat_runtime_brokers` (a P1.1-only Hub registers the experiment but nothing reads it), so its two columns are identical.
- **Old Runtime Broker:** a binary that predates this contract. A new binary with no `server.broker.instances` behaves the same, except that it rejects a non-empty `expectedRuntimeTargetId`, which a new Hub never sends to a legacy row.
- **New Runtime Broker:** a binary hosting a flat instance. In **P1** that means co-located only: its own Hub is the same binary and new, so "old Hub" can only mean another, older replica of the same deployment. A remote flat instance is refused in P1 (`flat_runtime_broker_remote_unsupported`). The **P2.1** rows below describe remote flat instances, which use the required activation validation (R10).

The new-Hub/new-Runtime Broker pairing is specified in sections 6–10.

For the old-Hub columns there are two cases:
- **fresh:** the flat instance meets the old Hub at first registration;
- **rollback:** the flat instance was registered under a new Hub, which was then downgraded.

| Exchange | New Hub + old Runtime Broker, experiment on | New Hub + old Runtime Broker, experiment off | Old Hub + new Runtime Broker (experiment on) | Old Hub + new Runtime Broker (experiment off) |
|---|---|---|---|---|
| Registration | Legacy path (no descriptor). The name match uses `GetLegacyRuntimeBrokerByName`, so a flat row is never adopted. An ID match on a flat row gets 409 `runtime_target_changed` (R4). Creating a row whose name/slug collides with a flat row gets 409 `runtime_broker_name_conflict`. No echo (legacy row) | Same as experiment on: legacy registration is not gated | P1 (co-located): registration is in-process in the instance's own (new) Hub and validated as a bound result (R10); older replicas take no part. P2.1 fresh: the Hub drops the descriptor, adopts by name or creates a legacy row, and returns no acknowledgement. The instance refuses with `runtime_target_binding_conflict` (another row's ID returned by a name match) or `runtime_target_ack_missing`, sends no join, saves no credentials and reports possible leftovers (R9). Nothing is cleaned up automatically. P2.1 rollback: the next start's **required activation validation** sees no `runtimeTarget` (`runtime_target_ack_missing`), so the instance does not activate under a downgraded Hub | Same as experiment on |
| Join | Legacy join, unchanged. A join without a descriptor on a flat row gets 409 before any secret change | Same | P1 (co-located): no HTTP join (in-memory credentials). P2.1 fresh: not reached after a refused registration. **P2.1 mixed replicas** (registration acknowledged by a capable replica, join served by an unaware one): the join response lacks the acknowledgement, so the secret is discarded and the instance is not activated (`runtime_target_ack_missing`, phase join); leftovers as in R9 (iii). P2.1 rollback: no join on restart; activation validation refuses as above | Same |
| Heartbeat | Unchanged for legacy rows | Unchanged | P1: heartbeats go to the in-process Hub; older replicas that receive them record status as today. P2.1 fresh: not reached (not activated). P2.1 rollback: not reached after a restart (activation refused). While an already-activated process keeps running across a Hub downgrade (validation is per activation, not periodic), the old Hub records status as today; the instance reports no profiles, so nothing is backfilled; old full-row writes leave the target columns intact | Same |
| Create (interactive) | Legacy rows only: unchanged, no pin, no field sent. A client-supplied `expectedRuntimeTargetId` gets 409 `runtime_target_mismatch` (empty actual) | Unchanged. A client-supplied `expectedRuntimeTargetId` gets 412 `experiment_disabled` | Fresh: unreachable (not registered). Rollback: the old Hub sends no `expectedRuntimeTargetId` and gets 412 `runtime_target_required` (before any profile check), a loud failure with no side effects on the instance; the old Hub's own create rollback applies | Same |
| Start | Legacy rows: unchanged (no pin, so no pre-check refusal, and no field sent) | Unchanged | Rollback: accepted. The start wire has no profile, the instance uses its single target, and a missing `expectedRuntimeTargetId` is accepted on start | Same |
| Restart | Legacy rows: unchanged (user stop + start; cross-node restart) | Unchanged | Rollback: accepted, as for start (the stop leg goes to the same instance) | Same |
| Reincarnate | Legacy rows: unchanged; reprovision carries no field | Unchanged | Rollback: the worker stops the agent, then the reprovision (create-shaped, no field) gets 412 `runtime_target_required`. The worker's existing failure path re-renders the previous config, and the agent is left stopped. This is a known limitation of a Hub rollback while flat agents exist | Same |
| Scheduler dispatch | `providers[0]` legacy: unchanged. If `providers[0]` is flat, section 7 rules apply (not an old-Runtime Broker case) | Same. A flat `providers[0]` gets the refusal as a failed run | Rollback: the create gets 412 `runtime_target_required`, recorded as a failed scheduled run, with no side effects on the instance | Same |

Mixed Hub replicas after activation (a capable replica activated the instance): a create from an **unaware** replica arrives without `expectedRuntimeTargetId` and is refused by the Runtime Broker with 412 `runtime_target_required` before side effects. A capable replica with the experiment off refuses new creates itself with 412 `experiment_disabled` and still sends the field on lifecycle dispatches. Starts and restarts reach the single target, which is unchanged. This is why the rollout must complete on every replica before flat instances are enabled (R9).

Runtime Broker binary rollback (a new Hub with an old binary started where a flat instance ran): the old binary uses the legacy identity and credential paths, not `runtime-brokers/<key>/`, so it registers as the legacy Runtime Broker. Flat agents stay pinned to the flat row, which shows offline. Their starts go to that row and fail as the Runtime Broker being unavailable. Nothing is retargeted.

## 11. Interaction with run ID, start claims, run intent and recovery

| New element | Interaction |
|---|---|
| Pinned placement columns | Set in the same `CreateAgent` transaction as the row (interactive and scheduled creates), or for an existing agent with an empty Runtime Broker by `SetAgentPinnedRuntimeTarget` before its start dispatch (section 9 step 4); in both cases before run intent, start claim, `beginRun` and launch. Never cleared by `forgetRuntimeTarget`, run-ID writes, launch end, start-claim settlement or reaper actions. Never used as a start-claim target |
| `runtime_brokers.runtime_target_*` | Written only by `CreateRuntimeBroker` and `SetRuntimeBrokerTarget`. Heartbeat write-backs can't roll it back. Independent of `BrokerTargetInventory` |
| `expectedRuntimeTargetId` and the flat refusals | Checked before claim/intent/run-ID writes on the Hub and before any launch bookkeeping on the Runtime Broker. No start marker is set, so a claimed start (once ptone/scion#3081 wires claims) settles as "did not act" through `isConfirmedStartNotActedOnError`. A create rolls back with nothing to compensate |
| Stale-pin refusal | Through the handler pre-checks (user restart before its stop leg; lifecycle start, wake and create-on-existing before reservation and run intent; after the ptone/scion#3081 rebase, in `startAgentCore` before the claim), it happens before any claim, intent, reservation, credential or run-ID write. Through the dispatcher backstop (cross-node execution), the intent was already recorded, and the refusal happens before credential mint and run ID and settles under the terminal-for-intent rule (section 7). Reincarnation is guarded by its handler pre-check. Its backstop can fire only after the worker's stop and reprovision, and only if state changed after the pre-check (only an older binary can cause that). It is then classified through `reincarnationStartLeftNoContainer` as confirmed not-acted-on, and the worker's existing failure path, which re-renders the previous config, applies. Either way it is classified as confirmed not-acted-on |
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
  - build a database at the pre-change shape: first `DROP INDEX` the named unique index (SQLite cannot drop an indexed column), then drop the new columns through raw SQL;
  - insert legacy agents (`applied_config.profile`, `runtimeTarget`/`runtimeTargetCandidate`, `createInputs.profile`) and legacy Runtime Brokers (`runtimes` profiles JSON, `default_profile`);
  - re-run `AutoMigrate`;
  - assert every legacy value round-trips unchanged and the new columns read NULL/empty;
  - assert two brokers with NULL `runtime_target_id` coexist under the unique index.
- **Postgres** runs the same test under `-tags integration`, active only with `SCION_TEST_POSTGRES_URL`. Without it, the test **skips with a message**.
  - **Location and client:** every group C test lives in `pkg/store/entadapter` and uses the dual-dialect `pkg/store/enttest` client (the `run_intent_store_test.go` convention), so the same test body runs on SQLite normally and on Postgres under `-tags integration`. That includes the additive-upgrade test (`TestFlatPlacementColumns_AdditiveUpgrade_Postgres` is the Postgres-only raw-SQL variant, in the same package).
  - The group C prefixes are **added to the `-run` regex of `make test-launch-store-postgres`**: `TestFlatPlacementColumns_AdditiveUpgrade_Postgres` (named exactly, so the SQLite raw-SQL variant does not run against the Postgres client), `TestCreateAgent_PinnedPlacement`, `TestUpdateAgent_PreservesPinnedPlacement`, `TestUpdateAgent_LegacyColumnSetPreservesPin`, `TestUpdateRuntimeBroker_`, `TestCreateRuntimeBroker_`, `TestRuntimeTargetID_`, `TestSetRuntimeBrokerTarget_`, `TestSetAgentPinnedRuntimeTarget_`, `TestRuntimeTargetCandidate_`, `TestPinnedPlacement_`, `TestFindOrphanedAgents_`, `TestReassignAgentsToBroker_`, `TestReassignProjectBroker_`, `TestGetLegacyRuntimeBrokerByName_`. That target fails if anything skips there.
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
  - Layers `[LayerServer]`, Default `false`, Stage `alpha`, Issue `ptone/scion#2926` (the delivery tracker, which outlives P1.1 until the experiment graduates or is retired), Owner `runtime-broker`, ReviewBy `2027-03-31`.
- **Enforcement:** in Hub code with `s.experimentEnabled(experiments.FlatRuntimeBrokers)`, at registration (R1, including the embedded wrapper) and at agent create (section 10).
  - It is not a UI-only flag and is not exposed through `/api/v1/settings/public`.
  - It is not added to `dispatchExperimentNames`. A Runtime Broker's flat behaviour comes from its own configuration.
- With the experiment off, every existing schema, API and behaviour works as before (tested).

## 15. Frozen tests

Stage B commits these names. Groups A–E (including the new pure mismatch tests) run for real in P1.1.

Group F tests are the dispatch half:
- They compile against real types and helpers in P1.1, in the default build. (The pkg/hub and cmd files carry the usual `//go:build !no_sqlite` tag, like the other SQLite-backed test files there.)
- Each calls `t.Skip(pendingFlatDispatch)`, where `const pendingFlatDispatch = "pending ptone/scion#3268: flat dispatch not wired yet"` is defined once per package, so deleting the constant forces every skip to be removed.
- **Compilation rule:** a group F body may reference only (1) symbols that exist at 28eb4f0, (2) symbols P1.1 adds, (3) raw JSON or string literals, or (4) the named F‑arrange helpers. P1.1 therefore adds, without callers: the type `*hub.RuntimeTargetRefusal{Code, Status, Message, Details}` with `Error()` in `pkg/hub/errors.go`; `store.PinnedPlacement`; and the store methods. Behaviour such as `isConfirmedStartNotActedOnError` recognizing the type is P1.2. The skipped assertions state it now, and P1.2 must make them pass. The Stage B review checks that group F compiles.
- **Bodies in P1.1 are complete arrange/act/assert.** They use:
  - the real error-code constants (added in P1.1, section 9);
  - the store with pins and targets (P1.1);
  - existing test harnesses (Hub test server, store fixtures, Runtime Broker test server);
  - raw-JSON request bodies for fields that are not added in P1.1 (the Hub public `CreateAgentRequest` in `handlers_agents_core.go`, `RemoteCreateAgentRequest`, Runtime Broker `CreateAgentRequest`, `StartExtras`).
- Where the thing under test has no P1.1 API (constructing a flat Runtime Broker instance, `RegisterEmbeddedFlatRuntimeBroker`, `StartExtras.ExpectedRuntimeTargetID`), the arrange step goes through a helper in the same test file: `newFlatInstanceTestServer` in pkg/runtimebroker, `registerEmbeddedFlatForTest` in pkg/hub. Its P1.1 body builds as much as the current code allows. Such tests are marked **F‑arrange** below.
- **Frozen test internals (accepted constraints for P1.2):**
  - The Runtime Broker group F tests reach into `*runtimebroker.Server` internals (`Handler`, `Start`, `hubConnections`, `dispatchAttempts`, `launchRegistry`).
  - The fixture types (`flatInstanceFixture`, `flatInstanceOpts`, `flatHubFixture` and their helper methods) are effectively frozen with the bodies; only the named F‑arrange helpers change.
  - The P1 remote-mode gate (`flat_runtime_broker_remote_unsupported`) is enforced in **both** `cmd` `startRuntimeBroker` and `runtimebroker.Server.Start`.
- **P1.2 removes the constant and may change only the bodies of those named arrange helpers.** Assertions and the act steps must pass unchanged, or come with a reviewed amendment to this appendix. The P1.2 review checks both.

**A. Settings (`pkg/config`)**
- `TestRuntimeBrokerInstances_OneDockerEntryRoundTrip`: settings save/load/save stays byte-stable.
- `TestRuntimeBrokerInstances_GlobalConfigRoundTrip`: settings.yaml → `LoadGlobalConfig` → `RuntimeBrokerConfig.Instances` → `ConvertGlobalToV1ServerConfig` gives the same entry.
- `TestRuntimeBrokerInstances_StrictLoaderRejectsTypeErrorAndUnknownKey`: covers the silent-fallback case.
- `TestRuntimeBrokerInstances_StrictLoaderFileResolution`: a global `server` section that fails to unmarshal is still the source (no `--config` fall-through), and an unparseable global settings.yaml is an error.
- `TestServerConfigPut_FileModePreservesRuntimeBrokerInstances` (pkg/hub, P1.1): file mode; presence read from the raw body. An absent key keeps the entry; an explicit list or `[]` is validated and applied.
- `TestServerConfigPut_WorkstationDBExplicitInstancesValidatedAndApplied` (pkg/hub, P1.1): workstation DB mode. A valid explicit list is written; an invalid one is rejected before any write.
- `TestServerConfigPut_WorkstationDBNullBrokerKeepsInstances` (pkg/hub, P1.1): a `null` `server.broker` (and `server`) keeps `instances` alongside the Hub-owned keys.
- `TestServerConfigPut_WorkstationDBUnrelatedKeysUnchanged` (pkg/hub, P1.1): edits to unrelated keys leave `instances` and the other leaves exactly as before.
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
- `TestRuntimeBrokerInstanceHosting_RemoteRefused`: non-empty `instances` with `hubInProcess=false` gives `flat_runtime_broker_remote_unsupported`, including with instance-scoped and legacy credential files present in the test HOME. Empty `instances` with `hubInProcess=false` is allowed (legacy remote hosting unchanged). Non-empty with `hubInProcess=true` is allowed. The case documenting that `--simulate-remote-broker` (Hub in process, but `colocatedBrokerRegisters` false) passes `hubInProcess=false` and is refused.

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
- `TestCheckActivationAck_MissingRegistrationAck`: `runtime_target_ack_missing`, phase register.
- `TestCheckActivationAck_MismatchedRegistrationAck`: `runtime_target_binding_conflict` for a different `brokerId` with no `runtimeTarget` (the name-adoption fixture) and for a matching ID with a different target.
- `TestCheckActivationAck_EvaluationOrder`: ID first, then missing target, then target mismatch.
- `TestCheckActivationAck_MissingJoinAck` and `TestCheckActivationAck_MismatchedJoinAck`
- `TestCheckActivationAck_DistinctCodes`: the two codes are distinct and stable.
- `TestCheckActivationAck_AllPhases`: register, join, embedded and activate phases use the same order and codes.
- `TestCheckActivationAck_MixedReplicaSequence`: register acknowledged, then a join response without `runtimeTarget`, gives `runtime_target_ack_missing` at phase join.
- `TestVerifyScope_DockerDaemonChangeRefused`
- `TestVerifyScope_DockerEndpointChangeSameDaemonAccepted`
- `TestVerifyScope_DockerEmptyDaemonIDIsError`
- `TestVerifyScope_KubernetesIgnoresContextAliasAndCredentials`
- `TestVerifyScope_KubernetesClusterOrNamespaceChangeRefused`

**C. Store and migrations (`pkg/store/entadapter`, dual-dialect `enttest` client)**
- `TestFlatPlacementColumns_AdditiveUpgrade_SQLite`
- `TestFlatPlacementColumns_AdditiveUpgrade_Postgres`
- `TestCreateAgent_PinnedPlacementRoundTrip`
- `TestUpdateAgent_PreservesPinnedPlacement`
- `TestUpdateAgent_LegacyColumnSetPreservesPin`
- `TestUpdateRuntimeBroker_PreservesRuntimeTarget`
- `TestUpdateRuntimeBroker_StripsProfilesOnFlatRow`: a model carrying profiles and a default profile is written with both empty.
- `TestUpdateRuntimeBroker_FlatRowWithLeftoverProfiles`: a flat row whose stored `runtimes`/`default_profile` were written by an older binary (raw SQL). A capability-only update succeeds and clears them.
- `TestUpdateRuntimeBroker_NonFlatProfilesReplacedWithKubernetesProfile`: a non-flat row's `Profiles` replaced with a single kubernetes profile succeeds and round-trips. Mirrors the GoogleCloudPlatform/scion#2480 GCP-identity dispatch fixture.
- `TestUpdateRuntimeBroker_NonFlatEndpointOnlyUpdate`: an Endpoint-only update of a non-flat row succeeds, leaving profiles untouched. Mirrors the ptone/scion#3091 reconcile fixture.
- `TestCreateRuntimeBroker_RuntimeTargetRoundTrip`: includes the strip (a model with profiles and a target is stored with empty `runtimes`/`default_profile`).
- `TestRuntimeTargetID_UniqueIndex`: a duplicate non-NULL value is rejected, and multiple NULLs coexist.
- `TestSetRuntimeBrokerTarget_LegacyRowNotFlat`
- `TestSetRuntimeBrokerTarget_SameTargetUpdatesDisplayName`
- `TestSetRuntimeBrokerTarget_DifferentTargetRejected`
- `TestSetAgentPinnedRuntimeTarget_CompareAndSet`: moves the pin and `runtime_broker_id` together and bumps `state_version`; an empty `expected.RuntimeBrokerID` matches a row whose `runtime_broker_id` is `''` and one where it is NULL.
- `TestSetAgentPinnedRuntimeTarget_MissReturnsPlacementChanged`
- `TestSetAgentPinnedRuntimeTarget_StaleUpdateAgentConflicts`: read the agent, move it, then `UpdateAgent` with the stale model returns a version conflict. The pin and `runtime_broker_id` stay equal.
- `TestRuntimeTargetCandidate_DoesNotTouchPin`
- `TestPinnedPlacement_StaleWhenBrokerMoved`
- `TestFindOrphanedAgents_LegacyOfflineAndMissingBrokersUnchanged`: covers an offline row, a deleted row, an online row (excluded), terminal phases (excluded) and soft-deleted agents (excluded).
- `TestFindOrphanedAgents_ExcludesPinnedAndFlat`
- `TestFindOrphanedAgents_FlatCurrentBrokerAdoptsNothing`
- `TestReassignAgentsToBroker_LegacyUnchanged`
- `TestReassignAgentsToBroker_NeverTargetsFlatBroker`
- `TestReassignAgentsToBroker_SkipsPinnedAgents`
- `TestReassignProjectBroker_LegacyUnchanged`: a legacy-to-legacy repoint, including a missing old row.
- `TestReassignProjectBroker_NeverRepointsToOrFromFlat`
- `TestGetLegacyRuntimeBrokerByName_IgnoresFlatRowWithSameName`: a legacy row and a flat row share a name (case-insensitive), and the legacy row is returned.

**D. Experiment (`pkg/experiments`, `pkg/hub`)**
- `TestFlatRuntimeBrokersExperimentRegistered`
- `TestFlatRuntimeBrokersExperiment_DefaultOffInHub`

**E. Wire types and pure checks (`pkg/api`, `pkg/hubclient`)**
- `TestRuntimeTargetDescriptor_JSONShape`
- `TestExpectedRuntimeTargetID_JSONKey`
- `TestCheckExpectedRuntimeTarget_MatchEmptyAndMismatch`
- `TestRuntimeTargetMismatch_MessageAndDetails`: frozen message and details keys, including an empty actual.

**F. Dispatch half (skipped until P1.2)**

Hub (`pkg/hub/flat_runtime_broker_contract_test.go`, plus one new file in `cmd/` for the embedded legacy registration):
- `TestFlatCreate_ExpectedTargetMismatchRejectedBeforeSideEffects`: no agent row, provider link, project default update, quota reservation, run intent, start claim, run ID or dispatch.
- `TestFlatCreate_ExpectedTargetTowardLegacyBrokerRejected`
- `TestFlatCreate_CheckPrecedence`: covers the empty-per-agent capability 412 before `experiment_disabled`, then target, then profile; and the lifecycle branch order.
- `TestFlatCreate_ExplicitProfileRejected`
- `TestFlatCreate_DefaultProfileNotAppliedWithWarning`
- `TestFlatCreate_PassthroughGateUsesTargetType`
- `TestFlatCreate_PinsPlacementAndSendsExpectedTarget`
- `TestFlatCreate_ExperimentOffRejected`
- `TestFlatCreate_ExperimentOffExistingPinnedAgentLifecycleWorks`: create-on-existing resume with no `runtimeBrokerId` (resolved default differs from the agent's Runtime Broker); the expected target is still sent; a client-supplied matching `expectedRuntimeTargetId` is accepted with the experiment off.
- `TestFlatCreate_ExistingAgentChecksUseAgentBrokerNotResolved`
- `TestFlatCreate_DeleteAndRecreateChecksBeforeDelete`: the new-create checks fail and the existing agent is not deleted.
- `TestFlatCreate_ExistingAgentWithoutBrokerTreatedAsNewCreate`: checks run; on success the pin and Runtime Broker are written together with a `state_version` bump before the start dispatch; `expectedRuntimeTargetId` is sent; the post-dispatch update lands without a version conflict; when the start fails, the placement stays persisted; a concurrent placement gets 409 `conflict`.
- `TestFlatCreate_LifecycleClientExpectedTargetComparedWithPin`
- `TestFlatCreate_RuntimeBrokerRejectionRelayed`: 409/422/412 relayed with code and details, start markers stripped, create rolled back.
- `TestFlatCreate_AccessCheckBeforeLink`
- `TestFlatStart_StalePinRefused_Handler`, `TestFlatStart_StalePinRefused_Restart`, `TestFlatStart_StalePinRefused_Reconcile`, `TestFlatStart_StalePinRefused_WakeDM`: one per start source class (handler pre-check, restart pre-check, dispatcher backstop, wake pre-check). Each relays a 409 with the frozen details, never 502.
- `TestFlatRestart_StalePinRefusedBeforeStopLeg`: no stop is dispatched; run intent and reservation are unchanged; 409 with the frozen details.
- `TestFlatStart_StalePinRefusalIsConfirmedNotActedOn`: `isConfirmedStartNotActedOnError` is true; no credential is minted and no compensating stop runs.
- `TestFlatStart_UnpinnedAgentOnFlatRowIsStale`: refused with `pinnedRuntimeBrokerId: ""`.
- `TestFlatStart_RuntimeBrokerMismatchOnStartRelayed`: a Runtime Broker 409 on start is relayed as 409, not 502.
- `TestFlatStart_RefusalIsTerminalForIntent`: the agent message is set, and reconcile does not re-dispatch the same intent.
- `TestFlatReincarnate_StalePinRefusedBeforeStop`: 409; no stop, reprovision, credential mint or reincarnation record.
- `TestFlatReincarnate_ReprovisionSendsExpectedTarget`: covers `buildCreateRequest` on the reprovision path.
- `TestFlatFinalizeEnv_SendsExpectedTarget`
- `TestFlatStart_CrossNodeRefusalRebuiltFromEnvelope`: an executing-node refusal reaches the requesting node as the typed 409, not 502.
- `TestFlatScheduledCreate_PinsPlacement`: a scheduled create on a flat `providers[0]` is pinned in the `CreateAgent` transaction and sends the expected target. Neither the passthrough nor the project active profile is applied, and `flatCreatePlacement` runs before `applyScheduledProjectDefaultGCPIdentity`/`deriveAgentConfig`.
- `TestFlatScheduledCreate_ExperimentOffRefused`: refused before any write, and the scheduler records a failed run.
- `TestFlatAutoProvide_NoAutomaticProjectLink`: a new project gets no provider link or default to an auto-provided flat row, or to the embedded flat instance, with the experiment on and off.
- `TestRegisterProjectBrokerID_FlatRowRefused`: 409 `runtime_broker_link_path_unsupported` before any project mutation; a legacy row is linked as today.
- `TestFlatCreate_UnlinkedFlatRowAuthorizationWins`: a caller denied by `canDispatchToBroker` on an explicit unlinked flat row gets today's authorization response, not 422.
- `TestFlatCreate_UnlinkedFlatRowNotReachedThroughDefaults`: an unlinked flat row is never selected as the project default or Hub default (fall-through unchanged).
- `TestLegacyCreate_AgentCallerCreateTimeLinkUnchanged`: an agent caller creating on an explicit, unlinked legacy row gets 403 and no provider link is written, as today (the resolver's project-update `CheckAccess` denies it). A caller denied by `canDispatchToBroker` who sends an `expectedRuntimeTargetId` toward an unlinked legacy row gets today's 403, not 409 or 412.
- `TestFlatCreate_UnlinkedFlatBrokerNotAutoLinked`: create with `runtimeBrokerId` of an unlinked flat row gives 422 `runtime_broker_not_linked`; no provider row and no project default are written.
- `TestFlatCreate_ExplicitLinkThenCreateWithBrokerIDOnly`: the P1 slice path. Link the flat row with `POST /api/v1/projects/{id}/providers`, then create with only `runtimeBrokerId`; the agent is pinned.
- `TestFlatCreate_AuthorizationBeforeFlatChecks` (fixture: a **linked** flat row): a caller denied by `canDispatchToBroker` gets the existing authorization error, even when a flat check would also fail. No flat code is returned and nothing is written.
- `TestFlatCreate_FlatChecksDoNotGrantDispatch`: a request that passes every flat check but fails `canDispatchToBroker` is denied.
- `TestFlatReincarnate_MoveStillNotImplemented`: a non-dry-run move of a pinned agent gets 501, unchanged.
- `TestFlatReincarnate_MoveDryRunReportsPinnedIneligible`: 409 `runtime_target_move_unsupported` through `writeMoveRefusal`; the verdict's first check `runtime_target` failed and the rest not evaluated.
- `TestFlatReincarnate_DryRunMoveOfStalePinGetsPlanAnswer`: a stale-pinned agent's dry-run move gets the move refusal, not `runtime_target_pin_stale`.
- `TestFlatReincarnate_ProfileNotRederived`
- `TestFlatRegistration_TargetChangeRejected`
- `TestFlatRegistration_JoinDescriptorMismatchKeepsSecret`: the existing secret is unchanged after a refused join.
- `TestFlatRegistration_ResponseEchoesRuntimeTarget`: the registration and join responses echo the stored descriptor for a flat row, and none for a legacy row.
- `TestFlatRegistration_LegacyRowNotConverted`
- `TestFlatRegistration_LegacyReRegistrationOfFlatIDRejected`
- `TestFlatRegistration_NameOrSlugCollisionOnCreateRejected`
- `TestFlatRegistration_ReRegistrationNotBlockedByLaterNameCollision`
- `TestFlatRegistration_NameChangeInConfigNotApplied`
- `TestFlatRegistration_ReRegistrationRequiresOwner`
- `TestFlatRegistration_ExperimentOffRejectsNewAllowsExisting`
- `TestFlatRegistration_EmbeddedPathUsesSharedRules` (F‑arrange)
- `TestFlatRegistration_EmbeddedBoundResultRequired` (F‑arrange): activation only when the returned row's ID and `runtimeTarget` match the identity (`CheckActivationAck`, phase embedded).
- `TestFlatRegistration_EmbeddedConflictingRowNotActivated` (F‑arrange): a stored different target gives `runtime_target_changed`/binding conflict; not activated; reported through `EmbeddedBrokerRegistrationFailed`.
- `TestFlatRegistration_EmbeddedLegacyRowNotActivated` (F‑arrange): a legacy row with the flat ID gives `runtime_broker_not_flat`; not activated; no automatic cleanup.
- `TestFlatRegistration_EmbeddedNameCollisionNotAdopted` (F‑arrange)
- `TestFlatRegistration_EmbeddedSideDuties` (F‑arrange): no provider link and no default for the flat row, `SetEmbeddedBrokerID`, and refusal reported without a legacy fallback.
- `TestLegacyRegistration_NameCollidingWithFlatRowRefused_Brokerauth`
- `TestLegacyRegistration_NameCollidingWithFlatRowRefused_Embedded` (in `cmd/`)
- `TestLegacyEmbeddedRegistration_SkipsFlatRowByName` (in `cmd/`)
- `TestDeprecatedRegisterProject_DoesNotAdoptFlatRow`: decided before any project mutation.
- `TestAdminPatch_RenameCollidingWithFlatRowRejected`
- `TestHeartbeat_DropsProfilesForFlatRow`
- `TestLegacyCreate_ExperimentOffUnchanged`

Runtime Broker (`pkg/runtimebroker/flat_runtime_broker_contract_test.go`). All are F‑arrange (built with `newFlatInstanceTestServer`) except the last:
- `TestFlatInstanceCreate_ExpectedTargetMismatchBeforeAttempt`: no attempt file, launch registry entry, project dirs/markers or runtime call.
- `TestFlatInstanceCreate_MissingExpectedTargetRejected`
- `TestFlatInstanceCreate_NonEmptyProfileRejected`
- `TestFlatInstanceCreate_CheckPrecedence`: covers decode, then required, then mismatch, then profile.
- `TestFlatInstanceCreate_EmptyProfileIgnoresSettingsActiveProfile`
- `TestFlatInstanceStart_ExpectedTargetMismatchRejected`
- `TestFlatInstanceStart_UndecodableBodyRejected`: unknown keys are still accepted.
- `TestFlatInstanceStart_WithoutExpectedTargetUsesOnlyTarget`
- `TestFlatInstanceStart_IgnoresSavedProfile`
- `TestFlatInstanceStart_MismatchKeepsRunIDFencing`
- `TestFlatInstanceCreate_FromUnawareReplicaRefused`: a create without `expectedRuntimeTargetId` after activation (unaware replica) gets 412 before side effects.
- `TestFlatInstanceServer_RemoteModeNeverActivates` (F‑arrange): with saved instance and legacy credentials and no in-process Hub, the instance is not activated (no connection, heartbeat or dispatch) and reports `flat_runtime_broker_remote_unsupported`.
- `TestFlatInstanceServer_LoadsNoLegacyCredentials` (F‑arrange): a co-located flat instance opens no Hub connection with a legacy multi-store or `broker-credentials.json` identity.
- `TestLegacyInstanceCreate_NonEmptyExpectedTargetRejected`: a full body using the existing test server and raw JSON.

**P2.1 registration and activation tests (frozen specification; committed with P2.1 in `pkg/brokerregistration`).** Their act functions (`brokerregistration.RegisterInstance`, `brokerregistration.ValidateActivation`) belong to P2.1, so these tests cannot compile in P1.1. They are frozen here by name, act function and assertion. P2.1 writes them unchanged against those functions; the pure decision logic they rely on is live in P1.1 (group B `TestCheckActivationAck_*`).
- `TestRegisterInstance_UnawareHubRefusedBeforeJoin`: no acknowledgement gives `runtime_target_ack_missing`; no join, no credentials saved, identity unchanged.
- `TestRegisterInstance_NameAdoptionRefused`: another row's ID with no `runtimeTarget` gives `runtime_target_binding_conflict`; no join.
- `TestRegisterInstance_TargetMismatchRefused`
- `TestRegisterInstance_JoinMissingAckDiscardsSecret`: no credentials saved; the refusal reports the rotated secret and that the row may show online.
- `TestRegisterInstance_JoinEchoMismatchDiscardsSecret`
- `TestRegisterInstance_MixedReplicasNotActivated`: register acknowledged by one fake replica, join served by an unaware one; nothing is saved or activated.
- `TestRegisterInstance_NoAutomaticCleanup`: no delete or rewrite of the pre-existing row; leftovers reported.
- `TestRegisterInstance_IdentityPreservedAfterRefusal`
- `TestRegisterInstance_UserCredentialRequired`: a Runtime Broker HMAC identity alone is not admitted.
- `TestRegisterInstance_CredentialsWrittenOnlyAfterBothAcks`
- `TestValidateActivation_SavedCredentialsDowngradedHubRefused`: saved credentials, the Hub returns no `runtimeTarget`; `runtime_target_ack_missing`; not activated.
- `TestValidateActivation_BindingConflictRefused`
- `TestValidateActivation_NoInstanceCredentials`: `flat_runtime_broker_not_registered`; legacy credentials present are ignored.
- `TestValidateActivation_EveryStart`: validation runs on each start, not once.

## 16. Scope

### P1 and P2 scope

- **P1 acceptance** is a real **co-located** slice: config, store, Hub create, Docker execution, the saved-placement view and restart/inventory, with the flat instance running in the Hub's process.
- **P2** proves the remote two-VM topology. P2.1 builds remote registration and the required activation validation exactly as frozen in R9/R10.
- P1 code fails closed for remote flat hosting (R10).

### Non-goals for P1.1

- No Runtime Broker startup, registration, dispatch, heartbeat or UI wiring (P1.2/P1.3).
- No Kubernetes implementation, other runtime types or target-health polling.
- No fleet conversion and no change to existing agents' data.

### P1.1 code surface

- `pkg/config`: `settings_v1.go`, `hub_config.go` (`RuntimeBrokerConfig`, conversion, legacy rejection, strict loader), schema, validator.
- `pkg/brokeridentity`: new, including `CheckActivationAck` and `AckError`.
- `config.CheckRuntimeBrokerInstanceHosting` (the P1 fail-closed gate) and the four instance-side codes in `pkg/api/runtime_target.go`.
- `pkg/api/runtime_target.go`: descriptor, pure check, the shared wire codes, and the two activation acknowledgement codes.
- `pkg/hub/errors.go`: also the `RuntimeTargetRefusal` type (fields and `Error()`, no callers) so that group F compiles.
- `pkg/ent/schema` (runtimebroker, agent) plus generated code.
- `pkg/store`: models, interface, and entadapter setters and guards, including the orphan guards in `agent_store.go` and the `UpdateRuntimeBroker` strip in `project_store.go`.
- Error-code constants only (no handler logic): the three shared codes in `pkg/api/runtime_target.go`, aliased in `pkg/hub/errors.go` and `pkg/runtimebroker/errors.go`, plus the Hub-only codes in `pkg/hub/errors.go`.
- `pkg/hub/admin_settings.go`: the file-mode carry-over and validation for `server.broker.instances`, plus its test. Confirmed for P1.1.
- `pkg/hub/admin_settings_workstation.go`: the additive workstation DB-mode guard (b), with its three tests. Ownership confirmed for P1.1.
- The store query `GetLegacyRuntimeBrokerByName` (filtered name lookup, T3).
- `GLOSSARY.md`: the two terms in section 6.
- `pkg/experiments/registry.go`.
- Additive struct fields only (no handler logic), exactly:
  - `RuntimeTarget *api.RuntimeTargetDescriptor` (`runtimeTarget,omitempty`) on Hub `CreateBrokerRegistrationRequest`, `BrokerJoinRequest`, `CreateBrokerRegistrationResponse` and `BrokerJoinResponse` (`pkg/hub/brokerauth.go`), and on hubclient `CreateBrokerRequest`, `JoinBrokerRequest`, `CreateBrokerResponse` and `JoinBrokerResponse` (`pkg/hubclient/runtime_brokers.go`);
  - `ExpectedRuntimeTargetID string` (`expectedRuntimeTargetId,omitempty`) on hubclient `CreateAgentRequest` (`pkg/hubclient/agents.go`);
  - store `RuntimeBroker.RuntimeTarget` and the `Agent` pin fields.
  - Not in P1.1 (P1.2, raw JSON in group F): the Hub public `CreateAgentRequest`, `RemoteCreateAgentRequest`, Runtime Broker `CreateAgentRequest` and `StartExtras`.
- The Makefile regex for `test-launch-store-postgres`.
- New test files.

## 17. Authorization layering (ptone/scion#3340)

This section states which layer decides what, so a flat dispatch has **exactly one** authorization rule. It uses only the public description of ptone/scion#3340 (explicit project-owner links; dispatch authorized by the link; existing links grandfathered).

**Dispatch authorization: one seam.** Placement of a new agent on a Runtime Broker for a project is authorized **only** by `canDispatchToBroker` (`pkg/hub/handlers_agent_create_helpers.go`). Lifecycle dispatch keeps the existing per-agent action authorization, and the scheduler dispatches only to the project's own provider links. The flat contract adds no rule to any of them.
- `checkBrokerDispatchAccess` is its response-writing wrapper and adds no rule.
- The resolver's selection filter calls the same function.
- After ptone/scion#3340 phase 1, `canDispatchToBroker` authorizes on an active project/Runtime Broker link, or the Runtime Broker acting as itself.
- The flat contract adds **no** other dispatch authorization check.

**Link creation is separate from dispatch authorization.** A link to a flat row comes **only** from the one explicit link action (`POST /api/v1/projects/{id}/providers`; `RegisterProject` with `brokerId` refuses flat rows, section 6; under that endpoint's own project-update permission; `scion runtime-broker provide`). This is the action ptone/scion#3340 turns into owner election. Flat rows **never** receive an automatic link, co-located (P1) included (R7, R8; section 9 step 1 answers 422 `runtime_broker_not_linked` rather than linking).

**Flat correctness checks are not authorization.** These are: R9/R10 activation, target binding (R3/R4), the expected-target mismatch, stale pins, the profile refusal and the `hub.flat_runtime_brokers` feature gate. They decide whether a request reaches the **right identity and target**. Passing them grants nothing. Their codes are never 401/403, and none of them reads permissions.

| Layer | Decides | Owner | Codes |
|---|---|---|---|
| Dispatch authorization | Whether the caller may dispatch to this Runtime Broker for this project | `canDispatchToBroker` (today; ptone/scion#3340 after its phase 1) | Today's authorization responses |
| Link creation | Whether a project is linked to a Runtime Broker | The explicit providers endpoint (ptone/scion#3340 owner election) | That endpoint's responses; `runtime_broker_link_path_unsupported` (`RegisterProject` with `brokerId` naming a flat row); `runtime_broker_not_linked` when a create targets an unlinked flat row |
| Flat correctness | Right identity and target | This appendix (P1) | `runtime_target_*`, `runtime_broker_not_flat`, `runtime_broker_name_conflict`, `runtime_profile_unsupported`, `experiment_disabled`, the instance-side codes |

**Order and precedence** (all before any write. The only exception is a **legacy** row's create-time link in step 1, which is unchanged from today and may persist when a later step refuses, as it does today. No flat-contract check runs before step 2, and no link is ever written for a flat row):
1. resolve (never links a flat row; for an explicit unlinked flat row, `canDispatchToBroker` first, then 422 `runtime_broker_not_linked`; legacy rows keep today's in-resolver link, behind its project-update `CheckAccess`; an agent caller on an unlinked legacy row gets today's 403 and no link);
2. dispatch authorization (`canDispatchToBroker` via `checkBrokerDispatchAccess`);
3. the existing step-2 checks (section 9);
4. the existing-agent read and branch;
5. the flat correctness checks, in the section 9 precedence;
6. the writes.

If dispatch authorization fails, its error is returned and no flat check is evaluated or reported. That includes `runtime_broker_not_linked`, which is answered only after `canDispatchToBroker` admits the caller. The lifecycle pre-check `checkPinnedPlacement` runs after the path's existing per-agent action authorization. The scheduler dispatches only to the project's own links. Registration follows the same layering: R6's existing registration authorization, then R1–R4.

**P1 slice path.** The test project obtains its link to the co-located flat instance explicitly, with `scion runtime-broker provide --broker <id> --project <p>` or `POST /api/v1/projects/{id}/providers`. Creates then send only `runtimeBrokerId` (`TestFlatCreate_ExplicitLinkThenCreateWithBrokerIDOnly`).

**Forward rule.** ptone/scion#3340 phase 1 changes dispatch authorization in one place, the body of `canDispatchToBroker` (or a successor that `checkBrokerDispatchAccess` and the resolver's selection filter call in its place), and changes link creation in the explicit link paths. The flat checks, their order after authorization, and their codes need no change. **The co-located auto-link convenience in ptone/scion#3340 does not apply to flat rows.** Its link paths must exclude every row with a stored runtime target, including the embedded flat instance that R7 records with `SetEmbeddedBrokerID`. They must not key the convenience on the embedded-Runtime Broker identity alone.

## Change log

- **Stage B review appendix changes A1–A5 (approved by the delivery lead):** the agent-caller legacy create is 403 with no link, as today (§9 step 1, §15, §17); `runtime_target_move_unsupported` is verdict-only through `writeMoveRefusal`; the empty-actual mismatch message reads "serves no runtime target"; group F's frozen internals and the two-place remote gate are documented; "no build tags" is replaced by "compiles in the default build".
- **r8 narrow fixes:** the legacy expected-target check stays in step 4 after authorization (step-1 placement removed); the legacy-link exception is scoped in the side-effects list and the §17 order heading; the `RegisterProject` `brokerId` refusal is placed as a read-only pre-mutation lookup; `runtime_broker_link_path_unsupported` is added to the §17 link-creation row.
- **r7 review-round-7 fixes (N1–N6, T1–T6):** `RegisterProject` with `brokerId` refuses flat rows (409 `runtime_broker_link_path_unsupported`), so the providers endpoint is the one explicit link path. For an explicit unlinked flat row, `canDispatchToBroker` runs before `runtime_broker_not_linked`, so authorization wins. Ack phases include embedded and activate. Legacy rows keep today's in-resolver link order (agent callers unchanged). `hubInProcess` is `colocatedBrokerRegisters`. Forward rule: #3340's co-located auto-link never applies to flat rows. Status line, change log, scope of the §17 headline, defaults-only fall-through, refusal list, `autoProvide` wording and a doc reference fixed.
- **r7 addendum (convergence with ptone/scion#3340, as corrected):** new section 17. Dispatch is authorized only by `canDispatchToBroker`; link creation only by the explicit providers endpoint; flat checks are correctness only; order and precedence frozen; forward seam. Flat rows never receive an automatic link, the P1 co-located instance included: R7 creates no link or default; R8 project-creation auto-provide skips flat rows; the resolver answers 422 `runtime_broker_not_linked` instead of linking. The P1 slice links explicitly. Cross-references in sections 6, 9 and 10. Tests added.
- **r7 B1 (review round 6; placement decision and architecture amendments):** P1 is co-located only; the embedded path validates a bound result against the persisted identity with the `CheckActivationAck` semantics; remote flat hosting fails closed in P1 even with saved credentials (`flat_runtime_broker_remote_unsupported`); P2.1 remote registration (user-authorized, acknowledgements validated before saving) and **required** activation validation on every remote start are frozen, with carriers, codes and tests; no HMAC self-registration; startup compatibility, not periodic monitoring; version matrix split into P1 co-located and P2.1 rows; P1/P2 scope notes.
- **r7 non-blocking fixes (review round 6, N1–N3, T1–T5):** `CheckActivationAck` evaluation order; join-phase leftovers (rotated secret, row shown online) and their recovery; an unaware replica's missing field separated from an experiment-off replica's own refusal; frozen `brokeridentity` types; old-Hub definition; heartbeat `runtimeTarget` naming; move-check order and API doc update; nullable `runtime_broker_id` matching.
- **r6 addendum (architecture decision on r5 B1):** refuse activation, no automatic downgrade. Two distinct acknowledgement codes (`runtime_target_ack_missing`, `runtime_target_binding_conflict`) and a pure `CheckActivationAck` (P1.1). Refusal is not a zero-side-effect guarantee; nothing is cleaned up automatically. All serving replicas must be capable with the experiment on, and dispatch keeps its expected-target guard. P2 forward rule: per-instance rejection. Activation compatibility is separate from credential monitoring. Mixed-replica and no-cleanup tests added; the version table is aligned.
- **r6 (review round 5):**
  - B1: registration and join responses echo the stored `runtimeTarget`; R9 instance-side acceptance (stop before join on a missing or mismatched echo or a different `brokerId`; join response checked too); upgrade order; leftovers and recovery; decision recorded as confirmed in the architecture consultation.
  - N1: §11 reincarnation sentence corrected. N2: dry-run move via `writeMoveRefusal` with the `runtime_target` check; pre-check at the in-place site. N3: frozen store signatures and sentinels; step-4 placement persists on a failed start; concurrent miss gets 409. N4: join check before secret rotation. N5: scheduler row (profile sources, ordering, broker row load, warning sink).
  - T1: Runtime Broker-reported profile write-backs (general rule). T2: 412 message. T3: `buildCreateRequest` uses any non-NULL pin. T4: restart claim and hold after the #3081 rebase. T5: exact P1.1 struct list. T6: filtered name lookup in `RegisterProject`. T7: status-only writers and placement-neutral per-agent operations listed as unaffected.
  - Version-pairing table for every wire exchange (§10).
- **r5 (review round 4, plus a full writer sweep):**
  - B1: the scheduler create path (`dispatchAgentEventHandler`) follows the new-create rules through a shared `flatCreatePlacement` helper; its passthrough pin is in §10; tests added.
  - N1: `checkPinnedPlacement` in `handleReincarnateAgent` before the worker (in scope, no postponement).
  - N2: §7, §8 and §11 list the step-4 caller; the setter returns the updated agent; the write comes before `beginStartDispatchHTTP`.
  - N3: the compilation rule; `RuntimeTargetRefusal` type and `PinnedPlacement` in P1.1.
  - N4: public-create bullets scoped to the new-create branch.
  - T1: separate null-keep list. T2: `buildCreateRequest` is the single setter. T3: retry and cross-node terms defined; typed refusal carried in the `dispatchFailureResult` envelope. T4: missing row means nil. T5: `CreateRuntimeBroker` strip. T6: R8 auto-provide. T7: experiment issue is the tracker.
  - Sweep: messaging-plugin Runtime Broker rows added to the writer table.
- **r4 (review round 3):**
  - B1: pure `checkPinnedPlacement` pre-check returning a typed `*hub.RuntimeTargetRefusal`, called before the user restart's stop leg and before reservation, intent and claim writes, with a backstop at the top of `DispatchAgentStart`/`DispatchAgentRestart`. Classified as confirmed not-acted-on, relayed as 409 by every handler switch. §11 corrected; restart tests added.
  - N1: per-path PUT rules: file mode (P1.1, raw-body presence), workstation DB mode (P1.1, additive guard, ownership confirmed), hosted unchanged 422.
  - N2: an existing agent with an empty Runtime Broker resolving to a flat row takes the new-create checks and is pinned atomically; a flat row with a NULL pin is stale.
  - N3: shared wire codes in `pkg/api`, aliased in both packages; start/restart use the create relay.
  - N4: the `startsInFlight` order is kept; the text is relaxed, with the reason.
  - T1: the existing step-2 checks are placed and ordered. T2: client expected target in the lifecycle branch. T3: filtered `GetLegacyRuntimeBrokerByName`. T4: exact Postgres test name in the regex. T5: tests added.
- **r3 (review round 2 and cross-lane conditions):**
  - B1: frozen Hub create order: resolve, access, read the existing agent, then lifecycle checks against the agent's Runtime Broker and pin, or new-create checks against the resolved Runtime Broker before any write including the recreate delete; then link. Added tests.
  - B2: `SetAgentPinnedRuntimeTarget` bumps `state_version`, with a stale-`UpdateAgent` conflict test.
  - B3: R2 applies only when a flat registration creates a row; re-registration is never blocked by a later collision; name and slug set only at creation; legacy row creators refuse to collide with flat rows; admin PATCH rename rule. Added tests.
  - N1: a missing broker row counts as legacy; Go-side exclusion; legacy regression tests. N2: the store strips profiles on flat rows instead of rejecting them; admin PATCH added to the writer table; leftover-profile test. N3: R6 authorization match. N4: admin PUT carry-over (P1.1) and a startup warning (P1.2). N5: strict-loader file resolution and comparison; json/yaml/koanf tags. N6: entadapter location, dual-dialect client, full regex. N7: error constants moved to P1.1, full group F bodies, F‑arrange helpers. N8: single dispatcher choke point, terminal-for-intent retry rule, per-source tests. N9: R7 embedded side duties. N10: the 501 stays for non-dry-run moves; the dry run reports ineligibility.
  - T1: check precedence. T2: named unique index; the migration test drops the index first. T3: case-insensitive name and exact slug; deprecated-path decision before mutation. T4: in-resolver CheckAccess and the 503 before the link. T5: unknown start keys ignored. T6: scope change means a new identity. T7: the expected target is sent to flat rows whatever the experiment state. T8: glossary terms and the hubclient response type.
  - Cross-lane: two non-flat `UpdateRuntimeBroker` tests (profiles replaced with a single kubernetes profile; Endpoint-only update) mirroring GoogleCloudPlatform/scion#2480 and ptone/scion#3091.
- **r2 (review round 1):**
  - Added the central invariant.
  - B1: orphan guards in the store (P1.1), stale pins refused rather than re-pinned, moves refused in P1.
  - B2: `RuntimeBrokerConfig` shape and conversions, the `server.yaml` rule, the strict loader.
  - B3: inventory of every Runtime Broker row writer, a single shared registration implementation, a target set only on row creation, `runtime_broker_not_flat`, and the `UpdateRuntimeBroker` profile guard.
  - N1: explicit relay case. N2: start-marker semantics, with Hub public envelopes stripped. N3: full code table. N4: every default-profile point, the passthrough gate by target type, and the dispatch warning. N5: existing-agent and move paths. N6: the flat instance bypasses resolution for an empty profile, and an undecodable start body is rejected. N7: CLI-based Docker probe, with an empty daemon ID as an error. N8: lock plus `link` creation and a precise first-boot rule against every legacy ID source. N9: `name` required, slug included, race accepted with a reason. N10: expected target toward legacy. N11: unique index. N12: Makefile regex. N13: settings rollback. N14: tests added.
  - T1: Title/Description frozen. T2: hubclient types named. T3: `UpdateProject` and the access-check order. T4: an empty list means legacy. T5: pure mismatch helper and tests in P1.1.
- **r1:** initial version (ab334be).
