/*
Copyright 2025 The Scion Authors.
*/

package commands

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/procreap"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/services"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/supervisor"
	"github.com/GoogleCloudPlatform/scion/pkg/util/fsutil"
)

// hubEnvVars lists the environment variables used by the Hub client.
// Leaking these to a subprocess (e.g., sciontool init) causes the child
// to talk to the real Hub and corrupt agent state. See issue #123.
var hubEnvVars = []string{
	"SCION_HUB_ENDPOINT",
	"SCION_HUB_URL",
	"SCION_AUTH_TOKEN",
	"SCION_AGENT_ID",
	"SCION_AGENT_MODE",
	"SCION_TRANSPORT_TOKEN",
	"SCION_TRANSPORT_TOKEN_FILE",
}

// scrubHubEnv clears all Hub-related environment variables for the
// duration of the test, preventing accidental communication with a
// real Hub when tests run inside an agent container.
func scrubHubEnv(t *testing.T) {
	t.Helper()
	for _, key := range hubEnvVars {
		t.Setenv(key, "")
	}
}

// filterHubEnv returns a copy of the environment with all Hub-related
// variables removed. Use when constructing exec.Cmd.Env to prevent
// credential leakage to child processes.
func filterHubEnv(env []string) []string {
	var filtered []string
	for _, e := range env {
		key, _, _ := strings.Cut(e, "=")
		if !slices.Contains(hubEnvVars, key) {
			filtered = append(filtered, e)
		}
	}
	return filtered
}

// TestInitProjectDataIsolation is a canary test that verifies sciontool source code
// does NOT import the pkg/config package, which contains project path resolution logic.
// This is a compile-time guarantee that in-container code cannot access project data paths.
// If this test fails, it means someone added a pkg/config import to sciontool code,
// which would break the agent isolation model.
func TestInitProjectDataIsolation(t *testing.T) {
	// Use go list to get all transitive dependencies of cmd/sciontool
	cmd := exec.Command("go", "list", "-deps", "./cmd/sciontool/...")
	cmd.Dir = filepath.Join(findRepoRoot(t))
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("go list failed (may not have full module context): %v", err)
	}

	deps := string(out)
	for _, line := range strings.Split(deps, "\n") {
		line = strings.TrimSpace(line)
		if line == "github.com/GoogleCloudPlatform/scion/pkg/config" {
			t.Fatal("sciontool must NOT import pkg/config (project path resolution). " +
				"In-container code should use the Hub API or agent-local files, " +
				"not filesystem-based project data access.")
		}
	}
}

// findRepoRoot walks up from the test file to find the go.mod directory.
func findRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get working directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find go.mod in any parent directory")
		}
		dir = parent
	}
}

func TestExtractChildCommand(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		expected []string
	}{
		{
			name:     "single command",
			args:     []string{"bash"},
			expected: []string{"bash"},
		},
		{
			name:     "command with args",
			args:     []string{"tmux", "new-session", "-A"},
			expected: []string{"tmux", "new-session", "-A"},
		},
		{
			name:     "empty args",
			args:     []string{},
			expected: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := extractChildCommand(tt.args)
			if len(result) != len(tt.expected) {
				t.Errorf("expected %d args, got %d", len(tt.expected), len(result))
				return
			}
			for i, v := range result {
				if v != tt.expected[i] {
					t.Errorf("arg[%d]: expected %q, got %q", i, tt.expected[i], v)
				}
			}
		})
	}
}

func TestInitCommand_Help(t *testing.T) {
	resetRootCmdState(t)
	var buf bytes.Buffer
	rootCmd.SetOut(&buf)
	rootCmd.SetArgs([]string{"init", "--help"})

	err := rootCmd.Execute()
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "init") {
		t.Error("help output should mention 'init'")
	}
	if !strings.Contains(output, "grace-period") {
		t.Error("help output should mention 'grace-period' flag")
	}
}

func TestInitCommand_GracePeriodFlag(t *testing.T) {
	// Verify the flag exists and has the expected default
	flag := initCmd.Flags().Lookup("grace-period")
	if flag == nil {
		t.Fatal("grace-period flag not found")
	}
	if flag.DefValue != "10s" {
		t.Errorf("expected default grace-period 10s, got %s", flag.DefValue)
	}
}

// TestInitCommand_Integration performs an integration test with a real subprocess.
// This is skipped in short mode as it involves actual process execution.
func TestInitCommand_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	if os.Getenv("SCION_INTEGRATION_TEST") == "" {
		t.Skip("skipping integration test: SCION_INTEGRATION_TEST not set")
	}

	// Clear Hub env vars so the subprocess cannot talk to the real Hub
	// and corrupt agent state. See issue #123.
	scrubHubEnv(t)

	// Build sciontool if needed for integration testing
	binPath := filepath.Join(t.TempDir(), "sciontool-test")
	cmd := exec.Command("go", "build", "-buildvcs=false", "-o", binPath, "../")
	if err := cmd.Run(); err != nil {
		t.Skipf("failed to build sciontool for integration test: %v", err)
	}

	// Test running a simple command — filter Hub env vars from the
	// subprocess environment as belt-and-suspenders protection.
	testCmd := exec.Command(binPath, "init", "--", "echo", "hello")
	testCmd.Env = filterHubEnv(os.Environ())
	output, err := testCmd.CombinedOutput()
	if err != nil {
		t.Errorf("init command failed: %v\nOutput: %s", err, output)
	}
	if !strings.Contains(string(output), "hello") {
		t.Errorf("expected output to contain 'hello', got: %s", output)
	}
}

func TestGitCloneWorkspace_NoCloneURL(t *testing.T) {
	// Ensure SCION_GIT_CLONE_URL is not set
	orig := os.Getenv("SCION_GIT_CLONE_URL")
	_ = os.Unsetenv("SCION_GIT_CLONE_URL")
	defer func() {
		if orig != "" {
			_ = os.Setenv("SCION_GIT_CLONE_URL", orig)
		}
	}()

	tmpWorkspace := t.TempDir()
	t.Setenv("SCION_WORKSPACE_PATH", tmpWorkspace)
	err := gitCloneWorkspace(0, 0, "/tmp", false)
	if err != nil {
		t.Errorf("expected nil error when SCION_GIT_CLONE_URL is not set, got: %v", err)
	}
}

// TestGitCloneWorkspace_EnforcedRefusesUndroppableCredentials proves
// gitCloneWorkspace refuses outright — before running any git command —
// when requirePrivilegeDrop is set but uid/gid do not both pass
// configureGitCommand's own Credential predicate, instead of silently
// running every git command as this process's own (root, in production)
// identity. SCION_GIT_CLONE_URL is set so the function does not return
// early via the "no clone URL configured" path above.
func TestGitCloneWorkspace_EnforcedRefusesUndroppableCredentials(t *testing.T) {
	t.Setenv("SCION_GIT_CLONE_URL", "https://example.invalid/repo.git")

	cases := []struct {
		name     string
		uid, gid int
	}{
		{name: "uid0", uid: 0, gid: 1000},
		{name: "gid0", uid: 1000, gid: 0},
		{name: "both0", uid: 0, gid: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			workspacePath := t.TempDir()
			t.Setenv("SCION_WORKSPACE_PATH", workspacePath)

			err := gitCloneWorkspace(tc.uid, tc.gid, "/tmp", true)
			if !errors.Is(err, errGitCloneWorkspacePrivilegeDropRequired) {
				t.Fatalf("gitCloneWorkspace(uid=%d, gid=%d, requirePrivilegeDrop=true) = %v, want errGitCloneWorkspacePrivilegeDropRequired", tc.uid, tc.gid, err)
			}
			if _, err := os.Stat(filepath.Join(workspacePath, ".git")); err == nil {
				t.Error("expected no .git to be created when the refusal fires before any git command runs")
			}
		})
	}
}

func TestGitCloneWorkspace_WorkspaceExists(t *testing.T) {
	// Create a temp dir with content to simulate existing workspace
	tmpDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmpDir, "README.md"), []byte("existing"), 0644); err != nil {
		t.Fatal(err)
	}

	if isWorkspaceEmpty(tmpDir) {
		t.Error("expected non-empty workspace to return false for isWorkspaceEmpty")
	}
}

func TestIsWorkspaceEmpty(t *testing.T) {
	t.Run("nonexistent directory", func(t *testing.T) {
		if !isWorkspaceEmpty("/nonexistent/path/12345") {
			t.Error("expected true for nonexistent directory")
		}
	})

	t.Run("empty directory", func(t *testing.T) {
		tmpDir := t.TempDir()
		if !isWorkspaceEmpty(tmpDir) {
			t.Error("expected true for empty directory")
		}
	})

	t.Run("directory with files", func(t *testing.T) {
		tmpDir := t.TempDir()
		_ = os.WriteFile(filepath.Join(tmpDir, "file.txt"), []byte("content"), 0644)
		if isWorkspaceEmpty(tmpDir) {
			t.Error("expected false for directory with files")
		}
	})

	t.Run("directory with only .scion marker", func(t *testing.T) {
		tmpDir := t.TempDir()
		_ = os.MkdirAll(filepath.Join(tmpDir, ".scion"), 0755)
		if !isWorkspaceEmpty(tmpDir) {
			t.Error("expected true when workspace contains only .scion marker")
		}
	})

	t.Run("directory with only .scion-volumes", func(t *testing.T) {
		tmpDir := t.TempDir()
		_ = os.MkdirAll(filepath.Join(tmpDir, ".scion-volumes"), 0755)
		if !isWorkspaceEmpty(tmpDir) {
			t.Error("expected true when workspace contains only .scion-volumes")
		}
	})

	t.Run("directory with .scion and .scion-volumes", func(t *testing.T) {
		tmpDir := t.TempDir()
		_ = os.MkdirAll(filepath.Join(tmpDir, ".scion"), 0755)
		_ = os.MkdirAll(filepath.Join(tmpDir, ".scion-volumes"), 0755)
		if !isWorkspaceEmpty(tmpDir) {
			t.Error("expected true when workspace contains only .scion and .scion-volumes")
		}
	})

	t.Run("directory with .scion marker and real content", func(t *testing.T) {
		tmpDir := t.TempDir()
		_ = os.MkdirAll(filepath.Join(tmpDir, ".scion"), 0755)
		_ = os.WriteFile(filepath.Join(tmpDir, "README.md"), []byte("content"), 0644)
		if isWorkspaceEmpty(tmpDir) {
			t.Error("expected false when workspace has .scion and real files")
		}
	})

	t.Run("directory with only .agents marker", func(t *testing.T) {
		tmpDir := t.TempDir()
		_ = os.MkdirAll(filepath.Join(tmpDir, ".agents"), 0755)
		if !isWorkspaceEmpty(tmpDir) {
			t.Error("expected true when workspace contains only .agents marker")
		}
	})

	t.Run("directory with all provisioning markers", func(t *testing.T) {
		tmpDir := t.TempDir()
		_ = os.MkdirAll(filepath.Join(tmpDir, ".scion"), 0755)
		_ = os.MkdirAll(filepath.Join(tmpDir, ".scion-volumes"), 0755)
		_ = os.MkdirAll(filepath.Join(tmpDir, ".agents"), 0755)
		if !isWorkspaceEmpty(tmpDir) {
			t.Error("expected true when workspace contains only .scion, .scion-volumes, and .agents")
		}
	})

	t.Run("directory with .agents and real content", func(t *testing.T) {
		tmpDir := t.TempDir()
		_ = os.MkdirAll(filepath.Join(tmpDir, ".agents"), 0755)
		_ = os.WriteFile(filepath.Join(tmpDir, "main.go"), []byte("package main"), 0644)
		if isWorkspaceEmpty(tmpDir) {
			t.Error("expected false when workspace has .agents and real files")
		}
	})
}

func TestSanitizeGitOutput(t *testing.T) {
	tests := []struct {
		name     string
		output   string
		token    string
		expected string
	}{
		{
			name:     "replaces token in output",
			output:   "fatal: Authentication failed for 'https://oauth2:ghp_secret123@github.com/org/repo.git/'",
			token:    "ghp_secret123",
			expected: "fatal: Authentication failed for 'https://oauth2:***@github.com/org/repo.git/'",
		},
		{
			name:     "replaces multiple occurrences",
			output:   "token ghp_abc then ghp_abc again",
			token:    "ghp_abc",
			expected: "token *** then *** again",
		},
		{
			name:     "empty token returns output unchanged",
			output:   "some output text",
			token:    "",
			expected: "some output text",
		},
		{
			name:     "no match returns output unchanged",
			output:   "nothing sensitive here",
			token:    "ghp_notpresent",
			expected: "nothing sensitive here",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := sanitizeGitOutput(tt.output, tt.token)
			if result != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, result)
			}
		})
	}
}

func TestBuildAuthenticatedURL(t *testing.T) {
	tests := []struct {
		name     string
		cloneURL string
		token    string
		expected string
	}{
		{
			name:     "adds oauth2 credentials to HTTPS URL",
			cloneURL: "https://github.com/org/repo.git",
			token:    "ghp_token123",
			expected: "https://oauth2:ghp_token123@github.com/org/repo.git",
		},
		{
			name:     "no token returns URL unchanged",
			cloneURL: "https://github.com/org/repo.git",
			token:    "",
			expected: "https://github.com/org/repo.git",
		},
		{
			name:     "handles URL without .git suffix",
			cloneURL: "https://github.com/org/repo",
			token:    "ghp_abc",
			expected: "https://oauth2:ghp_abc@github.com/org/repo",
		},
		{
			name:     "handles URL with port",
			cloneURL: "https://github.example.com:8443/org/repo.git",
			token:    "tok",
			expected: "https://oauth2:tok@github.example.com:8443/org/repo.git",
		},
		{
			name:     "schemeless URL gets https prefix added with token",
			cloneURL: "github.com/org/repo",
			token:    "ghp_abc",
			expected: "https://oauth2:ghp_abc@github.com/org/repo",
		},
		{
			name:     "schemeless URL gets https prefix added without token",
			cloneURL: "github.com/org/repo",
			token:    "",
			expected: "https://github.com/org/repo",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := buildAuthenticatedURL(tt.cloneURL, tt.token)
			if result != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, result)
			}
		})
	}
}

func TestBuildAuthenticatedURL_SpecialCharsInToken(t *testing.T) {
	tests := []struct {
		name     string
		token    string
		contains string
	}{
		{
			name:     "token with percent sign",
			token:    "ghp_abc%def",
			contains: "oauth2:ghp_abc%25def@",
		},
		{
			name:     "token with at sign",
			token:    "ghp_abc@def",
			contains: "oauth2:ghp_abc%40def@",
		},
		{
			name:     "token with hash sign",
			token:    "ghp_abc#def",
			contains: "oauth2:ghp_abc%23def@",
		},
		{
			name:     "token with all special characters",
			token:    "ghp_%@#tok",
			contains: "oauth2:ghp_%25%40%23tok@",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := buildAuthenticatedURL("https://github.com/org/repo.git", tt.token)
			if !strings.Contains(result, tt.contains) {
				t.Errorf("expected result to contain %q, got %q", tt.contains, result)
			}
		})
	}
}

func TestDetectDefaultBranch(t *testing.T) {
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "safe.bareRepository")
	t.Setenv("GIT_CONFIG_VALUE_0", "all")

	// Create a bare repo to serve as the "remote"
	remote := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = remote
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %s %v", args, out, err)
		}
	}
	run("init", "--bare", ".")
	// The bare repo's HEAD points to master by default in older git versions,
	// or main in newer ones. Set it explicitly for a deterministic test.
	run("symbolic-ref", "HEAD", "refs/heads/testbranch")

	// Create a local repo that has this bare repo as origin
	local := t.TempDir()
	localRun := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = local
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %s %v", args, out, err)
		}
	}
	localRun("init", ".")
	localRun("remote", "add", "origin", remote)

	// We need at least one commit in the remote for ls-remote to work
	// Create a commit directly in the bare repo
	tmpWork := t.TempDir()
	cloneRun := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = tmpWork
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %s %v", args, out, err)
		}
	}
	cloneRun("clone", remote, ".")
	cloneRun("config", "user.email", "test@test.com")
	cloneRun("config", "user.name", "Test")
	cloneRun("checkout", "-b", "testbranch")
	cloneRun("commit", "--allow-empty", "-m", "init")
	cloneRun("push", "origin", "testbranch")

	noop := func(cmd *exec.Cmd) {}
	result := detectDefaultBranch(local, noop)
	if result != "testbranch" {
		t.Errorf("expected 'testbranch', got %q", result)
	}
}

func TestSanitizeGitOutput_LongToken(t *testing.T) {
	// Fine-grained GitHub PATs are 93 characters long
	longToken := "github_pat_" + strings.Repeat("A", 82) // 93 chars total
	output := "fatal: Authentication failed for 'https://oauth2:" + longToken + "@github.com/org/repo.git/'"

	result := sanitizeGitOutput(output, longToken)

	if strings.Contains(result, longToken) {
		t.Error("long token should be redacted from output")
	}
	if !strings.Contains(result, "***") {
		t.Error("redacted token should be replaced with ***")
	}
	expected := "fatal: Authentication failed for 'https://oauth2:***@github.com/org/repo.git/'"
	if result != expected {
		t.Errorf("expected %q, got %q", expected, result)
	}
}

