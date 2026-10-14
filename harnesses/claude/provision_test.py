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

import hashlib
import importlib.util
import json
import os
import shutil
import stat
import subprocess
import tempfile
import unittest
from contextlib import contextmanager
from typing import Any
from unittest import mock

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
        # Never fall back to the real /workspace (ptone/scion#2993).
        "agent_workspace": os.path.join(home, "workspace"),
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
                "agent_workspace": os.path.join(tmp, "workspace"),
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
                "agent_workspace": os.path.join(tmp, "workspace"),
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
                "agent_workspace": os.path.join(tmp, "workspace"),
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

    def test_preset_empty_env_is_not_clobbered(self) -> None:
        """An empty pin still blocks the overlay in MergeEnvOverlay (presence wins)."""
        env, effort, warnings = self._effort("90", preset="")
        self.assertIsNone(effort)
        self.assertNotIn("CLAUDE_CODE_EFFORT_LEVEL", env)
        self.assertEqual(len(warnings), 1)
        self.assertIn("CLAUDE_CODE_EFFORT_LEVEL=''", warnings[0])

    def test_max_value_is_written_without_warning(self) -> None:
        """A customized table may name `max`; the env var accepts it."""
        thinking = {"levels": [{"max": 100, "value": "max"}]}
        env, effort, warnings = self._effort("60", thinking=thinking)
        self.assertEqual(env, {"CLAUDE_CODE_EFFORT_LEVEL": "max"})
        self.assertEqual(effort, "max")
        self.assertEqual(warnings, [])

    def test_undocumented_value_is_written_with_warning(self) -> None:
        thinking = {"levels": [{"max": 100, "value": "turbo"}]}
        env, effort, warnings = self._effort("60", thinking=thinking)
        self.assertEqual(env, {"CLAUDE_CODE_EFFORT_LEVEL": "turbo"})
        self.assertEqual(effort, "turbo")
        self.assertEqual(len(warnings), 1)
        self.assertIn("'turbo'", warnings[0])


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


def _select_with_candidates(home: str, candidates: dict[str, Any]):
    bundle = os.path.join(home, ".scion", "harness")
    os.makedirs(os.path.join(bundle, "inputs"), exist_ok=True)
    with open(os.path.join(bundle, "inputs", "auth-candidates.json"), "w", encoding="utf-8") as f:
        json.dump(candidates, f)
    ctx = make_ctx(home)
    ctx.info = lambda *a, **k: None
    return ctx, ctx.select_auth(provision.AUTH)


class VertexAuthSelectionTest(unittest.TestCase):
    # config.yaml's vertex-ai region group accepts CLOUD_ML_REGION; the
    # provisioner must accept it too (ptone/scion#4623).
    def test_explicit_vertex_ai_accepts_cloud_ml_region(self) -> None:
        with tempfile.TemporaryDirectory() as tmp, temporary_home(tmp):
            ctx, resolved = _select_with_candidates(tmp, {
                "explicit_type": "vertex-ai",
                "env_vars": ["GOOGLE_CLOUD_PROJECT", "CLOUD_ML_REGION"],
            })
            self.assertEqual(resolved.method, "vertex-ai")
            self.assertEqual(resolved.env_key, "CLOUD_ML_REGION")
            env = provision._build_env_overlay(ctx, resolved)
        self.assertEqual(env["CLOUD_ML_REGION"], "${CLOUD_ML_REGION}")
        self.assertEqual(env["CLAUDE_CODE_USE_VERTEX"], "1")

    def test_location_wins_over_region_placeholder_fallback(self) -> None:
        # Both keys present, no staged secrets: the LOCATION key is chosen and
        # the ${VAR} placeholder fallback is kept (ptone/scion#4719). The
        # process env is cleared: the conflict check reads it for these keys.
        with tempfile.TemporaryDirectory() as tmp, temporary_home(tmp), env_vars(
            GOOGLE_CLOUD_LOCATION=None, GOOGLE_CLOUD_REGION=None,
        ):
            ctx, resolved = _select_with_candidates(tmp, {
                "explicit_type": "vertex-ai",
                "env_vars": ["GOOGLE_CLOUD_PROJECT", "GOOGLE_CLOUD_REGION", "GOOGLE_CLOUD_LOCATION"],
            })
            warnings: list[str] = []
            ctx.warn = warnings.append
            env = provision._build_env_overlay(ctx, resolved)
        self.assertEqual(resolved.env_key, "GOOGLE_CLOUD_LOCATION")
        self.assertEqual(env["CLOUD_ML_REGION"], "${GOOGLE_CLOUD_LOCATION}")
        self.assertEqual(env["ANTHROPIC_VERTEX_PROJECT_ID"], "${GOOGLE_CLOUD_PROJECT}")
        self.assertEqual(warnings, [])

    def test_location_and_region_differ_location_wins_with_warning(self) -> None:
        with tempfile.TemporaryDirectory() as tmp, temporary_home(tmp):
            secrets = os.path.join(tmp, "secrets")
            os.makedirs(secrets)
            paths = {}
            for key, value in (
                ("GOOGLE_CLOUD_PROJECT", "proj-1"),
                ("GOOGLE_CLOUD_REGION", "us-central1"),
                ("GOOGLE_CLOUD_LOCATION", "us-east5"),
            ):
                paths[key] = os.path.join(secrets, key)
                with open(paths[key], "w", encoding="utf-8") as f:
                    f.write(value)
            ctx, resolved = _select_with_candidates(tmp, {
                "explicit_type": "vertex-ai",
                "env_vars": list(paths),
                "env_secret_files": paths,
            })
            warnings: list[str] = []
            ctx.warn = warnings.append
            env = provision._build_env_overlay(ctx, resolved)
        self.assertEqual(resolved.env_key, "GOOGLE_CLOUD_LOCATION")
        self.assertEqual(env["CLOUD_ML_REGION"], "us-east5")
        self.assertEqual(len(warnings), 1)
        self.assertIn("GOOGLE_CLOUD_REGION", warnings[0])

    def _env_only_overlay(self, location: str | None, region: str | None):
        # Both keys are candidates with no staged secrets, so Claude reads the
        # env values at runtime through the ${VAR} placeholder.
        with tempfile.TemporaryDirectory() as tmp, temporary_home(tmp), env_vars(
            GOOGLE_CLOUD_LOCATION=location, GOOGLE_CLOUD_REGION=region,
        ):
            ctx, resolved = _select_with_candidates(tmp, {
                "explicit_type": "vertex-ai",
                "env_vars": ["GOOGLE_CLOUD_PROJECT", "GOOGLE_CLOUD_REGION", "GOOGLE_CLOUD_LOCATION"],
            })
            warnings: list[str] = []
            ctx.warn = warnings.append
            env = provision._build_env_overlay(ctx, resolved)
        return env, warnings

    def test_env_only_location_and_region_differ_warns(self) -> None:
        # The conflict check reads the env, as Claude does (ptone/scion#4719).
        env, warnings = self._env_only_overlay("us-east5", "us-central1")
        self.assertEqual(env["CLOUD_ML_REGION"], "${GOOGLE_CLOUD_LOCATION}")
        self.assertEqual(len(warnings), 1, warnings)
        self.assertIn("GOOGLE_CLOUD_REGION", warnings[0])

    def test_env_only_location_and_region_equal_no_warning(self) -> None:
        _, warnings = self._env_only_overlay("us-east5", "us-east5")
        self.assertEqual(warnings, [])

    def test_env_only_single_key_no_warning(self) -> None:
        for location, region in (("us-east5", None), (None, "us-central1"), ("us-east5", "")):
            with self.subTest(location=location, region=region):
                _, warnings = self._env_only_overlay(location, region)
                self.assertEqual(warnings, [])

    def test_secret_file_only_keys_differ_warns(self) -> None:
        # Keys staged only as secret files (not in env_vars), e.g. recorded
        # secrets merged on restart: selection and _resolve read the files,
        # so the conflict check must too. Non-candidate env is not read.
        with tempfile.TemporaryDirectory() as tmp, temporary_home(tmp), env_vars(
            GOOGLE_CLOUD_LOCATION="us-east5", GOOGLE_CLOUD_REGION="us-east5",
        ):
            secrets = os.path.join(tmp, "secrets")
            os.makedirs(secrets)
            paths = {}
            for key, value in (
                ("GOOGLE_CLOUD_PROJECT", "proj-1"),
                ("GOOGLE_CLOUD_REGION", "us-central1"),
                ("GOOGLE_CLOUD_LOCATION", "europe-west4"),
            ):
                paths[key] = os.path.join(secrets, key)
                with open(paths[key], "w", encoding="utf-8") as f:
                    f.write(value)
            ctx, resolved = _select_with_candidates(tmp, {
                "explicit_type": "vertex-ai",
                "env_vars": [],
                "env_secret_files": paths,
            })
            warnings: list[str] = []
            ctx.warn = warnings.append
            env = provision._build_env_overlay(ctx, resolved)
        self.assertEqual(resolved.env_key, "GOOGLE_CLOUD_LOCATION")
        self.assertEqual(env["CLOUD_ML_REGION"], "europe-west4")
        self.assertEqual(len(warnings), 1, warnings)
        self.assertIn("'europe-west4'", warnings[0])
        self.assertIn("'us-central1'", warnings[0])

    def test_non_candidate_env_not_read(self) -> None:
        # A stale process-env value for a key that is neither a candidate nor
        # a staged secret is not part of resolution and must not warn.
        with tempfile.TemporaryDirectory() as tmp, temporary_home(tmp), env_vars(
            GOOGLE_CLOUD_LOCATION="us-east5", GOOGLE_CLOUD_REGION="us-central1",
        ):
            ctx, resolved = _select_with_candidates(tmp, {
                "explicit_type": "vertex-ai",
                "env_vars": ["GOOGLE_CLOUD_PROJECT", "GOOGLE_CLOUD_REGION"],
            })
            warnings: list[str] = []
            ctx.warn = warnings.append
            env = provision._build_env_overlay(ctx, resolved)
        self.assertEqual(env["CLOUD_ML_REGION"], "${GOOGLE_CLOUD_REGION}")
        self.assertEqual(warnings, [])

    def test_explicit_vertex_ai_without_region_is_rejected(self) -> None:
        with tempfile.TemporaryDirectory() as tmp, temporary_home(tmp):
            with self.assertRaises(scion_harness.ProvisionError):
                _select_with_candidates(tmp, {
                    "explicit_type": "vertex-ai",
                    "env_vars": ["GOOGLE_CLOUD_PROJECT"],
                })


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


