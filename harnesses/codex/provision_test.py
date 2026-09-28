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

    def test_disabled_otel_disables_all_exporters(self) -> None:
        with tempfile.TemporaryDirectory() as home, temporary_home(home):
            os.makedirs(os.path.join(home, ".codex"), exist_ok=True)
            with open(os.path.join(home, ".codex", "config.toml"), "w", encoding="utf-8") as f:
                f.write('[otel.exporter."otlp-grpc"]\nendpoint = "https://external.invalid:443"\n')
            provision._reconcile_codex_toml(None, None)
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
                provision._reconcile_codex_toml(None, None, reasoning_effort="medium")
                config_path = os.path.join(tmp, ".codex", "config.toml")
                with open(config_path, "r", encoding="utf-8") as f:
                    content = f.read()
                self.assertIn('model_reasoning_effort = "medium"', content)

    def test_reconcile_codex_toml_omits_reasoning_effort_when_none(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            with temporary_home(tmp):
                provision._reconcile_codex_toml(None, None, reasoning_effort=None)
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
                provision._reconcile_codex_toml(None, None, reasoning_effort="high")
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
                provision._reconcile_codex_toml(None, None, reasoning_effort="high")
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
                provision._reconcile_codex_toml(None, None, reasoning_effort="high")
                with open(config_path, "r", encoding="utf-8") as f:
                    content = f.read()
                self.assertIn('model_reasoning_effort = "high"', content)
                self.assertEqual(content.count("model_reasoning_effort"), 1)
                self.assertNotIn('reasoning_effort = "low"', content)
                self.assertNotIn('reasoning_effort = "medium"', content)
                self.assertIn('other_key = "value"', content)

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
