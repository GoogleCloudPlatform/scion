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

import importlib.util
import json
import os
import shlex
import subprocess
import tempfile
import unittest
import unittest.mock
from contextlib import contextmanager
from typing import Any

PROVISION_PATH = os.path.join(os.path.dirname(__file__), "provision.py")
SPEC = importlib.util.spec_from_file_location("antigravity_provision", PROVISION_PATH)
assert SPEC is not None
provision = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
SPEC.loader.exec_module(provision)

scion_harness = provision.scion_harness


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


# Mirrors harnesses/antigravity/config.yaml's model_aliases exactly (G4/am-dev
# scope rule: tests exercise the real pins, they do not restate or alter them).
ANTIGRAVITY_MODEL_ALIASES = {
    "small": "Gemini 3.1 Flash Lite",
    "medium": "Gemini 3.8 Flash (Medium)",
    "large": "Gemini 3.1 Pro (Low)",
    "extra-large": "Gemini 3.1 Pro (Low)",
}


def make_ctx(home: str, *, model: str | None = None) -> Any:
    harness_config: dict[str, Any] = {"model_aliases": dict(ANTIGRAVITY_MODEL_ALIASES)}
    if model is not None:
        harness_config["model"] = model
    manifest = {
        "harness_bundle_dir": os.path.join(home, ".scion", "harness"),
        "harness_config": harness_config,
        # Never fall back to the real /workspace (ptone/scion#2993).
        "agent_workspace": os.path.join(home, "workspace"),
    }
    return scion_harness.ProvisionContext("antigravity", manifest)


def _invoke(
    home: str,
    *,
    env_vars: list[str],
    explicit_type: str = "",
    harness_config: dict[str, Any] | None = None,
) -> dict:
    bundle = os.path.join(home, ".scion", "harness")
    os.makedirs(os.path.join(bundle, "inputs"), exist_ok=True)
    candidates = {"env_vars": env_vars}
    if explicit_type:
        candidates["explicit_type"] = explicit_type
    with open(os.path.join(bundle, "inputs", "auth-candidates.json"), "w", encoding="utf-8") as f:
        json.dump(candidates, f)

    # _generate_hooks_json (provision.py) writes .agents/hooks.json under
    # SCION_WORKSPACE_PATH (default "/workspace" -- the real repo checkout)
    # and chowns it. Without pinning this to a tempdir subdirectory, every
    # test run would write into the real workspace instead of its own
    # sandbox -- a live repo checkout, potentially shared, and (inside an
    # antigravity agent) the real hook wiring.
    ws = os.path.join(home, "workspace")
    os.makedirs(ws, exist_ok=True)

    manifest = {"harness_bundle_dir": bundle, "harness_config": harness_config or {}, "agent_workspace": ws}
    with temporary_home(home), unittest.mock.patch.dict(os.environ, {"SCION_WORKSPACE_PATH": ws}):
        ctx = scion_harness.ProvisionContext("antigravity", manifest)
        provision.provision(ctx)
        env_path = os.path.join(bundle, "outputs", "env.json")
        with open(env_path, "r", encoding="utf-8") as f:
            env = json.load(f)

    hooks_path = os.path.join(ws, ".agents", "hooks.json")
    assert os.path.isfile(hooks_path), f"expected {hooks_path} to exist (hooks.json wiring)"
    return env


