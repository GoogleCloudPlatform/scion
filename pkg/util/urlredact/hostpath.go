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

package urlredact

import (
	"encoding/json"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// HostAndPath returns raw parsed, with its user info, query and fragment
// removed, for a caller that shows a URL by its scheme, host, port and path
// only (a clone URL, a hub endpoint, a fetched URL). ok is false, and the
// caller shows nothing of raw, when raw does not parse or holds an '@' that
// is not the end of a user info the shared rule delimits: user info that
// Undelimited reports, and also any '@' after the authority. The second
// covers what Undelimited leaves to the reader, a password that reads as a
// port and a path ("https://u:1234/x@h") or a token that reads as a host
// ("https://tok/x@h"), at the cost of a URL with an e-mail address after
// its host, which is not shown either.
func HostAndPath(raw string) (u *url.URL, ok bool) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, false
	}
	if strings.Contains(raw, "@") {
		_, rest, hasAuthority := cutRawAuthority(raw)
		if !hasAuthority || strings.Contains(rest, "@") || undelimitedParsed(raw, u, nil) {
			return nil, false
		}
	}
	u.User = nil
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	u.RawFragment = ""
	return u, true
}

// minSecretLen is the length below which a part of a URL is not replaced
// on its own by RedactURLIn: replacing a shorter string would rewrite
// unrelated text.
const minSecretLen = 3

// RedactURLIn returns text, which may quote the URL raw or a URL built from
// it (an error from a request to raw, the output of a command run on raw),
// with no part of raw's credential in it:
//   - every rendering of raw is replaced by shown: raw as given and as
//     strconv.Quote writes it, and, when raw parses, as url.URL.String,
//     url.URL.Redacted and net/http's errors write it;
//   - when HostAndPath accepts raw, its user info followed by '@' (as
//     url.URL writes it and as written in raw), its query after '?' and its
//     fragment after '#' are removed wherever they still appear;
//   - when HostAndPath rejects a raw that holds an '@', the parts of it
//     that may hold user info are replaced by mask wherever they appear, as
//     given and as strconv.Quote, JSON, url.PathEscape and url.QueryEscape
//     write them (see rawUserInfoParts): replacing more than the user info
//     is safe, less is not. A raw without a scheme and "://" (or a leading
//     "//") has no such parts;
//   - last, the user info of every URL left in text is masked with mask by
//     the shared rule (RedactUserInfoIn).
//
// A raw of "" only has the last step applied.
func RedactURLIn(text, raw, shown, mask string) string {
	if raw != "" {
		u, err := url.Parse(raw)
		renderings := []string{raw, quotedBody(strconv.Quote(raw))}
		if err == nil {
			renderings = append(renderings, u.String(), u.Redacted(), netHTTPForm(u))
		}
		for _, r := range renderings {
			text = strings.ReplaceAll(text, r, shown)
		}
		if _, ok := HostAndPath(raw); ok {
			text = removeURLParts(text, raw, u)
		} else {
			for _, part := range rawUserInfoParts(raw) {
				text = replaceForms(text, part, mask)
			}
		}
	}
	return RedactUserInfoIn(text, mask)
}

// netHTTPForm returns u as net/http's client errors write it: with a
// password shown as "***".
func netHTTPForm(u *url.URL) string {
	if _, hasPassword := u.User.Password(); hasPassword {
		return strings.Replace(u.String(), u.User.String()+"@", u.User.Username()+":***@", 1)
	}
	return u.String()
}

// removeURLParts removes from text the user info, query and fragment of
// raw (parsed as u, which HostAndPath accepts).
func removeURLParts(text, raw string, u *url.URL) string {
	var parts []string
	// A user info with neither a user name nor a password ("@", ":@")
	// carries nothing, and removing it would rewrite unrelated text.
	if pw, _ := u.User.Password(); u.User != nil && (u.User.Username() != "" || pw != "") {
		parts = append(parts, u.User.String()+"@")
		if authority, _, ok := cutRawAuthority(raw); ok {
			if at := strings.LastIndex(authority, "@"); at > 0 {
				parts = append(parts, authority[:at+1])
			}
		}
	}
	if u.RawQuery != "" {
		parts = append(parts, "?"+u.RawQuery)
	}
	if u.Fragment != "" {
		parts = append(parts, "#"+u.EscapedFragment(), "#"+u.Fragment)
	}
	for _, p := range parts {
		text = strings.ReplaceAll(text, p, "")
	}
	return text
}

// rawUserInfoParts returns the parts of raw that may hold user info, read
// from raw itself, longest first, for a raw whose user info cannot be
// placed: the span from after its scheme and "://" (or a leading "//") up
// to its last '@', path included; each part of that span between two '@'
// (a URL printed with its user info removed up to its first '@', as git
// prints one, still holds the rest); and the authority as raw itself ends
// it, at its first '/', '?' or '#' (a URL built from raw with its query
// cleared still holds it). It returns nil when raw holds no '@' or has
// neither prefix.
func rawUserInfoParts(raw string) []string {
	authority, rest, ok := cutRawAuthority(raw)
	if !ok || !strings.Contains(raw, "@") {
		return nil
	}
	after := authority + rest
	at := strings.LastIndex(after, "@")
	if at < 0 {
		return []string{authority}
	}
	span := after[:at]
	parts := append([]string{span}, strings.Split(span, "@")...)
	parts = append(parts, authority)
	sort.SliceStable(parts, func(i, j int) bool { return len(parts[i]) > len(parts[j]) })
	return parts
}

// replaceForms replaces secret everywhere in text with mask: as given, and
// as strconv.Quote, JSON, url.PathEscape and url.QueryEscape write it. A
// secret shorter than minSecretLen is left alone.
func replaceForms(text, secret, mask string) string {
	if len(secret) < minSecretLen {
		return text
	}
	forms := []string{secret, quotedBody(strconv.Quote(secret)), url.PathEscape(secret), url.QueryEscape(secret)}
	if b, err := json.Marshal(secret); err == nil {
		forms = append(forms, quotedBody(string(b)))
	}
	for _, f := range forms {
		text = strings.ReplaceAll(text, f, mask)
	}
	return text
}

// quotedBody returns a quoted string without its outer quotes.
func quotedBody(q string) string {
	return q[1 : len(q)-1]
}
