# Harness-Config Authoring Guide

How to build a scion harness-config bundle for a coding-agent CLI. Written for
authors who already know their target tool and are building their first scion
harness — typically shipping the bundle in their own repository and installing
it with `scion harness-config install`.

The [codex bundle](codex/) is the reference example throughout: it exercises
every part of the system (declarative auth with env + file methods, custom MCP
translation, telemetry reconciliation, a `dialect.yaml` hook dialect, and a
`provision_test.py`). Read it side by side with this guide.

## What a harness-config is

A harness-config teaches scion how to run a coding-agent CLI (a "harness")
inside an agent container. It is a self-contained directory of:

- **Declarative metadata** (`config.yaml`) the host uses to build the CLI
  command line, select auth, advertise capabilities, and stage inputs.
- **A container-side provisioner** (`provision.py`) that runs inside the agent
  container at the pre-start lifecycle event and performs all harness-native
  file writes (auth files, client settings, MCP registration, instructions).
- **A container image** (`Dockerfile`) with the CLI installed, built on a
  scion base image.
- **Seed files** (`home/`) copied into the agent's home directory.

The split is deliberate: host and broker code never rewrite harness-native
config formats. The host stages *universal* inputs (instructions, system
prompt, auth candidates, MCP servers, telemetry) into a bundle directory in
the agent home; your `provision.py` translates them into whatever your tool
natively reads.

Users install a bundle from a local checkout or straight from GitHub:

```sh
scion harness-config install harnesses/codex
scion harness-config install github.com/GoogleCloudPlatform/scion/tree/main/harnesses/codex
```

Installed bundles live at `~/.scion/harness-configs/<name>/` (global) or
`.scion/harness-configs/<name>/` (project). Resolution precedence is
template → project → global. Related CLI: `scion harness-config
list|show|upgrade|reset|sync|pull`.

## Bundle layout

```
<name>/
  config.yaml        # required — harness metadata, parsed and schema-validated by the host
  provision.py       # container-side provisioner (pre-start)
  scion_harness.py   # vendored shared library imported by provision.py
  capture_auth.py    # thin shim: sys.exit(scion_harness.capture_auth_main())
  dialect.yaml       # optional — data-driven hook-event dialect
  home/              # seeded into the agent home at install time
    .bashrc
    .<tool>/...      # native client settings, hook wiring, notify scripts
  Dockerfile         # image build, FROM ${BASE_IMAGE} (a scion-base)
  cloudbuild.yaml    # optional — Cloud Build config for the image
  provision_test.py  # optional — unit tests for provision.py
  README.md          # install instructions, auth modes, build steps
```

At install time, `provision.py`, `scion_harness.py`, `capture_auth.py`,
`dialect.yaml` (plus `schema/`, `examples/`, `tests/fixtures/`) stay at the
bundle root; everything under `home/` is copied into the harness-config's
`home/` directory verbatim. `Dockerfile`, `cloudbuild.yaml`, `README.md`,
`provision_test.py`, `.gitkeep`, and `init-firewall.sh` are not installed.
Always place seed files under an explicit `home/` prefix — files at the bundle
root that aren't recognized support files are routed through a legacy
filename-mapping table you don't want to depend on.

The install machinery computes a content-hash revision over the bundle
(skipping `cloudbuild.yaml`, `README.md`, `.gitkeep`) that is stamped on
agents for audit, so keep generated files deterministic.

## config.yaml

Validated on load against the settings-v1 JSON schema (`scion` will refuse an
invalid bundle). Fields, grouped by concern:

### Identity

| Field | Meaning |
|---|---|
| `harness` | Harness type name (required). Also the default `--dialect` your hooks should use. |
| `name` | Optional override for the config's directory-derived name (no path separators). |
| `image` | Container image tag the runtime launches, e.g. `scion-codex:latest`. Must match what your Dockerfile builds. |
| `user` | In-container user; use `scion` (created by scion-base with uid 1000). |

### Provisioner

```yaml
provisioner:
  type: container-script
  interface_version: 1
  lib: vendored
  command: ["python3", "/home/scion/.scion/harness/provision.py"]
  timeout: 30s
  lifecycle_events: [pre-start]
  required_image_tools: [python3]
```

