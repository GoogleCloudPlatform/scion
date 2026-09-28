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
"""Codex container-side provisioner.

Runs inside the agent container during the pre-start lifecycle hook, invoked
by `sciontool harness provision --manifest ...`. The host-side
ContainerScriptHarness has already:

  * Staged this script and config.yaml under $HOME/.scion/harness/.
  * Written inputs/auth-candidates.json with the env-var names + paths to
    secret-value files under $HOME/.scion/harness/secrets/<NAME>.
  * Written inputs/telemetry.json describing the effective TelemetryConfig
    (the same struct ApplyTelemetrySettings receives).
  * Mounted any auth file (e.g. ~/.codex/auth.json) at the declared
    container_path, when auth-file mode is in use.

This script's job:

  1. Determine which auth method Codex will use, honoring an explicit
     selection if present and otherwise applying the same precedence:
         CodexAPIKey > OpenAIAPIKey > CodexAuthFile.
  2. For api-key methods, read the secret value and write
     `~/.codex/auth.json` with `{"auth_mode": "apikey", "OPENAI_API_KEY": "<value>"}`.
  3. Reconcile the [otel] section in `~/.codex/config.toml` from
     inputs/telemetry.json.
  4. Write outputs/resolved-auth.json describing the chosen method.
  5. Write outputs/env.json (intentionally empty).

The script is stdlib-only; it does manual TOML editing because tomllib (3.11+)
is read-only and we must avoid third-party dependencies.
"""

from __future__ import annotations

import json
import os
import sys
from typing import Any

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import scion_harness  # type: ignore[import-not-found]

assert scion_harness.INTERFACE_VERSION >= 2, (
    "codex provision.py requires scion_harness INTERFACE_VERSION >= 2; "
    f"got {scion_harness.INTERFACE_VERSION}"
)

CODEX_AUTH_FILE = "~/.codex/auth.json"
CODEX_CONFIG_FILE = "~/.codex/config.toml"

AUTH = scion_harness.AuthSpec(
    "codex",
    [
        scion_harness.env_method(
            "api-key",
            any_of=["CODEX_API_KEY", "OPENAI_API_KEY"],
            hint="set CODEX_API_KEY or OPENAI_API_KEY",
        ),
        scion_harness.file_method(
            "auth-file",
            path=CODEX_AUTH_FILE,
            hint=f"provide auth credentials at {CODEX_AUTH_FILE}",
            secret_key="CODEX_AUTH",
        ),
    ],
)


# --- Native auth writers ---------------------------------------------------


def _write_codex_auth_json(api_key: str) -> None:
    """Mirror the compiled ApplyAuthSettings: {"auth_mode": "apikey", ...}."""
    auth_dir = scion_harness.expand_path("~/.codex")
    os.makedirs(auth_dir, exist_ok=True)
    target = os.path.join(auth_dir, "auth.json")
    payload = {"auth_mode": "apikey", "OPENAI_API_KEY": api_key}
    tmp = target + ".tmp"
    with open(tmp, "w", encoding="utf-8") as f:
        json.dump(payload, f, indent=2)
        f.write("\n")
    os.chmod(tmp, 0o600)
    os.replace(tmp, target)


def _write_codex_auth_file(ctx: scion_harness.ProvisionContext) -> None:
    """Write ~/.codex/auth.json from a staged CODEX_AUTH file secret."""
    auth_content = _read_file_secret(ctx, "CODEX_AUTH")
    if not auth_content:
        return
    if not auth_content.strip():
        raise scion_harness.ProvisionError("CODEX_AUTH secret is empty")
    try:
        json.loads(auth_content)
    except json.JSONDecodeError as exc:
        raise scion_harness.ProvisionError(
            f"CODEX_AUTH secret is not valid JSON: {exc}"
        ) from exc
    auth_dir = scion_harness.expand_path("~/.codex")
    os.makedirs(auth_dir, exist_ok=True)
    target = os.path.join(auth_dir, "auth.json")
    tmp = target + ".tmp"
    with open(tmp, "w", encoding="utf-8") as f:
        f.write(auth_content)
    os.chmod(tmp, 0o600)
    os.replace(tmp, target)


# --- TOML reconciliation (otel) --------------------------------------------


def _resolve_endpoint(telemetry: dict[str, Any] | None, env: dict[str, str] | None) -> str:
    env = env or {}
    port = str(env.get("SCION_OTEL_GRPC_PORT") or "4317")
    if not port.isdecimal() or not 1 <= int(port) <= 65535:
        raise scion_harness.ProvisionError("invalid local telemetry gRPC port")
    return f"http://127.0.0.1:{port}"


