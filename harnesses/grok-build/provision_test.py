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
import tomllib
import unittest
from contextlib import contextmanager
from typing import Any
from unittest import mock

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


@contextmanager
def _also_strip_sections_damaging(header: str):
    """Force real damage for backstop/upper-bound tests (ptone/scion#2427
    review round 2, R2-b): monkeypatch scion_harness.strip_toml_sections so
    every call also (wrongly) strips the top-level section `header` (an
    exact "[table]" string) in addition to whatever the real predicate
    matches, simulating a bug in the line-oriented strip that corrupts
    something it doesn't own. Callers assert the resulting write is
    rejected (or, for _write_vertex_config/_write_vertex_model_alias,
    raises) — a managed_keys set that's too wide, or a key-path wrongly
    reverted to a bare top-level key, would make the check (wrongly) accept
    the damage instead."""
    real_strip = scion_harness.strip_toml_sections

    def damaging(content, predicate):
        content = real_strip(content, predicate)
        return real_strip(content, lambda h, hdr=header: h == hdr)

    with mock.patch.object(scion_harness, "strip_toml_sections", side_effect=damaging):
        yield


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

    _old_grok_home: str | None

    def setUp(self) -> None:
        super().setUp()
        self._old_grok_home = os.environ.pop("GROK_HOME", None)

    def tearDown(self) -> None:
        if self._old_grok_home is not None:
            os.environ["GROK_HOME"] = self._old_grok_home
        else:
            os.environ.pop("GROK_HOME", None)
        super().tearDown()

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

    _old_grok_home: str | None

    def setUp(self) -> None:
        super().setUp()
        self._old_grok_home = os.environ.pop("GROK_HOME", None)

    def tearDown(self) -> None:
        if self._old_grok_home is not None:
            os.environ["GROK_HOME"] = self._old_grok_home
        else:
            os.environ.pop("GROK_HOME", None)
        super().tearDown()

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

    _old_grok_home: str | None

    def setUp(self) -> None:
        super().setUp()
        self._old_grok_home = os.environ.pop("GROK_HOME", None)

    def tearDown(self) -> None:
        if self._old_grok_home is not None:
            os.environ["GROK_HOME"] = self._old_grok_home
        else:
            os.environ.pop("GROK_HOME", None)
        super().tearDown()

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

    def test_write_mcp_toml_preserves_cli_and_features_overlay(self) -> None:
        # Happy-path sanity check: [cli]/[features] survive a normal write.
        # This alone does NOT pin managed_keys={"mcp_servers"} — the strip
        # never touches those tables regardless of what's declared as
        # managed, so widening managed_keys to also cover them (M10 in the
        # review) would pass this test too (ptone/scion#2427 review round 2,
        # R2-b). See
        # test_write_mcp_toml_rejects_write_that_also_damages_overlay_table
        # below for the test that actually pins the upper bound.
        with tempfile.TemporaryDirectory() as tmp:
            ctx = _make_ctx()
            with temporary_home(tmp):
                grok_dir = os.path.join(tmp, ".grok")
                os.makedirs(grok_dir, exist_ok=True)
                config_path = os.path.join(grok_dir, "config.toml")
                with open(config_path, "w") as f:
                    f.write('[cli]\nauto_update = false\n\n[features]\ntelemetry = false\n')
                provision._write_mcp_toml(ctx, {"new-server": {"command": "new-cmd"}})
                with open(config_path, "rb") as f:
                    data = tomllib.load(f)
        self.assertEqual(data["cli"], {"auto_update": False})
        self.assertEqual(data["features"], {"telemetry": False})
        self.assertEqual(data["mcp_servers"]["new-server"]["command"], "new-cmd")

    def test_write_mcp_toml_rejects_write_that_also_damages_overlay_table(self) -> None:
        # ptone/scion#2427 review round 2 (R2-b): force real damage — strip
        # is patched to also (wrongly) drop [cli] — and assert
        # write_toml_if_preserves' managed_keys={"mcp_servers"} check
        # rejects the resulting damage. Widening managed_keys to also cover
        # "cli" (M10 in the review) would make this test fail, since the
        # check would then (wrongly) accept the damage.
        original = '[cli]\nauto_update = false\n\n[features]\ntelemetry = false\n'
        with tempfile.TemporaryDirectory() as tmp:
            ctx = _make_ctx()
            with temporary_home(tmp):
                grok_dir = os.path.join(tmp, ".grok")
                os.makedirs(grok_dir, exist_ok=True)
                config_path = os.path.join(grok_dir, "config.toml")
                with open(config_path, "w") as f:
                    f.write(original)
                warnings: list[str] = []
                ctx.warn = warnings.append  # type: ignore[method-assign]
                with _also_strip_sections_damaging("[cli]"):
                    provision._write_mcp_toml(ctx, {"new-server": {"command": "new-cmd"}})
                with open(config_path) as f:
                    after = f.read()
        self.assertEqual(after, original, "file must be left untouched when the edit would drop an unmanaged table")
        self.assertEqual(len(warnings), 1)

    def test_write_mcp_toml_strips_stale_server_alongside_multiline_string_with_brackets(self) -> None:
        # Regression test for ptone/scion#2427 review round 1 (R1): an
        # unbalanced bracket in a multi-line string ("use [brackets like
        # this") used to corrupt strip_toml_sections' depth tracking for
        # the rest of the file, so the stale [mcp_servers.old] section
        # below it was never recognized as a header and survived the
        # write untouched.
        with tempfile.TemporaryDirectory() as tmp:
            ctx = _make_ctx()
            with temporary_home(tmp):
                grok_dir = os.path.join(tmp, ".grok")
                os.makedirs(grok_dir, exist_ok=True)
                config_path = os.path.join(grok_dir, "config.toml")
                with open(config_path, "w") as f:
                    f.write(
                        'instructions = """\n'
                        "use [brackets like this\n"
                        '"""\n'
                        '[mcp_servers.old]\ncommand = "old-cmd"\n'
                    )
                provision._write_mcp_toml(ctx, {"new-server": {"command": "new-cmd"}})
                # Run again to simulate a container restart (pre-start hooks
                # re-run provision.py every start) — the reviewer's repro
                # showed every write after the first being rejected once
                # depth tracking was corrupted.
                provision._write_mcp_toml(ctx, {"newer-server": {"command": "newer-cmd"}})
                with open(config_path, "rb") as f:
                    data = tomllib.load(f)
        self.assertNotIn("old", data["mcp_servers"])
        self.assertNotIn("new-server", data["mcp_servers"])
        self.assertEqual(data["mcp_servers"]["newer-server"]["command"], "newer-cmd")
        self.assertEqual(data["instructions"], "use [brackets like this\n")

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

    def test_write_mcp_toml_strips_old_section_with_trailing_comment(self) -> None:
        # Repro case 1 from generalization-findings.md: a header with a
        # trailing comment (e.g. hand-edited) must still be recognized, or
        # re-writing the same table produces a duplicate
        # ('Cannot declare... twice').
        with tempfile.TemporaryDirectory() as tmp:
            ctx = _make_ctx()
            with temporary_home(tmp):
                grok_dir = os.path.join(tmp, ".grok")
                os.makedirs(grok_dir, exist_ok=True)
                config_path = os.path.join(grok_dir, "config.toml")
                with open(config_path, "w") as f:
                    f.write('[mcp_servers.foo] # added by user\ncommand = "old-cmd"\n')
                provision._write_mcp_toml(ctx, {"foo": {"command": "new-cmd"}})
                with open(config_path, "rb") as f:
                    data = tomllib.load(f)
        self.assertEqual(data["mcp_servers"]["foo"]["command"], "new-cmd")

    def test_write_mcp_toml_leaves_file_untouched_when_edit_would_corrupt_unmanaged_content(self) -> None:
        # write_toml_if_preserves backstop must not silently write a
        # corrupting edit. Originally used a header-shaped line, then an
        # escaped closing-delimiter sequence, inside a multi-line string to
        # trigger this; both are now correctly handled (ptone/scion#2427
        # review rounds 1 and 2). Per review round 2 (R2-b), this now forces
        # real damage instead: strip_toml_sections is patched to also
        # (wrongly) drop [other].
        original = 'notes = "keep me"\n[other]\nk = 1\n'
        self.assertEqual(tomllib.loads(original)["other"]["k"], 1, "sanity: original must be valid TOML")
        with tempfile.TemporaryDirectory() as tmp:
            ctx = _make_ctx()
            with temporary_home(tmp):
                grok_dir = os.path.join(tmp, ".grok")
                os.makedirs(grok_dir, exist_ok=True)
                config_path = os.path.join(grok_dir, "config.toml")
                with open(config_path, "w") as f:
                    f.write(original)
                warnings: list[str] = []
                ctx.warn = warnings.append  # type: ignore[method-assign]
                with _also_strip_sections_damaging("[other]"):
                    provision._write_mcp_toml(ctx, {"real": {"command": "x"}})
                with open(config_path) as f:
                    after = f.read()
        self.assertEqual(after, original, "file must be left untouched when the edit doesn't preserve content")
        self.assertEqual(len(warnings), 1)


# ---------------------------------------------------------------------------
# Config Hardening Tests
# ---------------------------------------------------------------------------


