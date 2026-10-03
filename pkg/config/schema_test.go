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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- DetectSettingsFormat tests ---

func TestDetectSettingsFormat_Empty(t *testing.T) {
	version, isLegacy := DetectSettingsFormat(nil)
	assert.Equal(t, "", version)
	assert.False(t, isLegacy)

	version, isLegacy = DetectSettingsFormat([]byte{})
	assert.Equal(t, "", version)
	assert.False(t, isLegacy)
}

func TestDetectSettingsFormat_Versioned(t *testing.T) {
	data := []byte(`
schema_version: "1"
active_profile: local
harness_configs:
  gemini:
    harness: gemini
    image: example.com/gemini:latest
`)
	version, isLegacy := DetectSettingsFormat(data)
	assert.Equal(t, "1", version)
	assert.False(t, isLegacy)
}

func TestDetectSettingsFormat_Legacy(t *testing.T) {
	data := []byte(`
active_profile: local
harnesses:
  gemini:
    image: example.com/gemini:latest
    user: scion
`)
	version, isLegacy := DetectSettingsFormat(data)
	assert.Equal(t, "", version)
	assert.True(t, isLegacy)
}

func TestDetectSettingsFormat_Minimal(t *testing.T) {
	// No schema_version, no harnesses — neither versioned nor legacy
	data := []byte(`
active_profile: local
default_template: gemini
`)
	version, isLegacy := DetectSettingsFormat(data)
	assert.Equal(t, "", version)
	assert.False(t, isLegacy)
}

func TestDetectSettingsFormat_InvalidYAML(t *testing.T) {
	data := []byte(`{{{invalid yaml`)
	version, isLegacy := DetectSettingsFormat(data)
	assert.Equal(t, "", version)
	assert.False(t, isLegacy)
}

func TestDetectSettingsFormat_VersionedTakesPrecedence(t *testing.T) {
	// If both schema_version and harnesses exist, it's versioned
	data := []byte(`
schema_version: "1"
harnesses:
  gemini:
    image: test
`)
	version, isLegacy := DetectSettingsFormat(data)
	assert.Equal(t, "1", version)
	assert.False(t, isLegacy)
}

func TestDetectSettingsFormat_V1RuntimeTypeIndicator(t *testing.T) {
	// v1-shaped: runtimes entry with type key but no schema_version
	data := []byte(`
active_profile: cr
runtimes:
  cr:
    type: cloudrun
    cloudrun:
      project_id: my-project
      location: us-central1
`)
	version, isLegacy := DetectSettingsFormat(data)
	assert.Equal(t, "1", version, "v1-shaped runtime with type key should be detected as versioned")
	assert.False(t, isLegacy)
}

func TestDetectSettingsFormat_V1RuntimeGKEIndicator(t *testing.T) {
	data := []byte(`
active_profile: k8s
runtimes:
  k8s:
    gke: true
    context: my-cluster
`)
	version, isLegacy := DetectSettingsFormat(data)
	assert.Equal(t, "1", version, "v1-shaped runtime with gke key should be detected as versioned")
	assert.False(t, isLegacy)
}

func TestDetectSettingsFormat_V1RuntimeListAllNamespacesIndicator(t *testing.T) {
	data := []byte(`
active_profile: k8s
runtimes:
  k8s:
    list_all_namespaces: true
`)
	version, isLegacy := DetectSettingsFormat(data)
	assert.Equal(t, "1", version, "v1-shaped runtime with list_all_namespaces key should be detected as versioned")
	assert.False(t, isLegacy)
}

func TestDetectSettingsFormat_LegacyRuntimeNoV1Indicators(t *testing.T) {
	// Legacy runtimes without v1-only keys should not trigger v1 detection
	data := []byte(`
active_profile: local
runtimes:
  local:
    host: docker
    namespace: default
`)
	version, isLegacy := DetectSettingsFormat(data)
	assert.Equal(t, "", version, "legacy runtime without v1 fields should not be detected as versioned")
	assert.False(t, isLegacy)
}

// --- ValidateSettings tests ---

func TestValidateSettings_ValidV1(t *testing.T) {
	data := []byte(`
schema_version: "1"
active_profile: local
default_template: gemini
cli:
  autohelp: true
  interactive_disabled: false
hub:
  enabled: true
  endpoint: "https://hub.example.com"
  project_id: "abc-123"
  local_only: false
runtimes:
  docker:
    type: docker
    host: ""
  container:
    type: container
harness_configs:
  gemini:
    harness: gemini
    image: "us-central1-docker.pkg.dev/test/scion-gemini:latest"
    user: scion
  claude:
    harness: claude
    image: "us-central1-docker.pkg.dev/test/scion-claude:latest"
    user: scion
profiles:
  local:
    runtime: container
  remote:
    runtime: kubernetes
`)
	errors, err := ValidateSettings(data, "1")
	require.NoError(t, err)
	assert.Empty(t, errors, "valid settings should produce no validation errors")
}

