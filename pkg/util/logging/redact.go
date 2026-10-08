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
	"net/url"
	"path"
	"strings"
)

// redactedValue replaces the value of a credential-bearing query parameter.
const redactedValue = "REDACTED"

// credentialQueryParams are query parameters whose values are bearer
// credentials and must never be written to logs. "sig" carries the Hub's
// skill file capability signature: anyone holding it can replay the download
// until it expires.
var credentialQueryParams = map[string]bool{
	"sig": true,
}

// RedactQuery returns rawQuery with the values of credential-bearing
// parameters replaced by REDACTED. Other parameters, and their order, are
// preserved. A query that cannot be parsed is dropped entirely rather than
// risk logging a credential.
func RedactQuery(rawQuery string) string {
	if rawQuery == "" {
		return ""
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

// credentialPathPrefixes are routes whose path, after the prefix, starts
// with a bearer credential: an artifact share-link token or an artifact
// view capability. Anyone holding such a path can replay it.
var credentialPathPrefixes = []string{
	"/api/v1/artifacts/shared/",
	"/api/v1/artifacts/view/",
}

// credentialPathPrefix returns the credential route p is under, or "". A
// path that only reaches the route once cleaned (dot segments, doubled
// slashes) counts, since the router cleans it the same way.
func credentialPathPrefix(p string) string {
	clean := path.Clean("/" + p)
	for _, prefix := range credentialPathPrefixes {
		if strings.HasPrefix(clean, prefix) || strings.Contains(p, prefix) {
			return prefix
		}
	}
	return ""
}

// IsCredentialPath reports whether the URL path p carries a bearer
// credential (see RedactPath), so the request must not be recorded with
// its path, for example in a trace.
func IsCredentialPath(p string) bool { return credentialPathPrefix(p) != "" }

// RedactPath returns the URL path p for a log line: a path under a route
// whose next segment is a bearer credential is cut to the route followed
// by REDACTED (dropping the rest, which may also be under the credential);
// any other path is returned unchanged.
func RedactPath(p string) string {
	if prefix := credentialPathPrefix(p); prefix != "" {
		return prefix + redactedValue
	}
	return p
}

// RedactURL returns u as a string with credential-bearing query parameter
// values and credential path segments redacted (see RedactQuery and
// RedactPath). u is not modified.
func RedactURL(u *url.URL) string {
	if u == nil {
		return ""
	}
	c := *u
	c.RawQuery = RedactQuery(u.RawQuery)
	prefix := credentialPathPrefix(u.Path)
	if prefix == "" {
		prefix = credentialPathPrefix(u.EscapedPath())
	}
	if prefix != "" {
		c.Path, c.RawPath = prefix+redactedValue, ""
	}
	return c.String()
}
