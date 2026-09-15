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

package runtime

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/GoogleCloudPlatform/scion/pkg/stagedsecrets"
)

func TestPrepareContainerSecretEnv(t *testing.T) {
	config := RunConfig{
		UnixUsername: "scion",
		Env:          []string{"EXISTING=value"},
		ResolvedSecrets: []api.ResolvedSecret{
			{Name: "CONFIG", Type: "variable", Target: "config", Value: "json-data"},
			{
				Name:   telemetryGCPCredentialsSecretName,
				Type:   "file",
				Target: "~/.config/gcp/sa.json",
				Value:  "credentials",
			},
		},
	}

	if err := prepareContainerSecretEnv(&config); err != nil {
		t.Fatalf("prepareContainerSecretEnv failed: %v", err)
	}
	if len(config.Env) != 3 {
		t.Fatalf("environment entries = %v, want existing, staged secrets, and telemetry credentials", config.Env)
	}
	if config.Env[0] != "EXISTING=value" {
		t.Errorf("existing environment moved or changed: %v", config.Env)
	}

	prefix := stagedsecrets.EnvVar + "="
	if !strings.HasPrefix(config.Env[1], prefix) {
		t.Fatalf("staged secret environment = %q, want %q prefix", config.Env[1], prefix)
	}
	staged, err := stagedsecrets.Decode(strings.TrimPrefix(config.Env[1], prefix))
	if err != nil {
		t.Fatalf("decode staged secrets: %v", err)
	}
	if staged.VariableSecrets["config"] != "json-data" {
		t.Errorf("staged variable secrets = %#v", staged.VariableSecrets)
	}

	wantTelemetry := telemetryGCPCredentialsEnvVar + "=/home/scion/.config/gcp/sa.json"
	if config.Env[2] != wantTelemetry {
		t.Errorf("telemetry environment = %q, want %q", config.Env[2], wantTelemetry)
	}

	envOnly := RunConfig{
		Env:             []string{"EXISTING=value"},
		ResolvedSecrets: []api.ResolvedSecret{{Name: "API_KEY", Type: "environment", Target: "API_KEY", Value: "secret"}},
	}
	if err := prepareContainerSecretEnv(&envOnly); err != nil {
		t.Fatalf("prepareContainerSecretEnv with environment secret failed: %v", err)
	}
	if len(envOnly.Env) != 1 || envOnly.Env[0] != "EXISTING=value" {
		t.Errorf("environment-only secret changed staged environment: %v", envOnly.Env)
	}
}

