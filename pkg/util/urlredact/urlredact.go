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

// Package urlredact holds the rule that decides how the user info of a URL
// is shown: masked on its own when it can be told apart from the host and
// path, and otherwise the whole URL after its scheme. pkg/config,
// pkg/apiclient, the log text sanitizer (pkg/util/logging) and the hub
// commands all use it, so a URL is shown the same way everywhere. It
// imports only the standard library, so any package can use it.
package urlredact

import (
	"net/url"
	"regexp"
	"strings"
)

// SchemePrefix is a URL's scheme and "://", as a regular expression.
const SchemePrefix = `[A-Za-z][A-Za-z0-9+.-]*://`

// Mask replaces the user info RedactUserInfo masks, and the URL MaskWhole
// masks, for the CLI and API client forms.
const Mask = "xxxxx"

var (
	// schemePrefixRe matches SchemePrefix at the start of a string only.
	schemePrefixRe = regexp.MustCompile("^" + SchemePrefix)
	// userInfoRe matches the user info of a URL that url.Parse rejects, up
	// to the last '@' before the path, query or fragment.
	userInfoRe = regexp.MustCompile("^(" + SchemePrefix + ")?[^/?#]*@")
	// textSchemeRe finds where a URL may start in a text.
	textSchemeRe = regexp.MustCompile(SchemePrefix)
)

// Undelimited reports whether raw may carry user info that cannot be told
// apart from its host and path, so that only its scheme can be shown (see
// MaskWhole). A '/', '?' or '#' in the user info moves where the authority
// seems to end, and an '@' in a password moves where the user info seems
// to end. That is the case when raw holds an '@' and either:
//   - raw does not parse, and the user info rule for URLs url.Parse
//     rejects does not match it, or raw holds another '@' after the match
//     (a password such as "p@ss:x/w0rd": the rule stops at the first '/',
//     and the rest of the password follows);
//   - raw parses with user info and another '@' follows its authority, in
//     the path, query or fragment (a password such as "p@ss/w0rd" parses as
//     user "p", host "ss" and a path);
//   - raw parses but its authority, cut from raw itself, does not end in a
//     host (see rawAuthorityIsHost).
//
// The '@' is looked for in raw, never in a part of it; without one there
// is no user info, and an '@' after the host of a URL without user info
// (an e-mail address, "/@name") is not masked. So a password that reads as
// a port and a path ("https://u:1234/x@h") or a bare token followed by a
// path ("https://tok/x@h") is not detected: such a URL is indistinguishable
// from one with an e-mail address in its path.
func Undelimited(raw string) bool {
	if !strings.Contains(raw, "@") {
		return false
	}
	u, err := url.Parse(raw)
	return undelimitedParsed(raw, u, err)
}

// undelimitedParsed is Undelimited for a raw that holds an '@', given the
// result of url.Parse(raw), so a caller that needs the parsed URL as well
// parses it once.
func undelimitedParsed(raw string, u *url.URL, err error) bool {
	if err != nil {
		m := userInfoRe.FindStringIndex(raw)
		return m == nil || strings.Contains(raw[m[1]:], "@")
	}
	authority, rest, hasAuthority := cutRawAuthority(raw)
	if u.User != nil && strings.Contains(rest, "@") {
		return true
	}
	return hasAuthority && !rawAuthorityIsHost(authority)
}

// MaskWhole returns the scheme of raw and "://" followed by mask, or mask
// alone when raw has no scheme. Nothing else of raw is kept.
func MaskWhole(raw, mask string) string {
	return schemePrefixRe.FindString(raw) + mask
}

// RedactUserInfo returns raw with the credential in its user info replaced
// by Mask: the password (see url.URL.Redacted), or a user name given
// without a password, which may itself be a token. A URL that does not
// parse has its whole user info replaced. A URL whose user info cannot be
// delimited (see Undelimited) shows only its scheme and Mask. A URL
// without user info is returned as given.
func RedactUserInfo(raw string) string {
	if !strings.Contains(raw, "@") {
		return raw
	}
	u, err := url.Parse(raw)
	if undelimitedParsed(raw, u, err) {
		return MaskWhole(raw, Mask)
	}
	if err != nil {
		return userInfoRe.ReplaceAllString(raw, "${1}"+Mask+"@")
	}
	if u.User == nil {
		return raw
	}
	if _, hasPassword := u.User.Password(); hasPassword {
		return u.Redacted()
	}
	u.User = url.User(Mask)
	return u.String()
}

