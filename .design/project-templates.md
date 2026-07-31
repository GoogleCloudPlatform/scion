# Project Templates: Settings Fallback UX & Project Clone

**Status:** Ready for implementation — all blocking questions resolved
**Author:** pt-arch (architect, project-templates workstream)
**Date:** 2026-07-28
**Implements:** #380 (project settings fallback UX), #377 / #300 (project clone)
**Explicitly deferred:** #381 (hub-level project defaults copy-on-create)
**Handoff target:** project-templates-lead (EM) for phase dispatch
**Reviewed:** all code claims verified against the tree at 8dbf167; corrections applied

---

## 1. Problem & Goals

Two related but independent pain points sit at the "how do I get a new project
configured the way I want" boundary.

### 1.1 Feature A — Project settings fallback is invisible

`GET /api/v1/projects/{id}/settings` returns only the values explicitly set on
the project (stored as `scion.io/*` annotations on `store.Project`). Every field
is optional. When a field is unset, the settings page renders an empty control
with a generic placeholder such as `None (use server default)` or `No limit`.

The user has no way to learn what "server default" actually resolves to. They
cannot answer the question *"if I leave this blank, what will my agents get?"*
without asking an administrator, because the only surface that exposes hub-level
agent defaults today is `GET /api/v1/admin/server-config`, which is hard-gated to
`user.Role() == "admin"` (`pkg/hub/admin_settings.go`). Project owners are
routinely not hub admins.

There is exactly one field where we already solved this, and it demonstrates the
target UX. Telemetry has a `GET /api/v1/settings/public` endpoint
(`pkg/hub/handlers_public_settings.go`) whose single value drives a select
option rendered as:

```
Use hub default (enabled)
```

