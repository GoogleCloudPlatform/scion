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
	"regexp"
	"strings"
)

// NormalizeCloneURL normalizes a project clone URL (a clone-url label or a git
// remote) to the form the Hub clones from. Surrounding whitespace is trimmed
// and local paths are returned unchanged. Every other value is first passed
// through SanitizeGitSourceURL, so the result never carries userinfo (an ssh
// or scp-style login is kept), a query string or a fragment; a value that
// cannot be sanitized unambiguously normalizes to "" (ResolveCloneURL then
// falls back to the git remote). Explicit http(s)://, ssh://, git:// and git@
// URLs are then kept as they are; other remotes (e.g. "github.com/org/repo")
// are converted to an HTTPS clone URL via ToHTTPSCloneURL.
//
// This is the single source of truth shared by the Hub (which clones from the
// result) and the CLI (which reports it), so the two cannot drift.
func NormalizeCloneURL(cloneURL string) string {
	cloneURL = trimSpaceEdges(cloneURL)
	if cloneURL == "" || isLocalPath(cloneURL) {
		return cloneURL
	}

	cloneURL = SanitizeGitSourceURL(cloneURL)
	if cloneURL == "" {
		return ""
	}
	lower := strings.ToLower(cloneURL)
	for _, prefix := range []string{"http://", "https://", "ssh://", "git://"} {
		if strings.HasPrefix(lower, prefix) {
			return cloneURL
		}
	}
	if strings.HasPrefix(cloneURL, "git@") {
		return cloneURL
	}

	return ToHTTPSCloneURL(cloneURL)
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

// StripQueryAndFragment drops everything from the first '?' or '#'. Git
// remotes never need either, and both can carry tokens (?access_token=...).
func StripQueryAndFragment(remote string) string {
	if i := strings.IndexAny(remote, "?#"); i >= 0 {
		return remote[:i]
	}
	return remote
}

// IsPrintableASCII reports whether s contains only printable, non-space
// ASCII (0x21-0x7E). This rejects whitespace, control and format characters
// (e.g. RTL overrides) and non-ASCII homoglyphs; IDN hosts must be given in
// punycode (xn--...).
func IsPrintableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// trimSpaceEdges trims ASCII whitespace from both ends of s.
func trimSpaceEdges(s string) string {
	return strings.Trim(s, " \t\n\v\f\r")
}

// isLocalPath reports whether s is a local filesystem path (absolute, or
// relative with an explicit ./ or ../ prefix) rather than a remote URL.
func isLocalPath(s string) bool {
	return filepath.IsAbs(s) || strings.HasPrefix(s, "./") || strings.HasPrefix(s, "../")
}

// uriScheme matches an RFC 3986 scheme: ALPHA *( ALPHA / DIGIT / "+" / "-" / "." ).
var uriScheme = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*$`)

// splitScheme splits value into its scheme and the text after "://". ok is
// false unless value starts with an RFC 3986 scheme followed by "://"; a
// "://" later in the value (user:pass@host/repo://) does not make it a
// scheme URL, and a single slash (https:/host) is not a scheme separator.
func splitScheme(value string) (scheme, rest string, ok bool) {
	i := strings.Index(value, "://")
	if i <= 0 || !uriScheme.MatchString(value[:i]) {
		return "", "", false
	}
	return strings.ToLower(value[:i]), value[i+len("://"):], true
}

// isSSHScheme reports whether scheme carries the ssh transport, where the
// userinfo login selects the account and is not a secret.
func isSSHScheme(scheme string) bool {
	return scheme == "ssh"
}

// scpLogin matches the login of an scp-style remote (login@host:path).
var scpLogin = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// splitSCP splits an scp-style remote "login@host:path" (no scheme) at its
// first '@' and the first ':' after it. ok is false unless the login matches
// scpLogin and the host is non-empty with no '/'. The path is returned as
// is and may itself contain '@'.
//
// Note: the login is not a secret in this form, so a value such as
// "TOKEN@github.com:org/repo" is indistinguishable from an ordinary login and
// is accepted. That ambiguity is inherent to scp syntax; credentials belong
// in project secrets or the GitHub App, not in the remote.
//
// It is intentional that ValidateCloneURLLabel refuses "git@host:repo@v1"
// while SanitizeGitSourceURL keeps it for the source-url label: the login is
// transport, and the '@' in the path is ref-like and carries no secret.
func splitSCP(value string) (login, host, path string, ok bool) {
	login, rest, found := strings.Cut(value, "@")
	if !found || !scpLogin.MatchString(login) {
		return "", "", "", false
	}
	host, path, found = strings.Cut(rest, ":")
	if !found || host == "" || strings.Contains(host, "/") {
		return "", "", "", false
	}
	return login, host, path, true
}

// Errors returned by ValidateCloneURLLabel.
var (
	ErrCloneURLInvalid  = errors.New("clone URL must contain only printable, non-space ASCII characters")
	ErrCloneURLUserinfo = errors.New("clone URL must not contain a username, password or token")
	ErrCloneURLQuery    = errors.New("clone URL must not contain a query string")
	ErrCloneURLFragment = errors.New("clone URL must not contain a fragment")
)

// ValidateCloneURLLabel reports whether value is a plain repository URL that
// may be stored as a project's clone-url label. An empty value is valid, and
// local paths are accepted as given. Otherwise the value must be printable,
// non-space ASCII with no query string or fragment, and must not contain '@'
// except as the login of the git transport: scp-style "git@host:org/repo" or
// "ssh://git@host/org/repo". Any other '@' is refused:
// before the first '/' it introduces userinfo (https://user:pass@host/...,
// https://TOKEN@host/..., user:pass@host/...), and after it the text could
// still be a password that looks like a port (https://user:8443/x@host/...).
func ValidateCloneURLLabel(value string) error {
	if value == "" {
		return nil
	}
	if !IsPrintableASCII(value) {
		return ErrCloneURLInvalid
	}
	if isLocalPath(value) {
		return nil
	}
	if strings.Contains(value, "?") {
		return ErrCloneURLQuery
	}
	if strings.Contains(value, "#") {
		return ErrCloneURLFragment
	}
	if !strings.Contains(value, "@") {
		return nil
	}
	if scheme, rest, ok := splitScheme(value); ok {
		if isSSHScheme(scheme) && strings.Count(rest, "@") == 1 {
			authority, _, _ := strings.Cut(rest, "/")
			if login, _, found := strings.Cut(authority, "@"); found && scpLogin.MatchString(login) {
				return nil
			}
		}
		return ErrCloneURLUserinfo
	}
	if _, _, path, ok := splitSCP(value); ok && !strings.Contains(path, "@") {
		return nil
	}
	return ErrCloneURLUserinfo
}

// SanitizeGitSourceURL returns a user-entered git remote with any query
// string, fragment and userinfo removed, keeping the URL otherwise as entered.
// Surrounding whitespace is trimmed and local paths are returned unchanged.
// An ssh:// or scp-style login (git@host:org/repo) is kept; http(s), git and
// other schemes lose all userinfo, including a bare token. When a credential
// cannot be removed unambiguously — the value contains other whitespace or
// control characters, or an '@' remains outside a transport login (for
// example a password that looks like a port, https://user:8443/x@host/repo)
// — the result is "" so that nothing which might be a secret is kept.
func SanitizeGitSourceURL(value string) string {
	value = trimSpaceEdges(value)
	if value == "" || isLocalPath(value) {
		return value
	}
	if !IsPrintableASCII(value) {
		return ""
	}
	value = StripQueryAndFragment(value)
	if !strings.Contains(value, "@") {
		return value
	}
	if scheme, rest, ok := splitScheme(value); ok {
		prefix := value[:len(value)-len(rest)]
		rest = StripGitURLCredentials(prefix + rest)[len(prefix):]
		authority, path, _ := strings.Cut(rest, "/")
		if strings.Contains(path, "@") {
			return ""
		}
		if login, _, found := strings.Cut(authority, "@"); found {
			if !isSSHScheme(scheme) || !scpLogin.MatchString(login) || strings.Count(authority, "@") != 1 {
				return ""
			}
		}
		return prefix + rest
	}
	if _, _, _, ok := splitSCP(value); ok {
		return value
	}
	// Schemeless host/path with userinfo before the first '/'
	// (user:pass@host/path, TOKEN@host/path): drop through the last '@' of
	// the authority. An '@' left in the path is ambiguous.
	authority, path, hasPath := strings.Cut(value, "/")
	if at := strings.LastIndex(authority, "@"); at >= 0 {
		authority = authority[at+1:]
	}
	if authority == "" || strings.Contains(path, "@") {
		return ""
	}
	if hasPath {
		return authority + "/" + path
	}
	return authority
}
