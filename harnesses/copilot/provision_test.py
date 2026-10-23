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
import io
import json
import os
import re
import tempfile
import unittest
from contextlib import contextmanager, redirect_stderr

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


class CopilotConfigFileAuthTest(BaseTelemetryTest):
    """auth-file selection when ~/.copilot/config.json is also a settings file.

    config.json is copilot's settings file and its sign-in file, and
    _ensure_settings writes trustedFolders into it on every provision, so a
    file holding only that key must not count as a supplied credential.
    All values are synthetic placeholders.
    """

    def setUp(self) -> None:
        super().setUp()
        self.home = tempfile.mkdtemp()
        self.bundle = os.path.join(self.home, ".scion", "harness")
        os.makedirs(os.path.join(self.bundle, "inputs"))
        self.config_path = os.path.join(self.home, ".copilot", "config.json")
        self._home_ctx = temporary_home(self.home)
        self._home_ctx.__enter__()
        self._saved_tokens = {
            k: os.environ.pop(k)
            for k in ("COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN")
            if k in os.environ
        }

    def tearDown(self) -> None:
        os.environ.update(self._saved_tokens)
        self._home_ctx.__exit__(None, None, None)
        super().tearDown()

    def _write_config(self, data: object) -> None:
        os.makedirs(os.path.dirname(self.config_path), exist_ok=True)
        with open(self.config_path, "w", encoding="utf-8") as f:
            json.dump(data, f)

    def _provision(self, candidates: dict) -> dict:
        with open(os.path.join(self.bundle, "inputs", "auth-candidates.json"), "w") as f:
            json.dump({"schema_version": 1, **candidates}, f)
        manifest = {
            "harness_bundle_dir": self.bundle,
            "agent_workspace": os.path.join(self.home, "workspace"),
            "harness_config": {
                "no_auth": {"behavior": "drop-to-shell", "command": "copilot login"},
            },
        }
        ctx = scion_harness.ProvisionContext("copilot", manifest)
        provision.provision(ctx)
        with open(os.path.join(self.bundle, "outputs", "resolved-auth.json")) as f:
            return json.load(f)

    def test_settings_only_config_does_not_select_auth_file(self) -> None:
        # The file the old home template shipped, and the one _ensure_settings writes.
        self._write_config({"trustedFolders": ["/workspace"]})
        self.assertEqual(self._provision({})["method"], "none")

    def test_settings_only_config_explicit_auth_file_fails(self) -> None:
        self._write_config({"trustedFolders": ["/workspace"]})
        with self.assertRaisesRegex(scion_harness.ProvisionError,
                                    "no auth file found.*holds no credential"):
            self._provision({"explicit_type": "auth-file"})

    def test_empty_config_does_not_select_auth_file(self) -> None:
        self._write_config({})
        self.assertEqual(self._provision({})["method"], "none")

    def test_restart_without_credential_stays_unauthenticated(self) -> None:
        # The first provision writes config.json (trustedFolders); the second
        # must not mistake it for a sign-in.
        self.assertEqual(self._provision({})["method"], "none")
        self.assertTrue(os.path.isfile(self.config_path))
        self.assertEqual(self._provision({})["method"], "none")

    def test_staged_config_secret_selects_auth_file(self) -> None:
        secret = os.path.join(self.bundle, "secrets", "COPILOT_CONFIG")
        os.makedirs(os.path.dirname(secret))
        with open(secret, "w", encoding="utf-8") as f:
            json.dump({"placeholder_sign_in": {"login": "example-user"}}, f)
        resolved = self._provision({
            "file_secret_files": {"COPILOT_CONFIG": "$HOME/.scion/harness/secrets/COPILOT_CONFIG"},
        })
        self.assertEqual(resolved["method"], "auth-file")
        self.assertTrue(resolved.get("config_file_written"))
        with open(self.config_path, encoding="utf-8") as f:
            written = json.load(f)
        self.assertIn("placeholder_sign_in", written)
        self.assertIn(os.path.join(self.home, "workspace"), written["trustedFolders"])

    def test_staged_settings_only_secret_still_selects_auth_file(self) -> None:
        # A credential the user supplies is never second-guessed.
        secret = os.path.join(self.bundle, "secrets", "COPILOT_CONFIG")
        os.makedirs(os.path.dirname(secret))
        with open(secret, "w", encoding="utf-8") as f:
            json.dump({"trustedFolders": ["/workspace"]}, f)
        resolved = self._provision({
            "file_secret_files": {"COPILOT_CONFIG": "$HOME/.scion/harness/secrets/COPILOT_CONFIG"},
        })
        self.assertEqual(resolved["method"], "auth-file")

    def test_in_container_sign_in_config_selects_auth_file(self) -> None:
        # e.g. after `copilot login` in the no-auth shell, then a restart.
        self._write_config({
            "trustedFolders": ["/workspace"],
            "placeholder_sign_in": {"login": "example-user"},
        })
        self.assertEqual(self._provision({})["method"], "auth-file")

    def test_api_key_still_wins_over_settings_only_config(self) -> None:
        self._write_config({"trustedFolders": ["/workspace"]})
        secret = os.path.join(self.bundle, "secrets", "COPILOT_GITHUB_TOKEN")
        os.makedirs(os.path.dirname(secret))
        with open(secret, "w", encoding="utf-8") as f:
            f.write("placeholder-token")
        resolved = self._provision({
            "env_secret_files": {"COPILOT_GITHUB_TOKEN": "$HOME/.scion/harness/secrets/COPILOT_GITHUB_TOKEN"},
        })
        self.assertEqual(resolved["method"], "api-key")

    # --- staged COPILOT_CONFIG secret copilot cannot read (ptone/scion#4665)
    # The start fails with a ProvisionError that names the staged file and
    # never includes its content; nothing is written to ~/.copilot. Long
    # integers and nesting too deep for Python are accepted: copilot and the
    # host read them, and the file is copied unchanged.

    STAGED_MARKER = "placeholder-marker-4665"

    def _stage_config_secret(self, raw: bytes) -> str:
        secret = os.path.join(self.bundle, "secrets", "COPILOT_CONFIG")
        os.makedirs(os.path.dirname(secret), exist_ok=True)
        with open(secret, "wb") as f:
            f.write(raw)
        return secret

    def _provision_staged(self) -> dict:
        return self._provision({
            "file_secret_files": {"COPILOT_CONFIG": "$HOME/.scion/harness/secrets/COPILOT_CONFIG"},
        })

    def _assert_staged_refused(self, raw: bytes, kind: str) -> None:
        secret = self._stage_config_secret(raw)
        with self.assertRaises(scion_harness.ProvisionError) as cm:
            self._provision_staged()
        msg = str(cm.exception)
        self.assertIn(f"COPILOT_CONFIG secret file {secret} ", msg)
        self.assertIn(kind, msg)
        self.assertNotIn(self.STAGED_MARKER, msg)
        self.assertIsNone(cm.exception.__cause__)
        self.assertTrue(cm.exception.__suppress_context__)
        self.assertFalse(os.path.exists(self.config_path))

    def test_staged_non_utf8_config_is_refused(self) -> None:
        raw = ('{"placeholder_sign_in": "' + self.STAGED_MARKER).encode() + b'\xff"}'
        self._assert_staged_refused(raw, "is not valid UTF-8")

    def test_staged_non_finite_config_is_refused(self) -> None:
        for lit in ("NaN", "Infinity", "-Infinity", "1e400", "-1e400"):
            with self.subTest(lit):
                raw = ('{"placeholder_sign_in": "' + self.STAGED_MARKER
                       + '", "n": ' + lit + "}").encode()
                self._assert_staged_refused(raw, "holds NaN, Infinity or a number beyond float range")

    def test_staged_invalid_json_config_is_refused(self) -> None:
        raw = ('{"placeholder_sign_in": "' + self.STAGED_MARKER + '",}').encode()
        self._assert_staged_refused(raw, "is not valid JSON")

    def test_staged_too_deep_config_is_accepted(self) -> None:
        # Copilot and the host read nesting deeper than Python's decoder
        # does; the file is copied as is, and _ensure_settings keeps it.
        raw = ('{"placeholder_sign_in": {"login": "example-user"}, "n": '
               + "[" * 100000 + "]" * 100000 + "}").encode()
        self.assertFalse(provision._validate_staged_config(raw.decode(), "/staged/COPILOT_CONFIG"))
        secret = self._stage_config_secret(raw)
        stderr = io.StringIO()
        with redirect_stderr(stderr):
            resolved = self._provision_staged()
        # ctx.info writes the note to stderr; it names the file, no content.
        note = (f"copilot provision: COPILOT_CONFIG secret file {secret} is nested "
                "too deeply to check; copying it unchanged")
        self.assertIn(note, stderr.getvalue().splitlines())
        self.assertNotIn("example-user", stderr.getvalue())
        self.assertNotIn("placeholder_sign_in", stderr.getvalue())
        self.assertEqual(resolved["method"], "auth-file")
        self.assertTrue(resolved.get("config_file_written"))
        self.assertEqual(self._read_raw(), raw)

    def test_staged_long_int_config_is_accepted(self) -> None:
        # Copilot and the host read long integers; the file is copied as is.
        raw = ('// note\n{"placeholder_sign_in": {"login": "example-user"}, "n": '
               + self.LONG_INT + "}").encode()
        self._stage_config_secret(raw)
        resolved = self._provision_staged()
        self.assertEqual(resolved["method"], "auth-file")
        self.assertTrue(resolved.get("config_file_written"))
        # _ensure_settings cannot rewrite it losslessly and the host's rule
        # says it may hold sign-in, so it stays byte-for-byte as staged.
        self.assertEqual(self._read_raw(), raw)

    # --- config.json that is not cleanly readable (ptone/scion#4624) -------
    # A file that cannot be decoded or converted is handled like unparseable
    # JSON: replaced, unless the host's rule says it may hold sign-in, in
    # which case it is left byte-for-byte untouched. Provisioning continues.

    LONG_INT = "1" * 5000  # beyond Python's 4300-digit int conversion limit

    def _write_raw(self, raw: bytes) -> None:
        os.makedirs(os.path.dirname(self.config_path), exist_ok=True)
        with open(self.config_path, "wb") as f:
            f.write(raw)

    def _read_raw(self) -> bytes:
        with open(self.config_path, "rb") as f:
            return f.read()

    def _provision_must_succeed(self) -> dict:
        # A formerly escaping exception is reported as a test failure.
        try:
            return self._provision({})
        except Exception as exc:  # noqa: BLE001
            self.fail(f"provisioning raised {type(exc).__name__}: {exc}")

    def _assert_replaced(self) -> None:
        with open(self.config_path, encoding="utf-8") as f:
            self.assertEqual(json.load(f),
                             {"trustedFolders": [os.path.join(self.home, "workspace")]})

    def test_non_utf8_config_is_replaced(self) -> None:
        self._write_raw(b'{"theme": "dark"}\xff\n')
        self.assertEqual(self._provision_must_succeed()["method"], "none")
        self._assert_replaced()

    def test_non_utf8_in_settings_string_is_replaced(self) -> None:
        # Never rewritten with U+FFFD: not sign-in, so replaced.
        self._write_raw(b'{"theme": "dark\xff"}')
        self.assertEqual(self._provision_must_succeed()["method"], "none")
        self._assert_replaced()

    def test_non_utf8_in_sign_in_file_is_untouched(self) -> None:
        raw = b'{"copilot_hmac_key": "placeholder-hmac", "theme": "dark\xff"}'
        self._write_raw(raw)
        self.assertEqual(self._provision_must_succeed()["method"], "auth-file")
        self.assertEqual(self._read_raw(), raw)

    def test_long_int_config_is_replaced(self) -> None:
        self._write_raw(('{"theme": ' + self.LONG_INT + '}').encode())
        self.assertEqual(self._provision_must_succeed()["method"], "none")
        self._assert_replaced()

    def test_long_int_in_sign_in_file_is_untouched(self) -> None:
        raw = ('{"copilot_hmac_key": "placeholder-hmac", "theme": '
               + self.LONG_INT + '}').encode()
        self._write_raw(raw)
        self.assertEqual(self._provision_must_succeed()["method"], "auth-file")
        self.assertEqual(self._read_raw(), raw)

    def test_long_int_unknown_key_is_untouched(self) -> None:
        # An unknown key counts as possible sign-in on the host (fail open).
        raw = ('{"placeholder_key": ' + self.LONG_INT + '}').encode()
        self._write_raw(raw)
        self.assertEqual(self._provision_must_succeed()["method"], "auth-file")
        self.assertEqual(self._read_raw(), raw)

    def test_nan_and_infinity_config_is_replaced(self) -> None:
        # Copilot's JSON parser rejects these and the host reads the file as
        # not a JSON object, so it is replaced rather than kept.
        for const in ("NaN", "Infinity", "-Infinity"):
            with self.subTest(const):
                self._write_raw(('{"theme": ' + const + '}').encode())
                self.assertEqual(self._provision_must_succeed()["method"], "none")
                self._assert_replaced()

    def test_nan_with_credential_is_replaced(self) -> None:
        raw = b'{"copilot_hmac_key": "placeholder-hmac", "theme": NaN}'
        self._write_raw(raw)
        # NaN makes the content unparseable on the host too: not sign-in.
        self.assertEqual(self._provision_must_succeed()["method"], "none")
        self._assert_replaced()

    def test_too_deep_sign_in_file_is_untouched(self) -> None:
        # Find the first depth Python's decoder rejects; Go's limit is
        # 10000 levels, so the host may still read sign-in from it.
        def doc(n: int) -> str:
            return '{"copilot_hmac_key": "placeholder-hmac", "theme": ' + "[" * n + "]" * n + "}"

        def too_deep(n: int) -> bool:
            try:
                json.loads(doc(n))
            except RecursionError:
                return True
            return False

        lo, hi = 1, 9998  # 9998 arrays + the object = 9999 levels, within Go's limit
        self.assertTrue(too_deep(hi), "Python's decoder accepts 9999 levels; test needs updating")
        while lo < hi:
            mid = (lo + hi) // 2
            if too_deep(mid):
                hi = mid
            else:
                lo = mid + 1
        raw = doc(lo).encode()
        self._write_raw(raw)
        self._provision_must_succeed()
        self.assertEqual(self._read_raw(), raw)

    def _assert_deep_sign_in_untouched(self, defect: bytes) -> None:
        # A sign-in key, then a decode or convert defect, then 9998 nested
        # arrays (9999 levels, within Go's 10000). The strict parse stops at
        # the defect; the host-rule parse then reaches the deep part.
        raw = (b'{"copilot_hmac_key": "placeholder-hmac", "model": ' + defect
               + b', "theme": ' + b"[" * 9998 + b"]" * 9998 + b"}")
        with self.assertRaises(ValueError):
            provision._load_config_strictly(raw)
        with self.assertRaises(RecursionError,
                               msg="Python's decoder accepts 9999 levels; test needs updating"):
            provision._config_holds_sign_in_or_too_deep(raw)
        self._write_raw(raw)
        self._provision_must_succeed()
        self.assertEqual(self._read_raw(), raw)

    def test_too_deep_sign_in_file_after_long_int_is_untouched(self) -> None:
        self._assert_deep_sign_in_untouched(self.LONG_INT.encode())

    def test_too_deep_sign_in_file_after_non_utf8_is_untouched(self) -> None:
        self._assert_deep_sign_in_untouched(b'"x\xff"')

    def test_float_overflow_config_is_replaced(self) -> None:
        # 1e400 loads as inf and would be written back as Infinity.
        for lit in ("1e400", "-1e400"):
            with self.subTest(lit):
                self._write_raw(('{"theme": ' + lit + '}').encode())
                self.assertEqual(self._provision_must_succeed()["method"], "none")
                self._assert_replaced()

    def test_float_overflow_in_sign_in_file_is_untouched(self) -> None:
        # The host accepts 1e400 (it never converts numbers), so keys decide.
        for raw in (b'{"copilot_hmac_key": "placeholder-hmac", "theme": 1e400}',
                    b'{"placeholder_key": 1e400}'):
            with self.subTest(raw):
                self._write_raw(raw)
                self.assertEqual(self._provision_must_succeed()["method"], "auth-file")
                self.assertEqual(self._read_raw(), raw)

    # --- settings.json read site --------------------------------------------

    def _settings_path(self) -> str:
        return os.path.join(self.home, ".copilot", "settings.json")

    def _assert_settings_defaults(self) -> None:
        with open(self._settings_path(), encoding="utf-8") as f:
            self.assertEqual(json.load(f), {"autoUpdate": False, "banner": "never",
                                            "remoteSessions": False})

    def _write_settings_raw(self, raw: bytes) -> None:
        os.makedirs(os.path.dirname(self._settings_path()), exist_ok=True)
        with open(self._settings_path(), "wb") as f:
            f.write(raw)

    def test_non_utf8_settings_is_replaced_with_defaults(self) -> None:
        self._write_settings_raw(b'{"theme": "dark\xff"}')
        self._provision_must_succeed()
        self._assert_settings_defaults()

    def test_long_int_settings_is_replaced_with_defaults(self) -> None:
        self._write_settings_raw(('{"theme": ' + self.LONG_INT + '}').encode())
        self._provision_must_succeed()
        self._assert_settings_defaults()

    def test_nan_and_infinity_settings_is_replaced_with_defaults(self) -> None:
        # Kept, they would be written back as non-standard JSON.
        for const in ("NaN", "Infinity", "-Infinity"):
            with self.subTest(const):
                self._write_settings_raw(('{"theme": ' + const + '}').encode())
                self._provision_must_succeed()
                self._assert_settings_defaults()

    def test_float_overflow_settings_is_replaced_with_defaults(self) -> None:
        for lit in ("1e400", "-1e400"):
            with self.subTest(lit):
                self._write_settings_raw(('{"theme": ' + lit + '}').encode())
                self._provision_must_succeed()
                self._assert_settings_defaults()

    def test_unparseable_settings_is_replaced_with_defaults(self) -> None:
        self._write_settings_raw(b'{"theme": ')
        self._provision_must_succeed()
        self._assert_settings_defaults()

    def test_clean_settings_keeps_values(self) -> None:
        self._write_settings_raw(b'{"autoUpdate": true, "theme": "dark"}')
        self._provision_must_succeed()
        with open(self._settings_path(), encoding="utf-8") as f:
            self.assertEqual(json.load(f),
                             {"autoUpdate": True, "banner": "never", "theme": "dark",
                              "remoteSessions": False})

    def test_explicit_remote_sessions_is_kept(self) -> None:
        self._write_settings_raw(b'{"remoteSessions": true}')
        self._provision_must_succeed()
        with open(self._settings_path(), encoding="utf-8") as f:
            self.assertTrue(json.load(f)["remoteSessions"])

    def test_unparseable_config_is_still_replaced(self) -> None:
        self._write_raw(b'{"theme": ')
        self.assertEqual(self._provision_must_succeed()["method"], "none")
        self._assert_replaced()

    def test_clean_config_is_kept_unchanged(self) -> None:
        ws = os.path.join(self.home, "workspace")
        self._write_raw(json.dumps({"theme": "dark", "trustedFolders": [ws]}).encode())
        before = self._read_raw()
        self.assertEqual(self._provision_must_succeed()["method"], "none")
        self.assertEqual(self._read_raw(), before)

    def test_clean_config_gains_workspace(self) -> None:
        self._write_raw(b'// comment line\n{"theme": "dark", "trustedFolders": ["/src"]}')
        self.assertEqual(self._provision_must_succeed()["method"], "none")
        with open(self.config_path, encoding="utf-8") as f:
            self.assertEqual(json.load(f), {
                "theme": "dark",
                "trustedFolders": ["/src", os.path.join(self.home, "workspace")],
            })


