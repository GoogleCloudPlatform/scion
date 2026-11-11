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
	"encoding/json"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// RedactDSNIn returns text with dsn replaced by its redacted form
// (RedactDatabaseURL) and every credential dsn carries masked in every
// rendering (RedactSecretForms). It covers:
//   - the DSN as given, and as strconv.Quote (and so Go's %q) writes it;
//   - each credential, as given and in its quoted, JSON and URL-escaped
//     forms;
//   - the value of an environment dump line, NAME=value, as env(1) prints
//     it or as a shell's export -p, declare -x or set quotes it: a value
//     that is the DSN is replaced by its redacted form, and a value that
//     carries one of its credentials is masked whole.
//
// An empty dsn returns text unchanged.
func RedactDSNIn(text, driver, dsn string) string {
	if dsn == "" {
		return text
	}
	secrets := dsnSecrets(dsn)
	redacted := RedactDatabaseURL(driver, dsn)
	text = redactEnvDumpValues(text, dsn, redacted, secrets)
	text = strings.ReplaceAll(text, dsn, redacted)
	text = strings.ReplaceAll(text, QuotedBody(dsn), redacted)
	for _, s := range secrets {
		text = RedactSecretForms(text, s)
	}
	return text
}

// dsnSecrets returns the credentials dsn carries: a postgres:// URL's
// userinfo password and password/sslpassword query parameters (also with
// an empty host, as in a Unix-socket URL such as
// "postgres://u:p@/db?host=/cloudsql/..."), or a keyword-form DSN's
// password values.
func dsnSecrets(dsn string) []string {
	var out []string
	if u, err := url.Parse(dsn); err == nil && (strings.EqualFold(u.Scheme, "postgres") || strings.EqualFold(u.Scheme, "postgresql")) {
		if pw, ok := u.User.Password(); ok {
			out = append(out, pw)
		}
		for k, vs := range u.Query() {
			if strings.EqualFold(k, "password") || strings.EqualFold(k, "sslpassword") {
				out = append(out, vs...)
			}
		}
		return out
	}
	for _, m := range keywordValueCredentialPattern.FindAllStringSubmatch(dsn, -1) {
		v := m[2]
		if len(v) >= 2 && (v[0] == '\'' || v[0] == '"') && v[len(v)-1] == v[0] {
			v = strings.NewReplacer(`\'`, `'`, `\"`, `"`, `\\`, `\`).Replace(v[1 : len(v)-1])
		}
		out = append(out, v)
	}
	return out
}

// minSecretLen is the length below which a secret is not replaced on its
// own: replacing a shorter string would rewrite unrelated text.
const minSecretLen = 3

// RedactSecretForms replaces secret (a password or a URL user info)
// everywhere in text: as given, and as strconv.Quote, JSON, url.PathEscape
// and url.QueryEscape write it, so no rendering of an error that quoted it
// shows it. A secret shorter than 3 characters is left alone.
func RedactSecretForms(text, secret string) string {
	if len(secret) < minSecretLen {
		return text
	}
	forms := []string{secret, QuotedBody(secret), url.PathEscape(secret), url.QueryEscape(secret)}
	if b, err := json.Marshal(secret); err == nil {
		forms = append(forms, string(b[1:len(b)-1]))
	}
	for _, f := range forms {
		text = strings.ReplaceAll(text, f, redactedSecret)
	}
	return text
}

// QuotedBody returns s as strconv.Quote writes it, without the outer
// quotes: the form Go's %q puts inside an error message.
func QuotedBody(s string) string {
	q := strconv.Quote(s)
	return q[1 : len(q)-1]
}

// envDumpLinePattern matches one line of an environment dump: an optional
// "export " or "declare -x " prefix, NAME=, then the value (group 2).
var envDumpLinePattern = regexp.MustCompile(`(?m)^([ \t]*(?:export[ \t]+|declare[ \t]+-x[ \t]+)?[A-Za-z_][A-Za-z0-9_]*=)(.*)$`)

// redactEnvDumpValues rewrites the value of every environment dump line
// whose value, unquoted as a shell quotes it, is dsn (to redacted) or
// carries one of secrets (to the masked placeholder). A shell's quoting
// escapes characters that neither the raw nor the strconv.Quote form of a
// credential matches ('$' and '`' in double quotes, ' in single quotes),
// so such a value is compared after unquoting.
func redactEnvDumpValues(text, dsn, redacted string, secrets []string) string {
	return envDumpLinePattern.ReplaceAllStringFunc(text, func(line string) string {
		m := envDumpLinePattern.FindStringSubmatch(line)
		value := strings.TrimRight(m[2], "\r")
		// A rewritten line keeps its line ending: the pattern's value
		// group runs up to a "\n", so it holds a CRLF line's "\r".
		eol := m[2][len(value):]
		v := shellUnquote(value)
		if v == dsn {
			return m[1] + redacted + eol
		}
		for _, s := range secrets {
			if len(s) >= minSecretLen && strings.Contains(v, s) {
				return m[1] + redactedSecret + eol
			}
		}
		return line
	})
}

// shellUnquote undoes the quoting a shell's export -p, declare -x or set
// puts on a value: double quotes, with backslash escapes of the backslash,
// the double quote, '$' and '`'; or single quotes, where a quote in the
// value is written as quote, backslash, quote, quote. Anything else is
// returned as is.
func shellUnquote(v string) string {
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		return strings.NewReplacer(`\\`, `\`, `\"`, `"`, `\$`, `$`, "\\`", "`").Replace(v[1 : len(v)-1])
	}
	if len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'' {
		return strings.ReplaceAll(v[1:len(v)-1], `'\''`, `'`)
	}
	return v
}
