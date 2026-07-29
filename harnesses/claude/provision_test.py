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
"""Unit tests for the Claude harness provisioner.

Run with:  python3 -m unittest provision_test -v
"""

from __future__ import annotations

import importlib.util
import json
import os
import tempfile
import unittest
from contextlib import contextmanager

PROVISION_PATH = os.path.join(os.path.dirname(__file__), "provision.py")
SPEC = importlib.util.spec_from_file_location("claude_provision", PROVISION_PATH)
assert SPEC is not None
provision = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
SPEC.loader.exec_module(provision)

scion_harness = provision.scion_harness

MODEL_ALIASES = {
    "small": "haiku",
    "medium": "sonnet",
    "large": "opus",
    "extra-large": "fable",
}


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
def env_vars(**values: str | None):
    """Temporarily set (or unset, with None) environment variables."""
    previous = {k: os.environ.get(k) for k in values}
    for key, val in values.items():
        if val is None:
            os.environ.pop(key, None)
        else:
            os.environ[key] = val
    try:
        yield
    finally:
        for key, val in previous.items():
            if val is None:
                os.environ.pop(key, None)
            else:
                os.environ[key] = val


def make_ctx(home: str, *, model_resolution: dict | None = None):
    manifest = {
        "harness_bundle_dir": os.path.join(home, ".scion", "harness"),
        "harness_config": {
            "model_aliases": dict(MODEL_ALIASES),
            "instructions_file": ".claude/CLAUDE.md",
        },
    }
    if model_resolution is not None:
        manifest["model_resolution"] = model_resolution
    return scion_harness.ProvisionContext("claude", manifest)


