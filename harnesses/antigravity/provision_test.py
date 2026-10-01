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
    }
    return scion_harness.ProvisionContext("antigravity", manifest)


def _invoke(home: str, *, env_vars: list[str], explicit_type: str = "") -> dict:
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

    manifest = {"harness_bundle_dir": bundle, "harness_config": {}}
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


if __name__ == "__main__":
    unittest.main()
