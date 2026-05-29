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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
)

func TestWriteFileSecrets(t *testing.T) {
	homeDir := t.TempDir()

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
	}

	mountSpecs, err := writeFileSecrets(homeDir, "/home/scion", secrets)
	if err != nil {
		t.Fatalf("writeFileSecrets failed: %v", err)
	}

	// Should only produce mount specs for file-type secrets
	if len(mountSpecs) != 2 {
		t.Fatalf("expected 2 mount specs, got %d", len(mountSpecs))
	}

	// Verify the first file was written with decoded base64 content
	secretsDir := filepath.Join(filepath.Dir(homeDir), "secrets")
	content, err := os.ReadFile(filepath.Join(secretsDir, "TLS_CERT"))
	if err != nil {
		t.Fatalf("failed to read TLS_CERT file: %v", err)
	}
	if string(content) != "cert-content" {
		t.Errorf("expected decoded content %q, got %q", "cert-content", string(content))
	}

	// Verify the second file was written with raw content (fallback)
	content, err = os.ReadFile(filepath.Join(secretsDir, "SSH_KEY"))
	if err != nil {
		t.Fatalf("failed to read SSH_KEY file: %v", err)
	}
	if string(content) != "raw-value-not-base64" {
		t.Errorf("expected raw content %q, got %q", "raw-value-not-base64", string(content))
	}

	// Verify file permissions
	info, err := os.Stat(filepath.Join(secretsDir, "TLS_CERT"))
	if err != nil {
		t.Fatalf("failed to stat TLS_CERT: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("expected file mode 0600, got %o", info.Mode().Perm())
	}
}

func TestWriteFileSecrets_NoFileSecrets(t *testing.T) {
	homeDir := t.TempDir()

	secrets := []api.ResolvedSecret{
		{Name: "KEY", Type: "environment", Target: "KEY", Value: "val"},
	}

	mountSpecs, err := writeFileSecrets(homeDir, "/home/scion", secrets)
	if err != nil {
		t.Fatalf("writeFileSecrets failed: %v", err)
	}
	if len(mountSpecs) != 0 {
		t.Errorf("expected 0 mount specs for non-file secrets, got %d", len(mountSpecs))
	}
}

func TestWriteFileSecrets_TildeExpansion(t *testing.T) {
	homeDir := t.TempDir()

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

	mountSpecs, err := writeFileSecrets(homeDir, "/home/gemini", secrets)
	if err != nil {
		t.Fatalf("writeFileSecrets failed: %v", err)
	}

	if len(mountSpecs) != 2 {
		t.Fatalf("expected 2 mount specs, got %d", len(mountSpecs))
	}

	// Verify ~/ was expanded to the container home directory
	secretsDir := filepath.Join(filepath.Dir(homeDir), "secrets")
	expectedMount0 := filepath.Join(secretsDir, "SSH_KEY") + ":/home/gemini/.ssh/id_rsa:ro"
	if mountSpecs[0] != expectedMount0 {
		t.Errorf("expected mount spec %q, got %q", expectedMount0, mountSpecs[0])
	}

	// Verify absolute path is unchanged
	expectedMount1 := filepath.Join(secretsDir, "ABS_CERT") + ":/etc/ssl/cert.pem:ro"
	if mountSpecs[1] != expectedMount1 {
		t.Errorf("expected mount spec %q, got %q", expectedMount1, mountSpecs[1])
	}
}

