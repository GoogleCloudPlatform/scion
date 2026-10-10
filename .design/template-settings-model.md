# Design: Template and Settings Model

## Status

**Decided** (2026-10-10). Implementation in progress; invariants I1-I5 are targets, not current behaviour. Implementation children are tracked on the epic ptone/scion#4215.

This document supersedes the template `config` field described in [`hosted/hosted-templates.md`](hosted/hosted-templates.md) (sections 4.1, 5.1, 7.4 and 10.1).

## 1. Context

A hosted template's agent config currently lives in two places: the template's files (`scion-agent.yaml` / `scion-agent.json`) and a writable `config` field on the hub's template row. The two can drift apart, and different code paths have read from different places. ptone/scion#2093 fixed one upload path; ptone/scion#4125 proposed a per-field source marker to extend that, which this model makes unnecessary. In the same way, hub and project settings have been merged into the requester's inline config, which makes their precedence relative to the template unclear.

This document records the model that removes both problems.

## 2. Direction

**A template's files are the single source of truth for its agent config.**

## 3. Invariants

| ID | Invariant |
|----|-----------|
| I1 | A template's agent config exists in exactly one place: its files (`scion-agent.yaml` / `.json`). The hub row has no writable config, only fields derived from the files. |
| I2 | Every change to a template's or harness-config's files goes through one commit function per kind. Nothing else writes `Files`, `ContentHash` or the derived fields. |
| I3 | Derived fields (`harness`, `defaultHarnessConfig`, the `agentConfig` snapshot) come from one parser and one key order: the broker's (`config.ResolveHarnessConfigName`). |
| I4 | Settings enter the agent at a declared tier. Defaults (hub, project) sit below the template. Policy (enforced values) sits above everything for the keys it controls. Nothing the hub supplies is written into the requester's `InlineConfig`. |
| I5 | A commit is atomic and safe under concurrent writers on a multi-replica hub. The Postgres row is the commit point, and storage content is immutable. |

## 4. Defaults vs Policy

Hub and project settings are **defaults**: they sit below the template. **Policy** (enforced values, for example project telemetry on/off) sits above everything for the keys it controls. Nothing the hub supplies is written into the requester's inline config.

Precedence, lowest to highest:

```
built-in defaults
  < broker settings (settings.yaml on the broker)
    < hub defaults
      < project defaults
        < harness-config (its config.yaml base layer)
          < template chain (default template < named template, its files)
            < requester inline config
              < requester explicit CLI flags (--model, --image, --harness-config, --enable/--disable-telemetry, ...)
                < policy (only for the keys it controls; today: project telemetry on/off)
```

- Defaults fill only what the template leaves unset. Policy overrides for its keys only.
- Hub-supplied values travel as separate tiers (hub defaults, policy) and are never merged into the requester's inline config.
- Known exceptions, unchanged by this model: resources have their own interleaving with broker profile and harness overrides; see [`reference/settings-precedence.md`](../docs-site/src/content/docs/reference/settings-precedence.md). The release that applies the model updates that page (ptone/scion#4225).

For example, a project telemetry policy beats a requester's explicit `--enable-telemetry` / `--disable-telemetry` flag.

## 5. Decided Outcomes

| Question | Outcome | Rationale |
|----------|---------|-----------|
| Model precedence | The template's model wins over project and hub default models. | Project and hub models are defaults under I4. A project that must force a model would use a policy (not built now). |
| Template API `config` field on create/update | Removed. The API answers `400` with a pointer to `scion-agent.yaml`. | I1: the files are the only place config lives. |
| Existing rows whose `config` holds values not in the files | Reported by `admin validate-resources`, then ignored. No write-back into YAML. | Makes drift visible without rewriting user-owned files. |
| Telemetry in templates | A template may set telemetry endpoints and credentials. The docs say endpoints belong in settings. | Keeps templates self-contained while steering shared endpoints to settings. |
| Template with both `harness_config` and `default_harness_config` | The broker's order wins (`default_harness_config` first). | The broker is what runs the agent (I3). |
| Project telemetry policy vs a requester's explicit `--enable-telemetry` / `--disable-telemetry` | Policy wins. | Policy sits above everything for the keys it controls (I4). |
| Storage layout for immutable versions | Hash-keyed blobs: `<StoragePath>/blobs/<sha256>`, with the manifest mapping each path to its hash. | Unchanged files are shared across versions without copies. The alternative was a per-version prefix with a server-side copy of unchanged files. The first step is a short spike to confirm that co-located brokers that read the storage directory directly still work. |

The model-precedence, harness-config-key and telemetry-policy outcomes are user-visible behaviour changes; the release notes cover them (ptone/scion#4225).

## 6. References

- Epic: ptone/scion#4215 (implementation children are tracked on the epic)
- Earlier per-path work: ptone/scion#2093, ptone/scion#4125
- Superseded sections: [`hosted/hosted-templates.md`](hosted/hosted-templates.md)
- Broker harness-config resolution: `pkg/config/resolve_harness_config.go`