func TestValidateSettings_MinimalValid(t *testing.T) {
	data := []byte(`
schema_version: "1"
`)
	errors, err := ValidateSettings(data, "1")
	require.NoError(t, err)
	assert.Empty(t, errors, "minimal valid settings should produce no errors")
}

func TestValidateSettings_UnknownTopLevelField(t *testing.T) {
	data := []byte(`
schema_version: "1"
unknown_field: value
`)
	errors, err := ValidateSettings(data, "1")
	require.NoError(t, err)
	assert.NotEmpty(t, errors, "unknown top-level field should produce validation error")

	// Check that the error mentions the unknown field
	found := false
	for _, e := range errors {
		if e.Path == "" || e.Path == "unknown_field" {
			found = true
			break
		}
	}
	assert.True(t, found, "should report error about unknown_field, got: %v", errors)
}

// Review finding O1 (ptone/scion#2270 round 3): design 4.4 lists the
// settings-v1 schema as in scope for the quotas.enforce_broker_quotas
// toggle. Without a "quotas" entry, the top-level "additionalProperties":
// false rejected a saved settings.yaml as soon as an admin flipped the
// switch in file mode, so `scion config validate` (and `config migrate`)
// would report it as invalid even though the hub itself loads and applies
// it fine.
func TestValidateSettings_Quotas(t *testing.T) {
	data := []byte(`
schema_version: "1"
quotas:
  enforce_broker_quotas: false
`)
	errors, err := ValidateSettings(data, "1")
	require.NoError(t, err)
	assert.Empty(t, errors, "a valid quotas.enforce_broker_quotas should produce no errors, got: %v", errors)
}

func TestValidateSettings_QuotasInvalidType(t *testing.T) {
	data := []byte(`
schema_version: "1"
quotas:
  enforce_broker_quotas: "yes"
`)
	errors, err := ValidateSettings(data, "1")
	require.NoError(t, err)
	assert.NotEmpty(t, errors, "a non-boolean enforce_broker_quotas should produce a validation error")
}

func TestValidateSettings_QuotasUnknownField(t *testing.T) {
	data := []byte(`
schema_version: "1"
quotas:
  unknown_field: true
`)
	errors, err := ValidateSettings(data, "1")
	require.NoError(t, err)
	assert.NotEmpty(t, errors, "an unknown field under quotas should produce a validation error")
}

// TestValidateSettings_AgentSecrets, TestValidateSettings_AgentSecretsInvalidType and
// TestValidateSettings_AgentSecretsUnknownField mirror the three Quotas tests above
// (round-1 review Rec1, ptone/scion#2291): the hand-written opsettings schema for
// agent_secrets is covered by opsettings_test.go's TestValidateValidDoc/InvalidDoc, but the
// separate settings-v1.schema.json entry — the one scion config validate and a saved
// settings.yaml actually go through — had no regression test, the same gap round 3 of
// ptone/scion#2270 found and fixed for quotas.
//
// TestValidateSettings_AgentSecrets is table-driven over true, false, and an
// empty agent_secrets object (round-2 review nit 1): all three are valid
// documents and must produce no errors.
func TestValidateSettings_AgentSecrets(t *testing.T) {
	tests := []struct {
		name string
		yaml string
	}{
		{"user_scope_only true", "agent_secrets:\n  user_scope_only: true\n"},
		{"user_scope_only false", "agent_secrets:\n  user_scope_only: false\n"},
		{"empty agent_secrets", "agent_secrets: {}\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := []byte("schema_version: \"1\"\n" + tt.yaml)
			errors, err := ValidateSettings(data, "1")
			require.NoError(t, err)
			assert.Empty(t, errors, "a valid agent_secrets document should produce no errors, got: %v", errors)
		})
	}
}

func TestValidateSettings_AgentSecretsInvalidType(t *testing.T) {
	data := []byte(`
schema_version: "1"
agent_secrets:
  user_scope_only: "yes"
`)
	errors, err := ValidateSettings(data, "1")
	require.NoError(t, err)
	assert.NotEmpty(t, errors, "a non-boolean user_scope_only should produce a validation error")
}

