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
	"bytes"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// RedactGitConfigSecrets parses a git config file and returns an equivalent
// config with credential-bearing material removed, together with the names of
// the keys it changed. The returned names are themselves redacted: a subsection
// that carries userinfo is never included verbatim (see notedKeyName), so neither
// the values nor the key names carry userinfo. It is used to seed a per-agent
// private .git/config so a copy of a linked checkout's host config does not carry
// the host user's credentials into the agent.
//
// The key policy is data, in gitConfigDropRules, gitConfigURLRules and
// gitConfigRemoteRefRules; the tests enumerate those tables, so a key added to
// the policy without a test case fails the test suite.
//
// It strips:
//   - every key in gitConfigDropRules, with or without a subsection,
//   - userinfo from the values of the keys in gitConfigURLRules, and of the keys
//     in gitConfigRemoteRefRules when the value is a URL or a remote-helper form
//     rather than a remote name (a remote name is kept unchanged); from
//     url.<base>.*, http.<base>.* and lfs.<base>.* subsection
//     headers; and from the <url> in an includeIf "hasconfig:remote.*.url:<url>"
//     subsection (http/https/ftp/ftps userinfo removed; any other scheme with a
//     password has all userinfo removed, username-only kept; remote-helper
//     "transport::url" forms handled recursively),
//   - any such URL-valued key or subsection that is URL-shaped ("://") and cannot
//     be parsed but still carries userinfo (the key is dropped entirely), and
//   - a http.proxy or remote.*.proxy value in the schemeless proxy form
//     user:password@host[:port] (which git accepts without a scheme): such a value
//     is dropped. Schemeless user:password@host is proxy syntax only; for remote
//     urls and url/includeIf bases a host:path form is scp and is kept.
//
// scp-style git remotes and url bases (git@host:path, with no "://") and an ssh
// URL with only a username (no password) are kept unchanged.
//
// The returned config is re-serialised from the parsed variables: comments are
// not preserved, but every retained key/value (including multi-valued keys and
// their order) is. A parse error is returned unchanged so the caller can fail
// closed rather than seed an unvetted copy.
func RedactGitConfigSecrets(raw []byte) (out []byte, stripped []string, err error) {
	vars, err := parseGitConfigFile(raw)
	if err != nil {
		return nil, nil, err
	}
	kept, stripped := redactGitConfigVars(vars)
	return serializeGitConfig(kept), stripped, nil
}

// redactGitConfigVars applies RedactGitConfigSecrets' key policy to vars that
// are already parsed and returns the variables to keep, with userinfo scrubbed,
// together with the redacted names of the keys it changed or dropped. It reads
// no file and runs no git; vars itself is not modified.
func redactGitConfigVars(vars []gitConfigVar) (kept []gitConfigVar, stripped []string) {
	kept = make([]gitConfigVar, 0, len(vars))
	seen := map[string]struct{}{}
	note := func(key string) {
		if _, ok := seen[key]; !ok {
			seen[key] = struct{}{}
			stripped = append(stripped, key)
		}
	}
	for _, v := range vars {
		// Keys in gitConfigDropRules — drop entirely.
		if isDroppedKey(v) {
			note(notedKeyName(v))
			continue
		}
		// Userinfo in the keys of gitConfigURLRules — rewrite the value, or drop the
		// key when the value is URL-shaped, cannot be parsed, but still carries
		// userinfo. For the proxy keys only, also drop the schemeless proxy form
		// user:password@host. The keys of gitConfigRemoteRefRules take the same path
		// only when the value is a URL or remote-helper form (see remoteRefIsURL); a
		// remote name never does.
		if isURLValuedKey(v) || (isRemoteRefKey(v) && remoteRefIsURL(v.Value)) {
			clean, changed, drop := sanitizeURLUserinfo(v.Value)
			if !drop && isProxyKey(v) && dropSchemelessProxyUserinfo(v.Value) {
				drop = true
			}
			if drop {
				note(notedKeyName(v))
				continue
			}
			if changed {
				note(notedKeyName(v))
				v.Value = clean
			}
		}
		// includeIf "hasconfig:remote.*.url:<url>" — sanitize the <url> after the
		// prefix; drop the key if that would drop. A rewrite keeps the prefix
		// bytes as written. The prefix is matched case-insensitively on purpose
		// (see hasConfigURLPrefix); git itself matches it case-sensitively.
		if v.Section == "includeif" && v.HasSubsection && hasPrefixFold(v.Subsection, hasConfigURLPrefix) {
			clean, changed, drop := sanitizeURLUserinfo(v.Subsection[len(hasConfigURLPrefix):])
			if drop {
				note(notedKeyName(v))
				continue
			}
			if changed {
				note(notedKeyName(v))
				v.Subsection = v.Subsection[:len(hasConfigURLPrefix)] + clean
			}
		}
		// Userinfo in a url.<base>.*, http.<base>.* or lfs.<base>.* subsection (the
		// base is the URL that keys the section) — rewrite the subsection, or drop the
		// key when the base is URL-shaped, cannot be parsed, but still carries userinfo.
		if (v.Section == "url" || v.Section == "http" || v.Section == "lfs") && v.HasSubsection {
			clean, changed, drop := sanitizeURLUserinfo(v.Subsection)
			if drop {
				note(notedKeyName(v))
				continue
			}
			if changed {
				note(notedKeyName(v))
				v.Subsection = clean
			}
		}
		kept = append(kept, v)
	}
	return kept, stripped
}

