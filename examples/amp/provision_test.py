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
"""Tests for SCION_HARNESS_OUTPUTS_DIR and SCION_HARNESS_SECRETS_DIR in the
Amp provisioner. Run with: python3 -m unittest provision_test"""

from __future__ import annotations

import importlib.util
import json
import os
import tempfile
import unittest
import unittest.mock

PROVISION_PATH = os.path.join(os.path.dirname(os.path.abspath(__file__)), "provision.py")
SPEC = importlib.util.spec_from_file_location("amp_provision", PROVISION_PATH)
assert SPEC is not None and SPEC.loader is not None
provision = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(provision)


class HarnessDirEnvTest(unittest.TestCase):
    def _run(self, tmp: str, outputs_dir: str, secrets_dir: str) -> tuple[int, str]:
        bundle = os.path.join(tmp, "bundle")
        os.makedirs(os.path.join(bundle, "inputs"))
        staged_dir = secrets_dir or os.path.join(bundle, "secrets")
        os.makedirs(staged_dir, exist_ok=True)
        with open(os.path.join(staged_dir, "AMP_API_KEY"), "w", encoding="utf-8") as f:
            f.write("amp-value\n")
        with open(os.path.join(bundle, "inputs", "auth-candidates.json"), "w", encoding="utf-8") as f:
            json.dump({
                "env_vars": ["AMP_API_KEY"],
                "env_secret_files": {"AMP_API_KEY": os.path.join(bundle, "secrets", "AMP_API_KEY")},
            }, f)
        env = {"SCION_HARNESS_OUTPUTS_DIR": outputs_dir, "SCION_HARNESS_SECRETS_DIR": secrets_dir}
        with unittest.mock.patch.dict(os.environ, env), \
                unittest.mock.patch.object(provision, "HARNESS_DIRS_ROOT", os.path.join(tmp, "mem")), \
                unittest.mock.patch.object(provision, "AMP_SETTINGS_FILE", os.path.join(tmp, "settings.json")):
            rc = provision._provision({"harness_bundle_dir": bundle})
        return rc, bundle

    def test_unset_uses_bundle_dirs(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            rc, bundle = self._run(tmp, "", "")
            self.assertEqual(rc, provision.EXIT_OK)
            with open(os.path.join(bundle, "outputs", "env.json"), encoding="utf-8") as f:
                self.assertEqual(json.load(f), {"AMP_API_KEY": "amp-value"})

    def test_env_moves_outputs_and_secrets(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            outputs_dir = os.path.join(tmp, "mem", "outputs")
            secrets_dir = os.path.join(tmp, "mem", "harness-secrets")
            rc, bundle = self._run(tmp, outputs_dir, secrets_dir)
            self.assertEqual(rc, provision.EXIT_OK)
            with open(os.path.join(outputs_dir, "env.json"), encoding="utf-8") as f:
                self.assertEqual(json.load(f), {"AMP_API_KEY": "amp-value"})
            self.assertTrue(os.path.isfile(os.path.join(outputs_dir, "resolved-auth.json")))
            self.assertFalse(os.path.exists(os.path.join(bundle, "outputs")))

    def test_relative_value_rejected(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            rc, _ = self._run(tmp, "relative/outputs", "")
            self.assertEqual(rc, provision.EXIT_ERROR)

    def test_value_outside_mem_dir_rejected(self) -> None:
        for case in ("/", "/etc", "{mem}", "{mem}/../outside", "{mem}x/outputs"):
            with self.subTest(case=case), tempfile.TemporaryDirectory() as tmp:
                outputs_dir = case.format(mem=os.path.join(tmp, "mem"))
                rc, bundle = self._run(tmp, outputs_dir, "")
                self.assertEqual(rc, provision.EXIT_ERROR)
                self.assertFalse(os.path.exists(os.path.join(bundle, "outputs")))
                self.assertFalse(os.path.exists(os.path.join(tmp, "outside")))

    def test_symlink_component_rejected(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            mem = os.path.join(tmp, "mem")
            os.makedirs(os.path.join(tmp, "other"))
            os.makedirs(mem)
            os.symlink(os.path.join(tmp, "other"), os.path.join(mem, "link"))
            rc, _ = self._run(tmp, os.path.join(mem, "link", "outputs"), "")
            self.assertEqual(rc, provision.EXIT_ERROR)
            self.assertFalse(os.path.exists(os.path.join(tmp, "other", "outputs")))


if __name__ == "__main__":
    unittest.main()