func TestUseDirectPasswdEdit(t *testing.T) {
	tests := []struct {
		name     string
		envVars  map[string]string
		expected bool
	}{
		{
			name:     "no env vars set",
			envVars:  map[string]string{},
			expected: false,
		},
		{
			name:     "container=podman",
			envVars:  map[string]string{"container": "podman"},
			expected: true,
		},
		{
			name:     "container=docker (not podman)",
			envVars:  map[string]string{"container": "docker"},
			expected: false,
		},
		{
			name:     "SCION_ALT_USERMOD set",
			envVars:  map[string]string{"SCION_ALT_USERMOD": "1"},
			expected: true,
		},
		{
			name:     "both set",
			envVars:  map[string]string{"container": "podman", "SCION_ALT_USERMOD": "1"},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Clear both env vars, then set what the test needs
			t.Setenv("container", "")
			t.Setenv("SCION_ALT_USERMOD", "")
			_ = os.Unsetenv("container")
			_ = os.Unsetenv("SCION_ALT_USERMOD")
			for k, v := range tt.envVars {
				t.Setenv(k, v)
			}

			result := useDirectPasswdEdit()
			if result != tt.expected {
				t.Errorf("expected %v, got %v", tt.expected, result)
			}
		})
	}
}

func TestIsUIDMapped(t *testing.T) {
	// On a normal (non-namespaced) host, all UIDs should be mapped.
	// /proc/self/uid_map typically shows "0 0 4294967295" or similar.
	// We test the function by verifying our own UID is mapped and
	// that an absurdly large UID is likely not mapped in rootless mode
	// (but may be mapped on a normal host).

	t.Run("current user UID is mapped", func(t *testing.T) {
		uid := os.Getuid()
		if !isUIDMapped(uid) {
			t.Errorf("expected current user UID %d to be mapped", uid)
		}
	})

	t.Run("UID 0 is always mapped", func(t *testing.T) {
		if !isUIDMapped(0) {
			t.Error("expected UID 0 to be mapped")
		}
	})
}

func TestIsAuthError(t *testing.T) {
	tests := []struct {
		name     string
		stderr   string
		expected bool
	}{
		{
			name:     "authentication failed",
			stderr:   "fatal: Authentication failed for 'https://github.com/org/repo.git/'",
			expected: true,
		},
		{
			name:     "could not read username",
			stderr:   "fatal: could not read Username for 'https://github.com': terminal prompts disabled",
			expected: true,
		},
		{
			name:     "403 forbidden",
			stderr:   "fatal: unable to access 'https://github.com/org/repo.git/': The requested URL returned error: 403",
			expected: true,
		},
		{
			name:     "401 unauthorized",
			stderr:   "fatal: unable to access 'https://github.com/org/repo.git/': The requested URL returned error: 401",
			expected: true,
		},
		{
			name:     "invalid credentials",
			stderr:   "remote: Invalid credentials",
			expected: true,
		},
		{
			name:     "branch not found",
			stderr:   "fatal: Remote branch 'nonexistent' not found in upstream origin",
			expected: false,
		},
		{
			name:     "network error",
			stderr:   "fatal: unable to access 'https://nonexistent.invalid/org/repo.git/': Could not resolve host: nonexistent.invalid",
			expected: false,
		},
		{
			name:     "empty stderr",
			stderr:   "",
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isAuthError(tt.stderr); got != tt.expected {
				t.Errorf("isAuthError(%q) = %v, want %v", tt.stderr, got, tt.expected)
			}
		})
	}
}

func TestFormatCloneError(t *testing.T) {
	t.Run("no token", func(t *testing.T) {
		err := formatCloneError("fatal: could not read Username", "")
		if !strings.Contains(err.Error(), "no GITHUB_TOKEN secret configured") {
			t.Errorf("expected 'no GITHUB_TOKEN' message, got: %v", err)
		}
		if !strings.Contains(err.Error(), "fatal: could not read Username") {
			t.Errorf("expected stderr in error, got: %v", err)
		}
	})

	t.Run("with token", func(t *testing.T) {
		err := formatCloneError("fatal: Authentication failed", "ghp_token123")
		// Auth errors should include guidance about checking credentials
		if !strings.Contains(err.Error(), "GITHUB_TOKEN") {
			t.Errorf("expected GITHUB_TOKEN guidance in error, got: %v", err)
		}
		if !strings.Contains(err.Error(), "fatal: Authentication failed") {
			t.Errorf("expected stderr in error, got: %v", err)
		}
	})

	t.Run("unclassified error with token", func(t *testing.T) {
		err := formatCloneError("fatal: disk full", "ghp_token123")
		if strings.Contains(err.Error(), "GITHUB_TOKEN") {
			t.Errorf("unclassified error should not mention GITHUB_TOKEN, got: %v", err)
		}
		if !strings.Contains(err.Error(), "unclassified error") {
			t.Errorf("expected 'unclassified error' in message, got: %v", err)
		}
		if !strings.Contains(err.Error(), "fatal: disk full") {
			t.Errorf("expected stderr in error, got: %v", err)
		}
	})
}

func TestIsClaude(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		expected bool
	}{
		{name: "claude binary", args: []string{"claude"}, expected: true},
		{name: "claude with args", args: []string{"claude", "--model", "opus"}, expected: true},
		{name: "full path to claude", args: []string{"/usr/local/bin/claude"}, expected: true},
		{name: "claude-code variant", args: []string{"claude-code"}, expected: true},
		{name: "tmux wrapping claude", args: []string{"tmux", "new-session", "-s", "scion", "claude", "--no-chrome"}, expected: true},
		{name: "tmux wrapping claude full path", args: []string{"tmux", "new-session", "-s", "scion", "/usr/local/bin/claude"}, expected: true},
		{name: "tmux with joined cmdline", args: []string{"tmux", "new-session", "-s", "scion", "claude --no-chrome --dangerously-skip-permissions"}, expected: true},
		{name: "tmux with joined cmdline full path", args: []string{"tmux", "new-session", "-s", "scion", "/usr/local/bin/claude --no-chrome"}, expected: true},
		{name: "gemini binary", args: []string{"gemini"}, expected: false},
		{name: "bash command", args: []string{"bash", "-c", "echo hello"}, expected: false},
		{name: "empty args", args: []string{}, expected: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isClaude(tt.args); got != tt.expected {
				t.Errorf("isClaude(%v) = %v, want %v", tt.args, got, tt.expected)
			}
		})
	}
}

func TestWriteEnvFile(t *testing.T) {
	tmpHome := t.TempDir()

	// Set some SCION_ env vars and a non-SCION var
	t.Setenv("SCION_AGENT_NAME", "test-agent")
	t.Setenv("SCION_AUTH_TOKEN", "secret-token-123")
	t.Setenv("SCION_HARNESS", "gemini")
	t.Setenv("NOT_SCION_VAR", "should-not-appear")

	writeEnvFile(tmpHome, 0, 0)

	envPath := filepath.Join(tmpHome, ".scion", "scion-env")
	data, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("failed to read scion-env file: %v", err)
	}

	content := string(data)

	// Should contain SCION_ vars
	if !strings.Contains(content, `export SCION_AGENT_NAME="test-agent"`) {
		t.Errorf("expected SCION_AGENT_NAME in env file, got:\n%s", content)
	}
	if !strings.Contains(content, `export SCION_AUTH_TOKEN="secret-token-123"`) {
		t.Errorf("expected SCION_AUTH_TOKEN in env file, got:\n%s", content)
	}
	if !strings.Contains(content, `export SCION_HARNESS="gemini"`) {
		t.Errorf("expected SCION_HARNESS in env file, got:\n%s", content)
	}

	// Should NOT contain non-SCION vars
	if strings.Contains(content, "NOT_SCION_VAR") {
		t.Errorf("unexpected NOT_SCION_VAR in env file")
	}

	// Should contain the header comment
	if !strings.Contains(content, "Auto-generated") {
		t.Errorf("expected header comment in env file")
	}
}

func TestWriteEnvFile_IncludesGitHubToken(t *testing.T) {
	tmpHome := t.TempDir()

	t.Setenv("GITHUB_TOKEN", "ghp_test123")

	writeEnvFile(tmpHome, 0, 0)

	envPath := filepath.Join(tmpHome, ".scion", "scion-env")
	data, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("failed to read scion-env file: %v", err)
	}

	if !strings.Contains(string(data), `export GITHUB_TOKEN="ghp_test123"`) {
		t.Errorf("expected GITHUB_TOKEN in env file, got:\n%s", string(data))
	}
}

func TestWriteEnvFile_ReflectsUpdatedGitHubToken(t *testing.T) {
	tmpHome := t.TempDir()

	t.Setenv("GITHUB_TOKEN", "ghs_initial_token_abc123")

	writeEnvFile(tmpHome, 0, 0)

	envPath := filepath.Join(tmpHome, ".scion", "scion-env")
	data, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("failed to read scion-env file: %v", err)
	}
	if !strings.Contains(string(data), `export GITHUB_TOKEN="ghs_initial_token_abc123"`) {
		t.Fatalf("expected initial GITHUB_TOKEN in env file, got:\n%s", string(data))
	}

	// Simulate what StartGitHubTokenRefresh does: os.Setenv then OnRefreshed calls writeEnvFile
	t.Setenv("GITHUB_TOKEN", "ghs_refreshed_token_xyz789")

	writeEnvFile(tmpHome, 0, 0)

	data, err = os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("failed to read scion-env file after refresh: %v", err)
	}
	content := string(data)

	if !strings.Contains(content, `export GITHUB_TOKEN="ghs_refreshed_token_xyz789"`) {
		t.Errorf("expected refreshed GITHUB_TOKEN in env file, got:\n%s", content)
	}
	if strings.Contains(content, "ghs_initial_token_abc123") {
		t.Errorf("stale initial token should not appear in env file after refresh")
	}
}

// TestWriteEnvFile_RefusesSymlinkAtFinalPath proves writeEnvFile refuses to
// write through a symlink: a workload that has replaced
// $HOME/.scion/scion-env with a symlink must have the write refused, with
// the symlink's target left untouched, instead of root following it.
func TestWriteEnvFile_RefusesSymlinkAtFinalPath(t *testing.T) {
	tmpHome := t.TempDir()
	scionDir := filepath.Join(tmpHome, ".scion")
	if err := os.MkdirAll(scionDir, 0755); err != nil {
		t.Fatalf("mkdir .scion: %v", err)
	}

	victim := filepath.Join(scionDir, "victim")
	if err := os.WriteFile(victim, []byte("do-not-touch"), 0600); err != nil {
		t.Fatalf("write victim: %v", err)
	}
	envPath := filepath.Join(scionDir, "scion-env")
	if err := os.Symlink(victim, envPath); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	t.Setenv("SCION_AGENT_NAME", "test-agent")
	writeEnvFile(tmpHome, 0, 0)

	data, err := os.ReadFile(victim)
	if err != nil {
		t.Fatalf("read victim: %v", err)
	}
	if string(data) != "do-not-touch" {
		t.Errorf("symlink target was modified: %q", data)
	}

	linkInfo, err := os.Lstat(envPath)
	if err != nil {
		t.Fatalf("lstat scion-env: %v", err)
	}
	if linkInfo.Mode()&os.ModeSymlink == 0 {
		t.Error("the symlink at the final path should be untouched")
	}
}

// TestWriteEnvFile_DirChownSurvivesSwapAfterWrite proves that, in the
// scenario where the workload — which owns $HOME and can observe the
// scion-env write completing (e.g. via inotify on $HOME/.scion) — renames
// $HOME/.scion away and drops a symlink to a victim directory in its place
// before root's directory chown runs, the chown lands on the original
// directory (wherever its entry ended up), never on the victim, because it
// operates on a directory fd resolved before the swap rather than
// re-resolving the path afterward.
//
// The swap is injected through writeEnvFileAfterWriteForTest rather than a
// real race, so this is deterministic: the seam fires at exactly the
// window the race needs (after the file write, before the
// directory chown), which a symlink planted before the call does not
// exercise — the write itself already refuses a pre-existing symlink, so
// only a swap injected in that specific window can distinguish this
// from the original path-based chown.
//
// scionDirOwnerUID is also overridden here to report uid 0 (root): without
// this, writeEnvFile's owner gating skips the chown entirely, because a
// freshly-created .scion directory is owned by the (non-root) test process
// itself, not root — which would make this test pass by doing nothing on
// the chown path at all. Forcing "currently root-owned" is what actually
// drives the fd-based chown this test exists to exercise.
func TestWriteEnvFile_DirChownSurvivesSwapAfterWrite(t *testing.T) {
	tmpHome := t.TempDir()
	victimDir := filepath.Join(tmpHome, "victim-dir")
	if err := os.MkdirAll(victimDir, 0700); err != nil {
		t.Fatalf("mkdir victim: %v", err)
	}
	victimInfoBefore, err := os.Stat(victimDir)
	if err != nil {
		t.Fatalf("stat victim before: %v", err)
	}

	scionDir := filepath.Join(tmpHome, ".scion")
	movedDir := filepath.Join(tmpHome, ".scion.moved")

	var movedStBefore *syscall.Stat_t
	writeEnvFileAfterWriteForTest = func(dir string) {
		if err := os.Rename(dir, movedDir); err != nil {
			t.Errorf("swap: rename %s: %v", dir, err)
			return
		}
		// Snapshot ctime right after the rename, before writeEnvFile's own
		// chown call runs against whatever fd it still holds — this is the
		// "before" baseline the final assertion below needs, captured at
		// movedDir's own final path (it didn't exist under that name before
		// this rename, so there's no earlier point to snapshot it at).
		movedInfoBefore, serr := os.Lstat(movedDir)
		if serr != nil {
			t.Errorf("lstat moved dir right after rename: %v", serr)
			return
		}
		movedStBefore = movedInfoBefore.Sys().(*syscall.Stat_t)
		// A brief settle so the chown call below is guaranteed to bump
		// ctime by a measurable amount — see dirfd's ctimeSettle for why
		// this matters on this test suite's filesystem/clock source.
		time.Sleep(15 * time.Millisecond)

		if err := os.Symlink(victimDir, dir); err != nil {
			t.Errorf("swap: symlink %s -> %s: %v", dir, victimDir, err)
		}
	}
	t.Cleanup(func() { writeEnvFileAfterWriteForTest = nil })

	origOwnerUID := scionDirOwnerUID
	scionDirOwnerUID = func(int) (uint32, error) { return 0, nil }
	t.Cleanup(func() { scionDirOwnerUID = origOwnerUID })

	t.Setenv("SCION_AGENT_NAME", "test-agent")
	writeEnvFile(tmpHome, os.Getuid(), os.Getgid())

	// The victim directory must be completely untouched: same mode, same
	// change time (chowning even to the same uid/gid still bumps ctime, so
	// an unchanged ctime proves chown(2) never ran against it).
	victimInfoAfter, err := os.Stat(victimDir)
	if err != nil {
		t.Fatalf("stat victim after: %v", err)
	}
	victimStBefore := victimInfoBefore.Sys().(*syscall.Stat_t)
	victimStAfter := victimInfoAfter.Sys().(*syscall.Stat_t)
	if statCtime(victimStAfter) != statCtime(victimStBefore) {
		t.Error("victim directory's change time advanced: it was chowned")
	}

	// ".scion" itself must still be the symlink the swap planted — nothing
	// should have unlinked or replaced it either.
	linkInfo, err := os.Lstat(scionDir)
	if err != nil {
		t.Fatalf("lstat .scion: %v", err)
	}
	if linkInfo.Mode()&os.ModeSymlink == 0 {
		t.Error("expected .scion to still be the symlink the swap planted")
	}

	// The real directory, now at its moved-away path, is the one that
	// should have been chowned (here, to the test's own uid/gid, which is
	// always permitted and still bumps ctime). Asserting the ctime actually
	// advanced (not just that the directory still exists) is what stops
	// this test from passing by doing nothing on the chown path.
	if movedStBefore == nil {
		t.Fatal("hook never captured movedStBefore — test setup is broken")
	}
	movedInfoAfter, err := os.Stat(movedDir)
	if err != nil {
		t.Fatalf("stat moved-away original .scion: %v", err)
	}
	movedStAfter := movedInfoAfter.Sys().(*syscall.Stat_t)
	if statCtime(movedStAfter) == statCtime(movedStBefore) {
		t.Error("moved-away .scion directory's change time did not advance: it was not chowned")
	}
}

func TestGitCloneWorkspace_DefaultEnvValues(t *testing.T) {
	// Set SCION_GIT_CLONE_URL to trigger the clone path, but use a URL
	// that will cause a predictable early failure (non-existent host).
	// This tests that the env parsing logic runs with correct defaults.
	t.Setenv("SCION_GIT_CLONE_URL", "https://nonexistent.invalid/org/repo.git")
	// Explicitly unset branch and depth to verify defaults
	t.Setenv("SCION_GIT_BRANCH", "")
	t.Setenv("SCION_GIT_DEPTH", "")
	t.Setenv("SCION_AGENT_NAME", "test-agent")
	t.Setenv("GITHUB_TOKEN", "")

	// gitCloneWorkspace will fail at the git clone step, but we can verify
	// the function doesn't panic and returns a meaningful error.
	// uid=0 exercises the scion-user fallback path (the lookup will fail
	// gracefully outside a container where no scion user exists).

	tmpWorkspace := t.TempDir()
	t.Setenv("SCION_WORKSPACE_PATH", tmpWorkspace)
	err := gitCloneWorkspace(0, 0, "/tmp", false)
	if err == nil {
		t.Fatal("expected error from git clone to nonexistent host")
	}
	// The error may come from git init, git fetch, or git clone depending
	// on how far the function gets before failing.
	errMsg := err.Error()
	if !strings.Contains(errMsg, "git clone failed") && !strings.Contains(errMsg, "git init failed") && !strings.Contains(errMsg, "git remote add failed") && !strings.Contains(errMsg, "failed") {
		t.Errorf("expected a git failure error, got: %v", err)
	}

	// Verify .git/ is removed after clone failure to prevent credential leak (miller79/scion#65).
	if _, err := os.Stat(filepath.Join(tmpWorkspace, ".git")); !os.IsNotExist(err) {
		t.Error(".git/ should be removed after clone failure to prevent credential leak")
	}
}

