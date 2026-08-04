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
"""Unit tests for the Copilot harness provisioner.

Run with:  python3 -m unittest provision_test -v
"""

from __future__ import annotations

import importlib.util
import os
import tempfile
import unittest
from contextlib import contextmanager

PROVISION_PATH = os.path.join(os.path.dirname(__file__), "provision.py")
SPEC = importlib.util.spec_from_file_location("copilot_provision", PROVISION_PATH)
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


class BaseTelemetryTest(unittest.TestCase):
    """Base class that isolates tests from host SCION_/OTEL_ env vars."""

    _saved_env: dict[str, str]

    def setUp(self) -> None:
        super().setUp()
        self._saved_env = {}
        for key in list(os.environ):
            if key.startswith(("SCION_", "OTEL_")):
                self._saved_env[key] = os.environ.pop(key)

    def tearDown(self) -> None:
        # Remove any SCION_/OTEL_ vars that tests may have set.
        for key in list(os.environ):
            if key.startswith(("SCION_", "OTEL_")):
                os.environ.pop(key, None)
        # Restore original env vars.
        os.environ.update(self._saved_env)
        super().tearDown()


class TelemetryEnabledTest(unittest.TestCase):
    """Tests for the _telemetry_enabled helper."""

    def test_none_returns_false(self) -> None:
        self.assertFalse(provision._telemetry_enabled(None))

    def test_empty_dict_returns_false(self) -> None:
        self.assertFalse(provision._telemetry_enabled({}))

    def test_enabled_true(self) -> None:
        self.assertTrue(provision._telemetry_enabled({"enabled": True}))

    def test_enabled_none_defaults_true(self) -> None:
        self.assertTrue(provision._telemetry_enabled({"enabled": None}))

    def test_enabled_false(self) -> None:
        self.assertFalse(provision._telemetry_enabled({"enabled": False}))


class BuildTelemetryEnvTest(BaseTelemetryTest):
    """Tests for _build_telemetry_env."""

    def test_defaults_point_to_local_grpc_receiver(self) -> None:
        env = provision._build_telemetry_env({"enabled": True}, None)
        self.assertEqual(env["COPILOT_TELEMETRY_ENABLED"], "true")
        self.assertEqual(env["OTEL_EXPORTER_OTLP_ENDPOINT"], "http://localhost:4317")
        self.assertEqual(env["OTEL_EXPORTER_OTLP_PROTOCOL"], "grpc")
        self.assertEqual(env["OTEL_METRICS_EXPORTER"], "otlp")
        self.assertEqual(env["OTEL_LOGS_EXPORTER"], "otlp")
        self.assertEqual(env["OTEL_METRIC_EXPORT_INTERVAL"], "30000")

    def test_cloud_endpoint_override(self) -> None:
        telemetry = {
            "enabled": True,
            "cloud": {
                "endpoint": "https://otel.example.com:4317",
                "protocol": "http",
            },
        }
        env = provision._build_telemetry_env(telemetry, None)
        self.assertEqual(env["OTEL_EXPORTER_OTLP_ENDPOINT"], "https://otel.example.com:4317")
        self.assertEqual(env["OTEL_EXPORTER_OTLP_PROTOCOL"], "http")

    def test_env_override_takes_precedence(self) -> None:
        telemetry = {
            "enabled": True,
            "cloud": {
                "endpoint": "http://cloud-collector:4317",
                "protocol": "grpc",
            },
        }
        env_overlay = {
            "SCION_COPILOT_OTEL_ENDPOINT": "http://custom-collector:4317",
            "SCION_COPILOT_OTEL_PROTOCOL": "http",
        }
        env = provision._build_telemetry_env(telemetry, env_overlay)
        self.assertEqual(env["OTEL_EXPORTER_OTLP_ENDPOINT"], "http://custom-collector:4317")
        self.assertEqual(env["OTEL_EXPORTER_OTLP_PROTOCOL"], "http")

    def test_scion_otel_endpoint_fallback(self) -> None:
        env_overlay = {"SCION_OTEL_ENDPOINT": "http://scion-collector:4317"}
        env = provision._build_telemetry_env({"enabled": True}, env_overlay)
        self.assertEqual(env["OTEL_EXPORTER_OTLP_ENDPOINT"], "http://scion-collector:4317")

    def test_headers_propagated_and_percent_encoded(self) -> None:
        telemetry = {
            "enabled": True,
            "cloud": {
                "headers": {"authorization": "Bearer tok", "x-meta": "val"},
            },
        }
        env = provision._build_telemetry_env(telemetry, None)
        self.assertIn("OTEL_EXPORTER_OTLP_HEADERS", env)
        # Values must be percent-encoded per the OTel SDK spec.
        # "Bearer tok" → "Bearer%20tok", "val" → "val" (no special chars).
        self.assertEqual(
            env["OTEL_EXPORTER_OTLP_HEADERS"],
            "authorization=Bearer%20tok,x-meta=val",
        )

    def test_tls_ca_file_propagated(self) -> None:
        telemetry = {
            "enabled": True,
            "cloud": {
                "tls": {"ca_file": "/etc/scion/ca.pem"},
            },
        }
        env = provision._build_telemetry_env(telemetry, None)
        self.assertEqual(env["OTEL_EXPORTER_OTLP_CERTIFICATE"], "/etc/scion/ca.pem")

    def test_no_headers_when_absent(self) -> None:
        env = provision._build_telemetry_env({"enabled": True}, None)
        self.assertNotIn("OTEL_EXPORTER_OTLP_HEADERS", env)
        self.assertNotIn("OTEL_EXPORTER_OTLP_CERTIFICATE", env)


