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

func provisionStatusFixture(t *testing.T, hookName, state, msg string) (*LifecycleManager, string) {
	t.Helper()
	t.Setenv("SCION_HARNESS_OUTPUTS_DIR", "")
	home := t.TempDir()
	outputs := filepath.Join(home, ".scion", "harness", "outputs")
	if err := os.MkdirAll(outputs, 0o755); err != nil {
		t.Fatal(err)
	}
	if state != "" {
		if err := WriteHarnessProvisionStatus(filepath.Join(outputs, "status.json"), state, msg); err != nil {
			t.Fatal(err)
		}
	}
	hooksDir := filepath.Join(home, ".scion", "hooks")
	mustWriteScript(t, filepath.Join(hooksDir, "pre-start.d", hookName), "#!/bin/sh\nexit 1\n")
	m := NewLifecycleManager()
	m.HooksDirs = []string{hooksDir}
	m.AgentHome = home
	return m, home
}

func TestRunPreStart_HarnessProvisionFailureIncludesRecordedError(t *testing.T) {
	m, _ := provisionStatusFixture(t, harnessProvisionHookFilename, ProvisionStatusFailed,
		"harness provisioner failed: exit status 1: claude provision: no valid auth method found")
	err := m.RunPreStart()
	if err == nil {
		t.Fatal("expected failure")
	}
	if !strings.Contains(err.Error(), "execution failed: exit status 1: harness provisioner failed") ||
		!strings.Contains(err.Error(), "no valid auth method found") {
		t.Errorf("err = %v", err)
	}
}

func TestRunPreStart_NoDetailUnlessProvisionHookFailedWithRecordedError(t *testing.T) {
	for _, tc := range []struct{ name, hook, state string }{
		{"other hook", "30-project-custom", ProvisionStatusFailed},
		{"status ok", harnessProvisionHookFilename, ProvisionStatusOK},
		{"status running", harnessProvisionHookFilename, ProvisionStatusRunning},
		{"no status", harnessProvisionHookFilename, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := provisionStatusFixture(t, tc.hook, tc.state, "recorded reason")
			err := m.RunPreStart()
			if err == nil {
				t.Fatal("expected failure")
			}
			if strings.Contains(err.Error(), "recorded reason") {
				t.Errorf("unexpected detail in %v", err)
			}
		})
	}
}

// The detail is read without following a symlinked status file.
func TestHarnessProvisionFailureDetail_RefusesSymlink(t *testing.T) {
	t.Setenv("SCION_HARNESS_OUTPUTS_DIR", "")
	home := t.TempDir()
	outputs := filepath.Join(home, ".scion", "harness", "outputs")
	if err := os.MkdirAll(outputs, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "elsewhere.json")
	if err := os.WriteFile(target, []byte(`{"schema_version":1,"state":"failed","error":"planted"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(outputs, "status.json")); err != nil {
		t.Fatal(err)
	}
	if d := HarnessProvisionFailureDetail(home); d != "" {
		t.Errorf("followed a symlinked status file: %q", d)
	}
	// Writing replaces the symlink itself, never the target.
	if err := WriteHarnessProvisionStatus(filepath.Join(outputs, "status.json"), ProvisionStatusOK, ""); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(target); !strings.Contains(string(data), "planted") {
		t.Errorf("write followed the symlink: target now %s", data)
	}
}

func TestSanitizeProvisionError(t *testing.T) {
	if got := sanitizeProvisionError("a\nb\t\x1b[31mc\r\n"); got != "a b [31mc" {
		t.Errorf("got %q", got)
	}
	long := strings.Repeat("é", 1000)
	got := sanitizeProvisionError(long)
	if len(got) > provisionStatusMaxError+len("…") || !strings.HasSuffix(got, "…") {
		t.Errorf("len %d", len(got))
	}
	if strings.ContainsRune(strings.TrimSuffix(got, "…"), '�') {
		t.Error("cut inside a rune")
	}
}