(Aside, since it misleads on inspection: despite the name and a stale
`// no auth required` comment at its registration, `/api/v1/settings/public` is
**not** unauthenticated. It sits behind `UnifiedAuthMiddleware` and is absent
from `isUnauthenticatedEndpoint`, so an anonymous request gets 401. The handler
itself performs no identity check, which is where the belief comes from. This
matters only for §10's retention rationale.)

**Goal:** generalise that pattern to the rest of the agent-defaults fields, so
that an unset project setting displays the hub fallback that will actually be
used.

### 1.2 Feature B — No way to seed a new project from an existing one

Configuring a project is a long tail of small decisions: default harness config,
model, thinking level, turn/duration/call limits, resource requests and limits,
GCP identity mode, a pre-start hook script, project-scoped environment
variables, and an injected-skills list. A team that has tuned one project has no
mechanism to reuse that work. The only path is to open both projects side by
side and retype every field.

Templates and harness configs already have a first-class clone
(`POST /api/v1/templates/{id}/clone`, `POST /api/v1/harness-configs/{id}/clone`).
Projects do not.

**Goal:** `POST /api/v1/projects/{id}/clone` creates a new project pre-populated
with the source project's *configuration*, with no runtime state, no secrets, and
no workspace history.

### 1.3 Why one design doc

The two features share a mental model ("where does this value come from?"), share
the annotation-based project settings data model, and share the project-settings
UI surface. Designing them together avoids a second pass over
`project_settings_handlers.go` and lets Feature B's clone list be defined
directly against the resolved-settings vocabulary Feature A introduces. They are
implemented in **sequential phases**, not together.

---

## 2. Non-Goals

1. **#381 hub-level project defaults copy-on-create is out of scope.** We are not
   adding a mechanism that stamps hub settings onto a project's annotations at
   creation time. Deferred by explicit decision. The fallback stays dynamic: an
   unset project field continues to resolve at agent-create time, not at
   project-create time. Feature A makes that resolution *visible*, not *frozen*.
2. **No tri-state control.** We are not introducing an
   inherit / override / explicitly-null tri-state checkbox per field. Unset stays
   unset; the hub value is presented as a hint next to an empty control.
3. **No chat integration cloning.** Telegram and Discord integration
   configuration is hub-level (`integration_configs` has no `project_id`) and its
   credentials are hub-scoped secrets. Project-level chat state lives in the
   plugin's own database (e.g. `extras/scion-telegram` `GroupLink`), outside the
   hub's transactional reach. Integrations are excluded from clone entirely.
4. **No secret cloning.** Secret values are never returned by `ListSecrets`, and
   for external backends (GCP Secret Manager) the value does not exist in the hub
   database at all. Cloning secret *names* without values would create a project
   that looks configured but fails at runtime, which is worse than an empty list.
5. **No agent, message, or history cloning.** A clone is a configuration seed, not
   a fork of activity.
6. **No workspace content cloning.** The new project runs the standard workspace
   initialisation path for its workspace mode, from the same git remote.
7. **No broker-side resolution changes.** The `harness_config` precedence fix in
   §7.2 is confined to the hub; `pkg/config/resolve_harness_config.go` is not
   touched. See §7.2 for why that is sufficient.

---

## 3. Background: how a project setting actually reaches an agent

This section is load-bearing for Feature A. It is written out because the
resolution chain is not obvious from any single file and two reviewers
independently guessed it wrong.

### 3.1 Project settings storage

Project settings are annotations on `store.Project.Annotations`, keyed by
constants at the top of `pkg/hub/project_settings_handlers.go`:

| Annotation key | Settings field |
| --- | --- |
| `scion.io/default-template` | `defaultTemplate` |
| `scion.io/default-harness-config` | `defaultHarnessConfig` |
| `scion.io/default-model` | `defaultModel` |
| `scion.io/default-thinking-level` | `defaultThinkingLevel` |
| `scion.io/telemetry-enabled` | `telemetryEnabled` |
| `scion.io/active-profile` | `activeProfile` |
| `scion.io/default-max-turns` | `defaultMaxTurns` |
| `scion.io/default-max-model-calls` | `defaultMaxModelCalls` |
| `scion.io/default-max-duration` | `defaultMaxDuration` |
| `scion.io/default-gcp-identity-mode` | `defaultGcpIdentityMode` |
| `scion.io/default-gcp-identity-service-account-id` | `defaultGcpIdentityServiceAccountId` |
| `scion.io/default-resources-cpu-request` | `defaultResources.cpuRequest` |
| `scion.io/default-resources-memory-request` | `defaultResources.memoryRequest` |
| `scion.io/default-resources-cpu-limit` | `defaultResources.cpuLimit` |
| `scion.io/default-resources-memory-limit` | `defaultResources.memoryLimit` |
| `scion.io/default-resources-disk` | `defaultResources.disk` |

`setOrDelete` / `setOrDeleteInt` mean an empty or zero value *removes* the
annotation rather than storing a zero. Unset is genuinely absent, which is what
makes the null semantics in §6.4 clean.

### 3.2 Hub-side application

`applyProjectDefaults(ac *store.AgentAppliedConfig, project *store.Project)`
fills `ac.HarnessConfig`, `ac.Model`, `ac.ThinkingLevel` and
`ac.InlineConfig.{MaxTurns,MaxModelCalls,MaxDuration,Resources}` **only if they
are currently unset**. It runs in the agent-create path
(`handlers_agents_core.go`) and in scheduler dispatch (`server.go`).

### 3.3 Hub-level agent defaults — the gap

`opsettings.AgentDefaultsSettings` (`pkg/config/opsettings/sections.go`) holds
`DefaultTemplate`, `DefaultHarnessConfig`, `DefaultMaxTurns`,
`DefaultMaxModelCalls`, `DefaultMaxDuration`, `DefaultResources`, registered as
the `agent_defaults` section and surfaced in the admin UI.

**These values are never read by the hub during agent create or dispatch.**
`ApplySnapshot` (`pkg/hub/operational_settings.go`) applies telemetry, admin
emails, auto-suspend, user access mode, GitHub app config and hub name — and
nothing from `agent_defaults`. `hub.ServerConfig` has no fields for them.

They are consumed **broker-side**, in `pkg/agent/provision.go`, from
`config.LoadEffectiveSettings(projectDir)` — that is, the *broker's own*
`~/.scion/settings.yaml` merged with the project's `.scion/settings.yaml` and
environment. `DefaultHarnessConfig` is step 7 of
`config.ResolveHarnessConfigName` (`pkg/config/resolve_harness_config.go`).

The bridge exists only in **file/SQLite mode**: `handlePutServerConfig` writes
the values into `<globalDir>/settings.yaml`, and a co-located broker reads that
file. In **Postgres mode** `handlePutServerConfigDB` writes database rows only,
and there is no transport that carries them to a remote broker.

**Consequence:** on any Postgres / multi-broker deployment, the hub-level agent
defaults are display-only. A "Hub default: 200 turns" hint would be false in
exactly the deployments Feature A is most valuable in.

**Resolved (D-1, 2026-07-28):** we fix this. Phase 2 applies hub agent defaults
hub-side, so they take effect on every deployment topology and the hint is
truthful. See §4.1 and §8 Phase 2.

### 3.4 Effective precedence

Target order after this work:

```
explicit agent-create request
  > project annotation
  > template
  > harness config
  > profile
  > hub default (applied hub-side; see §4.1)
  > compiled default
```

Today `harness_config` violates this by resolving the template ahead of the
project annotation, inverting positions 2 and 3. **Resolved (D-2, 2026-07-28):**
we normalise it to the order above — project annotation beats template, for all
fields. See §7.2.

---

## 4. Feature A — Project Settings Fallback UX

### 4.1 API design

#### Decision: new sub-resource, not an extension of the existing response

`hubclient.ProjectSettings` is used as **both** the `GET` response body and the
`PUT` request body (`pkg/hubclient/types.go`). Adding hub-fallback fields to it
would make them appear in the PUT contract, implying they are writable and
inviting clients to round-trip a GET into a PUT — which would silently promote
every hub fallback into an explicit project annotation. That is #381 by accident,
and #381 is deferred.

Therefore: a **new read-only endpoint**.

```
GET /api/v1/projects/{id}/settings/resolved
```

Authorization: `ActionRead` on the project — identical to
`GET /api/v1/projects/{id}/settings`. Deliberately *not* admin-gated; the whole
point is to expose hub defaults to non-admin project owners. Only the
`agent_defaults` section is exposed, never the full server config.

Routing: added to the `subPath` dispatch in `handleProjectRoutes`
(`pkg/hub/handlers_projects_core.go`), alongside `settings`, `pre-start-hooks`,
`discover-templates`. Note this is a chain of `if subPath == …` /
`if strings.HasPrefix(subPath, …)` checks, not a `switch`.

#### Response shape

> **STALE — NOT AS BUILT.** The response shape, Go types and rationale in the
> rest of §4.1 describe a `{value, source, projectValue, hubValue}` quad that was
> specified, reviewed and then deliberately **not** implemented. They are kept
> here as the record of a rejected design, not as a specification.
>
> As built: the response is `{project, settings}`, and each entry is
> `{projectSet, projectValue, hubDefault}` where `hubDefault` is the tri-state
> `"present" | "absent" | "unknown"`. There is **no** effective value, **no**
> `source`, and **no** hub value — only whether a hub default exists.
>
> Why: computing an effective value here would be a second implementation of the
> precedence order in a package that cannot observe changes to the first one, and
> the copy fails silently because a stale answer is still a well-formed answer.
> The `{projectValue, hubValue}` pairing is the same claim by another route — two
> adjacent fields with nothing between them assert that nothing is between them,
> which is false whenever the template or harness-config layer supplies the value.
> See the header comment in `pkg/hub/project_settings_resolved.go` and
> `docs-site/src/content/docs/reference/project-settings-resolved.md`.
> `TestResolvedSettingsResponseShape_NoEffectiveValue` enforces it.

```jsonc
{
  "project": { /* exactly the existing ProjectSettings object, unchanged */ },
  "fields": {
    "defaultHarnessConfig": {
      "value":  "claude",
      "source": "hub",
      "projectValue": null,
      "hubValue": "claude"
    },
    "defaultMaxTurns": {
      "value":  120,
      "source": "project",
      "projectValue": 120,
      "hubValue": 200
    },
    "defaultModel": {
      "value":  null,
      "source": "unset",
      "projectValue": null,
      "hubValue": null
    }
  }
}
```

Go types (new file `pkg/hub/project_settings_resolved.go`, mirrored in
`pkg/hubclient/types.go`):

```go
// ResolvedSettingSource describes where a resolved project setting came from.
type ResolvedSettingSource string

const (
    ResolvedSourceProject ResolvedSettingSource = "project" // explicit project annotation
    ResolvedSourceHub     ResolvedSettingSource = "hub"     // hub agent_defaults fallback
    ResolvedSourceUnset   ResolvedSettingSource = "unset"   // neither set
)

// ResolvedSettingField is one field's resolution result.
type ResolvedSettingField struct {
    Value        any                   `json:"value"`
    Source       ResolvedSettingSource `json:"source"`
    ProjectValue any                   `json:"projectValue"`
    HubValue     any                   `json:"hubValue"`
}

// ResolvedProjectSettings is the GET .../settings/resolved response.
type ResolvedProjectSettings struct {
    Project *hubclient.ProjectSettings       `json:"project"`
    Fields  map[string]ResolvedSettingField  `json:"fields"`
}
```

Rationale for the `{value, source, projectValue, hubValue}` quad rather than a
bare `hubDefaults` blob:

- It matches provenance conventions already established in the codebase:
  `SectionMetadata.Source` (`admin_settings_db.go`), `SupersededKey.Source`,
  `config.HarnessConfigResolution{Name, Source}`, `api.SecretKeyInfo.Source`.
- The UI needs `source` anyway to decide between rendering a value and rendering
  a hint; deriving it client-side from two nullable fields duplicates logic that
  belongs on the server.
- It leaves room to add `"template"` and `"harness-config"` sources later without
  a breaking change, should we later want to show template/harness-config
  provenance too.

#### Fields covered

Only the fields for which a hub-level fallback actually exists in
`opsettings.AgentDefaultsSettings`, plus telemetry which already has one:

`defaultTemplate`, `defaultHarnessConfig`, `defaultMaxTurns`,
`defaultMaxModelCalls`, `defaultMaxDuration`, `defaultResources`,
`telemetryEnabled`.

Fields with **no** hub-level counterpart — `defaultModel`,
`defaultThinkingLevel`, `activeProfile`, `defaultGcpIdentityMode`,
`defaultGcpIdentityServiceAccountId` — are still emitted, with
`hubValue: null` and `source: "project"` or `"unset"`. Emitting them uniformly
lets the UI iterate one map instead of maintaining a parallel allowlist, and
makes it a one-line server change if a hub-level default is added later.

#### Reading hub values

```go
func (s *Server) hubAgentDefaults() opsettings.AgentDefaultsSettings
```

- If `ops := s.GetOperationalSettings(); ops != nil` → read the `agent_defaults`
  section from `ops.Snapshot()`. This is the Postgres path and is where the
  values actually live.
- On any error, return the zero value. A missing hint degrades to today's
  behaviour; it must never fail the request.

**File-mode caveat.** `hub.Layer1Snapshot` does carry all six agent-defaults
fields, but `BuildLayer1SnapshotFromFile` deliberately leaves them zero (there
is an explicit comment saying so). So in file mode there is no in-process source
for them — they live in the on-disk `settings.yaml` that the co-located broker
reads directly. A naive "fall back to the bootstrap snapshot" would therefore be
a no-op returning zeros, and must not be written as though it were a real
fallback. Two acceptable outcomes for file mode: report `hubValue: null` (the
hint is simply unavailable, which is honest), or load `settings.yaml` directly.
**Recommendation: report null.** File mode is the single-binary case where the
existing broker-side resolution already works correctly; the hint is a
nice-to-have there, and reading a file from a request handler to produce a
placeholder is not worth the coupling. Implementer should confirm this is
acceptable before choosing otherwise.

Telemetry continues to come from `s.config.TelemetryDefault`, the same source
`handlePublicSettings` uses, to avoid two disagreeing answers for one field.

#### Applying hub defaults hub-side (D-1)

Per §3.3, in Postgres mode these hub values never reach agents, which would make
the hint a lie. **Decision: fix it.** Phase 2 adds

```go
// applyHubDefaults fills unset agent config fields from the hub-level
// agent_defaults section. Runs after applyProjectDefaults, so project
// annotations always win.
func applyHubDefaults(ac *store.AgentAppliedConfig, d opsettings.AgentDefaultsSettings)
```

called immediately after the existing `applyProjectDefaults` call at both sites:

- `handlers_agents_core.go` — the agent-create path
- `server.go` — scheduler dispatch

Semantics mirror `applyProjectDefaults` exactly: **only-if-unset**, field by
field, covering `HarnessConfig`, and `InlineConfig.{MaxTurns, MaxModelCalls,
MaxDuration, Resources}`. `DefaultTemplate` is handled earlier, alongside the
existing project-level default-template logic, because template resolution has
to happen before harness resolution.

This closes the Postgres gap and makes the resolved endpoint's `source: "hub"`
truthful on every topology.

**Behaviour change.** Existing Postgres deployments that already have
`agent_defaults` rows go from those rows being inert to being applied. This is
the intended fix, but it is a live behavioural change and must appear in release
notes. Deployments with no hub agent defaults configured see no change; §9.1
carries an explicit regression test for that case.

**Interaction with the broker.** The broker performs its own fallback from its
local `settings.yaml` (`ResolveHarnessConfigName` step 7). Once the hub stamps a
value into `AppliedConfig`, that value travels

```
AppliedConfig.HarnessConfig
  → httpdispatcher.go        RemoteAgentConfig.HarnessConfig
  → start_context.go         opts.HarnessConfig
  → run.go                   GetAgent(..., opts.HarnessConfig, ...)
  → provision.go             ProvisionAgent(..., harnessConfig, ...)
  → provision.go             HarnessConfigInputs{CLIFlag: harnessConfig}
```

and arrives as **`CLIFlag` — step 1**, the highest priority in the chain, above
the template (steps 3–4) and the broker's own settings default (step 7). Note
`HarnessConfigInputs.StoredConfig` is **not** populated from the hub on the
create path; it is the broker's own locally-merged config and matters only on
resume. So the hub-side value wins outright, and no broker change is required.

### 4.2 Data model changes

**None.** No schema migration, no new ent entity, no new annotation keys.
Feature A is a pure read-side projection over data that already exists in
`project.Annotations` and the `hub_settings` `agent_defaults` row.

`applyHubDefaults` reads existing opsettings state and writes only into the
in-memory `AgentAppliedConfig`; it persists nothing new.

### 4.3 UI specification

> **STALE — depends on the rejected §4.1 shape.** Every rule below reads
> `fields.<name>.hubValue`; the endpoint as built has no `fields` map and returns
> no hub value. The rules that render a hub value inline (`Hub default: ${hubValue}`,
> `Use hub default (${hubValue})`) cannot be implemented against the shipped
> response and are not merely renamed — the data is deliberately absent. A UI
> revision is out of scope for this phase; see §4.1's STALE note for why.

File: `web/src/components/pages/project-settings.ts`.

**Data loading.** The existing `loadSettings()` call to
`/api/v1/projects/${projectId}/settings` is replaced by a call to
`/api/v1/projects/${projectId}/settings/resolved`. The `project` sub-object
populates the existing form state exactly as today — zero changes to the PUT
path. The `fields` map is stored as `this.resolvedFields`. The separate fetch of
`/api/v1/settings/public` for `hubTelemetryDefault` is removed; telemetry now
comes from `fields.telemetryEnabled.hubValue`.

**Rendering rules.**

1. *Text and number inputs* (`defaultMaxTurns`, `defaultMaxModelCalls`,
   `defaultMaxDuration`, all five `defaultResources.*`): when the project value is
   unset and `hubValue` is non-null, set the input's `placeholder` to
   `Hub default: ${hubValue}`. This reuses the control's own affordance — no new
   DOM — and keeps the field visually empty, which correctly signals "unset".
   When `hubValue` is null, keep today's placeholder text.
2. *Select inputs* (`defaultTemplate`, `defaultHarnessConfig`): the existing
   empty/none option's label becomes `Use hub default (${hubValue})` when
   `hubValue` is non-null, falling back to today's `None (use server default)`
   when it is null. This is character-for-character the pattern already shipped
   for telemetry.
3. *Telemetry select*: unchanged in appearance; only its data source moves.
4. A single shared helper is added to the component:

   ```ts
   private hubHint(field: string): string | null
   ```

   returning `Hub default: <formatted>` or `null`. All call sites go through it,
   so the copy is defined once.

**Resource spec formatting.** `defaultResources` arrives as a nested object.
`hubHint('defaultResources.cpuRequest')` etc. index into it. `null` sub-fields
produce no hint.

**Explicitly not doing:** no badge component, no "inherited" chip, no italic
ghost-value overlay. Placeholder + option-label reuse is a ~60 line change with
no new visual vocabulary, and it matches the one precedent users have already
seen.

### 4.4 Harness fallback list consistency cleanup

Three components hardcode a fallback list of harness names for when the
harness-config API returns nothing, and they disagree:

| Component | Hardcoded list |
| --- | --- |
| `project-settings.ts` | `gemini`, `claude`, `opencode`, `codex` |
| `admin-server-config.ts` | `gemini-cli`, `claude`, `codex`, `copilot`, `opencode` |
| `agent-create.ts` | `gemini-cli`, `claude`, `codex`, `copilot`, `opencode` |

Each site duplicates the list twice over: once as a `fallbackNames` array
(`project-settings.ts` has only the markup form) and once as a hardcoded block
of `<sl-option>` elements.

The canonical set, from `harnesses/*/config.yaml`, is: `claude`, `codex`,
`copilot`, `gemini-cli`, `opencode`, `antigravity`, `hermes`.

`project-settings.ts` is outright wrong — `gemini` is not a harness name; the
correct identifier is `gemini-cli`. All three lists are missing `antigravity`
and `hermes`, and `project-settings.ts` is additionally missing `copilot`.

In `agent-create.ts` the list drives logic rather than just display:
`setHarnessFromValue()` treats any value absent from it as `__other__`, so with
an empty harness-config response a project defaulting to `antigravity` or
`hermes` renders as "Other..." and is silently demoted to a custom string.

**Fix.** New module `web/src/shared/harness-utils.ts`, following the existing
`web/src/shared/` convention (`model-utils.ts`, `types.ts`, `lineage.ts`):

```ts
/** Canonical harness identifiers, mirroring harnesses/ * /config.yaml. */
export const KNOWN_HARNESS_NAMES = [
  'claude',
  'codex',
  'copilot',
  'gemini-cli',
  'opencode',
  'antigravity',
  'hermes',
] as const;
```

All three components import it and delete both their local arrays and their
hardcoded `<sl-option>` blocks, rendering the fallback options by mapping over
`KNOWN_HARNESS_NAMES` instead. A comment on the constant points at `harnesses/`
as the source of truth.

The module also exports a `harnessDisplayName(name)` helper. The components
render human-readable labels ("Gemini CLI", "OpenCode"), and
`harnesses/<name>/config.yaml` has no display-name field to derive them from, so
the labels cannot be dropped without a visual regression. The helper is a
display-only map with a passthrough fallback for unknown identifiers;
`KNOWN_HARNESS_NAMES` remains the source of truth for which harnesses exist.

This is a small, independently reviewable, zero-risk change with no API
dependency — it is sequenced first (Phase 0) so it can land in parallel with the
backend work.

---

## 5. Feature B — Project Clone

### 5.1 API specification

```
POST /api/v1/projects/{id}/clone
```

Request:

```jsonc
{
  "name": "my-new-project",     // required
  "slug": "my-new-project-alt"  // optional explicit slug override
}
```

Response: **201 Created**, body is the `store.Project` — byte-identical in shape
to `POST /api/v1/projects`, so every existing client-side project renderer works
without modification.

Errors:

| Status | Condition |
| --- | --- |
| 400 | missing/blank `name`, or invalid explicit `slug` |
| 403 | caller lacks read on source, or lacks project-create at hub scope |
| 404 | source project not found (or not visible to caller) |
| 409 | explicit `slug` collides with an existing project |
| 500 | clone failed after rollback |

Note the deliberate asymmetry: when `slug` is **omitted**, the handler uses
`api.Slugify(name)` + `s.store.NextAvailableSlug(ctx, baseSlug)` and therefore
*cannot* 409 — it auto-disambiguates, exactly like `createProject`. 409 is
reachable only via an explicit `slug`. This means the common "clone this" button
never fails on a name collision.

Routing: `clone` is added to the `subPath` dispatch chain in
`handleProjectRoutes`, method-gated to POST.

`{id}` accepts an ID or a slug, consistent with every other project route.

### 5.2 Authorization model

Per confirmed decision:

Sketch — note the real API shapes, which are easy to get wrong: everything lives
in package `hub` (there is no `pkg/authz`), the field is `s.authzService`,
`CheckAccess` returns a `Decision` value rather than an `error`,
`ComputeScopeCapabilities` takes five arguments, and `Capabilities` is a bare
`Actions []string` with no `Can` helper.

```go
// 1. Caller must be able to read the source project.
if d := s.authzService.CheckAccess(ctx, identity, Resource{
    Type: "project", ID: src.ID, OwnerID: src.OwnerID,
}, ActionRead); !d.Allowed {
    Forbidden(w, "insufficient permission to read source project")
    return
}

// 2. Caller must be able to create projects at hub scope.
caps := s.authzService.ComputeScopeCapabilities(ctx, identity, "hub", "", "project")
if !slices.Contains(caps.Actions, ActionCreate) {
    Forbidden(w, "insufficient permission to create projects")
    return
}
```

The exact `ComputeScopeCapabilities` argument values should be confirmed against
a sibling call site during implementation.

Rationale and the extensibility note: today `createProject` performs **no**
authz check — any authenticated user may create a project — so check 2 is
currently vacuous. It is written explicitly anyway, against
`ScopeActions["project"]`, so that the day project creation is restricted, clone
is restricted with it and no one has to remember this handler exists.

Check 1 is the meaningful gate: *if you can see the configuration, you may copy
it*. This is intentionally permissive and matches `handleHarnessConfigClone`.
Should it need to narrow later (e.g. require `ActionUpdate`, or owner-only, or a
new `ActionClone`), the change is confined to this one block; the helper is
factored as `authorizeProjectClone(ctx, user, src) error` for exactly that
reason.

The clone's `OwnerID` and `CreatedBy` are the **caller**, never the source
project's owner. Cloning someone else's visible project gives you your own
project, not shared ownership of theirs. Visibility resets to
`store.VisibilityPrivate` (the `createProject` default) rather than inheriting
the source's — inheriting `public` would silently publish a project the caller
may not have intended to share.

### 5.3 What is copied

#### Copied

| Item | Store surface | Rationale |
| --- | --- | --- |
| **Settings annotations** (`scion.io/*`) | `project.Annotations` | The core of the feature. Only keys that are actually **set** are copied; absent stays absent (§6.4). |
| **Labels**, minus workspace-mode | `project.Labels` | Organisational metadata the user chose. `scion.dev/workspace-mode` is *re-derived*, not copied — see below. |
| **Git remote** | `project.GitRemote` | A clone of a project's config almost always targets the same repository. Also required for the workspace init path to work at all. |
| **Shared dirs** | `project.SharedDirs` | Pure configuration; meaningless to retype. |
| **Git identity config** | `project.GitIdentity` | Pure configuration. |
| **Default runtime broker** | `project.DefaultRuntimeBrokerID` | Configuration. If the broker is later removed, dispatch falls back exactly as it does for any project. |
| **Active pre-start hook** | `ProjectPreStartHook` (active only) | A tuned setup script is one of the highest-value things to carry over. |
| **Env vars** (scope=project, `Secret == false`) | `EnvVar` | Non-secret configuration. |
| **Injected skills** | `SkillInjection` (scope=project) | Configuration list; `SetSkillInjections` makes this a single atomic call. |
| **Project-scoped harness configs & templates** | `HarnessConfig`, `Template` | See §5.4 — deep-copied. |

**Workspace-mode label.** `scion.dev/workspace-mode` (`shared` /
`per-agent` / `worktree-per-agent`) is copied from the source but treated as
input to the standard `createProject` workspace-initialisation branch, not as an
inert label. The clone therefore ends up in the same workspace mode as its
source, provisioned fresh. The related `scion.dev/clone-url` and
`scion.dev/default-branch` labels are copied as-is since they describe the git
remote, which is also copied.

**Pre-start hook.** Only the **active** hook is copied
(`GetActiveProjectPreStartHook`). Archived revisions are history, not
configuration. The copy is created with a fresh UUID and slug, `CreatedBy` =
caller, and then activated via `ActivateProjectPreStartHook`. Note
`CreateProjectPreStartHook` already archives any existing active hook
atomically, so on a brand-new project a single `CreateProjectPreStartHook` with
`Status: active` suffices; the explicit activate call is belt-and-braces for the
case where workspace init has somehow installed one.

**Env vars.** `ListEnvVars(ctx, store.EnvVarFilter{Scope: store.ScopeProject,
ScopeID: srcID})` — note it takes a filter struct, not positional scope
arguments — then filtered on `Secret == false`.

That filter is belt-and-braces rather than the primary guard: `setEnvVar` routes
secret-flagged writes to the secret backend and then *deletes* any plain row, so
`Secret: true` entries generally do not exist in `env_vars` at all. The ones the
API returns are synthetic objects built from secret metadata with
`Value: "********"`, which `ListEnvVars` never produces. The explicit filter
stays anyway, because relying on that invariant silently is how a future change
leaks a secret into a clone.

`Sensitive: true` rows *are* copied. `Sensitive` only masks the value in API
responses; the column is a plain unencrypted string and the value is
legitimately part of the configuration. The unique constraint is
`(key, scope, scope_id)`, and the destination scope is a brand-new project ID, so
no conflict is possible; `CreateEnvVar` is used rather than `UpsertEnvVar`, so
that an unexpected conflict surfaces as an error rather than silently
overwriting.

#### Not copied

| Item | Why |
| --- | --- |
| **Secrets** | Values are unreadable by design; for GCP-SM they are not in the hub DB. Copying names without values yields a project that looks configured and fails at runtime. |
| **Chat integrations** (Telegram/Discord) | Hub-level, no `project_id`; credentials are hub-scoped secrets; per-project links live in the plugin's own DB. Confirmed exclusion. |
| **Agents** | Runtime state, not configuration. |
| **Messages / history / events** | Runtime state. |
| **Workspace content** | Re-initialised from the git remote by the standard path. |
| **Secret-backed env vars** (`Secret == true`) | See above. |
| **Scheduled events / schedules** | `ScheduledEvent` carries a `ProjectID`, but its payload references agents *by name*, and no agents are cloned. Copying them would produce schedules that fire into the void. Out of scope; revisit if requested. |
| **Project providers / contributors** | Access control, not configuration. The clone's membership is established by the standard group/policy creation for its new slug. |
| **GCP service accounts** | Bound to external IAM state; a copied binding would be wrong or a privilege leak. |
| **Notification subscriptions & subscription templates** | Per-user runtime preferences. |
| **Project sync state, user access tokens** | Runtime/credential state. |
| **Project-scoped skill bank entries** | Skill *versions* with storage payloads; deep-copying a skill bank is a materially larger feature. The injected-skills *list* is copied; if it references a project-scoped skill URI, that reference will need the source project to remain readable. Flagged as a known limitation. |

### 5.4 Project-scoped harness configs and templates: deep copy

A project-scoped `HarnessConfig` or `Template` is keyed
`(slug, scope, scope_id)` with `scope = "project"` and `scope_id = <projectID>`.
Its files live in object storage at
`storage.HarnessConfigStoragePath(hubID, scope, scopeID, slug)` /
`storage.TemplateStoragePath(...)`.

**Decision: deep-copy, do not reference by slug.**

Reference-by-slug is impossible without changing the resolution model: a
project-scoped config is looked up by `scope_id == thisProject`, so a clone that
merely records the slug would resolve to nothing. The alternatives were
(a) deep-copy, (b) leave project-scoped configs behind and let the clone's
`defaultHarnessConfig` annotation dangle. (b) produces a clone whose headline
setting is broken, which defeats the purpose.

Implementation follows `handleHarnessConfigClone` exactly:

1. `ListHarnessConfigs(ctx, HarnessConfigFilter{Scope: "project", ScopeID: srcID}, opts)`
2. For each: new `api.NewUUID()`, same slug (unique constraint is scoped to the
   new project ID, so the slug can be preserved — which matters, because the
   cloned `scion.io/default-harness-config` annotation refers to it by slug).
3. Recompute `StoragePath` for the new scope ID; `stor.Copy(ctx, src, dst)` per
   file; `stor.DeletePrefix(ctx, dstPrefix)` on any failure.
4. `s.store.CreateHarnessConfig`, mapping a UNIQUE violation to 409.

Identical treatment for `Template` via `ListTemplates` /
`TemplateStoragePath` / `CreateTemplate`, so that a cloned
`scion.io/default-template` annotation also resolves.

Preserving the slug is the key detail: it is what makes the copied annotations
valid in the new project without any rewriting.

### 5.5 Atomicity and rollback

There is no cross-store transaction spanning the project row, object storage,
and the authz group/policy tables. `createProject` already handles this with
**compensating rollback**, and clone follows the same pattern — this is
deliberate consistency with existing behaviour, not a new invention.

Ordering, with each step's compensation:

```
 1. load + authorize source                          — (no side effects)
 2. resolve name/slug (NextAvailableSlug)            — (no side effects)
 3. build clone Project struct (annotations, labels,
    git remote, shared dirs, git identity, broker)
 4. store.CreateProject                              → store.DeleteProject
 5. createProjectGroup                               → (cascades with project delete)
 6. createProjectMembersGroupAndPolicy               → (cascades with project delete)
 7. copy harness configs (storage + rows)            → DeletePrefix + DeleteHarnessConfigsByScope
 8. copy templates (storage + rows)                  → DeletePrefix + DeleteTemplatesByScope
 9. copy env vars                                    → DeleteEnvVarsByScope
10. SetSkillInjections (single atomic call)          → DeleteSkillInjectionsByScope
11. copy + activate pre-start hook                   → (see note below)
12. autoAssociateGitHubInstallation                  → best-effort, non-fatal
13. workspace init (shared vs hub-managed branch)    → full rollback
14. autoLinkProviders                                — best-effort, non-fatal
15. events.PublishProjectCreated                     — best-effort, non-fatal
16. 201 Created
```

**Step 11 has no direct compensation.** `DeleteProjectPreStartHook` returns
`store.ErrInvalidInput` when the hook is active, and step 11 leaves it active by
construction — so naming it as the rollback would fail on every invocation. The
hook is instead cleaned up transitively by step 4's `DeleteProject`. This is
safe because step 11 is the last hard-failing store write before the best-effort
tail, so the only way to unwind past it is a full project delete anyway. Do not
"fix" this by adding a `DeleteProjectPreStartHook` call.

**Step 15 reuses `PublishProjectCreated`.** There is no `PublishProjectCloned`
on the `EventPublisher` interface, and adding one would require touching the
interface, the noop implementation, and the builder for no subscriber benefit —
a clone *is* a project creation as far as every consumer is concerned.

Steps 1–13 are hard-failing: any error triggers unwinding of every completed
step in reverse and returns 500 (or the mapped status). Steps 12, 14 and 15 are
best-effort and match `createProject`'s existing treatment — a project that
exists but has not yet auto-linked a provider is a recoverable state; a project
that exists with half its harness configs is not.

The rollback logic is written as a `defer`-driven stack of closures:

```go
var rollback []func()
defer func() {
    if !committed {
        for i := len(rollback) - 1; i >= 0; i-- { rollback[i]() }
    }
}()
```

Each rollback closure logs its own failure and continues; a failed rollback must
never mask the original error. Every rollback uses `context.WithoutCancel` on
the request context, so that a client disconnect mid-clone does not abort the
cleanup and leave orphans — this is a hardening improvement over
`createProject`'s current behaviour and should be noted in review.

**Known residual risk.** If the process dies between steps 4 and 13 the clone is
left partially populated. This is identical to the existing exposure in
`createProject` and `handleHarnessConfigClone`, and is out of scope to fix here.
The `deleteProject` path will clean such a project up when the user deletes it,
with the caveat noted in §10 that `deleteProject` itself is incomplete.

### 5.6 CLI specification

```
scion project clone <source-slug-or-id> [--name <new-name>] [--slug <new-slug>]
```

Behaviour, following `cmd/project_rename.go` exactly:

1. `config.ResolveProjectPath(projectPath)` → `config.LoadSettings` →
   `getHubClient(settings)`
2. 30-second timeout context
3. `resolveProjectByNameOrID(ctx, client, sourceRef)`
4. `client.Projects().Clone(ctx, srcID, hubclient.CloneProjectRequest{...})`
5. `isJSONOutput()` → `outputJSON(ActionResult{...})`; otherwise a human line:
   `Cloned project "<source>" → "<new-name>" (slug: <new-slug>)`

If `--name` is omitted, default to `<source-name> copy`; the server's
`NextAvailableSlug` handles disambiguation, so repeated invocations produce
`… copy`, `… copy-2`, etc. without client-side logic.

New file `cmd/project_clone.go`, registered with
`projectCmd.AddCommand(projectCloneCmd)` in its `init()`, joining `init`, `list`,
`rename`, `skills`, `hook`, `prune`, `service-accounts`, `reconnect`.

SDK addition to the `ProjectService` interface (`pkg/hubclient/projects.go`):

```go
// Clone creates a new project seeded from an existing project's configuration.
Clone(ctx context.Context, sourceID string, req CloneProjectRequest) (*Project, error)
```

`CloneProjectRequest{Name, Slug string}` added to `pkg/hubclient/types.go`.

### 5.7 UI specification

**Entry point.** `web/src/components/pages/project-detail.ts`, in the existing
`header-actions` div alongside New Agent / Pull Latest / Metrics / Settings. A
`Clone` button, gated on `can(this.project?._capabilities, 'read')` — consistent
with §5.2's authorization model.

The projects list (`web/src/components/pages/projects.ts`) has no per-row action
menu today; introducing one is a larger UI change than this feature warrants.
Clone lives on the detail page only, in the first cut.

**Dialog.** An `<sl-dialog>` with:

- a `Name` text input, pre-filled with `<source name> copy`
- a read-only preview line: `Slug: <slugified name>` (client-side slugify for
  display only; the server is authoritative and may append a serial)
- a short, explicit summary of what will and will not be copied — this is the
  most important element in the dialog, because the exclusion list is
  surprising:

  > **Copies:** settings, labels, environment variables, injected skills,
  > pre-start hook, project harness configs and templates.
  > **Does not copy:** secrets, agents, history, or chat integrations.

- Cancel / Clone buttons; Clone shows a spinner and disables on submit.

On 201, navigate to the new project's detail page. On error, show the server's
message inline in the dialog without closing it, so the user does not lose the
name they typed.

---

## 6. Shared Infrastructure & Cross-Cutting Concerns

### 6.1 Settings-key registry

Both features enumerate the same set of project settings. Feature A needs the
list to build the `fields` map; Feature B needs it to copy annotations. Today
the list exists implicitly, as a series of hand-written `setOrDelete` calls.

A single table in `pkg/hub/project_settings_handlers.go`:

```go
// projectSettingKeys is the authoritative list of scion.io/* annotation keys
// that constitute project settings. Anything not in this list is not a project
// setting and is not copied by clone or reported by the resolved endpoint.
var projectSettingKeys = []string{
    projectSettingDefaultTemplate,
    projectSettingDefaultHarnessConfig,
    // … all sixteen
}
```

This is the mechanism that makes "copy the settings" precise. It is added in
Phase 2 and consumed by Phase 4.

**Note on annotation copying:** an audit confirmed that `scion.io/*` settings
keys are the *only* annotations read from `project.Annotations` anywhere in the
codebase. Copying the whole annotation map would in fact be safe today. The
registry is still preferred, because it is explicit, it makes the clone contract
reviewable, and it means a future non-settings annotation (a lock marker, a
migration flag) is not silently propagated into clones.

### 6.2 Provenance vocabulary

`ResolvedSettingSource` (`"project" | "hub" | "unset"`) is introduced by Feature
A and is the extension point for surfacing template and harness-config
provenance, should that be wanted later. It intentionally mirrors the existing
`SectionMetadata.Source` / `HarnessConfigResolution.Source` conventions rather
than inventing a fourth spelling.

### 6.3 Slug and group derivation

Project group slugs (`project:<slug>:agents`, `project:<slug>:members`) and the
policy `project:<slug>:member-create-agents` are **globally unique**. The clone
must derive all three from the **new** slug, which it gets for free by calling
the same `createProjectGroup` / `createProjectMembersGroupAndPolicy` helpers
`createProject` uses. No group state is copied from the source.

### 6.4 Null semantics

Confirmed decision, applied uniformly:

- A setting that is **unset** on the source project is **unset** on the clone.
  No coercion to zero, no coercion to the hub fallback value.
- Because `setOrDelete` removes the annotation on empty/zero, "unset" is
  literally "key absent from the map", so the clone logic is a straightforward
  `if v, ok := src.Annotations[k]; ok { dst.Annotations[k] = v }`.
- This keeps clone orthogonal to #381: a clone inherits the source's *unset-ness*
  and therefore continues to track the hub default dynamically, exactly as the
  source does.

---

## 7. Alternatives Considered

### 7.1 Extending `GET /settings` instead of a new `/settings/resolved`

Rejected: `hubclient.ProjectSettings` is shared between the GET response and the
PUT body. Adding hub values there makes a naive GET-modify-PUT round-trip
promote every hub fallback into an explicit project annotation — silently
implementing the deferred #381 and permanently detaching projects from hub
defaults. A separate read-only sub-resource has no such failure mode.

A rejected middle option was a `?resolved=true` query parameter on the existing
endpoint. It avoids a new route but produces a response whose shape depends on a
query parameter, which is hostile to typed clients and to the OpenAPI surface.

### 7.2 Normalising the `harness_config` precedence inconsistency (D-2)

`harness_config` resolves **template before project annotation** (the template is
resolved in the agent-create handler, then `applyProjectDefaults` fills the field
only if it is still empty). But `max_turns`, `max_model_calls`, `max_duration`
and `resources` resolve **project annotation before template**, because
annotations are baked into `InlineConfig`, which the broker merges *over* the
template chain in `MergeScionConfig`.

So the statement "project settings override the template" is true for limits and
resources and false for harness config.

**Decision: normalise to project-over-template.** A project setting is an
override; that is what the words mean and what the UI implies. Making
`harness_config` behave like every other field removes a trap that no user could
be expected to discover, and it means the resolved endpoint can present one
consistent ordering instead of a per-field footnote.

#### Scope of the change

Two hub call sites, plus nothing broker-side:

**1. `handlers_agents_core.go` — agent create.** Today:

```go
harnessConfig := req.HarnessConfig
if harnessConfig == "" {
    harnessConfig = s.getHarnessConfigFromTemplate(resolvedTemplate, "")
}
```

Becomes:

```go
harnessConfig := req.HarnessConfig
if harnessConfig == "" && project != nil && project.Annotations != nil {
    harnessConfig = project.Annotations[projectSettingDefaultHarnessConfig]
}
if harnessConfig == "" {
    harnessConfig = s.getHarnessConfigFromTemplate(resolvedTemplate, "")
}
```

**2. `server.go` — scheduler dispatch.** Today the template's harness config is
stamped onto `AppliedConfig.HarnessConfig` unconditionally whenever non-empty,
*before* `applyProjectDefaults` runs — so the only-if-unset guard can never fire.
The stamp becomes conditional on the project annotation being absent, mirroring
the existing default-template block immediately above it.

**3. `handlers_agent_create_helpers.go` — `populateAgentConfig`.** No change
needed. Its `hcName := agent.AppliedConfig.HarnessConfig; if hcName == "" { …
template … }` fallback runs *after* `applyProjectDefaults`, so it already sees
the project value first. It only stamps the config ID and content hash.

**Broker: no change.** Once the hub stamps a value into `AppliedConfig`, it
reaches the broker as `HarnessConfigInputs.CLIFlag` — **step 1** of
`ResolveHarnessConfigName`, above the template at steps 3–4. (`StoredConfig` is
*not* fed from the hub on the create path; it is the broker's own merged config
and only matters on resume.) The broker chain already honours whatever the hub
decides; the hub was the only place that inverted the precedence.

**One caveat for the implementer.** After dispatch, `httpdispatcher.go`
overwrites `AppliedConfig.HarnessConfig` with the broker's *resolved* answer.
That is fine — the resolved answer will now be the project's value — but it
means an end-to-end assertion on the persisted `AppliedConfig` must be made
either before dispatch or against the post-resolution value, not naively at an
arbitrary point.

#### Risk

This changes which harness config an agent gets, for the specific population of
projects that have **both** a `scion.io/default-harness-config` annotation
**and** a template that specifies a different harness config. Those agents move
from the template's choice to the project's. That is the intended correction, but
it is a live behavioural change: it belongs in release notes, and §9.1 carries a
dedicated test for the both-set case asserting the project value now wins.

Projects with only one of the two are unaffected, which is the overwhelming
majority.

### 7.3 Clone by reference (project inheritance / parent pointer)

A `parent_project_id` with live inheritance was considered and rejected. It is a
much larger data-model change, it creates a permanent coupling that users must
later be given a way to break, and it does not match the stated need — which is
"seed a new project from this one", a one-time copy. Templates and harness
configs already established copy-on-clone as the house pattern.

### 7.4 Referencing project-scoped harness configs by slug rather than deep-copying

Rejected as structurally impossible: project-scoped configs are resolved by
`scope_id == thisProject`, so a slug reference from a different project resolves
to nothing. See §5.4.

### 7.5 Cloning secret *names* with empty values

Rejected. It produces a project that appears configured and fails at agent start,
and there is no mechanism to notify the user which values need filling. An empty
secret list is honest.

---

## 8. Phased Implementation Plan

Phases are sequential. Each is independently reviewable, independently
shippable, and leaves the tree green.

### Phase 0 — Harness list consistency (no API dependency)

- Add `web/src/shared/harness-utils.ts` with `KNOWN_HARNESS_NAMES` and
  `harnessDisplayName()`.
- Consume it in `project-settings.ts`, `admin-server-config.ts` and
  `agent-create.ts`; delete the local arrays and the hardcoded `<sl-option>`
  blocks in all three.
- `make web-typecheck && make web`, plus a test asserting all three components
  import the shared constant rather than redeclaring a list.

*Rationale for going first:* zero backend dependency, fixes two outright bugs
(`gemini` is not a harness name; `agent-create.ts` demotes `antigravity` and
`hermes` to "Other..."), and can land in parallel with Phase 1.

**Size:** XS. **Files:** 5 (3 components, 1 new module, 1 new test).

### Phase 1 — Precedence normalisation (D-2)

Kept separate from Phase 2 because it is the only change in this workstream that
alters what existing agents receive, and it should be reviewable, releasable and
revertable on its own.

- `handlers_agents_core.go`: project annotation ahead of template for
  `harness_config` (§7.2 site 1).
- `server.go`: make the scheduler's template stamp conditional (§7.2 site 2).
- Tests per §9.1, especially the both-set case.
- Release-note entry.

**Size:** S. **Blocked by:** nothing.

### Phase 2 — Feature A backend

- `projectSettingKeys` registry (§6.1) in `project_settings_handlers.go`.
- `applyHubDefaults` (D-1) + its call sites in the agent-create path and
  scheduler dispatch, immediately after `applyProjectDefaults`.
- `pkg/hub/project_settings_resolved.go`: `ResolvedSettingField`,
  `ResolvedProjectSettings`, `hubAgentDefaults()`, `handleProjectSettingsResolved`.
- Route registration in `handleProjectRoutes`.
- `pkg/hubclient`: `ResolvedProjectSettings` types +
  `ProjectService.GetResolvedSettings`.
- Tests per §9.1.
- Release-note entry for the Postgres behaviour change.

**Size:** M. **Blocked by:** Phase 1 (both touch the same resolution call sites;
sequencing them avoids a merge conflict and keeps the two behaviour changes
independently bisectable).

### Phase 3 — Feature A frontend

- `project-settings.ts` switches to `/settings/resolved`.
- `hubHint()` helper; placeholder and select-label wiring.
- Remove the now-redundant `/api/v1/settings/public` fetch.
- Tests per §9.3.

**Size:** S. **Blocked by:** Phase 2.

### Phase 4 — Feature B backend

- `pkg/hub/project_clone.go`: `CloneProjectRequest`,
  `authorizeProjectClone`, `handleProjectClone`, the rollback stack, and the
  per-resource copy helpers.
- Route registration in `handleProjectRoutes`.
- Tests per §9.2.

**Size:** L — the largest phase by a wide margin. If the EM wants to split it:
4a = project row + annotations + labels + groups + rollback skeleton;
4b = env vars + injected skills + pre-start hook;
4c = harness configs + templates (storage copy).
Each sub-phase is independently testable and additive.

**Blocked by:** Phase 2 (for `projectSettingKeys`). Independent of Phase 3, so
4 and 3 can run in parallel across two implementers.

### Phase 5 — Feature B SDK + CLI

- `CloneProjectRequest` and `ProjectService.Clone` in `pkg/hubclient`.
- `cmd/project_clone.go`.
- `docs-site/src/content/docs/reference/cli.md` and `api.md`.

**Size:** S. **Blocked by:** Phase 4.

### Phase 6 — Feature B frontend

- Clone button in `project-detail.ts` header actions.
- Clone dialog with the copies/does-not-copy summary.
- Navigate-on-success, inline error handling.

**Size:** S. **Blocked by:** Phase 4.

### Phase 7 — Follow-up issues (not implementation)

File, do not fix:

1. `deleteProject` is incomplete — it omits `ProjectPreStartHook`, schedules,
   notification subscriptions, subscription templates, project sync state, user
   access tokens, and project-scoped skills. Discovered while building the clone
   inventory; a clone makes the leak more visible because there are now more
   projects.
2. Project-scoped skill-bank deep copy (§5.3 known limitation).
3. Scheduled-event cloning with agent-reference rewriting, if requested.

---

## 9. Testing Plan

### 9.1 Feature A — unit / handler tests

New file `pkg/hub/project_settings_resolved_test.go`, build tag
`//go:build !no_sqlite`, using `testServer(t)` /
`createTestProjectForSettings(t, s)` / `doRequest(t, srv, ...)` and testify.

| Case | Assertion |
| --- | --- |
| Project value set, hub value set | `source == "project"`, `value == projectValue`, `hubValue` still reported |
| Project unset, hub set | `source == "hub"`, `value == hubValue`, `projectValue == nil` |
| Both unset | `source == "unset"`, `value == nil` |
| Field with no hub counterpart (`defaultModel`) | present in map, `hubValue == nil` |
| `defaultResources` partially set | per-sub-field resolution is independent |
| Non-admin, non-owner with read access | 200 |
| No read access | 403 |
| Unknown project | 404 |
| Operational settings unavailable | 200 with all `hubValue == nil`; never 500 |
| Telemetry field | agrees with `GET /api/v1/settings/public` |
| `project` sub-object | byte-identical to `GET /settings` for the same project |

Additionally in the agent-create tests, for `applyHubDefaults` (D-1):

| Case | Assertion |
| --- | --- |
| Hub default set, project unset | agent's `AgentAppliedConfig` receives the hub value |
| Hub default set, project set | project value wins |
| Hub default set, explicit request value | request value wins |
| No hub default | behaviour byte-identical to today (regression guard) |
| Postgres-backed store | hub default is applied (the specific gap being closed) |

And for the precedence normalisation (D-2) — these are the regression-sensitive
ones, since they assert a *changed* behaviour:

| Case | Assertion |
| --- | --- |
| Project annotation set, template specifies a different harness config | **project value wins** (was: template) |
| Project annotation set, no template | project value |
| No project annotation, template set | template value (unchanged) |
| Explicit `req.HarnessConfig`, both others set | request value (unchanged) |
| Same four cases via **scheduler dispatch** | identical outcomes to the create path |
| `AppliedConfig` as handed to the dispatcher | carries the project's value (assert pre-dispatch — see the §7.2 caveat about `httpdispatcher.go` overwriting it with the broker's resolved answer) |