class AntigravityProvisionTest(unittest.TestCase):
    # Antigravity has no native OTel integration
    # (config.yaml's capabilities.telemetry.native_emitter is "no"), so
    # every auth method must declare the hooks usage source: it is the only
    # one that exists for this harness. This is the D10 opt-in, made
    # allowed by the fixture-backed mapping in
    # pkg/sciontool/hooks/dialects/testdata/antigravity/ (design §9,
    # ptone/scion#2053 phase 3d). Antigravity's PostInvocation carries no
    # usage/token fields (see that fixture's README), so this is
    # calls-only.

    def test_api_key_auth_declares_hooks_usage_source(self) -> None:
        # Clear ambient GOOGLE_CLOUD_* vars: env_fallback=True on the
        # vertex-ai method means a real value leaking in from the test
        # runner's own environment would otherwise outrank api-key
        # (methods are tried in declaration order, vertex-ai first).
        with tempfile.TemporaryDirectory() as tmp:
            with unittest.mock.patch.dict(
                os.environ,
                {
                    "GEMINI_API_KEY": "test-key",
                    "GOOGLE_CLOUD_PROJECT": "",
                    "GOOGLE_CLOUD_LOCATION": "",
                    "GOOGLE_CLOUD_REGION": "",
                    "AGY_TOKEN": "",
                },
            ):
                env = _invoke(tmp, env_vars=["GEMINI_API_KEY"])
        self.assertEqual(env["SCION_USAGE_SOURCE"], "hooks")

    def test_vertex_ai_auth_also_declares_hooks_usage_source(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            with unittest.mock.patch.object(provision, "_get_agy_version", return_value=(1, 2, 12)):
                with unittest.mock.patch.dict(
                    os.environ,
                    {"GOOGLE_CLOUD_PROJECT": "proj-1", "GOOGLE_CLOUD_REGION": "us-central1"},
                ):
                    env = _invoke(
                        tmp,
                        env_vars=["GOOGLE_CLOUD_PROJECT", "GOOGLE_CLOUD_REGION"],
                        explicit_type="vertex-ai",
                    )
        self.assertEqual(env["SCION_USAGE_SOURCE"], "hooks")
        self.assertEqual(env.get("AGY_ADC_AUTH"), "true")

    def test_no_auth_still_declares_hooks_usage_source(self) -> None:
        # explicit "none" is a valid mode for antigravity (file-only or
        # no-auth setups) -- the usage source is a property of the harness's
        # hook wiring, not of whether auth resolved to something usable.
        with tempfile.TemporaryDirectory() as tmp:
            env = _invoke(tmp, env_vars=[], explicit_type="none")
        self.assertEqual(env["SCION_USAGE_SOURCE"], "hooks")


class ModelResolutionTest(unittest.TestCase):
    """Covers ptone/scion#2453: provision.py used to read harness_config.model
    raw (never resolving a tier like "medium" through config.yaml's
    model_aliases) and ignored SCION_MODEL, the broker-resolved value,
    entirely. _resolve_model restores the same precedence claude/provision.py
    uses: SCION_MODEL (via scion_harness.resolve_model) first, then
    harness_config.model (via scion_harness.normalize_model_alias, not raw),
    then AGY_MODEL, then FLASH_MODEL. None of FLASH_MODEL, the AGY_MODEL
    fallback, or config.yaml's model_aliases values change here.
    """

    def test_scion_model_tier_resolves_through_config_model_aliases(self) -> None:
        with tempfile.TemporaryDirectory() as tmp, temporary_home(tmp):
            ctx = make_ctx(tmp)
            with env_vars(SCION_MODEL="medium", AGY_MODEL=None):
                model = provision._resolve_model(ctx)
        self.assertEqual(model, "Gemini 3.8 Flash (Medium)")

    def test_scion_model_concrete_id_case_preserved(self) -> None:
        with tempfile.TemporaryDirectory() as tmp, temporary_home(tmp):
            ctx = make_ctx(tmp)
            with env_vars(SCION_MODEL="Gemini-Custom-Preview", AGY_MODEL=None):
                model = provision._resolve_model(ctx)
        self.assertEqual(model, "Gemini-Custom-Preview")

    def test_scion_model_unset_uses_harness_config_model_tier(self) -> None:
        # harness_config.model carries a raw tier (e.g. from an older
        # template) -- it must be resolved through model_aliases exactly like
        # SCION_MODEL would be, not passed through to settings.json raw.
        with tempfile.TemporaryDirectory() as tmp, temporary_home(tmp):
            ctx = make_ctx(tmp, model="large")
            with env_vars(SCION_MODEL=None, AGY_MODEL=None):
                model = provision._resolve_model(ctx)
        self.assertEqual(model, "Gemini 3.1 Pro (Low)")

    def test_agy_model_env_fallback(self) -> None:
        with tempfile.TemporaryDirectory() as tmp, temporary_home(tmp):
            ctx = make_ctx(tmp)
            with env_vars(SCION_MODEL=None, AGY_MODEL="agy-operator-model"):
                model = provision._resolve_model(ctx)
        self.assertEqual(model, "agy-operator-model")

    def test_flash_model_fallback_when_nothing_requested(self) -> None:
        with tempfile.TemporaryDirectory() as tmp, temporary_home(tmp):
            ctx = make_ctx(tmp)
            with env_vars(SCION_MODEL=None, AGY_MODEL=None):
                model = provision._resolve_model(ctx)
        self.assertEqual(model, provision.FLASH_MODEL)

    # --- Precedence ordering (review round 1, R1) -----------------------
    # Each test below pins one step of the precedence chain by setting both
    # the winning source and the next lower one, so a mutant that swaps two
    # steps' check order fails here. Verified by hand, each swap kills
    # exactly the one test that sets both of the swapped sources: swapping
    # the SCION_MODEL/harness_config.model checks in _resolve_model breaks
    # only test_scion_model_wins_over_harness_config_model (review round 3,
    # N1 — test_harness_config_model_wins_over_agy_model leaves SCION_MODEL
    # unset, so that swap doesn't affect it); separately, swapping the
    # harness_config.model/AGY_MODEL checks breaks only
    # test_harness_config_model_wins_over_agy_model.

    def test_scion_model_wins_over_harness_config_model(self) -> None:
        with tempfile.TemporaryDirectory() as tmp, temporary_home(tmp):
            ctx = make_ctx(tmp, model="large")
            with env_vars(SCION_MODEL="medium", AGY_MODEL=None):
                model = provision._resolve_model(ctx)
        self.assertEqual(model, "Gemini 3.8 Flash (Medium)")

    def test_harness_config_model_wins_over_agy_model(self) -> None:
        with tempfile.TemporaryDirectory() as tmp, temporary_home(tmp):
            ctx = make_ctx(tmp, model="large")
            with env_vars(SCION_MODEL=None, AGY_MODEL="agy-operator-model"):
                model = provision._resolve_model(ctx)
        self.assertEqual(model, "Gemini 3.1 Pro (Low)")

    def test_agy_model_wins_over_flash_model(self) -> None:
        with tempfile.TemporaryDirectory() as tmp, temporary_home(tmp):
            ctx = make_ctx(tmp)
            with env_vars(SCION_MODEL=None, AGY_MODEL="agy-operator-model"):
                model = provision._resolve_model(ctx)
        self.assertEqual(model, "agy-operator-model")
        self.assertNotEqual(model, provision.FLASH_MODEL)

    def test_blank_scion_model_falls_through_to_harness_config_model(self) -> None:
        # Whitespace-only SCION_MODEL must be treated as unset, not as a
        # (non-matching) concrete model that would block the fallback chain.
        with tempfile.TemporaryDirectory() as tmp, temporary_home(tmp):
            ctx = make_ctx(tmp, model="small")
            with env_vars(SCION_MODEL="  ", AGY_MODEL=None):
                model = provision._resolve_model(ctx)
        self.assertEqual(model, "Gemini 3.1 Flash Lite")

    def test_harness_config_model_concrete_id_case_preserved(self) -> None:
        with tempfile.TemporaryDirectory() as tmp, temporary_home(tmp):
            ctx = make_ctx(tmp, model="Gemini-Custom-Preview")
            with env_vars(SCION_MODEL=None, AGY_MODEL=None):
                model = provision._resolve_model(ctx)
        self.assertEqual(model, "Gemini-Custom-Preview")


class SettingsJsonReprovisionTest(unittest.TestCase):
    """Covers ptone/scion#2453: _prestage_onboarding used to write the model
    key into settings.json only on first creation, so re-provisioning an
    already-provisioned home left a stale model and never touched
    modelProvider either. Both must now refresh on every provision while
    preserving unrelated keys already present in the file.
    """

    def test_existing_settings_model_key_updated_other_keys_preserved(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            cli_dir = os.path.join(tmp, ".gemini", "antigravity-cli")
            os.makedirs(cli_dir, exist_ok=True)
            settings_path = os.path.join(cli_dir, "settings.json")
            with open(settings_path, "w", encoding="utf-8") as f:
                json.dump(
                    {
                        "colorScheme": "light",
                        "model": "Gemini 3.1 Pro (Low)",
                        "customUserSetting": "keep-me",
                    },
                    f,
                )

            provision._prestage_onboarding(
                tmp,
                workspace=os.path.join(tmp, "workspace"),
                model="Gemini 3.8 Flash (Medium)",
            )

            with open(settings_path, "r", encoding="utf-8") as f:
                settings = json.load(f)

        self.assertEqual(settings["model"], "Gemini 3.8 Flash (Medium)")
        self.assertEqual(settings["customUserSetting"], "keep-me")
        self.assertEqual(settings["colorScheme"], "light")

    def test_existing_settings_user_set_color_scheme_preserved(self) -> None:
        # Upstream Gemini review comment (GoogleCloudPlatform/scion#2190,
        # harnesses/antigravity/provision.py:630): back-filling missing
        # onboarding defaults into a valid-but-incomplete existing file must
        # not override a value the user (or a prior provision) already set.
        with tempfile.TemporaryDirectory() as tmp:
            cli_dir = os.path.join(tmp, ".gemini", "antigravity-cli")
            os.makedirs(cli_dir, exist_ok=True)
            settings_path = os.path.join(cli_dir, "settings.json")
            with open(settings_path, "w", encoding="utf-8") as f:
                json.dump({"colorScheme": "light", "model": "old-model"}, f)

            provision._prestage_onboarding(
                tmp,
                workspace=os.path.join(tmp, "workspace"),
                model="new-model",
            )

            with open(settings_path, "r", encoding="utf-8") as f:
                settings = json.load(f)

        self.assertEqual(settings["colorScheme"], "light")

    def test_existing_settings_missing_onboarding_complete_is_backfilled(self) -> None:
        # Upstream Gemini review comment: a valid existing settings.json that
        # lacks onboardingComplete (e.g. hand-edited, or written by an older
        # provisioner version) must get it back-filled -- otherwise a
        # headless agent stalls on AGY's interactive onboarding, which this
        # key exists to skip -- while every other key is preserved.
        with tempfile.TemporaryDirectory() as tmp:
            cli_dir = os.path.join(tmp, ".gemini", "antigravity-cli")
            os.makedirs(cli_dir, exist_ok=True)
            settings_path = os.path.join(cli_dir, "settings.json")
            with open(settings_path, "w", encoding="utf-8") as f:
                json.dump({"model": "old-model", "customUserSetting": "keep-me"}, f)

            provision._prestage_onboarding(
                tmp,
                workspace=os.path.join(tmp, "workspace"),
                model="new-model",
            )

            with open(settings_path, "r", encoding="utf-8") as f:
                settings = json.load(f)

        self.assertIs(settings["onboardingComplete"], True)
        self.assertEqual(settings["customUserSetting"], "keep-me")

    def test_existing_settings_workspace_appended_to_trusted_workspaces(self) -> None:
        # Upstream Gemini review comment: the current workspace must be
        # present in trustedWorkspaces (the trust-dialog skip) even on
        # re-provision into a settings.json that already lists other
        # workspaces -- existing entries must be kept, not replaced.
        with tempfile.TemporaryDirectory() as tmp:
            cli_dir = os.path.join(tmp, ".gemini", "antigravity-cli")
            os.makedirs(cli_dir, exist_ok=True)
            settings_path = os.path.join(cli_dir, "settings.json")
            with open(settings_path, "w", encoding="utf-8") as f:
                json.dump(
                    {"model": "old-model", "trustedWorkspaces": ["/some/other/workspace"]},
                    f,
                )

            workspace = os.path.join(tmp, "workspace")
            provision._prestage_onboarding(tmp, workspace=workspace, model="new-model")

            with open(settings_path, "r", encoding="utf-8") as f:
                settings = json.load(f)

        self.assertEqual(
            settings["trustedWorkspaces"], ["/some/other/workspace", workspace]
        )

    def test_existing_settings_model_provider_still_set_for_api_key_auth(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            cli_dir = os.path.join(tmp, ".gemini", "antigravity-cli")
            os.makedirs(cli_dir, exist_ok=True)
            settings_path = os.path.join(cli_dir, "settings.json")
            with open(settings_path, "w", encoding="utf-8") as f:
                json.dump({"model": "old-model"}, f)

            provision._prestage_onboarding(
                tmp,
                workspace=os.path.join(tmp, "workspace"),
                model="new-model",
                auth_method="api-key",
            )

            with open(settings_path, "r", encoding="utf-8") as f:
                settings = json.load(f)

        self.assertEqual(settings["model"], "new-model")
        self.assertEqual(settings["modelProvider"], "gemini")

    def test_existing_settings_model_provider_absent_for_non_api_key_auth(self) -> None:
        # Review round 2, O1: the api-key-only modelProvider guard is a
        # rewritten branch in this PR, and writing modelProvider under
        # vertex-ai/ADC/oauth would be the auth-mode regression the brief
        # warns about. Pin it for the existing-file path.
        with tempfile.TemporaryDirectory() as tmp:
            cli_dir = os.path.join(tmp, ".gemini", "antigravity-cli")
            os.makedirs(cli_dir, exist_ok=True)
            settings_path = os.path.join(cli_dir, "settings.json")
            with open(settings_path, "w", encoding="utf-8") as f:
                json.dump({"model": "old-model"}, f)

            provision._prestage_onboarding(
                tmp,
                workspace=os.path.join(tmp, "workspace"),
                model="new-model",
                auth_method="vertex-ai",
            )

            with open(settings_path, "r", encoding="utf-8") as f:
                settings = json.load(f)

        self.assertEqual(settings["model"], "new-model")
        self.assertNotIn("modelProvider", settings)

    def test_malformed_existing_settings_model_provider_absent_for_non_api_key_auth(self) -> None:
        # Same as above (O1), but for the malformed-file fallback path added
        # in round 1 (R2), per the review's "also covers the 'every auth
        # mode' wording" suggestion.
        with tempfile.TemporaryDirectory() as tmp:
            cli_dir = os.path.join(tmp, ".gemini", "antigravity-cli")
            os.makedirs(cli_dir, exist_ok=True)
            settings_path = os.path.join(cli_dir, "settings.json")
            with open(settings_path, "w", encoding="utf-8") as f:
                f.write("{bad json")

            provision._prestage_onboarding(
                tmp,
                workspace=os.path.join(tmp, "workspace"),
                model="new-model",
                auth_method="vertex-ai",
            )

            with open(settings_path, "r", encoding="utf-8") as f:
                settings = json.load(f)

        self.assertEqual(settings["model"], "new-model")
        self.assertNotIn("modelProvider", settings)

    def test_non_utf8_existing_settings_falls_back_to_fresh_defaults(self) -> None:
        # Review round 3, O1: load_json's open() raises UnicodeDecodeError
        # (a ValueError, not a json.JSONDecodeError) on a non-UTF-8 file.
        # This read now runs in every auth mode (not just api-key, as
        # before this PR), so a non-UTF-8 settings.json must fall back to
        # the fresh defaults instead of crashing provisioning.
        with tempfile.TemporaryDirectory() as tmp:
            cli_dir = os.path.join(tmp, ".gemini", "antigravity-cli")
            os.makedirs(cli_dir, exist_ok=True)
            settings_path = os.path.join(cli_dir, "settings.json")
            with open(settings_path, "wb") as f:
                f.write(b"\xff\xfe{")

            workspace = os.path.join(tmp, "workspace")
            provision._prestage_onboarding(
                tmp,
                workspace=workspace,
                model="new-model",
                auth_method="vertex-ai",
            )

            with open(settings_path, "r", encoding="utf-8") as f:
                settings = json.load(f)

        self.assertEqual(settings["model"], "new-model")
        self.assertNotIn("modelProvider", settings)
        self.assertIs(settings["onboardingComplete"], True)
        self.assertEqual(settings["trustedWorkspaces"], [workspace])

    def test_malformed_existing_settings_falls_back_to_fresh_defaults(self) -> None:
        # Review round 1, R2: a malformed (or unexpectedly non-dict) existing
        # settings.json must not be rewritten as bare {"model": ...} -- that
        # drops onboardingComplete/trustedWorkspaces, which this function
        # exists to pre-stage so a headless agent skips AGY's interactive
        # onboarding/trust prompts. It should fall back to the same fresh
        # defaults the first-creation path writes, same as if the file never
        # existed, with model (and modelProvider for api-key) applied on top.
        with tempfile.TemporaryDirectory() as tmp:
            cli_dir = os.path.join(tmp, ".gemini", "antigravity-cli")
            os.makedirs(cli_dir, exist_ok=True)
            settings_path = os.path.join(cli_dir, "settings.json")
            with open(settings_path, "w", encoding="utf-8") as f:
                f.write("{bad json")

            workspace = os.path.join(tmp, "workspace")
            provision._prestage_onboarding(
                tmp,
                workspace=workspace,
                model="Gemini 3.8 Flash (Medium)",
                auth_method="api-key",
            )

            with open(settings_path, "r", encoding="utf-8") as f:
                settings = json.load(f)

        self.assertEqual(settings["model"], "Gemini 3.8 Flash (Medium)")
        self.assertEqual(settings["modelProvider"], "gemini")
        self.assertIs(settings["onboardingComplete"], True)
        self.assertEqual(settings["trustedWorkspaces"], [workspace])


class ProvisionModelWiringTest(unittest.TestCase):
    """Review round 4, O1: every model test above calls _resolve_model or
    _prestage_onboarding directly, so none pins the provision() call site
    that actually had the #2453 bug (harness_config.get("model") passed raw,
    SCION_MODEL never read). Drive the real provision() entry point via the
    existing _invoke harness and assert the resolved model lands in
    settings.json. Verified by hand: reverting the provision() call site to
    main's raw expression (ctx.harness_config.get("model") or
    os.environ.get("AGY_MODEL", "") or FLASH_MODEL) makes this test fail
    while the rest of the suite (which exercises _resolve_model and
    _prestage_onboarding directly, not through provision()) still passes.
    """

    def test_scion_model_tier_lands_in_settings_json_via_provision(self) -> None:
        # Deliberately "large", not "medium": FLASH_MODEL's literal value is
        # "Gemini 3.8 Flash (Medium)", the same string the "medium" alias
        # resolves to, so a mutant that falls all the way back to FLASH_MODEL
        # would coincidentally match and go undetected. "large" resolves to
        # "Gemini 3.1 Pro (Low)", which differs from FLASH_MODEL, so the
        # wiring mutant (reverting this call site to main's raw expression)
        # is actually caught here -- confirmed by hand.
        with tempfile.TemporaryDirectory() as tmp:
            with env_vars(SCION_MODEL="large", AGY_MODEL=None):
                _invoke(
                    tmp,
                    env_vars=[],
                    explicit_type="none",
                    harness_config={"model_aliases": dict(ANTIGRAVITY_MODEL_ALIASES)},
                )

            settings_path = os.path.join(tmp, ".gemini", "antigravity-cli", "settings.json")
            with open(settings_path, "r", encoding="utf-8") as f:
                settings = json.load(f)

        self.assertEqual(settings["model"], "Gemini 3.1 Pro (Low)")



# Mirrors harnesses/antigravity/config.yaml's `thinking:` block exactly. The
# Go test TestEmbeddedHarnessThinkingBlocks pins the yaml side to the same
# literal table, so the two cannot drift.
ANTIGRAVITY_THINKING = {
    "levels": [
        {"max": 25, "value": "low"},
        {"max": 50, "value": "medium"},
        {"max": 100, "value": "high"},
    ],
}


class ProvisionThinkingWiringTest(unittest.TestCase):
    """ptone/scion#2673: the level -> `agy --effort` tier table moved from a
    hard-coded _resolve_thinking_tier into config.yaml's `thinking:` block
    (quartile cut points 25 low / 50 medium / 100 high). Drives the real
    provision() entry point and reads the generated agy-wrapper.sh."""

    def _wrapper(
        self,
        raw: str | None,
        thinking: dict[str, Any] | None = None,
        *,
        omit_thinking: bool = False,
    ) -> tuple[str, list[str]]:
        """Provision and return (agy-wrapper.sh, warnings). `thinking`
        None means config.yaml's table; any other value, including {}, is
        passed through as-is. omit_thinking drops the key entirely."""
        harness_config: dict[str, Any] = {"model_aliases": dict(ANTIGRAVITY_MODEL_ALIASES)}
        if not omit_thinking:
            harness_config["thinking"] = ANTIGRAVITY_THINKING if thinking is None else thinking
        warnings: list[str] = []
        real_warn = scion_harness.ProvisionContext.warn

        def capture(ctx: Any, message: str) -> None:
            warnings.append(message)
            real_warn(ctx, message)

        with tempfile.TemporaryDirectory() as tmp:
            with env_vars(SCION_THINKING_LEVEL=raw, SCION_MODEL=None, AGY_MODEL=None), \
                    unittest.mock.patch.object(scion_harness.ProvisionContext, "warn", capture):
                _invoke(
                    tmp,
                    env_vars=[],
                    explicit_type="none",
                    harness_config=harness_config,
                )
            wrapper_path = os.path.join(tmp, ".scion", "harness", "agy-wrapper.sh")
            with open(wrapper_path, "r", encoding="utf-8") as f:
                return f.read(), warnings

    def test_effort_tier_follows_quartile_table(self) -> None:
        cases = (
            ("0", "low"),
            ("25", "low"),
            ("26", "medium"),
            ("30", "medium"),  # low under the old 50/75 cut points
            ("50", "medium"),
            ("51", "high"),
            ("60", "high"),  # medium under the old 50/75 cut points
            ("100", "high"),
            ("150", "high"),
            ("-5", "low"),  # silently dropped by the old .isdigit() check
        )
        for raw, tier in cases:
            with self.subTest(raw=raw):
                wrapper, warnings = self._wrapper(raw)
                self.assertIn(f"--effort {tier} ", wrapper)
                self.assertEqual(wrapper.count("--effort"), 1)
                self.assertEqual(warnings, [])

    def test_effort_tier_is_shell_quoted(self) -> None:
        """Tier values come from config.yaml (any non-empty string), so a
        typo like "high max" must reach agy as one quoted argument rather
        than splitting the wrapper's command line."""
        thinking = {"levels": [{"max": 100, "value": "high max"}]}
        wrapper, _ = self._wrapper("60", thinking)
        self.assertIn("--effort 'high max' ", wrapper)
        self.assertNotIn("--effort high max", wrapper)

    def test_missing_thinking_block_warns_when_level_requested(self) -> None:
        """Gemini review G1: a stale/customized config.yaml without the
        thinking block silently drops a requested level, so warn."""
        for label, kwargs in (("omitted", {"omit_thinking": True}), ("empty", {"thinking": {}})):
            with self.subTest(block=label):
                wrapper, warnings = self._wrapper("60", **kwargs)
                self.assertNotIn("--effort", wrapper)
                self.assertTrue(
                    any("no thinking block" in w for w in warnings),
                    f"expected a missing-thinking-block warning, got: {warnings}",
                )

    def test_missing_thinking_block_silent_without_level(self) -> None:
        """No level requested -> no --effort is the intended outcome, so a
        missing block must not warn on every start."""
        for raw in (None, "   "):
            with self.subTest(raw=raw):
                wrapper, warnings = self._wrapper(raw, omit_thinking=True)
                self.assertNotIn("--effort", wrapper)
                self.assertEqual(warnings, [])
                _, warnings = self._wrapper(raw, thinking={})
                self.assertFalse(
                    any("no thinking block" in w for w in warnings),
                    f"unexpected missing-thinking-block warning: {warnings}",
                )

    def test_unset_level_passes_no_effort_flag(self) -> None:
        for raw in (None, "", "   "):
            with self.subTest(raw=raw):
                wrapper, warnings = self._wrapper(raw)
                self.assertNotIn("--effort", wrapper)
                self.assertEqual(warnings, [])

    def test_invalid_level_passes_no_effort_flag_and_warns(self) -> None:
        for raw in ("abc", "1.5"):
            with self.subTest(raw=raw):
                wrapper, warnings = self._wrapper(raw)
                self.assertNotIn("--effort", wrapper)
                self.assertTrue(
                    any("not a valid integer" in w for w in warnings),
                    f"expected an invalid-level warning, got: {warnings}",
                )


if __name__ == "__main__":
    unittest.main()


class WrapperSecretsDirTest(unittest.TestCase):
    """agy-wrapper.sh reads AGY_TOKEN from SCION_HARNESS_SECRETS_DIR when set."""

    def _wrapper(self, home: str, secrets_dir: str | None) -> str:
        provision._generate_wrapper_script(home, True, False, secrets_dir=secrets_dir)
        with open(os.path.join(home, ".scion", "harness", "agy-wrapper.sh"), encoding="utf-8") as f:
            return f.read()

    def test_default_reads_bundle_secrets(self) -> None:
        with tempfile.TemporaryDirectory() as home:
            script = self._wrapper(home, None)
        self.assertIn(os.path.join(home, ".scion", "harness", "secrets", "AGY_TOKEN"), script)

    def test_secrets_dir_used_when_set(self) -> None:
        with tempfile.TemporaryDirectory() as home:
            script = self._wrapper(home, "/run/scion/mem/harness-secrets")
        self.assertIn("/run/scion/mem/harness-secrets/AGY_TOKEN", script)
        self.assertNotIn(os.path.join(home, ".scion", "harness", "secrets", "AGY_TOKEN"), script)

    def test_secrets_dir_is_shell_quoted(self) -> None:
        secrets_dir = "/run/scion/mem/harness secrets/it's"
        with tempfile.TemporaryDirectory() as home:
            script = self._wrapper(home, secrets_dir)
            wrapper = os.path.join(home, ".scion", "harness", "agy-wrapper.sh")
            check = subprocess.run(["bash", "-n", wrapper], capture_output=True, text=True)
        quoted = shlex.quote(secrets_dir + "/AGY_TOKEN")
        self.assertIn(f"if [ -f {quoted} ]; then", script)
        self.assertIn(f"< {quoted} 2>/dev/null", script)
        self.assertEqual(check.returncode, 0, check.stderr)

    def test_secrets_dir_outside_mem_dir_rejected(self) -> None:
        with tempfile.TemporaryDirectory() as tmp, \
                unittest.mock.patch.dict(os.environ, {
                    "SCION_HARNESS_SECRETS_DIR": "/etc",
                    "SCION_HARNESS_OUTPUTS_DIR": "",
                }):
            with self.assertRaises(scion_harness.ProvisionError):
                _invoke(tmp, env_vars=[], explicit_type="none")

    def test_provision_passes_env_value(self) -> None:
        seen: dict[str, Any] = {}

        def fake_wrapper(*args: Any, **kwargs: Any) -> None:
            seen["secrets_dir"] = kwargs.get("secrets_dir")

        for value, want in (("", None), ("/run/scion/mem/harness-secrets", "/run/scion/mem/harness-secrets")):
            with self.subTest(value=value):
                seen.clear()
                with tempfile.TemporaryDirectory() as tmp, \
                        unittest.mock.patch.object(provision, "_generate_wrapper_script", fake_wrapper), \
                        unittest.mock.patch.dict(os.environ, {
                            "SCION_HARNESS_SECRETS_DIR": value,
                            "SCION_HARNESS_OUTPUTS_DIR": "",
                        }):
                    _invoke(tmp, env_vars=[], explicit_type="none")
                self.assertEqual(seen["secrets_dir"], want)
