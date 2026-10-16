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
import re
import stat
import tempfile
import unittest
import unittest.mock
from contextlib import contextmanager

HERE = os.path.dirname(os.path.abspath(__file__))
SPEC = importlib.util.spec_from_file_location("pi_provision", os.path.join(HERE, "provision.py"))
assert SPEC is not None and SPEC.loader is not None
provision = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(provision)
sh = provision.sh

SECRET = "sk-test-SECRET-VALUE-0123456789"


@contextmanager
def env(home: str, **extra: str):
    """Isolated HOME plus a clean set of the pi/Scion knobs."""
    drop = [k for k in os.environ if k.startswith(("SCION_PI_", "SCION_MODEL", "SCION_HARNESS_"))]
    with unittest.mock.patch.dict(os.environ, {"HOME": home, **extra}, clear=False):
        for k in drop:
            if k not in extra:
                os.environ.pop(k, None)
        yield


def invoke(
    home: str,
    *,
    env_vars: list[str] | None = None,
    explicit_type: str = "",
    auth_secret: str | None = None,
    harness_config: dict | None = None,
    mcp_servers: dict | None = None,
    system_prompt: str = "",
    instructions: str = "",
) -> dict:
    bundle = os.path.join(home, ".scion", "harness")
    inputs = os.path.join(bundle, "inputs")
    os.makedirs(inputs, exist_ok=True)
    if mcp_servers is not None:
        with open(os.path.join(inputs, "mcp-servers.json"), "w", encoding="utf-8") as f:
            json.dump({"mcp_servers": mcp_servers}, f)
    if system_prompt:
        with open(os.path.join(inputs, "system-prompt.md"), "w", encoding="utf-8") as f:
            f.write(system_prompt)
    if instructions:
        with open(os.path.join(inputs, "instructions.md"), "w", encoding="utf-8") as f:
            f.write(instructions)
    candidates: dict = {"env_vars": env_vars} if env_vars else {}
    if explicit_type:
        candidates["explicit_type"] = explicit_type
    if auth_secret is not None:
        secrets = os.path.join(bundle, "secrets")
        os.makedirs(secrets, exist_ok=True)
        spath = os.path.join(secrets, "PI_AUTH")
        with open(spath, "w", encoding="utf-8") as f:
            f.write(auth_secret)
        candidates["file_secret_files"] = {"PI_AUTH": spath}
    if candidates:
        with open(os.path.join(inputs, "auth-candidates.json"), "w", encoding="utf-8") as f:
            json.dump(candidates, f)
    manifest = {
        "harness_bundle_dir": bundle,
        "harness_config": harness_config if harness_config is not None else {"no_auth": {"behavior": "drop-to-shell"}},
        "agent_workspace": os.path.join(home, "workspace"),
    }
    ctx = sh.ProvisionContext("pi", manifest)
    provision.provision(ctx)
    with open(os.path.join(bundle, "outputs", "env.json"), encoding="utf-8") as f:
        return json.load(f)


def read_json(home: str, *rel: str) -> dict:
    with open(os.path.join(home, *rel), encoding="utf-8") as f:
        return json.load(f)


SETTINGS = (".pi", "agent", "settings.json")
AUTH = (".pi", "agent", "auth.json")


