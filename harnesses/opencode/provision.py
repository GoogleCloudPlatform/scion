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
"""OpenCode container-side provisioner.

Runs inside the agent container during the pre-start lifecycle hook, invoked
by `sciontool harness provision --manifest ...`. The host-side
ContainerScriptHarness has already:

  * Staged this script and config.yaml under $HOME/.scion/harness/.
  * Projected available auth env vars into the container's launch environment
    (so the OpenCode child process will see ANTHROPIC_API_KEY, OPENAI_API_KEY,
    etc. — but `sciontool harness provision` strips them from THIS script's env
    for containment, so we read the *names* of available creds from
    inputs/auth-candidates.json instead of os.environ).
  * Mounted any auth file (e.g. ~/.local/share/opencode/auth.json) at the
    declared container_path.

This script's job is therefore minimal:

  1. Determine which auth method OpenCode will use, honoring an explicit
     selection if present and otherwise applying the same precedence as the
     compiled OpenCode harness:
         AnthropicAPIKey > OpenAIAPIKey > OpenCodeAuthFile > VertexAI.
  2. Fail (exit 1) with an actionable message if no method is available.
  3. Write outputs/resolved-auth.json describing the choice (for diagnostics
     and resume-time consistency).
  4. Write outputs/env.json — for api-key/auth-file this is empty (the
     harness child already inherits the projected env); for vertex-ai it
     contains ${VAR} placeholders so the host can expand at launch.

The script is intentionally stdlib-only so it works on any container image
that ships python3 (declared in config.yaml's required_image_tools).
"""

from __future__ import annotations

import json
import os
import sys
from typing import Any

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import scion_harness as sh  # noqa: E402

assert sh.INTERFACE_VERSION >= 2, (
    "opencode provision.py requires scion_harness INTERFACE_VERSION >= 2; "
    f"got {sh.INTERFACE_VERSION}. Update the staged scion_harness.py."
)

OPENCODE_AUTH_FILE = "~/.local/share/opencode/auth.json"
OPENCODE_CONFIG_FILE = "~/.config/opencode/opencode.json"

VALID_AUTH_TYPES = ("api-key", "auth-file", "vertex-ai")

AUTH = sh.AuthSpec(
    harness="opencode",
    methods=[
        sh.env_method(
            "api-key",
            any_of=["ANTHROPIC_API_KEY", "OPENAI_API_KEY"],
            hint="set ANTHROPIC_API_KEY or OPENAI_API_KEY",
        ),
        sh.file_method("auth-file", path=OPENCODE_AUTH_FILE, secret_key="OPENCODE_AUTH"),
    ],
)


# ---------------------------------------------------------------------------
# Vertex AI helpers (native — multi-group env check doesn't fit AuthSpec)
# ---------------------------------------------------------------------------

_VERTEX_PROJECT_VARS = ("GOOGLE_CLOUD_PROJECT", "VERTEXAI_PROJECT")
_VERTEX_LOCATION_VARS = ("GOOGLE_CLOUD_REGION", "GOOGLE_CLOUD_LOCATION", "VERTEX_LOCATION")


def _has_vertex(ctx: sh.ProvisionContext) -> bool:
    env_keys = ctx.env_keys
    has_project = bool(env_keys & set(_VERTEX_PROJECT_VARS))
    has_location = bool(env_keys & set(_VERTEX_LOCATION_VARS))
    # gcp_metadata_mode is not currently populated in auth-candidates.json by
    # the Go staging layer; this guard is reserved for future use.
    gcp_meta_mode = str(ctx.candidates.get("gcp_metadata_mode") or "").strip()
    return has_project and has_location and gcp_meta_mode != "block"


def _select_vertex_explicit(ctx: sh.ProvisionContext) -> sh.ResolvedAuth:
    env_keys = ctx.env_keys
    has_project = bool(env_keys & set(_VERTEX_PROJECT_VARS))
    has_location = bool(env_keys & set(_VERTEX_LOCATION_VARS))
    gcp_meta_mode = str(ctx.candidates.get("gcp_metadata_mode") or "").strip()

    if not has_project or not has_location:
        raise sh.ProvisionError(
            "opencode: auth type 'vertex-ai' selected but missing "
            "GOOGLE_CLOUD_PROJECT/VERTEXAI_PROJECT and/or "
            "GOOGLE_CLOUD_REGION/GOOGLE_CLOUD_LOCATION/VERTEX_LOCATION"
        )
    if gcp_meta_mode == "block":
        raise sh.ProvisionError(
            "opencode: auth type 'vertex-ai' selected but GCP metadata "
            "access is blocked (gcp_metadata_mode='block')"
        )
    return sh.ResolvedAuth(method="vertex-ai")


def _vertex_env_overlay(ctx: sh.ProvisionContext) -> dict[str, str]:
    """Build vertex env.json with ${VAR} placeholders (§4.2)."""
    env_keys = ctx.env_keys
    project_key = next((k for k in _VERTEX_PROJECT_VARS if k in env_keys), "")
    location_key = next((k for k in _VERTEX_LOCATION_VARS if k in env_keys), "")

    env: dict[str, str] = {}
    if project_key:
        placeholder = f"${{{project_key}}}"
        env["VERTEXAI_PROJECT"] = placeholder
        env["GOOGLE_CLOUD_PROJECT"] = placeholder
    if location_key:
        placeholder = f"${{{location_key}}}"
        env["VERTEX_LOCATION"] = placeholder
        env["GOOGLE_CLOUD_REGION"] = placeholder
    return env


# ---------------------------------------------------------------------------
# MCP translation (native — OpenCode uses local/remote, not stdio/sse)
# ---------------------------------------------------------------------------


