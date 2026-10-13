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
  - Model is passed via the host-side --model CLI flag.
  - settings.json is read, schema_version ensured, and written back
    preserving the static hook wiring from the seed file.
  - Agent Plugins staged in inputs/plugins.json are installed natively with
    the muse plugins CLI (MuseInstaller), as the last step.
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

# All hook events that the muse-code harness requires wired to sciontool.
_HOOK_EVENTS = (
    "SessionStart",
    "SessionEnd",
    "UserPromptSubmit",
    "PreToolUse",
    "PostToolUse",
    "PreLLMCall",
    "PostLLMCall",
    "PermissionRequest",
    "PreCompact",
    "PostCompact",
    "SubagentStart",
    "SubagentStop",
    "Stop",
)

_DEFAULT_HOOK_ENTRY: list[dict[str, Any]] = [
    {
        "matcher": "*",
        "hooks": [
            {
                "name": "scion-hook",
                "type": "command",
                "command": "sciontool hook --dialect=muse-code",
            }
        ],
    }
]

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


# --- Agent Plugins (hub.agent_plugins) --------------------------------------
#
# Route: `muse plugins install <abs ~/.scion/agent-plugins/bundles/<n>>
# --scope user --json` copies the bundle into muse's digest-pinned cache and
# records it in $XDG_DATA_HOME/muse/plugins/installed.json. A plain copy is
# never registered, so the CLI is the only writer.
#
# CLI surface observed on muse 1.4.6-R6000.1 and 1.4.7-R6101.1 (smoke run,
# muse-code-smoke.md), which differs from the design's draft:
#   * removal is `muse plugins remove <id> --json` (there is no `uninstall`);
#     it is not idempotent: an unknown id exits 1 with error code
#     "unknown-plugin".
#   * `approve <id> [--json]` and `enable <id> [--json]` take no --scope.
#   * install enables the plugin by default (installed.enabled = true,
#     trust "user-local"); approve/enable only run when the result asks.
#   * state lives under $XDG_DATA_HOME/muse (default ~/.local/share/muse),
#     not under ~/.config, so a re-provision of the harness home/ tree does
#     not reset it; is_present still reads installed.json, never just a dir.
#   * Agent Plugins MCP servers are reported "not supported in this release;
#     it stays visible and inactive": skills load, MCP does not (≤ 1.4.7).

MUSE_PLUGINS_MIN_VERSION = (1, 4, 6)  # earliest version observed with support
_APPROVAL_KEYS = ("needs_approval", "approval_required", "requires_approval", "pending_approval")
_UNTRUSTED = ("untrusted", "pending", "unapproved")


def _xdg_dir(var: str, home: str, default_rel: str) -> str:
    """$<var> when it is an absolute path (XDG spec: relative values are
    ignored), else <home>/<default_rel>."""
    value = os.environ.get(var) or ""
    if os.path.isabs(value):
        return value
    return os.path.join(home, default_rel)


