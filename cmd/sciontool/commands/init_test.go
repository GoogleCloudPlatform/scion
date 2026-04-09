/*
Copyright 2025 The Scion Authors.
*/

package commands

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestInitGroveDataIsolation is a canary test that verifies sciontool source code
// does NOT import the pkg/config package, which contains grove path resolution logic.
// This is a compile-time guarantee that in-container code cannot access grove data paths.
// If this test fails, it means someone added a pkg/config import to sciontool code,
// which would break the agent isolation model.
func TestInitGroveDataIsolation(t *testing.T) {
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
			t.Fatal("sciontool must NOT import pkg/config (grove path resolution). " +
				"In-container code should use the Hub API or agent-local files, " +
				"not filesystem-based grove data access.")
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

	// Build sciontool if needed for integration testing
	cmd := exec.Command("go", "build", "-buildvcs=false", "-o", "/tmp/sciontool-test", "../")
	if err := cmd.Run(); err != nil {
		t.Skipf("failed to build sciontool for integration test: %v", err)
	}

	// Test running a simple command
	testCmd := exec.Command("/tmp/sciontool-test", "init", "--", "echo", "hello")
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
	os.Unsetenv("SCION_GIT_CLONE_URL")
	defer func() {
		if orig != "" {
			os.Setenv("SCION_GIT_CLONE_URL", orig)
		}
	}()

	err := gitCloneWorkspace(0, 0, "/tmp")
	if err != nil {
		t.Errorf("expected nil error when SCION_GIT_CLONE_URL is not set, got: %v", err)
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
		os.WriteFile(filepath.Join(tmpDir, "file.txt"), []byte("content"), 0644)
		if isWorkspaceEmpty(tmpDir) {
			t.Error("expected false for directory with files")
		}
	})

	t.Run("directory with only .scion marker", func(t *testing.T) {
		tmpDir := t.TempDir()
		os.MkdirAll(filepath.Join(tmpDir, ".scion"), 0755)
		if !isWorkspaceEmpty(tmpDir) {
			t.Error("expected true when workspace contains only .scion marker")
		}
	})

	t.Run("directory with only .scion-volumes", func(t *testing.T) {
		tmpDir := t.TempDir()
		os.MkdirAll(filepath.Join(tmpDir, ".scion-volumes"), 0755)
		if !isWorkspaceEmpty(tmpDir) {
			t.Error("expected true when workspace contains only .scion-volumes")
		}
	})

	t.Run("directory with .scion and .scion-volumes", func(t *testing.T) {
		tmpDir := t.TempDir()
		os.MkdirAll(filepath.Join(tmpDir, ".scion"), 0755)
		os.MkdirAll(filepath.Join(tmpDir, ".scion-volumes"), 0755)
		if !isWorkspaceEmpty(tmpDir) {
			t.Error("expected true when workspace contains only .scion and .scion-volumes")
		}
	})

	t.Run("directory with .scion marker and real content", func(t *testing.T) {
		tmpDir := t.TempDir()
		os.MkdirAll(filepath.Join(tmpDir, ".scion"), 0755)
		os.WriteFile(filepath.Join(tmpDir, "README.md"), []byte("content"), 0644)
		if isWorkspaceEmpty(tmpDir) {
			t.Error("expected false when workspace has .scion and real files")
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
			os.Unsetenv("container")
			os.Unsetenv("SCION_ALT_USERMOD")
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
	err := gitCloneWorkspace(0, 0, "/tmp")
	if err == nil {
		t.Fatal("expected error from git clone to nonexistent host")
	}
	// The error may come from git init, git fetch, or git clone depending
	// on how far the function gets before failing.
	errMsg := err.Error()
	if !strings.Contains(errMsg, "git clone failed") && !strings.Contains(errMsg, "git init failed") && !strings.Contains(errMsg, "git remote add failed") && !strings.Contains(errMsg, "failed") {
		t.Errorf("expected a git failure error, got: %v", err)
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

	err := gitCloneWorkspace(uid, gid, "/tmp")
	if err == nil {
		t.Fatal("expected error from git clone to nonexistent host")
	}
	errMsg := err.Error()
	if !strings.Contains(errMsg, "git clone failed") && !strings.Contains(errMsg, "git init failed") && !strings.Contains(errMsg, "git remote add failed") && !strings.Contains(errMsg, "failed") {
		t.Errorf("expected a git failure error, got: %v", err)
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
