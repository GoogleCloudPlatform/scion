# P5-CR Cloud Run flat target contract amendment

Decision date: 2026-11-04. Tracking issue: ptone/scion#7308. This amendment adds `cloudrun` as a flat Runtime Broker target type, first for co-located hosts only. It amends only the named rules of the P1 contract (`.design/flat-runtime-brokers-contract.md`) and of appendix-p3 (`contract-appendix-p3.md`), and the named frozen test expectations. Where this file and the frozen text differ on a Cloud Run subject, this file is authoritative; the frozen text carries a pointer here. No production code is supplied here. Delivery of each implementation phase includes this amendment.

Terms follow `GLOSSARY.md`. The CR items are numbered as in the design; CR3a precedes CR3 because it amends section 4, which CR3 depends on.

## Amendments to the P1 contract

**CR1: §2 frozen Go shapes.** `V1RuntimeTargetConfig` and `RuntimeTargetConfig` each gain `ProjectID`, `Region`, `ServiceAccount`, `Network`, `Subnetwork`, `CredentialsFile` (string), with keys `project_id`/`projectId`, `region`/`region`, `service_account`/`serviceAccount`, `network`/`network`, `subnetwork`/`subnetwork`, `credentials_file`/`credentialsFile` (json/yaml/koanf, omitempty). Both conversion directions, strict loading, the startup deep-equal mapping and the schema preserve them. They are Cloud-Run-only. `credentials_file` is a required field for `cloudrun`, not a reserved key.

**CR2: §2 validation.** `runtime_target.type` accepts `docker`, `kubernetes` and `cloudrun`. For `cloudrun`, the following are required:
- `project_id`, in GCP project ID form. A domain-scoped project ID (`example.com:proj`) is refused with its own message: "domain-scoped project IDs are not supported";
- `region`;
- `service_account`, a service-account email. The project default service accounts (`<number>-compute@developer.gserviceaccount.com`, and `<project_id>@appspot.gserviceaccount.com`) are refused: "the project default service accounts are not allowed";
- at least one of `network`/`subnetwork`;
- `credentials_file`, one absolute local file path with no tilde, environment expansion or path list.

Both refusals compare case-insensitively: the compute-default form `^[0-9]+-compute@developer\.gserviceaccount\.com$` and the equality with `<project_id>@appspot.gserviceaccount.com`. P13's comparison with the probed number is case-insensitive too.

Kubernetes-only fields are refused for `cloudrun` ("field not valid for runtime target type cloudrun"), and the six Cloud Run fields are refused for `docker` and `kubernetes`. Any other type is "unsupported runtime target type "x" (supported: docker, kubernetes, cloudrun)".

All of these form rules live in `ValidateRuntimeBrokerInstances` itself, so every write path that validates instance settings applies them, the Hub admin server-config PUT included. The schema and the validator agree on every case. The credentials file's existence, type, owner, mode and parse are activation checks on the host (P6, P7), not static validation.

**CR3a: §4 frozen Go types and file shape.** `ExecutionScope` gains `CloudRun *CloudRunScope` (JSON `cloudrun`, omitempty). `type CloudRunScope struct { ProjectNumber, Region, ProjectID string }`, with JSON keys `projectNumber`, `region`, `projectId` (`projectId` omitempty). `schemaVersion` stays 1. `ProjectID` is informational and keeps its first-boot value: the file has no mutable fields. An older binary refuses `type: cloudrun` at config validation, and if it reads such a file it reports `ErrIdentityCorrupt` (never re-minted). The directory must not be removed after a rollback.

**CR3: §5 Cloud Run (new subsection after Kubernetes).**
- Record: `{ "type": "cloudrun", "cloudrun": { "projectNumber": <Resource Manager project number>, "region": <configured region>, "projectId": <configured project ID, informational> } }`.
- **Identity fields: `projectNumber` + `region`. The normative scope identity string is `cloudrun:<projectNumber>/<region>`.** The host's scope grouping key is derived from the same function.
- Excluded from identity: project ID, service account, network, subnetwork, credentials-file path and credentials. Rotating credentials or changing the runtime service account or network keeps the target. Configuring another project or region is refused like a daemon change (`ErrExecutionScopeChanged`).
- Cloud Run Admin operations use the probed project number as the parent project. Other clients may use the configured project ID, because GCP project IDs are globally unique and never reused.
- Credentials:
  - `runtime_target.credentials_file` is **required**. It is the sole credential source, with no fallback to Application Default Credentials or the metadata server.
  - Accepted credential types: `service_account` and `impersonated_service_account` only. Every other type is refused, including any external-account credential source.
  - The file must be a regular file, not a symlink, owned by the service user, with mode 0600 or 0400, checked on the opened handle.
  - The credential bytes that are parsed are read from the same opened handle that passed the file checks; the file is never re-opened by path between the check and the parse.
  - Process environment is never mutated. The same credentials serve the probe and all operations. No proactive polling.
