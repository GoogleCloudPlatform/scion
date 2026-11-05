/*
Copyright 2026 The Scion Authors.
*/

package commands

import (
	"context"
	"encoding/json"
	"errors"
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
	if len(detail) > 1024 {
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

// The provisioner output scrub applies the line and short-value rules:
// neither a single line of a multi-line staged file nor a short staged value
// reaches the status file. TestProvisionStatusRecorder_FinishScrubs covers
// the second pass over the recorded error.
func TestRunHarnessProvision_FailureStatusMasksLinesAndShortValues(t *testing.T) {
	const line = "placeholder-line-two-value"
	const short = "plv-1x"
	for _, tc := range []struct {
		name, echo, want string
	}{
		{"line", line, "[REDACTED]"},
		{"short", short, provisionerOutputOmitted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, manifestPath, statusPath := provisionFixture(t,
				"#!/bin/sh\necho \"provision: rejected "+tc.echo+"\" >&2\nexit 1\n")
			secrets := filepath.Join(home, ".scion", "harness", "secrets")
			writeTestFile(t, filepath.Join(secrets, "FILE_SECRET"), "placeholder-line-one-value\n"+line+"\n")
			writeTestFile(t, filepath.Join(secrets, "SHORT_SECRET"), short)

			if err := runHarnessProvision(context.Background(), manifestPath); err == nil {
				t.Fatal("expected the provisioner to fail")
			}
			raw, _ := os.ReadFile(statusPath)
			if strings.Contains(string(raw), tc.echo) {
				t.Fatalf("status file contains the staged value: %s", raw)
			}
			_, msg := readStatus(t, statusPath)
			if !strings.Contains(msg, "harness provisioner failed") || !strings.Contains(msg, tc.want) {
				t.Errorf("status error = %q, want the failure and %q", msg, tc.want)
			}
		})
	}
}

// finish scrubs the recorded error itself, with the same rules, whatever
// produced it.
func TestProvisionStatusRecorder_FinishScrubs(t *testing.T) {
	t.Setenv("SCION_HARNESS_SECRETS_DIR", "")
	dir := t.TempDir()
	bundle := filepath.Join(dir, ".scion", "harness")
	writeTestFile(t, filepath.Join(bundle, "secrets", "FILE_SECRET"), "placeholder-line-one-value\nplaceholder-line-two-value\n")
	writeTestFile(t, filepath.Join(bundle, "secrets", "SHORT_SECRET"), "plv-1x")
	statusPath := filepath.Join(dir, "status.json")
	rec := &provisionStatusRecorder{path: statusPath, manifest: &containerProvisionManifest{HarnessBundleDir: bundle}}

	rec.finish(errors.New("setup failed: placeholder-line-two-value"))
	if _, msg := readStatus(t, statusPath); msg != "setup failed: [REDACTED]" {
		t.Errorf("line: status error = %q", msg)
	}
	rec.finish(errors.New("setup failed: plv-1x"))
	if _, msg := readStatus(t, statusPath); msg != provisionerOutputOmitted {
		t.Errorf("short: status error = %q", msg)
	}
}

// memOutputsFixture stages a bundle whose outputs are moved to the in-memory
// outputs directory, as on a Kubernetes NFS-home pod, with a non-root writer
// (the test process's own uid). The outputs directory does not exist yet.
// The umask is fixed for the test (setProvisionUmask), so what the writer
// creates does not depend on the caller's umask.
func memOutputsFixture(t *testing.T, script string) (home, manifestPath, outDir string) {
	t.Helper()
	uid := os.Geteuid()
	if uid == 0 {
		t.Skip("needs a non-root test process: only a non-root writer creates the in-memory outputs directory")
	}
	t.Cleanup(setProvisionUmask())
	home = t.TempDir()
	t.Setenv("HOME", home)
	memRoot := t.TempDir()
	t.Cleanup(hooks.SetHarnessDirsRootForTest(memRoot))
	t.Cleanup(hooks.SetProvisionStatusGeteuidForTest(func() int { return uid }))
	outDir = filepath.Join(memRoot, "outputs")
	t.Setenv("SCION_HARNESS_OUTPUTS_DIR", outDir)
	t.Setenv("SCION_HARNESS_SECRETS_DIR", "")
	bundle := filepath.Join(home, ".scion", "harness")
	scriptPath := filepath.Join(bundle, "provision.sh")
	writeTestFile(t, scriptPath, script)
	if err := os.Chmod(scriptPath, 0o755); err != nil {
		t.Fatal(err)
	}
	return home, writeManifest(t, bundle, baseManifest(t, home, scriptPath)), outDir
}

// On a first start with the in-memory outputs directory missing, the
// "running" status is recorded before the provisioner runs (the script
// creates no directory), and so is the final one.
func TestRunHarnessProvision_FirstStartRecordsRunningInMemOutputs(t *testing.T) {
	_, manifestPath, outDir := memOutputsFixture(t,
		"#!/bin/sh\ngrep -q '\"state\":\"running\"' \"$SCION_HARNESS_OUTPUTS_DIR/status.json\" || exit 3\nexit 0\n")
	if err := runHarnessProvision(context.Background(), manifestPath); err != nil {
		t.Fatalf("runHarnessProvision: %v", err)
	}
	if state, _ := readStatus(t, filepath.Join(outDir, "status.json")); state != hooks.ProvisionStatusOK {
		t.Errorf("state = %q, want %q", state, hooks.ProvisionStatusOK)
	}
}

// A failure before the provisioner runs on a first start is recorded in the
// in-memory outputs directory, so the pre-start error keeps its reason.
func TestRunHarnessProvision_FirstStartEarlyFailureRecordedInMemOutputs(t *testing.T) {
	home, _, _ := memOutputsFixture(t, "#!/bin/sh\nexit 0\n")
	if err := runHarnessProvision(context.Background(), filepath.Join(home, "missing.json")); err == nil {
		t.Fatal("expected a missing manifest to fail")
	}
	if d := hooks.HarnessProvisionFailureDetail(home); !strings.Contains(d, "read manifest") {
		t.Errorf("failure detail = %q, want the manifest error", d)
	}
}

// An existing in-memory outputs directory that is not private to the writer
// is left alone: the status is not recorded, a warning says so, and the
// provision result is unchanged.
func TestRunHarnessProvision_UnsafeMemOutputsDirWarns(t *testing.T) {
	_, manifestPath, outDir := memOutputsFixture(t, "#!/bin/sh\nexit 0\n")
	if err := os.Mkdir(outDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "agent.log")
	setTestLogPath(t, logPath)
	if err := runHarnessProvision(context.Background(), manifestPath); err != nil {
		t.Fatalf("runHarnessProvision: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(outDir, "status.json")); !os.IsNotExist(err) {
		t.Errorf("status.json written into an unsafe directory (err %v)", err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "[WARN] provision status not recorded: ") {
		t.Errorf("agent.log has no status warning:\n%s", data)
	}
}
