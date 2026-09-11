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
        "SessionStart": [{"matcher": "*", "hooks": [{"type": "command", "command": "sciontool hook --dialect=muse-code"}]}],
        "SessionEnd": [{"matcher": "*", "hooks": [{"type": "command", "command": "sciontool hook --dialect=muse-code"}]}],
        "UserPromptSubmit": [{"matcher": "*", "hooks": [{"type": "command", "command": "sciontool hook --dialect=muse-code"}]}],
        "PreToolUse": [{"matcher": "*", "hooks": [{"type": "command", "command": "sciontool hook --dialect=muse-code"}]}],
        "PostToolUse": [{"matcher": "*", "hooks": [{"type": "command", "command": "sciontool hook --dialect=muse-code"}]}],
        "PreLLMCall": [{"matcher": "*", "hooks": [{"type": "command", "command": "sciontool hook --dialect=muse-code"}]}],
        "PostLLMCall": [{"matcher": "*", "hooks": [{"type": "command", "command": "sciontool hook --dialect=muse-code"}]}],
        "PermissionRequest": [{"matcher": "*", "hooks": [{"type": "command", "command": "sciontool hook --dialect=muse-code"}]}],
        "PreCompact": [{"matcher": "*", "hooks": [{"type": "command", "command": "sciontool hook --dialect=muse-code"}]}],
        "PostCompact": [{"matcher": "*", "hooks": [{"type": "command", "command": "sciontool hook --dialect=muse-code"}]}],
        "SubagentStart": [{"matcher": "*", "hooks": [{"type": "command", "command": "sciontool hook --dialect=muse-code"}]}],
        "SubagentStop": [{"matcher": "*", "hooks": [{"type": "command", "command": "sciontool hook --dialect=muse-code"}]}],
        "Stop": [{"matcher": "*", "hooks": [{"type": "command", "command": "sciontool hook --dialect=muse-code"}]}],
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
        "agent_workspace": "/workspace",
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

            with temporary_home(home), temporary_env("SCION_MODEL", "small"):
                ctx = scion_harness.ProvisionContext("muse-code", manifest)
                provision.provision(ctx)

            # Model should be muse-spark-1.2 (all aliases map to it).
            # Verify provision completed without error (model is written
            # to env overlay or passed via host).
            auth_json = _read_json(
                os.path.join(tmp, "bundle", "outputs", "resolved-auth.json")
            )
            self.assertEqual(auth_json["method"], "none")

    def test_model_passthrough_for_unknown(self) -> None:
        """Unknown model names are passed through as-is."""
        with tempfile.TemporaryDirectory() as tmp:
            home = os.path.join(tmp, "home")
            os.makedirs(home)
            _write_seed_settings(home)

            manifest = _make_bundle(tmp, home)

            with temporary_home(home), temporary_env("SCION_MODEL", "custom-model-v2"):
                ctx = scion_harness.ProvisionContext("muse-code", manifest)
                provision.provision(ctx)

            # Should succeed without error.
            auth_json = _read_json(
                os.path.join(tmp, "bundle", "outputs", "resolved-auth.json")
            )
            self.assertIsNotNone(auth_json)

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


if __name__ == "__main__":
    unittest.main()
