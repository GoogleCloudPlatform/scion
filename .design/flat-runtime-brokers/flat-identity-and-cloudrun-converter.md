# Flat Runtime Brokers: GCP identity on flat targets, and the Cloud Run converter

Status: accepted design. Part A accepted 2026-11-13; Part B accepted 2026-11-16 (maintainer, revision r11 of the working draft); Q5 (passthrough gate) ruled 2026-11-16 and folded in as revision r13 (r14: provider-grant path narrowed to brokers the caller can dispatch to). This document amends `p5-cloudrun-amendment.md` (CR3 per-agent identity bullet / E-10, CR4 `service_account` storage as the private GCP host pair, CR9 instance keys, CR10 identity keys) and `contract-appendix-p3.md` FC10 (Hub default per Runtime Broker). Where it differs from those texts on these subjects, it is authoritative. No production code is supplied here.

- **Issues:** ptone/scion#7764 (per-agent GCP identity on flat Cloud Run), ptone/scion#4895 (Hub default GCP service account per flat Runtime Broker), ptone/scion#7308 remainder (Phase 2 converter, Phase 3 CR8 region, live E2E). Parent: ptone/scion#2926.
- **Implementation issues:** C1 ptone/scion#7801, C2 ptone/scion#7802, C3 ptone/scion#7803 (under ptone/scion#7308); I1 ptone/scion#8182, I2 ptone/scion#8183, I4 ptone/scion#8186, I5 ptone/scion#8187 (under ptone/scion#7764); I3 ptone/scion#8184, I3w ptone/scion#8185 (under ptone/scion#4895); I5a ptone/scion#8238 (under ptone/scion#7764).

## Decisions (maintainer, dated)

