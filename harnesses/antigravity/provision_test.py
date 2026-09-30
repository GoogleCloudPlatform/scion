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


if __name__ == "__main__":
    unittest.main()