func TestGitCloneWorkspace_NonZeroUIDChownsWorkspace(t *testing.T) {
	// Verify that gitCloneWorkspace chowns /workspace before cloning when
	// a non-zero uid is provided. We use a temp dir as the workspace and
	// our own uid/gid so the chown succeeds without root.
	tmpDir := t.TempDir()

	// Monkey-patch: override workspacePath by setting clone URL so the
	// function proceeds past the early exit, but it will fail at git clone.
	// The important thing is it doesn't panic on chown.
	t.Setenv("SCION_GIT_CLONE_URL", "https://nonexistent.invalid/org/repo.git")
	t.Setenv("SCION_GIT_BRANCH", "main")
	t.Setenv("SCION_GIT_DEPTH", "1")
	t.Setenv("SCION_AGENT_NAME", "test-chown")
	t.Setenv("GITHUB_TOKEN", "")

	// We can't override the hardcoded /workspace path, so we test that
	// the function proceeds without panic when uid > 0. The chown of
	// /workspace will fail (not writable in test), but the error is logged,
	// not returned, so the function continues to the git clone step.
	uid := os.Getuid()
	gid := os.Getgid()
	_ = tmpDir // workspace path is hardcoded; this confirms the logic flow

	tmpWorkspace := t.TempDir()
	t.Setenv("SCION_WORKSPACE_PATH", tmpWorkspace)
	err := gitCloneWorkspace(uid, gid, "/tmp", false)
	if err == nil {
		t.Fatal("expected error from git clone to nonexistent host")
	}
	errMsg := err.Error()
	if !strings.Contains(errMsg, "git clone failed") && !strings.Contains(errMsg, "git init failed") && !strings.Contains(errMsg, "git remote add failed") && !strings.Contains(errMsg, "failed") {
		t.Errorf("expected a git failure error, got: %v", err)
	}

	// Verify .git/ is removed after clone failure to prevent credential leak (miller79/scion#65).
	if _, err := os.Stat(filepath.Join(tmpWorkspace, ".git")); !os.IsNotExist(err) {
		t.Error(".git/ should be removed after clone failure to prevent credential leak")
	}
}

func TestConfigureGitCommand_SkipsCredentialOverrideWhenAlreadyRunningAsTargetUser(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "git", "status")

	configureGitCommand(cmd, os.Getuid(), os.Getgid())

	if !slices.Contains(cmd.Env, "GIT_TERMINAL_PROMPT=0") {
		t.Fatal("expected GIT_TERMINAL_PROMPT=0 to be set")
	}
	if cmd.SysProcAttr != nil {
		t.Fatalf("expected no credential override when already running as target user, got %#v", cmd.SysProcAttr)
	}
}

func TestConfigureGitCommand_SkipsCredentialOverrideForNonRootDifferentTarget(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("test only covers non-root behavior")
	}

	cmd := exec.CommandContext(context.Background(), "git", "status")
	configureGitCommand(cmd, os.Getuid()+1, os.Getgid())

	if !slices.Contains(cmd.Env, "GIT_TERMINAL_PROMPT=0") {
		t.Fatal("expected GIT_TERMINAL_PROMPT=0 to be set")
	}
	if cmd.SysProcAttr != nil {
		t.Fatalf("expected no credential override for non-root process, got %#v", cmd.SysProcAttr)
	}
}

// TestConfigureGitCommand_PropagatesTrustBundleEnv proves configureGitCommand
// passes GIT_SSL_CAINFO through to the `git` subprocess it configures:
// configureGitCommand (init.go) builds cmd.Env as
// append(os.Environ(), "GIT_TERMINAL_PROMPT=0") — a full copy of the process
// environment, not an allowlisted subset — so any CA-bundle var already set
// in this process's own environment reaches the `git` subprocess unchanged,
// with no code change needed here to carry it through.
func TestConfigureGitCommand_PropagatesTrustBundleEnv(t *testing.T) {
	t.Setenv("GIT_SSL_CAINFO", "/run/ate/trust-bundle.pem")

	cmd := exec.CommandContext(context.Background(), "git", "status")
	configureGitCommand(cmd, os.Getuid(), os.Getgid())

	if !slices.Contains(cmd.Env, "GIT_SSL_CAINFO=/run/ate/trust-bundle.pem") {
		t.Errorf("cmd.Env = %v, want it to contain GIT_SSL_CAINFO=/run/ate/trust-bundle.pem", cmd.Env)
	}
}

func TestEnsureWorkspaceOwnership_SkipsChownWhenNonRoot(t *testing.T) {
	chownCalled := false
	chown := func(string, int, int) error {
		chownCalled = true
		return nil
	}

	ensureWorkspaceOwnership("/workspace", 1000, 1000, 1000, chown)

	if chownCalled {
		t.Fatal("expected chown to be skipped when already running as non-root")
	}
}

func TestEnsureWorkspaceOwnership_ChownsWhenRoot(t *testing.T) {
	var gotPath string
	var gotUID, gotGID int
	chown := func(path string, uid, gid int) error {
		gotPath = path
		gotUID = uid
		gotGID = gid
		return nil
	}

	ensureWorkspaceOwnership("/workspace", 1000, 1000, 0, chown)

	if gotPath != "/workspace" || gotUID != 1000 || gotGID != 1000 {
		t.Fatalf("unexpected chown call: path=%q uid=%d gid=%d", gotPath, gotUID, gotGID)
	}
}

// --- chownTreeRootOwned input validation ---
//
// The full table of rejected critical-system-path names, the device-boundary
// walk behavior, and the mountinfo-based bind-source check are all tested
// directly against pkg/util/fsutil (TestCheckRoot_*, TestChownTree_*,
// TestCheckMountSourceReader_*), which is what chownTreeRootOwned delegates
// to. Tests here only cover this call site's own wiring, and never invoke
// the recursive chown on anything other than a t.TempDir() tree.

// TestChownTreeRootOwned_RejectsSymlinkToCriticalPath proves the guard
// resolves symlinks before checking, so a workspace path that is itself a
// symlink pointing at a critical system path cannot slip through. The
// symlink lives under t.TempDir(); only its target names a critical path,
// and resolution happens inside CheckRoot before any walk starts.
func TestChownTreeRootOwned_RejectsSymlinkToCriticalPath(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "workspace-link")
	if err := os.Symlink("/etc", link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	_, _, err := chownTreeRootOwned(link, os.Getuid(), os.Getgid(), true)
	if !errors.Is(err, fsutil.ErrCriticalSystemPath) {
		t.Fatalf("chownTreeRootOwned(%q) = %v, want ErrCriticalSystemPath (resolves to /etc)", link, err)
	}
}

// TestChownTreeRootOwned_RejectsFilesystemRootLookalike covers a directory laid
// out like a filesystem root in a temp dir: a workspace whose path carries no
// critical-path name, but whose contents look like a filesystem root (an
// etc/passwd, a usr/bin, and a proc marker). This must be refused, and
// nothing under it may be touched.
func TestChownTreeRootOwned_RejectsFilesystemRootLookalike(t *testing.T) {
	dir := t.TempDir()
	mustMkdirAllT(t, filepath.Join(dir, "etc"))
	mustWriteFileT(t, filepath.Join(dir, "etc", "passwd"), "root:x:0:0:root:/root:/bin/sh\n")
	mustMkdirAllT(t, filepath.Join(dir, "usr", "bin"))
	mustMkdirAllT(t, filepath.Join(dir, "proc")) // stand-in for a procfs mount
	mustMkdirAllT(t, filepath.Join(dir, "boot"))

	passwdPath := filepath.Join(dir, "etc", "passwd")
	before, err := os.Lstat(passwdPath)
	if err != nil {
		t.Fatalf("lstat before: %v", err)
	}
	beforeStat := before.Sys().(*syscall.Stat_t)

	_, _, err = chownTreeRootOwned(dir, os.Getuid()+1, os.Getgid()+1, true)
	if !errors.Is(err, fsutil.ErrFilesystemRootLookalike) {
		t.Fatalf("chownTreeRootOwned(%q) = %v, want ErrFilesystemRootLookalike", dir, err)
	}

	after, err := os.Lstat(passwdPath)
	if err != nil {
		t.Fatalf("lstat after: %v", err)
	}
	afterStat := after.Sys().(*syscall.Stat_t)
	if afterStat.Uid != beforeStat.Uid || afterStat.Gid != beforeStat.Gid {
		t.Errorf("ownership changed despite refusal: before uid=%d gid=%d, after uid=%d gid=%d",
			beforeStat.Uid, beforeStat.Gid, afterStat.Uid, afterStat.Gid)
	}
}

// TestChownTreeRootOwned_ToleratesSingleRealMarker proves the heuristic
// requires more than one marker, so an ordinary workspace that happens to
// contain exactly one of fsutil's root-lookalike markers (here, a top-level
// "boot/" directory) is not falsely rejected. The full per-marker table and
// the exactly-at-threshold boundary are covered once, directly, in
// pkg/util/fsutil; this is a thin wiring check that chownTreeRootOwned
// doesn't somehow apply a different threshold at this call site.
func TestChownTreeRootOwned_ToleratesSingleRealMarker(t *testing.T) {
	dir := t.TempDir()
	mustMkdirAllT(t, filepath.Join(dir, "boot"))

	if _, _, err := chownTreeRootOwned(dir, os.Getuid(), os.Getgid(), true); err != nil {
		t.Fatalf("chownTreeRootOwned(%q) = %v, want nil (exactly one real marker: boot)", dir, err)
	}
}

// TestChownTreeRootOwned_AllowsOrdinaryWorkspace proves the guard is not
// overbroad: a normal workspace directory must still be walked successfully.
func TestChownTreeRootOwned_AllowsOrdinaryWorkspace(t *testing.T) {
	dir := t.TempDir()
	mustMkdirAllT(t, filepath.Join(dir, "src"))
	mustWriteFileT(t, filepath.Join(dir, "README.md"), "hello\n")

	if _, _, err := chownTreeRootOwned(dir, os.Getuid(), os.Getgid(), true); err != nil {
		t.Fatalf("chownTreeRootOwned(%q) = %v, want nil", dir, err)
	}
}

// TestChownTreeRootOwned_MissingRootIsNoOp is a regression test: a missing
// fix-up target (e.g. an image that never creates /workspace) must be
// treated as nothing to fix up, not an error to log. This call site is the
// one exception to fsutil.ChownTree's normal missing-root-is-an-error
// policy -- see chownTreeRootOwned's doc comment for why.
func TestChownTreeRootOwned_MissingRootIsNoOp(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "does-not-exist")

	if _, _, err := chownTreeRootOwned(missing, os.Getuid(), os.Getgid(), true); err != nil {
		t.Fatalf("chownTreeRootOwned(%q) = %v, want nil for a missing root", missing, err)
	}
}

// mustMkdirDeepChainT creates a chain of exactly depth nested directories
// directly under root (root's own direct child -- the chain's firstName
// level -- is depth 1 in dirfd.ChownTreeNoFollow's own counting), named
// firstName, then "d" for every level in between, then finalName for the
// depth-th and last level, and returns the full path to that final
// directory. firstName keeps two chains built under the same root from
// colliding from their very first level, since every "d" level after that
// is otherwise identically named.
//
// This is used to deterministically drive dirfd.ChownTreeNoFollow's own
// max-recursion-depth guard without needing root privilege or any
// permission-bit trick: see
// TestChownTreeRootOwned_PropagatesRealEnumerationFailures's doc comment for
// why a permission-denied subdirectory no longer reaches that guard at all
// under the current fd-based walk.
func mustMkdirDeepChainT(t *testing.T, root string, depth int, firstName, finalName string) string {
	t.Helper()
	segments := make([]string, 0, depth)
	segments = append(segments, firstName)
	for i := 2; i < depth; i++ {
		segments = append(segments, "d")
	}
	segments = append(segments, finalName)
	path := filepath.Join(append([]string{root}, segments...)...)
	if err := os.MkdirAll(path, 0755); err != nil {
		t.Fatalf("mkdir deep chain under %s: %v", root, err)
	}
	return path
}

// TestChownTreeRootOwned_PropagatesRealEnumerationFailures proves that only
// a root which does not exist at all is a no-op: a real, reproducible
// failure encountered while walking an *existing* root must still be
// reported, not merely logged and discarded.
//
// This used to use a mode-0 subdirectory to trigger an EACCES enumeration
// failure, back when chownTreeRootOwned walked with filepath.WalkDir. The
// dirfd-based walk it uses now resolves every entry through
// openat(O_PATH|O_NOFOLLOW) once the openat(O_DIRECTORY|O_NOFOLLOW) attempt
// fails, and O_PATH does not check the target's own permission bits at all
// (verified against a real mode-0 directory: the openat succeeds, and so
// does fstat and even a self-chown through the resulting descriptor) -- so a
// permission-denied subdirectory is no longer an error case for this walk at
// all, just an ordinary leaf it correctly declines to recurse into. The
// walk's own max-recursion-depth guard (dirfd.ChownTreeNoFollow's
// maxWalkDepth, 1024) is the one failure mode this walk still reports for an
// ordinary, unprivileged, reproducible condition, so this test drives that
// instead.
func TestChownTreeRootOwned_PropagatesRealEnumerationFailures(t *testing.T) {
	dir := t.TempDir()
	deepest := mustMkdirDeepChainT(t, dir, 1024, "chain", "too-deep")

	if _, _, err := chownTreeRootOwned(dir, os.Getuid(), os.Getgid(), true); err == nil {
		t.Fatalf("chownTreeRootOwned(%q) = nil, want an error: %q is 1024 levels deep", dir, deepest)
	}
}

// TestChownTreeRootOwned_SkipsEntriesNotOwnedByRoot proves the hardcoded
// ownerUID=0 argument to fsutil.ChownTreeOwnedByUID is actually wired: none
// of the files this test creates are owned by UID 0, and the target uid/gid
// differ from their current owner, so a dropped or broken filter would
// either change their ownership (as root) or fail outright (unprivileged,
// EPERM) -- either way, the assertions below would catch it.
func TestChownTreeRootOwned_SkipsEntriesNotOwnedByRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("every file is root-owned as root, so this can't distinguish a working filter from a dropped one")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	mustWriteFileT(t, path, "hi\n")

	before, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat before: %v", err)
	}
	beforeStat := before.Sys().(*syscall.Stat_t)

	if _, _, err := chownTreeRootOwned(dir, os.Getuid()+1, os.Getgid()+1, true); err != nil {
		t.Fatalf("chownTreeRootOwned(%q) = %v, want nil: the uid-0 filter should skip every entry", dir, err)
	}

	after, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat after: %v", err)
	}
	afterStat := after.Sys().(*syscall.Stat_t)
	if afterStat.Uid != beforeStat.Uid || afterStat.Gid != beforeStat.Gid {
		t.Errorf("ownership changed despite the uid-0 filter: before uid=%d gid=%d, after uid=%d gid=%d",
			beforeStat.Uid, beforeStat.Gid, afterStat.Uid, afterStat.Gid)
	}
}

// TestChownTreeRootOwned_ChecksMountSource proves chownTreeRootOwned
// actually calls checkMountSource and respects its refusal, without needing
// a real mount: it stubs the package variable to simulate a critical bind
// source and confirms both that the error propagates and that nothing under
// root was touched (the check runs before the walk).
func TestChownTreeRootOwned_ChecksMountSource(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	mustWriteFileT(t, path, "hi\n")
	before, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat before: %v", err)
	}
	beforeStat := before.Sys().(*syscall.Stat_t)

	orig := checkMountSource
	checkMountSource = func(root string) error { return fsutil.ErrCriticalMountSource }
	defer func() { checkMountSource = orig }()

	_, _, err = chownTreeRootOwned(dir, os.Getuid()+1, os.Getgid()+1, true)
	if !errors.Is(err, fsutil.ErrCriticalMountSource) {
		t.Fatalf("chownTreeRootOwned(%q) = %v, want it to propagate the stubbed ErrCriticalMountSource", dir, err)
	}

	after, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat after: %v", err)
	}
	afterStat := after.Sys().(*syscall.Stat_t)
	if afterStat.Uid != beforeStat.Uid || afterStat.Gid != beforeStat.Gid {
		t.Errorf("ownership changed despite the stubbed mount-source refusal: before uid=%d gid=%d, after uid=%d gid=%d",
			beforeStat.Uid, beforeStat.Gid, afterStat.Uid, afterStat.Gid)
	}
}

// TestChownTreeRootOwned_InvalidRootNeverReachesMountSource proves the
// ordering chownTreeRootOwned's own comment describes: fsutil.CheckRoot runs
// before checkMountSource, so a root CheckRoot already refuses is never
// looked up in the mount table at all. Stubbing checkMountSource to return
// nil (the "I was never called, nothing to object to" answer) and then
// asserting the call still never happened is what actually locks in the
// ordering: asserting only the returned error's type would pass just as
// well if the two checks ran in the other order and checkMountSource's own
// (different) refusal reached the caller instead.
func TestChownTreeRootOwned_InvalidRootNeverReachesMountSource(t *testing.T) {
	var mountSourceCalls int
	orig := checkMountSource
	checkMountSource = func(root string) error {
		mountSourceCalls++
		return nil
	}
	defer func() { checkMountSource = orig }()

	const criticalRoot = "/etc"
	_, _, err := chownTreeRootOwned(criticalRoot, os.Getuid()+1, os.Getgid()+1, true)
	if !errors.Is(err, fsutil.ErrCriticalSystemPath) {
		t.Fatalf("chownTreeRootOwned(%q) = %v, want fsutil.ErrCriticalSystemPath from CheckRoot", criticalRoot, err)
	}
	if mountSourceCalls != 0 {
		t.Errorf("expected checkMountSource to never run for a root CheckRoot already refuses, got %d call(s)", mountSourceCalls)
	}
}