### 9.2 Feature B — unit / handler tests

New file `pkg/hub/project_clone_test.go`, same harness.

*Happy path:*

| Case | Assertion |
| --- | --- |
| Full-fidelity clone | every copied item in §5.3 present on the clone with correct values |
| Unset annotations | absent on the clone — **not** zero, **not** hub-resolved (§6.4) |
| New identity | clone has a distinct ID, distinct slug, `OwnerID`/`CreatedBy` == caller |
| Visibility | clone is `private` even when the source is `public` |
| Slug omitted, name collides | auto-serialised, 201 (never 409) |
| Explicit colliding slug | 409 |
| Groups | `project:<newslug>:agents` and `:members` exist; source's groups untouched |
| Harness config slug preserved | cloned `default-harness-config` annotation resolves in the new project |
| Storage | cloned harness-config/template files exist at the recomputed path |

*Exclusions (negative assertions — these are the tests that matter most, because
a leak here is a security or correctness bug):*

| Case | Assertion |
| --- | --- |
| Source has secrets | clone has zero secrets |
| Source has secret-backed env vars | those are absent from the clone; `Sensitive: true` ones are present with their values |
| Source has agents | clone has zero agents |
| Source has archived pre-start hooks | only the active one is copied, and it is active on the clone |
| Source has integrations | clone has none |

