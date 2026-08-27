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
"""Unit tests for the Grok Build harness provisioner.

Run with:  python3 -m unittest provision_test -v
"""

from __future__ import annotations

import importlib.util
import json
import os
import tempfile
import unittest
from contextlib import contextmanager
from typing import Any

PROVISION_PATH = os.path.join(os.path.dirname(__file__), "provision.py")
SPEC = importlib.util.spec_from_file_location("grok_build_provision", PROVISION_PATH)
assert SPEC is not None
provision = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
SPEC.loader.exec_module(provision)

scion_harness = provision.scion_harness


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


def _make_ctx(
    manifest: dict[str, Any] | None = None,
) -> scion_harness.ProvisionContext:
    """Build a ProvisionContext with sensible defaults for testing."""
    m: dict[str, Any] = {
        "command": "provision",
        "harness_config": {
            "no_auth": {"behavior": "drop-to-shell"},
            "instructions_file": "AGENTS.md",
            "system_prompt_mode": "prepend_to_instructions",
            "skills_dir": ".grok/skills",
        },
    }
    if manifest:
        m.update(manifest)
    ctx = scion_harness.ProvisionContext("grok-build", m)
    return ctx


# ---------------------------------------------------------------------------
# Auth Selection Tests
# ---------------------------------------------------------------------------


