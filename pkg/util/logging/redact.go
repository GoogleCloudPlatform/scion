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

package logging

import (
	"net/http"
	"net/url"
	"strings"
)

// redactedValue replaces the value of a credential-bearing query parameter.
const redactedValue = "REDACTED"

// credentialQueryParams are query parameters whose values are bearer
// credentials and must never be written to logs. "sig" carries the Hub's
// skill file capability signature: anyone holding it can replay the download
// until it expires. "code" carries one-time sign-in codes: the web's dev
// sign-in link and OAuth authorization codes on the login callback.
var credentialQueryParams = map[string]bool{
	"sig":  true,
	"code": true,
}

// RedactQuery returns rawQuery with the values of credential-bearing
// parameters replaced by REDACTED. Other parameters, and their order, are
// preserved. A query that cannot be parsed, or that names an artifact or
// port view URL or holds a key or value that does not decode
// (credentialQuery), is replaced whole by REDACTED.
func RedactQuery(rawQuery string) string {
	if rawQuery == "" {
		return ""
	}
	if credentialQuery(rawQuery) {
		return redactedValue
	}
	parts := strings.Split(rawQuery, "&")
	for i, p := range parts {
		key, _, hasValue := strings.Cut(p, "=")
		name, err := url.QueryUnescape(key)
		if err != nil {
			return redactedValue
		}
		if credentialQueryParams[strings.ToLower(name)] && hasValue {
			parts[i] = key + "=" + redactedValue
		}
	}
	return strings.Join(parts, "&")
}

// RedactedArtifactPath is how every artifact request path is logged.
const RedactedArtifactPath = "/api/v1/artifacts/" + redactedValue

// RedactedPath is how a request path is logged that is still encoded
// when decoding stops, so that neither an artifact nor a port view path
// can be ruled out.
const RedactedPath = "/" + redactedValue

// RedactedDeliveryPath is how every attachment delivery request path is
// logged: the path carries a single-use delivery capability
// (ptone/scion#4596).
const RedactedDeliveryPath = "/api/v1/attachment-deliveries/" + redactedValue

// RedactedPortViewPath is how every port view request path is logged.
const RedactedPortViewPath = "/api/v1/portview/" + redactedValue

// maxUnescapeRounds bounds how many times pathMentions percent-decodes a
// path while looking for the artifact routes, the attachment delivery
// route or the port view route.
const maxUnescapeRounds = 4

// artifactPath reports whether the URL path p (decoded or escaped) may
// belong to the artifact routes, some of which carry bearer credentials in
// their path (share-link tokens, view capabilities). It does not try to
// recognise the credential routes: any path that, lowercased and
// percent-decoded up to maxUnescapeRounds times, contains "artifacts"
// anywhere counts, so no spelling of a path (dot segments, doubled or
// escaped slashes, escaped letters) reaches a log or a trace whole.
// Paths that merely contain the word are redacted too; that is the
// intended trade-off.
func artifactPath(p string) bool { return pathMentions(p, "artifacts") }

// pathMentions reports whether p, lowercased and percent-decoded up to
// maxUnescapeRounds times, contains word at any step. Input that still
// holds a '%' when decoding stops (it does not decode, or is encoded more
// than maxUnescapeRounds times) also counts as a match.
func pathMentions(p, word string) bool {
	found, undecoded := scanPath(p, word)
	return found || undecoded
}

// stillEncoded reports whether p, lowercased and percent-decoded up to
// maxUnescapeRounds times, still holds a '%' when decoding stops.
func stillEncoded(p string) bool {
	_, undecoded := scanPath(p, "")
	return undecoded
}

// scanPath decodes p (lowercased) up to maxUnescapeRounds times. found
// reports whether word appears at any step; undecoded whether a '%' is
// left when decoding stops. An empty word looks for nothing: found is
// false and every round runs, so undecoded always reflects the end.
func scanPath(p, word string) (found, undecoded bool) {
	s := strings.ToLower(p)
	for range maxUnescapeRounds {
		if word != "" && strings.Contains(s, word) {
			return true, false
		}
		u, err := url.PathUnescape(s)
		if err != nil || u == s {
			break
		}
		s = strings.ToLower(u)
	}
	return word != "" && strings.Contains(s, word), strings.Contains(s, "%")
}

// credentialQuery reports whether a raw query (or fragment) may carry an
// artifact, port view or attachment delivery URL (mentionsCredentialRoute),
// applied to the whole query and to each key and value
// on its own ('&', ';' and '=' separate them), so a key or value that does
// not decode (a match by pathMentions) cannot hide another.
func credentialQuery(rawQuery string) bool {
	if mentionsCredentialRoute(rawQuery) {
		return true
	}
	for _, part := range strings.FieldsFunc(rawQuery, func(r rune) bool { return r == '&' || r == ';' || r == '=' }) {
		if mentionsCredentialRoute(part) {
			return true
		}
	}
	return false
}

