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

import os
import importlib.util
import tempfile
import tomllib
import unittest
from contextlib import contextmanager
from typing import Any
from unittest import mock

PROVISION_PATH = os.path.join(os.path.dirname(__file__), "provision.py")
SPEC = importlib.util.spec_from_file_location("codex_provision", PROVISION_PATH)
assert SPEC is not None
provision = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
SPEC.loader.exec_module(provision)

scion_harness = provision.scion_harness

MANAGED_BEGIN = "<!-- BEGIN SCION MANAGED -->"
MANAGED_END = "<!-- END SCION MANAGED -->"

LEGACY_BEGIN = "<!-- BEGIN SCION MANAGED CODEX INSTRUCTIONS -->"
LEGACY_END = "<!-- END SCION MANAGED CODEX INSTRUCTIONS -->"


# A scratch workspace for _test_ctx, so a context never falls back to the
# real /workspace (ptone/scion#2993). Created in setUpModule and removed in
# tearDownModule.
_scratch_workspace: tempfile.TemporaryDirectory[str] | None = None


def setUpModule() -> None:
    global _scratch_workspace
    _scratch_workspace = tempfile.TemporaryDirectory(prefix="codex-provision-test-")


def tearDownModule() -> None:
    global _scratch_workspace
    if _scratch_workspace is not None:
        _scratch_workspace.cleanup()
        _scratch_workspace = None


def _test_ctx() -> "scion_harness.ProvisionContext":
    """A minimal ProvisionContext for calling _reconcile_codex_toml directly
    in tests that don't otherwise need a full provision() manifest."""
    assert _scratch_workspace is not None, "setUpModule did not run"
    return scion_harness.ProvisionContext("codex", {
        "agent_workspace": os.path.join(_scratch_workspace.name, "workspace"),
    })


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
def temporary_env(name: str, value: str | None):
    """Temporarily set (or unset, with None) a single environment variable."""
    old = os.environ.get(name)
    if value is None:
        os.environ.pop(name, None)
    else:
        os.environ[name] = value
    try:
        yield
    finally:
        if old is None:
            os.environ.pop(name, None)
        else:
            os.environ[name] = old


# Mirrors harnesses/codex/config.yaml's `thinking:` block exactly. The Go
# test TestEmbeddedHarnessThinkingBlocks (harnesses/thinking_config_test.go)
# pins the yaml side to the same literal table, so the two cannot drift.
CODEX_THINKING = {
    "levels": [
        {"max": 25, "value": "low"},
        {"max": 50, "value": "medium"},
        {"max": 75, "value": "high"},
        {"max": 100, "value": "xhigh"},
    ],
    "default": "medium",
}

_DEFAULT_HARNESS_CONFIG: dict[str, Any] = {"thinking": CODEX_THINKING}


def _invoke_provision(
    thinking_level: str | None,
    harness_config: dict[str, Any] | None = None,
) -> tuple[str, list[str]]:
    """Run the real provision() entry point end to end and return the
    written ~/.codex/config.toml content plus every ctx.warn message.

    Modeled on telemetry_provision_test.py's _invoke: a temp HOME, a
    minimal bundle dir, auth forced to "none", and provision() itself
    invoked -- not just the helpers it calls -- so a revert of the
    SCION_THINKING_LEVEL wiring inside provision() actually fails the
    test (ptone/scion#2484 review round 1, R1). `thinking_level` is the
    raw (unstripped) env value; None means unset. `harness_config`
    defaults to one carrying config.yaml's thinking block, as the real
    manifest does.
    """
    if harness_config is None:
        harness_config = _DEFAULT_HARNESS_CONFIG
    warnings: list[str] = []
    with tempfile.TemporaryDirectory() as tmp:
        home = os.path.join(tmp, "home")
        bundle = os.path.join(home, ".scion", "harness")
        os.makedirs(os.path.join(bundle, "inputs"))
        with temporary_home(home), temporary_env("SCION_THINKING_LEVEL", thinking_level):
            ctx = scion_harness.ProvisionContext("codex", {
                "harness_bundle_dir": bundle,
                "harness_config": harness_config,
                "agent_workspace": os.path.join(tmp, "workspace"),
            })
            ctx.select_auth = lambda _: scion_harness.ResolvedAuth("none")
            ctx.warn = warnings.append  # type: ignore[method-assign]
            provision.provision(ctx)
        config_path = os.path.join(home, ".codex", "config.toml")
        with open(config_path, "r", encoding="utf-8") as f:
            return f.read(), warnings


@contextmanager
def _also_strip_key_damaging(header: str):
    """Force real damage for backstop tests (ptone/scion#2427 review round
    2, R2-b): monkeypatch scion_harness.strip_toml_top_level_key so every
    call to it *also* (wrongly) strips the top-level section `header` (an
    exact "[table]" string), simulating a bug in the line-oriented model/
    effort strip that corrupts something it doesn't own. Only affects the
    full-edit path (_reconcile_codex_toml's otel-only fallback rebuilds
    from the untouched original and never calls strip_toml_top_level_key),
    so this isolates "full edit gets rejected" from "fallback stays clean"
    without relying on any specific strip_toml_sections residual gap."""
    real_strip_key = scion_harness.strip_toml_top_level_key

    def damaging(content: str, key: str) -> str:
        content = real_strip_key(content, key)
        return scion_harness.strip_toml_sections(content, lambda h, hdr=header: h == hdr)

    with mock.patch.object(scion_harness, "strip_toml_top_level_key", side_effect=damaging):
        yield


@contextmanager
def _force_is_header(fake_header_line: str):
    """Force real damage (ptone/scion#2427 review round 2, R2-b) by making
    scion_harness.is_toml_table_header report True for the exact raw line
    `fake_header_line`, regardless of its actual depth — simulating a
    (currently hypothetical, since there are no known string-tracking gaps
    left as of review round 5) bug where a line that is really inside a
    multi-line string gets mistaken for a genuine table header. This is
    deterministic and doesn't depend on finding a real remaining parsing
    gap to trigger it."""
    real_is_header = scion_harness.is_toml_table_header

    def wrapper(line: str, depth: int) -> bool:
        if line == fake_header_line:
            return True
        return real_is_header(line, depth)

    with mock.patch.object(scion_harness, "is_toml_table_header", side_effect=wrapper):
        yield


@contextmanager
def _also_strip_sections_damaging(header: str):
    """Like _also_strip_key_damaging, but for call sites that only use
    strip_toml_sections (e.g. _write_mcp_to_config): every call also
    (wrongly) strips `header` in addition to whatever the real predicate
    matches."""
    real_strip = scion_harness.strip_toml_sections

    def damaging(content: str, predicate) -> str:
        content = real_strip(content, predicate)
        return real_strip(content, lambda h, hdr=header: h == hdr)

    with mock.patch.object(scion_harness, "strip_toml_sections", side_effect=damaging):
        yield


