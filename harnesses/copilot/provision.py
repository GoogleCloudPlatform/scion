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
"""Copilot container-side provisioner.

Runs inside the agent container during the pre-start lifecycle hook.
Uses scion_harness library for auth selection, instruction projection,
MCP translation, and output writing.

Copilot-native concerns handled here:
  - Auth token is always exposed as COPILOT_GITHUB_TOKEN in env.json.
  - MCP servers translate to Copilot's native format in ~/.copilot/mcp-config.json
    (stdio→local, sse/streamable-http→http).
  - Instructions project to ~/.copilot/copilot-instructions.md (configurable).
  - ~/.copilot/settings.json and config.json get sane defaults.
  - Native OTel telemetry, when enabled, always routes to sciontool's local
    receiver (never a cloud endpoint); see the Telemetry section below.

Exception to §4.2 (env.json placeholder policy): copilot receives its auth
token exclusively via env.json — the runtime env projection does not deliver
auth env vars to the copilot process.  Raw token values are written with 0600
permissions on the secret file.
"""

from __future__ import annotations

import json
import os
import sys
from typing import Any

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import scion_harness

assert scion_harness.INTERFACE_VERSION >= 2, (
    "copilot provision.py requires scion_harness INTERFACE_VERSION >= 2; "
    f"got {scion_harness.INTERFACE_VERSION}"
)

COPILOT_CONFIG_FILE = "~/.copilot/config.json"