// TestChownTreeRootOwned_AggregatesMultiplePerEntryFailures proves a second,
// independent per-entry failure is not lost behind the first: two
// independent depth-cap cutoffs under the same root must both be reported
// (joined, each attributed to its own entry), not just whichever one
// dirfd.ChownTreeNoFollow's walk happens to visit first.
// dirfd.ChownTreeNoFollow's own returned error can only ever report root's
// own open/stat failure, never a per-entry one (see its doc comment), so
// chownTreeRootOwned collects every per-entry failure passed to onErr
// itself, wraps each with the entry's own name (onErr's own error carries no
// name or path -- see chownTreeRootOwned's onErr closure), and joins them --
// this is what that join and that wrapping actually prove, beyond the
// single-failure case TestChownTreeRootOwned_PropagatesRealEnumerationFailures
// already covers. See that test's doc comment for why two merely
// unreadable subdirectories (this test's original construction) no longer
// reach any per-entry failure at all under the current fd-based walk, and
// why the max-recursion-depth guard is used here instead.
func TestChownTreeRootOwned_AggregatesMultiplePerEntryFailures(t *testing.T) {
	dir := t.TempDir()
	mustMkdirDeepChainT(t, dir, 1024, "chain-a", "too-deep-a")
	mustMkdirDeepChainT(t, dir, 1024, "chain-b", "too-deep-b")

	_, _, err := chownTreeRootOwned(dir, os.Getuid(), os.Getgid(), true)
	if err == nil {
		t.Fatalf("chownTreeRootOwned(%q) = nil, want a non-nil error: both chain-a and chain-b are 1024 levels deep", dir)
	}
	if !strings.Contains(err.Error(), "too-deep-a") || !strings.Contains(err.Error(), "too-deep-b") {
		t.Errorf("chownTreeRootOwned(%q) error = %v, want it to mention both depth-cap cutoffs", dir, err)
	}
}