# --- Agent Plugins -----------------------------------------------------------

R = "${CLAUDE_PLUGIN_ROOT}"
WRAP = ["-c", 'cd -- "$1" || exit 127; shift; exec "$@"', "sh"]

# Translation table (design 3.2), row by row: input mcp.json server ->
# Claude mcpServers entry. D is substituted with the plugin's data dir.
GOLDEN_SERVERS_IN: dict[str, dict[str, Any]] = {
    "bare": {"type": "stdio", "command": "npx", "args": ["-y", "srv"]},
    "relative": {"type": "stdio", "command": "./bin/x"},
    "cwd-rel": {"type": "stdio", "command": "./bin/x", "cwd": "./d"},
    "cwd-dot": {"type": "stdio", "command": "./bin/x", "cwd": "./"},
    "cwd-root": {"type": "stdio", "command": "./bin/x", "cwd": "${PLUGIN_ROOT}/sub"},
    "cwd-data": {"type": "stdio", "command": "./bin/x", "cwd": "${PLUGIN_DATA}/cache"},
    "cwd-data-exact": {"type": "stdio", "command": "./bin/x", "cwd": "${PLUGIN_DATA}"},
    "expand": {
        "type": "stdio", "command": "./bin/x",
        "args": ["--root", "${PLUGIN_ROOT}/cfg.json", "--data=${PLUGIN_DATA}/x", "$1", "$HOME", "lit"],
        "env": {"CFG": "${PLUGIN_ROOT}/c", "DIR": "${PLUGIN_DATA}", "PLAIN": "v"},
    },
    "http": {"type": "streamable-http", "url": "https://x.example/mcp", "headers": {"X-Tenant": "t"}},
    "http-plain": {"type": "streamable-http", "url": "http://127.0.0.1:8765/mcp"},
    "events": {"type": "sse", "url": "https://x.example/sse"},
}


def golden_servers_out(data: str) -> dict[str, dict[str, Any]]:
    def stdio(cwd: str, *rest: str, env: dict[str, str] | None = None) -> dict[str, Any]:
        e = dict(env or {})
        e["PLUGIN_ROOT"] = R
        e["PLUGIN_DATA"] = data
        return {"type": "stdio", "command": "/bin/sh", "args": [*WRAP, cwd, *rest], "env": e}

    return {
        "bare": stdio(R, "npx", "-y", "srv"),
        "relative": stdio(R, R + "/bin/x"),
        "cwd-rel": stdio(R + "/d", R + "/bin/x"),
        "cwd-dot": stdio(R, R + "/bin/x"),
        "cwd-root": stdio(R + "/sub", R + "/bin/x"),
        "cwd-data": stdio(data + "/cache", R + "/bin/x"),
        "cwd-data-exact": stdio(data, R + "/bin/x"),
        "expand": stdio(
            R, R + "/bin/x", "--root", R + "/cfg.json", "--data=" + data + "/x", "$1", "$HOME", "lit",
            env={"CFG": R + "/c", "DIR": data, "PLAIN": "v"},
        ),
        "http": {"type": "http", "url": "https://x.example/mcp", "headers": {"X-Tenant": "t"}},
        "http-plain": {"type": "http", "url": "http://127.0.0.1:8765/mcp"},
        "events": {"type": "sse", "url": "https://x.example/sse"},
    }


def _plugin(name: str = "acme.tools", servers: dict[str, Any] | None = None, **raw: Any):
    entry = {"name": name, "version": "1.2.0", "description": "Acme", **raw}
    return scion_harness.AgentPlugin(
        name=name, version="1.2.0", digest="d", staged_dir="/nonexistent", skills=[],
        skills_dir_conflict=bool(raw.get("skills_dir_conflict")), mcp_servers=servers, raw=entry,
    )


class _Warnings:
    def __init__(self) -> None:
        self.messages: list[str] = []

    def __call__(self, msg: str) -> None:
        self.messages.append(msg)


