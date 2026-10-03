#!/usr/bin/env python3
# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
"""Grok Build container-side provisioner.

Runs inside the agent container during the pre-start lifecycle hook.
Uses scion_harness library for auth selection, instruction projection,
MCP translation, and output writing.

Grok-build-native concerns handled here:
  - Auth token is exposed as XAI_API_KEY in env.json, or auth.json is
    written to ~/.grok/auth.json from a staged file secret.
  - MCP servers translate to TOML [mcp_servers.*] entries in
    ~/.grok/config.toml (stdio→command/args/env, sse/http→url/headers).
  - Instructions project to .grok/AGENTS.md (configurable via instructions_file).
  - System prompt is written to .grok/system-prompt.md and passed via
    --system-prompt-override (native routing).
  - ~/.grok/config.toml gets hardened defaults (auto-update off, telemetry
    off, memory off, subagents off).
  - Hook wiring to sciontool via ~/.grok/hooks/scion.json.
"""

from __future__ import annotations

import json
import os
import re as _re
import sys
from typing import Any

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import scion_harness

assert scion_harness.INTERFACE_VERSION >= 2, (
    "grok-build provision.py requires scion_harness INTERFACE_VERSION >= 2; "
    f"got {scion_harness.INTERFACE_VERSION}"
)

AUTH = scion_harness.AuthSpec(
    "grok-build",
    [
        scion_harness.env_method(
            "api-key",
            any_of=["XAI_API_KEY"],
            hint="set XAI_API_KEY with an xAI API key",
            env_fallback=True,
        ),
        scion_harness.file_method(
            "auth-file",
            path="~/.grok/auth.json",
            hint="provide grok auth at ~/.grok/auth.json",
            secret_key="GROK_AUTH",
        ),
        scion_harness.env_method(
            "vertex-ai",
            any_of=["GOOGLE_CLOUD_PROJECT", "SCION_METADATA_PROJECT_ID"],
            hint="set GOOGLE_CLOUD_PROJECT for Vertex AI model routing",
        ),
    ],
    fallback_to_none_on_error=True,
)


def _grok_config_dir(ctx: scion_harness.ProvisionContext) -> str:
    """Resolve the grok config directory, respecting GROK_HOME if set."""
    return os.environ.get("GROK_HOME") or os.path.join(ctx.home, ".grok")


def _read_token(ctx: scion_harness.ProvisionContext, env_key: str) -> str:
    """Read the token for an env-based auth method.

    Expands $HOME-style variables in secret file paths, then falls back to
    os.environ (hub-registered configs may not stage secret files).
    """
    path = ctx.env_secret_files.get(env_key)
    if path:
        expanded = scion_harness.expand_path(path)
        try:
            with open(expanded, "r", encoding="utf-8") as f:
                return f.read().rstrip("\r\n")
        except OSError:
            pass
    return os.environ.get(env_key, "")


def _write_auth_file(ctx: scion_harness.ProvisionContext) -> None:
    """Write ~/.grok/auth.json from a staged GROK_AUTH file secret."""
    content = ctx.read_file_secret("GROK_AUTH")
    if not content:
        raise scion_harness.ProvisionError(
            "auth-file method selected but GROK_AUTH secret is missing; "
            "check that the credential file was staged"
        )
    if not content.strip():
        raise scion_harness.ProvisionError("GROK_AUTH secret is empty")
    try:
        json.loads(content)
    except json.JSONDecodeError as exc:
        raise scion_harness.ProvisionError(
            f"GROK_AUTH secret is not valid JSON: {exc}"
        ) from exc
    config_dir = _grok_config_dir(ctx)
    os.makedirs(config_dir, exist_ok=True)
    target = os.path.join(config_dir, "auth.json")
    tmp = target + ".tmp"
    with open(tmp, "w", encoding="utf-8") as f:
        f.write(content)
    os.chmod(tmp, 0o600)
    os.replace(tmp, target)