`type: container-script` is the only supported type for new harnesses.
`lib: vendored` tells the host to stage *your* bundle's `scion_harness.py`
next to `provision.py` (the alternative, `injected`, uses the copy embedded
in the scion binary — fine for quick experiments, but vendored gives you a
pinned, reviewable library version). `required_image_tools` lists binaries
that must exist in the image (`python3` always; add `jq` etc. if your
provision-time hook commands need them).

### Command construction

```yaml
command:
  base: ["codex", "--sandbox", "danger-full-access", ...]
  task_flag: "--prompt"          # omit for positional task argument
  task_position: after_base_args # or before_base_args
  resume_flag: "resume --last"   # split on whitespace into argv tokens
  system_prompt_flag: "--system-prompt"  # only if the CLI accepts a native flag
```

The host assembles: `base... [resume tokens] [harness args] [task]` (default
`after_base_args`), or `base... [resume tokens] [task] [harness args]` with
`before_base_args`. Pick the position that matches how your CLI parses argv
when extra flags are passed through — this is a common parity bug (see the
comments in the codex and opencode configs). `resume_flag` is whitespace-split,
so multi-token resume subcommands work. If `system_prompt_flag` is set, the
host appends it with the staged system-prompt content at launch.

Base flags should put the tool in full-permission, non-interactive-approval
mode (`--yolo`, `--dangerously-skip-permissions`, `approval_policy = never`,
etc.) — the agent runs unattended inside a sandboxed container.

### Prompts, instructions, skills

| Field | Meaning |
|---|---|
| `config_dir` | Harness-native config directory relative to home, e.g. `.codex`. |
| `skills_dir` | Where installed skills land, e.g. `.codex/skills`. |
| `instructions_file` | Home-relative file your provisioner projects scion instructions into, e.g. `.codex/AGENTS.md`. |
| `system_prompt_file` | Home-relative native system-prompt file, if the tool has one. |
| `system_prompt_mode` | `native` (tool has a real system-prompt channel), `prepend_to_instructions` (downgrade into the instructions file), or `none`. |

`scion_harness.project_instructions()` honors `system_prompt_mode` and
`skills_dir` from these fields. It projects system prompt and instruction
content by default, but `include_skills` defaults to `False` because scion's
host-side provisioner already installs skills as individual files under
`skills_dir`. Only pass `include_skills=True` for a harness that has no native
skills directory and must inline `SKILL.md` content into its instruction file.

### Models

```yaml
model_aliases:
  small: gpt-5.4-mini
  medium: gpt-5.4
  large: gpt-5.5
  extra-large: gpt-5.5
```

Templates reference abstract sizes; the alias resolves to a concrete model at
provision time (available to your script via the manifest's
`model_resolution`). Provide all four conventional aliases.

### Environment

- `env`: static env vars set in the container.
- `env_template`: values with placeholder expansion — `{{ .AgentName }}`,
  `{{ .AgentHome }}`, `{{ .UnixUsername }}` (only these three).

### Interrupts

`interrupt_key` (tmux key, e.g. `C-c` or `Escape`), and for tools needing a
multi-key interrupt, `interrupt_signal: sequence` +
`interrupt_sequence: [Escape, C-c]` (each entry sent as a separate key).

### Capabilities

An honest support matrix consumed by templates, preflight checks, and UIs:

```yaml
capabilities:
  limits:
    max_turns: { support: "yes" }
    max_model_calls: { support: "no", reason: "no model start/end hook events" }
    max_duration: { support: "yes" }
  telemetry:
    enabled: { support: "yes" }
    native_emitter: { support: "yes" }
  prompts:
    system_prompt: { support: "partial", reason: "downgraded into AGENTS.md" }
    agent_instructions: { support: "yes" }
  auth:
    api_key: { support: "yes" }
    auth_file: { support: "yes" }
    oauth_token: { support: "no" }
    vertex_ai: { support: "no", reason: "..." }
  mcp:
    stdio: { support: "yes" }
    sse: { support: "partial", reason: "mapped to single HTTP type" }
    streamable_http: { support: "yes" }
    project_scope: { support: "no", reason: "demoted to global" }
  resume: { support: "yes" }
```

