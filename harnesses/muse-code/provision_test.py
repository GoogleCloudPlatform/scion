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

from __future__ import annotations

import json
import os
import importlib.util
import tempfile
import unittest
from contextlib import contextmanager

PROVISION_PATH = os.path.join(os.path.dirname(__file__), "provision.py")
SPEC = importlib.util.spec_from_file_location("muse_code_provision", PROVISION_PATH)
assert SPEC is not None
provision = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
SPEC.loader.exec_module(provision)

scion_harness = provision.scion_harness

MANAGED_BEGIN = "<!-- BEGIN SCION MANAGED -->"
MANAGED_END = "<!-- END SCION MANAGED -->"

# Seed settings.json content matching the home/ seed file.
SEED_SETTINGS = {
    "schema_version": 1,
    "hooks": {
        "SessionStart": [{"matcher": "*", "hooks": [{"name": "scion-hook", "type": "command", "command": "sciontool hook --dialect=muse-code"}]}],
        "SessionEnd": [{"matcher": "*", "hooks": [{"name": "scion-hook", "type": "command", "command": "sciontool hook --dialect=muse-code"}]}],
        "UserPromptSubmit": [{"matcher": "*", "hooks": [{"name": "scion-hook", "type": "command", "command": "sciontool hook --dialect=muse-code"}]}],
        "PreToolUse": [{"matcher": "*", "hooks": [{"name": "scion-hook", "type": "command", "command": "sciontool hook --dialect=muse-code"}]}],
        "PostToolUse": [{"matcher": "*", "hooks": [{"name": "scion-hook", "type": "command", "command": "sciontool hook --dialect=muse-code"}]}],
        "PreLLMCall": [{"matcher": "*", "hooks": [{"name": "scion-hook", "type": "command", "command": "sciontool hook --dialect=muse-code"}]}],
        "PostLLMCall": [{"matcher": "*", "hooks": [{"name": "scion-hook", "type": "command", "command": "sciontool hook --dialect=muse-code"}]}],
        "PermissionRequest": [{"matcher": "*", "hooks": [{"name": "scion-hook", "type": "command", "command": "sciontool hook --dialect=muse-code"}]}],
        "PreCompact": [{"matcher": "*", "hooks": [{"name": "scion-hook", "type": "command", "command": "sciontool hook --dialect=muse-code"}]}],
        "PostCompact": [{"matcher": "*", "hooks": [{"name": "scion-hook", "type": "command", "command": "sciontool hook --dialect=muse-code"}]}],
        "SubagentStart": [{"matcher": "*", "hooks": [{"name": "scion-hook", "type": "command", "command": "sciontool hook --dialect=muse-code"}]}],
        "SubagentStop": [{"matcher": "*", "hooks": [{"name": "scion-hook", "type": "command", "command": "sciontool hook --dialect=muse-code"}]}],
        "Stop": [{"matcher": "*", "hooks": [{"name": "scion-hook", "type": "command", "command": "sciontool hook --dialect=muse-code"}]}],
    },
}


@contextmanager
def temporary_home(path: str):
    old_home = os.environ.get("HOME")
    os.environ["HOME"] = path
    try:
        yield
    finally:
        if old_home is None:
            os.environ.pop("HOME", None)
        else:
            os.environ["HOME"] = old_home


@contextmanager
def temporary_env(key: str, value: str | None):
    """Temporarily set or unset an environment variable."""
    old = os.environ.get(key)
    if value is not None:
        os.environ[key] = value
    else:
        os.environ.pop(key, None)
    try:
        yield
    finally:
        if old is None:
            os.environ.pop(key, None)
        else:
            os.environ[key] = old


def _make_bundle(tmp: str, home: str, *, candidates: dict | None = None,
                 mcp_servers: dict | None = None,
                 instructions: str = "", system_prompt: str = "",
                 harness_config: dict | None = None) -> dict:
    """Create a fake harness bundle and return the manifest dict."""
    bundle = os.path.join(tmp, "bundle")
    workspace = os.path.join(tmp, "workspace")
    os.makedirs(workspace, exist_ok=True)
    inputs_dir = os.path.join(bundle, "inputs")
    outputs_dir = os.path.join(bundle, "outputs")
    os.makedirs(inputs_dir, exist_ok=True)
    os.makedirs(outputs_dir, exist_ok=True)

    if candidates is not None:
        with open(os.path.join(inputs_dir, "auth-candidates.json"), "w") as f:
            json.dump(candidates, f)

    if mcp_servers is not None:
        with open(os.path.join(inputs_dir, "mcp-servers.json"), "w") as f:
            json.dump({"mcp_servers": mcp_servers}, f)

    if instructions:
        with open(os.path.join(inputs_dir, "instructions.md"), "w") as f:
            f.write(instructions)

    if system_prompt:
        with open(os.path.join(inputs_dir, "system-prompt.md"), "w") as f:
            f.write(system_prompt)

    config = harness_config or {
        "instructions_file": "AGENTS.md",
        "skills_dir": ".muse/skills",
        "system_prompt_mode": "prepend_to_instructions",
        "model_aliases": {
            "small": "muse-spark-1.2",
            "medium": "muse-spark-1.2",
            "large": "muse-spark-1.2",
            "extra-large": "muse-spark-1.2",
        },
        "mcp": {
            "global_config_file": ".config/muse/settings.json",
            "global_config_path": "mcp_servers",
            "transport_field": "transport",
            "transport_map": {
                "stdio": "stdio",
                "sse": "streamable_http",
                "streamable-http": "streamable_http",
            },
        },
        "no_auth": {
            "behavior": "drop-to-shell",
        },
    }

    manifest = {
        "harness_bundle_dir": bundle,
        "agent_home": home,
        # Keep the workspace inside the test's tempdir so provisioning can
        # never write into a live checkout mounted at /workspace.
        "agent_workspace": workspace,
        "harness_config": config,
    }

    return manifest


