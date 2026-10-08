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

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
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
	if err == nil || !strings.Contains(err.Error(), "provisioner block") {
		t.Fatalf("expected missing provisioner error, got %v", err)
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

func TestRunHarnessProvision_ResolvesHomePrefixInManifestPaths(t *testing.T) {
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

	// Build manifest with literal $HOME paths, matching what the host-side
	// containerBundlePath() produces for container portability.
	manifest := containerProvisionManifest{
		SchemaVersion:    1,
		Command:          "provision",
		AgentName:        "agent",
		AgentHome:        "$HOME",
		AgentWorkspace:   "$HOME/workspace",
		HarnessBundleDir: "$HOME/.scion/harness",
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
			Env:          "$HOME/.scion/harness/outputs/env.json",
			ResolvedAuth: "$HOME/.scion/harness/outputs/resolved-auth.json",
		},
		Platform: map[string]string{"goos": "linux"},
	}
	manifestPath := writeManifest(t, bundle, manifest)

	if err := runHarnessProvision(context.Background(), manifestPath); err != nil {
		t.Fatalf("runHarnessProvision with $HOME paths: %v", err)
	}

	for _, want := range []string{envOut, authOut} {
		if _, err := os.Stat(want); err != nil {
			t.Errorf("missing output %s: %v", want, err)
		}
	}
}

func TestResolveManifestHomePaths(t *testing.T) {
	m := &containerProvisionManifest{
		HarnessBundleDir: "$HOME/.scion/harness",
		AgentHome:        "$HOME",
		AgentWorkspace:   "$HOME/workspace",
		Outputs: containerOutputs{
			Env:          "$HOME/.scion/harness/outputs/env.json",
			ResolvedAuth: "$HOME/.scion/harness/outputs/resolved-auth.json",
			Status:       "$HOME/.scion/harness/outputs/status.json",
		},
		Inputs: map[string]string{
			"auth_candidates": "$HOME/.scion/harness/inputs/auth-candidates.json",
		},
	}

	t.Setenv("HOME", "/home/scion")
	resolveManifestHomePaths(m, "/home/scion")

	if m.HarnessBundleDir != "/home/scion/.scion/harness" {
		t.Errorf("HarnessBundleDir = %q, want /home/scion/.scion/harness", m.HarnessBundleDir)
	}
	if m.AgentHome != "/home/scion" {
		t.Errorf("AgentHome = %q, want /home/scion", m.AgentHome)
	}
	if m.Outputs.Env != "/home/scion/.scion/harness/outputs/env.json" {
		t.Errorf("Outputs.Env = %q", m.Outputs.Env)
	}
	if m.Inputs["auth_candidates"] != "/home/scion/.scion/harness/inputs/auth-candidates.json" {
		t.Errorf("Inputs[auth_candidates] = %q", m.Inputs["auth_candidates"])
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

// TestRunHarnessProvision_HarnessOutputsDirEnv checks that with
// SCION_HARNESS_OUTPUTS_DIR set the provisioner sees the variable and its
// outputs are validated there, and the bundle's outputs/ is not used.
func TestRunHarnessProvision_HarnessOutputsDirEnv(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
		wantErr string
	}{{"valid", `{"K":"v"}`, ""}, {"invalid", "not-json", "invalid env output"}} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			memRoot := t.TempDir()
			t.Cleanup(hooks.SetHarnessDirsRootForTest(memRoot))
			outDir := filepath.Join(memRoot, "outputs")
			t.Setenv("SCION_HARNESS_OUTPUTS_DIR", outDir)
			t.Setenv("SCION_HARNESS_SECRETS_DIR", "")

			bundle := filepath.Join(home, ".scion", "harness")
			scriptPath := filepath.Join(bundle, "provision.sh")
			writeTestFile(t, scriptPath, "#!/bin/sh\nmkdir -p \"$SCION_HARNESS_OUTPUTS_DIR\"\nprintf '"+tc.content+"' > \"$SCION_HARNESS_OUTPUTS_DIR/env.json\"\nexit 0\n")
			if err := os.Chmod(scriptPath, 0755); err != nil {
				t.Fatal(err)
			}
			manifestPath := writeManifest(t, bundle, baseManifest(t, home, scriptPath))

			err := runHarnessProvision(context.Background(), manifestPath)
			if tc.wantErr == "" && err != nil {
				t.Fatalf("runHarnessProvision: %v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
			if _, err := os.Stat(filepath.Join(outDir, "env.json")); err != nil {
				t.Errorf("output not in SCION_HARNESS_OUTPUTS_DIR: %v", err)
			}
			if _, err := os.Stat(filepath.Join(bundle, "outputs")); err == nil {
				t.Error("bundle outputs/ was used")
			}
		})
	}
}

