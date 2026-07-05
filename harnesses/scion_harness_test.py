#!/usr/bin/env python3
"""Unit tests for scion_harness.py — the shared harness provisioner library.

Table-driven tests covering:
  - Auth engine (explicit selection, precedence, no-auth gate, error messages)
  - Outputs writer (resolved-auth.json schema v2, env.json)
  - Instruction projection (compose, strip, legacy markers, unclosed guard)
  - TOML helpers (escape, inline table, string array, strip sections)
  - Secret whitespace policy
  - read_json_skipping_comment_lines
  - capture_auth_main
  - run() scaffold
"""
from __future__ import annotations

import json
import os
import sys
import tempfile
import textwrap
import unittest
from typing import Any
from unittest import mock

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import scion_harness as sh


class TestVersionContract(unittest.TestCase):
    def test_interface_version(self):
        self.assertEqual(sh.INTERFACE_VERSION, 2)

    def test_lib_version_is_date(self):
        parts = sh.LIB_VERSION.split("-")
        self.assertEqual(len(parts), 3)
        self.assertEqual(len(parts[0]), 4)


# ---------------------------------------------------------------------------
# Auth engine
# ---------------------------------------------------------------------------


def _make_ctx(
    harness: str = "test",
    candidates: dict[str, Any] | None = None,
    harness_config: dict[str, Any] | None = None,
    secret_dir: str | None = None,
) -> sh.ProvisionContext:
    """Build a ProvisionContext with a synthetic manifest pointing at a temp bundle."""
    bundle = tempfile.mkdtemp()
    inputs = os.path.join(bundle, "inputs")
    os.makedirs(inputs, exist_ok=True)

    cand = candidates if candidates is not None else {}
    with open(os.path.join(inputs, "auth-candidates.json"), "w") as f:
        json.dump(cand, f)

    manifest: dict[str, Any] = {
        "harness_bundle_dir": bundle,
        "agent_workspace": "/workspace",
        "harness_config": harness_config or {},
    }
    return sh.ProvisionContext(harness, manifest)


class TestAuthExplicitSelection(unittest.TestCase):
    """Explicit type validation and selection."""

    def _spec(self) -> sh.AuthSpec:
        return sh.AuthSpec("test", [
            sh.env_method("api-key", any_of=["API_KEY"]),
            sh.env_method("oauth-token", any_of=["OAUTH_TOKEN"]),
            sh.file_method("auth-file", path="~/.test/creds.json"),
        ])

    def test_explicit_valid_type_present(self):
        ctx = _make_ctx(candidates={"explicit_type": "api-key", "env_vars": ["API_KEY"],
                                     "env_secret_files": {"API_KEY": "/dev/null"}})
        result = ctx.select_auth(self._spec())
        self.assertEqual(result.method, "api-key")
        self.assertEqual(result.env_key, "API_KEY")

    def test_explicit_invalid_type_raises(self):
        ctx = _make_ctx(candidates={"explicit_type": "magic"})
        with self.assertRaises(sh.ProvisionError) as cm:
            ctx.select_auth(self._spec())
        self.assertIn("magic", str(cm.exception))
        self.assertIn("valid types", str(cm.exception))

    def test_explicit_type_missing_creds_raises(self):
        ctx = _make_ctx(candidates={"explicit_type": "api-key"})
        with self.assertRaises(sh.ProvisionError) as cm:
            ctx.select_auth(self._spec())
        self.assertIn("api-key", str(cm.exception))


class TestAuthPrecedence(unittest.TestCase):
    """Auto-detection follows spec list order."""

    def test_first_match_wins(self):
        spec = sh.AuthSpec("test", [
            sh.env_method("primary", any_of=["PRIMARY_KEY"]),
            sh.env_method("secondary", any_of=["SECONDARY_KEY"]),
        ])
        ctx = _make_ctx(candidates={
            "env_vars": ["PRIMARY_KEY", "SECONDARY_KEY"],
            "env_secret_files": {"PRIMARY_KEY": "/dev/null", "SECONDARY_KEY": "/dev/null"},
        })
        result = ctx.select_auth(spec)
        self.assertEqual(result.method, "primary")
        self.assertEqual(result.env_key, "PRIMARY_KEY")

    def test_fallback_to_second(self):
        spec = sh.AuthSpec("test", [
            sh.env_method("primary", any_of=["PRIMARY_KEY"]),
            sh.env_method("secondary", any_of=["SECONDARY_KEY"]),
        ])
        ctx = _make_ctx(candidates={
            "env_vars": ["SECONDARY_KEY"],
            "env_secret_files": {"SECONDARY_KEY": "/dev/null"},
        })
        result = ctx.select_auth(spec)
        self.assertEqual(result.method, "secondary")

    def test_any_of_picks_first_present(self):
        spec = sh.AuthSpec("test", [
            sh.env_method("api-key", any_of=["KEY_A", "KEY_B", "KEY_C"]),
        ])
        ctx = _make_ctx(candidates={
            "env_vars": ["KEY_B", "KEY_C"],
            "env_secret_files": {"KEY_B": "/dev/null", "KEY_C": "/dev/null"},
        })
        result = ctx.select_auth(spec)
        self.assertEqual(result.env_key, "KEY_B")


