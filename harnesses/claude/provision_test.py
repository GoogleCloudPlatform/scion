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


def make_ctx(home: str):
    manifest = {
        "harness_bundle_dir": os.path.join(home, ".scion", "harness"),
        "harness_config": {
            "model_aliases": dict(MODEL_ALIASES),
            "instructions_file": ".claude/CLAUDE.md",
        },
    }
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
            "extra-large": "fable",
        }
        for raw, want in cases.items():
            with self.subTest(raw=raw):
                with tempfile.TemporaryDirectory() as tmp, temporary_home(tmp):
                    ctx = make_ctx(tmp)
                    self.assertEqual(provision._resolve_model_alias(ctx, raw), want)

    def test_shorthand_set_matches_go_normalize_model_alias(self) -> None:
        """Python must not accept spellings the Go --model path rejects.

        config.NormalizeModelAlias (pkg/config/templates.go) handles only
        s/m/l/xl plus lower-casing, and config.ResolveModelAlias gates the
        map lookup on config.KnownModelAliases. A spelling accepted only here
        would make ANTHROPIC_MODEL disagree with the higher-precedence
        --model flag.
        """
        self.assertEqual(
            provision.MODEL_ALIAS_SHORTHAND,
            {"s": "small", "m": "medium", "l": "large", "xl": "extra-large"},
        )
        self.assertEqual(
            provision.KNOWN_MODEL_ALIASES,
            frozenset({"small", "medium", "large", "extra-large"}),
        )

        with tempfile.TemporaryDirectory() as tmp, temporary_home(tmp):
            ctx = make_ctx(tmp)
            # Spellings Go does not normalize stay concrete (lower-cased),
            # exactly as `config.ResolveModelAlias` would leave them.
            for raw in ("xlarge", "extra_large"):
                with self.subTest(raw=raw):
                    self.assertEqual(provision._resolve_model_alias(ctx, raw), raw)

    def test_unmapped_alias_falls_back_to_the_tier_name(self) -> None:
        """Matches config.ResolveModelAlias: unmapped alias passes through."""
        with tempfile.TemporaryDirectory() as tmp, temporary_home(tmp):
            manifest = {
                "harness_bundle_dir": os.path.join(tmp, ".scion", "harness"),
                "harness_config": {"model_aliases": {"small": "haiku"}},
            }
            ctx = scion_harness.ProvisionContext("claude", manifest)
            self.assertEqual(provision._resolve_model_alias(ctx, "large"), "large")

    def test_custom_non_tier_alias_keys_are_ignored(self) -> None:
        """Only the four canonical tiers resolve, as on the Go side."""
        with tempfile.TemporaryDirectory() as tmp, temporary_home(tmp):
            manifest = {
                "harness_bundle_dir": os.path.join(tmp, ".scion", "harness"),
                "harness_config": {"model_aliases": {"fast": "haiku"}},
            }
            ctx = scion_harness.ProvisionContext("claude", manifest)
            self.assertEqual(provision._resolve_model_alias(ctx, "fast"), "fast")

    def test_concrete_model_passes_through_unchanged(self) -> None:
        with tempfile.TemporaryDirectory() as tmp, temporary_home(tmp):
            ctx = make_ctx(tmp)
            with env_vars(SCION_MODEL="claude-sonnet-4-5", ANTHROPIC_MODEL=None):
                env: dict[str, str] = {}
                model = provision._apply_model(ctx, env)

        self.assertEqual(model, "claude-sonnet-4-5")
        self.assertEqual(env["ANTHROPIC_MODEL"], "claude-sonnet-4-5")

    def test_no_requested_model_falls_back_to_default(self) -> None:
        with tempfile.TemporaryDirectory() as tmp, temporary_home(tmp):
            ctx = make_ctx(tmp)
            with env_vars(SCION_MODEL=None, ANTHROPIC_MODEL=None):
                env: dict[str, str] = {}
                model = provision._apply_model(ctx, env)

        self.assertEqual(model, provision.DEFAULT_MODEL)
        self.assertEqual(env["ANTHROPIC_MODEL"], provision.DEFAULT_MODEL)

    def test_claude_settings_json_is_left_untouched(self) -> None:
        """The provisioner must not rewrite the file that holds the deny list.

        ~/.claude/settings.json carries the agent's permissions.deny list,
        the disable* flags and the sciontool hooks. Model selection goes
        through ANTHROPIC_MODEL, which outranks the settings `model` key
        anyway, so there is no reason for this script to touch the file.
        """
        seed = os.path.join(
            os.path.dirname(__file__), "home", ".claude", "settings.json"
        )
        with open(seed, "rb") as f:
            original = f.read()

        with tempfile.TemporaryDirectory() as tmp, temporary_home(tmp):
            settings_path = os.path.join(tmp, ".claude", "settings.json")
            os.makedirs(os.path.dirname(settings_path))
            with open(settings_path, "wb") as f:
                f.write(original)

            ctx = make_ctx(tmp)
            with env_vars(SCION_MODEL="medium", ANTHROPIC_MODEL=None):
                provision._apply_model(ctx, {})

            with open(settings_path, "rb") as f:
                after = f.read()

        self.assertEqual(after, original)
        # Sanity-check the fixture really is the containment-bearing file.
        settings = json.loads(original)
        self.assertIn("EnterPlanMode", settings["permissions"]["deny"])
        self.assertIs(settings["disableBundledSkills"], True)

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