def _apply_native_system_prompt(ctx: scion_harness.ProvisionContext) -> None:
    """Write the staged system prompt to the native grok CLI location.

    config.yaml declares system_prompt_file (.grok/system-prompt.md) and
    system_prompt_mode (native), so the prompt goes into its own file rather
    than being prepended to the instructions file. The Go-side harness reads
    this file and passes it via --system-prompt-override.
    """
    system_prompt = ctx.read_input_text("system-prompt.md")
    if not system_prompt.strip():
        return

    target = str(ctx.harness_config.get("system_prompt_file") or "")
    if not target:
        return

    full = os.path.join(ctx.home, target)
    parent = os.path.dirname(full)
    if parent:
        os.makedirs(parent, exist_ok=True)
    tmp = full + ".tmp"
    with open(tmp, "w", encoding="utf-8") as f:
        f.write(system_prompt)
    os.replace(tmp, full)
    ctx.info(f"wrote system prompt to {full}")


# ---------------------------------------------------------------------------
# Vertex AI configuration
# ---------------------------------------------------------------------------

_VERTEX_MODEL_ID = "xai/grok-4.6"
_VERTEX_AUTH_PROVIDER_NAME = "vertex-grok"
_VERTEX_MODEL_CONFIG_NAME = "vertex-grok"


def _configure_vertex_ai(
    ctx: scion_harness.ProvisionContext,
    env: dict[str, str],
) -> dict[str, Any]:
    """Configure grok to route inference through Vertex AI Model Garden.

    Writes:
      - [auth_provider.vertex-grok] with gcloud token command
      - [model.vertex-grok] with Vertex AI base_url and auth_provider ref
      - [models] default = "vertex-grok"
    """
    project = _read_token(ctx, "GOOGLE_CLOUD_PROJECT")
    if not project:
        # Fallback: when GCP identity is assigned, the platform injects the
        # project ID as SCION_METADATA_PROJECT_ID.
        project = os.environ.get("SCION_METADATA_PROJECT_ID", "").strip()
    if not project:
        raise scion_harness.ProvisionError(
            "vertex-ai auth selected but GOOGLE_CLOUD_PROJECT is empty"
        )
    env["GOOGLE_CLOUD_PROJECT"] = project

    # Region is optional — when empty, use the global multi-region endpoint.
    region = ""
    for key in ("GOOGLE_CLOUD_REGION", "CLOUD_ML_REGION", "GOOGLE_CLOUD_LOCATION"):
        val = _read_token(ctx, key)
        if val:
            region = val
            env[key] = val
            break

    # Normalize "global" to empty — the global Vertex AI endpoint uses the
    # plain hostname (aiplatform.googleapis.com), not a region-prefixed one.
    region = region.strip()
    if region.lower() == "global":
        region = ""

    # Construct Vertex AI base URL.
    if region:
        base_url = (
            f"https://{region}-aiplatform.googleapis.com"
            f"/v1beta1/projects/{project}/locations/{region}/endpoints/openapi"
        )
    else:
        base_url = (
            f"https://aiplatform.googleapis.com"
            f"/v1beta1/projects/{project}/locations/global/endpoints/openapi"
        )

    # Place ADC credentials file if staged.
    adc_content = ctx.read_file_secret("gcloud-adc")
    if adc_content:
        adc_dir = os.path.join(ctx.home, ".config", "gcloud")
        os.makedirs(adc_dir, exist_ok=True)
        adc_target = os.path.join(adc_dir, "application_default_credentials.json")
        scion_harness.atomic_write_text(adc_target, adc_content, mode=0o600)
        env["GOOGLE_APPLICATION_CREDENTIALS"] = adc_target
        ctx.info(f"placed ADC credentials at {adc_target}")

    # Resolve model ID — aliases map to the default Vertex AI model since
    # Vertex AI Model Garden may not have all xAI models available.
    raw_model = os.environ.get("SCION_MODEL", "").strip()
    if raw_model:
        aliases = ctx.harness_config.get("model_aliases")
        if not isinstance(aliases, dict):
            aliases = {}
        if raw_model.lower() in aliases:
            # Scion size alias (small, medium, large) — use default Vertex model.
            model_id = _VERTEX_MODEL_ID
            ctx.info(f"vertex-ai: resolved alias '{raw_model}' to {_VERTEX_MODEL_ID}")
        elif "/" not in raw_model:
            # Pre-resolved model name without publisher prefix (e.g., "grok-4"
            # from broker alias resolution). Not a valid Vertex AI model ID —
            # Vertex requires <publisher>/<model> format.
            model_id = _VERTEX_MODEL_ID
            ctx.info(
                f"vertex-ai: model '{raw_model}' lacks publisher prefix, "
                f"using default {_VERTEX_MODEL_ID}"
            )
        else:
            # Fully-qualified model ID with publisher prefix (e.g., "xai/grok-4.2").
            model_id = raw_model
    else:
        model_id = _VERTEX_MODEL_ID

    # Write Vertex AI model config to config.toml.
    _write_vertex_config(ctx, base_url, model_id)

    # Create a model config block matching the raw SCION_MODEL name so that
    # --model <name> on the command line (injected by the Go side) routes
    # through vertex-ai instead of falling back to the direct xAI API.
    if raw_model and raw_model != _VERTEX_MODEL_CONFIG_NAME:
        _write_vertex_model_alias(ctx, base_url, model_id, raw_model)

    # Set GROK_DEFAULT_MODEL so grok uses the vertex-grok config block.
    # This is belt-and-suspenders alongside [models] default in config.toml —
    # the env var cannot be overwritten by grok's /model command at runtime.
    env["GROK_DEFAULT_MODEL"] = _VERTEX_MODEL_CONFIG_NAME

    ctx.info(f"vertex-ai: project={project} model={model_id} base_url={base_url}")

    return {"vertex_ai": True, "vertex_base_url": base_url}