func TestSerializeSecrets(t *testing.T) {
	secrets := []api.ResolvedSecret{
		{
			Name:   "TLS_CERT",
			Type:   "file",
			Target: "/etc/ssl/cert.pem",
			Value:  base64.StdEncoding.EncodeToString([]byte("cert-content")),
			Source: "user",
		},
		{
			Name:   "API_KEY",
			Type:   "environment",
			Target: "API_KEY",
			Value:  "sk-123",
			Source: "user",
		},
		{
			Name:   "SSH_KEY",
			Type:   "file",
			Target: "/home/scion/.ssh/id_rsa",
			Value:  "raw-value-not-base64",
			Source: "project",
		},
		{
			Name:   "CONFIG",
			Type:   "variable",
			Target: "config",
			Value:  `{"a":"b"}`,
			Source: "user",
		},
		{
			Name:   "TOKEN",
			Type:   "variable",
			Target: "token",
			Value:  "abc123",
			Source: "project",
		},
	}

	encoded, err := serializeSecrets("/home/scion", secrets)
	if err != nil {
		t.Fatalf("serializeSecrets failed: %v", err)
	}
	if encoded == "" {
		t.Fatal("expected non-empty encoded string")
	}

	// Decode and verify the structure
	staged, err := stagedsecrets.Decode(encoded)
	if err != nil {
		t.Fatalf("stagedsecrets.Decode failed: %v", err)
	}

	// Should have 2 file secrets (environment type is excluded)
	if len(staged.FileSecrets) != 2 {
		t.Fatalf("expected 2 file secrets, got %d", len(staged.FileSecrets))
	}

	// Verify file secret targets
	if staged.FileSecrets[0].Target != "/etc/ssl/cert.pem" {
		t.Errorf("expected target /etc/ssl/cert.pem, got %s", staged.FileSecrets[0].Target)
	}
	if staged.FileSecrets[1].Target != "/home/scion/.ssh/id_rsa" {
		t.Errorf("expected target /home/scion/.ssh/id_rsa, got %s", staged.FileSecrets[1].Target)
	}

	// Verify base64-encoded values are decodable
	data, err := base64.StdEncoding.DecodeString(staged.FileSecrets[0].Value)
	if err != nil {
		t.Fatalf("failed to decode file secret value: %v", err)
	}
	if string(data) != "cert-content" {
		t.Errorf("expected cert-content, got %s", string(data))
	}

	// Raw values should have been re-encoded as base64
	data, err = base64.StdEncoding.DecodeString(staged.FileSecrets[1].Value)
	if err != nil {
		t.Fatalf("raw value should be re-encoded as base64: %v", err)
	}
	if string(data) != "raw-value-not-base64" {
		t.Errorf("expected raw-value-not-base64, got %s", string(data))
	}

	// Should have 2 variable secrets
	if len(staged.VariableSecrets) != 2 {
		t.Fatalf("expected 2 variable secrets, got %d", len(staged.VariableSecrets))
	}
	if staged.VariableSecrets["config"] != `{"a":"b"}` {
		t.Errorf("config value mismatch: got %q", staged.VariableSecrets["config"])
	}
	if staged.VariableSecrets["token"] != "abc123" {
		t.Errorf("token value mismatch: got %q", staged.VariableSecrets["token"])
	}
}

func TestSerializeSecrets_NoFileOrVariableSecrets(t *testing.T) {
	secrets := []api.ResolvedSecret{
		{Name: "KEY", Type: "environment", Target: "KEY", Value: "val"},
	}

	encoded, err := serializeSecrets("/home/scion", secrets)
	if err != nil {
		t.Fatalf("serializeSecrets failed: %v", err)
	}
	if encoded != "" {
		t.Errorf("expected empty string for env-only secrets, got %q", encoded)
	}
}

func TestSerializeSecrets_TildeExpansion(t *testing.T) {
	secrets := []api.ResolvedSecret{
		{
			Name:   "SSH_KEY",
			Type:   "file",
			Target: "~/.ssh/id_rsa",
			Value:  base64.StdEncoding.EncodeToString([]byte("ssh-key-content")),
			Source: "user",
		},
		{
			Name:   "ABS_CERT",
			Type:   "file",
			Target: "/etc/ssl/cert.pem",
			Value:  base64.StdEncoding.EncodeToString([]byte("cert-content")),
			Source: "user",
		},
	}

	encoded, err := serializeSecrets("/home/gemini", secrets)
	if err != nil {
		t.Fatalf("serializeSecrets failed: %v", err)
	}

	staged, err := stagedsecrets.Decode(encoded)
	if err != nil {
		t.Fatalf("stagedsecrets.Decode failed: %v", err)
	}

	if staged.FileSecrets[0].Target != "/home/gemini/.ssh/id_rsa" {
		t.Errorf("expected tilde-expanded target, got %s", staged.FileSecrets[0].Target)
	}
	if staged.FileSecrets[1].Target != "/etc/ssl/cert.pem" {
		t.Errorf("expected absolute target unchanged, got %s", staged.FileSecrets[1].Target)
	}
}