Every field takes `support: yes|partial|no` plus an optional `reason` (give
one for anything but `yes`). What each signals:

- **limits.max_turns / max_model_calls** — enforceable only if your hook
  wiring emits the corresponding normalized events (`prompt-submit`,
  `model-start`/`model-end`). Don't claim support without the events.
- **limits.max_duration** — wall-clock; effectively always `yes`.
- **telemetry.enabled / native_emitter** — whether scion can toggle telemetry
  and whether the tool emits OTEL natively (vs. scion inferring from hooks).
- **prompts.system_prompt** — `yes` only for a true native system-prompt
  channel; `partial` when `system_prompt_mode: prepend_to_instructions`.
- **auth.\*** — which auth types in your `auth:` block actually work.
- **mcp.\*** — transports your provisioner translates; `project_scope`
  reflects whether project-scoped servers are honored or demoted to global.
- **resume** — whether `resume_flag` genuinely restores a session.

### Auth

```yaml
auth:
  default_type: api-key
  types:
    api-key:
      required_env:
        - any_of: ["CODEX_API_KEY", "OPENAI_API_KEY"]
    auth-file:
      required_files:
        - name: CODEX_AUTH
          type: file
          target_suffix: "/.codex/auth.json"
          field: CodexAuthFile
  autodetect:
    env:
      CODEX_API_KEY: api-key
      OPENAI_API_KEY: api-key
    files:
      CODEX_AUTH: auth-file
```

- `types.<name>.required_env` — list of `any_of` groups; each group needs at
  least one var present. Multiple groups = all groups required (e.g.
  vertex-ai needs a project var AND a region var).
- `types.<name>.required_files` — file credentials. Key fields:
  - `name`: secret key (also the capture-auth key and staging filename).
  - `target_suffix`: in-container path suffix, e.g. `/.codex/auth.json`;
    used to match host file mappings and to derive the capture source
    (`~` + suffix).
  - `field`: the Go `AuthConfig` field this file maps to (needed for the
    hardcoded set of known credential files the host knows how to gather;
    see Known Gaps).
  - `alternative_env_keys`: env vars that satisfy the requirement instead of
    the file (e.g. `GOOGLE_APPLICATION_CREDENTIALS` for ADC).
  - `skipped_when_gcp_service_account_assigned`: drop the requirement when a
    GCP workload identity is attached.
  - `required`: broker must source the secret before dispatch (vs.
    documentary).
- `autodetect` — maps discovered env vars / file secrets to an auth type when
  the user didn't select one explicitly.

Conventional type names — `api-key`, `oauth-token`, `auth-file`, `vertex-ai` —
line up with the `capabilities.auth` matrix; use them.

### no_auth

```yaml
no_auth:
  behavior: drop-to-shell
  command: codex login --device-auth
  message: |
    This agent started without credentials.
    Run: codex login --device-auth
    Then run: python3 /home/scion/.scion/harness/capture_auth.py
```

When no credentials are found, instead of failing, the agent drops to a shell
with your message. The user logs in interactively, then runs the staged
`capture_auth.py`, which reads `inputs/capture-auth-config.json` (generated by
the host from your `required_files` declarations) and stores each credential
as a project secret via `sciontool secret set` — so the next agent starts
authenticated.

### MCP (declarative mapping)

If your tool's MCP registry is a JSON file with per-server entries that match
the universal schema 1:1 (command/args/env/url/headers plus a transport-type
field), declare a mapping and skip custom code:

```yaml
mcp:
  global_config_file: .copilot/mcp-config.json   # home-relative
  global_config_path: mcpServers                 # dotted path in the JSON
  transport_field: type
  transport_map:
    stdio: local
    sse: http
    streamable-http: http
```

Your provision.py then calls `scion_harness.apply_mcp_servers_simple(...)`.
If the native format is TOML or structurally different (codex, opencode),
leave `mcp:` out and write a translate function using
`scion_harness.apply_mcp_translated(...)` instead.

### dialect (inline)

There is also a free-form `dialect:` map field in the schema; current bundles
use a separate `dialect.yaml` file instead (see Hooks below).

## Runtime contract: host staging → provision.py