func TestWriteFileSecrets_PreCreatesParentDirs(t *testing.T) {
	// writeFileSecrets should pre-create the parent directory of file secret
	// mount targets inside the agent home so Docker does not create them as
	// root (which makes the agent dir undeletable by non-root users).
	homeDir := t.TempDir()

	secrets := []api.ResolvedSecret{
		{
			Name:   "telemetry-creds",
			Type:   "file",
			Target: "~/.scion/telemetry-gcp-credentials.json",
			Value:  base64.StdEncoding.EncodeToString([]byte("cred-data")),
			Source: "project",
		},
		{
			Name:   "nested-secret",
			Type:   "file",
			Target: "~/.config/deep/nested/secret.json",
			Value:  base64.StdEncoding.EncodeToString([]byte("nested-data")),
			Source: "user",
		},
		{
			Name:   "abs-secret",
			Type:   "file",
			Target: "/etc/ssl/cert.pem",
			Value:  base64.StdEncoding.EncodeToString([]byte("cert")),
			Source: "user",
		},
	}

	_, err := writeFileSecrets(homeDir, "/home/scion", secrets)
	if err != nil {
		t.Fatalf("writeFileSecrets failed: %v", err)
	}

	// .scion dir should be pre-created inside the agent home
	scionDir := filepath.Join(homeDir, ".scion")
	info, err := os.Stat(scionDir)
	if err != nil {
		t.Fatalf("expected %s to exist, got: %v", scionDir, err)
	}
	if !info.IsDir() {
		t.Fatalf("expected %s to be a directory", scionDir)
	}

	// Nested parent dirs should also be pre-created
	nestedDir := filepath.Join(homeDir, ".config", "deep", "nested")
	info, err = os.Stat(nestedDir)
	if err != nil {
		t.Fatalf("expected %s to exist, got: %v", nestedDir, err)
	}
	if !info.IsDir() {
		t.Fatalf("expected %s to be a directory", nestedDir)
	}

	// Absolute path outside container home should NOT create dirs inside agent home
	etcDir := filepath.Join(homeDir, "etc", "ssl")
	if _, err := os.Stat(etcDir); !os.IsNotExist(err) {
		t.Errorf("expected %s to NOT exist for absolute target outside container home", etcDir)
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

func TestWriteVariableSecrets(t *testing.T) {
	homeDir := t.TempDir()

	secrets := []api.ResolvedSecret{
		{Name: "CONFIG", Type: "variable", Target: "config", Value: `{"a":"b"}`, Source: "user"},
		{Name: "TOKEN", Type: "variable", Target: "token", Value: "abc123", Source: "project"},
		{Name: "ENV_KEY", Type: "environment", Target: "ENV_KEY", Value: "val", Source: "user"},
	}

	if err := writeVariableSecrets(homeDir, secrets); err != nil {
		t.Fatalf("writeVariableSecrets failed: %v", err)
	}

	// Read and verify secrets.json
	data, err := os.ReadFile(filepath.Join(homeDir, ".scion", "secrets.json"))
	if err != nil {
		t.Fatalf("failed to read secrets.json: %v", err)
	}

	var vars map[string]string
	if err := json.Unmarshal(data, &vars); err != nil {
		t.Fatalf("failed to unmarshal secrets.json: %v", err)
	}

	if len(vars) != 2 {
		t.Fatalf("expected 2 variable entries, got %d", len(vars))
	}
	if vars["config"] != `{"a":"b"}` {
		t.Errorf("config value mismatch: got %q", vars["config"])
	}
	if vars["token"] != "abc123" {
		t.Errorf("token value mismatch: got %q", vars["token"])
	}

	// Verify file permissions
	info, err := os.Stat(filepath.Join(homeDir, ".scion", "secrets.json"))
	if err != nil {
		t.Fatalf("failed to stat secrets.json: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("expected file mode 0600, got %o", info.Mode().Perm())
	}
}

func TestWriteVariableSecrets_NoVariables(t *testing.T) {
	homeDir := t.TempDir()

	secrets := []api.ResolvedSecret{
		{Name: "KEY", Type: "environment", Target: "KEY", Value: "val"},
	}

	if err := writeVariableSecrets(homeDir, secrets); err != nil {
		t.Fatalf("writeVariableSecrets failed: %v", err)
	}

	// secrets.json should NOT be created when there are no variable secrets
	if _, err := os.Stat(filepath.Join(homeDir, ".scion", "secrets.json")); !os.IsNotExist(err) {
		t.Error("expected secrets.json to not be created when no variable secrets exist")
	}
}

func TestWriteSecretMap(t *testing.T) {
	homeDir := t.TempDir()

	secrets := []api.ResolvedSecret{
		{Name: "CERT", Type: "file", Target: "/etc/ssl/cert.pem", Value: "data", Source: "user"},
		{Name: "KEY", Type: "file", Target: "/etc/ssl/key.pem", Value: "data", Source: "project"},
		{Name: "ENV", Type: "environment", Target: "ENV", Value: "val", Source: "user"},
	}

	if err := writeSecretMap(homeDir, "/home/scion", secrets); err != nil {
		t.Fatalf("writeSecretMap failed: %v", err)
	}

	secretsDir := filepath.Join(filepath.Dir(homeDir), "secrets")
	data, err := os.ReadFile(filepath.Join(secretsDir, "secret-map.json"))
	if err != nil {
		t.Fatalf("failed to read secret-map.json: %v", err)
	}

	var entries []secretMapEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		t.Fatalf("failed to unmarshal secret-map.json: %v", err)
	}

	if len(entries) != 2 {
		t.Fatalf("expected 2 entries in secret-map.json, got %d", len(entries))
	}

	if entries[0].Name != "CERT" || entries[0].Target != "/etc/ssl/cert.pem" || entries[0].Source != "CERT" {
		t.Errorf("unexpected first entry: %+v", entries[0])
	}
	if entries[1].Name != "KEY" || entries[1].Target != "/etc/ssl/key.pem" || entries[1].Source != "KEY" {
		t.Errorf("unexpected second entry: %+v", entries[1])
	}
}

func TestWriteSecretMap_NoFileSecrets(t *testing.T) {
	homeDir := t.TempDir()

	secrets := []api.ResolvedSecret{
		{Name: "KEY", Type: "environment", Target: "KEY", Value: "val"},
	}

	if err := writeSecretMap(homeDir, "/home/scion", secrets); err != nil {
		t.Fatalf("writeSecretMap failed: %v", err)
	}

	secretsDir := filepath.Join(filepath.Dir(homeDir), "secrets")
	if _, err := os.Stat(filepath.Join(secretsDir, "secret-map.json")); !os.IsNotExist(err) {
		t.Error("expected secret-map.json to not be created when no file secrets exist")
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

func TestInsertVolumeFlags(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		image      string
		mountSpecs []string
		want       []string
	}{
		{
			name:       "inserts before image",
			args:       []string{"run", "-d", "-e", "FOO=bar", "myimage:latest", "tmux", "new-session"},
			image:      "myimage:latest",
			mountSpecs: []string{"/host/secret:/container/secret:ro"},
			want:       []string{"run", "-d", "-e", "FOO=bar", "-v", "/host/secret:/container/secret:ro", "myimage:latest", "tmux", "new-session"},
		},
		{
			name:       "multiple mount specs",
			args:       []string{"run", "-d", "img:v1", "cmd"},
			image:      "img:v1",
			mountSpecs: []string{"/a:/b:ro", "/c:/d:ro"},
			want:       []string{"run", "-d", "-v", "/a:/b:ro", "-v", "/c:/d:ro", "img:v1", "cmd"},
		},
		{
			name:       "no mount specs returns args unchanged",
			args:       []string{"run", "-d", "img:v1", "cmd"},
			image:      "img:v1",
			mountSpecs: nil,
			want:       []string{"run", "-d", "img:v1", "cmd"},
		},
		{
			name:       "nil mount specs returns args unchanged",
			args:       []string{"run", "-d", "img:v1", "cmd"},
			image:      "img:v1",
			mountSpecs: []string{},
			want:       []string{"run", "-d", "img:v1", "cmd"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := insertVolumeFlags(tc.args, tc.image, tc.mountSpecs)
			if len(got) != len(tc.want) {
				t.Fatalf("length mismatch: got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("index %d: got %q, want %q\nfull result: %v", i, got[i], tc.want[i], got)
					break
				}
			}
		})
	}
}

func TestInsertVolumeFlags_SecretMountsBeforeImage(t *testing.T) {
	// Simulate the full flow: buildCommonRunArgs produces args with image+command at the end,
	// then insertVolumeFlags should place secret mounts before the image.
	config := RunConfig{
		Name:         "test-agent",
		UnixUsername: "scion",
		Image:        "test-image:latest",
		Harness:      harness.New("gemini"),
	}

	args, err := buildCommonRunArgs(config)
	if err != nil {
		t.Fatalf("buildCommonRunArgs failed: %v", err)
	}

	secretSpecs := []string{"/host/secrets/CERT:/etc/ssl/cert.pem:ro"}
	result := insertVolumeFlags(args, config.Image, secretSpecs)

	// Find the positions of the secret mount and image in the result
	secretIdx := -1
	imageIdx := -1
	for i, a := range result {
		if a == "/host/secrets/CERT:/etc/ssl/cert.pem:ro" {
			secretIdx = i
		}
		if a == "test-image:latest" {
			imageIdx = i
		}
	}

	if secretIdx < 0 {
		t.Fatal("secret mount spec not found in result args")
	}
	if imageIdx < 0 {
		t.Fatal("image not found in result args")
	}
	if secretIdx >= imageIdx {
		t.Errorf("secret mount (index %d) should appear before image (index %d), args: %v", secretIdx, imageIdx, result)
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