*Authorization:*

| Case | Assertion |
| --- | --- |
| Caller lacks read on source | 403, and **no** project is created (verify the count is unchanged) |
| Caller has read but not ownership | 201 — read is sufficient |
| Unauthenticated | 401 |

*Rollback:* using a fault-injecting store/storage wrapper, force a failure at
each of steps 7–13 and assert that afterwards: no project row, no groups, no
env vars, no skill injections, no pre-start hook, and no storage objects remain
under the clone's prefix. This is the highest-value test in the suite and should
be table-driven over the step index.

*Concurrency:* two simultaneous clones of the same source with the same name
both succeed with distinct slugs.

### 9.3 Frontend tests

- `harness-utils` is imported (not redeclared) by all three components — a
  grep-style assertion in the existing web test suite is sufficient. It should
  also catch hardcoded `<sl-option>` harness blocks, not just array literals.
- `KNOWN_HARNESS_NAMES` matches the directory names under `harnesses/`, so the
  list cannot silently drift from the source of truth.
- `project-settings.ts` renders `Hub default: 200` as the placeholder when
  `defaultMaxTurns` is unset with a hub value.
- Renders `Use hub default (claude)` as the empty select option label.
- Renders today's generic placeholder when `hubValue` is null.
- Round-trip guard: loading `/settings/resolved` and immediately saving must PUT
  a body identical to what loading `/settings` and saving would have produced —
  i.e. **hub values must never be promoted into the PUT**. This is the test that
  prevents accidental #381.
