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

"""Unit tests for the gemini-cli provisioner."""

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


def _select_vertex(env_vars: list[str]):
    with tempfile.TemporaryDirectory() as tmp:
        bundle = os.path.join(tmp, ".scion", "harness")
        os.makedirs(os.path.join(bundle, "inputs"))
        with open(os.path.join(bundle, "inputs", "auth-candidates.json"), "w", encoding="utf-8") as f:
            json.dump({"explicit_type": "vertex-ai", "env_vars": env_vars}, f)
        manifest = {"harness_bundle_dir": bundle, "harness_config": {}}
        ctx = scion_harness.ProvisionContext("gemini-cli", manifest)
        ctx.info = lambda *a, **k: None
        return ctx.select_auth(provision.AUTH)


class VertexAuthSelectionTest(unittest.TestCase):
    # config.yaml's vertex-ai requires a project and a region group; the
    # provisioner must agree (ptone/scion#4623).
    def test_project_and_each_region_key_select_vertex_ai(self) -> None:
        for region in ("GOOGLE_CLOUD_LOCATION", "GOOGLE_CLOUD_REGION", "CLOUD_ML_REGION"):
            with self.subTest(region=region):
                resolved = _select_vertex(["GOOGLE_CLOUD_PROJECT", region])
                self.assertEqual(resolved.method, "vertex-ai")

    def test_project_without_region_is_rejected(self) -> None:
        with self.assertRaises(scion_harness.ProvisionError):
            _select_vertex(["GOOGLE_CLOUD_PROJECT"])


class VertexEnvOverlayTest(unittest.TestCase):
    # Gemini reads only GOOGLE_CLOUD_LOCATION, so every accepted region key
    # must reach it (ptone/scion#4672). LOCATION wins over REGION, with a
    # warning when they differ (ptone/scion#4719).
    def _overlay(self, env: dict[str, str]) -> tuple[dict[str, str], list[str]]:
        warnings: list[str] = []
        with unittest.mock.patch.dict(os.environ, env, clear=True):
            overlay = provision._build_env_overlay("vertex-ai", "", warnings.append)
        return overlay, warnings

    def _location(self, env: dict[str, str]) -> str:
        return self._overlay(env)[0].get("GOOGLE_CLOUD_LOCATION", "")

    def test_cloud_ml_region_alone_sets_location(self) -> None:
        self.assertEqual(
            self._location({"GOOGLE_CLOUD_PROJECT": "p", "CLOUD_ML_REGION": "europe-west4"}),
            "europe-west4",
        )

    def test_only_region_sets_location(self) -> None:
        overlay, warnings = self._overlay({"GOOGLE_CLOUD_PROJECT": "p", "GOOGLE_CLOUD_REGION": "us-central1"})
        self.assertEqual(overlay["GOOGLE_CLOUD_LOCATION"], "us-central1")
        self.assertEqual(warnings, [])

    def test_only_location(self) -> None:
        overlay, warnings = self._overlay({"GOOGLE_CLOUD_PROJECT": "p", "GOOGLE_CLOUD_LOCATION": "us-east5"})
        self.assertEqual(overlay["GOOGLE_CLOUD_LOCATION"], "us-east5")
        self.assertNotIn("GOOGLE_CLOUD_REGION", overlay)
        self.assertEqual(warnings, [])

    def test_both_equal_no_warning(self) -> None:
        overlay, warnings = self._overlay({
            "GOOGLE_CLOUD_PROJECT": "p",
            "GOOGLE_CLOUD_LOCATION": "us-east5",
            "GOOGLE_CLOUD_REGION": "us-east5",
        })
        self.assertEqual(overlay["GOOGLE_CLOUD_LOCATION"], "us-east5")
        self.assertEqual(warnings, [])

    def test_both_differ_location_wins_with_warning(self) -> None:
        overlay, warnings = self._overlay({
            "GOOGLE_CLOUD_PROJECT": "p",
            "GOOGLE_CLOUD_LOCATION": "us-east5",
            "GOOGLE_CLOUD_REGION": "us-central1",
        })
        self.assertEqual(overlay["GOOGLE_CLOUD_LOCATION"], "us-east5")
        self.assertEqual(len(warnings), 1)
        self.assertIn("GOOGLE_CLOUD_LOCATION", warnings[0])
        self.assertIn("GOOGLE_CLOUD_REGION", warnings[0])

    def test_neither_omits_location(self) -> None:
        overlay, warnings = self._overlay({"GOOGLE_CLOUD_PROJECT": "p"})
        self.assertNotIn("GOOGLE_CLOUD_LOCATION", overlay)
        self.assertEqual(warnings, [])

    def test_empty_keys_are_omitted(self) -> None:
        overlay, _ = self._overlay({})
        self.assertEqual(overlay, {})
        overlay, _ = self._overlay({
            "GOOGLE_CLOUD_PROJECT": "",
            "GOOGLE_CLOUD_REGION": "",
            "GOOGLE_CLOUD_LOCATION": "us-east5",
        })
        self.assertEqual(overlay, {"GOOGLE_CLOUD_LOCATION": "us-east5"})

    def test_location_and_region_keep_precedence_over_cloud_ml_region(self) -> None:
        self.assertEqual(
            self._location({
                "GOOGLE_CLOUD_LOCATION": "us-east5",
                "GOOGLE_CLOUD_REGION": "us-central1",
                "CLOUD_ML_REGION": "europe-west4",
            }),
            "us-east5",
        )
        self.assertEqual(
            self._location({"GOOGLE_CLOUD_REGION": "us-central1", "CLOUD_ML_REGION": "europe-west4"}),
            "us-central1",
        )