class TestAuthNoAuthGate(unittest.TestCase):
    """No-auth behavior when no candidates staged."""

    def test_no_candidates_with_no_auth_behavior(self):
        ctx = _make_ctx(
            candidates={},
            harness_config={"no_auth": {"behavior": "allow"}},
        )
        spec = sh.AuthSpec("test", [
            sh.env_method("api-key", any_of=["API_KEY"]),
        ])
        result = ctx.select_auth(spec)
        self.assertEqual(result.method, "none")

    def test_no_candidates_without_no_auth_raises(self):
        ctx = _make_ctx(candidates={})
        spec = sh.AuthSpec("test", [
            sh.env_method("api-key", any_of=["API_KEY"]),
        ])
        with self.assertRaises(sh.ProvisionError):
            ctx.select_auth(spec)

    def test_fallback_to_none_on_error(self):
        ctx = _make_ctx(
            candidates={"env_vars": []},
            harness_config={"no_auth": {"behavior": "allow"}},
        )
        spec = sh.AuthSpec("test", [
            sh.env_method("api-key", any_of=["API_KEY"]),
        ], fallback_to_none_on_error=True)
        result = ctx.select_auth(spec)
        self.assertEqual(result.method, "none")


class TestAuthAllOf(unittest.TestCase):
    """all_of requires all keys present."""

    def test_all_of_all_present(self):
        spec = sh.AuthSpec("test", [
            sh.env_method("vertex-ai",
                          all_of=["GOOGLE_CLOUD_PROJECT"],
                          any_of=["GOOGLE_CLOUD_LOCATION", "GOOGLE_CLOUD_REGION"]),
        ])
        ctx = _make_ctx(candidates={
            "env_vars": ["GOOGLE_CLOUD_PROJECT", "GOOGLE_CLOUD_LOCATION"],
            "env_secret_files": {
                "GOOGLE_CLOUD_PROJECT": "/dev/null",
                "GOOGLE_CLOUD_LOCATION": "/dev/null",
            },
        })
        result = ctx.select_auth(spec)
        self.assertEqual(result.method, "vertex-ai")
        self.assertEqual(result.env_key, "GOOGLE_CLOUD_LOCATION")

    def test_all_of_missing_one(self):
        spec = sh.AuthSpec("test", [
            sh.env_method("vertex-ai",
                          all_of=["GOOGLE_CLOUD_PROJECT"],
                          any_of=["GOOGLE_CLOUD_LOCATION"]),
        ])
        ctx = _make_ctx(candidates={
            "env_vars": ["GOOGLE_CLOUD_LOCATION"],
            "env_secret_files": {"GOOGLE_CLOUD_LOCATION": "/dev/null"},
        })
        with self.assertRaises(sh.ProvisionError):
            ctx.select_auth(spec)


class TestAuthFileMethod(unittest.TestCase):
    """File-based auth method detection."""

    def test_file_in_file_paths(self):
        with tempfile.NamedTemporaryFile(suffix=".json", delete=False) as tmp:
            tmp.write(b"{}")
            tmp_path = tmp.name
        try:
            spec = sh.AuthSpec("test", [
                sh.file_method("auth-file", path=tmp_path),
            ])
            ctx = _make_ctx(candidates={"files": [{"container_path": tmp_path}]})
            result = ctx.select_auth(spec)
            self.assertEqual(result.method, "auth-file")
            self.assertEqual(result.auth_file, tmp_path)
        finally:
            os.unlink(tmp_path)

    def test_file_on_disk(self):
        with tempfile.NamedTemporaryFile(suffix=".json", delete=False) as tmp:
            tmp.write(b"{}")
            tmp_path = tmp.name
        try:
            spec = sh.AuthSpec("test", [
                sh.file_method("auth-file", path=tmp_path),
            ])
            ctx = _make_ctx(candidates={})
            result = ctx.select_auth(spec)
            self.assertEqual(result.method, "auth-file")
        finally:
            os.unlink(tmp_path)