class ConfigFileMayHoldCredentialTest(unittest.TestCase):
    """The content rule behind the auth-file present_check.

    It is the host's rule (copilotConfigHoldsSignIn): known settings keys do
    not count, non-empty credential keys do, an unknown key counts, and
    content that is not a JSON object does not. A file that cannot be read
    counts. All values are synthetic placeholders.
    """

    def _check(self, raw: str | bytes) -> bool:
        d = tempfile.mkdtemp()
        p = os.path.join(d, "config.json")
        with open(p, "wb") as f:
            f.write(raw if isinstance(raw, bytes) else raw.encode("utf-8"))
        return provision._config_file_may_hold_credential(p)

    def test_scion_written_settings_only(self) -> None:
        self.assertFalse(self._check('{"trustedFolders": ["/workspace"]}'))

    def test_copilot_settings_only(self) -> None:
        # The file of a user who signs in through the OS keychain.
        self.assertFalse(self._check(
            '{"trusted_folders": ["/src"], "banner": "never", "theme": "dark",'
            ' "logged_in_users": [{"host": "https://github.com", "login": "octo"}],'
            ' "last_logged_in_user": {"host": "https://github.com", "login": "octo"}}'))

    def test_settings_keys_case_insensitive(self) -> None:
        self.assertFalse(self._check('{"Trusted_Folders": [], "BANNER": "never"}'))

    def test_empty_object(self) -> None:
        self.assertFalse(self._check("{}"))

    def test_credential_keys_count(self) -> None:
        self.assertTrue(self._check(
            '{"trustedFolders": [], "copilot_tokens": {"https://github.com:octo": "placeholder-token"}}'))
        self.assertTrue(self._check('{"copilot_hmac_key": "placeholder-hmac"}'))
        self.assertTrue(self._check('{"theme": "dark", "capi_hmac_key": "placeholder-hmac"}'))
        self.assertTrue(self._check('{"COPILOT_TOKENS": {"https://github.com:octo": "placeholder-token"}}'))

    def test_empty_credential_values_do_not_count(self) -> None:
        self.assertFalse(self._check('{"trustedFolders": [], "copilot_tokens": {}}'))
        self.assertFalse(self._check('{"copilot_tokens": {"https://github.com:octo": "  "}}'))
        self.assertFalse(self._check('{"copilot_hmac_key": "", "capi_hmac_key": ""}'))

    def test_comment_lines_skipped(self) -> None:
        self.assertFalse(self._check('// managed file\n{"trustedFolders": []}'))
        self.assertTrue(self._check('// managed file\n{"placeholder_key": 1}'))

    def test_unknown_key_counts(self) -> None:
        self.assertTrue(self._check('{"trustedFolders": [], "placeholder_key": 1}'))

    def test_unparseable_is_not_sign_in(self) -> None:
        self.assertFalse(self._check("not json"))
        self.assertFalse(self._check('{"placeholder_key": '))
        self.assertFalse(self._check(""))

    def test_non_object_is_not_sign_in(self) -> None:
        self.assertFalse(self._check("[]"))
        self.assertFalse(self._check('["placeholder_key"]'))
        self.assertFalse(self._check("null"))

    def test_invalid_utf8(self) -> None:
        # Like Go's encoding/json: invalid UTF-8 inside a string is replaced,
        # outside one it makes the content unparseable.
        self.assertTrue(self._check(b'{"placeholder_key": "\xff"}'))
        self.assertFalse(self._check(b'{"trustedFolders": []}\xff'))

    def test_long_int_follows_keys(self) -> None:
        # The host never converts numbers (json.RawMessage), so an integer
        # past Python's 4300-digit limit does not make content unparseable.
        big = "1" * 5000
        self.assertFalse(self._check('{"theme": ' + big + '}'))
        self.assertFalse(self._check('{"copilot_tokens": {"placeholder": ' + big + '}}'))
        self.assertTrue(self._check('{"placeholder_key": ' + big + '}'))
        self.assertTrue(self._check('{"copilot_hmac_key": "placeholder-hmac", "theme": -' + big + '}'))

    def test_float_overflow_follows_keys(self) -> None:
        # Unlike NaN, 1e400 is valid JSON; the host never converts it.
        self.assertFalse(self._check('{"theme": 1e400}'))
        self.assertTrue(self._check('{"placeholder_key": 1e400}'))
        self.assertTrue(self._check('{"copilot_hmac_key": "placeholder-hmac", "theme": -1e400}'))
        self.assertFalse(self._check('{"copilot_hmac_key": 1e400}'))

    def test_nan_and_infinity_are_not_sign_in(self) -> None:
        for const in ("NaN", "Infinity", "-Infinity"):
            with self.subTest(const):
                self.assertFalse(self._check('{"placeholder_key": ' + const + '}'))

    def test_deep_nesting_is_not_sign_in(self) -> None:
        # Too deep for Python's decoder, so unparseable here. Go's limit is
        # 10000 levels; which shallower depths still differ from the host
        # depends on the decoder (with the C decoder, only 9998-9999).
        self.assertFalse(self._check('{"placeholder_key": ' + "[" * 100000 + "]" * 100000 + "}"))

    def test_unreadable_fails_open(self) -> None:
        # No host counterpart: the host applies the rule only to content it read.
        self.assertTrue(provision._config_file_may_hold_credential(
            os.path.join(tempfile.mkdtemp(), "missing.json")))
        self.assertTrue(provision._config_file_may_hold_credential(tempfile.mkdtemp()))


