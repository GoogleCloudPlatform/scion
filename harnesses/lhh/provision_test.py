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
SPEC = importlib.util.spec_from_file_location("lhh_provision", PROVISION_PATH)
assert SPEC is not None
provision = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
SPEC.loader.exec_module(provision)

scion_harness = provision.scion_harness

MANAGED_BEGIN = "<!-- BEGIN SCION MANAGED -->"
MANAGED_END = "<!-- END SCION MANAGED -->"


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
def temporary_env(**kwargs):
    """Temporarily set environment variables, restoring on exit."""
    old_values = {}
    for key, value in kwargs.items():
        old_values[key] = os.environ.get(key)
        if value is None:
            os.environ.pop(key, None)
        else:
            os.environ[key] = value
    try:
        yield
    finally:
        for key, old_value in old_values.items():
            if old_value is None:
                os.environ.pop(key, None)
            else:
                os.environ[key] = old_value


class LHHProvisionTest(unittest.TestCase):
    def test_instruction_projection_with_skills(self) -> None:
        """Verify that project_instructions with include_skills=True inlines skills."""
        with tempfile.TemporaryDirectory() as tmp:
            home = os.path.join(tmp, "home")
            bundle = os.path.join(tmp, "bundle")
            os.makedirs(os.path.join(bundle, "inputs"))
            os.makedirs(os.path.join(home, ".lhh"))

            with open(os.path.join(bundle, "inputs", "system-prompt.md"), "w", encoding="utf-8") as f:
                f.write("System rules")
            with open(os.path.join(bundle, "inputs", "instructions.md"), "w", encoding="utf-8") as f:
                f.write("Agent rules")

            manifest = {
                "harness_bundle_dir": bundle,
                "harness_config": {
                    "instructions_file": ".lhh/AGENTS.md",
                    "system_prompt_mode": "prepend_to_instructions",
                },
            }

            with temporary_home(home):
                ctx = scion_harness.ProvisionContext("lhh", manifest)
                scion_harness.project_instructions(ctx, ".lhh/AGENTS.md", include_skills=True)

            with open(os.path.join(home, ".lhh", "AGENTS.md"), "r", encoding="utf-8") as f:
                content = f.read()

            self.assertEqual(content.count(MANAGED_BEGIN), 1)
            self.assertIn("System rules", content)
            self.assertIn("Agent rules", content)

    def test_instruction_projection_idempotent(self) -> None:
        """Verify that running project_instructions twice produces the same result."""
        with tempfile.TemporaryDirectory() as tmp:
            home = os.path.join(tmp, "home")
            bundle = os.path.join(tmp, "bundle")
            os.makedirs(os.path.join(bundle, "inputs"))
            os.makedirs(os.path.join(home, ".lhh"))

            with open(os.path.join(bundle, "inputs", "system-prompt.md"), "w", encoding="utf-8") as f:
                f.write("System rules")
            with open(os.path.join(bundle, "inputs", "instructions.md"), "w", encoding="utf-8") as f:
                f.write("Agent rules")

            manifest = {
                "harness_bundle_dir": bundle,
                "harness_config": {
                    "instructions_file": ".lhh/AGENTS.md",
                    "system_prompt_mode": "prepend_to_instructions",
                },
            }

            with temporary_home(home):
                ctx = scion_harness.ProvisionContext("lhh", manifest)
                scion_harness.project_instructions(ctx, ".lhh/AGENTS.md", include_skills=True)
                scion_harness.project_instructions(ctx, ".lhh/AGENTS.md", include_skills=True)

            with open(os.path.join(home, ".lhh", "AGENTS.md"), "r", encoding="utf-8") as f:
                content = f.read()

            self.assertEqual(content.count(MANAGED_BEGIN), 1)

    def test_resolve_model_default(self) -> None:
        """Verify default model when SCION_MODEL is not set."""
        with tempfile.TemporaryDirectory() as tmp:
            home = os.path.join(tmp, "home")
            bundle = os.path.join(tmp, "bundle")
            os.makedirs(os.path.join(bundle, "inputs"))

            manifest = {
                "harness_bundle_dir": bundle,
                "harness_config": {
                    "model_aliases": {
                        "small": "gemini-flash-lite",
                        "medium": "gemini-3.6-flash",
                        "large": "gemini-3.1-pro-preview",
                        "extra-large": "gemini-3.1-pro-preview",
                    },
                },
            }

            with temporary_home(home), temporary_env(SCION_MODEL=""):
                ctx = scion_harness.ProvisionContext("lhh", manifest)
                model = provision._resolve_model(ctx)
                self.assertEqual(model, "gemini-3.6-flash")

    def test_resolve_model_alias(self) -> None:
        """Verify that SCION_MODEL alias is resolved to concrete model."""
        with tempfile.TemporaryDirectory() as tmp:
            home = os.path.join(tmp, "home")
            bundle = os.path.join(tmp, "bundle")
            os.makedirs(os.path.join(bundle, "inputs"))

            manifest = {
                "harness_bundle_dir": bundle,
                "harness_config": {
                    "model_aliases": {
                        "small": "gemini-flash-lite",
                        "medium": "gemini-3.6-flash",
                        "large": "gemini-3.1-pro-preview",
                        "extra-large": "gemini-3.1-pro-preview",
                    },
                },
            }

            with temporary_home(home), temporary_env(SCION_MODEL="large"):
                ctx = scion_harness.ProvisionContext("lhh", manifest)
                model = provision._resolve_model(ctx)
                self.assertEqual(model, "gemini-3.1-pro-preview")

    def test_resolve_model_shorthand(self) -> None:
        """Verify single-letter shorthand aliases."""
        with tempfile.TemporaryDirectory() as tmp:
            home = os.path.join(tmp, "home")
            bundle = os.path.join(tmp, "bundle")
            os.makedirs(os.path.join(bundle, "inputs"))

            manifest = {
                "harness_bundle_dir": bundle,
                "harness_config": {
                    "model_aliases": {
                        "small": "gemini-flash-lite",
                        "medium": "gemini-3.6-flash",
                        "large": "gemini-3.1-pro-preview",
                        "extra-large": "gemini-3.1-pro-preview",
                    },
                },
            }

            with temporary_home(home), temporary_env(SCION_MODEL="L"):
                ctx = scion_harness.ProvisionContext("lhh", manifest)
                model = provision._resolve_model(ctx)
                self.assertEqual(model, "gemini-3.1-pro-preview")

            with temporary_home(home), temporary_env(SCION_MODEL="s"):
                ctx = scion_harness.ProvisionContext("lhh", manifest)
                model = provision._resolve_model(ctx)
                self.assertEqual(model, "gemini-flash-lite")

            with temporary_home(home), temporary_env(SCION_MODEL="xl"):
                ctx = scion_harness.ProvisionContext("lhh", manifest)
                model = provision._resolve_model(ctx)
                self.assertEqual(model, "gemini-3.1-pro-preview")

    def test_resolve_model_concrete_passthrough(self) -> None:
        """Verify that a concrete model name passes through unchanged."""
        with tempfile.TemporaryDirectory() as tmp:
            home = os.path.join(tmp, "home")
            bundle = os.path.join(tmp, "bundle")
            os.makedirs(os.path.join(bundle, "inputs"))

            manifest = {
                "harness_bundle_dir": bundle,
                "harness_config": {
                    "model_aliases": {
                        "small": "gemini-flash-lite",
                    },
                },
            }

            with temporary_home(home), temporary_env(SCION_MODEL="gemini-2.5-pro"):
                ctx = scion_harness.ProvisionContext("lhh", manifest)
                model = provision._resolve_model(ctx)
                self.assertEqual(model, "gemini-2.5-pro")

    def test_build_env_overlay(self) -> None:
        """Verify env overlay contains all required keys."""
        env = provision._build_env_overlay("gemini-3.6-flash")
        self.assertEqual(env["GOOGLE_CLOUD_PROJECT"], "${GOOGLE_CLOUD_PROJECT}")
        self.assertEqual(env["GOOGLE_CLOUD_REGION"], "${GOOGLE_CLOUD_REGION}")
        self.assertEqual(env["GOOGLE_CLOUD_LOCATION"], "${GOOGLE_CLOUD_REGION}")
        self.assertEqual(env["GOOGLE_GENAI_USE_VERTEXAI"], "True")
        self.assertEqual(env["LHA_ROOT_MODEL"], "gemini-3.6-flash")
        self.assertEqual(env["LHA_ENVIRONMENT_BACKEND"], "local")


if __name__ == "__main__":
    unittest.main()
