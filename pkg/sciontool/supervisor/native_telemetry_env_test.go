package supervisor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
)

// clearNativeTelemetryEnv unsets every inherited variable that the native
// telemetry policy inspects (see hooks.ValidateNativeTelemetryEnv), plus
// SCION_* runtime contract variables, for the duration of the test. Agent
// containers export several of these (OTEL_EXPORTER_OTLP_*, CODEX_HOME,
// CLAUDE_CODE_ENABLE_TELEMETRY, ...), so without this the tests depend on
// the ambient environment. t.Setenv restores the original values.
func clearNativeTelemetryEnv(t *testing.T) {
	t.Helper()
	for _, entry := range os.Environ() {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || key == "" || !nativeTelemetryTestKey(key) {
			continue
		}
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatalf("unset %s: %v", key, err)
		}
	}
}

func nativeTelemetryTestKey(key string) bool {
	switch key {
	case "CLAUDE_CODE_ENABLE_TELEMETRY", "GROK_EXTERNAL_OTEL", "CODEX_HOME":
		return true
	}
	for _, prefix := range []string{"OTEL_", "GEMINI_TELEMETRY_", "COPILOT_OTEL_", "GROK_TELEMETRY_", "SCION_"} {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

func TestNativeTelemetryPolicyRejectsBeforeChildLaunch(t *testing.T) {
	for _, tc := range []struct {
		name           string
		policy         string
		inheritedKey   string
		inheritedValue string
		overrideKey    string
		overrideValue  string
	}{
		{"enabled endpoint", "enabled", "OTEL_EXPORTER_OTLP_ENDPOINT", "https://external.invalid", "", ""},
		{"disabled alias", "disabled", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "https://external.invalid", "", ""},
		{"gemini target", "enabled", "GEMINI_TELEMETRY_TARGET", "gcp", "", ""},
		{"codex home", "enabled", "CODEX_HOME", "/tmp/other", "", ""},
		{"secret override", "enabled", "", "", "OTEL_EXPORTER_OTLP_ENDPOINT", "https://external.invalid"},
		{"secret alias", "disabled", "", "", "GEMINI_TELEMETRY_OUTFILE", "/tmp/out"},
		{"marker", "enabled", hooks.NativeTelemetryPolicyKey, "disabled", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearNativeTelemetryEnv(t)
			if tc.inheritedKey != "" {
				t.Setenv(tc.inheritedKey, tc.inheritedValue)
			}
			out := filepath.Join(t.TempDir(), "started")
			cfg := DefaultConfig()
			cfg.NativeTelemetryPolicy = tc.policy
			cfg.EnvOverlay = map[string]string{
				"OTEL_EXPORTER_OTLP_ENDPOINT":  "http://127.0.0.1:24317",
				"CLAUDE_CODE_ENABLE_TELEMETRY": "1",
				"CODEX_HOME":                   "/home/scion/.codex",
			}
			if tc.overrideKey != "" {
				cfg.SecretOverrides = map[string]string{tc.overrideKey: tc.overrideValue}
			}
			code, err := New(cfg).Run(context.Background(), []string{"sh", "-c", "touch " + out})
			if code != 1 || err == nil || !strings.Contains(err.Error(), "native telemetry policy conflict") {
				t.Fatalf("code=%d err=%v", code, err)
			}
			if strings.Contains(err.Error(), "external.invalid") || strings.Contains(err.Error(), "/tmp/other") {
				t.Fatalf("value leaked: %v", err)
			}
			if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
				t.Fatalf("child launched: %v", statErr)
			}
		})
	}
}

func TestNativeTelemetryPolicyEffectiveChildEnv(t *testing.T) {
	for _, tc := range []struct{ name, policy, enabled, endpoint string }{
		{"enabled default port", "enabled", "1", "http://127.0.0.1:4317"},
		{"enabled custom port", "enabled", "1", "http://127.0.0.1:24317"},
		{"disabled", "disabled", "0", "http://127.0.0.1:4317"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearNativeTelemetryEnv(t)
			t.Setenv("UNRELATED_CLI", "cli")
			out := filepath.Join(t.TempDir(), "env")
			cfg := DefaultConfig()
			cfg.NativeTelemetryPolicy = tc.policy
			cfg.EnvOverlay = map[string]string{
				"CLAUDE_CODE_ENABLE_TELEMETRY": tc.enabled,
				"OTEL_EXPORTER_OTLP_ENDPOINT":  tc.endpoint,
				"CODEX_HOME":                   "/home/scion/.codex",
				"UNRELATED_CLI":                "generated",
				"UNRELATED_SECRET":             "placeholder",
			}
			cfg.SecretOverrides = map[string]string{"UNRELATED_SECRET": "fetched"}
			code, err := New(cfg).Run(context.Background(), []string{"sh", "-c", `printf '%s|%s|%s|%s|%s|%s' "$CLAUDE_CODE_ENABLE_TELEMETRY" "$OTEL_EXPORTER_OTLP_ENDPOINT" "$CODEX_HOME" "$UNRELATED_CLI" "$UNRELATED_SECRET" "$SCION_NATIVE_TELEMETRY_POLICY" > ` + out})
			if code != 0 || err != nil {
				t.Fatalf("code=%d err=%v", code, err)
			}
			got, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}
			want := tc.enabled + "|" + tc.endpoint + "|/home/scion/.codex|cli|fetched|"
			if string(got) != want {
				t.Fatalf("child env=%q, want %q", got, want)
			}
		})
	}
}

func TestNativeTelemetryNoPolicyKeepsLegacyPrecedence(t *testing.T) {
	clearNativeTelemetryEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://cli.invalid")
	out := filepath.Join(t.TempDir(), "endpoint")
	cfg := DefaultConfig()
	cfg.EnvOverlay = map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4317"}
	code, err := New(cfg).Run(context.Background(), []string{"sh", "-c", `printf '%s' "$OTEL_EXPORTER_OTLP_ENDPOINT" > ` + out})
	if code != 0 || err != nil {
		t.Fatalf("code=%d err=%v", code, err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "https://cli.invalid" {
		t.Fatalf("legacy env=%q", got)
	}
}