class TestAuthEnvFallback(unittest.TestCase):
    """env_fallback=True checks os.environ."""

    def test_env_fallback(self):
        spec = sh.AuthSpec("test", [
            sh.env_method("api-key", any_of=["MY_TOKEN"], env_fallback=True),
        ])
        with mock.patch.dict(os.environ, {"MY_TOKEN": "secret123"}):
            ctx = _make_ctx(candidates={"env_vars": []})
            result = ctx.select_auth(spec)
            self.assertEqual(result.method, "api-key")
            self.assertEqual(result.env_key, "MY_TOKEN")


# ---------------------------------------------------------------------------
# Outputs writer
# ---------------------------------------------------------------------------


class TestWriteOutputs(unittest.TestCase):
    def test_resolved_auth_schema_v2(self):
        ctx = _make_ctx()
        os.makedirs(os.path.join(ctx.bundle_dir, "outputs"), exist_ok=True)
        resolved = sh.ResolvedAuth(method="api-key", env_key="MY_KEY")
        ctx.write_outputs(resolved, env={"FOO": "${FOO}"})

        auth_path, env_path = ctx.output_paths()
        auth = json.load(open(auth_path))
        self.assertEqual(auth["schema_version"], 2)
        self.assertEqual(auth["harness"], "test")
        self.assertEqual(auth["method"], "api-key")
        self.assertEqual(auth["env_var"], "MY_KEY")

        env = json.load(open(env_path))
        self.assertEqual(env["FOO"], "${FOO}")

    def test_extra_fields_merged(self):
        ctx = _make_ctx()
        os.makedirs(os.path.join(ctx.bundle_dir, "outputs"), exist_ok=True)
        resolved = sh.ResolvedAuth(method="vertex-ai")
        ctx.write_outputs(resolved, extra={"vertex_ai": True})

        auth_path, _ = ctx.output_paths()
        auth = json.load(open(auth_path))
        self.assertTrue(auth["vertex_ai"])

    def test_no_auth_method(self):
        ctx = _make_ctx()
        os.makedirs(os.path.join(ctx.bundle_dir, "outputs"), exist_ok=True)
        resolved = sh.ResolvedAuth(method="none")
        ctx.write_outputs(resolved)

        auth_path, _ = ctx.output_paths()
        auth = json.load(open(auth_path))
        self.assertEqual(auth["method"], "none")
        self.assertNotIn("env_var", auth)


# ---------------------------------------------------------------------------
# Secret whitespace policy
# ---------------------------------------------------------------------------


class TestSecretWhitespace(unittest.TestCase):
    def test_rstrip_cr_lf_only(self):
        ctx = _make_ctx()
        secret_dir = os.path.join(ctx.bundle_dir, "secrets")
        os.makedirs(secret_dir, exist_ok=True)
        secret_path = os.path.join(secret_dir, "MY_KEY")
        with open(secret_path, "w") as f:
            f.write("  secret-value  \r\n")

        ctx._candidates = {
            "env_secret_files": {"MY_KEY": secret_path},
        }
        value = ctx.read_secret("MY_KEY")
        self.assertEqual(value, "  secret-value  ")

    def test_missing_secret_returns_empty(self):
        ctx = _make_ctx()
        value = ctx.read_secret("NONEXISTENT")
        self.assertEqual(value, "")

    def test_env_fallback(self):
        ctx = _make_ctx()
        with mock.patch.dict(os.environ, {"MY_KEY": "from-env"}):
            value = ctx.read_secret("MY_KEY", env_fallback=True)
            self.assertEqual(value, "from-env")


# ---------------------------------------------------------------------------
# Instruction projection
# ---------------------------------------------------------------------------