// StripGitConfigKeys re-serialises raw with every variable for which drop
// returns true removed. section and name are lower-cased (as git stores them);
// subsection keeps its case. It is used to remove pre-existing branch-upstream
// and autoSetupRemote entries before a fresh upstream pre-seed is appended, so
// the result never has duplicate single-valued keys. A parse error is returned
// so the caller can fail closed.
func StripGitConfigKeys(raw []byte, drop func(section, subsection string, hasSubsection bool, name string) bool) ([]byte, error) {
	vars, err := parseGitConfigFile(raw)
	if err != nil {
		return nil, err
	}
	kept := make([]gitConfigVar, 0, len(vars))
	for _, v := range vars {
		if drop(v.Section, v.Subsection, v.HasSubsection, v.Name) {
			continue
		}
		kept = append(kept, v)
	}
	return serializeGitConfig(kept), nil
}

// gitConfigKeyRule names a config key by its lower-cased section and variable
// name, with or without a subsection; Name "*" matches every variable of the
// section.
type gitConfigKeyRule struct {
	Section string
	Name    string
}

// matches reports whether v is the key r names.
func (r gitConfigKeyRule) matches(v gitConfigVar) bool {
	return v.Section == r.Section && (r.Name == "*" || v.Name == r.Name)
}

// gitConfigDropRules are the keys dropped entirely: credential-helper
// configuration, authorization headers, cookie files, client TLS key and
// certificate paths, and plaintext passwords. The CA trust settings
// (http.sslCAInfo, http.sslCAPath) are deliberately kept.
// The matching username keys (imap.user, sendemail.smtpuser, git-p4.user,
// gitcvs.dbuser) are deliberately kept.
var gitConfigDropRules = []gitConfigKeyRule{
	{"credential", "*"},
	{"http", "extraheader"},
	{"http", "cookiefile"},
	{"http", "sslkey"},  // http.sslKey and http.<url>.sslKey: the client TLS private key path
	{"http", "sslcert"}, // http.sslCert and http.<url>.sslCert: the client certificate path
	{"sendemail", "smtppass"},
	{"imap", "pass"},
	{"git-p4", "password"},
	{"gitcvs", "dbpass"}, // gitcvs.dbPass and gitcvs.<access method>.dbPass
}

// gitConfigURLRules are the keys whose value is, or may be, a URL (or a proxy
// address); userinfo in that value is scrubbed.
var gitConfigURLRules = []gitConfigKeyRule{
	{"remote", "url"},
	{"remote", "pushurl"},
	{"remote", "lfsurl"},
	{"remote", "lfspushurl"},
	{"lfs", "url"},
	{"lfs", "pushurl"},
	{"url", "insteadof"},
	{"url", "pushinsteadof"},
	{"http", "proxy"},   // http.proxy and http.<url>.proxy
	{"remote", "proxy"}, // remote.<name>.proxy
	{"submodule", "url"},
	{"bundle", "uri"},      // bundle.<id>.uri
	{"fetch", "bundleuri"}, // fetch.bundleURI
	{"imap", "host"},       // imap.host: an imap:// or imaps:// URL, or a bare host name (kept)
	{"svn-remote", "url"},  // git-svn
	{"svn-remote", "pushurl"},
	{"svn-remote", "commiturl"},   // git-svn: svn-remote.<name>.commiturl
	{"svn", "commiturl"},          // git-svn: svn.commiturl (no subsection)
	{"svn-remote", "rewriteroot"}, // git-svn: svn-remote.<name>.rewriteRoot
}