class AuthTest(unittest.TestCase):
    def test_api_key_selected_writes_no_auth_file(self) -> None:
        with tempfile.TemporaryDirectory() as tmp, env(tmp):
            out = invoke(tmp, env_vars=["ANTHROPIC_API_KEY"])
            self.assertFalse(os.path.exists(os.path.join(tmp, *AUTH)))
        self.assertEqual(out["PI_SKIP_VERSION_CHECK"], "1")
        self.assertEqual(out["PI_TELEMETRY"], "0")

    def test_env_json_never_contains_the_secret(self) -> None:
        with tempfile.TemporaryDirectory() as tmp, env(tmp, ANTHROPIC_API_KEY=SECRET):
            out = invoke(tmp, env_vars=["ANTHROPIC_API_KEY"])
        self.assertNotIn(SECRET, json.dumps(out))

    def test_auth_file_written_0600(self) -> None:
        body = json.dumps({"anthropic": {"type": "api_key", "key": SECRET}})
        with tempfile.TemporaryDirectory() as tmp, env(tmp):
            invoke(tmp, auth_secret=body)
            path = os.path.join(tmp, *AUTH)
            mode = stat.S_IMODE(os.stat(path).st_mode)
            self.assertEqual(mode, 0o600)
            self.assertEqual(read_json(tmp, *AUTH)["anthropic"]["key"], SECRET)

    def test_existing_auth_file_not_overwritten(self) -> None:
        with tempfile.TemporaryDirectory() as tmp, env(tmp):
            path = os.path.join(tmp, *AUTH)
            os.makedirs(os.path.dirname(path))
            with open(path, "w", encoding="utf-8") as f:
                f.write('{"refreshed": true}')
            os.chmod(path, 0o644)
            invoke(tmp, auth_secret='{"stale": true}')
            self.assertEqual(read_json(tmp, *AUTH), {"refreshed": True})
            self.assertEqual(stat.S_IMODE(os.stat(path).st_mode), 0o600)

    def test_invalid_auth_secret_errors_without_leaking_it(self) -> None:
        bad = "not-json " + SECRET
        with tempfile.TemporaryDirectory() as tmp, env(tmp):
            with self.assertRaises(sh.ProvisionError) as cm:
                invoke(tmp, auth_secret=bad)
            self.assertNotIn(SECRET, str(cm.exception))
            self.assertFalse(os.path.exists(os.path.join(tmp, *AUTH)))

    def test_non_object_and_empty_auth_secret_rejected(self) -> None:
        for body in ("[1,2]", "   "):
            with tempfile.TemporaryDirectory() as tmp, env(tmp):
                with self.assertRaises(sh.ProvisionError):
                    invoke(tmp, auth_secret=body)

    def test_unknown_explicit_type_rejected(self) -> None:
        with tempfile.TemporaryDirectory() as tmp, env(tmp):
            with self.assertRaises(sh.ProvisionError):
                invoke(tmp, env_vars=["ANTHROPIC_API_KEY"], explicit_type="vertex-ai")

    def test_no_credentials_with_no_auth_declared_is_not_fatal(self) -> None:
        with tempfile.TemporaryDirectory() as tmp, env(tmp):
            invoke(tmp)  # no_auth declared in the default harness_config

    def test_no_credentials_without_no_auth_fails(self) -> None:
        with tempfile.TemporaryDirectory() as tmp, env(tmp):
            with self.assertRaises(sh.ProvisionError):
                invoke(tmp, harness_config={})

    def test_api_key_vars_cover_anthropic_first(self) -> None:
        self.assertEqual(provision.API_KEY_VARS[0], "ANTHROPIC_API_KEY")
        self.assertEqual(len(provision.API_KEY_VARS), len(set(provision.API_KEY_VARS)))