def _resolve_otel_environment(telemetry: dict[str, Any], env: dict[str, str] | None) -> str:
    env = env or {}
    for key in ("SCION_CODEX_OTEL_ENVIRONMENT", "SCION_OTEL_ENVIRONMENT", "SCION_SERVER_ENV"):
        v = (env.get(key) or "").strip()
        if v:
            return v

    resource = telemetry.get("resource") or {}
    if isinstance(resource, dict):
        for key in ("deployment.environment", "service.environment", "environment"):
            v = str(resource.get(key) or "").strip()
            if v:
                return v

    return "production"


def _telemetry_enabled(telemetry: dict[str, Any] | None) -> bool:
    if not telemetry:
        return False
    enabled = telemetry.get("enabled")
    if enabled is None:
        return True
    return bool(enabled)


def _build_otel_section(telemetry: dict[str, Any], env: dict[str, str] | None) -> str:
    endpoint = _resolve_endpoint(telemetry, env)
    environment = _resolve_otel_environment(telemetry, env)

    exporter_key = "otlp-grpc"

    lines = [
        "[otel]",
        f'environment = "{scion_harness.toml_escape(environment)}"',
        "log_user_prompt = false",
        f'metrics_exporter."{exporter_key}".endpoint = "{scion_harness.toml_escape(endpoint)}"',
        f'exporter."{exporter_key}".endpoint = "{scion_harness.toml_escape(endpoint)}"',
        f'trace_exporter."{exporter_key}".endpoint = "{scion_harness.toml_escape(endpoint)}"',
    ]

    return "\n".join(lines) + "\n"


def _resolve_reasoning_effort(level: int) -> str:
    """Map a thinking level (0-100) to OpenAI reasoning_effort (low/medium/high/xhigh)."""
    level = max(0, min(100, level))
    if level >= 76:
        return "xhigh"
    if level >= 51:
        return "high"
    if level >= 26:
        return "medium"
    return "low"


def _is_toml_key_line(line: str, key: str) -> bool:
    """True if line is a top-level TOML assignment for exactly `key`."""
    s = line.strip()
    if not s.startswith(key):
        return False
    rest = s[len(key):]
    return len(rest) > 0 and rest[0] in (" ", "=", "\t")


def _strip_toml_top_level_key(content: str, key: str) -> str:
    """Remove a top-level TOML key = value line from content."""
    lines = content.split("\n")
    kept = []
    in_section = False
    for line in lines:
        s = line.strip()
        if s.startswith("["):
            in_section = True
        if not in_section and _is_toml_key_line(line, key):
            continue
        kept.append(line)
    return "\n".join(kept)


def _reconcile_codex_toml(
    telemetry: dict[str, Any] | None,
    env: dict[str, str] | None,
    reasoning_effort: str | None = None,
) -> None:
    codex_dir = scion_harness.expand_path("~/.codex")
    os.makedirs(codex_dir, exist_ok=True)
    config_path = os.path.join(codex_dir, "config.toml")
    content = ""
    if os.path.isfile(config_path):
        with open(config_path, "r", encoding="utf-8") as f:
            content = f.read()
    content = _strip_toml_top_level_key(content, "reasoning_effort")
    content = _strip_toml_top_level_key(content, "model_reasoning_effort")
    content = scion_harness.strip_toml_sections(content, lambda h: h == "[otel]" or h.startswith("[otel."))

    if reasoning_effort:
        re_line = f'model_reasoning_effort = "{scion_harness.toml_escape(reasoning_effort)}"'
        content = content.rstrip("\n\t ") + "\n" + re_line + "\n"

    section = (_build_otel_section(telemetry or {}, env) if _telemetry_enabled(telemetry)
               else '[otel]\nexporter = "none"\nmetrics_exporter = "none"\ntrace_exporter = "none"\n')
    content = content.rstrip("\n\t ") + "\n\n" + section
    content = content.strip() + "\n"
    scion_harness.atomic_write_text(config_path, content)


# --- MCP server emission ---------------------------------------------------


def _build_mcp_section(name: str, spec: dict[str, Any]) -> str | None:
    """Translate a universal MCPServerConfig into a TOML section. None on skip."""
    transport = (spec.get("transport") or "").strip()
    body: list[str] = [f"[mcp_servers.{name}]"]

    if transport == "stdio":
        cmd = spec.get("command")
        if not isinstance(cmd, str) or not cmd:
            print(f"codex provision: mcp server {name!r}: stdio transport missing command", file=sys.stderr)
            return None
        body.append(f'command = "{scion_harness.toml_escape(cmd)}"')
        args = spec.get("args") or []
        if isinstance(args, list) and args:
            body.append(f"args = {scion_harness.toml_string_array([str(a) for a in args])}")
        env = spec.get("env")
        if isinstance(env, dict) and env:
            body.append(f"env = {scion_harness.toml_inline_table({str(k): str(v) for k, v in env.items()})}")
    elif transport in ("sse", "streamable-http"):
        url = spec.get("url")
        if not isinstance(url, str) or not url:
            print(f"codex provision: mcp server {name!r}: {transport} transport missing url", file=sys.stderr)
            return None
        body.append(f'url = "{scion_harness.toml_escape(url)}"')
        headers = spec.get("headers")
        if isinstance(headers, dict) and headers:
            body.append(f"http_headers = {scion_harness.toml_inline_table({str(k): str(v) for k, v in headers.items()})}")
    else:
        print(f"codex provision: mcp server {name!r}: unsupported transport {transport!r}", file=sys.stderr)
        return None

    return "\n".join(body) + "\n"