- **Activation order.** Each step refuses only that instance, before registration, heartbeat or control channel:
  1. **Local preconditions, before the runtime is built** (no cloud call):
     - the host is co-located;
     - the host process is not itself a Cloud Run service;
     - the effective `server.project_storage` backend is `nfs`, and `server.project_storage.homes` is `nfs`;
     - the agent-facing hub endpoint is an absolute https URL whose host is neither loopback nor link-local, and whose source is not an IAP audience;
     - the credentials file passes the file checks.
  2. **Credential checks, after construction and before the probe:**
     - the credential type is accepted;
     - the instance principal resolves (otherwise refused);
     - `service_account` differs from the principal;
     - no earlier `cloudrun` instance of this host process, in configuration order, has the same principal (the later instance is refused);
     - the principal differs from the hub process's own ADC principal where that can be determined.
  3. **Probe**, before every activation including restart, with the instance's own credentials: read the project (number and ACTIVE state), then list Cloud Run instances in the region. A denial, a missing object, an inactive project, an empty number, an invalid region or a connectivity failure yields `ErrExecutionScopeUnidentified`. There is no fallback identity. Diagnostics distinguish denial, absence, inactive project and connectivity, and never print credential contents or the credentials-file path.
  4. `service_account` must not be the probed project's default compute account.
  5. Identity load or mint and `VerifyScope`, then the ownership preflight.
- Several `cloudrun` instances may share a scope, and they must have distinct principals.
- Identity IDs remain minted (§4) and are never derived from project or region, including the legacy stateless Cloud Run broker's derived ID.
- Every path that builds a `cloudrun` runtime or mints a `cloudrun` identity runs the same shared precheck: the host's per-instance preparation and `scion runtime-broker register --instance`. A refused path mints nothing: no identity directory and no Hub row.

**Activation checks and health reason codes.** The reason codes are not frozen. They are listed so that tests can name them; the order and the stage of each check are part of this amendment.

| Order | Stage | Check | Health reason code |
|---|---|---|---|
| P1 | precondition, before the runtime is built | the host is co-located | `cloudrun_remote_unsupported` |
| P2 | precondition | the process is not a Cloud Run service (`K_SERVICE`, read by exact name, is empty) | `cloudrun_host_on_cloudrun` |
| P3 | precondition | the effective `server.project_storage` backend is `nfs` | `cloudrun_storage_backend` |
| P4 | precondition | `server.project_storage.homes` is `nfs` | `cloudrun_homes_placement` |
| P5 | precondition | the agent hub endpoint, resolved through the same function that produces the value applied to the instance, is an absolute https URL whose host is neither loopback nor link-local, and whose source is not the IAP audience | `cloudrun_hub_endpoint` |
| P6 | precondition | `credentials_file` opens without following a symlink, and the opened handle is a regular file owned by the service user with mode 0600 or 0400 (checked on the handle, never by a path stat). The precheck hands that opened handle, or the bytes read from it, to the runtime factory | `cloudrun_credentials_file` |
| P7 | runtime factory | the credential type is `service_account` or `impersonated_service_account`, decided before the auth library acts on the file. The factory parses the bytes from the handle that passed P6 and never opens the file by path | `cloudrun_credentials_type` |
| P8 | runtime factory | the instance principal resolves: `client_email` for a key, the impersonation target for an impersonated credential | `cloudrun_principal_unresolved` |
| P9 | runtime factory | `service_account` differs from the instance principal | `cloudrun_principal_is_agent_sa` |
| P10 | runtime factory, host principal set | no earlier `cloudrun` instance of this host, in configuration order, has the same principal; the later one is refused | `cloudrun_principal_shared` |
| P11 | runtime factory | the principal differs from the hub process's own ADC principal, where that can be determined; an undeterminable hub principal skips this check and logs it at info | `cloudrun_principal_is_hub` |
| P12 | probe | the scope probe | the unidentified-scope path |
| P13 | after the probe | `service_account` is not `<probed number>-compute@developer.gserviceaccount.com` | `cloudrun_default_sa` |
| P14 | identity | `LoadOrCreate` and `VerifyScope` | the identity error codes |
| P15 | ownership | the ownership preflight (CR6) | `ownership_unresolved` |