At agent provision time, the host (`ContainerScriptHarness`) stages a bundle
into the agent home. Inside the container it appears as:

```
$HOME/.scion/harness/
  manifest.json        # what/where — read by scion_harness.run()
  config.yaml          # your config, verbatim
  provision.py         # your script (chmod 755)
  scion_harness.py     # staged per provisioner.lib mode
  capture_auth.py      # if present in the bundle
  dialect.yaml         # if present in the bundle
  inputs/
    instructions.md            # composed agent instructions (if any)
    system-prompt.md           # system prompt content (if any)
    auth-candidates.json       # candidate env vars, secret file paths, explicit type
    mcp-servers.json           # universal MCP server map (if any configured)
    telemetry.json             # effective TelemetryConfig + env overlay
    resolved-skills.json       # resolved skills (if any)
    capture-auth-config.json   # generated from auth required_files
  outputs/             # your script writes here
    resolved-auth.json
    env.json
  secrets/             # 0600 files, one per credential
    <ENV_VAR_NAME>
    <FILE_SECRET_NAME>
```

The host also writes a trusted lifecycle hook at
`$HOME/.scion/hooks/pre-start.d/20-harness-provision` that runs
`sciontool harness provision --manifest $HOME/.scion/harness/manifest.json`,
which executes your `provisioner.command` on every agent start/resume. Secret
env vars are stripped from the script's process environment — read secret
*values* from the staged files in `secrets/` (via the `ProvisionContext`
helpers), never from `os.environ`.

`manifest.json` gives you: `schema_version`, `command` (`"provision"`),
`agent_name`, `agent_home`, `agent_workspace`, `harness_bundle_dir`,
`harness_config` (your parsed config.yaml — read `instructions_file`,
`system_prompt_mode`, etc. from here rather than hardcoding), `inputs` /
`outputs` paths, `model_resolution`, and `platform`.

### What provision.py must do

1. Select an auth method (`ctx.select_auth(AUTH)`) and write the tool's
   native credential file(s) or env overlay.
2. Project instructions: `scion_harness.project_instructions(ctx,
   instructions_file)`. Leave `include_skills` at its default `False` when
   your config declares `skills_dir`; pass `include_skills=True` only for
   harnesses that cannot load skills from files.
3. Translate MCP servers (`apply_mcp_servers_simple` or
   `apply_mcp_translated`).
4. Reconcile telemetry into native client settings if the tool supports it
   (`ctx.telemetry`).
5. Any other native writes: trust the workspace, pre-approve keys, disable
   auto-update/onboarding prompts, generate hook files.
6. Write outputs: `ctx.write_outputs(resolved, env={...})` —
   `resolved-auth.json` records the chosen method; `env.json` maps env vars
   to project into the harness process (values may be literal or `${VAR}`
   placeholders resolved from the environment at launch).

Keep the script **stdlib-only** and **idempotent** — it runs on every start
and resume. Use the library's atomic write and managed-marker helpers; never
clobber user edits outside your managed regions.

Minimal skeleton (see [codex/provision.py](codex/provision.py) for the real
thing):

```python
import scion_harness

AUTH = scion_harness.AuthSpec("mytool", [
    scion_harness.env_method("api-key", any_of=["MYTOOL_API_KEY"]),
    scion_harness.file_method("auth-file", path="~/.mytool/auth.json",
                              secret_key="MYTOOL_AUTH"),
])

def provision(ctx: scion_harness.ProvisionContext) -> None:
    resolved = ctx.select_auth(AUTH)
    # ... write native auth/config files ...
    scion_harness.project_instructions(
        ctx, str(ctx.harness_config.get("instructions_file") or "AGENTS.md"))
    ctx.write_outputs(resolved, env={})

if __name__ == "__main__":
    scion_harness.run("mytool", provision)
```

## scion_harness.py

The shared, stdlib-only library. **Vendor it**: copy the canonical
`harnesses/scion_harness.py` from the scion repo into your bundle, set
`provisioner.lib: vendored`, and update your copy manually when you pick up
new library features. Guard against staleness at the top of provision.py:

```python
assert scion_harness.INTERFACE_VERSION >= 2
```

