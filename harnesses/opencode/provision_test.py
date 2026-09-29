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
SPEC = importlib.util.spec_from_file_location("opencode_provision", PROVISION_PATH)
assert SPEC is not None
provision = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
SPEC.loader.exec_module(provision)

scion_harness = provision.sh


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

    manifest = {"harness_bundle_dir": bundle, "harness_config": {}}
    with temporary_home(home):
        ctx = scion_harness.ProvisionContext("opencode", manifest)
        provision.provision(ctx)
        env_path = os.path.join(bundle, "outputs", "env.json")
        with open(env_path, "r", encoding="utf-8") as f:
            return json.load(f)


class OpencodeProvisionTest(unittest.TestCase):
    def test_api_key_auth_declares_hooks_usage_source(self) -> None:
        # OpenCode has no native OTel usage signal (config.yaml's
        # capabilities.telemetry.native_emitter is "no"), so usage comes from
        # hooks (design D9/D10, ptone/scion#2053 phase 3b). This is the D10
        # opt-in: the fixture-backed mapping in dialect.yaml is what makes
        # publishing hook usage for this harness allowed at all.
        with tempfile.TemporaryDirectory() as tmp:
            env = _invoke(tmp, env_vars=["ANTHROPIC_API_KEY"])
        self.assertEqual(env["SCION_USAGE_SOURCE"], "hooks")

    def test_vertex_ai_auth_also_declares_hooks_usage_source(self) -> None:
        # _vertex_env_overlay used to *replace* the env dict outright, which
        # would have silently dropped SCION_USAGE_SOURCE for every vertex-ai
        # agent. Pin that the vertex path merges instead.
        with tempfile.TemporaryDirectory() as tmp:
            with temporary_home(tmp), unittest.mock.patch.dict(
                os.environ,
                {"GOOGLE_CLOUD_PROJECT": "proj-1", "GOOGLE_CLOUD_REGION": "us-central1"},
            ):
                env = _invoke(tmp, env_vars=["GOOGLE_CLOUD_PROJECT", "GOOGLE_CLOUD_REGION"], explicit_type="vertex-ai")
        self.assertEqual(env["SCION_USAGE_SOURCE"], "hooks")
        self.assertIn("VERTEXAI_PROJECT", env)


if __name__ == "__main__":
    unittest.main()
