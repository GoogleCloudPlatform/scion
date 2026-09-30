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


def _test_ctx() -> "scion_harness.ProvisionContext":
    """A minimal ProvisionContext for calling _reconcile_codex_toml directly
    in tests that don't otherwise need a full provision() manifest."""
    return scion_harness.ProvisionContext("codex", {})


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

    def test_resolve_reasoning_effort_maps_thinking_levels(self) -> None:
        self.assertEqual(provision._resolve_reasoning_effort(0), "low")
        self.assertEqual(provision._resolve_reasoning_effort(25), "low")
        self.assertEqual(provision._resolve_reasoning_effort(26), "medium")
        self.assertEqual(provision._resolve_reasoning_effort(50), "medium")
        self.assertEqual(provision._resolve_reasoning_effort(51), "high")
        self.assertEqual(provision._resolve_reasoning_effort(75), "high")
        self.assertEqual(provision._resolve_reasoning_effort(76), "xhigh")
        self.assertEqual(provision._resolve_reasoning_effort(100), "xhigh")

    def test_resolve_reasoning_effort_clamps_out_of_range(self) -> None:
        self.assertEqual(provision._resolve_reasoning_effort(-10), "low")
        self.assertEqual(provision._resolve_reasoning_effort(150), "xhigh")

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

    def test_insert_toml_top_level_line_appends_when_no_table_header(self) -> None:
        result = provision._insert_toml_top_level_line('other_key = "value"\n', 'model = "x"')
        self.assertEqual(result, 'other_key = "value"\n\nmodel = "x"')

    def test_insert_toml_top_level_line_lands_before_first_table_header(self) -> None:
        content = 'other_key = "value"\n[features]\nhooks = true\n'
        result = provision._insert_toml_top_level_line(content, 'model = "x"')
        lines = result.split("\n")
        model_idx = lines.index('model = "x"')
        section_idx = lines.index("[features]")
        self.assertLess(model_idx, section_idx)

    def test_insert_toml_top_level_line_ignores_nested_array_line_with_trailing_comma(self) -> None:
        # Regression test for ptone/scion#2365 review round 2 (N2): a
        # top-level multi-line array whose element is itself an array on its
        # own line (e.g. `["x"],`) is syntactically indistinguishable by
        # shape alone from a quoted-key table header (`["x"]`). Without
        # bracket-depth tracking, this line was misidentified as a header
        # and the inserted key landed inside the array, breaking the file.
        #
        # Asserting with tomllib (round 3 review R2 fix): a plain line-index
        # comparison is satisfied even when the key is spliced *inside* the
        # array (which is invalid TOML) as long as some `[features]` line
        # still sorts after it, so it can't tell "inserted before the array"
        # apart from "inserted inside the array, before its closing bracket
        # line". Parsing with tomllib and checking the actual value structure
        # can't be fooled that way: it fails immediately if the mechanism
        # under test regresses.
        content = 'notify = [\n  "sh",\n  ["x"],\n]\n[features]\nhooks = true\n'
        result = provision._insert_toml_top_level_line(content, 'model = "y"')
        data = tomllib.loads(result)
        self.assertEqual(data["model"], "y")
        self.assertEqual(data["notify"], ["sh", ["x"]])
        self.assertEqual(data["features"], {"hooks": True})

    def test_insert_toml_top_level_line_ignores_nested_array_line_without_trailing_comma(self) -> None:
        # Same as above, but the nested-array line is the array's last
        # element (no trailing comma) — the shape most easily confused with
        # a real `["x"]` table header, since only bracket depth (not a
        # regex on the line's own shape) distinguishes the two.
        content = 'notify = [\n  "sh",\n  ["x"]\n]\n[features]\nhooks = true\n'
        result = provision._insert_toml_top_level_line(content, 'model = "y"')
        data = tomllib.loads(result)
        self.assertEqual(data["model"], "y")
        self.assertEqual(data["notify"], ["sh", ["x"]])
        self.assertEqual(data["features"], {"hooks": True})

    def test_strip_toml_top_level_key_ignores_nested_array_line(self) -> None:
        # Mirrors the _insert_toml_top_level_line regression above: a
        # top-level key placed after a multi-line array (but before any
        # real table header) must still be recognized and stripped, not
        # treated as already "in a section" because of the array's nested
        # bracketed element. Also checks the array and table are untouched.
        content = 'notify = [\n  "sh",\n  ["x"],\n]\nmodel = "stale"\n[features]\nhooks = true\n'
        result = provision._strip_toml_top_level_key(content, "model")
        data = tomllib.loads(result)
        self.assertNotIn("model", data)
        self.assertEqual(data["notify"], ["sh", ["x"]])
        self.assertEqual(data["features"], {"hooks": True})

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

    def test_toml_table_header_accepts_quoted_key_containing_equals_and_bracket(self) -> None:
        # Regression test for ptone/scion#2365 review round 3 (N1): a
        # quoted table-header key may itself contain `=` or `]` (e.g. a
        # filesystem path used as a projects key). The header regex used to
        # reject this shape entirely, treating the table's contents as
        # top-level.
        content = '[projects."/a=b]c"]\nmodel = "should-stay-here"\n'
        result = provision._insert_toml_top_level_line(content, 'model = "top-level"')
        data = tomllib.loads(result)
        self.assertEqual(data["model"], "top-level")
        self.assertEqual(data["projects"]["/a=b]c"]["model"], "should-stay-here")

    def test_reconcile_codex_toml_leaves_file_untouched_when_edit_does_not_round_trip(self) -> None:
        # Backstop test for ptone/scion#2365 review round 3 (F2): a
        # top-level multi-line basic string whose body happens to contain a
        # header-shaped line (e.g. a `developer_instructions` block
        # documenting TOML syntax) is a known gap this line-oriented editor
        # can't handle — the inserted `model` line lands inside the string
        # text instead of at the real top level. The result is still valid
        # TOML (the extra text is just part of the string), so it parses,
        # but the intended top-level `model` never actually appears. Rather
        # than silently ship that, _reconcile_codex_toml must detect the
        # mismatch, leave the existing file untouched, and log a warning.
        original = (
            'developer_instructions = """\n'
            "Header lines look like this:\n"
            "[IMPORTANT]\n"
            "Explanation text.\n"
            '"""\n'
            "[features]\n"
            "hooks = true\n"
        )

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

                provision._reconcile_codex_toml(ctx, None, None, model="gpt-6.1-sol")

                with open(config_path, "r", encoding="utf-8") as f:
                    after = f.read()

        self.assertEqual(after, original, "file must be left untouched when the edit doesn't round-trip")
        self.assertTrue(
            any("round-trip" in w for w in warnings),
            f"expected a round-trip warning to be logged, got: {warnings}",
        )

    def test_toml_edit_round_trips_accepts_valid_content_with_matching_model(self) -> None:
        self.assertTrue(provision._toml_edit_round_trips('model = "x"\n', "x"))

    def test_toml_edit_round_trips_rejects_invalid_toml(self) -> None:
        self.assertFalse(provision._toml_edit_round_trips("model = [unterminated\n", "x"))

    def test_toml_edit_round_trips_rejects_missing_top_level_model(self) -> None:
        self.assertFalse(provision._toml_edit_round_trips('[table]\nmodel = "x"\n', "x"))

    def test_toml_edit_round_trips_ignores_model_when_none_expected(self) -> None:
        self.assertTrue(provision._toml_edit_round_trips("other_key = 1\n", None))

    def test_strip_toml_top_level_key_section_safety(self) -> None:
        content = '[otel]\nreasoning_effort = "low"\n[other]\nkey = "val"\n'
        result = provision._strip_toml_top_level_key(content, "reasoning_effort")
        self.assertIn('reasoning_effort = "low"', result)

    def test_strip_toml_top_level_key_does_not_match_prefixed_keys(self) -> None:
        content = 'reasoning_effort = "low"\nreasoning_effort_extended = "yes"\n'
        result = provision._strip_toml_top_level_key(content, "reasoning_effort")
        self.assertNotIn('reasoning_effort = "low"', result)
        self.assertIn('reasoning_effort_extended = "yes"', result)


if __name__ == "__main__":
    unittest.main()
