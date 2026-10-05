# Flat Runtime Brokers: P1 contract appendix

Status: Stage A of ptone/scion#3267 (P1.1), for review. Parent phase ptone/scion#3266; delivery tracker ptone/scion#2926.

This appendix fixes the names and rules that P1.2 (ptone/scion#3268), P1.3 (ptone/scion#3269) and later phases build on. Changing a frozen name after acceptance needs a new review of this document. Where this document and the original illustrative design differ (for example the illustrative `schema_version: "2"` with a top-level `runtime_brokers` list), this document wins.

Reconciled against upstream main **`28eb4f0a5827854837d18b28b18e26688d1641cd`** (fetched 2026-10-05; this is also the planning SHA and the current fork main). The original evaluation used `b8735e7`; section 1 records what changed between the two.

Terms follow `GLOSSARY.md`: always say **Runtime Broker**, never bare "broker", in user-facing text. A **flat Runtime Broker** is a Runtime Broker identity that serves exactly one runtime target. A **Runtime Broker instance** is the in-process object hosting one such identity. The **legacy Runtime Broker** is today's profile-resolving Runtime Broker.

## 1. Source-map reconciliation (b8735e7 → 28eb4f0)

The history in this checkout is shallow, so this comparison is by tree content. About 1.6k files changed overall. On the paths this work touches (`pkg/config`, `pkg/ent/schema`, `pkg/store`, `pkg/runtimebroker`, the Hub create/dispatch/heartbeat code and the registration commands), 129 files changed, roughly +29k/-1.4k lines. The changes that affect this plan are below.

| Area | What is on main now | Effect on the plan |
|---|---|---|
| Run identity | `agents.run_id` (written only by `SetAgentRunID` / `CompareAndSwapAgentRunID`), the `scion.run_id` container label, `launch_*` columns, async launch (`beginAsyncLaunch`, `launchRegistry`), `beginSyncStart`, `startsInFlight` | Pinned placement is a new, independent column set. It never replaces or clears the run ID. Mismatch rejection happens before any of these are written (section 8). |
| Start claims | `start_claim_*` columns and `ClaimAgentStart(target)` in the store. Hub wiring is in flight in ptone/scion#3081 and ptone/scion#3091 and GoogleCloudPlatform/scion#2544. `start_claim_target` holds the *inventory target key* | The opaque runtime target ID (section 4) is not the inventory target key and is never written into `start_claim_target` (section 10). |
| Run intent | `run_intent`, `run_intent_at`, plus `run_intent_marked_at` in ptone/scion#3081 to detect writes by older code | Same pattern used here: separate columns written only by dedicated setters (section 7). |
| Recovery | `BrokerTargetInventory` table (kept out of `runtime_brokers` so write-backs cannot roll it back), recovery observations, the reaper's "fresh complete inventory of its target" | The inventory target key stays as it is: `auxiliaryRuntimeIdentity` (runtime name, plus context/namespace on Kubernetes). P1 does not change it. |
| Observed target | `applied_config.runtimeTarget` / `runtimeTargetCandidate` are string keys, promoted after two matching heartbeats (`nextRuntimeTarget`) and cleared by `forgetRuntimeTarget` after an accepted create/start/restart | Semantics unchanged. The JSON key `runtimeTarget` is already taken inside `appliedConfig`, so the agent's pinned placement uses a different name (section 7). |
| Existing-agent routing | `recorded_runtime.go` (ptone/scion#2748): the Hub sends `?runtime=<recorded type>` and the Runtime Broker searches runtimes of that type, answering 503 `runtime_unavailable` when none is registered | A flat instance has one runtime, so it can only match it or refuse. No new routing in P1.1. |
| Profile fallback | `resolveManagerForOpts` and `resolveRuntimeNameForOpts` still fall back **silently** to the default manager when a profile does not resolve. `buildStartContext` writes project markers/dirs before resolving | A flat instance must reject an incoming profile explicitly, before `beginCreateAttempt` and `buildStartContext` (section 9). It must not inherit the fallback. |
| Registration | `FindExistingBroker` matches **by name first**, then ID. The embedded path (`registerGlobalProjectAndBroker`) also looks up by name and adopts that ID. `resolveBrokerID` mints locally (settings → legacy → DB → new UUID) | A flat registration must never be adopted by name, otherwise a lost identity or a reused name would silently retarget (section 6). |
| Store write semantics | `UpdateAgent` and `UpdateRuntimeBroker` overwrite the full row of their known columns. `applied_config` and the `runtimes` (profiles) JSON are rewritten from the writer's struct | New placement data must live in new top-level columns left out of these updates. It must not be JSON keys inside `applied_config` or `BrokerProfile` (section 7). |
| Migrations | ent auto-migrate with `WithDropColumn(false)`, then ordered Go backfills in `CompositeStore.Migrate`. Postgres tests use `-tags integration` + `SCION_TEST_POSTGRES_URL` (`pkg/store/enttest`) | Additive nullable columns, no backfill needed (section 11). |
| Quota | Per-Runtime-Broker only: `max_agents_per_broker`, `BrokerSettings.MaxAgents`. No per-profile quota on main (that work is unmerged) | Each flat Runtime Broker is its own quota scope. Aggregate limits across split identities are a P3/P5 decision. |
| Conduit | `conduit_session.exec_scope`, `endpoint_incarnation`, `connection_epoch` exist. Nothing computes a Runtime Broker exec scope yet. Runtime Brokers are not on Conduit. Everything is gated by `hub.conduit` | Section 12 records the fencing contract that flat dispatch preserves. The Docker execution-scope record (section 5) is the natural future value for `exec_scope`, but P1 does not set it. |
| Settings | Storage/eviction keys on runtime and profile entries (`shared_dir_storage_backend`, `home_storage_*`, `safe_to_evict`, `priority_class_name`, `kubernetes_service_account_mappings`). `schema_version` is **not** checked at load (only `scion config validate` and migration use the JSON schema). Unknown keys are dropped silently by `LoadVersionedSettings` | Keep schema version `"1"` and add an additive key (section 2). Flat config gets its own explicit validator that the Runtime Broker startup calls, because the loader won't reject bad input. |
| Experiments | `pkg/experiments/registry.go` with server/web layers. `Server.experimentEnabled`. `requireExperiment` exists with no production callers. The Hub pushes selected flags to Runtime Brokers per dispatch (`dispatchExperimentNames`) | `hub.flat_runtime_brokers` is a server-layer experiment checked in Hub handlers. It does not need to reach Runtime Brokers (section 13). |

In-flight work this appendix was checked against: ptone/scion#3081, ptone/scion#3091 (start claims, queued stops, recovery), ptone/scion#3180 (per-dir shared-dir backends in `settings_v1.go` and the settings schema), GoogleCloudPlatform/scion#2544 (start-claim runner), GoogleCloudPlatform/scion#2480 (metadata mode), GoogleCloudPlatform/scion#2479 and GoogleCloudPlatform/scion#2483 (Conduit), and the workspace-recreation wire contract for GoogleCloudPlatform/scion#1931 (start/restart carry workspace inputs through `StartExtras`). The P1.1 changes below only add to those diffs. P1.1 does not edit `cmd/server_foreground.go`, `pkg/hub/httpdispatcher.go`, `pkg/runtimebroker/handlers.go` or `pkg/runtime/k8s_runtime.go`.

## 2. Settings: schema version and key placement

- **Schema version stays `"1"`.** The flat configuration is one new optional key, so v1 files without it are unchanged. A v2 schema would duplicate the whole v1 schema for one key. It would also gain nothing at load time, because `schema_version` is not checked there.
- **Key: `server.broker.instances`**, a list (YAML sequence). It is valid only in global settings, like the rest of `server`. It does **not** use the singular top-level `runtime` key (`V1RuntimeDefaultsConfig`, which holds global runtime behaviour defaults), and it does not use `runtimes` or `profiles`. Process-wide listener and Hub-connection settings (`server.broker.port`, `host`, `hub_endpoint`, …) stay where they are and are shared by every instance the process hosts (P2).
- Go types (in `pkg/config/settings_v1.go`):
  - `V1BrokerConfig.Instances []V1RuntimeBrokerInstanceConfig` with tag `instances`.
  - `V1RuntimeBrokerInstanceConfig { Key string "key"; Name string "name"; RuntimeTarget *V1RuntimeTargetConfig "runtime_target" }`.
  - `V1RuntimeTargetConfig { Type string "type"; DisplayName string "display_name"; Context string "context"; Namespace string "namespace" }`. `context`/`namespace` are Kubernetes-only and defined but not implemented in P1.

One-entry Docker example (invented values):

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

- `key` is required and is the **immutable local instance key** (section 3).
- `name` is the Runtime Broker name registered with the Hub. It is a mutable label, not identity. Default: `<server.broker.broker_name, else hostname>-<key>`.
- `runtime_target.type` is required. P1 accepts only `docker`. `kubernetes` is defined and rejected with "not implemented yet". Any other value is rejected as unsupported.
- `display_name` is optional, non-sensitive and shown in the UI. Default: the type.
- There are no per-instance `agent_defaults` in P1. P3.1 adds them additively under the entry.

**Validation** (`ValidateRuntimeBrokerInstances(instances) []ValidationError`, each error naming its settings path). The JSON schema (`$defs/runtimeBrokerInstance`, `additionalProperties: false`) mirrors these rules for `scion config validate`. Runtime Broker startup (P1.2) calls the validator and refuses to start on any error:

| Condition | Error (path, message gist) |
|---|---|
| duplicate `key` | `server.broker.instances[i].key`: duplicate instance key "k" (also at index j). Reported even when the count rule below also fails |
| more than one entry | `server.broker.instances`: only one Runtime Broker instance is supported in this release (P2 lifts this) |
| `key` missing or not matching `^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$` | invalid instance key |
| `runtime_target` missing, or `type` empty | runtime_target.type is required |
| `type: kubernetes` | Kubernetes Runtime Broker instances are not implemented yet |
| other `type` | unsupported runtime target type "x" (supported: docker) |
| Docker entry with `context` or `namespace` | field not valid for runtime target type docker |

Other rules:
- An unknown key inside an entry fails `scion config validate` through the schema. The loader itself keeps dropping unknown keys, as it does everywhere today.
- `server.broker.broker_id` keeps its current meaning: the legacy Runtime Broker identity. It never seeds a flat instance's ID. When `instances` is non-empty, the P1 process hosts only the flat instance (no legacy Runtime Broker) and logs that the legacy identity is not hosted. If the identity file's `runtimeBrokerId` equals `server.broker.broker_id`, that is an error.
- The co-located settings overlay (`SettingsOverlay`) replaces only `runtimes`, `profiles`, `harness_configs` and `image_registry`. It never reads or writes `server.broker.instances`. A test pins this.
- `active_profile`, `profiles` and `runtimes` are left untouched, because the local CLI keeps using them. A flat instance never consults them to select its target.

Relation to ptone/scion#3180: that PR adds `shared_dir_storage_backends` under `$defs/runtimeConfig` and `$defs/profileConfig` and resolver functions in `settings_v1.go`. The keys and definitions here do not overlap with it. If both land, the only expected conflict is textual adjacency.

## 3. Immutable local instance key

- `key` from the config is the local identity anchor. It names the instance's state directory and is recorded inside the identity file.
- Renaming `key` in config means a different instance. There is no rename path: the old directory is simply not loaded. Combined with the name rule in section 6, a renamed key with an unchanged `name` is refused by the Hub instead of creating a duplicate silently.
- The key is local only. It is not sent to the Hub in P1. P2 may add it to an observational host label.

## 4. Stable opaque IDs and local persistence

- **Runtime Broker ID**: a UUID minted locally at the instance's first boot. It is the existing `runtime_brokers.id` (a UUID column). It is never derived from settings, hostname or name.
- **Runtime target ID**: an independent random UUID, also minted locally at first boot. It is opaque and carries no meaning. It is unique per instance; one target ID never belongs to two Runtime Broker IDs.
- Both are persisted in the **instance identity file** before any registration attempt:
  - path: `<GetGlobalDir()>/runtime-brokers/<key>/identity.json` (normally `~/.scion/runtime-brokers/<key>/identity.json`);
  - directory 0700, file 0600, written atomically (temp file + rename + fsync).
  - Instance-scoped Hub credentials (P1.2) go in the same directory, `<...>/runtime-brokers/<key>/hub-credentials/<hub>.json`, in the existing `brokercredentials` format. P1.1 only reserves the path.

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

- Code location: a new package `pkg/brokeridentity`. It owns load/create/verify and has no runtime or Hub dependency. Docker daemon probing stays in the P1.2 caller, which passes the observed scope in.
- Immutable once written: `instanceKey`, `runtimeBrokerId`, `runtimeTarget.id`, `runtimeTarget.type` and the identity part of `executionScope` (section 5). No code path rewrites them. The file has no mutable fields in P1. `display_name` and `name` stay in settings.

**Missing or broken identity state is explicit:**

| Local state | Result |
|---|---|
| `runtime-brokers/<key>/` does not exist | first boot: mint both IDs, record the observed scope, write the file |
| directory exists, `identity.json` missing (e.g. credentials or state left behind) | error `ErrIdentityMissing`: identity state for instance "k" is missing but other state exists; restore the file or remove the directory to register a new Runtime Broker. Never re-minted |
| file unreadable, invalid JSON, unknown `schemaVersion`, any required field empty | error `ErrIdentityCorrupt`. Never re-minted |
| `instanceKey` in the file differs from the directory key | error `ErrIdentityKeyMismatch` |
| recorded type differs from the configured `runtime_target.type` | error `ErrRuntimeTargetTypeChanged`: a different target needs a different instance key |

Removing the whole directory is the operator's explicit request for a new identity. The result is a new Runtime Broker ID. It never takes over the old registration or its agents. If the `name` is unchanged, the Hub refuses the registration (section 6), so the mistake is visible rather than silent.

## 5. Target identity per runtime

Target identity is the runtime target ID (section 4) **bound to a normalized execution-scope record**. Names, display names, kubeconfig context aliases, file paths and credentials are never identity.

**Docker (implemented in P1):**
- Scope record `{ "type": "docker", "docker": { "daemonId": <docker info .ID>, "endpoint": <normalized endpoint> } }`.
- `endpoint` is the effective daemon address (`DOCKER_HOST`, else the default local socket), normalized: scheme lower-cased, `unix://` paths cleaned, `tcp://host:port` with an explicit port.
- **Identity fields:** `daemonId` when non-empty; otherwise `endpoint`.
- At every start (P1.2), the instance probes the daemon and calls `brokeridentity.VerifyScope(recorded, observed)`:
  - same identity → proceed; an endpoint-only change with the same daemon ID is accepted and logged;
  - a different daemon ID (another node or a reinstalled daemon) → error `ErrExecutionScopeChanged`, naming both values. The instance does not start and does not register. Existing placement is never re-pointed. The recourse is to restore the original daemon or configure a new instance key.
- Docker has no credentials to rotate. Socket permission changes do not affect identity.

**Kubernetes (defined, not implemented):**
- Scope record `{ "type": "kubernetes", "kubernetes": { "clusterUid": <metadata.uid of the kube-system namespace>, "namespace": <resolved namespace>, "apiServer": <normalized server URL, informational> } }`.
- **Identity fields:** `clusterUid` + `namespace`.
- The context name, kubeconfig path, user/auth entries and tokens are excluded, so credential rotation or renaming a context alias keeps the target. Pointing the alias at another cluster changes `clusterUid` and is refused like a Docker daemon change.
- An `apiServer` change with the same `clusterUid` is accepted and logged.
- Implementing this belongs to a later phase. P1.1 only freezes the record shape and ships a normalization/compare unit test.

In every case a scope change is a new target, which needs a new instance key and therefore a new Runtime Broker ID. Nothing retargets an existing Runtime Broker ID or an agent pinned to it.

## 6. Registration descriptor

Wire shape (JSON):

```json
"runtimeTarget": { "id": "0b9e…", "type": "docker", "displayName": "Local Docker" }
```

- Go type: `store.RuntimeTargetDescriptor { ID string "id"; Type string "type"; DisplayName string "displayName,omitempty" }`.
- It is sent on the remote registration requests (`CreateBrokerRegistrationRequest` and `BrokerJoinRequest`, plus the matching `hubclient` types). The embedded path writes the same descriptor through the same store setter.
- It is returned on the Runtime Broker API object as `runtimeTarget`.
- No scope record, endpoint, kubeconfig path or credential is ever sent to the Hub.
- A flat registration sends no `profiles` and no `defaultProfile`.

Hub rules (P1.2 implements, using the P1.1 store setter):
1. `runtimeTarget` present while `hub.flat_runtime_brokers` is off:
   - for a Runtime Broker ID with **no** stored target: 412 `experiment_disabled`, details `{experiment: "hub.flat_runtime_brokers"}`;
   - for an **already-flat** Runtime Broker ID with the same target: accepted, so turning the experiment off never strands existing agents.
2. A flat registration is matched **by Runtime Broker ID only**, never by name. If its `name` equals the name of another existing Runtime Broker → 409 `runtime_broker_name_conflict`, details `{name, existingRuntimeBrokerId}`.
3. Stored target empty → set it. Same ID → no-op (the display name may update). Different ID or type → 409 `runtime_target_changed`, details `{runtimeBrokerId, storedRuntimeTargetId, reportedRuntimeTargetId}`. The stored value is not modified.
4. A legacy registration (no `runtimeTarget`) against an ID that already has a stored target → 409 `runtime_target_changed` (a rolled-back binary cannot silently turn a flat identity back into a profile-resolving one).

The registration and control channel are not Conduit-fenced. Section 12 puts no constraint on this descriptor.

## 7. Pinned agent placement and persistence

New nullable columns. No existing column changes meaning.

**`runtime_brokers`** (`pkg/ent/schema/runtimebroker.go`):
- `runtime_target_id`: string, Optional, Nillable, indexed. NULL means legacy Runtime Broker.
- `runtime_target_type`: string, Optional, Default "".
- `runtime_target_display_name`: string, Optional, Default "".

Store model: `RuntimeBroker.RuntimeTarget *RuntimeTargetDescriptor` with JSON `runtimeTarget,omitempty`.

**`agents`** (`pkg/ent/schema/agent.go`):
- `pinned_runtime_broker_id`: string, Optional, Nillable.
- `pinned_runtime_target_id`: string, Optional, Nillable.
- `pinned_runtime_target_type`: string, Optional, Default "".

Store model: `Agent.PinnedRuntimeBrokerID`, `PinnedRuntimeTargetID`, `PinnedRuntimeTargetType`, all tagged `json:"-"` like the run/launch columns, so an API PATCH can't write them. P1.3 exposes them read-only on agent responses as:

```json
"pinnedRuntimeTarget": { "id": "…", "type": "docker", "runtimeBrokerId": "…" }
```

This deliberately avoids `appliedConfig.runtimeTarget`, which is the observed heartbeat key.

**Writers:**
- `CreateAgent` sets the pinned columns when the model carries them (the Hub create path, P1.2).
- `SetAgentPinnedRuntimeTarget(ctx, agentID, expected, next)` is a compare-and-set on the current values. It is used only for the stale-pin re-pin described below, and does not bump `state_version`.
- `SetRuntimeBrokerTarget(ctx, brokerID, desc)` sets the target when it is NULL, is a no-op when the ID matches, and returns `store.ErrRuntimeTargetChanged` otherwise.
- `buildAgentUpdate` (`UpdateAgent`) and `UpdateRuntimeBroker` **do not** write these columns, and nothing clears them. Deleting the row is the only removal.

**Old and legacy writers:**
- An older binary does not know the columns, so its `UpdateAgent`/`UpdateRuntimeBroker`/heartbeat write-backs leave them intact.
- A current binary's full-row updates also omit them. A pin can't be erased by a stale in-memory model.
- Tests prove this for both `UpdateAgent` and `UpdateRuntimeBroker`, including an update that rewrites `applied_config` and `runtimes`.

**Stale pin:** a pin is valid only while `pinned_runtime_broker_id == runtime_broker_id`. A legacy writer that moves an agent (for example an older Hub's reincarnation to another Runtime Broker) changes `runtime_broker_id` without touching the pin, and the pin then reads as stale. The Hub treats the agent as unpinned. If its current Runtime Broker is flat, it re-pins on the next start through `SetAgentPinnedRuntimeTarget` (P1.3 consumer). A stale pin never redirects a dispatch back to the old Runtime Broker.

Reincarnation snapshots and `applied_config.profile` / `createInputs.profile` stay readable and unchanged. A flat create leaves `profile` empty.

## 8. Expected-target request field and the mismatch error

**Field `expectedRuntimeTargetId`** (string, optional on the wire):
- **Hub public create API** (`POST /api/v1/projects/{id}/agents` request and the `hubclient` create request): optional. Clients normally send only `runtimeBrokerId`. A client that showed the user a specific target (UI, P4) sends it to guard against a stale view.
- **Hub → Runtime Broker create** (`RemoteCreateAgentRequest` and Runtime Broker `CreateAgentRequest`): a top-level key, *not* inside `config`. The Hub always sets it to the pinned target ID when dispatching to a flat Runtime Broker.
- **Hub → Runtime Broker start/restart**: `StartExtras.ExpectedRuntimeTargetID`, written by `applyStartExtras` as the top-level key `expectedRuntimeTargetId`, so the HTTP and control-channel transports stay in parity. It is set from the agent's valid pin (P1.3).

**Mismatch error.** The Hub and the Runtime Broker use the same envelope:

```json
HTTP 409 Conflict
{"error":{"code":"runtime_target_mismatch",
  "message":"Runtime Broker <runtimeBrokerId> serves runtime target <actual>, but the request expected <expected>",
  "details":{"runtimeBrokerId":"…","expectedRuntimeTargetId":"…","actualRuntimeTargetId":"…","startAttempted":false}}}
```

- Constants: `ErrCodeRuntimeTargetMismatch = "runtime_target_mismatch"` in both `pkg/hub/errors.go` and `pkg/runtimebroker/errors.go` (P1.2).
- The Hub relays a Runtime Broker 409 through `dispatchCreateErrorResponse` unchanged.
- `startAttempted:false` (an existing details key) tells start-claim settlement that the start definitely did not happen.

**Before any side effects:**
- **Hub create:** the checks run after the Runtime Broker is resolved and *before* any write.
  - Writes include the provider auto-link inside `resolveRuntimeBroker`, `handleExistingAgent`, quota reservation, `commitAgentCreate`, the run intent, the start claim, `beginRun`/`SetAgentRunID`, the agent token and dispatch.
  - P1.2 must therefore resolve the Runtime Broker without linking, run the flat checks, and only then link.
- **Runtime Broker create:** after decoding and validation, before `beginCreateAttempt` (no dispatch-attempt record, no launch registry entry, no NFS mount, no project markers or dirs, no auxiliary-manager cache entry).
  - The check is pure and deterministic, so a `requestId` replay gets the same answer.
- **Runtime Broker start/restart:** before `beginSyncStart` / `startsInFlight` / any runtime call.

## 9. Legacy negotiation

| Situation | Behaviour |
|---|---|
| Experiment off, legacy Runtime Brokers | Unchanged: current schema, profiles, fallbacks and APIs. New columns stay NULL |
| Experiment off, create targeting a Runtime Broker with a stored target | 412 `experiment_disabled` before side effects. Lifecycle operations on agents already pinned to it (stop, start, delete, logs, attach) keep working |
| Experiment on, create on a legacy Runtime Broker | Unchanged legacy path. No pin, no `expectedRuntimeTargetId` sent |
| Experiment on, create on a flat Runtime Broker with an **explicit** non-empty `profile` | Hub: 422 `runtime_profile_unsupported`, message "Runtime Broker <id> serves one runtime target and does not accept a Runtime Broker Profile (got \"<profile>\")", before side effects |
| Experiment on, flat target, profile only from defaults (project `scion.io/active-profile` annotation, user default, Hub passthrough pin) | Defaults are not applied. The applied config keeps `profile` empty and the request is sent without one. This is not an error: the user selected the Runtime Broker, and the default was never their placement choice |
| A flat instance receives a create/start/restart with non-empty `config.profile` (e.g. an older Hub) | Runtime Broker: 422 `runtime_profile_unsupported` before side effects. Never ignored or silently resolved |
| A flat instance receives a create without `expectedRuntimeTargetId` (an older Hub, or experiment off upstream) | Runtime Broker: 412 `runtime_target_required`, message "this Runtime Broker serves a single runtime target and requires expectedRuntimeTargetId; upgrade the Hub or enable hub.flat_runtime_brokers". A start/restart without it is accepted (it can only use the one target) |
| Rolled-back legacy binary re-registers a flat identity without a descriptor | 409 `runtime_target_changed` (section 6) |

Managed agents (`ManagedAgentsProfile`) don't go through a Runtime Broker and are unaffected.

## 10. Interaction with run ID, start claims, run intent and recovery

| New element | Interaction |
|---|---|
| Pinned placement columns | Set in the same `CreateAgent` transaction as the row. Written before the run intent, start claim, `beginRun` and launch. Never cleared by `forgetRuntimeTarget`, run-ID writes, launch end, start-claim settlement or reaper actions. Not used as a start-claim target |
| `runtime_brokers.runtime_target_*` | Written only by `SetRuntimeBrokerTarget`. Heartbeat write-backs (`UpdateRuntimeBroker`) can't roll it back. Independent of `BrokerTargetInventory` |
| `expectedRuntimeTargetId` | Checked before claim/intent/run-ID writes on the Hub and before any launch bookkeeping on the Runtime Broker. A rejection carries `startAttempted:false`, so a claimed start (once ptone/scion#3081 wires claims) settles as "definitely did not happen", and a create rolls back with nothing to compensate |
| Runtime target ID vs inventory target key | Distinct. `start_claim_target`, `InventoryTarget.id`, `BrokerTargetInventory` and `applied_config.runtimeTarget`/`runtimeTargetCandidate` keep using the inventory key (`auxiliaryRuntimeIdentity`). P1 introduces no mapping. If a later phase wants one, it is added alongside, never by overloading these |
| Run ID | Unchanged: minted by the Hub per create/start/restart, sent as `runId`, labelled `scion.run_id`. Flat instances follow the same path |
| Queued stops / recovery (ptone/scion#3091) | Unchanged. A flat Runtime Broker has one inventory target, so "fresh complete inventory of its target" means that instance's inventory |
| Quota | `max_agents_per_broker` applies per flat Runtime Broker ID |

## 11. Additive migration plan (SQLite and Postgres)

- Ent schema changes are the six optional columns in section 7 plus one index (`runtime_brokers.runtime_target_id`). They are applied by the existing `entc.AutoMigrate` (`WithDropColumn(false)`) on both dialects. There is no data backfill and no new step in `CompositeStore.Migrate`. NULL already means legacy.
- Generated code: `go generate ./pkg/ent`, committed. `make ent-check` must stay clean.
- Tests:
  - SQLite runs in normal CI. It creates a database at the pre-change shape (columns dropped through raw SQL), inserts legacy agent rows (with `applied_config.profile`, `runtimeTarget`/`runtimeTargetCandidate` and `createInputs.profile`) and legacy Runtime Broker rows (`runtimes` profiles JSON, `default_profile`), re-runs `AutoMigrate`, and asserts every legacy value round-trips unchanged and the new columns read as NULL/empty.
  - Postgres uses the same test under `-tags integration`, active only when `SCION_TEST_POSTGRES_URL` is set (the `pkg/store/enttest` convention). Without it, the Postgres case **skips with a message**. Real Postgres evidence is gathered in P1.4. The tests are added to the `test-launch-store-postgres`-style target list if one fits, so a SKIP there fails.
- Rollback: older binaries ignore the extra columns. Dropping them is a later, separately gated release.
- `RuntimeTargetCandidate` semantics are unchanged. A test asserts that `SetAgentRuntimeTarget`/`ClearAgentRuntimeTarget` never touch the pinned columns.

## 12. Conduit run-ID fencing preserved by flat dispatch

Flat dispatch does not change the Conduit fencing contract. Specifically:
- The Hub admits an agent Conduit session only if the Hello launch ID equals the agent's current `run_id`. An empty `run_id` matches nothing.
- Every dispatch path (create, start, restart, on flat and legacy Runtime Brokers alike) sets `SCION_LAUNCH_ID` and the `scion.run_id` label from the same value.
- Reusing a running container returns that container's run ID.
- `run_id` is never cleared. Pinned placement writes do not touch it.
- `startAttempted` and the run ID in start-failure responses survive unchanged. The flat rejections in sections 8 and 9 add `startAttempted:false` and never remove those keys.
- Registration and the control channel are not Conduit-fenced, so there is no constraint on the registration descriptor.

GoogleCloudPlatform/scion#2479 and GoogleCloudPlatform/scion#2483 are in flight in `cmd/server_foreground.go` (Conduit start paths), `pkg/hub/httpdispatcher.go` (additive Conduit capability) and `pkg/runtimebroker/handlers.go` (`SCION_LAUNCH_ID` on start/restart). P1.1 stays out of those files. P1.2 rebases onto them once they land, before its review.

## 13. Experiment registration

- Name **`hub.flat_runtime_brokers`** (registry names need a dotted prefix; `hub.` marks server behaviour). Exported constant `experiments.FlatRuntimeBrokers`.
- Settings: Layers `[LayerServer]`, Default `false`, Stage `alpha`, Issue `ptone/scion#3267`, Owner `runtime-broker`, ReviewBy `2027-03-31`.
- Enforced in Hub handlers with `s.experimentEnabled(experiments.FlatRuntimeBrokers)` at registration (section 6) and agent create (section 9). It is not a UI-only flag and is not added to `/api/v1/settings/public`.
- It is not added to `dispatchExperimentNames`. A Runtime Broker's flat behaviour comes from its own configuration. The Hub signals flat dispatch by sending `expectedRuntimeTargetId`.
- With it off, every existing schema, API and behaviour is usable exactly as before (tested).

## 14. Frozen tests

Stage B commits these names. The tests in groups A–E run for real in P1.1. The group F dispatch-half tests are committed with their scenario and expectations in the test body, and are skipped with `t.Skip("pending ptone/scion#3268: flat dispatch not wired yet")`, through one constant per package (`pendingFlatDispatch`). P1.2 removes the skip and must make them pass unchanged, or bring a reviewed amendment to this appendix.

**A. Settings (`pkg/config`)**
- `TestRuntimeBrokerInstances_OneDockerEntryRoundTrip`: save, load and re-save keep the entry byte-stable.
- `TestRuntimeBrokerInstances_DuplicateKeyRejected`
- `TestRuntimeBrokerInstances_MultipleEntriesRejected`
- `TestRuntimeBrokerInstances_InvalidKeyRejected`
- `TestRuntimeBrokerInstances_TargetTypeRequired`
- `TestRuntimeBrokerInstances_KubernetesNotImplemented`
- `TestRuntimeBrokerInstances_UnsupportedTypeRejected`
- `TestRuntimeBrokerInstances_DockerRejectsKubernetesFields`
- `TestRuntimeBrokerInstances_SchemaMatchesValidator`: `scion config validate` and the validator agree on every case above. The schema drift test stays green.
- `TestRuntimeBrokerInstances_OverlayDoesNotTouchInstances`
- `TestRuntimeBrokerInstances_LegacyConfigUnchanged`: v1 files without the key load exactly as before.

**B. Local identity (`pkg/brokeridentity`)**
- `TestIdentity_FirstBootMintsAndPersistsAcrossRestart`: same IDs after a reload (simulated restart).
- `TestIdentity_MissingWithLeftoverStateIsError`
- `TestIdentity_CorruptFileIsError`, `TestIdentity_UnknownSchemaVersionIsError`
- `TestIdentity_KeyMismatchIsError`
- `TestIdentity_TargetTypeChangeIsError`
- `TestIdentity_BrokerIDEqualsLegacyBrokerIDIsError`
- `TestVerifyScope_DockerDaemonChangeRefused`
- `TestVerifyScope_DockerEndpointChangeSameDaemonAccepted`
- `TestVerifyScope_KubernetesIgnoresContextAliasAndCredentials`, `TestVerifyScope_KubernetesClusterOrNamespaceChangeRefused` (normalization/compare only)

**C. Store and migrations (`pkg/store/entadapter`, `pkg/store/enttest`)**
- `TestFlatPlacementColumns_AdditiveUpgrade_SQLite`
- `TestFlatPlacementColumns_AdditiveUpgrade_Postgres` (`integration` tag, `SCION_TEST_POSTGRES_URL`)
- `TestUpdateAgent_PreservesPinnedPlacement`: a full-row update from a model without pin values, including a rewritten `applied_config`.
- `TestUpdateAgent_LegacyColumnSetPreservesPin`: an update through the ent client setting only pre-change columns, as an older binary would.
- `TestUpdateRuntimeBroker_PreservesRuntimeTarget`, including a profiles/`runtimes` rewrite.
- `TestSetRuntimeBrokerTarget_SetOnceThenImmutable` (same ID no-op, different ID or type → `ErrRuntimeTargetChanged`)
- `TestSetAgentPinnedRuntimeTarget_CompareAndSet`
- `TestRuntimeTargetCandidate_DoesNotTouchPin`
- `TestPinnedPlacement_StaleWhenBrokerMoved` (validity helper)

**D. Experiment (`pkg/experiments`, `pkg/hub`)**
- `TestFlatRuntimeBrokersExperimentRegistered`: server layer, default off, valid entry.
- `TestFlatRuntimeBrokersExperiment_DefaultOffInHub`: `experimentEnabled` reports false without an override.

**E. Wire types (`pkg/store`, `pkg/hubclient`)**
- `TestRuntimeTargetDescriptor_JSONShape`: `{"id","type","displayName"}`, `displayName` omitted when empty.
- `TestExpectedRuntimeTargetID_JSONKey` on the `hubclient` create request.

**F. Target mismatch and negotiation (dispatch half, skipped pending ptone/scion#3268)**

Hub (`pkg/hub/flat_runtime_broker_contract_test.go`):
- `TestFlatCreate_ExpectedTargetMismatchRejectedBeforeSideEffects`: 409 `runtime_target_mismatch`. Asserts no agent row, no provider link, no quota reservation, no run intent, no start claim, no run ID and no dispatch.
- `TestFlatCreate_ExplicitProfileRejected`: 422 `runtime_profile_unsupported`, with no side effects.
- `TestFlatCreate_DefaultProfileNotApplied`: the project annotation profile is not applied; the dispatch has an empty profile.
- `TestFlatCreate_PinsPlacementAndSendsExpectedTarget`
- `TestFlatCreate_ExperimentOffRejected`: 412 `experiment_disabled`.
- `TestFlatCreate_ExperimentOffExistingPinnedAgentLifecycleWorks`
- `TestFlatCreate_RuntimeBrokerMismatchRelayed`: a Runtime Broker 409 is relayed with `startAttempted:false`, and the create is rolled back.
- `TestFlatRegistration_TargetChangeRejected`: 409 `runtime_target_changed`; the stored target is unchanged.
- `TestFlatRegistration_LegacyReRegistrationOfFlatIDRejected`
- `TestFlatRegistration_NameCollisionNotAdopted`: 409 `runtime_broker_name_conflict`.
- `TestFlatRegistration_ExperimentOffRejectsNewAllowsExisting`
- `TestLegacyCreate_ExperimentOffUnchanged`

Runtime Broker (`pkg/runtimebroker/flat_runtime_broker_contract_test.go`):
- `TestFlatInstanceCreate_ExpectedTargetMismatchBeforeAttempt`: 409. Asserts no dispatch-attempt file, no launch registry entry, no project dirs/markers and no runtime call.
- `TestFlatInstanceCreate_MissingExpectedTargetRejected`: 412 `runtime_target_required`.
- `TestFlatInstanceCreate_NonEmptyProfileRejected`: 422 `runtime_profile_unsupported`, never resolved through the default-manager fallback.
- `TestFlatInstanceStart_ExpectedTargetMismatchRejected`
- `TestFlatInstanceStart_WithoutExpectedTargetUsesOnlyTarget`
- `TestFlatInstanceStart_MismatchKeepsRunIDFencing`: the error carries `startAttempted:false`, and run-ID and `SCION_LAUNCH_ID` behaviour is unchanged for accepted starts.

## 15. Explicit non-goals for P1.1

- No Runtime Broker startup, registration, dispatch, heartbeat or UI wiring (P1.2/P1.3).
- No Kubernetes implementation. No other runtime types. No target-health polling.
- No fleet conversion and no change to existing agents' data.

P1.1 code is limited to:
- `pkg/config` (settings, schema, validator);
- `pkg/brokeridentity` (new);
- `pkg/ent/schema` (runtimebroker, agent) plus generated code;
- `pkg/store` (models, interface, entadapter setters);
- `pkg/experiments/registry.go`;
- additive wire types in `pkg/hubclient`, where needed for group E;
- new test files.