func mustMkdirAllT(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

func mustWriteFileT(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestResolveIsSharedGitWorkspace verifies the compat shim logic for detecting
// a shared-plain git workspace. It covers both the new canonical vars and the
// legacy SCION_SHARED_WORKSPACE fallback path.
func TestResolveIsSharedGitWorkspace(t *testing.T) {
	cases := []struct {
		name          string
		workspaceMode string
		workspaceGit  string
		sharedLegacy  string
		want          bool
	}{
		{
			name:          "new vars: shared-plain + git → true",
			workspaceMode: "shared-plain",
			workspaceGit:  "true",
			want:          true,
		},
		{
			name:          "new vars: clone-per-agent + git → false (not shared)",
			workspaceMode: "clone-per-agent",
			workspaceGit:  "true",
			want:          false,
		},
		{
			name:          "new vars: shared-plain without git → false",
			workspaceMode: "shared-plain",
			workspaceGit:  "",
			want:          false,
		},
		{
			name:          "new vars: worktree-per-agent + git → false (not shared)",
			workspaceMode: "worktree-per-agent",
			workspaceGit:  "true",
			want:          false,
		},
		{
			name:         "legacy: SCION_SHARED_WORKSPACE=true → true (fallback)",
			sharedLegacy: "true",
			want:         true,
		},
		{
			name:         "legacy: SCION_SHARED_WORKSPACE absent → false",
			sharedLegacy: "",
			want:         false,
		},
		{
			name:          "new vars take precedence: mode set + legacy true → follows mode",
			workspaceMode: "clone-per-agent",
			workspaceGit:  "true",
			sharedLegacy:  "true",
			want:          false, // mode=clone-per-agent means not shared-plain
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SCION_WORKSPACE_MODE", tc.workspaceMode)
			t.Setenv("SCION_WORKSPACE_GIT", tc.workspaceGit)
			t.Setenv("SCION_SHARED_WORKSPACE", tc.sharedLegacy)

			got := resolveIsSharedGitWorkspace()
			if got != tc.want {
				t.Errorf("resolveIsSharedGitWorkspace() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestParseCapSetUID(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{
			name: "full capabilities (typical Docker root)",
			// CapEff with all bits set — includes CAP_SETUID (bit 7).
			input: "Name:\tinit\nCapEff:\t000001ffffffffff\n",
			want:  true,
		},
		{
			name: "CAP_SETUID present among limited caps",
			// Bits 0-7 set (0xff) — CAP_SETUID (bit 7) is present.
			input: "Name:\tinit\nCapInh:\t0000000000000000\nCapEff:\t00000000000000ff\n",
			want:  true,
		},
		{
			name: "CAP_SETUID absent (gVisor restricted sandbox)",
			// Only bits 0-6 set (0x7f) — CAP_SETUID (bit 7) is missing.
			input: "Name:\tinit\nCapEff:\t000000000000007f\n",
			want:  false,
		},
		{
			name:  "no capabilities at all",
			input: "Name:\tinit\nCapEff:\t0000000000000000\n",
			want:  false,
		},
		{
			name:  "no CapEff line",
			input: "Name:\tinit\nCapInh:\t0000000000000000\n",
			want:  false,
		},
		{
			name:  "empty input",
			input: "",
			want:  false,
		},
		{
			name:  "malformed hex value",
			input: "CapEff:\tnotahexvalue\n",
			want:  false,
		},
		{
			name: "only CAP_SETUID bit set",
			// Only bit 7 set (0x80).
			input: "CapEff:\t0000000000000080\n",
			want:  true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseCapSetUID(tc.input)
			if got != tc.want {
				t.Errorf("parseCapSetUID() = %v, want %v", got, tc.want)
			}
		})
	}
}

// --- Regression coverage for the PID-1 reaper race ---
//
// The reaper race itself (a SIGCHLD handler calling wait4(-1, ...) racing
// exec.Cmd.Wait for the same PID) is exercised in
// pkg/sciontool/procreap's tests, since that's where the fix (managed-PID
// registry consulted by the reaper) lives. The tests below cover the
// consequence that motivated the extra "clean up the whole attempt, not
// just .git" fix requested during review: a git step (e.g. `git checkout`)
// can genuinely succeed and populate the workspace, yet still be reported
// as a failure — the ECHILD race is one way that happens, but it is not
// the only one, so this cleanup path is tested independently of the race.

func TestPreExistingWorkspaceEntries(t *testing.T) {
	t.Run("nonexistent directory returns empty set", func(t *testing.T) {
		got := preExistingWorkspaceEntries("/nonexistent/path/12345")
		if len(got) != 0 {
			t.Errorf("expected empty set, got %v", got)
		}
	})

	t.Run("captures marker directories present before clone", func(t *testing.T) {
		tmpDir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(tmpDir, ".scion-volumes"), 0755); err != nil {
			t.Fatal(err)
		}
		got := preExistingWorkspaceEntries(tmpDir)
		if _, ok := got[".scion-volumes"]; !ok {
			t.Errorf("expected .scion-volumes to be captured, got %v", got)
		}
		if len(got) != 1 {
			t.Errorf("expected exactly 1 entry, got %v", got)
		}
	})
}

func TestCleanFailedCloneAttempt(t *testing.T) {
	t.Run("removes everything not in the pre-existing set", func(t *testing.T) {
		tmpDir := t.TempDir()
		// Pre-existing bind-mount marker, present before the clone attempt.
		if err := os.MkdirAll(filepath.Join(tmpDir, ".scion-volumes"), 0755); err != nil {
			t.Fatal(err)
		}
		preExisting := preExistingWorkspaceEntries(tmpDir)

		// Simulate what a clone attempt wrote: a real .git dir plus checked
		// out working-tree files (the leftover-files scenario from a git
		// step that succeeded but was reported as failed).
		if err := os.MkdirAll(filepath.Join(tmpDir, ".git"), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(tmpDir, "README.md"), []byte("hi"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(tmpDir, "pkg"), 0755); err != nil {
			t.Fatal(err)
		}

		cleanFailedCloneAttempt(tmpDir, preExisting)

		if !isWorkspaceEmpty(tmpDir) {
			entries, _ := os.ReadDir(tmpDir)
			names := make([]string, len(entries))
			for i, e := range entries {
				names[i] = e.Name()
			}
			t.Errorf("expected workspace to be cloneable again after cleanup, found: %v", names)
		}
		// The pre-existing marker must survive.
		if _, err := os.Stat(filepath.Join(tmpDir, ".scion-volumes")); err != nil {
			t.Errorf(".scion-volumes should have survived cleanup: %v", err)
		}
	})

	t.Run("never touches pre-existing content", func(t *testing.T) {
		tmpDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(tmpDir, "keep-me.txt"), []byte("pre-existing"), 0644); err != nil {
			t.Fatal(err)
		}
		preExisting := preExistingWorkspaceEntries(tmpDir)

		if err := os.MkdirAll(filepath.Join(tmpDir, ".git"), 0755); err != nil {
			t.Fatal(err)
		}

		cleanFailedCloneAttempt(tmpDir, preExisting)

		if _, err := os.Stat(filepath.Join(tmpDir, "keep-me.txt")); err != nil {
			t.Errorf("pre-existing content should never be removed by cleanup: %v", err)
		}
		if _, err := os.Stat(filepath.Join(tmpDir, ".git")); !os.IsNotExist(err) {
			t.Errorf("expected .git created by the attempt to be removed, stat err: %v", err)
		}
	})
}

// runGitForTest runs git in dir with the given args, failing the test on error.
func runGitForTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = filterHubEnv(os.Environ())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
}

// TestGitCloneWorkspace_LateFailureCleansUpWholeWorkspace_RetrySucceeds
// simulates exactly the scenario the review flagged: a git step (`git
// checkout`, populating the working tree) succeeds, but a later step in
// the same clone attempt fails — here we force the credential-helper config
// step to fail deterministically by pointing agentHome at a path whose
// parent doesn't exist, so `git config --file <agentHome>/.gitconfig ...`
// cannot create the file. This reproduces the shape of the reaper-race bug
// (a step reported as failed after real content was already written)
// without depending on the race itself being scheduled.
//
// It asserts the whole workspace — not just .git — is cleaned up, and that
// a subsequent retry (the same call, now with a valid agentHome) clones
// successfully into the now-empty workspace.
func TestGitCloneWorkspace_LateFailureCleansUpWholeWorkspace_RetrySucceeds(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	// A local, offline "origin" repo with one commit on main.
	originDir := t.TempDir()
	runGitForTest(t, originDir, "init", "-b", "main")
	runGitForTest(t, originDir, "config", "user.email", "test@example.com")
	runGitForTest(t, originDir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(originDir, "README.md"), []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}
	runGitForTest(t, originDir, "add", "README.md")
	runGitForTest(t, originDir, "commit", "-m", "init")

	workspacePath := t.TempDir()

	t.Setenv("SCION_GIT_CLONE_URL", "file://"+originDir)
	t.Setenv("SCION_GIT_BRANCH", "main")
	t.Setenv("SCION_GIT_DEPTH", "")
	t.Setenv("SCION_AGENT_NAME", "test-agent")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("SCION_WORKSPACE_PATH", workspacePath)
	t.Setenv("SCION_AGENT_BRANCH", "")

	// agentHome whose parent doesn't exist: the credential-helper config
	// step (which runs after the working tree is already checked out) will
	// fail to create <agentHome>/.gitconfig.
	badAgentHome := filepath.Join(t.TempDir(), "does-not-exist", "nested")

	err := gitCloneWorkspace(0, 0, badAgentHome, false)
	if err == nil {
		t.Fatal("expected gitCloneWorkspace to fail at the credential-helper config step")
	}
	if !strings.Contains(err.Error(), "credential helper") {
		t.Fatalf("expected failure at the credential-helper config step, got: %v", err)
	}

	// The whole attempt must be cleaned up — not just .git/ — so the
	// workspace is cloneable again. Before this fix, checked-out files from
	// the (successful) checkout step would remain, and isWorkspaceEmpty
	// would see them and skip cloning on retry forever.
	if !isWorkspaceEmpty(workspacePath) {
		entries, _ := os.ReadDir(workspacePath)
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("expected workspace to be fully cleaned up after failure, found: %v", names)
	}

	// Retry with a valid agentHome: must succeed and populate the workspace.
	goodAgentHome := t.TempDir()
	if err := gitCloneWorkspace(0, 0, goodAgentHome, false); err != nil {
		t.Fatalf("retry after cleanup failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspacePath, "README.md")); err != nil {
		t.Fatalf("expected README.md to exist after successful retry clone: %v", err)
	}
}

// TestRequirePrivilegeDropOrFail_EnforcedFailsClosed is the fail-closed
// case: an enforcing caller sets RequirePrivilegeDrop, and setupHostUser did
// not actually drop privileges (targetUID stayed 0). RunInit
// must refuse to start the harness rather than run it as root.
func TestRequirePrivilegeDropOrFail_EnforcedFailsClosed(t *testing.T) {
	err := requirePrivilegeDropOrFail(0, 0, true)
	if err == nil {
		t.Fatal("requirePrivilegeDropOrFail(0, 0, true) = nil, want an error — an enforcing caller must never start the harness as root")
	}
	if !errors.Is(err, errPrivilegeDropRequired) {
		t.Errorf("requirePrivilegeDropOrFail(0, 0, true) = %v, want errPrivilegeDropRequired", err)
	}
}

// TestRequirePrivilegeDropOrFail_EnforcedSucceedsWhenDropped confirms the
// gate does not fire when the drop actually happened (targetUID != 0) —
// the ordinary, successful case once the actor's capability set and
// SCION_HOST_UID/GID are both in place.
func TestRequirePrivilegeDropOrFail_EnforcedSucceedsWhenDropped(t *testing.T) {
	if err := requirePrivilegeDropOrFail(1000, 1000, true); err != nil {
		t.Errorf("requirePrivilegeDropOrFail(1000, 1000, true) = %v, want nil", err)
	}
}

// TestRequirePrivilegeDropOrFail_UnenforcedRootlessUnchanged is the
// unenforced control: RequirePrivilegeDrop is false (the plain `sciontool
// init` CLI entrypoint never sets it), so the generic rootless fallback
// (e.g. rootless Podman, targetUID legitimately staying 0) is not subject
// to this gate.
func TestRequirePrivilegeDropOrFail_UnenforcedRootlessUnchanged(t *testing.T) {
	if err := requirePrivilegeDropOrFail(0, 0, false); err != nil {
		t.Errorf("requirePrivilegeDropOrFail(0, 0, false) = %v, want nil (unenforced rootless fallback must be unaffected)", err)
	}
}

// TestRequirePrivilegeDropOrFail_EnforcedRefusesRootGID proves the gid
// clamp: in enforced mode a non-root UID paired with a root (0) GID must be
// refused, because the supervisor's and manager's credential drop both use
// UID>0 && GID>0 and would otherwise skip the drop entirely (full root). A
// uid-only predicate would let exactly this pair through.
func TestRequirePrivilegeDropOrFail_EnforcedRefusesRootGID(t *testing.T) {
	err := requirePrivilegeDropOrFail(1000, 0, true)
	if !errors.Is(err, errPrivilegeDropRequired) {
		t.Fatalf("requirePrivilegeDropOrFail(1000, 0, true) = %v, want errPrivilegeDropRequired", err)
	}
	if err := requirePrivilegeDropOrFail(1000, 0, false); err != nil {
		t.Errorf("requirePrivilegeDropOrFail(1000, 0, false) = %v, want nil (non-enforced mode is unaffected)", err)
	}
}

// TestRequirePrivilegeDropOrFail_EnforcedRefusesRootUIDWithNonRootGID is
// the other half of the gid-clamp predicate: UID 0 paired with a non-root GID must
// still be refused in enforced mode (the gid clamp must not replace the
// original uid check).
func TestRequirePrivilegeDropOrFail_EnforcedRefusesRootUIDWithNonRootGID(t *testing.T) {
	if err := requirePrivilegeDropOrFail(0, 1000, true); !errors.Is(err, errPrivilegeDropRequired) {
		t.Fatalf("requirePrivilegeDropOrFail(0, 1000, true) = %v, want errPrivilegeDropRequired", err)
	}
}

// TestGitCloneWorkspace_FailedCloneKeepsWorkspaceDir covers a workspace
// that is the root of a mount, as on Kubernetes where /workspace is a
// volume mount: the directory itself cannot be removed or replaced. A
// failed clone must clean up only the directory's contents, leaving the
// same directory (same inode) and anything that was already in it, so a
// retry clones into it again.
func TestGitCloneWorkspace_FailedCloneKeepsWorkspaceDir(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	originDir := t.TempDir()
	runGitForTest(t, originDir, "init", "-b", "main")
	runGitForTest(t, originDir, "config", "user.email", "test@example.com")
	runGitForTest(t, originDir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(originDir, "README.md"), []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}
	runGitForTest(t, originDir, "add", "README.md")
	runGitForTest(t, originDir, "commit", "-m", "init")

	// The workspace directory exists before the clone, with a marker
	// directory in it, as the mount and the runtime leave it.
	workspacePath := filepath.Join(t.TempDir(), "workspace")
	if err := os.MkdirAll(filepath.Join(workspacePath, ".scion-volumes"), 0755); err != nil {
		t.Fatal(err)
	}
	// Keep the directory open, so a removed directory's inode stays in use
	// and cannot be handed to a new directory at the same path.
	handle, err := os.Open(workspacePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = handle.Close() })
	before, err := handle.Stat()
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("SCION_GIT_CLONE_URL", "file://"+originDir)
	t.Setenv("SCION_GIT_BRANCH", "main")
	t.Setenv("SCION_GIT_DEPTH", "")
	t.Setenv("SCION_AGENT_NAME", "test-agent")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("SCION_WORKSPACE_PATH", workspacePath)
	t.Setenv("SCION_AGENT_BRANCH", "")

	badAgentHome := filepath.Join(t.TempDir(), "does-not-exist", "nested")
	if err := gitCloneWorkspace(0, 0, badAgentHome, false); err == nil {
		t.Fatal("expected gitCloneWorkspace to fail")
	}

	after, err := os.Stat(workspacePath)
	if err != nil {
		t.Fatalf("workspace directory removed by the cleanup: %v", err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("workspace directory was replaced by the cleanup, want the same directory")
	}
	entries, err := os.ReadDir(workspacePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != ".scion-volumes" {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("want only the pre-existing .scion-volumes after cleanup, found: %v", names)
	}

	if err := gitCloneWorkspace(0, 0, t.TempDir(), false); err != nil {
		t.Fatalf("retry after cleanup failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspacePath, "README.md")); err != nil {
		t.Fatalf("expected README.md after the retry: %v", err)
	}
	retried, err := os.Stat(workspacePath)
	if err != nil || !os.SameFile(before, retried) {
		t.Fatalf("retry replaced the workspace directory (err %v)", err)
	}
}

// TestPostPreStartOwnershipFixup_ForwardsRequirePrivilegeDrop proves that,
// with euid stubbed to 0, the body of postPreStartOwnershipFixup forwards its own
// requirePrivilegeDrop, unchanged, to chownTreeRootOwned for every directory
// it fixes up. Hardcoding false there would silently fall back to the
// path-based walk with no hard-link guard in enforced mode.
func TestPostPreStartOwnershipFixup_ForwardsRequirePrivilegeDrop(t *testing.T) {
	for _, want := range []bool{true, false} {
		t.Run(map[bool]string{true: "enforced", false: "non-enforced"}[want], func(t *testing.T) {
			origGeteuid, origChown := postPreStartGeteuid, runChownTreeRootOwned
			t.Cleanup(func() { postPreStartGeteuid, runChownTreeRootOwned = origGeteuid, origChown })

			workspace := t.TempDir()
			agentHome := t.TempDir()
			t.Setenv("SCION_WORKSPACE_PATH", workspace)

			type call struct {
				dir      string
				uid, gid int
				rpd      bool
			}
			var calls []call
			postPreStartGeteuid = func() int { return 0 }
			runChownTreeRootOwned = func(root string, uid, gid int, requirePrivilegeDrop bool) (int, int, error) {
				calls = append(calls, call{root, uid, gid, requirePrivilegeDrop})
				return 0, 0, nil
			}

			postPreStartOwnershipFixup(1000, 1001, agentHome, want)

			wantCalls := []call{{workspace, 1000, 1001, want}, {agentHome, 1000, 1001, want}}
			if !reflect.DeepEqual(calls, wantCalls) {
				t.Fatalf("chownTreeRootOwned calls = %+v, want %+v", calls, wantCalls)
			}
		})
	}
}

// TestSetupHostUser_ForwardsRequirePrivilegeDrop proves that, with getuid,
// CAP_SETUID and the UID-map check stubbed to "root, capable, mapped",
// setupHostUser reaches
// adjustScionUser and forward its own requirePrivilegeDrop to it unchanged,
// along with the parsed SCION_HOST_UID/GID.
func TestSetupHostUser_ForwardsRequirePrivilegeDrop(t *testing.T) {
	for _, want := range []bool{true, false} {
		t.Run(map[bool]string{true: "enforced", false: "non-enforced"}[want], func(t *testing.T) {
			origGetuid, origCap, origMapped, origAdjust := setupHostUserGetuid, setupHostUserHasCapSetUID, setupHostUserIsUIDMapped, runAdjustScionUser
			t.Cleanup(func() {
				setupHostUserGetuid, setupHostUserHasCapSetUID, setupHostUserIsUIDMapped, runAdjustScionUser = origGetuid, origCap, origMapped, origAdjust
			})
			t.Setenv("SCION_HOST_UID", "1234")
			t.Setenv("SCION_HOST_GID", "5678")
			t.Setenv("SCION_KEEPID_UID", "")

			setupHostUserGetuid = func() int { return 0 }
			setupHostUserHasCapSetUID = func() bool { return true }
			setupHostUserIsUIDMapped = func(int) bool { return true }
			called := 0
			var gotUID, gotGID int
			var gotHostUID, gotHostGID string
			var gotRPD bool
			runAdjustScionUser = func(uid, gid int, hostUID, hostGID string, requirePrivilegeDrop bool) (int, int, bool) {
				called++
				gotUID, gotGID, gotHostUID, gotHostGID, gotRPD = uid, gid, hostUID, hostGID, requirePrivilegeDrop
				return uid, gid, false
			}

			uid, gid, rootless := setupHostUser(want)

			if called != 1 {
				t.Fatalf("adjustScionUser called %d times, want 1", called)
			}
			if gotRPD != want {
				t.Errorf("adjustScionUser got requirePrivilegeDrop=%v, want %v", gotRPD, want)
			}
			if gotUID != 1234 || gotGID != 5678 || gotHostUID != "1234" || gotHostGID != "5678" {
				t.Errorf("adjustScionUser got (%d, %d, %q, %q), want (1234, 5678, \"1234\", \"5678\")", gotUID, gotGID, gotHostUID, gotHostGID)
			}
			if uid != 1234 || gid != 5678 || rootless {
				t.Errorf("setupHostUser = (%d, %d, %v), want adjustScionUser's (1234, 5678, false)", uid, gid, rootless)
			}
		})
	}
}

// TestSetupHostUser_RefusesUint32OverflowAndSentinelIDs proves setupHostUser
// closes the numeric fail-open where SCION_HOST_UID/GID values at or past
// 2^32 (or the 2^32-1 sentinel) pass every "uid > 0" (Go int) guard
// downstream and only fail once cast to uint32 for syscall.Credential,
// where they silently wrap around to 0 (root). setupHostUser routes both
// through rootexec.ValidWorkloadID at the parse site, so they are refused
// here instead of ever reaching adjustScionUser at all — the same "skip
// user setup, continue as root-eligible for DecideExecAsRoot to gate"
// outcome an unparseable value produces.
func TestSetupHostUser_RefusesUint32OverflowAndSentinelIDs(t *testing.T) {
	tests := []struct {
		name           string
		hostUID        string
		hostGID        string
		wantAdjustCall bool
	}{
		{"2^32 uid overflows uint32", "4294967296", "1000", false},
		{"2^32 gid overflows uint32", "1000", "4294967296", false},
		{"2^32-1 uid sentinel refused", "4294967295", "1000", false},
		{"2^32-1 gid sentinel refused", "1000", "4294967295", false},
		{"ordinary uid/gid pass", "1000", "1000", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			origGetuid, origCap, origMapped, origAdjust := setupHostUserGetuid, setupHostUserHasCapSetUID, setupHostUserIsUIDMapped, runAdjustScionUser
			t.Cleanup(func() {
				setupHostUserGetuid, setupHostUserHasCapSetUID, setupHostUserIsUIDMapped, runAdjustScionUser = origGetuid, origCap, origMapped, origAdjust
			})
			t.Setenv("SCION_HOST_UID", tt.hostUID)
			t.Setenv("SCION_HOST_GID", tt.hostGID)
			t.Setenv("SCION_KEEPID_UID", "")

			setupHostUserGetuid = func() int { return 0 }
			setupHostUserHasCapSetUID = func() bool { return true }
			setupHostUserIsUIDMapped = func(int) bool { return true }
			called := false
			runAdjustScionUser = func(uid, gid int, hostUID, hostGID string, requirePrivilegeDrop bool) (int, int, bool) {
				called = true
				return uid, gid, false
			}

			uid, gid, _ := setupHostUser(false)

			if called != tt.wantAdjustCall {
				t.Errorf("adjustScionUser called = %v, want %v", called, tt.wantAdjustCall)
			}
			if !tt.wantAdjustCall {
				if uid != 0 || gid != 0 {
					t.Errorf("setupHostUser = (%d, %d), want (0, 0): refused input must never reach a Credential as a wrapped uid/gid", uid, gid)
				}
			}
		})
	}
}

// TestSetupHostUser_ZeroUIDGIDModeGated proves SCION_HOST_UID/GID=0 is
// refused under RequirePrivilegeDrop (the whole point of the mode is to
// never regain root) and accepted outside it, proceeding as root when
// privilege drop is optional. The uid-only and gid-only enforced cases pin
// that either field being 0 trips the refusal on its own — not just the
// case where both happen to be 0 together, which a buggy comparison
// (e.g. uid == gid instead of uid == 0) could still pass via the
// "0 == 0" zero value.
func TestSetupHostUser_ZeroUIDGIDModeGated(t *testing.T) {
	tests := []struct {
		name                 string
		hostUID              string
		hostGID              string
		requirePrivilegeDrop bool
		wantAdjustCall       bool
	}{
		{"enforced: zero uid and gid refused", "0", "0", true, false},
		{"enforced: zero uid only refused", "0", "1000", true, false},
		{"enforced: zero gid only refused", "1000", "0", true, false},
		{"non-enforced: zero keeps base behavior", "0", "0", false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			origGetuid, origCap, origMapped, origAdjust := setupHostUserGetuid, setupHostUserHasCapSetUID, setupHostUserIsUIDMapped, runAdjustScionUser
			t.Cleanup(func() {
				setupHostUserGetuid, setupHostUserHasCapSetUID, setupHostUserIsUIDMapped, runAdjustScionUser = origGetuid, origCap, origMapped, origAdjust
			})
			t.Setenv("SCION_HOST_UID", tt.hostUID)
			t.Setenv("SCION_HOST_GID", tt.hostGID)
			t.Setenv("SCION_KEEPID_UID", "")

			setupHostUserGetuid = func() int { return 0 }
			setupHostUserHasCapSetUID = func() bool { return true }
			setupHostUserIsUIDMapped = func(int) bool { return true }
			called := false
			runAdjustScionUser = func(uid, gid int, hostUID, hostGID string, requirePrivilegeDrop bool) (int, int, bool) {
				called = true
				return uid, gid, false
			}

			setupHostUser(tt.requirePrivilegeDrop)

			if called != tt.wantAdjustCall {
				t.Errorf("adjustScionUser called = %v, want %v", called, tt.wantAdjustCall)
			}
		})
	}
}

// TestRunServicesStart_DefaultForwardsRequirePrivilegeDrop exercises the
// DEFAULT runServicesStart body, which the RunInit threading test replaces
// with a stub: with requirePrivilegeDrop
// true and <name>.stdout.log pre-planted as a hard link to a victim file,
// the service must be refused (so the default body forwarded the flag to
// Manager.Start), and the victim's content must be unchanged.
func TestRunServicesStart_DefaultForwardsRequirePrivilegeDrop(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	setTestLogPath(t, filepath.Join(home, "agent.log"))
	logDir := filepath.Join(home, ".scion", "services", "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(home, "victim")
	if err := os.WriteFile(victim, []byte("v"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(victim, filepath.Join(logDir, "svc.stdout.log")); err != nil {
		t.Fatal(err)
	}

	m := services.New(5 * time.Second)
	err := runServicesStart(context.Background(), m, []api.ServiceSpec{{Name: "svc", Command: []string{"sh", "-c", "echo PWNED"}}}, 0, 0, "", true)
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = m.Shutdown(ctx)
	}()
	if err == nil || !strings.Contains(err.Error(), "hard-linked") {
		t.Errorf("default runServicesStart err = %v, want the hard-linked log refusal (requirePrivilegeDrop must reach Manager.Start)", err)
	}
	got, rerr := os.ReadFile(victim)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(got) != "v" {
		t.Errorf("victim content = %q, want %q (unchanged)", got, "v")
	}
}

// TestHarnessSupervisorConfig pins harnessSupervisorConfig's mapping from
// its inputs to supervisor.Config: every field must come through unchanged,
// and WorkingDir in particular must be copied from opts.WorkingDir when set
// and be "" when it is not (the value every caller except substrate-serve's
// InitRunner passes, and what docker/k8s depend on for byte-identical
// behaviour).
func TestHarnessSupervisorConfig(t *testing.T) {
	const gracePeriod = 7 * time.Second
	envOverlay := map[string]string{"FOO": "bar"}
	secretOverrides := map[string]string{"SECRET": "shh"}

	tests := []struct {
		name             string
		opts             InitRunOptions
		want             string // expected WorkingDir
		wantPrivDropDrop bool
	}{
		{name: "WorkingDir set is copied through", opts: InitRunOptions{WorkingDir: "/workspace"}, want: "/workspace"},
		{name: "WorkingDir unset is empty", opts: InitRunOptions{}, want: ""},
		{name: "RequirePrivilegeDrop is copied through", opts: InitRunOptions{RequirePrivilegeDrop: true}, want: "", wantPrivDropDrop: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := harnessSupervisorConfig(tt.opts, gracePeriod, 1000, 1000, false, envOverlay, "enabled", secretOverrides)
			want := supervisor.Config{
				GracePeriod:           gracePeriod,
				UID:                   1000,
				GID:                   1000,
				Username:              "scion",
				Rootless:              false,
				EnvOverlay:            envOverlay,
				NativeTelemetryPolicy: "enabled",
				SecretOverrides:       secretOverrides,
				WorkingDir:            tt.want,
				RequirePrivilegeDrop:  tt.wantPrivDropDrop,
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("harnessSupervisorConfig() = %+v, want %+v", got, want)
			}
		})
	}
}

// TestHarnessSupervisorConfig_RequirePrivilegeDropIgnoresRootless pins that
// an enforced RequirePrivilegeDrop threads through to supervisor.Config
// unconditionally — a rootless setup-host-user result must not silently
// clear it. Rootless and RequirePrivilegeDrop are independent signals to
// the supervisor: the former describes the host UID-mapping state, the
// latter is the fail-closed enforcement flag from InitRunOptions.
func TestHarnessSupervisorConfig_RequirePrivilegeDropIgnoresRootless(t *testing.T) {
	got := harnessSupervisorConfig(InitRunOptions{RequirePrivilegeDrop: true}, 0, 0, 0, true, nil, "", nil)
	if !got.RequirePrivilegeDrop || !got.Rootless {
		t.Fatalf("enforced+rootless: RequirePrivilegeDrop=%v Rootless=%v, want both true", got.RequirePrivilegeDrop, got.Rootless)
	}
}

func TestResolveProjectHookPath(t *testing.T) {
	tests := []struct {
		name                 string
		agentHome            string
		requirePrivilegeDrop bool
		want                 string
	}{
		{
			name:                 "non-enforced mode stays under agentHome",
			agentHome:            "/home/scion",
			requirePrivilegeDrop: false,
			want:                 "/home/scion/.scion/hooks/pre-start.d/30-project-custom",
		},
		{
			name:                 "enforced mode redirects to hooks.EnforcedHooksDir, independent of agentHome",
			agentHome:            "/home/scion",
			requirePrivilegeDrop: true,
			want:                 hooks.EnforcedHooksDir + "/pre-start.d/30-project-custom",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveProjectHookPath(tt.agentHome, tt.requirePrivilegeDrop); got != tt.want {
				t.Errorf("resolveProjectHookPath(%q, %v) = %q, want %q", tt.agentHome, tt.requirePrivilegeDrop, got, tt.want)
			}
		})
	}
}

// TestBlockClaudeDebugSymlink_NonEnforced_KeepsPathBasedBehaviour proves
// non-substrate runtimes keep plain, path-based behavior: a pre-existing
// symlink at debugDir is followed (os.MkdirAll short-circuits, os.Chmod
// chmods the target), the same as a plain inline os.MkdirAll+os.Chmod would
// do. This is deliberate — see blockClaudeDebugSymlink's doc comment for why
// a legitimate non-substrate setup may symlink .claude itself (e.g. to a
// mounted volume), and refusing that would break it.
func TestBlockClaudeDebugSymlink_NonEnforced_KeepsPathBasedBehaviour(t *testing.T) {
	tmpHome := t.TempDir()
	victim := t.TempDir()
	if err := os.Chmod(victim, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(tmpHome, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	debugDir := filepath.Join(tmpHome, ".claude", "debug")
	if err := os.Symlink(victim, debugDir); err != nil {
		t.Fatal(err)
	}

	blockClaudeDebugSymlink(debugDir, false)

	info, err := os.Stat(victim)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o555 {
		t.Errorf("non-enforced mode: victim mode = %o, want 0555 (historical behaviour: chmod follows the symlink)", got)
	}
}

// TestBlockClaudeDebugSymlink_Enforced_RefusesSymlinkAndLeavesVictimUnchanged
// is the core regression test: a symlink planted at ~/.claude/debug before
// this runs, pointing at a victim directory, must never be chmod'd through —
// deterministic, no race required, since the symlink already exists when
// this function runs (matching the realistic case: a pre-start hook or
// sidecar plants it before line 836 in RunInit is reached).
func TestBlockClaudeDebugSymlink_Enforced_RefusesSymlinkAndLeavesVictimUnchanged(t *testing.T) {
	tmpHome := t.TempDir()
	victim := t.TempDir()
	if err := os.Chmod(victim, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(tmpHome, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	debugDir := filepath.Join(tmpHome, ".claude", "debug")
	if err := os.Symlink(victim, debugDir); err != nil {
		t.Fatal(err)
	}

	blockClaudeDebugSymlink(debugDir, true)

	info, err := os.Stat(victim)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Errorf("enforced mode must fail closed: victim mode = %o, want unchanged 0700", got)
	}
	linkInfo, err := os.Lstat(debugDir)
	if err != nil {
		t.Fatal(err)
	}
	if linkInfo.Mode()&os.ModeSymlink == 0 {
		t.Error("expected debugDir to still be the symlink the test planted — nothing should have removed or replaced it")
	}
}

// TestBlockClaudeDebugSymlink_Enforced_CreatesAndChmodsRealDir proves the
// legitimate case still works under enforced mode: when debugDir doesn't
// exist yet (the common case, and $HOME/.claude may not exist yet either,
// since this runs before the harness itself starts), it gets created and
// chmod'd 0555 exactly as intended.
func TestBlockClaudeDebugSymlink_Enforced_CreatesAndChmodsRealDir(t *testing.T) {
	tmpHome := t.TempDir()
	debugDir := filepath.Join(tmpHome, ".claude", "debug")

	blockClaudeDebugSymlink(debugDir, true)

	info, err := os.Stat(debugDir)
	if err != nil {
		t.Fatalf("expected debugDir to be created: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o555 {
		t.Errorf("mode = %o, want 0555", got)
	}
}

// TestBlockClaudeDebugSymlink_Enforced_ChmodSurvivesSwapAfterEnsure is the
// core regression test for the swap-after-resolve race: a workload process that renames debugDir away and
// plants a symlink to a victim directory in its place, in the exact window
// between EnsureDirNoFollow returning its fd and the chmod that follows,
// must not have the chmod land on the victim. The chmod is fd-based
// (d.Chmod, not os.Chmod(debugDir)), so it stays bound to the original
// directory no matter what its entry in the parent becomes afterward.
func TestBlockClaudeDebugSymlink_Enforced_ChmodSurvivesSwapAfterEnsure(t *testing.T) {
	tmpHome := t.TempDir()
	victim := t.TempDir()
	if err := os.Chmod(victim, 0o700); err != nil {
		t.Fatal(err)
	}
	debugDir := filepath.Join(tmpHome, ".claude", "debug")
	movedDir := filepath.Join(tmpHome, ".claude", "debug.moved")

	blockClaudeDebugAfterEnsureForTest = func(dir string) {
		if err := os.Rename(dir, movedDir); err != nil {
			t.Errorf("swap: rename %s: %v", dir, err)
			return
		}
		if err := os.Symlink(victim, dir); err != nil {
			t.Errorf("swap: symlink %s -> %s: %v", dir, victim, err)
		}
	}
	t.Cleanup(func() { blockClaudeDebugAfterEnsureForTest = nil })

	blockClaudeDebugSymlink(debugDir, true)

	victimInfo, err := os.Stat(victim)
	if err != nil {
		t.Fatal(err)
	}
	if got := victimInfo.Mode().Perm(); got != 0o700 {
		t.Errorf("victim mode = %o, want unchanged 0700 — chmod followed the swapped-in symlink", got)
	}

	linkInfo, err := os.Lstat(debugDir)
	if err != nil {
		t.Fatal(err)
	}
	if linkInfo.Mode()&os.ModeSymlink == 0 {
		t.Error("expected debugDir to still be the symlink the swap planted")
	}

	movedInfo, err := os.Stat(movedDir)
	if err != nil {
		t.Fatalf("stat moved-away original debugDir: %v", err)
	}
	if got := movedInfo.Mode().Perm(); got != 0o555 {
		t.Errorf("moved-away original debugDir mode = %o, want 0555 — the held fd's chmod should have landed here", got)
	}
}

// TestCleanGcloudConfigForMetadata_NonEnforced_KeepsHistoricalBehaviour
// proves non-substrate runtimes are byte-identical: entries under gcloudDir
// (except the preserved ADC file) are removed via the historical
// os.ReadDir+os.RemoveAll path, including through a symlinked gcloudDir
// itself — a legitimate non-substrate setup may bind-mount or symlink
// ~/.config/gcloud, and refusing that would break it.
func TestCleanGcloudConfigForMetadata_NonEnforced_KeepsHistoricalBehaviour(t *testing.T) {
	real := t.TempDir()
	if err := os.WriteFile(filepath.Join(real, "credentials.db"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, gcloudConfigKeepFile), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	tmpHome := t.TempDir()
	gcloudDir := filepath.Join(tmpHome, "gcloud-link")
	if err := os.Symlink(real, gcloudDir); err != nil {
		t.Fatal(err)
	}

	cleanGcloudConfigForMetadata(gcloudDir, false)

	if _, err := os.Stat(filepath.Join(real, "credentials.db")); !os.IsNotExist(err) {
		t.Errorf("non-enforced mode: expected credentials.db to be removed through the symlink (historical behaviour), err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(real, gcloudConfigKeepFile)); err != nil {
		t.Errorf("expected %s to be preserved: %v", gcloudConfigKeepFile, err)
	}
}

// TestCleanGcloudConfigForMetadata_Enforced_RefusesSymlinkAndLeavesVictimUnchanged
// is the core deterministic regression test: gcloudDir itself is a symlink
// to a victim directory (planted before this runs, so no race is required
// to demonstrate the class), and enforced mode must refuse it outright
// rather than enumerating/deleting through it.
func TestCleanGcloudConfigForMetadata_Enforced_RefusesSymlinkAndLeavesVictimUnchanged(t *testing.T) {
	victim := t.TempDir()
	sentinel := filepath.Join(victim, "sentinel")
	if err := os.WriteFile(sentinel, []byte("do-not-delete"), 0o600); err != nil {
		t.Fatal(err)
	}

	tmpHome := t.TempDir()
	gcloudDir := filepath.Join(tmpHome, ".config", "gcloud")
	if err := os.MkdirAll(filepath.Dir(gcloudDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, gcloudDir); err != nil {
		t.Fatal(err)
	}

	cleanGcloudConfigForMetadata(gcloudDir, true)

	entries, err := os.ReadDir(victim)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "sentinel" {
		t.Errorf("victim directory contents changed: %v", entries)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Errorf("sentinel must survive: %v", err)
	}
	linkInfo, err := os.Lstat(gcloudDir)
	if err != nil {
		t.Fatal(err)
	}
	if linkInfo.Mode()&os.ModeSymlink == 0 {
		t.Error("expected gcloudDir to still be the symlink the test planted")
	}
}

// TestCleanGcloudConfigForMetadata_Enforced_CleansRealDir proves the
// legitimate case still works under enforced mode: a real gcloudDir has its
// entries removed except the preserved ADC file.
func TestCleanGcloudConfigForMetadata_Enforced_CleansRealDir(t *testing.T) {
	tmpHome := t.TempDir()
	gcloudDir := filepath.Join(tmpHome, ".config", "gcloud")
	if err := os.MkdirAll(gcloudDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gcloudDir, "credentials.db"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gcloudDir, gcloudConfigKeepFile), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	cleanGcloudConfigForMetadata(gcloudDir, true)

	if _, err := os.Stat(filepath.Join(gcloudDir, "credentials.db")); !os.IsNotExist(err) {
		t.Errorf("expected credentials.db to be removed, err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(gcloudDir, gcloudConfigKeepFile)); err != nil {
		t.Errorf("expected %s to be preserved: %v", gcloudConfigKeepFile, err)
	}
}

// TestCleanGcloudConfigForMetadata_Enforced_MissingDirIsNoop proves the
// "nothing to clean" case is unaffected by the enforced-mode rewrite, and
// specifically that it is NOT misreported as a refused symlink: a missing
// directory must never produce an Error log line, only a genuinely refused
// symlink should. errors.Is(err, os.ErrNotExist) is what tells the two
// apart — os.IsNotExist would not (see readServicesYAML/OpenDirNoFollow's
// own doc comments for the same distinction), so this pins the log-level
// behaviour a regression back to os.IsNotExist would silently break.
func TestCleanGcloudConfigForMetadata_Enforced_MissingDirIsNoop(t *testing.T) {
	tmpHome := t.TempDir()
	logPath := filepath.Join(tmpHome, "capture.log")
	setTestLogPath(t, logPath)
	log.SetQuiet(true)
	t.Cleanup(func() { log.SetQuiet(false) })

	gcloudDir := filepath.Join(tmpHome, ".config", "gcloud")
	cleanGcloudConfigForMetadata(gcloudDir, true)

	// No log call at all is the expected outcome for a genuinely missing
	// directory, so the log file may not even exist yet.
	data, err := os.ReadFile(logPath)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("reading captured log: %v", err)
	}
	if strings.Contains(string(data), "ERROR") {
		t.Errorf("a missing gcloud dir must not log an Error line, got: %s", data)
	}
}

// TestCleanGcloudConfigForMetadata_Enforced_SymlinkLogsErrorLine is the
// discriminating half of the test above: a genuinely refused symlink DOES
// log an Error line, proving the missing-dir test isn't just vacuously
// passing because nothing is ever logged at all.
func TestCleanGcloudConfigForMetadata_Enforced_SymlinkLogsErrorLine(t *testing.T) {
	tmpHome := t.TempDir()
	logPath := filepath.Join(tmpHome, "capture.log")
	setTestLogPath(t, logPath)
	log.SetQuiet(true)
	t.Cleanup(func() { log.SetQuiet(false) })

	victim := t.TempDir()
	gcloudDir := filepath.Join(tmpHome, ".config", "gcloud")
	if err := os.MkdirAll(filepath.Dir(gcloudDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, gcloudDir); err != nil {
		t.Fatal(err)
	}

	cleanGcloudConfigForMetadata(gcloudDir, true)

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("reading captured log: %v", err)
	}
	if !strings.Contains(string(data), "ERROR") {
		t.Errorf("expected a refused symlink to log an Error line, got: %s", data)
	}
}

// TestChownTreeRootOwned_DirectCall is a thin call-site test proving
// chownTreeRootOwned, in enforced mode, delegates to the shared
// dirfd.ChownTreeNoFollow walk with the root-owned-only filter — the deeper
// symlink-swap race itself is covered once, thoroughly, at the dirfd level
// (TestChownTreeNoFollow_SurvivesIntermediateDirSwapMidWalk).
func TestChownTreeRootOwned_DirectCall(t *testing.T) {
	origFilter := chownTreeRootOwnedFilter
	t.Cleanup(func() { chownTreeRootOwnedFilter = origFilter })
	chownTreeRootOwnedFilter = func(uint32) bool { return true } // simulate root-owned

	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "a"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	uid, gid := os.Getuid(), os.Getgid()
	walked, changed, err := chownTreeRootOwned(home, uid, gid, true)
	if err != nil {
		t.Fatalf("chownTreeRootOwned: %v", err)
	}
	if walked != 2 { // root dir + "a"
		t.Errorf("walked = %d, want 2", walked)
	}
	if changed != 2 {
		t.Errorf("changed = %d, want 2", changed)
	}
}

// TestChownTreeRootOwned_NonEnforced_UsesPathBasedWalk proves the non-
// enforced branch drives the SAME chownTreeRootOwnedFilter decision through
// the historical filepath.WalkDir+os.Lchown implementation, not the
// fd-based one — both walks visit and chown the same entries here.
func TestChownTreeRootOwned_NonEnforced_UsesPathBasedWalk(t *testing.T) {
	origFilter := chownTreeRootOwnedFilter
	t.Cleanup(func() { chownTreeRootOwnedFilter = origFilter })
	chownTreeRootOwnedFilter = func(uint32) bool { return true }

	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "a"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	uid, gid := os.Getuid(), os.Getgid()
	walked, changed, err := chownTreeRootOwned(home, uid, gid, false)
	if err != nil {
		t.Fatalf("chownTreeRootOwned: %v", err)
	}
	if walked != 2 {
		t.Errorf("walked = %d, want 2", walked)
	}
	if changed != 2 {
		t.Errorf("changed = %d, want 2", changed)
	}
}

// TestChownTreeRootOwned_MissingRootIsSilentNoop proves that a missing
// root is a silent no-op (nil, 0, 0) on both branches, restoring the
// historical filepath.WalkDir contract (WalkDir passes the root's own lstat
// error to the callback, which returns nil) rather than surfacing as an
// error.
func TestChownTreeRootOwned_MissingRootIsSilentNoop(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")

	for _, enforced := range []bool{false, true} {
		walked, changed, err := chownTreeRootOwned(missing, os.Getuid(), os.Getgid(), enforced)
		if err != nil {
			t.Errorf("enforced=%v: err = %v, want nil", enforced, err)
		}
		if walked != 0 || changed != 0 {
			t.Errorf("enforced=%v: walked=%d changed=%d, want 0, 0", enforced, walked, changed)
		}
	}
}

// TestChownTreeRootOwned_NonEnforced_FollowsAncestorSymlink proves the
// runtime gating: on non-enforced runtimes, an ancestor-path symlink is followed
// (the historical filepath.WalkDir behaviour), not refused — a legitimate
// non-substrate setup may symlink an ancestor of the walked root (e.g. from
// a bind-mounted host path), and refusing that would break it.
func TestChownTreeRootOwned_NonEnforced_FollowsAncestorSymlink(t *testing.T) {
	origFilter := chownTreeRootOwnedFilter
	t.Cleanup(func() { chownTreeRootOwnedFilter = origFilter })
	chownTreeRootOwnedFilter = func(uint32) bool { return true }

	real := t.TempDir()
	actual := filepath.Join(real, "actual")
	if err := os.Mkdir(actual, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(actual, "a"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	link := filepath.Join(parent, "home-link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	// "home-link" is an ANCESTOR (intermediate) component of root, not
	// root's own leaf — "actual" is the real, walked directory.
	root := filepath.Join(link, "actual")

	uid, gid := os.Getuid(), os.Getgid()
	walked, changed, err := chownTreeRootOwned(root, uid, gid, false)
	if err != nil {
		t.Fatalf("chownTreeRootOwned: %v", err)
	}
	if walked != 2 || changed != 2 {
		t.Errorf("walked=%d changed=%d, want 2, 2 — expected the ancestor symlink to be followed", walked, changed)
	}
}

// TestChownTreeRootOwned_Enforced_RefusesAncestorSymlink proves the other
// half of the same runtime gating: on substrate (enforced), the same ancestor-path symlink is
// refused rather than followed, so the whole fixup for that root is skipped
// (nothing chowned) instead of silently descending through workload-
// controlled redirection.
func TestChownTreeRootOwned_Enforced_RefusesAncestorSymlink(t *testing.T) {
	origFilter := chownTreeRootOwnedFilter
	t.Cleanup(func() { chownTreeRootOwnedFilter = origFilter })
	chownTreeRootOwnedFilter = func(uint32) bool { return true }

	real := t.TempDir()
	actual := filepath.Join(real, "actual")
	if err := os.Mkdir(actual, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(actual, "a"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	link := filepath.Join(parent, "home-link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(link, "actual")

	uid, gid := os.Getuid(), os.Getgid()
	walked, changed, err := chownTreeRootOwned(root, uid, gid, true)
	if err == nil {
		t.Fatal("expected an error refusing the ancestor symlink in enforced mode")
	}
	if walked != 0 || changed != 0 {
		t.Errorf("walked=%d changed=%d, want 0, 0", walked, changed)
	}
}

// TestChownTreeRootOwned_Enforced_SkipsHardlinkedFile is the core
// regression test: the enforced branch chowns specifically root-owned
// entries, so a pre-planted hard link to a root-owned file is exactly what
// it would hand over if the hard-link guard were ever disabled for this
// call site. Forces the root-owned filter, creates a hard-linked pair, and
// asserts the target is left untouched.
func TestChownTreeRootOwned_Enforced_SkipsHardlinkedFile(t *testing.T) {
	origFilter := chownTreeRootOwnedFilter
	t.Cleanup(func() { chownTreeRootOwnedFilter = origFilter })
	chownTreeRootOwnedFilter = func(uint32) bool { return true }

	home := t.TempDir()
	target := filepath.Join(home, "target")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(target, filepath.Join(home, "hardlink")); err != nil {
		t.Fatal(err)
	}
	targetBefore := ownedTargetCtime(t, target)
	time.Sleep(15 * time.Millisecond)

	uid, gid := os.Getuid(), os.Getgid()
	if _, _, err := chownTreeRootOwned(home, uid, gid, true); err != nil {
		t.Fatalf("chownTreeRootOwned: %v", err)
	}
	if ownedTargetCtime(t, target) != targetBefore {
		t.Error("hard-linked target was chowned despite the enforced hard-link guard")
	}
}

func ownedTargetCtime(t *testing.T, path string) syscall.Timespec {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	return statCtime(info.Sys().(*syscall.Stat_t))
}

func TestIsRootOwned(t *testing.T) {
	if !isRootOwned(0) {
		t.Error("isRootOwned(0) = false, want true")
	}
	if isRootOwned(1000) {
		t.Error("isRootOwned(1000) = true, want false")
	}
}

// scionEnvFileCtime returns the ctime of $HOME/.scion for an owner-gating
// test below, after writeEnvFile has already run once to create it.
func scionEnvFileCtime(t *testing.T, tmpHome string) syscall.Timespec {
	t.Helper()
	info, err := os.Stat(filepath.Join(tmpHome, ".scion"))
	if err != nil {
		t.Fatalf("stat .scion: %v", err)
	}
	return statCtime(info.Sys().(*syscall.Stat_t))
}

// TestWriteEnvFile_ChownGating covers all three owner states writeEnvFile's
// fstat-based owner gate distinguishes: root-owned (uid 0) triggers the
// chown; anything else — the
// target uid itself (the normal steady-state case, once a previous run's
// chown already landed) or any other unexpected uid — is left alone. Each
// case drives scionDirOwnerUID directly rather than needing a real
// differently-owned directory (this test process cannot create one without
// real root).
//
// The "before" ctime is captured via writeEnvFileAfterWriteForTest, firing
// right after the env-file write/rename (which itself bumps .scion's own
// ctime, since that changes the directory's entries) and right before the
// chown gate runs — not before the whole call — so the content-write's own
// ctime bump doesn't get misread as evidence the chown ran.
func TestWriteEnvFile_ChownGating(t *testing.T) {
	tests := []struct {
		name      string
		reportUID uint32
		wantChown bool
	}{
		{name: "root-owned (uid 0) triggers chown", reportUID: 0, wantChown: true},
		{name: "already owned by target uid: no-op skip", reportUID: uint32(os.Getuid()), wantChown: false},
		{name: "unexpected other owner: refuse/skip", reportUID: 424242, wantChown: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpHome := t.TempDir()
			t.Setenv("SCION_AGENT_NAME", "test-agent")

			origOwnerUID := scionDirOwnerUID
			t.Cleanup(func() { scionDirOwnerUID = origOwnerUID })
			scionDirOwnerUID = func(int) (uint32, error) { return tt.reportUID, nil }

			var before syscall.Timespec
			writeEnvFileAfterWriteForTest = func(dir string) {
				before = scionEnvFileCtime(t, tmpHome)
				time.Sleep(15 * time.Millisecond)
			}
			t.Cleanup(func() { writeEnvFileAfterWriteForTest = nil })

			writeEnvFile(tmpHome, os.Getuid(), os.Getgid())
			after := scionEnvFileCtime(t, tmpHome)

			gotChown := after != before
			if gotChown != tt.wantChown {
				t.Errorf("chown occurred = %v, want %v", gotChown, tt.wantChown)
			}
		})
	}
}

// TestReadServicesYAML_NonEnforced_FollowsSymlink proves non-substrate
// runtimes are byte-identical to the historical os.ReadFile: a symlinked
// services config is followed and its content returned.
func TestReadServicesYAML_NonEnforced_FollowsSymlink(t *testing.T) {
	tmpHome := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpHome, ".scion"), 0o755); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(tmpHome, "real-services.yaml")
	content := []byte("- name: foo\n  command: [\"true\"]\n")
	if err := os.WriteFile(real, content, 0o644); err != nil {
		t.Fatal(err)
	}
	servicesPath := filepath.Join(tmpHome, ".scion", "scion-services.yaml")
	if err := os.Symlink(real, servicesPath); err != nil {
		t.Fatal(err)
	}

	data, err := readServicesYAML(servicesPath, false)
	if err != nil {
		t.Fatalf("readServicesYAML: %v", err)
	}
	if string(data) != string(content) {
		t.Errorf("data = %q, want %q", data, content)
	}
}

// TestReadServicesYAML_Enforced_RefusesSymlink is the core regression test:
// a symlink planted at scion-services.yaml (deterministic — the workload
// can plant it any time before this is read) must never be read through in
// enforced mode.
func TestReadServicesYAML_Enforced_RefusesSymlink(t *testing.T) {
	tmpHome := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpHome, ".scion"), 0o755); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(tmpHome, "victim.yaml")
	if err := os.WriteFile(victim, []byte("do-not-read"), 0o600); err != nil {
		t.Fatal(err)
	}
	servicesPath := filepath.Join(tmpHome, ".scion", "scion-services.yaml")
	if err := os.Symlink(victim, servicesPath); err != nil {
		t.Fatal(err)
	}

	data, err := readServicesYAML(servicesPath, true)
	if err == nil {
		t.Fatalf("expected an error refusing the symlink, got data=%q", data)
	}
	if data != nil {
		t.Errorf("expected no data on refusal, got %q", data)
	}
	linkInfo, err := os.Lstat(servicesPath)
	if err != nil {
		t.Fatal(err)
	}
	if linkInfo.Mode()&os.ModeSymlink == 0 {
		t.Error("expected scion-services.yaml to still be the symlink the test planted")
	}
}

// TestReadServicesYAML_Enforced_ReadsRealFile proves the legitimate case
// still works: a real, single-link regular file is read normally in
// enforced mode.
func TestReadServicesYAML_Enforced_ReadsRealFile(t *testing.T) {
	tmpHome := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpHome, ".scion"), 0o755); err != nil {
		t.Fatal(err)
	}
	servicesPath := filepath.Join(tmpHome, ".scion", "scion-services.yaml")
	content := []byte("- name: foo\n  command: [\"true\"]\n")
	if err := os.WriteFile(servicesPath, content, 0o644); err != nil {
		t.Fatal(err)
	}

	data, err := readServicesYAML(servicesPath, true)
	if err != nil {
		t.Fatalf("readServicesYAML: %v", err)
	}
	if string(data) != string(content) {
		t.Errorf("data = %q, want %q", data, content)
	}
}

// TestReadServicesYAML_Enforced_MissingFileIsQuietError proves a missing
// file is reported as an ordinary error (matching os.ReadFile's contract,
// which the caller already treats as "no services to start") WITHOUT being
// logged as a refused symlink — captures real log output and asserts no
// ERROR line, not just err!=nil (a bare err!=nil check can't distinguish
// "quiet ENOENT" from "logged refusal", so it doesn't discriminate the
// errors.Is-vs-os.IsNotExist gate this function relies on).
func TestReadServicesYAML_Enforced_MissingFileIsQuietError(t *testing.T) {
	tmpHome := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpHome, ".scion"), 0o755); err != nil {
		t.Fatal(err)
	}
	servicesPath := filepath.Join(tmpHome, ".scion", "scion-services.yaml")

	logPath := filepath.Join(tmpHome, "capture.log")
	setTestLogPath(t, logPath)
	log.SetQuiet(true)
	t.Cleanup(func() { log.SetQuiet(false) })

	if _, err := readServicesYAML(servicesPath, true); err == nil {
		t.Fatal("expected an error for a missing file")
	}

	data, err := os.ReadFile(logPath)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("reading captured log: %v", err)
	}
	if strings.Contains(string(data), "ERROR") {
		t.Errorf("a missing services file must not log an Error line, got: %s", data)
	}
}

// TestReadServicesYAML_Enforced_RefusesHardlink proves the Nlink check
// is the only defence against a pre-planted hard link to a root-only file
// on the same filesystem — a workload process can hard-link to a file it
// does not own (hard-linking only needs write access to the directory the
// link is created in). Without it, root would read and parse that file's
// content as if it were the services config.
func TestReadServicesYAML_Enforced_RefusesHardlink(t *testing.T) {
	tmpHome := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpHome, ".scion"), 0o755); err != nil {
		t.Fatal(err)
	}
	servicesPath := filepath.Join(tmpHome, ".scion", "scion-services.yaml")
	victim := filepath.Join(tmpHome, "victim")
	if err := os.WriteFile(victim, []byte("secret: do-not-read"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(victim, servicesPath); err != nil {
		t.Fatal(err)
	}

	if data, err := readServicesYAML(servicesPath, true); err == nil {
		t.Errorf("hard-linked services file was read: %q", data)
	}
}

// TestReadServicesYAML_Enforced_RefusesAncestorSymlink proves the no-follow
// resolution applies to every component of the path, not just the leaf: a
// symlinked $HOME/.scion (an ancestor of the services file, not the file
// itself) must be refused, not followed.
func TestReadServicesYAML_Enforced_RefusesAncestorSymlink(t *testing.T) {
	tmpHome, real := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(real, "scion-services.yaml"), []byte("- name: x\n  command: [\"true\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	scionDir := filepath.Join(tmpHome, ".scion")
	if err := os.Symlink(real, scionDir); err != nil {
		t.Fatal(err)
	}

	servicesPath := filepath.Join(scionDir, "scion-services.yaml")
	if data, err := readServicesYAML(servicesPath, true); err == nil {
		t.Errorf("ancestor symlink (.scion) was followed: %q", data)
	}
}

// TestReadServicesYAML_Enforced_RefusesFifoWithoutHang proves BOTH the S_IFREG check (a FIFO must be refused, not read as if it were a
// regular file) and O_NONBLOCK (the refusal must not require a writer to
// ever show up — a backgrounded pre-start-hook child could hold a FIFO
// open at this exact path and never write to it, which would otherwise
// hang root's init before the harness ever starts). A reader is held open
// (as openLogNoFollow's own FIFO test does) so the O_NONBLOCK open itself
// succeeds instead of failing ENXIO — the point is that the SUBSEQUENT
// fstat/S_IFREG check refuses it, and that neither step ever blocks.
func TestReadServicesYAML_Enforced_RefusesFifoWithoutHang(t *testing.T) {
	tmpHome := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpHome, ".scion"), 0o755); err != nil {
		t.Fatal(err)
	}
	servicesPath := filepath.Join(tmpHome, ".scion", "scion-services.yaml")
	if err := syscall.Mkfifo(servicesPath, 0o600); err != nil {
		t.Fatal(err)
	}

	rfd, err := syscall.Open(servicesPath, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatalf("open reader: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Close(rfd) })

	done := make(chan error, 1)
	go func() {
		_, err := readServicesYAML(servicesPath, true)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("FIFO accepted as services file")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("readServicesYAML hung on a FIFO")
	}
}

// TestReadServicesYAML_Enforced_RefusesOverCapFile bounds the
// enforced-mode read so a workload-planted multi-GB regular file cannot
// make root's own init process read the whole thing into memory before the
// harness ever starts.
func TestReadServicesYAML_Enforced_RefusesOverCapFile(t *testing.T) {
	tmpHome := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpHome, ".scion"), 0o755); err != nil {
		t.Fatal(err)
	}
	servicesPath := filepath.Join(tmpHome, ".scion", "scion-services.yaml")
	oversized := bytes.Repeat([]byte("a"), servicesYAMLMaxBytes+1)
	if err := os.WriteFile(servicesPath, oversized, 0o600); err != nil {
		t.Fatal(err)
	}

	if data, err := readServicesYAML(servicesPath, true); err == nil {
		t.Errorf("expected an error for an over-cap file, got %d bytes", len(data))
	}
}

// TestReadServicesYAML_Enforced_ReadsAtCapFile proves the boundary itself
// still works: a file exactly at the cap is read successfully.
func TestReadServicesYAML_Enforced_ReadsAtCapFile(t *testing.T) {
	tmpHome := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpHome, ".scion"), 0o755); err != nil {
		t.Fatal(err)
	}
	servicesPath := filepath.Join(tmpHome, ".scion", "scion-services.yaml")
	atCap := bytes.Repeat([]byte("a"), servicesYAMLMaxBytes)
	if err := os.WriteFile(servicesPath, atCap, 0o600); err != nil {
		t.Fatal(err)
	}

	data, err := readServicesYAML(servicesPath, true)
	if err != nil {
		t.Fatalf("readServicesYAML: %v", err)
	}
	if len(data) != servicesYAMLMaxBytes {
		t.Errorf("len(data) = %d, want %d", len(data), servicesYAMLMaxBytes)
	}
}

// TestReadServicesYAML_Enforced_ReadsReadOnlyFile pins that the enforced
// leaf open is read-only: a 0444 scion-services.yaml must still be read.
// An O_RDWR open (which would also sidestep the FIFO-blocking O_NONBLOCK
// guard, since a read-write FIFO open never blocks) needs write permission
// and fails here. This discriminates only as non-root
// (root bypasses the permission check), so it skips as root.
func TestReadServicesYAML_Enforced_ReadsReadOnlyFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file permission checks; this property is only observable as non-root")
	}
	tmpHome := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpHome, ".scion"), 0o755); err != nil {
		t.Fatal(err)
	}
	servicesPath := filepath.Join(tmpHome, ".scion", "scion-services.yaml")
	if err := os.WriteFile(servicesPath, []byte("- name: chrome\n"), 0o444); err != nil {
		t.Fatal(err)
	}

	data, err := readServicesYAML(servicesPath, true)
	if err != nil {
		t.Fatalf("readServicesYAML on a read-only file: %v", err)
	}
	if string(data) != "- name: chrome\n" {
		t.Errorf("data = %q, want the file's content", data)
	}
}

