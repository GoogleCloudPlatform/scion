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

    def test_build_otel_section_emits_traces_metrics_environment_and_tls(self) -> None:
        telemetry = {
            "enabled": True,
            "cloud": {
                "endpoint": "https://otel.example.com/v1/logs",
                "protocol": "http",
                "headers": {"x-otlp-meta": "abc123", "authorization": "Bearer token"},
                "tls": {"ca_file": "/etc/scion/ca.pem"},
            },
            "resource": {"deployment.environment": "staging"},
            "filter": {"events": {"include": ["agent.user.prompt"]}},
        }

        section = provision._build_otel_section(telemetry, None)

        self.assertIn('environment = "staging"', section)
        self.assertIn("log_user_prompt = true", section)
        self.assertIn('metrics_exporter = "statsig"', section)
        self.assertIn('exporter."otlp-http".endpoint = "https://otel.example.com/v1/logs"', section)
        self.assertIn('trace_exporter."otlp-http".endpoint = "https://otel.example.com/v1/logs"', section)
        self.assertIn(
            'exporter."otlp-http".headers = { "authorization" = "Bearer token", "x-otlp-meta" = "abc123" }',
            section,
        )
        self.assertIn(
            'trace_exporter."otlp-http".headers = { "authorization" = "Bearer token", "x-otlp-meta" = "abc123" }',
            section,
        )
        self.assertIn('exporter."otlp-http".tls.ca-certificate = "/etc/scion/ca.pem"', section)
        self.assertIn('trace_exporter."otlp-http".tls.ca-certificate = "/etc/scion/ca.pem"', section)

    def test_build_otel_section_uses_env_overrides_and_production_default(self) -> None:
        telemetry = {
            "enabled": True,
            "cloud": {
                "endpoint": "localhost:4317",
                "protocol": "grpc",
            },
            "resource": {"deployment.environment": "staging"},
            "filter": {"events": {"include": ["agent.user.prompt"], "exclude": ["agent.user.prompt"]}},
        }
        env = {
            "SCION_CODEX_OTEL_ENDPOINT": "collector.internal:4317",
            "SCION_CODEX_OTEL_PROTOCOL": "grpc",
            "SCION_CODEX_OTEL_ENVIRONMENT": "dev",
        }

        section = provision._build_otel_section(telemetry, env)

        self.assertIn('environment = "dev"', section)
        self.assertIn("log_user_prompt = false", section)
        self.assertIn('exporter."otlp-grpc".endpoint = "collector.internal:4317"', section)
        self.assertIn('trace_exporter."otlp-grpc".endpoint = "collector.internal:4317"', section)

        defaulted = provision._build_otel_section({"enabled": True}, None)
        self.assertIn('environment = "production"', defaulted)


    def test_resolve_reasoning_effort_maps_thinking_levels(self) -> None:
        self.assertEqual(provision._resolve_reasoning_effort(0), "low")
        self.assertEqual(provision._resolve_reasoning_effort(33), "low")
        self.assertEqual(provision._resolve_reasoning_effort(34), "medium")
        self.assertEqual(provision._resolve_reasoning_effort(50), "medium")
        self.assertEqual(provision._resolve_reasoning_effort(66), "medium")
        self.assertEqual(provision._resolve_reasoning_effort(67), "high")
        self.assertEqual(provision._resolve_reasoning_effort(100), "high")

    def test_resolve_reasoning_effort_clamps_out_of_range(self) -> None:
        self.assertEqual(provision._resolve_reasoning_effort(-10), "low")
        self.assertEqual(provision._resolve_reasoning_effort(150), "high")

    def test_reconcile_codex_toml_writes_reasoning_effort(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            with temporary_home(tmp):
                provision._reconcile_codex_toml(None, None, reasoning_effort="medium")
                config_path = os.path.join(tmp, ".codex", "config.toml")
                with open(config_path, "r", encoding="utf-8") as f:
                    content = f.read()
                self.assertIn('reasoning_effort = "medium"', content)

    def test_reconcile_codex_toml_omits_reasoning_effort_when_none(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            with temporary_home(tmp):
                provision._reconcile_codex_toml(None, None, reasoning_effort=None)
                config_path = os.path.join(tmp, ".codex", "config.toml")
                with open(config_path, "r", encoding="utf-8") as f:
                    content = f.read()
                self.assertNotIn("reasoning_effort", content)

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
                self.assertIn('reasoning_effort = "high"', content)
                self.assertNotIn('"low"', content)
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
