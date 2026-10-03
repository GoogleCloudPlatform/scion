/*
Copyright 2026 The Scion Authors.
*/

package hooks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadEnvOverlay_MissingFileIsNotError(t *testing.T) {
	got, err := LoadEnvOverlay(filepath.Join(t.TempDir(), "nope.json"), nil)
	if err != nil {
		t.Fatalf("expected nil error for missing file, got %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil map for missing file, got %v", got)
	}
}

func TestLoadEnvOverlay_EmptyPathIsNoop(t *testing.T) {
	got, err := LoadEnvOverlay("", nil)
	if err != nil || got != nil {
		t.Fatalf("expected nil/nil for empty path, got %v / %v", got, err)
	}
}

func TestLoadEnvOverlay_StringValues(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "env.json")
	if err := os.WriteFile(path, []byte(`{"FOO":"bar","BAZ":"qux"}`), 0644); err != nil {
		t.Fatal(err)
	}
	got, err := LoadEnvOverlay(path, nil)
	if err != nil {
		t.Fatalf("LoadEnvOverlay: %v", err)
	}
	if got["FOO"] != "bar" || got["BAZ"] != "qux" {
		t.Fatalf("unexpected overlay: %v", got)
	}
}

func TestLoadEnvOverlay_FromFileResolves(t *testing.T) {
	dir := t.TempDir()
	secrets := filepath.Join(dir, "secrets")
	if err := os.MkdirAll(secrets, 0700); err != nil {
		t.Fatal(err)
	}
	secretFile := filepath.Join(secrets, "ANTHROPIC_API_KEY")
	if err := os.WriteFile(secretFile, []byte("sk-test-12345\n"), 0600); err != nil {
		t.Fatal(err)
	}
	overlay := filepath.Join(dir, "env.json")
	body := `{"ANTHROPIC_API_KEY":{"from_file":"` + secretFile + `"}}`
	if err := os.WriteFile(overlay, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}

	got, err := LoadEnvOverlay(overlay, []string{dir})
	if err != nil {
		t.Fatalf("LoadEnvOverlay: %v", err)
	}
	// Trailing newline should be stripped.
	if got["ANTHROPIC_API_KEY"] != "sk-test-12345" {
		t.Fatalf("expected trimmed value, got %q", got["ANTHROPIC_API_KEY"])
	}
}

// TestLoadEnvOverlay_FromFileProjectedSecretStyleSymlinkResolves reproduces
// a real Kubernetes projected-secret volume layout mounted as an
// allowedRoot: a timestamped directory holding the actual key, a "..data"
// symlink to it, and the requested key itself as a symlink through
// "..data" (e.g. "token" -> "..data/token"). The from_file resolver must
// allow that layout: refusing every symlink unconditionally would make
// from_file unable to read a key out of a volume mounted this way — this is
// the acceptance-gate test for that behaviour.
func TestLoadEnvOverlay_FromFileProjectedSecretStyleSymlinkResolves(t *testing.T) {
	dir := t.TempDir()
	timestamped := "..2026_09_28_12_00_00.123456789"
	if err := os.MkdirAll(filepath.Join(dir, timestamped), 0700); err != nil {
		t.Fatal(err)
	}
	tokenPath := filepath.Join(dir, timestamped, "token")
	if err := os.WriteFile(tokenPath, []byte("sa-token-value\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(timestamped, filepath.Join(dir, "..data")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..data", "token"), filepath.Join(dir, "token")); err != nil {
		t.Fatal(err)
	}

	overlay := filepath.Join(dir, "env.json")
	body := `{"TOKEN":{"from_file":"` + filepath.Join(dir, "token") + `"}}`
	if err := os.WriteFile(overlay, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}

	got, err := LoadEnvOverlay(overlay, []string{dir})
	if err != nil {
		t.Fatalf("LoadEnvOverlay: %v", err)
	}
	if got["TOKEN"] != "sa-token-value" {
		t.Fatalf("expected the projected-secret token to resolve, got %q", got["TOKEN"])
	}
}

