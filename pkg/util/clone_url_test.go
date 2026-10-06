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

package util

import (
	"errors"
	"strings"
	"testing"
)

func TestNormalizeCloneURL(t *testing.T) {
	tests := []struct {
		name, input, want string
	}{
		{"empty", "", ""},
		{"git scheme preserved", "git://172.17.0.1:9418/org/repo", "git://172.17.0.1:9418/org/repo"},
		{"ssh scheme preserved", "ssh://git@github.com/org/repo.git", "ssh://git@github.com/org/repo.git"},
		{"scp-style ssh preserved", "git@github.com:org/repo.git", "git@github.com:org/repo.git"},
		{"https preserved", "https://github.com/org/repo.git", "https://github.com/org/repo.git"},
		{"https without .git preserved", "https://github.com/org/repo", "https://github.com/org/repo"},
		{"http preserved", "http://forgejo:3000/org/repo.git", "http://forgejo:3000/org/repo.git"},
		{"uppercase scheme preserved", "HTTPS://github.com/org/repo", "HTTPS://github.com/org/repo"},
		{"schemeless normalized to https", "github.com/org/repo", "https://github.com/org/repo.git"},
		{"schemeless with .git", "github.com/org/repo.git", "https://github.com/org/repo.git"},
		{"absolute path preserved", "/tmp/source-repo", "/tmp/source-repo"},
		{"relative path preserved", "./repo", "./repo"},
		{"https userinfo stripped", "https://user:pass@github.com/org/repo.git", "https://github.com/org/repo.git"},
		{"https token-only userinfo stripped", "https://TOKEN@github.com/org/repo.git", "https://github.com/org/repo.git"},
		{"http login stripped", "http://deploy@internal.host/repo.git", "http://internal.host/repo.git"},
		{"ssh password stripped, login kept", "ssh://git:pass@github.com/org/repo.git", "ssh://git@github.com/org/repo.git"},
		{"git scheme userinfo stripped", "git://user:pass@host/org/repo", "git://host/org/repo"},
		{"schemeless userinfo stripped", "user:pass@github.com/org/repo", "https://github.com/org/repo.git"},
		{"https query stripped", "https://github.com/org/repo.git?access_token=x", "https://github.com/org/repo.git"},
		{"https fragment stripped", "https://github.com/org/repo.git#main", "https://github.com/org/repo.git"},
		{"https userinfo and query stripped", "https://TOKEN@github.com/org/repo.git?x=1#y", "https://github.com/org/repo.git"},
		{"ssh query stripped, login kept", "ssh://git@github.com/org/repo.git?x=1", "ssh://git@github.com/org/repo.git"},
		{"scp query stripped", "git@github.com:org/repo.git?x=1", "git@github.com:org/repo.git"},
		{"schemeless query stripped", "github.com/org/repo?access_token=x", "https://github.com/org/repo.git"},
		{"schemeless fragment stripped", "github.com/org/repo#frag", "https://github.com/org/repo.git"},
		{"local path with hash unchanged", "/tmp/repo#1", "/tmp/repo#1"},
		{"leading whitespace local path", " /tmp/source-repo", "/tmp/source-repo"},
		{"leading whitespace userinfo stripped", " https://u:p@github.com/org/repo.git", "https://github.com/org/repo.git"},
		{"git+ssh mapped like ssh, password dropped", "git+ssh://u:p@h/r", "ssh://u@h/r"},
		{"ssh+git mapped like ssh", "ssh+git://git@github.com/org/repo.git", "ssh://git@github.com/org/repo.git"},
		{"file scheme unchanged", "file:///srv/git/repo.git", "file:///srv/git/repo.git"},
		{"scp custom login to https", "deploy@host:org/repo", "https://host/org/repo.git"},
		{"scp custom login with .git to https", "deploy@internal.host:team/project.git", "https://internal.host/team/project.git"},
		{"scp git login kept", "git@github.com:org/repo", "git@github.com:org/repo"},
		{"scp ipv6 custom login to https", "deploy@[::1]:org/repo", "https://[::1]/org/repo.git"},
		{"scp extra at in host dropped", "git@PW@host:org/repo", ""},
		{"scp path at dropped", "git@user:PW@host:org/repo", ""},
		{"network-path reference userinfo dropped", "//user:PW@host/repo", ""},
		{"password that looks like a port dropped", "https://user:8443/x@host/repo", ""},
		{"embedded newline dropped", "https://host/r\nhttps://u:PW@h/x", ""},
		{"scheme-like suffix userinfo stripped", "user:PW@host/org/repo://", "https://host/org/repo:.git"},
		{"single-slash scheme dropped", "https:/user:PW@host/r", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NormalizeCloneURL(tt.input); got != tt.want {
				t.Fatalf("NormalizeCloneURL(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestResolveCloneURL(t *testing.T) {
	tests := []struct {
		name, override, remote, want string
	}{
		{"override wins", "git://host/org/repo", "github.com/org/repo", "git://host/org/repo"},
		{"schemeless override normalized", "github.com/other/repo", "github.com/org/repo", "https://github.com/other/repo.git"},
		{"falls back to remote", "", "github.com/org/repo", "https://github.com/org/repo.git"},
		{"remote scheme not doubled", "", "https://github.com/org/repo", "https://github.com/org/repo"},
		{"both empty", "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ResolveCloneURL(tt.override, tt.remote); got != tt.want {
				t.Fatalf("ResolveCloneURL(%q, %q) = %q, want %q", tt.override, tt.remote, got, tt.want)
			}
		})
	}
}

func TestValidateCloneURLLabel(t *testing.T) {
	tests := []struct {
		name, input string
		want        error
	}{
		{"empty", "", nil},
		{"clean https", "https://github.com/org/repo.git", nil},
		{"clean https with port", "https://git.example.com:8443/org/repo.git", nil},
		{"clean http", "http://forgejo:3000/org/repo.git", nil},
		{"schemeless", "github.com/org/repo", nil},
		{"schemeless with port", "git.example.com:8443/org/repo", nil},
		{"scp-style", "git@github.com:org/repo.git", nil},
		{"scp-style custom login", "deploy@internal.host:team/project", nil},
		{"ssh login", "ssh://git@github.com/org/repo.git", nil},
		{"ssh custom login", "ssh://deploy@internal.host/team/project", nil},
		{"git scheme", "git://172.17.0.1:9418/org/repo", nil},
		{"absolute path", "/tmp/source-repo", nil},
		{"relative path", "./repo", nil},
		{"local path with hash", "/tmp/repo#1", nil},
		{"local path with query", "./repo?x", nil},
		{"at sign in path", "https://github.com/org/repo@v1", ErrCloneURLUserinfo},
		{"password that looks like a port", "https://user:8443/x@host/repo", ErrCloneURLUserinfo},
		{"scheme-like suffix", "user:PW@host/org/repo://", ErrCloneURLUserinfo},
		{"single-slash scheme", "https:/user:PW@host/r", ErrCloneURLUserinfo},
		{"embedded newline", "https://host/r\nhttps://u:PW@h/x", ErrCloneURLInvalid},
		{"leading space", " https://github.com/org/repo", ErrCloneURLInvalid},
		{"tab", "https://github.com/org/\trepo", ErrCloneURLInvalid},
		{"non-ASCII", "https://github.com/org/r\u00e9po", ErrCloneURLInvalid},
		{"scp path with at sign", "git@host:repo@v1", ErrCloneURLUserinfo},
		{"scp extra at in host", "git@PW@host:org/repo", ErrCloneURLUserinfo},
		{"scp many at signs", "git@a@b@host:x", ErrCloneURLUserinfo},
		{"scp ipv6 host", "deploy@[::1]:org/repo", nil},
		{"scp bracket missing close", "deploy@[::1:org/repo", ErrCloneURLUserinfo},
		{"scp bracket with slash", "deploy@[a/b]:org/repo", ErrCloneURLUserinfo},
		{"scp non-ipv6 bracket", "deploy@[notipv6]:org/repo", ErrCloneURLUserinfo},
		{"scp ipv6 zone id", "deploy@[fe80::1%25eth0]:org/repo", ErrCloneURLUserinfo},
		{"query inside password", "https://user:P?W@host/org/repo", ErrCloneURLQuery},
		{"schemeless ipv6 userinfo", "user:PW@[::1]/org/repo", ErrCloneURLUserinfo},
		{"scp userinfo in path", "git@user:PW@host:org/repo", ErrCloneURLUserinfo},
		{"network-path reference userinfo", "//user:PW@host/repo", ErrCloneURLUserinfo},
		{"scp token login is ambiguous and accepted", "TOKEN@github.com:org/repo", nil},
		{"ssh two at signs", "ssh://git@host/org/repo@v1", ErrCloneURLUserinfo},
		{"git+ssh login refused", "git+ssh://git@host/org/repo", ErrCloneURLUserinfo},
		{"mixed-case scheme ssh login", "SSH://git@github.com/org/repo", nil},
		{"ipv6 host", "https://[::1]:8443/org/repo", nil},
		{"file scheme", "file:///tmp/repo", nil},
		{"percent-encoded at in userinfo", "https://user%40x:pw@host/repo", ErrCloneURLUserinfo},
		{"https user and password", "https://user:pass@github.com/org/repo", ErrCloneURLUserinfo},
		{"https token-only userinfo", "https://TOKEN@github.com/org/repo", ErrCloneURLUserinfo},
		{"https empty password", "https://user:@github.com/org/repo", ErrCloneURLUserinfo},
		{"http login only", "http://deploy@internal.host/repo", ErrCloneURLUserinfo},
		{"uppercase scheme userinfo", "HTTPS://TOKEN@github.com/org/repo", ErrCloneURLUserinfo},
		{"ssh with password", "ssh://git:pass@github.com/org/repo", ErrCloneURLUserinfo},
		{"git scheme userinfo", "git://user@host/org/repo", ErrCloneURLUserinfo},
		{"schemeless user and password", "user:pass@github.com/org/repo", ErrCloneURLUserinfo},
		{"schemeless token-only", "TOKEN@github.com/org/repo", ErrCloneURLUserinfo},
		{"scp-style with password", "git:pass@github.com:org/repo", ErrCloneURLUserinfo},
		{"scp-style empty login", "@github.com:org/repo", ErrCloneURLUserinfo},
		{"query token", "https://github.com/org/repo.git?access_token=x", ErrCloneURLQuery},
		{"query on scp", "git@github.com:org/repo.git?x=1", ErrCloneURLQuery},
		{"fragment", "https://github.com/org/repo.git#main", ErrCloneURLFragment},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ValidateCloneURLLabel(tt.input)
			if !errors.Is(got, tt.want) || (tt.want == nil && got != nil) {
				t.Fatalf("ValidateCloneURLLabel(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestSanitizeGitSourceURL(t *testing.T) {
	tests := []struct {
		name, input, want string
	}{
		{"empty", "", ""},
		{"clean https", "https://github.com/org/repo.git", "https://github.com/org/repo.git"},
		{"clean schemeless", "github.com/org/repo", "github.com/org/repo"},
		{"scp-style login kept", "git@github.com:org/repo.git", "git@github.com:org/repo.git"},
		{"scp-style custom login kept", "deploy@internal.host:team/project", "deploy@internal.host:team/project"},
		{"ssh login kept", "ssh://git@github.com/org/repo.git", "ssh://git@github.com/org/repo.git"},
		{"local path unchanged", "/tmp/repo#1", "/tmp/repo#1"},
		{"https user and password", "https://user:pass@github.com/org/repo.git", "https://github.com/org/repo.git"},
		{"https token-only", "https://TOKEN@github.com/org/repo", "https://github.com/org/repo"},
		{"http login only", "http://deploy@internal.host/repo", "http://internal.host/repo"},
		{"ssh password dropped, login kept", "ssh://git:pass@github.com/org/repo.git", "ssh://git@github.com/org/repo.git"},
		{"schemeless user and password", "user:pass@github.com/org/repo", "github.com/org/repo"},
		{"schemeless token-only", "TOKEN@github.com/org/repo", "github.com/org/repo"},
		{"query", "https://github.com/org/repo.git?access_token=x", "https://github.com/org/repo.git"},
		{"fragment", "https://github.com/org/repo.git#main", "https://github.com/org/repo.git"},
		{"scp query", "git@github.com:org/repo.git?x=1", "git@github.com:org/repo.git"},
		{"userinfo, query and fragment", "https://TOKEN@github.com/org/repo?x=1#y", "https://github.com/org/repo"},
		{"inner space dropped", "https://github.com/org/re po", ""},
		{"control character dropped", "github.com/org/\x7frepo", ""},
		{"surrounding whitespace trimmed", "  https://github.com/org/repo\n", "https://github.com/org/repo"},
		{"password that looks like a port dropped", "https://user:8443/x@host/repo", ""},
		{"at sign in path dropped", "https://github.com/org/repo@v1", ""},
		{"scheme-like suffix", "user:PW@host/org/repo://", "host/org/repo://"},
		{"single-slash scheme dropped", "https:/user:PW@host/r", ""},
		{"embedded newline dropped", "https://host/r\nhttps://u:PW@h/x", ""},
		{"scp path with at sign dropped", "git@host:repo@v1", ""},
		{"scp userinfo in path dropped", "git@user:PW@host:org/repo", ""},
		{"scp extra at in host dropped", "git@PW@host:org/repo", ""},
		{"scp many at signs dropped", "git@a@b@host:x", ""},
		{"network-path reference userinfo dropped", "//user:PW@host/repo", ""},
		{"network-path reference without userinfo kept", "//host/repo", "//host/repo"},
		{"other scheme userinfo stripped", "git+ssh://u:p@h/r", "git+ssh://h/r"},
		{"ssh second at sign dropped", "ssh://git@host/org/repo@v1", ""},
		{"schemeless port kept", "user:pw@host:8443/org/repo", "host:8443/org/repo"},
		{"schemeless empty host dropped", "user@/org/repo", ""},
		{"schemeless ipv6 host userinfo stripped", "user:PW@[::1]/org/repo", "[::1]/org/repo"},
		{"schemeless ipv6 host with port userinfo stripped", "user:PW@[::1]:8443/org/repo", "[::1]:8443/org/repo"},
		{"schemeless bad bracket host dropped", "user:PW@[zz:yy]/org/repo", ""},
		{"scp ipv6 host kept", "deploy@[::1]:org/repo", "deploy@[::1]:org/repo"},
		{"scp ipv6 host path at dropped", "deploy@[::1]:org/repo@v1", ""},
		{"query inside password dropped", "https://user:P?W@host/org/repo", ""},
		{"fragment inside password dropped", "https://user:P#W@host/org/repo", ""},
		{"schemeless query inside password dropped", "user:P?W@host/org/repo", ""},
		{"scp bracket missing close dropped", "deploy@[::1:org/repo", ""},
		{"scp bracket with slash dropped", "deploy@[a/b]:org/repo", ""},
		{"scp non-ipv6 bracket dropped", "deploy@[notipv6]:org/repo", ""},
		{"schemeless non-ipv6 bracket with port dropped", "user:PW@[zz]:80/org/repo", ""},
		{"ipv6 zone id dropped", "user:PW@[fe80::1%25eth0]/org/repo", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SanitizeGitSourceURL(tt.input); got != tt.want {
				t.Fatalf("SanitizeGitSourceURL(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestSplitScheme(t *testing.T) {
	tests := []struct {
		in, scheme string
		ok         bool
	}{
		{"https://host/r", "https", true},
		{"HTTPS://host/r", "https", true},
		{"git+ssh://host/r", "git+ssh", true},
		{"user:PW@host/org/repo://", "", false},
		{"https:/host/r", "", false},
		{"://host/r", "", false},
		{"1http://host/r", "", false},
		{"host/r", "", false},
	}
	for _, tt := range tests {
		scheme, _, ok := splitScheme(tt.in)
		if scheme != tt.scheme || ok != tt.ok {
			t.Errorf("splitScheme(%q) = %q, %v; want %q, %v", tt.in, scheme, ok, tt.scheme, tt.ok)
		}
	}
}

func TestHTTPSCloneURL(t *testing.T) {
	tests := []struct {
		name, input, want string
	}{
		{"https", "https://github.com/org/repo", "https://github.com/org/repo.git"},
		{"schemeless", "github.com/org/repo", "https://github.com/org/repo.git"},
		{"scp git login", "git@github.com:org/repo.git", "https://github.com/org/repo.git"},
		{"scp custom login", "deploy@host:org/repo", "https://host/org/repo.git"},
		{"ssh login", "ssh://git@github.com/org/repo", "https://github.com/org/repo.git"},
		{"ssh port dropped", "ssh://git@host:22/org/repo", "https://host/org/repo.git"},
		{"https port kept", "https://host:8443/org/repo", "https://host:8443/org/repo.git"},
		{"https userinfo and query", "https://user:PW@github.com/org/repo?x=1", "https://github.com/org/repo.git"},
		{"scp userinfo in path", "git@user:PW@host:org/repo", ""},
		{"git+ssh like ssh", "git+ssh://git@host/x", "https://host/x.git"},
		{"ssh+git like ssh with port", "ssh+git://git@host:2222/org/repo", "https://host/org/repo.git"},
		{"scp ipv6 host", "deploy@[::1]:org/repo", "https://[::1]/org/repo.git"},
		{"ssh ipv6 host with port", "ssh://git@[::1]:22/org/repo", "https://[::1]/org/repo.git"},
		{"query inside password", "https://user:P?W@host/org/repo", ""},
		{"fragment inside password", "https://user:P#W@host/org/repo", ""},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := HTTPSCloneURL(tt.input)
			if got != tt.want {
				t.Fatalf("HTTPSCloneURL(%q) = %q, want %q", tt.input, got, tt.want)
			}
			if strings.Contains(got, "PW") {
				t.Fatalf("credential survived in %q", got)
			}
		})
	}
}

// TestNormalizeGitRemote_NoAtForLegitRemotes pins the precondition of the
// hub's git remote '@' guard: ordinary remotes normalize without '@', while
// an scp remote with a password in the path keeps one (and is refused).
func TestNormalizeGitRemote_NoAtForLegitRemotes(t *testing.T) {
	for _, r := range []string{
		"https://github.com/org/repo", "https://user:PW@github.com/org/repo",
		"https://TOKEN@github.com/org/repo.git", "ssh://git@github.com/org/repo.git",
		"git@github.com:org/repo.git", "github.com/org/repo", "http://forgejo:3000/org/repo.git",
	} {
		if got := NormalizeGitRemote(r); strings.Contains(got, "@") {
			t.Errorf("NormalizeGitRemote(%q) = %q, contains '@'", r, got)
		}
	}
	if got := NormalizeGitRemote("git@user:PW@host:org/repo"); !strings.Contains(got, "@") {
		t.Errorf("NormalizeGitRemote(scp with password) = %q; the hub guard relies on the '@' remaining", got)
	}
}

func TestCutQueryAndFragment(t *testing.T) {
	tests := []struct {
		in, want string
		ok       bool
	}{
		{"https://host/org/repo", "https://host/org/repo", true},
		{"https://host/org/repo?access_token=x", "https://host/org/repo", true},
		{"https://host/org/repo#frag", "https://host/org/repo", true},
		{"https://user:P?W@host/org/repo", "", false},
		{"https://user:P#W@host/org/repo", "", false},
		{"https://host/org/repo?x=a@b", "", false},
	}
	for _, tt := range tests {
		got, ok := CutQueryAndFragment(tt.in)
		if got != tt.want || ok != tt.ok {
			t.Errorf("CutQueryAndFragment(%q) = %q, %v; want %q, %v", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}

// TestPasswordWithQueryOrFragmentCharNotKept checks that no part of a
// password containing '?' or '#' survives sanitizing, normalizing or the
// HTTPS clone URL.
func TestPasswordWithQueryOrFragmentCharNotKept(t *testing.T) {
	for _, in := range []string{
		"https://user:PSECRET?WSECRET@host/org/repo",
		"https://user:PSECRET#WSECRET@host/org/repo",
		"user:PSECRET?WSECRET@host/org/repo",
	} {
		for name, got := range map[string]string{
			"SanitizeGitSourceURL": SanitizeGitSourceURL(in),
			"NormalizeCloneURL":    NormalizeCloneURL(in),
			"HTTPSCloneURL":        HTTPSCloneURL(in),
		} {
			if strings.Contains(got, "SECRET") {
				t.Errorf("%s(%q) = %q keeps part of the password", name, in, got)
			}
		}
	}
}

func TestNormalizeGitRemote_IPv6SCP(t *testing.T) {
	if got := NormalizeGitRemote("git@[::1]:org/repo.git"); got != "[::1]/org/repo" {
		t.Fatalf("NormalizeGitRemote(git@[::1]:org/repo.git) = %q, want %q", got, "[::1]/org/repo")
	}
	if got := NormalizeGitRemote("git@github.com:org/repo.git"); got != "github.com/org/repo" {
		t.Fatalf("NormalizeGitRemote(git@github.com:org/repo.git) = %q", got)
	}
}
