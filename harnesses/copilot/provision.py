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

    ctx.write_outputs(resolved, env=env, extra=extra)

    harness_cfg = ctx.harness_config
    instructions_file = harness_cfg.get('instructions_file') or '~/.copilot/copilot-instructions.md'
    target = os.path.expanduser(instructions_file)
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