REPO_ROOT = os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", ".."))
HOST_RULE_GO = os.path.join(REPO_ROOT, "pkg", "harness", "host_credentials_content.go")
HOST_FIXTURES = os.path.join(REPO_ROOT, "pkg", "harness", "testdata", "copilot_config_signin.json")

# Floors pinned so that a moved, emptied or refactored host file cannot make
# the cross-checks below pass vacuously. Raise them when the host lists grow.
MIN_HOST_FIXTURES = 34
HOST_SETTINGS_KEY_COUNT = 13
HOST_CREDENTIAL_KEY_COUNT = 3


class HostRuleCrossCheckTest(unittest.TestCase):
    """The container rule must match the host's copilotConfigHoldsSignIn.

    These read the Go source and test fixtures from the repository. They fail,
    never skip, when the files are missing or yield too little.
    """

    def _go_source(self) -> str:
        with open(HOST_RULE_GO, encoding="utf-8") as f:
            return f.read()

    def test_settings_keys_match_host(self) -> None:
        src = self._go_source()
        m = re.search(r"var copilotSettingsKeys = map\[string\]struct\{\}\{(.*?)\n\}", src, re.S)
        self.assertIsNotNone(m, "copilotSettingsKeys not found in " + HOST_RULE_GO)
        assert m is not None
        host_keys = re.findall(r'^\s*"([^"]+)":\s*\{\},', m.group(1), re.M)
        self.assertEqual(len(host_keys), HOST_SETTINGS_KEY_COUNT,
                         f"extracted {host_keys!r} from copilotSettingsKeys")
        self.assertEqual(frozenset(host_keys), provision._COPILOT_SETTINGS_KEYS)

    def test_credential_keys_match_host(self) -> None:
        src = self._go_source()
        m = re.search(r"func copilotConfigHoldsSignIn\(data \[\]byte\) bool \{(.*?)\n\}", src, re.S)
        self.assertIsNotNone(m, "copilotConfigHoldsSignIn not found in " + HOST_RULE_GO)
        assert m is not None
        host_keys: list[str] = []
        for case in re.findall(r"^\s*case (.+):$", m.group(1), re.M):
            host_keys += re.findall(r'"([^"]+)"', case)
        self.assertEqual(len(host_keys), HOST_CREDENTIAL_KEY_COUNT,
                         f"extracted {host_keys!r} from copilotConfigHoldsSignIn")
        self.assertEqual(frozenset(host_keys), provision._COPILOT_CREDENTIAL_KEYS)

    def test_host_fixtures(self) -> None:
        with open(HOST_FIXTURES, encoding="utf-8") as f:
            fixtures = json.load(f)
        self.assertGreaterEqual(len(fixtures), MIN_HOST_FIXTURES)
        d = tempfile.mkdtemp()
        for i, tc in enumerate(fixtures):
            with self.subTest(tc["name"]):
                p = os.path.join(d, f"config-{i}.json")
                with open(p, "w", encoding="utf-8", newline="") as f:
                    f.write(tc["data"])
                self.assertIs(provision._config_file_may_hold_credential(p), tc["signIn"],
                              f"{tc['name']}: {tc['data']!r}")