def _write_mcp_to_config(servers: dict[str, str]) -> None:
    """Write translated MCP server sections into ~/.codex/config.toml."""
    codex_dir = scion_harness.expand_path("~/.codex")
    os.makedirs(codex_dir, exist_ok=True)
    config_path = os.path.join(codex_dir, "config.toml")
    content = ""
    if os.path.isfile(config_path):
        with open(config_path, "r", encoding="utf-8") as f:
            content = f.read()
    content = scion_harness.strip_toml_sections(
        content, lambda h: h.startswith("[mcp_servers.")
    )
    sections = list(servers.values())
    appended = "\n".join(sections)
    content = content.rstrip("\n\t ") + "\n\n" + appended
    content = content.strip() + "\n"
    scion_harness.atomic_write_text(config_path, content)


# --- Entry point -----------------------------------------------------------


def _read_env_secret(ctx: scion_harness.ProvisionContext, name: str) -> str:
    """Read an env secret, expanding $HOME in the staged path."""
    path = ctx.env_secret_files.get(name, "")
    if not path:
        return ""
    path = scion_harness.expand_path(path)
    try:
        with open(path, "r", encoding="utf-8") as f:
            return f.read().rstrip("\r\n")
    except OSError:
        return ""


def _read_file_secret(ctx: scion_harness.ProvisionContext, name: str) -> str:
    """Read a file-type secret, expanding $HOME in the staged path."""
    path = ctx.file_secret_files.get(name, "")
    if not path:
        return ""
    path = scion_harness.expand_path(path)
    try:
        with open(path, "r", encoding="utf-8") as f:
            return f.read().rstrip("\r\n")
    except OSError:
        return ""


def provision(ctx: scion_harness.ProvisionContext) -> None:
    resolved = ctx.select_auth(AUTH)

    if resolved.method == "api-key":
        api_key = _read_env_secret(ctx, resolved.env_key)
        if not api_key:
            raise scion_harness.ProvisionError(
                f"chose api-key ({resolved.env_key}) but no secret value "
                "was staged at the recorded path; check ApplyAuthSettings"
            )
        _write_codex_auth_json(api_key)

    if resolved.method == "auth-file":
        _write_codex_auth_file(ctx)

    harness_cfg = ctx.harness_config
    instructions_file = str(harness_cfg.get("instructions_file") or ".codex/AGENTS.md")
    scion_harness.project_instructions(ctx, instructions_file)

    thinking_raw = os.environ.get("SCION_THINKING_LEVEL", "").strip()
    reasoning_effort: str | None = None
    if thinking_raw:
        try:
            thinking_level = int(thinking_raw)
            reasoning_effort = _resolve_reasoning_effort(thinking_level)
            ctx.info(f"thinking_level={thinking_level} reasoning_effort={reasoning_effort}")
        except ValueError:
            pass

    telemetry_payload = ctx.telemetry
    telemetry = telemetry_payload.get("telemetry") if isinstance(telemetry_payload, dict) else None
    env_overlay = telemetry_payload.get("env") if isinstance(telemetry_payload, dict) else None
    if not isinstance(env_overlay, dict):
        env_overlay = None
    _reconcile_codex_toml(
        telemetry if isinstance(telemetry, dict) else None,
        env_overlay,
        reasoning_effort=reasoning_effort,
    )

    extra: dict[str, Any] | None = None
    if resolved.method == "auth-file":
        extra = {"auth_file_written": True}
    ctx.write_outputs(resolved, env={
        "CODEX_HOME": os.path.join(ctx.home, ".codex"),
        "SCION_NATIVE_TELEMETRY_POLICY": "enabled" if _telemetry_enabled(telemetry) else "disabled",
    }, extra=extra)

    scion_harness.apply_mcp_translated(ctx, _build_mcp_section, _write_mcp_to_config)

    ctx.info(f"method={resolved.method}")


if __name__ == "__main__":
    scion_harness.run("codex", provision)
