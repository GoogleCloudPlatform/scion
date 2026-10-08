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

const statusTestSecret = "sk-ant-supersecret-0123456789abcdef"

// provisionFixture stages a bundle (with outputs/ and a staged secret, as
// the host does) whose provisioner runs script.
func provisionFixture(t *testing.T, script string) (home, manifestPath, statusPath string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SCION_HARNESS_OUTPUTS_DIR", "")
	t.Setenv("SCION_HARNESS_SECRETS_DIR", "")
	bundle := filepath.Join(home, ".scion", "harness")
	if err := os.MkdirAll(filepath.Join(bundle, "outputs"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(bundle, "secrets", "ANTHROPIC_API_KEY"), statusTestSecret)
	scriptPath := filepath.Join(bundle, "provision.sh")
	writeTestFile(t, scriptPath, script)
	if err := os.Chmod(scriptPath, 0o755); err != nil {
		t.Fatal(err)
	}
	manifestPath = writeManifest(t, bundle, baseManifest(t, home, scriptPath))
	return home, manifestPath, filepath.Join(bundle, "outputs", "status.json")
}

func readStatus(t *testing.T, path string) (state, errMsg string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read status: %v", err)
	}
	var st struct {
		State string `json:"state"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatalf("status is not JSON: %v (%s)", err, data)
	}
	return st.State, st.Error
}

// A failed provisioner records only the scrubbed error: a staged secret the
// script echoes to stderr never reaches the status file, the detail read
// back from it, or the pre-start hook error surfaced by the lifecycle
// manager.
func TestRunHarnessProvision_FailureStatusIsScrubbed(t *testing.T) {
	home, manifestPath, statusPath := provisionFixture(t,
		"#!/bin/sh\necho \"claude provision: no valid auth method found (key="+statusTestSecret+")\" >&2\nexit 1\n")

	if err := runHarnessProvision(context.Background(), manifestPath); err == nil {
		t.Fatal("expected the provisioner to fail")
	}
	state, msg := readStatus(t, statusPath)
	if state != hooks.ProvisionStatusFailed {
		t.Fatalf("state = %q, want failed", state)
	}
	raw, _ := os.ReadFile(statusPath)
	if strings.Contains(string(raw), statusTestSecret) {
		t.Fatalf("status file leaks the staged secret: %s", raw)
	}
	if !strings.Contains(msg, "no valid auth method found") || !strings.Contains(msg, "[REDACTED]") {
		t.Errorf("status error = %q, want the scrubbed reason", msg)
	}

	detail := hooks.HarnessProvisionFailureDetail(home)
	if detail == "" || strings.Contains(detail, statusTestSecret) {
		t.Fatalf("failure detail = %q", detail)
	}

	// The pre-start failure message surfaced by sciontool init.
	hooksDir := filepath.Join(home, ".scion", "hooks")
	writeTestFile(t, filepath.Join(hooksDir, "pre-start.d", "20-harness-provision"), "#!/bin/sh\nexit 1\n")
	if err := os.Chmod(filepath.Join(hooksDir, "pre-start.d", "20-harness-provision"), 0o755); err != nil {
		t.Fatal(err)
	}
	m := &hooks.LifecycleManager{HooksDirs: []string{hooksDir}, Handlers: map[string][]hooks.Handler{}, AgentHome: home}
	err := m.RunPreStart()
	if err == nil {
		t.Fatal("expected the pre-start hook to fail")
	}
	if strings.Contains(err.Error(), statusTestSecret) {
		t.Fatalf("pre-start error leaks the staged secret: %v", err)
	}
	if !strings.Contains(err.Error(), "exit status 1") || !strings.Contains(err.Error(), "no valid auth method found") {
		t.Errorf("pre-start error = %v, want the exit status and the provisioner reason", err)
	}
}

// A long, multi-line stderr is surfaced as one bounded line.
func TestRunHarnessProvision_FailureStatusIsBounded(t *testing.T) {
	home, manifestPath, _ := provisionFixture(t,
		"#!/bin/sh\ni=0\nwhile [ $i -lt 400 ]; do echo \"line $i of noisy provisioner output\" >&2; i=$((i+1)); done\nexit 1\n")
	if err := runHarnessProvision(context.Background(), manifestPath); err == nil {
		t.Fatal("expected the provisioner to fail")
	}
	detail := hooks.HarnessProvisionFailureDetail(home)
	if detail == "" {
		t.Fatal("no failure detail recorded")
	}
	if len(detail) > 1100 {
		t.Errorf("detail is %d bytes, want it bounded", len(detail))
	}
	if strings.ContainsAny(detail, "\n\r\t") {
		t.Errorf("detail contains control characters: %q", detail)
	}
}

// A successful run replaces a failure recorded by an earlier run, so a
// stale error is never surfaced.
func TestRunHarnessProvision_SuccessReplacesStaleFailure(t *testing.T) {
	home, manifestPath, statusPath := provisionFixture(t, "#!/bin/sh\nexit 0\n")
	if err := hooks.WriteHarnessProvisionStatus(statusPath, hooks.ProvisionStatusFailed, "stale failure"); err != nil {
		t.Fatal(err)
	}
	if err := runHarnessProvision(context.Background(), manifestPath); err != nil {
		t.Fatalf("runHarnessProvision: %v", err)
	}
	if state, _ := readStatus(t, statusPath); state != hooks.ProvisionStatusOK {
		t.Errorf("state = %q, want ok", state)
	}
	if d := hooks.HarnessProvisionFailureDetail(home); d != "" {
		t.Errorf("stale failure surfaced: %q", d)
	}
}

// A failure before the script runs (here a missing manifest) is recorded
// too, replacing a stale one.
func TestRunHarnessProvision_EarlyFailureRecorded(t *testing.T) {
	home, _, statusPath := provisionFixture(t, "#!/bin/sh\nexit 0\n")
	if err := hooks.WriteHarnessProvisionStatus(statusPath, hooks.ProvisionStatusFailed, "stale failure"); err != nil {
		t.Fatal(err)
	}
	if err := runHarnessProvision(context.Background(), filepath.Join(home, "missing.json")); err == nil {
		t.Fatal("expected a missing manifest to fail")
	}
	d := hooks.HarnessProvisionFailureDetail(home)
	if !strings.Contains(d, "read manifest") || strings.Contains(d, "stale failure") {
		t.Errorf("failure detail = %q, want the manifest error", d)
	}
}
