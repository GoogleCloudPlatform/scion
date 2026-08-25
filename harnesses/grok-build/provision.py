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
from urllib.parse import quote

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
    config_dir = os.environ.get("GROK_HOME") or scion_harness.expand_path("~/.grok")
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
            # Scion alias (small, medium, large) — use default Vertex model.
            model_id = _VERTEX_MODEL_ID
            ctx.info(f"vertex-ai: resolved alias '{raw_model}' to {_VERTEX_MODEL_ID}")
        else:
            # Explicit model ID (e.g., "xai/grok-4.2") — use as-is.
            model_id = raw_model
    else:
        model_id = _VERTEX_MODEL_ID

    # Write Vertex AI model config to config.toml.
    _write_vertex_config(ctx, base_url, model_id)

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
    config_path = os.path.join(ctx.home, ".grok", "config.toml")
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

[models]
default = "{_VERTEX_MODEL_CONFIG_NAME}"'''

    content = cleaned.rstrip("\n")
    if content:
        content += "\n\n"
    content += vertex_toml + "\n"

    scion_harness.atomic_write_text(config_path, content)


# ---------------------------------------------------------------------------
# MCP translation – TOML output for grok config
# ---------------------------------------------------------------------------


# TOML bare key: alphanumeric, dash, underscore only (TOML v1.0 §3.1).
_TOML_BARE_KEY_RE = _re.compile(r"^[A-Za-z0-9_-]+$")


def _write_mcp_toml(ctx: scion_harness.ProvisionContext, servers: dict[str, Any]) -> None:
    """Write MCP servers to ~/.grok/config.toml as [mcp_servers.*] sections."""
    config_path = os.path.join(ctx.home, ".grok", "config.toml")
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

    scion_harness.atomic_write_text(config_path, new_content)


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
    config_path = os.path.join(ctx.home, ".grok", "config.toml")
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

    scion_harness.atomic_write_text(config_path, content)


# ---------------------------------------------------------------------------
# Telemetry – native OTel export
# ---------------------------------------------------------------------------

_DEFAULT_OTEL_ENDPOINT = "http://localhost:4317"
_DEFAULT_OTEL_PROTOCOL = "grpc"


def _telemetry_enabled(telemetry: dict[str, Any] | None) -> bool:
    """Return True when the effective telemetry config says 'enabled'."""
    if not telemetry:
        return False
    enabled = telemetry.get("enabled")
    if enabled is None:
        return True
    return bool(enabled)


def _resolve_endpoint(telemetry: dict[str, Any] | None, env: dict[str, str] | None) -> str:
    """Resolve the OTLP endpoint from env overrides or telemetry config."""
    env = env or {}
    for key in ("SCION_GROK_BUILD_OTEL_ENDPOINT", "SCION_OTEL_ENDPOINT"):
        v = (env.get(key) or os.environ.get(key) or "").strip()
        if v:
            return v
    if telemetry and isinstance(telemetry.get("cloud"), dict):
        ep = (telemetry["cloud"].get("endpoint") or "").strip()
        if ep:
            return ep
    return _DEFAULT_OTEL_ENDPOINT


def _resolve_protocol(telemetry: dict[str, Any] | None, env: dict[str, str] | None) -> str:
    """Resolve the OTLP protocol from env overrides or telemetry config."""
    env = env or {}
    for key in ("SCION_GROK_BUILD_OTEL_PROTOCOL", "SCION_OTEL_PROTOCOL"):
        v = (env.get(key) or os.environ.get(key) or "").strip()
        if v:
            return v
    if telemetry and isinstance(telemetry.get("cloud"), dict):
        proto = (telemetry["cloud"].get("protocol") or "").strip()
        if proto:
            return proto
    return _DEFAULT_OTEL_PROTOCOL


def _build_telemetry_env(telemetry: dict[str, Any], env: dict[str, str] | None) -> dict[str, str]:
    """Build env vars that direct Grok's native OTel emitter to sciontool.

    Grok's OTel export honours standard OpenTelemetry SDK environment
    variables (OTEL_*) and the grok-specific GROK_TELEMETRY_ENABLED and
    GROK_EXTERNAL_OTEL flags.
    """
    env = env or {}
    endpoint = _resolve_endpoint(telemetry, env)
    protocol = _resolve_protocol(telemetry, env)

    otel_env: dict[str, str] = {
        "GROK_TELEMETRY_ENABLED": "true",
        "GROK_EXTERNAL_OTEL": "true",
        "OTEL_EXPORTER_OTLP_ENDPOINT": endpoint,
        "OTEL_EXPORTER_OTLP_PROTOCOL": protocol,
        "OTEL_METRICS_EXPORTER": "otlp",
        "OTEL_LOGS_EXPORTER": "otlp",
        "OTEL_METRIC_EXPORT_INTERVAL": "30000",
    }

    # Propagate custom headers when present.
    headers: dict[str, str] = {}
    for key in ("SCION_GROK_BUILD_OTEL_HEADERS", "SCION_OTEL_HEADERS"):
        v = (env.get(key) or os.environ.get(key) or "").strip()
        if v:
            try:
                parsed = json.loads(v)
                if isinstance(parsed, dict):
                    headers = parsed
                    break
            except json.JSONDecodeError:
                pass
    if not headers:
        cloud = telemetry.get("cloud") or {}
        if isinstance(cloud, dict) and isinstance(cloud.get("headers"), dict):
            headers = cloud["headers"]
    if headers:
        parts = [f"{k}={quote(str(v), safe='')}" for k, v in headers.items()]
        otel_env["OTEL_EXPORTER_OTLP_HEADERS"] = ",".join(sorted(parts))

    # TLS CA file for non-localhost collectors.
    ca_file = ""
    for key in ("SCION_GROK_BUILD_OTEL_CA_FILE", "SCION_OTEL_CA_FILE"):
        v = (env.get(key) or os.environ.get(key) or "").strip()
        if v:
            ca_file = v
            break
    if not ca_file:
        cloud = telemetry.get("cloud") or {}
        if isinstance(cloud, dict) and isinstance(cloud.get("tls"), dict):
            ca_file = str(cloud["tls"].get("ca_file") or "").strip()
    if ca_file:
        otel_env["OTEL_EXPORTER_OTLP_CERTIFICATE"] = ca_file

    return otel_env


# ---------------------------------------------------------------------------
# Hooks – sciontool event bridge
# ---------------------------------------------------------------------------

# Events that send synthetic echo payloads (no stdin from grok).
_ECHO_EVENTS = {"SessionStart", "SessionEnd"}

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
]


def _write_hooks(home: str) -> None:
    """Write ~/.grok/hooks/scion.json wiring Grok events to sciontool.

    Each hook fires ``sciontool hook --dialect=grok-build`` which processes
    the event through the grok-build mapping dialect.

    SessionStart/SessionEnd use echo with synthetic JSON payloads since
    those events may not provide stdin. All other events use cat to pipe
    the native payload.
    """
    hooks: dict[str, list[dict[str, Any]]] = {}
    for event in _GROK_HOOK_EVENTS:
        if event in _ECHO_EVENTS:
            cmd = (
                f"echo '{{\"hookEventName\": \"{event}\"}}' "
                f"| sciontool hook --dialect=grok-build"
            )
        else:
            cmd = "cat | sciontool hook --dialect=grok-build"
        timeout = 10 if event == "Stop" else 5
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

    hooks_dir = os.path.join(home, ".grok", "hooks")
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
    # The Go side does not populate ctx.model_resolution for out-of-tree
    # harnesses. Use the SCION_MODEL env var and resolve via model_aliases.
    # vertex-ai sets GROK_DEFAULT_MODEL in _configure_vertex_ai.
    if resolved.method != "vertex-ai":
        raw_model = os.environ.get("SCION_MODEL", "").strip()
        aliases = ctx.harness_config.get("model_aliases") or {}
        resolved_model = aliases.get(raw_model.lower(), raw_model) if raw_model else ""
        if resolved_model:
            env["GROK_DEFAULT_MODEL"] = resolved_model

    # --- Telemetry: inject native OTel env vars when enabled ----------------
    telemetry_payload = ctx.telemetry
    telemetry = telemetry_payload.get("telemetry") if isinstance(telemetry_payload, dict) else None
    env_overlay = telemetry_payload.get("env") if isinstance(telemetry_payload, dict) else None
    if not isinstance(env_overlay, dict):
        env_overlay = None

    if _telemetry_enabled(telemetry if isinstance(telemetry, dict) else None):
        otel_env = _build_telemetry_env(telemetry or {}, env_overlay)
        env.update(otel_env)
        ctx.info(f"telemetry: injected {len(otel_env)} OTel env var(s)")
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
    _write_hooks(ctx.home)

    ctx.info(f"method={resolved.method}")


if __name__ == "__main__":
    scion_harness.run("grok-build", provision)
