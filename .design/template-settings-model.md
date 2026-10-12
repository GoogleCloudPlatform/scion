# Design: Template and Settings Model

## Status

**Decided** (2026-10-10, revised 2026-10-11). Implementation is in progress: the invariants (section 3), the key classes and precedence (section 4) and the outcomes (section 5) describe the target, not current behaviour. Implementation children are tracked on the epic ptone/scion#4215. Local-only CLI mode is being removed; this model is specified for the hub and broker path and applies to local code paths only where it costs nothing.

This document supersedes the template `config` field described in [`hosted/hosted-templates.md`](hosted/hosted-templates.md) (sections 4.1, 5.1, 7.4, 10.1 and 10.2).

## 1. Context

A hosted template's agent config currently lives in two places: the template's files (`scion-agent.yaml` / `scion-agent.json`) and a writable `config` field on the hub's template row. The two can drift apart, and different code paths have read from different places. ptone/scion#2093 fixed one upload path; ptone/scion#4125 proposed a per-field source marker to extend that, which this model makes unnecessary. Separately, templates have been able to set operating-environment values (telemetry, images, volumes, resources) that belong to the deployment, and hub and project settings have been merged into the requester's inline config, which made their precedence relative to the template unclear.

## 2. Direction

1. **A template describes the agent's job, not its operating environment.** A template sets what the agent does. Where and how it runs (telemetry, image, volumes, resources, hub access) comes from settings and policy only.
2. **A template's files are the single source of truth for its job config.** The hub row keeps only fields derived from those files.
3. **The project is closer to the work than the template.** A project owner may run a template written by anyone, so project settings override the template for job keys.

## 3. Invariants

| ID | Invariant |
|----|-----------|
| I1 | A template's job config exists in exactly one place: its files (`scion-agent.yaml` / `.json`). The hub row has no writable config, only fields derived from the files. |
| I2 | Every change to a template's or harness-config's files goes through one commit function per kind. Nothing else writes `Files`, `ContentHash` or the derived fields. |
| I3 | Derived fields (`harness`, `defaultHarnessConfig`, the `agentConfig` snapshot) come from one parser and one key order: the broker's (`config.ResolveHarnessConfigName`). |
| I4 | Every config key is either a **job key** or an **environment key** (section 4). Templates set job keys only; environment keys in a template are ignored with a warning. Hub-supplied values travel as separate tiers (defaults, policy) and are never merged into the requester's inline config (skills excepted; see section 4). |
| I5 | A commit is atomic and safe under concurrent writers on a multi-replica hub. The Postgres row is the commit point, and storage content is immutable. |

## 4. Key classes and precedence

### Job keys (the template may set them; the project and the requester override)

`harness_config` / `default_harness_config`, `model`, `thinking_level`, `agent_instructions`, `system_prompt`, `skills`, `mcp_servers`, `command_args`, `task_flag`, the limits (`max_turns`, `max_model_calls`, `max_duration`), `services`, `secrets` (declaring which secret names the job needs; values always come from the environment), `message_mode`, and `env` for job-specific variables.

Precedence for job keys, lowest to highest:

```
built-in defaults
  < harness-config (its config.yaml base layer)
    < broker settings (settings.yaml on the broker)
      < hub defaults
        < template chain (default template < named template, its files)
          < project defaults (project settings and default annotations)
            < requester inline config
              < requester explicit CLI flags (--model, --harness-config, ...)
```

### Environment keys (settings and policy only; ignored in a template)

`telemetry`, `hub`, `image` (owned by the harness-config and settings/profiles), `resources`, `kubernetes`, `volumes`, and, within a template's `env:` block, reserved keys: `SCION_*`, `OTEL_*`, hub-only keys, and the credential keys the resolved harness-config declares in its auth metadata.

For telemetry, lowest to highest:

```
broker settings
  < hub telemetry config (a hub default)
    < requester on/off flag (--enable-telemetry / --disable-telemetry; no other telemetry fields)
      < policy (currently the only policy key: project telemetry on/off)
```

### Rules

- Defaults fill only what higher tiers leave unset. Policy overrides for its keys only.
- An environment key found in a template is ignored at load with a warning, and the admin validate report lists the template. Uploads are not rejected.
- Known exceptions, unchanged by this model:
  - Environment variables and secrets (runtime_broker, hub, project and user scopes, and template and harness-config env): their own orders.
  - Resources keep their own interleaving with broker profile and harness overrides; templates no longer contribute to it.
  - Skills: merged by scope rather than replaced along the job ladder, using the existing hub and broker merge rules. Hub-injected skills keep travelling in the requester's inline config, labelled by scope. This is an explicit exception to I4.

  See [`settings-precedence.md`](../docs-site/src/content/docs/reference/settings-precedence.md) for these orders; the release that applies this model updates that page (ptone/scion#4225).

## 5. Decided Outcomes

| Question | Outcome | Rationale |
|----------|---------|-----------|
| Project defaults vs the template (model, harness config, thinking level, limits) | The project wins. | The project owner may use any author's template; the project context is closer to the work. |
| Hub defaults vs the template for job keys | The template wins; hub defaults fill only what the template leaves unset. | A hub default is a deployment-wide fallback, further from the work than the template author. |
| Telemetry in templates | Never configured from a template. A template's `telemetry:` block is ignored. | Telemetry is operating environment, not part of the agent's job. |
| Other environment keys in templates (`hub`, `image`, `resources`, `kubernetes`, `volumes`, reserved `env` keys) | Ignored with a warning and listed by the admin validate report; uploads are not rejected. | Same principle; existing templates and imports keep working. |
| Requester telemetry | Only the on/off flag, below project policy. | Full telemetry configuration comes from settings. |
| Template API `config` field on create/update | Removed. The API answers `400` with a pointer to `scion-agent.yaml`. | I1: the files are the only place job config lives. |
| Existing rows whose `config` holds values not in the files | Reported by `GET /api/v1/admin/validate-resources`, then ignored. No write-back into YAML. | Makes drift visible without rewriting user-owned files. |
| Template with both `harness_config` and `default_harness_config` | The broker's order wins (`default_harness_config` first). | The broker is what runs the agent (I3). |
| Project telemetry policy vs a requester's explicit `--enable-telemetry` / `--disable-telemetry` | Policy wins. | Policy sits above everything for the keys it controls. |
| Storage layout for immutable versions | Hash-keyed blobs in a row-unique storage location next to the template's path, with the manifest mapping each path to its hash. | Unchanged files are shared across versions without copies. |
| Local-only mode | The same model where it is free; no dedicated local-only work. | Local-only mode is being removed. |

The hub-default-vs-template model change, the template environment-key exclusion, the requester telemetry restriction, the harness-config key order and the telemetry-policy outcome are user-visible behaviour changes; the release notes cover them (ptone/scion#4225).

## 6. References

- Epic: ptone/scion#4215 (implementation children are tracked on the epic)
- Earlier per-path work: ptone/scion#2093, ptone/scion#4125
- Superseded sections: [`hosted/hosted-templates.md`](hosted/hosted-templates.md)
- Broker harness-config resolution: `pkg/config/resolve_harness_config.go`
