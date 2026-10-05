// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// schemaV1DriftKeysDoc is a settings.yaml that uses every key added to
// settings-v1.schema.json to close the drift TestSettingsSchema_NoDriftFromGoTypes
// found (ptone/scion#2654, ptone/scion#2285, ptone/scion#2259). Each of these keys is accepted by
// the Go settings types, so a file using them must validate.
const schemaV1DriftKeysDoc = `schema_version: "1"
project_type: shadow
workspace_path: /home/user/src/app
default_runtime_broker: broker-1
default_timezone: America/Los_Angeles
auto_inject_gcloud_adc: true
auto_expose_ports:
  enabled: false
project_defaults:
  default_scratchpad: false
shared_dirs:
  - name: build-cache
    read_only: true
    in_workspace: true
managed_agents:
  google:
    api_key: secret
    base_agent: base
    model: m
hub:
  linked: true
harness_configs:
  claude:
    harness: claude
    image: example.com/claude:latest
    secrets:
      - key: gcloud-adc
        type: file
        alternative_env_keys: [GOOGLE_APPLICATION_CREDENTIALS]
profiles:
  local:
    runtime: k8s
    secrets:
      - key: TOKEN
        alternative_env_keys: [ALT_TOKEN]
runtimes:
  k8s:
    type: kubernetes
    list_all_namespaces: true
  cr:
    type: cloudrun
    cloudrun:
      project_id: p
      location: us-central1
      service_account: sa@p.iam.gserviceaccount.com
      network: n
      subnetwork: s
      nfs_server: 10.0.0.2
      nfs_export: /export
  cri:
    type: cloudrun-instances
    cloudrun_instances:
      project_id: p
      region: us-central1
  crs:
    type: cloudrun-sandbox
    cloudrun_sandbox:
      sandbox_bin: /usr/local/gcp/bin/sandbox
server:
  mode: hosted
  maintenance:
    deployment_tier: binary
    release_channel: stable
    update_policy: notify
    check_interval_hours: 6
    github_repo: owner/repo
  notification_channels:
    - type: slack
      params:
        webhook_url: https://example.com/hook
      filter_types: [stalled]
      filter_urgent_only: true
  message_broker:
    enabled: true
    type: inprocess
    types: [inprocess]
  native_chat:
    enabled: false
  plugins:
    broker:
      nats:
        path: /usr/local/bin/nats-plugin
        config:
          url: nats://localhost
        config_file: /etc/nats.yaml
        self_managed: false
        address: localhost:9000
        mode: grpc
        tls_cert_file: c.pem
        tls_key_file: k.pem
        tls_ca_file: ca.pem
        tls_skip_verify: false
        auth_type: google_id_token
        auth_audience: https://bridge.example.com
  github_app:
    app_id: 12345
    private_key_path: /etc/key.pem
    private_key: pem
    webhook_secret: s
    api_base_url: https://api.github.com
    webhooks_enabled: true
    installation_url: https://github.com/apps/x/installations/new
  oidc_login:
    enabled: true
    display_name: Corp SSO
    issuer_url: https://sso.example.com
    client_id: id
    client_secret: secret
    scopes: [openid, email]
  oidc:
    issuer_url: https://hub.example.com
    enabled: true
    token_lifetime: 15m
  federation:
    enabled: true
    trusted_issuers:
      - issuer_url: https://other-hub.example.com
        jwks_url: https://other-hub.example.com/jwks
        expected_audience: aud
        allowed_projects: [p1]
        allowed_root_users: [root@example.com]
        default_scopes: [read]
        issuer_type: hub
        default_role: viewer
        allowed_emails: [a@example.com]
        allowed_gcp_projects: [gp]
        allowed_domains: [example.com]
    algorithms: [RS256]
    refresh_interval: 1h
    debounce_interval: 30s
`