The numbers P1 to P15 name activation checks only; they are not delivery phases.

P1 to P6 make no cloud call and no metadata call: credentials are not parsed and the runtime is not built until they pass.

**CR4: §6.** "The scope record, endpoint, kubeconfig path and credentials are never sent" also covers `project_id`, `service_account`, `network`, `subnetwork` and `credentials_file`. The descriptor stays `{id, type, displayName}`.

**CR5: §14.** A second server-layer experiment, `hub.flat_cloudrun_target` (`experiments.FlatCloudRunTarget`), Default false, Stage alpha, Owner `runtime-broker`.
- With it off, a new flat registration whose target type is `cloudrun`, and agent create on a flat Runtime Broker whose stored target type is `cloudrun`, are refused with `experiment_disabled` naming it.
- It requires `hub.flat_runtime_brokers`. It is not exposed through `/api/v1/settings/public`, and it is not in `dispatchExperimentNames`.
- **GA gate:** it may not leave alpha or become default-on until agent secret delivery stops placing secrets as plain values in the Cloud Run Instance definition.

**CR6: ownership labels.**
- **Keys.** Every flat-path read of an ownership or identity label from runtime inventory uses the runtime's stored label key (`runtime.LabelKey`).
- **Values.** Every ownership value stored on Cloud Run is sanitization-invariant by construction:
  - lower-case UUIDs for the owner, agent and project IDs;
  - slugs of at most `api.MaxSlugLength` (63) characters in `[a-z0-9-]`, starting and ending alphanumeric (`pkg/api/slug.go`).
- At create, a launch whose owner, project, agent or name value the label rewrite would change is refused (`checkOwnerLabel`, `pkg/runtime/owner_label.go`).
- At read, a stored value that differs from the record, including by sanitization or truncation, is **unresolved and never attributed**. No fuzzy or sanitized matching is permitted.
- An unlabeled agent object on a Cloud Run scope (project, region), including one created by a legacy Runtime Broker or another Hub, makes ownership on that scope unresolved, and every configured instance on that scope is refused, as for other targets.
- Ownership labels are not a security boundary: anyone who can update Cloud Run instances in the project can rewrite them. Only operators may hold that right.

## Amendments to appendix-p3

**CR7: FC1.** `runtime_target` admits the CR1 fields for `type: cloudrun` (`# cloudrun only`).

**CR8: FC4-a rows (new).**

| Field | Publishable form | Why |
|---|---|---|
| `runtime_target.region` | as is | non-sensitive display scope |
| `runtime_target.project_id` | **never** (if needed later: hub admins only, by amendment) | cloud account identifier |
| `runtime_target.service_account` | **never** | local operator identity policy |
| `runtime_target.network`, `subnetwork` | **never** | infrastructure topology |
| `runtime_target.credentials_file` | **never** | a local credential path |

**CR9: FC6 rule 1.** "Group by resolved target scope (… ; for Cloud Run, type plus `project_id` and `region`)". Errata instance keys: `cloudrun` plus project and region, under the same key-safety and suffix rules.

**CR10: FC6 rule 6 (replacement text; the existing podman/container/sandbox/substrate sentence is kept verbatim).** "`list_all_namespaces: true` → blocking `flat_target_scope_violation`. A runtime type without a flat target (podman, container, cloudrun-sandbox, substrate) → blocking `unsupported_flat_target_type`, as is a `cloudrun_sandbox` or `substrate` block on any runtime entry. Runtime types `cloudrun` and `cloudrun-instances` convert to a `cloudrun` instance: from the `cloudrun` block `project_id`, `location` (as `region`), `service_account`, `network`, `subnetwork`, and from the `cloudrun_instances` block `project_id`, `region`. A `cloudrun` or `cloudrun_instances` block on an entry of another type → notice `field_not_valid_for_target_type`. `cloudrun.nfs_server` and `cloudrun.nfs_export` → notice `legacy_field_no_effect`. A converted `cloudrun` instance missing a field that CR2 requires → blocking `flat_target_field_required` at that field's legacy path. The instance is still proposed, and its profiles get no `ProfileMap` entry. `credentials_file` has no legacy source, so every converted `cloudrun` instance carries the notice `flat_target_operator_input_required` at `runtime_target.credentials_file`; the profile keeps its `ProfileMap` entry. The operator must add the field before the instance loads. A later required field with no legacy source carries the same notice at its path. A missing field with a legacy source is blocking, as above."