# Characters on which Python's str.splitlines()/str.strip() and the host's
# strings.Split(s, "\n")/strings.TrimSpace disagree (ptone/scion#4665).
PARITY_CHARS = ("\v", "\f", "\x1c", "\x1d", "\x1e", "\x1f", "\x85", "\u2028", "\u2029")


def parity_cases() -> list[tuple[str, str, object]]:
    """(name, content, expected) per character, by the host's rule; expected
    is the parsed value, or None when the host cannot parse the content."""
    cases: list[tuple[str, str, object]] = []
    for ch in PARITY_CHARS:
        name = f"U+{ord(ch):04X}"
        # Inside a string: JSON allows any character there except a control
        # character (below U+0020).
        cases.append((f"{name} in string", '{"k": "a' + ch + 'b"}',
                      {"k": "a" + ch + "b"} if ord(ch) >= 0x20 else None))
        # Leading a // line: trimmed, so the line is a comment, only if the
        # host counts it as whitespace.
        cases.append((f"{name} before comment line", '{"k": 1}\n' + ch + "// note\n",
                      {"k": 1} if ch in scion_harness.HOST_SPACE else None))
        # Not a line break for the host, so "// note" is not a comment line.
        cases.append((f"{name} as line break", '{"k": 1}' + ch + "// note\n", None))
    return cases