def _write_vertex_config(
    ctx: scion_harness.ProvisionContext,
    base_url: str,
    model_id: str,
) -> None:
    """Append Vertex AI auth_provider and model config to config.toml."""
    config_path = os.path.join(_grok_config_dir(ctx), "config.toml")
    os.makedirs(os.path.dirname(config_path), exist_ok=True)

    existing = ""
    if os.path.isfile(config_path):
        try:
            with open(config_path, "r", encoding="utf-8") as f:
                existing = f.read()
        except OSError:
            pass

    # Strip any existing vertex config sections to avoid duplicates.
    cleaned = scion_harness.strip_toml_sections(
        existing,
        lambda line: (
            line == f"[auth_provider.{_VERTEX_AUTH_PROVIDER_NAME}]"
            or line == f"[model.{_VERTEX_MODEL_CONFIG_NAME}]"
            or line == "[models]"
        ),
    )

    # Build the vertex config block.
    vertex_toml = f'''[auth_provider.{_VERTEX_AUTH_PROVIDER_NAME}]
command = "gcloud auth print-access-token"

[model.{_VERTEX_MODEL_CONFIG_NAME}]
model = "{scion_harness.toml_escape(model_id)}"
base_url = "{scion_harness.toml_escape(base_url)}"
auth_provider = "{_VERTEX_AUTH_PROVIDER_NAME}"
api_backend = "chat_completions"
supports_backend_search = false

[models]
default = "{_VERTEX_MODEL_CONFIG_NAME}"'''

    content = cleaned.rstrip("\n")
    if content:
        content += "\n\n"
    content += vertex_toml + "\n"

    # Key-path managed keys (rather than bare "auth_provider"/"model") so
    # this write can't be fooled into silently accepting damage to some
    # other tool's [auth_provider.*] or [model.*] sub-table — only the
    # named vertex-grok ones are actually written here. "models" stays a
    # bare top-level key: the whole [models] table (just `default = ...`)
    # is unconditionally replaced by this write, not a shared table this
    # site owns only part of.
    if not scion_harness.write_toml_if_preserves(
        ctx, config_path, existing, content,
        managed_keys={
            ("auth_provider", _VERTEX_AUTH_PROVIDER_NAME),
            ("model", _VERTEX_MODEL_CONFIG_NAME),
            "models",
        },
        what="vertex-ai auth/model config",
    ):
        # A vertex-auth agent with no vertex config is guaranteed broken —
        # grok would fall back to the direct xAI API and fail auth outright
        # — so this must fail loudly rather than continue as if it
        # succeeded (ptone/scion#2427 review round 1, R2). The caller
        # (_configure_vertex_ai) must not reach its GROK_DEFAULT_MODEL env
        # export or success log after this.
        raise scion_harness.ProvisionError(
            f"vertex-ai: failed to write auth_provider/model/models config "
            f"to {config_path}; grok would start without vertex routing "
            "configured (see the preceding warning for what blocked the "
            "write)"
        )


