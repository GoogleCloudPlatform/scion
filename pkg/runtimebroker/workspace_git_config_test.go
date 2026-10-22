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

package runtimebroker

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/gcp"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

func TestValidateOriginURL(t *testing.T) {
	for _, ok := range []string{
		"https://github.com/org/repo.git",
		"http://git.internal/org/repo",
		"ssh://git@github.com/org/repo.git",
		"ssh://git@host:2222/org/repo",
		"git://host/org/repo",
		"git@github.com:org/repo.git",
		"HTTPS://github.com/org/repo",
	} {
		if err := validateOriginURL(ok); err != nil {
			t.Errorf("validateOriginURL(%q) = %v, want ok", ok, err)
		}
	}
	for _, bad := range []string{
		"",
		"ext::sh -c touch% /tmp/pwned",
		"file::/etc",
		"file:///srv/repo",
		"/srv/repo",
		"../repo",
		"-uhelp",
		"https://host/repo\n[core]\n\thooksPath = /tmp",
		"https://host/repo\"x",
		"https://host/repo\\x",
		"https://host/repo;x",
		"https://host/repo#x",
		"https://host/re po",
		"https://",
		"deploy@host:org/repo",
		"git@host:org/repo; rm -rf /",
		"fd::17",
	} {
		if err := validateOriginURL(bad); err == nil {
			t.Errorf("validateOriginURL(%q) = nil, want refused", bad)
		}
	}
}

// executableGitConfigKeys are configuration keys that name a program, a
// command or a hook location; the minimal configuration holds none.
var executableGitConfigKeys = []string{
	"hookspath", "fsmonitor", "sshcommand", "editor", "pager", "alias", "filter",
	"askpass", "credential", "gpg", "program", "command", "textconv", "uploadpack",
	"receivepack", "proxy", "include", "worktree", "attributesfile", "excludesfile",
}