class AuthSelectionApiKeyTest(unittest.TestCase):
    """Test api-key auth selection."""

    def test_api_key_selected_when_xai_key_present(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            inputs_dir = os.path.join(tmp, "inputs")
            os.makedirs(inputs_dir)
            scion_harness.atomic_write_json(
                os.path.join(inputs_dir, "auth-candidates.json"),
                {"env_vars": ["XAI_API_KEY"], "env_secret_files": {}},
            )
            ctx = _make_ctx({"harness_bundle_dir": tmp})
            with temporary_home(tmp):
                resolved = ctx.select_auth(provision.AUTH)
            self.assertEqual(resolved.method, "api-key")
            self.assertEqual(resolved.env_key, "XAI_API_KEY")

    def test_api_key_with_secret_file(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            inputs_dir = os.path.join(tmp, "inputs")
            os.makedirs(inputs_dir)
            secret_path = os.path.join(tmp, "xai-secret")
            with open(secret_path, "w") as f:
                f.write("xai-test-key-123")
            scion_harness.atomic_write_json(
                os.path.join(inputs_dir, "auth-candidates.json"),
                {
                    "env_vars": ["XAI_API_KEY"],
                    "env_secret_files": {"XAI_API_KEY": secret_path},
                },
            )
            ctx = _make_ctx({"harness_bundle_dir": tmp})
            with temporary_home(tmp):
                resolved = ctx.select_auth(provision.AUTH)
            self.assertEqual(resolved.method, "api-key")
            self.assertEqual(resolved.env_key, "XAI_API_KEY")


class AuthSelectionAuthFileTest(unittest.TestCase):
    """Test auth-file auth selection."""

    def test_auth_file_selected_when_grok_auth_staged(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            inputs_dir = os.path.join(tmp, "inputs")
            os.makedirs(inputs_dir)
            secret_path = os.path.join(tmp, "grok-auth-secret")
            with open(secret_path, "w") as f:
                f.write('{"token": "test"}')
            scion_harness.atomic_write_json(
                os.path.join(inputs_dir, "auth-candidates.json"),
                {
                    "env_vars": [],
                    "file_secret_files": {"GROK_AUTH": secret_path},
                },
            )
            ctx = _make_ctx({"harness_bundle_dir": tmp})
            with temporary_home(tmp):
                resolved = ctx.select_auth(provision.AUTH)
            self.assertEqual(resolved.method, "auth-file")


class AuthSelectionVertexAiTest(unittest.TestCase):
    """Test vertex-ai auth selection."""

    def test_vertex_ai_selected_when_gcp_env_present(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            inputs_dir = os.path.join(tmp, "inputs")
            os.makedirs(inputs_dir)
            project_path = os.path.join(tmp, "gcp-project")
            with open(project_path, "w") as f:
                f.write("test-project-id")
            region_path = os.path.join(tmp, "gcp-region")
            with open(region_path, "w") as f:
                f.write("us-central1")
            scion_harness.atomic_write_json(
                os.path.join(inputs_dir, "auth-candidates.json"),
                {
                    "env_vars": ["GOOGLE_CLOUD_PROJECT", "GOOGLE_CLOUD_REGION"],
                    "env_secret_files": {
                        "GOOGLE_CLOUD_PROJECT": project_path,
                        "GOOGLE_CLOUD_REGION": region_path,
                    },
                },
            )
            ctx = _make_ctx({"harness_bundle_dir": tmp})
            with temporary_home(tmp):
                resolved = ctx.select_auth(provision.AUTH)
            self.assertEqual(resolved.method, "vertex-ai")

    def test_vertex_ai_env_overlay_sets_project_and_region(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            inputs_dir = os.path.join(tmp, "inputs")
            os.makedirs(inputs_dir)
            project_path = os.path.join(tmp, "gcp-project")
            with open(project_path, "w") as f:
                f.write("my-project")
            region_path = os.path.join(tmp, "gcp-region")
            with open(region_path, "w") as f:
                f.write("europe-west1")
            scion_harness.atomic_write_json(
                os.path.join(inputs_dir, "auth-candidates.json"),
                {
                    "env_vars": ["GOOGLE_CLOUD_PROJECT", "GOOGLE_CLOUD_REGION"],
                    "env_secret_files": {
                        "GOOGLE_CLOUD_PROJECT": project_path,
                        "GOOGLE_CLOUD_REGION": region_path,
                    },
                },
            )
            ctx = _make_ctx({"harness_bundle_dir": tmp})
            with temporary_home(tmp):
                provision.provision(ctx)
                outputs_path = os.path.join(tmp, "outputs", "env.json")
                self.assertTrue(os.path.isfile(outputs_path))
                with open(outputs_path) as f:
                    env_data = json.load(f)
                self.assertEqual(env_data["GOOGLE_CLOUD_PROJECT"], "my-project")
                self.assertEqual(env_data["GOOGLE_CLOUD_REGION"], "europe-west1")

    def test_vertex_ai_with_location_key(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            inputs_dir = os.path.join(tmp, "inputs")
            os.makedirs(inputs_dir)
            project_path = os.path.join(tmp, "gcp-project")
            with open(project_path, "w") as f:
                f.write("loc-project")
            location_path = os.path.join(tmp, "gcp-location")
            with open(location_path, "w") as f:
                f.write("asia-east1")
            scion_harness.atomic_write_json(
                os.path.join(inputs_dir, "auth-candidates.json"),
                {
                    "env_vars": ["GOOGLE_CLOUD_PROJECT", "GOOGLE_CLOUD_LOCATION"],
                    "env_secret_files": {
                        "GOOGLE_CLOUD_PROJECT": project_path,
                        "GOOGLE_CLOUD_LOCATION": location_path,
                    },
                },
            )
            ctx = _make_ctx({"harness_bundle_dir": tmp})
            with temporary_home(tmp):
                resolved = ctx.select_auth(provision.AUTH)
            self.assertEqual(resolved.method, "vertex-ai")

    def test_vertex_ai_location_key_env_overlay(self) -> None:
        """Full provision with GOOGLE_CLOUD_LOCATION normalizes to GOOGLE_CLOUD_REGION."""
        with tempfile.TemporaryDirectory() as tmp:
            inputs_dir = os.path.join(tmp, "inputs")
            os.makedirs(inputs_dir)
            project_path = os.path.join(tmp, "gcp-project")
            with open(project_path, "w") as f:
                f.write("loc-project")
            location_path = os.path.join(tmp, "gcp-location")
            with open(location_path, "w") as f:
                f.write("asia-east1")
            scion_harness.atomic_write_json(
                os.path.join(inputs_dir, "auth-candidates.json"),
                {
                    "env_vars": ["GOOGLE_CLOUD_PROJECT", "GOOGLE_CLOUD_LOCATION"],
                    "env_secret_files": {
                        "GOOGLE_CLOUD_PROJECT": project_path,
                        "GOOGLE_CLOUD_LOCATION": location_path,
                    },
                },
            )
            ctx = _make_ctx({"harness_bundle_dir": tmp})
            with temporary_home(tmp):
                provision.provision(ctx)
                outputs_path = os.path.join(tmp, "outputs", "env.json")
                self.assertTrue(os.path.isfile(outputs_path))
                with open(outputs_path) as f:
                    env_data = json.load(f)
                self.assertEqual(env_data["GOOGLE_CLOUD_PROJECT"], "loc-project")
                self.assertEqual(env_data["GOOGLE_CLOUD_REGION"], "asia-east1")


class AuthSelectionNoAuthTest(unittest.TestCase):
    """Test no-auth fallback."""

    def test_no_auth_when_no_candidates(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            inputs_dir = os.path.join(tmp, "inputs")
            os.makedirs(inputs_dir)
            ctx = _make_ctx({"harness_bundle_dir": tmp})
            with temporary_home(tmp):
                resolved = ctx.select_auth(provision.AUTH)
            self.assertEqual(resolved.method, "none")


# ---------------------------------------------------------------------------
# Auth File Write Tests
# ---------------------------------------------------------------------------


class WriteAuthFileTest(unittest.TestCase):
    """Test _write_auth_file writes, validates, and secures auth.json."""

    def test_valid_json_written(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            inputs_dir = os.path.join(tmp, "inputs")
            os.makedirs(inputs_dir)
            secret_path = os.path.join(tmp, "grok-auth-secret")
            with open(secret_path, "w") as f:
                f.write('{"token": "xai-test-123"}')
            scion_harness.atomic_write_json(
                os.path.join(inputs_dir, "auth-candidates.json"),
                {"file_secret_files": {"GROK_AUTH": secret_path}},
            )
            ctx = _make_ctx({"harness_bundle_dir": tmp})
            with temporary_home(tmp):
                provision._write_auth_file(ctx)
                auth_path = os.path.join(tmp, ".grok", "auth.json")
                self.assertTrue(os.path.isfile(auth_path))
                with open(auth_path) as f:
                    data = json.load(f)
                self.assertEqual(data["token"], "xai-test-123")

    def test_empty_secret_raises(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            inputs_dir = os.path.join(tmp, "inputs")
            os.makedirs(inputs_dir)
            secret_path = os.path.join(tmp, "grok-auth-secret")
            with open(secret_path, "w") as f:
                f.write("   ")
            scion_harness.atomic_write_json(
                os.path.join(inputs_dir, "auth-candidates.json"),
                {"file_secret_files": {"GROK_AUTH": secret_path}},
            )
            ctx = _make_ctx({"harness_bundle_dir": tmp})
            with temporary_home(tmp):
                with self.assertRaises(scion_harness.ProvisionError) as cm:
                    provision._write_auth_file(ctx)
                self.assertIn("empty", str(cm.exception).lower())

    def test_invalid_json_raises(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            inputs_dir = os.path.join(tmp, "inputs")
            os.makedirs(inputs_dir)
            secret_path = os.path.join(tmp, "grok-auth-secret")
            with open(secret_path, "w") as f:
                f.write("not-valid-json{{{")
            scion_harness.atomic_write_json(
                os.path.join(inputs_dir, "auth-candidates.json"),
                {"file_secret_files": {"GROK_AUTH": secret_path}},
            )
            ctx = _make_ctx({"harness_bundle_dir": tmp})
            with temporary_home(tmp):
                with self.assertRaises(scion_harness.ProvisionError) as cm:
                    provision._write_auth_file(ctx)
                self.assertIn("not valid JSON", str(cm.exception))

    def test_output_file_has_0600_permissions(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            inputs_dir = os.path.join(tmp, "inputs")
            os.makedirs(inputs_dir)
            secret_path = os.path.join(tmp, "grok-auth-secret")
            with open(secret_path, "w") as f:
                f.write('{"token": "secret"}')
            scion_harness.atomic_write_json(
                os.path.join(inputs_dir, "auth-candidates.json"),
                {"file_secret_files": {"GROK_AUTH": secret_path}},
            )
            ctx = _make_ctx({"harness_bundle_dir": tmp})
            with temporary_home(tmp):
                provision._write_auth_file(ctx)
                auth_path = os.path.join(tmp, ".grok", "auth.json")
                mode = os.stat(auth_path).st_mode & 0o777
                self.assertEqual(mode, 0o600)


# ---------------------------------------------------------------------------
# Instructions Tests
# ---------------------------------------------------------------------------


class InstructionsTest(unittest.TestCase):
    """Test instruction projection."""

    def test_instructions_projected_to_agents_md(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            inputs_dir = os.path.join(tmp, "inputs")
            os.makedirs(inputs_dir)
            with open(os.path.join(inputs_dir, "instructions.md"), "w") as f:
                f.write("Do the thing.\n")
            ctx = _make_ctx({"harness_bundle_dir": tmp})
            with temporary_home(tmp):
                target = os.path.join(tmp, "AGENTS.md")
                scion_harness.project_instructions(ctx, target)
                self.assertTrue(os.path.isfile(target))
                with open(target) as f:
                    content = f.read()
                self.assertIn("Do the thing.", content)
                self.assertIn("<!-- BEGIN SCION MANAGED -->", content)
                self.assertIn("<!-- END SCION MANAGED -->", content)


# ---------------------------------------------------------------------------
# MCP Translation Tests
# ---------------------------------------------------------------------------


class MCPTranslationTest(unittest.TestCase):
    """Test MCP translation via the end-to-end TOML write path.

    The translate function is a closure inside provision() (captures ctx for
    logging), so we test translation through _write_mcp_toml which exercises
    the full pipeline.
    """

    def test_stdio_translation_via_toml(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            ctx = _make_ctx()
            with temporary_home(tmp):
                grok_dir = os.path.join(tmp, ".grok")
                os.makedirs(grok_dir, exist_ok=True)
                servers = {
                    "test-server": {
                        "command": "node",
                        "args": ["server.js", "--port", "3000"],
                        "env": {"DEBUG": "true"},
                    }
                }
                provision._write_mcp_toml(ctx, servers)
                config_path = os.path.join(grok_dir, "config.toml")
                with open(config_path) as f:
                    content = f.read()
                self.assertIn("[mcp_servers.test-server]", content)
                self.assertIn('command = "node"', content)
                self.assertIn("args =", content)

    def test_sse_translation_via_toml(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            ctx = _make_ctx()
            with temporary_home(tmp):
                grok_dir = os.path.join(tmp, ".grok")
                os.makedirs(grok_dir, exist_ok=True)
                servers = {
                    "sse-server": {
                        "url": "https://example.com/sse",
                        "headers": {"Authorization": "Bearer token"},
                    }
                }
                provision._write_mcp_toml(ctx, servers)
                config_path = os.path.join(grok_dir, "config.toml")
                with open(config_path) as f:
                    content = f.read()
                self.assertIn("[mcp_servers.sse-server]", content)
                self.assertIn('"https://example.com/sse"', content)


class MCPTomlWriteTest(unittest.TestCase):
    """Test TOML output for MCP servers."""

    def test_write_mcp_toml_creates_sections(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            ctx = _make_ctx()
            with temporary_home(tmp):
                grok_dir = os.path.join(tmp, ".grok")
                os.makedirs(grok_dir, exist_ok=True)
                servers = {
                    "test-server": {
                        "command": "node",
                        "args": ["server.js"],
                        "env": {"KEY": "val"},
                    }
                }
                provision._write_mcp_toml(ctx, servers)
                config_path = os.path.join(grok_dir, "config.toml")
                self.assertTrue(os.path.isfile(config_path))
                with open(config_path) as f:
                    content = f.read()
                self.assertIn("[mcp_servers.test-server]", content)
                self.assertIn('command = "node"', content)

    def test_write_mcp_toml_strips_old_sections(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            ctx = _make_ctx()
            with temporary_home(tmp):
                grok_dir = os.path.join(tmp, ".grok")
                os.makedirs(grok_dir, exist_ok=True)
                config_path = os.path.join(grok_dir, "config.toml")
                # Write initial content with an MCP section.
                with open(config_path, "w") as f:
                    f.write("[mcp_servers.old]\ncommand = \"old-cmd\"\n\n[other]\nkey = \"val\"\n")
                servers = {"new-server": {"command": "new-cmd"}}
                provision._write_mcp_toml(ctx, servers)
                with open(config_path) as f:
                    content = f.read()
                self.assertNotIn("[mcp_servers.old]", content)
                self.assertIn("[mcp_servers.new-server]", content)
                self.assertIn("[other]", content)

    def test_write_mcp_toml_skips_invalid_bare_key_names(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            ctx = _make_ctx()
            with temporary_home(tmp):
                grok_dir = os.path.join(tmp, ".grok")
                os.makedirs(grok_dir, exist_ok=True)
                servers = {
                    "valid-name": {"command": "good"},
                    "bad.name": {"command": "dotted"},
                    "also bad": {"command": "space"},
                    "ok_name": {"command": "underscored"},
                }
                provision._write_mcp_toml(ctx, servers)
                config_path = os.path.join(grok_dir, "config.toml")
                with open(config_path) as f:
                    content = f.read()
                self.assertIn("[mcp_servers.valid-name]", content)
                self.assertIn("[mcp_servers.ok_name]", content)
                self.assertNotIn("bad.name", content)
                self.assertNotIn("also bad", content)


# ---------------------------------------------------------------------------
# Config Hardening Tests
# ---------------------------------------------------------------------------


class ConfigHardeningTest(unittest.TestCase):
    """Test config hardening writes correct TOML."""

    def test_hardening_writes_managed_block(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            ctx = _make_ctx()
            with temporary_home(tmp):
                grok_dir = os.path.join(tmp, ".grok")
                os.makedirs(grok_dir, exist_ok=True)
                provision._harden_config(ctx)
                config_path = os.path.join(grok_dir, "config.toml")
                self.assertTrue(os.path.isfile(config_path))
                with open(config_path) as f:
                    content = f.read()
                self.assertIn("# BEGIN SCION MANAGED", content)
                self.assertIn("# END SCION MANAGED", content)
                self.assertIn("auto_update = false", content)
                self.assertIn("telemetry = false", content)
                self.assertIn("feedback = false", content)
                self.assertIn("[memory]", content)
                self.assertIn("enabled = false", content)
                self.assertIn("[subagents]", content)

    def test_hardening_preserves_non_managed_content(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            ctx = _make_ctx()
            with temporary_home(tmp):
                grok_dir = os.path.join(tmp, ".grok")
                os.makedirs(grok_dir, exist_ok=True)
                config_path = os.path.join(grok_dir, "config.toml")
                with open(config_path, "w") as f:
                    f.write('[mcp_servers.my_server]\ncommand = "test"\n')
                provision._harden_config(ctx)
                with open(config_path) as f:
                    content = f.read()
                self.assertIn("[mcp_servers.my_server]", content)
                self.assertIn("# BEGIN SCION MANAGED", content)

    def test_hardening_replaces_existing_managed_block(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            ctx = _make_ctx()
            with temporary_home(tmp):
                grok_dir = os.path.join(tmp, ".grok")
                os.makedirs(grok_dir, exist_ok=True)
                config_path = os.path.join(grok_dir, "config.toml")
                with open(config_path, "w") as f:
                    f.write(
                        "# BEGIN SCION MANAGED\n"
                        "[cli]\nauto_update = true\n"
                        "# END SCION MANAGED\n"
                    )
                provision._harden_config(ctx)
                with open(config_path) as f:
                    content = f.read()
                # Should have exactly one managed block.
                self.assertEqual(content.count("# BEGIN SCION MANAGED"), 1)
                self.assertEqual(content.count("# END SCION MANAGED"), 1)
                self.assertIn("auto_update = false", content)


# ---------------------------------------------------------------------------
# Model Resolution Tests
# ---------------------------------------------------------------------------


class ModelResolutionTest(unittest.TestCase):
    """Test model resolution via SCION_MODEL env var and model_aliases."""

    _saved_scion_model: str | None

    def setUp(self) -> None:
        super().setUp()
        self._saved_scion_model = os.environ.pop("SCION_MODEL", None)

    def tearDown(self) -> None:
        os.environ.pop("SCION_MODEL", None)
        if self._saved_scion_model is not None:
            os.environ["SCION_MODEL"] = self._saved_scion_model
        super().tearDown()

    def _resolve(self, scion_model: str = "") -> str:
        """Simulate the model resolution logic from provision()."""
        if scion_model:
            os.environ["SCION_MODEL"] = scion_model
        else:
            os.environ.pop("SCION_MODEL", None)
        ctx = _make_ctx({
            "harness_config": {
                "no_auth": {"behavior": "drop-to-shell"},
                "instructions_file": "AGENTS.md",
                "model_aliases": {
                    "small": "grok-3-mini",
                    "medium": "grok-3",
                    "large": "grok-4",
                    "extra-large": "grok-4",
                },
            },
        })
        raw = os.environ.get("SCION_MODEL", "").strip()
        aliases = ctx.harness_config.get("model_aliases") or {}
        return aliases.get(raw.lower(), raw) if raw else ""

    def test_small_alias_resolves_to_grok_3_mini(self) -> None:
        self.assertEqual(self._resolve("small"), "grok-3-mini")

    def test_medium_alias_resolves_to_grok_3(self) -> None:
        self.assertEqual(self._resolve("medium"), "grok-3")

    def test_large_alias_resolves_to_grok_4(self) -> None:
        self.assertEqual(self._resolve("large"), "grok-4")

    def test_extra_large_alias_resolves_to_grok_4(self) -> None:
        self.assertEqual(self._resolve("extra-large"), "grok-4")

    def test_raw_model_name_passes_through(self) -> None:
        self.assertEqual(self._resolve("grok-4-turbo"), "grok-4-turbo")

    def test_empty_scion_model_returns_empty(self) -> None:
        self.assertEqual(self._resolve(""), "")

    def test_alias_is_case_insensitive(self) -> None:
        self.assertEqual(self._resolve("SMALL"), "grok-3-mini")
        self.assertEqual(self._resolve("Large"), "grok-4")


# ---------------------------------------------------------------------------
# Hook Write Tests
# ---------------------------------------------------------------------------


class HookWriteTest(unittest.TestCase):
    """Test _write_hooks produces correct JSON structure."""

    def test_hooks_written_correctly(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            provision._write_hooks(tmp)
            hooks_path = os.path.join(tmp, ".grok", "hooks", "scion.json")
            self.assertTrue(os.path.isfile(hooks_path))
            with open(hooks_path) as f:
                data = json.load(f)
            hooks = data["hooks"]
            # Check all expected events present.
            for event in provision._GROK_HOOK_EVENTS:
                self.assertIn(event, hooks, f"missing hook event: {event}")

    def test_session_start_uses_echo(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            provision._write_hooks(tmp)
            hooks_path = os.path.join(tmp, ".grok", "hooks", "scion.json")
            with open(hooks_path) as f:
                data = json.load(f)
            session_start = data["hooks"]["SessionStart"]
            cmd = session_start[0]["hooks"][0]["command"]
            self.assertIn("echo", cmd)
            self.assertIn("SessionStart", cmd)
            self.assertIn("sciontool hook --dialect=grok-build", cmd)

    def test_session_end_uses_echo(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            provision._write_hooks(tmp)
            hooks_path = os.path.join(tmp, ".grok", "hooks", "scion.json")
            with open(hooks_path) as f:
                data = json.load(f)
            session_end = data["hooks"]["SessionEnd"]
            cmd = session_end[0]["hooks"][0]["command"]
            self.assertIn("echo", cmd)
            self.assertIn("SessionEnd", cmd)

    def test_pre_tool_use_uses_cat(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            provision._write_hooks(tmp)
            hooks_path = os.path.join(tmp, ".grok", "hooks", "scion.json")
            with open(hooks_path) as f:
                data = json.load(f)
            pre_tool = data["hooks"]["PreToolUse"]
            cmd = pre_tool[0]["hooks"][0]["command"]
            self.assertTrue(cmd.startswith("cat |"))
            self.assertIn("sciontool hook --dialect=grok-build", cmd)

    def test_stop_has_longer_timeout(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            provision._write_hooks(tmp)
            hooks_path = os.path.join(tmp, ".grok", "hooks", "scion.json")
            with open(hooks_path) as f:
                data = json.load(f)
            stop = data["hooks"]["Stop"]
            timeout = stop[0]["hooks"][0]["timeout"]
            self.assertEqual(timeout, 10)


# ---------------------------------------------------------------------------
# Telemetry Tests
# ---------------------------------------------------------------------------


class BaseTelemetryTest(unittest.TestCase):
    """Base class that isolates tests from host SCION_/OTEL_/GROK_ env vars."""

    _saved_env: dict[str, str]

    def setUp(self) -> None:
        super().setUp()
        self._saved_env = {}
        for key in list(os.environ):
            if key.startswith(("SCION_", "OTEL_", "GROK_")):
                self._saved_env[key] = os.environ.pop(key)

    def tearDown(self) -> None:
        for key in list(os.environ):
            if key.startswith(("SCION_", "OTEL_", "GROK_")):
                os.environ.pop(key, None)
        os.environ.update(self._saved_env)
        super().tearDown()


class TelemetryEnabledTest(unittest.TestCase):
    """Tests for the _telemetry_enabled helper."""

    def test_none_returns_false(self) -> None:
        self.assertFalse(provision._telemetry_enabled(None))

    def test_empty_dict_returns_false(self) -> None:
        self.assertFalse(provision._telemetry_enabled({}))

    def test_enabled_true(self) -> None:
        self.assertTrue(provision._telemetry_enabled({"enabled": True}))

    def test_enabled_none_defaults_true(self) -> None:
        self.assertTrue(provision._telemetry_enabled({"enabled": None}))

    def test_enabled_false(self) -> None:
        self.assertFalse(provision._telemetry_enabled({"enabled": False}))


class BuildTelemetryEnvTest(BaseTelemetryTest):
    """Tests for _build_telemetry_env."""

    def test_defaults_point_to_local_grpc_receiver(self) -> None:
        env = provision._build_telemetry_env({"enabled": True}, None)
        self.assertEqual(env["GROK_TELEMETRY_ENABLED"], "true")
        self.assertEqual(env["GROK_EXTERNAL_OTEL"], "true")
        self.assertEqual(env["OTEL_EXPORTER_OTLP_ENDPOINT"], "http://localhost:4317")
        self.assertEqual(env["OTEL_EXPORTER_OTLP_PROTOCOL"], "grpc")
        self.assertEqual(env["OTEL_METRICS_EXPORTER"], "otlp")
        self.assertEqual(env["OTEL_LOGS_EXPORTER"], "otlp")
        self.assertEqual(env["OTEL_METRIC_EXPORT_INTERVAL"], "30000")

    def test_cloud_endpoint_override(self) -> None:
        telemetry = {
            "enabled": True,
            "cloud": {
                "endpoint": "https://otel.example.com:4317",
                "protocol": "http",
            },
        }
        env = provision._build_telemetry_env(telemetry, None)
        self.assertEqual(env["OTEL_EXPORTER_OTLP_ENDPOINT"], "https://otel.example.com:4317")
        self.assertEqual(env["OTEL_EXPORTER_OTLP_PROTOCOL"], "http")

    def test_env_override_takes_precedence(self) -> None:
        telemetry = {
            "enabled": True,
            "cloud": {
                "endpoint": "http://cloud-collector:4317",
                "protocol": "grpc",
            },
        }
        env_overlay = {
            "SCION_GROK_BUILD_OTEL_ENDPOINT": "http://custom-collector:4317",
            "SCION_GROK_BUILD_OTEL_PROTOCOL": "http",
        }
        env = provision._build_telemetry_env(telemetry, env_overlay)
        self.assertEqual(env["OTEL_EXPORTER_OTLP_ENDPOINT"], "http://custom-collector:4317")
        self.assertEqual(env["OTEL_EXPORTER_OTLP_PROTOCOL"], "http")

    def test_scion_otel_endpoint_fallback(self) -> None:
        env_overlay = {"SCION_OTEL_ENDPOINT": "http://scion-collector:4317"}
        env = provision._build_telemetry_env({"enabled": True}, env_overlay)
        self.assertEqual(env["OTEL_EXPORTER_OTLP_ENDPOINT"], "http://scion-collector:4317")

    def test_headers_propagated_and_percent_encoded(self) -> None:
        telemetry = {
            "enabled": True,
            "cloud": {
                "headers": {"authorization": "Bearer tok", "x-meta": "val"},
            },
        }
        env = provision._build_telemetry_env(telemetry, None)
        self.assertIn("OTEL_EXPORTER_OTLP_HEADERS", env)
        self.assertEqual(
            env["OTEL_EXPORTER_OTLP_HEADERS"],
            "authorization=Bearer%20tok,x-meta=val",
        )

    def test_tls_ca_file_propagated(self) -> None:
        telemetry = {
            "enabled": True,
            "cloud": {
                "tls": {"ca_file": "/etc/scion/ca.pem"},
            },
        }
        env = provision._build_telemetry_env(telemetry, None)
        self.assertEqual(env["OTEL_EXPORTER_OTLP_CERTIFICATE"], "/etc/scion/ca.pem")

    def test_no_headers_when_absent(self) -> None:
        env = provision._build_telemetry_env({"enabled": True}, None)
        self.assertNotIn("OTEL_EXPORTER_OTLP_HEADERS", env)
        self.assertNotIn("OTEL_EXPORTER_OTLP_CERTIFICATE", env)


class ResolveEndpointTest(BaseTelemetryTest):
    """Tests for _resolve_endpoint."""

    def test_default(self) -> None:
        self.assertEqual(provision._resolve_endpoint(None, None), "http://localhost:4317")

    def test_cloud_config(self) -> None:
        telemetry = {"cloud": {"endpoint": "https://collector:443"}}
        self.assertEqual(provision._resolve_endpoint(telemetry, None), "https://collector:443")

    def test_grok_env_override_wins(self) -> None:
        telemetry = {"cloud": {"endpoint": "https://collector:443"}}
        env = {"SCION_GROK_BUILD_OTEL_ENDPOINT": "http://custom:4317"}
        self.assertEqual(provision._resolve_endpoint(telemetry, env), "http://custom:4317")

    def test_grok_env_takes_precedence_over_scion_env(self) -> None:
        env = {
            "SCION_GROK_BUILD_OTEL_ENDPOINT": "http://grok-specific:4317",
            "SCION_OTEL_ENDPOINT": "http://generic-scion:4317",
        }
        self.assertEqual(provision._resolve_endpoint(None, env), "http://grok-specific:4317")

    def test_scion_env_fallback(self) -> None:
        env = {"SCION_OTEL_ENDPOINT": "http://scion:4317"}
        self.assertEqual(provision._resolve_endpoint(None, env), "http://scion:4317")


class ResolveProtocolTest(BaseTelemetryTest):
    """Tests for _resolve_protocol."""

    def test_default(self) -> None:
        self.assertEqual(provision._resolve_protocol(None, None), "grpc")

    def test_cloud_config(self) -> None:
        telemetry = {"cloud": {"protocol": "http"}}
        self.assertEqual(provision._resolve_protocol(telemetry, None), "http")

    def test_grok_env_override_wins(self) -> None:
        telemetry = {"cloud": {"protocol": "http"}}
        env = {"SCION_GROK_BUILD_OTEL_PROTOCOL": "grpc"}
        self.assertEqual(provision._resolve_protocol(telemetry, env), "grpc")

    def test_scion_env_fallback(self) -> None:
        env = {"SCION_OTEL_PROTOCOL": "http"}
        self.assertEqual(provision._resolve_protocol(None, env), "http")


class ResolveEndpointOsEnvTest(BaseTelemetryTest):
    """Tests for _resolve_endpoint os.environ fallback."""

    def test_os_environ_fallback(self) -> None:
        os.environ["SCION_OTEL_ENDPOINT"] = "http://from-os-env:4317"
        self.assertEqual(
            provision._resolve_endpoint(None, {}), "http://from-os-env:4317"
        )

    def test_grok_os_environ_takes_precedence(self) -> None:
        os.environ["SCION_GROK_BUILD_OTEL_ENDPOINT"] = "http://grok-os:4317"
        os.environ["SCION_OTEL_ENDPOINT"] = "http://generic-os:4317"
        self.assertEqual(
            provision._resolve_endpoint(None, {}), "http://grok-os:4317"
        )

    def test_env_overlay_beats_os_environ(self) -> None:
        os.environ["SCION_GROK_BUILD_OTEL_ENDPOINT"] = "http://from-os-env:4317"
        env = {"SCION_GROK_BUILD_OTEL_ENDPOINT": "http://from-overlay:4317"}
        self.assertEqual(
            provision._resolve_endpoint(None, env), "http://from-overlay:4317"
        )


class HeadersEnvTest(BaseTelemetryTest):
    """Tests for headers resolution from env vars in _build_telemetry_env."""

    def test_headers_from_env_overlay(self) -> None:
        env_overlay = {
            "SCION_OTEL_HEADERS": json.dumps({"x-api-key": "secret123"}),
        }
        env = provision._build_telemetry_env({"enabled": True}, env_overlay)
        self.assertEqual(env["OTEL_EXPORTER_OTLP_HEADERS"], "x-api-key=secret123")

    def test_headers_from_os_environ(self) -> None:
        os.environ["SCION_OTEL_HEADERS"] = json.dumps(
            {"authorization": "Bearer tok"}
        )
        env = provision._build_telemetry_env({"enabled": True}, {})
        self.assertEqual(
            env["OTEL_EXPORTER_OTLP_HEADERS"], "authorization=Bearer%20tok"
        )

    def test_grok_headers_env_takes_precedence(self) -> None:
        env_overlay = {
            "SCION_GROK_BUILD_OTEL_HEADERS": json.dumps({"x-grok": "1"}),
            "SCION_OTEL_HEADERS": json.dumps({"x-generic": "2"}),
        }
        env = provision._build_telemetry_env({"enabled": True}, env_overlay)
        self.assertEqual(env["OTEL_EXPORTER_OTLP_HEADERS"], "x-grok=1")

    def test_headers_env_beats_cloud_config(self) -> None:
        telemetry = {
            "enabled": True,
            "cloud": {"headers": {"x-cloud": "from-config"}},
        }
        env_overlay = {
            "SCION_OTEL_HEADERS": json.dumps({"x-env": "from-env"}),
        }
        env = provision._build_telemetry_env(telemetry, env_overlay)
        self.assertEqual(env["OTEL_EXPORTER_OTLP_HEADERS"], "x-env=from-env")

    def test_invalid_json_falls_back_to_cloud(self) -> None:
        telemetry = {
            "enabled": True,
            "cloud": {"headers": {"x-cloud": "val"}},
        }
        env_overlay = {"SCION_OTEL_HEADERS": "not-json"}
        env = provision._build_telemetry_env(telemetry, env_overlay)
        self.assertEqual(env["OTEL_EXPORTER_OTLP_HEADERS"], "x-cloud=val")


class CaFileEnvTest(BaseTelemetryTest):
    """Tests for TLS CA file resolution from env vars."""

    def test_ca_file_from_env_overlay(self) -> None:
        env_overlay = {"SCION_OTEL_CA_FILE": "/custom/ca.pem"}
        env = provision._build_telemetry_env({"enabled": True}, env_overlay)
        self.assertEqual(env["OTEL_EXPORTER_OTLP_CERTIFICATE"], "/custom/ca.pem")

    def test_ca_file_from_os_environ(self) -> None:
        os.environ["SCION_OTEL_CA_FILE"] = "/os-env/ca.pem"
        env = provision._build_telemetry_env({"enabled": True}, {})
        self.assertEqual(env["OTEL_EXPORTER_OTLP_CERTIFICATE"], "/os-env/ca.pem")

    def test_grok_ca_file_takes_precedence(self) -> None:
        env_overlay = {
            "SCION_GROK_BUILD_OTEL_CA_FILE": "/grok/ca.pem",
            "SCION_OTEL_CA_FILE": "/generic/ca.pem",
        }
        env = provision._build_telemetry_env({"enabled": True}, env_overlay)
        self.assertEqual(env["OTEL_EXPORTER_OTLP_CERTIFICATE"], "/grok/ca.pem")

    def test_no_ca_file_when_absent(self) -> None:
        env = provision._build_telemetry_env({"enabled": True}, {})
        self.assertNotIn("OTEL_EXPORTER_OTLP_CERTIFICATE", env)


if __name__ == "__main__":
    unittest.main()
