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
  - Thinking level resolution (parse/map/resolve_thinking)
"""
from __future__ import annotations

import json
import os
import random
import sys
import tempfile
import textwrap
import threading
import tomllib
import unittest
from typing import Any
from unittest import mock

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import scion_harness as sh


class TestVersionContract(unittest.TestCase):
    def test_interface_version(self):
        self.assertEqual(sh.INTERFACE_VERSION, 3)

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


class TestAuthExplicitTypeOverridesAutoDetect(unittest.TestCase):
    """Regression: explicit_type must bypass auto-detection priority ordering.

    When ANTHROPIC_API_KEY and GOOGLE_CLOUD_PROJECT are both present in
    candidates but explicit_type is "vertex-ai", select_auth must pick
    vertex-ai — not api-key (which would win under auto-detection because
    api-key has higher priority in the spec method list).
    """

    def _claude_spec(self) -> sh.AuthSpec:
        """Mirrors the AUTH spec from provision.py (Claude harness)."""
        return sh.AuthSpec("claude", [
            sh.env_method("api-key", any_of=["ANTHROPIC_API_KEY"]),
            sh.env_method("oauth-token", any_of=["CLAUDE_CODE_OAUTH_TOKEN"]),
            sh.env_method("vertex-ai",
                          all_of=["GOOGLE_CLOUD_PROJECT"],
                          any_of=["GOOGLE_CLOUD_LOCATION", "GOOGLE_CLOUD_REGION"]),
        ])

    def test_explicit_vertex_ai_wins_over_api_key(self):
        ctx = _make_ctx(candidates={
            "explicit_type": "vertex-ai",
            "env_vars": [
                "ANTHROPIC_API_KEY",
                "GOOGLE_CLOUD_PROJECT",
                "GOOGLE_CLOUD_REGION",
                "SCION_HARNESS_SELECTED_AUTH",
            ],
            "env_secret_files": {
                "ANTHROPIC_API_KEY": "/dev/null",
                "GOOGLE_CLOUD_PROJECT": "/dev/null",
                "GOOGLE_CLOUD_REGION": "/dev/null",
                "SCION_HARNESS_SELECTED_AUTH": "/dev/null",
            },
        })
        result = ctx.select_auth(self._claude_spec())
        self.assertEqual(result.method, "vertex-ai")

    def test_without_explicit_type_api_key_wins(self):
        """Verify auto-detection does pick api-key when explicit_type is empty."""
        ctx = _make_ctx(candidates={
            "env_vars": [
                "ANTHROPIC_API_KEY",
                "GOOGLE_CLOUD_PROJECT",
                "GOOGLE_CLOUD_REGION",
            ],
            "env_secret_files": {
                "ANTHROPIC_API_KEY": "/dev/null",
                "GOOGLE_CLOUD_PROJECT": "/dev/null",
                "GOOGLE_CLOUD_REGION": "/dev/null",
            },
        })
        result = ctx.select_auth(self._claude_spec())
        self.assertEqual(result.method, "api-key")


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

    def test_skills_excluded_by_default_and_included_explicitly(self):
        with tempfile.TemporaryDirectory() as home:
            skill_dir = os.path.join(home, ".test", "skills", "example")
            os.makedirs(skill_dir)
            with open(os.path.join(skill_dir, "SKILL.md"), "w") as f:
                f.write("# Example Skill\n\nUse this skill.")

            old_home = os.environ.get("HOME")
            os.environ["HOME"] = home
            try:
                ctx = _make_ctx(harness_config={"skills_dir": ".test/skills"})
                inputs = ctx.inputs_dir
                with open(os.path.join(inputs, "instructions.md"), "w") as f:
                    f.write("Do the thing.")

                default_target = os.path.join(home, "default.md")
                sh.project_instructions(ctx, default_target)
                with open(default_target) as f:
                    default_content = f.read()
                self.assertNotIn("# Skills", default_content)
                self.assertNotIn("# Example Skill", default_content)

                explicit_target = os.path.join(home, "explicit.md")
                sh.project_instructions(ctx, explicit_target, include_skills=True)
                with open(explicit_target) as f:
                    explicit_content = f.read()
                self.assertIn("# Skills", explicit_content)
                self.assertIn("# Example Skill", explicit_content)
            finally:
                if old_home is None:
                    os.environ.pop("HOME", None)
                else:
                    os.environ["HOME"] = old_home

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

    # --- Regression tests for the generalization-findings repro cases -----
    # (ptone/scion#2426 / generalization-findings.md "Reproduction of the
    # TOML fragility"). Each case pins one specific hardening the naive,
    # exact-string-match strip_toml_sections used to get wrong.

    def test_repro_1_header_with_trailing_comment_is_recognized(self):
        # Case 1: a header with a trailing comment (as a user or template
        # overlay might write) must still be recognized so re-appending the
        # same table doesn't produce a duplicate.
        content = '[mcp_servers.foo] # added by user\ncommand = "a"\n'
        result = sh.strip_toml_sections(
            content, lambda h: h.startswith("[mcp_servers.") and h.endswith("]")
        )
        appended = result.rstrip() + '\n\n[mcp_servers.foo]\ncommand = "b"\n'
        data = tomllib.loads(appended)
        self.assertEqual(data["mcp_servers"]["foo"]["command"], "b")

    def test_repro_2_nested_array_element_line_is_not_mistaken_for_header(self):
        # Case 2: a single-line nested array (e.g. `["a", "b"]`) inside a
        # multi-line array being stripped must not be mistaken for the next
        # table header — that used to stop the strip early, leaving
        # orphaned lines behind.
        content = (
            '[model.vertex-grok]\nmodel = "x"\nextra = [\n  ["a", "b"]\n]\n'
            'base_url = "u"\n\n[other]\nk = 1\n'
        )
        result = sh.strip_toml_sections(content, lambda h: h == "[model.vertex-grok]")
        data = tomllib.loads(result)
        self.assertNotIn("model", data)
        self.assertEqual(data["other"]["k"], 1)

    def test_repro_3_header_shaped_line_in_multiline_string_is_not_a_header(self):
        # Case 3: a header-shaped line inside a top-level multi-line string
        # was originally a documented residual gap of the line-oriented
        # strip_toml_sections (it isn't a full TOML tokenizer) — the file
        # could come out invalid. ptone/scion#2427 review round 1 (R1) added
        # multi-line-string tracking to toml_entering_array_depths, which
        # closes this specific gap as a side effect: the fake `[cli]` inside
        # the string is now correctly recognized as string content, not a
        # header, so only the real `[cli]` after the string closes is
        # stripped. See TestTomlEnteringArrayDepths for the unbalanced-
        # bracket-in-a-multi-line-string regression that R1 was actually
        # about, and TestWriteTomlIfPreserves for the narrower residual gap
        # (an escaped closing delimiter) still caught by the tomllib
        # backstop rather than by strip_toml_sections itself.
        content = 'note = """\n[cli]\nhello\n"""\n[cli]\nauto_update = true\n'
        result = sh.strip_toml_sections(content, lambda h: h == "[cli]")
        data = tomllib.loads(result)
        self.assertEqual(data["note"], "[cli]\nhello\n")
        self.assertNotIn("cli", data)

    def test_repro_4_header_whitespace_variant_is_recognized(self):
        # Case 4: legal header whitespace (`[ models ]`) must match a
        # predicate written against the canonical spelling (`[models]`).
        content = '[ models ]\ndefault = "mine"\n'
        result = sh.strip_toml_sections(content, lambda h: h == "[models]")
        appended = result.rstrip() + '\n\n[models]\ndefault = "vertex-grok"\n'
        data = tomllib.loads(appended)
        self.assertEqual(data["models"]["default"], "vertex-grok")

    def test_strip_toml_sections_handles_escaped_closing_delimiter(self):
        # ptone/scion#2427 review round 2, "Consider 2": an escaped closing-
        # delimiter sequence inside a multi-line *basic* string (TOML's way
        # to embed a literal triple-quote in the string body) must not
        # close the string early — a header-shaped line genuinely still
        # inside the string ("[cli]") must stay recognized as string
        # content, and only the real header after the string closes gets
        # stripped.
        content = (
            'note = """\n'
            'literal triple quote: \\"""\n'
            "[cli]\n"
            "more text\n"
            '"""\n'
            "[cli]\n"
            "auto_update = true\n"
        )
        result = sh.strip_toml_sections(content, lambda h: h == "[cli]")
        data = tomllib.loads(result)
        self.assertNotIn("cli", data)
        self.assertEqual(
            data["note"],
            'literal triple quote: """\n[cli]\nmore text\n',
        )