class TestStripManagedBlock(unittest.TestCase):
    def test_strip_standard_markers(self):
        content = "before\n<!-- BEGIN SCION MANAGED -->\nmanaged\n<!-- END SCION MANAGED -->\nafter"
        result = sh._strip_managed_block(content)
        self.assertIn("before", result)
        self.assertIn("after", result)
        self.assertNotIn("managed", result)

    def test_strip_codex_legacy_markers(self):
        content = "user\n<!-- BEGIN SCION MANAGED CODEX INSTRUCTIONS -->\nstuff\n<!-- END SCION MANAGED CODEX INSTRUCTIONS -->\nrest"
        result = sh._strip_managed_block(content)
        self.assertIn("user", result)
        self.assertIn("rest", result)
        self.assertNotIn("stuff", result)

    def test_strip_hermes_legacy_markers(self):
        content = "user\n<!-- BEGIN SCION MANAGED HERMES INSTRUCTIONS -->\nstuff\n<!-- END SCION MANAGED HERMES INSTRUCTIONS -->\nrest"
        result = sh._strip_managed_block(content)
        self.assertNotIn("stuff", result)

    def test_strip_copilot_legacy_markers(self):
        content = "user\n<!-- SCION_MANAGED_BEGIN -->\nstuff\n<!-- SCION_MANAGED_END -->\nrest"
        result = sh._strip_managed_block(content)
        self.assertNotIn("stuff", result)

    def test_unclosed_marker_preserved(self):
        content = "before\n<!-- BEGIN SCION MANAGED -->\nunclosed content"
        result = sh._strip_managed_block(content)
        self.assertEqual(result, content)

    def test_no_markers(self):
        content = "just plain text\n"
        result = sh._strip_managed_block(content)
        self.assertIn("just plain text", result)


class TestProjectInstructions(unittest.TestCase):
    def test_compose_with_instructions(self):
        ctx = _make_ctx(harness_config={"system_prompt_mode": "prepend_to_instructions"})
        inputs = ctx.inputs_dir
        with open(os.path.join(inputs, "instructions.md"), "w") as f:
            f.write("Do the thing.")
        with open(os.path.join(inputs, "system-prompt.md"), "w") as f:
            f.write("You are helpful.")

        target = os.path.join(tempfile.mkdtemp(), "AGENTS.md")
        sh.project_instructions(ctx, target, system_prompt_mode="prepend_to_instructions")

        content = open(target).read()
        self.assertIn("<!-- BEGIN SCION MANAGED -->", content)
        self.assertIn("<!-- END SCION MANAGED -->", content)
        self.assertIn("System Instruction", content)
        self.assertIn("You are helpful.", content)
        self.assertIn("Agent Instructions", content)
        self.assertIn("Do the thing.", content)

    def test_strips_existing_managed_block(self):
        ctx = _make_ctx()
        inputs = ctx.inputs_dir
        with open(os.path.join(inputs, "instructions.md"), "w") as f:
            f.write("New instructions.")

        target_dir = tempfile.mkdtemp()
        target = os.path.join(target_dir, "AGENTS.md")
        with open(target, "w") as f:
            f.write("<!-- BEGIN SCION MANAGED -->\nold\n<!-- END SCION MANAGED -->\nuser content\n")

        sh.project_instructions(ctx, target)

        content = open(target).read()
        self.assertNotIn("old", content)
        self.assertIn("New instructions.", content)
        self.assertIn("user content", content)


# ---------------------------------------------------------------------------
# TOML helpers
# ---------------------------------------------------------------------------


class TestTomlEscape(unittest.TestCase):
    def test_basic_escapes(self):
        self.assertEqual(sh.toml_escape('hello'), 'hello')
        self.assertEqual(sh.toml_escape('a"b'), 'a\\"b')
        self.assertEqual(sh.toml_escape('a\\b'), 'a\\\\b')
        self.assertEqual(sh.toml_escape('a\nb'), 'a\\nb')
        self.assertEqual(sh.toml_escape('a\rb'), 'a\\rb')
        self.assertEqual(sh.toml_escape('a\tb'), 'a\\tb')


class TestTomlInlineTable(unittest.TestCase):
    def test_sorted_keys(self):
        result = sh.toml_inline_table({"b": "2", "a": "1"})
        self.assertEqual(result, '{ "a" = "1", "b" = "2" }')


class TestTomlStringArray(unittest.TestCase):
    def test_basic(self):
        result = sh.toml_string_array(["x", "y"])
        self.assertEqual(result, '["x", "y"]')