(Bundles inside the scion repo itself are kept in sync mechanically via
`go generate ./harnesses/`, which stamps a `GENERATED FILE` header; external
bundles just track the canonical file.)

Key API surface:

- **`run(harness_name, provision_fn)`** — entry-point scaffold: parses
  `--manifest`, loads it, dispatches the `provision` command, maps
  `ProvisionError`/`OSError` to exit code 1 (`EXIT_UNSUPPORTED = 2` for
  unknown commands).
- **`ProvisionContext`** — properties: `bundle_dir`, `inputs_dir`, `home`,
  `workspace`, `harness_config`, `candidates`, `explicit_type`, `env_keys`,
  `file_paths`, `env_secret_files`, `file_secret_files`, `telemetry`,
  `model_resolution`. Methods: `read_secret(name)` / `read_file_secret(name)`
  (staged secret values, trailing newline stripped), `read_input_text(name)`,
  `select_auth(spec)`, `write_outputs(resolved, env=, extra=)`,
  `info()` / `warn()` (stderr).
- **Auth engine** — `AuthSpec(harness, [methods])` with
  `env_method(name, any_of=/all_of=, hint=, env_fallback=)` and
  `file_method(name, path=, secret_key=, hint=)`. `select_auth` honors an
  explicit user selection, otherwise tries methods **in declaration order**
  (your precedence), checking staged candidates/secrets before any env
  fallback; raises `ProvisionError` with the accumulated hints if nothing
  matches, or returns `ResolvedAuth(method="none")` when `no_auth` behavior
  is configured and no candidates were staged.
- **`project_instructions(ctx, target, ...)`** — composes
  `inputs/system-prompt.md` (per `system_prompt_mode`),
  `inputs/instructions.md`, and, only when `include_skills=True`, installed
  `SKILL.md` files from `skills_dir` into the target file inside
  `<!-- BEGIN/END SCION MANAGED -->` markers, preserving user content outside
  the block. `include_skills` defaults to `False`; keep it that way for
  harnesses that declare `skills_dir`, because those skills are installed as
  native files by the host-side provisioner.
- **MCP** — `apply_mcp_servers_simple(bundle_path, mcp_mapping, workspace)`
  for JSON-native tools (driven by the `mcp:` block);
  `apply_mcp_translated(ctx, translate_fn, write_fn)` for custom formats
  (warn-and-skip per server, demotes project scope to global).
- **File helpers** — `atomic_write_json`, `atomic_write_text` (tmp +
  `os.replace`), `expand_path`, `load_json`,
  `read_json_skipping_comment_lines`; TOML emit/reconcile helpers:
  `toml_escape`, `toml_inline_table`, `toml_string_array`,
  `strip_toml_sections` (tomllib is read-only, so TOML editing is manual).
- **`capture_auth_main()`** — the whole capture-auth flow; your
  `capture_auth.py` is a two-line shim around it. Exit codes: 0 captured,
  1 error, 2 no credentials found, 3 conflict (secret exists; `--force`).
- **`ProvisionError`** — raise for any fatal provisioning condition.

## Hooks and dialect.yaml

Scion observes the agent through hook events: your harness invokes
`sciontool hook --dialect=<name>` with a JSON event payload on stdin. The
event is normalized and drives status, logging, limits enforcement, Hub
updates, and telemetry.

Three wiring patterns, by how configurable your tool is:

1. **Static config in `home/`** — claude and gemini-cli ship hook wiring in
   their seeded settings files, piping every lifecycle event to
   `sciontool hook --dialect=...` ([claude/home/.claude/settings.json](claude/home/.claude/settings.json)).
2. **Notify/wrapper script in `home/`** — codex routes both its legacy
   `notify` callback and its hooks through
   [`scion_notify.sh`](codex/home/.codex/scion_notify.sh), which also turns
   turn-completion into `sciontool status task_completed`.
3. **Generated at provision time** — when hook config must contain runtime
   values or live in the workspace, write it from provision.py (antigravity
   generates `/workspace/.agents/hooks.json` wiring events through `jq` into
   `sciontool hook --dialect=antigravity`).

