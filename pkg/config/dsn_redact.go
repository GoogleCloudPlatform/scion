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

package config

import (
	"net/url"
	"regexp"
	"strings"
)

// redactedPlaceholder replaces a database connection string that could not
// be confidently parsed. Returning a fixed placeholder instead of the raw
// input guarantees that content we failed to recognize -- and therefore
// failed to vet for an embedded credential -- never reaches a log line.
const redactedPlaceholder = "<redacted>"

// redactedSecret replaces any password value RedactDatabaseURL masks. This
// matches the "xxxxx" convention already used by `scion admin promote`.
const redactedSecret = "xxxxx"

// connectionURLPattern matches the scheme prefix of a URL-style connection
// string, e.g. "postgres://" or "postgresql://".
var connectionURLPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*://`)

// keywordValueCredentialPattern matches a libpq keyword/value credential
// (password=... or sslpassword=...), including a single- or double-quoted
// value, so it can be masked without disturbing the rest of the DSN.
var keywordValueCredentialPattern = regexp.MustCompile(
	`(?i)\b(password|sslpassword)\s*=\s*('(?:[^'\\]|\\.)*'|"(?:[^"\\]|\\.)*"|\S+)`,
)

// credentialQueryKeys lists query-string parameter names that carry a
// credential and must be masked wherever they appear, e.g. in the query
// string of a URL-style connection string.
var credentialQueryKeys = map[string]bool{
	"password":    true,
	"sslpassword": true,
}

// RedactDatabaseURL returns dsn with any embedded database credential
// masked, safe to write to a log line, error message, or stdout. It never
// returns the original password.
//
// It understands the connection string shapes used across this codebase:
//   - a URL-style connection string, e.g.
//     "postgres://user:pw@host/db?sslmode=require", including a
//     "host=/cloudsql/..." query parameter for Cloud SQL Unix sockets and a
//     URL-encoded password
//   - libpq keyword/value pairs, e.g.
//     `host=... user=... password=secret dbname=...`, including quoted
//     values
//   - a credential carried in a query parameter rather than (or in addition
//     to) the URL userinfo, e.g. "?password=..." or "&sslpassword=..."
//   - a bare SQLite path, which carries no credential and is returned
//     unchanged
//
// An empty dsn is always returned unchanged, as is any dsn when driver is
// "sqlite" (SQLite connection strings are filesystem paths, never
// credentials). Anything else that cannot be confidently parsed as one of
// the recognized shapes is replaced with a fixed placeholder rather than
// risk leaking a credential embedded in a format this function does not
// recognize.
func RedactDatabaseURL(driver, dsn string) string {
	if dsn == "" {
		return dsn
	}
	if strings.EqualFold(driver, "sqlite") {
		return dsn
	}
	switch {
	case connectionURLPattern.MatchString(dsn):
		return redactConnectionURL(dsn)
	case strings.Contains(dsn, "="):
		return keywordValueCredentialPattern.ReplaceAllString(dsn, "${1}="+redactedSecret)
	default:
		// Not a recognizable URL or libpq keyword/value DSN. Since driver
		// isn't "sqlite" (handled above), we have no basis for trusting this
		// is a harmless bare path -- fail safe rather than risk an
		// unrecognized credential format reaching a log line.
		return redactedPlaceholder
	}
}

// redactConnectionURL masks the password in a URL-style connection string,
// whether it appears in the userinfo component or as a query parameter. It
// falls back to a placeholder if dsn cannot be parsed as a URL at all.
func redactConnectionURL(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return redactedPlaceholder
	}
	if u.User != nil {
		if _, has := u.User.Password(); has {
			u.User = url.UserPassword(u.User.Username(), redactedSecret)
		}
	}
	if u.RawQuery != "" {
		u.RawQuery = redactQueryCredentials(u.RawQuery)
	}
	return u.String()
}

// redactQueryCredentials masks any credential parameter (see
// credentialQueryKeys) in a raw URL query string, leaving every other
// parameter -- including an unencoded "host=/cloudsql/..." value -- exactly
// as it was.
func redactQueryCredentials(rawQuery string) string {
	pairs := strings.Split(rawQuery, "&")
	for i, pair := range pairs {
		if pair == "" {
			continue
		}
		key := pair
		if idx := strings.IndexByte(pair, '='); idx >= 0 {
			key = pair[:idx]
		}
		unescapedKey, err := url.QueryUnescape(key)
		if err != nil {
			unescapedKey = key
		}
		if credentialQueryKeys[strings.ToLower(unescapedKey)] {
			pairs[i] = key + "=" + redactedSecret
		}
	}
	return strings.Join(pairs, "&")
}
