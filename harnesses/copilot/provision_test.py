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

    def test_defaults_point_to_local_http_receiver(self) -> None:
        env = provision._build_telemetry_env(None)
        self.assertEqual(env["COPILOT_OTEL_ENABLED"], "true")
        self.assertEqual(env["COPILOT_OTEL_EXPORTER_TYPE"], "otlp-http")
        self.assertEqual(env["OTEL_EXPORTER_OTLP_ENDPOINT"], "http://127.0.0.1:4318")
        self.assertEqual(env["OTEL_EXPORTER_OTLP_PROTOCOL"], "http/protobuf")
        self.assertEqual(env["OTEL_METRICS_EXPORTER"], "otlp")
        self.assertEqual(env["OTEL_LOGS_EXPORTER"], "otlp")
        self.assertEqual(env["OTEL_METRIC_EXPORT_INTERVAL"], "30000")
        self.assertEqual(env["OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE"], "delta")

    def test_custom_port(self) -> None:
        env = provision._build_telemetry_env({"SCION_OTEL_HTTP_PORT": "14318"})
        self.assertEqual(env["OTEL_EXPORTER_OTLP_ENDPOINT"], "http://127.0.0.1:14318")

    def test_cloud_endpoint_is_never_used(self) -> None:
        """No cloud endpoint, provider or protocol config exists anymore --
        _build_telemetry_env only ever sees an env overlay, never the
        telemetry/cloud config dict, and always resolves to the local
        receiver."""
        env = provision._build_telemetry_env(None)
        self.assertEqual(env["OTEL_EXPORTER_OTLP_ENDPOINT"], "http://127.0.0.1:4318")

    def test_generic_scion_otel_endpoint_is_not_honored(self) -> None:
        """The generic SCION_OTEL_ENDPOINT cloud-config alias must no longer
        redirect native telemetry away from the local receiver (#2053)."""
        env_overlay = {"SCION_OTEL_ENDPOINT": "https://cloudtrace.googleapis.com:443"}
        env = provision._build_telemetry_env(env_overlay)
        self.assertEqual(env["OTEL_EXPORTER_OTLP_ENDPOINT"], "http://127.0.0.1:4318")

    def test_no_headers_or_ca_ever_set(self) -> None:
        """Headers and CA are never copied into the harness env; there is no
        cloud config left to copy them from."""
        env = provision._build_telemetry_env(None)
        self.assertNotIn("OTEL_EXPORTER_OTLP_HEADERS", env)
        self.assertNotIn("OTEL_EXPORTER_OTLP_CERTIFICATE", env)

    def test_debug_endpoint_override(self) -> None:
        env_overlay = {"SCION_COPILOT_OTEL_ENDPOINT": "http://debug-collector:4318"}
        env = provision._build_telemetry_env(env_overlay)
        self.assertEqual(env["OTEL_EXPORTER_OTLP_ENDPOINT"], "http://debug-collector:4318")

    def test_debug_protocol_override(self) -> None:
        env_overlay = {"SCION_COPILOT_OTEL_PROTOCOL": "http/json"}
        env = provision._build_telemetry_env(env_overlay)
        self.assertEqual(env["OTEL_EXPORTER_OTLP_PROTOCOL"], "http/json")


class ResolveEndpointTest(BaseTelemetryTest):
    """Tests for _resolve_endpoint."""

    def test_default(self) -> None:
        self.assertEqual(provision._resolve_endpoint(None), "http://127.0.0.1:4318")

    def test_custom_port(self) -> None:
        self.assertEqual(
            provision._resolve_endpoint({"SCION_OTEL_HTTP_PORT": "9999"}),
            "http://127.0.0.1:9999",
        )

    def test_invalid_port_raises(self) -> None:
        with self.assertRaises(provision.scion_harness.ProvisionError):
            provision._resolve_endpoint({"SCION_OTEL_HTTP_PORT": "not-a-port"})

    def test_debug_override_wins(self) -> None:
        env = {"SCION_COPILOT_OTEL_ENDPOINT": "http://custom:4318"}
        self.assertEqual(provision._resolve_endpoint(env), "http://custom:4318")

    def test_generic_scion_otel_endpoint_ignored(self) -> None:
        """Only the copilot-specific override is honored; the generic cloud
        alias is not (that alias is exactly what caused the #2053 bypass)."""
        env = {"SCION_OTEL_ENDPOINT": "https://cloudtrace.googleapis.com:443"}
        self.assertEqual(provision._resolve_endpoint(env), "http://127.0.0.1:4318")


class ResolveProtocolTest(BaseTelemetryTest):
    """Tests for _resolve_protocol."""

    def test_default(self) -> None:
        self.assertEqual(provision._resolve_protocol(None), "http/protobuf")

    def test_debug_override_wins(self) -> None:
        env = {"SCION_COPILOT_OTEL_PROTOCOL": "http/json"}
        self.assertEqual(provision._resolve_protocol(env), "http/json")

    def test_generic_scion_otel_protocol_ignored(self) -> None:
        env = {"SCION_OTEL_PROTOCOL": "grpc"}
        self.assertEqual(provision._resolve_protocol(env), "http/protobuf")


class ResolveEndpointOsEnvTest(BaseTelemetryTest):
    """Tests for _resolve_endpoint os.environ fallback."""

    def test_os_environ_debug_override(self) -> None:
        os.environ["SCION_COPILOT_OTEL_ENDPOINT"] = "http://copilot-os:4318"
        self.assertEqual(provision._resolve_endpoint({}), "http://copilot-os:4318")

    def test_generic_scion_otel_endpoint_os_environ_ignored(self) -> None:
        os.environ["SCION_OTEL_ENDPOINT"] = "https://cloudtrace.googleapis.com:443"
        self.assertEqual(provision._resolve_endpoint({}), "http://127.0.0.1:4318")

    def test_env_overlay_beats_os_environ(self) -> None:
        os.environ["SCION_COPILOT_OTEL_ENDPOINT"] = "http://from-os-env:4318"
        env = {"SCION_COPILOT_OTEL_ENDPOINT": "http://from-overlay:4318"}
        self.assertEqual(provision._resolve_endpoint(env), "http://from-overlay:4318")


class ResolveProtocolOsEnvTest(BaseTelemetryTest):
    """Tests for _resolve_protocol os.environ fallback."""

    def test_os_environ_debug_override(self) -> None:
        os.environ["SCION_COPILOT_OTEL_PROTOCOL"] = "http/json"
        self.assertEqual(provision._resolve_protocol({}), "http/json")

    def test_env_overlay_beats_os_environ(self) -> None:
        os.environ["SCION_COPILOT_OTEL_PROTOCOL"] = "http/json"
        env = {"SCION_COPILOT_OTEL_PROTOCOL": "grpc"}
        self.assertEqual(provision._resolve_protocol(env), "grpc")


if __name__ == "__main__":
    unittest.main()
