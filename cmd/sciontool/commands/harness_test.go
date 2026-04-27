/*
Copyright 2026 The Scion Authors.
*/

package commands

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func writeManifest(t *testing.T, dir string, m containerProvisionManifest) string {
	t.Helper()
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	path := filepath.Join(dir, "manifest.json")
	writeTestFile(t, path, string(data))
	return path
}

func baseManifest(t *testing.T, home, scriptPath string) containerProvisionManifest {
	t.Helper()
	bundle := filepath.Join(home, ".scion", "harness")
	return containerProvisionManifest{
		SchemaVersion:    1,
		Command:          "provision",
		AgentName:        "agent",
		AgentHome:        home,
		AgentWorkspace:   filepath.Join(home, "workspace"),
		HarnessBundleDir: bundle,
		HarnessConfig: containerHarnessCfg{
			Harness: "test",
			Provisioner: &containerProvisioner{
				Type:             "container-script",
				InterfaceVersion: 1,
				Command:          []string{scriptPath},
				Timeout:          "5s",
			},
		},
		Inputs: map[string]string{},
		Outputs: containerOutputs{
			Env:          filepath.Join(bundle, "outputs", "env.json"),
			ResolvedAuth: filepath.Join(bundle, "outputs", "resolved-auth.json"),
		},
		Platform: map[string]string{"goos": "linux"},
	}
}

func TestRunHarnessProvision_Success(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	bundle := filepath.Join(home, ".scion", "harness")
	if err := os.MkdirAll(filepath.Join(bundle, "outputs"), 0755); err != nil {
		t.Fatal(err)
	}
	envOut := filepath.Join(bundle, "outputs", "env.json")
	authOut := filepath.Join(bundle, "outputs", "resolved-auth.json")

	scriptPath := filepath.Join(bundle, "provision.sh")
	writeTestFile(t, scriptPath, "#!/bin/sh\nprintf '{\"ANTHROPIC_API_KEY\":{\"from_file\":\"/tmp/x\"}}\\n' > \""+envOut+"\"\nprintf '{\"method\":\"api-key\"}\\n' > \""+authOut+"\"\nexit 0\n")
	if err := os.Chmod(scriptPath, 0755); err != nil {
		t.Fatal(err)
	}

	manifest := baseManifest(t, home, scriptPath)
	manifestPath := writeManifest(t, bundle, manifest)

	if err := runHarnessProvision(context.Background(), manifestPath); err != nil {
		t.Fatalf("runHarnessProvision: %v", err)
	}

	for _, want := range []string{envOut, authOut} {
		if _, err := os.Stat(want); err != nil {
			t.Errorf("missing output %s: %v", want, err)
		}
	}
}

func TestRunHarnessProvision_RejectsManifestWithEscapingPaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	bundle := filepath.Join(home, ".scion", "harness")
	if err := os.MkdirAll(bundle, 0755); err != nil {
		t.Fatal(err)
	}

	scriptPath := filepath.Join(bundle, "noop.sh")
	writeTestFile(t, scriptPath, "#!/bin/sh\nexit 0\n")
	_ = os.Chmod(scriptPath, 0755)

	manifest := baseManifest(t, home, scriptPath)
	manifest.Outputs.Env = "/etc/passwd" // escapes allowed roots
	manifestPath := writeManifest(t, bundle, manifest)

	err := runHarnessProvision(context.Background(), manifestPath)
	if err == nil {
		t.Fatal("expected error for path escape")
	}
	if !strings.Contains(err.Error(), "escapes allowed roots") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestRunHarnessProvision_TimesOut(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	bundle := filepath.Join(home, ".scion", "harness")
	if err := os.MkdirAll(bundle, 0755); err != nil {
		t.Fatal(err)
	}

	scriptPath := filepath.Join(bundle, "sleep.sh")
	writeTestFile(t, scriptPath, "#!/bin/sh\nsleep 5\nexit 0\n")
	_ = os.Chmod(scriptPath, 0755)

	manifest := baseManifest(t, home, scriptPath)
	manifest.HarnessConfig.Provisioner.Timeout = "100ms"
	manifestPath := writeManifest(t, bundle, manifest)

	err := runHarnessProvision(context.Background(), manifestPath)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") && !strings.Contains(err.Error(), "signal") {
		t.Errorf("unexpected error (want timed out/signal): %v", err)
	}
}