**CR11: frozen finding codes.** Add `flat_target_field_required` and `flat_target_operator_input_required`.
- `flat_target_field_required` is always blocking. It covers a required field that has a legacy source but is missing, and the affected profiles get no `ProfileMap` entry.
- `flat_target_operator_input_required` is always a notice. It covers a required field with no legacy source, and the profile keeps its `ProfileMap` entry.
- `convert --plan` treats `flat_target_operator_input_required` as "operator decision required": exit 3, with the summary naming the field.

## Required acceptance coverage

These are frozen-test expectations for the implementation phases.

- Replace the "unsupported type" expectation for `cloudrun` with `TestRuntimeBrokerInstances_CloudRunAccepted`.
- Every named failure test below exists, including:
  - the `credentials_file` refusals: missing, symlink, wrong mode or owner, unaccepted type, executable source;
  - the principal refusals: unresolvable, equal to `service_account`, shared by two instances, equal to the hub's ADC principal;
  - the default-account refusals, static and probed;
  - the label-value tests;
  - the `ScopeKey`/`ScopeIdentity` parity test;
  - the https-only endpoint refusals.
- No test calls a cloud API. Real constructor coverage builds the per-instance credentials from fixture credential files.

### Named failure and fail-closed tests

| Test | Package | Asserts |
|---|---|---|
| `TestRuntimeBrokerInstances_CloudRunRequiresProjectRegionSA` | config | each missing or invalid required field gives its path-named error |
| `TestRuntimeBrokerInstances_CloudRunRequiresNetwork` | config | both network fields empty → error |
| `TestRuntimeBrokerInstances_CloudRunRejectsKubernetesFields` / `_DockerAndKubernetesRejectCloudRunFields` | config | cross-type refusals for every field |
| `TestRuntimeBrokerInstances_CloudRunCredentialsFileRequired` / `_CloudRunCredentialsFileAbsolutePath` | config | missing → required error; relative, `~`, env-syntax or list → path-form error |
| `TestRuntimeBrokerInstances_CloudRunRefusesDefaultServiceAccounts` | config | both `<n>-compute@developer.gserviceaccount.com` and `<project_id>@appspot.gserviceaccount.com` → "project default service accounts are not allowed"; mixed-case variants of both forms are refused |
| `TestRuntimeBrokerInstances_CloudRunRefusesDomainScopedProject` | config | `example.com:proj` → its own message |
| `TestRuntimeBrokerInstances_CloudRunAccepted` | config | replaces the unsupported-type expectation for cloudrun |
| `TestRuntimeBrokerInstances_UnsupportedTypeMessage` | config | the default message names "(supported: docker, kubernetes, cloudrun)" |
| `TestSettingsSchema_CloudRunParity` | config | schema and validator agree on all cases above |
| `TestAdminServerConfigPut_CloudRunValidation` | hub (shard) | the admin PUT refuses a cloudrun entry missing `credentials_file`, with a relative path, or with a default service account |
| `TestLoadOrCreate_CloudRunMintAndReload` / `_CloudRunScopeChangedRefuses` / `_CloudRunProjectIDAliasAccepted` | brokeridentity | mint; different number or region → `ErrExecutionScopeChanged`; same number with a different ID → accepted |
| `TestScopeIdentity_CloudRunEmptyNumberUnidentified` | brokeridentity | empty number or region → `ErrExecutionScopeUnidentified` |
| `TestScopeKey_CloudRunDistinctPerProjectRegion` | brokerhost | two projects, or two regions, give different keys |
| `TestScopeKey_DelegatesToScopeIdentity` | brokerhost | for docker, kubernetes and cloudrun scopes, `ScopeKey(s) == brokeridentity.ScopeIdentity(s)`, and the fallback applies only on error |
| `TestHostPrepare_PreconditionRunsBeforeNewRuntime` | brokerhost | a refusing precondition means `NewRuntime` and `ProbeScope` are never called, and the reason code reaches Status |
| `TestProbeCloudRunScope_Forbidden`, `_NotFound`, `_InactiveProject`, `_RegionInvalid`, `_ListForbidden`, `_Transport` | cmd | each → unidentified with a distinct diagnostic. No registration, heartbeat or control channel started. Message contains no credential path |
| `TestFlatHost_CloudRunRefusedInRemoteMode` | cmd | P1. A sibling docker instance activates |
| `TestFlatHost_CloudRunRefusedOnCloudRunService` | cmd | `K_SERVICE` set → refused before any cloud call (the fake client records zero calls) |
| `TestFlatHost_CloudRunRefusedNonNFSBackend` / `_RefusedHomesNotNFS` | cmd | P3 and P4 |
| `TestFlatHost_CloudRunRefusedLoopbackHubEndpoint` / `_RefusedIAPDerivedEndpoint` / `_RefusedHTTPHubEndpoint` | cmd | P5: loopback, link-local, IAP-derived and plain-http public endpoints are each refused |
| `TestFlatHost_CloudRunHubEndpointForms` | cmd | P5 refuses link-local IPv4 (169.254.0.0/16) and IPv6 (fe80::/10), a non-absolute URL, a non-http(s) scheme, plain http, loopback and an IAP-derived source |
| `TestFlatHost_CloudRunPreconditionsNoCloudCall` (table over P1 to P6) | cmd | for each refusal: no `NewRuntime`, no credential read, zero fake-client and metadata calls, the expected health reason code, and a docker sibling activates |
| `TestFlatHost_CloudRunProbeFailureSiblingActivates` | cmd | for each probe failure class a docker sibling activates |
| `TestFlatCloudRunHubEndpoint_NotAgentInfluenced` | runtimebroker | request-level HubEndpoint, resolved env and labels cannot change a flat cloudrun agent's hub URL |
| `TestHubEnv_FlatCloudRunNeverDerivesKService` | runtimebroker | flat + loopback → refusal, not a `K_SERVICE` URL |
| `TestFlatCloudRunCredentials_FileChecks` | cmd | symlink, mode 0644, non-regular file and wrong owner are each refused in the precondition, before any parse. Checked on the opened handle (Windows arm per `docs-repo/agents/windows-platform-seams.md`) |
| `TestFlatCloudRunCredentials_ParsedFromCheckedHandle` | cmd | a file swapped at the path after P6 is not the one parsed: the original handle's bytes are used, or the instance is refused. It never parses the swapped file |
| `TestFlatCloudRunCredentials_TypeRefused` | cmd/runtime | `authorized_user`, `external_account` (including an `executable` credential source, which must never be executed: a sentinel program records no run) and `external_account_authorized_user` are each refused; `service_account` and `impersonated_service_account` are accepted |
| `TestFlatCloudRunCredentials_ExplicitFileNoADCFallback` | cmd/runtime | with a malformed or unsupported file plus usable ADC available, the instance is refused and the ADC detector is never invoked. A sibling instance still activates |
| `TestFlatCloudRunCredentials_PerInstanceIsolation` | cmd/runtime | two instances with two fixture files give two distinct client credential objects. Process env is not modified |
| `TestFlatCloudRunPrincipal_Unresolvable` / `_EqualsServiceAccount` / `_ProbedComputeDefault` | cmd | P8, P9, P13 refusals |
| `TestFlatCloudRunPrincipal_SharedByTwoInstances` | cmd | two instances with the same principal: the later one in configuration order is refused, and the earlier one activates |
| `TestFlatCloudRunPrincipal_EqualsHubADC` | cmd | a determinable hub ADC principal equal to the instance principal → refused. An undeterminable hub principal → check skipped and logged |
| `TestRegisterInstance_CloudRunRefusedByPrecheck` | cmd | `register --instance` for a cloudrun instance is refused by the shared precheck: no identity directory, no hub call |
| `TestRegisterInstance_DockerRefusedByDefaultVolumeCheck` | cmd | a docker instance the host would refuse for its default volumes mints nothing |
| `TestFlatOwnershipPreflight_CloudRunSanitizedLabels` | cmd | an owned instance (sanitized keys) reconstructs. Another instance's object is ignored. An unlabeled object refuses |
| `TestFlatOwnershipPreflight_LegacyCloudRunAgentRefusesScope` | cmd | an unlabeled legacy instance on the same project and region refuses every instance on that scope, and an instance on another region still activates |
| `TestOwnershipReconstruct_UsesRuntimeLabelKey` | runtimebroker | ownership reads use `runtime.LabelKey` |
| `TestOwnershipReconstruct_CloudRunSlugAtBound` | runtimebroker | a 63-character canonical slug round-trips through sanitization and reconstructs |
| `TestOwnershipReconstruct_SanitizedValueUnresolved` | runtimebroker | a stored value that differs from the record only by sanitization or truncation is unresolved, never attributed |
| `TestOwnershipReconstruct_CloudRunHandleMatchesLaunch` / `TestReconcileAbsent_CloudRunListed` / `_CloudRunUnlisted` | runtimebroker | reconstructing a launched object adds no resource; `cloudrun_instance` counts as a main object |
| `TestFlatCloudRunCreate_LongProjectIDRefused` | runtime/agent | a `<uuid>__<slug>` project ID value is refused by `checkOwnerLabel` before provisioning. Hub-dispatched flat creates carry the UUID form |
| `TestFlatServer_CloudRunHasNoOwnershipSetupErr` | runtimebroker | a flat cloudrun server has no ownership setup error |
| `TestCloudRunResourceAbsent_NotFound` / `_RunIDMismatchAbsent` / `_SoftDeletedPresent` / `_TransportErrorIsError` / `_WrongKindIsError` | runtime | absence is decided by the run-ID label against the handle UID, never by the name alone |
| `TestFlatCloudRunRuntime_NoMetadataFallback` | runtime | the constructor refuses empty project or region. Config resolution never calls metadata |
| `TestFlatCloudRunRuntime_UsesShareNFSOnly` | runtime | the NFS target is always the share. No runtime override exists |
| `TestFlatCloudRun_HubRecordsOmitCloudFields` | hub/runtimebroker | registration, join, heartbeat, presentation, inventory, saved placement and identity JSON contain none of project_id, service account, network, subnetwork, credentials path |
| `TestFlatCloudRun_CredentialContentsNeverInAPI` | hub/runtimebroker | with a fixture credentials file, no API response body (admin server-config GET, broker, presentation, heartbeat) contains its contents; only the admin GET may show the path |
| `TestRegisterFlat_CloudRunExperimentOff` / `TestCreateAgent_FlatCloudRunExperimentOff` | hub | 412 `experiment_disabled` with the new experiment name. Docker and Kubernetes registration still succeed. An existing cloudrun row re-registers |
| `TestProfileConversion_CloudRunMapped`, `_CloudRunInstancesFieldRequired`, `_CloudRunSandboxStillBlocking`, `_CloudRunBlockOnDockerNotice`, `_CloudRunNFSFieldsNoEffect`, `_CloudRunSplitBySA` | config | the CR9 and CR10 rules and `flat_target_field_required` (blocking, no `ProfileMap` entry) |
| `TestProfileConversion_CloudRunCredentialsFileNotice` | config | `flat_target_operator_input_required` as a notice at `runtime_target.credentials_file`, and a kept `ProfileMap` entry |
| a `convert --plan` test | cmd | exit 3 for a plan whose only finding is `flat_target_operator_input_required`, with the summary naming the field |
| `TestPresentInstance_CloudRunRedaction` | config | the FC4-a rows of CR8 |
| `TestDoctor_FlatCloudRunNoHomesWarning` | cmd | the homes warning is skipped for a flat cloudrun instance |

### Positive and parity tests

- Round trip: marshal, strict load, deep-equal, for a full cloudrun entry, in both config families.
- Hub GCP passthrough stays denied for a flat cloudrun Runtime Broker (the existing test gains the cloudrun case).
- Full lifecycle through a flat cloudrun instance with a fake Instances client: create, pin, start, stop, restart, delete, a mismatched `expectedRuntimeTargetId` → 409, and a missing one → 412.

No other frozen expectation is implicitly waived.
