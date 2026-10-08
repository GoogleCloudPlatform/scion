---
title: Settings Precedence
description: Which setting wins when the same value is configured in more than one place.
---

Scion reads configuration from a lot of places: templates, `settings.yaml` files on the broker
and in your project, hub database scopes, project annotations, the agent-create request, and the
process environment. This page is the authoritative statement of which one wins.

## Read this first: there are two precedence systems, not one

The single most common mistake with Scion settings is to assume there is **one** ranked list of
sources that governs everything. There is not. There are **two independent systems**, and they
rank the same-named sources **differently**:

| System | Governs | Where the order is defined |
| --- | --- | --- |
| **A — Environment variables** | custom environment variables injected into the agent | `envScopesInPrecedenceOrder` in `pkg/hub/httpdispatcher.go`, plus the config seeding in the same file |
| **B — `ScionConfig`** | model, thinking level, `max_turns`, `max_model_calls`, `max_duration`, and `resources` | the ladder in `ResolveHarnessConfig` and `pkg/agent/provision.go` |

They are documented in two separate tables below, [System A](#system-a--environment-variables) and
[System B](#system-b--scionconfig-limits-and-resources), and **the tables are not
interchangeable.** A reader who takes a rule from one and applies it to the other will get
answers backwards. The clearest instance:

> `runtime_broker` ranks **lowest** for environment variables and **lowest** for the three scalar
> limits — but for `resources` the broker's own `settings.yaml` contributes at **three different
> ranks, two of them above the hub tier.** For `resources`, "`runtime_broker` is lowest" is not a
> rule that is stated backwards; it is a rule that **has no truth value**, because there is no
> single thing to rank. See [Resources interleave](#resources-interleaves-and-cannot-be-given-one-rank).

## How to read the tables

Every row that makes a claim about behaviour carries a status marker, defined once here:

| Marker | Meaning |
| --- | --- |
| *(no marker)* | Current behaviour, not changed by the settings-precedence release |
| **`Changed in this release`** | Behaviour changed by the settings-precedence release, with a one-line before → after. See the [release notes](/scion/release-notes/). |
| **`Unchanged`** | Explicitly *not* changed. Present because a neighbouring row would otherwise imply it was. |
| **`Known gap`** | Documented as broken or incomplete. Not fixed here. |
| **`Pending`** | The intended model is stated; the current behaviour does not match it and the resolution is not yet decided. |

### How the citations on this page are pinned

Precedence lives in a handful of small code regions that are edited often, so this page cites
**symbols and comment text**, not bare line numbers. A symbol that stops existing fails loudly —
your search returns nothing. A line number that has drifted always resolves, to real code, that
is confidently the wrong thing. Where a line reference is unavoidable it is written as
`file:line@<sha>` with the text it points at, and it names a **commit**, never a branch: a branch
name is not wrong today and will be wrong soon, with nothing to signal the transition.

One methodological note, because it changed a decision on this page: when asking whether a code
path is dead, ask about **method** reachability, not **type** reachability. The two come apart. A
type can be alive at dozens of construction sites and still carry a method nothing calls. "Is the
type reachable?" answers *yes* and argues for documenting a change that no user can observe.

---

## Bucket 1 — `SCION_*` variables that configure the broker process

These are read by the Scion broker (or CLI) **to configure itself as a process**. They are not
part of the agent settings chain at all, and an agent never sees them by this route.

They enter through the koanf environment layer in `LoadVersionedSettings`
(`pkg/config/settings_v1.go`), which is loaded **last** — after the embedded defaults, the global
`~/.scion/settings.yaml`, the in-repo `.scion/settings.yaml`, and any external project config.
Loaded last means **highest priority**: a `SCION_*` variable in the broker's process environment
overrides every settings file.

```
embedded defaults  <  ~/.scion/settings.yaml  <  .scion/settings.yaml
                   <  external project config  <  SCION_* process env
```

Key names are mapped by `versionedEnvKeyMapper`: the `SCION_` prefix is stripped, the remainder is
lowercased, and `_` becomes a settings path separator — so `SCION_HUB_PROJECT_ID` sets
`hub.project_id`.

:::caution[Same prefix, unrelated mechanism]
Bucket 1 and [Bucket 2](#bucket-2--scion_-variables-that-scion-injects-into-agents) both use the
`SCION_` prefix and are otherwise unrelated. Bucket 1 is *the broker reading its own
configuration*. Bucket 2 is *Scion writing variables into an agent's container*. Setting a
Bucket 2 variable in the broker's process environment does not configure an agent, and setting a
Bucket 1 variable in a harness config does not configure the broker.
:::

---

## Bucket 2 — `SCION_*` variables that Scion injects into agents

Scion injects a fixed set of `SCION_*` variables into every agent container in
`pkg/agent/run.go`. These are identity and telemetry plumbing, not user settings, and for most of
them **a user-supplied value is discarded**.

The column that matters is the last one. Scion writes some of these unconditionally, overwriting
whatever the hub or your configuration supplied; for others it checks first and defers to an
existing value.

| Variable | Meaning | Injection |
| --- | --- | --- |
| `SCION_AGENT_NAME` | agent name | **Unconditional** — overwrites |
| `SCION_PROJECT` | project name | **Unconditional** — overwrites |
| `SCION_TEMPLATE_NAME` | resolved template slug, or `custom` | **Unconditional** — overwrites |
| `SCION_CLI_MODE` | always `agent` inside a container | **Unconditional** — overwrites |
| `SCION_MAX_TURNS` | from the resolved `ScionConfig` | **Unconditional** — overwrites the hub-supplied value |
| `SCION_MAX_MODEL_CALLS` | from the resolved `ScionConfig` | **Unconditional** — overwrites the hub-supplied value |
| `SCION_MAX_DURATION` | from the resolved `ScionConfig` | **Unconditional** — overwrites the hub-supplied value |
| `SCION_WORKSPACE_MODE` | canonical workspace sharing mode (`shared-plain`, `clone-per-agent`, `worktree-per-agent`, or `empty-per-agent`) | **Unconditional** — overwrites |
| `SCION_WORKSPACE_GIT` | `"true"` when the workspace is a git repository, absent otherwise | **Unconditional** — overwrites |
| `SCION_TEMPLATE` | full template reference, for debugging | Set only when a template reference exists |
| `SCION_BROKER_NAME` | broker name, defaults to `local` | **Guarded** — defers to an existing value |
| `SCION_HARNESS` | harness name | **Guarded** — defers to an existing value |
| `SCION_MODEL` | resolved model | **Guarded** — defers to an existing value |
| `SCION_THINKING_LEVEL` | resolved thinking level | **Guarded** — defers to an existing value |
| `SCION_CREATOR` | OS user who created the agent | **Guarded** — defers to an existing value |

**Why the split matters.** The guarded set is how a hub-resolved value reaches the container: the
hub sends it, the broker sees the key is already present, and leaves it alone. The unconditional
set is not overridable in any useful sense — if you want a different `max_turns`, change the
setting that feeds `ScionConfig.MaxTurns` (see [System B](#system-b--scionconfig-limits-and-resources)),
not the environment variable.

### `Known gap` — the gemini-cli harness does not consume `SCION_THINKING_LEVEL`

Repo-wide, `SCION_THINKING_LEVEL` is honoured by exactly three harnesses: codex, antigravity and
claude.
Each declares a `thinking:` block in its `config.yaml` that maps the level to a native tier, and
its `provision.py` resolves it with `scion_harness.resolve_thinking` (see [Thinking Level
Map](/scion/reference/harness-settings/#thinking-level-map-thinking)). The gemini-cli
`config.yaml` has no `thinking:` block, and no gemini-cli harness file reads the variable. So
even with correct end-to-end delivery from the hub, **setting a thinking level for a gemini-cli
agent has no effect inside the container.** This is a harness feature request, not a precedence
bug.

*(Control for that absence claim: `SCION_MODEL` **is** read by
`harnesses/gemini-cli/provision.py`, where it resolves a `small`/`medium`/`large` alias against
the harness `config.yaml`, and falls back to that file's `model` default when `SCION_MODEL` is
empty — so the search does find gemini-cli's environment reads when they exist.)*

### `Known gap` — the gemini-cli redaction allowlist key is misspelled and inert

`harnesses/gemini-cli/home/.gemini/settings.json` sets
`security.allowedEnvironmentVariables` with a 34-entry array. **gemini-cli does not read that
key.** The real schema key is `security.environmentVariableRedaction.allowed`; the flat form
appears only in gemini-cli's own documentation, not in its settings schema, and there is no alias
map. The entire array is currently a no-op.

Three things follow, and each is independently true:

1. It is an **outbound** filter in the first place — it controls which variables gemini-cli passes
   to child processes it spawns (shell commands, MCP servers, hooks), not what gemini-cli itself
   reads from its own environment. It has never been a gate on anything Scion injects.
2. As written the key is inert, so the array does nothing at all.
3. Redaction is disabled by default regardless
   (`environmentVariableRedaction.enabled` defaults to `false`).

**This is deliberately not fixed in the settings-precedence release.** Renaming the key would
switch on a 34-entry allowlist covering `SCION_AUTH_TOKEN` and `GITHUB_TOKEN` — a security-posture
change that must not ride along in a precedence change. Adding an entry to a misspelled, disabled,
outbound-only filter would look like a fix and would not be one.

---

## Bucket 3 — end-user settings

This is the bucket you are almost certainly looking for. It has two sub-sections because, as
stated at the top, **there are two precedence systems.**

## System A — environment variables

This governs custom environment variables — the ones you set with `scion hub env set`, in a
harness config's `env:` block, or in an agent-create request.

### A1. The storage-scope ladder

:::note[This is a partial ladder — read A2 before using it]
The four rows below rank the **hub database storage scopes against each other**, and nothing
else. Something outranks all four of them. A descending list that stops here would imply `user`
is the top of the chain, and it is not — see [A2](#a2-the-config-rung-sits-above-all-four-storage-scopes).
:::

| Priority | Scope | Set with |
| --- | --- | --- |
| Highest of the four | `user` | `scion hub env set KEY=value` (the default) |
| | `project` | `scion hub env set --project KEY=value` |
| | `hub` | `scion hub env set --scope hub KEY=value` (admin-only) |
| Lowest of the four | `runtime_broker` | `scion hub env set --broker KEY=value` |

```
runtime_broker  <  hub  <  project  <  user
```

The order is stated in one place in the code, the `envScopePrecedence` list, and **the three
consumers in that file** derive their order from it: the resolver (`resolveEnvFromStorage`), the
provenance reporter that tells `scion hub env list` where a value came from (`buildEnvSources`),
and the startup shadow warning (`WarnOutrankedBrokerEnvKeys`). For those three, changing the list
is the only edit needed to change the **resolution** order.

:::caution[That is not the same as *everywhere*, and the difference is load-bearing]
A **fourth** provenance surface does not read this list. `Server.buildEnvGatherResponse`, in
`pkg/hub/handlers_agents_core.go`, answers the same "where did this value come from" question for
the env-gather path from its own hardcoded probe order: it defaults the reported scope to `hub`,
then checks `user`, `project`, `config` and `secret`, and **never consults `runtime_broker` at
all.**

**This is not a second precedence ladder, and reading it as one is the trap.** It is a *labeller*,
not a *resolver* — it never decides which value wins, only what to call the value it was already
handed. The ranking above is still the whole story for **resolution**. What this function can get
wrong is the **name** attached to a value, which is a reporting bug, not a precedence bug.

So the two surfaces disagree today, in both directions:

- A value set only at `runtime_broker` scope is reported as **`broker`** by `scion hub env list`
  and as **`hub`** by the env-gather API. Same key, same value, two answers — and the second one
  is not a lookup result. `"hub"` is the value the field is *initialised* to, and the code then
  tests `Scope == "hub"` to mean "not resolved yet". A broker-only key matches no probe, so it
  keeps the initial value and is reported as `hub` **not because it was found at hub scope, but
  because it was found nowhere** — and here "nowhere" is spelled the same as a real answer.
- The env-gather API can also answer **`config`** or **`secret`**, which are not env scopes in
  this ladder at all and cannot be set with `scion hub env set`.

Its `user`-before-`project` order agrees with this one **by coincidence rather than by
construction**, so reordering the list does not reach it — it has to be changed by hand. That
reporter is tracked as a separate follow-up.

Read the ranking above as governing **which value wins**, and the label that `scion hub env list`
prints. Do not read it as the only place a scope order is written down.
:::

:::caution[`hub`, not `global`]
The scope is called **`hub`** (`store.ScopeHub = "hub"`). Do not call it `global`. That word is an
established term of art elsewhere in Scion — `TemplateScopeGlobal`, `HarnessConfigScopeGlobal`,
and wide CLI use — where it means the top-level, no-project scope. Using it for the env hub scope
is a collision, and a reader who knows the other meaning will proceed confidently with the wrong
model.
:::

#### `Changed in this release` — `runtime_broker` demoted from highest to lowest

**Before:** `runtime_broker` was the **highest**-priority env scope; it beat `user`, `project` and
`hub`.
**After:** `runtime_broker` is the **lowest**; all three of the others beat it.

Three pairwise relations invert: `(user, runtime_broker)`, `(project, runtime_broker)` and
`(hub, runtime_broker)`. Each now resolves to the non-broker side.

:::danger[This is a reversal, not a fix]
Do not read this as a correction to something that was obviously wrong. It is a deliberate
reversal of a working behaviour, and **any deployment that set a broker-scoped variable
specifically to pin something a user or project also sets will silently flip.**

There is **no migration and no automatic warning** beyond the startup log described below, because
the hub cannot distinguish "deliberately pinned by an operator" from "set by accident".

If you operate a hub, review every key defined at broker scope before upgrading.
:::

To make the blast radius visible, the hub emits a **one-shot startup log** (`WarnOutrankedBrokerEnvKeys`)
listing every key defined at `runtime_broker` scope *and* at a scope that now outranks it. That
log is the only warning available.

**Why the demotion.** Broker-scoped env is the most infrastructural and least specific of the
four scopes, so it makes a better weakest-default than an override nobody can escape. Its previous
top rank was not a decision — it fell out of the order four near-identical blocks happened to
appear in.

:::note[Future direction]
The `runtime_broker` env scope may be removed entirely in a future release; bottom-ranking it is a
step in that direction. No such change is scheduled. It is recorded here so operators making
long-lived decisions about broker-scoped variables know the direction of travel.
:::

### A2. The config rung sits above all four storage scopes

Environment variables supplied in the **agent-create request** — via the API, the web form, or
`--config` — land in `AppliedConfig.Env`, and that map is seeded **first**. Storage values are
then used only to fill keys the config left unset.

```
runtime_broker  <  hub  <  project  <  user  <  request / --config env
```

So request env **outranks all four storage scopes, `user` included.**

#### The top rung is not a plain inequality, and a ranked list cannot express it

The guard that lets storage fill in a key is, in the code's own words:

> *Storage env vars fill in keys not already set (with a non-empty value) by explicit config env
> vars. Empty-value config entries are passthrough markers and should be overridden by storage
> values.*

Concretely:

- a config entry with a **non-empty** value **beats** every storage scope;
- a config entry with an **empty** value is a **deliberate passthrough marker**: it **yields** to
  storage, and the storage value wins.

**A total order cannot express this.** If you take the ladder above literally you will predict the
wrong winner for every passthrough marker. This is not a footnote; it is the actual behaviour of
the top rung.

:::caution[Where this relation is enforced, and where it is pinned]
The same guard appears at **four** sites — two entry paths (agent create, and
`DispatchAgentStart`) × two axes (env vars and secrets).

It is pinned by a test on **one** of them: the `DispatchAgentStart` env path. There is no
equivalent test on the agent-create path. That means the passthrough behaviour is real and
identical at all four sites today, but only one of them is protected against a future
simplification of the guard.

This note is here because a reference page is exactly the artifact that turns single-path
coverage into an assumed global guarantee. It is stated so it does not.
:::

### A3. The "settings file" tier is a composite, not a single scope

Below the hub storage scopes, an agent's environment also receives variables from settings files —
the broker's own `settings.yaml` and your project's `.scion/settings.yaml`. A table of *scopes*
naturally shows this as **one** tier. It is not one tier.

`ResolveHarnessConfig` builds the harness-config entry by starting from
`harness_configs.<hc>.env` as the **base** and merging profile-scoped maps over it with
`mergeMaps`, **whose second argument wins**. Until this release that produced **three** ranks
inside what the outer table shows as one tier:

```
profiles.<p>.harness_overrides.<hc>.env  >  profiles.<p>.env  >  harness_configs.<hc>.env
```

This sub-ladder is **invisible from the outer table.** A reader who sets the same key in
`profiles.<p>.env` and in `harness_configs.<hc>.env` gets no guidance whatever from a correct
scope table, because the scope table **cannot express the question.** A tier that hides a
sub-ladder cannot be corrected by re-ranking it — it can only be expanded, which is what this
section does.

**This release removes the middle rank and only the middle rank.** Two remain:

```
profiles.<p>.harness_overrides.<hc>.env  >  harness_configs.<hc>.env
```

:::caution[Read the two Changed rows together, or you will get this backwards]
`profiles.<p>.env` is retired; `profiles.<p>.harness_overrides.<hc>.env` is **not**. See
[`profiles.<name>.env` retired](#changed-in-this-release--profilesnameenv-retired-breaking) and,
immediately after it, [what continues to
work](#unchanged--profilesnameharness_overrideshcenv-continues-to-work). One of those rows exists
because the other one does.

In particular: **`harness_overrides.<hc>.env` still outranks `harness_configs.<hc>.env`.** Any
sentence of the shape *"harness-config env now takes precedence over profile env"* is false, not
merely outdated — the top rank did not move.
:::

### A4. Why System A and System B differ at all

They differ in exactly one structural property, and it explains everything else:

- **Environment variables COLLAPSE.** Inside `ResolveHarnessConfig` the settings-file env sources
  merge down to a **single map**, and that one map then enters the outer chain as a **single
  only-if-absent tier**. One contribution, one rank.
- **Resources INTERLEAVE.** The same broker `settings.yaml` contributes to `resources` at
  **three separate positions in the outer ladder, with the hub tier sandwiched between them.**

**A single-tier contribution can be given a single rank. An interleaved one cannot.** That is why
"`runtime_broker < hub`" is a well-formed, true statement about environment variables and a
statement with **no truth value** about `resources`.

:::note[Do not overstate the parallel — these are different execution paths]
The three-ranks-in-one-file shape appears on both axes, but not in the same mode. The env
injection in `pkg/agent/run.go` is gated on `!opts.BrokerMode`, so the env sub-ladder in A3 is the
**CLI / local** path. The `resources` interleave in
[System B](#resources-interleaves-and-cannot-be-given-one-rank) is the **broker / provisioning**
path. Same shape, different modes, and **only one of them interleaves with the hub tier.**
:::

### A5. Secrets rank `user` and `project` in the opposite direction

Secrets are resolved by a **different** ladder from environment variables, and the two disagree:

```
env vars:   runtime_broker  <  hub  <  project  <  user           (user wins)
secrets:    hub  <  user  <  project  <  runtime_broker           (project wins)
```

For environment variables, **`user` beats `project`**. For secrets, **`project` beats `user`** —
and `runtime_broker` is still highest for secrets.

This is a **filed difference, not a documented design**. Nobody has established that the
divergence is intentional, so this page does not present it as a second designed ladder. It is
tracked as [issue #624](https://github.com/ptone/scion/issues/624).

:::note[Related: two code comments state the secret order without `hub`]
`pkg/secret/secret.go` and `pkg/hub/httpdispatcher.go` both describe the secret order as
`user < project < runtime_broker`, omitting `hub`. Both backends do query the hub scope, and rank
it lowest. The ladder above is taken from the resolver implementation, not from those comments.
:::

### A6. `SCION_AUTO_EXPOSE_PORTS` has its own four-tier order

`SCION_AUTO_EXPOSE_PORTS`, which turns on the in-container
[auto-expose scanner](/scion/hosted/user/port-forwarding/#auto-expose-ports), is the one
environment variable that the hub also sets from a project annotation and a hub-wide default. It
resolves in the same order as [B1](#b1-harness-configuration-model-thinking-level-and-scalar-limits),
not the storage-scope ladder above. Higher tiers win; a lower tier applies only when every tier
above it left the key unset:

| Priority | Source | Where it is recorded |
| --- | --- | --- |
| Highest | the agent-create request (`config.env`), or the auto-expose control on the agent's configure page | the agent's explicit config, so it survives reincarnate |
| | the project annotation `scion.io/auto-expose-ports-enabled` | written by the hub at create and re-derived at reincarnate, never recorded as explicit |
| | template env and harness-config env (in broker mode harness-config env wins between the two; see [harness-config env now outranks template env](#changed-in-this-release--harness-config-env-now-outranks-template-env-in-broker-mode)) | the template and harness config |
| Lowest | the hub default, `auto_expose_ports.enabled` in the hub settings | not stored on the agent; the hub sends it on every create, start and restart, and the broker applies it last |

Because the hub default is read at each dispatch, changing it changes what an agent that inherits
it gets at its next start. Reincarnate re-reads the project annotation, so an annotation changed
since the agent was created takes effect there; a value the user set explicitly carries over
unchanged.

Agents created by an older hub may still carry a project or hub value stamped into their inline
config, where it looks explicit. The rerunnable maintenance migration `auto-expose-env-normalize` (run it
from the hub admin maintenance page, or with
`POST /api/v1/admin/maintenance/migrations/auto-expose-env-normalize/run`) removes such a stamp and re-derives the value from the project and
template exactly as reincarnate would. A running agent keeps the old value in its container until
it is next provisioned or reincarnated. A run that had to skip agents (its log reports
`skipped N agent(s)`) still shows as completed, and the maintenance page does not offer completed
migrations again, so re-run it with the `POST` call above.

The other auto-expose variables (`SCION_AUTO_EXPOSE_MODE`, `SCION_AUTO_EXPOSE_PORTS_LIST`,
`SCION_AUTO_EXPOSE_INTERVAL`, `SCION_AUTO_EXPOSE_MIN_PORT`) have no project or hub tier and follow
the ordinary env rules on this page.

:::caution[Two edges of this order]
- **A storage-scope value outranks template and harness-config env.** A
  `SCION_AUTO_EXPOSE_PORTS` set with `scion hub env set` (any scope) fills the key before the
  broker applies the template and harness-config tiers, so it beats both. It still loses to an
  explicit value and to the project annotation, and it beats the hub default.
- **A local CLI start on the broker host gets no hub default.** The hub default reaches the broker
  only with a hub dispatch. An agent started from the `scion` CLI on the broker machine itself
  does not receive it; with no higher tier set, auto-expose stays off.
:::

### `Changed in this release` — harness-config env now outranks template env in broker mode

**Before:** for hub-dispatched (broker-mode) agents, template env won over harness-config env.
**After:** harness-config env wins.

This changes rank, not presence — harness-config env already reached the container. What changed
is that it is now visible to the auth pipeline before credentials are resolved, so credentials
declared in a harness config (`GOOGLE_CLOUD_PROJECT`, `CLOUD_ML_REGION` and similar) are seen by
auth detection instead of arriving too late to matter.

**Local (non-broker) mode is unchanged.** This reordering applies to hub-dispatched agents only.
Do not read it as a global reordering.

### `Changed in this release` — `profiles.<name>.env` retired (**breaking**)

**Retired.** `profiles.<name>.env` no longer injects environment variables. Environment variables
declared directly under a profile are now ignored. This applies to both the versioned and the
pre-v1 settings **schemas**, so **migrating your schema version will not restore the behaviour.**

**This is more far-reaching than it sounds.** Profile env was not merely one env source among
several — in `ResolveHarnessConfig` it was merged as the **override** argument to `mergeMaps`, so
it **outranked** `harness_configs.<hc>.env` on any shared key. In the common layout where a
project's `.scion/settings.yaml` declares its environment under a profile, **that merge was the
mechanism by which project settings outranked global settings for environment variables.**
Retiring it removes that rung, in both the resolved config and the persisted `scion-agent.json`.

#### Migration — and read the whole of it, because the closest replacement is not equivalent

The closest replacement is **`harness_configs.<hc>.env`**, a top-level key, in the same settings
file. **But it is not profile-scoped.** The two keys are scoped on *orthogonal axes*:

| Key | Scope |
| --- | --- |
| `profiles.<p>.env` | **one profile**, across every harness config resolved under it |
| `harness_configs.<hc>.env` | **one harness config**, across **every profile** |

`HarnessConfigs` is a **top-level map**, and the lookup that seeds the base config
(`baseConfig := vs.HarnessConfigs[harnessConfigName]`) does not consult the profile at all. The
profile is read afterwards, for the overrides only.

So the naive migration — *move my `profiles.dev.env` values into `harness_configs.<hc>.env`* —
does two things at once:

1. **Fan-out.** You must duplicate the values into **every** harness config the profile used. One
   key becomes N.
2. **Leak.** Those values now apply to **every other profile that uses that harness config.**

:::danger[The leak is the dangerous one, and it is silent]
If you have `dev` and `prod` profiles sharing a harness config, and you migrate a dev-scoped
endpoint or credential into `harness_configs.<hc>.env`, **it silently applies to `prod` as well.**
`prod` never opted in and nothing warns you.

Measured with two profiles sharing one harness config, `prod` setting nothing:
`dev` resolves `{MIGRATED: from-harness-configs, PROFILE_SCOPED: dev-only}` and **`prod` resolves
`{MIGRATED: from-harness-configs}`** — the migrated key, in a profile that never asked for it.

**Audit every profile that shares a harness config before you move a key into it.**
:::

**There is no per-profile, all-harness-configs equivalent.** No surviving key reproduces
`profiles.<p>.env`'s scope. If your values genuinely need to be per-profile and span harness
configs, the closest thing that survives is
`profiles.<p>.harness_overrides.<hc>.env` — which *is* profile-scoped, but must be written out
once per harness config, so it pays the fan-out cost to avoid the leak.

*This gap is stated rather than solved: nobody has established that no other mechanism recovers
per-profile scope. If one is found, this guidance changes.*

**What the migration does preserve is rank.** A `harness_configs` entry in a project's
`.scion/settings.yaml` still outranks the same entry in the global settings file, so the
global-below-project ordering that profile env used to provide is retained. Verified by
measurement, with a discriminator key set in **both** the global and the project
`harness_configs` and absent from the template.

:::caution[The key is now rejected by the schema]
`profiles.<name>.env` has been removed from the `ProfileConfig` structs and the JSON schema.
Schema validation now **rejects** settings files that include this field. If your settings
files still contain `profiles.<name>.env`, you will receive a validation error on startup.
Search your settings files and migrate any remaining values before upgrading.
:::

**Why.** In the words of the change's author: *"settings schema has gotten pretty rich, need to
pare down the number of control and injection points, profiles are already a bit problematic in
other ways in the architecture."*

:::danger[Do not read this as a precedence correction]
An earlier version of Scion's own research asserted that harness-config env *replaced* profile
env. That claim was **inverted** — profile env was the winner, not the loser.

This retirement **does not make the old claim true.** It removes the middle rank of a three-rank
order and leaves the top one standing, so *"harness-config env now takes precedence over profile
env"* is still false: `profiles.<p>.harness_overrides.<hc>.env` outranks
`harness_configs.<hc>.env` exactly as before. The mechanism was removed; the claim was not
confirmed.
:::

### `Unchanged` — `profiles.<name>.harness_overrides.<hc>.env` continues to work

**Not retired.** `profiles.<name>.harness_overrides.<hc>.env` **continues to work.** It rides a
different merge in `ResolveHarnessConfig` and is unaffected by the retirement above. It remains
the **highest-ranked** env source in harness-config resolution.

This row exists precisely *because* the previous row exists. A reader who sees
`profiles.<name>.env` retired will reasonably assume the whole `profiles` env family went with it
and rip out configuration that works. Publishing "removed" for a surviving path — or leaving a
reader to infer it — is worse than saying nothing.

The **non-env** keys on the same `harness_overrides` entry are likewise unaffected: `image`,
`user`, `auth_selected_type` and `volumes` all behave exactly as before.

#### Exactly two things read this key, and they do not agree

Production code reads `profiles.<name>.harness_overrides.<hc>.env` in **two** places, for two
different purposes, with two different scoping rules. The difference is observable, so it is worth
knowing which is which:

| Reader | What it is for | Scoping |
| --- | --- | --- |
| `ResolveHarnessConfig` | **Resolution** — the values actually injected into the agent | **Filtered.** Only the override belonging to the harness config you selected is applied. |
| `(*Server).extractRequiredEnvKeys` | **Secret discovery** — deciding which keys must be looked up or prompted for | **Unfiltered.** Every `harness_overrides` entry on the profile is walked, whichever harness config it belongs to. |

The second reader is **broker-only**: it does not run in the local, non-broker path. Its
over-collection is recorded separately under
[the required-secret scan gap](#known-gap--the-required-secret-scan-collects-keys-from-config-the-agent-will-never-use)
— the downstream effect of a spuriously-required key has **not** been traced, so that entry
deliberately stops short of calling it a bug rather than a wart.

**There is no third channel — and specifically, there is no settings-file *layering* channel.**
This matters because it looks as though there is one. `MergeSettings` composes a whole
`harness_overrides` block when layering one settings file over another: it merges `Env` per key,
appends `Volumes`, and runs `MergeResourceSpec` over `Resources`. It reads as thoroughly
load-bearing. **It is dead code.** Renaming its declaration leaves `go build ./...` green, which
is the type checker asserting that no production caller exists — a claim grep cannot make, because
`go build` excludes `_test.go` by definition and so cannot be satisfied by a fixture. Under the
identical procedure, renaming `ResolveHarnessConfig` turns the build red. So do not infer a
file-layering precedence rule for this key from reading `MergeSettings`; nothing reaches it.

:::caution[If you go looking for this in the source, two of the three matches are decoys]
The line `if override.Env != nil {` appears at **three** sites, and one grep finds all three:

- `ResolveHarnessConfig` — **this key.** The one you want.
- `(*Settings).ResolveHarness` — same key, but the method is unreachable from production code.
- `MergeScionConfig` — **a different referent entirely.** The variable is named `override` and the
  field is named `Env`, so it matches every search anyone will write for this row, but it operates
  on `api.ScionConfig.Env`, which is not a settings-file key at all.

A maintainer who greps, finds `MergeScionConfig`, and "corrects" the row above will be documenting
the wrong type.
:::

<!--
  PHASE 11 NOTE, intentionally not user-visible.
  The (*Settings).ResolveHarness profile-env deletion deliberately gets NO entry here.
  Measured: that method has zero non-test callers (positive control: its VersionedSettings
  sibling returns 5 live sites under the same grep shape, with method values, interface
  method sets and reflection-by-name each excluded first). The hunk is therefore not
  user-visible, and a breaking-change notice for an unreachable path is a false notice.
  This omission is deliberate and measured, not an oversight -- do not "fix" it by adding
  a row.

  Do NOT describe the Settings type as "pre-v1" or "the old path". "v1"/"pre-v1" names the
  FILE SCHEMA only. Settings is the current in-memory type of the CLI and broker, live at 86
  non-test construction sites. It is a live type carrying one dead method, which is why the
  reachability question had to be asked about the METHOD and not the type.
-->


### `Known gap` — a hub-supplied value for an auth-candidate key may not reach the container

For keys that belong to an `any_of` auth candidate set, `pkg/agent/run.go` **deletes** from
`opts.Env` those auth-candidate keys that the resolved auth method does not require, mirroring the
`ResolvedSecrets` filter. The container then falls back to whatever the base layer supplies.

`GOOGLE_CLOUD_PROJECT` and `CLOUD_ML_REGION` are both in this family. If you configure one through
a harness config expecting unconditional delivery, note that **delivery is conditional on the
resolved auth method.**

### `Known gap` — `runtimes.<name>.env` in `settings.yaml` has no effect

`ResolveRuntime` merges profile env into `V1RuntimeConfig.Env`, and **nothing reads that field.**
The key looks like a documented settings field and is accepted by the schema, but setting it does
nothing. It is left in place rather than removed because removing a field users may have set
believing it worked is a UX conversation, not a cleanup.

### `Known gap` — the required-secret scan collects keys from config the agent will never use

The runtime broker's `extractRequiredEnvKeys` decides which env keys the caller must supply as
secrets, by walking settings and collecting every **empty-valued** key. It over-collects in three
distinct ways, and each is independently true:

1. **It walks `profiles.<p>.env`** — which, after the retirement above, **no longer injects
   anything.** You can be prompted for a secret whose value now goes nowhere. (The field still
   parses, so the scan still finds it.)
2. **It walks every `harness_overrides` entry on the profile, with no filter on the selected
   harness config**, while resolution uses exactly one. Keys required only by an override the
   agent will never use are still demanded. This one is unaffected by the retirement — the
   `harness_overrides` path survives, so **this gap outlives the change either way.**
3. **It walks every entry in `harness_configs`**, likewise unfiltered.

*Fence on the above: this is read from the collection loops. What a spurious required-secret does
further downstream has not been traced — the over-collection is measured, the consequence is not.*

## System B — `ScionConfig` limits and resources

This governs `model`, `thinking_level`, `harness_config`, `max_turns`, `max_model_calls`, `max_duration` and
`resources`. It is **not** the ladder in System A.

### B1. Harness configuration, model, thinking level, and scalar limits

For `harness_config`, `model`, `thinking_level`, `max_turns`, `max_model_calls` and `max_duration`, higher tiers win and lower tiers fill in
only what is still unset:

| Priority | Source |
| --- | --- |
| Highest | the agent-create request / inline config |
| | the project's `scion.io/default-*` annotation (e.g., `scion.io/default-harness-config`, `scion.io/default-model`, `scion.io/default-thinking-level`) |
| | the template's `scion-agent.yaml` (for `harness_config`: only a declared `harness_config` / `default_harness_config`, never the template's `harness` type) |
| | hub `agent_defaults` — **see [Bucket 4](#bucket-4--operatoradmin-settings), the position is not settled** |
| Lowest | the broker's own `settings.yaml` defaults (e.g., `default_max_turns` / `default_max_model_calls` / `default_max_duration`) |

For `model`, one more layer sits below the template on the broker. `ProvisionAgent`
(`pkg/agent/provision.go`) uses the harness-config's own `model` field (`config.yaml`) as the base
layer, so it fills in only when nothing above it sets a model. The broker then resolves that value
through the harness-config's `model_aliases` and injects the result as `SCION_MODEL`. The codex and
gemini-cli harness-configs both declare `model: medium` this way. The hub does not apply this
default itself: it resolves only an explicit tier.

#### `Changed in this release` — a template's harness type is no longer used as a harness-config name

**Before:** When a template declared no `harness_config` / `default_harness_config`, the Hub used the template's `harness` *type* (for example `claude`) as the harness-config name. That type comes from a `harness:` field in the template's `scion-agent.yaml`, or is inferred from the template's name for any template whose name contains `claude`, `gemini`, `opencode` or `codex`. Because the template tier sits above hub `agent_defaults`, the type silently beat an operator's `agent_defaults.default_harness_config`.
**After:** Only a declared `harness_config` / `default_harness_config` fills the template tier. When the template declares neither, the hub `agent_defaults.default_harness_config` applies. If that is unset too, the Runtime Broker resolves the harness config itself (profile `default_harness_config`, then the settings `default_harness_config`), as it already did for local agents. Agents created from such templates no longer show the harness type as their harness config.
Previously such templates used the harness-config named after the type: the hub's record if it had one (the common case, since hosted mode seeds bundled harness-configs and workstation mode imports `~/.scion/harness-configs/<type>`), otherwise the broker's `harness-configs/<type>` directory. Now, if none of these defaults is set, agent creation fails with `no harness-config resolved`. A broker default also now applies to any template that declares only a harness type (a `harness:` field, or a type inferred from its name): the shipped broker settings set `default_harness_config: antigravity`, so a template named, say, `team-claude-reviewer`, or one declaring `harness: claude`, that declares no harness config moves from the hub's `claude` harness-config to that default, which may be a different harness. To keep the old behaviour, declare `default_harness_config` in the template's `scion-agent.yaml`.

#### `Changed in this release` — project `default-harness-config` correctly outranks template harness config

**Before:** The project default setting `scion.io/default-harness-config` was silently outranked by the template's own `harness_config` on both interactive and scheduled agent-create paths.
**After:** The project setting `scion.io/default-harness-config` now correctly outranks the template's harness config, matching the standard precedence ordering of the other scalar and limit settings in this ladder.

:::tip[Where the two systems agree — worth stating positively]
On this axis `runtime_broker < hub` is **true**, which is the **same** relation the environment
variable ladder now uses. Two independent systems concurring on a rung is a good outcome and is
recorded deliberately: the next person to change either one needs to know the other exists and
currently agrees.
:::

### Resources interleaves and cannot be given one rank

`resources` does **not** follow the table above. The broker's own `settings.yaml` contributes at
**three** separate ranks, **two of them above the hub tier**:

```
harness_overrides.<hc>.resources   (broker settings.yaml)   <- highest, beats even the template
        >  template resources
        >  profiles.<p>.resources  (broker settings.yaml)
        >  default_resources       (broker settings.yaml)
        >  BuiltinDefaultResources()                        <- floor

   hub agent_defaults.resources  --  DELIBERATELY NOT A RUNG IN THIS CHAIN. Its position
   moves with the hub's storage mode, and issue #623 leaves even that reading unsettled.
   See "The rank of hub agent_defaults depends on the hub's storage mode" below.
```

**One file, three ranks, and the top one outranks the template.** A reader handed
"`runtime_broker` is lowest" as one uniform rule gets two of these three backwards.

This is not a rule you can repair by flipping an inequality — there is no single "broker
`settings.yaml` resources" thing to rank.

#### Why the hub tier is missing from that chain, and why that is the same rule twice

A chain has one rung per participant, so writing the hub tier into it would **publish a settled
precedence position** — the one thing this page says twice, below, that it is not publishing. It
would also be false half the time: in Postgres mode the hub tier sits just above
`default_resources`, and in **file mode it sits at the bottom of the broker chain, *below*
`default_resources`**. A chain cannot express "this rung moves with deployment mode" any more than
it can express an interleave.

That is this section's own rule applied a second time, on a second axis. **A contribution can be
given a single rank only if it occupies a single rank.** The broker's `settings.yaml` fails that
test because one file contributes at three ranks; hub `agent_defaults` fails it because one tier
contributes at different ranks in different deployments. Same defect, same remedy: show the
positions, do not manufacture a rung.

Stated against the scalar table above: **"hub `agent_defaults` sit above the broker's own
`settings.yaml`" is true of the three scalars, and for `resources` it reaches at most
`default_resources` — and in file mode not even that.** The same broker file also supplies profile
and harness-override resources, merged *above* the template, so those beat the hub tier per field
in either mode, while broker `default_max_turns` **loses** to hub `default_max_turns`. The hub tier
sits in a different place for `resources` than it does for the three scalars, and — unlike the
scalars — it does not sit in one place even within `resources`.

This is the conservative direction and it is deliberate: hub-wide defaults should not silently
override resources a broker operator set per profile.

:::note[Scope of the ladder above]
The five ranks are stated as of this release and hold for everything it ships. They assume
`profiles.<p>.resources` and `harness_overrides.<hc>.resources` continue to exist — they are two
of the five, and the two that outrank the hub tier in **both** hub storage modes. **Nothing in
this release removes them**, and
the retirement of `profiles.<p>.env` does not extend to them: `env` and `resources` are different
fields travelling different merges.
:::

`resources` also merges **field by field**, not as a whole block, via `MergeResourceSpec`. A
template that sets only a memory limit keeps that limit and still picks up a CPU limit from a
lower tier.

The floor, `BuiltinDefaultResources()`, fills in a CPU limit only when nothing else supplied one —
an agent that reached the container with no CPU limit would otherwise be able to saturate every
core on the host. It is gated by `runtime.enforce_resource_defaults` (default `true`) if an
operator needs the previous unlimited behaviour. The floor never sits below a larger CPU request:
a larger `requests.cpu` raises it to the request, and a larger `kubernetes.resources.requests.cpu`
sets `kubernetes.resources.limits.cpu` instead when `kubernetes.resources.limits.cpu` is unset,
leaving the limit Docker and Podman use at `2`. A CPU limit set at any tier is never changed, so
keep it at or above the CPU request.

### `Known gap` — `ScionConfig.Secrets` is inert

The `secrets` field on `ScionConfig` is accepted and persisted but is not acted on.

### `Known gap` — `default-model` and the argv path

The project `scion.io/default-model` annotation and `InlineConfig.Model` do not reach the argv
path for all harnesses. A model set this way can be persisted and displayed while the harness is
launched without it.

### Container image and Kubernetes image pull policy — a separate chain from B1

`image` and `kubernetes.imagePullPolicy` are **not** part of the B1 table above: they resolve
through their own chain, defined in `ProvisionAgent`'s harness-config merge
(`pkg/agent/provision.go`) and re-resolved independently, on every `Start` call, in
`pkg/agent/run.go`:

| Priority | `image` | `kubernetes.imagePullPolicy` |
| --- | --- | --- |
| Highest | the user's **explicit request image**: the current request's `opts.Image` (CLI `--image`; a local `--config` file's `image` is promoted to it; in hub mode, the Hub's explicit image, sent on create, start and restart), **else** the request image recorded when the agent was provisioned | the user's **explicit pull policy**: the current request's inline config, **else** the create-time inline value recorded at provision (there is no per-request pull-policy flag) |
| | an **explicitly set** `profiles.<p>.harness_overrides.<name>.image`, re-resolved from current settings on every `Start` | an **explicitly set** `profiles.<p>.harness_overrides.<name>.image_pull_policy`, **only when that same override also sets an image**, re-resolved likewise |
| | the current request's inline config image, **else** the create-time inline image (for an agent provisioned at this version or later; older agents rank it at the top tier, see below) | — (covered by the top row) |
| | the template chain's own `image` — re-read from disk on every `Start`, or, if the template can no longer be resolved, the template's own value recorded at provision | the template chain's own `kubernetes.imagePullPolicy`, same rule |
| | Hub settings `harness_configs.<name>.image`, re-resolved on every `Start` | Hub settings `harness_configs.<name>.image_pull_policy` (with a profile `image_pull_policy` whose override sets no image outranking it), likewise |
| Lowest | the harness-config file's own `config.yaml` `image`, re-read on every `Start` | the harness-config file's own `image_pull_policy`, likewise |

Only an *explicit* profile override moves above the template; the plain `harness_configs.<name>`
defaults stay below it. A profile's `image_pull_policy` moves up only together with a profile
`image` on the same override, so a profile that pins an image (for example a registry digest) and
its pull policy is not undone by a template's `imagePullPolicy: Never`, while a profile that sets
only a pull policy does not override a template's image and pull-policy pair. One asymmetry
remains: the user's explicit pull policy (from an inline config) ranks above the profile's, while
an inline *image* ranks below the profile's image. A caller that sends an inline image and pull
policy but no request image can therefore get the profile's image with its own pull policy. The CLI
and the Hub never do this; both promote an explicit image to the request tier. Every time a higher tier replaces a different value from a lower tier,
`Start` logs it at Info (`image resolution: lower-tier image replaced` / `… image pull policy
replaced`, with the replaced and winning values and both tiers), so a pin losing to a higher tier
is never silent. That includes the routine case of a settings default replacing the
harness-config file's image, so expect one such line on most starts.

`image_pull_policy` only affects the Kubernetes runtime; other runtimes ignore
`kubernetes.imagePullPolicy` entirely.

Every tier is resolved fresh at `Start` time, including for a restart of an already-provisioned
agent: `Start` re-reads the current template chain, the current harness-config file, and current
settings, rather than trusting the merged `image` that `ProvisionAgent` persisted into the agent's
`scion-agent.json`. That merged value folds in whatever inline, profile, settings and file values
applied at provision time, so it is never reused as any single tier.

What `Start` cannot re-derive, `ProvisionAgent` records per source. Image provenance (including
the provisioned profile and template) is recorded broker-side: `image-provenance.json` (mode
`0600`, written atomically, versioned) in the agent directory, next to `scion-agent.json` and
outside the agent home and every container mount. For an agent with that file, every
image-affecting input comes either from the current request or from current settings, template and
harness-config files resolved with the recorded profile and template, or from this record. No
`agent-info.json` field and no container state affects the image reference (including its tag or
digest), the registry rewrite, the pull policy, the template tier or the profile override:

- `requestImage`: the user's explicit request image. A plain restart replays it at the top tier, so
  a first start and a later restart rank it identically, locally and via the Hub.
- `inlineImage` / `inlineImagePullPolicy`: the create-time inline values, used when the current
  request's inline config doesn't set the field.
- `profile`: the settings profile the agent was provisioned with. Only this profile is used to look
  up the profile `harness_overrides` image and pull policy (and the settings-tier values that
  lookup folds in) and the profile-level `image_registry` rewrite, on every start and restart,
  local or broker. The profile a restart passes, or the one saved in `agent-info.json`, does not
  change it. Empty means no profile was set or active at provision; the profile active at start
  then applies, as for any settings lookup without a profile. Broker start/restart also classify
  the runtime with this profile rather than the one saved in `agent-info.json`, both for the
  preliminary classification (which sets the default GCP metadata mode when the Hub sends none,
  the Kubernetes `assign` mapping, the hub endpoint and extra hosts) and for the runtime the agent
  runs on (which decides whether a bare image is first looked up locally, and so whether the
  `image_registry` prefix is applied). If the two ever disagree, the start or restart is refused
  with `409 Conflict`. Other uses of the saved profile are unchanged.
- `template`: the template the agent was provisioned from. When a start carries no absolute
  template path (every local restart, and every hub start or restart), the template-tier image and
  pull policy, and the harness-config directories searched for the file tier, come from this
  template.
- `templateImage` / `templateImagePullPolicy`: the template chain's
  **own** values, with nothing else folded in. `Start` uses them as the template tier only when
  the template can no longer be resolved, and warns that it did. That happens when the template
  was renamed or deleted since provision, and routinely on a hub start or restart, whose dispatch
  carries no template, on a broker with no local copy. So a profile or settings pin removed since
  provision never lingers disguised as the template's value.

`agent-info.json` keeps display copies (`AgentInfo.Image`, `.ExplicitImage`,
`.ExplicitImagePullPolicy`, `.Profile`, `.Template`) for listing and status only.

If `image-provenance.json` exists but cannot be read or parsed, or lacks its version marker, the
start or restart fails with an error asking you to re-provision the agent (`scion reincarnate`, or
delete and re-create it); a broker returns `409 Conflict`. It never falls back to
`agent-info.json`.

For a shared-workspace agent, the record lives in the broker-side external agent directory, outside
every container mount, which the broker locates from the Hub-supplied project ID. The in-project
agents directory and the workspace's project-id marker do not affect where it is read. Restarts
carry the shared-workspace flag, as starts do, so both read the same directory. A broker start or
restart of a shared-workspace agent whose external agent directory is missing fails with `409
Conflict` (re-provision); the in-project directory is not used.

Shared-workspace dispatch verifies the project identity before loading project settings. On a
shared-workspace create, start or restart, the broker compares the project identity recorded in the
workspace with the project ID the Hub sent, before any project settings are loaded and before the
provisioned profile is computed. The identity is the `.scion/project-id` marker, or, when `.scion`
is itself a marker file, the project that file names. If they disagree, the dispatch is refused
with `409 Conflict`. The workspace value is only compared, never used to choose a directory. A
`.scion` directory without a project-id marker is accepted unchanged, and project settings then
come from the in-repo `.scion` only. An unreadable or empty marker file is refused. This also
applies to a project deleted and re-created under the same name whose workspace still carries the
old marker: in shared-workspace mode that dispatch is refused with `409 Conflict`. To resolve it,
remove or correct the workspace's `.scion/project-id` (or the `.scion` marker file) so it names the
agent's Hub project, or re-link the project.

An agent provisioned before image provenance was recorded falls back to its previous behaviour,
including reading those `agent-info.json` fields: its create-time inline image ranks at the top
tier, its saved or requested profile drives the override lookup, and the merged
`scion-agent.json` value stands in for an unresolvable template.

On a local restart, the Hub-settings tier itself is resolved against the profile the agent was
actually created with when the restart supplies none (`opts.Profile == ""`), matching the
broker's own restart-dispatch behavior (`agent.GetSavedProfile`) — not silently against whatever
profile happens to be active on the machine at restart time. For the image and pull-policy tiers of an agent with
broker-side image provenance, the recorded provisioned profile is used instead, as described
above.

In hub mode, the top tier carries **only the user's explicit image**. The Hub sends it as
`Config.Image` on create (and on the re-dispatches that reuse the create request: finalize-env and
reincarnate's reprovision), and as the start/restart body's `image`. All of these come from the
agent's recorded explicit inputs: `AppliedConfig.CreateInputs`, or the live `InlineConfig` for an
agent written before `CreateInputs` existed. A configure-page Save that only echoes the current
image back writes nothing into `InlineConfig`. A template's image is **not** sent as the top tier.
On create the broker reads it from the hydrated template, at the template tier. On a later start
or restart (which carries no template) it comes from the template's own value recorded at
provision, unless a template of that name exists locally.

`AppliedConfig.Image` is the Hub's display record: filled from the template at create, then
overwritten with the image the broker reports. The broker's provision-only response reports the
image `Start` will run. The field never feeds a later dispatch, so a hub restart re-resolves the
profile-override, settings and file tiers live rather than freezing the image applied at creation.
[Reincarnating an Agent](/scion/local/agent-lifecycle/#reincarnating-an-agent)'s plan preview
approximates this image precedence from the Hub's own view. It can differ from what the broker
runs: it still starts from an image set on the Hub template record, which the broker no longer
applies, and when the broker resolves the agent's harness config itself the preview cannot look up
the profile override. The broker resolves the final image, including the profile override, and the
image it reports replaces the preview once the agent starts.

:::note[Two different processes' settings, not one]
`Start`'s tiers above are resolved against the **broker's own** `LoadEffectiveSettings` call
(`pkg/agent/run.go`, `pkg/config/settings_v1.go`) — in postgres mode this only sees DB-backed Hub
settings through the process-global overlay a co-located hub populates
(`pkg/config/settings_overlay.go`), so a **remote** broker resolves this tier from its own local
settings file only. The reincarnate plan preview, by contrast, resolves settings from the
**hub's own** `LoadEffectiveSettings` call (`pkg/hub/reincarnate_config.go`), which always sees
DB-backed settings directly, regardless of where the broker that will actually run the agent
lives. The two can disagree for a remote-broker deployment; the broker's dispatch response is
authoritative for what actually runs.
:::

#### `Changed in ptone/scion#1799` — an explicit profile harness override image beats the template image

**Before:** a template's `image` (and an inline config `image`) always outranked
`profiles.<p>.harness_overrides.<name>.image`, silently: the only trace was a debug log. In hub
mode it was worse. The Hub copied the template's image into `AppliedConfig.Image` and sent it as
`Config.Image`, which the broker maps to the top tier, so a template image was unbeatable by
anything short of an explicit `--image`. The broker's reported image was written back into the
same field, so every later re-dispatch replayed it as the top tier.

**After:** an explicitly set profile override image outranks the template and inline tiers; only
the user's explicit request image ranks above it, and every replacement is logged at Info. An
explicitly set profile `image_pull_policy` moves up with it. Profile and settings pins are never
baked into the record `Start` falls back to for an unresolvable template, so removing a pin takes
effect on the next start or restart. A user's explicit image stays the top tier on every start and
restart, locally and via the Hub. The Hub sends only the user's explicit image as the top tier
(see above), so a template image is resolved at the template tier everywhere and the broker's
reported image no longer freezes. The plain `harness_configs.<name>` defaults still rank below the
template. The broker's provision-only response reports the image `Start` will run; the reincarnate
plan preview approximates it (see above).

**Upgrade both sides:** the new precedence needs the Hub *and* the Runtime Broker at this version
or later. An older broker ignores the start/restart `image` key and still ranks a template's image
above a profile override. An older Hub still sends a template's image as `Config.Image`, the top
tier. **Upgrade the Hub first, or together with the brokers:** an agent that an older Hub creates on
an upgraded broker has the template's image recorded as its explicit request image (the older Hub
sent it as `Config.Image`), and it keeps that image at the top tier, even over a profile override,
until it is reincarnated. The broker cannot tell that image apart from a genuine request image.

**Hub template records:** an image set only on the Hub template record (its stored config), and
not in the template's `scion-agent.yaml`, is no longer applied, because the broker reads the
template image from the template's files. Put it in the template file.

**Behaviour change to watch for:** a profile that sets `harness_overrides.<name>.image` now
overrides the image of every template that uses that harness config under that profile. Remove
the override, or pass `--image`, if a template's own image must win. Conversely, a create-time
`--image` (or `--config` image) now persists across plain restarts; a later local `scion start
--image` applies to that one start only (in Hub mode, `--image` is not applied to an existing
agent). Re-create the agent to drop a create-time image.

#### `Changed in ptone/scion#2156` — Hub settings now wins over the harness-config file's image default

**Before:** `ProvisionAgent` never copied `harness_configs.<name>.image` (or a profile's
`harness_overrides.<name>.image`) into an agent's config, and `run.go`'s own image resolution
let the harness-config file's default — inherited via `ProvisionAgent`'s merge — silently
re-clobber the settings value it had itself just resolved. A hub operator could not pin an
image without moving `:latest` in their own registry. The hub's `scion reincarnate` plan
preview had the identical gap: it filled a still-empty image from the harness config's own
stored image, never from Hub settings (`pkg/hub/reincarnate_config.go`).

**After:** a Hub settings image now wins over the harness-config file default, resolved fresh on
every `Start` (see above), and the reincarnate plan preview resolves the same settings tier
before falling back to the harness config's own stored image. An explicit template or
inline-config `image` still outranks settings, matching the pre-existing rule that a per-agent
override is the most specific source of truth. `kubernetes.imagePullPolicy` gained the same
settings-level (and harness-config-file-level) default; neither existed before this change.

**Backward compatibility:** an agent created *before* this change has no `explicitImage` /
`explicitImagePullPolicy` recorded on its `agent-info.json` (the fields didn't exist yet, and a
missing field decodes to empty). If that agent's image *or* `kubernetes.imagePullPolicy` came from
an inline `--config` at create time, its next plain local restart no longer re-applies that inline
value — it falls through to the template tier (if the agent's template sets one), then Hub
settings, then the file default, where before this change the persisted (then-unconditional) value
kept it regardless of any of those. This is an accepted, narrow trade-off: the code cannot tell "a
pre-upgrade agent with no recorded inline value" apart from "an agent that never had an inline
override" from the file alone. `--image` or `--config` on a **local `scion start`** applies to that
one start only — it is never written back to `agent-info.json` (only `ProvisionAgent`, which a
plain restart does not call, records `ExplicitImage`/`ExplicitImagePullPolicy`) — so passing it
again re-pins the value for that single start but does not make it survive the *next* plain
restart. Re-create the agent to make an inline value durable across restarts going forward (a
Hub-managed agent's *hub-dispatched* restarts are unaffected either way, since the broker resends
`AppliedConfig.InlineConfig` live on every call — reincarnating isn't a remedy here, since it
requires a Hub and a local agent's restarts don't go through it).

Separately: if an agent's **template** can no longer be resolved at all on a local restart
(renamed or deleted since the agent was created), the restart falls back to the image/pull-policy
recorded at its last provision, with a warning, rather than falling through to Hub settings or the
file default — matching the behavior before this change. Since ptone/scion#1799 that recorded
value is the template's own, recorded in broker-side agent state (`image-provenance.json`), not the
merged `scion-agent.json` value, for agents provisioned at that version or later.

---

## Bucket 4 — operator/admin settings

This bucket is separate from Bucket 3 on purpose. Bucket 3's audience is *"I am launching an
agent"*; Bucket 4's is *"I am setting a floor for other people's agents"*. Merging them would put
settings you cannot change into a table of settings you can.

Two mechanisms live here: **hub `agent_defaults`** (set by a hub admin, in the admin UI or
bootstrap config) and **project annotations** (the `scion.io/*` defaults on a project).

### Null means "unset", never "set to the default"

This distinction is load-bearing and is easy to lose:

- A **null project annotation** means *"not set at project level; fall through to the next
  source."* It does **not** mean *"explicitly set to whatever the hub default happens to be."*
- A **null or zero hub `agent_default`** means *"not configured at hub level."* It does **not**
  mean *"configured to zero."*
- **Cloning a project preserves nulls.** A null in the source project stays null in the clone. A
  clone does **not** stamp the hub's current defaults into the new project at clone time.

The practical consequence: if an admin later changes a hub default, projects that left the value
unset pick up the new value, and projects that explicitly set it do not. Treating null as
equivalent to "set to the hub default value" gets this backwards.

### `Pending` — the precedence position of hub `agent_defaults` is not settled

:::caution[Intended model, and a contradiction that is not yet resolved]
**Intended:** hub `agent_defaults` are a **low-priority fallback** — near the bottom of the chain,
supplying a value only when nothing else did.

**Current implementation:** they are applied at **agent-create time**, which in practice makes
them behave as though they were at or near the top.

**These do not agree, and the resolution is not decided.** It is deferred to a follow-up
workstream. This page therefore documents the *intended* model and flags the current behaviour;
**it does not publish a settled precedence position for hub `agent_defaults`.** Do not build on
either reading. Tracked as [issue #623](https://github.com/ptone/scion/issues/623).
:::

### The rank of hub `agent_defaults` depends on the hub's storage mode

Independently of the question above, the rank differs by hub mode, and this has not previously
been stated anywhere user-facing:

| Hub mode | Rank of hub `agent_defaults` |
| --- | --- |
| **Postgres mode** | Just **above** the broker's own `settings.yaml` defaults |
| **File mode** | **Bottom** of the broker chain |

In file mode the hub sends nothing, so a co-located broker reads the same `settings.yaml` and
applies those values itself, at the bottom tier. That is deliberate: applying hub defaults in file
mode too would promote existing file-mode defaults from the bottom of the broker chain up to the
hub tier — a silent behaviour change for deployed single-node installs.

Note also the `resources` asymmetry described in
[Resources interleaves](#resources-interleaves-and-cannot-be-given-one-rank): "above the broker's
own `settings.yaml` defaults" means `default_resources` **only**. Broker profile resources and
harness overrides live in the same file and are merged *above* the hub tier.

**This is why the hub tier does not appear as a rung in the `resources` chain.** The two rows of
the table above are two *different* positions in that chain — above `default_resources` in
Postgres mode, below it in file mode — so no single rung is correct. Read the chain for the five
ranks that do not move, and read this table for where the hub tier lands in your deployment. And
read both under the `Pending` box above: **issue #623 leaves even the Postgres-mode reading
unsettled**, so neither position is something to build on yet.

### `Known gap` — hub `agent_defaults` are provision-time-only

Hub `agent_defaults` **bind once, at agent create.** Editing them and restarting an agent has no
effect, and every already-running agent keeps the values it was created with. A restart refreshes
inline config but not hub `agent_defaults`.

This is [issue #623](https://github.com/ptone/scion/issues/623) and it is the motivation for the
follow-up workstream referenced above. It is the **same symptom** as the mode-dependent-rank
problem, and any fix for the rank must not land without a fix for this one — otherwise the
corrected rank simply becomes a value that is correct at create time and stale forever after.

### `Known gap` — file mode with a *remote* broker cannot reach hub `agent_defaults`

In file mode the hub does not transmit `agent_defaults`, and a remote broker has no co-located
`settings.yaml` to read them from. That deployment shape leaves them unreachable. This is
deliberate — the alternative regresses single-node installs — but if that shape matters to you it
is a follow-up, not current behaviour.

#### `Changed in this release` — Thinking level propagation and merging corrected

**Before:** A project's `scion.io/default-thinking-level` annotation reached the agent's environment, but was not written to `scion-agent.json` because `applyProjectDefaults` did not stamp it into `AppliedConfig`. This made it invisible on both `scion agent config` and the web configure form. Additionally, any thinking level supplied via the override position (e.g., `--config`, API agent creation, or the web configure form) was silently dropped by `MergeScionConfig`.
**After:** Both bugs are resolved. The default thinking level from project settings is now correctly stamped into `AppliedConfig` (persisted in `scion-agent.json` and visible in the UI), and `MergeScionConfig` correctly preserves and merges the `ThinkingLevel` from override-position configurations.

#### `Changed in this release` — Hub default model and thinking level applied to agents

**Before:** During file-to-DB configuration seeding, the Hub silently dropped `default_model`, `default_thinking_level`, `default_max_agent_role`, and `default_agent_role` settings, failing to import them into the `hub_settings` database. Furthermore, the Hub's `applyHubAgentDefaults` logic did not actually stamp `DefaultModel` and `DefaultThinkingLevel` onto newly provisioned agents.
**After:** Both gaps are resolved. The Hub's `extractAgentDefaults` parser now correctly extracts and seeds `default_model`, `default_thinking_level`, `default_max_agent_role`, and `default_agent_role` from configuration files into the settings database without dropping fields. During agent provisioning, `applyHubAgentDefaults` successfully stamps the global `DefaultModel` and `DefaultThinkingLevel` onto agents (using an only-if-empty guard to protect custom configurations from being overwritten).

---

## Design notes

### Deleting the `!opts.BrokerMode` conjunct, and what actually shipped

An earlier design alternative — "delete the `!opts.BrokerMode` conjunct and nothing else" — was
rejected on the grounds that it would deliver profile env into hub-dispatched agents' auth
overlays under the name "harness-config env", because `ResolveHarnessConfig` merges `profile.Env`
into its own result.

**That rejection reason does not distinguish the rejected alternative from what shipped.** What
shipped deletes the conjunct **and** the explicit profile branch. Wherever a harness config is
named — the common case in a hub deployment — the two are byte-for-byte identical, and the harm
the alternative was rejected for is fully present in both. The only behavioural difference is the
case where **no** harness config is named, and that harm travelled by a different mechanism
entirely.

What actually justifies the shipped design is the deletion of the explicit profile branch: it is
the only part of profile-env retirement reachable without a breaking change, and it removes the
one path where profile env was delivered *under its own name*. It is recorded here so that nobody
reads "that alternative was rejected" as meaning the laundering was avoided. Only removing the
`profile.Env` merge itself cures it.

### One matrix row still needs a rewrite, not a re-rank

Scion's internal precedence findings matrix has a summary row that compresses two separate
precedence systems into one line. It has no correct edit: a row spanning both systems cannot be
corrected, only rewritten. It is flagged here rather than resolved, because establishing the
correct rank needs a measurement, not a reading, and the removal of `profiles.<p>.env` turns it
into a rewrite in any case.

---

## Verifying behaviour yourself

The precedence packages can be exercised directly:

```sh
go build ./...
go test ./pkg/config ./pkg/agent -count=1
go test ./pkg/hub -count=1 -timeout 45m   # slow (see below); -race needs far more memory and time
```

A whole-repo `go test ./...` passes on `main` given enough time: CI's Full Test Suite runs
`go test -timeout 45m ./...`. With the default 10-minute timeout it fails in `pkg/hub`, whose
SQLite-enabled run takes longer than that (ptone/scion#1847), so pass `-timeout 45m`.

## See also

- [Agent Configuration (`scion-agent.yaml`)](/scion/reference/agent-config/) — the field reference for
  templates and agents, including the project-settings annotations named above.
- [Admin Settings](/scion/reference/admin-settings/) — where hub `agent_defaults` are configured.
- [Harness-Specific Settings](/scion/reference/harness-settings/) — configuration consumed by the tools
  running inside the container.
- [Migrating from grove names](/scion/reference/grove-removal/) — the removed `grove`-named flags,
  environment variables, and config keys, and their `project`-named replacements.
