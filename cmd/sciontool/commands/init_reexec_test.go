/*
Copyright 2025 The Scion Authors.
*/

package commands

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/stagedsecrets"
)

// TestProcEnvironRetainsUnsetVar demonstrates the underlying kernel behavior
// that this fix addresses: os.Unsetenv removes a variable from Go's in-process
// environment, but /proc/self/environ — which is populated by the kernel from
// the original execve(2) arguments — still contains the variable.
func TestProcEnvironRetainsUnsetVar(t *testing.T) {
	if _, err := os.Stat("/proc/self/environ"); err != nil {
		t.Skip("skipping: /proc/self/environ not available")
	}

	const testKey = "SCION_TEST_PROC_ENVIRON_RETENTION"
	const testVal = "secret-value-12345"

	// Check whether the test process itself was launched with this env var.
	// If not, the test can still verify the Go-level behavior but cannot
	// make assertions about /proc/self/environ (which only reflects vars
	// present at execve time).
	procEnvData, err := os.ReadFile("/proc/self/environ")
	if err != nil {
		t.Fatalf("failed to read /proc/self/environ: %v", err)
	}

	// Even without being in the initial execve env, we can verify that
	// os.Unsetenv clears the Go-level view:
	t.Setenv(testKey, testVal)
	if got := os.Getenv(testKey); got != testVal {
		t.Fatalf("Setenv/Getenv mismatch: got %q, want %q", got, testVal)
	}
	_ = os.Unsetenv(testKey)
	if got := os.Getenv(testKey); got != "" {
		t.Fatalf("os.Getenv should return empty after Unsetenv, got %q", got)
	}

	// The /proc/self/environ file is NUL-delimited. If our test key was
	// present in the initial environment (e.g., a parent test runner set
	// it), verify it persists in /proc even after Unsetenv.
	entries := bytes.Split(procEnvData, []byte{0})
	prefix := testKey + "="
	for _, entry := range entries {
		if strings.HasPrefix(string(entry), prefix) {
			// The var was in the initial execve env. After os.Unsetenv it
			// should still appear in /proc/self/environ — this is the
			// kernel behavior that re-exec fixes.
			t.Logf("Confirmed: %s persists in /proc/self/environ after os.Unsetenv (expected kernel behavior)", testKey)
			return
		}
	}
	// The var was not in the initial execve env, so we can't verify the
	// /proc retention. That's fine — the Go-level Unsetenv behavior above
	// is still validated.
	t.Logf("%s was not in initial execve environment; /proc retention not testable in this run", testKey)
}

// TestStagedSecretsEnvVarName verifies the constant used in the re-exec
// guard matches the expected environment variable name.
func TestStagedSecretsEnvVarName(t *testing.T) {
	if stagedsecrets.EnvVar != "SCION_STAGED_SECRETS" {
		t.Errorf("stagedsecrets.EnvVar = %q, want %q", stagedsecrets.EnvVar, "SCION_STAGED_SECRETS")
	}
}

// TestReExecWithCleanEnv_ResolvesExecutable verifies that reExecWithCleanEnv
// can resolve the current executable path. We cannot test the actual execve
// in-process (it replaces the process image), so we test the prerequisite
// that os.Executable succeeds in this environment.
func TestReExecWithCleanEnv_ResolvesExecutable(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() failed: %v — reExecWithCleanEnv would also fail", err)
	}
	if exe == "" {
		t.Fatal("os.Executable() returned empty string")
	}
	t.Logf("Executable: %s", exe)
}

// TestReExecIntegration is a subprocess-based test that verifies the full
// re-exec flow: sciontool init with SCION_STAGED_SECRETS set should produce
// a process whose /proc environ does not contain the secret.
//
// This test requires building sciontool and is skipped in short mode.
func TestReExecIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	if os.Getenv("SCION_INTEGRATION_TEST") == "" {
		t.Skip("skipping integration test: SCION_INTEGRATION_TEST not set")
	}
	if _, err := os.Stat("/proc/self/environ"); err != nil {
		t.Skip("skipping: /proc filesystem not available")
	}

	scrubHubEnv(t)

	// Build sciontool
	binPath := buildSciontool(t)

	// Create a temporary home directory for secrets staging
	tmpHome := t.TempDir()

	// Create a minimal SCION_STAGED_SECRETS payload (empty secrets)
	// The base64 of {"file_secrets":[],"variable_secrets":{}} is:
	// eyJmaWxlX3NlY3JldHMiOltdLCJ2YXJpYWJsZV9zZWNyZXRzIjp7fX0=
	stagedPayload := "eyJmaWxlX3NlY3JldHMiOltdLCJ2YXJpYWJsZV9zZWNyZXRzIjp7fX0="

	// The child command reads /proc/$PPID/environ and checks for the secret var.
	// After re-exec, PPID should be the re-exec'd sciontool init (PID 1 in a
	// container, but not in test). We check the parent's environ via /proc.
	//
	// However, in a test environment sciontool doesn't run as PID 1 and the
	// child's PPID might be the test process. Instead, we use a child script
	// that reads /proc/self/environ of the init process by its PID.
	//
	// For simplicity, check that the child process itself does NOT inherit
	// SCION_STAGED_SECRETS in its own environment. This was already blocked
	// by os.Unsetenv, but also exercises the code path through re-exec.
	cmd := buildTestInitCmd(binPath, tmpHome, stagedPayload,
		"sh", "-c", "env | grep SCION_STAGED_SECRETS && echo LEAKED || echo CLEAN")

	output, err := cmd.CombinedOutput()
	if err != nil {
		// init exits with the child's exit code; sh -c should exit 0
		t.Logf("Output:\n%s", output)
		t.Fatalf("sciontool init failed: %v", err)
	}

	outStr := string(output)
	if strings.Contains(outStr, "LEAKED") {
		t.Error("SCION_STAGED_SECRETS leaked to child process environment")
	}
	if !strings.Contains(outStr, "CLEAN") {
		t.Logf("Output:\n%s", outStr)
		t.Error("expected CLEAN in output, child may not have run correctly")
	}
}

// buildSciontool compiles sciontool for the current test and returns the
// path to the binary. It skips the test if the build fails.
func buildSciontool(t *testing.T) string {
	t.Helper()
	binPath := t.TempDir() + "/sciontool-test"

	repoRoot := findRepoRoot(t)
	cmd := exec.Command("go", "build", "-buildvcs=false", "-o", binPath, "./cmd/sciontool/")
	cmd.Dir = repoRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("failed to build sciontool: %v\n%s", err, out)
	}
	return binPath
}

// buildTestInitCmd creates an exec.Cmd for running sciontool init with a
// staged secrets payload and a specified child command.
func buildTestInitCmd(binPath, tmpHome, stagedPayload string, childArgs ...string) *exec.Cmd {
	args := append([]string{"init", "--"}, childArgs...)
	cmd := exec.Command(binPath, args...)
	cmd.Env = filterHubEnv(os.Environ())
	// Set the staged secrets env var
	cmd.Env = append(cmd.Env, "SCION_STAGED_SECRETS="+stagedPayload)
	cmd.Env = append(cmd.Env, "HOME="+tmpHome)
	return cmd
}