// mentionsCredentialRoute reports whether s may name a URL of a route that
// carries a capability in its path: an artifact, port view or attachment
// delivery URL (pathMentions for each route word).
func mentionsCredentialRoute(s string) bool {
	return pathMentions(s, "artifacts") || pathMentions(s, "portview") || pathMentions(s, "attachment-deliveries")
}

// deliveryPath reports whether the URL path p (decoded or escaped) may
// belong to the attachment delivery route, whose path carries a delivery
// capability. Like artifactPath it recognises any spelling: any path that
// mentions "attachment-deliveries" after decoding counts, and so does one
// that still holds a '%' when decoding stops (pathMentions).
func deliveryPath(p string) bool { return pathMentions(p, "attachment-deliveries") }

// isDeliveryURL applies deliveryPath to both forms of u's path.
func isDeliveryURL(u *url.URL) bool {
	return u != nil && (deliveryPath(u.Path) || deliveryPath(u.EscapedPath()))
}

// isArtifactURL applies artifactPath to both forms of u's path: the
// decoded path and the escaped path the router may see.
func isArtifactURL(u *url.URL) bool {
	return u != nil && (artifactPath(u.Path) || artifactPath(u.EscapedPath()))
}

// redactedPath returns the path u is logged with when it must not be
// logged as is (RedactedArtifactPath, RedactedPortViewPath,
// RedactedDeliveryPath, or RedactedPath when it is still encoded and names
// none of them), or "".
func redactedPath(u *url.URL) string {
	if u == nil {
		return ""
	}
	word := func(w string) bool {
		a, _ := scanPath(u.Path, w)
		b, _ := scanPath(u.EscapedPath(), w)
		return a || b
	}
	switch {
	case word("artifacts"):
		return RedactedArtifactPath
	case word("portview"):
		return RedactedPortViewPath
	case word("attachment-deliveries"):
		return RedactedDeliveryPath
	case stillEncoded(u.Path) || stillEncoded(u.EscapedPath()):
		// Still encoded when decoding stopped: neither route can be told.
		return RedactedPath
	}
	return ""
}

// IsCredentialURL reports whether a request for u must not be recorded
// with its path or query, for example in a trace: an artifact path
// (artifactPath), a port view path (pathMentions with "portview"), an
// attachment delivery path (deliveryPath), or a query that names any of
// them (credentialQuery). It shares its predicates with RequestPath,
// RedactQuery and RedactURL.
func IsCredentialURL(u *url.URL) bool {
	return redactedPath(u) != "" || (u != nil && credentialQuery(u.RawQuery))
}

// RequestPath returns r's URL path for a log line: RedactedArtifactPath
// for any request under the artifact routes, RedactedPortViewPath for any
// request under the port view route, RedactedDeliveryPath for the
// attachment delivery route (see artifactPath, deliveryPath and
// pathMentions with "portview", which check the decoded and the escaped
// path), the decoded path otherwise. Every log attribute that records a request path must use it.
func RequestPath(r *http.Request) string {
	if r == nil || r.URL == nil {
		return ""
	}
	if p := redactedPath(r.URL); p != "" {
		return p
	}
	return r.URL.Path
}

// RedactURL returns u as a string with its query redacted (see
// RedactQuery) and, for an artifact, port view or attachment delivery
// request (see RequestPath), the path replaced by its redacted form. u is
// not modified.
func RedactURL(u *url.URL) string {
	if u == nil {
		return ""
	}
	c := *u
	c.RawQuery = RedactQuery(u.RawQuery)
	if credentialQuery(u.EscapedFragment()) {
		c.Fragment, c.RawFragment = redactedValue, ""
	}
	if p := redactedPath(u); p != "" {
		c.Path, c.RawPath = p, ""
	}
	return c.String()
}

// RedactReferer returns a request's Referer value for a log line: as sent
// when it parses as a URL that RequestPath would log as is, with its
// credential-bearing query values and artifact or port view path
// redacted (RedactURL) otherwise, and redactedValue when it does not
// parse.
func RedactReferer(referer string) string {
	if referer == "" {
		return ""
	}
	u, err := url.Parse(referer)
	if err != nil {
		return redactedValue
	}
	return RedactURL(u)
}

// SubPath returns p, the path a proxied request is sent with to an agent
// port, for a log line: RedactedPath when it may name an artifact, port
// view or attachment delivery URL (mentionsCredentialRoute), p otherwise.
func SubPath(p string) string {
	if mentionsCredentialRoute(p) {
		return RedactedPath
	}
	return p
}
