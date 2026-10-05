# Flat Runtime Brokers: P1 contract appendix

Status: Stage A of ptone/scion#3267 (P1.1). **Revision 4** answers review rounds 1–3 and the cross-lane guard-test conditions; the change log is at the end. Parent phase: ptone/scion#3266. Delivery tracker: ptone/scion#2926.

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
    - `server.broker.instances` joins the keys kept when the body sets `server` or `server.broker` to `null` (next to the Hub-owned `broker_id`/`broker_token`), so only an explicit `instances` key removes it.
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
  - the Runtime Broker API object as `runtimeTarget`: store `RuntimeBroker.RuntimeTarget` (P1.1) and the hubclient response type `hubclient.RuntimeBroker` (`pkg/hubclient/types.go`), which gains `RuntimeTarget *api.RuntimeTargetDescriptor` in P1.3.
- **Naming:** on Runtime Broker objects `runtimeTarget` is this opaque descriptor. Inside `appliedConfig`, `runtimeTarget`/`runtimeTargetCandidate` stay the inventory target key strings. P1.1 adds `GLOSSARY.md` entries for **Runtime target ID** (opaque, stable, from the registration descriptor) and **Inventory target key** (the `auxiliaryRuntimeIdentity` string used by heartbeats, start claims and recovery), so the two aren't confused.
- The scope record, endpoint, kubeconfig path and credentials are never sent.
- A flat registration sends no `profiles` and no `defaultProfile`.

### Shared rules

All flat registration goes through **one Hub implementation**, `(*hub.Server).registerFlatRuntimeBroker` (P1.2). The embedded path calls it through an exported wrapper, `(*hub.Server).RegisterEmbeddedFlatRuntimeBroker`, so the experiment snapshot is always the Hub's own `experimentEnabled`.

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
- **R7 Embedded side duties.** The embedded flat path (`RegisterEmbeddedFlatRuntimeBroker`, called from startup in place of `registerGlobalProjectAndBroker`):
  - creates the global project and its provider link for the flat ID if they are missing;
  - sets the project's default Runtime Broker only when it has none;
  - records `SetEmbeddedBrokerID(flatID)`, so the Hub-default passthrough gate identifies the flat instance as the embedded Runtime Broker;
  - reports any refusal (412 `experiment_disabled` at first boot, `ErrExecutionScopeChanged`, an identity error) through `EmbeddedBrokerRegistrationFailed`. It never falls back to the legacy identity.

### Every writer

| Writer | Code (28eb4f0) | Flat rule | Phase |
|---|---|---|---|
| brokerauth create | `createBrokerRegistration` (`handlers_brokers.go`), `FindExistingBroker` (name first) | With `runtimeTarget`: R1–R3 and R6. Without it (legacy): `FindExistingBroker`'s name match uses `GetLegacyRuntimeBrokerByName` (flat rows are never candidates); an ID match on a flat row gets R4; **creating a row whose name/slug collides with a flat row gets 409 `runtime_broker_name_conflict`** | P1.2 |
| brokerauth join | `CompleteBrokerJoin` | The descriptor must equal the stored target (else 409 `runtime_target_changed`). For a flat row, `profiles`/`defaultProfile` are written empty | P1.2 |
| Embedded legacy registration | `cmd/server_broker.go registerGlobalProjectAndBroker` | Not run when `instances` is non-empty. When it runs (legacy process), its name lookup uses the filtered store query `GetLegacyRuntimeBrokerByName` (case-insensitive name **and** `runtime_target_id IS NULL`, P1.1). It never calls `GetRuntimeBrokerByName` and then skips, because that method's `First()` could return a flat row and hide a legacy row of the same name. an ID match on a flat row is a startup error naming R4, and **creating a row whose name/slug collides with a flat row is a startup error** | P1.2 |
| Embedded flat registration | new, `RegisterEmbeddedFlatRuntimeBroker` | R1–R7. No orphan reassignment, no profile write | P1.2 |
| Deprecated `RegisterProject` with `broker` | `pkg/hub/handlers_projects_core.go` (ID then name lookup, name/slug/profile overwrite) | Decided **in the pre-mutation lookup and authorization block**, before any project is created or changed: a flat row found by ID gets 409 `runtime_target_changed`; a name/slug collision with a flat row gets 409 `runtime_broker_name_conflict` (no adoption, no duplicate). It never writes profiles to a flat row | P1.2 |
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
| Reincarnate move | `handlers_agent_reincarnate.go`, `reincarnate_move.go` | Today a non-dry-run move already returns 501 `not_implemented`. **That stays unchanged in P1**: a flat or pinned move still gets 501. A `--dry-run` move of a pinned agent, or onto a flat Runtime Broker, reports a failed eligibility check with code `runtime_target_move_unsupported` in the plan, not an error response. When moves are implemented, the flat refusal (409 `runtime_target_move_unsupported`) must come before any move work, and a permitted explicit move re-pins in the same write as the `runtime_broker_id` change (`SetAgentPinnedRuntimeTarget`) | P1.2 |
| Create-on-existing | `handleExistingAgent` (resumes/starts on `existingAgent.RuntimeBrokerID`; has delete-and-recreate branches) | Uses the check order in section 9. Resuming or starting in place is a lifecycle operation, checked against `existingAgent.RuntimeBrokerID` and its pin, and not refused when the experiment is off. A delete-and-recreate branch is a new create, and the new-create checks run before the delete | P1.2 |