class ClaudeMcpTranslationTest(unittest.TestCase):
    def setUp(self) -> None:
        self.ctx = make_ctx("/home/x")
        self.warnings = _Warnings()
        self.ctx.warn = self.warnings  # type: ignore[method-assign]
        self.data = "/home/x/.scion/agent-plugins/data/acme.tools"

    def test_golden_translation(self) -> None:
        got = provision.translate_mcp_servers(self.ctx, _plugin(servers=GOLDEN_SERVERS_IN), self.data)
        self.assertEqual(got, golden_servers_out(self.data))
        self.assertEqual(self.warnings.messages, [])

    def test_wrapper_argv_layout(self) -> None:
        out = provision.translate_mcp_server(
            {"type": "stdio", "command": "./bin/x", "args": ["a", "b c"], "cwd": "./w"}, self.data)
        self.assertEqual(out["command"], "/bin/sh")
        # -c SCRIPT $0 $1=cwd $2=command $3..=args: each a separate element,
        # never joined into the script text.
        self.assertEqual(out["args"], ["-c", provision.STDIO_CWD_WRAPPER, "sh", R + "/w", R + "/bin/x", "a", "b c"])
        self.assertNotIn("b c", out["args"][1])

    def test_wrapper_runs_in_cwd_with_positional_args(self) -> None:
        # Executes the real wrapper with /bin/sh, as Claude would after
        # expanding ${CLAUDE_PLUGIN_ROOT}: cwd is applied and plugin strings
        # reach the program unparsed.
        with tempfile.TemporaryDirectory() as root:
            out = provision.translate_mcp_server(
                {"type": "stdio", "command": "printf", "args": ["%s|", "$1", "a b", "`id`", "${PLUGIN_ROOT}"]},
                self.data)
            args = [a.replace(R, root) for a in out["args"]]
            proc = subprocess.run([out["command"], *args], capture_output=True, text=True, cwd="/")
            self.assertEqual(proc.returncode, 0, proc.stderr)
            self.assertEqual(proc.stdout, f"$1|a b|`id`|{root}|")
            pwd = subprocess.run([out["command"], *[a.replace(R, root) for a in
                                  provision.translate_mcp_server({"type": "stdio", "command": "pwd"}, self.data)["args"]]],
                                 capture_output=True, text=True, cwd="/")
            self.assertEqual(os.path.realpath(pwd.stdout.strip()), os.path.realpath(root))
            bad = subprocess.run([out["command"], *[a.replace(R, os.path.join(root, "missing")) for a in out["args"]]],
                                 capture_output=True, text=True, cwd="/")
            self.assertEqual(bad.returncode, 127)

    def test_expansion_is_single_pass(self) -> None:
        # A data dir whose own text contains a placeholder is not expanded
        # again, and ${PLUGIN_ROOT} becomes Claude's root variable once.
        data = "/d/${PLUGIN_ROOT}"
        out = provision.translate_mcp_server(
            {"type": "stdio", "command": "x", "args": ["${PLUGIN_DATA}/y", "${PLUGIN_ROOT}${PLUGIN_DATA}"],
             "env": {"A": "${PLUGIN_DATA}"}}, data)
        self.assertEqual(out["args"][-2:], ["/d/${PLUGIN_ROOT}/y", R + "/d/${PLUGIN_ROOT}"])
        self.assertEqual(out["env"]["A"], "/d/${PLUGIN_ROOT}")
        self.assertEqual(provision._expand_plugin_placeholders("${PLUGIN_${PLUGIN_ROOT}}", "${PLUGIN_DATA}", "D"),
                         "${PLUGIN_${PLUGIN_DATA}}")

    def test_reserved_env_set_last(self) -> None:
        # Go rejects PLUGIN_ROOT/PLUGIN_DATA in env; even if one slipped
        # through, the reserved values win (spec 9.1).
        out = provision.translate_mcp_server(
            {"type": "stdio", "command": "x", "env": {"PLUGIN_DATA": "/evil", "Z": "1"}}, self.data)
        self.assertEqual(out["env"]["PLUGIN_DATA"], self.data)
        self.assertEqual(list(out["env"])[-2:], ["PLUGIN_ROOT", "PLUGIN_DATA"])

    def test_skip_rule(self) -> None:
        servers = {
            "home-arg": {"type": "stdio", "command": "x", "args": ["${HOME}/y"]},
            "claude-root": {"type": "stdio", "command": "x", "args": ["${CLAUDE_PLUGIN_ROOT}"]},
            "default": {"type": "stdio", "command": "x", "env": {"A": "${A:-1}"}},
            "partial": {"type": "stdio", "command": "x", "args": ["${PLUGIN_ROOT"]},
            "nested": {"type": "stdio", "command": "x", "args": ["${${PLUGIN_ROOT}}"]},
            "cwd": {"type": "stdio", "command": "x", "cwd": "./${HOME}"},
            "command": {"type": "stdio", "command": "${PLUGIN_ROOT}"},
            "url": {"type": "streamable-http", "url": "https://x.example/${PLUGIN_ROOT}"},
            "header": {"type": "sse", "url": "https://x.example/sse", "headers": {"A": "${TOKEN}"}},
            "ok": {"type": "stdio", "command": "x", "args": ["$HOME", "${PLUGIN_ROOT}", "$", "{x}"]},
        }
        got = provision.translate_mcp_servers(self.ctx, _plugin(servers=servers), self.data)
        self.assertEqual(sorted(got), ["ok"])
        skipped = sorted(n for n in servers if n != "ok")
        self.assertEqual(self.warnings.messages, [
            f"agent plugin acme.tools: mcp server {n} skipped for claude: "
            "value contains placeholder-like text that Claude would expand"
            for n in skipped
        ])

    def test_http_and_sse_mapping(self) -> None:
        self.assertEqual(provision.translate_mcp_server({"type": "streamable-http", "url": "https://a/m"}, self.data),
                         {"type": "http", "url": "https://a/m"})
        self.assertEqual(provision.translate_mcp_server(
            {"type": "sse", "url": "https://a/s", "headers": {"H": "v"}}, self.data),
            {"type": "sse", "url": "https://a/s", "headers": {"H": "v"}})

    def test_unknown_type_skipped(self) -> None:
        got = provision.translate_mcp_servers(self.ctx, _plugin(servers={"w": {"type": "websocket"}}), self.data)
        self.assertEqual(got, {})
        self.assertEqual(self.warnings.messages,
                         ["agent plugin acme.tools: mcp server w skipped for claude: unsupported type 'websocket'"])

    def test_manifest(self) -> None:
        m = provision.build_claude_manifest(self.ctx, _plugin(servers={"h": {"type": "sse", "url": "https://a/s"}}),
                                            self.data)
        self.assertEqual(m, {"name": "acme.tools", "version": "1.2.0", "description": "Acme",
                             "mcpServers": {"h": {"type": "sse", "url": "https://a/s"}}})
        bare = scion_harness.AgentPlugin("p", "", "d", "/x", [], False, None, {"name": "p"})
        self.assertEqual(provision.build_claude_manifest(self.ctx, bare, self.data), {"name": "p"})


class _NoCLI:
    """run_cli stand-in: claude is never run for plugins except --version."""

    def __init__(self, version: str = "2.1.296 (Claude Code)") -> None:
        self.version = version
        self.calls: list[list[str]] = []

    def __call__(self, argv, *, timeout=None, env=None):
        self.calls.append(list(argv))
        if argv[1:] == ["--version"]:
            return subprocess.CompletedProcess(argv, 0, self.version + "\n", "")
        return subprocess.CompletedProcess(argv, 2, "", "unexpected")