class GeminiMCPProvisionTest(unittest.TestCase):
    """MCP server translation and ~/.gemini/settings.json merge tests."""

    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory()
        self.home = self._tmp.name
        self.bundle = os.path.join(self.home, ".scion", "harness")
        os.makedirs(os.path.join(self.bundle, "inputs"))
        self.settings_path = os.path.join(self.home, ".gemini", "settings.json")
        os.makedirs(os.path.dirname(self.settings_path))
        shutil.copy(IMAGE_SETTINGS, self.settings_path)
        patcher = unittest.mock.patch.dict(os.environ, {"HOME": self.home})
        patcher.start()
        self.addCleanup(patcher.stop)
        os.environ.pop("SCION_MODEL", None)

    def tearDown(self) -> None:
        self._tmp.cleanup()

    def _ctx(
        self,
        harness_config: dict[str, Any] | None = None,
    ) -> tuple[Any, list[str]]:
        ctx = scion_harness.ProvisionContext(
            "gemini-cli",
            {
                "harness_bundle_dir": self.bundle,
                "harness_config": harness_config if harness_config is not None else _harness_config(),
                # Never fall back to the real /workspace (ptone/scion#2993).
                "agent_workspace": os.path.join(self._tmp.name, "workspace"),
            },
        )
        warnings: list[str] = []
        ctx.info = lambda *a, **k: None
        ctx.warn = warnings.append
        return ctx, warnings

    def _stage_mcp(self, mcp_servers: dict[str, Any]) -> None:
        with open(
            os.path.join(self.bundle, "inputs", "mcp-servers.json"),
            "w",
            encoding="utf-8",
        ) as f:
            json.dump({"mcp_servers": mcp_servers}, f)

    def _settings(self) -> dict[str, Any]:
        with open(self.settings_path, encoding="utf-8") as f:
            return json.load(f)

    def _apply_mcp(self, mcp_servers: dict[str, Any] | None) -> tuple[int, list[str]]:
        if mcp_servers is not None:
            self._stage_mcp(mcp_servers)
        ctx, warnings = self._ctx()
        count = scion_harness.apply_mcp_translated(
            ctx,
            lambda name, spec: provision._translate_mcp_server(name, spec, ctx.warn),
            provision._write_mcp_servers,
        )
        return count, warnings

    # --- transport translation -----------------------------------------

    def test_stdio_translation_with_args_and_env(self) -> None:
        warnings: list[str] = []
        entry = provision._translate_mcp_server(
            "fs",
            {
                "transport": "stdio",
                "command": "npx",
                "args": ["-y", "@modelcontextprotocol/server-filesystem", "/workspace"],
                "env": {"DEBUG": "1", "PORT": 8080},
                "scope": "global",
            },
            warnings.append,
        )
        self.assertEqual(
            entry,
            {
                "command": "npx",
                "args": ["-y", "@modelcontextprotocol/server-filesystem", "/workspace"],
                "env": {"DEBUG": "1", "PORT": "8080"},
            },
        )
        self.assertNotIn("type", entry)
        self.assertNotIn("scope", entry)
        self.assertEqual(warnings, [])

    def test_stdio_translation_omits_empty_args_and_env(self) -> None:
        warnings: list[str] = []
        entry = provision._translate_mcp_server(
            "minimal",
            {
                "transport": "stdio",
                "command": "/usr/local/bin/my-mcp",
                "args": [],
                "env": {},
            },
            warnings.append,
        )
        self.assertEqual(entry, {"command": "/usr/local/bin/my-mcp"})
        self.assertNotIn("args", entry)
        self.assertNotIn("env", entry)
        self.assertEqual(warnings, [])

    def test_sse_translation_with_and_without_headers(self) -> None:
        warnings: list[str] = []
        with_headers = provision._translate_mcp_server(
            "events",
            {
                "transport": "sse",
                "url": "https://mcp.example.com/sse",
                "headers": {"Authorization": "Bearer tok"},
            },
            warnings.append,
        )
        self.assertEqual(
            with_headers,
            {
                "url": "https://mcp.example.com/sse",
                "headers": {"Authorization": "Bearer tok"},
            },
        )
        self.assertNotIn("httpUrl", with_headers)
        self.assertNotIn("type", with_headers)

        without_headers = provision._translate_mcp_server(
            "events-plain",
            {
                "transport": "sse",
                "url": "https://mcp.example.com/sse",
                "headers": {},
            },
            warnings.append,
        )
        self.assertEqual(without_headers, {"url": "https://mcp.example.com/sse"})
        self.assertNotIn("headers", without_headers)
        self.assertEqual(warnings, [])

    def test_streamable_http_uses_http_url_not_url(self) -> None:
        warnings: list[str] = []
        with_headers = provision._translate_mcp_server(
            "api",
            {
                "transport": "streamable-http",
                "url": "https://mcp.example.com/mcp",
                "headers": {"X-API-Key": "secret"},
            },
            warnings.append,
        )
        self.assertEqual(
            with_headers,
            {
                "httpUrl": "https://mcp.example.com/mcp",
                "headers": {"X-API-Key": "secret"},
            },
        )
        self.assertNotIn("url", with_headers)
        self.assertNotIn("type", with_headers)

        without_headers = provision._translate_mcp_server(
            "api-plain",
            {
                "transport": "streamable-http",
                "url": "https://mcp.example.com/mcp",
                "headers": {},
            },
            warnings.append,
        )
        self.assertEqual(without_headers, {"httpUrl": "https://mcp.example.com/mcp"})
        self.assertNotIn("url", without_headers)
        self.assertNotIn("headers", without_headers)
        self.assertEqual(warnings, [])

    def test_translation_strips_whitespace_from_command_and_url_only(self) -> None:
        warnings: list[str] = []
        stdio_entry = provision._translate_mcp_server(
            "padded-stdio",
            {
                "transport": "stdio",
                "command": "  npx  ",
                "args": [" keep-leading ", "keep-trailing "],
                "env": {"KEY": " padded-val "},
            },
            warnings.append,
        )
        self.assertEqual(
            stdio_entry,
            {
                "command": "npx",
                "args": [" keep-leading ", "keep-trailing "],
                "env": {"KEY": " padded-val "},
            },
        )

        sse_entry = provision._translate_mcp_server(
            "padded-sse",
            {
                "transport": "sse",
                "url": "  https://mcp.example.com/sse \n",
                "headers": {"X-Header": " keep "},
            },
            warnings.append,
        )
        self.assertEqual(
            sse_entry,
            {
                "url": "https://mcp.example.com/sse",
                "headers": {"X-Header": " keep "},
            },
        )

        http_entry = provision._translate_mcp_server(
            "padded-http",
            {
                "transport": "streamable-http",
                "url": "\thttps://mcp.example.com/mcp  ",
                "headers": {"Authorization": " Bearer tok "},
            },
            warnings.append,
        )
        self.assertEqual(
            http_entry,
            {
                "httpUrl": "https://mcp.example.com/mcp",
                "headers": {"Authorization": " Bearer tok "},
            },
        )
        self.assertEqual(warnings, [])

    # --- scope demotion & settings merge -------------------------------

    def test_project_scope_is_demoted_to_global_settings(self) -> None:
        count, warnings = self._apply_mcp(
            {
                "proj-srv": {
                    "transport": "stdio",
                    "command": "python3",
                    "args": ["-m", "my_mcp"],
                    "scope": "project",
                }
            }
        )
        self.assertEqual(count, 1)
        self.assertEqual(warnings, [])
        settings = self._settings()
        self.assertEqual(
            settings["mcpServers"]["proj-srv"],
            {"command": "python3", "args": ["-m", "my_mcp"]},
        )
        self.assertFalse(
            os.path.exists(os.path.join(self._tmp.name, "workspace", ".gemini", "settings.json"))
        )

    def test_merge_preserves_existing_settings_and_user_mcp_servers(self) -> None:
        existing = self._settings()
        existing["security"]["auth"]["selectedType"] = "gemini-api-key"
        existing["model"]["name"] = "gemini-3.6-flash"
        existing["mcpServers"] = {
            "user-local": {"command": "/opt/user-mcp", "args": ["--flag"]},
            "shared": {"command": "old-cmd"},
        }
        scion_harness.atomic_write_json(self.settings_path, existing)

        count, warnings = self._apply_mcp(
            {
                "shared": {"transport": "stdio", "command": "new-cmd"},
                "remote-sse": {"transport": "sse", "url": "https://sse.example.com/events"},
                "remote-http": {
                    "transport": "streamable-http",
                    "url": "https://http.example.com/mcp",
                    "headers": {"X-Token": "abc"},
                },
            }
        )
        self.assertEqual(count, 3)
        self.assertEqual(warnings, [])

        merged = self._settings()
        # Top-level settings preserved
        self.assertEqual(merged["security"]["auth"]["selectedType"], "gemini-api-key")
        self.assertEqual(merged["telemetry"], existing["telemetry"])
        self.assertEqual(merged["model"]["name"], "gemini-3.6-flash")
        self.assertIs(merged["model"]["skipNextSpeakerCheck"], True)
        self.assertEqual(merged["hooks"], existing["hooks"])
        self.assertEqual(merged["tools"], existing["tools"])
        self.assertEqual(merged["experimental"], existing["experimental"])

        # Existing user entry preserved, same-named entry overridden, new entries added
        self.assertEqual(
            merged["mcpServers"],
            {
                "user-local": {"command": "/opt/user-mcp", "args": ["--flag"]},
                "shared": {"command": "new-cmd"},
                "remote-sse": {"url": "https://sse.example.com/events"},
                "remote-http": {
                    "httpUrl": "https://http.example.com/mcp",
                    "headers": {"X-Token": "abc"},
                },
            },
        )

    def test_write_creates_missing_settings_file_and_recovers_non_dict_mcp_servers(self) -> None:
        # Missing settings file and parent directory
        shutil.rmtree(os.path.dirname(self.settings_path))
        provision._write_mcp_servers({"s1": {"command": "echo"}})
        self.assertEqual(self._settings(), {"mcpServers": {"s1": {"command": "echo"}}})

        # Non-dict mcpServers field inside a valid settings object
        scion_harness.atomic_write_json(
            self.settings_path,
            {"yolo": True, "mcpServers": "corrupt"},
        )
        provision._write_mcp_servers({"s3": {"httpUrl": "https://example.com/mcp"}})
        self.assertEqual(
            self._settings(),
            {"yolo": True, "mcpServers": {"s3": {"httpUrl": "https://example.com/mcp"}}},
        )

    def test_fail_closed_on_malformed_non_utf8_and_non_dict_settings_json(self) -> None:
        cases = (
            ("malformed_json", b'// jsonc comment\n{"hooks": {"SessionStart": []}}\n'),
            ("non_utf8_bytes", b"\xff\xfe\x00invalid-utf8"),
            ("json_array", b'["not", "an", "object"]\n'),
            ("json_null", b"null\n"),
        )
        for label, raw_bytes in cases:
            with self.subTest(case=label):
                with open(self.settings_path, "wb") as f:
                    f.write(raw_bytes)
                count, warnings = self._apply_mcp(
                    {"s1": {"transport": "stdio", "command": "echo"}}
                )
                self.assertEqual(count, 0)
                self.assertEqual(len(warnings), 1)
                self.assertIn("failed to write MCP config", warnings[0])
                with open(self.settings_path, "rb") as f:
                    self.assertEqual(f.read(), raw_bytes)

    # --- validation warnings & skip behavior ---------------------------

    def test_invalid_entries_warn_and_skip_while_valid_entries_write(self) -> None:
        count, warnings = self._apply_mcp(
            {
                "bad-stdio-missing": {"transport": "stdio"},
                "bad-stdio-empty": {"transport": "stdio", "command": ""},
                "bad-stdio-type": {"transport": "stdio", "command": ["npx"]},
                "bad-sse-missing": {"transport": "sse"},
                "bad-sse-type": {"transport": "sse", "url": 123},
                "bad-http-missing": {"transport": "streamable-http", "url": ""},
                "bad-http-type": {"transport": "streamable-http", "url": None},
                "bad-transport": {"transport": "grpc", "url": "grpc://example.com"},
                "missing-transport": {"command": "npx"},
                "good-stdio": {"transport": "stdio", "command": "npx", "args": ["-y", "srv"]},
                "good-http": {"transport": "streamable-http", "url": "https://example.com/mcp"},
            }
        )
        self.assertEqual(count, 2)
        self.assertEqual(len(warnings), 9)
        joined = "\n".join(warnings)
        for bad_name in (
            "bad-stdio-missing",
            "bad-stdio-empty",
            "bad-stdio-type",
            "bad-sse-missing",
            "bad-sse-type",
            "bad-http-missing",
            "bad-http-type",
            "bad-transport",
            "missing-transport",
        ):
            self.assertIn(bad_name, joined)

        mcp_servers = self._settings()["mcpServers"]
        self.assertEqual(
            mcp_servers,
            {
                "good-stdio": {"command": "npx", "args": ["-y", "srv"]},
                "good-http": {"httpUrl": "https://example.com/mcp"},
            },
        )

    # --- absent or empty inputs/mcp-servers.json -----------------------

    def test_absent_mcp_servers_file_leaves_settings_untouched(self) -> None:
        with open(self.settings_path, encoding="utf-8") as f:
            before = f.read()
        count, warnings = self._apply_mcp(None)
        self.assertEqual(count, 0)
        self.assertEqual(warnings, [])
        with open(self.settings_path, encoding="utf-8") as f:
            after = f.read()
        self.assertEqual(after, before)

    def test_empty_mcp_servers_map_leaves_settings_untouched(self) -> None:
        with open(self.settings_path, encoding="utf-8") as f:
            before = f.read()
        count, warnings = self._apply_mcp({})
        self.assertEqual(count, 0)
        self.assertEqual(warnings, [])
        with open(self.settings_path, encoding="utf-8") as f:
            after = f.read()
        self.assertEqual(after, before)

    # --- end-to-end provision(ctx) -------------------------------------

    def test_provision_end_to_end_writes_mcp_alongside_auth_telemetry_and_model(self) -> None:
        with open(
            os.path.join(self.bundle, "inputs", "auth-candidates.json"),
            "w",
            encoding="utf-8",
        ) as f:
            json.dump({"env_vars": ["GEMINI_API_KEY"]}, f)
        self._stage_mcp(
            {
                "local-tool": {
                    "transport": "stdio",
                    "command": "node",
                    "args": ["server.js"],
                    "env": {"NODE_ENV": "production"},
                },
                "sse-tool": {
                    "transport": "sse",
                    "url": "https://mcp.example.com/sse",
                },
                "http-tool": {
                    "transport": "streamable-http",
                    "url": "https://mcp.example.com/mcp",
                    "headers": {"Authorization": "Bearer xyz"},
                    "scope": "project",
                },
            }
        )

        ctx, warnings = self._ctx()
        with unittest.mock.patch.dict(os.environ, {"GEMINI_API_KEY": "test-key"}):
            provision.provision(ctx)

        self.assertEqual(warnings, [])
        settings = self._settings()
        self.assertEqual(settings["security"]["auth"]["selectedType"], "gemini-api-key")
        self.assertIn("telemetry", settings)
        self.assertEqual(settings["telemetry"]["target"], "local")
        self.assertEqual(settings["model"]["name"], "gemini-3.6-flash")
        self.assertIs(settings["model"]["skipNextSpeakerCheck"], True)
        self.assertEqual(
            settings["mcpServers"],
            {
                "local-tool": {
                    "command": "node",
                    "args": ["server.js"],
                    "env": {"NODE_ENV": "production"},
                },
                "sse-tool": {
                    "url": "https://mcp.example.com/sse",
                },
                "http-tool": {
                    "httpUrl": "https://mcp.example.com/mcp",
                    "headers": {"Authorization": "Bearer xyz"},
                },
            },
        )
        self.assertNotIn("url", settings["mcpServers"]["http-tool"])

    def test_provision_end_to_end_warns_via_ctx_and_preserves_existing_mcp(self) -> None:
        existing = self._settings()
        existing["mcpServers"] = {"preexisting": {"command": "/opt/pre"}}
        scion_harness.atomic_write_json(self.settings_path, existing)

        with open(
            os.path.join(self.bundle, "inputs", "auth-candidates.json"),
            "w",
            encoding="utf-8",
        ) as f:
            json.dump({"env_vars": ["GEMINI_API_KEY"]}, f)
        self._stage_mcp(
            {
                "broken": {"transport": "streamable-http"},
                "valid": {"transport": "stdio", "command": "npx"},
            }
        )

        ctx, warnings = self._ctx()
        with unittest.mock.patch.dict(os.environ, {"GEMINI_API_KEY": "test-key"}):
            provision.provision(ctx)

        self.assertEqual(len(warnings), 1)
        self.assertIn("broken", warnings[0])
        self.assertEqual(
            self._settings()["mcpServers"],
            {
                "preexisting": {"command": "/opt/pre"},
                "valid": {"command": "npx"},
            },
        )

    # --- config.yaml capabilities.mcp consistency ----------------------

    def test_config_yaml_declares_mcp_capabilities(self) -> None:
        with open(CONFIG_YAML, encoding="utf-8") as f:
            text = f.read()
        self.assertIn("\n  mcp:\n", text)
        self.assertIn('\n    stdio: { support: "yes" }\n', text)
        self.assertIn('\n    sse: { support: "yes" }\n', text)
        self.assertIn('\n    streamable_http: { support: "yes" }\n', text)
        self.assertIn('\n    project_scope: { support: "no", reason:', text)


if __name__ == "__main__":
    unittest.main()
