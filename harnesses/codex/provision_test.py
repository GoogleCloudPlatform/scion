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
import io
import tempfile
import unittest
from contextlib import contextmanager
from contextlib import redirect_stderr

PROVISION_PATH = os.path.join(os.path.dirname(__file__), "provision.py")
SPEC = importlib.util.spec_from_file_location("codex_provision", PROVISION_PATH)
assert SPEC is not None
provision = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
SPEC.loader.exec_module(provision)


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
    def test_instruction_projection_composes_prompts_and_skills(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            home = os.path.join(tmp, "home")
            manifest_home = os.path.join(tmp, "host-side-home")
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
                "agent_home": manifest_home,
                "harness_config": {
                    "instructions_file": ".codex/AGENTS.md",
                    "skills_dir": ".codex/skills",
                    "system_prompt_mode": "prepend_to_instructions",
                },
            }

            with temporary_home(home):
                provision._apply_instruction_projection(bundle, manifest)
                provision._apply_instruction_projection(bundle, manifest)

            with open(os.path.join(home, ".codex", "AGENTS.md"), "r", encoding="utf-8") as f:
                content = f.read()

            self.assertEqual(content.count(provision.SCION_MANAGED_BEGIN), 1)
            self.assertIn("# System Instruction\n\nSystem rules", content)
            self.assertIn("# Agent Instructions\n\nAgent rules", content)
            self.assertIn("# Skills\n\n## example\n\n# Example Skill", content)
            self.assertIn("# Skills\n\n## example\n\n# Example Skill\n\nUse this skill.\n\n## second", content)
            self.assertIn(
                "# System Instruction\n\nSystem rules\n\n"
                "# Agent Instructions\n\nAgent rules\n\n"
                "# Skills\n\n## example",
                content,
            )

    def test_instruction_projection_cleans_stale_managed_block_when_inputs_empty(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            home = os.path.join(tmp, "home")
            manifest_home = os.path.join(tmp, "host-side-home")
            bundle = os.path.join(tmp, "bundle")
            os.makedirs(os.path.join(bundle, "inputs"))
            os.makedirs(os.path.join(home, ".codex"))

            agents_path = os.path.join(home, ".codex", "AGENTS.md")
            with open(agents_path, "w", encoding="utf-8") as f:
                f.write(
                    f"{provision.SCION_MANAGED_BEGIN}\n\n"
                    "# Agent Instructions\n\nOld managed content\n\n"
                    f"{provision.SCION_MANAGED_END}\n\n"
                    "# User Notes\n\nKeep this.\n"
                )

            manifest = {
                "agent_home": manifest_home,
                "harness_config": {
                    "instructions_file": ".codex/AGENTS.md",
                    "skills_dir": ".codex/skills",
                    "system_prompt_mode": "prepend_to_instructions",
                },
            }

            with temporary_home(home):
                provision._apply_instruction_projection(bundle, manifest)

            with open(agents_path, "r", encoding="utf-8") as f:
                content = f.read()

            self.assertNotIn(provision.SCION_MANAGED_BEGIN, content)
            self.assertNotIn("Old managed content", content)
            self.assertEqual(content, "# User Notes\n\nKeep this.\n")

    def test_instruction_projection_removes_file_when_only_stale_managed_block_remains(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            home = os.path.join(tmp, "home")
            manifest_home = os.path.join(tmp, "host-side-home")
            bundle = os.path.join(tmp, "bundle")
            os.makedirs(os.path.join(bundle, "inputs"))
            os.makedirs(os.path.join(home, ".codex"))

            agents_path = os.path.join(home, ".codex", "AGENTS.md")
            with open(agents_path, "w", encoding="utf-8") as f:
                f.write(
                    f"{provision.SCION_MANAGED_BEGIN}\n\n"
                    "# Agent Instructions\n\nOld managed content\n\n"
                    f"{provision.SCION_MANAGED_END}\n"
                )

            manifest = {
                "agent_home": manifest_home,
                "harness_config": {
                    "instructions_file": ".codex/AGENTS.md",
                    "skills_dir": ".codex/skills",
                    "system_prompt_mode": "prepend_to_instructions",
                },
            }

            with temporary_home(home):
                provision._apply_instruction_projection(bundle, manifest)

            self.assertFalse(os.path.exists(agents_path))

    def test_strip_scion_managed_block_preserves_content_when_end_missing(self) -> None:
        content = (
            "# Before\n\n"
            f"{provision.SCION_MANAGED_BEGIN}\n\n"
            "# Agent Instructions\n\nManaged without an end marker\n\n"
            "# After\n"
        )
        stderr = io.StringIO()

        with redirect_stderr(stderr):
            got = provision._strip_scion_managed_block(content)

        self.assertEqual(got, content)
        self.assertIn("Aborting strip to prevent data loss", stderr.getvalue())

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


    def test_apply_thinking_budget_inserts_key(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            with temporary_home(tmp):
                codex_dir = os.path.join(tmp, ".codex")
                os.makedirs(codex_dir)
                config_path = os.path.join(codex_dir, "config.toml")
                with open(config_path, "w") as f:
                    f.write('[otel]\nenabled = true\n')

                provision._apply_thinking_budget("model_reasoning_effort", "medium")

                with open(config_path) as f:
                    content = f.read()
                self.assertIn('model_reasoning_effort = "medium"', content)
                self.assertIn("[otel]", content)

    def test_apply_thinking_budget_replaces_existing_key(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            with temporary_home(tmp):
                codex_dir = os.path.join(tmp, ".codex")
                os.makedirs(codex_dir)
                config_path = os.path.join(codex_dir, "config.toml")
                with open(config_path, "w") as f:
                    f.write('model_reasoning_effort = "low"\n\n[otel]\nenabled = true\n')

                provision._apply_thinking_budget("model_reasoning_effort", "high")

                with open(config_path) as f:
                    content = f.read()
                self.assertIn('model_reasoning_effort = "high"', content)
                self.assertNotIn('"low"', content)
                self.assertEqual(content.count("model_reasoning_effort"), 1)


if __name__ == "__main__":
    unittest.main()