class CodexProvisionTest(unittest.TestCase):
    def test_instruction_projection_composes_prompts_without_skills_by_default(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            home = os.path.join(tmp, "home")
            bundle = os.path.join(tmp, "bundle")
            os.makedirs(os.path.join(bundle, "inputs"))
            os.makedirs(os.path.join(home, ".codex", "skills", "example"))
            os.makedirs(os.path.join(home, ".codex", "skills", "second"))

            with open(os.path.join(bundle, "inputs", "system-prompt.md"), "w", encoding="utf-8") as f:
                f.write("System rules")
            with open(os.path.join(bundle, "inputs", "instructions.md"), "w", encoding="utf-8") as f:
                f.write("Agent rules")
            with open(
                os.path.join(home, ".codex", "skills", "example", "SKILL.md"),
                "w",
                encoding="utf-8",
            ) as f:
                f.write("# Example Skill\n\nUse this skill.")
            with open(
                os.path.join(home, ".codex", "skills", "second", "SKILL.md"),
                "w",
                encoding="utf-8",
            ) as f:
                f.write("# Second Skill\n\nUse this other skill.")

            manifest = {
                "harness_bundle_dir": bundle,
                "agent_workspace": os.path.join(tmp, "workspace"),
                "harness_config": {
                    "instructions_file": ".codex/AGENTS.md",
                    "skills_dir": ".codex/skills",
                    "system_prompt_mode": "prepend_to_instructions",
                },
            }

            with temporary_home(home):
                ctx = scion_harness.ProvisionContext("codex", manifest)
                scion_harness.project_instructions(ctx, ".codex/AGENTS.md")
                scion_harness.project_instructions(ctx, ".codex/AGENTS.md")

            with open(os.path.join(home, ".codex", "AGENTS.md"), "r", encoding="utf-8") as f:
                content = f.read()

            self.assertEqual(content.count(MANAGED_BEGIN), 1)
            self.assertIn("# System Instruction\n\nSystem rules", content)
            self.assertIn("# Agent Instructions\n\nAgent rules", content)
            self.assertNotIn("# Skills", content)
            self.assertNotIn("# Example Skill", content)
            self.assertIn(
                "# System Instruction\n\nSystem rules\n\n"
                "# Agent Instructions\n\nAgent rules",
                content,
            )

    def test_instruction_projection_cleans_stale_managed_block_when_inputs_empty(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            home = os.path.join(tmp, "home")
            bundle = os.path.join(tmp, "bundle")
            os.makedirs(os.path.join(bundle, "inputs"))
            os.makedirs(os.path.join(home, ".codex"))

            agents_path = os.path.join(home, ".codex", "AGENTS.md")
            with open(agents_path, "w", encoding="utf-8") as f:
                f.write(
                    f"{LEGACY_BEGIN}\n\n"
                    "# Agent Instructions\n\nOld managed content\n\n"
                    f"{LEGACY_END}\n\n"
                    "# User Notes\n\nKeep this.\n"
                )

            manifest = {
                "harness_bundle_dir": bundle,
                "agent_workspace": os.path.join(tmp, "workspace"),
                "harness_config": {
                    "instructions_file": ".codex/AGENTS.md",
                    "skills_dir": ".codex/skills",
                    "system_prompt_mode": "prepend_to_instructions",
                },
            }

            with temporary_home(home):
                ctx = scion_harness.ProvisionContext("codex", manifest)
                scion_harness.project_instructions(ctx, ".codex/AGENTS.md")

            with open(agents_path, "r", encoding="utf-8") as f:
                content = f.read()

            self.assertNotIn(LEGACY_BEGIN, content)
            self.assertNotIn("Old managed content", content)
            self.assertEqual(content, "# User Notes\n\nKeep this.\n")

    def test_instruction_projection_removes_file_when_only_stale_managed_block_remains(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            home = os.path.join(tmp, "home")
            bundle = os.path.join(tmp, "bundle")
            os.makedirs(os.path.join(bundle, "inputs"))
            os.makedirs(os.path.join(home, ".codex"))

            agents_path = os.path.join(home, ".codex", "AGENTS.md")
            with open(agents_path, "w", encoding="utf-8") as f:
                f.write(
                    f"{LEGACY_BEGIN}\n\n"
                    "# Agent Instructions\n\nOld managed content\n\n"
                    f"{LEGACY_END}\n"
                )

            manifest = {
                "harness_bundle_dir": bundle,
                "agent_workspace": os.path.join(tmp, "workspace"),
                "harness_config": {
                    "instructions_file": ".codex/AGENTS.md",
                    "skills_dir": ".codex/skills",
                    "system_prompt_mode": "prepend_to_instructions",
                },
            }

            with temporary_home(home):
                ctx = scion_harness.ProvisionContext("codex", manifest)
                scion_harness.project_instructions(ctx, ".codex/AGENTS.md")

            self.assertFalse(os.path.exists(agents_path))

    def test_native_otel_routes_only_to_local_receiver(self) -> None:
        telemetry = {"enabled": True, "cloud": {"endpoint": "cloudtrace.googleapis.com:443", "protocol": "http", "headers": {"authorization": "secret"}}}
        env = {"SCION_CODEX_OTEL_ENDPOINT": "external.invalid:4317", "SCION_OTEL_GRPC_PORT": "14317"}
        section = provision._build_otel_section(telemetry, env)
        self.assertIn('metrics_exporter."otlp-grpc".endpoint = "http://127.0.0.1:14317"', section)
        self.assertIn('exporter."otlp-grpc".endpoint = "http://127.0.0.1:14317"', section)
        self.assertIn('trace_exporter."otlp-grpc".endpoint = "http://127.0.0.1:14317"', section)
        self.assertIn('log_user_prompt = false', section)
        self.assertNotIn("cloudtrace", section)
        self.assertNotIn("external.invalid", section)
        self.assertNotIn("secret", section)
        self.assertNotIn("statsig", section)

    def test_native_metrics_off_on_gcp_via_configured_provider(self) -> None:
        # ptone/scion#2053 design §3.7 "codex" bullet: native metrics stay
        # off on GCP, the same as claude, because the GCP admission
        # allowlist rejects codex's raw metric attributes. Logs and traces
        # are unaffected (narrow to metrics).
        telemetry = {"enabled": True, "cloud": {"provider": "gcp"}}
        env = {"SCION_OTEL_GRPC_PORT": "14317"}
        section = provision._build_otel_section(telemetry, env)
        self.assertIn('metrics_exporter = "none"', section)
        self.assertNotIn('metrics_exporter."otlp-grpc"', section)
        self.assertIn('exporter."otlp-grpc".endpoint = "http://127.0.0.1:14317"', section)
        self.assertIn('trace_exporter."otlp-grpc".endpoint = "http://127.0.0.1:14317"', section)

    def test_native_metrics_off_on_gcp_via_env_override(self) -> None:
        # The env override (SCION_TELEMETRY_CLOUD_PROVIDER) wins over the
        # staged telemetry config, mirroring claude's provider resolution.
        telemetry = {"enabled": True, "cloud": {"provider": "generic-otlp"}}
        env = {"SCION_TELEMETRY_CLOUD_PROVIDER": "gcp", "SCION_OTEL_GRPC_PORT": "14317"}
        section = provision._build_otel_section(telemetry, env)
        self.assertIn('metrics_exporter = "none"', section)

    def test_native_metrics_stay_on_for_non_gcp_provider(self) -> None:
        telemetry = {"enabled": True, "cloud": {"provider": "generic-otlp"}}
        env = {"SCION_OTEL_GRPC_PORT": "14317"}
        section = provision._build_otel_section(telemetry, env)
        self.assertIn('metrics_exporter."otlp-grpc".endpoint = "http://127.0.0.1:14317"', section)

    def test_telemetry_output_env_sets_usage_source_native_when_enabled(self) -> None:
        env = provision._telemetry_output_env({"enabled": True})
        self.assertEqual(env["SCION_NATIVE_TELEMETRY_POLICY"], "enabled")
        self.assertEqual(env["SCION_USAGE_SOURCE"], "native")

    def test_telemetry_output_env_omits_usage_source_when_disabled(self) -> None:
        env = provision._telemetry_output_env({"enabled": False})
        self.assertEqual(env["SCION_NATIVE_TELEMETRY_POLICY"], "disabled")
        self.assertNotIn("SCION_USAGE_SOURCE", env)

    def test_telemetry_output_env_omits_usage_source_when_unset(self) -> None:
        env = provision._telemetry_output_env(None)
        self.assertEqual(env["SCION_NATIVE_TELEMETRY_POLICY"], "disabled")
        self.assertNotIn("SCION_USAGE_SOURCE", env)

    def test_telemetry_enabled_treats_non_dict_as_disabled(self) -> None:
        # A non-dict truthy value used to crash `.get()`. The production
        # writer (ApplyTelemetrySettings) never produces one, but this pins
        # the defensive fallback instead of relying on that invariant.
        for value in (True, "enabled", [1, 2, 3]):
            with self.subTest(value=value):
                self.assertFalse(provision._telemetry_enabled(value))
                # And the callers that gate on it don't crash either.
                env = provision._telemetry_output_env(value)
                self.assertEqual(env["SCION_NATIVE_TELEMETRY_POLICY"], "disabled")
                self.assertNotIn("SCION_USAGE_SOURCE", env)
        # False (a falsy non-dict) and {} (an empty config) are both not
        # enabled and must not crash.
        self.assertFalse(provision._telemetry_enabled(False))
        self.assertFalse(provision._telemetry_enabled({}))

    def test_telemetry_provider_treats_non_dict_telemetry_and_cloud_as_absent(self) -> None:
        # (telemetry or {}).get("cloud") used to crash on a truthy non-dict
        # telemetry. Also covers a dict telemetry whose "cloud" key is
        # itself a non-dict value.
        for value in (True, "enabled", [1, 2, 3]):
            with self.subTest(value=value):
                self.assertEqual(provision._telemetry_provider(value, None), "")
        self.assertEqual(provision._telemetry_provider({"cloud": "gcp"}, None), "")
        self.assertEqual(provision._telemetry_provider({"cloud": {"provider": "gcp"}}, None), "gcp")

    def test_disabled_otel_disables_all_exporters(self) -> None:
        with tempfile.TemporaryDirectory() as home, temporary_home(home):
            os.makedirs(os.path.join(home, ".codex"), exist_ok=True)
            with open(os.path.join(home, ".codex", "config.toml"), "w", encoding="utf-8") as f:
                f.write('[otel.exporter."otlp-grpc"]\nendpoint = "https://external.invalid:443"\n')
            provision._reconcile_codex_toml(_test_ctx(), None, None)
            with open(os.path.join(home, ".codex", "config.toml"), encoding="utf-8") as f:
                content = f.read()
        self.assertIn('exporter = "none"', content)
        self.assertIn('metrics_exporter = "none"', content)
        self.assertIn('trace_exporter = "none"', content)
        self.assertNotIn('external.invalid', content)

    # -- SCION_THINKING_LEVEL -> model_reasoning_effort (ptone/scion#2673) --
    #
    # Characterization table: the bucket table and the "medium" default
    # (ptone/scion#2479) moved from hard-coded provision.py helpers into
    # config.yaml's `thinking:` block. Every expected value below is the
    # output of the pre-migration _resolve_reasoning_effort /
    # _resolve_reasoning_effort_env pair, so this pins codex's behaviour as
    # byte-for-byte unchanged. Drives the real provision() entry point.

    def test_provision_reasoning_effort_matches_pre_migration_behaviour(self) -> None:
        cases = (
            # (raw SCION_THINKING_LEVEL, expected effort, expect a warning)
            (None, "medium", False),
            ("", "medium", False),
            ("   ", "medium", False),
            ("-10", "low", False),
            ("0", "low", False),
            ("25", "low", False),
            ("26", "medium", False),
            ("50", "medium", False),
            ("51", "high", False),
            ("75", "high", False),
            (" 75 ", "high", False),
            ("76", "xhigh", False),
            ("100", "xhigh", False),
            ("150", "xhigh", False),
            ("abc", "medium", True),
        )
        for raw, expected_effort, expect_warning in cases:
            with self.subTest(raw=raw):
                content, warnings = _invoke_provision(raw)
                self.assertEqual(
                    tomllib.loads(content).get("model_reasoning_effort"), expected_effort
                )
                self.assertEqual(content.count("model_reasoning_effort"), 1)
                if expect_warning:
                    self.assertTrue(
                        any("not a valid integer" in w for w in warnings),
                        f"expected an invalid-level warning, got: {warnings}",
                    )
                else:
                    self.assertEqual(warnings, [])

    def test_provision_without_thinking_block_writes_no_effort_and_warns(self) -> None:
        """Risk R1: a stale/customized config.yaml without the thinking
        block loses the medium default. provision() must not invent one,
        and must warn loudly that model_reasoning_effort was not written."""
        for raw in (None, "60"):
            with self.subTest(raw=raw):
                content, warnings = _invoke_provision(raw, harness_config={})
                self.assertNotIn("reasoning_effort", content)
                self.assertTrue(
                    any("no thinking block" in w for w in warnings),
                    f"expected a missing-thinking-block warning, got: {warnings}",
                )

    def test_provision_reasoning_effort_follows_config_thinking_block(self) -> None:
        """The table is data: a different thinking block changes the output,
        proving provision.py no longer hard-codes the buckets."""
        harness_config = {
            "thinking": {"levels": [{"max": 100, "value": "minimal"}], "default": "high"},
        }
        for raw, expected in (("10", "minimal"), (None, "high")):
            with self.subTest(raw=raw):
                content, _ = _invoke_provision(raw, harness_config=harness_config)
                self.assertEqual(tomllib.loads(content).get("model_reasoning_effort"), expected)

    def test_reconcile_codex_toml_writes_model_reasoning_effort(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            with temporary_home(tmp):
                provision._reconcile_codex_toml(_test_ctx(), None, None, reasoning_effort="medium")
                config_path = os.path.join(tmp, ".codex", "config.toml")
                with open(config_path, "r", encoding="utf-8") as f:
                    content = f.read()
                self.assertIn('model_reasoning_effort = "medium"', content)

    def test_reconcile_codex_toml_omits_reasoning_effort_when_none(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            with temporary_home(tmp):
                provision._reconcile_codex_toml(_test_ctx(), None, None, reasoning_effort=None)
                config_path = os.path.join(tmp, ".codex", "config.toml")
                with open(config_path, "r", encoding="utf-8") as f:
                    content = f.read()
                self.assertNotIn("reasoning_effort", content)
                self.assertNotIn("model_reasoning_effort", content)

    def test_reconcile_codex_toml_replaces_existing_reasoning_effort(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            with temporary_home(tmp):
                codex_dir = os.path.join(tmp, ".codex")
                os.makedirs(codex_dir)
                config_path = os.path.join(codex_dir, "config.toml")
                with open(config_path, "w", encoding="utf-8") as f:
                    f.write('reasoning_effort = "low"\nother_key = "value"\n')
                provision._reconcile_codex_toml(_test_ctx(), None, None, reasoning_effort="high")
                with open(config_path, "r", encoding="utf-8") as f:
                    content = f.read()
                self.assertIn('model_reasoning_effort = "high"', content)
                self.assertNotIn('"low"', content)
                self.assertIn('other_key = "value"', content)

    def test_reconcile_codex_toml_replaces_baked_in_model_reasoning_effort(self) -> None:
        """Verify that a pre-existing model_reasoning_effort (from the image config) gets replaced."""
        with tempfile.TemporaryDirectory() as tmp:
            with temporary_home(tmp):
                codex_dir = os.path.join(tmp, ".codex")
                os.makedirs(codex_dir)
                config_path = os.path.join(codex_dir, "config.toml")
                with open(config_path, "w", encoding="utf-8") as f:
                    f.write('model_reasoning_effort = "medium"\nother_key = "value"\n')
                provision._reconcile_codex_toml(_test_ctx(), None, None, reasoning_effort="high")
                with open(config_path, "r", encoding="utf-8") as f:
                    content = f.read()
                self.assertIn('model_reasoning_effort = "high"', content)
                self.assertEqual(content.count("model_reasoning_effort"), 1)
                self.assertNotIn('"medium"', content)
                self.assertIn('other_key = "value"', content)

    def test_reconcile_codex_toml_strips_both_old_and_new_keys(self) -> None:
        """Verify both reasoning_effort and model_reasoning_effort are stripped before writing."""
        with tempfile.TemporaryDirectory() as tmp:
            with temporary_home(tmp):
                codex_dir = os.path.join(tmp, ".codex")
                os.makedirs(codex_dir)
                config_path = os.path.join(codex_dir, "config.toml")
                with open(config_path, "w", encoding="utf-8") as f:
                    f.write(
                        'model_reasoning_effort = "medium"\n'
                        'reasoning_effort = "low"\n'
                        'other_key = "value"\n'
                    )
                provision._reconcile_codex_toml(_test_ctx(), None, None, reasoning_effort="high")
                with open(config_path, "r", encoding="utf-8") as f:
                    content = f.read()
                self.assertIn('model_reasoning_effort = "high"', content)
                self.assertEqual(content.count("model_reasoning_effort"), 1)
                self.assertNotIn('reasoning_effort = "low"', content)
                self.assertNotIn('reasoning_effort = "medium"', content)
                self.assertIn('other_key = "value"', content)

    def test_reconcile_codex_toml_writes_model(self) -> None:
        # ptone/scion#2365: SCION_MODEL (already alias-resolved by the Go
        # side) must be written into config.toml rather than relying on a
        # static baked-in `model` line, so alias updates take effect without
        # rebuilding the harness image.
        with tempfile.TemporaryDirectory() as tmp:
            with temporary_home(tmp):
                provision._reconcile_codex_toml(_test_ctx(), None, None, model="gpt-6.1-sol")
                config_path = os.path.join(tmp, ".codex", "config.toml")
                with open(config_path, "r", encoding="utf-8") as f:
                    content = f.read()
                self.assertIn('model = "gpt-6.1-sol"', content)

    def test_reconcile_codex_toml_omits_model_when_none(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            with temporary_home(tmp):
                provision._reconcile_codex_toml(_test_ctx(), None, None, model=None)
                config_path = os.path.join(tmp, ".codex", "config.toml")
                with open(config_path, "r", encoding="utf-8") as f:
                    content = f.read()
                self.assertNotIn("model =", content)

    def test_reconcile_codex_toml_replaces_baked_in_model(self) -> None:
        """A static `model` baked into the harness image (or a stale prior
        provision) must not silently win over the resolved SCION_MODEL."""
        with tempfile.TemporaryDirectory() as tmp:
            with temporary_home(tmp):
                codex_dir = os.path.join(tmp, ".codex")
                os.makedirs(codex_dir)
                config_path = os.path.join(codex_dir, "config.toml")
                with open(config_path, "w", encoding="utf-8") as f:
                    f.write('model = "gpt-5.5"\nother_key = "value"\n')
                provision._reconcile_codex_toml(_test_ctx(), None, None, model="gpt-6.1-sol")
                with open(config_path, "r", encoding="utf-8") as f:
                    content = f.read()
                self.assertIn('model = "gpt-6.1-sol"', content)
                self.assertEqual(content.count("model ="), 1)
                self.assertNotIn("gpt-5.5", content)
                self.assertIn('other_key = "value"', content)

    def test_reconcile_codex_toml_does_not_clobber_model_reasoning_effort_key(self) -> None:
        """`model` must be stripped as a top-level key without matching the
        `model_reasoning_effort` key's shared prefix."""
        with tempfile.TemporaryDirectory() as tmp:
            with temporary_home(tmp):
                provision._reconcile_codex_toml(_test_ctx(), None, None, reasoning_effort="high", model="gpt-6.1-sol")
                config_path = os.path.join(tmp, ".codex", "config.toml")
                with open(config_path, "r", encoding="utf-8") as f:
                    content = f.read()
                self.assertIn('model = "gpt-6.1-sol"', content)
                self.assertIn('model_reasoning_effort = "high"', content)

    def test_reconcile_codex_toml_writes_model_and_reasoning_effort_at_top_level_with_tables_present(self) -> None:
        # Regression test for ptone/scion#2365 review round 1 (C1): the
        # shipped home/.codex/config.toml has [features], [[hooks...]], and
        # [projects."/workspace"] tables. Appending at EOF used to land
        # `model`/`model_reasoning_effort` inside the last table instead of
        # at the top level, where codex never reads them. Seed the real
        # shipped fixture (not an empty file) so a regression to
        # end-of-file appending is actually caught by tomllib parsing the
        # wrong table.
        real_config_path = os.path.join(os.path.dirname(__file__), "home", ".codex", "config.toml")
        with open(real_config_path, "r", encoding="utf-8") as f:
            real_config = f.read()

        with tempfile.TemporaryDirectory() as tmp:
            with temporary_home(tmp):
                codex_dir = os.path.join(tmp, ".codex")
                os.makedirs(codex_dir)
                config_path = os.path.join(codex_dir, "config.toml")
                with open(config_path, "w", encoding="utf-8") as f:
                    f.write(real_config)

                provision._reconcile_codex_toml(_test_ctx(), None, None, model="gpt-6.1-sol", reasoning_effort="high")

                with open(config_path, "rb") as f:
                    data = tomllib.load(f)

        self.assertEqual(data["model"], "gpt-6.1-sol")
        self.assertEqual(data["model_reasoning_effort"], "high")
        # And not misfiled into the trailing table.
        self.assertNotIn("model", data.get("projects", {}).get("/workspace", {}))
        self.assertNotIn("model_reasoning_effort", data.get("projects", {}).get("/workspace", {}))

    def test_reconcile_codex_toml_idempotent_across_reprovision_with_tables_present(self) -> None:
        # Regression test for ptone/scion#2365 review round 1 (C1): pre-start
        # hooks re-run provision.py on every container start against the
        # same persisted agent home, so a second reconcile must not produce
        # a duplicate top-level key (which broke TOML parsing entirely).
        real_config_path = os.path.join(os.path.dirname(__file__), "home", ".codex", "config.toml")
        with open(real_config_path, "r", encoding="utf-8") as f:
            real_config = f.read()

        with tempfile.TemporaryDirectory() as tmp:
            with temporary_home(tmp):
                codex_dir = os.path.join(tmp, ".codex")
                os.makedirs(codex_dir)
                config_path = os.path.join(codex_dir, "config.toml")
                with open(config_path, "w", encoding="utf-8") as f:
                    f.write(real_config)

                provision._reconcile_codex_toml(_test_ctx(), None, None, model="gpt-6.1-sol", reasoning_effort="high")
                # Second provision (e.g. a container restart) resolves a
                # different model; must still parse and reflect only the
                # latest value, not a duplicate key.
                provision._reconcile_codex_toml(_test_ctx(), None, None, model="gpt-6-astra", reasoning_effort="medium")

                with open(config_path, "rb") as f:
                    data = tomllib.load(f)

        self.assertEqual(data["model"], "gpt-6-astra")
        self.assertEqual(data["model_reasoning_effort"], "medium")

    # Unit tests for insert_toml_top_level_line / strip_toml_top_level_key /
    # is_toml_table_header themselves now live in scion_harness_test.py
    # (TestInsertTomlTopLevelLine, TestStripTomlTopLevelKey,
    # TestIsTomlTableHeader) — those helpers moved to the shared lib
    # (ptone/scion#2426) since grok-build needs the same hardening. The
    # end-to-end tests below still exercise this module's own
    # _reconcile_codex_toml pipeline, which now delegates to them.

    def test_reconcile_codex_toml_handles_nested_array_in_notify_with_trailing_comma(self) -> None:
        # End-to-end version of the N2 regression: a config.toml whose
        # `notify` array contains a nested single-element array must still
        # reconcile to valid, parseable TOML with `model` at the top level.
        with tempfile.TemporaryDirectory() as tmp:
            with temporary_home(tmp):
                codex_dir = os.path.join(tmp, ".codex")
                os.makedirs(codex_dir)
                config_path = os.path.join(codex_dir, "config.toml")
                with open(config_path, "w", encoding="utf-8") as f:
                    f.write('notify = [\n  "sh",\n  ["x"],\n]\n\n[features]\nhooks = true\n')

                provision._reconcile_codex_toml(_test_ctx(), None, None, model="gpt-6.1-sol")

                with open(config_path, "rb") as f:
                    data = tomllib.load(f)

        self.assertEqual(data["model"], "gpt-6.1-sol")
        self.assertEqual(data["notify"], ["sh", ["x"]])
        self.assertEqual(data["features"], {"hooks": True})

    def test_reconcile_codex_toml_handles_nested_array_in_notify_without_trailing_comma(self) -> None:
        # Same as above, without a trailing comma on the array's last
        # element — added per review round 3 (R2): the mutation testing that
        # exposed the vacuous with-trailing-comma tests didn't have an
        # end-to-end case for this shape at all.
        with tempfile.TemporaryDirectory() as tmp:
            with temporary_home(tmp):
                codex_dir = os.path.join(tmp, ".codex")
                os.makedirs(codex_dir)
                config_path = os.path.join(codex_dir, "config.toml")
                with open(config_path, "w", encoding="utf-8") as f:
                    f.write('notify = [\n  "sh",\n  ["x"]\n]\n\n[features]\nhooks = true\n')

                provision._reconcile_codex_toml(_test_ctx(), None, None, model="gpt-6.1-sol")

                with open(config_path, "rb") as f:
                    data = tomllib.load(f)

        self.assertEqual(data["model"], "gpt-6.1-sol")
        self.assertEqual(data["notify"], ["sh", ["x"]])
        self.assertEqual(data["features"], {"hooks": True})

    def test_reconcile_codex_toml_ignores_unbalanced_bracket_in_preamble_comment(self) -> None:
        # Regression test for ptone/scion#2365 review round 3 (R1): a single
        # unbalanced `[` inside an unrelated top-level comment used to stick
        # the naive bracket-depth counter above zero for the rest of the
        # file, silently deleting model/model_reasoning_effort from every
        # table-scoped occurrence (e.g. a user's own [profiles.fast]) and
        # misplacing the inserted top-level key into the last table instead.
        # String/comment masking before counting fixes this.
        real_config_path = os.path.join(os.path.dirname(__file__), "home", ".codex", "config.toml")
        with open(real_config_path, "r", encoding="utf-8") as f:
            real_config = f.read()

        content = (
            "# temperature range [0, 1)\n"
            + real_config
            + '\n[profiles.fast]\nmodel = "gpt-user-profile"\nmodel_reasoning_effort = "low"\n'
        )

        with tempfile.TemporaryDirectory() as tmp:
            with temporary_home(tmp):
                codex_dir = os.path.join(tmp, ".codex")
                os.makedirs(codex_dir)
                config_path = os.path.join(codex_dir, "config.toml")
                with open(config_path, "w", encoding="utf-8") as f:
                    f.write(content)

                provision._reconcile_codex_toml(_test_ctx(), None, None, model="gpt-6.1-sol", reasoning_effort="high")

                with open(config_path, "rb") as f:
                    data = tomllib.load(f)

        self.assertEqual(data["model"], "gpt-6.1-sol")
        self.assertEqual(data["model_reasoning_effort"], "high")
        self.assertEqual(data["profiles"]["fast"]["model"], "gpt-user-profile")
        self.assertEqual(data["profiles"]["fast"]["model_reasoning_effort"], "low")

    def test_reconcile_codex_toml_ignores_unbalanced_bracket_in_preamble_string(self) -> None:
        # Same regression as above, but the unbalanced bracket is inside a
        # string value rather than a comment.
        real_config_path = os.path.join(os.path.dirname(__file__), "home", ".codex", "config.toml")
        with open(real_config_path, "r", encoding="utf-8") as f:
            real_config = f.read()

        content = (
            'approval_hint = "press [ to go"\n'
            + real_config
            + '\n[profiles.fast]\nmodel = "gpt-user-profile"\nmodel_reasoning_effort = "low"\n'
        )

        with tempfile.TemporaryDirectory() as tmp:
            with temporary_home(tmp):
                codex_dir = os.path.join(tmp, ".codex")
                os.makedirs(codex_dir)
                config_path = os.path.join(codex_dir, "config.toml")
                with open(config_path, "w", encoding="utf-8") as f:
                    f.write(content)

                provision._reconcile_codex_toml(_test_ctx(), None, None, model="gpt-6.1-sol", reasoning_effort="high")

                with open(config_path, "rb") as f:
                    data = tomllib.load(f)

        self.assertEqual(data["model"], "gpt-6.1-sol")
        self.assertEqual(data["model_reasoning_effort"], "high")
        self.assertEqual(data["profiles"]["fast"]["model"], "gpt-user-profile")
        self.assertEqual(data["profiles"]["fast"]["model_reasoning_effort"], "low")

    # --- ptone/scion#2427 review round 1 (R1): unbalanced bracket in a
    # *multi-line* string, using the reviewer's own repro content, on the
    # real shipped image-home config.toml. Unlike the single-line preamble
    # tests above (which the old code already handled correctly — masking
    # was line-scoped, so a single-line string's brackets never leaked),
    # this bracket sits inside a multi-line string that the old
    # toml_entering_array_depths had no way to know it was still inside,
    # permanently corrupting depth for every later line — including the
    # shipped seed's real [otel] section, breaking both telemetry states.

    def test_reconcile_codex_toml_reconciles_otel_past_unbalanced_bracket_in_multiline_string_telemetry_disabled(
        self,
    ) -> None:
        real_config_path = os.path.join(os.path.dirname(__file__), "home", ".codex", "config.toml")
        with open(real_config_path, "r", encoding="utf-8") as f:
            real_config = f.read()

        content = (
            'developer_instructions = """\n'
            "Prefix tasks with [TODO or [WIP when unfinished.\n"
            '"""\n'
            + real_config
        )
        self.assertIsNotNone(tomllib.loads(content), "sanity: content must be valid TOML")

        with tempfile.TemporaryDirectory() as tmp:
            with temporary_home(tmp):
                codex_dir = os.path.join(tmp, ".codex")
                os.makedirs(codex_dir)
                config_path = os.path.join(codex_dir, "config.toml")
                with open(config_path, "w", encoding="utf-8") as f:
                    f.write(content)

                ctx = _test_ctx()
                warnings: list[str] = []
                ctx.info = warnings.append  # type: ignore[method-assign]
                provision._reconcile_codex_toml(ctx, None, None, model="gpt-6.1-sol", reasoning_effort="high")

                with open(config_path, "rb") as f:
                    data = tomllib.load(f)

        self.assertEqual(data["model"], "gpt-6.1-sol")
        self.assertEqual(data["model_reasoning_effort"], "high")
        self.assertEqual(
            data["developer_instructions"],
            "Prefix tasks with [TODO or [WIP when unfinished.\n",
        )
        self.assertEqual(data["otel"]["exporter"], "none")
        self.assertEqual(data["otel"]["metrics_exporter"], "none")
        self.assertEqual(data["otel"]["trace_exporter"], "none")
        self.assertNotIn("statsig", str(data["otel"]))
        # The rest of the shipped seed (untouched by this script) must
        # survive past the multi-line string and the real [otel] section.
        self.assertEqual(data["projects"]["/workspace"]["trust_level"], "trusted")
        self.assertEqual(data["notice"]["model_migrations"]["gpt-5.2"], "gpt-5.5")
        self.assertEqual(warnings, [], f"expected no fallback warning, got: {warnings}")

    def test_reconcile_codex_toml_reconciles_otel_past_unbalanced_bracket_in_multiline_string_telemetry_enabled(
        self,
    ) -> None:
        real_config_path = os.path.join(os.path.dirname(__file__), "home", ".codex", "config.toml")
        with open(real_config_path, "r", encoding="utf-8") as f:
            real_config = f.read()

        content = (
            'developer_instructions = """\n'
            "Prefix tasks with [TODO or [WIP when unfinished.\n"
            '"""\n'
            + real_config
        )

        with tempfile.TemporaryDirectory() as tmp:
            with temporary_home(tmp):
                codex_dir = os.path.join(tmp, ".codex")
                os.makedirs(codex_dir)
                config_path = os.path.join(codex_dir, "config.toml")
                with open(config_path, "w", encoding="utf-8") as f:
                    f.write(content)

                ctx = _test_ctx()
                warnings: list[str] = []
                ctx.info = warnings.append  # type: ignore[method-assign]
                provision._reconcile_codex_toml(
                    ctx,
                    {"enabled": True},
                    {"SCION_OTEL_GRPC_PORT": "14317"},
                    model="gpt-6.1-sol",
                    reasoning_effort="high",
                )

                with open(config_path, "rb") as f:
                    data = tomllib.load(f)

        self.assertEqual(data["model"], "gpt-6.1-sol")
        self.assertEqual(data["model_reasoning_effort"], "high")
        self.assertEqual(
            data["developer_instructions"],
            "Prefix tasks with [TODO or [WIP when unfinished.\n",
        )
        self.assertEqual(data["otel"]["exporter"]["otlp-grpc"]["endpoint"], "http://127.0.0.1:14317")
        self.assertNotIn("statsig", str(data["otel"]))
        self.assertEqual(data["projects"]["/workspace"]["trust_level"], "trusted")
        self.assertEqual(warnings, [], f"expected no fallback warning, got: {warnings}")

    def _reconcile_and_capture(
        self, original: str, **kwargs: Any
    ) -> tuple[str, list[str]]:
        """Runs _reconcile_codex_toml against `original` content and returns
        (file content after the call, list of ctx.info messages logged)."""
        with tempfile.TemporaryDirectory() as tmp:
            with temporary_home(tmp):
                codex_dir = os.path.join(tmp, ".codex")
                os.makedirs(codex_dir)
                config_path = os.path.join(codex_dir, "config.toml")
                with open(config_path, "w", encoding="utf-8") as f:
                    f.write(original)

                ctx = _test_ctx()
                warnings: list[str] = []
                ctx.info = warnings.append  # type: ignore[method-assign]

                provision._reconcile_codex_toml(ctx, None, None, **kwargs)

                with open(config_path, "r", encoding="utf-8") as f:
                    after = f.read()
        return after, warnings

    def test_reconcile_codex_toml_applies_otel_but_not_model_when_string_is_corrupted(self) -> None:
        # Backstop test for ptone/scion#2365 review round 4 (N1): when the
        # model/model_reasoning_effort edit doesn't preserve the file,
        # _reconcile_codex_toml must still apply the telemetry (otel)
        # reconciliation on a fallback pass that never touches the string —
        # telemetry-disabled enforcement must not silently no-op just
        # because the model couldn't be written. codex still gets an
        # explicit model via SCION_MODEL/--model argv regardless of what
        # config.toml says.
        #
        # Originally used a header-shaped line ("[IMPORTANT]") inside the
        # string to trigger the corruption; ptone/scion#2427 review round 1
        # (R1) added multi-line-string tracking to
        # scion_harness.toml_entering_array_depths, which fixed that
        # specific case. Then used a prose line starting with "model"
        # instead ("model choice affects everything here."); review round 2
        # ("Consider 1") fixed that case too (strip_toml_top_level_key now
        # skips lines inside a multi-line string). Per review round 2
        # (R2-b), this now forces real damage instead of relying on any
        # specific strip_toml_sections/strip_toml_top_level_key residual
        # gap: strip_toml_top_level_key is patched to also (wrongly) drop
        # [features], corrupting the full edit; the otel-only fallback
        # rebuilds from the untouched original and never calls
        # strip_toml_top_level_key, so it stays clean.
        original = (
            'developer_instructions = """\n'
            "some ordinary prose here.\n"
            '"""\n'
            "[features]\n"
            "hooks = true\n"
            '[otel.exporter."otlp-grpc"]\n'
            'endpoint = "https://ext.invalid"\n'
        )

        with _also_strip_key_damaging("[features]"):
            after, warnings = self._reconcile_and_capture(original, model="gpt-6.1-sol")
        data = tomllib.loads(after)

        self.assertNotIn("model", data, "model must not be written when the edit doesn't preserve content")
        self.assertEqual(data["features"], {"hooks": True}, "features must survive via the untouched fallback")
        self.assertEqual(data["otel"]["exporter"], "none", "telemetry must still be reconciled to disabled")
        self.assertTrue(
            any("telemetry" in w and "model" in w for w in warnings),
            f"expected a warning naming both telemetry and model, got: {warnings}",
        )

    def test_reconcile_codex_toml_writes_effort_past_header_shaped_line_in_multiline_string(self) -> None:
        # Originally a regression test for ptone/scion#2365 review round 4
        # (R1), repro (a): a header-shaped line ("[IMPORTANT]") inside a
        # top-level multi-line string used to fool insert_toml_top_level_line
        # into splicing model_reasoning_effort into the string's body
        # instead of at the real top level (past the string, before
        # [features]). ptone/scion#2427 review round 1 fixed this directly —
        # scion_harness.toml_entering_array_depths now tracks multi-line
        # strings, so is_toml_table_header correctly excludes "[IMPORTANT]"
        # here — so this is now a positive test: the effort is written
        # correctly and the string is untouched, with no fallback needed.
        original = (
            'developer_instructions = """\n'
            "Headers look like:\n"
            "[IMPORTANT]\n"
            "text\n"
            '"""\n'
            "[features]\n"
            "hooks = true\n"
        )

        after, warnings = self._reconcile_and_capture(original, model=None, reasoning_effort="high")
        data = tomllib.loads(after)

        self.assertEqual(data["model_reasoning_effort"], "high")
        self.assertEqual(data["developer_instructions"], "Headers look like:\n[IMPORTANT]\ntext\n")
        self.assertEqual(warnings, [], f"expected no fallback warning, got: {warnings}")

    def test_reconcile_codex_toml_writes_model_and_effort_past_prose_that_looks_like_keys(self) -> None:
        # Originally a regression test for ptone/scion#2365 review round 4
        # (R1), repro (b): _is_toml_key_line matched any line whose stripped
        # text starts with "model"/"reasoning_effort" followed by a space,
        # `=`, or tab — including prose lines inside a top-level multi-line
        # string that merely happen to start that way, so
        # strip_toml_top_level_key deleted them from the string's body.
        #
        # ptone/scion#2427 review round 2 ("Consider 1") fixed this
        # directly: strip_toml_top_level_key now skips any line whose
        # entering depth is the multi-line-string sentinel, the same way
        # is_toml_table_header already did — so this is now a positive
        # test. See scion_harness_test.py's
        # test_ignores_prose_line_inside_multiline_string_that_starts_with_the_key
        # for the lib-level version of this fix.
        original = (
            'developer_instructions = """\n'
            "model choice is up to you.\n"
            "reasoning_effort matters\n"
            '"""\n'
            "[features]\n"
            "hooks = true\n"
        )

        after, warnings = self._reconcile_and_capture(original, model="gpt-6.1-sol", reasoning_effort="high")
        data = tomllib.loads(after)

        self.assertEqual(data["model"], "gpt-6.1-sol")
        self.assertEqual(data["model_reasoning_effort"], "high")
        self.assertEqual(
            data["developer_instructions"],
            "model choice is up to you.\nreasoning_effort matters\n",
        )
        self.assertEqual(warnings, [], f"expected no fallback warning, got: {warnings}")

    def test_reconcile_codex_toml_leaves_file_completely_untouched_when_otel_fallback_also_fails(self) -> None:
        # Defense-in-depth test: if even the telemetry-only fallback edit
        # doesn't preserve the file's content, _reconcile_codex_toml must
        # give up entirely rather than write anything — no model, no
        # effort, no telemetry change.
        #
        # Originally used a fake "[otel...]"-shaped line inside a top-level
        # multi-line string to trigger this, then (after ptone/scion#2427
        # review round 1 fixed that) an escaped closing-delimiter sequence.
        # Review round 2 ("Consider 2") closed the escaped-delimiter gap
        # too, so this now forces real damage instead: strip_toml_sections
        # is patched to also (wrongly) drop [features] on every call,
        # corrupting both the full edit's otel reconcile and the otel-only
        # fallback's (both go through strip_toml_sections for the otel
        # swap), so neither path preserves the file and both are rejected.
        original = (
            'developer_instructions = """\n'
            "some ordinary prose here.\n"
            '"""\n'
            "[features]\n"
            "hooks = true\n"
            '[otel.exporter."otlp-grpc"]\n'
            'endpoint = "https://ext.invalid"\n'
        )

        with _also_strip_sections_damaging("[features]"):
            after, warnings = self._reconcile_and_capture(original, model="gpt-6.1-sol")

        self.assertEqual(after, original, "file must be left completely untouched when even the fallback fails")
        self.assertTrue(
            any("fallback" in w for w in warnings),
            f"expected a warning naming the failed fallback, got: {warnings}",
        )

    def test_reconcile_codex_toml_preserves_nested_table_with_unbalanced_bracket_in_multiline_string(self) -> None:
        # Originally a regression test for ptone/scion#2365 review round 5
        # (N1), using an unbalanced "[" inside a top-level multi-line string
        # to hide the real [profiles.fast] header and force the full edit
        # to be rejected, so it could pin _toml_edit_preserves comparing the
        # whole parsed document (not just top-level scalars) — a "shallow"
        # mutant would pass every other test while silently dropping
        # profiles.fast's contents on this exact input.
        #
        # ptone/scion#2427 review round 1 (R1) fixed the unbalanced-bracket
        # class directly (scion_harness.toml_entering_array_depths now
        # tracks multi-line strings, so the bracket inside the string no
        # longer corrupts depth for [profiles.fast]) — so the full edit now
        # succeeds cleanly instead of needing the fallback, and this is a
        # positive test. The deep-vs-shallow-compare mutation coverage this
        # test originally existed for now lives directly on
        # scion_harness.toml_edit_preserves — see scion_harness_test.py's
        # TestTomlEditPreserves.test_ignores_nested_contents_of_a_managed_key
        # and the key-path sibling-subtable tests.
        original = (
            'developer_instructions = """\n'
            "use [ brackets\n"
            '"""\n'
            "[profiles.fast]\n"
            'model = "fast-model"\n'
            'model_reasoning_effort = "low"\n'
        )

        after, warnings = self._reconcile_and_capture(original, model=None, reasoning_effort=None)
        data = tomllib.loads(after)

        self.assertEqual(
            data["profiles"]["fast"],
            {"model": "fast-model", "model_reasoning_effort": "low"},
        )
        self.assertEqual(warnings, [], f"expected no fallback warning, got: {warnings}")

    def test_reconcile_codex_toml_warns_when_stale_top_level_model_survives_full_edit_path(self) -> None:
        # Regression test for ptone/scion#2365 review round 5 (N3), full
        # edit path: a header-shaped line inside an earlier top-level
        # multi-line string can hide the real table header that would
        # otherwise let _strip_toml_top_level_key find and remove a later,
        # genuinely top-level `model` line. With SCION_MODEL empty
        # (model=None), nothing replaces that stale value, so codex runs on
        # it with no --model argv either — this is observability only (a
        # warning), not a behavior change: the file content is unaffected.
        #
        # Originally used a plain header-shaped line ("[foo]") with balanced
        # brackets; ptone/scion#2427 review round 1 (R1) fixed that case
        # directly. Then used an escaped closing-delimiter sequence to
        # achieve the same hiding effect; review round 2 ("Consider 2")
        # closed that gap too. Both known string-tracking gaps are now
        # closed, so per review round 2 (R2-b) this forces the hiding
        # directly instead of hunting for another real parsing gap:
        # is_toml_table_header is patched to (wrongly) report the raw
        # "[foo]" line as a real header — simulating whatever future bug
        # might reintroduce this class of mistake — hiding the real
        # top-level `model` line that follows before the true first header
        # ([features]).
        original = (
            'developer_instructions = """\n'
            "[foo]\n"
            '"""\n'
            'model = "gpt-5.5"\n'
            "[features]\n"
            "hooks = true\n"
        )
        self.assertEqual(tomllib.loads(original)["model"], "gpt-5.5", "sanity: original must be valid TOML")

        with _force_is_header("[foo]"):
            after, warnings = self._reconcile_and_capture(original, model=None)
        data = tomllib.loads(after)

        self.assertEqual(data.get("model"), "gpt-5.5", "stale model is expected to survive; this is observability only")
        self.assertTrue(
            any("still sets top-level model" in w and "line-oriented strip" in w for w in warnings),
            f"expected a stale-model warning naming the header-hiding cause, got: {warnings}",
        )

    def test_reconcile_codex_toml_warns_when_stale_top_level_model_survives_otel_only_fallback_path(self) -> None:
        # Regression test for ptone/scion#2365 review round 6 (N1): the
        # stale-model warning has three call sites in _reconcile_codex_toml
        # (full edit, otel-only fallback, fully-untouched), but only the
        # full-edit path had a test — removing the call on either fallback
        # path left all existing tests green. Here, the full edit is
        # rejected, so the otel-only fallback runs and keeps the stale
        # top-level `model` from the original file untouched.
        #
        # Originally used a prose line inside a multi-line string
        # ('model_reasoning_effort = "x"') to trigger the rejection;
        # ptone/scion#2427 review round 2 ("Consider 1") fixed that class of
        # bug directly (strip_toml_top_level_key now skips lines inside a
        # multi-line string), so per review round 2 (R2-b) this now forces
        # the full edit's rejection directly: strip_toml_top_level_key is
        # patched to also (wrongly) drop [features].
        original = (
            'model = "gpt-5.5"\n'
            "[features]\n"
            "hooks = true\n"
            '[otel.exporter."otlp-grpc"]\n'
            'endpoint = "https://ext.invalid"\n'
        )

        with _also_strip_key_damaging("[features]"):
            after, warnings = self._reconcile_and_capture(original, model=None, reasoning_effort="x")
        data = tomllib.loads(after)

        self.assertEqual(data.get("model"), "gpt-5.5", "stale model is expected to survive; this is observability only")
        self.assertTrue(
            any("still sets top-level model" in w and "rejected" in w for w in warnings),
            f"expected a stale-model warning naming the rejected-edit cause, got: {warnings}",
        )

    def test_reconcile_codex_toml_warns_when_stale_top_level_model_survives_fully_untouched_path(self) -> None:
        # Regression test for ptone/scion#2365 review round 6 (N1): the
        # third call site, reached when even the otel-only fallback is
        # rejected (here, a NaN value makes tomllib equality always false,
        # so every comparison fails and config.toml is left completely
        # untouched) but a stale top-level `model` is still present in the
        # unmodified original.
        original = 'x = nan\nmodel = "old"\n'

        after, warnings = self._reconcile_and_capture(original, model=None)

        self.assertEqual(after, original, "file must be left completely untouched")
        self.assertTrue(
            any("still sets top-level model" in w and "completely untouched" in w for w in warnings),
            f"expected a stale-model warning naming the fully-untouched cause, got: {warnings}",
        )

    def test_warn_if_stale_top_level_model_survives_is_silent_when_model_provided(self) -> None:
        warnings: list[str] = []
        ctx = _test_ctx()
        ctx.info = warnings.append  # type: ignore[method-assign]
        provision._warn_if_stale_top_level_model_survives(ctx, 'model = "gpt-5.5"\n', "gpt-5.5", "some reason")
        self.assertEqual(warnings, [])

    def test_warn_if_stale_top_level_model_survives_is_silent_when_no_model_key(self) -> None:
        warnings: list[str] = []
        ctx = _test_ctx()
        ctx.info = warnings.append  # type: ignore[method-assign]
        provision._warn_if_stale_top_level_model_survives(ctx, "other_key = 1\n", None, "some reason")
        self.assertEqual(warnings, [])

    def test_warn_if_stale_top_level_model_survives_warns_on_stale_model(self) -> None:
        warnings: list[str] = []
        ctx = _test_ctx()
        ctx.info = warnings.append  # type: ignore[method-assign]
        provision._warn_if_stale_top_level_model_survives(ctx, 'model = "gpt-5.5"\n', None, "some reason")
        self.assertEqual(len(warnings), 1)
        self.assertIn("gpt-5.5", warnings[0])

    def test_warn_if_stale_top_level_model_survives_includes_the_given_reason(self) -> None:
        # Regression test for ptone/scion#2365 review round 6 (N2): the
        # warning must include the caller-supplied reason, since a single
        # generic hint misled on two of the three call sites (see the three
        # end-to-end tests below, one per call site, each asserting its own
        # distinct reason text).
        warnings: list[str] = []
        ctx = _test_ctx()
        ctx.info = warnings.append  # type: ignore[method-assign]
        provision._warn_if_stale_top_level_model_survives(
            ctx, 'model = "gpt-5.5"\n', None, "a distinctive marker reason"
        )
        self.assertEqual(len(warnings), 1)
        self.assertIn("a distinctive marker reason", warnings[0])

    def test_toml_edit_preserves_accepts_valid_content_with_matching_model_and_effort(self) -> None:
        self.assertTrue(
            provision._toml_edit_preserves(
                'other_key = 1\n', 'other_key = 1\nmodel = "x"\nmodel_reasoning_effort = "high"\n', "x", "high"
            )
        )

    def test_toml_edit_preserves_rejects_invalid_toml(self) -> None:
        self.assertFalse(provision._toml_edit_preserves("", "model = [unterminated\n", "x", None))

    def test_toml_edit_preserves_rejects_missing_top_level_model(self) -> None:
        # ptone/scion#2365 review round 5 (N2): use an identical
        # original/content baseline so the unmanaged-content diff trivially
        # passes and only the explicit model check can reject this — an
        # empty `original` (as in the pre-round-5 version of this test)
        # made the unmanaged diff itself the reason for rejection ([table]
        # appearing where nothing existed before), so the model check could
        # be deleted entirely without this test noticing.
        baseline = '[table]\nmodel = "x"\n'
        self.assertFalse(provision._toml_edit_preserves(baseline, baseline, "x", None))

    def test_toml_edit_preserves_rejects_missing_top_level_effort(self) -> None:
        # Same fix as the model test above, for the effort check.
        baseline = '[table]\nmodel_reasoning_effort = "high"\n'
        self.assertFalse(provision._toml_edit_preserves(baseline, baseline, None, "high"))

    def test_toml_edit_preserves_ignores_model_and_effort_when_none_expected(self) -> None:
        self.assertTrue(provision._toml_edit_preserves("other_key = 1\n", "other_key = 1\n", None, None))

    def test_toml_edit_preserves_rejects_unmanaged_key_changed(self) -> None:
        self.assertFalse(
            provision._toml_edit_preserves('other_key = "before"\n', 'other_key = "after"\nmodel = "x"\n', "x", None)
        )

    def test_toml_edit_preserves_ignores_managed_keys_when_diffing(self) -> None:
        # reasoning_effort is stripped unconditionally (see
        # _MANAGED_TOP_LEVEL_KEYS) even though this script never writes it,
        # so its disappearance alone must not fail the preservation check.
        self.assertTrue(
            provision._toml_edit_preserves(
                'reasoning_effort = "low"\nother_key = 1\n',
                'other_key = 1\nmodel = "x"\n',
                "x",
                None,
            )
        )

    def test_toml_edit_preserves_true_when_original_unparseable(self) -> None:
        self.assertTrue(provision._toml_edit_preserves("not [valid toml", 'model = "x"\n', "x", None))

    # --- _write_mcp_to_config: validate-before-write ------------------------
    # Regression coverage for ptone/scion#2426 (generalization-findings.md
    # G1): this writer used to call scion_harness.atomic_write_text directly
    # with no tomllib preserve-check at all — "The MCP writer has no tomllib
    # preserve-check" per the findings note. It now goes through
    # scion_harness.write_toml_if_preserves, the same as grok-build's MCP
    # writer.

    def test_write_mcp_to_config_creates_sections(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            with temporary_home(tmp):
                ctx = _test_ctx()
                provision._write_mcp_to_config(ctx, {"foo": '[mcp_servers.foo]\ncommand = "a"\n'})
                config_path = os.path.join(tmp, ".codex", "config.toml")
                with open(config_path, encoding="utf-8") as f:
                    content = f.read()
        self.assertIn("[mcp_servers.foo]", content)
        self.assertIn('command = "a"', content)

    def test_write_mcp_to_config_preserves_overlay_table(self) -> None:
        # Happy-path sanity check: an unrelated overlay table survives a
        # normal write. This alone does NOT pin managed_keys={"mcp_servers"}
        # — the line-oriented strip never touches [profiles.fast] regardless
        # of what's declared as managed, so widening managed_keys to also
        # cover "profiles" would pass this test too (ptone/scion#2427 review
        # round 2, R2-b). See
        # test_write_mcp_to_config_rejects_write_that_also_damages_overlay_table
        # below for the test that actually pins the upper bound, by forcing
        # real damage and asserting it gets rejected.
        with tempfile.TemporaryDirectory() as tmp:
            with temporary_home(tmp):
                codex_dir = os.path.join(tmp, ".codex")
                os.makedirs(codex_dir)
                config_path = os.path.join(codex_dir, "config.toml")
                with open(config_path, "w", encoding="utf-8") as f:
                    f.write('[profiles.fast]\nmodel = "gpt-user-profile"\n')

                ctx = _test_ctx()
                provision._write_mcp_to_config(ctx, {"foo": '[mcp_servers.foo]\ncommand = "a"\n'})

                with open(config_path, "rb") as f:
                    data = tomllib.load(f)
        self.assertEqual(data["profiles"]["fast"]["model"], "gpt-user-profile")
        self.assertEqual(data["mcp_servers"]["foo"]["command"], "a")

    def test_write_mcp_to_config_rejects_write_that_also_damages_overlay_table(self) -> None:
        # ptone/scion#2427 review round 2 (R2-b): force real damage — strip
        # is patched to also (wrongly) drop [profiles.fast] — and assert
        # write_toml_if_preserves' managed_keys={"mcp_servers"} check
        # rejects the resulting damage. Widening managed_keys to also cover
        # "profiles" (M11 in the review) would make this test fail, since
        # the check would then (wrongly) accept the damage.
        original = '[profiles.fast]\nmodel = "gpt-user-profile"\n'
        with tempfile.TemporaryDirectory() as tmp:
            with temporary_home(tmp):
                codex_dir = os.path.join(tmp, ".codex")
                os.makedirs(codex_dir)
                config_path = os.path.join(codex_dir, "config.toml")
                with open(config_path, "w", encoding="utf-8") as f:
                    f.write(original)

                ctx = _test_ctx()
                warnings: list[str] = []
                ctx.warn = warnings.append  # type: ignore[method-assign]
                with _also_strip_sections_damaging("[profiles.fast]"):
                    provision._write_mcp_to_config(ctx, {"foo": '[mcp_servers.foo]\ncommand = "a"\n'})

                with open(config_path, encoding="utf-8") as f:
                    after = f.read()
        self.assertEqual(after, original, "file must be left untouched when the edit would drop an unmanaged table")
        self.assertEqual(len(warnings), 1)

    def test_write_mcp_to_config_strips_old_sections_including_commented_header(self) -> None:
        # Repro case 1 from generalization-findings.md: a header with a
        # trailing comment must be recognized so re-writing the same table
        # doesn't produce a duplicate ('Cannot declare... twice').
        with tempfile.TemporaryDirectory() as tmp:
            with temporary_home(tmp):
                codex_dir = os.path.join(tmp, ".codex")
                os.makedirs(codex_dir)
                config_path = os.path.join(codex_dir, "config.toml")
                with open(config_path, "w", encoding="utf-8") as f:
                    f.write('[mcp_servers.foo] # added by user\ncommand = "a"\n')

                ctx = _test_ctx()
                provision._write_mcp_to_config(ctx, {"foo": '[mcp_servers.foo]\ncommand = "b"\n'})

                with open(config_path, "rb") as f:
                    data = tomllib.load(f)
        self.assertEqual(data["mcp_servers"]["foo"]["command"], "b")

    def test_write_mcp_to_config_leaves_file_untouched_when_edit_would_corrupt_unmanaged_content(self) -> None:
        # write_toml_if_preserves must catch a corrupting edit via the
        # tomllib backstop and leave the file untouched rather than write
        # invalid or damaged TOML.
        #
        # Originally used a header-shaped line, then an escaped
        # closing-delimiter sequence, inside a multi-line string to trigger
        # this; both are now correctly handled (ptone/scion#2427 review
        # rounds 1 and 2 — see
        # test_write_mcp_to_config_strips_old_sections_including_commented_header
        # and scion_harness_test.py's repro tests). Per review round 2
        # (R2-b), this now forces real damage instead: strip_toml_sections
        # is patched to also (wrongly) drop [features].
        original = 'developer_instructions = "keep me"\n[features]\nhooks = true\n'
        self.assertEqual(tomllib.loads(original)["features"]["hooks"], True, "sanity: original must be valid TOML")
        with tempfile.TemporaryDirectory() as tmp:
            with temporary_home(tmp):
                codex_dir = os.path.join(tmp, ".codex")
                os.makedirs(codex_dir)
                config_path = os.path.join(codex_dir, "config.toml")
                with open(config_path, "w", encoding="utf-8") as f:
                    f.write(original)

                ctx = _test_ctx()
                warnings: list[str] = []
                ctx.warn = warnings.append  # type: ignore[method-assign]
                with _also_strip_sections_damaging("[features]"):
                    provision._write_mcp_to_config(ctx, {"real": '[mcp_servers.real]\ncommand = "x"\n'})

                with open(config_path, encoding="utf-8") as f:
                    after = f.read()
        self.assertEqual(after, original, "file must be left untouched when the edit doesn't preserve content")
        self.assertEqual(len(warnings), 1)


if __name__ == "__main__":
    unittest.main()
