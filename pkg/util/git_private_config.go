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
// the values nor the key names carry a credential. It is used to seed a per-agent
// private .git/config so a copy of a linked checkout's host config does not carry
// the host user's credentials into the agent.
//
// It strips:
//   - every credential.* key (the git credential-helper configuration),
//   - every http.<url>.extraheader (authorization headers),
//   - every http.<url>.cookiefile (a path to stored session cookies),
//   - every sendemail.smtppass (an SMTP password),
//   - userinfo from URL values in remote.*.url / pushurl / lfsurl / lfspushurl,
//     lfs.url / lfs.pushurl, url.*.insteadof / pushinsteadof, http.proxy and
//     remote.*.proxy; from url.<base>.*, http.<base>.* and lfs.<base>.* subsection
//     headers; and from the <url> in an includeIf "hasconfig:remote.*.url:<url>"
//     subsection (http/https/ftp/ftps userinfo removed; other schemes keep only the
//     username; remote-helper "transport::url" forms handled recursively), and
//   - any such URL-valued key or subsection that is URL-shaped ("://") and cannot
//     be parsed but still carries userinfo, or whose opaque form hides userinfo
//     (the key is dropped entirely, so the userinfo cannot survive in a value,
//     header or key name).
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
	kept := make([]gitConfigVar, 0, len(vars))
	seen := map[string]struct{}{}
	note := func(key string) {
		if _, ok := seen[key]; !ok {
			seen[key] = struct{}{}
			stripped = append(stripped, key)
		}
	}
	for _, v := range vars {
		// credential.* — drop entirely.
		if v.Section == "credential" {
			note(notedKeyName(v))
			continue
		}
		// http.<url>.extraheader / http.<url>.cookiefile — drop entirely.
		if v.Section == "http" && (v.Name == "extraheader" || v.Name == "cookiefile") {
			note(notedKeyName(v))
			continue
		}
		// sendemail.smtppass (with or without an identity subsection) — a plaintext
		// password; drop entirely.
		if v.Section == "sendemail" && v.Name == "smtppass" {
			note(notedKeyName(v))
			continue
		}
		// Userinfo in URL-valued keys — rewrite the value, or drop the key when the
		// value is URL-shaped, cannot be parsed, but still carries userinfo.
		if isURLValuedKey(v) {
			clean, changed, drop := sanitizeURLUserinfo(v.Value)
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
		// prefix; drop the key if that would drop.
		const hasConfigPrefix = "hasconfig:remote.*.url:"
		if v.Section == "includeif" && v.HasSubsection && strings.HasPrefix(v.Subsection, hasConfigPrefix) {
			clean, changed, drop := sanitizeURLUserinfo(v.Subsection[len(hasConfigPrefix):])
			if drop {
				note(notedKeyName(v))
				continue
			}
			if changed {
				note(notedKeyName(v))
				v.Subsection = hasConfigPrefix + clean
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
	return serializeGitConfig(kept), stripped, nil
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

// isURLValuedKey reports whether a config variable's value is a URL whose
// userinfo should be scrubbed.
func isURLValuedKey(v gitConfigVar) bool {
	switch {
	case v.Section == "remote" && (v.Name == "url" || v.Name == "pushurl" || v.Name == "lfsurl" || v.Name == "lfspushurl"):
		return true
	case v.Section == "lfs" && (v.Name == "url" || v.Name == "pushurl"):
		return true
	case v.Section == "url" && (v.Name == "insteadof" || v.Name == "pushinsteadof"):
		return true
	case v.Section == "http" && v.Name == "proxy": // http.proxy and http.<url>.proxy
		return true
	case v.Section == "remote" && v.Name == "proxy": // remote.<name>.proxy
		return true
	}
	return false
}

// helperPrefixRe matches a git remote-helper transport prefix, e.g. "http::" in
// "http::https://host/x". It is "<transport>::" where <transport> is a scheme-like
// token (a letter then letters/digits/+/-/.) followed by "::". A normal URL scheme
// ("https://") has a single ":" then "//", so it does not match.
var helperPrefixRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*::`)

// sanitizeURLUserinfo strips credential-bearing userinfo from a git config URL,
// returning the cleaned string, whether it changed, and whether the caller should
// drop the whole key. It is shared by both callers: URL-valued keys (remote.*.url
// and friends) and url.<base>/http.<base>/lfs.<base> subsection headers. Applied in
// order:
//
//   - A remote-helper prefix "<transport>::<url>" is split off; the remainder is
//     sanitized recursively and the prefix re-prepended (changed/drop propagate).
//   - A parseable http/https/ftp/ftps URL that carries userinfo has it removed
//     entirely (changed).
//   - A parseable URL of any other scheme that carries a password keeps only the
//     username (e.g. ssh://u:pw@h -> ssh://u@h, socks5 likewise), changed; with no
//     password it is kept unchanged (e.g. ssh://u@h, scp-like ssh users).
//   - A parseable opaque form with no authority but an "@" in the opaque part
//     (e.g. a schemeless proxy "u:pw@proxy:8080") is dropped.
//   - A string that will not parse but is URL-shaped ("://") and still carries
//     userinfo before the path is dropped, so the userinfo cannot survive.
//   - Anything else (e.g. an scp-style git@host:path remote or url.<base> base with
//     no "://") is kept unchanged.
//
// Known limit: a split-authority base whose authority holds a non-numeric port and
// no "@" (for example [url "https://alice:tok"] used only as an insteadOf alias)
// does not parse and is kept unchanged.
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
				if _, hasPW := u.User.Password(); hasPW {
					u.User = url.User(u.User.Username())
					return u.String(), true, false
				}
				return s, false, false
			}
		}
		if strings.Contains(u.Opaque, "@") {
			return "", false, true
		}
		return s, false, false
	}
	if strings.Contains(t, "://") && hasUserinfoBeforePath(t) {
		return "", false, true
	}
	return s, false, false
}

// notedKeyName returns a key name safe to record in the stripped list: a
// subsection that carries userinfo is never included verbatim. The subsection is
// run through sanitizeURLUserinfo — its cleaned form if that changed it,
// "<redacted>" if it would be dropped — and any credential.<url> subsection is
// always "<redacted>".
func notedKeyName(v gitConfigVar) string {
	if !v.HasSubsection {
		return v.Key()
	}
	sub := v.Subsection
	if v.Section == "credential" {
		sub = "<redacted>"
	} else if clean, changed, drop := sanitizeURLUserinfo(v.Subsection); drop {
		sub = "<redacted>"
	} else if changed {
		sub = clean
	}
	return v.Section + "." + sub + "." + v.Name
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