func TestValidateSettings_AgentSecretsUnknownField(t *testing.T) {
	data := []byte(`
schema_version: "1"
agent_secrets:
  unknown_field: true
`)
	errors, err := ValidateSettings(data, "1")
	require.NoError(t, err)
	assert.NotEmpty(t, errors, "an unknown field under agent_secrets should produce a validation error")
}

func TestValidateSettings_InvalidSchemaVersion(t *testing.T) {
	data := []byte(`
schema_version: "2"
`)
	errors, err := ValidateSettings(data, "1")
	require.NoError(t, err)
	assert.NotEmpty(t, errors, "wrong schema_version value should produce validation error")
}

func TestValidateSettings_InvalidRuntimeType(t *testing.T) {
	data := []byte(`
schema_version: "1"
runtimes:
  custom:
    type: invalid_type
`)
	errors, err := ValidateSettings(data, "1")
	require.NoError(t, err)
	assert.NotEmpty(t, errors, "invalid runtime type should produce validation error")
}

func TestValidateSettings_MissingRequiredHarnessField(t *testing.T) {
	data := []byte(`
schema_version: "1"
harness_configs:
  test:
    image: test:latest
    user: scion
`)
	errors, err := ValidateSettings(data, "1")
	require.NoError(t, err)
	assert.NotEmpty(t, errors, "missing required 'harness' field should produce validation error")
}

func TestValidateSettings_MissingRequiredProfileRuntime(t *testing.T) {
	data := []byte(`
schema_version: "1"
profiles:
  test:
    env:
      FOO: bar
`)
	errors, err := ValidateSettings(data, "1")
	require.NoError(t, err)
	assert.NotEmpty(t, errors, "missing required 'runtime' in profile should produce validation error")
}

func TestValidateSettings_UnknownFieldInHub(t *testing.T) {
	data := []byte(`
schema_version: "1"
hub:
  enabled: true
  token: "secret"
`)
	errors, err := ValidateSettings(data, "1")
	require.NoError(t, err)
	assert.NotEmpty(t, errors, "unknown field 'token' in hub should produce validation error")
}

func TestValidateSettings_UnsupportedVersion(t *testing.T) {
	data := []byte(`schema_version: "99"`)
	_, err := ValidateSettings(data, "99")
	assert.Error(t, err, "unsupported schema version should return an error")
	assert.Contains(t, err.Error(), "unsupported schema version")
}

func TestValidateSettings_InvalidYAML(t *testing.T) {
	data := []byte(`{{{not yaml`)
	_, err := ValidateSettings(data, "1")
	assert.Error(t, err, "invalid YAML should return a parse error")
}

func TestValidateSettings_ServerSection(t *testing.T) {
	data := []byte(`
schema_version: "1"
server:
  env: prod
  log_level: info
  log_format: text
  hub:
    port: 9810
    host: "0.0.0.0"
  broker:
    enabled: true
    port: 9800
    broker_id: "test-broker-uuid"
    auto_provide: true
  database:
    driver: sqlite
  auth:
    dev_mode: false
  storage:
    provider: local
  secrets:
    backend: local
`)
	errors, err := ValidateSettings(data, "1")
	require.NoError(t, err)
	assert.Empty(t, errors, "valid server section should produce no errors")
}

func TestValidateSettings_WorkspaceStorageNFSAutoMount(t *testing.T) {
	data := []byte(`
schema_version: "1"
server:
  workspace_storage:
    backend: nfs
    nfs:
      mount_root: /mnt/nfs
      auto_mount: true
      shares:
        - id: ws1
          server: 10.0.0.2
          export: /scion-workspaces
`)
	errors, err := ValidateSettings(data, "1")
	require.NoError(t, err)
	assert.Empty(t, errors, "workspace_storage with nfs.auto_mount should validate")

	bad := []byte(`
schema_version: "1"
server:
  workspace_storage:
    nfs:
      auto_mount: "yes"
`)
	errors, err = ValidateSettings(bad, "1")
	require.NoError(t, err)
	assert.NotEmpty(t, errors, "non-boolean nfs.auto_mount should produce a validation error")
}

func TestValidateSettings_InvalidServerLogLevel(t *testing.T) {
	data := []byte(`
schema_version: "1"
server:
  log_level: verbose
`)
	errors, err := ValidateSettings(data, "1")
	require.NoError(t, err)
	assert.NotEmpty(t, errors, "invalid log_level should produce validation error")
}