**Stale pin.** A pin is valid only while `pinned_runtime_broker_id == runtime_broker_id`. With the guards above, no current-binary automatic writer can create a stale pin. Only an older binary can (for example an older Hub's move or orphan reassignment).
- On a stale pin, the Hub **refuses** start/restart/create-on-existing dispatch with 409 `runtime_target_pin_stale`, details `{agentId, pinnedRuntimeBrokerId, runtimeBrokerId}`.
- **An agent on a flat row with a NULL pin is also stale.** It is refused the same way, with `pinnedRuntimeBrokerId: ""`. Section 9's rule for agents with an empty Runtime Broker means no current writer produces this state.
- **Pure pre-check (P1.2):** `(*hub.Server).checkPinnedPlacement(agent *store.Agent) error`.
  - It reads only the agent and its Runtime Broker row.
  - It returns nil, or a typed **`*hub.RuntimeTargetRefusal{Code, Status, Message, Details}`** (code `runtime_target_pin_stale`, status 409).
  - The same type carries the Hub's other flat refusals (`runtime_target_mismatch` on the lifecycle branch, `runtime_profile_unsupported`, `experiment_disabled`).
- **Where `checkPinnedPlacement` is called:**
  - **User restart** (`handlers_agent_lifecycle.go`, stop + start): before `beginStartDispatchHTTP`, `recordRunIntent` and **the stop leg**, next to the existing `requireEmptyPerAgentBrokerCapabilityForAgent` pre-check. A refused restart never stops the agent and leaves the run intent and reservation unchanged.
  - **Lifecycle start, wake-on-DM and create-on-existing:** before `beginStartDispatch`/`beginStartDispatchHTTP` and `recordRunIntent`. After the ptone/scion#3081 rebase (which P1.2 rebases onto), this means inside `startAgentCore`, **before the start claim**, the lifecycle op and the capacity hold.
  - **Backstop:** also at the very top of `HTTPAgentDispatcher.DispatchAgentStart` and `DispatchAgentRestart`, before `launchGuardError`, `buildStartEnv` (agent credential mint) and `beginRun`. This covers sources that have no handler pre-check (reconcile, reincarnation, and the start-claim runner once wired).
  - Setting `StartExtras.ExpectedRuntimeTargetID` from a valid pin stays in the dispatcher (P1.2/P1.3).
- **Classification:** `isConfirmedStartNotActedOnError` returns true for `*hub.RuntimeTargetRefusal` (via `errors.As`). No compensating stop runs, no credential is minted, and a held claim settles as "did not act".
- **Relay:** every handler error switch that can receive the refusal (lifecycle start and restart, create-on-existing via `writeExistingAgentGuardError` or a sibling, and wake) writes its own Status, Code and Details. It is never mapped to 502 `runtime_error`.
- **Retry behaviour:** when a refusal is reached only in the dispatcher backstop (reconcile, reincarnation), the intent was already recorded. Reconcile and the run-intent backstop treat it as **terminal for that intent**: the agent message is set to the refusal message, the intent is settled through the existing definite-start-failure path, and the same intent is not re-dispatched. No hot retry loop. The same applies to a Runtime Broker 409 `runtime_target_mismatch` on start/restart.
- Stop and delete still go to the current `runtime_broker_id`, so the agent can be cleaned up.
- There is no automatic re-pin. Repair is an explicit operator action (P5 tooling, or delete and recreate).
- `SetAgentPinnedRuntimeTarget` exists for that explicit move. No start path calls it.

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

**Writers (P1.1 store surface):**
- `CreateAgent` sets the pinned columns when the model carries them.
- `SetAgentPinnedRuntimeTarget(ctx, agentID, expected, next PinnedPlacement)` compare-and-sets all three columns plus `runtime_broker_id` together, and **bumps `state_version` in the same conditional update**. `buildAgentUpdate` writes `runtime_broker_id` from the in-memory model, so without the bump a stale `UpdateAgent` would pass its CAS and write the old Runtime Broker back, leaving a stale pin. With the bump it gets a version conflict. (`run_id` can skip the bump only because `buildAgentUpdate` never writes it.) It is used only by a future explicit move.
- `CreateRuntimeBroker` writes the target columns from `RuntimeTarget`.
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
  - Field present and resolved Runtime Broker legacy: 409 `runtime_target_mismatch` with an empty `actualRuntimeTargetId`.
  - Field present and experiment off: 412 `experiment_disabled`.
- **Hub → Runtime Broker create** (`RemoteCreateAgentRequest` and Runtime Broker `CreateAgentRequest`): a top-level key, not inside `config`. The Hub always sets it to the pinned target ID when dispatching to a flat Runtime Broker, and never sets it toward a legacy one.
  - A current-binary **legacy** Runtime Broker that receives a non-empty value rejects it with 409 `runtime_target_mismatch` (empty actual); it never ignores it. Older binaries ignore unknown keys, but a current Hub never sends the key to a legacy row.
- **Hub → Runtime Broker start/restart:** `StartExtras.ExpectedRuntimeTargetID`, written by `applyStartExtras` as the top-level key `expectedRuntimeTargetId`, so both transports stay in parity. It is set from the agent's valid pin in `DispatchAgentStart`/`DispatchAgentRestart` (section 7, P1.2/P1.3).
- **The Hub sends `expectedRuntimeTargetId` to a flat row whatever the experiment state.** The experiment gates new flat registrations and new creates. It never removes the field from dispatches to an existing flat row, so experiment-off lifecycle operations that reach a Runtime Broker create (the create-on-existing provisioning branch) don't hit 412 `runtime_target_required`.

### Pure check helper (P1.1)

`api.CheckExpectedRuntimeTarget(runtimeBrokerID, actual, expected string) *api.RuntimeTargetMismatch` lives in `pkg/api/runtime_target.go`.
- It returns nil when `expected == ""` or `expected == actual`.
- Otherwise it returns the details for the envelope.
- `api.RuntimeTargetMismatch.Message()` produces the frozen message: "Runtime Broker <runtimeBrokerId> serves runtime target <actual>, but the request expected <expected>". When `actual` is empty, the target text reads "no runtime target".
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
| `runtime_target_move_unsupported` | 409 | `ErrCodeRuntimeTargetMoveUnsupported` | hub | `agentId`, `runtimeBrokerId` | Move eligibility: a dry-run plan check in P1 (non-dry-run moves stay 501), and an error once moves exist (section 7) |
| `runtime_target_pin_stale` | 409 | `ErrCodeRuntimeTargetPinStale` | hub | `agentId`, `pinnedRuntimeBrokerId`, `runtimeBrokerId` | Stale pin (section 7) |
| `experiment_disabled` | 412 | `ErrCodeExperimentDisabled` | hub | `experiment` | Flat registration/create with `hub.flat_runtime_brokers` off |

Notes on the codes:
- `experiment_disabled` is deliberately 412 with a code, not `requireExperiment`'s 404 `route`. These are existing endpoints, and the caller has to learn why a flat request was refused.
- **Start markers:** on the Runtime Broker → Hub hop, a rejection envelope *without* `startAttempted:true` already means "did not act" (`brokerStartAttempted` only treats `true` as meaningful). Flat rejections therefore never set the marker. The run ID is still echoed where today's start-failure responses echo it. The **Hub public** envelopes never contain start markers, following the existing practice of not exposing them to clients.
- **Shared wire codes:** the three codes that cross the Hub/Runtime Broker hop (`runtime_target_mismatch`, `runtime_profile_unsupported`, `runtime_target_required`) are defined once in `pkg/api/runtime_target.go` and aliased in `pkg/hub/errors.go` and `pkg/runtimebroker/errors.go`, following the `wsprotocol.ErrCodeRuntimeAttachUnsupported` precedent. The Hub-only codes are defined in `pkg/hub/errors.go`.
- **Relay:** today `dispatchCreateErrorResponse` maps an unclassified Runtime Broker 409 to 502 `runtime_error`. P1.2 adds an explicit relay case modelled on `relaySkillResolutionError`. It passes through status, code, message and the frozen details keys for the three shared codes, with start markers stripped. **Start and restart responses use the same relay** (the lifecycle, create-on-existing and wake error switches), so a Runtime Broker 409/412/422 on those paths is never mapped to 502.

### Before any side effects

- **Hub create** (P1.2). Writes that must not happen before the checks:
  - the provider auto-link (`AddProjectProvider`) and the project default-broker `UpdateProject` inside `resolveRuntimeBroker`;
  - `handleExistingAgent`, including its delete in the delete-and-recreate branches;
  - quota reservation;
  - `commitAgentCreate`;
  - run intent and start claim;
  - `beginRun`/`SetAgentRunID`;
  - agent token and dispatch.

  **Frozen order:**
  1. **Resolve** the Runtime Broker (pure). Linking is split out of `resolveRuntimeBroker`; the in-resolver project-update `CheckAccess` and the offline 503 stay in this step, before any link.
  2. **Access:** `checkBrokerDispatchAccess`, unchanged. The existing pure checks that follow it today stay here, unchanged and in their current order: `requireEmptyPerAgentBrokerCapability` (412), GCP passthrough authorization, and service-account assignment validation.
  3. **Read the existing agent** (`GetAgentBySlug`, a pure read moved up from its current position) and decide which `handleExistingAgent` branch applies, without executing it.
  4. **Branch:**
     - **Resume or start in place** (an existing agent with a non-empty `RuntimeBrokerID` kept): apply the *lifecycle* rules against `existingAgent.RuntimeBrokerID` and its pin: `checkPinnedPlacement`, the expected target taken from the pin, an explicit profile against a flat row refused (422), and **no experiment refusal**.
       - A client-supplied `expectedRuntimeTargetId` here is compared with the agent's **valid pin**. A mismatch, including an unpinned agent or an agent on a legacy row, gets 409 `runtime_target_mismatch`.
     - **Existing agent with an empty `RuntimeBrokerID`** (which today adopts the resolved Runtime Broker, `handleExistingAgent`): when the resolved Runtime Broker is flat, it takes the *new-create* checks (experiment, expected target, profile). On success its pin and `runtime_broker_id` are written together by `SetAgentPinnedRuntimeTarget` (expected: empty placement), which bumps `state_version`, before dispatch. It never sits on a flat row unpinned. When the resolved Runtime Broker is legacy, today's behaviour is unchanged.
     - **New create** (no existing agent, or a delete-and-recreate branch): apply the *new-create* checks against the resolved Runtime Broker, before any write including that delete.
  5. **Link**, then the existing flow continues unchanged.
- **Check precedence** (the first failing check wins):
  - **Hub new create:** the existing step-2 checks first (so 412 `empty_per_agent` capability comes before 412 `experiment_disabled`), then experiment (412 `experiment_disabled`), then expected target (409 `runtime_target_mismatch`, including toward a legacy row), then explicit profile (422 `runtime_profile_unsupported`).
  - **Hub lifecycle branch:** stale pin (409 `runtime_target_pin_stale`), then client expected target (409 `runtime_target_mismatch`), then explicit profile (422).
  - **Runtime Broker:** decode (400), then target required (412, create only), then mismatch (409), then profile (422).
- **Runtime Broker create:** after decode and validation, before `beginCreateAttempt` (no attempt record, launch registry entry, NFS mount, project markers/dirs or auxiliary-manager cache). The check is deterministic, so a `requestId` replay gets the same answer.
- **Runtime Broker start/restart:** before `beginSyncStart` and any runtime call. The handler opens its in-memory `startsInFlight` entry at entry, before the body is decoded, and that order is **not** changed (it avoids reordering a handler that in-flight work also touches). The entry closes on return, so a refused start is visible in at most one heartbeat's in-flight list, which recovery already tolerates.

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
| Flat instance start/restart | The start wire has no profile field. The profile today comes from the saved agent profile or settings, and the flat instance ignores both (row above). A body that fails to decode (invalid JSON or a type error) is rejected with 400 `invalid_request`, not treated as "no `expectedRuntimeTargetId`". **Unknown keys stay ignored**: no `DisallowUnknownFields`, per the start/restart version-skew contract for GoogleCloudPlatform/scion#1931. A start without the field is accepted, since it can only use the one target |
| Rolled-back legacy binary re-registers a flat identity without a descriptor | 409 `runtime_target_changed` (R4) |

Managed agents (`ManagedAgentsProfile`) don't use a Runtime Broker and are unaffected.

## 11. Interaction with run ID, start claims, run intent and recovery

| New element | Interaction |
|---|---|
| Pinned placement columns | Set in the same `CreateAgent` transaction as the row, before run intent, start claim, `beginRun` and launch. Never cleared by `forgetRuntimeTarget`, run-ID writes, launch end, start-claim settlement or reaper actions. Never used as a start-claim target |
| `runtime_brokers.runtime_target_*` | Written only by `CreateRuntimeBroker` and `SetRuntimeBrokerTarget`. Heartbeat write-backs can't roll it back. Independent of `BrokerTargetInventory` |
| `expectedRuntimeTargetId` and the flat refusals | Checked before claim/intent/run-ID writes on the Hub and before any launch bookkeeping on the Runtime Broker. No start marker is set, so a claimed start (once ptone/scion#3081 wires claims) settles as "did not act" through `isConfirmedStartNotActedOnError`. A create rolls back with nothing to compensate |
| Stale-pin refusal | Through the handler pre-checks (user restart before its stop leg; lifecycle start, wake and create-on-existing before reservation and run intent; after the ptone/scion#3081 rebase, in `startAgentCore` before the claim), it happens before any claim, intent, reservation, credential or run-ID write. Through the dispatcher backstop (reconcile, reincarnation), the intent was already recorded, and the refusal happens before credential mint and run ID and settles under the terminal-for-intent rule (section 7). Either way it is classified as confirmed not-acted-on |
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
- **Bodies in P1.1 are complete arrange/act/assert.** They use:
  - the real error-code constants (added in P1.1, section 9);
  - the store with pins and targets (P1.1);
  - existing test harnesses (Hub test server, store fixtures, Runtime Broker test server);
  - raw-JSON request bodies for fields that P1.2 adds to request structs (`expectedRuntimeTargetId`, `runtimeTarget`).
- Where the thing under test has no P1.1 API (constructing a flat Runtime Broker instance, `RegisterEmbeddedFlatRuntimeBroker`, `StartExtras.ExpectedRuntimeTargetID`), the arrange step goes through a helper in the same test file: `newFlatInstanceTestServer` in pkg/runtimebroker, `registerEmbeddedFlatForTest` in pkg/hub. Its P1.1 body builds as much as the current code allows. Such tests are marked **F‑arrange** below.
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
- `TestCreateRuntimeBroker_RuntimeTargetRoundTrip`
- `TestRuntimeTargetID_UniqueIndex`: a duplicate non-NULL value is rejected, and multiple NULLs coexist.
- `TestSetRuntimeBrokerTarget_LegacyRowNotFlat`
- `TestSetRuntimeBrokerTarget_SameTargetUpdatesDisplayName`
- `TestSetRuntimeBrokerTarget_DifferentTargetRejected`
- `TestSetAgentPinnedRuntimeTarget_CompareAndSet`: moves the pin and `runtime_broker_id` together and bumps `state_version`.
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
- `TestFlatCreate_ExperimentOffExistingPinnedAgentLifecycleWorks`: create-on-existing resume with no `runtimeBrokerId` (resolved default differs from the agent's Runtime Broker), and the expected target is still sent.
- `TestFlatCreate_ExistingAgentChecksUseAgentBrokerNotResolved`
- `TestFlatCreate_DeleteAndRecreateChecksBeforeDelete`: the new-create checks fail and the existing agent is not deleted.
- `TestFlatCreate_ExistingAgentWithoutBrokerTreatedAsNewCreate`: checks run, and on success the pin and Runtime Broker are written together with a `state_version` bump.
- `TestFlatCreate_LifecycleClientExpectedTargetComparedWithPin`
- `TestFlatCreate_RuntimeBrokerRejectionRelayed`: 409/422/412 relayed with code and details, start markers stripped, create rolled back.
- `TestFlatCreate_AccessCheckBeforeLink`
- `TestFlatStart_StalePinRefused_Handler`, `TestFlatStart_StalePinRefused_Restart`, `TestFlatStart_StalePinRefused_Reconcile`, `TestFlatStart_StalePinRefused_WakeDM`: one per start source class (handler pre-check, restart pre-check, dispatcher backstop, wake pre-check). Each relays a 409 with the frozen details, never 502.
- `TestFlatRestart_StalePinRefusedBeforeStopLeg`: no stop is dispatched; run intent and reservation are unchanged; 409 with the frozen details.
- `TestFlatStart_StalePinRefusalIsConfirmedNotActedOn`: `isConfirmedStartNotActedOnError` is true; no credential is minted and no compensating stop runs.
- `TestFlatStart_UnpinnedAgentOnFlatRowIsStale`: refused with `pinnedRuntimeBrokerId: ""`.
- `TestFlatStart_RuntimeBrokerMismatchOnStartRelayed`: a Runtime Broker 409 on start is relayed as 409, not 502.
- `TestFlatStart_RefusalIsTerminalForIntent`: the agent message is set, and reconcile does not re-dispatch the same intent.
- `TestFlatReincarnate_MoveStillNotImplemented`: a non-dry-run move of a pinned agent gets 501, unchanged.
- `TestFlatReincarnate_MoveDryRunReportsPinnedIneligible`: the plan check carries code `runtime_target_move_unsupported`.
- `TestFlatReincarnate_ProfileNotRederived`
- `TestFlatRegistration_TargetChangeRejected`
- `TestFlatRegistration_LegacyRowNotConverted`
- `TestFlatRegistration_LegacyReRegistrationOfFlatIDRejected`
- `TestFlatRegistration_NameOrSlugCollisionOnCreateRejected`
- `TestFlatRegistration_ReRegistrationNotBlockedByLaterNameCollision`
- `TestFlatRegistration_NameChangeInConfigNotApplied`
- `TestFlatRegistration_ReRegistrationRequiresOwner`
- `TestFlatRegistration_ExperimentOffRejectsNewAllowsExisting`
- `TestFlatRegistration_EmbeddedPathUsesSharedRules` (F‑arrange)
- `TestFlatRegistration_EmbeddedSideDuties` (F‑arrange): provider link, default only when unset, `SetEmbeddedBrokerID`, and refusal reported without a legacy fallback.
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
- `TestLegacyInstanceCreate_NonEmptyExpectedTargetRejected`: a full body using the existing test server and raw JSON.

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
- `pkg/store`: models, interface, and entadapter setters and guards, including the orphan guards in `agent_store.go` and the `UpdateRuntimeBroker` strip in `project_store.go`.
- Error-code constants only (no handler logic): the three shared codes in `pkg/api/runtime_target.go`, aliased in `pkg/hub/errors.go` and `pkg/runtimebroker/errors.go`, plus the Hub-only codes in `pkg/hub/errors.go`.
- `pkg/hub/admin_settings.go`: the file-mode carry-over and validation for `server.broker.instances`, plus its test. Confirmed for P1.1.
- `pkg/hub/admin_settings_workstation.go`: the additive workstation DB-mode guard (b), with its three tests. Ownership confirmed for P1.1.
- The store query `GetLegacyRuntimeBrokerByName` (filtered name lookup, T3).
- `GLOSSARY.md`: the two terms in section 6.
- `pkg/experiments/registry.go`.
- Additive fields on the hubclient registration/create request types and on the Hub `CreateBrokerRegistrationRequest`/`BrokerJoinRequest` structs (the type definitions only, no handler logic).
- The Makefile regex for `test-launch-store-postgres`.
- New test files.

## Change log

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