class SettingsTest(unittest.TestCase):
    def test_safe_defaults(self) -> None:
        with tempfile.TemporaryDirectory() as tmp, env(tmp):
            invoke(tmp, env_vars=["ANTHROPIC_API_KEY"])
            s = read_json(tmp, *SETTINGS)
        self.assertEqual(s["defaultProjectTrust"], "never")
        self.assertIs(s["quietStartup"], True)
        self.assertIs(s["enableInstallTelemetry"], False)

    def test_user_settings_preserved_and_idempotent(self) -> None:
        with tempfile.TemporaryDirectory() as tmp, env(tmp):
            path = os.path.join(tmp, *SETTINGS)
            os.makedirs(os.path.dirname(path))
            with open(path, "w", encoding="utf-8") as f:
                json.dump({"theme": "light", "quietStartup": False}, f)
            invoke(tmp, env_vars=["ANTHROPIC_API_KEY"])
            first = read_json(tmp, *SETTINGS)
            invoke(tmp, env_vars=["ANTHROPIC_API_KEY"])
            second = read_json(tmp, *SETTINGS)
        self.assertEqual(first, second)
        self.assertEqual(first["theme"], "light")
        self.assertIs(first["quietStartup"], False)

    def test_malformed_existing_settings_errors(self) -> None:
        with tempfile.TemporaryDirectory() as tmp, env(tmp):
            path = os.path.join(tmp, *SETTINGS)
            os.makedirs(os.path.dirname(path))
            with open(path, "w", encoding="utf-8") as f:
                f.write("{nope")
            with self.assertRaises(sh.ProvisionError):
                invoke(tmp, env_vars=["ANTHROPIC_API_KEY"])

    def test_trust_env_values(self) -> None:
        for value in ("never", "ask", "always"):
            with tempfile.TemporaryDirectory() as tmp, env(tmp, SCION_PI_PROJECT_TRUST=value):
                invoke(tmp, env_vars=["ANTHROPIC_API_KEY"])
                self.assertEqual(read_json(tmp, *SETTINGS)["defaultProjectTrust"], value)

    def test_trust_env_invalid_errors(self) -> None:
        with tempfile.TemporaryDirectory() as tmp, env(tmp, SCION_PI_PROJECT_TRUST="yes"):
            with self.assertRaises(sh.ProvisionError):
                invoke(tmp, env_vars=["ANTHROPIC_API_KEY"])

    def test_trust_env_beats_settings_overlay(self) -> None:
        overlay = json.dumps({"defaultProjectTrust": "always"})
        with tempfile.TemporaryDirectory() as tmp, env(
            tmp, SCION_PI_SETTINGS=overlay, SCION_PI_PROJECT_TRUST="ask"
        ):
            invoke(tmp, env_vars=["ANTHROPIC_API_KEY"])
            self.assertEqual(read_json(tmp, *SETTINGS)["defaultProjectTrust"], "ask")

    def test_settings_overlay_cannot_set_invalid_trust(self) -> None:
        overlay = json.dumps({"defaultProjectTrust": "whatever"})
        with tempfile.TemporaryDirectory() as tmp, env(tmp, SCION_PI_SETTINGS=overlay):
            with self.assertRaises(sh.ProvisionError):
                invoke(tmp, env_vars=["ANTHROPIC_API_KEY"])

    def test_settings_overlay_deep_merges(self) -> None:
        overlay = json.dumps({"theme": "dark", "compaction": {"enabled": False}})
        with tempfile.TemporaryDirectory() as tmp, env(tmp, SCION_PI_SETTINGS=overlay):
            invoke(tmp, env_vars=["ANTHROPIC_API_KEY"])
            s = read_json(tmp, *SETTINGS)
        self.assertEqual(s["theme"], "dark")
        self.assertEqual(s["compaction"], {"enabled": False})
        self.assertEqual(s["defaultProjectTrust"], "never")

    def test_settings_overlay_rejects_credentials_without_echoing_values(self) -> None:
        overlay = json.dumps({"providers": {"x": {"apiKey": SECRET}}})
        with tempfile.TemporaryDirectory() as tmp, env(tmp, SCION_PI_SETTINGS=overlay):
            with self.assertRaises(sh.ProvisionError) as cm:
                invoke(tmp, env_vars=["ANTHROPIC_API_KEY"])
        self.assertNotIn(SECRET, str(cm.exception))
        self.assertIn("apiKey", str(cm.exception))

    def test_settings_overlay_malformed_or_non_object(self) -> None:
        for raw in ("{bad " + SECRET, "[1]"):
            with tempfile.TemporaryDirectory() as tmp, env(tmp, SCION_PI_SETTINGS=raw):
                with self.assertRaises(sh.ProvisionError) as cm:
                    invoke(tmp, env_vars=["ANTHROPIC_API_KEY"])
                self.assertNotIn(SECRET, str(cm.exception))

    def test_overlay_does_not_disable_auth_no_overwrite_protection(self) -> None:
        # The overlay only reaches settings.json; it cannot reach auth handling.
        overlay = json.dumps({"overwriteAuth": True, "authFile": "/tmp/x"})
        with tempfile.TemporaryDirectory() as tmp, env(tmp, SCION_PI_SETTINGS=overlay):
            path = os.path.join(tmp, *AUTH)
            os.makedirs(os.path.dirname(path))
            with open(path, "w", encoding="utf-8") as f:
                f.write('{"keep": 1}')
            invoke(tmp, auth_secret='{"new": 1}')
            self.assertEqual(read_json(tmp, *AUTH), {"keep": 1})

    def test_provider_env(self) -> None:
        with tempfile.TemporaryDirectory() as tmp, env(tmp, SCION_PI_PROVIDER="openai"):
            invoke(tmp, env_vars=["OPENAI_API_KEY"])
            self.assertEqual(read_json(tmp, *SETTINGS)["defaultProvider"], "openai")

    def test_provider_env_invalid(self) -> None:
        with tempfile.TemporaryDirectory() as tmp, env(tmp, SCION_PI_PROVIDER="a b;rm"):
            with self.assertRaises(sh.ProvisionError):
                invoke(tmp, env_vars=["ANTHROPIC_API_KEY"])

    def test_concrete_model_sets_provider_and_model(self) -> None:
        with tempfile.TemporaryDirectory() as tmp, env(tmp, SCION_MODEL="anthropic/claude-sonnet-4-5:high"):
            invoke(tmp, env_vars=["ANTHROPIC_API_KEY"])
            s = read_json(tmp, *SETTINGS)
        self.assertEqual(s["defaultProvider"], "anthropic")
        self.assertEqual(s["defaultModel"], "claude-sonnet-4-5")

    def test_tier_alias_not_written(self) -> None:
        with tempfile.TemporaryDirectory() as tmp, env(tmp, SCION_MODEL="large"):
            invoke(tmp, env_vars=["ANTHROPIC_API_KEY"])
            s = read_json(tmp, *SETTINGS)
        self.assertNotIn("defaultModel", s)