class _ClaudePluginsBase(unittest.TestCase):
    def setUp(self) -> None:
        tmp = tempfile.TemporaryDirectory(prefix="claude-plugins-test-")
        self.addCleanup(tmp.cleanup)
        self.home = tmp.name
        self.inputs = os.path.join(self.home, ".scion", "harness", "inputs")
        os.makedirs(self.inputs)
        patcher = mock.patch.dict(os.environ, {"HOME": self.home})
        patcher.start()
        self.addCleanup(patcher.stop)
        self.cli = _NoCLI()
        p = mock.patch.object(scion_harness, "run_cli", self.cli)
        p.start()
        self.addCleanup(p.stop)
        self.skills = os.path.join(self.home, ".claude", "skills")
        self.root = os.path.join(self.home, ".scion", "agent-plugins")

    def stage(self, plugins: dict[str, dict[str, Any]]) -> None:
        """plugins: name -> {digest, skill (SKILL.md text), conflict, servers, invalid}."""
        shutil.rmtree(os.path.join(self.inputs, "plugins"), ignore_errors=True)
        entries = []
        for name, spec in plugins.items():
            d = os.path.join(self.inputs, "plugins", name)
            os.makedirs(os.path.join(d, "skills", "hello"))
            os.makedirs(os.path.join(d, "bin"))
            with open(os.path.join(d, "plugin.json"), "w") as f:
                json.dump({"$schema": "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json", "name": name,
                           "version": "0.1.0"}, f)
            with open(os.path.join(d, "skills", "hello", "SKILL.md"), "w") as f:
                f.write(spec.get("skill", "PLUM-7"))
            with open(os.path.join(d, "bin", "srv"), "w") as f:
                f.write("#!/bin/sh\n")
            os.chmod(os.path.join(d, "bin", "srv"), 0o755)
            mcp = None
            if "servers" in spec or "invalid" in spec:
                mcp = {"servers": spec.get("servers", {}), "invalid": spec.get("invalid", [])}
            entries.append({"name": name, "version": "0.1.0", "description": "Smoke", "dir": f"plugins/{name}",
                            "digest": spec.get("digest", "d1"), "skills": ["hello"],
                            "skills_dir_conflict": bool(spec.get("conflict")), "mcp": mcp})
        with open(os.path.join(self.inputs, "plugins.json"), "w") as f:
            json.dump({"schema_version": 1, "spec_version": "1.0.0", "plugins": entries}, f)

    def ctx(self, warnings: list[str] | None = None):
        ctx = make_ctx(self.home)
        ctx.harness_config["skills_dir"] = ".claude/skills"
        if warnings is not None:
            ctx.warn = warnings.append  # type: ignore[method-assign]
        return ctx

    def reconcile(self, warnings: list[str] | None = None, claude_version: str | None = None) -> None:
        """What provision() does last."""
        ctx = self.ctx(warnings)
        inst = provision.ClaudePluginInstaller(ctx, claude_version=claude_version)
        inst.release_stale()
        scion_harness.reconcile_agent_plugins(ctx, inst)

    def state(self) -> dict[str, Any]:
        path = os.path.join(self.root, "state-claude.json")
        if not os.path.exists(path):
            return {}
        with open(path) as f:
            return json.load(f)["plugins"]

    def status(self) -> dict[str, Any]:
        with open(os.path.join(self.root, "status-claude.json")) as f:
            return json.load(f)

    def read(self, *parts: str) -> str:
        with open(os.path.join(self.skills, *parts)) as f:
            return f.read()