AUTH = scion_harness.AuthSpec(
    "copilot",
    [
        scion_harness.env_method(
            "api-key",
            any_of=["COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"],
            hint=(
                "set COPILOT_GITHUB_TOKEN, GH_TOKEN, or GITHUB_TOKEN "
                'with a fine-grained PAT that has "Copilot Requests" permission'
            ),
            env_fallback=True,
        ),
        scion_harness.file_method(
            "auth-file",
            path=COPILOT_CONFIG_FILE,
            hint=f"provide copilot config at {COPILOT_CONFIG_FILE}",
            secret_key="COPILOT_CONFIG",
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


def _write_copilot_config_file(ctx: scion_harness.ProvisionContext) -> None:
    """Write ~/.copilot/config.json from a staged COPILOT_CONFIG file secret."""
    content = ctx.read_file_secret("COPILOT_CONFIG")
    if not content:
        return
    if not content.strip():
        raise scion_harness.ProvisionError("COPILOT_CONFIG secret is empty")
    try:
        lines = [ln for ln in content.splitlines() if not ln.strip().startswith("//")]
        json.loads("\n".join(lines))
    except json.JSONDecodeError as exc:
        raise scion_harness.ProvisionError(
            f"COPILOT_CONFIG secret is not valid JSON: {exc}"
        ) from exc
    config_dir = scion_harness.expand_path("~/.copilot")
    os.makedirs(config_dir, exist_ok=True)
    target = os.path.join(config_dir, "config.json")
    tmp = target + ".tmp"
    with open(tmp, "w", encoding="utf-8") as f:
        f.write(content)
    os.chmod(tmp, 0o600)
    os.replace(tmp, target)


def _write_mcp_config(ctx: scion_harness.ProvisionContext, servers: dict[str, Any]) -> None:
    """Write MCP servers to ~/.copilot/mcp-config.json."""
    config_dir = os.path.join(ctx.home, ".copilot")
    os.makedirs(config_dir, exist_ok=True)
    config_path = os.path.join(config_dir, "mcp-config.json")
    scion_harness.atomic_write_json(config_path, {"mcpServers": servers})


# ---------------------------------------------------------------------------
# Telemetry – native OTel export
# ---------------------------------------------------------------------------
#
# Copilot's native OTel export always targets sciontool's local receiver.
# Previously this fell back to the shared SCION_OTEL_ENDPOINT (the cloud
# telemetry config alias) and copied cloud headers/CA into the harness env,
# which sent native telemetry straight to the cloud endpoint, bypassing
# sciontool's redaction and identity stamping entirely (fork issue #2053,
# phase 0). Copilot CLI v1.0.88's enterprise-managed OTel export activates
# via COPILOT_OTEL_ENABLED (COPILOT_TELEMETRY_ENABLED is not a real flag) and
# supports only the otlp-http exporter type (no otlp-grpc); the sciontool
# receiver's HTTP endpoint in turn accepts only protobuf
# (receiver.go:319-321), so the protocol here is fixed at http/protobuf.

# Default local HTTP port for sciontool's OTLP receiver.
_DEFAULT_OTEL_HTTP_PORT = "4318"
_DEFAULT_OTEL_PROTOCOL = "http/protobuf"


def _telemetry_enabled(telemetry: dict[str, Any] | None) -> bool:
    """Return True when the effective telemetry config says 'enabled'."""
    if not telemetry:
        return False
    enabled = telemetry.get("enabled")
    if enabled is None:
        return True
    return bool(enabled)


def _resolve_endpoint(env: dict[str, str] | None) -> str:
    """Resolve the OTLP endpoint Copilot's native exporter is pointed at.

    Always the local sciontool receiver, unless SCION_COPILOT_OTEL_ENDPOINT is
    set. That override is a local-debugging escape hatch only: it bypasses
    sciontool's redaction and identity stamping entirely, so it must never be
    pointed at anything but a local collector. The generic SCION_OTEL_ENDPOINT
    (the shared cloud-config alias) and telemetry.cloud.endpoint are
    deliberately NOT honored here anymore.
    """
    env = env or {}
    override = (env.get("SCION_COPILOT_OTEL_ENDPOINT") or os.environ.get("SCION_COPILOT_OTEL_ENDPOINT") or "").strip()
    if override:
        return override
    port = str(env.get("SCION_OTEL_HTTP_PORT") or os.environ.get("SCION_OTEL_HTTP_PORT") or _DEFAULT_OTEL_HTTP_PORT)
    if not port.isdecimal() or not 1 <= int(port) <= 65535:
        raise scion_harness.ProvisionError("invalid local telemetry HTTP port")
    return f"http://127.0.0.1:{port}"


def _resolve_protocol(env: dict[str, str] | None) -> str:
    """Resolve the OTLP protocol.

    Always http/protobuf -- the only protocol the local receiver's HTTP
    endpoint accepts -- unless SCION_COPILOT_OTEL_PROTOCOL is set. Unlike
    SCION_COPILOT_OTEL_ENDPOINT (_resolve_endpoint), this override does not
    itself bypass redaction or identity stamping: it only changes the wire
    format used to reach whichever endpoint is in effect, so a debug
    endpoint pointed at a non-protobuf collector can still be reached.
    """
    env = env or {}
    override = (env.get("SCION_COPILOT_OTEL_PROTOCOL") or os.environ.get("SCION_COPILOT_OTEL_PROTOCOL") or "").strip()
    if override:
        return override
    return _DEFAULT_OTEL_PROTOCOL


def _build_telemetry_env(env: dict[str, str] | None) -> dict[str, str]:
    """Build env vars that direct Copilot CLI's native OTel emitter to sciontool.

    This always targets the local receiver -- see _resolve_endpoint.

    OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE=delta requests delta
    temporality for Copilot's native metrics (design §3.7). A real capture
    (phase 3a) shows Copilot CLI 1.0.89 does not honor this -- it has no
    documented temporality override at all, and every point it exports is
    cumulative regardless of this setting. The usage deriver's metric rule
    (ptone/scion#2053 phase 3a) converts cumulative points to per-export
    deltas itself rather than relying on the source to do it (design §5's
    copilot row). The env var is kept anyway: it is harmless, forward
    compatible if a future Copilot release adds real support, and not left
    to the default, which an unpinned CLI could change.
    """
    return {
        "COPILOT_OTEL_ENABLED": "true",
        "COPILOT_OTEL_EXPORTER_TYPE": "otlp-http",
        "OTEL_EXPORTER_OTLP_ENDPOINT": _resolve_endpoint(env),
        "OTEL_EXPORTER_OTLP_PROTOCOL": _resolve_protocol(env),
        "OTEL_METRICS_EXPORTER": "otlp",
        "OTEL_LOGS_EXPORTER": "otlp",
        "OTEL_METRIC_EXPORT_INTERVAL": "30000",
        "OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE": "delta",
    }


# ---------------------------------------------------------------------------
# Hooks – sciontool event bridge
# ---------------------------------------------------------------------------

_COPILOT_HOOK_EVENTS = [
    "sessionStart",
    "sessionEnd",
    "userPromptSubmitted",
    "preToolUse",
    "postToolUse",
    "errorOccurred",
    "agentStop",
    "subagentStop",
]


def _write_hooks(home: str) -> None:
    """Write ~/.copilot/hooks/scion.json wiring Copilot events to sciontool.

    Each hook fires ``sciontool hook <event> --dialect=copilot`` which creates
    a synthetic event and processes it through the copilot mapping dialect
    (staged as dialect.yaml alongside this script).
    """
    hooks: dict[str, list[dict[str, Any]]] = {}
    for event in _COPILOT_HOOK_EVENTS:
        hooks[event] = [
            {
                "type": "command",
                "bash": f"sciontool hook {event} --dialect=copilot",
            }
        ]

    hooks_data: dict[str, Any] = {"version": 1, "hooks": hooks}

    hooks_dir = os.path.join(home, ".copilot", "hooks")
    os.makedirs(hooks_dir, exist_ok=True)
    hooks_path = os.path.join(hooks_dir, "scion.json")
    try:
        scion_harness.atomic_write_json(hooks_path, hooks_data)
    except (OSError, PermissionError) as exc:
        print(
            f"copilot provision: warning: could not write hooks to "
            f"{hooks_path}: {exc}",
            file=sys.stderr,
        )


def _ensure_settings(ctx: scion_harness.ProvisionContext) -> None:
    """Ensure ~/.copilot/settings.json and config.json have sane defaults."""
    config_dir = os.path.join(ctx.home, ".copilot")
    os.makedirs(config_dir, exist_ok=True)

    settings_path = os.path.join(config_dir, "settings.json")
    settings: dict[str, Any] = {}
    if os.path.isfile(settings_path):
        try:
            loaded = scion_harness.load_json(settings_path)
            if isinstance(loaded, dict):
                settings = loaded
        except (OSError, json.JSONDecodeError):
            pass

    defaults = {"autoUpdate": False, "banner": "never"}
    changed = False
    for key, value in defaults.items():
        if key not in settings:
            settings[key] = value
            changed = True
    if changed:
        scion_harness.atomic_write_json(settings_path, settings)

    config_path = os.path.join(config_dir, "config.json")
    config: dict[str, Any] = {}
    if os.path.isfile(config_path):
        try:
            loaded = scion_harness.read_json_skipping_comment_lines(config_path)
            if isinstance(loaded, dict):
                config = loaded
        except (OSError, json.JSONDecodeError):
            pass

    folders = config.get("trustedFolders")
    if not isinstance(folders, list):
        config["trustedFolders"] = [ctx.workspace]
        scion_harness.atomic_write_json(config_path, config)
    elif ctx.workspace not in folders:
        folders.append(ctx.workspace)
        scion_harness.atomic_write_json(config_path, config)


def provision(ctx: scion_harness.ProvisionContext) -> None:
    """Main provisioning logic for the Copilot harness."""

    # Auth selection. Copilot falls back to no-auth when selection fails
    # and no explicit type was requested (preserves pre-library behavior).
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
        env["COPILOT_GITHUB_TOKEN"] = secret

    if resolved.method == "auth-file":
        _write_copilot_config_file(ctx)
        extra = {"config_file_written": True}

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
    if telemetry_enabled:
        # Copilot's usage (gen_ai.api.calls / scion.usage.tokens) is derived
        # by sciontool's receiver from the native gen_ai.client.inference.*
        # metrics (design §5's copilot row, ptone/scion#2053 phase 3a). This
        # is narrow to usage only (D4): tool and session hook telemetry are
        # unaffected, and unset here means no usage is published at all
        # (D10).
        env["SCION_USAGE_SOURCE"] = "native"
    # --- end telemetry ----------------------------------------------------

    ctx.write_outputs(resolved, env=env, extra=extra)

    harness_cfg = ctx.harness_config
    instructions_file = harness_cfg.get('instructions_file') or '.copilot/copilot-instructions.md'
    target = os.path.join(ctx.home, instructions_file)
    os.makedirs(os.path.dirname(target), exist_ok=True)
    try:
        scion_harness.project_instructions(ctx, target)
    except OSError as exc:
        ctx.warn(f"failed to project instructions: {exc}")

    def translate_mcp(name: str, spec: dict[str, Any]) -> dict[str, Any] | None:
        transport = (spec.get("transport") or "").strip()

        if transport == "stdio":
            cmd = spec.get("command")
            if not isinstance(cmd, str) or not cmd:
                ctx.info(f"mcp server {name!r}: stdio transport missing command")
                return None
            out: dict[str, Any] = {"type": "local", "command": cmd}
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
            out = {"type": "http", "url": url}
            headers = spec.get("headers")
            if isinstance(headers, dict) and headers:
                out["headers"] = {str(k): str(v) for k, v in headers.items()}
            return out

        ctx.info(f"mcp server {name!r}: unsupported transport {transport!r}")
        return None

    scion_harness.apply_mcp_translated(
        ctx, translate_mcp, lambda servers: _write_mcp_config(ctx, servers)
    )

    _write_hooks(ctx.home)

    try:
        _ensure_settings(ctx)
    except OSError as exc:
        ctx.warn(f"failed to write settings: {exc}")

    ctx.info(f"method={resolved.method}")


if __name__ == "__main__":
    scion_harness.run("copilot", provision)