def _translate_mcp_server(name: str, spec: dict[str, Any]) -> dict[str, Any] | None:
    """Translate a universal MCPServerConfig into OpenCode's native shape.

    OpenCode uses a different schema from Claude/Gemini:
      - parent key is "mcp" (not "mcpServers")
      - "type": "local" | "remote" instead of stdio/sse/streamable-http
      - local entries take a single "command" array (no separate args)
      - local env var key is "environment" (not "env")
      - remote entries take url/headers/oauth

    Returns None if the entry is unusable (warns to stderr).
    """
    transport = (spec.get("transport") or "").strip()

    if transport == "stdio":
        cmd = spec.get("command")
        if not isinstance(cmd, str) or not cmd:
            print(f"opencode provision: mcp server {name!r}: stdio transport missing command", file=sys.stderr)
            return None
        args = spec.get("args") or []
        if not isinstance(args, list):
            args = []
        out: dict[str, Any] = {
            "type": "local",
            "command": [cmd] + [str(a) for a in args],
        }
        env = spec.get("env")
        if isinstance(env, dict) and env:
            out["environment"] = {str(k): str(v) for k, v in env.items()}
        return out

    if transport in ("sse", "streamable-http"):
        url = spec.get("url")
        if not isinstance(url, str) or not url:
            print(f"opencode provision: mcp server {name!r}: {transport} transport missing url", file=sys.stderr)
            return None
        out = {"type": "remote", "url": url}
        headers = spec.get("headers")
        if isinstance(headers, dict) and headers:
            out["headers"] = {str(k): str(v) for k, v in headers.items()}
        return out

    print(f"opencode provision: mcp server {name!r}: unsupported transport {transport!r}", file=sys.stderr)
    return None


def _write_opencode_auth_file(ctx: sh.ProvisionContext) -> None:
    """Write ~/.local/share/opencode/auth.json from a staged OPENCODE_AUTH file secret."""
    if "OPENCODE_AUTH" not in ctx.file_secret_files:
        return
    content = ctx.read_file_secret("OPENCODE_AUTH")
    if not content.strip():
        raise sh.ProvisionError("OPENCODE_AUTH secret is empty")
    try:
        json.loads(content)
    except json.JSONDecodeError as exc:
        raise sh.ProvisionError(
            f"OPENCODE_AUTH secret is not valid JSON: {exc}"
        ) from exc
    target = sh.expand_path(OPENCODE_AUTH_FILE)
    sh.atomic_write_text(target, content, mode=0o600)


def _write_mcp_config(servers: dict[str, Any]) -> None:
    """Merge translated MCP servers into ~/.config/opencode/opencode.json."""
    config_path = sh.expand_path(OPENCODE_CONFIG_FILE)
    config_data: dict[str, Any] = {}
    if os.path.isfile(config_path):
        try:
            existing = sh.load_json(config_path)
        except (OSError, json.JSONDecodeError):
            existing = {}
        if isinstance(existing, dict):
            config_data = existing

    mcp_block = config_data.get("mcp")
    if not isinstance(mcp_block, dict):
        mcp_block = {}
    for name, native in servers.items():
        mcp_block[name] = native
    config_data["mcp"] = mcp_block
    sh.atomic_write_json(config_path, config_data)


def _write_model_config(model: str) -> None:
    """Write the resolved model into ~/.config/opencode/opencode.json."""
    config_path = sh.expand_path(OPENCODE_CONFIG_FILE)
    config_data: dict[str, Any] = {}
    if os.path.isfile(config_path):
        try:
            existing = sh.load_json(config_path)
        except (OSError, json.JSONDecodeError):
            existing = {}
        if isinstance(existing, dict):
            config_data = existing

    if model:
        config_data["model"] = model
    else:
        config_data.pop("model", None)
    sh.atomic_write_json(config_path, config_data)


# ---------------------------------------------------------------------------
# Main provision logic
# ---------------------------------------------------------------------------


def provision(ctx: sh.ProvisionContext) -> None:
    explicit = ctx.explicit_type

    if explicit == "vertex-ai":
        resolved = _select_vertex_explicit(ctx)
    elif explicit and explicit not in ("api-key", "auth-file"):
        raise sh.ProvisionError(
            f"opencode: unknown auth type {explicit!r}; "
            f"valid types are: {', '.join(VALID_AUTH_TYPES)}"
        )
    else:
        try:
            resolved = ctx.select_auth(AUTH)
        except sh.ProvisionError:
            if not explicit and _has_vertex(ctx):
                resolved = sh.ResolvedAuth(method="vertex-ai")
            else:
                raise

    extra: dict[str, Any] = {}
    env: dict[str, str] = {}

    if resolved.method == "auth-file":
        _write_opencode_auth_file(ctx)
        extra["auth_file_written"] = True

    if resolved.method == "vertex-ai":
        extra["vertex_project_env"] = "VERTEXAI_PROJECT"
        extra["vertex_location_env"] = "VERTEX_LOCATION"
        env = _vertex_env_overlay(ctx)

    ctx.write_outputs(resolved, env=env, extra=extra)

    sh.apply_mcp_translated(ctx, _translate_mcp_server, _write_mcp_config)

    resolved_model = str(ctx.model_resolution.get("resolved_model") or "").strip()
    if not resolved_model:
        resolved_model = os.environ.get("SCION_MODEL", "").strip()
    _write_model_config(resolved_model)

    ctx.info(f"method={resolved.method}" + (f" model={resolved_model}" if resolved_model else ""))


if __name__ == "__main__":
    sh.run("opencode", provision)
