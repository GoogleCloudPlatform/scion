# Flat Runtime Brokers P3: frozen contract appendix (FC1–FC11)

Status: **frozen once the P3.1-a change is accepted** (ptone/scion#3277, parent ptone/scion#3276). This appendix extends the P1 appendix (`.design/flat-runtime-brokers-contract.md`) and the P2 amendments in this directory. Where they disagree on a P3 subject, this document wins. Changing a frozen name, type, rule or code after acceptance requires another review of this document.

Base: upstream main `3a9e2aa33ba74835ba4e844746dfefce955d0494`. File references name a symbol rather than a line where possible.

Terms follow `GLOSSARY.md`: **Runtime Broker**, **flat Runtime Broker**, **Runtime Broker instance** (the in-process host of one flat identity), **legacy Runtime Broker** (profile-resolving, Hub-connected). Hubs are required: no contract here assumes a process without a Hub.

## 0. What P4 may rely on, and what is still open

The P4 work depends on eight points. Each is frozen here or deliberately deferred with a reason:

| P4 dependency | Contract |
|---|---|
| Instance settings schema | FC1, FC2 |
| What the Hub may see of an instance | FC4 (nothing new published in P3; the maximum publishable set is frozen) |
| Precedence of the new values | FC5 |
| Translating old profile requests | FC6 (`ProfileMap`) |
| Zero, false and unknown-key semantics | FC7 |
| Saved storage records and applied config | FC9 |
| Project-level overrides | FC8 (no new project rung; deferred) |
| Per-profile default GCP service account | FC10 (node-local; Hub default per instance deferred) |
| Capacity surfaces | FC11 |

**Delivery of the schema is phased.** FC1 freezes every field now. P3.1-a wires `runtime_target.shared_dir_storage_class` and `runtime_target.shared_dir_size`. The other FC1 fields are **reserved** until they are wired (after P3.1-b, only `agent_defaults` is): the strict instance loader refuses each of them with an explicit error naming its path ("reserved … not supported in this release"), never by silently dropping it. P3.1-b wires the remaining `runtime_target` fields and P3.1-c wires `agent_defaults`; each removes its keys from the reserved list in the same change that wires them. A reserved key never has a partial effect.

| Field group | Wired in |
|---|---|
| `runtime_target.shared_dir_storage_class`, `shared_dir_size` | P3.1-a |
| `runtime_target.gke`, `priority_class_name`, `safe_to_evict`, `shared_dir_storage_backend`, `shared_dir_storage_backends`, `home_storage_backend`, `home_storage_leaf` | P3.1-b |
| `agent_defaults.*` | P3.1-c |
| Conversion (FC6) and presentation (FC4-a) | P3.1-d |

## FC1. Instance settings schema

The instance entry stays `server.broker.instances[]` in the global `settings.yaml`. `schema_version` stays `"1"`. All additions are optional.

```yaml
server:
  broker:
    instances:
      - key: k8s-nfs-1                 # frozen in P1
        name: k8s-nfs-1                # frozen in P1
        runtime_target:
          # frozen in P1/P2
          type: kubernetes             # docker | kubernetes
          display_name: ""
          context: ""                  # kubernetes only
          namespace: ""                # kubernetes only
          kubeconfig: ""               # kubernetes only
          kubernetes_block_service_account: ""       # kubernetes only
          kubernetes_service_account_mappings: {}     # kubernetes only
          # P3 (frozen here)
          gke: false                   # kubernetes only
          priority_class_name: ""      # kubernetes only
          shared_dir_storage_class: "" # kubernetes only
          shared_dir_size: ""          # kubernetes only
          safe_to_evict: null          # kubernetes only; tri-state
          shared_dir_storage_backend: ""        # docker and kubernetes; local | nfs
          shared_dir_storage_backends: {}       # docker and kubernetes; dir name -> local | nfs
          home_storage_backend: ""     # kubernetes only
          home_storage_leaf: ""        # kubernetes only
        agent_defaults:                # P3 (frozen here)
          default_harness_config: ""
          image_registry: ""
          volumes: []
          resources: null
          harness_overrides: {}
          secrets: []
          clone_depth: ""
```

**Go types** (`pkg/config/settings_v1.go`). Field names and types are exactly those of the runtime entry (`V1RuntimeConfig`) and profile (`V1ProfileConfig`) fields they replace, so a legacy value converts by copy, never by reinterpretation.

```go
type V1RuntimeTargetConfig struct {
    Type, DisplayName, Context, Namespace, Kubeconfig string            // P1
    KubernetesBlockServiceAccount    string                              // P2
    KubernetesServiceAccountMappings map[string]string                   // P2
    GKE                      bool              `json:"gke,omitempty" ...`
    PriorityClassName        string            `json:"priority_class_name,omitempty" ...`
    SharedDirStorageClass    string            `json:"shared_dir_storage_class,omitempty" ...`
    SharedDirSize            string            `json:"shared_dir_size,omitempty" ...`
    SafeToEvict              *bool             `json:"safe_to_evict,omitempty" ...`
    SharedDirStorageBackend  string            `json:"shared_dir_storage_backend,omitempty" ...`
    SharedDirStorageBackends map[string]string `json:"shared_dir_storage_backends,omitempty" ...`
    HomeStorageBackend       string            `json:"home_storage_backend,omitempty" ...`
    HomeStorageLeaf          string            `json:"home_storage_leaf,omitempty" ...`
}

type V1RuntimeBrokerInstanceConfig struct {
    Key, Name     string
    RuntimeTarget *V1RuntimeTargetConfig
    AgentDefaults *V1InstanceAgentDefaults `json:"agent_defaults,omitempty" yaml:"agent_defaults,omitempty" koanf:"agent_defaults"`
}

type V1InstanceAgentDefaults struct {
    DefaultHarnessConfig string                       `json:"default_harness_config,omitempty" ...`
    ImageRegistry        string                       `json:"image_registry,omitempty" ...`
    Volumes              []api.VolumeMount            `json:"volumes,omitempty" ...`
    Resources            *api.ResourceSpec            `json:"resources,omitempty" ...`
    HarnessOverrides     map[string]V1HarnessOverride `json:"harness_overrides,omitempty" ...`
    Secrets              []api.RequiredSecret         `json:"secrets,omitempty" ...`
    CloneDepth           api.CloneDepth               `json:"clone_depth,omitempty" ...`
}
```

Every tag is `json`, `yaml` and `koanf` with the same snake_case name. Each Go field lands in the change that wires it (see section 0).

**Per-type validity** (`ValidateRuntimeBrokerInstances`). A Docker entry refuses `gke`, `priority_class_name`, `shared_dir_storage_class`, `shared_dir_size`, `safe_to_evict`, `home_storage_backend` and `home_storage_leaf` with `field not valid for runtime target type docker`, as it already refuses `context`, `namespace`, `kubeconfig` and the service-account fields. Values use the existing validators: `ValidateSharedDirSize` (a positive Kubernetes quantity), DNS-1123 subdomain for `priority_class_name`, the backend enum `local | nfs`, `api.CloneDepthFromValue`, and the harness-override validation used for profiles.

**JSON schema.** `$defs/runtimeBrokerInstance` and its `runtime_target` object keep `additionalProperties: false`. They gain the target fields above, and a new `$defs/instanceAgentDefaults` (also `additionalProperties: false`) is referenced from `agent_defaults`. The schema gains each property in the change that wires it, so `scion config validate` and the strict loader always accept the same keys.

## FC2. Server-config mirror and loaders

- `pkg/config/hub_config.go` `RuntimeTargetConfig` / `RuntimeBrokerInstanceConfig` mirror every FC1 field with camelCase `json`/`yaml`/`koanf` tags: `gke`, `priorityClassName`, `sharedDirStorageClass`, `sharedDirSize`, `safeToEvict`, `sharedDirStorageBackend`, `sharedDirStorageBackends`, `homeStorageBackend`, `homeStorageLeaf`, and `agentDefaults{defaultHarnessConfig, imageRegistry, volumes, resources, harnessOverrides, secrets, cloneDepth}`.
- `ConvertV1ServerToGlobalConfig` / `ConvertGlobalToV1ServerConfig` (through `v1InstancesToGlobal` / `globalInstancesToV1`) deep-copy each field: maps, slices and pointers are never shared between the two forms.
- The strict loader `LoadRuntimeBrokerInstances` is the only reader of instance policy. It refuses, each with an error naming the settings path: an unknown key on an instance entry or under `runtime_target` (and, once wired, under `agent_defaults`); a reserved key (section 0); a non-string scalar where a string is required; any per-type validity error. The startup deep-equal check between the strict result and the lenient server config (`loadServerRuntimeBrokerInstances`) covers the new fields automatically.

## FC3. Write paths and the file-only ownership boundary

- Instance entries are Layer-0 settings: the `server.broker` prefix in `pkg/config/opsettings/koanf.go` keeps them file-owned. A request never changes them in hosted database mode (P1 appendix section 2 (c)).
- The file-mode admin PUT (`pkg/hub/admin_settings_flat_instances.go`, strict decode) and the workstation database PUT (`pkg/hub/admin_settings_workstation.go`) validate the whole entry, including the new blocks, with `ValidateRuntimeBrokerInstances`, and keep the stored list when the body omits `server.broker.instances`.
- P3 opens no Hub editing of any instance value. Whether P4.1 later allows editing of `agent_defaults` is a P4.1 decision. `runtime_target` stays operator- and file-owned in every mode.

## FC4. Hub-visible fields

**Frozen: P3 publishes nothing new to the Hub.** The registration descriptor stays `api.RuntimeTargetDescriptor{id, type, displayName}` (`pkg/api/runtime_target.go`). P3 makes no registration, heartbeat or store change for instance policy.

P3 also freezes the **maximum publishable set**: the Go type `config.FlatInstancePresentation`, built only by `config.PresentInstance(inst V1RuntimeBrokerInstanceConfig)` (P3.1-d). Its fields are exactly the "published" column of FC4-a. Anything not listed there is never publishable, and a field added to FC1 later is never publishable until this table is amended. A redaction unit test covers every row. How and whether P4.1 transports the set is a P4 decision; the safety boundary is frozen here.

### FC4-a. Every field of an instance entry and its publishable form

| Source field | Published as | Reason |
|---|---|---|
| `key`, `name` | as is | labels, not credentials |
| `runtime_target.type`, `display_name` | as is | already in the descriptor |
| `runtime_target.namespace` | as is | non-sensitive display scope |
| `runtime_target.context` | **never** | a local kubeconfig alias, not a stable identity |
| `runtime_target.kubeconfig` | **never** | a local credential path |
| `runtime_target.kubernetes_service_account_mappings`, `kubernetes_block_service_account` | **never** | local operator identity policy |
| `runtime_target.gke` | as is (bool) | behaviour flag |
| `runtime_target.priority_class_name`, `shared_dir_storage_class`, `shared_dir_size` | as is | scheduling and storage defaults |
| `runtime_target.safe_to_evict` | as is, tri-state (unset / true / false) | scheduling default |
| `runtime_target.shared_dir_storage_backend`, `shared_dir_storage_backends` (dir name → `local`/`nfs`), `home_storage_backend` | as is | backend choice only; NFS server addresses live in `server.shared_dir_storage.nfs`, not in the instance |
| `runtime_target.home_storage_leaf` | **never** | a host path component |
| `agent_defaults.default_harness_config`, `image_registry`, `clone_depth` | as is | names and values without credentials |
| `agent_defaults.resources` | as is (requests / limits) | quantities |
| `agent_defaults.volumes[]` (`api.VolumeMount`) | **`target`, `read_only`, `type` only**; `source`, `bucket`, `prefix`, `server`, `mode`, `volume_name` **never** | host paths and storage locations are node-local layout |
| `agent_defaults.secrets[]` (`api.RequiredSecret`) | **`key`, `type`, `description` only**; `target`, `alternative_env_keys` **never** | declarations carry no value |
| `agent_defaults.harness_overrides.<hc>` | the `<hc>` name; `image`, `image_pull_policy`, `auth_selected_type`, `user` as is; `env` as **keys only** (values never); `volumes` as `target`/`read_only`/`type` only; `resources` as is | env values may hold credentials; host paths are node-local |

## FC5. Precedence statement

Normative: **each field keeps its own existing precedence chain, and the instance value occupies exactly the rung the profile → runtime-entry value occupied.** No new global chain is introduced. Hub `agent_defaults`, template and agent overrides, and global settings keep their positions.

| Field | Rung of the instance value (chain otherwise unchanged) |
|---|---|
| `shared_dir_storage_class`, `shared_dir_size` | below the template/agent `kubernetes.shared_dir_*` (settings fill only empty values, `ApplySharedDirDefaults`); unset → the runtime default (10Gi) |
| `safe_to_evict` | below the template `kubernetes.safeToEvict`, which wins whether true or false (`ApplySafeToEvictDefault`); it stays an overridable default, never mandatory policy |
| `priority_class_name` | below the template/agent `kubernetes.priorityClassName` |
| `gke` | consumed when the instance runtime is built |
| `shared_dir_storage_backend(s)` | between the agent's recorded backend (always kept) and `server.shared_dir_storage.backend`; flat order is instance per-dir, instance single, global |
| `home_storage_backend`, `home_storage_leaf` | above `server.home_storage`; first start only, the recorded decision is kept afterwards |
| `default_harness_config` | rung 6 of `ResolveHarnessConfigName`: below template and stored rungs, above top-level `default_harness_config` |
| `image_registry` | above top-level `image_registry` |
| `volumes` | appended after harness-config volumes, same order as the profile's |
| `resources` | own/template over instance, instance over Hub `agent_defaults.resources`, over `default_resources` |
| `harness_overrides.<hc>.*` | as the profile's: the explicit override image outranks the template image (ptone/scion#1799); `auth_selected_type` for the active harness config only; **empty-value env keys are collected from all harness overrides**, not only the active one |
| `harness_overrides.<hc>.resources` | merged **over** own/template resources merged over instance resources, so it ranks above the agent's own resources, exactly as the profile's did |
| `secrets` | step 3c of required-env extraction: between settings-level (3b) and template (3d) |
| `clone_depth` | below template/agent `clone_depth` |

**Fall-through.** The flat branch replaces only the profile/runtime-entry rung. When the instance field is unset, resolution continues to the same lower rungs the legacy path uses (for example `server.shared_dir_storage`, `server.home_storage`, top-level `image_registry`, the `harness_configs.<hc>` base, top-level `default_harness_config`); it never stops at "unset".

**Shadowing fold for the backend pair.** Today's order is profile per-dir, profile single, runtime per-dir, runtime single. A profile's single value therefore shadows the runtime entry's per-dir map. Conversion reproduces that (FC6 rule 2a); an independent per-key merge is wrong.

**Mechanism.** Frozen: the exported API named here (`FlatInstancePolicy`, `WithFlatInstancePolicy`, `FlatInstancePolicyFrom`, `ForFlatInstance`, the derived `ProfileResolutionFrom`, `ForProfileResolution` for the Hub) and the guard-test rule. Not frozen: unexported internals such as the view's policy field and the per-resolver branch layout, which a later refactor may change without amending this appendix as long as the frozen API and rules hold. The exported constructor `NewFlatInstancePolicy` also stays outside this freeze: the appendix freezes a minimum API, and the constructor may change as long as the policy it builds keeps the rules above. A request served by a flat instance carries a `config.FlatInstancePolicy` on its context (`config.WithFlatInstancePolicy`, read by `config.FlatInstancePolicyFrom`; nil means legacy). The policy is an immutable deep copy built once per instance server from its strictly loaded entry. `ProfileResolutionFrom(ctx)` returns `ProfileResolutionFlatInstance` exactly when a policy is present. `LoadEffectiveSettingsFor`, `LoadGlobalSettingsWithOverlayFor` and the Runtime Broker's `settingsView` return `vs.ForFlatInstance(policy)`: a view with no profiles and no active profile that carries the policy in an unexported field. Each resolver has a flat branch that reads only the typed policy field and never calls `ResolveProfileValue` or reads `Profiles` / `Runtimes`; a source-scan guard test in `pkg/config` enforces this. Source labels name the instance path, for example `server.broker.instances[k8s-nfs-1].runtime_target.shared_dir_size`. `ForProfileResolution(ProfileResolutionFlatInstance)` remains the policy-less view for the Hub's reincarnation planning, which never sees node-local policy.

## FC6. Conversion API and finding codes

Pure, deterministic, side-effect free (`pkg/config/profile_conversion.go`, P3.1-d):

```go
type ProfileConversionInput struct {
    Settings    *VersionedSettings  // effective legacy settings (global + Hub overlay); not a flat view
    RawSettings []byte              // raw YAML/JSON, for unknown-key and removed-key detection
    Profiles    []string            // profiles in scope (default: all)
    Choices     []ConversionChoice  // explicit operator choices from a reviewed manifest
}
type ConversionChoice struct {
    TargetGroup   string   // the proposed instance Key of the group (see below)
    Field         string   // a legacy settings path; empty for a Split choice
    ChosenProfile string   // or "template:<name>"; empty for a Split choice
    Split         []string // keep these profiles on separate same-scope instances; empty for a field choice
}
type ProfileConversionResult struct {
    Instances  []V1RuntimeBrokerInstanceConfig // one per target group; Key/Name proposed, operator-editable
    ProfileMap map[string]string               // profile -> instance key
    Findings   []ConversionFinding             // sorted by path
}
type ConversionFinding struct {
    Code     string   // frozen set below
    Severity string   // "blocking" | "notice"
    Path     string   // legacy settings path, e.g. "profiles.batch.shared_dir_size"
    Profiles []string
    Message  string   // actionable
}
func ConvertProfilesToInstances(in ProfileConversionInput) ProfileConversionResult
```

`ConversionChoice.TargetGroup` is the proposed instance `Key` of that group in `ProfileConversionResult.Instances`, derived deterministically from the resolved target scope, so the same input always yields the same key. A choice is one of two kinds: a **Split** choice sets `TargetGroup` and `Split` only (`Field` and `ChosenProfile` empty); a **field** choice sets `TargetGroup`, `Field` and `ChosenProfile` and leaves `Split` empty.

Rules:
1. **Group by resolved target scope** (type plus normalized context/namespace/kubeconfig for Kubernetes; the local daemon for Docker), not by entry name. Grouping is a proposal: `ConversionChoice.Split` keeps profiles on separate same-scope instances.
2. **Fold per profile first**: the profile value, else the runtime-entry value; maps per key (the service-account mappings).
   - 2a. **Shadowing fold for the backend pair**: effective single = profile single, else runtime single; effective per-dir map = the profile's entries plus the runtime entry's entries **only when the profile sets no single value**.
3. **Agree or conflict per group and field**: deep equality (`*bool` compares nil, false and true as distinct; lists are order-sensitive). Disagreement is a blocking `profile_conflict` naming the field, the profiles and their values, with the two operator dispositions: a chosen default plus explicit template or harness configuration, or intentionally separate instances. A matching `ConversionChoice` turns it into the notice `profile_conflict_resolved`.
4. **Never auto-merge, never generate templates.**
5. No-effect legacy fields (`runtimes.<r>.host`, `.env`, `.sync`, `profiles.<p>.default_template`) → notice `legacy_field_no_effect`. `profiles.<p>.env` in raw input → notice `legacy_field_removed`.
6. `list_all_namespaces: true` → blocking `flat_target_scope_violation`. A runtime type without a flat target (podman, container, cloudrun, cloudrun-instances, cloudrun-sandbox, substrate) → blocking `unsupported_flat_target_type`.
7. Unknown keys under `profiles.<p>` / `runtimes.<r>` → blocking `unknown_settings_key`.
8. Secret declarations are copied; no secret value is ever read, resolved or emitted, and the converter has no secret-backend dependency.

**Frozen finding codes:** `unsupported_flat_target_type`, `flat_target_scope_violation`, `profile_conflict`, `profile_conflict_resolved`, `legacy_field_no_effect`, `legacy_field_removed`, `unknown_settings_key`, `field_not_valid_for_target_type`.

`ProfileMap` is the explicit, unambiguous migration mapping that P4.1 uses to translate an old `profile` request field or `defaultProfile` preference. A profile with a blocking finding, or with no `ProfileMap` entry, has no translation; P4.1 returns its actionable error.

## FC7. Zero, false and unknown-key rules

| Field | Unset | Explicit values |
|---|---|---|
| `safe_to_evict` | `nil` | `false` and `true` are both preserved (pointer kept end to end) |
| `gke` | `false` = auto-detect | `true` forces GKE mode; an explicit `false` is not distinguishable from unset, as in legacy settings |
| strings | `""` | any non-empty value |
| `clone_depth` | `""` | `"full"` is an explicit value and is preserved |
| maps, lists | empty | non-empty |
| `resources` | `nil` | zero quantities inside a spec are merged as today |

- An unknown key on an instance entry, under `runtime_target` or under `agent_defaults` is a strict-load error naming its settings path. A reserved key (section 0) is a strict-load error naming its path and saying it is not supported in this release.
- Admin PUT presence: the existing raw-body rule applies to the whole `instances` list. An instance entry is replaced as a unit, so an omitted inner key means unset, not "keep"; there is no per-field merge inside an entry.
- Round trip: marshal → strict load → deep-equal.

## FC8. Project-level overrides on flat instances

Project settings never set instance values. The strict loader reads only global settings, so project `settings.yaml`, project Hub settings maps (`profiles`, `runtimes`, `activeProfile`; `hubclient.ProjectSettings`) and the `scion.io/active-profile` annotation cannot change an instance's `runtime_target` or `agent_defaults`, and they are not applied to pinned agents (P1 appendix section 10; P3.2 stops both create paths, the interactive create and the scheduled one, from writing a profile onto a pinned agent). Non-profile project-merged settings (for example top-level `default_harness_config`, `image_registry`, `harness_configs`) keep their existing lower rungs. **Deferred:** whether a project may carry new flat workload overrides. No such feature exists or is proposed in P3; it would be a new rung.

## FC9. Saved storage records and applied config

**Frozen as unchanged.** The `shared-dir-storage.json` and `home-storage.json` formats and semantics are untouched: a recorded backend is always kept, a settings change only warns, and instance policy affects only first-start resolution. P3 adds no instance-default provenance to `agents.applied_config` and no agent columns. The resolved source label appears only in Runtime Broker logs and errors.

## FC10. GCP service-account policy on flat Kubernetes instances

**Frozen:** flat service-account policy stays node-local in `runtime_target` (`kubernetes_block_service_account`, `kubernetes_service_account_mappings`; `pkg/runtimebroker/flat_k8s_identity.go`) and is never sent to the Hub (FC4-a "never"). The per-profile default service-account project annotation (`scion.io/default-gcp-identity-service-account-id-by-profile`) is not applied to pinned agents. **Deferred to P4.1:** a per-instance Hub-side default service account. It is a Hub settings feature with authorization implications, not a profile-default mapping, and has no source field in FC1.

## FC11. Capacity surfaces

The API stays as it is, plus one new error code:
- `max_agents_per_broker` is scoped per Runtime Broker ID, so each flat instance has its own limit; two instances on one host have two independent caps.
- `BrokerSettings.MaxAgents` (`pkg/store/models.go`): `nil` inherits, `0` is unlimited, `> 0` is a cap.
- The providers view reports `agentLimit`, `agentCount` and `agentLimitSource` (`pkg/hub/handlers_env_secrets.go`).
- **New:** the admin usage row gains a `scopeConflict` flag for a pinned agent whose active reservation's scope differs from its Runtime Broker. Such a reservation is reported (log, metric, flag) and never moved, released or backfilled (P3.3).
- Reservations on a confirmed-deleted Runtime Broker scope are released with an audit event, so usage shows no rows on deleted Runtime Brokers (P3.3).
- **New:** a typed 409 with code `broker_reservation_scope_conflict` when an agent that already holds capacity on one Runtime Broker is admitted on another ("agent <id> already holds capacity on Runtime Broker <A>; it cannot be admitted on <B>"). The existing reservation is kept and no new one is made. Store failures stay 500 (P3.3).
- There are no profile rows and no host-wide or profile capacity scopes.

## Not frozen

P4 is free to choose: the Hub transport of FC4, web and CLI presentation, flat-awareness of `/api/v1/info`, health-summary and GCP service-account status display of the runtime target, and the `hub.*` → `server.broker.*` key move. Sequence P4 settings-file edits after the P3.1 schema changes merge, to avoid schema conflicts.