class TestStripTomlSections(unittest.TestCase):
    def test_strip_otel(self):
        content = "key = 1\n\n[otel]\nenabled = true\n\n[other]\nval = 2\n"
        result = sh.strip_toml_sections(content, lambda h: h == "[otel]")
        self.assertNotIn("[otel]", result)
        self.assertNotIn("enabled = true", result)
        self.assertIn("[other]", result)
        self.assertIn("val = 2", result)

    def test_strip_mcp_sections(self):
        content = "base = 1\n\n[mcp_servers.foo]\ncommand = x\n\n[mcp_servers.bar]\nurl = y\n"
        result = sh.strip_toml_sections(content, lambda h: h.startswith("[mcp_servers."))
        self.assertNotIn("[mcp_servers.", result)
        self.assertIn("base = 1", result)

    def test_no_match_unchanged(self):
        content = "[regular]\nkey = val\n"
        result = sh.strip_toml_sections(content, lambda h: h == "[nonexistent]")
        self.assertEqual(result, content)


# ---------------------------------------------------------------------------
# atomic_write_text
# ---------------------------------------------------------------------------


class TestAtomicWriteText(unittest.TestCase):
    def test_write_and_read(self):
        path = os.path.join(tempfile.mkdtemp(), "test.txt")
        sh.atomic_write_text(path, "hello world\n")
        with open(path) as f:
            self.assertEqual(f.read(), "hello world\n")

    def test_write_with_mode(self):
        path = os.path.join(tempfile.mkdtemp(), "secret.txt")
        sh.atomic_write_text(path, "secret\n", mode=0o600)
        stat = os.stat(path)
        self.assertEqual(stat.st_mode & 0o777, 0o600)


# ---------------------------------------------------------------------------
# read_json_skipping_comment_lines
# ---------------------------------------------------------------------------


class TestReadJsonSkippingComments(unittest.TestCase):
    def test_strips_comments(self):
        path = os.path.join(tempfile.mkdtemp(), "config.json")
        with open(path, "w") as f:
            f.write('// This is a comment\n{"key": "value"}\n')
        result = sh.read_json_skipping_comment_lines(path)
        self.assertEqual(result, {"key": "value"})

    def test_no_comments(self):
        path = os.path.join(tempfile.mkdtemp(), "config.json")
        with open(path, "w") as f:
            f.write('{"key": "value"}\n')
        result = sh.read_json_skipping_comment_lines(path)
        self.assertEqual(result, {"key": "value"})


# ---------------------------------------------------------------------------
# MCP translation helper
# ---------------------------------------------------------------------------


class TestApplyMcpTranslated(unittest.TestCase):
    def test_translate_and_write(self):
        ctx = _make_ctx()
        inputs = ctx.inputs_dir
        with open(os.path.join(inputs, "mcp-servers.json"), "w") as f:
            json.dump({"mcp_servers": {
                "server1": {"transport": "stdio", "command": "cmd1"},
                "server2": {"transport": "sse", "url": "http://example.com"},
            }}, f)

        written_servers: dict[str, Any] = {}

        def translate(name, spec):
            return {"name": name, "type": spec.get("transport")}

        def write(servers):
            written_servers.update(servers)

        count = sh.apply_mcp_translated(ctx, translate, write)
        self.assertEqual(count, 2)
        self.assertIn("server1", written_servers)
        self.assertIn("server2", written_servers)

    def test_skip_none_translations(self):
        ctx = _make_ctx()
        inputs = ctx.inputs_dir
        with open(os.path.join(inputs, "mcp-servers.json"), "w") as f:
            json.dump({"mcp_servers": {
                "good": {"transport": "stdio", "command": "cmd"},
                "bad": {"transport": "unknown"},
            }}, f)

        results: dict[str, Any] = {}

        def translate(name, spec):
            if spec.get("transport") == "unknown":
                return None
            return {"ok": True}

        def write(servers):
            results.update(servers)

        count = sh.apply_mcp_translated(ctx, translate, write)
        self.assertEqual(count, 1)
        self.assertIn("good", results)
        self.assertNotIn("bad", results)


# ---------------------------------------------------------------------------
# run() scaffold
# ---------------------------------------------------------------------------


