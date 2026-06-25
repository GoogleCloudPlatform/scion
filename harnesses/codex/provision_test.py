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

PROVISION_PATH = os.path.join(os.path.dirname(__file__), "provision.py")
SPEC = importlib.util.spec_from_file_location("codex_provision", PROVISION_PATH)
assert SPEC is not None
provision = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
SPEC.loader.exec_module(provision)


class CodexProvisionTest(unittest.TestCase):
    def test_instruction_projection_composes_prompts_and_skills(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            home = os.path.join(tmp, "home")
            bundle = os.path.join(tmp, "bundle")
            os.makedirs(os.path.join(bundle, "inputs"))
            os.makedirs(os.path.join(home, ".codex", "skills", "example"))

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

            manifest = {
                "agent_home": home,
                "harness_config": {
                    "instructions_file": ".codex/AGENTS.md",
                    "skills_dir": ".codex/skills",
                    "system_prompt_mode": "prepend_to_instructions",
                },
            }

            provision._apply_instruction_projection(bundle, manifest)
            provision._apply_instruction_projection(bundle, manifest)

            with open(os.path.join(home, ".codex", "AGENTS.md"), "r", encoding="utf-8") as f:
                content = f.read()

            self.assertEqual(content.count(provision.SCION_MANAGED_BEGIN), 1)
            self.assertIn("# System Instruction\n\nSystem rules", content)
            self.assertIn("# Agent Instructions\n\nAgent rules", content)
            self.assertIn("# Skills\n\n## example\n\n# Example Skill", content)


if __name__ == "__main__":
    unittest.main()
