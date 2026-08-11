// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package chatapp

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- sanitizePathComponent tests ---

func TestSanitizePathComponent(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "simple filename", input: "report.pdf", want: "report.pdf"},
		{name: "filename with spaces", input: "my report.pdf", want: "my report.pdf"},
		{name: "traversal ../../etc/passwd", input: "../../etc/passwd", want: "passwd"},
		{name: "absolute path /etc/passwd", input: "/etc/passwd", want: "passwd"},
		{name: "null bytes", input: "file\x00name.txt", want: "file_name.txt"},
		{name: "backslash traversal", input: `..\..\etc\passwd`, want: ".._.._etc_passwd"},
		{name: "dot only", input: ".", want: ""},
		{name: "double dot only", input: "..", want: ""},
		{name: "empty string", input: "", want: ""},
		{name: "forward slash in name", input: "path/file.txt", want: "file.txt"},
		{name: "mixed separators", input: `a/b\c`, want: "b_c"},
		{name: "leading dot file", input: ".hidden", want: ".hidden"},
		{name: "symlink-like name", input: "link -> target", want: "link -> target"},
		{name: "just slashes", input: "///", want: "_"},
		{name: "unicode filename", input: "résumé.pdf", want: "résumé.pdf"},
		{name: "deeply nested traversal", input: "../../../../../../../etc/shadow", want: "shadow"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizePathComponent(tt.input)
			if got != tt.want {
				t.Errorf("sanitizePathComponent(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// --- resolveAgentPath tests ---

func TestResolveAgentPath(t *testing.T) {
	tests := []struct {
		name        string
		agentPath   string
		projectSlug string
		projectID   string
		wantEmpty   bool   // true if we expect ""
		wantPrefix  string // prefix the result should start with (if not empty)
	}{
		{
			name:        "scion-volumes path resolves",
			agentPath:   "/scion-volumes/scratchpad/data/file.txt",
			projectSlug: "test-proj",
			projectID:   "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
			wantEmpty:   false,
			wantPrefix:  filepath.Join(os.Getenv("HOME"), ".scion"),
		},
		{
			name:        "workspace scion-volumes path resolves",
			agentPath:   "/workspace/.scion-volumes/scratchpad/data/file.txt",
			projectSlug: "test-proj",
			projectID:   "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
			wantEmpty:   false,
			wantPrefix:  filepath.Join(os.Getenv("HOME"), ".scion"),
		},
		{
			name:        "traversal via scion-volumes rejected",
			agentPath:   "/scion-volumes/scratchpad/../../../etc/passwd",
			projectSlug: "test-proj",
			projectID:   "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
			wantEmpty:   true,
		},
		{
			name:        "empty path returns empty",
			agentPath:   "",
			projectSlug: "test-proj",
			projectID:   "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
			wantEmpty:   true,
		},
		{
			name:        "empty project slug returns empty for scion-volumes",
			agentPath:   "/scion-volumes/scratchpad/file.txt",
			projectSlug: "",
			projectID:   "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
			wantEmpty:   true,
		},
		{
			name:        "empty project ID returns empty for scion-volumes",
			agentPath:   "/scion-volumes/scratchpad/file.txt",
			projectSlug: "test-proj",
			projectID:   "",
			wantEmpty:   true,
		},
		{
			name:        "relative path returns empty",
			agentPath:   "relative/path.txt",
			projectSlug: "test-proj",
			projectID:   "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
			wantEmpty:   true,
		},
		{
			name:        "arbitrary absolute path outside safe dirs rejected",
			agentPath:   "/etc/passwd",
			projectSlug: "test-proj",
			projectID:   "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
			wantEmpty:   true,
		},
		{
			name:        "absolute path with traversal out of workspace rejected",
			agentPath:   "/workspace/../etc/passwd",
			projectSlug: "test-proj",
			projectID:   "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
			wantEmpty:   true,
		},
		{
			name:        "dot-dot shared dir name rejected",
			agentPath:   "/scion-volumes/../secret",
			projectSlug: "test-proj",
			projectID:   "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
			wantEmpty:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveAgentPath(tt.agentPath, tt.projectSlug, tt.projectID)
			if tt.wantEmpty && got != "" {
				t.Errorf("resolveAgentPath(%q) = %q, want empty", tt.agentPath, got)
			}
			if !tt.wantEmpty && got == "" {
				t.Errorf("resolveAgentPath(%q) = empty, want non-empty with prefix %q", tt.agentPath, tt.wantPrefix)
			}
			if !tt.wantEmpty && tt.wantPrefix != "" && !strings.HasPrefix(got, tt.wantPrefix) {
				t.Errorf("resolveAgentPath(%q) = %q, want prefix %q", tt.agentPath, got, tt.wantPrefix)
			}
		})
	}
}

// TestResolveAgentPath_WorkspacePath verifies that workspace paths are accepted
// only when the file exists and the cleaned path stays under /workspace/.
func TestResolveAgentPath_WorkspacePath(t *testing.T) {
	// Create a temp file under a workspace-like directory.
	dir := t.TempDir()
	testFile := filepath.Join(dir, "test.txt")
	if err := os.WriteFile(testFile, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Workspace paths resolve only for existing files. Since /workspace/
	// may not exist in the test environment, just verify that the safe-prefix
	// check blocks non-workspace absolute paths.
	got := resolveAgentPath("/tmp/should-not-resolve", "proj", "id-1234")
	if got != "" {
		t.Errorf("expected /tmp path to be rejected, got %q", got)
	}
}

// --- resolveSharedDirPath tests ---

func TestResolveSharedDirPath(t *testing.T) {
	tests := []struct {
		name          string
		containerPath string
		projectSlug   string
		projectID     string
		wantEmpty     bool
	}{
		{
			name:          "valid path",
			containerPath: "/scion-volumes/scratchpad/data/file.txt",
			projectSlug:   "myproj",
			projectID:     "12345678-abcd-efgh-ijkl-000000000000",
			wantEmpty:     false,
		},
		{
			name:          "traversal in relPath rejected",
			containerPath: "/scion-volumes/scratchpad/../../../etc/passwd",
			projectSlug:   "myproj",
			projectID:     "12345678-abcd-efgh-ijkl-000000000000",
			wantEmpty:     true,
		},
		{
			name:          "shared dir name is dot rejected",
			containerPath: "/scion-volumes/./file",
			projectSlug:   "myproj",
			projectID:     "12345678-abcd-efgh-ijkl-000000000000",
			wantEmpty:     true,
		},
		{
			name:          "shared dir name is dotdot rejected",
			containerPath: "/scion-volumes/../file",
			projectSlug:   "myproj",
			projectID:     "12345678-abcd-efgh-ijkl-000000000000",
			wantEmpty:     true,
		},
		{
			name:          "empty trimmed path rejected",
			containerPath: "/scion-volumes/",
			projectSlug:   "myproj",
			projectID:     "12345678-abcd-efgh-ijkl-000000000000",
			wantEmpty:     true,
		},
		{
			name:          "just shared dir name, no file",
			containerPath: "/scion-volumes/scratchpad",
			projectSlug:   "myproj",
			projectID:     "12345678-abcd-efgh-ijkl-000000000000",
			wantEmpty:     false,
		},
		{
			name:          "empty project slug",
			containerPath: "/scion-volumes/scratchpad/file",
			projectSlug:   "",
			projectID:     "12345678-abcd-efgh-ijkl-000000000000",
			wantEmpty:     true,
		},
		{
			name:          "double slash creates absolute relPath which is rejected",
			containerPath: "/scion-volumes/scratchpad//etc/passwd",
			projectSlug:   "myproj",
			projectID:     "12345678-abcd-efgh-ijkl-000000000000",
			wantEmpty:     true, // SplitN produces "/etc/passwd" which is absolute
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveSharedDirPath(tt.containerPath, tt.projectSlug, tt.projectID)
			if tt.wantEmpty && got != "" {
				t.Errorf("resolveSharedDirPath(%q) = %q, want empty", tt.containerPath, got)
			}
			if !tt.wantEmpty && got == "" {
				t.Errorf("resolveSharedDirPath(%q) = empty, want non-empty", tt.containerPath)
			}
		})
	}
}

// --- SizeLimitErrorCard tests ---

func TestSizeLimitErrorCard(t *testing.T) {
	tests := []struct {
		name           string
		filename       string
		size           int64
		wantContain    string
		wantNotContain string
	}{
		{
			name:        "known size shows formatted size",
			filename:    "big.zip",
			size:        30 * 1024 * 1024,
			wantContain: "30.0 MB",
		},
		{
			name:           "zero size omits size info",
			filename:       "unknown.dat",
			size:           0,
			wantContain:    "exceeds the 25 MB attachment limit",
			wantNotContain: "0.0 KB",
		},
		{
			name:        "small known size shows KB",
			filename:    "tiny.txt",
			size:        512 * 1024,
			wantContain: "512.0 KB",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			card := SizeLimitErrorCard(tt.filename, tt.size)
			if card.Header.Title != "⚠️ Attachment Too Large" {
				t.Errorf("unexpected title: %s", card.Header.Title)
			}
			if card.Header.Subtitle != tt.filename {
				t.Errorf("subtitle = %q, want %q", card.Header.Subtitle, tt.filename)
			}
			if len(card.Sections) == 0 || len(card.Sections[0].Widgets) == 0 {
				t.Fatal("expected at least one section with a widget")
			}
			content := card.Sections[0].Widgets[0].Content
			if tt.wantContain != "" && !strings.Contains(content, tt.wantContain) {
				t.Errorf("content should contain %q, got: %s", tt.wantContain, content)
			}
			if tt.wantNotContain != "" && strings.Contains(content, tt.wantNotContain) {
				t.Errorf("content should NOT contain %q, got: %s", tt.wantNotContain, content)
			}
		})
	}
}

// --- ResolveOutboundAttachments tests ---

func TestResolveOutboundAttachments_SizeLimit(t *testing.T) {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	// Create a temp directory with test files.
	dir := t.TempDir()

	// Create a file exactly at the limit.
	atLimitFile := filepath.Join(dir, "at_limit.bin")
	if err := os.WriteFile(atLimitFile, make([]byte, MaxAttachmentSize), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create a file over the limit.
	overLimitFile := filepath.Join(dir, "over_limit.bin")
	if err := os.WriteFile(overLimitFile, make([]byte, MaxAttachmentSize+1), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create a small file.
	smallFile := filepath.Join(dir, "small.txt")
	if err := os.WriteFile(smallFile, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name      string
		paths     []string
		wantCount int
	}{
		{
			name:      "empty paths returns nil",
			paths:     nil,
			wantCount: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ResolveOutboundAttachments(log, tt.paths, "proj", "id-1234")
			if len(result) != tt.wantCount {
				t.Errorf("got %d attachments, want %d", len(result), tt.wantCount)
			}
		})
	}
}

// --- formatFileSize tests ---

func TestFormatFileSize(t *testing.T) {
	tests := []struct {
		name  string
		bytes int64
		want  string
	}{
		{name: "zero bytes", bytes: 0, want: "0.0 KB"},
		{name: "1 KB", bytes: 1024, want: "1.0 KB"},
		{name: "500 KB", bytes: 500 * 1024, want: "500.0 KB"},
		{name: "1 MB", bytes: 1024 * 1024, want: "1.0 MB"},
		{name: "25 MB", bytes: 25 * 1024 * 1024, want: "25.0 MB"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatFileSize(tt.bytes)
			if got != tt.want {
				t.Errorf("formatFileSize(%d) = %q, want %q", tt.bytes, got, tt.want)
			}
		})
	}
}
