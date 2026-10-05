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
	"path/filepath"
	"strings"
)

// NormalizeCloneURL normalizes a project clone URL (a clone-url label or a git
// remote) to the form the Hub clones from. Explicit http(s)://, ssh://, git://
// and git@ URLs are preserved apart from dropping any query string, fragment
// and userinfo (an ssh:// login is kept; see StripGitURLCredentials). Local
// paths are returned unchanged. Schemeless remotes (e.g.
// "github.com/org/repo") lose any query string or fragment and are converted
// to an HTTPS clone URL via ToHTTPSCloneURL, which also drops any userinfo.
//
// This is the single source of truth shared by the Hub (which clones from the
// result) and the CLI (which reports it), so the two cannot drift.
func NormalizeCloneURL(cloneURL string) string {
	if cloneURL == "" {
		return ""
	}

	if filepath.IsAbs(cloneURL) || strings.HasPrefix(cloneURL, "./") || strings.HasPrefix(cloneURL, "../") {
		return cloneURL
	}

	cloneURL = stripURLQueryAndFragment(cloneURL)
	lower := strings.ToLower(cloneURL)
	for _, prefix := range []string{"http://", "https://", "ssh://", "git://"} {
		if strings.HasPrefix(lower, prefix) {
			return StripGitURLCredentials(cloneURL)
		}
	}
	if strings.HasPrefix(cloneURL, "git@") {
		return cloneURL
	}

	return ToHTTPSCloneURL(cloneURL)
}

// stripURLQueryAndFragment drops everything from the first '?' or '#'. Git
// remotes never need either, and both can carry tokens (?access_token=...).
func stripURLQueryAndFragment(remote string) string {
	if i := strings.IndexAny(remote, "?#"); i >= 0 {
		return remote[:i]
	}
	return remote
}

// ResolveCloneURL returns the URL the Hub clones a project from: the
// normalized clone-url label override when set, otherwise the normalized git
// remote.
func ResolveCloneURL(override, gitRemote string) string {
	if override = NormalizeCloneURL(override); override != "" {
		return override
	}
	return NormalizeCloneURL(gitRemote)
}

// Errors returned by ValidateCloneURLLabel.
var (
	ErrCloneURLUserinfo = errors.New("clone URL must not contain a username, password or token")
	ErrCloneURLQuery    = errors.New("clone URL must not contain a query string")
	ErrCloneURLFragment = errors.New("clone URL must not contain a fragment")
)

// ValidateCloneURLLabel reports whether value is a plain repository URL that
// may be stored as a project's clone-url label. Values carrying userinfo
// (https://user:pass@host/..., https://TOKEN@host/..., user:pass@host/...),
// a query string or a fragment are refused. A login alone is allowed where it
// is part of the git transport rather than a credential: scp-style
// "git@host:org/repo" and "ssh://git@host/org/repo". An empty value is valid.
func ValidateCloneURLLabel(value string) error {
	if value == "" {
		return nil
	}
	if strings.Contains(value, "?") {
		return ErrCloneURLQuery
	}
	if strings.Contains(value, "#") {
		return ErrCloneURLFragment
	}
	if strings.Contains(value, "://") {
		if StripGitURLCredentials(value) != value {
			return ErrCloneURLUserinfo
		}
		return nil
	}
	if filepath.IsAbs(value) || strings.HasPrefix(value, "./") || strings.HasPrefix(value, "../") {
		return nil
	}
	if schemelessUserinfoEnd(value) >= 0 {
		return ErrCloneURLUserinfo
	}
	return nil
}

// schemelessUserinfoEnd returns the index of the '@' that ends the userinfo of
// a schemeless remote (user:pass@host/path, TOKEN@host/path), or -1 when it
// has none. An '@' before the first '/' introduces userinfo; only the scp
// form (login@host:path, login without ':') carries a plain login, which is
// not userinfo here.
func schemelessUserinfoEnd(value string) int {
	authority := value
	if slash := strings.Index(authority, "/"); slash >= 0 {
		authority = authority[:slash]
	}
	at := strings.LastIndex(authority, "@")
	if at < 0 {
		return -1
	}
	login, host := authority[:at], authority[at+1:]
	if login != "" && !strings.ContainsAny(login, ":@") && strings.Contains(host, ":") {
		return -1
	}
	return at
}

// SanitizeGitSourceURL returns a user-entered git remote with any query
// string, fragment and userinfo removed, keeping the URL otherwise as entered.
// An ssh:// or scp-style login (git@host:org/repo) is kept, as in
// StripGitURLCredentials; http(s) and git URLs lose all userinfo, including a
// bare token. Local paths are returned unchanged.
func SanitizeGitSourceURL(value string) string {
	if value == "" || filepath.IsAbs(value) || strings.HasPrefix(value, "./") || strings.HasPrefix(value, "../") {
		return value
	}
	value = stripURLQueryAndFragment(value)
	if strings.Contains(value, "://") {
		return StripGitURLCredentials(value)
	}
	if at := schemelessUserinfoEnd(value); at >= 0 {
		return value[at+1:]
	}
	return value
}