class ConfigHardeningTest(unittest.TestCase):
    """Test config hardening writes correct TOML."""

    _old_grok_home: str | None

    def setUp(self) -> None:
        super().setUp()
        self._old_grok_home = os.environ.pop("GROK_HOME", None)

    def tearDown(self) -> None:
        if self._old_grok_home is not None:
            os.environ["GROK_HOME"] = self._old_grok_home
        else:
            os.environ.pop("GROK_HOME", None)
        super().tearDown()

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

    def test_hardening_preserves_overlay_table_next_to_managed_keys(self) -> None:
        # Happy-path sanity check: an unrelated overlay table (here [tools],
        # not one of "cli"/"features"/"memory"/"subagents") survives a
        # normal hardening write. This alone does NOT pin managed_keys — the
        # line-oriented strip never touches [tools] regardless of what's
        # declared as managed, so a managed_keys set widened to also cover
        # "tools" would pass this test too (ptone/scion#2427 review round 2,
        # R2-b). See
        # test_hardening_rejects_write_that_also_damages_overlay_table below
        # for the test that actually pins the upper bound, by forcing real
        # damage and asserting it gets rejected.
        with tempfile.TemporaryDirectory() as tmp:
            ctx = _make_ctx()
            with temporary_home(tmp):
                grok_dir = os.path.join(tmp, ".grok")
                os.makedirs(grok_dir, exist_ok=True)
                config_path = os.path.join(grok_dir, "config.toml")
                with open(config_path, "w") as f:
                    f.write('[cli]\nauto_update = true\n\n[tools]\ncustom_flag = "keep-me"\n')
                provision._harden_config(ctx)
                with open(config_path, "rb") as f:
                    data = tomllib.load(f)
        self.assertEqual(data["tools"], {"custom_flag": "keep-me"})
        self.assertEqual(data["cli"]["auto_update"], False)

    def test_hardening_rejects_write_that_also_damages_overlay_table(self) -> None:
        # ptone/scion#2427 review round 2 (R2-b): force real damage — strip
        # is patched to also (wrongly) drop [tools] — and assert
        # write_toml_if_preserves' managed_keys=
        # {"cli","features","memory","subagents"} check rejects the
        # resulting damage. Widening managed_keys to also cover "tools" (M9
        # in the review) would make this test fail, since the check would
        # then (wrongly) accept the damage.
        original = '[cli]\nauto_update = true\n\n[tools]\ncustom_flag = "keep-me"\n'
        with tempfile.TemporaryDirectory() as tmp:
            ctx = _make_ctx()
            with temporary_home(tmp):
                grok_dir = os.path.join(tmp, ".grok")
                os.makedirs(grok_dir, exist_ok=True)
                config_path = os.path.join(grok_dir, "config.toml")
                with open(config_path, "w") as f:
                    f.write(original)
                warnings: list[str] = []
                ctx.warn = warnings.append  # type: ignore[method-assign]
                with _also_strip_sections_damaging("[tools]"):
                    provision._harden_config(ctx)
                with open(config_path) as f:
                    after = f.read()
        self.assertEqual(after, original, "file must be left untouched when the edit would drop an unmanaged table")
        self.assertEqual(len(warnings), 1)

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

    def test_hardening_handles_template_overlay_section_with_trailing_comment(self) -> None:
        # Regression test for the real bug reproduced in
        # generalization-findings.md ("grok real" row): a template-home
        # overlay's ~/.grok/config.toml can legitimately contain
        # `[features] # <comment>` (e.g. hand-annotated or copied from
        # another config). The old naive strip_toml_sections didn't
        # recognize the commented header, so appending the hardening
        # block's own `[features]` table produced invalid TOML
        # ("Cannot declare ('features',) twice"). It must now produce a
        # single, valid [features] table.
        with tempfile.TemporaryDirectory() as tmp:
            ctx = _make_ctx()
            with temporary_home(tmp):
                grok_dir = os.path.join(tmp, ".grok")
                os.makedirs(grok_dir, exist_ok=True)
                config_path = os.path.join(grok_dir, "config.toml")
                with open(config_path, "w") as f:
                    f.write("[features] # template overlay\ntelemetry = true\n")
                provision._harden_config(ctx)
                with open(config_path, "rb") as f:
                    data = tomllib.load(f)
        self.assertEqual(data["features"]["telemetry"], False)
        self.assertNotIn("BEGIN SCION MANAGED", data)

    def test_hardening_and_mcp_survive_escaped_delimiter_on_the_opening_line(self) -> None:
        # Regression test for ptone/scion#2427 review round 4 (R4-1): an
        # escaped closing-delimiter sequence on the *same line that opens*
        # a multi-line basic string used to close the string early even
        # after review round 2's escape fix (that fix only covered a string
        # already open when a line starts). On this input, hardening never
        # applied and MCP registration was rejected on every restart after
        # the first — main handled it correctly. Assert hardening applies
        # on first contact, and that MCP registration on a *second* pass
        # (simulating a container restart) is accepted with no duplicate,
        # not rejected.
        with tempfile.TemporaryDirectory() as tmp:
            ctx = _make_ctx()
            with temporary_home(tmp):
                grok_dir = os.path.join(tmp, ".grok")
                os.makedirs(grok_dir, exist_ok=True)
                config_path = os.path.join(grok_dir, "config.toml")
                with open(config_path, "w") as f:
                    f.write('notes = """foo \\""" bar\n[fake]\n"""\n')

                warnings: list[str] = []
                ctx.warn = warnings.append  # type: ignore[method-assign]

                provision._harden_config(ctx)
                provision._write_mcp_toml(ctx, {"srv": {"command": "x"}})
                # Second pass, simulating a restart.
                provision._write_mcp_toml(ctx, {"srv": {"command": "y"}})

                with open(config_path, "rb") as f:
                    data = tomllib.load(f)
        self.assertEqual(warnings, [], f"expected no rejected writes, got: {warnings}")
        self.assertEqual(data["cli"]["auto_update"], False, "hardening must be applied")
        self.assertEqual(data["notes"], 'foo """ bar\n[fake]\n', "the string content must survive intact")
        self.assertEqual(
            data["mcp_servers"], {"srv": {"command": "y"}}, "second pass must replace, not duplicate, the server"
        )

    def test_hardening_and_mcp_survive_extra_quote_before_closing_delimiter(self) -> None:
        # Regression test for ptone/scion#2427 review round 5 (R5-1): 1-2
        # extra content quotes immediately before a multi-line string's
        # closing delimiter (`""""`, valid TOML for content ending in a
        # literal quote) used to leave a leftover quote character that
        # mis-paired with the next quote on the line, miscounting the `[`
        # inside it as real structure. On this input, hardening never
        # applied and MCP registration was stuck at its first-pass value on
        # every later restart — main handled it correctly. Assert hardening
        # applies, and MCP registration on a second pass is accepted with
        # no duplicate, not rejected.
        with tempfile.TemporaryDirectory() as tmp:
            ctx = _make_ctx()
            with temporary_home(tmp):
                grok_dir = os.path.join(tmp, ".grok")
                os.makedirs(grok_dir, exist_ok=True)
                config_path = os.path.join(grok_dir, "config.toml")
                with open(config_path, "w") as f:
                    f.write('banned = ["""say "hi"""", "[x"]\n')

                warnings: list[str] = []
                ctx.warn = warnings.append  # type: ignore[method-assign]

                provision._harden_config(ctx)
                provision._write_mcp_toml(ctx, {"srv": {"command": "x"}})
                # Second pass, simulating a restart.
                provision._write_mcp_toml(ctx, {"srv": {"command": "y"}})

                with open(config_path, "rb") as f:
                    data = tomllib.load(f)
        self.assertEqual(warnings, [], f"expected no rejected writes, got: {warnings}")
        self.assertEqual(data["cli"]["auto_update"], False, "hardening must be applied")
        self.assertEqual(data["banned"], ['say "hi"', "[x"], "the string content must survive intact")
        self.assertEqual(
            data["mcp_servers"], {"srv": {"command": "y"}}, "second pass must replace, not duplicate, the server"
        )

    def test_hardening_leaves_file_untouched_when_edit_would_corrupt_unmanaged_content(self) -> None:
        # write_toml_if_preserves backstop: a rejected edit must leave the
        # original file on disk untouched. Originally used a header-shaped
        # line, then an escaped closing-delimiter sequence, inside a
        # multi-line string to trigger this; both are now correctly handled
        # (ptone/scion#2427 review rounds 1 and 2). Per review round 2
        # (R2-b), this now forces real damage instead: strip_toml_sections
        # is patched to also (wrongly) drop [other].
        original = 'notes = "keep me"\n[other]\nk = 1\n'
        self.assertEqual(tomllib.loads(original)["other"]["k"], 1, "sanity: original must be valid TOML")
        with tempfile.TemporaryDirectory() as tmp:
            ctx = _make_ctx()
            with temporary_home(tmp):
                grok_dir = os.path.join(tmp, ".grok")
                os.makedirs(grok_dir, exist_ok=True)
                config_path = os.path.join(grok_dir, "config.toml")
                with open(config_path, "w") as f:
                    f.write(original)
                warnings: list[str] = []
                ctx.warn = warnings.append  # type: ignore[method-assign]
                with _also_strip_sections_damaging("[other]"):
                    provision._harden_config(ctx)
                with open(config_path) as f:
                    after = f.read()
        self.assertEqual(after, original, "file must be left untouched when the edit doesn't preserve content")
        self.assertEqual(len(warnings), 1)


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
        """Exercise the shared scion_harness.resolve_model helper, the way
        provision() now does for the non-vertex-ai path."""
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
                    "medium": "grok-4.5",
                    "large": "grok-4.6",
                    "extra-large": "grok-4.6",
                },
            },
        })
        return scion_harness.resolve_model(ctx)

    def test_small_alias_resolves_to_grok_3_mini(self) -> None:
        self.assertEqual(self._resolve("small"), "grok-3-mini")

    def test_medium_alias_resolves_to_grok_4_5(self) -> None:
        self.assertEqual(self._resolve("medium"), "grok-4.5")

    def test_large_alias_resolves_to_grok_4_6(self) -> None:
        self.assertEqual(self._resolve("large"), "grok-4.6")

    def test_extra_large_alias_resolves_to_grok_4_6(self) -> None:
        self.assertEqual(self._resolve("extra-large"), "grok-4.6")

    def test_raw_model_name_passes_through(self) -> None:
        self.assertEqual(self._resolve("grok-4-turbo"), "grok-4-turbo")

    def test_empty_scion_model_returns_empty(self) -> None:
        self.assertEqual(self._resolve(""), "")

    def test_alias_is_case_insensitive(self) -> None:
        self.assertEqual(self._resolve("SMALL"), "grok-3-mini")
        self.assertEqual(self._resolve("Large"), "grok-4.6")

    def test_shorthand_letters_now_expand_to_tiers(self) -> None:
        """Behavior difference from the pre-G3 lowercase-only lookup: that
        code did `aliases.get(raw.lower(), raw)` with no shorthand table, so
        a bare "s"/"m"/"l"/"xl" never matched a model_aliases key and passed
        straight through as a literal (invalid) model name. The shared
        resolve_model helper expands shorthand the same way Go's
        NormalizeModelAlias does, so these now resolve correctly.
        """
        self.assertEqual(self._resolve("s"), "grok-3-mini")
        self.assertEqual(self._resolve("m"), "grok-4.5")
        self.assertEqual(self._resolve("l"), "grok-4.6")
        self.assertEqual(self._resolve("xl"), "grok-4.6")

    def test_concrete_model_case_is_preserved(self) -> None:
        """Not a behavior difference: the pre-G3 lookup's fallback was `raw`
        (original case), and the shared resolve_model helper also returns
        the caller's original spelling for a concrete (non-tier) name — see
        R1 in the round-1 review. Only the four canonical tiers are
        normalized for the alias-table lookup.
        """
        self.assertEqual(self._resolve("Grok-4-Turbo"), "Grok-4-Turbo")

    def test_provision_writes_resolved_model_to_grok_default_model_env(self) -> None:
        """N2 of the round-1 review: end-to-end check that the non-vertex-ai
        path in provision() actually wires scion_harness.resolve_model's
        result into GROK_DEFAULT_MODEL in env.json, not just that the helper
        itself resolves correctly in isolation.
        """
        with tempfile.TemporaryDirectory() as tmp:
            home = os.path.join(tmp, "home")
            bundle = os.path.join(tmp, "bundle")
            os.makedirs(home)
            os.makedirs(os.path.join(bundle, "outputs"))
            ctx = _make_ctx({
                "harness_bundle_dir": bundle,
                "harness_config": {
                    "no_auth": {"behavior": "drop-to-shell"},
                    "instructions_file": "AGENTS.md",
                    "skills_dir": ".grok/skills",
                    "system_prompt_mode": "prepend_to_instructions",
                    "model_aliases": {
                        "small": "grok-3-mini",
                        "medium": "grok-4.5",
                        "large": "grok-4.6",
                        "extra-large": "grok-4.6",
                    },
                },
            })
            os.environ["SCION_MODEL"] = "m"
            try:
                with temporary_home(home):
                    provision.provision(ctx)
            finally:
                os.environ.pop("SCION_MODEL", None)

            with open(os.path.join(bundle, "outputs", "env.json")) as f:
                env = json.load(f)
        self.assertEqual(env["GROK_DEFAULT_MODEL"], "grok-4.5")