func TestSerializeSecrets_DuplicateTargetKeepsLater(t *testing.T) {
	secrets := []api.ResolvedSecret{
		{Name: "CERT_V1", Type: "file", Target: "/etc/cert.pem", Value: base64.StdEncoding.EncodeToString([]byte("v1")), Source: "user"},
		{Name: "CERT_V2", Type: "file", Target: "/etc/cert.pem", Value: base64.StdEncoding.EncodeToString([]byte("v2")), Source: "project"},
	}

	encoded, err := serializeSecrets("/home/scion", secrets)
	if err != nil {
		t.Fatalf("serializeSecrets failed: %v", err)
	}

	staged, err := stagedsecrets.Decode(encoded)
	if err != nil {
		t.Fatalf("stagedsecrets.Decode failed: %v", err)
	}

	// Dedup should keep only one entry per target (last wins).
	if len(staged.FileSecrets) != 1 {
		t.Fatalf("expected 1 file secret after dedup, got %d", len(staged.FileSecrets))
	}
	if staged.FileSecrets[0].Name != "CERT_V2" {
		t.Errorf("expected CERT_V2 to win, got %s", staged.FileSecrets[0].Name)
	}
	data, err := base64.StdEncoding.DecodeString(staged.FileSecrets[0].Value)
	if err != nil {
		t.Fatalf("failed to decode value: %v", err)
	}
	if string(data) != "v2" {
		t.Errorf("expected v2 content, got %q", string(data))
	}
}

func TestSerializeAndWriteRoundTrip(t *testing.T) {
	homeDir := t.TempDir()
	targetDir := t.TempDir()

	secrets := []api.ResolvedSecret{
		{
			Name:   "CERT",
			Type:   "file",
			Target: filepath.Join(targetDir, "cert.pem"),
			Value:  base64.StdEncoding.EncodeToString([]byte("my-cert")),
			Source: "user",
		},
		{
			Name:   "CONFIG",
			Type:   "variable",
			Target: "app_config",
			Value:  `{"db":"postgres"}`,
			Source: "project",
		},
		{
			Name:   "ENV_KEY",
			Type:   "environment",
			Target: "ENV_KEY",
			Value:  "should-be-excluded",
			Source: "user",
		},
	}

	// Broker side: serialize
	encoded, err := serializeSecrets("/unused", secrets)
	if err != nil {
		t.Fatalf("serializeSecrets failed: %v", err)
	}

	// Container side: decode + write
	staged, err := stagedsecrets.Decode(encoded)
	if err != nil {
		t.Fatalf("stagedsecrets.Decode failed: %v", err)
	}
	if err := stagedsecrets.Write(homeDir, staged); err != nil {
		t.Fatalf("stagedsecrets.Write failed: %v", err)
	}

	// Verify file secret
	content, err := os.ReadFile(filepath.Join(targetDir, "cert.pem"))
	if err != nil {
		t.Fatalf("failed to read cert.pem: %v", err)
	}
	if string(content) != "my-cert" {
		t.Errorf("expected my-cert, got %q", string(content))
	}

	// Verify variable secret
	data, err := os.ReadFile(filepath.Join(homeDir, ".scion", "secrets.json"))
	if err != nil {
		t.Fatalf("failed to read secrets.json: %v", err)
	}
	var vars map[string]string
	if err := json.Unmarshal(data, &vars); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	if vars["app_config"] != `{"db":"postgres"}` {
		t.Errorf("variable secret mismatch: got %q", vars["app_config"])
	}
}

func TestExpandTildeTarget(t *testing.T) {
	tests := []struct {
		target        string
		containerHome string
		expected      string
	}{
		{"~/.ssh/id_rsa", "/home/gemini", "/home/gemini/.ssh/id_rsa"},
		{"~/config.json", "/home/scion", "/home/scion/config.json"},
		{"/etc/ssl/cert.pem", "/home/gemini", "/etc/ssl/cert.pem"},
		{"~", "/home/gemini", "~"}, // bare ~ without / is not expanded
	}
	for _, tc := range tests {
		result := expandTildeTarget(tc.target, tc.containerHome)
		if result != tc.expected {
			t.Errorf("expandTildeTarget(%q, %q) = %q, want %q", tc.target, tc.containerHome, result, tc.expected)
		}
	}
}