class ClaudeAgentPluginsTest(_ClaudePluginsBase):
    def test_install_writes_plugin_dir(self) -> None:
        self.stage({"scion-smoke": {"servers": {"s": {"type": "stdio", "command": "./bin/srv"}}}})
        warnings: list[str] = []
        self.reconcile(warnings)
        self.assertEqual(warnings, [])
        self.assertEqual(self.cli.calls, [["claude", "--version"]])
        target = os.path.join(self.skills, "scion-smoke")
        # Bundle files unchanged, generated manifest beside them.
        self.assertEqual(self.read("scion-smoke", "skills", "hello", "SKILL.md"), "PLUM-7")
        self.assertTrue(os.path.isfile(os.path.join(target, "plugin.json")))
        self.assertTrue(os.access(os.path.join(target, "bin", "srv"), os.X_OK))
        manifest = json.loads(self.read("scion-smoke", ".claude-plugin", "plugin.json"))
        data = os.path.join(self.root, "data", "scion-smoke")
        self.assertEqual(manifest, {
            "name": "scion-smoke", "version": "0.1.0", "description": "Smoke",
            "mcpServers": {"s": {"type": "stdio", "command": "/bin/sh",
                                 "args": [*WRAP, R, R + "/bin/srv"],
                                 "env": {"PLUGIN_ROOT": R, "PLUGIN_DATA": data}}},
        })
        # native_ref records every file Scion wrote, with its sha256, and
        # every dir: the basis of "Scion removes only what Scion wrote".
        ref = self.state()["scion-smoke"]["native_ref"]
        self.assertEqual(ref["path"], target)
        self.assertEqual(sorted(ref["files"]), [
            ".claude-plugin/plugin.json", "bin/srv", "plugin.json", "skills/hello/SKILL.md"])
        for rel, sha in ref["files"].items():
            with open(os.path.join(target, rel), "rb") as f:
                self.assertEqual(hashlib.sha256(f.read()).hexdigest(), sha, rel)
        self.assertEqual(sorted(ref["dirs"]), [".claude-plugin", "bin", "skills", "skills/hello"])
        self.assertEqual(self.status()["installed"], ["scion-smoke"])
        self.assertEqual([e for e in os.listdir(self.skills) if e.startswith(".")], [], "no tmp/old left behind")

        # Steady state: no CLI call at all.
        self.cli.calls.clear()
        self.reconcile()
        self.assertEqual(self.cli.calls, [])

    def test_data_dir_created_0700(self) -> None:
        self.stage({"p": {}})
        self.reconcile()
        data = os.path.join(self.root, "data", "p")
        self.assertEqual(stat.S_IMODE(os.stat(data).st_mode), 0o700)
        # Kept on removal (spec 9.1 makes deletion optional).
        with open(os.path.join(data, "keep"), "w") as f:
            f.write("x")
        self.stage({})
        self.reconcile()
        self.assertFalse(os.path.exists(os.path.join(self.skills, "p")))
        self.assertTrue(os.path.isfile(os.path.join(data, "keep")))

    def test_reuses_provision_claude_version(self) -> None:
        self.stage({"p": {}})
        self.reconcile(claude_version="2.1.296")
        self.assertEqual(self.cli.calls, [])
        self.assertIn("p", self.state())

    def test_old_claude_skips(self) -> None:
        self.stage({"p": {}})
        warnings: list[str] = []
        self.reconcile(warnings, claude_version="2.1.100")
        self.assertIn("claude 2.1.100 is older than 2.1.157", warnings[0])
        self.assertFalse(os.path.exists(os.path.join(self.skills, "p")))

    def test_conflict_skip(self) -> None:
        os.makedirs(os.path.join(self.skills, "p"))
        with open(os.path.join(self.skills, "p", "SKILL.md"), "w") as f:
            f.write("template skill")
        self.stage({"p": {"conflict": True}})
        warnings: list[str] = []
        self.reconcile(warnings)
        self.assertEqual(warnings, [
            "agent plugin p: install failed (plugin p not installed for claude: a skill named p is already "
            "installed in the skills dir); will retry next start",
        ])
        self.assertEqual(os.listdir(os.path.join(self.skills, "p")), ["SKILL.md"])
        self.assertNotIn("p", self.state())
        self.assertEqual(self.status()["installed"], [])
        self.assertEqual([s["name"] for s in self.status()["skipped"]], ["p"])

    def test_unowned_dir_not_touched(self) -> None:
        # A same-named dir Scion did not install (not in state), even one
        # that looks like a plugin, is never replaced.
        user = os.path.join(self.skills, "p", ".claude-plugin")
        os.makedirs(user)
        with open(os.path.join(user, "plugin.json"), "w") as f:
            f.write('{"name": "p"}')
        self.stage({"p": {}})
        warnings: list[str] = []
        self.reconcile(warnings)
        self.assertEqual(len(warnings), 1, warnings)
        self.assertIn("exists and was not installed by Scion", warnings[0])
        self.assertEqual(self.read("p", ".claude-plugin", "plugin.json"), '{"name": "p"}')
        self.assertFalse(os.path.exists(os.path.join(self.skills, "p", "plugin.json")))
        self.assertNotIn("p", self.state())

    def test_digest_change_atomic_swap(self) -> None:
        self.stage({"p": {"digest": "d1", "skill": "PLUM-7"}})
        self.reconcile()
        self.stage({"p": {"digest": "d2", "skill": "PLUM-8"}})
        warnings: list[str] = []
        self.reconcile(warnings)
        self.assertEqual(warnings, [])
        self.assertEqual(self.read("p", "skills", "hello", "SKILL.md"), "PLUM-8")
        self.assertEqual(self.state()["p"]["digest"], "d2")
        self.assertEqual([e for e in os.listdir(self.skills) if e.startswith(".")], [])

    def test_swap_replaces_owned_dir_when_uninstall_fails(self) -> None:
        # The previous version could not be removed (uninstall raised);
        # the install still swaps the owned dir atomically.
        self.stage({"p": {"digest": "d1", "skill": "PLUM-7"}})
        self.reconcile()
        self.stage({"p": {"digest": "d2", "skill": "PLUM-8"}})
        warnings: list[str] = []
        with mock.patch.object(provision.ClaudePluginInstaller, "uninstall",
                               side_effect=scion_harness.PluginInstallError("busy")):
            self.reconcile(warnings)
        self.assertEqual(self.read("p", "skills", "hello", "SKILL.md"), "PLUM-8")
        self.assertEqual(self.state()["p"]["digest"], "d2")

    def test_failed_swap_keeps_old_dir(self) -> None:
        self.stage({"p": {"digest": "d1", "skill": "PLUM-7"}})
        self.reconcile()
        self.stage({"p": {"digest": "d2", "skill": "PLUM-8"}})
        real_rename = os.rename
        target = os.path.join(self.skills, "p")

        def rename(src, dst):
            if dst == target and src.endswith(".p.tmp"):
                raise OSError("disk full")
            return real_rename(src, dst)

        warnings: list[str] = []
        with mock.patch.object(provision.ClaudePluginInstaller, "uninstall", return_value=None), \
                mock.patch.object(os, "rename", rename):
            self.reconcile(warnings)
        self.assertEqual(self.read("p", "skills", "hello", "SKILL.md"), "PLUM-7")
        self.assertTrue(any("install failed" in w and "disk full" in w for w in warnings), warnings)
        self.assertEqual([e for e in os.listdir(self.skills) if e.startswith(".")], [])

    def test_claude_specific_paths_not_installed(self) -> None:
        # Review round 1, finding 1: claude 2.1.296 loads these from a plugin
        # root by default (.mcp.json even next to manifest mcpServers), which
        # would bypass Go validation and grant hooks/agents/commands.
        self.stage({"p": {"servers": {"s": {"type": "stdio", "command": "./bin/srv"}}}})
        staged = os.path.join(self.inputs, "plugins", "p")
        files = {
            ".mcp.json": '{"mcpServers": {"evil": {"command": "/bin/true"}}}',
            ".lsp.json": "{}",
            "hooks/hooks.json": '{"hooks": {}}',
            "commands/c.md": "c",
            "agents/a.md": "a",
            "monitors/monitors.json": "[]",
            "output-styles/o.md": "o",
            "workflows/w.md": "w",
            "themes/t.json": "{}",
            "settings.json": "{}",
            "lib/keep.txt": "kept",
            "skills/hello/hooks/notes.md": "nested names are not plugin-root paths",
        }
        for rel, text in files.items():
            os.makedirs(os.path.dirname(os.path.join(staged, rel)), exist_ok=True)
            with open(os.path.join(staged, rel), "w") as f:
                f.write(text)
        warnings: list[str] = []
        self.reconcile(warnings)
        target = os.path.join(self.skills, "p")
        for rel in provision.CLAUDE_STRIPPED_PATHS:
            self.assertFalse(os.path.lexists(os.path.join(target, rel)), rel)
        self.assertEqual(self.read("p", "lib", "keep.txt"), "kept")
        self.assertTrue(os.path.isfile(os.path.join(target, "skills", "hello", "hooks", "notes.md")))
        self.assertTrue(os.path.isfile(os.path.join(target, "bin", "srv")))
        self.assertTrue(os.path.isfile(os.path.join(target, "plugin.json")))
        self.assertEqual(sorted(warnings), sorted(
            f"agent plugin p: ignored claude-specific {rel} in bundle" for rel in provision.CLAUDE_STRIPPED_PATHS))
        manifest = json.loads(self.read("p", ".claude-plugin", "plugin.json"))
        self.assertEqual(sorted(manifest["mcpServers"]), ["s"])

    def test_server_using_stripped_path_skipped(self) -> None:
        ctx = self.ctx()
        warnings = _Warnings()
        ctx.warn = warnings  # type: ignore[method-assign]
        servers = {
            "hook-cmd": {"type": "stdio", "command": "./hooks/run"},
            "agents-cwd": {"type": "stdio", "command": "x", "cwd": "./agents"},
            "root-cwd": {"type": "stdio", "command": "x", "cwd": "${PLUGIN_ROOT}/commands/sub"},
            "dot": {"type": "stdio", "command": "./.mcp.json"},
            "root-arg": {"type": "stdio", "command": "node", "args": ["${PLUGIN_ROOT}/hooks/x.js"]},
            "env-ref": {"type": "stdio", "command": "x", "env": {"CFG": "${PLUGIN_ROOT}/agents/c.json"}},
            "root-cwd-rel-arg": {"type": "stdio", "command": "node", "args": ["./hooks/x.js"]},
            "ok": {"type": "stdio", "command": "./bin/hooks", "cwd": "./lib/agents",
                   "args": ["./hooks/relative-to-cwd", "${PLUGIN_ROOT}/lib/hooks"],
                   "env": {"A": "./hooks", "B": "${PLUGIN_DATA}/hooks"}},
        }
        got = provision.translate_mcp_servers(ctx, _plugin(servers=servers), "/d")
        self.assertEqual(sorted(got), ["ok"])
        self.assertIn("agent plugin acme.tools: mcp server hook-cmd skipped for claude: "
                      "it uses hooks, which is not installed for claude", warnings.messages)
        self.assertIn("agent plugin acme.tools: mcp server root-arg skipped for claude: "
                      "it uses hooks, which is not installed for claude", warnings.messages)
        self.assertIn("agent plugin acme.tools: mcp server env-ref skipped for claude: "
                      "it uses agents, which is not installed for claude", warnings.messages)
        self.assertIn("agent plugin acme.tools: mcp server root-cwd-rel-arg skipped for claude: "
                      "it uses hooks, which is not installed for claude", warnings.messages)
        self.assertEqual(len(warnings.messages), 7)

    def test_old_copy_removal_failure_is_a_warning(self) -> None:
        # Review round 1, finding 2: nothing after the swap fails the
        # install, so the new dir is always recorded (owned).
        self.stage({"p": {"digest": "d1"}})
        self.reconcile()
        self.stage({"p": {"digest": "d2", "skill": "PLUM-8"}})
        real_remove = provision._remove_path

        def remove(path: str) -> None:
            if path.endswith(".p.old") and os.path.lexists(path):
                raise OSError("busy")
            real_remove(path)

        warnings: list[str] = []
        with mock.patch.object(provision.ClaudePluginInstaller, "uninstall", return_value=None), \
                mock.patch.object(provision, "_remove_path", remove):
            self.reconcile(warnings)
        self.assertEqual(self.read("p", "skills", "hello", "SKILL.md"), "PLUM-8")
        self.assertEqual(self.state()["p"]["digest"], "d2")
        self.assertEqual(len(warnings), 1, warnings)
        self.assertIn("cannot remove the previous copy", warnings[0])
        # The next install clears the leftover.
        self.stage({"p": {"digest": "d3", "skill": "PLUM-9"}})
        self.reconcile()
        self.assertEqual([e for e in os.listdir(self.skills) if e.startswith(".")], [])

    def test_data_dir_failure_leaves_nothing_in_place(self) -> None:
        self.stage({"p": {}})
        blocker = os.path.join(self.home, "blocker")
        with open(blocker, "w") as f:
            f.write("not a dir")
        warnings: list[str] = []
        with mock.patch.object(provision.ClaudePluginInstaller, "data_dir",
                               lambda _self, n: os.path.join(blocker, n)):
            self.reconcile(warnings)
        self.assertFalse(os.path.exists(os.path.join(self.skills, "p")))
        self.assertNotIn("p", self.state())
        self.assertEqual(len(warnings), 1, warnings)
        self.assertIn("agent plugin p: install failed (cannot write", warnings[0])

    def test_digest_change_after_manual_manifest_removal(self) -> None:
        # Review round 1, finding 3: same outcome as without a digest change.
        self.stage({"p": {"digest": "d1"}})
        self.reconcile()
        shutil.rmtree(os.path.join(self.skills, "p", ".claude-plugin"))
        self.stage({"p": {"digest": "d2", "skill": "PLUM-8"}})
        warnings: list[str] = []
        self.reconcile(warnings)
        self.assertEqual(warnings, [])
        self.assertEqual(self.read("p", "skills", "hello", "SKILL.md"), "PLUM-8")
        self.assertTrue(os.path.isfile(os.path.join(self.skills, "p", ".claude-plugin", "plugin.json")))
        self.assertEqual(self.state()["p"]["digest"], "d2")

    def test_missing_data_dir_reinstalls(self) -> None:
        # Review round 1, FYI 6: PLUGIN_DATA must exist before launch.
        self.stage({"p": {}})
        self.reconcile()
        shutil.rmtree(os.path.join(self.root, "data", "p"))
        self.cli.calls.clear()
        self.reconcile()
        self.assertEqual(self.cli.calls, [["claude", "--version"]])
        self.assertTrue(os.path.isdir(os.path.join(self.root, "data", "p")))

    def test_leftovers_swept_on_fast_path(self) -> None:
        # Review round 2, finding 3.
        self.stage({"p": {}})
        self.reconcile()
        for leftover in (".p.old", ".p.tmp"):
            os.makedirs(os.path.join(self.skills, leftover, ".claude-plugin"))
        os.makedirs(os.path.join(self.skills, ".other.old"))  # not an owned name
        self.cli.calls.clear()
        self.reconcile()
        self.assertEqual(self.cli.calls, [])
        self.assertEqual(sorted(e for e in os.listdir(self.skills) if e.startswith(".")), [".other.old"])

    def test_manually_removed_is_reinstalled(self) -> None:
        self.stage({"p": {}})
        self.reconcile()
        shutil.rmtree(os.path.join(self.skills, "p", ".claude-plugin"))
        self.cli.calls.clear()
        self.reconcile()
        self.assertEqual(self.cli.calls, [["claude", "--version"]])
        self.assertTrue(os.path.isfile(os.path.join(self.skills, "p", ".claude-plugin", "plugin.json")))

    def test_reprovision_reset_home_config(self) -> None:
        # Regression for the codex lesson: a re-provision recopies the
        # harness-config home/ files. Claude registers a skills-dir plugin
        # by its directory only, so resetting the shipped ~/.claude.json and
        # ~/.claude/settings.json keeps it registered (fast path), while a
        # reset that drops the plugin dir makes the next start reinstall.
        self.stage({"p": {}})
        self.reconcile()
        shipped = os.path.join(os.path.dirname(os.path.abspath(provision.__file__)), "home")
        for rel in (".claude.json", os.path.join(".claude", "settings.json")):
            os.makedirs(os.path.dirname(os.path.join(self.home, rel)), exist_ok=True)
            shutil.copyfile(os.path.join(shipped, rel), os.path.join(self.home, rel))
        self.cli.calls.clear()
        self.reconcile()
        self.assertEqual(self.cli.calls, [])
        shutil.rmtree(self.skills)
        self.reconcile()
        self.assertEqual(self.cli.calls, [["claude", "--version"]])
        self.assertTrue(os.path.isfile(os.path.join(self.skills, "p", ".claude-plugin", "plugin.json")))

    def test_skills_dir_change_reinstalls(self) -> None:
        self.stage({"p": {}})
        self.reconcile()
        inst = provision.ClaudePluginInstaller(self.ctx())
        ref = self.state()["p"]["native_ref"]
        self.assertTrue(inst.is_present("p", ref))
        self.assertFalse(inst.is_present("p", {**ref, "path": os.path.join(self.home, "other", "p")}))

    def test_is_present_compares_generated_manifest_sha(self) -> None:
        self.stage({"p": {}})
        self.reconcile()
        inst = provision.ClaudePluginInstaller(self.ctx())
        ref = self.state()["p"]["native_ref"]
        with open(os.path.join(self.skills, "p", ".claude-plugin", "plugin.json"), "w") as f:
            f.write('{"name": "p", "template": true}')
        self.assertFalse(inst.is_present("p", ref))
        self.assertFalse(inst.is_present("p", {"path": ref["path"]}), "no recorded sha: not present")

    def test_invalid_servers_rewarned_and_placeholder_skipped(self) -> None:
        self.stage({"p": {
            "servers": {"ok": {"type": "sse", "url": "https://a/s"},
                        "env": {"type": "stdio", "command": "x", "args": ["${HOME}"]}},
            "invalid": [{"severity": "warning", "message": 'mcp server "old" skipped: bad url'}],
        }})
        warnings: list[str] = []
        self.reconcile(warnings)
        self.assertEqual(warnings, [
            'agent plugin p: mcp server "old" skipped: bad url',
            "agent plugin p: mcp server env skipped for claude: value contains placeholder-like text that Claude would expand",
        ])
        manifest = json.loads(self.read("p", ".claude-plugin", "plugin.json"))
        self.assertEqual(sorted(manifest["mcpServers"]), ["ok"])

    def test_bundle_claude_plugin_dir_replaced(self) -> None:
        self.stage({"p": {}})
        shipped = os.path.join(self.inputs, "plugins", "p", ".claude-plugin")
        os.makedirs(shipped)
        with open(os.path.join(shipped, "plugin.json"), "w") as f:
            f.write('{"name": "evil", "hooks": {}}')
        self.reconcile()
        self.assertEqual(json.loads(self.read("p", ".claude-plugin", "plugin.json"))["name"], "p")

    def test_provision_reconciles_last(self) -> None:
        # The universal MCP merge and the instructions write come first;
        # plugin reconcile runs last (design 2.1). The MCP merge is stubbed:
        # CLAUDE_MCP_MAPPING resolves ~/.claude.json at import time.
        self.stage({"p": {}})
        ctx = self.ctx()
        ctx.select_auth = lambda _: scion_harness.ResolvedAuth("none")  # type: ignore[method-assign]
        order: list[str] = []
        real_reconcile = scion_harness.reconcile_agent_plugins
        real_instructions = scion_harness.project_instructions

        def reconcile(*a: Any, **k: Any) -> None:
            order.append("plugins")
            real_reconcile(*a, **k)

        def instructions(*a: Any, **k: Any) -> Any:
            order.append("instructions")
            return real_instructions(*a, **k)

        with mock.patch.object(provision, "_detect_claude_version", return_value="2.1.296"), \
                mock.patch.object(scion_harness, "apply_mcp_servers_simple",
                                  side_effect=lambda *a, **k: order.append("mcp") or 0), \
                mock.patch.object(scion_harness, "project_instructions", side_effect=instructions), \
                mock.patch.object(scion_harness, "reconcile_agent_plugins", side_effect=reconcile), \
                env_vars(SCION_MODEL=None, ANTHROPIC_MODEL=None, SCION_THINKING_LEVEL=None):
            provision.provision(ctx)
        self.assertEqual(order, ["mcp", "instructions", "plugins"])
        self.assertTrue(os.path.isfile(os.path.join(self.skills, "p", ".claude-plugin", "plugin.json")))
        # provision() reuses its own `claude --version`.
        self.assertEqual(self.cli.calls, [])

    def test_parse_semver_alias(self) -> None:
        self.assertIs(provision._parse_semver, scion_harness.parse_semver)
        self.assertEqual(provision._parse_semver("2.1.270"), (2, 1, 270))
        self.assertEqual(provision._parse_semver("2.1.270-beta.1"), (2, 1, 270))
        self.assertIsNone(provision._parse_semver(""))
        self.assertIsNone(provision._parse_semver("2.1"))


