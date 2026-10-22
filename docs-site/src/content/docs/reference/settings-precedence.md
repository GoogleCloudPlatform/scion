---
title: Settings Precedence
description: Which setting wins when the same value is configured in more than one place.
---

Scion reads configuration from a lot of places: templates, `settings.yaml` files on the broker
and in your project, hub database scopes, project annotations, the agent-create request, and the
process environment. This page is the authoritative statement of which one wins.

## The model: a template describes the job, settings describe the environment

**`Changed in this release`** ([release notes](/scion/release-notes/#templates-describe-the-job-not-the-environment)).
A template says what the agent does. Where and how it runs comes from the harness-config,
settings and profiles, the hub and the project, never from the template. Every key in
`scion-agent.yaml` therefore belongs to one of two classes. The design is recorded in
`.design/template-settings-model.md`.

| Class | Keys | In a template | Where the value comes from |
| --- | --- | --- | --- |
| **Job keys** | `harness_config` / `default_harness_config`, `model`, `thinking_level`, `agent_instructions`, `system_prompt`, `skills`, `mcp_servers`, `command_args`, `task_flag`, `max_turns` / `max_model_calls` / `max_duration`, `services`, `secrets` (names only), `message_mode` | Set by the template; the project and the requester override it | The [job-key ladder](#the-job-key-ladder) |
| **Template env** | `env` | A fallback for the job's own tools | Applied only when no other source sets the key; see [template env](#template-env-is-a-lowest-precedence-fallback) |
| **Environment keys** | `telemetry`, `hub`, `image`, `resources`, `kubernetes`, `volumes` | **Ignored, with a warning** | Harness-config, broker settings and profiles, hub settings, project policy, and the requester's own inline config |

An environment key in a template is ignored when the template is loaded. The agent start
prints `template <name> sets environment key(s) <keys>; ignored (environment comes from
settings)`, the hub logs a warning naming the keys when the template is committed, and
`GET /api/v1/admin/validate-resources` lists the template under `templateConfigFindings` (see
the [API reference](/scion/reference/api/#admin-apiv1admin)). Uploads are not rejected. This is
a **breaking** change for templates that set these keys: move the values to settings, a profile,
the harness-config or the hub defaults. The
requester's own inline config (`--config`, the create API, the web form) can still set `image`,
`resources`, `kubernetes` and `volumes` for that one agent; its `telemetry` can only turn
telemetry on or off.

### The job-key ladder

Lowest to highest. A lower tier fills only what every higher tier left unset:

```
built-in defaults
  < harness-config base (its config.yaml)
    < broker settings (settings.yaml on the broker)
      < hub defaults (agent_defaults)
        < template (see the note below)
          < project defaults (project settings and scion.io/default-* annotations)
            < requester inline config
              < requester CLI flags (--model, --harness-config, ...)
```

The template rung is the template chain (the default template, then the named template) where
the broker resolves a key. The hub's own rungs for `model`, `thinking_level` and `harness_config`
read only the named template's `agentConfig` snapshot.

**`Changed in this release`:** the hub defaults sit **below** the template for every job key.
Before, a hub `default_model` (and `default_thinking_level`) beat the template's value; now the
hub default applies only when neither the template nor the project sets one. The project still
beats the template. Per-key details, including the harness-config name order, are in
[B1](#b1-harness-configuration-model-thinking-level-and-scalar-limits). Skills are the one
exception: they are merged by scope rather than replaced along the ladder, by the existing hub
and broker merge rules. Hub-injected skills keep travelling in the requester's inline config,
labelled by scope.

### The telemetry ladder

Lowest to highest:

```
broker settings (settings.yaml telemetry)
  < hub telemetry config (a hub default)
    < requester on/off (--enable-telemetry / --disable-telemetry; no other fields)
      < project telemetry policy
```

**`Changed in this release`:**

- A template's `telemetry:` block is ignored (it is an environment key).
- The requester can only turn telemetry on or off. Other inline telemetry fields are ignored,
  with a create warning, on every broker.
- Project telemetry policy beats `--enable-telemetry` / `--disable-telemetry` and
  `SCION_TELEMETRY_ENABLED`.
- Hub telemetry defaults and project policy travel to the broker as separate tiers on every
  create, start and restart, to every broker. They are never written into the agent's inline
  config or sent inline. A broker older than the `templateTiers` capability ignores both tiers,
  so its agents get only its own settings telemetry plus the requester's on/off; the hub logs a
  warning once per such broker.
- Broker settings telemetry is applied at each start and is not persisted in the agent's
  `scion-agent.json`.

### Template env is a lowest-precedence fallback

**`Changed in this release`.** Before, template `env:` values were merged into the agent's
config and beat settings harness-config env. Now a template's `env:` block is its own layer,
for the job's own tools:

- **Lowest precedence.** Each variable is set only when no other source sets the key. That
  includes every [System A](#system-a--environment-variables) source (hub, project, user and
  broker scopes, request env, harness-config env) and the env the runtime adds itself: resolved
  auth env, harness env, telemetry env and env-type secrets.
  A source whose value is empty adds no entry, so the template fills such a key.
- **Literal values.** No `${VAR}` expansion against the broker host and no passthrough of an
  empty value from the host. An empty value is skipped with a warning; a value containing
  `${...}` is used as written, with a warning.
- **`SCION_*` keys are ignored.** That namespace belongs to the platform.
- **Never a credential.** Every env key the harness-config's auth metadata lists, for any auth
  type, is never filled from a template, even when nothing else sets it: the `required_env[].any_of`
  keys, the `required_files[].alternative_env_keys` and the `autodetect` env keys. So a template
  never supplies or selects a credential. Template env does not count toward auth detection or
  the env-gather required keys.
- **Live.** It is never written into `scion-agent.json`. It is recomputed from the current
  template on every start, so a template edit takes effect on the agent's next start. If the
  template can no longer be found, the start warns and applies no template env.
- **Existing agents.** A key baked into an older agent's stored config with the same value as
  the live template is treated as the template fallback from its next start. Nothing on disk is
  rewritten.
- The hub-only `TZ` rule applies to template env like any broker-local layer.

## Read this first: there are two precedence systems, not one

The single most common mistake with Scion settings is to assume there is **one** ranked list of
sources that governs everything. There is not. There are **two independent systems**, and they
rank the same-named sources **differently**:

| System | Governs | Where the order is defined |
| --- | --- | --- |
| **A — Environment variables** | custom environment variables injected into the agent | `envScopesInPrecedenceOrder` in `pkg/hub/httpdispatcher.go`, plus the config seeding in the same file |
| **B — `ScionConfig`** | model, thinking level, `max_turns`, `max_model_calls`, `max_duration`, and `resources` | the ladder in `ResolveHarnessConfig` and `pkg/agent/provision.go` |

Telemetry and template `env:` have their own rules, stated in [the model](#the-model-a-template-describes-the-job-settings-describe-the-environment)
above; neither is a rung of System A or System B.

They are documented in two separate tables below, [System A](#system-a--environment-variables) and
[System B](#system-b--scionconfig-job-keys-limits-and-resources), and **the tables are not
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
(`pkg/config/settings_v1.go`; `LoadSettings` and `LoadEffectiveSettings` both read through its layers via `loadVersionedSettings` in `pkg/config/settings_view.go`), loaded **last** — after the embedded defaults, the global
`~/.scion/settings.yaml`, the in-repo `.scion/settings.yaml`, and any external project config.
Legacy-format settings files (those without `schema_version`) are converted to the v1 shape at
their own layer when read, keeping only keys whose legacy source key the file sets, and a versioned file's legacy camelCase `hub.projectId` is read as a fallback when
`hub.project_id` is absent. Loaded last means **highest priority**: a `SCION_*` variable in the
broker's process environment overrides every settings file. An exported but empty `SCION_*`
variable counts as unset, so it never clears a value from a settings file. Inside a Hub-managed agent container (`SCION_AGENT_ID` set), `SCION_WORKSPACE_PATH` and `SCION_RUNTIME`, which the runtime sets for the agent, are ignored as settings overrides, so they do not override `workspace_path` or `runtime`. The retired
`SCION_HUB_BROKER_ID`, `SCION_HUB_BROKER_TOKEN`, `SCION_HUB_BROKER_NICKNAME`, and `SCION_BUCKET_*`
variables are not read.

```
embedded defaults  <  ~/.scion/settings.yaml  <  .scion/settings.yaml
                   <  external project config  <  SCION_* process env
```

Key names are mapped by `versionedEnvKeyMapper`: the `SCION_` prefix is stripped, the remainder is
lowercased, and `_` becomes a settings path separator — so `SCION_HUB_PROJECT_ID` sets
`hub.project_id`.

`SCION_HUB_PROJECT_ID` also outranks the project's `.scion/project-id` file, and a warning names
both sources when they disagree. `SCION_PROJECT_ID` ranks lower: it sets the top-level
`project_id`, and a `hub.project_id` from the settings files or the `project-id` file wins over it.
When both variables are set, `SCION_HUB_PROJECT_ID` wins.

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
| `SCION_CLI_MODE` | always `agent` inside a container (outside one, it outranks the `cli.mode` setting; see below) | **Unconditional** — overwrites |
| `SCION_MAX_TURNS` | from the resolved `ScionConfig` | **Unconditional** — overwrites the hub-supplied value |
| `SCION_MAX_MODEL_CALLS` | from the resolved `ScionConfig` | **Unconditional** — overwrites the hub-supplied value |
| `SCION_MAX_DURATION` | from the resolved `ScionConfig` | **Unconditional** — overwrites the hub-supplied value |
| `SCION_WORKSPACE_MODE` | canonical workspace sharing mode (`shared-plain`, `clone-per-agent`, `worktree-per-agent`, or `empty-per-agent`) | **Unconditional** — overwrites |
| `SCION_WORKSPACE_GIT` | `"true"` when the workspace is a git repository, absent otherwise | **Unconditional** — overwrites |
| `SCION_TEMPLATE` | full template reference, for debugging | Set only when a template reference exists |
| `SCION_BROKER_NAME` | broker name, defaults to `local` | **Guarded** — defers to an existing value |
| `SCION_HARNESS` | harness name | **Guarded** — defers to an existing value |
| `SCION_MODEL` | resolved model; also the harness's `--model` argument | **Guarded** — defers to an existing value (the hub-resolved model, or an explicit `--env`); a size alias in it is resolved |
| `SCION_THINKING_LEVEL` | resolved thinking level | **Guarded** — defers to an existing value |
| `SCION_CREATOR` | OS user who created the agent | **Guarded** — defers to an existing value |

**Why the split matters.** The guarded set is how a hub-resolved value reaches the container: the
hub sends it, the broker sees the key is already present, and leaves it alone. The unconditional
set is not overridable in any useful sense — if you want a different `max_turns`, change the
setting that feeds `ScionConfig.MaxTurns` (see [System B](#system-b--scionconfig-job-keys-limits-and-resources)),
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

The CLI mode itself is chosen as `SCION_CLI_MODE`, then the `cli.mode` setting (from the
merged settings files), then `human`. `scion config set cli.mode agent` therefore switches
every later command on that machine (or in that project) to agent mode, and agent mode removes
the `config` commands, so `scion config unset cli.mode` is then unavailable. To return, run
`SCION_CLI_MODE=human scion config unset [--global] cli.mode`, or remove `mode` from the `cli`
section of the settings file by hand.

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

:::note[Do not overstate the parallel — same path, different shape]
The three-ranks-in-one-file shape appears on both axes. The env injection in `pkg/agent/run.go`
(`resolveAuthEnvOverlay`) is no longer gated on `!opts.BrokerMode`: it runs on broker starts too,
and it only fills keys that the start's env (hub-supplied values included) has not already set. The
CLI no longer takes a non-broker path (local mode was removed); every agent now starts through a
broker. So the env sub-ladder in A3 and the `resources` interleave in
[System B](#resources-interleaves-and-cannot-be-given-one-rank) both run on the **broker /
provisioning** path. They differ in shape, not in mode: the env sub-ladder collapses into one
only-if-absent tier, and **only `resources` interleaves with the hub tier.**
:::

### A5. Secrets rank `user` and `project` in the opposite direction

Secrets are resolved by a **different** ladder from environment variables, and the two disagree:

```
env vars:   runtime_broker  <  hub  <  project  <  user           (user wins)
secrets:    hub  <  user  <  project  <  runtime_broker           (project wins)
```

For environment variables, **`user` beats `project`**. For secrets, **`project` beats `user`** —
and `runtime_broker` is still highest for secrets.

This is a **known difference, not a documented design**. Nobody has established that the
divergence is intentional, so this page does not present it as a second designed ladder.

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
| | harness-config env | the harness config |
| Lowest | the hub default, `auto_expose_ports.enabled` in the hub settings | not stored on the agent; the hub sends it on every create, start and restart, and the broker applies it after the tiers above |

Because the hub default is read at each dispatch, changing it changes what an agent that inherits
it gets at its next start. Reincarnate re-reads the project annotation, so an annotation changed
since the agent was created takes effect there; a value the user set explicitly carries over
unchanged.

A template cannot set `SCION_AUTO_EXPOSE_PORTS`: it is a `SCION_*` key, which template env
ignores (`Changed in this release`: template env used to be a tier of this order).

Agents created by an older hub may still carry a project or hub value stamped into their inline
config, where it looks explicit. The rerunnable maintenance migration `auto-expose-env-normalize` (run it
from the hub admin maintenance page, or with
`POST /api/v1/admin/maintenance/migrations/auto-expose-env-normalize/run`) removes such a stamp and re-derives the value from the project
exactly as reincarnate would. A running agent keeps the old value in its container until
it is next provisioned or reincarnated. A run that had to skip agents (its log reports
`skipped N agent(s)`) still shows as completed, and the maintenance page does not offer completed
migrations again, so re-run it with the `POST` call above.

The other auto-expose variables (`SCION_AUTO_EXPOSE_MODE`, `SCION_AUTO_EXPOSE_PORTS_LIST`,
`SCION_AUTO_EXPOSE_INTERVAL`, `SCION_AUTO_EXPOSE_MIN_PORT`) have no project or hub tier and follow
the ordinary env rules on this page.

:::caution[An edge of this order]
- **A storage-scope value outranks harness-config env.** A
  `SCION_AUTO_EXPOSE_PORTS` set with `scion hub env set` (any scope) fills the key before the
  Runtime Broker applies the harness-config tier, so it beats it. It still loses to an
  explicit value and to the project annotation, and it beats the hub default.
:::

### `Changed in this release` — template env is a fallback below every other source

**Before:** template env was merged into the agent's config. For hub-dispatched (broker-mode)
agents an earlier change had already put harness-config env above it, so that credentials
declared in a harness config (`GOOGLE_CLOUD_PROJECT`, `CLOUD_ML_REGION` and similar) reach auth
detection; in local mode template env still beat harness-config env.
**After:** template env is a separate, lowest-precedence fallback on every path. It never
enters the agent's config, its values are literal, `SCION_*` keys are ignored, and no env key
the harness-config's auth metadata lists is ever filled from it. See
[Template env is a lowest-precedence fallback](#template-env-is-a-lowest-precedence-fallback)
for the full rule.

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
Saving the profiles section of the Hub's admin server settings also rejects it, along with any
other key a profile has no field for.
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
| `(*runtimebroker.Server).extractRequiredEnvKeys` | **Secret discovery** — deciding which keys must be looked up or prompted for | **Unfiltered.** Every `harness_overrides` entry on the profile is walked, whichever harness config it belongs to. |

The second reader is **broker-only** and runs only in the broker's create-time env-gather
preflight (`createAgent`, when the request asks for env-gather and auth is on). It does not run on
start or restart. Its
over-collection is recorded separately under
[the required-secret scan gap](#known-gap--the-required-secret-scan-collects-keys-from-config-the-agent-will-never-use)
— the downstream effect of a spuriously-required key has **not** been traced, so that entry
deliberately stops short of calling it a bug rather than a wart.

**There is no third channel — and specifically, there is no settings-file *layering* channel.**
Settings files are merged by the one settings loader as plain data, and no code composes a
`harness_overrides` block across files (the former `MergeSettings` layering helper, which never
had a production caller, has been removed). So there is no file-layering precedence rule for this
key beyond the ordinary last-layer-wins merge of the loader.

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

## System B — `ScionConfig` job keys, limits and resources

This governs `model`, `thinking_level`, `harness_config`, `max_turns`, `max_model_calls`, `max_duration`
(the job keys of the [job-key ladder](#the-job-key-ladder)) and `resources`, which is an environment
key with its own chain. It is **not** the ladder in System A.

### B1. Harness configuration, model, thinking level, and scalar limits

For `harness_config`, `model`, `thinking_level`, `max_turns`, `max_model_calls` and `max_duration`, higher tiers win and lower tiers fill in
only what is still unset:

| Priority | Source |
| --- | --- |
| Highest | the requester's explicit CLI flags (`--model`, `--harness-config`, ...) |
| | the agent-create request / inline config |
| | the project's `scion.io/default-*` annotation (e.g., `scion.io/default-harness-config`, `scion.io/default-model`, `scion.io/default-thinking-level`) |
| | the template's `scion-agent.yaml` (for `harness_config`: only a declared `harness_config` / `default_harness_config`, never the template's `harness` type) |
| | hub `agent_defaults` (`default_harness_config`, `default_model`, `default_thinking_level`, resolved by the hub at create; `default_max_turns`, `default_max_model_calls`, `default_max_duration`, applied by the broker on every start in a database-backed hub; in file mode the hub sends nothing and the broker's `settings.yaml` defaults apply alone) |
| | the broker's own `settings.yaml` defaults (e.g., `default_max_turns` / `default_max_model_calls` / `default_max_duration`) |
| Lowest | the harness-config's own `config.yaml` (for `model`; see below) |

When a template declares both `harness_config` and `default_harness_config`,
`default_harness_config` wins, on the hub and on the broker (`config.ResolveHarnessConfigName`).

For `model`, the harness-config sits below everything on the broker. `ProvisionAgent`
(`pkg/agent/provision.go`) uses the harness-config's own `model` field (`config.yaml`) as the base
layer, so it fills in only when nothing above it sets a model. The broker then resolves that value
through the harness-config's `model_aliases` and injects the result as `SCION_MODEL`. The codex and
gemini-cli harness-configs both declare `model: medium` this way. The hub does not apply this
default itself: it resolves only an explicit tier.

The model reaches the harness through one channel. When the hub resolved a model (any tier from
the request down to the hub default), it sends it as `SCION_MODEL`, and the broker uses that value
over its own config, the harness-config default included. The broker passes the same value to the
harness as `--model`, so the environment and the command line never disagree. A size alias the hub
could not resolve is resolved on the broker through the harness-config's `model_aliases` (or the
harness's built-in table), never replaced by the broker's own model. An explicit `SCION_MODEL` in
the request env (`--env`) also wins and reaches the command line.

Because the hub `agent_defaults.default_model` now reaches the harness command line, it must be a
model every harness that can use it accepts. A size alias (`small`, `medium`, `large`,
`extra-large`) is portable; a concrete model ID of one vendor (for example a Claude model on a
gemini-cli agent) makes that agent fail at launch.

#### `Changed in this release` — argv `--model` follows the hub-resolved model

**Before:** the broker built the harness's `--model` argument from its own config (the request's
inline model, else its template or the harness-config default), while `SCION_MODEL` carried the
hub-resolved model. A project or hub default model was therefore shown and exported as
`SCION_MODEL` while the harness ran with the harness-config default (for example the `medium`
model for codex and gemini-cli), and a size alias the hub could not resolve was replaced by the
broker's own model.
**After:** both carry one effective model: `SCION_MODEL` when set (hub-resolved or explicit),
else the broker's own config model, with any alias resolved
([ptone/scion#3914](https://github.com/ptone/scion/issues/3914)).

#### `Changed in this release` — hub default model and thinking level sit below the template

**Before:** the hub stamped `agent_defaults.default_model` and `default_thinking_level` into the
agent's applied config whenever the request and the project left them unset. The broker ranks
that config above the template, so the hub default beat a model or thinking level the template
set.
**After:** the hub applies the template's `model` and `thinking_level` (from the template's
read-only `agentConfig`) after the project defaults and before its own defaults. The hub default
applies only when neither the template nor the project sets a value; the project still beats
the template. `harness_config` and the three limits already followed this order.

#### `Changed in this release` — `default_harness_config` beats `harness_config` in a template

**Before:** the hub derived a template's default harness config with its own key order, which
could pick `harness_config` when both keys were set, while the broker picked
`default_harness_config`.
**After:** the hub derives it with the broker's parser and key order, so `default_harness_config`
wins everywhere. On the hub, a template's harness type (shown in template lists) comes from an
explicit `harness:` key before it is inferred from the template's name.

#### `Changed in this release` — a template's harness type is no longer used as a harness-config name

**Before:** When a template declared no `harness_config` / `default_harness_config`, the Hub used the template's `harness` *type* (for example `claude`) as the harness-config name. That type comes from a `harness:` field in the template's `scion-agent.yaml`, or is inferred from the template's name for any template whose name contains `claude`, `gemini`, `opencode` or `codex`. Because the template tier sits above hub `agent_defaults`, the type silently beat an operator's `agent_defaults.default_harness_config`.
**After:** Only a declared `harness_config` / `default_harness_config` fills the template tier. When the template declares neither, the hub `agent_defaults.default_harness_config` applies. If that is unset too, the Runtime Broker resolves the harness config itself (profile `default_harness_config`, then the settings `default_harness_config`), as it already did when no hub default applied. Agents created from such templates no longer show the harness type as their harness config.
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

`resources` does **not** follow the table above. It is an environment key: a template's
`resources:` block is ignored (`Changed in this release`; before, the template was a rung of this
chain). The broker's own `settings.yaml` contributes at **three** separate ranks, **two of them
above the hub tier**:

```
harness_overrides.<hc>.resources   (broker settings.yaml)   <- highest
        >  the agent's own resources  (the requester's inline config, plus the
                                       project scion.io/default-resources-* the hub stamps into it)
        >  profiles.<p>.resources  (broker settings.yaml)
        >  hub agent_defaults.resources                     <- database-backed hubs only (not file mode); see below
        >  default_resources       (broker settings.yaml)
        >  BuiltinDefaultResources()                        <- floor
```

The whole chain is resolved again on **every start** (`resolveEnvironmentResources` in
`pkg/agent/agent_environment.go`), and only the agent's own resources are written to its
`scion-agent.json`. **`Changed in this release`:** settings, profile and hub resources used to be
baked into `scion-agent.json` at provision; now a change to any of them takes effect on the
agent's next start. The hub sends `agent_defaults.resources` on create, start and restart.
Agents provisioned before this change keep their baked resources until they are reprovisioned.

**One file, three ranks, and the top one outranks the agent's own value.** A reader handed
"`runtime_broker` is lowest" as one uniform rule gets two of these three backwards.

This is not a rule you can repair by flipping an inequality — there is no single "broker
`settings.yaml` resources" thing to rank.

#### Where the hub tier sits, and why it moves with the storage mode

In a database-backed hub (SQLite or Postgres, not file mode) the hub sends `agent_defaults.resources` and the broker merges it just above
`default_resources`. In **file mode the hub sends nothing**: a co-located broker reads the same
`settings.yaml` itself, so those values land at `default_resources`, the bottom of the broker
chain. See [The rank of hub `agent_defaults` depends on the hub's storage
mode](#the-rank-of-hub-agent_defaults-depends-on-the-hubs-storage-mode).

Stated against the scalar table above: **"hub `agent_defaults` sit above the broker's own
`settings.yaml`" is true of the three scalars, and for `resources` it reaches at most
`default_resources`.** The same broker file also supplies profile and harness-override resources,
merged *above* the hub tier, while broker `default_max_turns` **loses** to hub
`default_max_turns`. This is the conservative direction and it is deliberate: hub-wide defaults
should not silently override resources a broker operator set per profile.

`resources` also merges **field by field**, not as a whole block, via `MergeResourceSpec`. An
inline config that sets only a memory limit keeps that limit and still picks up a CPU limit from a
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

### Container image and Kubernetes image pull policy — a separate chain from B1

`image` and `kubernetes.imagePullPolicy` are environment keys, **not** part of the B1 table
above. They resolve through their own chain, on every `Start` call, in `pkg/agent/run.go`. In
short, lowest to highest: harness-config file < settings < (inline config) < profile override <
request `--image`.

**`Changed in this release`:** the template tier is gone. A template's `image` and
`kubernetes.imagePullPolicy` are ignored with a warning, the hub no longer copies a template's
image into an agent, and settings- and profile-derived images and pull policies are no longer
written into `scion-agent.json`.

| Priority | `image` | `kubernetes.imagePullPolicy` |
| --- | --- | --- |
| Highest | the user's **explicit request image**: the current request's `opts.Image` (CLI `--image`; a local `--config` file's `image` is promoted to it; the Hub's explicit image, sent on create, start and restart), **else** the request image recorded when the agent was provisioned | the user's **explicit pull policy**: the current request's inline config, **else** the create-time inline value recorded at provision (there is no per-request pull-policy flag) |
| | an **explicitly set** `profiles.<p>.harness_overrides.<name>.image`, re-resolved from current settings on every `Start` | an **explicitly set** `profiles.<p>.harness_overrides.<name>.image_pull_policy`, **only when that same override also sets an image**, re-resolved likewise |
| | the current request's inline config image, **else** the create-time inline image (for an agent provisioned at this version or later; older agents rank it at the top tier, see below) | — (covered by the top row) |
| | *legacy only:* for an agent provisioned before this release whose template can no longer be resolved, the template's own `image` recorded at its provision (with a start warning) | *legacy only:* the template's own `kubernetes.imagePullPolicy`, same rule |
| | Hub settings `harness_configs.<name>.image`, re-resolved on every `Start` | Hub settings `harness_configs.<name>.image_pull_policy` (with a profile `image_pull_policy` whose override sets no image outranking it), likewise |
| Lowest | the harness-config file's own `config.yaml` `image`, re-read on every `Start` | the harness-config file's own `image_pull_policy`, likewise |

Only an *explicit* profile override moves above the inline config; the plain
`harness_configs.<name>` defaults stay below it. A profile's `image_pull_policy` moves up only
together with a profile `image` on the same override, so a profile that pins an image (for
example a registry digest) also pins its pull policy, while a profile that sets only a pull policy
ranks at the settings tier. One asymmetry
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
agent: `Start` re-reads the current harness-config file and current settings. `ProvisionAgent`
writes only the requester's own `image` (the inline config's) into the agent's
`scion-agent.json`, never a settings, profile or harness-config file value. An agent provisioned
before this release may still carry a merged `image` there, which folds in whatever inline,
profile, settings and file values applied at provision time; it is never reused as any single
tier, except as the legacy stand-in described below.

What `Start` cannot re-derive, `ProvisionAgent` records per source. Image provenance (including
the provisioned profile and template) is recorded broker-side: `image-provenance.json` (mode
`0600`, written atomically, versioned) in the agent directory, next to `scion-agent.json` and
outside the agent home and every container mount. For an agent with that file, every
image-affecting input comes either from the current request or from current settings and
harness-config files resolved with the recorded profile and template, or from this record. No
`agent-info.json` field and no container state affects the image reference (including its tag or
digest), the registry rewrite, the pull policy or the profile override:

- `requestImage`: the user's explicit request image. A plain restart replays it at the top tier, so
  a first start and a later restart rank it identically.
- `inlineImage` / `inlineImagePullPolicy`: the create-time inline values, used when the current
  request's inline config doesn't set the field.
- `profile`: the settings profile the agent was provisioned with. Only this profile is used to look
  up the profile `harness_overrides` image and pull policy (and the settings-tier values that
  lookup folds in) and the profile layers of the `image_registry` rewrite (see
  [Image registry order](#image-registry-order) for the full order), on every start and restart.
  The profile a restart passes, or the one saved in `agent-info.json`, does not
  change it. Empty means no profile was set or active at provision; the profile active at start
  then applies, as for any settings lookup without a profile. Broker start/restart also classify
  the runtime with this profile rather than the one saved in `agent-info.json`, both for the
  preliminary classification (which sets the default GCP metadata mode when the Hub sends none,
  the Kubernetes `assign` mapping, the hub endpoint and extra hosts) and for the runtime the agent
  runs on (which decides whether a bare image is first looked up locally, and so whether the
  `image_registry` prefix is applied). If the two ever disagree, the start or restart is refused
  with `409 Conflict`. Other uses of the saved profile are unchanged.
- `template`: the template the agent was provisioned from. When a start carries no absolute
  template path (every start or restart), the harness-config
  directories searched for the file tier come from this template.
- `templateImage` / `templateImagePullPolicy`: read-only legacy fields. New agents record none,
  because a template no longer sets an image. For an agent provisioned before this release they
  hold the template chain's **own** values, with nothing else folded in, and `Start` replays them
  only when the template can no longer be resolved, with a warning. That happens when the template
  was renamed or deleted since provision, and routinely on a hub start or restart on a broker with
  no local copy. So a profile or settings pin removed since provision never lingers disguised as
  the template's value.

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

On every start and restart, the Runtime Broker sets the start's profile to the one saved in
`agent-info.json` (`agent.GetSavedProfile`; a flat broker has no profiles and skips this), so the
Hub-settings tier resolves against the profile the agent was created with, never against whatever
profile happens to be active on the broker at restart time. For the image and pull-policy tiers of an agent with
broker-side image provenance, the recorded provisioned profile is used instead, as described
above.

The top tier carries **only the user's explicit image**. The Hub sends it as
`Config.Image` on create (and on the re-dispatches that reuse the create request: finalize-env and
reincarnate's reprovision), and as the start/restart body's `image`. All of these come from the
agent's recorded explicit inputs: `AppliedConfig.CreateInputs`, or the live `InlineConfig` for an
agent written before `CreateInputs` existed. A configure-page Save that only echoes the current
image back writes nothing into `InlineConfig`. A template's image is never sent and never
applied.

`AppliedConfig.Image` is the Hub's display record of the image the broker reports. The Hub no
longer fills it from the template at create (`Changed in this release`). The broker's provision-only response reports the
image `Start` will run. The field never feeds a later dispatch, so a hub restart re-resolves the
profile-override, settings and file tiers live rather than freezing the image applied at creation.
[Reincarnating an Agent](/scion/local/agent-lifecycle/#reincarnating-an-agent)'s plan preview
approximates this image precedence from the Hub's own view. It can differ from what the broker
runs: when the broker resolves the agent's harness config itself the preview cannot look up the
profile override. The broker resolves the final image, including the profile override, and the
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

#### `Changed in this release` — templates no longer set the image

**Before:** a template's `image` and `kubernetes.imagePullPolicy` were a tier of this chain,
between the inline config and Hub settings, re-read from the template on every start.
**After:** they are ignored with a warning, like every environment key. The image comes from the
harness-config file, settings, the profile override or the requester. Settings- and
profile-derived images and pull policies are applied at each start and no longer written into
`scion-agent.json`. An agent provisioned before this release keeps its recorded template image
only as the legacy stand-in for an unresolvable template (see the table above). To keep a custom
image for a role, set it in a harness-config (`config.yaml` `image`) or in settings
(`harness_configs.<name>.image`, or a profile `harness_overrides.<name>.image`).

The two sections below record earlier changes to this chain. Their mentions of a template tier
describe behaviour before this release.

#### `Changed in GoogleCloudPlatform/scion#2646` — an explicit profile harness override image beats the template image

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
restart. The Hub sends only the user's explicit image as the top tier
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
template image from the template's files. (Since this release a template's image is ignored altogether; put it in a
harness-config or in settings.)

**Behaviour change to watch for:** a profile that sets `harness_overrides.<name>.image` now
overrides the image of every template that uses that harness config under that profile. Remove
the override, or pass `--image`, if a template's own image must win. Conversely, a create-time
`--image` (or `--config` image) now persists across plain restarts. A later `scion start --image`
is not applied to an existing agent; the CLI warns that the flag is ignored. Re-create the agent to
change or drop a create-time image.

#### `Changed in GoogleCloudPlatform/scion#2074` — Hub settings now win over the harness-config file's image default

**Before:** `ProvisionAgent` never copied `harness_configs.<name>.image` (or a profile's
`harness_overrides.<name>.image`) into an agent's config, and `run.go`'s own image resolution
let the harness-config file's default — inherited via `ProvisionAgent`'s merge — silently
re-clobber the settings value it had itself just resolved. A hub operator could not pin an
image without moving `:latest` in their own registry. The hub's `scion reincarnate` plan
preview had the identical gap: it filled a still-empty image from the harness config's own
stored image, never from Hub settings (`pkg/hub/reincarnate_config.go`).

**After:** a Hub settings image now wins over the harness-config file default, resolved fresh on
every `Start` (see above), and the reincarnate plan preview resolves the same settings tier
before falling back to the harness config's own stored image. An explicit inline-config `image`
still outranks settings (a template's `image` is now ignored), matching the pre-existing rule that a per-agent
override is the most specific source of truth. `kubernetes.imagePullPolicy` gained the same
settings-level (and harness-config-file-level) default; neither existed before this change.

**Backward compatibility:** an agent created *before* this change has no `explicitImage` /
`explicitImagePullPolicy` recorded on its `agent-info.json` (the fields didn't exist yet, and a
missing field decodes to empty). If that agent's image *or* `kubernetes.imagePullPolicy` came from
an inline `--config` at create time, a plain *local* restart (local restarts have since been
removed) no longer re-applied that inline value — it fell through to the template tier, then Hub
settings, then the file default. This was an accepted, narrow trade-off: the code cannot tell "a
pre-upgrade agent with no recorded inline value" apart from "an agent that never had an inline
override" from the file alone. A restart through the Hub is unaffected: the Hub resends the
agent's recorded explicit image and inline config on every dispatch, so the value is re-applied.
A later `scion start --image` is not applied to an existing agent (the CLI warns that the flag is
ignored), so re-create the agent to change a create-time image.

Separately: if an agent's **template** can no longer be resolved at all on a restart
(renamed or deleted since the agent was created), the restart falls back to the image/pull-policy
recorded at its last provision, with a warning, rather than falling through to Hub settings or the
file default — matching the behavior before this change. Since GoogleCloudPlatform/scion#2646
that recorded value is the template's own, recorded in broker-side agent state
(`image-provenance.json`), not the merged `scion-agent.json` value, for agents provisioned at
that version or later. A template's `image` is now ignored, so new agents record no template
value; values recorded for older agents are still replayed.

### Image registry order

Introduced by GoogleCloudPlatform/scion#3786. A bare image name (one without a registry host,
such as `scion-claude:latest`) is rewritten to `<image_registry>/<name>`. Every place that
resolves `image_registry` uses one order: the source decides first, and within a source the
more specific scope wins.

| Rank | Layer | Where it comes from |
| :--- | :--- | :--- |
| 1 | Node env | `SCION_IMAGE_REGISTRY` of the process that runs the agent (a Runtime Broker), or of the Hub for Hub-wide reads |
| 2 | Hub profile | `profiles.<name>.image_registry` saved in the Hub's settings |
| 3 | Hub top level | `image_registry` saved in the Hub's settings (for example through the setup wizard or the admin settings page) |
| 4 | File profile or instance | `profiles.<name>.image_registry` in the settings file, or a flat instance's `agent_defaults.image_registry` |
| 5 | File top level | `image_registry` in the settings file |
| 6 | Default | none: images are not rewritten |

What this means in practice:

- `SCION_IMAGE_REGISTRY` on a Runtime Broker overrides every settings value for the agents that
  broker runs. It is not a settings value: `scion config get image_registry` and `scion init` do
  not show or save it.
- A saved Hub top-level value beats a settings-file profile or instance value. Only a saved Hub
  profile value beats it. To keep a per-profile registry, save it in the Hub profile.
- Only values an admin saved count as Hub values. The values the Hub seeds from its settings file
  at startup are file values.
- On the Hub, the file tier is the Hub's startup merge: coded defaults, then
  `SCION_SEED_IMAGEREGISTRY`, then `settings.yaml`, then `SCION_SERVER_IMAGEREGISTRY` (each later
  one wins). A saved Hub value beats all of them.
- Hub-wide reads (setup status, image pull, harness image status, maintenance builds) use the Hub
  process's own `SCION_IMAGE_REGISTRY` as the node env and do not use profile values. Under HA,
  each replica uses its own environment for these reads.
- When the Hub rewrites an agent's explicit image for a dispatch, it uses only the target Runtime
  Broker's reported `SCION_IMAGE_REGISTRY`, never the Hub's own. For a remote Runtime Broker, the
  file tiers are the Hub's own settings file; images taken from a template are resolved by the
  Runtime Broker with its own settings file. For a flat Runtime Broker the Hub does not rewrite the
  image, and the Runtime Broker resolves it with its instance value.
- `SCION_MAINTENANCE_IMAGE_REGISTRY` applies only to maintenance image builds. It is not an agent
  image registry, and a Runtime Broker does not start with it as the only registry. The Hub logs a
  warning at startup when it is set and differs from the resolved value.

A Hub value is saved only when an admin changes it. Saving an unrelated setting, or saving a page
that echoes the value it shows, stores nothing. At the top level, the field shows the saved value,
or else this Hub replica's settings-file value, and never an environment value. Saving the value
shown does not pin it; change it once to pin it. A profile's field shows only a saved value, so
typing any value into it saves that value.

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

### Where hub `agent_defaults` rank

**`Changed in this release`**: Hub `agent_defaults` are a **low-priority fallback**: for every job
key they sit below the template and the project, and above only the broker's own
`settings.yaml` defaults and the harness-config base (see [the job-key
ladder](#the-job-key-ladder)). Before, a hub `default_model` or `default_thinking_level` was
stamped into the agent's applied config at create, where it beat the template.

- **Limits and `resources`** (`default_max_turns`, `default_max_model_calls`,
  `default_max_duration`, `resources`): the Runtime Broker applies them on every start, below the
  template and the agent's own config (for `resources`, also below the broker's profile resources
  and harness overrides; see
  [Resources interleaves](#resources-interleaves-and-cannot-be-given-one-rank)). A change to them
  reaches an agent at its next start or restart (see
  [the change below](#changed-in-this-release--hub-agent_defaults-limits-and-resources-apply-on-every-start)).
- **The other `agent_defaults`** (default template, harness config, harness auth, model,
  thinking level) are resolved by the hub at agent create, below the template and the project,
  and stored with the agent. A later change to them does not reach agents that already exist.

Project-level default limits (project settings) are also written into the agent's config at
create. They count as the agent's own values, outrank the template, and are not refreshed on
start.

### The rank of hub `agent_defaults` depends on the hub's storage mode

The rank of the limits and `resources` differs by hub mode:

| Hub mode | Rank of hub `agent_defaults` |
| --- | --- |
| **Database-backed** (SQLite or Postgres) | Just **above** the broker's own `settings.yaml` defaults |
| **File mode** | **Bottom** of the broker chain |

In file mode the hub sends nothing, so a co-located broker reads the same `settings.yaml` and
applies those values itself, at the bottom tier. That is deliberate: applying hub defaults in file
mode too would promote existing file-mode defaults from the bottom of the broker chain up to the
hub tier — a silent behaviour change for deployed single-node installs.

Note also the `resources` asymmetry described in
[Resources interleaves](#resources-interleaves-and-cannot-be-given-one-rank): "above the broker's
own `settings.yaml` defaults" means `default_resources` **only**. Broker profile resources and
harness overrides live in the same file and are merged *above* the hub tier.

**This is why the hub rung in the `resources` chain is marked "database-backed hubs only".** The two
rows of the table above are two *different* positions in that chain — just above
`default_resources` in a database-backed hub, at `default_resources` in file mode.

### `Changed in this release` — hub `agent_defaults` limits and resources apply on every start

**Before:** the hub `agent_defaults` limits (`default_max_turns`, `default_max_model_calls`,
`default_max_duration`) took effect only once, when the agent was created. If you edited them
and then restarted an agent, nothing changed.

**After:** the hub sends its current limits and `resources` defaults with every start and
restart. The Runtime Broker applies them on each start and does not save them in the agent's
`scion-agent.json`. The same holds for the broker's own `settings.yaml` limit defaults. A limit
the agent sets itself (from its template or its own config) still wins over both. A limit
cleared on the hub falls back to the template's value, or to the current defaults when the
template sets none.

Agents created before this change keep the limits they have stored. Their saved values look
the same as values set on the agent itself, so a changed default does not reach them.

The hub's other `agent_defaults` (default template, harness config, harness auth, model,
thinking level) are still applied only at agent create.

:::note[Upgrade the hub before the brokers]
A broker at this release talking to an older hub receives hub `agent_defaults.resources` only at
create, not on later starts, and, because it no longer persists them, applies them only on the
agent's first start.
:::

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
go test ./pkg/hub -count=1 -timeout 60m   # slow (see below); -race needs far more memory and time
```

A whole-repo `go test ./...` passes on `main` given enough time: CI's Full Test Suite runs
`go test -timeout 60m ./...`. With the default 10-minute timeout it fails in `pkg/hub`, whose
SQLite-enabled run takes longer than that (GoogleCloudPlatform/scion#2842), so pass `-timeout 60m`.

## See also

- [Agent Configuration (`scion-agent.yaml`)](/scion/reference/agent-config/) — the field reference for
  templates and agents, including the project-settings annotations named above.
- [Admin Settings](/scion/reference/admin-settings/) — where hub `agent_defaults` are configured.
- [Harness-Specific Settings](/scion/reference/harness-settings/) — configuration consumed by the tools
  running inside the container.
- [Migrating from grove names](/scion/reference/grove-removal/) — the removed `grove`-named flags,
  environment variables, and config keys, and their `project`-named replacements.