func TestValidateSettings_UnknownFieldInServer(t *testing.T) {
	data := []byte(`
schema_version: "1"
server:
  unknown_server_field: true
`)
	errors, err := ValidateSettings(data, "1")
	require.NoError(t, err)
	assert.NotEmpty(t, errors, "unknown field in server should produce validation error")
}

func TestValidateSettings_HarnessConfigWithAllFields(t *testing.T) {
	data := []byte(`
schema_version: "1"
harness_configs:
  gemini-custom:
    harness: gemini
    image: "example.com/gemini:v2"
    user: scion
    model: "gemini-2.5-pro"
    args: ["--sandbox=strict"]
    env:
      GEMINI_SAFETY: "maximum"
    volumes:
      - source: /host/path
        target: /container/path
        read_only: true
    auth_selected_type: "vertex-ai"
    provisioner:
      type: builtin
      interface_version: 1
      command: ["python3", "/home/scion/.scion/harness/provision.py"]
      timeout: 30s
      lifecycle_events: ["pre-start"]
      required_image_tools: ["python3"]
    config_dir: .gemini
    skills_dir: .gemini/skills
    interrupt_key: Escape
    instructions_file: .gemini/GEMINI.md
    system_prompt_file: .gemini/system_prompt.md
    system_prompt_mode: native
    command:
      base: ["gemini", "--yolo"]
      resume_flag: "--resume"
      task_flag: "--prompt-interactive"
      task_position: after_base_args
    env_template:
      GEMINI_CLI_NO_RELAUNCH: "true"
    capabilities:
      limits:
        max_turns: { support: "yes" }
        max_model_calls: { support: "yes" }
        max_duration: { support: "yes" }
      telemetry:
        enabled: { support: "yes" }
        native_emitter: { support: "yes" }
      prompts:
        system_prompt: { support: "yes" }
        agent_instructions: { support: "yes" }
      auth:
        api_key: { support: "yes" }
        auth_file: { support: "yes" }
        oauth_token: { support: "no" }
        vertex_ai: { support: "yes" }
    auth:
      default_type: api-key
      types:
        api-key:
          required_env:
            - any_of: ["GEMINI_API_KEY"]
        vertex-ai:
          required_env:
            - any_of: ["GOOGLE_CLOUD_PROJECT"]
          required_files:
            - name: gcloud-adc
              type: file
              description: "ADC file"
              alternative_env_keys: ["GOOGLE_APPLICATION_CREDENTIALS"]
              skipped_when_gcp_service_account_assigned: true
      autodetect:
        env:
          GEMINI_API_KEY: api-key
        files:
          gcloud-adc: vertex-ai
    dialect:
      event_name_field: event_type
`)
	errors, err := ValidateSettings(data, "1")
	require.NoError(t, err)
	assert.Empty(t, errors, "harness config with all valid fields should pass")
}

func TestValidateSettings_ProfileWithOverrides(t *testing.T) {
	data := []byte(`
schema_version: "1"
profiles:
  staging:
    runtime: docker
    default_template: gemini
    default_harness_config: gemini
    resources:
      requests:
        cpu: "500m"
        memory: "512Mi"
      limits:
        cpu: "2"
        memory: "2Gi"
      disk: "10Gi"
    harness_overrides:
      gemini:
        image: "custom:staging"
        env:
          EXTRA: "value"
`)
	errors, err := ValidateSettings(data, "1")
	require.NoError(t, err)
	assert.Empty(t, errors, "profile with overrides should pass validation")
}

// --- ValidateAgentConfig tests ---

func TestValidateAgentConfig_Valid(t *testing.T) {
	data := []byte(`
schema_version: "1"
harness_config: gemini
image: "example.com/gemini:latest"
user: scion
model: "gemini-2.5-pro"
max_turns: 50
max_duration: "2h"
env:
  FOO: bar
`)
	errors, err := ValidateAgentConfig(data, "1")
	require.NoError(t, err)
	assert.Empty(t, errors, "valid agent config should produce no errors")
}

func TestValidateAgentConfig_WithServices(t *testing.T) {
	data := []byte(`
schema_version: "1"
harness_config: gemini
services:
  - name: browser
    command: ["chromium", "--headless"]
    restart: on-failure
    ready_check:
      type: tcp
      target: "localhost:9222"
      timeout: "10s"
`)
	errors, err := ValidateAgentConfig(data, "1")
	require.NoError(t, err)
	assert.Empty(t, errors, "agent config with services should pass validation")
}

