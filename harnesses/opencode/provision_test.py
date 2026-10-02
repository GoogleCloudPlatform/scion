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


CONFIG_REL = os.path.join(".config", "opencode", "opencode.json")


def _read_config(home: str) -> dict:
    with open(os.path.join(home, CONFIG_REL), "r", encoding="utf-8") as f:
        return json.load(f)


def _invoke(
    home: str,
    *,
    env_vars: list[str],
    explicit_type: str = "",
    harness_config: dict | None = None,
    mcp_servers: dict | None = None,
) -> dict:
    bundle = os.path.join(home, ".scion", "harness")
    os.makedirs(os.path.join(bundle, "inputs"), exist_ok=True)
    if mcp_servers is not None:
        with open(os.path.join(bundle, "inputs", "mcp-servers.json"), "w", encoding="utf-8") as f:
            json.dump({"mcp_servers": mcp_servers}, f)
    candidates = {"env_vars": env_vars}
    if explicit_type:
        candidates["explicit_type"] = explicit_type
    with open(os.path.join(bundle, "inputs", "auth-candidates.json"), "w", encoding="utf-8") as f:
        json.dump(candidates, f)

    manifest = {"harness_bundle_dir": bundle, "harness_config": harness_config or {}}
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


class ModelResolutionTest(unittest.TestCase):
    """provision() used to read the dead ctx.model_resolution manifest field
    (the Go side never populates it — see G3 in the generalization audit)
    before falling through to the raw SCION_MODEL env var, with no
    model_aliases mapping at all. It now calls the shared
    scion_harness.resolve_model(ctx) helper.
    """

    def _resolved_model(
        self, tmp: str, *, scion_model: str | None, harness_config: dict | None = None
    ) -> str | None:
        with unittest.mock.patch.dict(os.environ, {}, clear=False):
            if scion_model is None:
                os.environ.pop("SCION_MODEL", None)
            else:
                os.environ["SCION_MODEL"] = scion_model
            _invoke(tmp, env_vars=["ANTHROPIC_API_KEY"], harness_config=harness_config)
        return _read_config(tmp).get("model")

    def test_no_model_requested_omits_model_key(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            self.assertIsNone(self._resolved_model(tmp, scion_model=None))

    def test_concrete_model_passes_through(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            model = self._resolved_model(tmp, scion_model="anthropic/claude-sonnet-4-5")
            self.assertEqual(model, "anthropic/claude-sonnet-4-5")

    def test_case_sensitive_concrete_model_id_is_preserved(self) -> None:
        """R1 of the round-1 review: a concrete model ID's case must survive
        resolve_model unchanged. SCION_MODEL can arrive un-normalized from an
        explicit source Go never touches (a template/hub `env:` block, or
        `--env SCION_MODEL=...`) — Go's run.go::reResolveModelAlias only
        rewrites tier names, not concrete ones. OpenAI fine-tuned model IDs
        carry a mixed-case suffix and OpenCode authenticates with
        OPENAI_API_KEY, so this is a real path, not a hypothetical.
        """
        with tempfile.TemporaryDirectory() as tmp:
            case_sensitive_id = "openai/ft:gpt-4o-mini-2024-07-18:my-org::AbC12xYz"
            model = self._resolved_model(tmp, scion_model=case_sensitive_id)
            self.assertEqual(model, case_sensitive_id)

    def test_size_alias_now_resolves_through_model_aliases(self) -> None:
        """Behavior difference from before G3: a bare size alias used to be
        written into the config verbatim (e.g. "medium") because the
        dead ctx.model_resolution read always fell through to the raw
        SCION_MODEL value with no alias lookup. resolve_model now maps it
        through this harness's own config.yaml model_aliases.
        """
        with tempfile.TemporaryDirectory() as tmp:
            model = self._resolved_model(
                tmp,
                scion_model="medium",
                harness_config={"model_aliases": {"medium": "anthropic/claude-sonnet-4-5"}},
            )
            self.assertEqual(model, "anthropic/claude-sonnet-4-5")

    def test_shorthand_now_resolves_through_model_aliases(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            model = self._resolved_model(
                tmp,
                scion_model="m",
                harness_config={"model_aliases": {"medium": "anthropic/claude-sonnet-4-5"}},
            )
            self.assertEqual(model, "anthropic/claude-sonnet-4-5")


class ConfigSchemaTest(unittest.TestCase):
    """ptone/scion#2679: opencode 1.x (sst/opencode) loads only
    config.json, opencode.json and opencode.jsonc from ~/.config/opencode
    (packages/opencode/src/config/config.ts in v1.18.34). The provisioner
    used to write ~/.config/opencode/.opencode.json with the legacy
    Go-opencode keys (mcpServers, providers, agents), which the installed
    CLI never reads.
    """

    VERTEX_ENV = {"GOOGLE_CLOUD_PROJECT": "proj-1", "GOOGLE_CLOUD_REGION": "us-central1"}

    def _invoke_vertex(self, tmp: str, *, scion_model: str | None = None) -> None:
        with temporary_home(tmp), unittest.mock.patch.dict(os.environ, self.VERTEX_ENV):
            if scion_model is None:
                os.environ.pop("SCION_MODEL", None)
            else:
                os.environ["SCION_MODEL"] = scion_model
            _invoke(tmp, env_vars=list(self.VERTEX_ENV), explicit_type="vertex-ai")

    def test_legacy_dotfile_is_not_written(self) -> None:
        with tempfile.TemporaryDirectory() as tmp, unittest.mock.patch.dict(
            os.environ, {"SCION_MODEL": "anthropic/claude-sonnet-4-5"}
        ):
            _invoke(tmp, env_vars=["ANTHROPIC_API_KEY"])
            self.assertTrue(os.path.isfile(os.path.join(tmp, CONFIG_REL)))
            self.assertFalse(os.path.exists(os.path.join(tmp, ".config", "opencode", ".opencode.json")))

    def test_mcp_servers_merge_under_mcp_key(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            os.makedirs(os.path.dirname(os.path.join(tmp, CONFIG_REL)))
            with open(os.path.join(tmp, CONFIG_REL), "w", encoding="utf-8") as f:
                json.dump({"$schema": "https://opencode.ai/config.json", "mcp": {"keep": {"enabled": False}}}, f)
            _invoke(
                tmp,
                env_vars=["ANTHROPIC_API_KEY"],
                mcp_servers={
                    "local-tool": {"transport": "stdio", "command": "npx", "args": ["tool"], "env": {"A": "1"}},
                    "remote-tool": {"transport": "sse", "url": "https://example.com/mcp"},
                },
            )
            config = _read_config(tmp)
        self.assertNotIn("mcpServers", config)
        self.assertEqual(config["$schema"], "https://opencode.ai/config.json")
        self.assertEqual(
            config["mcp"],
            {
                "keep": {"enabled": False},
                "local-tool": {"type": "local", "command": ["npx", "tool"], "environment": {"A": "1"}},
                "remote-tool": {"type": "remote", "url": "https://example.com/mcp"},
            },
        )

    def test_vertex_writes_current_provider_schema(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            self._invoke_vertex(tmp)
            config = _read_config(tmp)
        self.assertEqual(config["model"], "google-vertex/gemini-2.5-pro")
        self.assertEqual(config["small_model"], "google-vertex/gemini-2.5-flash")
        self.assertIn("github-copilot", config["disabled_providers"])
        for legacy in ("providers", "agents", "mcpServers"):
            self.assertNotIn(legacy, config)

    def test_vertex_explicit_model_wins(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            self._invoke_vertex(tmp, scion_model="google-vertex/gemini-2.5-flash-lite")
            config = _read_config(tmp)
        self.assertEqual(config["model"], "google-vertex/gemini-2.5-flash-lite")

    def test_seeded_home_config_uses_loaded_filename(self) -> None:
        home_dir = os.path.join(os.path.dirname(__file__), "home", ".config", "opencode")
        self.assertTrue(os.path.isfile(os.path.join(home_dir, "opencode.json")))
        self.assertFalse(os.path.exists(os.path.join(home_dir, ".opencode.json")))


if __name__ == "__main__":
    unittest.main()