class TestRunScaffold(unittest.TestCase):
    def test_missing_manifest_exits_1(self):
        with self.assertRaises(SystemExit) as cm:
            with mock.patch("sys.argv", ["provision.py", "--manifest", "/nonexistent/manifest.json"]):
                sh.run("test", lambda ctx: None)
        self.assertEqual(cm.exception.code, 1)

    def test_provision_success(self):
        bundle = tempfile.mkdtemp()
        manifest_path = os.path.join(bundle, "manifest.json")
        with open(manifest_path, "w") as f:
            json.dump({"command": "provision", "harness_bundle_dir": bundle}, f)

        called = []

        def provision_fn(ctx):
            called.append(True)

        with self.assertRaises(SystemExit) as cm:
            with mock.patch("sys.argv", ["provision.py", "--manifest", manifest_path]):
                sh.run("test", provision_fn)
        self.assertEqual(cm.exception.code, 0)
        self.assertTrue(called)

    def test_unsupported_command_exits_2(self):
        bundle = tempfile.mkdtemp()
        manifest_path = os.path.join(bundle, "manifest.json")
        with open(manifest_path, "w") as f:
            json.dump({"command": "unknown_cmd"}, f)

        with self.assertRaises(SystemExit) as cm:
            with mock.patch("sys.argv", ["provision.py", "--manifest", manifest_path]):
                sh.run("test", lambda ctx: None)
        self.assertEqual(cm.exception.code, 2)

    def test_provision_error_exits_1(self):
        bundle = tempfile.mkdtemp()
        manifest_path = os.path.join(bundle, "manifest.json")
        with open(manifest_path, "w") as f:
            json.dump({"command": "provision", "harness_bundle_dir": bundle}, f)

        def bad_provision(ctx):
            raise sh.ProvisionError("something broke")

        with self.assertRaises(SystemExit) as cm:
            with mock.patch("sys.argv", ["provision.py", "--manifest", manifest_path]):
                sh.run("test", bad_provision)
        self.assertEqual(cm.exception.code, 1)


# ---------------------------------------------------------------------------
# ProvisionContext properties
# ---------------------------------------------------------------------------


class TestProvisionContext(unittest.TestCase):
    def test_workspace_default(self):
        ctx = _make_ctx()
        self.assertEqual(ctx.workspace, "/workspace")

    def test_workspace_from_manifest(self):
        manifest = {"agent_workspace": "/custom/workspace", "harness_bundle_dir": "/tmp"}
        ctx = sh.ProvisionContext("test", manifest)
        self.assertEqual(ctx.workspace, "/custom/workspace")

    def test_harness_config(self):
        ctx = _make_ctx(harness_config={"no_auth": {"behavior": "allow"}})
        self.assertEqual(ctx.harness_config["no_auth"]["behavior"], "allow")

    def test_env_keys_from_candidates(self):
        ctx = _make_ctx(candidates={"env_vars": ["KEY_A", "KEY_B"]})
        self.assertEqual(ctx.env_keys, {"KEY_A", "KEY_B"})

    def test_file_paths_from_candidates(self):
        ctx = _make_ctx(candidates={"files": [{"container_path": "/path/a"}, {"container_path": "/path/b"}]})
        self.assertEqual(ctx.file_paths, ["/path/a", "/path/b"])

    def test_model_resolution(self):
        manifest = {
            "harness_bundle_dir": "/tmp",
            "model_resolution": {"resolved_model": "claude-sonnet"},
        }
        ctx = sh.ProvisionContext("test", manifest)
        self.assertEqual(ctx.model_resolution["resolved_model"], "claude-sonnet")


# ---------------------------------------------------------------------------
# Original API preserved
# ---------------------------------------------------------------------------


class TestOriginalAPI(unittest.TestCase):
    """Ensure original functions still work."""

    def test_expand_path(self):
        with mock.patch.dict(os.environ, {"HOME": "/home/test"}):
            self.assertEqual(sh.expand_path("~/foo"), "/home/test/foo")

    def test_load_json(self):
        path = os.path.join(tempfile.mkdtemp(), "test.json")
        with open(path, "w") as f:
            json.dump({"a": 1}, f)
        self.assertEqual(sh.load_json(path), {"a": 1})

    def test_atomic_write_json(self):
        path = os.path.join(tempfile.mkdtemp(), "out.json")
        sh.atomic_write_json(path, {"b": 2})
        with open(path) as f:
            data = json.load(f)
        self.assertEqual(data, {"b": 2})

    def test_warn_outputs_to_stderr(self):
        import io
        with mock.patch("sys.stderr", new_callable=io.StringIO) as fake_stderr:
            sh.warn("test warning")
            self.assertIn("scion_harness: test warning", fake_stderr.getvalue())


if __name__ == "__main__":
    unittest.main()