func TestValidateSettings_DriftKeysValidate(t *testing.T) {
	errs, err := ValidateSettings([]byte(schemaV1DriftKeysDoc), "1")
	require.NoError(t, err)
	assert.Empty(t, errs, "every Go-accepted settings key should validate, got: %v", errs)

	// The same document must also load into the Go types with the values
	// intact, so the schema keys are the keys the loader actually reads.
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "settings.yaml"), []byte(schemaV1DriftKeysDoc), 0o644))
	vs, err := loadVersionedSettingsFileOnly(dir)
	require.NoError(t, err)
	require.NotNil(t, vs.AutoExposePorts)
	require.NotNil(t, vs.AutoExposePorts.Enabled)
	assert.False(t, *vs.AutoExposePorts.Enabled)
	require.NotNil(t, vs.ProjectDefaults)
	require.NotNil(t, vs.ProjectDefaults.DefaultScratchpad)
	assert.False(t, *vs.ProjectDefaults.DefaultScratchpad)
	require.NotNil(t, vs.Server)
	require.NotNil(t, vs.Server.Maintenance)
	assert.Equal(t, "binary", vs.Server.Maintenance.DeploymentTier)
	assert.Equal(t, 6, vs.Server.Maintenance.CheckIntervalHours)
	assert.True(t, vs.Runtimes["k8s"].ListAllNamespaces)
	require.NotNil(t, vs.Runtimes["cri"].CloudRunInstances)
	assert.Equal(t, "us-central1", vs.Runtimes["cri"].CloudRunInstances.Region)
	require.NotNil(t, vs.Runtimes["crs"].CloudRunSandbox)
	assert.Equal(t, "/usr/local/gcp/bin/sandbox", vs.Runtimes["crs"].CloudRunSandbox.SandboxBin)
	require.NotNil(t, vs.Runtimes["cr"].CloudRun)
	assert.Equal(t, "us-central1", vs.Runtimes["cr"].CloudRun.Location)
}

// TestValidateSettings_DriftKeysRejectBadInput mirrors the quotas
// invalid-type / unknown-field tests for the sections added alongside the
// drift guard.
func TestValidateSettings_DriftKeysRejectBadInput(t *testing.T) {
	// want is a substring that must appear in one error's path or message,
	// naming the key under test, so an unrelated error cannot satisfy a case.
	// For an unknown field it is the closed object's path: the validator
	// reports additionalProperties failures under a $ref as "<object path>:
	// validation failed" without the field name.
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{"auto_expose_ports.enabled non-boolean", "auto_expose_ports:\n  enabled: \"yes\"\n", "auto_expose_ports/enabled"},
		{"auto_expose_ports unknown field", "auto_expose_ports:\n  unknown_field: true\n", "auto_expose_ports"},
		{"project_defaults.default_scratchpad non-boolean", "project_defaults:\n  default_scratchpad: \"yes\"\n", "project_defaults/default_scratchpad"},
		{"project_defaults unknown field", "project_defaults:\n  unknown_field: true\n", "project_defaults"},
		{"server.maintenance.check_interval_hours non-integer", "server:\n  maintenance:\n    check_interval_hours: soon\n", "server/maintenance/check_interval_hours"},
		{"server.maintenance unknown field", "server:\n  maintenance:\n    unknown_field: x\n", "server/maintenance"},
		{"server.mode unknown value", "server:\n  mode: cluster\n", "server/mode"},
		{"runtimes.*.list_all_namespaces non-boolean", "runtimes:\n  k8s:\n    type: kubernetes\n    list_all_namespaces: \"yes\"\n", "runtimes/k8s/list_all_namespaces"},
		{"runtimes.*.cloudrun unknown field", "runtimes:\n  cr:\n    type: cloudrun\n    cloudrun:\n      region: us-central1\n", "runtimes/cr/cloudrun"},
		{"runtimes.*.cloudrun_instances unknown field", "runtimes:\n  cri:\n    type: cloudrun-instances\n    cloudrun_instances:\n      location: us-central1\n", "runtimes/cri/cloudrun_instances"},
		{"runtimes.*.cloudrun_sandbox unknown field", "runtimes:\n  crs:\n    type: cloudrun-sandbox\n    cloudrun_sandbox:\n      bin: x\n", "runtimes/crs/cloudrun_sandbox"},
		{"shared_dirs entry without name", "shared_dirs:\n  - read_only: true\n", "shared_dirs/0"},
		{"shared_dirs invalid name", "shared_dirs:\n  - name: Build_Cache\n", "shared_dirs/0/name"},
		{"federation trusted issuer without issuer_url", "server:\n  federation:\n    trusted_issuers:\n      - jwks_url: https://x\n", "server/federation/trusted_issuers/0"},
		{"federation trusted issuer empty issuer_url", "server:\n  federation:\n    trusted_issuers:\n      - issuer_url: \"\"\n", "trusted_issuers/0/issuer_url"},
		{"federation unknown issuer_type", "server:\n  federation:\n    trusted_issuers:\n      - issuer_url: https://x\n        issuer_type: robot\n", "trusted_issuers/0/issuer_type"},
		{"federation unsupported algorithm", "server:\n  federation:\n    algorithms: [HS256]\n", "server/federation/algorithms/0"},
		{"federation refresh_interval without unit", "server:\n  federation:\n    refresh_interval: \"3600\"\n", "server/federation/refresh_interval"},
		{"federation refresh_interval unquoted non-zero integer", "server:\n  federation:\n    refresh_interval: 5\n", "server/federation/refresh_interval"},
		{"federation debounce_interval not a duration", "server:\n  federation:\n    debounce_interval: soon\n", "server/federation/debounce_interval"},
		{"oidc token_lifetime empty", "server:\n  oidc:\n    token_lifetime: \"\"\n", "server/oidc/token_lifetime"},
		{"notification params number", "server:\n  notification_channels:\n    - type: webhook\n      params:\n        retries: 3\n", "server/notification_channels/0/params/retries"},
		{"oidc token_lifetime quoted integer", "server:\n  oidc:\n    token_lifetime: \"900000000000\"\n", "server/oidc/token_lifetime"},
		{"server.shared_dir_storage.nfs.auto_mount (ignored there)", "server:\n  shared_dir_storage:\n    nfs:\n      auto_mount: true\n", "server/shared_dir_storage/nfs"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := []byte("schema_version: \"1\"\n" + tt.yaml)
			errs, err := ValidateSettings(data, "1")
			require.NoError(t, err)
			require.NotEmpty(t, errs, "expected a validation error")
			found := false
			for _, e := range errs {
				if strings.Contains(e.Path+" "+e.Message, tt.want) {
					found = true
				}
			}
			assert.True(t, found, "expected an error naming %q, got: %v", tt.want, errs)
		})
	}
}