// TestReadServicesYAML_NonEnforced_ReadsOverCapFile pins the claim that the
// 1 MiB cap is enforced-mode only: the non-enforced branch is an unbounded
// os.ReadFile, byte-identical to its previous behaviour, so a file over the
// cap is returned whole.
func TestReadServicesYAML_NonEnforced_ReadsOverCapFile(t *testing.T) {
	tmpHome := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpHome, ".scion"), 0o755); err != nil {
		t.Fatal(err)
	}
	servicesPath := filepath.Join(tmpHome, ".scion", "scion-services.yaml")
	overCap := bytes.Repeat([]byte("a"), servicesYAMLMaxBytes+1)
	if err := os.WriteFile(servicesPath, overCap, 0o600); err != nil {
		t.Fatal(err)
	}

	data, err := readServicesYAML(servicesPath, false)
	if err != nil {
		t.Fatalf("non-enforced readServicesYAML: %v", err)
	}
	if len(data) != servicesYAMLMaxBytes+1 {
		t.Errorf("len(data) = %d, want %d (non-enforced mode must not apply the cap)", len(data), servicesYAMLMaxBytes+1)
	}
}

// TestValidateServiceSpecs_DropsInvalidNamesKeepsValidOnes proves the
// authoritative parse-time gate: an invalid Name is dropped from the list
// (logged, never a raw workload-chosen string), while every valid entry —
// regardless of position — passes through unchanged.
func TestValidateServiceSpecs_DropsInvalidNamesKeepsValidOnes(t *testing.T) {
	specs := []api.ServiceSpec{
		{Name: "chrome", Command: []string{"true"}},
		{Name: "../escape", Command: []string{"true"}},
		{Name: "vnc", Command: []string{"true"}},
	}

	got := validateServiceSpecs(specs)

	var names []string
	for _, s := range got {
		names = append(names, s.Name)
	}
	want := []string{"chrome", "vnc"}
	if len(names) != len(want) {
		t.Fatalf("validateServiceSpecs() = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("validateServiceSpecs()[%d] = %q, want %q", i, names[i], want[i])
		}
	}
}

