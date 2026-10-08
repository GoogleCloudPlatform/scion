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

"""Model-selection tests for the gemini-cli provisioner (ptone/scion#2674)."""

from __future__ import annotations

import importlib.util
import json
import os
import shutil
import tempfile
import unittest
import unittest.mock
from typing import Any

HERE = os.path.dirname(os.path.abspath(__file__))
PROVISION_PATH = os.path.join(HERE, "provision.py")
SPEC = importlib.util.spec_from_file_location("gemini_cli_provision", PROVISION_PATH)
assert SPEC is not None
provision = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
SPEC.loader.exec_module(provision)

scion_harness = provision.scion_harness

IMAGE_SETTINGS = os.path.join(HERE, "home", ".gemini", "settings.json")
CONFIG_YAML = os.path.join(HERE, "config.yaml")

# Mirrors harnesses/gemini-cli/config.yaml exactly; test_config_yaml_* below
# fails if the real file drifts from these values.
GEMINI_MODEL_ALIASES = {
    "small": "gemini-3.5-flash-lite",
    "medium": "gemini-3.6-flash",
    "large": "gemini-3.1-pro-preview",
    "extra-large": "gemini-3.1-pro-preview",
}
DEFAULT_TIER = "medium"


def _harness_config(model: str | None = DEFAULT_TIER) -> dict[str, Any]:
    cfg: dict[str, Any] = {
        "model_aliases": dict(GEMINI_MODEL_ALIASES),
        "no_auth": {"behavior": "allow"},
    }
    if model is not None:
        cfg["model"] = model
    return cfg


class GeminiModelTest(unittest.TestCase):
    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory()
        self.home = self._tmp.name
        self.bundle = os.path.join(self.home, ".scion", "harness")
        os.makedirs(os.path.join(self.bundle, "inputs"))
        self.settings_path = os.path.join(self.home, ".gemini", "settings.json")
        os.makedirs(os.path.dirname(self.settings_path))
        # Start from the real image settings.json, as the broker does.
        shutil.copy(IMAGE_SETTINGS, self.settings_path)
        patcher = unittest.mock.patch.dict(os.environ, {"HOME": self.home})
        patcher.start()
        self.addCleanup(patcher.stop)
        os.environ.pop("SCION_MODEL", None)

    def tearDown(self) -> None:
        self._tmp.cleanup()

    def _ctx(self, harness_config: dict[str, Any]) -> Any:
        return scion_harness.ProvisionContext(
            "gemini-cli",
            {
                "harness_bundle_dir": self.bundle,
                "harness_config": harness_config,
                # Never fall back to the real /workspace (ptone/scion#2993).
                "agent_workspace": os.path.join(self._tmp.name, "workspace"),
            },
        )

    def _settings(self) -> dict[str, Any]:
        with open(self.settings_path, encoding="utf-8") as f:
            return json.load(f)

    def _apply(self, scion_model: str | None, harness_config: dict[str, Any]) -> dict[str, Any]:
        if scion_model is None:
            os.environ.pop("SCION_MODEL", None)
        else:
            os.environ["SCION_MODEL"] = scion_model
        provision._apply_model(self._ctx(harness_config))
        return self._settings()

    # --- shipped files -------------------------------------------------

    def test_image_settings_has_no_model_pin(self) -> None:
        with open(IMAGE_SETTINGS, encoding="utf-8") as f:
            settings = json.load(f)
        self.assertNotIn("name", settings["model"])
        self.assertIs(settings["model"]["skipNextSpeakerCheck"], True)

    def test_config_yaml_declares_default_tier_and_aliases(self) -> None:
        with open(CONFIG_YAML, encoding="utf-8") as f:
            text = f.read()
        self.assertIn(f"\nmodel: {DEFAULT_TIER}\n", text)
        for tier, name in GEMINI_MODEL_ALIASES.items():
            self.assertIn(f"\n  {tier}: {name}\n", text)

    # --- precedence ----------------------------------------------------

    def test_unset_scion_model_uses_harness_config_default_tier(self) -> None:
        settings = self._apply(None, _harness_config())
        self.assertEqual(settings["model"]["name"], "gemini-3.6-flash")
        self.assertIs(settings["model"]["skipNextSpeakerCheck"], True)

    def test_empty_scion_model_uses_harness_config_default_tier(self) -> None:
        for value in ("", "   "):
            with self.subTest(value=value):
                settings = self._apply(value, _harness_config())
                self.assertEqual(settings["model"]["name"], "gemini-3.6-flash")

    def test_scion_model_alias_resolves_through_model_aliases(self) -> None:
        for value, want in (("large", "gemini-3.1-pro-preview"), ("S", "gemini-3.5-flash-lite"),
                            ("xl", "gemini-3.1-pro-preview")):
            with self.subTest(value=value):
                settings = self._apply(value, _harness_config())
                self.assertEqual(settings["model"]["name"], want)

    def test_explicit_scion_model_wins_over_harness_config_default(self) -> None:
        settings = self._apply("gemini-2.5-Pro-custom", _harness_config())
        self.assertEqual(settings["model"]["name"], "gemini-2.5-Pro-custom")

    def test_harness_config_concrete_model_passes_through(self) -> None:
        settings = self._apply(None, _harness_config("gemini-custom-x"))
        self.assertEqual(settings["model"]["name"], "gemini-custom-x")

    def test_no_model_anywhere_removes_stale_name(self) -> None:
        settings = self._settings()
        settings["model"]["name"] = "gemini-3.5-flash"
        scion_harness.atomic_write_json(self.settings_path, settings)
        settings = self._apply(None, _harness_config(model=None))
        self.assertNotIn("name", settings["model"])
        self.assertIs(settings["model"]["skipNextSpeakerCheck"], True)

    def test_refresh_replaces_stale_name(self) -> None:
        settings = self._settings()
        settings["model"]["name"] = "gemini-3.5-flash"
        scion_harness.atomic_write_json(self.settings_path, settings)
        settings = self._apply(None, _harness_config())
        self.assertEqual(settings["model"]["name"], "gemini-3.6-flash")

    # --- end to end through provision() --------------------------------

    def test_provision_writes_default_tier_when_scion_model_unset(self) -> None:
        with open(os.path.join(self.bundle, "inputs", "auth-candidates.json"), "w") as f:
            json.dump({"env_vars": []}, f)
        ctx = self._ctx(_harness_config())
        ctx.select_auth = lambda _spec: scion_harness.ResolvedAuth("none")
        provision.provision(ctx)
        settings = self._settings()
        self.assertEqual(settings["model"]["name"], "gemini-3.6-flash")
        self.assertIs(settings["model"]["skipNextSpeakerCheck"], True)


if __name__ == "__main__":
    unittest.main()
