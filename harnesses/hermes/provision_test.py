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
import tempfile
import unittest
from contextlib import contextmanager, redirect_stderr

PROVISION_PATH = os.path.join(os.path.dirname(__file__), "provision.py")
SPEC = importlib.util.spec_from_file_location("hermes_provision", PROVISION_PATH)
assert SPEC is not None
provision = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
SPEC.loader.exec_module(provision)


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


class AuthResolutionTest(unittest.TestCase):
    def test_anthropic_takes_precedence_over_openai_and_google(self) -> None:
        env_keys = {"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "GOOGLE_API_KEY"}
        method, key = provision._select_auth_key("", env_keys)
        self.assertEqual(method, "api-key")
        self.assertEqual(key, "ANTHROPIC_API_KEY")

    def test_openai_takes_precedence_over_google(self) -> None:
        env_keys = {"OPENAI_API_KEY", "GOOGLE_API_KEY"}
        method, key = provision._select_auth_key("", env_keys)
        self.assertEqual(method, "api-key")
        self.assertEqual(key, "OPENAI_API_KEY")

    def test_google_key_used_when_alone(self) -> None:
        env_keys = {"GOOGLE_API_KEY"}
        method, key = provision._select_auth_key("", env_keys)
        self.assertEqual(method, "api-key")
        self.assertEqual(key, "GOOGLE_API_KEY")

    def test_explicit_api_key_type_accepted(self) -> None:
        env_keys = {"GOOGLE_API_KEY"}
        method, key = provision._select_auth_key("api-key", env_keys)
        self.assertEqual(method, "api-key")
        self.assertEqual(key, "GOOGLE_API_KEY")

    def test_unknown_auth_type_raises(self) -> None:
        with self.assertRaises(ValueError) as ctx:
            provision._select_auth_key("auth-file", {"ANTHROPIC_API_KEY"})
        self.assertIn("only 'api-key' is supported", str(ctx.exception))

    def test_no_keys_raises(self) -> None:
        with self.assertRaises(ValueError) as ctx:
            provision._select_auth_key("", set())
        self.assertIn("no valid API key found", str(ctx.exception))


class NoAuthModeTest(unittest.TestCase):
    def test_no_auth_activates_when_env_keys_empty_but_candidates_has_metadata(self) -> None:
        """auth-candidates.json always has metadata (schema_version etc.) even
        with zero credentials. The no-auth check must look at env_keys, not
        at whether the candidates dict is truthy."""
        with tempfile.TemporaryDirectory() as tmp:
            home = os.path.join(tmp, "home")
            bundle = os.path.join(tmp, "bundle")
            os.makedirs(os.path.join(bundle, "inputs"))
            os.makedirs(os.path.join(bundle, "outputs"))
            os.makedirs(home)

            candidates = {"schema_version": 1, "env_vars": [], "env_secret_files": {}}
            with open(os.path.join(bundle, "inputs", "auth-candidates.json"), "w") as f:
                json.dump(candidates, f)

            manifest = {
                "harness_bundle_dir": bundle,
                "harness_config": {
                    "instructions_file": "AGENTS.md",
                    "skills_dir": ".hermes/skills",
                    "system_prompt_mode": "none",
                    "no_auth": {"behavior": "drop-to-shell"},
                },
                "outputs": {
                    "env": os.path.join(bundle, "outputs", "env.json"),
                    "resolved_auth": os.path.join(bundle, "outputs", "resolved-auth.json"),
                },
            }

            stderr = io.StringIO()
            with temporary_home(home), redirect_stderr(stderr):
                rc = provision._provision(manifest)

            self.assertEqual(rc, provision.EXIT_OK)
            self.assertIn("no-auth mode", stderr.getvalue())

            with open(os.path.join(bundle, "outputs", "resolved-auth.json"), "r") as f:
                resolved = json.load(f)
            self.assertEqual(resolved["method"], "none")


class InstructionProjectionTest(unittest.TestCase):
    def test_composes_prompts_and_skills(self) -> None:
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
                "harness_config": {
                    "instructions_file": "AGENTS.md",
                    "skills_dir": ".hermes/skills",
                    "system_prompt_mode": "prepend_to_instructions",
                },
            }

            with temporary_home(home):
                provision._apply_instruction_projection(bundle, manifest)
                provision._apply_instruction_projection(bundle, manifest)

            with open(os.path.join(home, "AGENTS.md"), "r") as f:
                content = f.read()

            self.assertEqual(content.count(provision.SCION_MANAGED_BEGIN), 1)
            self.assertIn("# System Instruction\n\nSystem rules", content)
            self.assertIn("# Agent Instructions\n\nAgent rules", content)
            self.assertIn("## example\n\n# Example Skill\n\nUse this skill.", content)
            self.assertIn("## second\n\n# Second Skill\n\nUse this other skill.", content)

    def test_cleans_stale_managed_block_when_inputs_empty(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            home = os.path.join(tmp, "home")
            bundle = os.path.join(tmp, "bundle")
            os.makedirs(os.path.join(bundle, "inputs"))
            os.makedirs(home)

            agents_path = os.path.join(home, "AGENTS.md")
            with open(agents_path, "w") as f:
                f.write(
                    f"{provision.SCION_MANAGED_BEGIN}\n\n"
                    "# Agent Instructions\n\nOld managed content\n\n"
                    f"{provision.SCION_MANAGED_END}\n\n"
                    "# User Notes\n\nKeep this.\n"
                )

            manifest = {
                "harness_config": {
                    "instructions_file": "AGENTS.md",
                    "skills_dir": ".hermes/skills",
                    "system_prompt_mode": "prepend_to_instructions",
                },
            }

            with temporary_home(home):
                provision._apply_instruction_projection(bundle, manifest)

            with open(agents_path, "r") as f:
                content = f.read()

            self.assertNotIn(provision.SCION_MANAGED_BEGIN, content)
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
                    f"{provision.SCION_MANAGED_BEGIN}\n\n"
                    "# Agent Instructions\n\nOld managed content\n\n"
                    f"{provision.SCION_MANAGED_END}\n"
                )

            manifest = {
                "harness_config": {
                    "instructions_file": "AGENTS.md",
                    "skills_dir": ".hermes/skills",
                    "system_prompt_mode": "prepend_to_instructions",
                },
            }

            with temporary_home(home):
                provision._apply_instruction_projection(bundle, manifest)

            self.assertFalse(os.path.exists(agents_path))

    def test_preserves_content_when_end_marker_missing(self) -> None:
        content = (
            "# Before\n\n"
            f"{provision.SCION_MANAGED_BEGIN}\n\n"
            "# Agent Instructions\n\nManaged without an end marker\n\n"
            "# After\n"
        )
        stderr = io.StringIO()

        with redirect_stderr(stderr):
            got = provision._strip_scion_managed_block(content)

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

            with temporary_home(home):
                count = provision._apply_mcp_servers(bundle)

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