func TestValidateAgentConfig_InvalidMaxDuration(t *testing.T) {
	data := []byte(`
schema_version: "1"
harness_config: gemini
max_duration: "2 hours"
`)
	errors, err := ValidateAgentConfig(data, "1")
	require.NoError(t, err)
	assert.NotEmpty(t, errors, "invalid max_duration format should produce validation error")
}

func TestValidateAgentConfig_InvalidPriorityClassName(t *testing.T) {
	data := []byte(`
schema_version: "1"
harness_config: gemini
kubernetes:
  priorityClassName: "Not_A_Valid_Name"
`)
	errors, err := ValidateAgentConfig(data, "1")
	require.NoError(t, err)
	assert.NotEmpty(t, errors, "invalid priorityClassName should produce validation error")
}

func TestValidateAgentConfig_EmptyPriorityClassName(t *testing.T) {
	// An explicit empty string means "unset" to buildPod and
	// mergeKubernetesConfig and must not fail schema validation.
	data := []byte(`
schema_version: "1"
harness_config: gemini
kubernetes:
  priorityClassName: ""
`)
	errors, err := ValidateAgentConfig(data, "1")
	require.NoError(t, err)
	assert.Empty(t, errors, "an empty priorityClassName means unset and must pass validation")
}

func TestValidateAgentConfig_InvalidMaxTurns(t *testing.T) {
	data := []byte(`
schema_version: "1"
harness_config: gemini
max_turns: 0
`)
	errors, err := ValidateAgentConfig(data, "1")
	require.NoError(t, err)
	assert.NotEmpty(t, errors, "max_turns=0 should produce validation error (minimum is 1)")
}

// --- Schema retrieval tests ---

func TestGetSettingsSchemaJSON(t *testing.T) {
	data, err := GetSettingsSchemaJSON("1")
	require.NoError(t, err)
	assert.NotEmpty(t, data)
	assert.Contains(t, string(data), `"$schema"`)
	assert.Contains(t, string(data), `"Scion Settings"`)
}

func TestGetSettingsSchemaJSON_UnsupportedVersion(t *testing.T) {
	_, err := GetSettingsSchemaJSON("99")
	assert.Error(t, err)
}

func TestGetAgentSchemaJSON(t *testing.T) {
	data, err := GetAgentSchemaJSON("1")
	require.NoError(t, err)
	assert.NotEmpty(t, data)
	assert.Contains(t, string(data), `"Scion Agent Configuration"`)
}

func TestGetAgentSchemaJSON_UnsupportedVersion(t *testing.T) {
	_, err := GetAgentSchemaJSON("99")
	assert.Error(t, err)
}

// --- ValidationError tests ---

func TestValidationError_String(t *testing.T) {
	e := ValidationError{Path: "hub/endpoint", Message: "must be a valid URI"}
	assert.Equal(t, "hub/endpoint: must be a valid URI", e.Error())

	e2 := ValidationError{Path: "", Message: "root-level error"}
	assert.Equal(t, "root-level error", e2.Error())
}

// --- Edge cases ---

func TestValidateSettings_EmptyDocument(t *testing.T) {
	// An empty YAML document should be parsed as nil, which becomes a
	// null value. The schema expects an object, so this should fail.
	data := []byte(``)
	errors, err := ValidateSettings(data, "1")
	require.NoError(t, err)
	assert.NotEmpty(t, errors, "empty document should fail validation (not an object)")
}

func TestValidateSettings_JSONInput(t *testing.T) {
	// The validator should handle JSON input (which is valid YAML).
	data := []byte(`{"schema_version": "1", "active_profile": "local"}`)
	errors, err := ValidateSettings(data, "1")
	require.NoError(t, err)
	assert.Empty(t, errors, "valid JSON input should pass validation")
}

func TestDetectSettingsFormat_JSONInput(t *testing.T) {
	data := []byte(`{"schema_version": "1", "harness_configs": {}}`)
	version, isLegacy := DetectSettingsFormat(data)
	assert.Equal(t, "1", version)
	assert.False(t, isLegacy)
}

func TestDetectSettingsFormat_LegacyJSON(t *testing.T) {
	data := []byte(`{"harnesses": {"gemini": {"image": "test"}}}`)
	version, isLegacy := DetectSettingsFormat(data)
	assert.Equal(t, "", version)
	assert.True(t, isLegacy)
}

// --- Comprehensive full-schema validation tests ---