// gitConfigRemoteRefRules are the keys whose value names a remote but may
// instead be a URL (git accepts either); they are scrubbed like a URL-valued
// key only when remoteRefIsURL reports a URL.
var gitConfigRemoteRefRules = []gitConfigKeyRule{
	{"branch", "remote"},
	{"branch", "pushremote"},
	{"remote", "pushdefault"},
}

// keyMatchesAny reports whether v is named by any rule in rules.
func keyMatchesAny(rules []gitConfigKeyRule, v gitConfigVar) bool {
	for _, r := range rules {
		if r.matches(v) {
			return true
		}
	}
	return false
}

// isDroppedKey reports whether a config variable is dropped entirely.
func isDroppedKey(v gitConfigVar) bool {
	return keyMatchesAny(gitConfigDropRules, v)
}

// isURLValuedKey reports whether a config variable's value is a URL whose
// userinfo should be scrubbed.
func isURLValuedKey(v gitConfigVar) bool {
	return keyMatchesAny(gitConfigURLRules, v)
}

// isRemoteRefKey reports whether a config variable names a remote that may be
// given as a URL instead (gitConfigRemoteRefRules).
func isRemoteRefKey(v gitConfigVar) bool {
	return keyMatchesAny(gitConfigRemoteRefRules, v)
}

// remoteRefIsURL reports whether a remote-reference value is a URL or a
// remote-helper form rather than a remote name: it contains "://" or starts
// with a "<transport>::" prefix. A remote name contains neither, so it is kept
// unchanged; an scp-style host:path value is not touched either.
func remoteRefIsURL(s string) bool {
	t := strings.TrimSpace(s)
	return strings.Contains(t, "://") || helperPrefixRe.MatchString(t)
}

// isProxyKey reports whether v is a proxy key (http.proxy, http.<url>.proxy or
// remote.<name>.proxy) — the only keys for which a schemeless user:password@host
// value is treated as proxy userinfo rather than an scp host:path.
func isProxyKey(v gitConfigVar) bool {
	return (v.Section == "http" || v.Section == "remote") && v.Name == "proxy"
}

