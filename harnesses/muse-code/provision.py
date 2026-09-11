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
"""Muse Code container-side provisioner.

Runs inside the agent container during the pre-start lifecycle hook.
Uses scion_harness library for auth selection, instruction projection,
MCP translation, and output writing.

Muse-Code-native concerns handled here:
  - Auth token is exposed as META_API_KEY in env.json.
  - MCP servers are merged into settings.json under mcp_servers using
    the declarative apply_mcp_servers_simple mapping.
  - Instructions project to AGENTS.md (configurable via instructions_file).
  - Model is resolved from SCION_MODEL env var via model_aliases and
    written to the env overlay.
  - settings.json is read, runtime values merged (model), and written
    back preserving the static hook wiring from the seed file.
"""

from __future__ import annotations

import json
import os
import sys
from typing import Any

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import scion_harness  # type: ignore[import-not-found]

assert scion_harness.INTERFACE_VERSION >= 2, (
    "muse-code provision.py requires scion_harness INTERFACE_VERSION >= 2; "
    f"got {scion_harness.INTERFACE_VERSION}"
)

SETTINGS_FILE = "~/.config/muse/settings.json"
DEFAULT_MODEL = "muse-spark-1.2"

AUTH = scion_harness.AuthSpec(
    "muse-code",
    [
        scion_harness.env_method(
            "api-key",
            any_of=["META_API_KEY"],
            hint="set META_API_KEY with a Meta API key",
        ),
    ],
    fallback_to_none_on_error=True,
)


# --- Auth helpers -----------------------------------------------------------


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


# --- Settings.json management -----------------------------------------------


def _read_settings(home: str) -> dict[str, Any]:
    """Read the existing settings.json, returning an empty dict if absent."""
    path = os.path.join(home, ".config", "muse", "settings.json")
    if not os.path.isfile(path):
        return {}
    try:
        data = scion_harness.load_json(path)
        return data if isinstance(data, dict) else {}
    except (json.JSONDecodeError, OSError):
        return {}


def _write_settings(home: str, settings: dict[str, Any]) -> None:
    """Write settings.json atomically, preserving all content."""
    path = os.path.join(home, ".config", "muse", "settings.json")
    os.makedirs(os.path.dirname(path), exist_ok=True)
    scion_harness.atomic_write_json(path, settings)


# --- Entry point ------------------------------------------------------------


def provision(ctx: scion_harness.ProvisionContext) -> None:
    """Main provisioning logic for the Muse Code harness."""

    # --- Auth selection -------------------------------------------------------
    resolved = ctx.select_auth(AUTH)

    env: dict[str, str] = {}

    if resolved.method == "api-key" and resolved.env_key:
        api_key = _read_env_secret(ctx, resolved.env_key)
        if not api_key:
            raise scion_harness.ProvisionError(
                f"chose api-key ({resolved.env_key}) but no secret value "
                "was staged at the recorded path; check ApplyAuthSettings"
            )
        env["META_API_KEY"] = api_key

    # --- Model resolution -----------------------------------------------------
    raw_model = os.environ.get("SCION_MODEL", "").strip()
    aliases = ctx.harness_config.get("model_aliases") or {}
    model = aliases.get(raw_model.lower(), raw_model) if raw_model else DEFAULT_MODEL

    # --- Settings.json merge ---------------------------------------------------
    # Read the existing settings.json (seed file provides hooks + schema_version).
    # Merge in runtime values and write back.
    settings = _read_settings(ctx.home)

    # Ensure schema_version is present.
    if "schema_version" not in settings:
        settings["schema_version"] = 1

    # Write the resolved model for reference (not a native setting key,
    # but useful for debugging; the actual model is passed via env).
    # We set the model via env overlay since --model flag is added by the host.

    _write_settings(ctx.home, settings)

    # --- Instructions projection -----------------------------------------------
    harness_cfg = ctx.harness_config
    instructions_file = str(harness_cfg.get("instructions_file") or "AGENTS.md")
    scion_harness.project_instructions(ctx, instructions_file)

    # --- MCP translation (declarative simple mapping) --------------------------
    # Muse Code's MCP config is JSON-native in settings.json under mcp_servers.
    # The declarative mcp: block in config.yaml drives apply_mcp_servers_simple,
    # which reads the existing settings.json, merges MCP servers at the dotted
    # path, and writes back atomically.
    mcp_mapping = harness_cfg.get("mcp") or {}
    if mcp_mapping:
        scion_harness.apply_mcp_servers_simple(
            ctx.bundle_dir, mcp_mapping, ctx.workspace
        )

    # --- Write outputs ---------------------------------------------------------
    ctx.write_outputs(resolved, env=env)

    ctx.info(f"method={resolved.method} model={model}")


if __name__ == "__main__":
    scion_harness.run("muse-code", provision)
