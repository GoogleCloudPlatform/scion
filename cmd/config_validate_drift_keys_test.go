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

package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestConfigValidateCmd_DriftKeysValidate checks that `scion config validate`
// accepts each settings key that settings-v1.schema.json was missing even
// though the Go settings types load it (ptone/scion#2654, #2285, #2259).
func TestConfigValidateCmd_DriftKeysValidate(t *testing.T) {
	tests := []struct {
		name string
		yaml string
	}{
		{"auto_expose_ports", "auto_expose_ports:\n  enabled: false\n"},
		{"project_defaults", "project_defaults:\n  default_scratchpad: false\n"},
		{"project_type", "project_type: shadow\n"},
		{"workspace_path", "workspace_path: /src/app\n"},
		{"shared_dirs", "shared_dirs:\n  - name: build-cache\n    read_only: true\n    in_workspace: true\n"},
		{"managed_agents", "managed_agents:\n  google:\n    api_key: k\n    base_agent: b\n    model: m\n"},
		{"default_runtime_broker", "default_runtime_broker: broker-1\n"},
		{"default_timezone", "default_timezone: America/Los_Angeles\n"},
		{"auto_inject_gcloud_adc", "auto_inject_gcloud_adc: true\n"},
		{"hub.linked", "hub:\n  linked: true\n"},
		{"secrets alternative_env_keys", "profiles:\n  local:\n    runtime: docker\n    secrets:\n      - key: gcloud-adc\n        type: file\n        alternative_env_keys: [GOOGLE_APPLICATION_CREDENTIALS]\n"},
		{"runtimes list_all_namespaces", "runtimes:\n  k8s:\n    type: kubernetes\n    list_all_namespaces: true\n"},
		{"runtimes cloudrun", "runtimes:\n  cr:\n    type: cloudrun\n    cloudrun:\n      project_id: p\n      location: us-central1\n      service_account: sa\n      network: n\n      subnetwork: s\n      nfs_server: 10.0.0.2\n      nfs_export: /export\n"},
		{"runtimes cloudrun_instances", "runtimes:\n  cri:\n    type: cloudrun-instances\n    cloudrun_instances:\n      project_id: p\n      region: us-central1\n"},
		{"runtimes cloudrun_sandbox", "runtimes:\n  crs:\n    type: cloudrun-sandbox\n    cloudrun_sandbox:\n      sandbox_bin: /usr/local/gcp/bin/sandbox\n"},
		{"server.mode", "server:\n  mode: hosted\n"},
		{"server.maintenance", "server:\n  maintenance:\n    deployment_tier: binary\n    release_channel: stable\n    update_policy: notify\n    check_interval_hours: 6\n    github_repo: owner/repo\n"},
		{"server.notification_channels", "server:\n  notification_channels:\n    - type: slack\n      params:\n        webhook_url: https://example.com/hook\n      filter_types: [stalled]\n      filter_urgent_only: true\n"},
		{"server.message_broker", "server:\n  message_broker:\n    enabled: true\n    type: inprocess\n    types: [inprocess]\n"},
		{"server.native_chat", "server:\n  native_chat:\n    enabled: false\n"},
		{"server.plugins", "server:\n  plugins:\n    broker:\n      nats:\n        mode: grpc\n        address: localhost:9000\n        config:\n          url: nats://localhost\n"},
		{"server.github_app", "server:\n  github_app:\n    app_id: 12345\n    private_key_path: /etc/key.pem\n    webhooks_enabled: true\n"},
		{"server.oidc_login", "server:\n  oidc_login:\n    enabled: true\n    issuer_url: https://sso.example.com\n    client_id: id\n    scopes: [openid]\n"},
		{"server.oidc", "server:\n  oidc:\n    enabled: true\n    issuer_url: https://hub.example.com\n    token_lifetime: 15m\n"},
		{"server.federation", "server:\n  federation:\n    enabled: true\n    trusted_issuers:\n      - issuer_url: https://other.example.com\n        allowed_domains: [example.com]\n    algorithms: [RS256]\n    refresh_interval: 1h\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			writeGlobalSettingsForValidate(t, "schema_version: \"1\"\n"+tt.yaml)
			output, err := runConfigValidate(t)
			require.NoError(t, err, "`scion config validate` should accept %s; output:\n%s", tt.name, output)
			assert.NotContains(t, output, "ERROR")
		})
	}
}