- Clone dialog: pre-fills `<name> copy`, disables on submit, navigates on 201,
  keeps the dialog open and shows the message on error.

### 9.4 Integration

- `make ci` (fmt-check, vet, lint, test) green at every phase boundary.
- `make web-typecheck && make web` for Phases 0, 3, 6.
- Manual smoke on a Postgres-backed hub for Phase 2: set `agent_defaults` in the
  admin UI, create an agent in a project with no overrides, and confirm the agent
  actually receives them. This is the scenario that was silently broken.
- Manual smoke for Phase 1: a project with a `default-harness-config` annotation
  and a template naming a different one now yields the project's.

---

## 10. Migration & Backward Compatibility

**Schema:** none. Neither feature adds an ent entity, a column, or an index. No
migration is required for either.

**API compatibility:**

- `GET /api/v1/projects/{id}/settings` — unchanged, byte for byte.
- `PUT /api/v1/projects/{id}/settings` — unchanged. This is deliberate and is
  guarded by the round-trip test in §9.3.
- `GET /api/v1/settings/public` — retained. The web UI stops using it, but it is
  a published endpoint that may have external consumers. (It is authenticated,
  contrary to its name — see §1.1 — so "public" here means "not admin-gated".)
- `GET /api/v1/projects/{id}/settings/resolved` — new; additive.
- `POST /api/v1/projects/{id}/clone` — new; additive.
- Older web clients against a newer hub: unaffected, since the endpoints they use
  are unchanged. Newer web clients against an older hub: the resolved fetch 404s;
  `project-settings.ts` must fall back to `/settings` and render today's generic
  placeholders. **This fallback is required, not optional** — it is the only
  backward-compatibility work in the whole design, and it belongs in Phase 3.