- 2026-11-13: Part A accepted (C1–C3), including the transitional blocking finding for the legacy identity keys.
- 2026-11-13 (architect, within Part A, from C1 review): `cloudrun-instances` service account and network are operator input; the CR9 instance key omits the project ID (A.3.4).
- 2026-11-13, Q1: flat Cloud Run identity is always native. The instance always runs as a service account; passthrough is treated like assign (runs as the instance's `service_account`); block runs as a no-role account. A non-service-account identity or an emulated metadata server on Cloud Run is deferred.
- 2026-11-14, Q2: an agent with no GCP identity runs as the no-role block account (legacy parity). Consequence: `block_service_account` is required on a flat `cloudrun` instance.
- 2026-11-14, Q3: no service-account report from flat instances; the Runtime Broker is authoritative for the allow-list; the Hub decides native from the stored target type.
- 2026-11-16, Q4: ptone/scion#4895 is a project-scoped per-broker default service account, assign only. C3 live verification is owned by the Cloud Run lane.
- 2026-11-16: Part B accepted (I1–I5).
- 2026-11-16, Q5: passthrough on flat Cloud Run. (a) The instance's `service_account` becomes the broker row's GCP host pair, so the gate's actAs check tests the account passthrough runs as. Extension: the broker-ownership check is skipped when the broker owner granted the broker as a provider on the agent's project; the actAs check on the service account always runs before the agent is provisioned. Architect readings (owner-consent test, all broker types, legacy host-identity broker excluded, start/restart unchanged) and a display redaction of the pair for non-updaters were accepted without objection (§B.4.5).

---

# Summary

Two independent tracks:

- **Part A — converter track (ptone/scion#7308 remainder).** Fully specified by the landed CR8–CR11 contract. It is independent of the identity decisions: the only overlap is the two legacy identity keys (`cloudrun.assignable_service_accounts`, `cloudrun.block_service_account`), which Part A refuses explicitly (blocking finding, never a silent drop) until Part B adds their flat home, then Part B phase I4 turns the refusal into folding.
- **Part B — one identity model for flat targets (ptone/scion#7764 + ptone/scion#4895).** Node-local identity policy stays on `runtime_target` (as flat Kubernetes already does). Flat Cloud Run identity is always native (the instance itself runs as the chosen account; never emulator-only). The Hub learns "native" from the stored target type, not from a profile report, so no new heartbeat report and no DB migration are needed. Hub default selection gains one project-scoped rung keyed by flat Runtime Broker ID.

No phase in this design adds a DB migration. No `DowngradeFloor` bump.

---

# Part A — Converter, CR8 presentation, live E2E (ptone/scion#7308) — accepted 2026-11-13

## A.1 Problem & goals

Legacy `cloudrun` / `cloudrun-instances` runtime entries cannot be converted: `scion runtime-broker convert --plan` reports blocking `unsupported_flat_target_type`. `runtime_target.region` is not published although CR8 allows it. Flat Cloud Run has never run end to end on real GCP.

Success: a legacy Cloud Run host gets a correct `--plan` (instances, findings, exit code 3 naming `credentials_file`); `region` shows in `scion hub brokers info`, the web target card and the conversion plan; one live run on GCP passes.

## A.2 Non-goals

- `convert --apply` for Cloud Run. It keeps refusing (`RefusePlanNotClean`, because every converted Cloud Run instance carries the `credentials_file` operator-input notice). Operators copy the `--plan` YAML into `settings.yaml` and add `credentials_file`. A `--choices credentials_file.<key>=<path>` extension is a separate item with ptone/scion#7714 / ptone/scion#3288; not designed here.
- `cloudrun-sandbox` (stays blocking `unsupported_flat_target_type`, CR10).
- Carrying identity policy onto flat instances (Part B, phase I4).
- ptone/scion#7309 secret delivery (pre-GA gate, separate).

## A.3 Design

The rules are CR9, CR10, CR11 and CR8 in `p5-cloudrun-amendment.md`; this section only fixes what the contract left open.

**A.3.1 Converter (`pkg/config/profile_conversion.go`)**

1. Constants `ConversionFlatTargetFieldRequired = "flat_target_field_required"`, `ConversionFlatTargetOperatorInputRequired = "flat_target_operator_input_required"`.
2. `flatTargetType`: `cloudrun` and `cloudrun-instances` → `cloudrun`. Remove the `rt.CloudRun != nil || rt.CloudRunInstances != nil` blanket blocking branch; keep blocking for `cloudrun_sandbox`/`substrate` blocks on any entry.
3. Field sources (profile value shadows runtime value, as for Kubernetes):
   - `cloudrun` block: `project_id`, `location` → `region`, `service_account`, `network`, `subnetwork`.
   - `cloudrun_instances` block: `project_id`, `region`.
4. Grouping key: `("cloudrun", project_id, region)` after `location`→`region`. Split within a group when `service_account`, `network` or `subnetwork` differ (same split/suffix mechanics as Kubernetes; instance key = `cloudrun` + project + region under the existing key-safety rules, CR9 errata).
5. Findings:
   - `cloudrun.nfs_server`, `cloudrun.nfs_export` → notice `legacy_field_no_effect`.
   - `cloudrun` or `cloudrun_instances` block on an entry of another type → notice `field_not_valid_for_target_type` (extend the existing cross-target check at `:509-560`).
   - CR2-required field with a legacy source but empty (`project_id`, `region`, `service_account`, both of `network`/`subnetwork`) → blocking `flat_target_field_required` at the field's legacy path; instance still proposed; affected profiles get no `ProfileMap` entry.
   - Every converted `cloudrun` instance → notice `flat_target_operator_input_required` at `server.broker.instances[<i>].runtime_target.credentials_file`; `ProfileMap` entry kept.
   - **Identity keys (transitional, this design's decision):** a non-empty `cloudrun.assignable_service_accounts` or `cloudrun.block_service_account` on a converted entry or profile → **blocking** `field_not_valid_for_target_type` at that legacy path, message: `"<path> cannot be carried to a flat cloudrun instance yet (ptone/scion#7764); remove it from the legacy entry or wait for flat identity support"`. Rationale: FC6 forbids a silent drop, and dropping an allow-list silently changes which accounts agents can use. Blocking is correct because flat Cloud Run refuses all per-agent identity until Part B lands, so converting such a host would change agent behavior. Phase I4 replaces this with folding. No new frozen code is introduced (severity is per emission).
   - The `service_account` value is checked by CR2's form rules in the proposed instance; a legacy default compute/App Engine account produces blocking `flat_target_field_required` with the CR2 refusal text appended (an invalid value is treated as missing, since the instance could not load).
6. `ProfileMap` entries for Cloud Run profiles map to the proposed instance key exactly as for Kubernetes.

**A.3.2 `convert --plan` (`pkg/brokerconvert/plan.go`, `render.go`)**

- `summarize`: `DecisionRequired = true` when any finding is `flat_target_operator_input_required` (in addition to capacity decisions) → `ExitBlocked` (3).
- `findingTexts` entries for both codes.
- Human summary names each operator-input field, e.g. `operator input required: server.broker.instances[cloudrun-myproj-us-central1].runtime_target.credentials_file`. JSON keeps the findings array (no schema bump: additive text only).

**A.3.3 CR8 region presentation (separable from A.3.1)**

- `pkg/config/flat_instance_presentation.go`: `PresentedTarget.Region string` (json `region,omitempty`), populated for `cloudrun` only, in `PresentTarget` and `PresentedTargetFromRuntimeTarget`. Flip the test row `"region"` to `fc4AsIs`; add `TestPresentInstance_CloudRunRedaction` (region present, the five CR8 "never" fields absent).
- Mirror through any `pkg/api` / hub presentation struct carrying `PresentedTarget` (heartbeat → stored broker presentation → API). Additive JSON; no DB column (stored inside the existing presentation blob — developer confirms; if it is a typed column, stop and flag, since that would be a migration).
- `cmd/runtime_target_format.go`: print `Region:` for `cloudrun`.
- Web: `web/src/shared/types.ts` `BrokerRuntimeTarget.region?`; `runtime-target.ts` `runtimeTargetRows` adds a Region row, `runtimeTargetLabel` → `Cloud Run · <region>`; `conversion-plan.ts` `instanceRows` shows region, `FINDING_TEXTS` gains both CR11 codes. Follow `web/AGENTS.md`.
- Docs: `hosted/ha/multi-broker.md:110`, `reference/server-config.md:430-435` (region is published), CLI convert docs (two new findings, exit 3, the identity-key refusal).

**A.3.4 Clarifications from C1 review (architect rulings within accepted Part A)**

1. **`cloudrun-instances` entries (R1 → option A).** Legacy `V1CloudRunInstancesConfig` has only `project_id` and `region`, and the legacy `cloudrun-instances` runtime never reads a `cloudrun` block (`pkg/runtime/factory.go` `NewCloudRunRuntimeFromInstances`). So for an instance backed only by `cloudrun-instances` profiles:
   - `project_id`, `region` have a legacy source (`runtimes.<r>.cloudrun_instances.*`): empty → blocking `flat_target_field_required` at that path.
   - `service_account` and `network`/`subnetwork` have **no legacy source** → notice `flat_target_operator_input_required` at `server.broker.instances[<i>].runtime_target.service_account` and `...runtime_target.network` (one finding for the network pair; message says "network or subnetwork"), alongside `credentials_file`. `ProfileMap` entry kept; exit 3; summary names all three fields.
   - A `cloudrun` block on a `cloudrun-instances` entry stays notice `field_not_valid_for_target_type` and is **not** folded (option C rejected: the legacy runtime ignored it, folding would change the account agents run as).
   - A `cloudrun-instances` profile never merges into an instance with a `cloudrun` profile in the same project/region: absent vs set `service_account` is a differing value, so the existing split rule gives it its own instance. Never adopt a sibling's service account.
   - Option B rejected: it makes `cloudrun-instances` unconvertible and contradicts CR10's "required field with no legacy source → notice".
2. **Instance key (R2) — erratum to CR9.** The proposed instance key reaches the Hub (`ConversionPlanPresentation.Instances[].Key`, web plan card), and CR8 says `project_id` is never published. So the key is `cloudrun-<region>` (key-safe), and the existing FC6 sha256 8-hex suffix rule disambiguates two groups that would share a key (two projects in one region, or Split members). The suffix is computed over the full scope including the project, as today's rule says; a truncated hash is not the value. Finding paths and messages: paths never contain values; finding messages are not part of the presentation, but the human/JSON local plan may name the project (local operator output). Test: `TestProfileConversion_CloudRunKeyOmitsProjectID` (two projects, same region → distinct keys, neither contains the project ID; presentation JSON contains no project ID).

## A.4 Alternatives considered (A)

1. **Fold identity keys now onto new flat fields** — rejected: couples A to unresolved Part B semantics (block account in particular); fields that the broker would refuse at dispatch anyway.
2. **Notice + drop for identity keys** — rejected: violates FC6 (no silent drop of a configured field) and silently widens/narrows agent identity.
3. **New frozen code `flat_target_field_unsupported`** — rejected: a transitional state does not justify a frozen-code amendment; `field_not_valid_for_target_type` blocking with explicit text is accurate enough and disappears in I4.

## A.5 Phases (A)

| Phase | Scope | Size | Depends on |
|---|---|---|---|
| **C1** (vertical slice) | A.3.1 + A.3.2 + the 7 named tests + `TestConvertPlan_CloudRunOperatorInputExits3` + identity-key refusal test | M | none |
| **C2** | A.3.3 (region + web finding copy + docs) | S | none; may run parallel to C1 (web `FINDING_TEXTS` is just strings) |
| **C3** (owned by the Cloud Run lane owner since 2026-11-16) | Live GCP E2E of a flat `cloudrun` instance (A.6 list), including `convert --plan` on a real legacy Cloud Run settings file | S (operational) | C1, C2; coordinate environment with the Cloud Run lane owner (ptone/scion#7695, ptone/scion#7780) |

**Tracker:** C1 = ptone/scion#7801, C2 = ptone/scion#7802, C3 = ptone/scion#7803 (sub-issues of ptone/scion#7308; C3 blocked-by C1 and C2).

C1 is the slice: settings in → proposed instance → findings → plan exit code → human/JSON render. Nothing fans out from it except C3.

## A.6 Acceptance criteria (A)

- Named tests from `p5-cloudrun-amendment.md:204-207` exist and pass: `TestProfileConversion_CloudRunMapped`, `_CloudRunInstancesFieldRequired`, `_CloudRunSandboxStillBlocking`, `_CloudRunBlockOnDockerNotice`, `_CloudRunNFSFieldsNoEffect`, `_CloudRunSplitBySA`, `_CloudRunCredentialsFileNotice`, `TestConvertPlan_CloudRunOperatorInputExits3`, `TestPresentInstance_CloudRunRedaction`.
- New: `TestProfileConversion_CloudRunIdentityKeysBlocking` (each key, on runtime entry and on profile: blocking, path named, no `ProfileMap` entry); `TestProfileConversion_CloudRunLocationToRegion`; `TestProfileConversion_CloudRunDefaultSARequired`; split on differing `network`/`subnetwork`.
- A converted proposed instance plus a filled-in `credentials_file` passes `ValidateRuntimeBrokerInstances` (round-trip test: plan YAML → strict load).
- `convert --plan` on a Docker/Kubernetes-only host is byte-for-byte unchanged (golden).
- `scion hub brokers info` and the web card show region for flat Cloud Run; `TestFlatCloudRun_HubRecordsOmitCloudFields` still passes (region allowed, the others not).
- C3 runbook note (r9, ptone/scion#8075): Cloud Run Instances are shared-CPU (6.25% baseline per vCPU plus a ~500s burst). C3's pass criteria are functional (lifecycle, ownership, redaction), not build/test throughput. Use short, low-CPU agent tasks; record wall-clock timings for information only; a slow CPU-bound step is not a C3 failure.
- C3: activation, create/attach/message/stop/restart/delete, ownership reconcile after broker restart, on a real project with Filestore; evidence records exact revision; no credential path or project number in any Hub response captured.

## A.7 Migration / rollback (A)

Pure additions to a read-only planner and a presentation field. Rollback = revert; an older Hub ignores the extra JSON `region`. No migration.

---

# Part B — One GCP identity model for flat Runtime Brokers (ptone/scion#7764, ptone/scion#4895) — accepted 2026-11-16

## B.1 Problem & goals

1. A flat `cloudrun` instance refuses every per-agent GCP identity (E-10). Any project or hub default identity makes every create on it fail; all agents share one account.
2. A project cannot give agents on one flat Runtime Broker a different default service account from agents on another (the per-profile default does not apply to flat brokers).
3. The Hub's native-identity seams (`markCloudRunNative`, GCP-token withholding, precheck, SA reports) are keyed on `broker.Profiles`, which flat brokers do not have.

Goals: assign and block on flat Cloud Run with legacy-native parity (allow-list, no-role block account, refusal texts naming the setting); defined behavior for unset mode and defaults; a per-broker default rung; one model that reads the same for Docker, Kubernetes and Cloud Run.

## B.2 Non-goals

- Changing legacy (profile) Cloud Run identity, `hub.cloudrun_native_identity` semantics on legacy brokers, or ptone/scion#7558's interim legacy refusal.
- Flat SA capability reports to the Hub (see §B.5 alt 2; flat Kubernetes precedent: none).
- Picker `assignStatus` for flat brokers (ptone/scion#3283; shows "unknown" for flat).
- A broker-global default set by a broker admin (cross-project authorization; §B.5 alt 4).
- Remote-mode flat Cloud Run; ptone/scion#7309.
- Running a Cloud Run instance as a non-service-account identity, and any emulated metadata server on flat Cloud Run (the maintainer, 2026-11-13: deferred to a future iteration).

## B.3 The model (all flat targets)

| | Docker | Kubernetes | Cloud Run |
|---|---|---|---|
| Node-local policy (on `runtime_target`, FC4-a never, not in `ExecutionScope`, snapshot at instance start) | none | `kubernetes_service_account_mappings`, `kubernetes_block_service_account` (landed) | **new:** `assignable_service_accounts`, `block_service_account` |
| assign | metadata emulator + Hub-minted token | KSA from instance mapping | **instance runs as the account** (must be in allow-list); Hub mints no GCP token |
| block | emulator blocks | block KSA / namespace default + emulator | **instance runs as `block_service_account`** (required field) + emulator block (second layer) |
| unset (no identity, no applicable default) | emulator block | upstream default | **instance runs as `block_service_account`** + emulator block (the maintainer Q2 = b) |
| explicit passthrough | allowed per existing rules | existing rules | **instance runs as `runtime_target.service_account`** (the maintainer Q1 ruling) |
| Hub-default / confined-project-default passthrough | allowed (docker/podman gate) | denied by gate → unset | denied by gate → unset → block account; a confined passthrough reaching the broker runs as block (legacy parity) |
| Hub default ladder | explicit > **by-broker default** > project default > hub default (same for all three) |||
| Hub knows "native" from | — | — | stored `broker.RuntimeTarget.Type == "cloudrun"` on a flat row (no report) |
| Allow-list authority | — | broker (no Hub report) | broker (no Hub report) |

Key properties:
- **Always native on flat Cloud Run** (Q1). An emulator-only assign/block is never used, so CR3's invariant (never put an emulator over a real metadata server for a different account) holds. `hub.cloudrun_native_identity` does not apply to flat brokers; `hub.flat_cloudrun_target` (existing, alpha) is the gate.
- **Hub/broker agreement by marker, fail-closed on version skew.** For assign and block the broker still requires the native marker (`RemoteGCPIdentityConfig.NativeCloudRun` / `SCION_METADATA_NATIVE_CLOUDRUN=1`): an old Hub never marks a flat dispatch → new broker refuses assign/block. A new Hub with an old broker → E-10 refusal. Unset mode does not need the marker (no Hub GCP token is involved), preserving today's behavior.

## B.4 Proposed design

### B.4.1 Settings (`pkg/config`)

`V1RuntimeTargetConfig` / `RuntimeTargetConfig` gain (keys json/yaml/koanf; camel mirror `assignableServiceAccounts`, `blockServiceAccount`):

```go
AssignableServiceAccounts []string // cloudrun only
BlockServiceAccount       string   // cloudrun only
```

Validation in `ValidateRuntimeBrokerInstances` (so the admin server-config PUT applies it). **De-duplication (C1 review follow-up):** I1 first extracts the CR2 service-account form rules into one shared function (e.g. `validateCloudRunServiceAccountForm(path, email, projectID) error`), used by `validateCloudRunTarget`, the converter's `checkProposedCloudRunTarget`, and the new allow-list/block checks below, with no behavior change for the existing callers (existing CR2 tests unchanged). Both fields are in `cloudRunOnlyFieldsSet`; every value passes CR2's `service_account` form rules (user-managed regex, project-default refusal, service-agent refusal — this closes ptone/scion#7466 for flat); lower-case, no duplicates, at most 64 entries; `block_service_account` not in `assignable_service_accounts`; `block_service_account` is **required** for `cloudrun` (missing → "server.broker.instances[<k>].runtime_target.block_service_account is required: agents with no GCP identity run as this no-role account") and differs from `service_account`. Listing `service_account` in the allow-list is allowed (assigning it is equivalent to passthrough). Schema mirrors. `NewFlatInstancePolicy` clones the slice. FC4-a rows: both **never**. Not in `ExecutionScope`.

Activation (`cmd/server_flat_cloudrun.go`): extend P9 (principal and impersonation source differ from every allow-list account and the block account) and P13 (none is the probed compute default). Same reason codes. Doctor: `cloudRunActAsCheck` uses the instance credentials and iterates all agent accounts (follow-up, I5).

### B.4.2 Runtime Broker (`pkg/runtimebroker`)

- `flatCloudRunIdentityPolicy` snapshot at server construction (shape of `flat_k8s_identity.go`): normalized allow-list set, block account, instance `service_account`. Never reads profiles, `vs.Runtimes`, DB overlay or project settings.
- Replace `flatCloudRunGCPIdentityRefusal` (early, before side effects) with `flatCloudRunIdentityDecision(in)` returning the account or a 400, before ownership/file side effects (keeps E-10's ordering property):

```
mode := effective mode (request GCPIdentity, else sealed SCION_METADATA_MODE, else unset)
if mode == passthrough && RequireLocalRuntime: mode = block   // legacy parity
switch mode:
  unset:        account = policy.Block                                // the maintainer Q2 = b; no marker needed (more restrictive, no Hub token)
  passthrough:  account = policy.ServiceAccount                      // the maintainer Q1: passthrough ≈ assign of the instance account; no marker needed
  assign:       require marker else 400 (hub too old)
                require email in allow-list else 400 naming
                  server.broker.instances[<key>].runtime_target.assignable_service_accounts
                account = email
  block:        require marker else 400
                account = policy.Block                               // always set (required field)
```

- Late path: flat branch in `resolveCloudRunIdentity` returns the same decision with `Selection: s.flatDispatchSelection()`, so `rejectCloudRunIdentityRuntimeChange` runs on start/restart. Remove `cloudRunFlatRefusal` branches.
- Always set `cfg.CloudRunIdentity` on flat (unset and block → block account, passthrough → `service_account`), so the `scion-identity` label and `clearExistingForIdentity` (delete-and-recreate on start/restart) apply uniformly. `gcpMetadataMode`: assign and passthrough → passthrough-with-informational-env (as legacy assign); block and unset → `block` (emulator second layer). `refuseCloudRunInitFirewall` applies to assign.
- No heartbeat SA report from flat instances (unchanged exclusion).

### B.4.3 Hub (`pkg/hub`)

One shared predicate, using CR12's shared type matcher:

```go
// flatCloudRunBroker: broker.IsFlat() && broker.RuntimeTarget != nil &&
//   broker.RuntimeTarget.Type == config.RuntimeTargetTypeCloudRun (via the CR12 shared matcher)
```

- `cloudRunNativeLive`: if `flatCloudRunBroker(broker)` → native = true regardless of `hub.cloudrun_native_identity` (gate argument ignored for flat). So `markCloudRunNative` stamps the marker and persists `NativeCloudRun` before dispatch; `agentCloudRunNativelyAssigned` withholds `ScopeAgentGCPToken` and refuses `/api/v1/agent/gcp-token`; stop/delete clearing is unchanged.
- `cloudRunIdentityPrecheck` / `cloudRunIdentityRefusal`: for a flat Cloud Run broker, skip the profile-report logic entirely (no flat report exists); passthrough is not refused. Allow-list and block-account presence are left to the broker (fail-closed there, before any instance exists).
- `kubernetesIdentityPrecheck`: unchanged (flat has no profile → skipped already).
- Error attribution: when a broker 400 rejects an identity that came from a default rung, the create response appends the rung's `fix()` text ("set by the project default for broker X; change it at …"). Requires knowing the rung at create time, which the create handler has; it is not persisted. (Addresses the ptone/scion#7763 confusion for flat.)

### B.4.4 Per-broker default (ptone/scion#4895)

- Project setting `defaultGCPIdentityServiceAccountIDByBroker: map[brokerID]saID`, stored as annotation `scion.io/default-gcp-identity-service-account-id-by-broker` (same storage pattern as by-profile; no DB schema change).
- Write rules (project settings PUT): project admin (same as project default); refused with `experiment_disabled` unless `hub.flat_runtime_brokers` is on (checked in the hub); keys must be existing **flat** Runtime Broker IDs (legacy brokers keep by-profile); values pass the existing `validateProfileDefaultSAIDs` checks (project SA, verified); entry cap as by-profile.
- Read rules: new rung `brokerDefaultTier(brokerName)` between explicit request and project default, in both the interactive create path (`handlers_agents_core.go` near `:2249`) and the scheduled path (`server.go` near `:6293`), via `resolveDefaultSAAssignmentCore` (so authorization, unavailable/unverified refusals and P10 "never silently fall back" apply unchanged). Consulted only when the agent's Runtime Broker is flat; the broker is already pinned by placement. An entry for a deleted broker is ignored at read and removable at write.
- Produces `assign` only (parity with by-profile). A per-broker default *mode* (e.g. block) is out of scope unless Q4 says otherwise.
- CLI: `scion project identity default set|unset --broker <name|id> <sa>` (mutually exclusive with `--profile`), `show` lists the rung. Web: project settings GCP identity section gets a "Per Runtime Broker" list (flat brokers that provide to the project), hidden unless the web flag for `hub.flat_runtime_brokers` is on. Agent create form's inherited-identity hint shows the rung (`agent-config-inherited.ts`).
- Cross-target: on flat Cloud Run the default account must also be in that instance's allow-list; otherwise the broker refuses with attribution (B.4.3).

### B.4.5 Passthrough gate (Q5, ruled 2026-11-16)

Today `evaluatePassthroughIdentity` (`pkg/hub/passthrough_gate.go`) runs (1) broker owner or Hub admin, (2) the GCP host pair is registered, (3) caller `actAs` on `broker.GCPHostServiceAccountEmail`. It runs before dispatch on every passthrough surface: create (`SurfacePassthroughCreate`), PATCH, project default (settings PUT and apply), translation, translation review, and reincarnate move.

**1. The flat Cloud Run instance's account becomes the broker row's GCP host pair** (so check 3 tests the account passthrough actually runs as):

- Co-located (embedded) flat Cloud Run: `registerEmbeddedFlat` writes `runtime_target.service_account` and its project, parsed from the user-managed SA email `<name>@<project>.iam.gserviceaccount.com`, which CR2 guarantees. It goes through the detected-style setter (`SetRuntimeBrokerGCPHostDetected` semantics) on every activation: the value follows config changes, and a pair set or cleared through the update API (`GCPHostSourceAdmin`) wins, with a logged warning on difference. For cloudrun-target rows the embedded registration **does not record `HostIdentity`**: the Hub process's metadata identity is not the instance's account, and recording it would show a permanent `mismatch`.
- Remote flat Cloud Run (`scion broker register --instance`): the registration request carries `gcpHostServiceAccountEmail` = `runtime_target.service_account` and `gcpHostProjectId` = its project. The existing host-SA claim rules apply unchanged: the registering user needs actAs on the pair (`evaluateHostSAClaim`), a fill happens only while it is unset, and differences come back as `KeptSetting`. A later `service_account` change on a remote instance needs a re-register after the pair is cleared, or an update through the API. This is documented in I5.
- Other flat targets: unchanged.

**2. Display redaction (correction to the Q5 wording).** Broker GET and list responses serialize `store.RuntimeBroker`, including `gcp_host_service_account_email` and `gcp_host_project_id`, to every broker reader. So for flat Cloud Run rows, `brokerForDisplay*` clears both fields unless the viewer's capabilities include `update` on the broker. This follows the same rule `withHostIdentity` uses for detail. The broker's own reads (`brokerSelf`) may keep them. Legacy rows are unchanged. `TestFlat_CloudRunHubRecordsOmitCloudFields` keeps asserting no SA email for a reader, and gains an owner/admin case that sees the pair. CR4 amendment: "`service_account` is never sent in the descriptor or presentation. It is stored as the row's private GCP host pair and shown only to the broker's updaters."

**3. Check 1 gains the provider-grant path** (maintainer's extension, all broker types):

```
check1 := isAdmin || broker.CreatedBy == caller
       || (projectID != "" && !hostIdentityIsHubOwn(broker) &&
           s.brokerProviderHasOwnerConsent(ctx, broker, projectID) &&
           s.canUseBrokerForProject(ctx, broker, project))   // r14: never broader than dispatch
// hostIdentityIsHubOwn: s.isEmbeddedBroker(broker.ID) && !flatCloudRunBroker(broker),
//   i.e. the pair is the Hub process's own metadata identity; it keeps today's admin-only rule.
```

`evaluatePassthroughIdentity` gains a `projectID` parameter. Each surface passes the agent's or target project: create → request project; PATCH → agent's project; project default → that project; translation/review → agent's project; move → destination project. Callers already enforce project agent-create rights. `brokerProviderHasOwnerConsent` is the existing dispatch owner-consent test: an operator-provisioned broker with no owner, `LinkedBy` = owner, `auto-provide`, or an active super-admin linker. Reusing it keeps one definition of "the owner granted this broker to the project". **r14:** the path also requires `canUseBrokerForProject`. For legacy rows that is already implied by consent. For flat rows, where a consented link alone does not admit dispatch (`canDispatchToBroker`: AutoProvide or `broker.dispatch`), it keeps passthrough from being broader than create on every surface. Without it, a non-owner could PATCH passthrough onto a flat broker they cannot create on. The denial message changes to "requires broker ownership, Hub admin, or the broker being provided to this project by its owner".

**4. Checks 2 and 3 are unchanged and never skipped.** They still run before dispatch, so no agent is provisioned without actAs on the account. Start and restart of an existing agent are not re-gated (today's behavior; default accepted by silence).

Audit: the allow log, and the denial log when check 1 passed but actAs failed, records `authPath` (`owner` > `admin` > `provider_grant`, by precedence). `store.SAAssignmentEvent` is unchanged.

## B.5 Alternatives considered (B)

1. **Flat Cloud Run follows `hub.cloudrun_native_identity` (emulator when off)** — rejected: re-opens the exact hole E-10 closed (agent can reach the real metadata server for `service_account` under an emulator "assign"/"block").
2. **Target-level SA capability report on flat heartbeat (`TargetSAMappings`) so the Hub prechecks allow-list membership and the picker shows attachability** — deferred, not rejected: adds an API field, store fields on `RuntimeBroker` (a DB migration), freshness/version rules and sends SA emails to the Hub, for an earlier error the broker already gives before any side effect. Flat Kubernetes chose no report (P2 amendment rev 6); keeping both targets consistent. Revisit with ptone/scion#3283 picker work.
3. **Reuse `runtime_target.service_account` as the block account (no `block_service_account`)** — viable and simpler, but CR3 documents `service_account` as the agents' working account ("grant only what agents need"), not a zero-role account; making block equal to passthrough would make "block" promise nothing. Rejected by the Q2 ruling (a separate, required no-role account).
4. **Broker-global default set by a broker admin (literal "per-instance")** — rejected: a broker serving several projects would let its admin attach one project's verified SA to another project's agents, or require a new cross-project authorization contract. Project-scoped by-broker map has the same UX for the project and reuses the existing project-admin + SA-verification checks.
5. **Record an alternative remedy for ptone/scion#4895 (project default + explicit)** — rejected: a project using flat Docker and flat Cloud Run needs different defaults (any project SA on Docker vs. an allow-listed account on Cloud Run); without the rung, a project default `assign` breaks one of them.
6. **Synthetic profile for flat brokers so existing profile-keyed Hub code works** — rejected (same reason as P2 amendment alt 1): brings profiles back into flat.

## B.6 Migration / rollout (B)

- Gates: flat Cloud Run identity behind existing `hub.flat_cloudrun_target`; by-broker default behind `hub.flat_runtime_brokers` (hub-checked). No new experiment.
- **Breaking (alpha, no deprecation path):** an existing flat `cloudrun` instance without `block_service_account` fails validation after upgrade and is not activated; operators add a no-role account first (release note + docs; tell the Cloud Run lane owner before I1 merges, since their live environments may run flat Cloud Run). Existing no-identity agents switch from `service_account` to the block account; next start recreates the instance with the `scion-identity` label (delete-and-recreate is already the flat start path under native — developer confirms no data loss beyond what restart already implies; homes are on NFS).
- Skew: covered in §B.3 (fail-closed both ways).
- Rollback: reverting the broker restores E-10 (identity creates fail with the old text; unset agents unaffected). Reverting the Hub: by-broker annotation is ignored by older Hubs; agents recorded `NativeCloudRun` keep tokens withheld until stop/delete (safe).
- No DB migration; no `DowngradeFloor` bump.
- **Forward compatibility with a possible Worker Pools backend (ptone/scion#8075, options with the maintainer):** Part B's model is independent of the Cloud Run resource primitive. Node-local policy on `runtime_target`, the Hub's native decision from the stored target type, and the no-report broker authority all carry over, because a Worker Pool revision also runs as one service account per workload. What would change is broker/runtime-side only (where `cfg.CloudRunIdentity` is applied, the recreate-on-identity-change mechanics, and the exec path, since Worker Pools have no ingress). If the maintainer picks Worker Pools as a new target type or a `runtime_target` field, the matcher `flatCloudRunBroker` must include it. No Part B phase waits on that decision.
- Contract text: amend p5 CR3 "Per-agent GCP identity" bullet and CR10 (identity keys fold) and FC10 (Hub default per broker) when the phases land.

## B.7 Questions and rulings

1. **Q1 — Always native on flat Cloud Run?** Options: (a) always native, ignore `hub.cloudrun_native_identity` for flat [recommended]; (b) require `hub.cloudrun_native_identity` on, refuse identity when off; (c) emulator when off. **Status: RULED (a), 2026-11-13; also passthrough supported as the instance account.**
2. **Q2 — passthrough account, unset and block semantics.** Confirm passthrough = `runtime_target.service_account`. Unset: (a) → `service_account`, same as passthrough (today's behavior) [recommended]; (b) → block account (legacy parity; breaks flat instances without one). Block: required `block_service_account` (no-role), refused if not configured; flat ships block now rather than inheriting ptone/scion#7558's interim legacy refusal. **Status: RULED (b) unset → block account, 2026-11-14; passthrough/block readings confirmed by no objection.**
3. **Q3 — Hub prechecks without a report.** (a) no flat SA report; broker is authoritative; Hub decides native from target type [recommended]; (b) add target-level report (alt 2). **Status: RULED (a), 2026-11-14.**
4. **Q4 — ptone/scion#4895 scope.** (a) project-scoped by-broker default SA, assign only [recommended]; (b) also allow a per-broker default mode (block); (c) broker-global default; (d) record alternative remedy. **Status: RULED (a), 2026-11-16.**
5. **Q5 — passthrough authorization on flat Cloud Run.** The gate's actAs check targeted `broker.GCPHostServiceAccountEmail`, which on flat Cloud Run was not the account passthrough runs as. Options: (a) register `runtime_target.service_account` as the row's GCP host pair [recommended]; (b) skip the actAs check on flat Cloud Run (owner/admin only); (c) leave as is (wrong account). **Status: RULED (a) plus the provider-grant extension, 2026-11-16; design §B.4.5.** Correction made while scoping: the pair is serialized to broker readers, so flat Cloud Run display redacts it.

## B.8 Implementation phases (B)

| Phase | Scope | Size | Depends on |
|---|---|---|---|
| **I1** (vertical slice) | Flat Cloud Run **default path (unset/block)** end to end: shared CR2 SA-form validator (de-dup with converter) + both fields (`block_service_account` required, `assignable_service_accounts` validated but assign still refused) + FC4-a + P9/P13 over all agent accounts; broker policy snapshot + early/late decision for unset, block and confined passthrough → block account (assign and explicit passthrough still refused with the E-10 text); Hub `flatCloudRunBroker` native decision (marker, token withholding, precheck skip); start/restart consistency; fake-Instances lifecycle create→start→restart→stop as the block account; skew tests. **Converter touch (keeps C1's plan→strict-load round trip valid):** every converted `cloudrun` instance gets notice `flat_target_operator_input_required` at `runtime_target.block_service_account` until I4 folds the legacy value | M | Q1 ✔, Q2 ✔, Q3 ✔ |
| **I2** | **Assign** (allow-list, not-attachable 400 naming the setting, marker required) + explicit **passthrough** → `service_account`; identity-change recreate on restart | S–M | I1 validated |
| **I3** | ptone/scion#4895 by-broker default: settings PUT/GET + validation + both ladder paths + CLI `--broker` + Hub error attribution | M | Q4 ✔. Independent of I1 code (different Hub files); may run parallel to I1. Its Cloud Run end-to-end check waits for I1 |
| **I3w** | Web: project settings per-broker list + inherited-identity hint | S | I3 |
| **I4** | Converter folds `assignable_service_accounts` / `block_service_account` (replaces C1 transitional refusal; split instances on differing values). `block_service_account` is now CR2-required: a `cloudrun` entry without it → blocking `flat_target_field_required` at `runtimes.<r>.cloudrun.block_service_account` (has a legacy source); a `cloudrun-instances`-only instance → notice `flat_target_operator_input_required` (no legacy source, as A.3.4). Also retire/adjust the web `FINDING_TEXTS.field_not_valid_for_target_type` copy written for the transitional refusal (C2 review follow-up) and the CLI `findingTexts` / docs wording that mention ptone/scion#7764 | S | C1, I2 |
| **I5** | Docs (server-config, multi-broker, experiments note that `hub.cloudrun_native_identity` does not apply to flat), contract amendments in `.design/flat-runtime-brokers/`, doctor actAs with instance creds, live GCP identity E2E (assign, block, unset, not-attachable refusal, default rung; run in the Cloud Run lane's environment, as with C3) | S–M | I2, I3 |
| **I5a** (ptone/scion#8238) | §B.4.5: flat Cloud Run GCP host pair from `runtime_target.service_account` (embedded detected-style write with no host observation for cloudrun rows; remote `register --instance` sends the pair); display redaction of the pair for flat Cloud Run non-updaters; gate check 1 provider-grant path with `projectID` on all six surfaces; audit path label; tests per §B.9 Q5 rows | S–M (Hub gate + registration + display; no schema/migration) | I2. Must land before I5 Step B docs describe passthrough authorization; the I5 live E2E adds one passthrough case after it |


**Tracker:** I1 = ptone/scion#8182, I2 = ptone/scion#8183, I4 = ptone/scion#8186, I5 = ptone/scion#8187 (sub-issues of ptone/scion#7764); I3 = ptone/scion#8184, I3w = ptone/scion#8185 (sub-issues of ptone/scion#4895). I5a = ptone/scion#8238 (sub-issue of ptone/scion#7764). Blocked-by: I2←I1, I3w←I3, I4←I2, I5a←I2, I5←I2+I3+I5a (I5a blocks only I5's passthrough wording and passthrough E2E case).

Fan-out from I1 (I2, I4) is conditional on I1 being validated (tests + review + one live no-identity create running as the block account on GCP, in the Cloud Run lane's environment if available).

## B.9 Acceptance criteria (B)

- Strict load/schema/validator parity for both fields, incl. default/service-agent refusals, cross-type refusal, duplicates, overlap rules; admin PUT applies them.
- Activation refuses an instance whose principal or impersonation source equals any agent account, or whose agent accounts include the probed compute default; a sibling instance activates.
- `block_service_account` missing on a flat `cloudrun` instance → validation error naming the path (strict load and admin PUT).
- Assign with a listed account → instance created with that SA and `scion-identity` label; unlisted → 400 naming the setting path, no ownership record, no instance; without the native marker → 400.
- Hub: a flat Cloud Run agent never gets `ScopeAgentGCPToken`; `/agent/gcp-token` refused; `NativeCloudRun` persisted before dispatch and cleared on confirmed stop/delete; all with `hub.cloudrun_native_identity` off and on. Legacy profile behavior unchanged (existing tests).
- Unset, block and confined passthrough → run as the block account with `SCION_METADATA_MODE=block`; unset needs no marker, block does. Explicit passthrough → runs as `service_account`, no marker required.
- Start/restart: identity change recreates the instance; runtime/instance mismatch refuses (`rejectCloudRunIdentityRuntimeChange`).
- By-broker default: applies only to agents on that flat broker; explicit request wins; project default applies on other brokers; unverified/unauthorized SA refuses (no fallback); write refused when experiment off or key is not a flat broker; scheduled path matches interactive path.
- No SA email or the new fields appear in any presentation/public broker response (extend `TestFlat_CloudRunHubRecordsOmitCloudFields`).
- **Q5 (I5a):** co-located flat Cloud Run row's GCP host pair = `runtime_target.service_account` and project, refreshed on activation, admin-pinned value kept; no host observation recorded for cloudrun rows. Remote `register --instance` fills it under the existing actAs claim rule.
- **Q5:** broker GET/list for a reader without update shows no flat Cloud Run GCP host pair; owner/admin sees it; legacy rows unchanged.
- **Q5 gate matrix** (each surface: create, PATCH, project default, translation, review, move): owner + actAs → allow; admin + actAs → allow; non-owner project member + owner-consented provider link + actAs → allow (`provider_grant` in log); same without actAs → deny, no dispatch; non-owner + link with empty/placeholder `LinkedBy` → deny; flat row, consented link, AutoProvide off, non-owner without `broker.dispatch` → deny; same with AutoProvide on + actAs → allow; non-owner + no link → deny; legacy embedded broker + non-admin + link → deny (unchanged); flat Cloud Run passthrough checks actAs on `service_account`, not the Hub host account.
- `TestNewFlatInstancePolicy_CopiesEveryReferenceField` and `TestPresentInstance_EveryFieldHasAnFC4aRow` updated.