// TestValidateSettings_DurationsAndWeakMapValues covers values the loader
// accepts and the schema must therefore accept too: Go duration strings
// (time.ParseDuration accepts "0", "-0" and "+0"; an empty federation
// interval means the default), integer nanoseconds for token_lifetime, and
// unquoted numbers and booleans in plugin config, which decode weakly to
// their string form. Notification params are string-only (see the reject
// cases): the hub decodes seeded channels strictly.
func TestValidateSettings_DurationsAndWeakMapValues(t *testing.T) {
	tests := []struct {
		name string
		yaml string
	}{
		{"token_lifetime 15m", "server:\n  oidc:\n    token_lifetime: 15m\n"},
		{"token_lifetime 1h30m", "server:\n  oidc:\n    token_lifetime: 1h30m\n"},
		{"token_lifetime integer nanoseconds", "server:\n  oidc:\n    token_lifetime: 900000000000\n"},
		{"federation intervals", "server:\n  federation:\n    refresh_interval: 1h30m\n    debounce_interval: 500ms\n"},
		{"federation interval 0", "server:\n  federation:\n    refresh_interval: \"0\"\n"},
		{"plugin config number and boolean", "server:\n  plugins:\n    broker:\n      nats:\n        config:\n          port: 4222\n          tls: true\n          url: nats://x\n"},
		{"federation intervals empty (default)", "server:\n  federation:\n    refresh_interval: \"\"\n    debounce_interval: \"\"\n"},
		{"federation intervals unquoted 0", "server:\n  federation:\n    refresh_interval: 0\n    debounce_interval: 0\n"},
		{"federation intervals signed zero", "server:\n  federation:\n    refresh_interval: \"-0\"\n    debounce_interval: \"+0\"\n"},
		{"token_lifetime signed zero", "server:\n  oidc:\n    token_lifetime: \"-0\"\n"},
		{"token_lifetime plus zero", "server:\n  oidc:\n    token_lifetime: \"+0\"\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := []byte("schema_version: \"1\"\n" + tt.yaml)
			errs, err := ValidateSettings(data, "1")
			require.NoError(t, err)
			assert.Empty(t, errs)
		})
	}
}
