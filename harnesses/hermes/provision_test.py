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
import io
import json
import os
import sys
import tempfile
import unittest
from contextlib import contextmanager, redirect_stderr

# Import scion_harness from the harnesses root (sibling of hermes/).
sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))
import scion_harness  # type: ignore[import-not-found]

PROVISION_PATH = os.path.join(os.path.dirname(__file__), "provision.py")
SPEC = importlib.util.spec_from_file_location("hermes_provision", PROVISION_PATH)
assert SPEC is not None
provision = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
SPEC.loader.exec_module(provision)

MANAGED_BEGIN = scion_harness._MANAGED_BEGIN_STANDARD
MANAGED_END = scion_harness._MANAGED_END_STANDARD
LEGACY_BEGIN = "<!-- BEGIN SCION MANAGED HERMES INSTRUCTIONS -->"
LEGACY_END = "<!-- END SCION MANAGED HERMES INSTRUCTIONS -->"


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


def _make_ctx(
    bundle_dir: str,
    env_vars: list[str] | None = None,
    explicit_type: str = "",
    env_secret_files: dict[str, str] | None = None,
    harness_config: dict | None = None,
) -> scion_harness.ProvisionContext:
    """Build a ProvisionContext with auth-candidates staged in bundle_dir."""
    os.makedirs(os.path.join(bundle_dir, "inputs"), exist_ok=True)
    os.makedirs(os.path.join(bundle_dir, "outputs"), exist_ok=True)

    candidates: dict = {}
    if env_vars is not None or explicit_type or env_secret_files is not None:
        candidates["env_vars"] = env_vars or []
        candidates["env_secret_files"] = env_secret_files or {}
        if explicit_type:
            candidates["explicit_type"] = explicit_type

    cand_path = os.path.join(bundle_dir, "inputs", "auth-candidates.json")
    with open(cand_path, "w") as f:
        json.dump(candidates, f)

    manifest = {
        "harness_bundle_dir": bundle_dir,
        "harness_config": harness_config or {},
    }
    return scion_harness.ProvisionContext("hermes", manifest)


class AuthResolutionTest(unittest.TestCase):
    def _select(self, env_vars: list[str], explicit: str = "") -> scion_harness.ResolvedAuth:
        with tempfile.TemporaryDirectory() as tmp:
            ctx = _make_ctx(os.path.join(tmp, "bundle"), env_vars=env_vars, explicit_type=explicit)
            return ctx.select_auth(provision.AUTH)

    def test_anthropic_takes_precedence_over_openai_and_google(self) -> None:
        result = self._select(["ANTHROPIC_API_KEY", "OPENAI_API_KEY", "GOOGLE_API_KEY"])
        self.assertEqual(result.method, "api-key")
        self.assertEqual(result.env_key, "ANTHROPIC_API_KEY")

    def test_openai_takes_precedence_over_google(self) -> None:
        result = self._select(["OPENAI_API_KEY", "GOOGLE_API_KEY"])
        self.assertEqual(result.method, "api-key")
        self.assertEqual(result.env_key, "OPENAI_API_KEY")

    def test_google_key_used_when_alone(self) -> None:
        result = self._select(["GOOGLE_API_KEY"])
        self.assertEqual(result.method, "api-key")
        self.assertEqual(result.env_key, "GOOGLE_API_KEY")

    def test_explicit_api_key_type_accepted(self) -> None:
        result = self._select(["GOOGLE_API_KEY"], explicit="api-key")
        self.assertEqual(result.method, "api-key")
        self.assertEqual(result.env_key, "GOOGLE_API_KEY")

    def test_unknown_auth_type_raises(self) -> None:
        with self.assertRaises(scion_harness.ProvisionError) as ctx:
            self._select(["ANTHROPIC_API_KEY"], explicit="auth-file")
        self.assertIn("valid types are", str(ctx.exception))

    def test_vertex_project_id_selects_vertex_ai(self) -> None:
        result = self._select(["VERTEX_PROJECT_ID"])
        self.assertEqual(result.method, "vertex-ai")
        self.assertEqual(result.env_key, "VERTEX_PROJECT_ID")

    def test_google_cloud_project_selects_vertex_ai(self) -> None:
        result = self._select(["GOOGLE_CLOUD_PROJECT"])
        self.assertEqual(result.method, "vertex-ai")
        self.assertEqual(result.env_key, "GOOGLE_CLOUD_PROJECT")

    def test_api_key_takes_precedence_over_vertex(self) -> None:
        result = self._select(["ANTHROPIC_API_KEY", "VERTEX_PROJECT_ID"])
        self.assertEqual(result.method, "api-key")
        self.assertEqual(result.env_key, "ANTHROPIC_API_KEY")

    def test_explicit_vertex_ai_type_accepted(self) -> None:
        result = self._select(["VERTEX_PROJECT_ID"], explicit="vertex-ai")
        self.assertEqual(result.method, "vertex-ai")
        self.assertEqual(result.env_key, "VERTEX_PROJECT_ID")

    def test_no_keys_raises(self) -> None:
        with self.assertRaises(scion_harness.ProvisionError) as ctx:
            self._select([])
        self.assertIn("no valid auth method found", str(ctx.exception))