class CommentLineHostParityTest(unittest.TestCase):
    """The strict loader and the staged-config check split lines and trim
    whitespace as the host does (ptone/scion#4665)."""

    def test_strict_loader_matches_host(self) -> None:
        cases = parity_cases()
        self.assertEqual(len(cases), 3 * len(PARITY_CHARS))
        for name, content, expected in cases:
            with self.subTest(name):
                raw = content.encode("utf-8")
                if expected is None:
                    with self.assertRaises(ValueError):
                        provision._load_config_strictly(raw)
                else:
                    self.assertEqual(provision._load_config_strictly(raw), expected)

    def test_strict_loader_lone_carriage_return_is_not_a_line_break(self) -> None:
        with self.assertRaises(ValueError):
            provision._load_config_strictly(b'{"k": 1}\r// note\n')

    def test_strict_loader_crlf_comment_lines_are_dropped(self) -> None:
        self.assertEqual(
            provision._load_config_strictly(b'// note\r\n{"k": 1}\r\n  // more\r\n'),
            {"k": 1})

    def test_staged_config_check_matches_host(self) -> None:
        for name, content, expected in parity_cases():
            with self.subTest(name):
                if expected is None:
                    with self.assertRaises(scion_harness.ProvisionError):
                        provision._validate_staged_config(content, "/staged/COPILOT_CONFIG")
                else:
                    self.assertTrue(
                        provision._validate_staged_config(content, "/staged/COPILOT_CONFIG"))

    def test_host_comment_line_rule_unchanged(self) -> None:
        # The cases above encode the host's rule; fail if the host changes it.
        with open(HOST_RULE_GO, encoding="utf-8") as f:
            src = f.read()
        m = re.search(r"func copilotConfigHoldsSignIn\(data \[\]byte\) bool \{(.*?)\n\}", src, re.S)
        self.assertIsNotNone(m, "copilotConfigHoldsSignIn not found in " + HOST_RULE_GO)
        assert m is not None
        self.assertIn('strings.Split(string(data), "\\n")', m.group(1))
        self.assertIn('strings.HasPrefix(strings.TrimSpace(ln), "//")', m.group(1))


# ---------------------------------------------------------------------------
# Agent Plugins
# ---------------------------------------------------------------------------

# The config.json header copilot 1.0.97 writes when it rewrites the file.
_COPILOT_CONFIG_HEADER = "// User settings belong in settings.json.\n// This file is managed automatically.\n"


class _FakeCopilotCLI:
    """Stands in for scion_harness.run_cli: records argv and COPILOT_HOME and
    simulates the copilot 1.0.97 plugin CLI (shapes observed in the smoke run,
    smoke/copilot.md) against a temp COPILOT_HOME."""

    def __init__(self, version: str = "GitHub Copilot CLI 1.0.97.\nRun 'copilot update' to check for updates.\n"):
        self.version = version
        self.calls: list[list[str]] = []
        self.homes: list[str] = []
        self.envs: list[dict[str, str]] = []
        self.timeouts: list[float | None] = []
        self.fail_install = False
        self.install_registers = True
        self.omit_cache_path = False
        self.omit_enabled = False
        self.no_cache_dir = False
        # Leave config.json unparseable by the strict loader after install.
        self.corrupt_after_install = False
        # A user's same-named plugin from a local marketplace ("live", not
        # in installedPlugins); a bare-name uninstall disables it instead
        # of touching the direct install (copilot 1.0.97).
        self.user_marketplace_plugin: str | None = None
        self.user_marketplace_disabled = False
        self.uninstall_keeps_entry = False
        self.uninstall_rc = 0
        self.unreadable_uninstall_rc = 1

    def _config_path(self, home: str) -> str:
        return os.path.join(home, "config.json")

    def _load(self, home: str) -> dict:
        try:
            with open(self._config_path(home), encoding="utf-8") as f:
                text = "".join(ln for ln in f if not ln.lstrip().startswith("//"))
        except FileNotFoundError:
            return {}
        return json.loads(text) if text.strip() else {}

    def _save(self, home: str, data: dict) -> None:
        os.makedirs(home, exist_ok=True)
        with open(self._config_path(home), "w", encoding="utf-8") as f:
            f.write(_COPILOT_CONFIG_HEADER + json.dumps(data, indent=2) + "\n")

    def __call__(self, argv, *, timeout=None, env=None):
        import shutil
        import subprocess
        self.calls.append(list(argv))
        env = env or {}
        home = env.get("COPILOT_HOME", "")
        self.homes.append(home)
        self.envs.append(env)
        self.timeouts.append(timeout)
        args = argv[1:]
        if args == ["--version"]:
            return subprocess.CompletedProcess(argv, 0, self.version, "")
        if args[:2] == ["plugin", "install"]:
            if self.fail_install:
                return subprocess.CompletedProcess(argv, 1, "", "Failed to install plugin: boom")
            src = args[2]
            with open(os.path.join(src, "plugin.json"), encoding="utf-8") as f:
                name = json.load(f)["name"]
            # <id> is the source dir's basename, not the plugin name.
            cache = os.path.join(home, "installed-plugins", "_direct", os.path.basename(src))
            shutil.rmtree(cache, ignore_errors=True)
            if not self.no_cache_dir:
                shutil.copytree(src, cache)
            if self.install_registers:
                data = self._load(home)
                plugins = [e for e in data.get("installedPlugins", []) if e.get("name") != name]
                entry = {"name": name, "marketplace": "", "version": "0.1.0",
                         "installed_at": "2026-10-13T21:58:10.207811723Z", "cache_path": cache,
                         "enabled": True, "source": {"source": "local", "path": src}}
                if self.omit_cache_path:
                    entry.pop("cache_path")
                if self.omit_enabled:
                    entry.pop("enabled")
                plugins.append(entry)
                data["installedPlugins"] = plugins
                self._save(home, data)
            if self.corrupt_after_install:
                with open(self._config_path(home), "w", encoding="utf-8") as f:
                    f.write('{"n": ' + "9" * 5000 + "}")
            return subprocess.CompletedProcess(
                argv, 0, f'Plugin "{name}" installed successfully. Installed 1 skill.\n',
                "\nWarning: Direct plugin installs (repos, URLs, local paths) are deprecated. "
                "Only plugin@marketplace installs will be supported in a future release.\n")
        if args[:2] == ["plugin", "uninstall"]:
            arg = args[2]
            if not arg.endswith("@") and arg == self.user_marketplace_plugin:
                self.user_marketplace_disabled = True
                return subprocess.CompletedProcess(
                    argv, 0, f'Plugin "{arg}@usermkt" disabled. It is still on disk at /mkt — nothing was removed.\n', "")
            name = arg[:-1] if arg.endswith("@") else arg
            try:
                data = self._load(home)
            except ValueError:
                # Content the provisioner's strict loader cannot parse; the
                # real CLI's parser may differ, so the outcome is scripted.
                return subprocess.CompletedProcess(argv, self.unreadable_uninstall_rc, "", "")
            plugins = data.get("installedPlugins", [])
            entry = next((e for e in plugins if e.get("name") == name and not e.get("marketplace")), None)
            if self.uninstall_keeps_entry:
                return subprocess.CompletedProcess(argv, self.uninstall_rc, "", "error: busy")
            if entry is None:
                return subprocess.CompletedProcess(
                    argv, 1, "", f'Failed to uninstall plugin: Error: Request plugins.uninstall failed '
                    f'with message: Plugin "{arg}" is not installed\n')
            shutil.rmtree(entry.get("cache_path", ""), ignore_errors=True)
            data["installedPlugins"] = [e for e in plugins if e is not entry]
            self._save(home, data)
            return subprocess.CompletedProcess(argv, 0, f'Plugin "{arg}" uninstalled successfully.\n', "")
        return subprocess.CompletedProcess(argv, 2, "", "unexpected")