// TestLoadEnvOverlay_FromFileSymlinkInsideRootEscapesRejected fails if
// containment is checked by comparing path strings instead of walking an
// fd chain anchored at the allowed root: the symlink's own name sits
// inside the allowed root, but its target does not, so a string-prefix (or
// filepath.Rel) check alone would accept it.
func TestLoadEnvOverlay_FromFileSymlinkInsideRootEscapesRejected(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	secretFile := filepath.Join(outside, "leaked")
	if err := os.WriteFile(secretFile, []byte("nope"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(secretFile, link); err != nil {
		t.Fatal(err)
	}

	overlay := filepath.Join(dir, "env.json")
	body := `{"X":{"from_file":"` + link + `"}}`
	if err := os.WriteFile(overlay, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadEnvOverlay(overlay, []string{dir})
	if err == nil {
		t.Fatal("expected a symlink whose name is inside the allowed root but whose target is not to be rejected")
	}
}

// TestLoadEnvOverlay_FromFileParentEscapeRejected fails if the containment
// check is dropped or bypassed: a from_file value that walks back out of
// the allowed root via ".." must be refused even though evaluating it
// component-by-component would eventually land back inside a different
// permitted root's tree.
func TestLoadEnvOverlay_FromFileParentEscapeRejected(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	secretFile := filepath.Join(outside, "leaked")
	if err := os.WriteFile(secretFile, []byte("nope"), 0600); err != nil {
		t.Fatal(err)
	}

	escaping := filepath.Join(dir, "..", filepath.Base(outside), "leaked")
	overlay := filepath.Join(dir, "env.json")
	body := `{"X":{"from_file":"` + escaping + `"}}`
	if err := os.WriteFile(overlay, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadEnvOverlay(overlay, []string{dir})
	if err == nil || !strings.Contains(err.Error(), "escapes allowed roots") {
		t.Fatalf("expected escape rejection, got %v", err)
	}
}

// TestLoadEnvOverlay_OverlayFileSymlinkRejected fails if the overlay file's
// own read stops refusing symlinks: swapping the overlay path itself for a
// symlink must not make LoadEnvOverlay read through it.
func TestLoadEnvOverlay_OverlayFileSymlinkRejected(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.json")
	if err := os.WriteFile(real, []byte(`{"FOO":"bar"}`), 0644); err != nil {
		t.Fatal(err)
	}
	overlay := filepath.Join(dir, "env.json")
	if err := os.Symlink(real, overlay); err != nil {
		t.Fatal(err)
	}

	_, err := LoadEnvOverlay(overlay, nil)
	if err == nil {
		t.Fatal("expected an error reading an overlay path that is a symlink, got nil")
	}
}

func TestLoadEnvOverlay_FromFileEscapingPathRejected(t *testing.T) {
	dir := t.TempDir()
	other := t.TempDir()
	// secret lives outside the allowed root
	secretFile := filepath.Join(other, "leaked")
	if err := os.WriteFile(secretFile, []byte("nope"), 0600); err != nil {
		t.Fatal(err)
	}
	overlay := filepath.Join(dir, "env.json")
	body := `{"X":{"from_file":"` + secretFile + `"}}`
	if err := os.WriteFile(overlay, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadEnvOverlay(overlay, []string{dir})
	if err == nil || !strings.Contains(err.Error(), "escapes allowed roots") {
		t.Fatalf("expected escape rejection, got %v", err)
	}
}

func TestLoadEnvOverlay_FromFileMissingFails(t *testing.T) {
	dir := t.TempDir()
	overlay := filepath.Join(dir, "env.json")
	body := `{"X":{"from_file":"` + filepath.Join(dir, "nope") + `"}}`
	if err := os.WriteFile(overlay, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadEnvOverlay(overlay, []string{dir})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not-found, got %v", err)
	}
}

func TestLoadEnvOverlay_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	overlay := filepath.Join(dir, "env.json")
	if err := os.WriteFile(overlay, []byte("{not json"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadEnvOverlay(overlay, []string{dir})
	if err == nil || !strings.Contains(err.Error(), "parse env overlay") {
		t.Fatalf("expected parse error, got %v", err)
	}
}

func TestLoadEnvOverlay_RejectsInvalidKey(t *testing.T) {
	dir := t.TempDir()
	overlay := filepath.Join(dir, "env.json")
	if err := os.WriteFile(overlay, []byte(`{"FOO BAR":"x"}`), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadEnvOverlay(overlay, []string{dir})
	if err == nil || !strings.Contains(err.Error(), "invalid key") {
		t.Fatalf("expected invalid-key, got %v", err)
	}
}

func TestLoadEnvOverlay_RejectsOversizedSecret(t *testing.T) {
	dir := t.TempDir()
	secretFile := filepath.Join(dir, "huge")
	big := make([]byte, maxEnvSecretFileBytes+1)
	for i := range big {
		big[i] = 'a'
	}
	if err := os.WriteFile(secretFile, big, 0600); err != nil {
		t.Fatal(err)
	}
	overlay := filepath.Join(dir, "env.json")
	body := `{"X":{"from_file":"` + secretFile + `"}}`
	if err := os.WriteFile(overlay, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadEnvOverlay(overlay, []string{dir})
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("expected size rejection, got %v", err)
	}
}

func TestLoadEnvOverlay_RejectsMalformedObjectValue(t *testing.T) {
	dir := t.TempDir()
	overlay := filepath.Join(dir, "env.json")
	if err := os.WriteFile(overlay, []byte(`{"X":{"unknown":"y"}}`), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadEnvOverlay(overlay, []string{dir})
	if err == nil {
		t.Fatal("expected error for missing from_file")
	}
}

func TestMergeEnvOverlay_RuntimeEnvWins(t *testing.T) {
	env := []string{"FOO=runtime", "PATH=/usr/bin"}
	overlay := map[string]string{"FOO": "overlay-value", "BAR": "added"}
	got := MergeEnvOverlay(env, overlay)

	values := envMap(got)
	if values["FOO"] != "runtime" {
		t.Errorf("expected runtime FOO to win, got %q", values["FOO"])
	}
	if values["BAR"] != "added" {
		t.Errorf("expected BAR added by overlay, got %q", values["BAR"])
	}
	if values["PATH"] != "/usr/bin" {
		t.Errorf("expected PATH preserved, got %q", values["PATH"])
	}
}

func TestMergeEnvOverlay_EmptyOverlayIsPassthrough(t *testing.T) {
	env := []string{"A=1"}
	got := MergeEnvOverlay(env, nil)
	if len(got) != 1 || got[0] != "A=1" {
		t.Fatalf("expected passthrough, got %v", got)
	}
}

func TestValidateNativeTelemetryEnv(t *testing.T) {
	policy := map[string]string{
		"CLAUDE_CODE_ENABLE_TELEMETRY": "1",
		"OTEL_EXPORTER_OTLP_ENDPOINT":  "http://127.0.0.1:24317",
		"GEMINI_TELEMETRY_ENABLED":     "true",
		"CODEX_HOME":                   "/home/scion/.codex",
	}
	for _, tc := range []struct {
		name      string
		env       []string
		overrides map[string]string
		conflict  bool
	}{
		{"matching and unrelated", []string{"OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:24317", "TOKEN=old"}, map[string]string{"TOKEN": "new"}, false},
		{"external endpoint", []string{"OTEL_EXPORTER_OTLP_ENDPOINT=https://external.invalid"}, nil, true},
		{"alternate endpoint", []string{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=https://external.invalid"}, nil, true},
		{"alternate protocol", []string{"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL=http/protobuf"}, nil, true},
		{"SDK disabled", []string{"OTEL_SDK_DISABLED=true"}, nil, true},
		{"gemini alias", []string{"GEMINI_TELEMETRY_OUTFILE=/tmp/out"}, nil, true},
		{"copilot alias", []string{"COPILOT_OTEL_EXPORTER_TYPE=file"}, nil, true},
		{"grok telemetry alias", []string{"GROK_TELEMETRY_ENABLED=false"}, nil, true},
		{"grok external otel alias", []string{"GROK_EXTERNAL_OTEL=false"}, nil, true},
		{"claude disabled", []string{"CLAUDE_CODE_ENABLE_TELEMETRY=0"}, nil, true},
		{"codex redirect", []string{"CODEX_HOME=/tmp/other"}, nil, true},
		{"secret override", nil, map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "https://external.invalid"}, true},
		{"secret alias", nil, map[string]string{"GEMINI_TELEMETRY_TARGET": "gcp"}, true},
		{"secret copilot alias", nil, map[string]string{"COPILOT_OTEL_ENABLED": "false"}, true},
		{"secret grok alias", nil, map[string]string{"GROK_TELEMETRY_ENABLED": "false"}, true},
		{"marker", []string{NativeTelemetryPolicyKey + "=disabled"}, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateNativeTelemetryEnv("enabled", tc.env, policy, tc.overrides)
			if (err != nil) != tc.conflict {
				t.Fatalf("conflict=%v, err=%v", tc.conflict, err)
			}
			if err != nil && (strings.Contains(err.Error(), "external.invalid") || strings.Contains(err.Error(), "/tmp/other")) {
				t.Fatalf("diagnostic leaked value: %v", err)
			}
		})
	}
	if err := ValidateNativeTelemetryEnv("disabled", []string{"OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4317"}, map[string]string{"CLAUDE_CODE_ENABLE_TELEMETRY": "0"}, nil); err == nil {
		t.Fatal("disabled policy accepted inherited exporter endpoint")
	}
	// With telemetry disabled, provision.py never emits COPILOT_OTEL_* or
	// GROK_TELEMETRY_*/GROK_EXTERNAL_OTEL (see harnesses/telemetry_provision_test.py),
	// so an inherited copy of one of these must still be rejected, the same
	// as an inherited OTEL_* var.
	if err := ValidateNativeTelemetryEnv("disabled", []string{"COPILOT_OTEL_ENABLED=true"}, nil, nil); err == nil {
		t.Fatal("disabled policy accepted inherited COPILOT_OTEL_ENABLED")
	}
	if err := ValidateNativeTelemetryEnv("disabled", []string{"GROK_TELEMETRY_ENABLED=true"}, nil, nil); err == nil {
		t.Fatal("disabled policy accepted inherited GROK_TELEMETRY_ENABLED")
	}
	if err := ValidateNativeTelemetryEnv("disabled", []string{"GROK_EXTERNAL_OTEL=true"}, nil, nil); err == nil {
		t.Fatal("disabled policy accepted inherited GROK_EXTERNAL_OTEL")
	}
}

func TestMergeEnvOverlayWithNativeTelemetryPolicy(t *testing.T) {
	generated := map[string]string{
		"OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4317",
		"TOKEN":                       "generated",
	}
	if _, err := MergeEnvOverlayWithNativeTelemetryPolicy("enabled", []string{"OTEL_EXPORTER_OTLP_ENDPOINT=https://external.invalid"}, generated, nil); err == nil {
		t.Fatal("conflicting endpoint merged")
	}
	if _, err := MergeEnvOverlayWithNativeTelemetryPolicy("disabled", []string{"OTEL_TRACES_EXPORTER=otlp"}, map[string]string{"CLAUDE_CODE_ENABLE_TELEMETRY": "0"}, nil); err == nil {
		t.Fatal("disabled policy accepted exporter alias")
	}
	got, err := MergeEnvOverlayWithNativeTelemetryPolicy("enabled", []string{"TOKEN=cli"}, generated, map[string]string{"SECRET": "fetched"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "TOKEN=cli,OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4317" {
		t.Fatalf("merged env=%v", got)
	}
	if _, err := MergeEnvOverlayWithNativeTelemetryPolicy("enabled", nil, generated, map[string]string{NativeTelemetryPolicyKey: "enabled"}); err == nil {
		t.Fatal("matching spoofed marker accepted")
	}
}

func envMap(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, e := range env {
		i := strings.IndexByte(e, '=')
		if i < 0 {
			continue
		}
		m[e[:i]] = e[i+1:]
	}
	return m
}