class MuseInstaller(scion_harness.PluginInstaller):
    harness = "muse-code"
    cli = "muse"
    min_version = MUSE_PLUGINS_MIN_VERSION

    def __init__(self, ctx: scion_harness.ProvisionContext):
        self.ctx = ctx
        # sciontool runs the provisioner with a minimal env (HOME, PATH,
        # SCION_*), so XDG_* are normally unset here and muse falls back to
        # its defaults, exactly as at runtime unless a template overrides
        # XDG_*. Pass both explicitly so the CLI and is_present always agree
        # (risk R4).
        self.data_home = _xdg_dir("XDG_DATA_HOME", ctx.home, os.path.join(".local", "share"))
        self.config_home = _xdg_dir("XDG_CONFIG_HOME", ctx.home, ".config")
        self.muse_data = os.path.join(self.data_home, "muse")

    def _env(self) -> dict[str, str]:
        return {
            **os.environ,
            "XDG_DATA_HOME": self.data_home,
            "XDG_CONFIG_HOME": self.config_home,
            # The image sets this, but sciontool's minimal env drops it; a
            # CLI self-update must never run inside provisioning.
            "MUSE_NO_AUTO_UPDATE": "1",
        }

    def _run(self, *args: str, timeout: float = scion_harness.AGENT_PLUGIN_CLI_TIMEOUT):
        return scion_harness.run_cli(["muse", *args], timeout=self.cli_timeout(timeout), env=self._env())

    def detect_version(self) -> tuple[int, int, int] | None:
        # "Muse Code 1.4.7 (1.4.7-R6101.1)"
        proc = self._run("--version", timeout=scion_harness.AGENT_PLUGIN_VERSION_TIMEOUT)
        if proc.returncode != 0:
            return None
        return scion_harness.parse_semver(proc.stdout or proc.stderr or "")

    def _registry(self) -> dict[str, Any]:
        """The plugins object of installed.json, or {} when absent/unreadable."""
        path = os.path.join(self.muse_data, "plugins", "installed.json")
        try:
            doc = scion_harness.load_json(path)
        except (OSError, ValueError):
            return {}
        plugins = doc.get("plugins") if isinstance(doc, dict) else None
        return plugins if isinstance(plugins, dict) else {}

    def _record(self, name: str) -> dict[str, Any] | None:
        rec = self._registry().get(name)
        return rec if isinstance(rec, dict) else None

    def _cache_dir(self, rec: dict[str, Any]) -> str:
        """installed.json stores cache_path relative to $XDG_DATA_HOME/muse."""
        path = rec.get("cache_path")
        if not isinstance(path, str) or not path:
            return ""
        return path if os.path.isabs(path) else os.path.join(self.muse_data, path)

    @staticmethod
    def _parse(proc) -> dict[str, Any]:
        try:
            doc = json.loads(proc.stdout or "null")
        except json.JSONDecodeError:
            return {}
        return doc if isinstance(doc, dict) else {}

    @staticmethod
    def _needs_approval(doc: dict[str, Any], installed: dict[str, Any]) -> bool:
        for src in (doc, installed):
            if any(src.get(k) for k in _APPROVAL_KEYS):
                return True
        if str(installed.get("trust") or "").lower() in _UNTRUSTED:
            return True
        warning = doc.get("warning")
        return isinstance(warning, str) and "approve" in warning.lower()

    def install(self, plugin: scion_harness.AgentPlugin, bundle_dir: str) -> dict[str, Any]:
        proc = self._run("plugins", "install", os.path.abspath(bundle_dir), "--scope", "user", "--json")
        if proc.returncode != 0:
            raise scion_harness.cli_failure("muse plugins install", proc)
        doc = self._parse(proc)
        installed = doc.get("installed") if isinstance(doc.get("installed"), dict) else {}
        for d in doc.get("diagnostics") or []:
            if isinstance(d, dict) and d.get("severity") == "warning" and d.get("message"):
                self.ctx.warn(f"agent plugin {plugin.name}: muse: {d['message']}")
        if self._needs_approval(doc, installed):
            proc = self._run("plugins", "approve", plugin.name, "--json")
            if proc.returncode != 0:
                raise scion_harness.cli_failure("muse plugins approve", proc)
        if installed.get("enabled") is False:
            proc = self._run("plugins", "enable", plugin.name, "--json")
            if proc.returncode != 0:
                raise scion_harness.cli_failure("muse plugins enable", proc)
        # Judge by muse's own registry, not by the CLI output.
        rec = self._record(plugin.name)
        if rec is None:
            raise scion_harness.PluginInstallError(
                f"muse plugins install exit 0 but {plugin.name} is not in installed.json")
        if rec.get("enabled") is not True:
            raise scion_harness.PluginInstallError(
                f"muse plugins install exit 0 but {plugin.name} is not enabled in installed.json")
        return {
            "id": plugin.name,
            "cache_path": self._cache_dir(rec),
            "package_sha256": str(rec.get("package_sha256") or ""),
        }

    def uninstall(self, name: str, native_ref: dict[str, Any]) -> None:
        """muse plugins remove. Success is judged by installed.json: the
        plugin must no longer be registered. An already-removed plugin
        (exit 1, "unknown-plugin") therefore counts as removed."""
        plugin_id = str(native_ref.get("id") or name)
        proc = self._run("plugins", "remove", plugin_id, "--json")
        if self._record(plugin_id) is not None:
            if proc.returncode != 0:
                raise scion_harness.cli_failure("muse plugins remove", proc)
            raise scion_harness.PluginInstallError(
                f"muse plugins remove exit 0 but {plugin_id} is still in installed.json")
        if proc.returncode != 0:
            self.ctx.info(f"agent plugin {name}: muse plugins remove exit {proc.returncode}, plugin no longer registered")

    def is_present(self, name: str, native_ref: dict[str, Any]) -> bool:
        """muse still has the plugin registered and enabled in installed.json
        and its cache dir exists. Checking the cache dir alone would miss a
        reset or edited registry (a disable, a removal, or a home/ re-provision
        that covers $XDG_DATA_HOME). Reads files only; no CLI call."""
        rec = self._record(str(native_ref.get("id") or name))
        if rec is None or rec.get("enabled") is not True:
            return False
        cache = self._cache_dir(rec)
        return bool(cache) and os.path.isdir(cache)


# --- Entry point ------------------------------------------------------------


def provision(ctx: scion_harness.ProvisionContext) -> None:
    """Main provisioning logic for the Muse Code harness."""

    # --- Auth selection -------------------------------------------------------
    resolved = ctx.select_auth(AUTH)

    env: dict[str, str] = {}

    if resolved.method == "api-key" and resolved.env_key:
        api_key = ctx.read_secret(resolved.env_key)
        if not api_key:
            raise scion_harness.ProvisionError(
                f"chose api-key ({resolved.env_key}) but no secret value "
                "was staged at the recorded path; check ApplyAuthSettings"
            )
        env["META_API_KEY"] = api_key

    # --- Model resolution -----------------------------------------------------
    # Model is passed via the host-side --model CLI flag; no env overlay needed.
    model = scion_harness.resolve_model(ctx)

    # --- Settings.json merge ---------------------------------------------------
    # Read the existing settings.json (seed file provides hooks + schema_version).
    # Ensure schema_version is present and write back.
    settings = _read_settings(ctx.home)

    if "schema_version" not in settings:
        settings["schema_version"] = 1

    # Ensure all hook events are wired, even on cold start when the seed
    # file has not been copied yet (or the copy failed).
    hooks = settings.setdefault("hooks", {})
    for event in _HOOK_EVENTS:
        if event not in hooks:
            hooks[event] = _DEFAULT_HOOK_ENTRY

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

    # --- Agent Plugins (last: the muse CLI is the final writer) ---------------
    scion_harness.reconcile_agent_plugins(ctx, MuseInstaller(ctx))


if __name__ == "__main__":
    scion_harness.run("muse-code", provision)