// The kinds of user info locateUserInfo reports.
const (
	noUserInfo   = iota // nothing to mask
	wholeURL            // the user info cannot be delimited: mask all but the scheme
	userInfoSpan        // the user info is raw[start:end]
)

// locateUserInfo reports where the user info of raw is, by the rule of
// Undelimited, parsing raw once: noUserInfo, wholeURL, or
// userInfoSpan with raw[start:end] the whole user info (the user name
// too), which ends at the last '@' of the authority.
func locateUserInfo(raw string) (kind, start, end int) {
	if !strings.Contains(raw, "@") {
		return noUserInfo, 0, 0
	}
	u, err := url.Parse(raw)
	if undelimitedParsed(raw, u, err) {
		return wholeURL, 0, 0
	}
	if err == nil && u.User == nil {
		return noUserInfo, 0, 0
	}
	if authority, rest, ok := cutRawAuthority(raw); ok {
		at := strings.LastIndex(authority, "@")
		if at < 0 {
			return noUserInfo, 0, 0
		}
		start = len(raw) - len(rest) - len(authority)
		return userInfoSpan, start, start + at
	}
	// No scheme and no "//": the user info rule from the start of raw.
	m := userInfoRe.FindStringIndex(raw)
	if m == nil {
		return noUserInfo, 0, 0
	}
	return userInfoSpan, 0, m[1] - 1
}

// textURLSpace is the white space that ends a URL in a text: ASCII only,
// the set of RE2's \s plus '\v', as in pkg/config's text patterns. A
// non-ASCII space (U+00A0, U+2028 and the like, as in a copied URL) stays
// inside the URL, which then fails to parse and has its user info masked
// up to its '@', failing closed.
const textURLSpace = " \t\n\v\f\r"

// textURLTrailing is the punctuation that ends a sentence, a quotation or
// an escape around a URL in a text, rather than ending the URL itself.
const textURLTrailing = ".,;:!?)]}>'\"`\\"

// The limits on the work RedactUserInfoIn does in one run of text without
// white space. Past either one, the rest of the run is masked whole when an
// '@' follows: that fails closed.
const (
	// MaxTextURLLen bounds a run from its first scheme: a run longer than
	// this, from that scheme to the next white space, with an '@' after
	// the scheme, is masked whole from the scheme. It only ever fires on a
	// run's first URL (every later one is shorter) and keeps the cost per
	// parse small; linear time does not depend on it.
	MaxTextURLLen = 4096
	// MaxTextURLsPerRun is the most URLs RedactUserInfoIn checks in one
	// run (a URL inside another one counts); past it, the rest of the run
	// is masked whole when an '@' follows. It bounds the parses per run,
	// which keeps the scan linear in the length of the text.
	MaxTextURLsPerRun = 16
)

// RedactUserInfoIn returns text with the whole user info of every URL in
// it, the user name too, replaced by mask, and every other byte kept; a URL
// whose user info cannot be delimited (see Undelimited) shows only its
// scheme and mask, with the rest of its run. A URL starts at a scheme and
// "://" and runs to the next ASCII white space (textURLSpace), less the
// punctuation that ends it (textURLTrailing); white space is the only
// boundary, so a quote or
// bracket inside a password cannot end the URL early. A URL inside another
// one (a redirect parameter, or two URLs with no space between them) is
// checked on its own as well, unless the outer URL was masked whole.
//
// It takes time linear in the length of text: each run is cut once, a run
// with no '@' after a scheme is skipped, each URL is parsed once, and past
// the first MaxTextURLsPerRun URLs of a run (or in a run longer than
// MaxTextURLLen from its first scheme) the rest of the run is masked whole
// when an '@' follows.
func RedactUserInfoIn(text, mask string) string {
	var b strings.Builder
	written := 0 // text[written:] is not in b yet
	pos := 0
	for pos < len(text) {
		loc := textSchemeRe.FindStringIndex(text[pos:])
		if loc == nil {
			break
		}
		start := pos + loc[0]
		end := len(text)
		if i := strings.IndexAny(text[start:], textURLSpace); i >= 0 {
			end = start + i
		}
		if strings.IndexByte(text[start:end], '@') >= 0 {
			b.WriteString(text[written:start])
			redactRunUserInfo(&b, text[start:end], mask)
			written = end
		}
		pos = end
	}
	if written == 0 {
		return text
	}
	b.WriteString(text[written:])
	return b.String()
}