func TestValidateSettings_CompleteSchema(t *testing.T) {
	// Exercises every section and field in the settings v1 schema.
	data := []byte(`
schema_version: "1"
active_profile: local
default_template: gemini
default_harness_config: gemini

server:
  env: prod
  log_level: info
  log_format: json
  hub:
    port: 9810
    host: "0.0.0.0"
    public_url: "https://hub.example.com"
    read_timeout: "30s"
    write_timeout: "60s"
    admin_emails:
      - "admin@example.com"
    soft_delete_retention: "72h"
    soft_delete_retain_files: true
    cors:
      enabled: true
      allowed_origins: ["https://app.example.com"]
      allowed_methods: ["GET", "POST", "PUT", "DELETE"]
      allowed_headers: ["Authorization", "Content-Type"]
      max_age: 3600
  broker:
    enabled: true
    port: 9800
    host: "0.0.0.0"
    read_timeout: "30s"
    write_timeout: "120s"
    hub_endpoint: "https://hub.example.com"
    broker_id: "broker-uuid-1234"
    broker_name: "prod-broker-1"
    broker_nickname: "Broker One"
    broker_token: "secret-token"
    auto_provide: true
    cors:
      enabled: true
      allowed_origins: ["*"]
  database:
    driver: postgres
    url: "postgres://localhost:5432/scion"
  auth:
    dev_mode: true
    dev_token: "dev-secret-token"
    dev_token_file: "/run/secrets/dev-token"
    authorized_domains: ["example.com", "test.com"]
  oauth:
    web:
      google:
        client_id: "web-google-id"
        client_secret: "web-google-secret"
      github:
        client_id: "web-github-id"
        client_secret: "web-github-secret"
    cli:
      google:
        client_id: "cli-google-id"
        client_secret: "cli-google-secret"
      github:
        client_id: "cli-github-id"
        client_secret: "cli-github-secret"
    device:
      google:
        client_id: "device-google-id"
        client_secret: "device-google-secret"
      github:
        client_id: "device-github-id"
        client_secret: "device-github-secret"
  storage:
    provider: gcs
    bucket: "scion-templates"
    local_path: "/var/scion/storage"
  secrets:
    backend: gcpsm
    gcp_project_id: "my-project"
    gcp_credentials: "/path/to/creds.json"

hub:
  enabled: true
  endpoint: "https://hub.example.com"
  project_id: "project-abc-123"
  local_only: false

cli:
  autohelp: true
  interactive_disabled: false

telemetry:
  enabled: true
  cloud:
    enabled: true
    endpoint: "https://otel.example.com:4317"
    protocol: grpc
    headers:
      Authorization: "Bearer token123"
    tls:
      enabled: true
      insecure_skip_verify: false
    batch:
      max_size: 512
      timeout: "5s"
  hub:
    enabled: true
    report_interval: "30s"
  local:
    enabled: true
    file: "/var/log/scion-telemetry.jsonl"
    console: true
  filter:
    enabled: true
    respect_debug_mode: true
    events:
      include: ["agent.start", "agent.stop"]
      exclude: ["agent.heartbeat"]
    attributes:
      redact: ["api_key", "token"]
      hash: ["user_email"]
    sampling:
      default: 0.5
      rates:
        "agent.start": 1.0
        "agent.heartbeat": 0.1
  resource:
    "service.name": "scion"
    "deployment.environment": "production"

runtimes:
  docker:
    type: docker
    host: "unix:///var/run/docker.sock"
    env:
      DOCKER_TLS_VERIFY: "1"
    sync: "tar"
  container:
    type: container
  k8s-prod:
    type: kubernetes
    context: "gke_project_zone_cluster"
    namespace: "scion-agents"
    gke: true

harness_configs:
  gemini:
    harness: gemini
    image: "us-central1-docker.pkg.dev/project/scion-gemini:latest"
    user: scion
    model: "gemini-2.5-pro"
    args: ["--sandbox=strict"]
    env:
      GEMINI_SAFETY: "maximum"
    volumes:
      - source: /host/config
        target: /container/config
        read_only: true
        type: local
    auth_selected_type: "vertex-ai"
    secrets:
      - key: GEMINI_API_KEY
        description: "Gemini API key"
        type: environment
      - key: service_account
        description: "Service account JSON"
        type: file
        target: /run/secrets/sa.json
  claude:
    harness: claude
    image: "us-central1-docker.pkg.dev/project/scion-claude:latest"
    user: scion
  opencode:
    harness: opencode
    image: "example.com/opencode:latest"
  codex:
    harness: codex
    image: "example.com/codex:latest"
  custom-generic:
    harness: generic
    image: "example.com/custom:latest"
    model: "custom-model"
    args: ["--verbose"]

profiles:
  local:
    runtime: container
    default_template: gemini
    default_harness_config: gemini
    volumes:
      - source: /tmp/scion
        target: /workspace/tmp
    resources:
      requests:
        cpu: "500m"
        memory: "512Mi"
      limits:
        cpu: "2"
        memory: "2Gi"
      disk: "10Gi"
    harness_overrides:
      gemini:
        image: "custom:local"
        user: dev
        env:
          EXTRA: "value"
        volumes:
          - source: /local/override
            target: /container/override
        resources:
          requests:
            cpu: "1"
            memory: "1Gi"
        auth_selected_type: "api-key"
    secrets:
      - key: PROFILE_SECRET
        description: "A profile-level secret"
  remote:
    runtime: k8s-prod
    default_template: gemini
`)
	errors, err := ValidateSettings(data, "1")
	require.NoError(t, err)
	assert.Empty(t, errors, "complete settings schema example should produce no validation errors, got: %v", errors)
}

