/*
Copyright 2026 The Scion Authors.
*/

package commands

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
)

// captureStdout redirects os.Stdout for the duration of fn and returns
// everything written to it — the same technique captureStderr
// (substrate_rootfs_test.go) uses for the log package's stderr output,
// applied here since checkWorkspaceGit writes with fmt.Println/Printf
// directly to os.Stdout rather than through pkg/sciontool/log.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	fn()
	os.Stdout = orig
	_ = w.Close()
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	_ = r.Close()
	return buf.String()
}

// TestCheckWorkspaceGit_NoOpWhenEnvUnset proves the function does nothing
// at all — no output, no failure increment — when SCION_WORKSPACE_GIT
// isn't set, which is the state every non-substrate `doctor` invocation
// runs under.
func TestCheckWorkspaceGit_NoOpWhenEnvUnset(t *testing.T) {
	t.Setenv("SCION_WORKSPACE_GIT", "")
	failures := 0
	out := captureStdout(t, func() { checkWorkspaceGit(&failures) })
	if out != "" {
		t.Errorf("checkWorkspaceGit() output = %q, want empty when SCION_WORKSPACE_GIT is unset", out)
	}
	if failures != 0 {
		t.Errorf("failures = %d, want 0", failures)
	}
}

// TestCheckWorkspaceGit_NonRootRunsTheRealCheck is the required positive
// pairing for the root-refusal branch below: as a non-root process (every
// test in this suite runs as one), the function must actually reach and
// run the real git check, not silently skip it the way it does for root.
func TestCheckWorkspaceGit_NonRootRunsTheRealCheck(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires a non-root euid to exercise the non-root path")
	}
	t.Setenv("SCION_WORKSPACE_GIT", "1")
	failures := 0
	out := captureStdout(t, func() { checkWorkspaceGit(&failures) })
	if !strings.Contains(out, "Workspace/Git State") {
		t.Errorf("checkWorkspaceGit() output = %q, want it to reach the real check header", out)
	}
	if strings.Contains(out, "Running as root") {
		t.Errorf("checkWorkspaceGit() output = %q, want no root-refusal line for a non-root process", out)
	}
}

// TestCheckWorkspaceGit_RootRefusesRatherThanTrustGitconfig is the genuine
// root-only path for the root refusal: run as actual root,
// the function must refuse outright (no git subprocess run at all) rather
// than trust a workload-controlled gitconfig. Skips otherwise — a fake
// euid can't be injected without a production seam, and this codebase's
// convention for a check that is genuinely root-only (see
// TestVerifySelfBinaryRootOwned_AcceptsRealRootOwnedBinary) is to skip
// rather than fake it.
func TestCheckWorkspaceGit_RootRefusesRatherThanTrustGitconfig(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to exercise the refusal path for real")
	}
	t.Setenv("SCION_WORKSPACE_GIT", "1")
	failures := 0
	out := captureStdout(t, func() { checkWorkspaceGit(&failures) })
	if !strings.Contains(out, "Running as root") {
		t.Errorf("checkWorkspaceGit() output = %q, want the root-refusal line", out)
	}
	if failures != 0 {
		t.Errorf("failures = %d, want 0 (refusal is not itself a failure)", failures)
	}
}