# ---------------------------------------------------------------------------
# Hook Write Tests
# ---------------------------------------------------------------------------


class HookWriteTest(unittest.TestCase):
    """Test _write_hooks produces correct JSON structure."""

    _old_grok_home: str | None

    def setUp(self) -> None:
        super().setUp()
        self._old_grok_home = os.environ.pop("GROK_HOME", None)

    def tearDown(self) -> None:
        if self._old_grok_home is not None:
            os.environ["GROK_HOME"] = self._old_grok_home
        else:
            os.environ.pop("GROK_HOME", None)
        super().tearDown()

    def test_hooks_written_correctly(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            ctx = _make_ctx()
            with temporary_home(tmp):
                provision._write_hooks(ctx)
            hooks_path = os.path.join(tmp, ".grok", "hooks", "scion.json")
            self.assertTrue(os.path.isfile(hooks_path))
            with open(hooks_path) as f:
                data = json.load(f)
            hooks = data["hooks"]
            # Check all expected events present.
            for event in provision._GROK_HOOK_EVENTS:
                self.assertIn(event, hooks, f"missing hook event: {event}")

    def test_session_start_uses_echo_with_source(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            ctx = _make_ctx()
            with temporary_home(tmp):
                provision._write_hooks(ctx)
            hooks_path = os.path.join(tmp, ".grok", "hooks", "scion.json")
            with open(hooks_path) as f:
                data = json.load(f)
            session_start = data["hooks"]["SessionStart"]
            cmd = session_start[0]["hooks"][0]["command"]
            self.assertIn("echo", cmd)
            self.assertIn("SessionStart", cmd)
            self.assertIn("source", cmd)
            self.assertIn("sciontool hook --dialect=grok-build", cmd)

    def test_session_end_uses_echo_with_reason(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            ctx = _make_ctx()
            with temporary_home(tmp):
                provision._write_hooks(ctx)
            hooks_path = os.path.join(tmp, ".grok", "hooks", "scion.json")
            with open(hooks_path) as f:
                data = json.load(f)
            session_end = data["hooks"]["SessionEnd"]
            cmd = session_end[0]["hooks"][0]["command"]
            self.assertIn("echo", cmd)
            self.assertIn("SessionEnd", cmd)
            self.assertIn("reason", cmd)

    def test_pre_tool_use_uses_cat(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            ctx = _make_ctx()
            with temporary_home(tmp):
                provision._write_hooks(ctx)
            hooks_path = os.path.join(tmp, ".grok", "hooks", "scion.json")
            with open(hooks_path) as f:
                data = json.load(f)
            pre_tool = data["hooks"]["PreToolUse"]
            cmd = pre_tool[0]["hooks"][0]["command"]
            self.assertTrue(cmd.startswith("cat |"))
            self.assertIn("sciontool hook --dialect=grok-build", cmd)

    def test_stop_has_longer_timeout(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            ctx = _make_ctx()
            with temporary_home(tmp):
                provision._write_hooks(ctx)
            hooks_path = os.path.join(tmp, ".grok", "hooks", "scion.json")
            with open(hooks_path) as f:
                data = json.load(f)
            stop = data["hooks"]["Stop"]
            timeout = stop[0]["hooks"][0]["timeout"]
            self.assertEqual(timeout, 60)

    def test_subagent_stop_has_longer_timeout(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            ctx = _make_ctx()
            with temporary_home(tmp):
                provision._write_hooks(ctx)
            hooks_path = os.path.join(tmp, ".grok", "hooks", "scion.json")
            with open(hooks_path) as f:
                data = json.load(f)
            subagent_stop = data["hooks"]["SubagentStop"]
            timeout = subagent_stop[0]["hooks"][0]["timeout"]
            self.assertEqual(timeout, 60)


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
        env = provision._build_telemetry_env(None)
        self.assertEqual(env["GROK_TELEMETRY_ENABLED"], "true")
        self.assertEqual(env["GROK_EXTERNAL_OTEL"], "true")
        self.assertEqual(env["OTEL_EXPORTER_OTLP_ENDPOINT"], "http://127.0.0.1:4317")
        self.assertEqual(env["OTEL_EXPORTER_OTLP_PROTOCOL"], "grpc")
        self.assertEqual(env["OTEL_METRICS_EXPORTER"], "otlp")
        self.assertEqual(env["OTEL_LOGS_EXPORTER"], "otlp")
        self.assertEqual(env["OTEL_METRIC_EXPORT_INTERVAL"], "30000")

    def test_custom_port(self) -> None:
        env = provision._build_telemetry_env({"SCION_OTEL_GRPC_PORT": "14317"})
        self.assertEqual(env["OTEL_EXPORTER_OTLP_ENDPOINT"], "http://127.0.0.1:14317")

    def test_generic_scion_otel_endpoint_is_not_honored(self) -> None:
        """The generic SCION_OTEL_ENDPOINT cloud-config alias must no longer
        redirect native telemetry away from the local receiver (#2053)."""
        env_overlay = {"SCION_OTEL_ENDPOINT": "https://cloudtrace.googleapis.com:443"}
        env = provision._build_telemetry_env(env_overlay)
        self.assertEqual(env["OTEL_EXPORTER_OTLP_ENDPOINT"], "http://127.0.0.1:4317")

    def test_no_headers_or_ca_ever_set(self) -> None:
        """Headers and CA are never copied into the harness env; there is no
        cloud config left to copy them from."""
        env = provision._build_telemetry_env(None)
        self.assertNotIn("OTEL_EXPORTER_OTLP_HEADERS", env)
        self.assertNotIn("OTEL_EXPORTER_OTLP_CERTIFICATE", env)

    def test_debug_endpoint_override(self) -> None:
        env_overlay = {"SCION_GROK_BUILD_OTEL_ENDPOINT": "http://debug-collector:4317"}
        env = provision._build_telemetry_env(env_overlay)
        self.assertEqual(env["OTEL_EXPORTER_OTLP_ENDPOINT"], "http://debug-collector:4317")


class ResolveEndpointTest(BaseTelemetryTest):
    """Tests for _resolve_endpoint."""

    def test_default(self) -> None:
        self.assertEqual(provision._resolve_endpoint(None), "http://127.0.0.1:4317")

    def test_custom_port(self) -> None:
        self.assertEqual(
            provision._resolve_endpoint({"SCION_OTEL_GRPC_PORT": "9999"}),
            "http://127.0.0.1:9999",
        )

    def test_invalid_port_raises(self) -> None:
        with self.assertRaises(provision.scion_harness.ProvisionError):
            provision._resolve_endpoint({"SCION_OTEL_GRPC_PORT": "not-a-port"})

    def test_debug_override_wins(self) -> None:
        env = {"SCION_GROK_BUILD_OTEL_ENDPOINT": "http://custom:4317"}
        self.assertEqual(provision._resolve_endpoint(env), "http://custom:4317")

    def test_generic_scion_otel_endpoint_ignored(self) -> None:
        """Only the grok-specific override is honored; the generic cloud
        alias is not (that alias is exactly what caused the #2053 bypass)."""
        env = {"SCION_OTEL_ENDPOINT": "https://cloudtrace.googleapis.com:443"}
        self.assertEqual(provision._resolve_endpoint(env), "http://127.0.0.1:4317")


class ResolveEndpointOsEnvTest(BaseTelemetryTest):
    """Tests for _resolve_endpoint os.environ fallback."""

    def test_os_environ_debug_override(self) -> None:
        os.environ["SCION_GROK_BUILD_OTEL_ENDPOINT"] = "http://grok-os:4317"
        self.assertEqual(provision._resolve_endpoint({}), "http://grok-os:4317")

    def test_generic_scion_otel_endpoint_os_environ_ignored(self) -> None:
        os.environ["SCION_OTEL_ENDPOINT"] = "https://cloudtrace.googleapis.com:443"
        self.assertEqual(provision._resolve_endpoint({}), "http://127.0.0.1:4317")

    def test_env_overlay_beats_os_environ(self) -> None:
        os.environ["SCION_GROK_BUILD_OTEL_ENDPOINT"] = "http://from-os-env:4317"
        env = {"SCION_GROK_BUILD_OTEL_ENDPOINT": "http://from-overlay:4317"}
        self.assertEqual(provision._resolve_endpoint(env), "http://from-overlay:4317")


# ---------------------------------------------------------------------------
# Vertex config TOML writers: validate-before-write regression tests
# ---------------------------------------------------------------------------


class VertexConfigTomlWriteTest(unittest.TestCase):
    """Direct tests of _write_vertex_config / _write_vertex_model_alias's
    TOML editing, independent of vertex-ai auth selection/env resolution
    (covered by VertexAIAuthTest below)."""

    _old_grok_home: str | None

    def setUp(self) -> None:
        super().setUp()
        self._old_grok_home = os.environ.pop("GROK_HOME", None)

    def tearDown(self) -> None:
        if self._old_grok_home is not None:
            os.environ["GROK_HOME"] = self._old_grok_home
        else:
            os.environ.pop("GROK_HOME", None)
        super().tearDown()

    def test_write_vertex_config_strips_header_whitespace_variant(self) -> None:
        # Repro case 4 from generalization-findings.md: `[ models ]` (legal
        # TOML whitespace) must still be recognized by the exact-string
        # predicate `line == "[models]"`, or the stale table survives
        # alongside the freshly-appended one ('Cannot declare... twice').
        with tempfile.TemporaryDirectory() as tmp:
            ctx = _make_ctx()
            with temporary_home(tmp):
                grok_dir = os.path.join(tmp, ".grok")
                os.makedirs(grok_dir, exist_ok=True)
                config_path = os.path.join(grok_dir, "config.toml")
                with open(config_path, "w") as f:
                    f.write('[ models ]\ndefault = "stale"\n')
                provision._write_vertex_config(ctx, "https://example/v1", "xai/grok-4.6")
                with open(config_path, "rb") as f:
                    data = tomllib.load(f)
        self.assertEqual(data["models"]["default"], "vertex-grok")

    def test_write_vertex_config_raises_and_leaves_file_untouched_when_edit_would_corrupt_unmanaged_content(
        self,
    ) -> None:
        # A plain header-shaped line inside a well-formed multi-line string,
        # then an escaped closing-delimiter sequence, were both previously
        # used to trigger this; both are now correctly handled
        # (ptone/scion#2427 review rounds 1 and 2). Per review round 2
        # (R2-b), this now forces real damage instead — strip_toml_sections
        # is patched to also (wrongly) drop an unrelated sibling
        # [auth_provider.other] block — which also directly pins the
        # key-path scoping (M6 in the review): if the key-paths were ever
        # reverted to bare "auth_provider"/"model", this damage would fall
        # inside the exempted top-level "auth_provider" key and the test
        # would fail to see a raise.
        #
        # Per review round 1 (R2): a vertex-auth agent with no vertex config
        # is guaranteed broken, so a rejected write must fail loudly
        # (ProvisionError) rather than continue as if it had succeeded.
        original = '[auth_provider.other]\ncommand = "other-cmd"\n'
        self.assertIsNotNone(tomllib.loads(original), "sanity: original must be valid TOML")
        with tempfile.TemporaryDirectory() as tmp:
            ctx = _make_ctx()
            with temporary_home(tmp):
                grok_dir = os.path.join(tmp, ".grok")
                os.makedirs(grok_dir, exist_ok=True)
                config_path = os.path.join(grok_dir, "config.toml")
                with open(config_path, "w") as f:
                    f.write(original)
                warnings: list[str] = []
                ctx.warn = warnings.append  # type: ignore[method-assign]
                with _also_strip_sections_damaging("[auth_provider.other]"):
                    with self.assertRaises(scion_harness.ProvisionError):
                        provision._write_vertex_config(ctx, "https://example/v1", "xai/grok-4.6")
                with open(config_path) as f:
                    after = f.read()
        self.assertEqual(after, original, "file must be left untouched when the edit doesn't preserve content")
        self.assertEqual(len(warnings), 1)
        self.assertIn("vertex-ai auth/model config", warnings[0])

    def test_write_vertex_model_alias_raises_and_leaves_file_untouched_when_edit_would_corrupt_unmanaged_content(
        self,
    ) -> None:
        # A plain header-shaped line, then an escaped closing-delimiter
        # sequence, inside a multi-line string were both previously used to
        # trigger this; both are now correctly handled (ptone/scion#2427
        # review rounds 1 and 2). Per review round 2 (R2-b), this now forces
        # real damage instead — strip_toml_sections is patched to also
        # (wrongly) drop a sibling [model.custom] block — which also
        # directly pins the key-path scoping (M7 in the review): if
        # ("model", alias_name) were ever reverted to bare "model", this
        # damage would fall inside the exempted top-level "model" key and
        # the test would fail to see a raise.
        #
        # Per review round 2 (R2-a): the alias write is part of the same
        # guaranteed-broken-if-missing path as the primary vertex config, so
        # a rejected write must raise instead of warning and continuing
        # into a misleading success log.
        with tempfile.TemporaryDirectory() as tmp:
            ctx = _make_ctx()
            with temporary_home(tmp):
                grok_dir = os.path.join(tmp, ".grok")
                os.makedirs(grok_dir, exist_ok=True)
                config_path = os.path.join(grok_dir, "config.toml")
                provision._write_vertex_config(ctx, "https://example/v1", "xai/grok-4.6")
                with open(config_path) as f:
                    baseline = f.read()
                original = baseline + '\n[model.custom]\nurl = "keep-me"\n'
                self.assertIsNotNone(tomllib.loads(original), "sanity: original must be valid TOML")
                with open(config_path, "w") as f:
                    f.write(original)
                warnings: list[str] = []
                ctx.warn = warnings.append  # type: ignore[method-assign]
                with _also_strip_sections_damaging("[model.custom]"):
                    with self.assertRaises(scion_harness.ProvisionError):
                        provision._write_vertex_model_alias(ctx, "https://example/v1", "xai/grok-4.6", "grok-4.2")
                with open(config_path) as f:
                    after = f.read()
        self.assertEqual(after, original, "file must be left untouched when the edit doesn't preserve content")
        self.assertEqual(len(warnings), 1)
        self.assertIn("vertex-ai model alias 'grok-4.2'", warnings[0])

    def test_write_vertex_model_alias_accepts_dotted_bare_header(self) -> None:
        # ptone/scion#2427 review round 3 ("Consider", dotted bare alias
        # header): a hand-written *bare* dotted header like
        # "[model.grok-4.2]" is a *different* TOML path than the quoted
        # alias this function writes ([model."grok-4.2"]) — it parses as
        # model.grok-4."2", not model."grok-4.2". Before this fix, the
        # strip predicate matched the bare form unconditionally, so it
        # stripped that unrelated table; the ("model", alias_name) key-path
        # check then correctly saw an unmanaged change and raised
        # ProvisionError on every start. Since alias_name is not a valid
        # TOML bare key (it contains a dot), the bare-form predicate must
        # not be included at all: the unrelated table survives untouched,
        # the write is accepted, and the file stays valid.
        with tempfile.TemporaryDirectory() as tmp:
            ctx = _make_ctx()
            with temporary_home(tmp):
                grok_dir = os.path.join(tmp, ".grok")
                os.makedirs(grok_dir, exist_ok=True)
                config_path = os.path.join(grok_dir, "config.toml")
                with open(config_path, "w") as f:
                    f.write("[model.grok-4.2]\nmodel = \"old\"\n")

                provision._write_vertex_model_alias(ctx, "https://example/v1", "xai/grok-4.6", "grok-4.2")

                with open(config_path, "rb") as f:
                    data = tomllib.load(f)
        # The hand-written bare-dotted table (model.grok-4."2") survives,
        # untouched and distinct from the new quoted alias.
        self.assertEqual(data["model"]["grok-4"]["2"], {"model": "old"})
        self.assertEqual(data["model"]["grok-4.2"]["model"], "xai/grok-4.6")


# ---------------------------------------------------------------------------
# Vertex AI Auth Tests
# ---------------------------------------------------------------------------


class VertexAIAuthTest(unittest.TestCase):
    """Test Vertex AI auth selection and provisioning."""

    _saved_env: dict[str, str]

    def setUp(self) -> None:
        super().setUp()
        self._saved_env = {}
        for key in list(os.environ):
            if key.startswith(("GOOGLE_", "CLOUD_ML_", "SCION_MODEL", "SCION_METADATA_", "GROK_")):
                self._saved_env[key] = os.environ.pop(key)

    def tearDown(self) -> None:
        for key in list(os.environ):
            if key.startswith(("GOOGLE_", "CLOUD_ML_", "SCION_MODEL", "SCION_METADATA_", "GROK_")):
                os.environ.pop(key, None)
        os.environ.update(self._saved_env)
        super().tearDown()

    def test_vertex_ai_selected_when_project_present(self) -> None:
        """vertex-ai auth is selected when GOOGLE_CLOUD_PROJECT is in env_vars
        and neither XAI_API_KEY nor GROK_AUTH are present."""
        with tempfile.TemporaryDirectory() as tmp:
            inputs_dir = os.path.join(tmp, "inputs")
            os.makedirs(inputs_dir)
            secret_path = os.path.join(tmp, "project-id")
            with open(secret_path, "w") as f:
                f.write("my-gcp-project")
            scion_harness.atomic_write_json(
                os.path.join(inputs_dir, "auth-candidates.json"),
                {
                    "env_vars": ["GOOGLE_CLOUD_PROJECT"],
                    "env_secret_files": {
                        "GOOGLE_CLOUD_PROJECT": secret_path,
                    },
                    "file_secret_files": {},
                },
            )
            ctx = _make_ctx({"harness_bundle_dir": tmp})
            with temporary_home(tmp):
                resolved = ctx.select_auth(provision.AUTH)
            self.assertEqual(resolved.method, "vertex-ai")

    def test_vertex_config_written_to_config_toml(self) -> None:
        """When vertex-ai method is used, config.toml contains correct entries."""
        with tempfile.TemporaryDirectory() as tmp:
            inputs_dir = os.path.join(tmp, "inputs")
            os.makedirs(inputs_dir)
            secret_path = os.path.join(tmp, "project-id")
            with open(secret_path, "w") as f:
                f.write("my-gcp-project")
            scion_harness.atomic_write_json(
                os.path.join(inputs_dir, "auth-candidates.json"),
                {
                    "env_vars": ["GOOGLE_CLOUD_PROJECT"],
                    "env_secret_files": {
                        "GOOGLE_CLOUD_PROJECT": secret_path,
                    },
                    "file_secret_files": {},
                },
            )
            ctx = _make_ctx({"harness_bundle_dir": tmp})
            env: dict[str, str] = {}
            with temporary_home(tmp):
                provision._configure_vertex_ai(ctx, env)
                config_path = os.path.join(tmp, ".grok", "config.toml")
                self.assertTrue(os.path.isfile(config_path))
                with open(config_path) as f:
                    content = f.read()
            self.assertIn("[auth_provider.vertex-grok]", content)
            self.assertIn("[model.vertex-grok]", content)
            self.assertIn("[models]", content)
            self.assertIn('default = "vertex-grok"', content)
            self.assertIn("my-gcp-project", content)
            model = tomllib.loads(content)["model"]["vertex-grok"]
            self.assertEqual(model["api_backend"], "chat_completions")
            self.assertIs(model["supports_backend_search"], False)

    def test_vertex_global_endpoint_when_no_region(self) -> None:
        """When GOOGLE_CLOUD_REGION is not set, the base_url uses the global
        endpoint."""
        with tempfile.TemporaryDirectory() as tmp:
            inputs_dir = os.path.join(tmp, "inputs")
            os.makedirs(inputs_dir)
            secret_path = os.path.join(tmp, "project-id")
            with open(secret_path, "w") as f:
                f.write("my-gcp-project")
            scion_harness.atomic_write_json(
                os.path.join(inputs_dir, "auth-candidates.json"),
                {
                    "env_vars": ["GOOGLE_CLOUD_PROJECT"],
                    "env_secret_files": {
                        "GOOGLE_CLOUD_PROJECT": secret_path,
                    },
                    "file_secret_files": {},
                },
            )
            ctx = _make_ctx({"harness_bundle_dir": tmp})
            env: dict[str, str] = {}
            with temporary_home(tmp):
                provision._configure_vertex_ai(ctx, env)
                config_path = os.path.join(tmp, ".grok", "config.toml")
                with open(config_path) as f:
                    content = f.read()
            self.assertIn(
                "https://aiplatform.googleapis.com"
                "/v1beta1/projects/my-gcp-project/locations/global/endpoints/openapi",
                content,
            )

    def test_vertex_regional_endpoint_when_region_set(self) -> None:
        """When GOOGLE_CLOUD_REGION is set, the base_url uses the regional
        endpoint."""
        with tempfile.TemporaryDirectory() as tmp:
            inputs_dir = os.path.join(tmp, "inputs")
            os.makedirs(inputs_dir)
            project_path = os.path.join(tmp, "project-id")
            with open(project_path, "w") as f:
                f.write("my-gcp-project")
            region_path = os.path.join(tmp, "region")
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
                    "file_secret_files": {},
                },
            )
            ctx = _make_ctx({"harness_bundle_dir": tmp})
            env: dict[str, str] = {}
            with temporary_home(tmp):
                provision._configure_vertex_ai(ctx, env)
                config_path = os.path.join(tmp, ".grok", "config.toml")
                with open(config_path) as f:
                    content = f.read()
            self.assertIn(
                "https://us-central1-aiplatform.googleapis.com"
                "/v1beta1/projects/my-gcp-project/locations/us-central1/endpoints/openapi",
                content,
            )

    def test_vertex_global_region_uses_plain_hostname(self) -> None:
        """When GOOGLE_CLOUD_REGION is set to 'global', the base_url must use
        the plain hostname (aiplatform.googleapis.com), NOT
        'global-aiplatform.googleapis.com'."""
        with tempfile.TemporaryDirectory() as tmp:
            inputs_dir = os.path.join(tmp, "inputs")
            os.makedirs(inputs_dir)
            project_path = os.path.join(tmp, "project-id")
            with open(project_path, "w") as f:
                f.write("my-gcp-project")
            region_path = os.path.join(tmp, "region")
            with open(region_path, "w") as f:
                f.write("global")
            scion_harness.atomic_write_json(
                os.path.join(inputs_dir, "auth-candidates.json"),
                {
                    "env_vars": ["GOOGLE_CLOUD_PROJECT", "GOOGLE_CLOUD_REGION"],
                    "env_secret_files": {
                        "GOOGLE_CLOUD_PROJECT": project_path,
                        "GOOGLE_CLOUD_REGION": region_path,
                    },
                    "file_secret_files": {},
                },
            )
            ctx = _make_ctx({"harness_bundle_dir": tmp})
            env: dict[str, str] = {}
            with temporary_home(tmp):
                provision._configure_vertex_ai(ctx, env)
                config_path = os.path.join(tmp, ".grok", "config.toml")
                with open(config_path) as f:
                    content = f.read()
            # Must use the global endpoint (plain hostname), not region-prefixed.
            self.assertIn(
                "https://aiplatform.googleapis.com"
                "/v1beta1/projects/my-gcp-project/locations/global/endpoints/openapi",
                content,
            )
            # Must NOT contain the invalid region-prefixed hostname.
            self.assertNotIn("global-aiplatform.googleapis.com", content)

    def test_vertex_global_location_uses_plain_hostname(self) -> None:
        """When GOOGLE_CLOUD_LOCATION is set to 'global', the base_url must use
        the plain hostname — same fix applies regardless of which env var
        provides the region."""
        with tempfile.TemporaryDirectory() as tmp:
            inputs_dir = os.path.join(tmp, "inputs")
            os.makedirs(inputs_dir)
            project_path = os.path.join(tmp, "project-id")
            with open(project_path, "w") as f:
                f.write("my-gcp-project")
            location_path = os.path.join(tmp, "location")
            with open(location_path, "w") as f:
                f.write("global")
            scion_harness.atomic_write_json(
                os.path.join(inputs_dir, "auth-candidates.json"),
                {
                    "env_vars": [
                        "GOOGLE_CLOUD_PROJECT", "GOOGLE_CLOUD_LOCATION",
                    ],
                    "env_secret_files": {
                        "GOOGLE_CLOUD_PROJECT": project_path,
                        "GOOGLE_CLOUD_LOCATION": location_path,
                    },
                    "file_secret_files": {},
                },
            )
            ctx = _make_ctx({"harness_bundle_dir": tmp})
            env: dict[str, str] = {}
            with temporary_home(tmp):
                provision._configure_vertex_ai(ctx, env)
                config_path = os.path.join(tmp, ".grok", "config.toml")
                with open(config_path) as f:
                    content = f.read()
            self.assertIn(
                "https://aiplatform.googleapis.com"
                "/v1beta1/projects/my-gcp-project/locations/global/endpoints/openapi",
                content,
            )
            self.assertNotIn("global-aiplatform.googleapis.com", content)

    def test_vertex_adc_placed_when_staged(self) -> None:
        """When gcloud-adc file secret is staged, it is written to
        ~/.config/gcloud/application_default_credentials.json and
        GOOGLE_APPLICATION_CREDENTIALS is set."""
        with tempfile.TemporaryDirectory() as tmp:
            inputs_dir = os.path.join(tmp, "inputs")
            os.makedirs(inputs_dir)
            project_path = os.path.join(tmp, "project-id")
            with open(project_path, "w") as f:
                f.write("my-gcp-project")
            adc_path = os.path.join(tmp, "adc-creds")
            with open(adc_path, "w") as f:
                f.write('{"type": "authorized_user", "client_id": "test"}')
            scion_harness.atomic_write_json(
                os.path.join(inputs_dir, "auth-candidates.json"),
                {
                    "env_vars": ["GOOGLE_CLOUD_PROJECT"],
                    "env_secret_files": {
                        "GOOGLE_CLOUD_PROJECT": project_path,
                    },
                    "file_secret_files": {
                        "gcloud-adc": adc_path,
                    },
                },
            )
            ctx = _make_ctx({"harness_bundle_dir": tmp})
            env: dict[str, str] = {}
            with temporary_home(tmp):
                provision._configure_vertex_ai(ctx, env)
                target = os.path.join(
                    tmp, ".config", "gcloud",
                    "application_default_credentials.json",
                )
                self.assertTrue(os.path.isfile(target))
                with open(target) as f:
                    content = f.read()
                self.assertIn("authorized_user", content)
                mode = os.stat(target).st_mode & 0o777
                self.assertEqual(mode, 0o600)
            self.assertIn("GOOGLE_APPLICATION_CREDENTIALS", env)
            self.assertEqual(env["GOOGLE_APPLICATION_CREDENTIALS"], target)

    def test_vertex_no_adc_still_works(self) -> None:
        """vertex-ai works without ADC credentials (workload identity
        environments)."""
        with tempfile.TemporaryDirectory() as tmp:
            inputs_dir = os.path.join(tmp, "inputs")
            os.makedirs(inputs_dir)
            project_path = os.path.join(tmp, "project-id")
            with open(project_path, "w") as f:
                f.write("my-gcp-project")
            scion_harness.atomic_write_json(
                os.path.join(inputs_dir, "auth-candidates.json"),
                {
                    "env_vars": ["GOOGLE_CLOUD_PROJECT"],
                    "env_secret_files": {
                        "GOOGLE_CLOUD_PROJECT": project_path,
                    },
                    "file_secret_files": {},
                },
            )
            ctx = _make_ctx({"harness_bundle_dir": tmp})
            env: dict[str, str] = {}
            with temporary_home(tmp):
                extra = provision._configure_vertex_ai(ctx, env)
            self.assertTrue(extra.get("vertex_ai"))
            self.assertNotIn("GOOGLE_APPLICATION_CREDENTIALS", env)

    def test_vertex_auth_provider_command(self) -> None:
        """Verify the auth_provider block uses 'gcloud auth print-access-token'
        as the command."""
        with tempfile.TemporaryDirectory() as tmp:
            inputs_dir = os.path.join(tmp, "inputs")
            os.makedirs(inputs_dir)
            project_path = os.path.join(tmp, "project-id")
            with open(project_path, "w") as f:
                f.write("my-gcp-project")
            scion_harness.atomic_write_json(
                os.path.join(inputs_dir, "auth-candidates.json"),
                {
                    "env_vars": ["GOOGLE_CLOUD_PROJECT"],
                    "env_secret_files": {
                        "GOOGLE_CLOUD_PROJECT": project_path,
                    },
                    "file_secret_files": {},
                },
            )
            ctx = _make_ctx({"harness_bundle_dir": tmp})
            env: dict[str, str] = {}
            with temporary_home(tmp):
                provision._configure_vertex_ai(ctx, env)
                config_path = os.path.join(tmp, ".grok", "config.toml")
                with open(config_path) as f:
                    content = f.read()
            self.assertIn(
                'command = "gcloud auth print-access-token"', content
            )

    def test_api_key_preferred_over_vertex_ai(self) -> None:
        """When both XAI_API_KEY and GOOGLE_CLOUD_PROJECT are present,
        api-key is selected (listed first in AUTH)."""
        with tempfile.TemporaryDirectory() as tmp:
            inputs_dir = os.path.join(tmp, "inputs")
            os.makedirs(inputs_dir)
            scion_harness.atomic_write_json(
                os.path.join(inputs_dir, "auth-candidates.json"),
                {
                    "env_vars": ["XAI_API_KEY", "GOOGLE_CLOUD_PROJECT"],
                    "env_secret_files": {},
                    "file_secret_files": {},
                },
            )
            ctx = _make_ctx({"harness_bundle_dir": tmp})
            with temporary_home(tmp):
                resolved = ctx.select_auth(provision.AUTH)
            self.assertEqual(resolved.method, "api-key")

    def test_vertex_sets_grok_default_model_env(self) -> None:
        """vertex-ai sets GROK_DEFAULT_MODEL env var as config fallback."""
        with tempfile.TemporaryDirectory() as tmp:
            inputs_dir = os.path.join(tmp, "inputs")
            os.makedirs(inputs_dir)
            secret_path = os.path.join(tmp, "project-id")
            with open(secret_path, "w") as f:
                f.write("my-gcp-project")
            scion_harness.atomic_write_json(
                os.path.join(inputs_dir, "auth-candidates.json"),
                {
                    "env_vars": ["GOOGLE_CLOUD_PROJECT"],
                    "env_secret_files": {
                        "GOOGLE_CLOUD_PROJECT": secret_path,
                    },
                    "file_secret_files": {},
                },
            )
            ctx = _make_ctx({"harness_bundle_dir": tmp})
            env: dict[str, str] = {}
            with temporary_home(tmp):
                provision._configure_vertex_ai(ctx, env)
            self.assertEqual(env["GROK_DEFAULT_MODEL"], "vertex-grok")

    def test_vertex_alias_resolves_to_default(self) -> None:
        """Scion model aliases resolve to the default Vertex AI model."""
        with tempfile.TemporaryDirectory() as tmp:
            inputs_dir = os.path.join(tmp, "inputs")
            os.makedirs(inputs_dir)
            secret_path = os.path.join(tmp, "project-id")
            with open(secret_path, "w") as f:
                f.write("my-gcp-project")
            scion_harness.atomic_write_json(
                os.path.join(inputs_dir, "auth-candidates.json"),
                {
                    "env_vars": ["GOOGLE_CLOUD_PROJECT"],
                    "env_secret_files": {
                        "GOOGLE_CLOUD_PROJECT": secret_path,
                    },
                    "file_secret_files": {},
                },
            )
            ctx = _make_ctx({
                "harness_bundle_dir": tmp,
                "harness_config": {
                    "no_auth": {"behavior": "drop-to-shell"},
                    "instructions_file": "AGENTS.md",
                    "model_aliases": {
                        "small": "grok-3-mini",
                        "medium": "grok-4.5",
                        "large": "grok-4.6",
                        "extra-large": "grok-4.6",
                    },
                },
            })
            env: dict[str, str] = {}
            os.environ["SCION_MODEL"] = "small"
            try:
                with temporary_home(tmp):
                    provision._configure_vertex_ai(ctx, env)
                    config_path = os.path.join(tmp, ".grok", "config.toml")
                    with open(config_path) as f:
                        content = f.read()
            finally:
                os.environ.pop("SCION_MODEL", None)
            # Alias "small" should resolve to default Vertex model.
            self.assertIn("xai/grok-4.6", content)
            # The raw model name appears as an alias section header — that's
            # the fix for --model routing — but NOT as a model value.
            self.assertIn('[model."small"]', content)
            self.assertNotIn('model = "small"', content)

    def test_vertex_bare_model_falls_back_to_default(self) -> None:
        """Pre-resolved model name without publisher prefix (e.g., 'grok-4')
        falls back to the default Vertex AI model instead of passing through."""
        with tempfile.TemporaryDirectory() as tmp:
            inputs_dir = os.path.join(tmp, "inputs")
            os.makedirs(inputs_dir)
            secret_path = os.path.join(tmp, "project-id")
            with open(secret_path, "w") as f:
                f.write("my-gcp-project")
            scion_harness.atomic_write_json(
                os.path.join(inputs_dir, "auth-candidates.json"),
                {
                    "env_vars": ["GOOGLE_CLOUD_PROJECT"],
                    "env_secret_files": {
                        "GOOGLE_CLOUD_PROJECT": secret_path,
                    },
                    "file_secret_files": {},
                },
            )
            ctx = _make_ctx({
                "harness_bundle_dir": tmp,
                "harness_config": {
                    "no_auth": {"behavior": "drop-to-shell"},
                    "instructions_file": "AGENTS.md",
                    "model_aliases": {
                        "small": "grok-3-mini",
                        "medium": "grok-4.5",
                        "large": "grok-4.6",
                        "extra-large": "grok-4.6",
                    },
                },
            })
            env: dict[str, str] = {}
            os.environ["SCION_MODEL"] = "grok-4"
            try:
                with temporary_home(tmp):
                    provision._configure_vertex_ai(ctx, env)
                    config_path = os.path.join(tmp, ".grok", "config.toml")
                    with open(config_path) as f:
                        content = f.read()
            finally:
                os.environ.pop("SCION_MODEL", None)
            # Bare "grok-4" lacks publisher prefix — should use default model.
            self.assertIn("xai/grok-4.6", content)
            # The raw model name appears as an alias section header — that's
            # the fix for --model routing — but NOT as a model value.
            self.assertIn('[model."grok-4"]', content)
            self.assertNotIn('model = "grok-4"', content)

    def test_vertex_qualified_model_passes_through(self) -> None:
        """Fully-qualified model ID with publisher prefix passes through."""
        with tempfile.TemporaryDirectory() as tmp:
            inputs_dir = os.path.join(tmp, "inputs")
            os.makedirs(inputs_dir)
            secret_path = os.path.join(tmp, "project-id")
            with open(secret_path, "w") as f:
                f.write("my-gcp-project")
            scion_harness.atomic_write_json(
                os.path.join(inputs_dir, "auth-candidates.json"),
                {
                    "env_vars": ["GOOGLE_CLOUD_PROJECT"],
                    "env_secret_files": {
                        "GOOGLE_CLOUD_PROJECT": secret_path,
                    },
                    "file_secret_files": {},
                },
            )
            ctx = _make_ctx({"harness_bundle_dir": tmp})
            env: dict[str, str] = {}
            os.environ["SCION_MODEL"] = "xai/grok-4.2"
            try:
                with temporary_home(tmp):
                    provision._configure_vertex_ai(ctx, env)
                    config_path = os.path.join(tmp, ".grok", "config.toml")
                    with open(config_path) as f:
                        content = f.read()
            finally:
                os.environ.pop("SCION_MODEL", None)
            self.assertIn("xai/grok-4.2", content)
            self.assertNotIn("xai/grok-4.6", content)

    def test_vertex_explicit_model_id_passes_through(self) -> None:
        """Explicit model IDs (non-aliases) pass through to vertex config."""
        with tempfile.TemporaryDirectory() as tmp:
            inputs_dir = os.path.join(tmp, "inputs")
            os.makedirs(inputs_dir)
            secret_path = os.path.join(tmp, "project-id")
            with open(secret_path, "w") as f:
                f.write("my-gcp-project")
            scion_harness.atomic_write_json(
                os.path.join(inputs_dir, "auth-candidates.json"),
                {
                    "env_vars": ["GOOGLE_CLOUD_PROJECT"],
                    "env_secret_files": {
                        "GOOGLE_CLOUD_PROJECT": secret_path,
                    },
                    "file_secret_files": {},
                },
            )
            ctx = _make_ctx({"harness_bundle_dir": tmp})
            env: dict[str, str] = {}
            os.environ["SCION_MODEL"] = "xai/grok-4.2"
            try:
                with temporary_home(tmp):
                    provision._configure_vertex_ai(ctx, env)
                    config_path = os.path.join(tmp, ".grok", "config.toml")
                    with open(config_path) as f:
                        content = f.read()
            finally:
                os.environ.pop("SCION_MODEL", None)
            self.assertIn("xai/grok-4.2", content)
            self.assertNotIn("xai/grok-4.6", content)

    def test_vertex_detected_from_metadata_project_id(self) -> None:
        """GCP identity's SCION_METADATA_PROJECT_ID triggers vertex-ai auth."""
        with tempfile.TemporaryDirectory() as tmp:
            inputs_dir = os.path.join(tmp, "inputs")
            os.makedirs(inputs_dir)
            scion_harness.atomic_write_json(
                os.path.join(inputs_dir, "auth-candidates.json"),
                {
                    "env_vars": ["SCION_METADATA_PROJECT_ID"],
                    "env_secret_files": {},
                    "file_secret_files": {},
                },
            )
            ctx = _make_ctx({"harness_bundle_dir": tmp})
            with temporary_home(tmp):
                resolved = ctx.select_auth(provision.AUTH)
            self.assertEqual(resolved.method, "vertex-ai")

    def test_vertex_uses_metadata_project_id_fallback(self) -> None:
        """Falls back to SCION_METADATA_PROJECT_ID when GOOGLE_CLOUD_PROJECT is absent."""
        with tempfile.TemporaryDirectory() as tmp:
            inputs_dir = os.path.join(tmp, "inputs")
            os.makedirs(inputs_dir)
            # No GOOGLE_CLOUD_PROJECT secret file — only SCION_METADATA_PROJECT_ID
            # is available via the environment (injected by the platform).
            scion_harness.atomic_write_json(
                os.path.join(inputs_dir, "auth-candidates.json"),
                {
                    "env_vars": ["SCION_METADATA_PROJECT_ID"],
                    "env_secret_files": {},
                    "file_secret_files": {},
                },
            )
            ctx = _make_ctx({"harness_bundle_dir": tmp})
            env: dict[str, str] = {}
            os.environ["SCION_METADATA_PROJECT_ID"] = "my-assigned-project"
            try:
                with temporary_home(tmp):
                    provision._configure_vertex_ai(ctx, env)
                    config_path = os.path.join(tmp, ".grok", "config.toml")
                    with open(config_path) as f:
                        content = f.read()
            finally:
                os.environ.pop("SCION_METADATA_PROJECT_ID", None)
            # The project from SCION_METADATA_PROJECT_ID should appear in config.
            self.assertIn("my-assigned-project", content)
            # GOOGLE_CLOUD_PROJECT should be exported for downstream tools.
            self.assertEqual(env["GOOGLE_CLOUD_PROJECT"], "my-assigned-project")

    def test_vertex_model_alias_created_for_cli_override(self) -> None:
        """When SCION_MODEL is a concrete model name (e.g., 'grok-4.6'),
        config.toml must contain BOTH [model.vertex-grok] AND a
        [model."grok-4.6"] block routing through the vertex auth_provider."""
        with tempfile.TemporaryDirectory() as tmp:
            inputs_dir = os.path.join(tmp, "inputs")
            os.makedirs(inputs_dir)
            secret_path = os.path.join(tmp, "project-id")
            with open(secret_path, "w") as f:
                f.write("my-gcp-project")
            scion_harness.atomic_write_json(
                os.path.join(inputs_dir, "auth-candidates.json"),
                {
                    "env_vars": ["GOOGLE_CLOUD_PROJECT"],
                    "env_secret_files": {
                        "GOOGLE_CLOUD_PROJECT": secret_path,
                    },
                    "file_secret_files": {},
                },
            )
            ctx = _make_ctx({
                "harness_bundle_dir": tmp,
                "harness_config": {
                    "no_auth": {"behavior": "drop-to-shell"},
                    "instructions_file": "AGENTS.md",
                    "model_aliases": {
                        "small": "grok-3-mini",
                        "medium": "grok-4.5",
                        "large": "grok-4.6",
                    },
                },
            })
            env: dict[str, str] = {}
            os.environ["SCION_MODEL"] = "grok-4.6"
            try:
                with temporary_home(tmp):
                    provision._configure_vertex_ai(ctx, env)
                    config_path = os.path.join(tmp, ".grok", "config.toml")
                    with open(config_path) as f:
                        content = f.read()
            finally:
                os.environ.pop("SCION_MODEL", None)
            # Must have the default vertex-grok block.
            self.assertIn("[model.vertex-grok]", content)
            # Must also have the alias block for the CLI model name.
            self.assertIn('[model."grok-4.6"]', content)
            # Both blocks must route through the vertex auth_provider.
            self.assertEqual(
                content.count(f'auth_provider = "{provision._VERTEX_AUTH_PROVIDER_NAME}"'),
                2,
            )
            models = tomllib.loads(content)["model"]
            for name in ("vertex-grok", "grok-4.6"):
                self.assertEqual(models[name]["api_backend"], "chat_completions")
                self.assertIs(models[name]["supports_backend_search"], False)

    def test_vertex_model_alias_not_created_when_matches_config_name(self) -> None:
        """When SCION_MODEL equals the vertex config name ('vertex-grok'),
        no duplicate alias block is created."""
        with tempfile.TemporaryDirectory() as tmp:
            inputs_dir = os.path.join(tmp, "inputs")
            os.makedirs(inputs_dir)
            secret_path = os.path.join(tmp, "project-id")
            with open(secret_path, "w") as f:
                f.write("my-gcp-project")
            scion_harness.atomic_write_json(
                os.path.join(inputs_dir, "auth-candidates.json"),
                {
                    "env_vars": ["GOOGLE_CLOUD_PROJECT"],
                    "env_secret_files": {
                        "GOOGLE_CLOUD_PROJECT": secret_path,
                    },
                    "file_secret_files": {},
                },
            )
            ctx = _make_ctx({"harness_bundle_dir": tmp})
            env: dict[str, str] = {}
            os.environ["SCION_MODEL"] = "vertex-grok"
            try:
                with temporary_home(tmp):
                    provision._configure_vertex_ai(ctx, env)
                    config_path = os.path.join(tmp, ".grok", "config.toml")
                    with open(config_path) as f:
                        content = f.read()
            finally:
                os.environ.pop("SCION_MODEL", None)
            # Only one model block — no alias needed.
            self.assertEqual(
                content.count(f'auth_provider = "{provision._VERTEX_AUTH_PROVIDER_NAME}"'),
                1,
            )

    def test_vertex_model_alias_not_created_when_scion_model_empty(self) -> None:
        """When SCION_MODEL is empty, no alias block is created."""
        with tempfile.TemporaryDirectory() as tmp:
            inputs_dir = os.path.join(tmp, "inputs")
            os.makedirs(inputs_dir)
            secret_path = os.path.join(tmp, "project-id")
            with open(secret_path, "w") as f:
                f.write("my-gcp-project")
            scion_harness.atomic_write_json(
                os.path.join(inputs_dir, "auth-candidates.json"),
                {
                    "env_vars": ["GOOGLE_CLOUD_PROJECT"],
                    "env_secret_files": {
                        "GOOGLE_CLOUD_PROJECT": secret_path,
                    },
                    "file_secret_files": {},
                },
            )
            ctx = _make_ctx({"harness_bundle_dir": tmp})
            env: dict[str, str] = {}
            os.environ.pop("SCION_MODEL", None)
            with temporary_home(tmp):
                provision._configure_vertex_ai(ctx, env)
                config_path = os.path.join(tmp, ".grok", "config.toml")
                with open(config_path) as f:
                    content = f.read()
            self.assertEqual(
                content.count(f'auth_provider = "{provision._VERTEX_AUTH_PROVIDER_NAME}"'),
                1,
            )

    def test_vertex_model_alias_for_scion_alias_resolves_correctly(self) -> None:
        """When SCION_MODEL is a Scion size alias (e.g. 'small'), the alias
        block uses the raw name 'small' so --model small routes through vertex,
        and the model field uses the default Vertex model ID."""
        with tempfile.TemporaryDirectory() as tmp:
            inputs_dir = os.path.join(tmp, "inputs")
            os.makedirs(inputs_dir)
            secret_path = os.path.join(tmp, "project-id")
            with open(secret_path, "w") as f:
                f.write("my-gcp-project")
            scion_harness.atomic_write_json(
                os.path.join(inputs_dir, "auth-candidates.json"),
                {
                    "env_vars": ["GOOGLE_CLOUD_PROJECT"],
                    "env_secret_files": {
                        "GOOGLE_CLOUD_PROJECT": secret_path,
                    },
                    "file_secret_files": {},
                },
            )
            ctx = _make_ctx({
                "harness_bundle_dir": tmp,
                "harness_config": {
                    "no_auth": {"behavior": "drop-to-shell"},
                    "instructions_file": "AGENTS.md",
                    "model_aliases": {
                        "small": "grok-3-mini",
                        "medium": "grok-4.5",
                        "large": "grok-4.6",
                    },
                },
            })
            env: dict[str, str] = {}
            os.environ["SCION_MODEL"] = "small"
            try:
                with temporary_home(tmp):
                    provision._configure_vertex_ai(ctx, env)
                    config_path = os.path.join(tmp, ".grok", "config.toml")
                    with open(config_path) as f:
                        content = f.read()
            finally:
                os.environ.pop("SCION_MODEL", None)
            # The alias block should use the raw name "small".
            self.assertIn('[model."small"]', content)
            # Both blocks must route through vertex.
            self.assertEqual(
                content.count(f'auth_provider = "{provision._VERTEX_AUTH_PROVIDER_NAME}"'),
                2,
            )

    def test_vertex_empty_project_raises(self) -> None:
        """When GOOGLE_CLOUD_PROJECT is empty, ProvisionError is raised."""
        with tempfile.TemporaryDirectory() as tmp:
            inputs_dir = os.path.join(tmp, "inputs")
            os.makedirs(inputs_dir)
            # Create an empty project file.
            project_path = os.path.join(tmp, "project-id")
            with open(project_path, "w") as f:
                f.write("")
            scion_harness.atomic_write_json(
                os.path.join(inputs_dir, "auth-candidates.json"),
                {
                    "env_vars": ["GOOGLE_CLOUD_PROJECT"],
                    "env_secret_files": {
                        "GOOGLE_CLOUD_PROJECT": project_path,
                    },
                    "file_secret_files": {},
                },
            )
            ctx = _make_ctx({"harness_bundle_dir": tmp})
            env: dict[str, str] = {}
            with temporary_home(tmp):
                with self.assertRaises(scion_harness.ProvisionError) as cm:
                    provision._configure_vertex_ai(ctx, env)
                self.assertIn("GOOGLE_CLOUD_PROJECT", str(cm.exception))

    def test_configure_vertex_ai_raises_and_does_not_report_success_when_config_write_rejected(self) -> None:
        # ptone/scion#2427 review round 1 (R2): _write_vertex_config raising
        # on a rejected write must propagate out of _configure_vertex_ai
        # before it exports GROK_DEFAULT_MODEL or logs the misleading
        # "vertex-ai: project=... model=..." success line — a vertex-auth
        # agent with no vertex config is guaranteed broken (grok falls back
        # to the direct xAI API and fails auth), so this must fail loudly
        # rather than report success.
        with tempfile.TemporaryDirectory() as tmp:
            inputs_dir = os.path.join(tmp, "inputs")
            os.makedirs(inputs_dir)
            project_path = os.path.join(tmp, "project-id")
            with open(project_path, "w") as f:
                f.write("my-gcp-project")
            scion_harness.atomic_write_json(
                os.path.join(inputs_dir, "auth-candidates.json"),
                {
                    "env_vars": ["GOOGLE_CLOUD_PROJECT"],
                    "env_secret_files": {"GOOGLE_CLOUD_PROJECT": project_path},
                    "file_secret_files": {},
                },
            )
            ctx = _make_ctx({"harness_bundle_dir": tmp})
            env: dict[str, str] = {}
            with temporary_home(tmp):
                grok_dir = os.path.join(tmp, ".grok")
                os.makedirs(grok_dir, exist_ok=True)
                config_path = os.path.join(grok_dir, "config.toml")
                # Originally used an escaped closing-delimiter sequence to
                # force a rejection; that gap is closed as of
                # ptone/scion#2427 review round 2 ("Consider 2"). Per review
                # round 2 (R2-b), this now forces real damage instead:
                # strip_toml_sections is patched to also (wrongly) drop an
                # unrelated sibling [auth_provider.other] block.
                original = '[auth_provider.other]\ncommand = "other-cmd"\n'
                self.assertIsNotNone(tomllib.loads(original), "sanity: original must be valid TOML")
                with open(config_path, "w") as f:
                    f.write(original)

                with _also_strip_sections_damaging("[auth_provider.other]"):
                    with self.assertRaises(scion_harness.ProvisionError):
                        provision._configure_vertex_ai(ctx, env)

                with open(config_path) as f:
                    after = f.read()
            self.assertEqual(after, original, "config.toml must be left untouched")
        self.assertNotIn("GROK_DEFAULT_MODEL", env, "GROK_DEFAULT_MODEL must not be exported on a rejected write")

    def test_configure_vertex_ai_raises_and_does_not_report_success_when_alias_write_rejected(self) -> None:
        # ptone/scion#2427 review round 3 (Nit): mirrors the config-rejection
        # test above, but for the alias write's raise (R2-a) — asserts that
        # GROK_DEFAULT_MODEL is not exported and no "vertex-ai: project=..."
        # success line is logged when _write_vertex_model_alias's write is
        # the one that's rejected (the primary vertex config write itself
        # succeeds normally). Uses a targeted patch on write_toml_if_preserves
        # (rather than the section-damage helper) so only the alias step is
        # affected, not the earlier config step.
        with tempfile.TemporaryDirectory() as tmp:
            inputs_dir = os.path.join(tmp, "inputs")
            os.makedirs(inputs_dir)
            project_path = os.path.join(tmp, "project-id")
            with open(project_path, "w") as f:
                f.write("my-gcp-project")
            scion_harness.atomic_write_json(
                os.path.join(inputs_dir, "auth-candidates.json"),
                {
                    "env_vars": ["GOOGLE_CLOUD_PROJECT"],
                    "env_secret_files": {"GOOGLE_CLOUD_PROJECT": project_path},
                    "file_secret_files": {},
                },
            )
            ctx = _make_ctx({"harness_bundle_dir": tmp})
            env: dict[str, str] = {}
            os.environ["SCION_MODEL"] = "grok-4.2"
            info_lines: list[str] = []
            ctx.info = info_lines.append  # type: ignore[method-assign]

            real_write_if_preserves = scion_harness.write_toml_if_preserves

            def fail_only_alias(ctx_arg, path, original, content, managed_keys=(), *, what="", mode=None):
                if what.startswith("vertex-ai model alias"):
                    ctx_arg.warn(f"{what}: forced rejection for test")
                    return False
                return real_write_if_preserves(ctx_arg, path, original, content, managed_keys, what=what, mode=mode)

            with temporary_home(tmp):
                with mock.patch.object(scion_harness, "write_toml_if_preserves", side_effect=fail_only_alias):
                    with self.assertRaises(scion_harness.ProvisionError):
                        provision._configure_vertex_ai(ctx, env)
                # The primary vertex config write (unaffected by the patch)
                # must still have succeeded.
                config_path = os.path.join(tmp, ".grok", "config.toml")
                with open(config_path, "rb") as f:
                    data = tomllib.load(f)
        self.assertEqual(data["model"]["vertex-grok"]["model"], "xai/grok-4.6")
        self.assertNotIn("GROK_DEFAULT_MODEL", env, "GROK_DEFAULT_MODEL must not be exported on a rejected write")
        self.assertFalse(
            any("vertex-ai: project=" in line for line in info_lines),
            f"expected no success line, got: {info_lines}",
        )


# ---------------------------------------------------------------------------
# Native System Prompt Tests
# ---------------------------------------------------------------------------


class NativeSystemPromptTest(unittest.TestCase):
    """Test native system prompt routing via _apply_native_system_prompt."""

    def test_system_prompt_written_to_native_file(self) -> None:
        """System prompt is written to .grok/system-prompt.md when staged."""
        with tempfile.TemporaryDirectory() as tmp:
            inputs_dir = os.path.join(tmp, "inputs")
            os.makedirs(inputs_dir)
            with open(os.path.join(inputs_dir, "system-prompt.md"), "w") as f:
                f.write("You are a helpful assistant.\n")
            ctx = _make_ctx({
                "harness_bundle_dir": tmp,
                "harness_config": {
                    "no_auth": {"behavior": "drop-to-shell"},
                    "instructions_file": ".grok/AGENTS.md",
                    "system_prompt_file": ".grok/system-prompt.md",
                    "system_prompt_mode": "native",
                    "skills_dir": ".grok/skills",
                },
            })
            with temporary_home(tmp):
                provision._apply_native_system_prompt(ctx)
                target = os.path.join(tmp, ".grok", "system-prompt.md")
                self.assertTrue(os.path.isfile(target))
                with open(target) as f:
                    content = f.read()
                self.assertEqual(content, "You are a helpful assistant.\n")

    def test_system_prompt_not_in_agents_md(self) -> None:
        """When native routing is used, system prompt is NOT in AGENTS.md."""
        with tempfile.TemporaryDirectory() as tmp:
            inputs_dir = os.path.join(tmp, "inputs")
            os.makedirs(inputs_dir)
            with open(os.path.join(inputs_dir, "system-prompt.md"), "w") as f:
                f.write("You are a system prompt.\n")
            with open(os.path.join(inputs_dir, "instructions.md"), "w") as f:
                f.write("Do the thing.\n")
            ctx = _make_ctx({
                "harness_bundle_dir": tmp,
                "harness_config": {
                    "no_auth": {"behavior": "drop-to-shell"},
                    "instructions_file": ".grok/AGENTS.md",
                    "system_prompt_file": ".grok/system-prompt.md",
                    "system_prompt_mode": "native",
                    "skills_dir": ".grok/skills",
                },
            })
            with temporary_home(tmp):
                provision._apply_native_system_prompt(ctx)
                target = os.path.join(tmp, ".grok", "AGENTS.md")
                os.makedirs(os.path.dirname(target), exist_ok=True)
                scion_harness.project_instructions(
                    ctx, target, system_prompt_mode="none",
                )
                with open(target) as f:
                    content = f.read()
                self.assertIn("Do the thing.", content)
                self.assertNotIn("You are a system prompt.", content)

    def test_no_system_prompt_file_when_empty(self) -> None:
        """When no system prompt is staged, .grok/system-prompt.md is not created."""
        with tempfile.TemporaryDirectory() as tmp:
            inputs_dir = os.path.join(tmp, "inputs")
            os.makedirs(inputs_dir)
            # Stage an empty system prompt.
            with open(os.path.join(inputs_dir, "system-prompt.md"), "w") as f:
                f.write("   \n")
            ctx = _make_ctx({
                "harness_bundle_dir": tmp,
                "harness_config": {
                    "no_auth": {"behavior": "drop-to-shell"},
                    "instructions_file": ".grok/AGENTS.md",
                    "system_prompt_file": ".grok/system-prompt.md",
                    "system_prompt_mode": "native",
                    "skills_dir": ".grok/skills",
                },
            })
            with temporary_home(tmp):
                provision._apply_native_system_prompt(ctx)
                target = os.path.join(tmp, ".grok", "system-prompt.md")
                self.assertFalse(os.path.isfile(target))


if __name__ == "__main__":
    unittest.main()