class TestTomlMaskStringsAndComments(unittest.TestCase):
    def test_blanks_basic_string(self):
        self.assertEqual(sh.toml_mask_strings_and_comments('hint = "press [ to go"'), 'hint = ""')

    def test_blanks_literal_string(self):
        self.assertEqual(sh.toml_mask_strings_and_comments("hint = 'press [ to go'"), 'hint = ""')

    def test_strips_trailing_comment(self):
        self.assertEqual(sh.toml_mask_strings_and_comments("# temperature range [0, 1)"), "")

    def test_leaves_structural_brackets_alone(self):
        self.assertEqual(sh.toml_mask_strings_and_comments("[features]"), "[features]")


class TestTomlEnteringArrayDepths(unittest.TestCase):
    def test_flat_lines_stay_at_zero(self):
        lines = ["a = 1", "[table]", "b = 2"]
        self.assertEqual(sh.toml_entering_array_depths(lines), [0, 0, 0])

    def test_multiline_array_increases_depth_for_body_lines(self):
        lines = ["extra = [", '  "a",', '  "b"', "]", "next = 2"]
        self.assertEqual(sh.toml_entering_array_depths(lines), [0, 1, 1, 1, 0])

    def test_unbalanced_bracket_in_comment_does_not_leak_depth(self):
        # ptone/scion#2365 review round 3 regression: a stray "[" inside a
        # comment or string must not be counted, or it would poison the
        # depth for the rest of the file.
        lines = ["# temperature range [0, 1)", "[table]", "k = 1"]
        self.assertEqual(sh.toml_entering_array_depths(lines), [0, 0, 0])

    def test_unbalanced_bracket_in_string_does_not_leak_depth(self):
        lines = ['hint = "press [ to go"', "[table]", "k = 1"]
        self.assertEqual(sh.toml_entering_array_depths(lines), [0, 0, 0])

    # --- ptone/scion#2427 review round 1 (R1) ------------------------------
    # An unbalanced bracket inside a *multi-line* ('\"\"\"'/"'''") string
    # used to permanently corrupt the running depth for every later line in
    # the file, since the per-line-only masking had no memory of being
    # inside a string that opened on an earlier line.

    def test_unbalanced_bracket_in_basic_multiline_string_does_not_leak_depth(self):
        lines = [
            'developer_instructions = """',
            "Prefix tasks with [TODO or [WIP when unfinished.",
            '"""',
            "[cli]",
            "auto_update = true",
        ]
        depths = sh.toml_entering_array_depths(lines)
        # Lines 1-2 are inside the open string (entering depth is the
        # in-string sentinel); line 3, the real header, must see depth 0
        # again once the string has closed — not a residue of the
        # unbalanced brackets in line 1's prose.
        self.assertEqual(depths[0], 0)
        self.assertEqual(depths[1], sh._TOML_IN_MULTILINE_STRING)
        self.assertEqual(depths[2], sh._TOML_IN_MULTILINE_STRING)
        self.assertEqual(depths[3], 0)
        self.assertTrue(sh.is_toml_table_header(lines[3], depths[3]))

    def test_unbalanced_bracket_in_literal_multiline_string_does_not_leak_depth(self):
        lines = [
            "developer_instructions = '''",
            "Prefix tasks with [TODO or [WIP when unfinished.",
            "'''",
            "[cli]",
            "auto_update = true",
        ]
        depths = sh.toml_entering_array_depths(lines)
        self.assertEqual(depths[0], 0)
        self.assertEqual(depths[1], sh._TOML_IN_MULTILINE_STRING)
        self.assertEqual(depths[2], sh._TOML_IN_MULTILINE_STRING)
        self.assertEqual(depths[3], 0)
        self.assertTrue(sh.is_toml_table_header(lines[3], depths[3]))

    def test_strip_toml_sections_still_finds_header_after_unbalanced_multiline_string(self):
        # End-to-end version: strip_toml_sections must still find and strip
        # the real [cli] section after a multi-line string containing an
        # unbalanced bracket, not silently stop stripping for the rest of
        # the file.
        content = (
            'developer_instructions = """\n'
            "Prefix tasks with [TODO or [WIP when unfinished.\n"
            '"""\n'
            "[cli]\n"
            "auto_update = true\n"
            "[other]\n"
            "k = 1\n"
        )
        result = sh.strip_toml_sections(content, lambda h: h == "[cli]")
        data = tomllib.loads(result)
        self.assertNotIn("cli", data)
        self.assertEqual(data["other"]["k"], 1)

    # --- ptone/scion#2427 review round 2, "Consider 2": escaped
    # closing-delimiter handling in a multi-line basic string. -----------

    def test_escaped_closing_delimiter_does_not_close_basic_multiline_string(self):
        lines = [
            'note = """',
            'literal triple quote: \\"""',
            "[cli]",
            '"""',
            "[other]",
            "k = 1",
        ]
        depths = sh.toml_entering_array_depths(lines)
        # Line 1 (the escaped delimiter) and line 2 (the fake header) are
        # both still inside the open string; only line 4 ([other]) is a
        # real header.
        self.assertEqual(depths[0], 0)
        self.assertEqual(depths[1], sh._TOML_IN_MULTILINE_STRING)
        self.assertEqual(depths[2], sh._TOML_IN_MULTILINE_STRING)
        self.assertEqual(depths[4], 0)
        self.assertTrue(sh.is_toml_table_header(lines[4], depths[4]))

    def test_escaped_delimiter_handling_is_specific_to_basic_strings(self):
        # Literal multi-line strings ('''...''') have no escapes at all in
        # TOML — a backslash there is just a literal backslash character,
        # not an escape, so the same handling must not apply to them.
        lines = [
            "note = '''",
            "a backslash: \\",
            "'''",
            "[other]",
            "k = 1",
        ]
        depths = sh.toml_entering_array_depths(lines)
        self.assertEqual(depths[1], sh._TOML_IN_MULTILINE_STRING)
        self.assertEqual(depths[3], 0)
        self.assertTrue(sh.is_toml_table_header(lines[3], depths[3]))

    def test_escaped_closing_delimiter_on_the_opening_line_does_not_close_early(self):
        # ptone/scion#2427 review round 4 (R4-1): the round-2 escape fix
        # only applied to the state-scan branch used for lines that start
        # *already inside* an open string. The opening branch (a line that
        # starts outside any string and opens one) searched for the closing
        # delimiter with a separate, non-escape-aware `line.find`, so an
        # escaped closing-delimiter sequence on the *same line that opens
        # the string* still closed it early — the identical bug Consider 2
        # closed for every other line, just not this one. Fixed by having
        # the opening branch hand off to the escape-aware state-scan branch
        # instead of duplicating the search.
        lines = [
            'a = """foo \\""" bar',
            "[fake]",
            '"""',
            "[cli]",
            "x = 1",
        ]
        depths = sh.toml_entering_array_depths(lines)
        self.assertEqual(depths, [0, sh._TOML_IN_MULTILINE_STRING, sh._TOML_IN_MULTILINE_STRING, 0, 0])
        result = sh.strip_toml_sections("\n".join(lines) + "\n", lambda h: h == "[cli]")
        data = tomllib.loads(result)
        self.assertNotIn("cli", data)
        self.assertEqual(data["a"], 'foo """ bar\n[fake]\n')

    # --- ptone/scion#2427 review round 5 (R5-1): 1-2 extra content quotes
    # immediately before a closing delimiter (e.g. `""""`, `'''''`). ------

    def test_extra_quote_before_basic_closing_delimiter_with_later_string_on_same_line(self):
        # Per TOML 1.0, `"""say "hi""""` is the content `say "hi"` closed
        # by the *last* three quotes — one extra `"` of content immediately
        # precedes the real closing delimiter. Naively stopping after the
        # first 3-quote run leaves that extra `"` to be mis-scanned as
        # opening a new single-line string, mis-pairing it with the next
        # `"` on the line (here, the start of the second list element) and
        # miscounting the `[` inside that element as real structure.
        content = 'banned = ["""say "hi"""", "[x"]\n[cli]\nauto_update = true\n'
        self.assertEqual(tomllib.loads(content)["banned"], ['say "hi"', "[x"])
        depths = sh.toml_entering_array_depths(content.split("\n"))
        self.assertEqual(depths, [0, 0, 0, 0])
        result = sh.strip_toml_sections(content, lambda h: h == "[cli]")
        data = tomllib.loads(result)
        self.assertNotIn("cli", data)
        self.assertEqual(data["banned"], ['say "hi"', "[x"])

    def test_extra_quote_before_literal_closing_delimiter_with_later_comment_on_same_line(self):
        # The ''' equivalent: `'''Always answer with 'OK''''` is the
        # content `Always answer with 'OK'` closed by the last three
        # quotes. The leftover `'` used to pair with the next `'` in the
        # trailing comment's `'['` fragment, hiding the comment's `[` from
        # view — harmlessly here, since it's already inside a comment, but
        # it corrupted depth for the rest of the file all the same.
        content = (
            "note = '''Always answer with 'OK'''' # type '[' to open a menu\n"
            "[cli]\nauto_update = true\n"
        )
        self.assertEqual(tomllib.loads(content)["note"], "Always answer with 'OK'")
        depths = sh.toml_entering_array_depths(content.split("\n"))
        self.assertEqual(depths, [0, 0, 0, 0])
        result = sh.strip_toml_sections(content, lambda h: h == "[cli]")
        data = tomllib.loads(result)
        self.assertNotIn("cli", data)
        self.assertEqual(data["note"], "Always answer with 'OK'")