class ResolveEndpointTest(BaseTelemetryTest):
    """Tests for _resolve_endpoint."""

    def test_default(self) -> None:
        self.assertEqual(provision._resolve_endpoint(None, None), "http://localhost:4317")

    def test_cloud_config(self) -> None:
        telemetry = {"cloud": {"endpoint": "https://collector:443"}}
        self.assertEqual(provision._resolve_endpoint(telemetry, None), "https://collector:443")

    def test_copilot_env_override_wins(self) -> None:
        telemetry = {"cloud": {"endpoint": "https://collector:443"}}
        env = {"SCION_COPILOT_OTEL_ENDPOINT": "http://custom:4317"}
        self.assertEqual(provision._resolve_endpoint(telemetry, env), "http://custom:4317")

    def test_copilot_env_takes_precedence_over_scion_env(self) -> None:
        env = {
            "SCION_COPILOT_OTEL_ENDPOINT": "http://copilot-specific:4317",
            "SCION_OTEL_ENDPOINT": "http://generic-scion:4317",
        }
        self.assertEqual(provision._resolve_endpoint(None, env), "http://copilot-specific:4317")

    def test_scion_env_fallback(self) -> None:
        env = {"SCION_OTEL_ENDPOINT": "http://scion:4317"}
        self.assertEqual(provision._resolve_endpoint(None, env), "http://scion:4317")


class ResolveProtocolTest(BaseTelemetryTest):
    """Tests for _resolve_protocol."""

    def test_default(self) -> None:
        self.assertEqual(provision._resolve_protocol(None, None), "grpc")

    def test_cloud_config(self) -> None:
        telemetry = {"cloud": {"protocol": "http"}}
        self.assertEqual(provision._resolve_protocol(telemetry, None), "http")

    def test_copilot_env_override_wins(self) -> None:
        telemetry = {"cloud": {"protocol": "http"}}
        env = {"SCION_COPILOT_OTEL_PROTOCOL": "grpc"}
        self.assertEqual(provision._resolve_protocol(telemetry, env), "grpc")

    def test_scion_env_fallback(self) -> None:
        env = {"SCION_OTEL_PROTOCOL": "http"}
        self.assertEqual(provision._resolve_protocol(None, env), "http")


class ResolveEndpointOsEnvTest(BaseTelemetryTest):
    """Tests for _resolve_endpoint os.environ fallback."""

    def test_os_environ_fallback(self) -> None:
        os.environ["SCION_OTEL_ENDPOINT"] = "http://from-os-env:4317"
        self.assertEqual(
            provision._resolve_endpoint(None, {}), "http://from-os-env:4317"
        )

    def test_copilot_os_environ_takes_precedence(self) -> None:
        os.environ["SCION_COPILOT_OTEL_ENDPOINT"] = "http://copilot-os:4317"
        os.environ["SCION_OTEL_ENDPOINT"] = "http://generic-os:4317"
        self.assertEqual(
            provision._resolve_endpoint(None, {}), "http://copilot-os:4317"
        )

    def test_env_overlay_beats_os_environ(self) -> None:
        os.environ["SCION_COPILOT_OTEL_ENDPOINT"] = "http://from-os-env:4317"
        env = {"SCION_COPILOT_OTEL_ENDPOINT": "http://from-overlay:4317"}
        self.assertEqual(
            provision._resolve_endpoint(None, env), "http://from-overlay:4317"
        )


class ResolveProtocolOsEnvTest(BaseTelemetryTest):
    """Tests for _resolve_protocol os.environ fallback."""

    def test_os_environ_fallback(self) -> None:
        os.environ["SCION_OTEL_PROTOCOL"] = "http"
        self.assertEqual(provision._resolve_protocol(None, {}), "http")

    def test_copilot_os_environ_takes_precedence(self) -> None:
        os.environ["SCION_COPILOT_OTEL_PROTOCOL"] = "grpc"
        os.environ["SCION_OTEL_PROTOCOL"] = "http"
        self.assertEqual(provision._resolve_protocol(None, {}), "grpc")

    def test_env_overlay_beats_os_environ(self) -> None:
        os.environ["SCION_COPILOT_OTEL_PROTOCOL"] = "http"
        env = {"SCION_COPILOT_OTEL_PROTOCOL": "grpc"}
        self.assertEqual(provision._resolve_protocol(None, env), "grpc")