class McpAndPromptTest(unittest.TestCase):
    def test_mcp_translation(self) -> None:
        servers = {
            "fs": {"transport": "stdio", "command": "npx", "args": ["-y", "x"], "env": {"A": "b"}},
            "web": {"transport": "streamable-http", "url": "https://example.invalid/mcp", "headers": {"H": "v"}},
            "old": {"transport": "sse", "url": "https://example.invalid/sse"},
            "bad name": {"transport": "stdio", "command": "x"},
        }
        with tempfile.TemporaryDirectory() as tmp, env(tmp):
            invoke(tmp, env_vars=["ANTHROPIC_API_KEY"], mcp_servers=servers)
            cfg = read_json(tmp, ".pi", "agent", "mcp.json")["mcpServers"]
        self.assertEqual(sorted(cfg), ["fs", "web"])
        self.assertEqual(cfg["fs"], {"command": "npx", "args": ["-y", "x"], "env": {"A": "b"}})
        self.assertEqual(cfg["web"], {"url": "https://example.invalid/mcp", "headers": {"H": "v"}})

    def test_system_prompt_written_to_append_file(self) -> None:
        with tempfile.TemporaryDirectory() as tmp, env(tmp):
            invoke(tmp, env_vars=["ANTHROPIC_API_KEY"], system_prompt="be careful\n")
            with open(os.path.join(tmp, ".pi", "agent", "APPEND_SYSTEM.md"), encoding="utf-8") as f:
                self.assertEqual(f.read(), "be careful\n")

    def test_no_system_prompt_no_file(self) -> None:
        with tempfile.TemporaryDirectory() as tmp, env(tmp):
            invoke(tmp, env_vars=["ANTHROPIC_API_KEY"])
            self.assertFalse(os.path.exists(os.path.join(tmp, ".pi", "agent", "APPEND_SYSTEM.md")))

    def test_instructions_written_to_agents_md(self) -> None:
        with tempfile.TemporaryDirectory() as tmp, env(tmp):
            invoke(tmp, env_vars=["ANTHROPIC_API_KEY"], instructions="use sciontool\n")
            with open(os.path.join(tmp, ".pi", "agent", "AGENTS.md"), encoding="utf-8") as f:
                self.assertIn("use sciontool", f.read())


class StaticBundleTest(unittest.TestCase):
    def _read(self, *rel: str) -> str:
        with open(os.path.join(HERE, *rel), encoding="utf-8") as f:
            return f.read()

    def test_dockerfile_pins_version_and_ignores_scripts(self) -> None:
        d = self._read("Dockerfile")
        self.assertRegex(d, r"ARG PI_VERSION=\d+\.\d+\.\d+\n")
        self.assertIn("--ignore-scripts", d)
        self.assertNotRegex(d, r"(?i)@latest|:latest|PI_VERSION=latest")
        self.assertIn("@earendil-works/pi-coding-agent@${PI_VERSION}", d)

    def test_config_declares_pi_and_safe_env(self) -> None:
        c = self._read("config.yaml")
        self.assertRegex(c, r"(?m)^harness: pi$")
        self.assertIn("PI_TELEMETRY", c)
        self.assertIn("PI_SKIP_VERSION_CHECK", c)
        self.assertIn("scion-pi:1.3.0", c)

    def test_dialect_events_match_bridge_emissions(self) -> None:
        d = self._read("dialect.yaml")
        b = self._read("home", ".pi", "agent", "extensions", "scion-bridge.js")
        emitted = set(re.findall(r"emit\('([a-z_]+)'", b))
        mapped = set(re.findall(r"(?m)^  ([a-z_]+):$", d))
        mapped -= {"fields"}
        self.assertEqual(emitted, mapped)

    def test_seed_settings_are_safe(self) -> None:
        s = json.loads(self._read("home", ".pi", "agent", "settings.json"))
        self.assertEqual(s["defaultProjectTrust"], "never")

    def test_no_secrets_or_internal_names_in_bundle(self) -> None:
        pat = re.compile(r"sk-[A-Za-z0-9]{16,}|ghp_[A-Za-z0-9]{20,}|BEGIN [A-Z ]*PRIVATE KEY", re.I)
        for root, _dirs, files in os.walk(HERE):
            for name in files:
                if name == "provision_test.py" or name.endswith(".pyc"):
                    continue
                with open(os.path.join(root, name), encoding="utf-8", errors="ignore") as f:
                    self.assertIsNone(pat.search(f.read()), os.path.join(root, name))


if __name__ == "__main__":
    unittest.main()