class ClaudeOwnershipTest(_ClaudePluginsBase):
    """Review round 5: "Scion removes only what Scion wrote". Every
    ownership path (restart, update, drop, conflict, sweep) against content
    Scion did not write in an installed plugin dir. Go's util.CopyDir merge
    of a same-named template skill dir is simulated by writing its files
    into ~/.claude/skills/p/ (overwriting same-named files)."""

    GEN = ".claude-plugin/plugin.json"

    def install_p(self) -> None:
        self.stage({"p": {"digest": "d1"}})
        self.reconcile()
        self.assertIn("p", self.state())

    def put(self, rel: str, text: str) -> None:
        path = os.path.join(self.skills, "p", rel)
        os.makedirs(os.path.dirname(path), exist_ok=True)
        with open(path, "w") as f:
            f.write(text)

    # Foreign content kinds: rel path -> text that must survive.
    def foreign(self, kind: str) -> dict[str, str]:
        return {
            "template-skill": {"SKILL.md": "template skill"},
            "template-dir-no-skill-md": {"README.md": "template resources", "skills/tpl/SKILL.md": "tpl"},
            "template-claude-plugin": {self.GEN: '{"name": "p", "template": true}',
                                       "skills/native/SKILL.md": "native"},
            "hand-added": {"notes.txt": "mine"},
            "hand-edited": {"skills/hello/SKILL.md": "edited by hand"},
            "symlinked-skill": {},
        }[kind]

    def apply(self, kind: str) -> dict[str, str]:
        files = self.foreign(kind)
        for rel, text in files.items():
            self.put(rel, text)
        if kind == "symlinked-skill":
            outside = os.path.join(self.home, "elsewhere.md")
            with open(outside, "w") as f:
                f.write("linked")
            os.symlink(outside, os.path.join(self.skills, "p", "SKILL.md"))
        return files

    def assert_kept(self, kind: str, files: dict[str, str]) -> None:
        for rel, text in files.items():
            self.assertEqual(self.read("p", *rel.split("/")), text, f"{kind}: {rel}")
        if kind == "symlinked-skill":
            self.assertTrue(os.path.islink(os.path.join(self.skills, "p", "SKILL.md")))
            with open(os.path.join(self.home, "elsewhere.md")) as f:
                self.assertEqual(f.read(), "linked")

    def assert_scion_files_gone(self, kind: str, kept: dict[str, str]) -> None:
        # Every file Scion wrote whose content still matched is gone; only
        # the foreign content (and dirs holding it) remains.
        for rel in ("plugin.json", "bin/srv", "skills/hello/SKILL.md", self.GEN):
            if rel in kept:
                continue
            self.assertFalse(os.path.lexists(os.path.join(self.skills, "p", rel)), f"{kind}: {rel}")

    KINDS = ("template-skill", "template-dir-no-skill-md", "template-claude-plugin",
             "hand-added", "hand-edited", "symlinked-skill")

    def fresh(self) -> None:
        for d in (os.path.join(self.home, ".claude"), os.path.join(self.home, ".scion", "agent-plugins")):
            shutil.rmtree(d, ignore_errors=True)

    def run_path(self, path: str, warnings: list[str]) -> None:
        if path == "restart":
            self.reconcile(warnings)
        elif path == "update":
            self.stage({"p": {"digest": "d2", "skill": "PLUM-8"}})
            self.reconcile(warnings)
        elif path == "drop":
            self.stage({})
            self.reconcile(warnings)
        elif path == "conflict":
            self.stage({"p": {"digest": "d1", "conflict": True}})
            self.reconcile(warnings)
        else:
            raise AssertionError(path)

    def test_foreign_content_is_never_removed(self) -> None:
        for kind in self.KINDS:
            for path in ("restart", "update", "drop", "conflict"):
                with self.subTest(kind=kind, path=path):
                    self.fresh()
                    self.install_p()
                    files = self.apply(kind)
                    warnings: list[str] = []
                    self.run_path(path, warnings)
                    self.assert_kept(kind, files)
                    if kind == "hand-edited" and path == "restart":
                        # Nothing to do on a steady-state start: the edit is
                        # left alone and the plugin stays installed.
                        self.assertIn("p", self.state())
                        self.assertEqual(warnings, [])
                        continue
                    # Handed back: the record is gone, so Scion never
                    # touches the dir again, and only Scion's files went.
                    self.assertNotIn("p", self.state(), warnings)
                    self.assert_scion_files_gone(kind, files)
                    self.assertTrue(any("removed only the files Scion installed" in w
                                        or "which was left in place" in w for w in warnings), warnings)
                    # And a later start leaves everything as it is.
                    self.reconcile([])
                    self.assert_kept(kind, files)

    def test_handed_back_dir_is_not_owned_any_more(self) -> None:
        self.install_p()
        self.apply("template-skill")
        target = os.path.join(self.skills, "p")
        warnings: list[str] = []
        self.reconcile(warnings)
        not_owned = (f"agent plugin p: install failed (plugin p not installed for claude: {target} exists "
                     "and was not installed by Scion); will retry next start")
        self.assertEqual(warnings, [
            f"agent plugin p: {target} now also holds content Scion did not install (SKILL.md); "
            "removed only the files Scion installed (the rest was left in place)",
            not_owned,
        ])
        self.assertEqual(os.listdir(target), ["SKILL.md"])
        warnings.clear()
        self.reconcile(warnings)
        self.assertEqual(warnings, [not_owned], "the hand-back is not repeated")

    def test_conflict_without_foreign_content_removes_scion_files(self) -> None:
        # Go flags the conflict (for example a platform skill name), but the
        # dir holds only Scion's files: they all go, and so does the dir.
        self.install_p()
        warnings: list[str] = []
        self.stage({"p": {"conflict": True}})
        self.reconcile(warnings)
        self.assertFalse(os.path.exists(os.path.join(self.skills, "p")))
        self.assertEqual(warnings, [
            "agent plugin p: a skill named p now shares its directory; removed only the files Scion installed",
            "agent plugin p: install failed (plugin p not installed for claude: a skill named p is already "
            "installed in the skills dir); will retry next start",
        ])

    def test_bundle_root_skill_md_all_paths(self) -> None:
        # A dual-packaged bundle ships a root SKILL.md: it is never
        # installed, so the dir holds only Scion's files on every path.
        stripped = "agent plugin p: ignored root SKILL.md in bundle (Agent Plugins skills live under skills/)"
        target = os.path.join(self.skills, "p")

        def stage(digest: str, skill: str) -> None:
            self.stage({"p": {"digest": digest, "skill": skill}})
            with open(os.path.join(self.inputs, "plugins", "p", "SKILL.md"), "w") as f:
                f.write("root " + skill)

        stage("d1", "PLUM-7")
        warnings: list[str] = []
        self.reconcile(warnings)
        self.assertEqual(warnings, [stripped])
        self.assertFalse(os.path.lexists(os.path.join(target, "SKILL.md")))
        self.assertNotIn("SKILL.md", self.state()["p"]["native_ref"]["files"])
        # restart: fast path
        self.cli.calls.clear()
        warnings.clear()
        self.reconcile(warnings)
        self.assertEqual((self.cli.calls, warnings), ([], []))
        # same digest, data dir deleted: reinstalled
        shutil.rmtree(os.path.join(self.root, "data", "p"))
        self.reconcile(warnings)
        self.assertEqual(warnings, [stripped])
        # update
        stage("d2", "PLUM-8")
        warnings.clear()
        self.reconcile(warnings)
        self.assertEqual(warnings, [stripped])
        self.assertEqual(self.read("p", "skills", "hello", "SKILL.md"), "PLUM-8")
        self.assertEqual(self.state()["p"]["digest"], "d2")
        # drop: the dir goes entirely
        self.stage({})
        warnings.clear()
        self.reconcile(warnings)
        self.assertEqual(warnings, [])
        self.assertFalse(os.path.exists(target))
        self.assertEqual(self.status()["removed"], ["p"])

    def test_clean_paths_remove_everything(self) -> None:
        # No foreign content: drop removes the dir; a manifest deleted by
        # hand is reinstalled at the same digest and replaced on update.
        self.install_p()
        os.unlink(os.path.join(self.skills, "p", ".claude-plugin", "plugin.json"))
        warnings: list[str] = []
        self.reconcile(warnings)
        self.assertEqual(warnings, [])
        self.assertTrue(os.path.isfile(os.path.join(self.skills, "p", ".claude-plugin", "plugin.json")))
        os.unlink(os.path.join(self.skills, "p", ".claude-plugin", "plugin.json"))
        self.stage({"p": {"digest": "d2", "skill": "PLUM-8"}})
        self.reconcile(warnings)
        self.assertEqual(warnings, [])
        self.assertEqual(self.read("p", "skills", "hello", "SKILL.md"), "PLUM-8")
        self.stage({})
        self.reconcile(warnings)
        self.assertEqual(warnings, [])
        self.assertFalse(os.path.exists(os.path.join(self.skills, "p")))

    def test_failed_install_after_clean_uninstall_leaves_nothing(self) -> None:
        self.install_p()
        self.stage({"p": {"digest": "d2", "skill": "PLUM-8"}})
        real_copytree = shutil.copytree

        def copytree(src, dst, *a, **k):
            if str(dst).endswith(".p.tmp"):
                raise OSError("disk full")
            return real_copytree(src, dst, *a, **k)

        warnings: list[str] = []
        with mock.patch.object(provision.shutil, "copytree", copytree):
            self.reconcile(warnings)
        self.assertFalse(os.path.exists(os.path.join(self.skills, "p")))
        self.assertTrue(any("disk full" in w for w in warnings), warnings)
        warnings.clear()
        self.reconcile(warnings)
        self.assertEqual(warnings, [])
        self.assertEqual(self.read("p", "skills", "hello", "SKILL.md"), "PLUM-8")

    def test_uninstall_failure_judged_by_result(self) -> None:
        self.install_p()
        ref = self.state()["p"]["native_ref"]
        inst = provision.ClaudePluginInstaller(self.ctx())
        with mock.patch.object(os, "unlink", lambda *a, **k: None):
            with self.assertRaisesRegex(scion_harness.PluginInstallError, "still present"):
                inst.uninstall("p", ref)
        inst.uninstall("p", ref)
        self.assertFalse(os.path.exists(os.path.join(self.skills, "p")))
        inst.uninstall("never-installed", {})

    def test_install_guard_hands_back_unverified_owned_dir(self) -> None:
        # The last guard before the swap: an owned dir with foreign content
        # that release_stale did not see (simulated by skipping it).
        self.install_p()
        self.put("notes.txt", "mine")
        os.unlink(os.path.join(self.skills, "p", ".claude-plugin", "plugin.json"))
        ctx = self.ctx([])
        inst = provision.ClaudePluginInstaller(ctx, claude_version="2.1.296")
        [p] = scion_harness.read_agent_plugins(ctx)
        with self.assertRaisesRegex(scion_harness.PluginInstallError, "holds content Scion did not install"):
            inst.install(p, p.staged_dir)
        self.assertEqual(self.read("p", "notes.txt"), "mine")
        self.assertFalse(os.path.exists(os.path.join(self.skills, "p", "bin", "srv")))

    def test_legacy_record_without_files_is_handed_back(self) -> None:
        # A record that lists no files (written before this change) owns
        # nothing: the dir is handed back untouched.
        self.install_p()
        path = os.path.join(self.root, "state-claude.json")
        with open(path) as f:
            doc = json.load(f)
        doc["plugins"]["p"]["native_ref"] = {"path": os.path.join(self.skills, "p")}
        with open(path, "w") as f:
            json.dump(doc, f)
        self.stage({})
        warnings: list[str] = []
        self.reconcile(warnings)
        self.assertTrue(os.path.isfile(os.path.join(self.skills, "p", "bin", "srv")))
        self.assertNotIn("p", self.state())


if __name__ == "__main__":
    unittest.main()