func TestFindGCPTelemetryCredentialPath_Present(t *testing.T) {
	secrets := []api.ResolvedSecret{
		{Name: "other-secret", Type: "file", Target: "/etc/other", Value: "data"},
		{Name: "scion-telemetry-gcp-credentials", Type: "file", Target: "/etc/gcp/sa.json", Value: "key-data"},
	}

	got := findGCPTelemetryCredentialPath(secrets, "/home/scion")
	want := "/etc/gcp/sa.json"
	if got != want {
		t.Errorf("findGCPTelemetryCredentialPath() = %q, want %q", got, want)
	}
}

func TestFindGCPTelemetryCredentialPath_Absent(t *testing.T) {
	secrets := []api.ResolvedSecret{
		{Name: "other-secret", Type: "file", Target: "/etc/other", Value: "data"},
	}

	got := findGCPTelemetryCredentialPath(secrets, "/home/scion")
	if got != "" {
		t.Errorf("findGCPTelemetryCredentialPath() = %q, want empty string", got)
	}
}

func TestFindGCPTelemetryCredentialPath_WrongType(t *testing.T) {
	secrets := []api.ResolvedSecret{
		{Name: "scion-telemetry-gcp-credentials", Type: "environment", Target: "GCP_CREDS", Value: "key-data"},
	}

	got := findGCPTelemetryCredentialPath(secrets, "/home/scion")
	if got != "" {
		t.Errorf("findGCPTelemetryCredentialPath() = %q, want empty string for environment type", got)
	}
}

func TestFindGCPTelemetryCredentialPath_TildeExpansion(t *testing.T) {
	secrets := []api.ResolvedSecret{
		{Name: "scion-telemetry-gcp-credentials", Type: "file", Target: "~/.config/gcp/sa.json", Value: "key-data"},
	}

	got := findGCPTelemetryCredentialPath(secrets, "/home/gemini")
	want := "/home/gemini/.config/gcp/sa.json"
	if got != want {
		t.Errorf("findGCPTelemetryCredentialPath() = %q, want %q", got, want)
	}
}

func TestBuildCommonRunArgs_EnvironmentSecrets(t *testing.T) {
	secrets := []api.ResolvedSecret{
		{Name: "API_KEY", Type: "environment", Target: "API_KEY", Value: "sk-123", Source: "user"},
		{Name: "DB_PASS", Type: "environment", Target: "DATABASE_PASSWORD", Value: "secret", Source: "project"},
		{Name: "CONFIG", Type: "variable", Target: "config", Value: "json-data", Source: "user"},
		{Name: "CERT", Type: "file", Target: "/etc/cert.pem", Value: "data", Source: "user"},
	}

	config := RunConfig{
		Name:            "test-agent",
		UnixUsername:    "scion",
		Image:           "test:latest",
		Harness:         harness.New("gemini"),
		ResolvedSecrets: secrets,
	}

	args, err := buildCommonRunArgs(config)
	if err != nil {
		t.Fatalf("buildCommonRunArgs failed: %v", err)
	}

	argsStr := joinArgs(args)

	// Environment secrets should be injected
	if !containsArg(args, "-e", "API_KEY=sk-123") {
		t.Errorf("expected environment secret API_KEY in args, got: %s", argsStr)
	}
	if !containsArg(args, "-e", "DATABASE_PASSWORD=secret") {
		t.Errorf("expected environment secret DATABASE_PASSWORD in args, got: %s", argsStr)
	}

	// Variable and file secrets should NOT be injected as env vars
	if containsArg(args, "-e", "config=json-data") {
		t.Errorf("variable secret should not be injected as env var")
	}
}

// containsArg checks if the args slice contains flag followed by value.
func containsArg(args []string, flag, value string) bool {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

func joinArgs(args []string) string {
	result := ""
	for _, a := range args {
		result += a + " "
	}
	return result
}