_VERTEX_ENV_KEYS = [
    "VERTEX_PROJECT_ID", "VERTEX_REGION", "VERTEX_CREDENTIALS_PATH",
    "GOOGLE_CLOUD_PROJECT", "GOOGLE_CLOUD_REGION", "GOOGLE_CLOUD_LOCATION",
    "GOOGLE_APPLICATION_CREDENTIALS",
]


@contextmanager
def clean_vertex_env():
    """Temporarily unset Vertex/GCP env vars so tests aren't affected by the host."""
    saved = {k: os.environ.pop(k) for k in _VERTEX_ENV_KEYS if k in os.environ}
    try:
        yield
    finally:
        os.environ.update(saved)


class VertexAIProvisionTest(unittest.TestCase):
    def test_vertex_ai_sets_default_model_and_env(self) -> None:
        with tempfile.TemporaryDirectory() as tmp, clean_vertex_env():
            home = os.path.join(tmp, "home")
            bundle = os.path.join(tmp, "bundle")
            os.makedirs(os.path.join(bundle, "outputs"))
            os.makedirs(home)

            ctx = _make_ctx(
                bundle,
                env_vars=["VERTEX_PROJECT_ID"],
                env_secret_files={"VERTEX_PROJECT_ID": ""},
                harness_config={
                    "instructions_file": "AGENTS.md",
                    "skills_dir": ".hermes/skills",
                    "system_prompt_mode": "none",
                },
            )

            stderr = io.StringIO()
            with temporary_home(home), redirect_stderr(stderr):
                provision.provision(ctx)

            with open(os.path.join(bundle, "outputs", "resolved-auth.json"), "r") as f:
                resolved = json.load(f)
            self.assertEqual(resolved["method"], "vertex-ai")
            self.assertTrue(resolved.get("vertex_ai"))

            with open(os.path.join(bundle, "outputs", "env.json"), "r") as f:
                env = json.load(f)
            self.assertEqual(env["HERMES_INFERENCE_MODEL"], "google/gemini-2.5-flash")
            self.assertEqual(env["VERTEX_REGION"], "us-central1")

    def test_vertex_ai_respects_explicit_model(self) -> None:
        with tempfile.TemporaryDirectory() as tmp, clean_vertex_env():
            home = os.path.join(tmp, "home")
            bundle = os.path.join(tmp, "bundle")
            os.makedirs(os.path.join(bundle, "outputs"))
            os.makedirs(home)

            manifest = {
                "harness_bundle_dir": bundle,
                "harness_config": {
                    "instructions_file": "AGENTS.md",
                    "skills_dir": ".hermes/skills",
                    "system_prompt_mode": "none",
                },
                "model_resolution": {"resolved_model": "google/gemini-2.5-pro"},
            }

            os.makedirs(os.path.join(bundle, "inputs"), exist_ok=True)
            candidates = {
                "env_vars": ["VERTEX_PROJECT_ID"],
                "env_secret_files": {"VERTEX_PROJECT_ID": ""},
            }
            with open(os.path.join(bundle, "inputs", "auth-candidates.json"), "w") as f:
                json.dump(candidates, f)

            ctx = scion_harness.ProvisionContext("hermes", manifest)

            stderr = io.StringIO()
            with temporary_home(home), redirect_stderr(stderr):
                provision.provision(ctx)

            with open(os.path.join(bundle, "outputs", "env.json"), "r") as f:
                env = json.load(f)
            self.assertEqual(env["HERMES_INFERENCE_MODEL"], "google/gemini-2.5-pro")