def _write_seed_settings(home: str) -> None:
    """Write the seed settings.json into the home dir."""
    settings_dir = os.path.join(home, ".config", "muse")
    os.makedirs(settings_dir, exist_ok=True)
    with open(os.path.join(settings_dir, "settings.json"), "w") as f:
        json.dump(SEED_SETTINGS, f, indent=2)


def _read_json(path: str) -> dict:
    with open(path, "r") as f:
        return json.load(f)


class MuseCodeProvisionTest(unittest.TestCase):
    """Tests for the Muse Code provisioner."""

    def test_auth_api_key_present(self) -> None:
        """When META_API_KEY is staged, provision succeeds and writes env."""
        with tempfile.TemporaryDirectory() as tmp:
            home = os.path.join(tmp, "home")
            os.makedirs(home)
            _write_seed_settings(home)

            # Stage a secret file for META_API_KEY.
            secrets_dir = os.path.join(tmp, "bundle", "secrets")
            os.makedirs(secrets_dir, exist_ok=True)
            secret_path = os.path.join(secrets_dir, "META_API_KEY")
            with open(secret_path, "w") as f:
                f.write("test-meta-api-key-123\n")

            candidates = {
                "env_vars": ["META_API_KEY"],
                "env_secret_files": {"META_API_KEY": secret_path},
            }
            manifest = _make_bundle(tmp, home, candidates=candidates)

            with temporary_home(home), temporary_env("SCION_MODEL", ""):
                ctx = scion_harness.ProvisionContext("muse-code", manifest)
                provision.provision(ctx)

            # Check env.json was written with the API key.
            env_json = _read_json(
                os.path.join(tmp, "bundle", "outputs", "env.json")
            )
            self.assertEqual(env_json.get("META_API_KEY"), "test-meta-api-key-123")

            # Check resolved-auth.json.
            auth_json = _read_json(
                os.path.join(tmp, "bundle", "outputs", "resolved-auth.json")
            )
            self.assertEqual(auth_json["method"], "api-key")
            self.assertEqual(auth_json["env_var"], "META_API_KEY")

    def test_auth_api_key_absent_no_auth_fallback(self) -> None:
        """When no credentials are staged and no_auth is configured, falls back to none."""
        with tempfile.TemporaryDirectory() as tmp:
            home = os.path.join(tmp, "home")
            os.makedirs(home)
            _write_seed_settings(home)

            # No candidates staged at all.
            manifest = _make_bundle(tmp, home)

            with temporary_home(home), temporary_env("SCION_MODEL", ""):
                ctx = scion_harness.ProvisionContext("muse-code", manifest)
                provision.provision(ctx)

            auth_json = _read_json(
                os.path.join(tmp, "bundle", "outputs", "resolved-auth.json")
            )
            self.assertEqual(auth_json["method"], "none")

    def test_model_alias_resolution(self) -> None:
        """SCION_MODEL tier names resolve through model_aliases."""
        with tempfile.TemporaryDirectory() as tmp:
            home = os.path.join(tmp, "home")
            os.makedirs(home)
            _write_seed_settings(home)

            manifest = _make_bundle(tmp, home)

            messages: list[str] = []
            with temporary_home(home), temporary_env("SCION_MODEL", "small"):
                ctx = scion_harness.ProvisionContext("muse-code", manifest)
                ctx.info = messages.append  # type: ignore[method-assign]
                provision.provision(ctx)

            # Model should be muse-spark-1.2 (all aliases map to it).
            auth_json = _read_json(
                os.path.join(tmp, "bundle", "outputs", "resolved-auth.json")
            )
            self.assertEqual(auth_json["method"], "none")
            self.assertTrue(any("model=muse-spark-1.2" in m for m in messages), messages)

    def test_model_passthrough_for_unknown(self) -> None:
        """Unknown model names are passed through as-is."""
        with tempfile.TemporaryDirectory() as tmp:
            home = os.path.join(tmp, "home")
            os.makedirs(home)
            _write_seed_settings(home)

            manifest = _make_bundle(tmp, home)

            messages: list[str] = []
            with temporary_home(home), temporary_env("SCION_MODEL", "custom-model-v2"):
                ctx = scion_harness.ProvisionContext("muse-code", manifest)
                ctx.info = messages.append  # type: ignore[method-assign]
                provision.provision(ctx)

            # Should succeed without error.
            auth_json = _read_json(
                os.path.join(tmp, "bundle", "outputs", "resolved-auth.json")
            )
            self.assertIsNotNone(auth_json)
            self.assertTrue(any("model=custom-model-v2" in m for m in messages), messages)

    def test_model_shorthand_now_resolves_through_aliases(self) -> None:
        """Behavior difference from the pre-G3 lowercase-only lookup: that
        code did `aliases.get(raw.lower(), raw)` with no shorthand table, so
        a bare "s" never matched a model_aliases key and was logged as the
        literal (invalid) model name "s". The shared resolve_model helper
        expands shorthand the same way Go's NormalizeModelAlias does, so
        this now resolves to the tier's concrete model.
        """
        with tempfile.TemporaryDirectory() as tmp:
            home = os.path.join(tmp, "home")
            os.makedirs(home)
            _write_seed_settings(home)

            manifest = _make_bundle(tmp, home)

            messages: list[str] = []
            with temporary_home(home), temporary_env("SCION_MODEL", "s"):
                ctx = scion_harness.ProvisionContext("muse-code", manifest)
                ctx.info = messages.append  # type: ignore[method-assign]
                provision.provision(ctx)

            self.assertTrue(any("model=muse-spark-1.2" in m for m in messages), messages)

    def test_instruction_projection(self) -> None:
        """Instructions and system prompt are projected into AGENTS.md."""
        with tempfile.TemporaryDirectory() as tmp:
            home = os.path.join(tmp, "home")
            os.makedirs(home)
            _write_seed_settings(home)

            manifest = _make_bundle(
                tmp, home,
                instructions="Follow these rules.",
                system_prompt="You are a helpful coding assistant.",
            )

            with temporary_home(home), temporary_env("SCION_MODEL", ""):
                ctx = scion_harness.ProvisionContext("muse-code", manifest)
                provision.provision(ctx)

            agents_path = os.path.join(home, "AGENTS.md")
            self.assertTrue(os.path.isfile(agents_path))
            with open(agents_path, "r") as f:
                content = f.read()

            self.assertIn(MANAGED_BEGIN, content)
            self.assertIn(MANAGED_END, content)
            self.assertIn("# System Instruction", content)
            self.assertIn("You are a helpful coding assistant.", content)
            self.assertIn("# Agent Instructions", content)
            self.assertIn("Follow these rules.", content)

    def test_mcp_stdio_translation(self) -> None:
        """Stdio MCP servers are merged into settings.json under mcp_servers."""
        with tempfile.TemporaryDirectory() as tmp:
            home = os.path.join(tmp, "home")
            os.makedirs(home)
            _write_seed_settings(home)

            mcp_servers = {
                "my-tool": {
                    "transport": "stdio",
                    "command": "my-mcp-server",
                    "args": ["--verbose"],
                    "env": {"MY_VAR": "value"},
                },
            }
            manifest = _make_bundle(tmp, home, mcp_servers=mcp_servers)

            with temporary_home(home), temporary_env("SCION_MODEL", ""):
                ctx = scion_harness.ProvisionContext("muse-code", manifest)
                provision.provision(ctx)

            settings_path = os.path.join(home, ".config", "muse", "settings.json")
            settings = _read_json(settings_path)

            # MCP servers should be present.
            self.assertIn("mcp_servers", settings)
            self.assertIn("my-tool", settings["mcp_servers"])
            server = settings["mcp_servers"]["my-tool"]
            self.assertEqual(server["transport"], "stdio")
            self.assertEqual(server["command"], "my-mcp-server")
            self.assertEqual(server["args"], ["--verbose"])
            self.assertEqual(server["env"], {"MY_VAR": "value"})

    def test_mcp_streamable_http_translation(self) -> None:
        """Streamable HTTP MCP servers are translated correctly."""
        with tempfile.TemporaryDirectory() as tmp:
            home = os.path.join(tmp, "home")
            os.makedirs(home)
            _write_seed_settings(home)

            mcp_servers = {
                "remote-api": {
                    "transport": "streamable-http",
                    "url": "https://api.example.com/mcp",
                    "headers": {"Authorization": "Bearer tok"},
                },
            }
            manifest = _make_bundle(tmp, home, mcp_servers=mcp_servers)

            with temporary_home(home), temporary_env("SCION_MODEL", ""):
                ctx = scion_harness.ProvisionContext("muse-code", manifest)
                provision.provision(ctx)

            settings_path = os.path.join(home, ".config", "muse", "settings.json")
            settings = _read_json(settings_path)

            self.assertIn("mcp_servers", settings)
            self.assertIn("remote-api", settings["mcp_servers"])
            server = settings["mcp_servers"]["remote-api"]
            # streamable-http maps to streamable_http in Muse Code's format.
            self.assertEqual(server["transport"], "streamable_http")
            self.assertEqual(server["url"], "https://api.example.com/mcp")
            self.assertEqual(server["headers"], {"Authorization": "Bearer tok"})

    def test_settings_json_preserves_hooks_after_provision(self) -> None:
        """Hook wiring from seed file is preserved after provisioning."""
        with tempfile.TemporaryDirectory() as tmp:
            home = os.path.join(tmp, "home")
            os.makedirs(home)
            _write_seed_settings(home)

            manifest = _make_bundle(tmp, home)

            with temporary_home(home), temporary_env("SCION_MODEL", ""):
                ctx = scion_harness.ProvisionContext("muse-code", manifest)
                provision.provision(ctx)

            settings_path = os.path.join(home, ".config", "muse", "settings.json")
            settings = _read_json(settings_path)

            # schema_version must be preserved.
            self.assertEqual(settings.get("schema_version"), 1)

            # All 13 hook events must be present.
            hooks = settings.get("hooks", {})
            expected_events = [
                "SessionStart", "SessionEnd", "UserPromptSubmit",
                "PreToolUse", "PostToolUse", "PreLLMCall", "PostLLMCall",
                "PermissionRequest", "PreCompact", "PostCompact",
                "SubagentStart", "SubagentStop", "Stop",
            ]
            for event in expected_events:
                self.assertIn(event, hooks, f"Hook event {event} missing after provision")

            # Each hook should reference sciontool hook --dialect=muse-code.
            for event in expected_events:
                hook_list = hooks[event]
                self.assertTrue(len(hook_list) > 0)
                cmd = hook_list[0]["hooks"][0]["command"]
                self.assertIn("sciontool hook --dialect=muse-code", cmd)

    def test_settings_json_preserves_hooks_after_mcp_merge(self) -> None:
        """Hook wiring is preserved even when MCP servers are added."""
        with tempfile.TemporaryDirectory() as tmp:
            home = os.path.join(tmp, "home")
            os.makedirs(home)
            _write_seed_settings(home)

            mcp_servers = {
                "test-server": {
                    "transport": "stdio",
                    "command": "test-cmd",
                },
            }
            manifest = _make_bundle(tmp, home, mcp_servers=mcp_servers)

            with temporary_home(home), temporary_env("SCION_MODEL", ""):
                ctx = scion_harness.ProvisionContext("muse-code", manifest)
                provision.provision(ctx)

            settings_path = os.path.join(home, ".config", "muse", "settings.json")
            settings = _read_json(settings_path)

            # Both hooks and MCP servers must coexist.
            self.assertIn("hooks", settings)
            self.assertIn("mcp_servers", settings)
            self.assertIn("test-server", settings["mcp_servers"])
            self.assertEqual(settings["schema_version"], 1)
            self.assertIn("SessionStart", settings["hooks"])
            self.assertIn("Stop", settings["hooks"])

    def test_env_output_contains_api_key(self) -> None:
        """env.json includes META_API_KEY when api-key auth is selected."""
        with tempfile.TemporaryDirectory() as tmp:
            home = os.path.join(tmp, "home")
            os.makedirs(home)
            _write_seed_settings(home)

            secrets_dir = os.path.join(tmp, "bundle", "secrets")
            os.makedirs(secrets_dir, exist_ok=True)
            secret_path = os.path.join(secrets_dir, "META_API_KEY")
            with open(secret_path, "w") as f:
                f.write("my-secret-key")

            candidates = {
                "env_vars": ["META_API_KEY"],
                "env_secret_files": {"META_API_KEY": secret_path},
            }
            manifest = _make_bundle(tmp, home, candidates=candidates)

            with temporary_home(home), temporary_env("SCION_MODEL", ""):
                ctx = scion_harness.ProvisionContext("muse-code", manifest)
                provision.provision(ctx)

            env_json = _read_json(
                os.path.join(tmp, "bundle", "outputs", "env.json")
            )
            self.assertEqual(env_json["META_API_KEY"], "my-secret-key")

    def test_env_output_empty_for_no_auth(self) -> None:
        """env.json is empty when no auth is resolved."""
        with tempfile.TemporaryDirectory() as tmp:
            home = os.path.join(tmp, "home")
            os.makedirs(home)
            _write_seed_settings(home)

            manifest = _make_bundle(tmp, home)

            with temporary_home(home), temporary_env("SCION_MODEL", ""):
                ctx = scion_harness.ProvisionContext("muse-code", manifest)
                provision.provision(ctx)

            env_json = _read_json(
                os.path.join(tmp, "bundle", "outputs", "env.json")
            )
            self.assertEqual(env_json, {})

    def test_mcp_sse_mapped_to_streamable_http(self) -> None:
        """SSE transport servers are mapped to streamable_http."""
        with tempfile.TemporaryDirectory() as tmp:
            home = os.path.join(tmp, "home")
            os.makedirs(home)
            _write_seed_settings(home)

            mcp_servers = {
                "sse-server": {
                    "transport": "sse",
                    "url": "https://sse.example.com/events",
                    "headers": {"Authorization": "Bearer tok"},
                },
            }
            manifest = _make_bundle(tmp, home, mcp_servers=mcp_servers)

            with temporary_home(home), temporary_env("SCION_MODEL", ""):
                ctx = scion_harness.ProvisionContext("muse-code", manifest)
                provision.provision(ctx)

            settings_path = os.path.join(home, ".config", "muse", "settings.json")
            settings = _read_json(settings_path)

            self.assertIn("mcp_servers", settings)
            self.assertIn("sse-server", settings["mcp_servers"])
            server = settings["mcp_servers"]["sse-server"]
            # SSE must be mapped to streamable_http per transport_map.
            self.assertEqual(server["transport"], "streamable_http")
            self.assertEqual(server["url"], "https://sse.example.com/events")

    def test_provision_cold_start_no_settings(self) -> None:
        """Provision succeeds when no settings.json exists (cold start).

        All 13 hook events must be populated even without a seed file.
        """
        with tempfile.TemporaryDirectory() as tmp:
            home = os.path.join(tmp, "home")
            os.makedirs(home)
            # Deliberately do NOT call _write_seed_settings(home).

            manifest = _make_bundle(tmp, home)

            with temporary_home(home), temporary_env("SCION_MODEL", ""):
                ctx = scion_harness.ProvisionContext("muse-code", manifest)
                provision.provision(ctx)

            # settings.json should be created with schema_version.
            settings_path = os.path.join(home, ".config", "muse", "settings.json")
            self.assertTrue(os.path.isfile(settings_path))
            settings = _read_json(settings_path)
            self.assertEqual(settings.get("schema_version"), 1)

            # All 13 hook events must be populated on cold start.
            hooks = settings.get("hooks", {})
            expected_events = [
                "SessionStart", "SessionEnd", "UserPromptSubmit",
                "PreToolUse", "PostToolUse", "PreLLMCall", "PostLLMCall",
                "PermissionRequest", "PreCompact", "PostCompact",
                "SubagentStart", "SubagentStop", "Stop",
            ]
            self.assertEqual(len(expected_events), 13)
            for event in expected_events:
                self.assertIn(
                    event, hooks,
                    f"Hook event {event} missing on cold start (no seed file)",
                )
                hook_list = hooks[event]
                self.assertTrue(len(hook_list) > 0)
                cmd = hook_list[0]["hooks"][0]["command"]
                self.assertIn("sciontool hook --dialect=muse-code", cmd)

            # Auth should fall back to none.
            auth_json = _read_json(
                os.path.join(tmp, "bundle", "outputs", "resolved-auth.json")
            )
            self.assertEqual(auth_json["method"], "none")

    def test_provision_idempotent(self) -> None:
        """Running provision twice produces the same result."""
        with tempfile.TemporaryDirectory() as tmp:
            home = os.path.join(tmp, "home")
            os.makedirs(home)
            _write_seed_settings(home)

            manifest = _make_bundle(
                tmp, home,
                instructions="Test instructions",
            )

            with temporary_home(home), temporary_env("SCION_MODEL", "medium"):
                ctx = scion_harness.ProvisionContext("muse-code", manifest)
                provision.provision(ctx)

                # Run again.
                ctx2 = scion_harness.ProvisionContext("muse-code", manifest)
                provision.provision(ctx2)

            agents_path = os.path.join(home, "AGENTS.md")
            with open(agents_path, "r") as f:
                content = f.read()

            # Managed block should appear exactly once.
            self.assertEqual(content.count(MANAGED_BEGIN), 1)