class HeadersEnvTest(BaseTelemetryTest):
    """Tests for headers resolution from env vars in _build_telemetry_env."""

    def test_headers_from_env_overlay(self) -> None:
        import json as _json

        env_overlay = {
            "SCION_OTEL_HEADERS": _json.dumps({"x-api-key": "secret123"}),
        }
        env = provision._build_telemetry_env({"enabled": True}, env_overlay)
        self.assertEqual(env["OTEL_EXPORTER_OTLP_HEADERS"], "x-api-key=secret123")

    def test_headers_from_os_environ(self) -> None:
        import json as _json

        os.environ["SCION_OTEL_HEADERS"] = _json.dumps(
            {"authorization": "Bearer tok"}
        )
        env = provision._build_telemetry_env({"enabled": True}, {})
        self.assertEqual(
            env["OTEL_EXPORTER_OTLP_HEADERS"], "authorization=Bearer%20tok"
        )

    def test_copilot_headers_env_takes_precedence(self) -> None:
        import json as _json

        env_overlay = {
            "SCION_COPILOT_OTEL_HEADERS": _json.dumps({"x-copilot": "1"}),
            "SCION_OTEL_HEADERS": _json.dumps({"x-generic": "2"}),
        }
        env = provision._build_telemetry_env({"enabled": True}, env_overlay)
        self.assertEqual(env["OTEL_EXPORTER_OTLP_HEADERS"], "x-copilot=1")

    def test_headers_env_beats_cloud_config(self) -> None:
        import json as _json

        telemetry = {
            "enabled": True,
            "cloud": {"headers": {"x-cloud": "from-config"}},
        }
        env_overlay = {
            "SCION_OTEL_HEADERS": _json.dumps({"x-env": "from-env"}),
        }
        env = provision._build_telemetry_env(telemetry, env_overlay)
        self.assertEqual(env["OTEL_EXPORTER_OTLP_HEADERS"], "x-env=from-env")

    def test_invalid_json_falls_back_to_cloud(self) -> None:
        telemetry = {
            "enabled": True,
            "cloud": {"headers": {"x-cloud": "val"}},
        }
        env_overlay = {"SCION_OTEL_HEADERS": "not-json"}
        env = provision._build_telemetry_env(telemetry, env_overlay)
        self.assertEqual(env["OTEL_EXPORTER_OTLP_HEADERS"], "x-cloud=val")


class CaFileEnvTest(BaseTelemetryTest):
    """Tests for TLS CA file resolution from env vars in _build_telemetry_env."""

    def test_ca_file_from_env_overlay(self) -> None:
        env_overlay = {"SCION_OTEL_CA_FILE": "/custom/ca.pem"}
        env = provision._build_telemetry_env({"enabled": True}, env_overlay)
        self.assertEqual(env["OTEL_EXPORTER_OTLP_CERTIFICATE"], "/custom/ca.pem")

    def test_ca_file_from_os_environ(self) -> None:
        os.environ["SCION_OTEL_CA_FILE"] = "/os-env/ca.pem"
        env = provision._build_telemetry_env({"enabled": True}, {})
        self.assertEqual(env["OTEL_EXPORTER_OTLP_CERTIFICATE"], "/os-env/ca.pem")

    def test_copilot_ca_file_takes_precedence(self) -> None:
        env_overlay = {
            "SCION_COPILOT_OTEL_CA_FILE": "/copilot/ca.pem",
            "SCION_OTEL_CA_FILE": "/generic/ca.pem",
        }
        env = provision._build_telemetry_env({"enabled": True}, env_overlay)
        self.assertEqual(env["OTEL_EXPORTER_OTLP_CERTIFICATE"], "/copilot/ca.pem")

    def test_ca_file_env_beats_cloud_config(self) -> None:
        telemetry = {
            "enabled": True,
            "cloud": {"tls": {"ca_file": "/cloud/ca.pem"}},
        }
        env_overlay = {"SCION_OTEL_CA_FILE": "/env/ca.pem"}
        env = provision._build_telemetry_env(telemetry, env_overlay)
        self.assertEqual(env["OTEL_EXPORTER_OTLP_CERTIFICATE"], "/env/ca.pem")

    def test_no_ca_file_when_absent(self) -> None:
        env = provision._build_telemetry_env({"enabled": True}, {})
        self.assertNotIn("OTEL_EXPORTER_OTLP_CERTIFICATE", env)


if __name__ == "__main__":
    unittest.main()