class ModelResolutionTest(unittest.TestCase):
    def test_size_alias_resolves_through_config_model_aliases(self) -> None:
        with tempfile.TemporaryDirectory() as tmp, temporary_home(tmp):
            ctx = make_ctx(tmp)
            with env_vars(SCION_MODEL="medium", ANTHROPIC_MODEL=None):
                env: dict[str, str] = {}
                model = provision._apply_model(ctx, env)

        self.assertEqual(model, "sonnet")
        self.assertEqual(env["ANTHROPIC_MODEL"], "sonnet")

    def test_alias_matching_is_case_insensitive_and_accepts_shorthand(self) -> None:
        cases = {
            "Medium": "sonnet",
            "L": "opus",
            "XL": "fable",
            "SMALL": "haiku",
        }
        for raw, want in cases.items():
            with self.subTest(raw=raw):
                with tempfile.TemporaryDirectory() as tmp, temporary_home(tmp):
                    ctx = make_ctx(tmp)
                    self.assertEqual(provision._resolve_model_alias(ctx, raw), want)

    def test_concrete_model_passes_through_unchanged(self) -> None:
        with tempfile.TemporaryDirectory() as tmp, temporary_home(tmp):
            ctx = make_ctx(tmp)
            with env_vars(SCION_MODEL="claude-sonnet-4-5", ANTHROPIC_MODEL=None):
                env: dict[str, str] = {}
                model = provision._apply_model(ctx, env)

        self.assertEqual(model, "claude-sonnet-4-5")
        self.assertEqual(env["ANTHROPIC_MODEL"], "claude-sonnet-4-5")

    def test_manifest_model_resolution_wins_over_scion_model_env(self) -> None:
        with tempfile.TemporaryDirectory() as tmp, temporary_home(tmp):
            ctx = make_ctx(tmp, model_resolution={"resolved_model": "haiku"})
            with env_vars(SCION_MODEL="large", ANTHROPIC_MODEL=None):
                env: dict[str, str] = {}
                model = provision._apply_model(ctx, env)

        self.assertEqual(model, "haiku")

    def test_no_requested_model_falls_back_to_default(self) -> None:
        with tempfile.TemporaryDirectory() as tmp, temporary_home(tmp):
            ctx = make_ctx(tmp)
            with env_vars(SCION_MODEL=None, ANTHROPIC_MODEL=None):
                env: dict[str, str] = {}
                model = provision._apply_model(ctx, env)

        self.assertEqual(model, provision.DEFAULT_MODEL)
        self.assertEqual(env["ANTHROPIC_MODEL"], provision.DEFAULT_MODEL)

    def test_resolved_model_written_to_claude_settings(self) -> None:
        with tempfile.TemporaryDirectory() as tmp, temporary_home(tmp):
            settings_path = os.path.join(tmp, ".claude", "settings.json")
            os.makedirs(os.path.dirname(settings_path))
            with open(settings_path, "w", encoding="utf-8") as f:
                json.dump({"includeCoAuthoredBy": False}, f)

            ctx = make_ctx(tmp)
            with env_vars(SCION_MODEL="medium", ANTHROPIC_MODEL=None):
                provision._apply_model(ctx, {})

            with open(settings_path, "r", encoding="utf-8") as f:
                settings = json.load(f)

        self.assertEqual(settings["model"], "sonnet")
        # Existing keys are preserved.
        self.assertIs(settings["includeCoAuthoredBy"], False)

    def test_preset_anthropic_model_is_reported_but_not_overwritten(self) -> None:
        """A container env ANTHROPIC_MODEL outranks the overlay — warn about it."""
        warnings: list[str] = []
        with tempfile.TemporaryDirectory() as tmp, temporary_home(tmp):
            ctx = make_ctx(tmp)
            ctx.warn = warnings.append  # type: ignore[method-assign]
            with env_vars(SCION_MODEL="medium", ANTHROPIC_MODEL="opus"):
                env: dict[str, str] = {}
                model = provision._apply_model(ctx, env)

        self.assertEqual(model, "sonnet")
        self.assertEqual(env["ANTHROPIC_MODEL"], "sonnet")
        self.assertEqual(len(warnings), 1)
        self.assertIn("ANTHROPIC_MODEL", warnings[0])

    def test_no_warning_when_preset_matches_resolved_model(self) -> None:
        warnings: list[str] = []
        with tempfile.TemporaryDirectory() as tmp, temporary_home(tmp):
            ctx = make_ctx(tmp)
            ctx.warn = warnings.append  # type: ignore[method-assign]
            with env_vars(SCION_MODEL="medium", ANTHROPIC_MODEL="sonnet"):
                provision._apply_model(ctx, {})

        self.assertEqual(warnings, [])

    def test_no_warning_when_no_model_requested(self) -> None:
        """An operator-set ANTHROPIC_MODEL with no requested model is intentional."""
        warnings: list[str] = []
        with tempfile.TemporaryDirectory() as tmp, temporary_home(tmp):
            ctx = make_ctx(tmp)
            ctx.warn = warnings.append  # type: ignore[method-assign]
            with env_vars(SCION_MODEL=None, ANTHROPIC_MODEL="claude-opus-4-6"):
                provision._apply_model(ctx, {})

        self.assertEqual(warnings, [])


class ConfigYamlTest(unittest.TestCase):
    def test_config_yaml_does_not_pin_anthropic_model(self) -> None:
        """config.yaml env entries land in the container env and would win.

        Keeping ANTHROPIC_MODEL out of that block is what lets provision.py's
        env overlay apply the agent's requested model.
        """
        config_path = os.path.join(os.path.dirname(__file__), "config.yaml")
        with open(config_path, "r", encoding="utf-8") as f:
            lines = [line.rstrip("\n") for line in f]

        in_env = False
        env_keys: list[str] = []
        for line in lines:
            if line.startswith("env:"):
                in_env = True
                continue
            if in_env:
                if not line.startswith((" ", "\t")):
                    break
                stripped = line.strip()
                if not stripped or stripped.startswith("#"):
                    continue
                env_keys.append(stripped.split(":", 1)[0].strip())

        self.assertIn("ANTHROPIC_DEFAULT_HAIKU_MODEL", env_keys)
        self.assertNotIn("ANTHROPIC_MODEL", env_keys)


if __name__ == "__main__":
    unittest.main()
