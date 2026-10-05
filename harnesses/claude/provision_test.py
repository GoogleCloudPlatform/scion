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
from typing import Any

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

    def test_apply_model_is_case_insensitive_and_accepts_shorthand(self) -> None:
        """scion_harness.resolve_model normalizes shorthand/case before lookup.

        This used to be a claude-local mirror of config.NormalizeModelAlias /
        config.ResolveModelAlias (pkg/config/templates.go); the mirror moved
        to the shared scion_harness.resolve_model helper (unit-tested in
        scion_harness_test.py), so this is now an end-to-end check that
        _apply_model wires SCION_MODEL through that helper correctly.
        """
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
                    with env_vars(SCION_MODEL=raw, ANTHROPIC_MODEL=None):
                        env: dict[str, str] = {}
                        model = provision._apply_model(ctx, env)
                self.assertEqual(model, want)
                self.assertEqual(env["ANTHROPIC_MODEL"], want)

    def test_apply_model_unmapped_alias_falls_back_to_the_tier_name(self) -> None:
        """Matches config.ResolveModelAlias: unmapped alias passes through."""
        with tempfile.TemporaryDirectory() as tmp, temporary_home(tmp):
            manifest = {
                "harness_bundle_dir": os.path.join(tmp, ".scion", "harness"),
                "harness_config": {"model_aliases": {"small": "haiku"}},
            }
            ctx = scion_harness.ProvisionContext("claude", manifest)
            with env_vars(SCION_MODEL="large", ANTHROPIC_MODEL=None):
                env: dict[str, str] = {}
                model = provision._apply_model(ctx, env)
        self.assertEqual(model, "large")

    def test_apply_model_missing_harness_config_falls_back_to_tier_name(self) -> None:
        """Regression guard for the gemini review on GoogleCloudPlatform/scion#1891:
        ctx.harness_config must never be treated as unconditionally truthy/dict-like
        without a guard. A manifest with no "harness_config" key at all (so the
        property defaults to {}) must resolve to the bare tier name, not raise.
        """
        with tempfile.TemporaryDirectory() as tmp, temporary_home(tmp):
            manifest = {
                "harness_bundle_dir": os.path.join(tmp, ".scion", "harness"),
            }
            ctx = scion_harness.ProvisionContext("claude", manifest)
            with env_vars(SCION_MODEL="large", ANTHROPIC_MODEL=None):
                env: dict[str, str] = {}
                model = provision._apply_model(ctx, env)
        self.assertEqual(model, "large")

    def test_apply_model_custom_non_tier_alias_keys_are_ignored(self) -> None:
        """Only the four canonical tiers resolve, as on the Go side."""
        with tempfile.TemporaryDirectory() as tmp, temporary_home(tmp):
            manifest = {
                "harness_bundle_dir": os.path.join(tmp, ".scion", "harness"),
                "harness_config": {"model_aliases": {"fast": "haiku"}},
            }
            ctx = scion_harness.ProvisionContext("claude", manifest)
            with env_vars(SCION_MODEL="fast", ANTHROPIC_MODEL=None):
                env: dict[str, str] = {}
                model = provision._apply_model(ctx, env)
        self.assertEqual(model, "fast")

    def test_concrete_model_passes_through_unchanged(self) -> None:
        with tempfile.TemporaryDirectory() as tmp, temporary_home(tmp):
            ctx = make_ctx(tmp)
            with env_vars(SCION_MODEL="claude-sonnet-4-5", ANTHROPIC_MODEL=None):
                env: dict[str, str] = {}
                model = provision._apply_model(ctx, env)

        self.assertEqual(model, "claude-sonnet-4-5")
        self.assertEqual(env["ANTHROPIC_MODEL"], "claude-sonnet-4-5")

    def test_concrete_model_case_is_preserved(self) -> None:
        """Behavior difference from claude's old private mirror (R1 of the
        round-1 review): the old `_normalize_model_alias` lower-cased every
        value, tier or not, so a mixed-case concrete model name supplied via
        an explicit SCION_MODEL (a template/hub `env:` block — Go's
        reResolveModelAlias only rewrites tier names, not concrete ones)
        would have been lower-cased before reaching ANTHROPIC_MODEL. The
        shared scion_harness.resolve_model only normalizes case to decide
        whether a value is a known tier; a concrete name passes through with
        the caller's original spelling. Anthropic model IDs happen to be
        lowercase already, so this is harmless for claude in practice.
        """
        with tempfile.TemporaryDirectory() as tmp, temporary_home(tmp):
            ctx = make_ctx(tmp)
            with env_vars(SCION_MODEL="Claude-Sonnet-4-5", ANTHROPIC_MODEL=None):
                env: dict[str, str] = {}
                model = provision._apply_model(ctx, env)

        self.assertEqual(model, "Claude-Sonnet-4-5")
        self.assertEqual(env["ANTHROPIC_MODEL"], "Claude-Sonnet-4-5")

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

    def test_opus_5_5_falls_back_to_opus_4_8_on_old_claude_code(self) -> None:
        """Claude Code < 2.1.280 rejects claude-opus-5-5* and opus with 400."""
        for requested in ("claude-opus-5-5", "claude-opus-5-5@default", "opus", None):
            with self.subTest(requested=requested):
                warnings: list[str] = []
                with tempfile.TemporaryDirectory() as tmp, temporary_home(tmp):
                    ctx = make_ctx(tmp)
                    ctx.warn = warnings.append  # type: ignore[method-assign]
                    with env_vars(SCION_MODEL=requested, ANTHROPIC_MODEL=None):
                        env: dict[str, str] = {}
                        model = provision._apply_model(ctx, env, claude_version="2.1.270")

                self.assertEqual(model, "claude-opus-4-8")
                self.assertEqual(env["ANTHROPIC_MODEL"], "claude-opus-4-8")
                self.assertEqual(len(warnings), 1)
                self.assertIn("2.1.280", warnings[0])
                self.assertIn("claude_code_version_too_old", warnings[0])

    def test_opus_5_5_passes_through_on_claude_code_2_1_280_or_newer(self) -> None:
        for version in ("2.1.280", "2.1.295", "2.2.0"):
            for requested in ("claude-opus-5-5@default", "opus"):
                with self.subTest(version=version, requested=requested):
                    warnings: list[str] = []
                    with tempfile.TemporaryDirectory() as tmp, temporary_home(tmp):
                        ctx = make_ctx(tmp)
                        ctx.warn = warnings.append  # type: ignore[method-assign]
                        with env_vars(SCION_MODEL=requested, ANTHROPIC_MODEL=None):
                            env: dict[str, str] = {}
                            model = provision._apply_model(ctx, env, claude_version=version)

                    self.assertEqual(model, requested)
                    self.assertEqual(env["ANTHROPIC_MODEL"], requested)
                    self.assertEqual(warnings, [])

    def test_non_opus_5_5_models_unaffected_by_old_claude_code(self) -> None:
        for requested in ("claude-opus-4-8", "claude-sonnet-4-5", "haiku"):
            with self.subTest(requested=requested):
                warnings: list[str] = []
                with tempfile.TemporaryDirectory() as tmp, temporary_home(tmp):
                    ctx = make_ctx(tmp)
                    ctx.warn = warnings.append  # type: ignore[method-assign]
                    with env_vars(SCION_MODEL=requested, ANTHROPIC_MODEL=None):
                        env: dict[str, str] = {}
                        model = provision._apply_model(ctx, env, claude_version="2.1.270")

                self.assertEqual(model, requested)
                self.assertEqual(env["ANTHROPIC_MODEL"], requested)
                self.assertEqual(warnings, [])

    def test_build_env_overlay_disables_autoupdater(self) -> None:
        with tempfile.TemporaryDirectory() as tmp, temporary_home(tmp):
            ctx = make_ctx(tmp)
            auth = scion_harness.ResolvedAuth(method="api-key", env_key="ANTHROPIC_API_KEY")
            env = provision._build_env_overlay(ctx, auth)

        self.assertEqual(env.get("CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST"), "1")
        self.assertEqual(env.get("DISABLE_AUTOUPDATER"), "1")


