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
		{"other scheme userinfo stripped", "git+ssh://u:p@h/r", "https://git+ssh://h/r.git"},
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
		{"scp path with at sign kept", "git@host:repo@v1", "git@host:repo@v1"},
		{"other scheme userinfo stripped", "git+ssh://u:p@h/r", "git+ssh://h/r"},
		{"ssh second at sign dropped", "ssh://git@host/org/repo@v1", ""},
		{"schemeless port kept", "user:pw@host:8443/org/repo", "host:8443/org/repo"},
		{"schemeless empty host dropped", "user@/org/repo", ""},
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
