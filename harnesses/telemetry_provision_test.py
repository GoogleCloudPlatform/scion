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
                well_known_gcp_credentials=False, cloud_endpoint=None):
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
                })
                telemetry = {'enabled': enabled}
                if provider is not None or cloud_endpoint is not None:
                    telemetry['cloud'] = {}
                    if provider is not None:
                        telemetry['cloud']['provider'] = provider
                    if cloud_endpoint is not None:
                        telemetry['cloud']['endpoint'] = cloud_endpoint
                source_env = {'SCION_OTEL_GRPC_PORT': str(port)}
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
        }
        with patch.dict(os.environ, inherited):
            claude_env, _ = self._invoke('claude', True, 14317, provider='otlp')
            gemini_env, gemini_config = self._invoke('gemini-cli', True, 14317)
            codex_env, codex_config = self._invoke('codex', True, 14317)
        self.assertEqual(claude_env['OTEL_EXPORTER_OTLP_ENDPOINT'], 'http://127.0.0.1:14317')
        self.assertEqual(gemini_env['GEMINI_TELEMETRY_OTLP_ENDPOINT'], 'http://127.0.0.1:14317')
        self.assertEqual(gemini_config['otlpEndpoint'], 'http://127.0.0.1:14317')
        self.assertEqual(gemini_env['GEMINI_TELEMETRY_OUTFILE'], '')
        self.assertNotEqual(codex_env['CODEX_HOME'], inherited['CODEX_HOME'])
        self.assertIn('http://127.0.0.1:14317', codex_config)


if __name__ == '__main__':
    unittest.main()