class TestIsTomlTableHeader(unittest.TestCase):
    def test_recognizes_simple_header_at_depth_zero(self):
        self.assertTrue(sh.is_toml_table_header("[cli]", 0))

    def test_rejects_header_shape_when_depth_nonzero(self):
        # A `["x"]`-shaped line is indistinguishable from a header by shape
        # alone; depth tracking is what tells them apart.
        self.assertFalse(sh.is_toml_table_header('["x"]', 1))

    def test_rejects_non_header_line(self):
        self.assertFalse(sh.is_toml_table_header('key = "value"', 0))

    def test_rejects_array_continuation_line_with_trailing_comma(self):
        self.assertFalse(sh.is_toml_table_header('  ["x"],', 1))

    def test_accepts_double_bracket_header(self):
        self.assertTrue(sh.is_toml_table_header("[[hooks]]", 0))

    def test_accepts_quoted_key_containing_equals_and_bracket(self):
        # ptone/scion#2365 review round 3 (N1): a quoted table-header key
        # may itself contain `=` or `]` (e.g. a filesystem path).
        self.assertTrue(sh.is_toml_table_header('[projects."/a=b]c"]', 0))


class TestNormalizeTomlHeader(unittest.TestCase):
    def test_returns_none_for_non_header(self):
        self.assertIsNone(sh.normalize_toml_header('key = "value"'))

    def test_strips_trailing_comment(self):
        self.assertEqual(
            sh.normalize_toml_header("[mcp_servers.foo] # added by user"),
            "[mcp_servers.foo]",
        )

    def test_strips_whitespace_around_bracket_and_dots(self):
        self.assertEqual(sh.normalize_toml_header("[ models ]"), "[models]")

    def test_preserves_whitespace_inside_quoted_key(self):
        self.assertEqual(sh.normalize_toml_header('["a b"]'), '["a b"]')

    def test_preserves_double_bracket(self):
        self.assertEqual(sh.normalize_toml_header("[[ hooks ]]"), "[[hooks]]")

    def test_preserves_quoted_key_with_special_chars(self):
        self.assertEqual(
            sh.normalize_toml_header('[projects."/a=b]c"]'),
            '[projects."/a=b]c"]',
        )


class TestStripTomlTopLevelKey(unittest.TestCase):
    def test_removes_top_level_key(self):
        content = 'model = "old"\nother = 1\n'
        result = sh.strip_toml_top_level_key(content, "model")
        self.assertNotIn('model = "old"', result)
        self.assertIn("other = 1", result)

    def test_leaves_table_scoped_key_of_same_name_alone(self):
        content = '[otel]\nreasoning_effort = "low"\n[other]\nkey = "val"\n'
        result = sh.strip_toml_top_level_key(content, "reasoning_effort")
        self.assertIn('reasoning_effort = "low"', result)

    def test_does_not_match_prefixed_key(self):
        content = 'reasoning_effort = "low"\nreasoning_effort_extended = "yes"\n'
        result = sh.strip_toml_top_level_key(content, "reasoning_effort")
        self.assertNotIn('reasoning_effort = "low"', result)
        self.assertIn('reasoning_effort_extended = "yes"', result)

    def test_ignores_nested_array_line_that_looks_like_a_header(self):
        # Mirrors the insert_toml_top_level_line regression below: a
        # top-level key placed after a multi-line array (but before any
        # real table header) must still be recognized and stripped.
        content = 'notify = [\n  "sh",\n  ["x"],\n]\nmodel = "stale"\n[features]\nhooks = true\n'
        result = sh.strip_toml_top_level_key(content, "model")
        data = tomllib.loads(result)
        self.assertNotIn("model", data)
        self.assertEqual(data["notify"], ["sh", ["x"]])
        self.assertEqual(data["features"], {"hooks": True})

    def test_ignores_prose_line_inside_multiline_string_that_starts_with_the_key(self):
        # ptone/scion#2427 review round 2, "Consider 1": a prose line like
        # "model choice matters." inside a multi-line string must not be
        # mistaken for a top-level `model = ...` assignment and stripped
        # from the string's body, just because is_toml_key_line only looks
        # at the line's own text. The line's entering depth is the
        # multi-line-string sentinel, which now gates the key-line check
        # the same way it already gates header detection.
        content = (
            'developer_instructions = """\n'
            "model choice matters.\n"
            '"""\n'
            "[features]\n"
            "hooks = true\n"
        )
        result = sh.strip_toml_top_level_key(content, "model")
        data = tomllib.loads(result)
        self.assertEqual(data["developer_instructions"], "model choice matters.\n")
        self.assertEqual(data["features"], {"hooks": True})


class TestInsertTomlTopLevelLine(unittest.TestCase):
    def test_appends_when_no_table_header(self):
        result = sh.insert_toml_top_level_line('other_key = "value"\n', 'model = "x"')
        self.assertEqual(result, 'other_key = "value"\n\nmodel = "x"')

    def test_lands_before_first_table_header(self):
        content = 'other_key = "value"\n[features]\nhooks = true\n'
        result = sh.insert_toml_top_level_line(content, 'model = "x"')
        lines = result.split("\n")
        self.assertLess(lines.index('model = "x"'), lines.index("[features]"))

    def test_ignores_nested_array_line_with_trailing_comma(self):
        # ptone/scion#2365 review round 2 (N2): a top-level multi-line array
        # whose element is itself an array on its own line (`["x"],`) is
        # syntactically indistinguishable by shape alone from a quoted-key
        # table header (`["x"]`). Without bracket-depth tracking, this line
        # was misidentified as a header and the inserted key landed inside
        # the array, breaking the file.
        content = 'notify = [\n  "sh",\n  ["x"],\n]\n[features]\nhooks = true\n'
        result = sh.insert_toml_top_level_line(content, 'model = "y"')
        data = tomllib.loads(result)
        self.assertEqual(data["model"], "y")
        self.assertEqual(data["notify"], ["sh", ["x"]])
        self.assertEqual(data["features"], {"hooks": True})

    def test_ignores_nested_array_line_without_trailing_comma(self):
        content = 'notify = [\n  "sh",\n  ["x"]\n]\n[features]\nhooks = true\n'
        result = sh.insert_toml_top_level_line(content, 'model = "y"')
        data = tomllib.loads(result)
        self.assertEqual(data["model"], "y")
        self.assertEqual(data["notify"], ["sh", ["x"]])
        self.assertEqual(data["features"], {"hooks": True})