func TestEnsureMinimalGitConfig(t *testing.T) {
	t.Run("writes core and origin", func(t *testing.T) {
		ws := t.TempDir()
		if err := os.MkdirAll(filepath.Join(ws, ".git/refs/heads"), 0o755); err != nil {
			t.Fatal(err)
		}
		wrote, err := ensureMinimalGitConfig(ws, "https://github.com/org/repo.git")
		if err != nil || !wrote {
			t.Fatalf("wrote=%v err=%v", wrote, err)
		}
		b, err := os.ReadFile(filepath.Join(ws, ".git/config"))
		if err != nil {
			t.Fatal(err)
		}
		cfg := string(b)
		if !strings.Contains(cfg, "\turl = https://github.com/org/repo.git\n") || !strings.Contains(cfg, "bare = false") {
			t.Errorf("config = %q", cfg)
		}
		lower := strings.ToLower(cfg)
		for _, key := range executableGitConfigKeys {
			if strings.Contains(lower, key) {
				t.Errorf("minimal config contains %q:\n%s", key, cfg)
			}
		}
	})

	t.Run("unsafe URL gives no remote", func(t *testing.T) {
		ws := t.TempDir()
		if err := os.MkdirAll(filepath.Join(ws, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		wrote, err := ensureMinimalGitConfig(ws, "ext::sh -c evil")
		if !wrote || !errors.Is(err, errUnsafeCloneURL) {
			t.Fatalf("wrote=%v err=%v, want written without remote and errUnsafeCloneURL", wrote, err)
		}
		b, _ := os.ReadFile(filepath.Join(ws, ".git/config"))
		if strings.Contains(string(b), "remote") || strings.Contains(string(b), "ext::") {
			t.Errorf("config = %q", b)
		}
	})

	t.Run("no URL gives core only", func(t *testing.T) {
		ws := t.TempDir()
		if err := os.MkdirAll(filepath.Join(ws, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		if wrote, err := ensureMinimalGitConfig(ws, ""); !wrote || err != nil {
			t.Fatalf("wrote=%v err=%v", wrote, err)
		}
		b, _ := os.ReadFile(filepath.Join(ws, ".git/config"))
		if string(b) != minimalGitConfigCore {
			t.Errorf("config = %q", b)
		}
	})

	t.Run("existing config kept", func(t *testing.T) {
		ws := t.TempDir()
		if err := os.MkdirAll(filepath.Join(ws, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(ws, ".git/config"), []byte("mine"), 0o644); err != nil {
			t.Fatal(err)
		}
		if wrote, err := ensureMinimalGitConfig(ws, "https://h/r"); wrote || err != nil {
			t.Fatalf("wrote=%v err=%v", wrote, err)
		}
		if b, _ := os.ReadFile(filepath.Join(ws, ".git/config")); string(b) != "mine" {
			t.Errorf("config = %q", b)
		}
	})

	t.Run("no .git, .git file or .git symlink left alone", func(t *testing.T) {
		ws := t.TempDir()
		if wrote, err := ensureMinimalGitConfig(ws, "https://h/r"); wrote || err != nil {
			t.Fatalf("no .git: wrote=%v err=%v", wrote, err)
		}
		if err := os.WriteFile(filepath.Join(ws, ".git"), []byte("gitdir: /x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if wrote, err := ensureMinimalGitConfig(ws, "https://h/r"); wrote || err != nil {
			t.Fatalf(".git file: wrote=%v err=%v", wrote, err)
		}
		ws2, outside := t.TempDir(), t.TempDir()
		if err := os.Symlink(outside, filepath.Join(ws2, ".git")); err != nil {
			t.Skipf("symlinks unsupported: %v", err)
		}
		if wrote, err := ensureMinimalGitConfig(ws2, "https://h/r"); wrote || err != nil {
			t.Fatalf(".git symlink: wrote=%v err=%v", wrote, err)
		}
		if _, err := os.Lstat(filepath.Join(outside, "config")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("config written through a .git symlink: %v", err)
		}
	})
}

// TestWorkspaceDownloaders_Defaults pins which gcp transfer each broker
// workspace leg uses by default: uploads carry content and git data, the
// create-time download applies both, and an apply (sync-to) applies only
// content.
func TestWorkspaceDownloaders_Defaults(t *testing.T) {
	srv := New(DefaultServerConfig(), &mockAgentManager{}, &runtime.MockRuntime{NameFunc: func() string { return "docker" }}, config.RuntimeBuildRecord{})
	same := func(a, b any) bool { return reflect.ValueOf(a).Pointer() == reflect.ValueOf(b).Pointer() }
	if !same(srv.workspaceUploader(), gcp.SyncWorkspaceToGCS) {
		t.Error("workspace upload is not gcp.SyncWorkspaceToGCS")
	}
	if !same(srv.workspaceDownloader(), gcp.SyncWorkspaceFromGCS) {
		t.Error("create-time workspace download is not gcp.SyncWorkspaceFromGCS")
	}
	if !same(srv.workspaceApplyDownloader(), gcp.SyncWorkspaceContentFromGCS) {
		t.Error("workspace apply download is not gcp.SyncWorkspaceContentFromGCS")
	}
}

// parseGitConfig flattens the minimal configuration into "section.key=value"
// lines ("section.subsection.key=value" for a quoted subsection), failing on
// anything it does not recognise.
func parseGitConfig(t *testing.T, cfg string) []string {
	t.Helper()
	var out []string
	section := ""
	for _, line := range strings.Split(strings.TrimSuffix(cfg, "\n"), "\n") {
		switch {
		case strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]"):
			inner := line[1 : len(line)-1]
			if name, sub, ok := strings.Cut(inner, " \""); ok {
				if !strings.HasSuffix(sub, "\"") {
					t.Fatalf("bad subsection line %q", line)
				}
				section = name + "." + strings.TrimSuffix(sub, "\"")
			} else {
				section = inner
			}
		case strings.HasPrefix(line, "\t"):
			k, v, ok := strings.Cut(strings.TrimPrefix(line, "\t"), " = ")
			if !ok || section == "" {
				t.Fatalf("bad key line %q", line)
			}
			out = append(out, section+"."+k+"="+v)
		default:
			t.Fatalf("unexpected config line %q", line)
		}
	}
	return out
}

// TestEnsureMinimalGitConfig_ExactKeys pins the exact keys and values the
// broker writes in each case; nothing else may ever appear.
func TestEnsureMinimalGitConfig_ExactKeys(t *testing.T) {
	core := func(version string) []string {
		return []string{"core.repositoryformatversion=" + version, "core.filemode=true", "core.bare=false", "core.logallrefupdates=true"}
	}
	origin := []string{"remote.origin.url=https://github.com/org/repo.git", "remote.origin.fetch=+refs/heads/*:refs/remotes/origin/*"}
	sha1 := "0123456789abcdef0123456789abcdef01234567"
	sha256 := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	for name, tc := range map[string]struct {
		files map[string]string
		url   string
		want  []string
	}{
		"sha1 branch with origin": {
			files: map[string]string{"HEAD": "ref: refs/heads/feature/x-1\n", "refs/heads/feature/x-1": sha1 + "\n"},
			url:   "https://github.com/org/repo.git",
			want:  append(append(core("0"), origin...), "branch.feature/x-1.remote=origin", "branch.feature/x-1.merge=refs/heads/feature/x-1"),
		},
		"sha256 detached with origin": {
			files: map[string]string{"HEAD": sha256 + "\n"},
			url:   "https://github.com/org/repo.git",
			want:  append(append(core("1"), "extensions.objectformat=sha256"), origin...),
		},
		"sha256 branch from packed-refs": {
			files: map[string]string{"HEAD": "ref: refs/heads/main\n", "packed-refs": "# pack-refs with: peeled\n" + sha256 + " refs/heads/main\n"},
			url:   "https://github.com/org/repo.git",
			want:  append(append(append(core("1"), "extensions.objectformat=sha256"), origin...), "branch.main.remote=origin", "branch.main.merge=refs/heads/main"),
		},
		"invalid branch name omitted": {
			files: map[string]string{"HEAD": "ref: refs/heads/feature/.hidden\n", "refs/heads/feature/.hidden": sha1 + "\n"},
			url:   "https://github.com/org/repo.git",
			want:  append(core("0"), origin...),
		},
		"branch without origin": {
			files: map[string]string{"HEAD": "ref: refs/heads/main\n", "refs/heads/main": sha1 + "\n"},
			url:   "",
			want:  core("0"),
		},
	} {
		t.Run(name, func(t *testing.T) {
			ws := t.TempDir()
			for rel, content := range tc.files {
				p := filepath.Join(ws, ".git", filepath.FromSlash(rel))
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if wrote, err := ensureMinimalGitConfig(ws, tc.url); !wrote || err != nil {
				t.Fatalf("wrote=%v err=%v", wrote, err)
			}
			b, err := os.ReadFile(filepath.Join(ws, ".git/config"))
			if err != nil {
				t.Fatal(err)
			}
			if got := parseGitConfig(t, string(b)); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("config keys:\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}

func TestValidBranchName(t *testing.T) {
	for _, ok := range []string{"main", "feature/x-1", "release/v1.2", "a_b", "x.y"} {
		if !validBranchName(ok) {
			t.Errorf("validBranchName(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{
		"", "-x", "/x", "x/", "x.", "a..b", "a//b", ".hidden", "feature/.hidden", "x.lock", "feature/x.lock/y",
		"a b", "a\"b", "a\\b", "a@{b", "a~b", "a^b", "a:b", "a?b", "a*b", "a[b", "ü", strings.Repeat("a", 201),
	} {
		if validBranchName(bad) {
			t.Errorf("validBranchName(%q) = true, want false", bad)
		}
	}
	if !quotedSubsectionSafe(branchNameChars) {
		t.Error("branchNameChars needs escaping in a quoted subsection")
	}
	if quotedSubsectionSafe("a\"b") || quotedSubsectionSafe(`a\b`) {
		t.Error("quotedSubsectionSafe accepts a quote or a backslash")
	}
}