// helperPrefixRe matches a git remote-helper transport prefix, e.g. "http::" in
// "http::https://host/x". It is "<transport>::" where <transport> is a scheme-like
// token (a letter then letters/digits/+/-/.) followed by "::". A normal URL scheme
// ("https://") has a single ":" then "//", so it does not match.
var helperPrefixRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*::`)

// sanitizeURLUserinfo strips userinfo from a git config URL, returning the cleaned
// string, whether it changed, and whether the caller should drop the whole key. It
// is shared by both callers: URL-valued keys (remote.*.url and friends) and
// url.<base>/http.<base>/lfs.<base> subsection headers. Applied in order:
//
//   - A remote-helper prefix "<transport>::<url>" is split off; the remainder is
//     sanitized recursively and the prefix re-prepended (changed/drop propagate).
//   - A parseable http/https/ftp/ftps URL that carries userinfo has it removed
//     entirely (changed).
//   - A parseable URL of any other scheme that carries a password has ALL userinfo
//     removed (e.g. ssh://u:pw@h -> ssh://h, socks5 likewise), changed; with no
//     password it is kept unchanged (e.g. ssh://u@h, scp-like ssh users).
//   - A string that will not parse but is URL-shaped ("://") and still carries
//     userinfo before the path is dropped, so the userinfo cannot survive.
//   - Anything else (e.g. an scp-style git@host:path remote or url.<base> base with
//     no "://") is kept unchanged.
//
// The schemeless proxy form user:password@host[:port] (no "://") is NOT handled
// here — for a remote url or a url/includeIf base that shape is scp, not userinfo.
// It is handled only for proxy keys by dropSchemelessProxyUserinfo.
//
// Known limit: a base whose authority holds a non-numeric port and no "@", or whose
// "@" comes only after a "/" (for example a password with an unescaped "/"), does
// not parse and is kept unchanged. A schemeless host:path value (scp form) in a
// non-proxy key is kept unchanged; git reads it as a host and path, not as userinfo.
func sanitizeURLUserinfo(s string) (clean string, changed, drop bool) {
	t := strings.TrimSpace(s)
	if m := helperPrefixRe.FindString(t); m != "" {
		rc, rchanged, rdrop := sanitizeURLUserinfo(t[len(m):])
		switch {
		case rdrop:
			return "", false, true
		case rchanged:
			return m + rc, true, false
		default:
			return s, false, false
		}
	}
	u, err := url.Parse(t)
	if err == nil {
		if u.User != nil {
			switch u.Scheme {
			case "http", "https", "ftp", "ftps":
				u.User = nil
				return u.String(), true, false
			default:
				// Any other scheme: a password is non-functional or secret, so remove
				// ALL userinfo; a username with no password is kept (e.g. ssh://git@h).
				if _, hasPW := u.User.Password(); hasPW {
					u.User = nil
					return u.String(), true, false
				}
				return s, false, false
			}
		}
		return s, false, false
	}
	if strings.Contains(t, "://") && hasUserinfoBeforePath(t) {
		return "", false, true
	}
	return s, false, false
}

// dropSchemelessProxyUserinfo reports whether a proxy-key value (http.proxy, with or
// without a subsection, or remote.<name>.proxy) is the schemeless proxy form
// user:password@host[:port] and should be dropped. Git accepts a proxy as
// [protocol://][user[:password]@]host[:port]; a value with no "://" that contains an
// "@" with a ":" before the last "@" carries a password, so drop it. A username-only
// value (user@host, no ":" before the last "@") is kept. This is applied only to
// proxy keys: for a remote url or a url/includeIf base the same host:path shape is
// scp and must be kept, so it is not handled in sanitizeURLUserinfo.
func dropSchemelessProxyUserinfo(s string) bool {
	t := strings.TrimSpace(s)
	if strings.Contains(t, "://") {
		return false
	}
	at := strings.LastIndex(t, "@")
	if at < 0 {
		return false
	}
	return strings.Contains(t[:at], ":")
}

// hasConfigURLPrefix is the includeIf condition prefix whose remainder is a URL
// that may carry userinfo. It is shared by the redaction branch and
// safeSubsectionLabel. git matches the includeIf hasconfig prefix
// case-sensitively; we match case-insensitively on purpose, which can only
// over-redact, and every includeif key is refused by the policy anyway.
const hasConfigURLPrefix = "hasconfig:remote.*.url:"

// notedKeyName returns a key name safe to record or print: the subsection is
// replaced by safeSubsectionLabel (a URL by its scheme and host only, a plain
// name unchanged, credential.<url> and anything unclassifiable by
// "<redacted>"). It names keys in the stripped list and in refusals; it never
// rewrites config content kept for git.
func notedKeyName(v gitConfigVar) string {
	if !v.HasSubsection {
		return v.Key()
	}
	return v.Section + "." + safeSubsectionLabel(v.Section, v.Subsection) + "." + v.Name
}

// hasUserinfoBeforePath reports whether s has an "@" before the first "/" of its
// path (ignoring the scheme's "://"), i.e. it looks like it carries userinfo.
func hasUserinfoBeforePath(s string) bool {
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if j := strings.IndexByte(s, '/'); j >= 0 {
		s = s[:j]
	}
	return strings.IndexByte(s, '@') >= 0
}

// serializeGitConfig renders variables back to git config file syntax. Values
// are always quoted and escaped, which git always accepts, so no value can be
// misparsed after a round trip.
func serializeGitConfig(vars []gitConfigVar) []byte {
	var b bytes.Buffer
	type sectionKey struct {
		section string
		sub     string
		hasSub  bool
	}
	var cur sectionKey
	started := false
	for _, v := range vars {
		k := sectionKey{v.Section, v.Subsection, v.HasSubsection}
		if !started || k != cur {
			if v.HasSubsection {
				fmt.Fprintf(&b, "[%s \"%s\"]\n", v.Section, escapeConfigSubsection(v.Subsection))
			} else {
				fmt.Fprintf(&b, "[%s]\n", v.Section)
			}
			cur = k
			started = true
		}
		fmt.Fprintf(&b, "\t%s = %s\n", v.Name, escapeConfigValue(v.Value))
	}
	return b.Bytes()
}

// escapeConfigSubsection escapes a subsection for the [section "sub"] form:
// git only recognises \" and \\ there.
func escapeConfigSubsection(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return r.Replace(s)
}

// EscapeGitConfigSubsection escapes a string for use as a quoted git config
// subsection ([section "sub"]). git only recognises \" and \\ there, so a value
// containing a double quote or backslash must be escaped or it breaks the
// section header (and every subsequent key).
func EscapeGitConfigSubsection(s string) string {
	return escapeConfigSubsection(s)
}

// EscapeGitConfigValue renders s as a fully quoted, escaped git config value
// (including the surrounding quotes), which git always accepts, so no value can
// be misparsed after writing it.
func EscapeGitConfigValue(s string) string {
	return escapeConfigValue(s)
}

// escapeConfigValue renders a value as a quoted git config string.
func escapeConfigValue(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}