class VertexRegionFallbackTest(unittest.TestCase):
    """Test VERTEX_REGION → GOOGLE_CLOUD_REGION → GOOGLE_CLOUD_LOCATION → us-central1."""

    def _region(self, env_overrides: dict[str, str]) -> str:
        with tempfile.TemporaryDirectory() as tmp, clean_vertex_env():
            bundle = os.path.join(tmp, "bundle")
            os.makedirs(os.path.join(bundle, "inputs"))
            os.makedirs(os.path.join(bundle, "outputs"))

            secrets_dir = os.path.join(bundle, "secrets")
            os.makedirs(secrets_dir, exist_ok=True)
            env_secret_files: dict[str, str] = {}
            for k, v in env_overrides.items():
                path = os.path.join(secrets_dir, k)
                with open(path, "w") as f:
                    f.write(v)
                env_secret_files[k] = path

            ctx = _make_ctx(bundle, env_vars=list(env_overrides), env_secret_files=env_secret_files)
            return provision._build_vertex_env(ctx).get("VERTEX_REGION", "")

    def test_vertex_region_preferred(self) -> None:
        self.assertEqual(
            self._region({"VERTEX_REGION": "europe-west1", "GOOGLE_CLOUD_REGION": "asia-east1"}),
            "europe-west1",
        )

    def test_google_cloud_region_second(self) -> None:
        self.assertEqual(self._region({"GOOGLE_CLOUD_REGION": "asia-east1"}), "asia-east1")

    def test_google_cloud_location_third(self) -> None:
        self.assertEqual(self._region({"GOOGLE_CLOUD_LOCATION": "us-west1"}), "us-west1")

    def test_default_us_central1(self) -> None:
        self.assertEqual(self._region({}), "us-central1")


class NoAuthModeTest(unittest.TestCase):
    def test_no_auth_activates_when_env_keys_empty_but_candidates_has_metadata(self) -> None:
        """auth-candidates.json always has metadata (schema_version etc.) even
        with zero credentials. The no-auth check must look at env_keys, not
        at whether the candidates dict is truthy."""
        with tempfile.TemporaryDirectory() as tmp:
            home = os.path.join(tmp, "home")
            bundle = os.path.join(tmp, "bundle")
            os.makedirs(os.path.join(bundle, "outputs"))
            os.makedirs(home)

            ctx = _make_ctx(
                bundle,
                env_vars=[],
                env_secret_files={},
                harness_config={
                    "instructions_file": "AGENTS.md",
                    "skills_dir": ".hermes/skills",
                    "system_prompt_mode": "none",
                    "no_auth": {"behavior": "drop-to-shell"},
                },
            )

            stderr = io.StringIO()
            with temporary_home(home), redirect_stderr(stderr):
                provision.provision(ctx)

            self.assertIn("no-auth", stderr.getvalue())

            with open(os.path.join(bundle, "outputs", "resolved-auth.json"), "r") as f:
                resolved = json.load(f)
            self.assertEqual(resolved["method"], "none")