# --- Agent Plugins ----------------------------------------------------------


class _FakeMuseCLI:
    """Stands in for scion_harness.run_cli and simulates the muse plugins CLI
    against $XDG_DATA_HOME/muse, pinned to the surface and output shapes of
    the real muse 1.4.6-R6000.1 / 1.4.7-R6101.1 (smoke run,
    muse-code-smoke.md): install --scope user --json, approve/enable/remove
    <id> --json with no --scope, remove of an unknown id exits 1 with
    "unknown-plugin", and `uninstall` does not exist (exit 2)."""

    def __init__(self, version: str = "Muse Code 1.4.7 (1.4.7-R6101.1)"):
        self.version = version
        self.calls: list[list[str]] = []
        self.envs: list[dict[str, str]] = []
        self.timeouts: list[float | None] = []
        self.install_rc = 0
        self.install_enabled = True
        self.install_trust = "user-local"
        self.install_registers = True
        self.remove_keeps_record = False
        self.remove_rc: int | None = None

    def _lockfile(self, env) -> str:
        return os.path.join(env["XDG_DATA_HOME"], "muse", "plugins", "installed.json")

    def _load(self, env) -> dict:
        try:
            with open(self._lockfile(env)) as f:
                return json.load(f)
        except OSError:
            return {"version": 1, "plugins": {}}

    def _save(self, env, doc: dict) -> None:
        path = self._lockfile(env)
        os.makedirs(os.path.dirname(path), exist_ok=True)
        with open(path, "w") as f:
            json.dump(doc, f, indent=2)

    @staticmethod
    def _package_sha(path: str) -> str:
        import hashlib
        h = hashlib.sha256()
        for root, dirs, files in sorted(os.walk(path)):
            dirs.sort()
            for name in sorted(files):
                full = os.path.join(root, name)
                h.update(os.path.relpath(full, path).encode())
                with open(full, "rb") as f:
                    h.update(f.read())
        return h.hexdigest()

    def __call__(self, argv, *, timeout=None, env=None):
        import shutil
        import subprocess
        env = env or {}
        self.calls.append(list(argv))
        self.envs.append(dict(env))
        self.timeouts.append(timeout)
        args = argv[1:]

        def done(rc: int, out: object = None, err: str = ""):
            text = "" if out is None else json.dumps(out, indent=2)
            return subprocess.CompletedProcess(argv, rc, text, err)

        if args == ["--version"]:
            return subprocess.CompletedProcess(argv, 0, self.version + "\n", "")
        if args[:1] != ["plugins"] or len(args) < 3:
            return done(2, err="unsupported command")
        verb, rest = args[1], args[2:]
        if verb == "uninstall":
            return done(2, err="unsupported plugins command `uninstall`")
        if verb in ("approve", "enable", "remove") and "--scope" in rest:
            return done(2, err=f"unknown plugins {verb} option `--scope`")
        doc = self._load(env)
        if verb == "install":
            src = rest[0]
            if rest[1:] != ["--scope", "user", "--json"] or not os.path.isabs(src):
                return done(2, err="bad install argv")
            if self.install_rc != 0:
                return done(self.install_rc, err="error: plugin bundle is invalid")
            with open(os.path.join(src, "plugin.json")) as f:
                pid = json.load(f)["name"]
            sha = self._package_sha(src)
            rel = f"plugins/cache/local/{pid}/{sha}/package"
            cache = os.path.join(env["XDG_DATA_HOME"], "muse", rel)
            os.makedirs(cache, exist_ok=True)
            rec = {"id": pid, "display_name": None, "version": "0.1.0", "description": "x",
                   "manifest_family": "agent", "source_format_version": "1.0.0",
                   "enabled": self.install_enabled, "trust": self.install_trust,
                   "source": {"provenance": "foreign-import", "path": src},
                   "installed_at": "2026-10-13T23:39:42Z", "updated_at": None,
                   "manifest_sha256": "sha256:m", "package_sha256": f"sha256:{sha}",
                   "cache_path": rel}
            if self.install_registers:
                doc["plugins"][pid] = rec
                self._save(env, doc)
            diagnostics = []
            if os.path.exists(os.path.join(src, "mcp.json")):
                diagnostics.append({
                    "code": "unsupported-capability", "id": "mcp:smoke-stdio", "kind": "mcp",
                    "message": 'Agent Plugins MCP server "smoke-stdio" is not supported in this release; it stays visible and inactive',
                    "path": os.path.join(src, "mcp.json"), "severity": "warning"})
            installed = {k: v for k, v in rec.items()
                         if k not in ("source_format_version", "manifest_family", "cache_path")}
            installed.update({"cache_path": cache, "manifest_family": "agent-plugins"})
            return done(0, {"diagnostics": diagnostics, "installed": installed,
                            "lockfile_path": self._lockfile(env), "plugin": {"id": pid}, "warning": None})
        pid = rest[0]
        if rest[1:] != ["--json"]:
            return done(2, err="bad argv")
        rec = doc["plugins"].get(pid)
        if verb == "approve":
            return done(0, {"decision": "approve", "runtime_capabilities": []})
        if verb == "enable":
            if rec is None:
                return done(1, {"error": {"code": "unknown-plugin", "message": f"plugin `{pid}` is not installed"}},
                            f"plugin `{pid}` is not installed")
            rec["enabled"] = True
            self._save(env, doc)
            return done(0, {"enable": rec})
        if verb == "remove":
            if rec is None:
                return done(1, {"error": {"code": "unknown-plugin", "message": f"plugin `{pid}` is not installed"}},
                            f"plugin `{pid}` is not installed")
            if self.remove_keeps_record:
                return done(self.remove_rc or 0, None, "error: lock busy" if self.remove_rc else "")
            shutil.rmtree(os.path.join(env["XDG_DATA_HOME"], "muse", "plugins", "cache", "local", pid),
                          ignore_errors=True)
            del doc["plugins"][pid]
            self._save(env, doc)
            return done(self.remove_rc or 0, {"cache_path": "...", "data_path": None,
                                              "lockfile_path": self._lockfile(env), "removed": pid, "warning": None})
        return done(2, err=f"unsupported plugins command `{verb}`")