func TestRunHarnessProvision_RejectsUnknownSchemaVersion(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	bundle := filepath.Join(home, ".scion", "harness")
	if err := os.MkdirAll(bundle, 0755); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(bundle, "noop.sh")
	writeTestFile(t, scriptPath, "#!/bin/sh\nexit 0\n")
	_ = os.Chmod(scriptPath, 0755)

	manifest := baseManifest(t, home, scriptPath)
	manifest.SchemaVersion = 99
	manifestPath := writeManifest(t, bundle, manifest)

	err := runHarnessProvision(context.Background(), manifestPath)
	if err == nil || !strings.Contains(err.Error(), "schema_version") {
		t.Fatalf("expected schema_version error, got %v", err)
	}
}

func TestRunHarnessProvision_RejectsMissingProvisioner(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	bundle := filepath.Join(home, ".scion", "harness")
	if err := os.MkdirAll(bundle, 0755); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(bundle, "noop.sh")
	writeTestFile(t, scriptPath, "#!/bin/sh\nexit 0\n")
	_ = os.Chmod(scriptPath, 0755)

	manifest := baseManifest(t, home, scriptPath)
	manifest.HarnessConfig.Provisioner = nil
	manifestPath := writeManifest(t, bundle, manifest)

	err := runHarnessProvision(context.Background(), manifestPath)
	if err == nil || !strings.Contains(err.Error(), "container-script") {
		t.Fatalf("expected provisioner type error, got %v", err)
	}
}

func TestRunHarnessProvision_InvalidEnvJSONFails(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	bundle := filepath.Join(home, ".scion", "harness")
	if err := os.MkdirAll(filepath.Join(bundle, "outputs"), 0755); err != nil {
		t.Fatal(err)
	}
	envOut := filepath.Join(bundle, "outputs", "env.json")
	scriptPath := filepath.Join(bundle, "writebad.sh")
	writeTestFile(t, scriptPath, "#!/bin/sh\nprintf 'not-json' > \""+envOut+"\"\nexit 0\n")
	_ = os.Chmod(scriptPath, 0755)

	manifest := baseManifest(t, home, scriptPath)
	manifestPath := writeManifest(t, bundle, manifest)

	err := runHarnessProvision(context.Background(), manifestPath)
	if err == nil || !strings.Contains(err.Error(), "invalid env output") {
		t.Fatalf("expected env validation error, got %v", err)
	}
}

func TestRunHarnessProvision_PropagatesScriptStderr(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	bundle := filepath.Join(home, ".scion", "harness")
	if err := os.MkdirAll(bundle, 0755); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(bundle, "fail.sh")
	writeTestFile(t, scriptPath, "#!/bin/sh\necho boom >&2\nexit 1\n")
	_ = os.Chmod(scriptPath, 0755)

	manifest := baseManifest(t, home, scriptPath)
	manifestPath := writeManifest(t, bundle, manifest)

	err := runHarnessProvision(context.Background(), manifestPath)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected stderr in error, got %v", err)
	}
}

func TestRunHarnessProvision_ExitCodeTwoIsUnsupported(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	bundle := filepath.Join(home, ".scion", "harness")
	if err := os.MkdirAll(bundle, 0755); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(bundle, "unsupported.sh")
	writeTestFile(t, scriptPath, "#!/bin/sh\nexit 2\n")
	_ = os.Chmod(scriptPath, 0755)

	manifest := baseManifest(t, home, scriptPath)
	manifestPath := writeManifest(t, bundle, manifest)

	err := runHarnessProvision(context.Background(), manifestPath)
	if err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("expected unsupported command error, got %v", err)
	}
}

func TestScrubSecrets_RedactsAuthCandidateValues(t *testing.T) {
	home := t.TempDir()
	bundle := filepath.Join(home, ".scion", "harness")
	if err := os.MkdirAll(filepath.Join(bundle, "inputs"), 0755); err != nil {
		t.Fatal(err)
	}
	candidatesPath := filepath.Join(bundle, "inputs", "auth-candidates.json")
	writeTestFile(t, candidatesPath, `{"env_vars":["sk-secret-value-here"]}`)

	m := &containerProvisionManifest{
		Inputs: map[string]string{"auth_candidates": candidatesPath},
	}
	scrubbed := scrubSecrets("the value sk-secret-value-here was leaked", m)
	if strings.Contains(scrubbed, "sk-secret-value-here") {
		t.Errorf("secret not redacted: %q", scrubbed)
	}
	if !strings.Contains(scrubbed, "[REDACTED]") {
		t.Errorf("missing redaction marker: %q", scrubbed)
	}
}