def _write_vertex_model_alias(
    ctx: scion_harness.ProvisionContext,
    base_url: str,
    model_id: str,
    alias_name: str,
) -> None:
    """Add a model config block that aliases a raw model name to vertex-ai.

    When the Go side injects --model <name> on the command line, grok looks
    for [model.<name>] in config.toml. Without this block, grok falls back to
    the direct xAI API and gets a 401 when vertex-ai auth is in use.
    """
    config_path = os.path.join(_grok_config_dir(ctx), "config.toml")
    if not os.path.isfile(config_path):
        return  # _write_vertex_config should have created it

    with open(config_path, "r", encoding="utf-8") as f:
        original = f.read()

    # Strip any existing block with this alias name to avoid duplicates.
    # Only match the bare-key form ([model.<alias_name>]) when alias_name is
    # actually a valid TOML bare key: for a dotted name like "grok-4.2" (the
    # realistic case — model names commonly contain dots), that bare-looking
    # header is a *different* TOML path ([model.grok-4]["2"], not
    # [model."grok-4.2"]), so matching it here would strip and discard an
    # unrelated user table instead of leaving it alone, turning a harmless
    # hand-written overlay into a hard ProvisionError from the ("model",
    # alias_name) key-path check below (ptone/scion#2427 review round 3).
    escaped_alias = scion_harness.toml_escape(alias_name)
    headers = {f'[model."{escaped_alias}"]'}
    if _TOML_BARE_KEY_RE.match(alias_name):
        headers.add(f"[model.{alias_name}]")
    content = scion_harness.strip_toml_sections(original, lambda line: line in headers)

    # Append the alias block.  Use quoted key so dots in the model name
    # (e.g. "grok-4.6") are treated as a single key, not a TOML path.
    alias_toml = f'''
[model."{escaped_alias}"]
model = "{scion_harness.toml_escape(model_id)}"
base_url = "{scion_harness.toml_escape(base_url)}"
auth_provider = "{_VERTEX_AUTH_PROVIDER_NAME}"
api_backend = "chat_completions"
supports_backend_search = false'''

    content = content.rstrip("\n") + "\n" + alias_toml + "\n"
    # Key-path ("model", alias_name), not bare "model": this write owns
    # only its own alias sub-table, not the whole shared [model.*] table
    # (which also holds _write_vertex_config's own vertex-grok block and
    # possibly a user's [model.custom]).
    if not scion_harness.write_toml_if_preserves(
        ctx, config_path, original, content,
        managed_keys={("model", alias_name)},
        what=f"vertex-ai model alias '{alias_name}'",
    ):
        # Without this alias block, grok falls back to the direct xAI API
        # for --model <alias_name> and gets a 401 when vertex-ai auth is in
        # use — the same guaranteed-broken-if-missing outcome
        # _write_vertex_config's own raise is about, so this must also fail
        # loudly instead of warning and continuing into a misleading
        # success log (ptone/scion#2427 review round 2, R2-a).
        raise scion_harness.ProvisionError(
            f"vertex-ai: failed to write model alias '{alias_name}' to "
            f"{config_path}; grok would fall back to the direct xAI API "
            "for this model and fail auth (see the preceding warning for "
            "what blocked the write)"
        )
    ctx.info(f"vertex-ai: created model alias '{alias_name}' -> vertex endpoint")


