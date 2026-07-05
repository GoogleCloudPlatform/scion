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
"""Hermes container-side provisioner.

Runs inside the agent container during the pre-start lifecycle hook, invoked
by `sciontool harness provision --manifest ...`. The host-side
ContainerScriptHarness has already:

  * Staged this script and config.yaml under $HOME/.scion/harness/.
  * Written inputs/auth-candidates.json with the env-var names + paths to
    secret-value files under $HOME/.scion/harness/secrets/<NAME>.

This script's job:

  1. Determine which API key is available, with precedence:
         ANTHROPIC_API_KEY > OPENAI_API_KEY > GOOGLE_API_KEY.
  2. Read the secret value from the staged secrets/<NAME> file and write it
     to ~/.hermes/.env (Hermes reads secrets from this dotenv file).
  3. Compose staged Scion prompt inputs into AGENTS.md (instruction
     projection — Hermes auto-reads AGENTS.md as context).
  4. Apply MCP server configuration to ~/.hermes/mcp.json.
  5. Write outputs/resolved-auth.json and outputs/env.json (env overlay
     with HERMES_YOLO_MODE, HERMES_QUIET, HERMES_ACCEPT_HOOKS, and
     optionally HERMES_INFERENCE_MODEL).

The script is stdlib-only — no third-party dependencies.
"""

from __future__ import annotations

import os
import sys
from typing import Any

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import scion_harness  # type: ignore[import-not-found]

assert scion_harness.INTERFACE_VERSION >= 2, (
    f"scion_harness INTERFACE_VERSION {scion_harness.INTERFACE_VERSION} < 2"
)

# ---------------------------------------------------------------------------
# Auth specification (declarative)
# ---------------------------------------------------------------------------

AUTH = scion_harness.AuthSpec(
    "hermes",
    [
        scion_harness.env_method(
            "api-key",
            any_of=["ANTHROPIC_API_KEY", "OPENAI_API_KEY", "GOOGLE_API_KEY"],
            hint="set ANTHROPIC_API_KEY, OPENAI_API_KEY, or GOOGLE_API_KEY",
        ),
    ],
    fallback_to_none_on_error=True,
)

# ---------------------------------------------------------------------------
# Native: ~/.hermes/.env writer
# ---------------------------------------------------------------------------


def _write_hermes_env(env_vars: dict[str, str]) -> None:
    """Write key=value pairs to ~/.hermes/.env."""
    hermes_dir = scion_harness.expand_path("~/.hermes")
    os.makedirs(hermes_dir, exist_ok=True)
    target = os.path.join(hermes_dir, ".env")
    scion_harness.atomic_write_text(
        target,
        "".join(f"{k}={v}\n" for k, v in sorted(env_vars.items())),
        mode=0o600,
    )


# ---------------------------------------------------------------------------
# Native: MCP server translation for ~/.hermes/mcp.json
# ---------------------------------------------------------------------------


def _build_mcp_entry(name: str, spec: dict[str, Any]) -> dict[str, Any] | None:
    """Translate a universal MCPServerConfig into a Hermes mcp.json entry."""
    transport = (spec.get("transport") or "").strip()

    if transport == "stdio":
        cmd = spec.get("command")
        if not isinstance(cmd, str) or not cmd:
            print(f"hermes provision: mcp server {name!r}: stdio transport missing command", file=sys.stderr)
            return None
        entry: dict[str, Any] = {"command": cmd}
        args = spec.get("args") or []
        if isinstance(args, list) and args:
            entry["args"] = [str(a) for a in args]
        env = spec.get("env")
        if isinstance(env, dict) and env:
            entry["env"] = {str(k): str(v) for k, v in env.items()}
        return entry
    elif transport in ("sse", "streamable-http"):
        url = spec.get("url")
        if not isinstance(url, str) or not url:
            print(f"hermes provision: mcp server {name!r}: {transport} transport missing url", file=sys.stderr)
            return None
        entry = {"url": url}
        headers = spec.get("headers")
        if isinstance(headers, dict) and headers:
            entry["headers"] = {str(k): str(v) for k, v in headers.items()}
        return entry
    else:
        print(f"hermes provision: mcp server {name!r}: unsupported transport {transport!r}", file=sys.stderr)
        return None


def _apply_mcp_servers(ctx: scion_harness.ProvisionContext) -> int:
    """Write MCP server config to ~/.hermes/mcp.json.

    Returns the number of servers written.  Removes a stale mcp.json when
    no servers are configured (prevents Hermes from loading old config).
    """
    hermes_dir = scion_harness.expand_path("~/.hermes")
    config_path = os.path.join(hermes_dir, "mcp.json")

    try:
        servers = scion_harness.read_mcp_servers(ctx.bundle_dir)
    except ValueError as exc:
        ctx.info(str(exc))
        return 0

    if not servers:
        if os.path.isfile(config_path):
            try:
                os.remove(config_path)
                ctx.info("removed stale mcp.json (no servers configured)")
            except OSError as exc:
                ctx.warn(f"could not remove stale mcp.json: {exc}")
        return 0

    def write_fn(servers_dict: dict[str, Any]) -> None:
        os.makedirs(hermes_dir, exist_ok=True)
        scion_harness.atomic_write_json(config_path, {"mcpServers": servers_dict})
        os.chmod(config_path, 0o600)

    return scion_harness.apply_mcp_translated(ctx, _build_mcp_entry, write_fn)


# ---------------------------------------------------------------------------
# Provision logic
# ---------------------------------------------------------------------------


def provision(ctx: scion_harness.ProvisionContext) -> None:
    """Hermes provisioning logic."""
    resolved = ctx.select_auth(AUTH)

    # Write the API key to ~/.hermes/.env so Hermes can read it.
    if resolved.method == "api-key":
        api_key = ctx.read_secret(resolved.env_key)
        if not api_key:
            raise scion_harness.ProvisionError(
                f"chose api-key ({resolved.env_key}) but no secret value "
                "was staged at the recorded path; check ApplyAuthSettings"
            )
        _write_hermes_env({resolved.env_key: api_key})

    # Instruction projection via the lib.
    harness_cfg = ctx.harness_config
    instructions_file = str(harness_cfg.get("instructions_file") or "AGENTS.md")
    scion_harness.project_instructions(
        ctx,
        instructions_file,
        skills_dir=str(harness_cfg.get("skills_dir") or ".hermes/skills"),
    )

    # Build env overlay — injected into the container environment by sciontool.
    env_payload: dict[str, str] = {
        "HERMES_HOME": "/home/scion/.hermes",
        "HERMES_YOLO_MODE": "1",
        "HERMES_QUIET": "1",
        "HERMES_ACCEPT_HOOKS": "auto",
    }

    # Model resolution passthrough.
    resolved_model = str(ctx.model_resolution.get("resolved_model") or "").strip()
    if resolved_model:
        env_payload["HERMES_INFERENCE_MODEL"] = resolved_model

    # Write standard outputs.
    ctx.write_outputs(resolved, env=env_payload)

    # MCP server configuration.
    _apply_mcp_servers(ctx)

    ctx.info(f"method={resolved.method}")


if __name__ == "__main__":
    scion_harness.run("hermes", provision)
