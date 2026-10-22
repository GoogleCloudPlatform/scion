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

package apiclient

import (
	"errors"
	"net/url"
	"regexp"
	"strings"
)

// urlUserInfoRe matches the user info of a URL that url.Parse rejects: up
// to the last '@' before the path, query or fragment.
var urlUserInfoRe = regexp.MustCompile("^(" + urlSchemePrefix + ")?[^/?#]*@")

// redactedSecret replaces each credential redactURLCredentials masks.
const redactedSecret = "xxxxx"

// redactURLCredentials returns raw with its credentials redacted as
// config.RedactURL does: the user info, and the value of every query or
// fragment parameter with a credential-like name. It is a copy of those
// rules (this package imports no scion package); change both together.
func redactURLCredentials(raw string) string {
	s := raw
	if u, err := url.Parse(raw); err == nil {
		if u.User != nil {
			if _, hasPassword := u.User.Password(); hasPassword {
				s = u.Redacted()
			} else {
				u.User = url.User(redactedSecret)
				s = u.String()
			}
		}
	} else {
		s = urlUserInfoRe.ReplaceAllString(raw, "${1}"+redactedSecret+"@")
	}
	return redactURLParams(s)
}

// The rules below are a copy of pkg/config/url_redact.go; change both
// together.

// urlCredentialParamNames are query and fragment parameter names whose
// value RedactURL masks, compared lower-cased. A name that contains
// "token", "secret", "password", "passwd", "apikey", "api_key", "api-key"
// or "jwt", or that ends in "_key" or "-key", is masked too (see
// isURLCredentialParam).
var urlCredentialParamNames = map[string]bool{
	"key":               true,
	"auth":              true,
	"authorization":     true,
	"code":              true,
	"credential":        true,
	"assertion":         true,
	"client_assertion":  true,
	"sig":               true,
	"signature":         true,
	"pwd":               true,
	"pass":              true,
	"jsessionid":        true,
	"phpsessid":         true,
	"x-amz-credential":  true,
	"x-amz-signature":   true,
	"x-goog-credential": true,
	"x-goog-signature":  true,
}

// isURLCredentialParam reports whether a query or fragment parameter name
// (already unescaped) names a credential.
func isURLCredentialParam(name string) bool {
	n := strings.ToLower(name)
	if urlCredentialParamNames[n] {
		return true
	}
	for _, s := range []string{"token", "secret", "password", "passwd", "apikey", "api_key", "api-key", "jwt"} {
		if strings.Contains(n, s) {
			return true
		}
	}
	return strings.HasSuffix(n, "_key") || strings.HasSuffix(n, "-key")
}

// The URL delimiters redactURLParams splits on, each also in the JSON
// escaped form a URL takes inside JSON text (\u0026 for "&" and so on).
var (
	urlFragmentDelims = []string{"#", `\u0023`}
	urlQueryDelims    = []string{"?", `\u003f`}
	urlParamDelims    = []string{"&", ";", `\u0026`, `\u003b`}
	urlValueDelims    = []string{"=", `\u003d`}
)

// asciiLower returns s with only the ASCII letters A-Z lower-cased. Unlike
// strings.ToLower it keeps every byte position (non-ASCII and invalid
// UTF-8 bytes are copied as they are), so an index found in its result is
// an index into s.
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

// cutDelim cuts s at the first of delims (the \u forms matched
// case-insensitively, ASCII only) and returns the parts and the delimiter
// as written.
func cutDelim(s string, delims []string) (before, delim, after string, found bool) {
	lower := asciiLower(s)
	at, width := -1, 0
	for _, d := range delims {
		if i := strings.Index(lower, d); i >= 0 && (at < 0 || i < at) {
			at, width = i, len(d)
		}
	}
	if at < 0 {
		return s, "", "", false
	}
	return s[:at], s[at : at+width], s[at+width:], true
}

