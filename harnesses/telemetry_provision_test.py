"""Effective native telemetry provisioner output checks (configuration, not emission)."""
import importlib.util
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).parent


class TelemetryProvisionTest(unittest.TestCase):
    def _invoke(self, harness, enabled, port, provider=None, extra_env=None,
                well_known_gcp_credentials=False, cloud_endpoint=None,
                port_env_key='SCION_OTEL_GRPC_PORT',
                cloud_headers=None, cloud_ca_file=None):
        with tempfile.TemporaryDirectory() as tmp:
            home = Path(tmp)
            bundle = home / '.scion' / 'harness'
            (bundle / 'inputs').mkdir(parents=True)
            if well_known_gcp_credentials:
                (home / '.scion' / 'telemetry-gcp-credentials.json').write_text('{}')
            spec = importlib.util.spec_from_file_location('provision', ROOT / harness / 'provision.py')
            module = importlib.util.module_from_spec(spec)
            with patch.dict(os.environ, {'HOME': tmp}, clear=False):
                spec.loader.exec_module(module)
                ctx = module.scion_harness.ProvisionContext(harness, {
                    'harness_bundle_dir': str(bundle),
                    'harness_config': {'no_auth': {'behavior': 'allow'}},
                    # Never fall back to the real /workspace (ptone/scion#2993).
                    'agent_workspace': str(home / 'workspace'),
                })
                telemetry = {'enabled': enabled}
                if provider is not None or cloud_endpoint is not None or cloud_headers is not None or cloud_ca_file is not None:
                    telemetry['cloud'] = {}
                    if provider is not None:
                        telemetry['cloud']['provider'] = provider
                    if cloud_endpoint is not None:
                        telemetry['cloud']['endpoint'] = cloud_endpoint
                    if cloud_headers is not None:
                        telemetry['cloud']['headers'] = cloud_headers
                    if cloud_ca_file is not None:
                        telemetry['cloud']['tls'] = {'ca_file': cloud_ca_file}
                source_env = {port_env_key: str(port)}
                source_env.update(extra_env or {})
                (bundle / 'inputs' / 'telemetry.json').write_text(json.dumps({
                    'telemetry': telemetry, 'env': source_env,
                }))
                ctx.select_auth = lambda _: module.scion_harness.ResolvedAuth('none')
                module.provision(ctx)
                env = json.loads((bundle / 'outputs' / 'env.json').read_text())
                self.assertEqual(env['SCION_NATIVE_TELEMETRY_POLICY'], 'enabled' if enabled else 'disabled')
                config = None
                if harness == 'gemini-cli':
                    config = json.loads((home / '.gemini' / 'settings.json').read_text())['telemetry']
                if harness == 'codex':
                    config = (home / '.codex' / 'config.toml').read_text()
                return env, config

    def test_claude_default_custom_and_disabled(self):
        for enabled, port in ((True, 4317), (True, 14317), (False, 14317)):
            with self.subTest(enabled=enabled, port=port):
                env, _ = self._invoke('claude', enabled, port, provider='otlp')
                self.assertEqual(env['CLAUDE_CODE_ENABLE_TELEMETRY'], '1' if enabled else '0')
                self.assertEqual(env['OTEL_METRICS_EXPORTER'], 'otlp' if enabled else 'none')
                self.assertEqual(env['OTEL_LOGS_EXPORTER'], 'otlp' if enabled else 'none')
                self.assertEqual(env['OTEL_EXPORTER_OTLP_ENDPOINT'], f'http://127.0.0.1:{port}')

    def test_claude_gcp_logs_only_and_generic_metrics(self):
        for provider, enabled, port, metrics, logs in (
            ('gcp', True, 4317, 'none', 'otlp'),
            ('gcp', True, 14317, 'none', 'otlp'),
            ('gcp', False, 14317, 'none', 'none'),
            ('generic', True, 4317, 'otlp', 'otlp'),
            (None, True, 14317, 'otlp', 'otlp'),
        ):
            with self.subTest(provider=provider, enabled=enabled, port=port):
                env, _ = self._invoke('claude', enabled, port, provider=provider,
                                      cloud_endpoint='https://generic.invalid/v1' if provider is None else None)
                self.assertEqual(env['OTEL_METRICS_EXPORTER'], metrics)
                self.assertEqual(env['OTEL_LOGS_EXPORTER'], logs)
                self.assertEqual(env['OTEL_EXPORTER_OTLP_ENDPOINT'], f'http://127.0.0.1:{port}')
                self.assertEqual(env['OTEL_TRACES_EXPORTER'], 'none')

    def test_claude_staged_provider_and_implicit_gcp_credentials(self):
        env, _ = self._invoke('claude', True, 14317, extra_env={
            'SCION_TELEMETRY_CLOUD_PROVIDER': 'gcp',
        })
        self.assertEqual(env['OTEL_METRICS_EXPORTER'], 'none')
        with self.assertRaisesRegex(Exception, 'explicit telemetry cloud provider required'):
            self._invoke('claude', True, 14317, extra_env={
                'SCION_OTEL_GCP_CREDENTIALS': '/private/key.json',
            })
        with self.assertRaisesRegex(Exception, 'explicit telemetry cloud provider required'):
            self._invoke('claude', True, 14317, well_known_gcp_credentials=True)
        with self.assertRaisesRegex(Exception, 'explicit telemetry cloud provider required'):
            self._invoke('claude', True, 14317)
        with self.assertRaisesRegex(Exception, 'explicit telemetry cloud provider required'):
            self._invoke('claude', True, 14317, cloud_endpoint='https://generic.invalid/v1',
                         well_known_gcp_credentials=True)
        env, _ = self._invoke('claude', True, 14317, cloud_endpoint='https://generic.invalid/v1',
                              extra_env={'SCION_TELEMETRY_CLOUD_PROVIDER': 'gcp'})
        self.assertEqual(env['OTEL_METRICS_EXPORTER'], 'none')
        with self.assertRaisesRegex(Exception, 'conflicting telemetry cloud provider'):
            self._invoke('claude', True, 14317, provider='generic', extra_env={
                'SCION_TELEMETRY_CLOUD_PROVIDER': 'gcp',
            })
        disabled, _ = self._invoke('claude', False, 14317, provider='generic', extra_env={
            'SCION_TELEMETRY_CLOUD_PROVIDER': 'gcp',
        })
        self.assertEqual(disabled['OTEL_METRICS_EXPORTER'], 'none')
        self.assertEqual(disabled['OTEL_LOGS_EXPORTER'], 'none')

    def test_claude_sets_usage_source_native_only_when_enabled(self):
        enabled_env, _ = self._invoke('claude', True, 4317, provider='otlp')
        self.assertEqual(enabled_env['SCION_USAGE_SOURCE'], 'native')
        disabled_env, _ = self._invoke('claude', False, 4317, provider='otlp')
        self.assertNotIn('SCION_USAGE_SOURCE', disabled_env)

    def test_gemini_default_custom_and_disabled(self):
        for enabled, port in ((True, 4317), (True, 14317), (False, 14317)):
            with self.subTest(enabled=enabled, port=port):
                env, config = self._invoke('gemini-cli', enabled, port)
                self.assertEqual(env['GEMINI_TELEMETRY_ENABLED'], str(enabled).lower())
                self.assertEqual(config['enabled'], enabled)
                self.assertFalse(config['traces'])
                self.assertEqual(env['GEMINI_TELEMETRY_TRACES_ENABLED'], 'false')
                self.assertEqual(config['otlpEndpoint'], f'http://127.0.0.1:{port}')
                self.assertEqual(config['target'], 'local')
                self.assertNotIn('outfile', config)

    def test_gemini_sets_usage_source_native_only_when_enabled(self):
        # ptone/scion#2234: the native gemini_cli.api_response rule is
        # fixture-vetted, so gemini-cli now declares the D4/D10 opt-in the
        # same way claude, codex and copilot do.
        enabled_env, _ = self._invoke('gemini-cli', True, 4317)
        self.assertEqual(enabled_env['SCION_USAGE_SOURCE'], 'native')
        disabled_env, _ = self._invoke('gemini-cli', False, 4317)
        self.assertNotIn('SCION_USAGE_SOURCE', disabled_env)

    def test_codex_default_custom_and_disabled(self):
        for enabled, port in ((True, 4317), (True, 14317), (False, 14317)):
            with self.subTest(enabled=enabled, port=port):
                env, config = self._invoke('codex', enabled, port)
                self.assertTrue(env['CODEX_HOME'].endswith('/.codex'))
                self.assertIn('[otel]', config)
                if enabled:
                    self.assertIn(f'http://127.0.0.1:{port}', config)
                    self.assertIn('metrics_exporter."otlp-grpc".endpoint', config)
                else:
                    self.assertIn('metrics_exporter = "none"', config)
                    self.assertIn('trace_exporter = "none"', config)
                self.assertNotIn('statsig', config)
                self.assertNotIn('cloudtrace.googleapis.com', config)

    def test_codex_gcp_disables_native_metrics_but_keeps_logs(self):
        # ptone/scion#2053 design §3.7 "codex" bullet: native metrics stay
        # off on GCP, the same as claude (test_claude_gcp_logs_only_and_generic_metrics
        # above), narrow to metrics only -- logs and traces are unaffected.
        for provider, enabled, port in (
            ('gcp', True, 4317),
            ('gcp', True, 14317),
            ('gcp', False, 14317),
            ('generic', True, 14317),
        ):
            with self.subTest(provider=provider, enabled=enabled, port=port):
                env, config = self._invoke('codex', enabled, port, provider=provider)
                if not enabled:
                    self.assertIn('metrics_exporter = "none"', config)
                    self.assertIn('exporter = "none"', config)
                    self.assertIn('trace_exporter = "none"', config)
                elif provider == 'gcp':
                    self.assertIn('metrics_exporter = "none"', config)
                    self.assertIn(f'exporter."otlp-grpc".endpoint = "http://127.0.0.1:{port}"', config)
                    self.assertIn(f'trace_exporter."otlp-grpc".endpoint = "http://127.0.0.1:{port}"', config)
                else:
                    self.assertIn(f'metrics_exporter."otlp-grpc".endpoint = "http://127.0.0.1:{port}"', config)

    def test_codex_sets_usage_source_native_only_when_enabled(self):
        enabled_env, _ = self._invoke('codex', True, 4317)
        self.assertEqual(enabled_env['SCION_USAGE_SOURCE'], 'native')
        disabled_env, _ = self._invoke('codex', False, 4317)
        self.assertNotIn('SCION_USAGE_SOURCE', disabled_env)

    def test_copilot_default_custom_and_disabled(self):
        for enabled, port in ((True, 4318), (True, 14318), (False, 14318)):
            with self.subTest(enabled=enabled, port=port):
                env, _ = self._invoke('copilot', enabled, port, port_env_key='SCION_OTEL_HTTP_PORT')
                if enabled:
                    self.assertEqual(env['COPILOT_OTEL_ENABLED'], 'true')
                    self.assertEqual(env['COPILOT_OTEL_EXPORTER_TYPE'], 'otlp-http')
                    self.assertEqual(env['OTEL_EXPORTER_OTLP_ENDPOINT'], f'http://127.0.0.1:{port}')
                    self.assertEqual(env['OTEL_EXPORTER_OTLP_PROTOCOL'], 'http/protobuf')
                    self.assertEqual(env['OTEL_METRICS_EXPORTER'], 'otlp')
                    self.assertEqual(env['OTEL_LOGS_EXPORTER'], 'otlp')
                    self.assertEqual(env['OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE'], 'delta')
                else:
                    self.assertNotIn('COPILOT_OTEL_ENABLED', env)
                self.assertNotIn('OTEL_EXPORTER_OTLP_HEADERS', env)
                self.assertNotIn('OTEL_EXPORTER_OTLP_CERTIFICATE', env)

    def test_copilot_sets_usage_source_native_only_when_enabled(self):
        # ptone/scion#2053 phase 3a: copilot's usage rule is metric-sourced;
        # the deriver converts its cumulative-only metrics to deltas itself
        # (design §5). SCION_USAGE_SOURCE=native is the same D4/D10 opt-in
        # claude and codex already use.
        enabled_env, _ = self._invoke('copilot', True, 14318, port_env_key='SCION_OTEL_HTTP_PORT')
        self.assertEqual(enabled_env['SCION_USAGE_SOURCE'], 'native')
        disabled_env, _ = self._invoke('copilot', False, 14318, port_env_key='SCION_OTEL_HTTP_PORT')
        self.assertNotIn('SCION_USAGE_SOURCE', disabled_env)

    def test_copilot_never_reaches_cloud_endpoint(self):
        # #2053: copilot used to resolve SCION_OTEL_ENDPOINT (the generic
        # cloud-config alias) ahead of the local receiver, and to copy cloud
        # headers/CA into the harness env. Seed all of those inputs -- from
        # both the telemetry.cloud config and the env overlay -- so these
        # assertions would actually catch a regression, not just pass because
        # nothing was ever offered to copy.
        env, _ = self._invoke(
            'copilot', True, 14318, port_env_key='SCION_OTEL_HTTP_PORT',
            cloud_endpoint='https://generic.invalid/v1',
            cloud_headers={'authorization': 'Bearer cloud-token'},
            cloud_ca_file='/cloud/ca.pem',
            extra_env={
                'SCION_OTEL_ENDPOINT': 'cloudtrace.googleapis.com:443',
                'SCION_OTEL_HEADERS': json.dumps({'authorization': 'Bearer env-token'}),
                'SCION_OTEL_CA_FILE': '/env/ca.pem',
            },
        )
        self.assertEqual(env['OTEL_EXPORTER_OTLP_ENDPOINT'], 'http://127.0.0.1:14318')
        self.assertNotIn('OTEL_EXPORTER_OTLP_HEADERS', env)
        self.assertNotIn('OTEL_EXPORTER_OTLP_CERTIFICATE', env)

    def test_grok_build_default_custom_and_disabled(self):
        for enabled, port in ((True, 4317), (True, 14317), (False, 14317)):
            with self.subTest(enabled=enabled, port=port):
                env, _ = self._invoke('grok-build', enabled, port)
                if enabled:
                    self.assertEqual(env['GROK_TELEMETRY_ENABLED'], 'true')
                    self.assertEqual(env['GROK_EXTERNAL_OTEL'], 'true')
                    self.assertEqual(env['OTEL_EXPORTER_OTLP_ENDPOINT'], f'http://127.0.0.1:{port}')
                    self.assertEqual(env['OTEL_METRICS_EXPORTER'], 'otlp')
                    self.assertEqual(env['OTEL_LOGS_EXPORTER'], 'otlp')
                else:
                    self.assertNotIn('GROK_TELEMETRY_ENABLED', env)
                self.assertNotIn('OTEL_EXPORTER_OTLP_HEADERS', env)
                self.assertNotIn('OTEL_EXPORTER_OTLP_CERTIFICATE', env)

    def test_grok_build_never_reaches_cloud_endpoint(self):
        # #2053: grok-build had the same cloud-endpoint/header/CA bypass as
        # copilot. Seed all of those inputs (see test_copilot_never_reaches_
        # cloud_endpoint) so the assertions are meaningful.
        env, _ = self._invoke(
            'grok-build', True, 14317,
            cloud_endpoint='https://generic.invalid/v1',
            cloud_headers={'authorization': 'Bearer cloud-token'},
            cloud_ca_file='/cloud/ca.pem',
            extra_env={
                'SCION_OTEL_ENDPOINT': 'cloudtrace.googleapis.com:443',
                'SCION_OTEL_HEADERS': json.dumps({'authorization': 'Bearer env-token'}),
                'SCION_OTEL_CA_FILE': '/env/ca.pem',
            },
        )
        self.assertEqual(env['OTEL_EXPORTER_OTLP_ENDPOINT'], 'http://127.0.0.1:14317')
        self.assertNotIn('OTEL_EXPORTER_OTLP_HEADERS', env)
        self.assertNotIn('OTEL_EXPORTER_OTLP_CERTIFICATE', env)

    def test_installed_codex_0154_loads_generated_config(self):
        binary = shutil.which('codex')
        if not binary or subprocess.run([binary, '--version'], capture_output=True, text=True).stdout.strip() != 'codex-cli 0.154.0':
            self.skipTest('pinned Codex CLI 0.154.0 unavailable')
        spec = importlib.util.spec_from_file_location('codex_provision', ROOT / 'codex' / 'provision.py')
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        for enabled in (True, False):
            with self.subTest(enabled=enabled), tempfile.TemporaryDirectory() as home:
                with patch.dict(os.environ, {'HOME': home}, clear=False):
                    module._reconcile_codex_toml({'enabled': enabled}, {'SCION_OTEL_GRPC_PORT': '14317'})
                    result = subprocess.run([binary, 'features', 'list'], capture_output=True, text=True)
                self.assertEqual(result.returncode, 0, result.stderr)

    def test_conflicting_inherited_values_do_not_change_generated_files(self):
        # The supervisor must separately reject conflicts before child launch.
        inherited = {
            'OTEL_EXPORTER_OTLP_ENDPOINT': 'https://external.invalid:443',
            'GEMINI_TELEMETRY_OTLP_ENDPOINT': 'https://external.invalid:443',
            'GEMINI_TELEMETRY_OUTFILE': '/tmp/bypass.json',
            'CODEX_HOME': '/tmp/bypass-codex',
            'SCION_CODEX_OTEL_ENDPOINT': 'https://external.invalid:443',
            'SCION_OTEL_ENDPOINT': 'https://external.invalid:443',
        }
        with patch.dict(os.environ, inherited):
            claude_env, _ = self._invoke('claude', True, 14317, provider='otlp')
            gemini_env, gemini_config = self._invoke('gemini-cli', True, 14317)
            codex_env, codex_config = self._invoke('codex', True, 14317)
            copilot_env, _ = self._invoke('copilot', True, 14318, port_env_key='SCION_OTEL_HTTP_PORT')
            grok_env, _ = self._invoke('grok-build', True, 14317)
        self.assertEqual(claude_env['OTEL_EXPORTER_OTLP_ENDPOINT'], 'http://127.0.0.1:14317')
        self.assertEqual(gemini_env['GEMINI_TELEMETRY_OTLP_ENDPOINT'], 'http://127.0.0.1:14317')
        self.assertEqual(gemini_config['otlpEndpoint'], 'http://127.0.0.1:14317')
        self.assertEqual(gemini_env['GEMINI_TELEMETRY_OUTFILE'], '')
        self.assertNotEqual(codex_env['CODEX_HOME'], inherited['CODEX_HOME'])
        self.assertIn('http://127.0.0.1:14317', codex_config)
        self.assertEqual(copilot_env['OTEL_EXPORTER_OTLP_ENDPOINT'], 'http://127.0.0.1:14318')
        self.assertEqual(grok_env['OTEL_EXPORTER_OTLP_ENDPOINT'], 'http://127.0.0.1:14317')


if __name__ == '__main__':
    unittest.main()