`sciontool` has built-in dialects for `claude`, `gemini`, and `codex`. For any
other harness, ship a **`dialect.yaml`** in the bundle — it is staged to
`$HOME/.scion/harness/dialect.yaml`, and `sciontool hook` loads it when its
`dialect:` name matches the `--dialect` flag (a bundled dialect also overrides
a built-in of the same name):

```yaml
dialect: mytool
event_name_fields: [hook_event_name, type, event]  # tried in order
mappings:
  BeforeTool:
    event: tool-start
    fields:               # optional dotted-path field extraction
      tool_name: toolCall.name
      tool_input: toolCall.args
  AfterAgent:
    event: agent-end
```

Map into the normalized event vocabulary: `session-start`, `session-end`,
`prompt-submit`, `tool-start`, `tool-end`, `model-start`, `model-end`,
`agent-end`, `subagent-end`, `response-complete`, `notification`.
Extractable fields include `prompt`, `tool_name`, `tool_input`,
`tool_output`, `message`, `session_id`, `success`, `error`, `assistant_text`,
`file_path`, and `input_tokens` / `output_tokens` / `cached_tokens`.

Your `capabilities.limits` claims must match this wiring: `max_turns` needs
`prompt-submit`, `max_model_calls` needs model start/end events.

Agent-side status signaling (`sciontool status task_completed|blocked|ask_user`)
is usually driven by agent instructions/templates, but a hook script may emit
it directly where the tool has a reliable completion callback (codex).

## Dockerfile and image pipeline

Every scion harness image must be built **on a scion base image** — the base
provides the `scion` user (uid 1000, zsh), `sciontool` (the container
entrypoint is `sciontool init --`), python3, node/npm with a shared global
prefix, and the standard developer toolchain. Take the base as a build arg:

```dockerfile
# syntax=docker/dockerfile:1
ARG BASE_IMAGE
FROM ${BASE_IMAGE}

ARG MYTOOL_VERSION=latest

# Create the tool's config + skills dirs owned by the agent user.
RUN mkdir -p /home/scion/.mytool/skills && \
    chown -R scion:scion /home/scion/.mytool

# Install the CLI on the standard PATH; clean caches.
RUN npm install -g @acme/mytool@${MYTOOL_VERSION} \
    && npm cache clean --force

CMD ["mytool"]
```

Rules of thumb:

- **Never hardcode the base** — accept `BASE_IMAGE` so users can pin a
  registry/tag (`scion-base:latest` locally, or the published
  `us-central1-docker.pkg.dev/.../scion-base:<tag>`).
- **Own what the agent writes**: pre-create the tool's config dir and any
  cache/state dirs and `chown scion:scion` them. Root-owned surprises in
  `$HOME` are the most common first-run failure.
- Do root-level work (`apt-get`, sudoers entries, firewall scripts) as
  `USER root`; the entrypoint handles dropping privileges at runtime — don't
  end with a `USER` directive that breaks `sciontool init`.
- Install the CLI at a pinned or arg-controlled version; symlink into
  `/usr/local/bin` if the installer doesn't.
- Tag the result to match `config.yaml`'s `image:` field.

Local build:

```sh
docker build --build-arg BASE_IMAGE=scion-base:latest \
  -t scion-mytool:latest -f Dockerfile .
```

For published images, ship a `cloudbuild.yaml` following the codex pattern:
buildx multi-arch (`linux/amd64,linux/arm64`), `--build-arg
BASE_IMAGE=$_REGISTRY/scion-base:$_TAG`, tags for both `$_SHORT_SHA` and
`$_TAG`, `--pull --push`. See [codex/cloudbuild.yaml](codex/cloudbuild.yaml)
and `image-build/` in the scion repo for how the base images themselves are
built.

## home/ seed files

Everything under `home/` is copied into the agent home. Use it for:

- `.bashrc` sourcing scion env.
- Native client settings that are static: disable auto-update, onboarding
  nags, and telemetry prompts; set `approval_policy`/yolo mode; pre-trust
  `/workspace`; wire hooks (patterns above).
- Notify/hook shell scripts.

Anything that depends on runtime values (workspace path, model, credentials,
telemetry endpoint) belongs in `provision.py`, not in a seed file.

## Testing and iteration

