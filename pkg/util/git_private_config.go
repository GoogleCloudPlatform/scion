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
	"strings"
)

// RedactGitConfigSecrets parses a git config file and returns an equivalent
// config with credential-bearing material removed, together with the names of
// the keys it changed (never their values). It is used to seed a per-agent
// private .git/config so a copy of a linked checkout's host
// config does not carry the host user's credentials into the agent.
//
// It strips:
//   - every credential.* key (the git credential-helper configuration),
//   - every http.<url>.extraheader (authorization headers),
//   - every http.<url>.cookiefile (a path to stored session cookies),
//   - userinfo (user[:password]) from http(s) URL values in remote.*.url /
//     remote.*.pushurl / remote.*.lfsurl / remote.*.lfspushurl / lfs.url /
//     lfs.pushurl / url.*.insteadof / url.*.pushinsteadof and from the
//     subsection of a url.<base>.* or http.<base>.* entry, and
//   - any URL-valued key whose value cannot be parsed but carries userinfo.
//
// A url.<base>.* or http.<base>.* key whose base subsection cannot be parsed but
// carries userinfo is dropped entirely, so the userinfo cannot survive in the
// section header.
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
			note(v.Key())
			continue
		}
		// http.<url>.extraheader / http.<url>.cookiefile — drop entirely.
		if v.Section == "http" && (v.Name == "extraheader" || v.Name == "cookiefile") {
			note(v.Key())
			continue
		}
		// Userinfo in URL-valued keys — rewrite the value, or drop the key when
		// the value cannot be parsed but still carries userinfo.
		if isURLValuedKey(v) {
			clean, changed, drop := sanitizeURLValue(v.Value)
			if drop {
				note(v.Key())
				continue
			}
			if changed {
				note(v.Key())
				v.Value = clean
			}
		}
		// Userinfo in a url.<base>.* or http.<base>.* subsection (the base is the
		// URL that keys the section) — rewrite the subsection, or drop the key when
		// the base cannot be parsed but still carries userinfo.
		if (v.Section == "url" || v.Section == "http") && v.HasSubsection {
			clean, changed, drop := stripURLUserinfo(v.Subsection)
			if drop {
				note(v.Key())
				continue
			}
			if changed {
				note(v.Key())
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
	}
	return false
}

// sanitizeURLValue strips userinfo from a parseable http(s) URL value. When the
// value cannot be parsed but still carries userinfo (an "@" before the path), it
// signals drop so the caller removes the whole key rather than leak it. Non-URL
// or non-http(s) values (e.g. scp-style git@host:path) are returned unchanged.
func sanitizeURLValue(s string) (clean string, changed, drop bool) {
	t := strings.TrimSpace(s)
	u, err := url.Parse(t)
	if err == nil {
		if (u.Scheme != "http" && u.Scheme != "https") || u.User == nil {
			return s, false, false
		}
		u.User = nil
		return u.String(), true, false
	}
	if hasUserinfoBeforePath(t) {
		return "", false, true
	}
	return s, false, false
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

// stripURLUserinfo removes any user[:password]@ userinfo from an http(s) URL,
// returning the cleaned string and whether it changed. When the string cannot be
// parsed but still carries userinfo (an "@" before the path), it signals drop so
// the caller removes the whole key rather than leave the userinfo in the section
// header. Non-URL or non-http(s) strings (e.g. scp-style git@host:path) are
// returned unchanged. Mirrors sanitizeURLValue.
func stripURLUserinfo(s string) (clean string, changed, drop bool) {
	t := strings.TrimSpace(s)
	u, err := url.Parse(t)
	if err == nil {
		if (u.Scheme != "http" && u.Scheme != "https") || u.User == nil {
			return s, false, false
		}
		u.User = nil
		return u.String(), true, false
	}
	if hasUserinfoBeforePath(t) {
		return "", false, true
	}
	return s, false, false
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