func TestValidateAgentConfig_CompleteSchema(t *testing.T) {
	// Exercises every section and field in the agent v1 schema.
	data := []byte(`
schema_version: "1"
harness_config: gemini
harness: gemini
image: "us-central1-docker.pkg.dev/project/scion-gemini:latest"
user: scion
model: "gemini-2.5-pro"
args: ["--sandbox=strict"]
detached: true
config_dir: "/home/scion/.config"
command_args: ["--verbose"]
max_turns: 100
max_duration: "4h"

env:
  GEMINI_SAFETY: "maximum"
  WORKSPACE: "/workspace"

volumes:
  - source: /host/config
    target: /container/config
    read_only: true
    type: local
  - target: /data/gcs
    type: gcs
    bucket: "my-bucket"
    prefix: "data/"
    mode: "ro"

resources:
  requests:
    cpu: "1"
    memory: "1Gi"
  limits:
    cpu: "4"
    memory: "8Gi"
  disk: "20Gi"

services:
  - name: browser
    command: ["chromium", "--headless", "--remote-debugging-port=9222"]
    restart: on-failure
    env:
      DISPLAY: ":99"
    ready_check:
      type: tcp
      target: "localhost:9222"
      timeout: "10s"
  - name: database
    command: ["postgres", "-D", "/data"]
    restart: always
    ready_check:
      type: http
      target: "http://localhost:5432/health"
      timeout: "30s"
  - name: delay-service
    command: ["sleep", "5"]
    restart: "no"
    ready_check:
      type: delay
      target: "3s"
      timeout: "5s"

auth_selectedType: "vertex-ai"

hub:
  endpoint: "https://hub.example.com"

telemetry:
  enabled: true
  cloud:
    enabled: true
    endpoint: "https://otel.example.com:4317"
    protocol: grpc
    headers:
      Authorization: "Bearer token"
    tls:
      enabled: true
      insecure_skip_verify: false
    batch:
      max_size: 256
      timeout: "3s"
  hub:
    enabled: true
    report_interval: "15s"
  local:
    enabled: true
    file: "/var/log/agent-telemetry.jsonl"
    console: false
  filter:
    enabled: true
    respect_debug_mode: true
    events:
      include: ["tool.call", "llm.turn"]
      exclude: ["agent.heartbeat"]
    attributes:
      redact: ["api_key"]
      hash: ["user_id"]
    sampling:
      default: 1.0
      rates:
        "tool.call": 0.5
  resource:
    "agent.name": "test-agent"

secrets:
  - key: API_KEY
    description: "Primary API key"
    type: environment
  - key: SERVICE_ACCOUNT
    description: "Service account credentials"
    type: file
    target: /run/secrets/sa.json

agent_instructions: "You are a helpful coding assistant."
system_prompt: "Follow best practices and write clean code."
default_harness_config: gemini

kubernetes:
  context: "gke_project_zone_cluster"
  namespace: "scion-agents"
  runtimeClassName: "gvisor"
  serviceAccountName: "scion-agent-sa"
  priorityClassName: "scion-agent-priority"
  resources:
    requests:
      cpu: "2"
      memory: "4Gi"
    limits:
      cpu: "4"
      memory: "8Gi"
`)
	errors, err := ValidateAgentConfig(data, "1")
	require.NoError(t, err)
	assert.Empty(t, errors, "complete agent config schema example should produce no validation errors, got: %v", errors)
}