# Mirrors harnesses/claude/config.yaml's `thinking:` block exactly. The Go
# test TestEmbeddedHarnessThinkingBlocks pins the yaml side to the same
# literal table, so the two cannot drift.
CLAUDE_THINKING = {
    "levels": [
        {"max": 25, "value": "low"},
        {"max": 50, "value": "medium"},
        {"max": 75, "value": "high"},
        {"max": 100, "value": "xhigh"},
    ],
}


def make_thinking_ctx(home: str, thinking: Any = CLAUDE_THINKING, *, omit_thinking: bool = False):
    ctx = make_ctx(home)
    if not omit_thinking:
        ctx.harness_config["thinking"] = thinking
    return ctx


class EffortTest(unittest.TestCase):
    """ptone/scion#3011: SCION_THINKING_LEVEL -> CLAUDE_CODE_EFFORT_LEVEL."""

    def _effort(
        self,
        raw: str | None,
        *,
        preset: str | None = None,
        thinking: Any = CLAUDE_THINKING,
        omit_thinking: bool = False,
    ) -> tuple[dict[str, str], str | None, list[str]]:
        warnings: list[str] = []
        with tempfile.TemporaryDirectory() as tmp, temporary_home(tmp):
            ctx = make_thinking_ctx(tmp, thinking, omit_thinking=omit_thinking)
            ctx.warn = warnings.append  # type: ignore[method-assign]
            with env_vars(SCION_THINKING_LEVEL=raw, CLAUDE_CODE_EFFORT_LEVEL=preset):
                env: dict[str, str] = {}
                effort = provision._apply_effort(ctx, env)
        return env, effort, warnings

    def test_level_maps_through_quartile_table(self) -> None:
        cases = (
            ("0", "low"),
            ("25", "low"),
            ("26", "medium"),
            ("50", "medium"),
            ("51", "high"),
            ("75", "high"),
            ("76", "xhigh"),
            ("100", "xhigh"),
            ("150", "xhigh"),
            ("-5", "low"),
        )
        for raw, want in cases:
            with self.subTest(raw=raw):
                env, effort, warnings = self._effort(raw)
                self.assertEqual(effort, want)
                self.assertEqual(env, {"CLAUDE_CODE_EFFORT_LEVEL": want})
                self.assertEqual(warnings, [])

    def test_no_level_sets_nothing(self) -> None:
        """Decided requirement: no level means Claude keeps its per-model default."""
        for raw in (None, "", "   "):
            with self.subTest(raw=raw):
                env, effort, warnings = self._effort(raw)
                self.assertIsNone(effort)
                self.assertNotIn("CLAUDE_CODE_EFFORT_LEVEL", env)
                self.assertEqual(warnings, [])

    def test_invalid_level_sets_nothing_and_warns(self) -> None:
        env, effort, warnings = self._effort("abc")
        self.assertIsNone(effort)
        self.assertNotIn("CLAUDE_CODE_EFFORT_LEVEL", env)
        self.assertTrue(any("not a valid integer" in w for w in warnings), warnings)

    def test_preset_env_is_not_clobbered(self) -> None:
        """A template/harness-config env pin wins; the overlay must not shadow it."""
        env, effort, warnings = self._effort("90", preset="low")
        self.assertIsNone(effort)
        self.assertNotIn("CLAUDE_CODE_EFFORT_LEVEL", env)
        self.assertEqual(len(warnings), 1)
        self.assertIn("CLAUDE_CODE_EFFORT_LEVEL='low'", warnings[0])

    def test_preset_env_without_level_is_silent(self) -> None:
        env, effort, warnings = self._effort(None, preset="high")
        self.assertIsNone(effort)
        self.assertEqual(env, {})
        self.assertEqual(warnings, [])

    def test_missing_thinking_block_warns_only_when_level_requested(self) -> None:
        for label, kwargs in (("omitted", {"omit_thinking": True}), ("empty", {"thinking": {}})):
            with self.subTest(block=label):
                env, effort, warnings = self._effort("60", **kwargs)
                self.assertIsNone(effort)
                self.assertEqual(env, {})
                self.assertTrue(any("no thinking block" in w for w in warnings), warnings)
                env, effort, warnings = self._effort(None, **kwargs)
                self.assertEqual((env, effort), ({}, None))
                # An empty block still draws resolve_thinking's "malformed"
                # warning; the requested-level warning must not fire.
                self.assertFalse(any("no thinking block" in w for w in warnings), warnings)

    def test_undocumented_value_is_written_with_warning(self) -> None:
        """A customized table may name `max`, which the env var rejects."""
        thinking = {"levels": [{"max": 100, "value": "max"}]}
        env, effort, warnings = self._effort("60", thinking=thinking)
        self.assertEqual(env, {"CLAUDE_CODE_EFFORT_LEVEL": "max"})
        self.assertEqual(effort, "max")
        self.assertEqual(len(warnings), 1)
        self.assertIn("'max'", warnings[0])