func TestRunHarnessProvision_RelativeHarnessDirRejected(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SCION_HARNESS_OUTPUTS_DIR", "outputs")
	bundle := filepath.Join(home, ".scion", "harness")
	scriptPath := filepath.Join(bundle, "noop.sh")
	writeTestFile(t, scriptPath, "#!/bin/sh\nexit 0\n")
	_ = os.Chmod(scriptPath, 0755)
	err := runHarnessProvision(context.Background(), writeManifest(t, bundle, baseManifest(t, home, scriptPath)))
	if err == nil || !strings.Contains(err.Error(), "SCION_HARNESS_OUTPUTS_DIR must be an absolute path") {
		t.Fatalf("err = %v", err)
	}
}

// TestRunHarnessProvision_HarnessDirOutsideMemDirRejected checks that a
// directory override outside the in-memory directory stops provisioning
// before the provisioner runs.
func TestRunHarnessProvision_HarnessDirOutsideMemDirRejected(t *testing.T) {
	for _, tc := range []struct{ env, value string }{
		{"SCION_HARNESS_OUTPUTS_DIR", "/"},
		{"SCION_HARNESS_OUTPUTS_DIR", "/etc"},
		{"SCION_HARNESS_SECRETS_DIR", "/run/scion/mem/../agent-secrets"},
		{"SCION_HARNESS_SECRETS_DIR", "/run/scion/memx"},
	} {
		t.Run(tc.env+"="+tc.value, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("SCION_HARNESS_OUTPUTS_DIR", "")
			t.Setenv("SCION_HARNESS_SECRETS_DIR", "")
			t.Setenv(tc.env, tc.value)
			bundle := filepath.Join(home, ".scion", "harness")
			ran := filepath.Join(home, "ran")
			scriptPath := filepath.Join(bundle, "touch.sh")
			writeTestFile(t, scriptPath, "#!/bin/sh\ntouch '"+ran+"'\nexit 0\n")
			_ = os.Chmod(scriptPath, 0755)
			err := runHarnessProvision(context.Background(), writeManifest(t, bundle, baseManifest(t, home, scriptPath)))
			if err == nil || !strings.Contains(err.Error(), tc.env+" must be a directory below /run/scion/mem") {
				t.Fatalf("err = %v", err)
			}
			if _, err := os.Stat(ran); err == nil {
				t.Error("provisioner ran")
			}
		})
	}
}

// TestScrubSecrets_HarnessSecretsDir checks that staged secret values are
// read from the bundle's secrets/ always, and also from
// SCION_HARNESS_SECRETS_DIR when it is set.
func TestScrubSecrets_HarnessSecretsDir(t *testing.T) {
	home := t.TempDir()
	bundle := filepath.Join(home, ".scion", "harness")
	memRoot := t.TempDir()
	t.Cleanup(hooks.SetHarnessDirsRootForTest(memRoot))
	memDir := filepath.Join(memRoot, "harness-secrets")
	writeTestFile(t, filepath.Join(bundle, "secrets", "A"), "bundle-secret-value\n")
	writeTestFile(t, filepath.Join(memDir, "A"), "memory-secret-value\n")
	m := &containerProvisionManifest{HarnessBundleDir: bundle}
	const line = "bundle-secret-value memory-secret-value"

	t.Setenv("SCION_HARNESS_SECRETS_DIR", "")
	if got := scrubSecrets(line, m); got != "[REDACTED] memory-secret-value" {
		t.Errorf("unset: %q", got)
	}
	t.Setenv("SCION_HARNESS_SECRETS_DIR", memDir)
	if got := scrubSecrets(line, m); got != "[REDACTED] [REDACTED]" {
		t.Errorf("set: %q", got)
	}
	t.Setenv("SCION_HARNESS_SECRETS_DIR", "/etc")
	if got := scrubSecrets(line, m); got != "[REDACTED] memory-secret-value" {
		t.Errorf("rejected override: %q", got)
	}
}

// A multi-line staged file is masked as a whole and line by line, so a
// single line of it in the output is masked too.
func TestScrubSecrets_MultiLineStagedFileMasksEachLine(t *testing.T) {
	t.Setenv("SCION_HARNESS_SECRETS_DIR", "")
	bundle := filepath.Join(t.TempDir(), ".scion", "harness")
	writeTestFile(t, filepath.Join(bundle, "secrets", "A"),
		"placeholder-line-one-value\n  placeholder-line-two-value \r\n\n")
	m := &containerProvisionManifest{HarnessBundleDir: bundle}

	if got := scrubSecrets("found placeholder-line-two-value in config", m); got != "found [REDACTED] in config" {
		t.Errorf("single line: %q", got)
	}
	whole := "placeholder-line-one-value\n  placeholder-line-two-value"
	if got := scrubSecrets("dump: "+whole+" end", m); got != "dump: [REDACTED] end" {
		t.Errorf("whole value: %q", got)
	}
}