func TestValidateSettings_RuntimeWithGKE(t *testing.T) {
	data := []byte(`
schema_version: "1"
runtimes:
  k8s:
    type: kubernetes
    context: "gke_project_zone_cluster"
    namespace: "default"
    gke: true
`)
	errors, err := ValidateSettings(data, "1")
	require.NoError(t, err)
	assert.Empty(t, errors, "runtime with gke field should pass validation")
}

func TestValidateSettings_RuntimeWithPriorityClassName(t *testing.T) {
	data := []byte(`
schema_version: "1"
runtimes:
  k8s:
    type: kubernetes
    priority_class_name: scion-agent-priority
`)
	errors, err := ValidateSettings(data, "1")
	require.NoError(t, err)
	assert.Empty(t, errors, "runtime with a valid priority_class_name should pass validation")
}

func TestValidateSettings_RuntimeWithInvalidPriorityClassName(t *testing.T) {
	data := []byte(`
schema_version: "1"
runtimes:
  k8s:
    type: kubernetes
    priority_class_name: Not_A_Valid_Name
`)
	errors, err := ValidateSettings(data, "1")
	require.NoError(t, err)
	assert.NotEmpty(t, errors, "runtime with an invalid priority_class_name should fail validation")
}

func TestValidateSettings_RuntimeWithEmptyPriorityClassName(t *testing.T) {
	// An explicit empty string means "unset" to the runtime (buildPod and
	// mergeKubernetesConfig both treat "" as unset) and must not fail
	// schema validation the way a real invalid name does — for example
	// after settings migration or a round-trip that writes the zero value.
	data := []byte(`
schema_version: "1"
runtimes:
  k8s:
    type: kubernetes
    priority_class_name: ""
`)
	errors, err := ValidateSettings(data, "1")
	require.NoError(t, err)
	assert.Empty(t, errors, "an empty priority_class_name means unset and must pass validation")
}

func TestValidateSettings_ServerHubSoftDelete(t *testing.T) {
	data := []byte(`
schema_version: "1"
server:
  hub:
    port: 9810
    soft_delete_retention: "72h"
    soft_delete_retain_files: true
`)
	errors, err := ValidateSettings(data, "1")
	require.NoError(t, err)
	assert.Empty(t, errors, "server hub with soft_delete fields should pass validation")
}

func TestValidateAgentConfig_WithSecrets(t *testing.T) {
	data := []byte(`
schema_version: "1"
harness_config: gemini
secrets:
  - key: API_KEY
    description: "API key for the service"
    type: environment
  - key: CERT_FILE
    type: file
    target: /run/secrets/cert.pem
`)
	errors, err := ValidateAgentConfig(data, "1")
	require.NoError(t, err)
	assert.Empty(t, errors, "agent config with secrets should pass validation")
}

func TestValidateAgentConfig_WithHub(t *testing.T) {
	data := []byte(`
schema_version: "1"
harness_config: gemini
hub:
  endpoint: "https://hub.example.com"
`)
	errors, err := ValidateAgentConfig(data, "1")
	require.NoError(t, err)
	assert.Empty(t, errors, "agent config with hub should pass validation")
}

func TestValidateAgentConfig_WithAgnosticFields(t *testing.T) {
	data := []byte(`
schema_version: "1"
harness_config: gemini
agent_instructions: "You are a coding assistant."
system_prompt: "Write clean, tested code."
default_harness_config: claude
`)
	errors, err := ValidateAgentConfig(data, "1")
	require.NoError(t, err)
	assert.Empty(t, errors, "agent config with agnostic template fields should pass validation")
}

func TestValidateAgentConfig_ServiceWithEnv(t *testing.T) {
	data := []byte(`
schema_version: "1"
harness_config: gemini
services:
  - name: db
    command: ["postgres"]
    env:
      PGDATA: "/var/lib/postgresql/data"
      POSTGRES_PASSWORD: "test"
`)
	errors, err := ValidateAgentConfig(data, "1")
	require.NoError(t, err)
	assert.Empty(t, errors, "service with env should pass validation")
}

func TestValidateAgentConfig_ServiceWithDelayCheck(t *testing.T) {
	data := []byte(`
schema_version: "1"
harness_config: gemini
services:
  - name: slow-start
    command: ["./start.sh"]
    ready_check:
      type: delay
      target: "5s"
      timeout: "10s"
`)
	errors, err := ValidateAgentConfig(data, "1")
	require.NoError(t, err)
	assert.Empty(t, errors, "service with delay ready_check should pass validation")
}