**Behavioural compatibility:**

There are exactly **two** behavioural changes in this design. Both are
deliberate corrections, both are release-note items, and both are isolated into
their own phase so they can be reverted independently.

- **Phase 1 (D-2).** Projects that have *both* a
  `scion.io/default-harness-config` annotation *and* a template naming a
  different harness config will switch from the template's choice to the
  project's. Projects with only one of the two — the overwhelming majority — are
  unaffected. Guarded by the both-set test in §9.1.
- **Phase 2 (D-1).** Postgres-mode deployments that already have `agent_defaults`
  rows go from those rows being inert to being applied. Deployments that have
  never set hub agent defaults see no change; guarded by the "no hub default →
  identical to today" regression test in §9.1.

Neither change can affect a **running** agent: both apply at agent-create time
only, and an existing agent's `AppliedConfig` is already persisted.

- Phase 0 changes which harness names appear in a *fallback* list used only when
  the harness-config API returns nothing. In practice this list is rarely
  reached; correcting `gemini` → `gemini-cli` fixes a latent bug, and adding
  `antigravity`/`hermes` stops `agent-create.ts` demoting those two to
  "Other..." when the fallback path is taken.
- Feature B is purely additive; no existing project is touched by a clone.

**Data compatibility:** clones created by Phase 4 are ordinary projects. Nothing
marks them as clones, so no code needs to learn about a new project kind, and
they are indistinguishable to every existing code path. (A `clonedFrom`
provenance annotation was considered and dropped — it would be the first
non-settings `scion.io/*` annotation and would need a story for what happens
when the source is deleted.)