// redactURLParams returns raw (a URL, or the part of one from its path
// on) with the value of every credential parameter in its query and its
// fragment replaced by xxxxx, and the same for its path parameters
// (redactPathParams). A fragment that holds a route with its own
// query (#/path?code=...) has that query redacted, and the part before its
// "?" is redacted as a parameter list too (a "?" can also sit inside a
// fragment parameter's value). Parameters are split on "&" and ";", so a
// credential value that holds a literal ";" is masked only up to it. Every
// other byte, including the order and spelling of the other parameters,
// is kept.
func redactURLParams(raw string) string {
	rest, fdelim, fragment, hasFragment := cutDelim(raw, urlFragmentDelims)
	base, qdelim, query, hasQuery := cutDelim(rest, urlQueryDelims)
	out := redactPathParams(base)
	if hasQuery {
		out += qdelim + redactParamList(query)
	}
	if hasFragment {
		if route, rq, routeQuery, ok := cutDelim(fragment, urlQueryDelims); ok {
			out += fdelim + redactParamList(route) + rq + redactParamList(routeQuery)
		} else {
			out += fdelim + redactParamList(fragment)
		}
	}
	return out
}

// urlSchemePrefix is a URL's scheme and "://", the anchored prefix the
// user info rules (userInfoRe) also start with.
const urlSchemePrefix = `[A-Za-z][A-Za-z0-9+.-]*://`

// urlSchemePrefixRe matches urlSchemePrefix at the start of a string only.
var urlSchemePrefixRe = regexp.MustCompile("^" + urlSchemePrefix)

// redactPathParams masks the credential values of the path parameters in
// base (a URL up to its query, or only its path): each ";name=value" in a
// path segment, as in /app;jsessionid=X. The scheme and authority are
// skipped only when base STARTS with a scheme, so a URL embedded later in
// the path (/app;jsessionid=X/r/https://y/) never hides the parameters
// before it, and a whole URL and its path alone are masked the same way.
func redactPathParams(base string) string {
	prefix, path := "", base
	if scheme := urlSchemePrefixRe.FindString(base); scheme != "" {
		afterScheme, _ := strings.CutPrefix(base, scheme)
		authority, rest, hasPath := strings.Cut(afterScheme, "/")
		if !hasPath {
			return base
		}
		prefix, path = scheme+authority+"/", rest
	}
	if !strings.Contains(path, ";") {
		return base
	}
	segments := strings.Split(path, "/")
	for i, segment := range segments {
		name, params, hasParams := strings.Cut(segment, ";")
		if !hasParams {
			continue
		}
		list := strings.Split(params, ";")
		for k, param := range list {
			list[k] = redactParam(param, 1)
		}
		segments[i] = name + ";" + strings.Join(list, ";")
	}
	return prefix + strings.Join(segments, "/")
}

// redactParamList masks the credential values of a list of name=value
// pairs. A name that does not unescape is compared as written.
func redactParamList(list string) string {
	return redactParamListAt(list, 0)
}

// redactParamListAt is redactParamList for a list at nesting depth depth
// (0 for a URL's own query or fragment, 1 for the query of a URL inside a
// parameter value).
func redactParamListAt(list string, depth int) string {
	var b strings.Builder
	for {
		pair, delim, rest, more := cutDelim(list, urlParamDelims)
		b.WriteString(redactParam(pair, depth))
		if !more {
			return b.String()
		}
		b.WriteString(delim)
		list = rest
	}
}

// redactParam masks the value of one name=value pair with a credential
// name. A value that is itself a URL with a query (it holds "://" and a
// "?"), under any name, keeps its destination readable: the part after its
// first "?" is redacted as a parameter list, one level deep only, by its
// plain-text structure (no further percent-decoding). For example
// next=https://h/cb?token=X becomes next=https://h/cb?token=xxxxx.
func redactParam(pair string, depth int) string {
	name, vdelim, value, hasValue := cutDelim(pair, urlValueDelims)
	if !hasValue {
		return pair
	}
	unescaped, err := url.QueryUnescape(name)
	if err != nil {
		unescaped = name
	}
	if isURLCredentialParam(unescaped) {
		return name + vdelim + redactedSecret
	}
	if depth == 0 && strings.Contains(value, "://") {
		if dest, qdelim, query, ok := cutDelim(value, urlQueryDelims); ok {
			return name + vdelim + dest + qdelim + redactParamListAt(query, depth+1)
		}
	}
	return pair
}

// redactURLError returns err with the URL of the *url.Error in its chain
// shown without its credentials (see redactURLCredentials). It changes
// that *url.Error in place, so it is applied to the error net/http or
// url.Parse returns, before any wrapping (fmt.Errorf fixes the wrapped
// message when it is called).
func redactURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		ue.URL = redactURLCredentials(ue.URL)
	}
	return err
}