# ---------------------------------------------------------------------------
# MCP translation – TOML output for grok config
# ---------------------------------------------------------------------------


# TOML bare key: alphanumeric, dash, underscore only (TOML v1.0 §3.1).
_TOML_BARE_KEY_RE = _re.compile(r"^[A-Za-z0-9_-]+$")


def _write_mcp_toml(ctx: scion_harness.ProvisionContext, servers: dict[str, Any]) -> None:
    """Write MCP servers to ~/.grok/config.toml as [mcp_servers.*] sections."""
    config_path = os.path.join(_grok_config_dir(ctx), "config.toml")
    os.makedirs(os.path.dirname(config_path), exist_ok=True)

    existing = ""
    if os.path.isfile(config_path):
        try:
            with open(config_path, "r", encoding="utf-8") as f:
                existing = f.read()
        except OSError:
            pass

    # Strip old [mcp_servers.*] sections.
    cleaned = scion_harness.strip_toml_sections(
        existing,
        lambda line: line.startswith("[mcp_servers.") and line.endswith("]"),
    )

    # Build new TOML sections.
    sections: list[str] = []
    for name in sorted(servers.keys()):
        if not _TOML_BARE_KEY_RE.match(name):
            ctx.warn(
                f"mcp server {name!r}: name is not a valid TOML bare key "
                "(alphanumeric, dash, underscore only); skipping"
            )
            continue
        entry = servers[name]
        lines: list[str] = [f"[mcp_servers.{name}]"]
        for key in sorted(entry.keys()):
            value = entry[key]
            if isinstance(value, str):
                lines.append(f'{key} = "{scion_harness.toml_escape(value)}"')
            elif isinstance(value, list):
                lines.append(f"{key} = {scion_harness.toml_string_array(value)}")
            elif isinstance(value, dict):
                lines.append(f"{key} = {scion_harness.toml_inline_table(value)}")
        sections.append("\n".join(lines))

    new_content = cleaned.rstrip("\n")
    if sections:
        if new_content:
            new_content += "\n\n"
        new_content += "\n\n".join(sections) + "\n"
    elif new_content:
        new_content += "\n"

    scion_harness.write_toml_if_preserves(
        ctx, config_path, existing, new_content,
        managed_keys={"mcp_servers"}, what="MCP server registration",
    )


# ---------------------------------------------------------------------------
# Config hardening – managed TOML block
# ---------------------------------------------------------------------------

_MANAGED_BEGIN = "# BEGIN SCION MANAGED"
_MANAGED_END = "# END SCION MANAGED"

_HARDENING_TOML = """\
# BEGIN SCION MANAGED
[cli]
auto_update = false

[features]
telemetry = false
feedback = false

[memory]
enabled = false

[subagents]
enabled = false
# END SCION MANAGED"""