class TestTomlEditPreserves(unittest.TestCase):
    def test_accepts_generator_managed_keys_with_multiple_entries(self):
        # ptone/scion#2427 review round 2 (typing nit): managed_keys is
        # iterated more than once internally, so a one-shot generator must
        # not be silently exhausted after the first pass — that would make
        # every managed key after the first look unmanaged and over-reject
        # the edit.
        self.assertTrue(
            sh.toml_edit_preserves(
                "other_key = 1\n",
                'other_key = 1\nmodel = "x"\n[otel]\nenabled = true\n',
                (k for k in ("model", "otel")),
            )
        )

    def test_accepts_generator_managed_keys_mixing_bare_key_and_key_path(self):
        # A generator mixing a bare top-level key (consumed while building
        # the internal `top_level` set on the first pass) and a key-path
        # (only found on the later `paths` pass): without materializing
        # managed_keys up front, the bare-string comprehension alone
        # exhausts a one-shot generator, so the key-path is silently
        # dropped from `paths` and its subtree looks unmanaged, causing an
        # otherwise-safe edit to be over-rejected.
        self.assertTrue(
            sh.toml_edit_preserves(
                "[model.a]\nx = 1\n",
                '[model.a]\nx = 1\n[model.b]\ny = 2\n[otel]\nenabled = true\n',
                (k for k in ("otel", ("model", "b"))),
            )
        )

    def test_accepts_valid_content_with_managed_keys_changed(self):
        self.assertTrue(
            sh.toml_edit_preserves(
                "other_key = 1\n",
                'other_key = 1\nmodel = "x"\n',
                {"model"},
            )
        )

    def test_rejects_invalid_toml(self):
        self.assertFalse(sh.toml_edit_preserves("", "model = [unterminated\n"))

    def test_true_when_original_unparseable(self):
        # No parseable baseline to diff against — only "does content parse"
        # applies.
        self.assertTrue(sh.toml_edit_preserves("not [valid toml", 'model = "x"\n'))

    def test_rejects_unmanaged_key_changed(self):
        self.assertFalse(
            sh.toml_edit_preserves(
                'other_key = "before"\n',
                'other_key = "after"\nmodel = "x"\n',
                {"model"},
            )
        )

    def test_default_managed_keys_is_empty(self):
        # With no managed_keys supplied, every top-level key must be
        # unchanged for the edit to be considered safe.
        self.assertFalse(sh.toml_edit_preserves('k = 1\n', 'k = 1\nnew_key = 2\n'))
        self.assertTrue(sh.toml_edit_preserves('k = 1\n', 'k = 1\n'))

    def test_ignores_nested_contents_of_a_managed_key(self):
        # Managed-ness is checked at top-level-key granularity: a nested
        # table under a managed top-level key can change freely.
        self.assertTrue(
            sh.toml_edit_preserves(
                '[model.a]\nx = 1\n',
                '[model.a]\nx = 1\n[model.b]\ny = 2\n',
                {"model"},
            )
        )

    # --- Key-path managed keys (ptone/scion#2427 review round 1, "Consider") ---
    # A bare top-level key like "model" lets a write silently delete an
    # unrelated sibling sub-table (e.g. a user's own [model.custom]) if
    # strip_toml_sections' section boundary ever miscounted. A key-path
    # (a tuple) narrows the exemption to only the sub-table a write
    # actually owns.

    def test_key_path_allows_only_its_own_subtable_to_change(self):
        self.assertTrue(
            sh.toml_edit_preserves(
                '[model.a]\nx = 1\n',
                '[model.a]\nx = 2\n',
                {("model", "a")},
            )
        )

    def test_key_path_rejects_change_to_sibling_subtable(self):
        # The write only owns ("model", "a"); a sibling [model.custom]
        # changing (or vanishing) must still be caught.
        self.assertFalse(
            sh.toml_edit_preserves(
                '[model.a]\nx = 1\n[model.custom]\ny = 1\n',
                '[model.a]\nx = 2\n[model.custom]\ny = 2\n',
                {("model", "a")},
            )
        )

    def test_key_path_rejects_sibling_subtable_removed_entirely(self):
        self.assertFalse(
            sh.toml_edit_preserves(
                '[model.a]\nx = 1\n[model.custom]\ny = 1\n',
                '[model.a]\nx = 2\n',
                {("model", "a")},
            )
        )

    def test_key_path_first_write_with_no_parent_table_yet(self):
        # The very first write to a file with no top-level "model" table at
        # all must not be rejected just because the managed sub-table now
        # exists — an emptied-out parent (an artifact of dropping the one
        # sub-table this write owns) must compare equal to "absent".
        self.assertTrue(
            sh.toml_edit_preserves(
                "",
                '[model.a]\nx = 1\n',
                {("model", "a")},
            )
        )

    def test_key_path_sibling_subtable_survives_first_write(self):
        # Same as above, but a sibling sub-table already exists under the
        # shared parent table — it must be required to survive unchanged.
        self.assertTrue(
            sh.toml_edit_preserves(
                '[model.custom]\ny = 1\n',
                '[model.custom]\ny = 1\n[model.a]\nx = 1\n',
                {("model", "a")},
            )
        )
        self.assertFalse(
            sh.toml_edit_preserves(
                '[model.custom]\ny = 1\n',
                '[model.a]\nx = 1\n',
                {("model", "a")},
            )
        )