// redactRunUserInfo writes to b the run (text from a scheme to the next
// white space, holding an '@') with the user info of every URL in it
// replaced by mask (see RedactUserInfoIn).
func redactRunUserInfo(b *strings.Builder, run, mask string) {
	lastAt := strings.LastIndexByte(run, '@')
	written := 0 // run[written:] is not in b yet
	p := 0       // the start of the URL being checked; run starts with a scheme
	for n := 1; p < lastAt; n++ {
		raw := strings.TrimRight(run[p:], textURLTrailing)
		if n > MaxTextURLsPerRun || len(raw) > MaxTextURLLen {
			b.WriteString(run[written:p])
			b.WriteString(MaskWhole(raw, mask))
			b.WriteString(run[p+len(raw):])
			return
		}
		kind, start, end := locateUserInfo(raw)
		next := p + len(schemePrefixRe.FindString(raw))
		switch kind {
		case wholeURL:
			b.WriteString(run[written:p])
			b.WriteString(MaskWhole(raw, mask))
			b.WriteString(run[p+len(raw):])
			return
		case userInfoSpan:
			b.WriteString(run[written : p+start])
			b.WriteString(mask)
			written = p + end
			// A URL inside the user info is masked with it.
			next = max(next, written)
		}
		loc := textSchemeRe.FindStringIndex(run[next:])
		if loc == nil {
			break
		}
		p = next + loc[0]
	}
	b.WriteString(run[written:])
}

// cutRawAuthority cuts raw, after its scheme and "://" (or a leading
// "//"), at its first '/', '?' or '#': the authority, and the rest from
// that delimiter on. hasAuthority is false when raw has neither prefix.
func cutRawAuthority(raw string) (authority, rest string, hasAuthority bool) {
	if scheme := schemePrefixRe.FindString(raw); scheme != "" {
		authority, hasAuthority = raw[len(scheme):], true
	} else {
		authority, hasAuthority = strings.CutPrefix(raw, "//")
	}
	if !hasAuthority {
		return "", "", false
	}
	if i := strings.IndexAny(authority, "/?#"); i >= 0 {
		authority, rest = authority[:i], authority[i:]
	}
	return authority, rest, true
}

// rawAuthorityIsHost reports whether authority (see cutRawAuthority) ends
// in a host: after its last '@', if any, a name of ASCII letters, digits,
// '-', '.' and any non-ASCII character but U+FFFD (none of which can hide
// a '/', '?' or '#'), or a bracketed IP literal, then an optional ':' and
// digits. An empty authority passes.
func rawAuthorityIsHost(authority string) bool {
	rest := authority
	if rest == "" {
		return true
	}
	if i := strings.LastIndex(rest, "@"); i >= 0 {
		rest = rest[i+1:]
	}
	host, port := rest, ""
	if strings.HasPrefix(host, "[") {
		end := strings.IndexByte(host, ']')
		if end < 2 {
			return false
		}
		for _, r := range host[1:end] {
			if !isASCIIAlnum(r) && !strings.ContainsRune(":.%-", r) {
				return false
			}
		}
		if after := host[end+1:]; after != "" {
			p, hasPort := strings.CutPrefix(after, ":")
			if !hasPort {
				return false
			}
			port = p
		}
		host = ""
	} else {
		if i := strings.LastIndexByte(host, ':'); i >= 0 {
			host, port = host[:i], host[i+1:]
		}
		if host == "" {
			return false
		}
	}
	for _, r := range port {
		if r < '0' || r > '9' {
			return false
		}
	}
	for _, r := range host {
		if !isASCIIAlnum(r) && r != '-' && r != '.' && (r < 0x80 || r == '\uFFFD') {
			return false
		}
	}
	return true
}

// isASCIIAlnum reports whether r is an ASCII letter or digit.
func isASCIIAlnum(r rune) bool {
	return r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
}