// A staged value shorter than minMaskLen cannot be masked in place, so the
// output is omitted when it occurs there, and left alone when it does not.
func TestScrubSecrets_ShortStagedValueOmitsOutput(t *testing.T) {
	t.Setenv("SCION_HARNESS_SECRETS_DIR", "")
	bundle := filepath.Join(t.TempDir(), ".scion", "harness")
	writeTestFile(t, filepath.Join(bundle, "secrets", "SHORT"), "plv-1x\n")
	writeTestFile(t, filepath.Join(bundle, "secrets", "LONG"), "placeholder-value-1")
	m := &containerProvisionManifest{HarnessBundleDir: bundle}

	if got := scrubSecrets("auth failed for plv-1x", m); got != provisionerOutputOmitted {
		t.Errorf("short value present: %q", got)
	}
	if got := scrubSecrets("auth failed: placeholder-value-1", m); got != "auth failed: [REDACTED]" {
		t.Errorf("short value absent: %q", got)
	}
}

// A short line of a multi-line staged file is handled like a short value.
func TestScrubSecrets_ShortLineOfMultiLineFileOmitsOutput(t *testing.T) {
	t.Setenv("SCION_HARNESS_SECRETS_DIR", "")
	bundle := filepath.Join(t.TempDir(), ".scion", "harness")
	writeTestFile(t, filepath.Join(bundle, "secrets", "A"), "placeholder-user-value\nplv-2y\n")
	m := &containerProvisionManifest{HarnessBundleDir: bundle}

	if got := scrubSecrets("password plv-2y rejected", m); got != provisionerOutputOmitted {
		t.Errorf("short line present: %q", got)
	}
}

// Punctuation-only lines of a pretty-printed JSON staged file are skipped:
// a brace in the output does not omit it, and the lines holding the secret
// are still masked.
func TestScrubSecrets_JSONStagedFileSkipsPunctuationLines(t *testing.T) {
	t.Setenv("SCION_HARNESS_SECRETS_DIR", "")
	bundle := filepath.Join(t.TempDir(), ".scion", "harness")
	writeTestFile(t, filepath.Join(bundle, "secrets", "AUTH"),
		"{\n  \"tokens\": {\n    \"access_token\": \"placeholder-token-value-1\"\n  },\n  \"items\": [\n    \"placeholder-item-value-2\"\n  ]\n}\n")
	m := &containerProvisionManifest{HarnessBundleDir: bundle}

	in := `parsed {"mode": 1} and [] then saw "access_token": "placeholder-token-value-1" and "placeholder-item-value-2"`
	want := `parsed {"mode": 1} and [] then saw [REDACTED] and [REDACTED]`
	if got := scrubSecrets(in, m); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// When one staged value contains another, the longer one is masked whole
// rather than leaving the rest of it behind.
func TestScrubSecrets_LongerStagedValueMaskedFirst(t *testing.T) {
	t.Setenv("SCION_HARNESS_SECRETS_DIR", "")
	bundle := filepath.Join(t.TempDir(), ".scion", "harness")
	writeTestFile(t, filepath.Join(bundle, "secrets", "A"), "placeholder-value-1")
	writeTestFile(t, filepath.Join(bundle, "secrets", "B"), "placeholder-value-1-extended")
	m := &containerProvisionManifest{HarnessBundleDir: bundle}

	if got := scrubSecrets("saw placeholder-value-1-extended here", m); got != "saw [REDACTED] here" {
		t.Errorf("got %q", got)
	}
}

// Staged values are trimmed like lines; a whitespace-only value is ignored
// rather than omitting any output that contains a space.
func TestScrubSecrets_StagedValuesAreTrimmed(t *testing.T) {
	t.Setenv("SCION_HARNESS_SECRETS_DIR", "")
	bundle := filepath.Join(t.TempDir(), ".scion", "harness")
	writeTestFile(t, filepath.Join(bundle, "secrets", "A"), "  placeholder-value-1 \n")
	writeTestFile(t, filepath.Join(bundle, "secrets", "B"), "   \n")
	m := &containerProvisionManifest{HarnessBundleDir: bundle}

	if got := scrubSecrets("saw placeholder-value-1.", m); got != "saw [REDACTED]." {
		t.Errorf("got %q", got)
	}
}

// A staged file whose lines are separated by lone CRs is masked line by
// line too.
func TestScrubSecrets_CROnlyStagedFileMasksEachLine(t *testing.T) {
	t.Setenv("SCION_HARNESS_SECRETS_DIR", "")
	bundle := filepath.Join(t.TempDir(), ".scion", "harness")
	writeTestFile(t, filepath.Join(bundle, "secrets", "A"), "placeholder-line-one-value\rplaceholder-line-two-value\r")
	m := &containerProvisionManifest{HarnessBundleDir: bundle}

	if got := scrubSecrets("saw placeholder-line-one-value and placeholder-line-two-value", m); got != "saw [REDACTED] and [REDACTED]" {
		t.Errorf("got %q", got)
	}
}