**Known pre-existing defect surfaced but not fixed:** `deleteProject` does not
clean up `ProjectPreStartHook`, schedules, notification subscriptions,
subscription templates, project sync state, user access tokens, or project-scoped
skills. Clone does not make this worse per project, but it makes projects cheaper
to create. Filed as a Phase 7 follow-up.

---

## 11. Open Questions

### Resolved

- **D-1 — Postgres agent-defaults gap (resolved 2026-07-28).** Hub-level
  `agent_defaults` never reach agents in Postgres mode (§3.3). **Decision: fix
  it.** Phase 2 applies hub defaults hub-side via `applyHubDefaults`. See §4.1.
- **D-2 — `harness_config` precedence inconsistency (resolved 2026-07-28).**
  **Decision: normalise so the project annotation overrides the template**, for
  all fields. Phase 1. See §7.2.

Both were confirmed by the product owner. No blocking questions remain; the two
below are non-blocking scope calls the EM can decide during implementation.

### 11.1 OQ-3: Should clone appear on the projects list page?

Non-blocking; Phase 6 scope. The list page has no per-row action menu today, so
adding one is a larger UI change than this feature warrants.
Recommendation: detail page only in the first cut; revisit if users ask.

### 11.2 OQ-4: Scheduled events

Non-blocking. Excluded per §5.3 because their payloads reference agents by name
and no agents are cloned. Recommendation: accept the exclusion and file the
Phase 7 follow-up to clone them with reference rewriting if it is ever
requested.