class ProvisionEffortWiringTest(unittest.TestCase):
    """Drives the real provision() entry point and reads outputs/env.json."""

    def _env_json(self, raw: str | None) -> dict[str, str]:
        with tempfile.TemporaryDirectory() as tmp, temporary_home(tmp):
            os.makedirs(os.path.join(tmp, ".scion", "harness", "inputs"))
            ctx = make_thinking_ctx(tmp)
            ctx.select_auth = lambda _: scion_harness.ResolvedAuth("none")  # type: ignore[method-assign]
            with env_vars(
                SCION_THINKING_LEVEL=raw, CLAUDE_CODE_EFFORT_LEVEL=None,
                SCION_MODEL=None, ANTHROPIC_MODEL=None,
            ):
                provision.provision(ctx)
            with open(os.path.join(tmp, ".scion", "harness", "outputs", "env.json"), encoding="utf-8") as f:
                return json.load(f)

    def test_level_reaches_env_json(self) -> None:
        self.assertEqual(self._env_json("80").get("CLAUDE_CODE_EFFORT_LEVEL"), "xhigh")

    def test_no_level_leaves_env_json_without_effort(self) -> None:
        env = self._env_json(None)
        self.assertNotIn("CLAUDE_CODE_EFFORT_LEVEL", env)
        self.assertIn("ANTHROPIC_MODEL", env)  # sanity: the overlay was written


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