def _harden_config(ctx: scion_harness.ProvisionContext) -> None:
    """Write hardened settings to ~/.grok/config.toml with managed markers."""
    config_path = os.path.join(_grok_config_dir(ctx), "config.toml")
    os.makedirs(os.path.dirname(config_path), exist_ok=True)

    existing = ""
    if os.path.isfile(config_path):
        try:
            with open(config_path, "r", encoding="utf-8") as f:
                existing = f.read()
        except OSError:
            pass

    # Strip any existing managed block.
    content = existing
    begin_idx = content.find(_MANAGED_BEGIN)
    if begin_idx != -1:
        end_idx = content.find(_MANAGED_END, begin_idx)
        if end_idx != -1:
            end_idx += len(_MANAGED_END)
            # Consume trailing newline.
            if end_idx < len(content) and content[end_idx] == "\n":
                end_idx += 1
            content = content[:begin_idx] + content[end_idx:]

    # Also strip standalone sections that overlap with our managed settings.
    for section_name in ("cli", "features", "memory", "subagents"):
        content = scion_harness.strip_toml_sections(
            content,
            lambda line, sn=section_name: line == f"[{sn}]",
        )

    content = content.rstrip("\n")
    if content:
        content += "\n\n"
    content += _HARDENING_TOML + "\n"

    scion_harness.write_toml_if_preserves(
        ctx, config_path, existing, content,
        managed_keys={"cli", "features", "memory", "subagents"},
        what="config hardening",
    )


# ---------------------------------------------------------------------------
# Telemetry – native OTel export
# ---------------------------------------------------------------------------
#
# Grok's native OTel export always targets sciontool's local receiver.
# Previously this fell back to the shared SCION_OTEL_ENDPOINT (the cloud
# telemetry config alias) and copied cloud headers/CA into the harness env,
# which sent native telemetry straight to the cloud endpoint, bypassing
# sciontool's redaction and identity stamping entirely (fork issue #2053,
# phase 0). Grok's own OTel env vars (GROK_TELEMETRY_ENABLED,
# GROK_EXTERNAL_OTEL) and its exact protocol support are unverified against
# primary docs (findings-q4-harness-matrix.md); this phase keeps the
# pre-existing gRPC-on-4317 assumption, which already matches sciontool's
# local gRPC receiver default (the same one claude/codex use), and fixes only
# the endpoint/header/CA bypass. Verifying Grok's actual OTel wire support is
# left to a follow-up, alongside grok-build usage derivation (out of scope,
# D6/D8).

_OTEL_PROTOCOL = "grpc"
_DEFAULT_OTEL_GRPC_PORT = "4317"


def _telemetry_enabled(telemetry: dict[str, Any] | None) -> bool:
    """Return True when the effective telemetry config says 'enabled'."""
    if not telemetry:
        return False
    enabled = telemetry.get("enabled")
    if enabled is None:
        return True
    return bool(enabled)


def _resolve_endpoint(env: dict[str, str] | None) -> str:
    """Resolve the OTLP endpoint Grok's native exporter is pointed at.

    Always the local sciontool receiver, unless SCION_GROK_BUILD_OTEL_ENDPOINT
    is set. That override is a local-debugging escape hatch only: it bypasses
    sciontool's redaction and identity stamping entirely, so it must never be
    pointed at anything but a local collector. The generic SCION_OTEL_ENDPOINT
    (the shared cloud-config alias) and telemetry.cloud.endpoint are
    deliberately NOT honored here anymore.
    """
    env = env or {}
    override = (env.get("SCION_GROK_BUILD_OTEL_ENDPOINT") or os.environ.get("SCION_GROK_BUILD_OTEL_ENDPOINT") or "").strip()
    if override:
        return override
    port = str(env.get("SCION_OTEL_GRPC_PORT") or os.environ.get("SCION_OTEL_GRPC_PORT") or _DEFAULT_OTEL_GRPC_PORT)
    if not port.isdecimal() or not 1 <= int(port) <= 65535:
        raise scion_harness.ProvisionError("invalid local telemetry gRPC port")
    return f"http://127.0.0.1:{port}"


def _build_telemetry_env(env: dict[str, str] | None) -> dict[str, str]:
    """Build env vars that direct Grok's native OTel emitter to sciontool.

    Grok's OTel export honours standard OpenTelemetry SDK environment
    variables (OTEL_*) and the grok-specific GROK_TELEMETRY_ENABLED and
    GROK_EXTERNAL_OTEL flags. This always targets the local receiver -- see
    _resolve_endpoint.
    """
    return {
        "GROK_TELEMETRY_ENABLED": "true",
        "GROK_EXTERNAL_OTEL": "true",
        "OTEL_EXPORTER_OTLP_ENDPOINT": _resolve_endpoint(env),
        "OTEL_EXPORTER_OTLP_PROTOCOL": _OTEL_PROTOCOL,
        "OTEL_METRICS_EXPORTER": "otlp",
        "OTEL_LOGS_EXPORTER": "otlp",
        "OTEL_METRIC_EXPORT_INTERVAL": "30000",
    }


