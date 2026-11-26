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
    def setUp(self):
        # Scrub ambient telemetry/usage-ingest env vars so tests are hermetic
        # inside Scion agent containers where sciontool sets them (ptone/scion#9492).
        env_patch = patch.dict(os.environ, {}, clear=False)
        env_patch.start()
        self.addCleanup(env_patch.stop)
        for key in (
            'SCION_USAGE_HUB_INGEST',
            'SCION_NATIVE_METERING_SUPPORTED',
            'SCION_TELEMETRY_ENABLED',
            'SCION_OTEL_GCP_CREDENTIALS',
            'SCION_OTEL_ENDPOINT',
            'SCION_TELEMETRY_CLOUD_PROVIDER',
        ):
            os.environ.pop(key, None)

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

    def _invoke_with_env(self, harness, telemetry_config, port,
                         port_env_key='SCION_OTEL_GRPC_PORT',
                         extra_staged_env=None, process_env=None,
                         well_known_gcp_credentials=False):
        with tempfile.TemporaryDirectory() as tmp:
            home = Path(tmp)
            bundle = home / '.scion' / 'harness'
            (bundle / 'inputs').mkdir(parents=True)
            if well_known_gcp_credentials:
                (home / '.scion' / 'telemetry-gcp-credentials.json').write_text('{}')
            spec = importlib.util.spec_from_file_location('provision', ROOT / harness / 'provision.py')
            module = importlib.util.module_from_spec(spec)
            env_patch = {'HOME': tmp}
            if process_env:
                env_patch.update(process_env)
            with patch.dict(os.environ, env_patch, clear=False):
                # Ensure ambient SCION_USAGE_HUB_INGEST / SCION_NATIVE_METERING_SUPPORTED /
                # SCION_TELEMETRY_ENABLED don't leak from the outer environment unless explicitly set.
                for key in ('SCION_USAGE_HUB_INGEST', 'SCION_NATIVE_METERING_SUPPORTED', 'SCION_TELEMETRY_ENABLED'):
                    if not process_env or key not in process_env:
                        os.environ.pop(key, None)
                spec.loader.exec_module(module)
                ctx = module.scion_harness.ProvisionContext(harness, {
                    'harness_bundle_dir': str(bundle),
                    'harness_config': {'no_auth': {'behavior': 'allow'}},
                    'agent_workspace': str(home / 'workspace'),
                })
                source_env = {port_env_key: str(port)}
                source_env.update(extra_staged_env or {})
                (bundle / 'inputs' / 'telemetry.json').write_text(json.dumps({
                    'telemetry': telemetry_config,
                    'env': source_env,
                }))
                ctx.select_auth = lambda _: module.scion_harness.ResolvedAuth('none')
                module.provision(ctx)
                env = json.loads((bundle / 'outputs' / 'env.json').read_text())
                config = None
                if harness == 'gemini-cli':
                    config = json.loads((home / '.gemini' / 'settings.json').read_text())['telemetry']
                if harness == 'codex':
                    config = (home / '.codex' / 'config.toml').read_text()
                return env, config

    def test_hub_usage_ingest_auto_enables_loopback_only_on_default_config(self):
        # ptone/scion#9492: with default/unset telemetry (telemetry: null),
        # SCION_USAGE_HUB_INGEST=1, and SCION_NATIVE_METERING_SUPPORTED=1,
        # covered harnesses auto-enable loopback-only native OTel to sciontool
        # with no cloud provider required and no prompt logging enabled.
        metering_env = {'SCION_USAGE_HUB_INGEST': '1', 'SCION_NATIVE_METERING_SUPPORTED': '1'}
        claude_env, _ = self._invoke_with_env(
            'claude', None, 14317, process_env=metering_env,
        )
        self.assertEqual(claude_env['SCION_NATIVE_TELEMETRY_POLICY'], 'metering')
        self.assertEqual(claude_env['CLAUDE_CODE_ENABLE_TELEMETRY'], '1')
        self.assertEqual(claude_env['OTEL_LOGS_EXPORTER'], 'otlp')
        self.assertEqual(claude_env['OTEL_METRICS_EXPORTER'], 'none')
        self.assertEqual(claude_env['OTEL_TRACES_EXPORTER'], 'none')
        self.assertEqual(claude_env['OTEL_EXPORTER_OTLP_ENDPOINT'], 'http://127.0.0.1:14317')
        self.assertEqual(claude_env['SCION_USAGE_SOURCE'], 'native')
        self.assertNotIn('OTEL_LOG_USER_PROMPTS', claude_env)

        codex_env, codex_cfg = self._invoke_with_env(
            'codex', None, 14317, process_env=metering_env,
        )
        self.assertEqual(codex_env['SCION_NATIVE_TELEMETRY_POLICY'], 'metering')
        self.assertEqual(codex_env['SCION_USAGE_SOURCE'], 'native')
        self.assertIn('exporter."otlp-grpc".endpoint = "http://127.0.0.1:14317"', codex_cfg)
        self.assertIn('metrics_exporter = "none"', codex_cfg)
        self.assertIn('trace_exporter = "none"', codex_cfg)
        self.assertIn('log_user_prompt = false', codex_cfg)

        gemini_env, gemini_cfg = self._invoke_with_env(
            'gemini-cli', None, 14317, process_env=metering_env,
        )
        self.assertEqual(gemini_env['SCION_NATIVE_TELEMETRY_POLICY'], 'metering')
        self.assertEqual(gemini_env['GEMINI_TELEMETRY_ENABLED'], 'true')
        self.assertEqual(gemini_env['GEMINI_TELEMETRY_TARGET'], 'local')
        self.assertEqual(gemini_env['GEMINI_TELEMETRY_OTLP_ENDPOINT'], 'http://127.0.0.1:14317')
        self.assertEqual(gemini_env['GEMINI_TELEMETRY_LOG_PROMPTS'], 'false')
        self.assertEqual(gemini_env['GEMINI_TELEMETRY_TRACES_ENABLED'], 'false')
        self.assertEqual(gemini_env['SCION_USAGE_SOURCE'], 'native')
        self.assertTrue(gemini_cfg['enabled'])
        self.assertFalse(gemini_cfg['logPrompts'])
        self.assertFalse(gemini_cfg['traces'])

        copilot_env, _ = self._invoke_with_env(
            'copilot', None, 14318, port_env_key='SCION_OTEL_HTTP_PORT',
            process_env=metering_env,
        )
        self.assertEqual(copilot_env['SCION_NATIVE_TELEMETRY_POLICY'], 'metering')
        self.assertEqual(copilot_env['COPILOT_OTEL_ENABLED'], 'true')
        self.assertEqual(copilot_env['OTEL_EXPORTER_OTLP_ENDPOINT'], 'http://127.0.0.1:14318')
        self.assertEqual(copilot_env['OTEL_METRICS_EXPORTER'], 'otlp')
        self.assertEqual(copilot_env['OTEL_LOGS_EXPORTER'], 'none')
        self.assertEqual(copilot_env['OTEL_TRACES_EXPORTER'], 'none')
        self.assertEqual(copilot_env['SCION_USAGE_SOURCE'], 'native')

        # grok-build has no native usage rule in sciontool yet; stays off.
        grok_env, _ = self._invoke_with_env(
            'grok-build', None, 14317, process_env=metering_env,
        )
        self.assertEqual(grok_env['SCION_NATIVE_TELEMETRY_POLICY'], 'disabled')
        self.assertNotIn('GROK_TELEMETRY_ENABLED', grok_env)

    def test_hub_usage_ingest_without_metering_supported_marker_never_emits_metering(self):
        # O1 compatibility (ptone/scion#9492): an older P1-era sciontool sets
        # SCION_USAGE_HUB_INGEST=1 but does not set SCION_NATIVE_METERING_SUPPORTED=1
        # and rejects SCION_NATIVE_TELEMETRY_POLICY=metering at init. Without
        # the marker, provisioners must stay on enabled/disabled, never metering.
        for tel_cfg, want_policy in (
            (None, 'disabled'),
            ({'enabled': True, 'cloud': {'provider': 'otlp'}}, 'enabled'),
        ):
            for harness, port_key, port in (
                ('claude', 'SCION_OTEL_GRPC_PORT', 4317),
                ('codex', 'SCION_OTEL_GRPC_PORT', 4317),
                ('gemini-cli', 'SCION_OTEL_GRPC_PORT', 4317),
                ('copilot', 'SCION_OTEL_HTTP_PORT', 4318),
            ):
                with self.subTest(tel_cfg=tel_cfg, harness=harness):
                    env, _ = self._invoke_with_env(
                        harness, tel_cfg, port, port_env_key=port_key,
                        process_env={'SCION_USAGE_HUB_INGEST': '1'},
                    )
                    self.assertEqual(env['SCION_NATIVE_TELEMETRY_POLICY'], want_policy)
                    self.assertNotEqual(env['SCION_NATIVE_TELEMETRY_POLICY'], 'metering')

    def test_hub_usage_ingest_unset_telemetry_with_cloud_creds_or_endpoint_emits_metering_policy(self):
        # R1 (ptone/scion#9492): when telemetry is unset (None),
        # SCION_USAGE_HUB_INGEST=1, and SCION_NATIVE_METERING_SUPPORTED=1, even
        # if ambient cloud credentials or an OTLP endpoint exist, provisioners
        # must emit SCION_NATIVE_TELEMETRY_POLICY=metering so sciontool derives
        # usage and never forwards native harness signals to cloud.
        base = {'SCION_USAGE_HUB_INGEST': '1', 'SCION_NATIVE_METERING_SUPPORTED': '1'}
        scenarios = (
            ('well_known_gcp_creds', {}, dict(base), True),
            ('process_gcp_creds', {}, {**base, 'SCION_OTEL_GCP_CREDENTIALS': '/tmp/key.json'}, False),
            ('staged_otel_endpoint', {'SCION_OTEL_ENDPOINT': 'https://collector.invalid:4317'}, dict(base), False),
            ('process_otel_endpoint', {}, {**base, 'SCION_OTEL_ENDPOINT': 'https://collector.invalid:4317'}, False),
        )
        for name, staged_env, proc_env, well_known in scenarios:
            for harness, port_key, port in (
                ('claude', 'SCION_OTEL_GRPC_PORT', 4317),
                ('codex', 'SCION_OTEL_GRPC_PORT', 4317),
                ('gemini-cli', 'SCION_OTEL_GRPC_PORT', 4317),
                ('copilot', 'SCION_OTEL_HTTP_PORT', 4318),
            ):
                with self.subTest(scenario=name, harness=harness):
                    env, _ = self._invoke_with_env(
                        harness, None, port, port_env_key=port_key,
                        extra_staged_env=staged_env, process_env=proc_env,
                        well_known_gcp_credentials=well_known,
                    )
                    self.assertEqual(env['SCION_NATIVE_TELEMETRY_POLICY'], 'metering')
                    self.assertEqual(env['SCION_USAGE_SOURCE'], 'native')
                    if harness == 'claude':
                        self.assertEqual(env['OTEL_METRICS_EXPORTER'], 'none')
                        self.assertEqual(env['OTEL_LOGS_EXPORTER'], 'otlp')
                        self.assertNotIn('OTEL_LOG_USER_PROMPTS', env)

    def test_hub_usage_ingest_off_preserves_default_disabled(self):
        for flag in (None, '0'):
            proc_env = {'SCION_USAGE_HUB_INGEST': flag} if flag is not None else {}
            for harness, port_key, port in (
                ('claude', 'SCION_OTEL_GRPC_PORT', 4317),
                ('codex', 'SCION_OTEL_GRPC_PORT', 4317),
                ('gemini-cli', 'SCION_OTEL_GRPC_PORT', 4317),
                ('copilot', 'SCION_OTEL_HTTP_PORT', 4318),
            ):
                with self.subTest(flag=flag, harness=harness):
                    env, _ = self._invoke_with_env(
                        harness, None, port, port_env_key=port_key, process_env=proc_env,
                    )
                    self.assertEqual(env['SCION_NATIVE_TELEMETRY_POLICY'], 'disabled')
                    self.assertNotIn('SCION_USAGE_SOURCE', env)

    def test_claude_enabled_true_without_cloud_provider_uses_loopback_when_hub_active(self):
        # Gate 1 scenario: telemetry.enabled=true with no cloud provider must
        # succeed in loopback-only mode when SCION_USAGE_HUB_INGEST=1, and keep
        # failing when SCION_USAGE_HUB_INGEST=0.
        metering_env = {'SCION_USAGE_HUB_INGEST': '1', 'SCION_NATIVE_METERING_SUPPORTED': '1'}
        env, _ = self._invoke_with_env(
            'claude', {'enabled': True}, 14317,
            process_env=metering_env,
        )
        self.assertEqual(env['SCION_NATIVE_TELEMETRY_POLICY'], 'metering')
        self.assertEqual(env['CLAUDE_CODE_ENABLE_TELEMETRY'], '1')
        self.assertEqual(env['OTEL_LOGS_EXPORTER'], 'otlp')
        self.assertEqual(env['OTEL_METRICS_EXPORTER'], 'none')
        self.assertEqual(env['SCION_USAGE_SOURCE'], 'native')

        with self.assertRaisesRegex(Exception, 'explicit telemetry cloud provider required'):
            self._invoke_with_env(
                'claude', {'enabled': True}, 14317,
                process_env={'SCION_USAGE_HUB_INGEST': '0', 'SCION_NATIVE_METERING_SUPPORTED': '1'},
            )

        # Safety invariant preserved: if GCP credentials are present without an
        # explicit cloud provider, Claude still aborts even with Hub ingest on.
        with self.assertRaisesRegex(Exception, 'explicit telemetry cloud provider required'):
            self._invoke_with_env(
                'claude', {'enabled': True}, 14317,
                process_env=metering_env,
                well_known_gcp_credentials=True,
            )

    def test_explicit_telemetry_disabled_overrides_hub_usage_ingest(self):
        # Explicit telemetry.enabled=false at any layer (telemetry.json, staged
        # env, or process env) must override SCION_USAGE_HUB_INGEST=1.
        base = {'SCION_USAGE_HUB_INGEST': '1', 'SCION_NATIVE_METERING_SUPPORTED': '1'}
        cases = (
            ('telemetry_json_false', {'enabled': False}, {}, dict(base)),
            ('staged_env_false', None, {'SCION_TELEMETRY_ENABLED': 'false'}, dict(base)),
            ('process_env_false', None, {}, {**base, 'SCION_TELEMETRY_ENABLED': 'false'}),
        )
        for name, tel_cfg, staged_env, proc_env in cases:
            for harness, port_key, port in (
                ('claude', 'SCION_OTEL_GRPC_PORT', 4317),
                ('codex', 'SCION_OTEL_GRPC_PORT', 4317),
                ('gemini-cli', 'SCION_OTEL_GRPC_PORT', 4317),
                ('copilot', 'SCION_OTEL_HTTP_PORT', 4318),
            ):
                with self.subTest(case=name, harness=harness):
                    env, cfg = self._invoke_with_env(
                        harness, tel_cfg, port, port_env_key=port_key,
                        extra_staged_env=staged_env, process_env=proc_env,
                    )
                    self.assertEqual(env['SCION_NATIVE_TELEMETRY_POLICY'], 'disabled')
                    self.assertNotIn('SCION_USAGE_SOURCE', env)
                    if harness == 'claude':
                        self.assertEqual(env['CLAUDE_CODE_ENABLE_TELEMETRY'], '0')
                    elif harness == 'codex':
                        self.assertIn('exporter = "none"', cfg)
                    elif harness == 'gemini-cli':
                        self.assertEqual(env['GEMINI_TELEMETRY_ENABLED'], 'false')
                        self.assertFalse(cfg['enabled'])
                    elif harness == 'copilot':
                        self.assertNotIn('COPILOT_OTEL_ENABLED', env)


if __name__ == '__main__':
    unittest.main()
