/*
Copyright 2025 The Scion Authors.
*/

package commands

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

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

	err := gitCloneWorkspace()
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
			name:     "non-parseable URL returns as-is",
			cloneURL: "not-a-url",
			token:    "ghp_abc",
			expected: "not-a-url",
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