# ---------------------------------------------------------------------------
# Hooks – sciontool event bridge
# ---------------------------------------------------------------------------

# All hook events.
_GROK_HOOK_EVENTS = [
    "SessionStart",
    "SessionEnd",
    "UserPromptSubmit",
    "Stop",
    "StopFailure",
    "StopCancelled",
    "PreToolUse",
    "PostToolUse",
    "PostToolUseFailure",
    "SubagentStop",
    "Notification",
    "PermissionDenied",
    "SubagentStart",
    "PreCompact",
    "PostCompact",
]


def _write_hooks(ctx: scion_harness.ProvisionContext) -> None:
    """Write hooks/scion.json under the grok config dir, wiring events to sciontool.

    Each hook fires ``sciontool hook --dialect=grok-build`` which processes
    the event through the grok-build mapping dialect.

    SessionStart/SessionEnd use echo with synthetic JSON payloads since
    those events may not provide stdin. All other events use cat to pipe
    the native payload.
    """
    hooks: dict[str, list[dict[str, Any]]] = {}
    for event in _GROK_HOOK_EVENTS:
        if event == "SessionStart":
            cmd = (
                "echo '{\"hookEventName\": \"SessionStart\", \"source\": \"new\"}' "
                "| sciontool hook --dialect=grok-build"
            )
        elif event == "SessionEnd":
            cmd = (
                "echo '{\"hookEventName\": \"SessionEnd\", \"reason\": \"end_turn\"}' "
                "| sciontool hook --dialect=grok-build"
            )
        else:
            cmd = "cat | sciontool hook --dialect=grok-build"
        timeout = 60 if event in ("Stop", "SubagentStop") else 5
        hooks[event] = [
            {
                "hooks": [
                    {
                        "type": "command",
                        "command": cmd,
                        "timeout": timeout,
                    }
                ]
            }
        ]

    hooks_data: dict[str, Any] = {"hooks": hooks}

    hooks_dir = os.path.join(_grok_config_dir(ctx), "hooks")
    os.makedirs(hooks_dir, exist_ok=True)
    hooks_path = os.path.join(hooks_dir, "scion.json")
    try:
        scion_harness.atomic_write_json(hooks_path, hooks_data)
    except (OSError, PermissionError) as exc:
        print(
            f"grok-build provision: warning: could not write hooks to "
            f"{hooks_path}: {exc}",
            file=sys.stderr,
        )


# ---------------------------------------------------------------------------
# Main provision function
# ---------------------------------------------------------------------------