// captureStderr is defined in substrate_rootfs_test.go and reused here.

// gitConfigGet reads key from the gitconfig file at path via git itself,
// returning "" if the key is absent or the file can't be read — good enough
// for test assertions, which always know what they expect to find.
func gitConfigGet(t *testing.T, path, key string) string {
	t.Helper()
	out, err := exec.Command("git", "config", "--file", path, "--get", key).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// TestConfigureSharedWorkspaceGit_NeverConsultsPATHForGit pins that the git
// invocation never consults $PATH: with a planted "git" placed first on
// $PATH, the real, trusted git must still run — rootexec.Resolve's fixed
// search list, not $PATH, decides which binary this function execs — so the
// planted one never runs, and the gitconfig content this function produces
// still appears.
func TestConfigureSharedWorkspaceGit_NeverConsultsPATHForGit(t *testing.T) {
	realPath := os.Getenv("PATH")
	dir := t.TempDir()
	marker := filepath.Join(dir, "planted-ran")
	fake := filepath.Join(dir, "git")
	script := "#!/bin/sh\ntouch " + marker + "\nexit 1\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	agentHome := t.TempDir()
	if err := configureSharedWorkspaceGit(agentHome, 0, 0, false); err != nil {
		t.Fatalf("configureSharedWorkspaceGit: %v", err)
	}

	if _, err := os.Stat(marker); err == nil {
		t.Fatal("configureSharedWorkspaceGit executed a planted git from $PATH")
	}

	// Restore a real PATH before using the test's own git-based verification
	// helper: gitConfigGet (unlike the production code under test) resolves
	// "git" the ordinary way, through $PATH.
	t.Setenv("PATH", realPath)
	gitconfigPath := filepath.Join(agentHome, ".gitconfig")
	if got := gitConfigGet(t, gitconfigPath, "user.email"); got != "agent@scion.dev" {
		t.Errorf("user.email = %q, want agent@scion.dev (the real, resolved git must still have run)", got)
	}
}

// TestConfigureSharedWorkspaceGit_RunsUnderActiveReaperWithoutECHILD is the
// regression test for the property that configureSharedWorkspaceGit's
// internal runGitConfig closure must invoke git through procreap's managed
// exec API, not a raw cmd.CombinedOutput(), because sciontool init's real
// PID-1 reaper (procreap.StartReaper) is active for the whole lifetime of
// every one of these calls in production. A raw CombinedOutput() call here
// is exactly the shape of bug TestManagedService_StartSurvivesReaperRace
// (pkg/sciontool/services) guards against for the services manager: the
// reaper's generic wait4(-1, ...) can steal the git child's exit status
// from cmd.Wait() before CombinedOutput() gets to it, surfacing as an
// ECHILD-shaped "wait: no child processes" error that makes runGitConfig
// silently drop that config key (it only logs, it has no error to return).
//
// A real, live procreap reaper must run for this to be a faithful
// reproduction — a fake or absent reaper can't race anything (same
// requirement TestManagedService_StartSurvivesReaperRace documents).
//
// Positive control: this test is not vacuously green. Reverting
// runGitConfig's call back to a raw cmd.CombinedOutput() makes this test
// fail under `go test -race -count=5 -run
// TestConfigureSharedWorkspaceGit_RunsUnderActiveReaperWithoutECHILD
// ./cmd/sciontool/commands/`; with procreap.CombinedOutputManaged in place
// it passes reliably.
//
// The reaper runs until its process exits and reaps every child that is not
// started through procreap, so this test runs in a child copy of the test
// binary (see runInReaperChild). Started in this process, it would keep
// reaping the git children of the tests that run after it.
func TestConfigureSharedWorkspaceGit_RunsUnderActiveReaperWithoutECHILD(t *testing.T) {
	if os.Getenv(reaperChildEnv) != "1" {
		runInReaperChild(t)
		return
	}
	// Keep the marker out of the environment of every process the child
	// body starts (git, etc.).
	if err := os.Unsetenv(reaperChildEnv); err != nil {
		t.Fatalf("os.Unsetenv(%s): %v", reaperChildEnv, err)
	}
	procreap.StartReaper()

	// log.Init() runs first because the logger's lazy initialization is not concurrency-safe.
	log.Init()

	const iterations = 50
	// Pre-create every agentHome serially: t.TempDir() and t.Fatal are not
	// safe to call from multiple goroutines, so none of the concurrent work
	// below may call either.
	agentHomes := make([]string, iterations)
	for i := range agentHomes {
		agentHomes[i] = t.TempDir()
	}

	var wg sync.WaitGroup
	gotContents := make([]string, iterations)
	errs := make([]error, iterations)
	for i := 0; i < iterations; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := configureSharedWorkspaceGit(agentHomes[i], 0, 0, false); err != nil {
				errs[i] = fmt.Errorf("configureSharedWorkspaceGit: %w", err)
				return
			}
			gitconfigPath := filepath.Join(agentHomes[i], ".gitconfig")
			// Read the written file directly rather than shelling out to
			// `git config --get` (gitConfigGet's usual helper): gitConfigGet
			// is itself a raw, unmanaged exec.Command(...).Output() call, and
			// spawning one of those from 50 goroutines while the real
			// procreap reaper this test just started is live would just move
			// the exact ECHILD race this test exists to catch into the
			// verification step instead of the code under test — a false
			// failure that has nothing to do with runGitConfig's own
			// managed-exec behavior. A plain file read has no subprocess to
			// race at all.
			content, err := os.ReadFile(gitconfigPath)
			if err != nil {
				errs[i] = fmt.Errorf("os.ReadFile: %w", err)
				return
			}
			gotContents[i] = string(content)
		}(i)
	}
	wg.Wait()

	const wantLine = "email = agent@scion.dev"
	for i, got := range gotContents {
		if errs[i] != nil {
			t.Errorf("iteration %d: %v", i, errs[i])
			continue
		}
		if !strings.Contains(got, wantLine) {
			t.Errorf("iteration %d: gitconfig content = %q, want it to contain %q (a managed-exec regression would race the reaper and leave this key unset)", i, got, wantLine)
		}
	}
}