---

## 12. Acceptance Criteria

**Feature A**

1. A non-admin project owner can see, for every agent-defaults field they leave
   blank, the hub value that will be used — without any admin permission.
2. `GET /api/v1/projects/{id}/settings` and `PUT` are unchanged.
3. Loading the settings page and pressing Save without editing anything produces
   a PUT identical to today's — no hub value is ever promoted into a project
   annotation.
4. All three harness fallback lists are identical and canonical, because all
   three are the same list.
5. A Postgres-mode hub with `agent_defaults` configured actually applies them to
   new agents (D-1).
6. A project whose `default-harness-config` annotation disagrees with its
   template now yields the project's value, in both the create path and
   scheduler dispatch (D-2).

**Feature B**

6. `POST /api/v1/projects/{id}/clone` returns 201 with a new project carrying
   every item in §5.3's copy list and none in its exclusion list.
7. A partial failure at any step leaves no trace: no project, no groups, no
   storage objects.
8. A caller with read-only access to the source can clone it and owns the result.
9. `scion project clone <ref>` works and honours `--format json`.
10. The web dialog states plainly what is and is not copied.
11. Unset settings on the source remain unset on the clone.

---

## 13. Summary of Confirmed Decisions

Recorded here so implementers do not relitigate them:

1. Hub defaults are read from the existing opsettings system
   (`AgentDefaultsSettings`), not from the `HubSettings` key-value table.
2. Clone authorization = read on source + hub project-create; written to be
   narrowable later.
3. Chat integrations are hub-level and excluded from clone entirely.
4. Null stays null: unset source settings are unset on the clone, never coerced
   to zero or to the hub fallback.
5. Both features are designed together, implemented in sequential phases.
6. #381 (copy-on-create stamping of hub defaults) is deferred and not designed.
7. Feature A's UX is a hint on an empty control — placeholder text or an option
   label — not a tri-state control and not copy-on-create.
8. **D-1:** the Postgres agent-defaults gap is fixed as part of this work — hub
   defaults are applied hub-side so the UI hint is truthful on every topology.
9. **D-2:** `harness_config` precedence is normalised so the project annotation
   overrides the template, consistent with limits and resources.