def provision(ctx: scion_harness.ProvisionContext) -> None:
    """Main provisioning logic for the Grok Build harness."""

    # Auth selection.
    try:
        resolved = ctx.select_auth(AUTH)
    except scion_harness.ProvisionError:
        if ctx.explicit_type:
            raise
        ctx.info("auth selection failed; falling back to no-auth mode")
        resolved = scion_harness.ResolvedAuth(method="none")

    env: dict[str, str] = {}
    extra: dict[str, Any] | None = None
    if resolved.method == "api-key" and resolved.env_key:
        secret = _read_token(ctx, resolved.env_key)
        if not secret:
            raise scion_harness.ProvisionError(
                f"chose api-key ({resolved.env_key}) but no secret "
                "value was staged at the recorded path; check ApplyAuthSettings"
            )
        env["XAI_API_KEY"] = secret

    if resolved.method == "auth-file":
        _write_auth_file(ctx)
        extra = {"auth_file_written": True}

    if resolved.method == "vertex-ai":
        extra = _configure_vertex_ai(ctx, env)

    # --- Model resolution ---------------------------------------------------
    # vertex-ai sets GROK_DEFAULT_MODEL in _configure_vertex_ai.
    if resolved.method != "vertex-ai":
        resolved_model = scion_harness.resolve_model(ctx)
        if resolved_model:
            env["GROK_DEFAULT_MODEL"] = resolved_model

    # --- Telemetry: inject native OTel env vars when enabled ----------------
    telemetry_payload = ctx.telemetry
    telemetry = telemetry_payload.get("telemetry") if isinstance(telemetry_payload, dict) else None
    env_overlay = telemetry_payload.get("env") if isinstance(telemetry_payload, dict) else None
    if not isinstance(env_overlay, dict):
        env_overlay = None

    telemetry_enabled = _telemetry_enabled(telemetry if isinstance(telemetry, dict) else None)
    if telemetry_enabled:
        otel_env = _build_telemetry_env(env_overlay)
        env.update(otel_env)
        ctx.info(f"telemetry: injected {len(otel_env)} OTel env var(s)")
    # SCION_NATIVE_TELEMETRY_POLICY lets the env guard (hooks/envoverlay.go)
    # protect the OTEL_* vars above from being overridden at runtime.
    env["SCION_NATIVE_TELEMETRY_POLICY"] = "enabled" if telemetry_enabled else "disabled"
    # --- end telemetry ------------------------------------------------------

    ctx.write_outputs(resolved, env=env, extra=extra)

    # --- System prompt (native routing) -------------------------------------
    _apply_native_system_prompt(ctx)

    # --- Instructions projection --------------------------------------------
    harness_cfg = ctx.harness_config
    instructions_file = harness_cfg.get("instructions_file") or ".grok/AGENTS.md"
    target = os.path.join(ctx.home, instructions_file)
    os.makedirs(os.path.dirname(target), exist_ok=True)
    # include_skills left at default False: config.yaml declares skills_dir,
    # so the host-side provisioner installs skills as individual files.
    try:
        scion_harness.project_instructions(ctx, target, system_prompt_mode="none")
    except OSError as exc:
        ctx.warn(f"failed to project instructions: {exc}")

    # --- MCP translation (TOML output) --------------------------------------
    def translate_mcp(name: str, spec: dict[str, Any]) -> dict[str, Any] | None:
        transport = (spec.get("transport") or "").strip()

        if transport == "stdio":
            cmd = spec.get("command")
            if not isinstance(cmd, str) or not cmd:
                ctx.info(f"mcp server {name!r}: stdio transport missing command")
                return None
            out: dict[str, Any] = {"command": cmd}
            args = spec.get("args") or []
            if isinstance(args, list) and args:
                out["args"] = [str(a) for a in args]
            env_map = spec.get("env")
            if isinstance(env_map, dict) and env_map:
                out["env"] = {str(k): str(v) for k, v in env_map.items()}
            return out

        if transport in ("sse", "streamable-http"):
            url = spec.get("url")
            if not isinstance(url, str) or not url:
                ctx.info(
                    f"mcp server {name!r}: {transport} transport missing url"
                )
                return None
            out = {"url": url}
            headers = spec.get("headers")
            if isinstance(headers, dict) and headers:
                out["headers"] = {str(k): str(v) for k, v in headers.items()}
            return out

        ctx.info(f"mcp server {name!r}: unsupported transport {transport!r}")
        return None

    scion_harness.apply_mcp_translated(
        ctx, translate_mcp, lambda servers: _write_mcp_toml(ctx, servers)
    )

    # --- Config hardening ---------------------------------------------------
    _harden_config(ctx)

    # --- Hook wiring --------------------------------------------------------
    _write_hooks(ctx)

    ctx.info(f"method={resolved.method}")


if __name__ == "__main__":
    scion_harness.run("grok-build", provision)
