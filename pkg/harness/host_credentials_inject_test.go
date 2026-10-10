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

package harness

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// shippedClaudeAuthMeta returns the embedded claude harness auth metadata,
// so the tests exercise exactly what a shipped harness declares.
func shippedClaudeAuthMeta(t *testing.T) *config.HarnessAuthMetadata {
	t.Helper()
	return loadAuthMetaFromHarness(t, "claude")
}

func TestShippedRequiredFiles_IncludesHarnessLoginFiles(t *testing.T) {
	set := shippedRequiredFiles()
	for _, want := range []shippedRequiredFile{
		{"CLAUDE_AUTH", "ClaudeAuthFile", ".claude/.credentials.json"},
		{"CODEX_AUTH", "CodexAuthFile", ".codex/auth.json"},
		{"GEMINI_OAUTH_CREDS", "OAuthCreds", ".gemini/oauth_creds.json"},
	} {
		if _, ok := set[want]; !ok {
			t.Errorf("shipped required files missing %+v", want)
		}
	}
	for k := range set {
		if k.rel == ".ssh/id_ed25519" {
			t.Errorf("unexpected shipped entry %+v", k)
		}
	}
}

func TestInjectableHostCredentialFiles_ShippedHarnessFile(t *testing.T) {
	home := t.TempDir()
	p := writeHomeFile(t, home, "/.claude/.credentials.json")
	got := InjectableHostCredentialFiles(shippedClaudeAuthMeta(t), home)
	if len(got) != 1 || got[0].Name != "CLAUDE_AUTH" {
		t.Fatalf("InjectableHostCredentialFiles = %+v, want CLAUDE_AUTH", got)
	}
	want, _ := filepath.EvalSymlinks(p)
	if got[0].Path != want {
		t.Fatalf("Path = %q, want resolved %q", got[0].Path, want)
	}
}

func TestInjectableHostCredentialFiles_NonShippedEntryRejected(t *testing.T) {
	home := t.TempDir()
	writeHomeFile(t, home, "/.ssh/id_ed25519")
	writeHomeFile(t, home, "/.claude/.credentials.json")
	meta := &config.HarnessAuthMetadata{Types: map[string]config.HarnessAuthTypeMetadata{
		"a": {RequiredFiles: []config.HarnessAuthFileRequirement{
			{Name: "SSH_KEY", Field: "SSHKeyFile", TargetSuffix: "/.ssh/id_ed25519"},
		}},
		"b": {RequiredFiles: []config.HarnessAuthFileRequirement{
			// Shipped file, but under a name the shipped config does not use.
			{Name: "renamed", Field: "ClaudeAuthFile", TargetSuffix: "/.claude/.credentials.json"},
		}},
	}}
	if got := InjectableHostCredentialFiles(meta, home); got != nil {
		t.Fatalf("expected nothing, got %+v", got)
	}
}

func TestInjectableHostCredentialFiles_Symlinks(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	outside := filepath.Join(root, "outside", "secret.json")
	if err := os.MkdirAll(filepath.Dir(outside), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home, ".claude", ".credentials.json")
	meta := shippedClaudeAuthMeta(t)

	t.Run("file symlink pointing out of home is rejected", func(t *testing.T) {
		_ = os.Remove(link)
		if err := os.Symlink(outside, link); err != nil {
			t.Fatal(err)
		}
		if got := InjectableHostCredentialFiles(meta, home); got != nil {
			t.Fatalf("expected nothing, got %+v", got)
		}
	})

	t.Run("directory symlink pointing out of home is rejected", func(t *testing.T) {
		claudeDir := filepath.Join(home, ".claude")
		if err := os.RemoveAll(claudeDir); err != nil {
			t.Fatal(err)
		}
		outDir := filepath.Join(root, "outside", "claude")
		writeHomeFile(t, outDir, "/.credentials.json")
		if err := os.Symlink(outDir, claudeDir); err != nil {
			t.Fatal(err)
		}
		if got := InjectableHostCredentialFiles(meta, home); got != nil {
			t.Fatalf("expected nothing, got %+v", got)
		}
		if err := os.Remove(claudeDir); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(claudeDir, 0o755); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("symlink inside home is followed", func(t *testing.T) {
		target := writeHomeFile(t, home, "/.dotfiles/claude-credentials.json")
		_ = os.Remove(link)
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		got := InjectableHostCredentialFiles(meta, home)
		want, _ := filepath.EvalSymlinks(target)
		if len(got) != 1 || got[0].Path != want {
			t.Fatalf("InjectableHostCredentialFiles = %+v, want path %q", got, want)
		}
	})

	t.Run("home reached through a symlink still works", func(t *testing.T) {
		homeLink := filepath.Join(root, "home-link")
		if err := os.Symlink(home, homeLink); err != nil {
			t.Fatal(err)
		}
		if got := InjectableHostCredentialFiles(meta, homeLink); len(got) != 1 {
			t.Fatalf("expected the in-home file via a symlinked home, got %+v", got)
		}
	})
}

func TestInjectableHostCredentialFiles_NotRegularFile(t *testing.T) {
	home := t.TempDir()
	// A directory where the credential file should be.
	if err := os.MkdirAll(filepath.Join(home, ".claude", ".credentials.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := InjectableHostCredentialFiles(shippedClaudeAuthMeta(t), home); got != nil {
		t.Fatalf("expected nothing for a directory, got %+v", got)
	}
}

func TestPathWithin(t *testing.T) {
	tests := []struct {
		dir, path string
		want      bool
	}{
		{"/home/u", "/home/u/.claude/x", true},
		{"/home/u", "/home/u", false},
		{"/home/u", "/home/user2/x", false},
		{"/home/u", "/etc/passwd", false},
		{"/home/u", "/home/u/../v/x", false},
	}
	for _, tt := range tests {
		if got := pathWithin(tt.dir, tt.path); got != tt.want {
			t.Errorf("pathWithin(%q, %q) = %v, want %v", tt.dir, tt.path, got, tt.want)
		}
	}
}