class TestWriteTomlIfPreserves(unittest.TestCase):
    def _ctx(self) -> tuple["sh.ProvisionContext", list[str]]:
        ctx = sh.ProvisionContext("test", {})
        warnings: list[str] = []
        ctx.warn = warnings.append  # type: ignore[method-assign]
        return ctx, warnings

    def test_writes_when_edit_preserves(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = os.path.join(tmp, "config.toml")
            ctx, warnings = self._ctx()
            wrote = sh.write_toml_if_preserves(
                ctx, path, "", 'model = "x"\n', managed_keys={"model"}
            )
            self.assertTrue(wrote)
            with open(path, encoding="utf-8") as f:
                self.assertEqual(f.read(), 'model = "x"\n')
            self.assertEqual(warnings, [])

    def test_leaves_file_untouched_and_warns_on_rejected_edit(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = os.path.join(tmp, "config.toml")
            original = 'other_key = "before"\n'
            with open(path, "w", encoding="utf-8") as f:
                f.write(original)
            ctx, warnings = self._ctx()
            wrote = sh.write_toml_if_preserves(
                ctx,
                path,
                original,
                'other_key = "corrupted"\nmodel = "x"\n',
                managed_keys={"model"},
            )
            self.assertFalse(wrote)
            with open(path, encoding="utf-8") as f:
                self.assertEqual(f.read(), original)
            self.assertEqual(len(warnings), 1)
            self.assertIn(path, warnings[0])

    def test_warning_names_the_what_label_and_managed_keys(self):
        # ptone/scion#2427 review round 1 (R2): the warning on a rejected
        # write must say *what* failed and *what it owned*, not just that
        # some generic TOML edit was rejected — otherwise every call site's
        # failure looks identical in the logs.
        with tempfile.TemporaryDirectory() as tmp:
            path = os.path.join(tmp, "config.toml")
            original = 'other_key = "before"\n'
            with open(path, "w", encoding="utf-8") as f:
                f.write(original)
            ctx, warnings = self._ctx()
            wrote = sh.write_toml_if_preserves(
                ctx,
                path,
                original,
                'other_key = "corrupted"\nmodel = "x"\n',
                managed_keys={"model", ("auth_provider", "vertex-grok")},
                what="vertex-ai auth/model config",
            )
            self.assertFalse(wrote)
            self.assertEqual(len(warnings), 1)
            self.assertIn("vertex-ai auth/model config", warnings[0])
            self.assertIn("model", warnings[0])
            self.assertIn("auth_provider.vertex-grok", warnings[0])
            self.assertIn("NOT applied", warnings[0])

    def test_warning_names_all_managed_keys_when_given_a_generator(self):
        # ptone/scion#2427 review round 2 (typing nit): write_toml_if_preserves
        # iterates managed_keys again (separately from toml_edit_preserves)
        # to build the warning message on a rejected write — a one-shot
        # generator must not come out already exhausted by that point.
        with tempfile.TemporaryDirectory() as tmp:
            path = os.path.join(tmp, "config.toml")
            original = 'other_key = "before"\n'
            with open(path, "w", encoding="utf-8") as f:
                f.write(original)
            ctx, warnings = self._ctx()
            wrote = sh.write_toml_if_preserves(
                ctx,
                path,
                original,
                'other_key = "corrupted"\nmodel = "x"\n',
                managed_keys=(k for k in ("model", ("auth_provider", "vertex-grok"))),
                what="vertex-ai auth/model config",
            )
            self.assertFalse(wrote)
            self.assertEqual(len(warnings), 1)
            self.assertIn("model", warnings[0])
            self.assertIn("auth_provider.vertex-grok", warnings[0])

    def test_warning_has_a_sensible_default_when_what_is_omitted(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = os.path.join(tmp, "config.toml")
            original = 'other_key = "before"\n'
            with open(path, "w", encoding="utf-8") as f:
                f.write(original)
            ctx, warnings = self._ctx()
            wrote = sh.write_toml_if_preserves(
                ctx, path, original, 'other_key = "corrupted"\nmodel = "x"\n', managed_keys={"model"}
            )
            self.assertFalse(wrote)
            self.assertEqual(len(warnings), 1)
            self.assertIn(path, warnings[0])

    def test_backstop_rejects_edit_that_drops_an_unmanaged_section(self):
        # Defense-in-depth, independent of any specific strip_toml_sections
        # gap: both previously-documented residual gaps are closed as of
        # ptone/scion#2427 review rounds 1 and 2 (multi-line-string bracket
        # tracking, and escaped closing-delimiter handling — see
        # TestTomlEnteringArrayDepths and
        # test_strip_toml_sections_handles_escaped_closing_delimiter below).
        # Even so, if a future bug in the line-oriented editor ever dropped
        # more than a caller's predicate asked for, the tomllib round-trip
        # backstop must still catch it and leave the file on disk untouched
        # rather than write the damage.
        original = 'note = "keep me"\n[cli]\nauto_update = true\n[otel]\nenabled = true\n'
        # Simulates a hypothetical strip bug: stripping [otel] also (wrongly)
        # dropped the unrelated [cli] section.
        content = 'note = "keep me"\n[otel]\nenabled = false\n'
        with tempfile.TemporaryDirectory() as tmp:
            path = os.path.join(tmp, "config.toml")
            with open(path, "w", encoding="utf-8") as f:
                f.write(original)
            ctx, warnings = self._ctx()
            wrote = sh.write_toml_if_preserves(ctx, path, original, content, managed_keys={"otel"})
            self.assertFalse(wrote)
            with open(path, encoding="utf-8") as f:
                self.assertEqual(f.read(), original)
            self.assertEqual(len(warnings), 1)


# ---------------------------------------------------------------------------
# Seeded, bounded scanner fuzz test (ptone/scion#2427 review round 5,
# "Consider"). Three consecutive review rounds each found a scanner edge
# by hand (an unbalanced bracket in a multi-line string, an escaped
# closing-delimiter sequence, and the same sequence on the string's
# opening line, and 1-2 extra quote characters before a closing
# delimiter). A small, deterministic fuzz test that runs on every test
# invocation is meant to catch the next one automatically instead of
# waiting for the next manual review round.
#
# This is trimmed from the reviewer's own fuzzer
# (/scion-volumes/scratchpad/projects/toml-harden/fuzz/fuzz.py): same
# fragment grammar, but the oracle here checks strip_toml_sections'
# *output* against a fresh tomllib parse (remove the same key from the
# original's own parsed data and compare) rather than comparing detected
# headers against the generator's own bookkeeping of where it inserted
# headers. The bookkeeping approach has a known class of false positive:
# a value fragment can coincidentally form text that, once a preceding
# multi-line string correctly closes, tomllib itself treats as a genuine
# extra top-level table the generator didn't intend — confirmed by loading
# such a case directly with tomllib. The strip/tomllib oracle used here
# doesn't have that failure mode, since it never trusts anything but
# tomllib's own parse of the input and the output.
#
# Deliberately does not fuzz strip_toml_top_level_key: it only handles
# single-line values (removes exactly the `key = ...` line), so a
# multi-line array or string value under a stripped key leaves orphan
# lines — pre-existing behavior, caught by the tomllib backstop at every
# real call site, and out of scope for this PR (ptone/scion#2427 review
# round 5 FYI, declined).
# ---------------------------------------------------------------------------


class TestTomlScannerFuzz(unittest.TestCase):
    _BASIC_FRAGMENTS = [
        "x", "[", "]", "[fake]", '\\"""', "\\\\", '\\"', '""', '"',
        "'''", "'", "#", "\n", "\\\n", "\n[fake2]\n", " model = 1", '\\""',
        "{", ",",
    ]
    _LITERAL_FRAGMENTS = [
        "x", "[", "]", "[fake]", '"""', "\\", "''", "'", "#", "\n",
        "\n[fake3]\n", '"', '\\"',
    ]
    _SHORT_BASIC_FRAGMENTS = ["x", "[", "]", "'''", '\\"', "\\\\", "'", "#", "{"]
    _SHORT_LITERAL_FRAGMENTS = ["x", "[", "]", '"""', "\\", '"', "#"]
    _KEYS = ["a", "b_c", '"q[k]"']
    _COMMENTS = ["", " # c", ' # """', " # '''", " # [x", " # ]"]

    def _multiline_basic(self, rng: random.Random) -> str:
        body = "".join(rng.choice(self._BASIC_FRAGMENTS) for _ in range(rng.randint(0, 6)))
        return '"""' + body + rng.choice(['"""', '""""', '"""""'])

    def _multiline_literal(self, rng: random.Random) -> str:
        body = "".join(rng.choice(self._LITERAL_FRAGMENTS) for _ in range(rng.randint(0, 6)))
        return "'''" + body + rng.choice(["'''", "''''", "'''''"])

    def _short_basic(self, rng: random.Random) -> str:
        body = "".join(rng.choice(self._SHORT_BASIC_FRAGMENTS) for _ in range(rng.randint(0, 4)))
        return '"' + body + '"'

    def _short_literal(self, rng: random.Random) -> str:
        body = "".join(rng.choice(self._SHORT_LITERAL_FRAGMENTS) for _ in range(rng.randint(0, 4)))
        return "'" + body + "'"

    def _scalar(self, rng: random.Random, depth: int = 0) -> str:
        choice = rng.randint(0, 7 if depth < 2 else 5)
        if choice == 0:
            return self._multiline_basic(rng)
        if choice == 1:
            return self._multiline_literal(rng)
        if choice == 2:
            return self._short_basic(rng)
        if choice == 3:
            return self._short_literal(rng)
        if choice == 4:
            return str(rng.randint(0, 9))
        if choice == 5:
            return '"[x]"'
        if choice == 6:
            items = [self._scalar(rng, depth + 1) for _ in range(rng.randint(0, 3))]
            sep = rng.choice([", ", ",\n  ", ",\n"])
            return "[" + rng.choice(["", "\n  "]) + sep.join(items) + rng.choice(["", ",\n", "\n"]) + "]"
        items = [f"k{i} = {self._scalar(rng, depth + 1)}" for i in range(rng.randint(0, 2))]
        return "{" + ", ".join(items) + "}"

    def _doc(self, rng: random.Random) -> tuple[str, int]:
        """Returns (document_text, table_count); tables are named t0, t1, ..."""
        lines: list[str] = []
        used: set[str] = set()
        num_tables = 0

        def kv() -> None:
            key = rng.choice(self._KEYS)
            while key in used:
                key = key + "z"
            used.add(key)
            value = self._scalar(rng)
            lines.extend(f"{key} = {value}{rng.choice(self._COMMENTS)}".split("\n"))

        for _ in range(rng.randint(0, 3)):
            kv()
        for _ in range(rng.randint(1, 3)):
            used.clear()
            lines.append(f"[t{num_tables}]{rng.choice(self._COMMENTS)}")
            num_tables += 1
            for _ in range(rng.randint(0, 3)):
                kv()
        return "\n".join(lines), num_tables

    def test_strip_toml_sections_matches_tomllib_across_seeded_fuzz_corpus(self):
        # ~2000 generated documents from a fixed seed, a bit over half of
        # which are valid TOML (the grammar deliberately generates plenty
        # of invalid TOML too, which is simply skipped). Deterministic;
        # runs in well under 1 second.
        seeds = (1,)
        docs_per_seed = 2000
        tested = 0
        failures: list[tuple[str, str]] = []

        for seed in seeds:
            rng = random.Random(seed)
            for _ in range(docs_per_seed):
                text, num_tables = self._doc(rng)
                try:
                    original_data = tomllib.loads(text)
                except tomllib.TOMLDecodeError:
                    continue
                tested += 1
                target = f"t{rng.randrange(num_tables)}"
                stripped = sh.strip_toml_sections(text, lambda h, t=target: h == f"[{t}]")
                try:
                    stripped_data = tomllib.loads(stripped)
                except tomllib.TOMLDecodeError as e:
                    failures.append((f"seed={seed}: strip [{target}] produced invalid TOML: {e}", text))
                    continue
                expected = dict(original_data)
                expected.pop(target)
                if stripped_data != expected:
                    failures.append(
                        (
                            f"seed={seed}: strip [{target}] mismatch: "
                            f"got {stripped_data!r}, want {expected!r}",
                            text,
                        )
                    )

        self.assertGreater(tested, 0, "sanity: the fuzz corpus must produce at least one valid document")
        if failures:
            detail = "\n\n".join(f"{msg}\n{text!r}" for msg, text in failures[:5])
            self.fail(f"{len(failures)}/{tested} fuzzed documents failed:\n\n{detail}")


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


# ---------------------------------------------------------------------------
# Model resolution (G3)
# ---------------------------------------------------------------------------


class TestResolveModel(unittest.TestCase):
    def test_unset_returns_empty(self):
        ctx = _make_ctx(harness_config={"model_aliases": {"medium": "claude-sonnet"}})
        with mock.patch.dict(os.environ, {}, clear=False):
            os.environ.pop("SCION_MODEL", None)
            self.assertEqual(sh.resolve_model(ctx), "")

    def test_empty_string_returns_empty(self):
        ctx = _make_ctx(harness_config={"model_aliases": {"medium": "claude-sonnet"}})
        with mock.patch.dict(os.environ, {"SCION_MODEL": "   "}):
            self.assertEqual(sh.resolve_model(ctx), "")

    def test_tier_full_spelling(self):
        ctx = _make_ctx(harness_config={"model_aliases": {"medium": "claude-sonnet"}})
        with mock.patch.dict(os.environ, {"SCION_MODEL": "medium"}):
            self.assertEqual(sh.resolve_model(ctx), "claude-sonnet")

    def test_tier_shorthand_letter(self):
        ctx = _make_ctx(harness_config={"model_aliases": {"medium": "claude-sonnet"}})
        with mock.patch.dict(os.environ, {"SCION_MODEL": "m"}):
            self.assertEqual(sh.resolve_model(ctx), "claude-sonnet")

    def test_tier_shorthand_xl(self):
        ctx = _make_ctx(harness_config={"model_aliases": {"extra-large": "claude-opus"}})
        with mock.patch.dict(os.environ, {"SCION_MODEL": "xl"}):
            self.assertEqual(sh.resolve_model(ctx), "claude-opus")

    def test_tier_case_insensitive(self):
        ctx = _make_ctx(harness_config={"model_aliases": {"large": "claude-opus"}})
        with mock.patch.dict(os.environ, {"SCION_MODEL": "LARGE"}):
            self.assertEqual(sh.resolve_model(ctx), "claude-opus")

    def test_concrete_model_passes_through(self):
        ctx = _make_ctx(harness_config={"model_aliases": {"medium": "claude-sonnet"}})
        with mock.patch.dict(os.environ, {"SCION_MODEL": "claude-opus-4-8"}):
            self.assertEqual(sh.resolve_model(ctx), "claude-opus-4-8")

    def test_unknown_alias_passes_through_unchanged(self):
        """A non-tier value is concrete and must not be re-cased — it is not
        run through _MODEL_ALIAS_SHORTHAND/lower() for the return value, only
        for the known-tier check."""
        ctx = _make_ctx(harness_config={"model_aliases": {"medium": "claude-sonnet"}})
        with mock.patch.dict(os.environ, {"SCION_MODEL": "Nonexistent-Tier"}):
            self.assertEqual(sh.resolve_model(ctx), "Nonexistent-Tier")

    def test_missing_alias_table_passes_tier_through(self):
        ctx = _make_ctx(harness_config={})
        with mock.patch.dict(os.environ, {"SCION_MODEL": "medium"}):
            self.assertEqual(sh.resolve_model(ctx), "medium")

    def test_tier_not_in_alias_table_passes_through(self):
        ctx = _make_ctx(harness_config={"model_aliases": {"small": "claude-haiku"}})
        with mock.patch.dict(os.environ, {"SCION_MODEL": "medium"}):
            self.assertEqual(sh.resolve_model(ctx), "medium")

    def test_unmapped_tier_passes_through_normalized_not_raw(self):
        """R2 round-2 nit N1: an unmapped tier must fall back to the
        *normalized* tier name, not the caller's raw spelling — matching Go,
        which always returns the normalized form for a known tier. Mutation
        check: `aliases.get(normalized, raw)` instead of
        `aliases.get(normalized, normalized)` survives every other test here
        because they all pass an already-normalized tier spelling ("medium").
        This one uses shorthand plus mixed case ("M") to catch that mutant.
        """
        ctx = _make_ctx(harness_config={"model_aliases": {"small": "claude-haiku"}})
        with mock.patch.dict(os.environ, {"SCION_MODEL": "M"}):
            self.assertEqual(sh.resolve_model(ctx), "medium")

    def test_concrete_model_case_is_preserved(self):
        """R1: concrete (non-tier) model names must not be lower-cased.

        SCION_MODEL can arrive un-normalized from an explicit source Go never
        touches (a template/hub `env:` block, `--env SCION_MODEL=...`) — see
        run.go's reResolveModelAlias, which only rewrites tier names,  not
        concrete ones. Case-sensitive concrete IDs are real (e.g. OpenAI
        fine-tuned model suffixes), so resolve_model must preserve them
        exactly.
        """
        ctx = _make_ctx(harness_config={"model_aliases": {"medium": "claude-sonnet"}})
        case_sensitive_id = "ft:gpt-4o-mini-2024-07-18:my-org::AbC12xYz"
        with mock.patch.dict(os.environ, {"SCION_MODEL": case_sensitive_id}):
            self.assertEqual(sh.resolve_model(ctx), case_sensitive_id)

    def test_non_tier_alias_table_key_is_never_consulted(self):
        """N1: the known-tier gate must run before any model_aliases lookup.

        A model_aliases table may carry non-canonical keys (template authors
        occasionally add fast/cheap/etc. aliases of their own). SCION_MODEL
        matching one of those non-tier keys verbatim must not be rewritten —
        resolve_model only maps the four canonical tiers, never arbitrary
        table keys. Mutation check: deleting the
        `if normalized not in _KNOWN_MODEL_ALIASES` gate makes this fail,
        since "fast" would then resolve through the table to "x".
        """
        ctx = _make_ctx(harness_config={"model_aliases": {"fast": "x"}})
        with mock.patch.dict(os.environ, {"SCION_MODEL": "fast"}):
            self.assertEqual(sh.resolve_model(ctx), "fast")


# ---------------------------------------------------------------------------
# Thinking level resolution
# ---------------------------------------------------------------------------

_CODEX_THINKING = {
    "levels": [
        {"max": 25, "value": "low"},
        {"max": 50, "value": "medium"},
        {"max": 75, "value": "high"},
        {"max": 100, "value": "xhigh"},
    ],
    "default": "medium",
}


class TestParseThinkingLevel(unittest.TestCase):
    def test_unset(self):
        for raw in (None, "", "  ", "\t\n"):
            with self.subTest(raw=raw):
                self.assertEqual(sh.parse_thinking_level(raw), (None, False))

    def test_invalid(self):
        for raw in ("abc", "1.5", "5x", "--5", "1e2"):
            with self.subTest(raw=raw):
                self.assertEqual(sh.parse_thinking_level(raw), (None, True))

    def test_valid_and_clamped(self):
        cases = {
            "-5": 0,
            "0": 0,
            "150": 100,
            "100": 100,
            "+7": 7,
            " 50 ": 50,
            "25": 25,
        }
        for raw, want in cases.items():
            with self.subTest(raw=raw):
                self.assertEqual(sh.parse_thinking_level(raw), (want, False))


class TestMapThinkingLevel(unittest.TestCase):
    _CASES = [
        (0, "low"),
        (25, "low"),
        (26, "medium"),
        (50, "medium"),
        (51, "high"),
        (75, "high"),
        (76, "xhigh"),
        (100, "xhigh"),
    ]

    def test_codex_table(self):
        for level, want in self._CASES:
            with self.subTest(level=level):
                self.assertEqual(sh.map_thinking_level(level, _CODEX_THINKING), want)

    def test_unsorted_levels_same_result(self):
        shuffled = dict(_CODEX_THINKING)
        shuffled["levels"] = list(reversed(_CODEX_THINKING["levels"]))
        for level, want in self._CASES:
            with self.subTest(level=level):
                self.assertEqual(sh.map_thinking_level(level, shuffled), want)

    def test_above_last_max_returns_last_value(self):
        cfg = {"levels": [{"max": 10, "value": "low"}, {"max": 60, "value": "high"}]}
        self.assertEqual(sh.map_thinking_level(99, cfg), "high")

    def test_missing_or_malformed_returns_none(self):
        malformed = [
            None,
            {},
            "levels",
            {"levels": []},
            {"levels": "low"},
            {"levels": ["low"]},
            {"levels": [{"max": "25", "value": "low"}]},
            {"levels": [{"max": True, "value": "low"}]},
            {"levels": [{"max": 25.0, "value": "low"}]},
            {"levels": [{"value": "low"}]},
            {"levels": [{"max": 100}]},
            {"levels": [{"max": 100, "value": ""}]},
            {"levels": [{"max": 100, "value": 3}]},
            {"levels": [{"max": 100, "value": "high"}], "default": ""},
            {"levels": [{"max": 100, "value": "high"}], "default": 5},
        ]
        for cfg in malformed:
            with self.subTest(cfg=cfg):
                self.assertIsNone(sh.map_thinking_level(50, cfg))


class TestResolveThinking(unittest.TestCase):
    def _ctx(self, harness_config: dict[str, Any] | None) -> tuple["sh.ProvisionContext", list[str], list[str]]:
        manifest: dict[str, Any] = {}
        if harness_config is not None:
            manifest["harness_config"] = harness_config
        ctx = sh.ProvisionContext("test", manifest)
        infos: list[str] = []
        warns: list[str] = []
        ctx.info = infos.append  # type: ignore[method-assign]
        ctx.warn = warns.append  # type: ignore[method-assign]
        return ctx, infos, warns

    def test_set_level_maps(self):
        ctx, infos, warns = self._ctx({"thinking": _CODEX_THINKING})
        self.assertEqual(sh.resolve_thinking(ctx, "60"), "high")
        self.assertEqual(infos, ["thinking_level=60 value=high"])
        self.assertEqual(warns, [])

    def test_set_level_clamped(self):
        ctx, infos, _ = self._ctx({"thinking": _CODEX_THINKING})
        self.assertEqual(sh.resolve_thinking(ctx, "-10"), "low")
        self.assertEqual(sh.resolve_thinking(ctx, "150"), "xhigh")
        self.assertEqual(infos, ["thinking_level=0 value=low", "thinking_level=100 value=xhigh"])

    def test_unset_with_default(self):
        ctx, infos, warns = self._ctx({"thinking": _CODEX_THINKING})
        self.assertEqual(sh.resolve_thinking(ctx, ""), "medium")
        self.assertEqual(infos, ["thinking_level=<unset>, value=medium (default)"])
        self.assertEqual(warns, [])

    def test_invalid_with_default_warns(self):
        ctx, infos, warns = self._ctx({"thinking": _CODEX_THINKING})
        self.assertEqual(sh.resolve_thinking(ctx, "abc"), "medium")
        self.assertEqual(warns, ["thinking_level='abc' is not a valid integer; value=medium (default)"])
        self.assertEqual(infos, [])

    def test_invalid_logs_stripped_value(self):
        ctx, _, warns = self._ctx({"thinking": _CODEX_THINKING})
        self.assertEqual(sh.resolve_thinking(ctx, "  abc \n"), "medium")
        self.assertEqual(warns, ["thinking_level='abc' is not a valid integer; value=medium (default)"])
        ctx, infos, _ = self._ctx({})
        self.assertIsNone(sh.resolve_thinking(ctx, " x "))
        self.assertEqual(infos, ["thinking_level='x' ignored (harness has no thinking map)"])

    def test_unset_without_default_returns_none(self):
        cfg = {"levels": [{"max": 100, "value": "high"}]}
        ctx, infos, warns = self._ctx({"thinking": cfg})
        self.assertIsNone(sh.resolve_thinking(ctx, "  "))
        self.assertEqual(infos, ["thinking_level=<unset>, value=<cli default> (default)"])
        self.assertEqual(warns, [])

    def test_invalid_without_default_returns_none(self):
        cfg = {"levels": [{"max": 100, "value": "high"}]}
        ctx, _, warns = self._ctx({"thinking": cfg})
        self.assertIsNone(sh.resolve_thinking(ctx, "1.5"))
        self.assertEqual(warns, ["thinking_level='1.5' is not a valid integer; value=<cli default> (default)"])

    def test_invalid_non_string_with_block_warns(self):
        ctx, infos, warns = self._ctx({"thinking": _CODEX_THINKING})
        self.assertEqual(sh.resolve_thinking(ctx, 3.5), "medium")  # type: ignore[arg-type]
        self.assertEqual(warns, ["thinking_level='3.5' is not a valid integer; value=medium (default)"])
        self.assertEqual(infos, [])

    def test_invalid_non_string_without_block_info(self):
        ctx, infos, warns = self._ctx({})
        self.assertIsNone(sh.resolve_thinking(ctx, 3.5))  # type: ignore[arg-type]
        self.assertEqual(infos, ["thinking_level='3.5' ignored (harness has no thinking map)"])
        self.assertEqual(warns, [])

    def test_no_block_set_level_info(self):
        ctx, infos, warns = self._ctx({"model_aliases": {}})
        self.assertIsNone(sh.resolve_thinking(ctx, "40"))
        self.assertEqual(infos, ["thinking_level=40 ignored (harness has no thinking map)"])
        self.assertEqual(warns, [])

    def test_no_block_no_manifest_config(self):
        ctx, infos, warns = self._ctx(None)
        self.assertIsNone(sh.resolve_thinking(ctx, ""))
        self.assertEqual(infos, [])
        self.assertEqual(warns, [])

    def test_malformed_block_warns_and_returns_none(self):
        ctx, infos, warns = self._ctx({"thinking": {"levels": [{"max": "x", "value": "low"}], "default": "low"}})
        self.assertIsNone(sh.resolve_thinking(ctx, "30"))
        self.assertEqual(len(warns), 1)
        self.assertIn("malformed", warns[0])
        self.assertEqual(infos, ["thinking_level=30 ignored (harness has no thinking map)"])

    def test_malformed_block_unset_level_returns_none_not_default(self):
        ctx, _, warns = self._ctx({"thinking": {"levels": [], "default": "medium"}})
        self.assertIsNone(sh.resolve_thinking(ctx, ""))
        self.assertEqual(len(warns), 1)

    def test_reads_env_when_raw_omitted(self):
        ctx, _, _ = self._ctx({"thinking": _CODEX_THINKING})
        with mock.patch.dict(os.environ, {sh.THINKING_LEVEL_ENV: "26"}):
            self.assertEqual(sh.resolve_thinking(ctx), "medium")
        with mock.patch.dict(os.environ, {}, clear=False):
            os.environ.pop(sh.THINKING_LEVEL_ENV, None)
            self.assertEqual(sh.resolve_thinking(ctx), "medium")

    def test_reads_block_from_manifest_json(self):
        manifest = json.loads(json.dumps({"harness_config": {"thinking": _CODEX_THINKING}}))
        ctx = sh.ProvisionContext("test", manifest)
        ctx.info = lambda _m: None  # type: ignore[method-assign]
        self.assertEqual(sh.resolve_thinking(ctx, "76"), "xhigh")


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


# ---------------------------------------------------------------------------
# atomic_write_json: refuses to write through a planted symlink or FIFO
# ---------------------------------------------------------------------------


class TestAtomicWriteJsonSymlinkGuards(unittest.TestCase):
    """atomic_write_json must never follow a symlink or block on a FIFO
    planted at either the parent directory or the temp file name, regardless
    of which uid calls it — this is the guard that protects every caller of
    the helper, not just the ones already careful about their own inputs.
    """

    def test_normal_write_creates_expected_content(self):
        directory = tempfile.mkdtemp()
        path = os.path.join(directory, "nested", "out.json")
        sh.atomic_write_json(path, {"b": 2, "a": 1})
        with open(path, encoding="utf-8") as f:
            content = f.read()
        # sort_keys=True, indent=2, trailing newline — the documented format,
        # unchanged by the guard.
        self.assertEqual(content, '{\n  "a": 1,\n  "b": 2\n}\n')

    def test_overwrite_replaces_content_atomically(self):
        path = os.path.join(tempfile.mkdtemp(), "out.json")
        sh.atomic_write_json(path, {"first": True})
        sh.atomic_write_json(path, {"second": True})
        with open(path, encoding="utf-8") as f:
            data = json.load(f)
        self.assertEqual(data, {"second": True})
        # No leftover temp file after a successful write: the directory must
        # contain exactly the final name, nothing else.
        self.assertEqual(os.listdir(os.path.dirname(path)), ["out.json"])

    def test_symlinked_parent_directory_is_refused(self):
        # Mirrors a workload committing ".agents" as a symlink to a directory
        # it does not own: writing hooks.json must never land inside that
        # target directory.
        root = tempfile.mkdtemp()
        sentinel_dir = os.path.join(root, "sentinel")
        os.makedirs(sentinel_dir)
        planted_parent = os.path.join(root, ".agents")
        os.symlink(sentinel_dir, planted_parent)

        with self.assertRaises(OSError):
            sh.atomic_write_json(os.path.join(planted_parent, "hooks.json"), {"x": 1})

        self.assertEqual(os.listdir(sentinel_dir), [])

    def test_symlinked_temp_target_is_refused_and_sentinel_untouched(self):
        # Mirrors a workload committing a symlink at the exact temp name
        # atomic_write_json is about to create: the write must never go
        # through that symlink. The real temp name is unique per call (pid +
        # a monotonic timestamp) precisely so this can't be predicted and
        # pre-planted from outside; _atomic_tmp_name is monkeypatched to a
        # fixed, known name so the test can still plant the adversarial entry
        # at the exact path this call will use, and prove O_EXCL still
        # refuses a pre-existing entry there rather than following or
        # truncating it.
        directory = tempfile.mkdtemp()
        fixed_tmp_name = ".hooks.json.tmp-fixed-for-test"
        sentinel_file = os.path.join(directory, "sentinel.txt")
        with open(sentinel_file, "w", encoding="utf-8") as f:
            f.write("original contents\n")
        os.symlink(sentinel_file, os.path.join(directory, fixed_tmp_name))

        with mock.patch.object(sh, "_atomic_tmp_name", return_value=fixed_tmp_name):
            with self.assertRaises(OSError):
                sh.atomic_write_json(os.path.join(directory, "hooks.json"), {"y": 1})

        with open(sentinel_file, encoding="utf-8") as f:
            self.assertEqual(f.read(), "original contents\n")
        self.assertFalse(os.path.exists(os.path.join(directory, "hooks.json")))

    def test_fifo_at_temp_target_does_not_hang(self):
        # Same predictability problem as the symlink test above: the FIFO
        # must sit at the exact name atomic_write_json will try to create, so
        # _atomic_tmp_name is monkeypatched to a fixed name for this call.
        directory = tempfile.mkdtemp()
        fixed_tmp_name = ".out.json.tmp-fixed-for-test"
        fifo_path = os.path.join(directory, fixed_tmp_name)
        os.mkfifo(fifo_path)

        result: dict[str, BaseException | None] = {"error": None}

        def call():
            try:
                with mock.patch.object(sh, "_atomic_tmp_name", return_value=fixed_tmp_name):
                    sh.atomic_write_json(os.path.join(directory, "out.json"), {"z": 1})
            except BaseException as exc:  # noqa: BLE001 - captured for the main thread
                result["error"] = exc

        thread = threading.Thread(target=call, daemon=True)
        thread.start()
        thread.join(timeout=5)
        self.assertFalse(
            thread.is_alive(),
            "atomic_write_json hung opening a pre-existing FIFO at the temp path",
        )
        self.assertIsInstance(result["error"], OSError)

    def test_stale_temp_file_does_not_block_a_later_write(self):
        # A leftover from an old, killed-mid-write process (or, under a fixed
        # ".tmp" naming scheme, simply the previous call's own temp file)
        # must never permanently block every subsequent write the way a
        # fixed temp name would: the unique-per-call name means a stale file
        # sitting at some OTHER call's old temp name is simply irrelevant to
        # this one.
        directory = tempfile.mkdtemp()
        path = os.path.join(directory, "out.json")
        with open(path + ".tmp", "w", encoding="utf-8") as f:
            f.write("leftover from a previous, interrupted write\n")

        sh.atomic_write_json(path, {"ok": True})

        with open(path, encoding="utf-8") as f:
            self.assertEqual(json.load(f), {"ok": True})

    def test_fdopen_failure_closes_fd_without_leaking(self):
        """If os.fdopen itself raises before wrapping fd in a file object,
        nothing else owns that raw fd number yet, so atomic_write_json must
        close it explicitly rather than leaking it.
        """
        directory = tempfile.mkdtemp()
        path = os.path.join(directory, "out.json")

        open_fds_before = set(os.listdir("/proc/self/fd"))
        with mock.patch("os.fdopen", side_effect=OSError("boom")):
            with self.assertRaises(OSError):
                sh.atomic_write_json(path, {"a": 1})
        open_fds_after = set(os.listdir("/proc/self/fd"))

        self.assertEqual(
            open_fds_before,
            open_fds_after,
            "atomic_write_json leaked a file descriptor when os.fdopen failed",
        )
        self.assertFalse(os.path.exists(path))

    def test_replace_failure_cleans_up_temp_file(self):
        """A failed os.replace (e.g. a cross-device rename, or the
        destination directory vanishing) must clean up the temp file the
        same way a failed write does — os.replace runs inside the same
        try/except as the write, not after it, so its own failure is
        covered by the identical cleanup-and-reraise path.
        """
        directory = tempfile.mkdtemp()
        path = os.path.join(directory, "out.json")

        with mock.patch("os.replace", side_effect=OSError("boom")):
            with self.assertRaises(OSError):
                sh.atomic_write_json(path, {"a": 1})

        self.assertEqual(
            os.listdir(directory),
            [],
            "a failed os.replace must not leave the temp file behind",
        )

    def test_temp_name_is_unique_per_call(self):
        # Pins the property the two tests above depend on: two calls in a
        # row never reuse the same temp name, so the second call's own
        # O_EXCL create can never spuriously collide with the first call's
        # (already-renamed-away) temp file.
        first = sh._atomic_tmp_name("out.json")
        second = sh._atomic_tmp_name("out.json")
        self.assertNotEqual(first, second)
        self.assertTrue(first.startswith(".out.json.tmp-"))
        self.assertTrue(second.startswith(".out.json.tmp-"))


if __name__ == "__main__":
    unittest.main()