class CopilotAgentPluginsTest(BaseTelemetryTest):
    def setUp(self) -> None:
        super().setUp()
        tmp = tempfile.TemporaryDirectory(prefix="copilot-plugins-test-")
        self.addCleanup(tmp.cleanup)
        self.tmp = tmp.name
        self.home = os.path.join(self.tmp, "home")
        self.bundle = os.path.join(self.home, ".scion", "harness")
        self.inputs = os.path.join(self.bundle, "inputs")
        os.makedirs(self.inputs)
        self.copilot_home = os.path.join(self.home, ".copilot")
        self.config_path = os.path.join(self.copilot_home, "config.json")
        home_ctx = temporary_home(self.home)
        home_ctx.__enter__()
        self.addCleanup(home_ctx.__exit__, None, None, None)
        saved = {k: os.environ.pop(k) for k in ("COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN", "COPILOT_HOME")
                 if k in os.environ}
        self.addCleanup(os.environ.update, saved)
        self.cli = _FakeCopilotCLI()
        original = scion_harness.run_cli
        scion_harness.run_cli = self.cli
        self.addCleanup(setattr, scion_harness, "run_cli", original)

    def stage(self, names: dict[str, str]) -> None:
        import shutil
        shutil.rmtree(os.path.join(self.inputs, "plugins"), ignore_errors=True)
        entries = []
        for name, digest in names.items():
            d = os.path.join(self.inputs, "plugins", name)
            os.makedirs(os.path.join(d, "skills", "hello"))
            with open(os.path.join(d, "plugin.json"), "w") as f:
                json.dump({"$schema": "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json",
                           "name": name, "version": "0.1.0"}, f)
            with open(os.path.join(d, "skills", "hello", "SKILL.md"), "w") as f:
                f.write(f"---\nname: hello\ndescription: d\n---\n{digest}\n")
            entries.append({"name": name, "version": "0.1.0", "dir": f"plugins/{name}", "digest": digest,
                            "skills": ["hello"], "skills_dir_conflict": False, "mcp": None})
        with open(os.path.join(self.inputs, "plugins.json"), "w") as f:
            json.dump({"schema_version": 1, "spec_version": "1.0.0", "plugins": entries}, f)

    def ctx(self, warnings: list[str] | None = None, infos: list[str] | None = None):
        ctx = scion_harness.ProvisionContext("copilot", {
            "harness_bundle_dir": self.bundle,
            "agent_workspace": os.path.join(self.tmp, "workspace"),
            "harness_config": {"no_auth": {"behavior": "drop-to-shell", "command": "copilot login"}},
        })
        if warnings is not None:
            ctx.warn = warnings.append  # type: ignore[method-assign]
        if infos is not None:
            ctx.info = infos.append  # type: ignore[method-assign]
        return ctx

    def reconcile(self, warnings: list[str] | None = None, **kw) -> None:
        ctx = self.ctx(warnings)
        scion_harness.reconcile_agent_plugins(ctx, provision.CopilotPluginInstaller(ctx), **kw)

    def state(self) -> dict:
        path = os.path.join(self.home, ".scion", "agent-plugins", "state-copilot.json")
        if not os.path.exists(path):
            return {}  # nothing was ever recorded
        with open(path) as f:
            return json.load(f)["plugins"]

    def status(self) -> dict:
        with open(os.path.join(self.home, ".scion", "agent-plugins", "status-copilot.json")) as f:
            return json.load(f)

    def config(self) -> dict:
        return self.cli._load(self.copilot_home)

    def bundle_dir(self, name: str) -> str:
        return os.path.join(self.home, ".scion", "agent-plugins", "bundles", name)

    def direct_dir(self, name: str) -> str:
        return os.path.join(self.copilot_home, "installed-plugins", "_direct", name)

    def test_install_argv_env_and_native_ref(self) -> None:
        self.stage({"scion-smoke": "d1"})
        warnings: list[str] = []
        self.reconcile(warnings)
        self.assertEqual(warnings, [])
        self.assertEqual(self.cli.calls, [
            ["copilot", "--version"],
            ["copilot", "plugin", "install", self.bundle_dir("scion-smoke")],
        ])
        self.assertEqual(set(self.cli.homes), {self.copilot_home})
        self.assertTrue(all(e.get("COPILOT_AUTO_UPDATE") == "false" for e in self.cli.envs))
        self.assertEqual(self.state()["scion-smoke"]["native_ref"],
                         {"name": "scion-smoke", "path": self.direct_dir("scion-smoke")})
        self.assertEqual(self.status()["installed"], ["scion-smoke"])
        self.assertEqual(self.status()["cli_version"], "1.0.97")

    def test_restart_is_fast_path(self) -> None:
        self.stage({"scion-smoke": "d1"})
        self.reconcile()
        self.cli.calls.clear()
        self.reconcile()
        self.assertEqual(self.cli.calls, [])

    def test_dotted_name(self) -> None:
        self.stage({"scion.smoke": "d1"})
        self.reconcile()
        self.assertEqual(self.state()["scion.smoke"]["native_ref"]["path"], self.direct_dir("scion.smoke"))
        self.cli.calls.clear()
        self.reconcile()
        self.assertEqual(self.cli.calls, [])

    def test_old_cli_skips_without_changes(self) -> None:
        self.cli.version = "GitHub Copilot CLI 1.0.73.\n"
        self.stage({"a": "d1"})
        warnings: list[str] = []
        self.reconcile(warnings)
        self.assertEqual(self.cli.calls, [["copilot", "--version"]])
        self.assertEqual(len(warnings), 1)
        self.assertIn("copilot 1.0.73 is older than 1.0.74", warnings[0])

    def test_removal_and_digest_change(self) -> None:
        self.stage({"a": "d1", "b": "d1"})
        self.reconcile()
        self.stage({"b": "d2"})
        self.cli.calls.clear()
        self.reconcile()
        self.assertEqual(self.cli.calls, [
            ["copilot", "--version"],
            ["copilot", "plugin", "uninstall", "a@"],
            ["copilot", "plugin", "uninstall", "b@"],
            ["copilot", "plugin", "install", self.bundle_dir("b")],
        ])
        self.assertEqual([e["name"] for e in self.config()["installedPlugins"]], ["b"])
        with open(os.path.join(self.direct_dir("b"), "skills", "hello", "SKILL.md")) as f:
            self.assertIn("d2", f.read())
        self.assertEqual(self.status()["removed"], ["a"])
        self.assertEqual(sorted(self.state()), ["b"])

    def test_uninstall_not_installed_counts_as_removed(self) -> None:
        self.stage({"a": "d1"})
        self.reconcile()
        # The registration is gone already (e.g. the user uninstalled it);
        # copilot exits 1 for "not installed", which is still a removal.
        with open(self.config_path, "w") as f:
            json.dump({"trustedFolders": ["/workspace"]}, f)
        self.stage({})
        infos: list[str] = []
        inst = provision.CopilotPluginInstaller(self.ctx(infos=infos))
        inst.uninstall("a", {})
        self.assertIn("agent plugin a: copilot plugin uninstall exit 1, plugin no longer registered", infos)

    def test_uninstall_still_registered_fails(self) -> None:
        self.stage({"a": "d1"})
        self.reconcile()
        inst = provision.CopilotPluginInstaller(self.ctx())
        self.cli.uninstall_keeps_entry = True
        self.cli.uninstall_rc = 1
        with self.assertRaisesRegex(scion_harness.PluginInstallError, "copilot plugin uninstall exit 1"):
            inst.uninstall("a", {})
        self.cli.uninstall_rc = 0
        with self.assertRaisesRegex(scion_harness.PluginInstallError, "still registered"):
            inst.uninstall("a", {})
        # Kept in state for a retry at the next start.
        self.stage({})
        warnings: list[str] = []
        self.reconcile(warnings)
        self.assertIn("a", self.state())
        self.assertTrue(any("removal failed" in w for w in warnings))

    def test_install_failure_not_recorded(self) -> None:
        self.cli.fail_install = True
        self.stage({"a": "d1"})
        warnings: list[str] = []
        self.reconcile(warnings)
        self.assertEqual(warnings, [
            "agent plugin a: install failed (copilot plugin install exit 1: Failed to install plugin: boom); "
            "will retry next start"])
        self.assertEqual(self.state(), {})

    def test_install_exit_zero_without_registration_fails(self) -> None:
        self.cli.install_registers = False
        self.stage({"a": "d1"})
        warnings: list[str] = []
        self.reconcile(warnings)
        self.assertEqual(len(warnings), 1)
        self.assertIn("is not registered as enabled", warnings[0])
        self.assertEqual(self.state(), {})

    def test_direct_dir_discovery_without_cache_path(self) -> None:
        # No cache_path in the registration: scan _direct/ for a dir whose
        # plugin.json names the plugin (the dir name need not match).
        self.cli.omit_cache_path = True
        self.stage({"a": "d1"})
        other = os.path.join(self.copilot_home, "installed-plugins", "_direct", "0-other")
        os.makedirs(other)
        with open(os.path.join(other, "plugin.json"), "w") as f:
            json.dump({"name": "other"}, f)
        ctx = self.ctx()
        inst = provision.CopilotPluginInstaller(ctx)
        [p] = scion_harness.read_agent_plugins(ctx)
        bundle = scion_harness.mirror_bundle(p)
        renamed = os.path.join(self.tmp, "src-x")
        os.rename(bundle, renamed)
        ref = inst.install(p, renamed)
        self.assertEqual(ref, {"name": "a", "path": self.direct_dir("src-x")})
        self.assertTrue(inst.is_present("a", ref))

    def test_uninstall_targets_direct_install_only(self) -> None:
        # copilot 1.0.97: a bare-name uninstall acts on a same-named
        # marketplace plugin (the user's); "<name>@" targets the direct one.
        self.stage({"a": "d1"})
        self.reconcile()
        self.cli.user_marketplace_plugin = "a"
        self.stage({})
        self.cli.calls.clear()
        self.reconcile()
        self.assertEqual(self.cli.calls[-1], ["copilot", "plugin", "uninstall", "a@"])
        self.assertFalse(self.cli.user_marketplace_disabled)
        self.assertEqual(self.config()["installedPlugins"], [])
        self.assertEqual(self.status()["removed"], ["a"])

    def test_removal_after_registration_lost_removes_cache_dir(self) -> None:
        # Auth-file agents: config.json is rewritten from the secret every
        # start, so at removal time the registration is already gone and the
        # CLI (exit 1, not installed) removes nothing. The _direct dir must
        # not be orphaned.
        self.stage({"a": "d1"})
        self.reconcile()
        self.assertTrue(os.path.isdir(self.direct_dir("a")))
        with open(self.config_path, "w") as f:
            json.dump({"copilot_tokens": {"https://github.com:octo": "placeholder-token"}}, f)
        self.stage({})
        warnings: list[str] = []
        self.reconcile(warnings)
        self.assertEqual(warnings, [])
        self.assertEqual(self.cli.calls[-1], ["copilot", "plugin", "uninstall", "a@"])
        self.assertFalse(os.path.exists(self.direct_dir("a")))
        self.assertTrue(os.path.isdir(os.path.join(self.copilot_home, "installed-plugins", "_direct")))
        self.assertEqual(self.status()["removed"], ["a"])
        self.assertEqual(self.state(), {})

    def test_uninstall_with_unreadable_config_keeps_state(self) -> None:
        # Round 2: an existing config.json the strict loader cannot parse
        # (here an integer beyond Python's conversion limit) is not "not
        # registered": copilot may still list the plugin. The removal fails,
        # the state record and the _direct dir are kept, and it is retried.
        self.stage({"a": "d1"})
        self.reconcile()
        data = self.config()
        with open(self.config_path, "w") as f:
            f.write(json.dumps(data)[:-1] + ', "n": ' + "9" * 5000 + "}")
        self.stage({})
        for rc in (1, 0):
            self.cli.unreadable_uninstall_rc = rc
            warnings: list[str] = []
            self.reconcile(warnings)
            self.assertEqual(len(warnings), 1, warnings)
            self.assertIn(f"agent plugin a: removal failed (copilot plugin uninstall exit {rc}, "
                          "but cannot read ~/.copilot/config.json to confirm the result (ValueError))", warnings[0])
            self.assertIn("a", self.state())
            self.assertTrue(os.path.isdir(self.direct_dir("a")))
        # Lenient elsewhere: is_present treats it as not registered.
        inst = provision.CopilotPluginInstaller(self.ctx())
        self.assertFalse(inst.is_present("a", self.state()["a"]["native_ref"]))
        # Missing config.json is still "not registered": the removal succeeds.
        os.remove(self.config_path)
        self.reconcile()
        self.assertEqual(self.state(), {})
        self.assertFalse(os.path.exists(self.direct_dir("a")))

    def test_leftover_dir_removal_is_confined_to_direct(self) -> None:
        inst = provision.CopilotPluginInstaller(self.ctx(infos=[]))
        outside = os.path.join(self.tmp, "outside")
        os.makedirs(outside)
        root = os.path.join(self.copilot_home, "installed-plugins", "_direct")
        nested = os.path.join(root, "x", "y")
        os.makedirs(nested)
        link = os.path.join(root, "link")
        os.symlink(outside, link)
        for path in (outside, root, nested, link, os.path.join(root, "..", "..")):
            inst.uninstall("gone", {"path": path})
        self.assertTrue(os.path.isdir(outside))
        self.assertTrue(os.path.isdir(nested))
        self.assertTrue(os.path.islink(link))
        inst.uninstall("gone", {"path": os.path.join(root, "x")})
        self.assertFalse(os.path.exists(os.path.join(root, "x")))

    def test_install_without_enabled_flag_fails_loudly(self) -> None:
        # One predicate for install and is_present: an entry without
        # enabled: true is not recorded (a warning, retried next start)
        # rather than recorded and silently reinstalled every start.
        self.cli.omit_enabled = True
        self.stage({"a": "d1"})
        warnings: list[str] = []
        self.reconcile(warnings)
        self.assertEqual(len(warnings), 1)
        self.assertIn("is not registered as enabled", warnings[0])
        self.assertEqual(self.state(), {})

    def test_install_with_unreadable_config_names_the_cause(self) -> None:
        # Upstream review (gemini, GoogleCloudPlatform/scion#3735): install
        # reads the registration strictly, so an unparseable config.json is
        # reported as such rather than as "not registered as enabled". The
        # outcome is unchanged: not recorded, retried next start.
        self.cli.corrupt_after_install = True
        self.stage({"a": "d1"})
        warnings: list[str] = []
        self.reconcile(warnings)
        self.assertEqual(warnings, [
            "agent plugin a: install failed (copilot plugin install exit 0, but cannot read "
            "~/.copilot/config.json to confirm the result (ValueError)); will retry next start"])
        self.assertEqual(self.state(), {})
        self.assertEqual(self.status()["installed"], [])

    def test_install_without_install_dir_fails_loudly(self) -> None:
        self.cli.omit_cache_path = True
        self.cli.no_cache_dir = True
        self.stage({"a": "d1"})
        warnings: list[str] = []
        self.reconcile(warnings)
        self.assertEqual(len(warnings), 1)
        self.assertIn("registered a but no install dir was found", warnings[0])
        self.assertEqual(self.state(), {})
        self.assertEqual(self.status()["installed"], [])

    def test_is_present_requires_enabled_registration(self) -> None:
        self.stage({"a": "d1"})
        self.reconcile()
        inst = provision.CopilotPluginInstaller(self.ctx())
        ref = self.state()["a"]["native_ref"]
        self.assertTrue(inst.is_present("a", ref))
        data = self.config()
        data["installedPlugins"][0]["enabled"] = False
        self.cli._save(self.copilot_home, data)
        self.assertFalse(inst.is_present("a", ref))
        data["installedPlugins"][0]["enabled"] = True
        data["installedPlugins"][0]["marketplace"] = "awesome-copilot"
        self.cli._save(self.copilot_home, data)
        self.assertFalse(inst.is_present("a", ref), "a marketplace plugin of the same name is not ours")
        data["installedPlugins"][0]["marketplace"] = ""
        self.cli._save(self.copilot_home, data)
        self.assertTrue(inst.is_present("a", ref))
        import shutil
        shutil.rmtree(self.direct_dir("a"))
        self.assertFalse(inst.is_present("a", ref))

    def test_reset_config_reinstalls(self) -> None:
        # Regression: the image ships config.json as {"trustedFolders":
        # [...]} (Dockerfile) and a re-provision can reset the home, which
        # drops installedPlugins while the _direct cache and Scion's state
        # survive. The next start must reinstall, not take the fast path.
        self.stage({"scion-smoke": "d1"})
        provision.provision(self.ctx())
        self.assertTrue(os.path.isdir(self.direct_dir("scion-smoke")))
        with open(self.config_path, "w") as f:
            f.write('{\n  "trustedFolders": ["/workspace"]\n}\n')
        self.cli.calls.clear()
        provision.provision(self.ctx())
        self.assertEqual(self.cli.calls, [
            ["copilot", "--version"],
            ["copilot", "plugin", "install", self.bundle_dir("scion-smoke")],
        ])
        self.assertEqual([e["name"] for e in self.config()["installedPlugins"]], ["scion-smoke"])
        self.cli.calls.clear()
        provision.provision(self.ctx())
        self.assertEqual(self.cli.calls, [])

    def test_auth_file_config_rewrite_reinstalls(self) -> None:
        # An auth-file provision writes the staged COPILOT_CONFIG over
        # config.json on every start, dropping installedPlugins.
        secret = os.path.join(self.bundle, "secrets", "COPILOT_CONFIG")
        os.makedirs(os.path.dirname(secret))
        with open(secret, "w", encoding="utf-8") as f:
            json.dump({"copilot_tokens": {"https://github.com:octo": "placeholder-token"}}, f)
        with open(os.path.join(self.inputs, "auth-candidates.json"), "w") as f:
            json.dump({"schema_version": 1, "file_secret_files": {
                "COPILOT_CONFIG": "$HOME/.scion/harness/secrets/COPILOT_CONFIG"}}, f)
        self.stage({"a": "d1"})
        provision.provision(self.ctx())
        self.assertIn("installedPlugins", self.config())
        self.assertIn("copilot_tokens", self.config())
        self.cli.calls.clear()
        provision.provision(self.ctx())
        self.assertEqual(self.cli.calls[-1], ["copilot", "plugin", "install", self.bundle_dir("a")])
        self.assertIn("copilot_tokens", self.config())

    def test_provision_reconciles_after_ensure_settings(self) -> None:
        self.stage({"a": "d1"})
        provision.provision(self.ctx())
        with open(self.config_path) as f:
            raw = f.read()
        # The CLI wrote last (its header survives), and it kept the
        # trustedFolders entry _ensure_settings wrote before it.
        self.assertTrue(raw.startswith(_COPILOT_CONFIG_HEADER))
        data = self.config()
        self.assertEqual(data["trustedFolders"], [os.path.join(self.tmp, "workspace")])
        self.assertEqual([e["name"] for e in data["installedPlugins"]], ["a"])

    def test_plugin_registry_is_not_sign_in(self) -> None:
        # Regression: installedPlugins is a settings key. Without it in
        # _COPILOT_SETTINGS_KEYS, a restart after any plugin install would
        # look signed in (auth-file) with no credential.
        self.stage({"a": "d1"})
        warnings: list[str] = []
        provision.provision(self.ctx(warnings))
        self.assertFalse(provision._config_file_may_hold_credential(self.config_path))
        provision.provision(self.ctx())
        with open(os.path.join(self.bundle, "outputs", "resolved-auth.json")) as f:
            self.assertEqual(json.load(f)["method"], "none")
        # Also after the last plugin is removed (installedPlugins: []).
        self.stage({})
        provision.provision(self.ctx())
        self.assertEqual(self.config()["installedPlugins"], [])
        self.assertFalse(provision._config_file_may_hold_credential(self.config_path))

    def test_cli_timeouts_capped_by_budget(self) -> None:
        self.stage({"a": "d1"})
        ticks = iter([0.0] + [17.0] * 50)
        self.reconcile(budget_s=20.0, clock=lambda: next(ticks))
        # --version 10s default and install 15s default, both capped to 3s.
        self.assertEqual(self.cli.timeouts, [3.0, 3.0])

if __name__ == "__main__":
    unittest.main()