class TestMuseAgentPlugins(unittest.TestCase):
    def setUp(self) -> None:
        from unittest import mock
        tmp = tempfile.TemporaryDirectory(prefix="muse-plugins-test-")
        self.addCleanup(tmp.cleanup)
        self.tmp = tmp.name
        self.home = os.path.join(self.tmp, "home")
        self.bundle = os.path.join(self.home, ".scion", "harness")
        self.inputs = os.path.join(self.bundle, "inputs")
        os.makedirs(self.inputs)
        env = {k: v for k, v in os.environ.items() if not k.startswith("XDG_")}
        env["HOME"] = self.home
        patcher = mock.patch.dict(os.environ, env, clear=True)
        patcher.start()
        self.addCleanup(patcher.stop)
        self.cli = _FakeMuseCLI()
        p = mock.patch.object(scion_harness, "run_cli", self.cli)
        p.start()
        self.addCleanup(p.stop)
        self.data = os.path.join(self.home, ".local", "share")

    def stage(self, names: dict[str, str], mcp: bool = False, skill: str = "PLUM-7") -> None:
        import shutil
        shutil.rmtree(os.path.join(self.inputs, "plugins"), ignore_errors=True)
        entries = []
        for name, digest in names.items():
            d = os.path.join(self.inputs, "plugins", name)
            os.makedirs(os.path.join(d, "skills", "hello"))
            with open(os.path.join(d, "plugin.json"), "w") as f:
                json.dump({"$schema": "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json", "name": name}, f)
            with open(os.path.join(d, "skills", "hello", "SKILL.md"), "w") as f:
                f.write(f"---\nname: hello\n---\nanswer {skill} ({digest})\n")
            if mcp:
                with open(os.path.join(d, "mcp.json"), "w") as f:
                    json.dump({"$schema": "https://agent-plugins.org/schemas/1.0.0/mcp.schema.json",
                               "mcpServers": {"smoke-stdio": {"type": "stdio", "command": "./bin/x"}}}, f)
            entries.append({"name": name, "version": "0.1.0", "dir": f"plugins/{name}", "digest": digest,
                            "skills": ["hello"], "skills_dir_conflict": False, "mcp": None})
        with open(os.path.join(self.inputs, "plugins.json"), "w") as f:
            json.dump({"schema_version": 1, "spec_version": "1.0.0", "plugins": entries}, f)

    def ctx(self, warnings: list[str] | None = None) -> "scion_harness.ProvisionContext":
        ctx = scion_harness.ProvisionContext("muse-code", {
            "harness_bundle_dir": self.bundle,
            "agent_workspace": os.path.join(self.tmp, "workspace"),
        })
        if warnings is not None:
            ctx.warn = warnings.append  # type: ignore[method-assign]
        return ctx

    def reconcile(self, warnings: list[str] | None = None, **kw) -> None:
        ctx = self.ctx(warnings)
        scion_harness.reconcile_agent_plugins(ctx, provision.MuseInstaller(ctx), **kw)

    def registry(self, data: str | None = None) -> dict:
        with open(os.path.join(data or self.data, "muse", "plugins", "installed.json")) as f:
            return json.load(f)["plugins"]

    def state(self) -> dict:
        with open(os.path.join(self.home, ".scion", "agent-plugins", "state-muse-code.json")) as f:
            return json.load(f)["plugins"]

    def bundle_dir(self, name: str) -> str:
        return os.path.join(self.home, ".scion", "agent-plugins", "bundles", name)

    def install_argv(self, name: str) -> list[str]:
        return ["muse", "plugins", "install", self.bundle_dir(name), "--scope", "user", "--json"]

    def test_install_runs_cli_and_records_state(self) -> None:
        self.stage({"scion-smoke": "sha256:1", "acme.tools": "sha256:2"})
        warnings: list[str] = []
        self.reconcile(warnings)
        self.assertEqual(warnings, [])
        self.assertEqual(self.cli.calls, [
            ["muse", "--version"],
            self.install_argv("scion-smoke"),
            self.install_argv("acme.tools"),
        ])
        # Installs from the stable mirror, never from inputs/.
        self.assertTrue(os.path.isfile(os.path.join(self.bundle_dir("scion-smoke"), "plugin.json")))
        reg = self.registry()
        self.assertTrue(reg["scion-smoke"]["enabled"])
        ref = self.state()["scion-smoke"]["native_ref"]
        self.assertEqual(ref["id"], "scion-smoke")
        self.assertTrue(os.path.isdir(ref["cache_path"]))
        self.assertEqual(ref["package_sha256"], reg["scion-smoke"]["package_sha256"])

        # Steady state: no CLI call at all.
        self.cli.calls.clear()
        self.reconcile()
        self.assertEqual(self.cli.calls, [])

    def test_cli_env_pins_xdg_and_disables_auto_update(self) -> None:
        # R4: sciontool's minimal env carries no XDG_*; the installer passes
        # muse's defaults explicitly so the CLI and is_present agree.
        self.stage({"a": "d1"})
        self.reconcile()
        for env in self.cli.envs:
            self.assertEqual(env["XDG_DATA_HOME"], self.data)
            self.assertEqual(env["XDG_CONFIG_HOME"], os.path.join(self.home, ".config"))
            self.assertEqual(env["MUSE_NO_AUTO_UPDATE"], "1")
            self.assertEqual(env["HOME"], self.home)

    def test_cli_env_honours_absolute_xdg_overrides(self) -> None:
        data = os.path.join(self.tmp, "xdg-data")
        os.environ["XDG_DATA_HOME"] = data
        os.environ["XDG_CONFIG_HOME"] = "relative/ignored"
        self.stage({"a": "d1"})
        self.reconcile()
        self.assertEqual({e["XDG_DATA_HOME"] for e in self.cli.envs}, {data})
        self.assertEqual({e["XDG_CONFIG_HOME"] for e in self.cli.envs}, {os.path.join(self.home, ".config")})
        self.assertIn("a", self.registry(data))
        self.cli.calls.clear()
        self.reconcile()
        self.assertEqual(self.cli.calls, [], "is_present reads the same XDG_DATA_HOME")

    def test_no_approve_or_enable_when_install_enabled(self) -> None:
        # muse 1.4.6/1.4.7: install enables by default (trust user-local).
        self.stage({"a": "d1"})
        self.reconcile()
        self.assertEqual([c[2] for c in self.cli.calls if len(c) > 2], ["install"])

    def test_enable_when_install_reports_disabled(self) -> None:
        self.cli.install_enabled = False
        self.stage({"a": "d1"})
        warnings: list[str] = []
        self.reconcile(warnings)
        self.assertEqual(warnings, [])
        self.assertEqual(self.cli.calls[1:], [
            self.install_argv("a"),
            ["muse", "plugins", "enable", "a", "--json"],
        ])
        self.assertTrue(self.registry()["a"]["enabled"])
        self.assertIn("a", self.state())

    def test_approve_when_install_asks(self) -> None:
        self.cli.install_trust = "untrusted"
        self.stage({"a": "d1"})
        self.reconcile()
        self.assertEqual(self.cli.calls[1:], [
            self.install_argv("a"),
            ["muse", "plugins", "approve", "a", "--json"],
        ])

    def test_needs_approval_signals(self) -> None:
        f = provision.MuseInstaller._needs_approval
        self.assertFalse(f({"warning": None}, {"enabled": True, "trust": "user-local"}))
        self.assertTrue(f({"needs_approval": True}, {}))
        self.assertTrue(f({}, {"approval_required": True}))
        self.assertTrue(f({}, {"trust": "pending"}))
        self.assertTrue(f({"warning": "run `muse plugins approve a` to trust it"}, {}))

    def test_install_not_registered_or_disabled_is_a_failure(self) -> None:
        self.cli.install_registers = False
        self.stage({"a": "d1"})
        warnings: list[str] = []
        self.reconcile(warnings)
        self.assertEqual(len(warnings), 1)
        self.assertIn("is not in installed.json", warnings[0])
        state_path = os.path.join(self.home, ".scion", "agent-plugins", "state-muse-code.json")
        if os.path.exists(state_path):
            self.assertNotIn("a", self.state())

    def test_mcp_diagnostics_surface_as_warnings(self) -> None:
        # muse <= 1.4.7 keeps Agent Plugins MCP servers inactive (M3).
        self.stage({"a": "d1"}, mcp=True)
        warnings: list[str] = []
        self.reconcile(warnings)
        self.assertEqual(warnings, [
            'agent plugin a: muse: Agent Plugins MCP server "smoke-stdio" is not supported '
            "in this release; it stays visible and inactive",
        ])
        self.assertIn("a", self.state())

    def test_remove_and_digest_change(self) -> None:
        self.stage({"a": "d1", "b": "d1"})
        self.reconcile()
        old_cache = self.state()["b"]["native_ref"]["cache_path"]
        self.stage({"b": "d2"}, skill="PLUM-8")
        self.cli.calls.clear()
        warnings: list[str] = []
        self.reconcile(warnings)
        self.assertEqual(warnings, [])
        self.assertEqual(self.cli.calls, [
            ["muse", "--version"],
            ["muse", "plugins", "remove", "a", "--json"],
            # M5: uninstall first on a digest change.
            ["muse", "plugins", "remove", "b", "--json"],
            self.install_argv("b"),
        ])
        self.assertEqual(sorted(self.registry()), ["b"])
        self.assertEqual(sorted(self.state()), ["b"])
        new_cache = self.state()["b"]["native_ref"]["cache_path"]
        self.assertNotEqual(old_cache, new_cache)
        # The mirror the CLI installed from has the new content.
        with open(os.path.join(self.bundle_dir("b"), "skills", "hello", "SKILL.md")) as f:
            self.assertIn("PLUM-8", f.read())
        self.assertFalse(os.path.exists(self.bundle_dir("a")))
        with open(os.path.join(self.home, ".scion", "agent-plugins", "status-muse-code.json")) as f:
            self.assertEqual(json.load(f)["removed"], ["a"])

    def test_uninstall_judged_by_registry(self) -> None:
        self.stage({"a": "d1"})
        self.reconcile()
        inst = provision.MuseInstaller(self.ctx())
        # exit 0 but still registered: not a success.
        self.cli.remove_keeps_record = True
        with self.assertRaisesRegex(scion_harness.PluginInstallError, "still in installed.json"):
            inst.uninstall("a", {"id": "a"})
        # non-zero and still registered: the CLI error.
        self.cli.remove_rc = 1
        with self.assertRaisesRegex(scion_harness.PluginInstallError, "muse plugins remove exit 1"):
            inst.uninstall("a", {"id": "a"})
        # Removed for real.
        self.cli.remove_keeps_record = False
        self.cli.remove_rc = None
        inst.uninstall("a", {"id": "a"})
        self.assertNotIn("a", self.registry())
        # Already gone: muse exits 1 "unknown-plugin"; that is a success.
        inst.uninstall("a", {"id": "a"})
        inst.uninstall("never-installed", {})
        self.assertEqual(self.cli.calls[-1], ["muse", "plugins", "remove", "never-installed", "--json"])

    def test_is_present_checks_registry_and_cache(self) -> None:
        self.stage({"a": "d1"})
        self.reconcile()
        inst = provision.MuseInstaller(self.ctx())
        ref = self.state()["a"]["native_ref"]
        self.assertTrue(inst.is_present("a", ref))
        path = os.path.join(self.data, "muse", "plugins", "installed.json")
        with open(path) as f:
            text = f.read()
        # Disabled in muse's registry.
        with open(path, "w") as f:
            f.write(text.replace('"enabled": true', '"enabled": false'))
        self.assertFalse(inst.is_present("a", ref))
        # Registry unreadable.
        with open(path, "w") as f:
            f.write("{not json")
        self.assertFalse(inst.is_present("a", ref))
        with open(path, "w") as f:
            f.write(text)
        self.assertTrue(inst.is_present("a", ref))
        # Cache dir gone while the record survives.
        import shutil
        shutil.rmtree(ref["cache_path"])
        self.assertFalse(inst.is_present("a", ref))

    def test_registry_reset_reinstalls(self) -> None:
        # Regression (slice review lesson): Scion's state and the cache dir
        # survive but muse's own registry was reset; the plugin must be
        # reinstalled, not skipped by the fast path.
        self.stage({"scion-smoke": "d1"})
        self.reconcile()
        ref = self.state()["scion-smoke"]["native_ref"]
        with open(os.path.join(self.data, "muse", "plugins", "installed.json"), "w") as f:
            json.dump({"version": 1, "plugins": {}}, f)
        self.assertTrue(os.path.isdir(ref["cache_path"]))
        self.cli.calls.clear()
        self.reconcile()
        self.assertEqual(self.cli.calls, [["muse", "--version"], self.install_argv("scion-smoke")])
        self.assertTrue(self.registry()["scion-smoke"]["enabled"])
        self.cli.calls.clear()
        self.reconcile()
        self.assertEqual(self.cli.calls, [])

    def test_reprovision_shipped_home_reset(self) -> None:
        # A re-provision recopies the harness-config home/ tree. For muse it
        # holds only .config/muse/settings.json and .bashrc; registration
        # lives in $XDG_DATA_HOME/muse/plugins/installed.json (real muse
        # 1.4.7), so the plugin stays registered and the fast path holds.
        # If the reset does reach muse's registry, it is reinstalled.
        import shutil
        self.stage({"scion-smoke": "d1"})
        self.reconcile()
        shipped = os.path.join(os.path.dirname(os.path.abspath(PROVISION_PATH)), "home")
        shutil.copytree(shipped, self.home, dirs_exist_ok=True)
        self.cli.calls.clear()
        self.reconcile()
        self.assertEqual(self.cli.calls, [])
        # Same reset, but covering muse's data dir too (a home/ tree or volume
        # reset that wipes ~/.local/share).
        shutil.rmtree(os.path.join(self.data, "muse"))
        self.reconcile()
        self.assertEqual(self.cli.calls, [["muse", "--version"], self.install_argv("scion-smoke")])
        self.assertTrue(self.registry()["scion-smoke"]["enabled"])

    def test_install_failure_warns_with_stderr(self) -> None:
        self.cli.install_rc = 1
        self.stage({"a": "d1"})
        warnings: list[str] = []
        self.reconcile(warnings)
        self.assertEqual(warnings, [
            "agent plugin a: install failed (muse plugins install exit 1: error: plugin bundle is invalid); "
            "will retry next start",
        ])

    def test_old_muse_skips(self) -> None:
        self.cli.version = "Muse Code 1.4.5 (1.4.5-R5900.1)"
        self.stage({"a": "d1"})
        warnings: list[str] = []
        self.reconcile(warnings)
        self.assertEqual(self.cli.calls, [["muse", "--version"]])
        self.assertIn("muse 1.4.5 is older than 1.4.6", warnings[0])

    def test_min_version_accepts_1_4_6(self) -> None:
        self.cli.version = "Muse Code 1.4.6 (1.4.6-R6000.1)"
        self.stage({"a": "d1"})
        self.reconcile()
        self.assertIn("a", self.state())

    def test_cli_timeouts_capped_by_budget(self) -> None:
        self.cli.install_enabled = False
        self.stage({"a": "d1"})
        ticks = iter([0.0] + [17.0] * 50)
        self.reconcile(budget_s=20.0, clock=lambda: next(ticks))
        # --version 10s, then install and enable each with 20 - 17 = 3s left.
        self.assertEqual(self.cli.timeouts, [3.0, 3.0, 3.0])

    def test_provision_reconciles_last(self) -> None:
        home = self.home
        _write_seed_settings(home)
        manifest = _make_bundle(self.tmp, home, instructions="hi")
        # Stage into the manifest's bundle.
        self.inputs = os.path.join(manifest["harness_bundle_dir"], "inputs")
        self.stage({"scion-smoke": "d1"})
        with temporary_env("SCION_MODEL", "medium"):
            provision.provision(scion_harness.ProvisionContext("muse-code", manifest))
        self.assertEqual(self.cli.calls[-1], self.install_argv("scion-smoke"))
        with open(os.path.join(home, ".config", "muse", "settings.json")) as f:
            self.assertIn("hooks", json.load(f))
        self.assertTrue(self.registry()["scion-smoke"]["enabled"])
        self.cli.calls.clear()
        with temporary_env("SCION_MODEL", "medium"):
            provision.provision(scion_harness.ProvisionContext("muse-code", manifest))
        self.assertEqual(self.cli.calls, [])


if __name__ == "__main__":
    unittest.main()