- **Unit-test provision.py** with a fixtures-based `provision_test.py`
  (pattern: [codex/provision_test.py](codex/provision_test.py)) — fake a
  bundle dir with `manifest.json` + `inputs/`, run `provision()`, assert on
  the native files and `outputs/`.
- **Install locally**: `scion harness-config install ./path/to/bundle` (this
  also schema-validates `config.yaml`), then create an agent with the config
  and watch the pre-start provision log.
- **Re-run in place**: inside a container,
  `python3 ~/.scion/harness/provision.py --manifest ~/.scion/harness/manifest.json`
  re-executes provisioning against the staged inputs.
- **Test hook mapping** by piping a captured payload:
  `echo '{"hook_event_name": "BeforeTool", ...}' | sciontool hook --dialect=mytool`.

## New-harness checklist

1. `config.yaml`: identity, provisioner block, command block, model aliases,
   instructions/system-prompt fields, honest capabilities, auth types +
   autodetect, `no_auth` drop-to-shell.
2. Vendor `scion_harness.py`; write `provision.py` (auth → native files,
   instructions projection, MCP, telemetry, outputs); add the two-line
   `capture_auth.py`.
3. `dialect.yaml` + hook wiring (static, notify script, or generated) if the
   tool exposes lifecycle events; align `capabilities.limits`.
4. `home/` seeds: `.bashrc`, native settings with auto-update/onboarding
   disabled and full-permission mode.
5. `Dockerfile` on `${BASE_IMAGE}`, config dirs chowned to `scion`;
   `cloudbuild.yaml` if publishing.
6. `provision_test.py`, `README.md` (install command, auth-mode table, build
   instructions).
7. Install, create an agent per auth mode you claim to support, and verify:
   command line, resume, instructions file content, MCP registration, hook
   events, `no_auth` → login → `capture_auth.py` round trip.

## Known Gaps & Future Work

Current limitations of the harness-config system — listed here so the
guidance above stays aspirational-free. Don't design around these as if
permanent; check whether they've been fixed.

- **Project-scoped MCP** is not implemented for most harnesses;
  `apply_mcp_translated` demotes project-scoped servers to global with a log
  line. Claude's simple mapping is the only project-scope path exercised.
- **Version numbering is not unified**: `provisioner.interface_version` in
  config.yaml (1), the manifest `schema_version` (1), and
  `scion_harness.INTERFACE_VERSION` (2) are three separate counters. Guard on
  the library's `INTERFACE_VERSION`; the other two are effectively constant
  today.
- **`outputs/status.json`** is declared in the manifest outputs but nothing
  writes or reads it yet.
- **Root-level seed-file mapping is hardcoded**: bundle files outside `home/`
  that aren't recognized support files are placed via a per-filename switch
  (`config.toml` → `.codex/`, etc.). The `home/` prefix path is the
  supported, generic mechanism.
- **`required_files.field` couples to compiled-in Go struct fields**
  (`AuthConfig`), so fully novel file credentials for out-of-tree harnesses
  may not be gathered host-side without a scion change; env-based secrets and
  the capture-auth flow are the portable path (antigravity's `AGY_TOKEN`
  works this way).
- **capture-auth source derivation** assumes the credential lives at
  `~` + `target_suffix` inside the container; credentials at other locations
  need a custom `capture_auth.py`.
- **MappingDialect has no per-field source fallbacks** — one dotted path per
  field (codex's hyphenated `last-assistant-message` legacy key can't be
  expressed alongside the new one).
- **`sciontool hook --dialect` help text** only advertises the built-in
  dialects; bundled `dialect.yaml` dialects work but are undocumented there.
- **Vendored-lib drift for external bundles** is manual: nothing warns when
  your vendored `scion_harness.py` falls behind the canonical copy (in-repo
  bundles are covered by `go generate` + a sync test). The host logs the
  staged `LIB_VERSION` at provision time — check it when debugging.
- **`HasSystemPrompt`** checks for the native system-prompt file on disk, but
  the file is only written at pre-start, so host-side checks before first
  start can misreport.
- **Model alias keys** (`small`/`medium`/`large`/`extra-large`) are
  convention, not schema-enforced; missing aliases fail at resolution time,
  not install time.