// TestConfigureSharedWorkspaceGit_PreservesExistingUnrelatedKeys proves a
// legitimate pre-existing .gitconfig's unrelated content survives the
// rewrite: git config --file edits the file in place, so a key this
// function never touches keeps its value.
func TestConfigureSharedWorkspaceGit_PreservesExistingUnrelatedKeys(t *testing.T) {
	agentHome := t.TempDir()
	gitconfigPath := filepath.Join(agentHome, ".gitconfig")
	if err := os.WriteFile(gitconfigPath, []byte("[foo]\n\tbar = baz\n"), 0o644); err != nil {
		t.Fatalf("write gitconfig: %v", err)
	}

	if err := configureSharedWorkspaceGit(agentHome, 0, 0, false); err != nil {
		t.Fatalf("configureSharedWorkspaceGit: %v", err)
	}

	if got := gitConfigGet(t, gitconfigPath, "foo.bar"); got != "baz" {
		t.Errorf("foo.bar = %q, want baz (pre-existing unrelated key lost)", got)
	}
	if got := gitConfigGet(t, gitconfigPath, "user.email"); got != "agent@scion.dev" {
		t.Errorf("user.email = %q, want agent@scion.dev", got)
	}
}

// TestConfigureSharedWorkspaceGit_IdempotentOnSecondRun proves running
// configureSharedWorkspaceGit twice in a row (e.g. across a restart)
// produces the same stable content.
func TestConfigureSharedWorkspaceGit_IdempotentOnSecondRun(t *testing.T) {
	agentHome := t.TempDir()
	gitconfigPath := filepath.Join(agentHome, ".gitconfig")

	if err := configureSharedWorkspaceGit(agentHome, 0, 0, false); err != nil {
		t.Fatalf("configureSharedWorkspaceGit (first run): %v", err)
	}
	first, err := os.ReadFile(gitconfigPath)
	if err != nil {
		t.Fatalf("read after first run: %v", err)
	}

	if err := configureSharedWorkspaceGit(agentHome, 0, 0, false); err != nil {
		t.Fatalf("configureSharedWorkspaceGit (second run): %v", err)
	}
	second, err := os.ReadFile(gitconfigPath)
	if err != nil {
		t.Fatalf("read after second run: %v", err)
	}

	if string(first) != string(second) {
		t.Errorf("gitconfig drifted across a second run:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
}

// TestConfigureSharedWorkspaceGit_SymlinkPreservedAndWrittenThrough proves a
// symlinked $HOME/.gitconfig — the layout a dotfile manager (chezmoi, stow,
// dotbot, ...) commonly produces — survives AS a symlink: git config --file
// resolves and rewrites its target in place, the same as any other command
// the workload could run against its own file.
func TestConfigureSharedWorkspaceGit_SymlinkPreservedAndWrittenThrough(t *testing.T) {
	agentHome := t.TempDir()
	store := filepath.Join(t.TempDir(), "dotfiles")
	if err := os.Mkdir(store, 0o700); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(store, "gitconfig")
	if err := os.WriteFile(real, []byte("[foo]\n\tbar = baz\n"), 0o600); err != nil {
		t.Fatalf("write real gitconfig: %v", err)
	}
	gitconfigPath := filepath.Join(agentHome, ".gitconfig")
	if err := os.Symlink(real, gitconfigPath); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if err := configureSharedWorkspaceGit(agentHome, 0, 0, false); err != nil {
		t.Fatalf("configureSharedWorkspaceGit: %v", err)
	}

	fi, err := os.Lstat(gitconfigPath)
	if err != nil {
		t.Fatalf("lstat gitconfig: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Error("gitconfigPath is no longer a symlink after configureSharedWorkspaceGit; expected it to be preserved")
	}
	if got := gitConfigGet(t, gitconfigPath, "foo.bar"); got != "baz" {
		t.Errorf("foo.bar = %q, want baz (the symlink's pre-existing content must survive)", got)
	}
	realData, err := os.ReadFile(real)
	if err != nil {
		t.Fatalf("read real gitconfig (the symlink's target): %v", err)
	}
	if !strings.Contains(string(realData), "agent@scion.dev") {
		t.Errorf("the symlink's target was not written through: %q", realData)
	}
}

// TestConfigureSharedWorkspaceGit_SymlinkedHomeAncestorWorks proves a
// symlinked ancestor directory above agentHome — not just the .gitconfig
// leaf itself — does not make the install fail: plain path resolution
// (used throughout this function) follows it like any other directory a
// legitimate deployment might reach through a symlinked mount.
func TestConfigureSharedWorkspaceGit_SymlinkedHomeAncestorWorks(t *testing.T) {
	real := t.TempDir()
	parent := t.TempDir()
	linked := filepath.Join(parent, "home-link")
	if err := os.Symlink(real, linked); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	agentHome := filepath.Join(linked, "agent")
	if err := os.Mkdir(agentHome, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := configureSharedWorkspaceGit(agentHome, 0, 0, false); err != nil {
		t.Fatalf("configureSharedWorkspaceGit: %v", err)
	}

	gitconfigPath := filepath.Join(real, "agent", ".gitconfig")
	if got := gitConfigGet(t, gitconfigPath, "user.email"); got != "agent@scion.dev" {
		t.Errorf("user.email = %q, want agent@scion.dev (install through a symlinked ancestor must succeed)", got)
	}
}

// TestConfigureSharedWorkspaceGit_WithoutRunScionAvailable proves the
// credential helper's configuration has no dependency on /run/scion, or any
// other staging directory, existing at all: configureSharedWorkspaceGit runs
// the git config calls directly against the real gitconfig path under the
// workload's own identity, so a runtime where /run/scion is entirely absent
// has no bearing here.
func TestConfigureSharedWorkspaceGit_WithoutRunScionAvailable(t *testing.T) {
	if _, err := os.Stat("/run/scion"); err == nil {
		t.Skip("/run/scion exists in this environment; this test wants to prove it is not needed, not that it is absent")
	}
	agentHome := t.TempDir()
	if err := configureSharedWorkspaceGit(agentHome, 0, 0, false); err != nil {
		t.Fatalf("configureSharedWorkspaceGit: %v", err)
	}
	gitconfigPath := filepath.Join(agentHome, ".gitconfig")
	if got := gitConfigGet(t, gitconfigPath, "credential.helper"); got == "" {
		t.Error("credential.helper is empty; expected it to be configured without /run/scion available")
	}
}

// TestConfigureSharedWorkspaceGit_AttackSymlinkToRootOwnedFileLeftByteIdentical
// is the core safety property the uid/gid>0 branch buys: a workload that
// symlinks its own .gitconfig at a root-owned file it does not own gains
// nothing from it — the git process configureSharedWorkspaceGit spawns runs
// AS the workload (SysProcAttr.Credential), so its own open(2) for writing
// hits an ordinary permission error on the root-owned target, exactly as it
// would if the workload had tried this itself directly, leaving that target
// byte-identical. This is only observable when this test process is real
// root (Credential only takes effect for a process already running as uid
// 0), so it is skipped rather than asserted vacuously otherwise.
func TestConfigureSharedWorkspaceGit_AttackSymlinkToRootOwnedFileLeftByteIdentical(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("requires real root: SysProcAttr.Credential only takes effect for a process already running as uid 0")
	}
	scionUser, err := user.Lookup("scion")
	if err != nil {
		t.Skipf("no scion user in this environment: %v", err)
	}
	uid, _ := strconv.Atoi(scionUser.Uid)
	gid, _ := strconv.Atoi(scionUser.Gid)
	if uid <= 0 || gid <= 0 {
		t.Skip("scion user has no usable non-root uid/gid in this environment")
	}

	agentHome := t.TempDir()
	rootOwned := filepath.Join(t.TempDir(), "root-secret")
	const rootContent = "[secret]\n\ttoken = do-not-touch\n"
	if err := os.WriteFile(rootOwned, []byte(rootContent), 0o600); err != nil {
		t.Fatal(err)
	}
	gitconfigPath := filepath.Join(agentHome, ".gitconfig")
	if err := os.Symlink(rootOwned, gitconfigPath); err != nil {
		t.Fatal(err)
	}

	// The git invocation itself is expected to fail (permission denied
	// against a target the workload identity cannot write) — this test's
	// own assertion is about the root-owned file's content, not about
	// configureSharedWorkspaceGit's own return value, since that failure is
	// logged and swallowed the same way any other per-call git failure is.
	_ = configureSharedWorkspaceGit(agentHome, uid, gid, true)

	after, err := os.ReadFile(rootOwned)
	if err != nil {
		t.Fatalf("read root-owned target: %v", err)
	}
	if string(after) != rootContent {
		t.Errorf("root-owned file was modified: %q, want byte-identical %q", after, rootContent)
	}
}

// TestConfigureSharedWorkspaceGit_EnforcedRefusesWithoutUsableUID proves
// RequirePrivilegeDrop with no usable uid/gid refuses outright, with the
// sentinel, before running git at all.
func TestConfigureSharedWorkspaceGit_EnforcedRefusesWithoutUsableUID(t *testing.T) {
	// The guard is uid > 0 && gid > 0: every case here has at least one of
	// the two not usable, so all four must refuse the same way.
	for _, tc := range []struct {
		name     string
		uid, gid int
	}{
		{"zero uid and gid", 0, 0},
		{"usable uid, zero gid", 1000, 0},
		{"zero uid, usable gid", 0, 1000},
		{"negative uid and gid", -1, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agentHome := t.TempDir()
			err := configureSharedWorkspaceGit(agentHome, tc.uid, tc.gid, true)
			if !errors.Is(err, errSharedWorkspaceGitPrivilegeDropRequired) {
				t.Fatalf("err = %v, want errSharedWorkspaceGitPrivilegeDropRequired", err)
			}
			if _, statErr := os.Stat(filepath.Join(agentHome, ".gitconfig")); statErr == nil {
				t.Error("expected no .gitconfig to be installed when privilege drop is required but refused")
			}
		})
	}
}

// TestConfigureSharedWorkspaceGit_FifoDoesNotHang proves a FIFO planted at
// $HOME/.gitconfig with no writer is refused immediately — via a stat, which
// never blocks, unlike opening the FIFO for real — rather than hanging
// RunInit forever. This stat runs unconditionally, regardless of which of
// the uid/gid cases above is taken: it guards a self-contained availability
// failure, not a symlink-specific privilege question the uid separation
// already answers.
func TestConfigureSharedWorkspaceGit_FifoDoesNotHang(t *testing.T) {
	agentHome := t.TempDir()
	gitconfigPath := filepath.Join(agentHome, ".gitconfig")
	if err := syscall.Mkfifo(gitconfigPath, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}

	output := captureStderr(t, func() {
		done := make(chan struct{})
		go func() {
			_ = configureSharedWorkspaceGit(agentHome, 0, 0, false)
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("configureSharedWorkspaceGit blocked on a FIFO planted at .gitconfig")
		}
	})

	if !strings.Contains(output, "WARN") {
		t.Errorf("expected a WARN in the output, got: %s", output)
	}
	fi, err := os.Lstat(gitconfigPath)
	if err != nil {
		t.Fatalf("lstat gitconfig: %v", err)
	}
	if fi.Mode()&os.ModeNamedPipe == 0 {
		t.Errorf("gitconfigPath mode = %v, want unchanged FIFO (refused and left in place, not replaced)", fi.Mode())
	}
}

// TestInitRunOptions_ZeroValueForwardsTermSignal proves the zero-value
// InitRunOptions{} matches sciontool init's historical CLI default (term
// signal forwarding on) without the CLI needing to set anything, and that
// DisableTermSignalForwarding actually flips it off for an in-process
// caller that owns its own SIGTERM handling.
func TestInitRunOptions_ZeroValueForwardsTermSignal(t *testing.T) {
	if !(InitRunOptions{}).forwardsTermSignal() {
		t.Error("zero-value InitRunOptions must forward term signals by default, matching sciontool init's historical CLI behaviour")
	}
	if (InitRunOptions{DisableTermSignalForwarding: true}).forwardsTermSignal() {
		t.Error("DisableTermSignalForwarding: true must turn off forwarding")
	}
}

// TestNewLifecycleManager_WiresEnforcedModeConsistently is the wiring test
// for the switch that turns on every generic enforced-privilege-drop
// behaviour at once: EnforcePrivilegeDrop, WorkloadUID/WorkloadGID, and the
// hub token-file owner-check flag newLifecycleManager reports back for
// RunInit to pass to hub.EnforceTokenFileOwnerChecks all derive from the
// identical requirePrivilegeDrop input. A change that stops threading any
// one of them through independently must fail here, not survive to be
// caught only by a much larger, harder-to-diagnose end-to-end test.
func TestNewLifecycleManager_WiresEnforcedModeConsistently(t *testing.T) {
	tests := []struct {
		name                 string
		requirePrivilegeDrop bool
		targetUID, targetGID int
	}{
		{"enforced with a real workload identity", true, 1000, 1000},
		{"unenforced (docker/base parity)", false, 0, 0},
		{"unenforced with a resolved identity anyway", false, 1000, 1000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agentHome := "/home/scion"
			lm, enforceTokenOwnerChecks := newLifecycleManager(agentHome, tt.targetUID, tt.targetGID, tt.requirePrivilegeDrop)

			if lm.EnforcePrivilegeDrop != tt.requirePrivilegeDrop {
				t.Errorf("EnforcePrivilegeDrop = %v, want %v", lm.EnforcePrivilegeDrop, tt.requirePrivilegeDrop)
			}
			if lm.WorkloadUID != tt.targetUID {
				t.Errorf("WorkloadUID = %d, want %d", lm.WorkloadUID, tt.targetUID)
			}
			if lm.WorkloadGID != tt.targetGID {
				t.Errorf("WorkloadGID = %d, want %d", lm.WorkloadGID, tt.targetGID)
			}
			if enforceTokenOwnerChecks != tt.requirePrivilegeDrop {
				t.Errorf("enforceTokenOwnerChecks = %v, want %v (must track requirePrivilegeDrop exactly)", enforceTokenOwnerChecks, tt.requirePrivilegeDrop)
			}

			// HooksDirs ordering: the per-agent hooks directory must always
			// be last (system-then-per-agent, per AddHooksDir's own doc
			// comment), and EnforcedHooksDir must appear, before it, only
			// when enforced.
			wantDirs := defaultHooksDirsForTest()
			if tt.requirePrivilegeDrop {
				wantDirs = append(wantDirs, hooks.EnforcedHooksDir)
			}
			wantDirs = append(wantDirs, filepath.Join(agentHome, ".scion", "hooks"))
			if !reflect.DeepEqual(lm.HooksDirs, wantDirs) {
				t.Errorf("HooksDirs = %v, want %v", lm.HooksDirs, wantDirs)
			}
		})
	}
}

// defaultHooksDirsForTest returns the default discovery list a fresh
// hooks.NewLifecycleManager starts with, so
// TestNewLifecycleManager_WiresEnforcedModeConsistently's expected HooksDirs
// stays correct regardless of $SCION_HOOKS_DIR in the test environment,
// without duplicating NewLifecycleManager's own resolution logic here.
func defaultHooksDirsForTest() []string {
	return hooks.NewLifecycleManager().HooksDirs
}

// reaperChildEnv is set in the child test process that runInReaperChild
// starts.
const reaperChildEnv = "SCION_TEST_REAPER_CHILD"

// runInReaperChild runs the calling test alone in a child copy of the test
// binary, with reaperChildEnv set, and fails the test if it fails there. A
// test that starts the procreap reaper uses it so the reaper ends with the
// child process.
func runInReaperChild(t *testing.T) {
	t.Helper()
	quotedName := regexp.QuoteMeta(t.Name())
	args := []string{"-test.run=^" + quotedName + "$", "-test.count=1", "-test.v"}
	// Propagate the parent's deadline so a hung child cannot outlive the
	// parent: CommandContext does not kill the child if the parent panics
	// on its own -timeout.
	if d, ok := t.Deadline(); ok {
		if remaining := time.Until(d); remaining > 0 {
			args = append(args, "-test.timeout="+remaining.String())
		}
	}
	cmd := exec.CommandContext(t.Context(), os.Args[0], args...)
	cmd.Env = append(os.Environ(), reaperChildEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s in a child test process: %v\n%s", t.Name(), err, out)
	}
	// Guard against a vacuous pass: match the whole result line so a test
	// like <name>_Longer or <name>/sub cannot satisfy it.
	passLine := regexp.MustCompile(`(?m)^--- PASS: ` + quotedName + ` \(`)
	if !passLine.Match(out) {
		t.Fatalf("%s did not run in the child test process:\n%s", t.Name(), out)
	}
}