class InstructionProjectionTest(unittest.TestCase):
    def test_composes_prompts_without_skills_by_default(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            home = os.path.join(tmp, "home")
            bundle = os.path.join(tmp, "bundle")
            os.makedirs(os.path.join(bundle, "inputs"))
            os.makedirs(os.path.join(home, ".hermes", "skills", "example"))
            os.makedirs(os.path.join(home, ".hermes", "skills", "second"))

            with open(os.path.join(bundle, "inputs", "system-prompt.md"), "w") as f:
                f.write("System rules")
            with open(os.path.join(bundle, "inputs", "instructions.md"), "w") as f:
                f.write("Agent rules")
            with open(
                os.path.join(home, ".hermes", "skills", "example", "SKILL.md"), "w"
            ) as f:
                f.write("# Example Skill\n\nUse this skill.")
            with open(
                os.path.join(home, ".hermes", "skills", "second", "SKILL.md"), "w"
            ) as f:
                f.write("# Second Skill\n\nUse this other skill.")

            manifest = {
                "harness_bundle_dir": bundle,
                "harness_config": {
                    "instructions_file": "AGENTS.md",
                    "skills_dir": ".hermes/skills",
                    "system_prompt_mode": "prepend_to_instructions",
                },
            }
            ctx = scion_harness.ProvisionContext("hermes", manifest)

            with temporary_home(home):
                scion_harness.project_instructions(
                    ctx, "AGENTS.md", skills_dir=".hermes/skills",
                )
                scion_harness.project_instructions(
                    ctx, "AGENTS.md", skills_dir=".hermes/skills",
                )

            with open(os.path.join(home, "AGENTS.md"), "r") as f:
                content = f.read()

            self.assertEqual(content.count(MANAGED_BEGIN), 1)
            self.assertIn("# System Instruction\n\nSystem rules", content)
            self.assertIn("# Agent Instructions\n\nAgent rules", content)
            self.assertNotIn("# Skills", content)
            self.assertNotIn("# Example Skill", content)

    def test_cleans_stale_managed_block_when_inputs_empty(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            home = os.path.join(tmp, "home")
            bundle = os.path.join(tmp, "bundle")
            os.makedirs(os.path.join(bundle, "inputs"))
            os.makedirs(home)

            agents_path = os.path.join(home, "AGENTS.md")
            with open(agents_path, "w") as f:
                f.write(
                    f"{LEGACY_BEGIN}\n\n"
                    "# Agent Instructions\n\nOld managed content\n\n"
                    f"{LEGACY_END}\n\n"
                    "# User Notes\n\nKeep this.\n"
                )

            manifest = {
                "harness_bundle_dir": bundle,
                "harness_config": {
                    "instructions_file": "AGENTS.md",
                    "skills_dir": ".hermes/skills",
                    "system_prompt_mode": "prepend_to_instructions",
                },
            }
            ctx = scion_harness.ProvisionContext("hermes", manifest)

            with temporary_home(home):
                scion_harness.project_instructions(
                    ctx, "AGENTS.md", skills_dir=".hermes/skills",
                )

            with open(agents_path, "r") as f:
                content = f.read()

            self.assertNotIn(LEGACY_BEGIN, content)
            self.assertNotIn("Old managed content", content)
            self.assertEqual(content, "# User Notes\n\nKeep this.\n")

    def test_removes_file_when_only_stale_managed_block_remains(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            home = os.path.join(tmp, "home")
            bundle = os.path.join(tmp, "bundle")
            os.makedirs(os.path.join(bundle, "inputs"))
            os.makedirs(home)

            agents_path = os.path.join(home, "AGENTS.md")
            with open(agents_path, "w") as f:
                f.write(
                    f"{LEGACY_BEGIN}\n\n"
                    "# Agent Instructions\n\nOld managed content\n\n"
                    f"{LEGACY_END}\n"
                )

            manifest = {
                "harness_bundle_dir": bundle,
                "harness_config": {
                    "instructions_file": "AGENTS.md",
                    "skills_dir": ".hermes/skills",
                    "system_prompt_mode": "prepend_to_instructions",
                },
            }
            ctx = scion_harness.ProvisionContext("hermes", manifest)

            with temporary_home(home):
                scion_harness.project_instructions(
                    ctx, "AGENTS.md", skills_dir=".hermes/skills",
                )

            self.assertFalse(os.path.exists(agents_path))

    def test_preserves_content_when_end_marker_missing(self) -> None:
        content = (
            "# Before\n\n"
            f"{LEGACY_BEGIN}\n\n"
            "# Agent Instructions\n\nManaged without an end marker\n\n"
            "# After\n"
        )
        stderr = io.StringIO()

        with redirect_stderr(stderr):
            got = scion_harness._strip_managed_block(content)

        self.assertEqual(got, content)
        self.assertIn("Aborting strip to prevent data loss", stderr.getvalue())


class MCPEntryBuildingTest(unittest.TestCase):
    def test_stdio_transport(self) -> None:
        spec = {
            "transport": "stdio",
            "command": "npx",
            "args": ["-y", "@modelcontextprotocol/server-filesystem"],
            "env": {"HOME": "/home/scion"},
        }
        entry = provision._build_mcp_entry("fs", spec)
        self.assertIsNotNone(entry)
        self.assertEqual(entry["command"], "npx")
        self.assertEqual(entry["args"], ["-y", "@modelcontextprotocol/server-filesystem"])
        self.assertEqual(entry["env"], {"HOME": "/home/scion"})

    def test_sse_transport(self) -> None:
        spec = {
            "transport": "sse",
            "url": "https://mcp.example.com/sse",
            "headers": {"Authorization": "Bearer tok"},
        }
        entry = provision._build_mcp_entry("remote", spec)
        self.assertIsNotNone(entry)
        self.assertEqual(entry["url"], "https://mcp.example.com/sse")
        self.assertEqual(entry["headers"], {"Authorization": "Bearer tok"})

    def test_streamable_http_transport(self) -> None:
        spec = {
            "transport": "streamable-http",
            "url": "https://mcp.example.com/stream",
        }
        entry = provision._build_mcp_entry("stream", spec)
        self.assertIsNotNone(entry)
        self.assertEqual(entry["url"], "https://mcp.example.com/stream")
        self.assertNotIn("headers", entry)

    def test_unknown_transport_returns_none(self) -> None:
        spec = {"transport": "grpc", "url": "localhost:50051"}
        stderr = io.StringIO()
        with redirect_stderr(stderr):
            entry = provision._build_mcp_entry("bad", spec)
        self.assertIsNone(entry)
        self.assertIn("unsupported transport", stderr.getvalue())

    def test_stdio_missing_command_returns_none(self) -> None:
        spec = {"transport": "stdio"}
        stderr = io.StringIO()
        with redirect_stderr(stderr):
            entry = provision._build_mcp_entry("no-cmd", spec)
        self.assertIsNone(entry)
        self.assertIn("missing command", stderr.getvalue())

    def test_stale_mcp_json_removed_when_no_servers(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            home = os.path.join(tmp, "home")
            bundle = os.path.join(tmp, "bundle")
            hermes_dir = os.path.join(home, ".hermes")
            os.makedirs(os.path.join(bundle, "inputs"))
            os.makedirs(hermes_dir)

            mcp_path = os.path.join(hermes_dir, "mcp.json")
            with open(mcp_path, "w") as f:
                json.dump({"mcpServers": {"old": {"command": "old-server"}}}, f)
            self.assertTrue(os.path.isfile(mcp_path))

            manifest = {"harness_bundle_dir": bundle}
            ctx = scion_harness.ProvisionContext("hermes", manifest)
            with temporary_home(home):
                count = provision._apply_mcp_servers(ctx)

            self.assertEqual(count, 0)
            self.assertFalse(os.path.isfile(mcp_path))

    def test_sse_missing_url_returns_none(self) -> None:
        spec = {"transport": "sse"}
        stderr = io.StringIO()
        with redirect_stderr(stderr):
            entry = provision._build_mcp_entry("no-url", spec)
        self.assertIsNone(entry)
        self.assertIn("missing url", stderr.getvalue())


if __name__ == "__main__":
    unittest.main()
