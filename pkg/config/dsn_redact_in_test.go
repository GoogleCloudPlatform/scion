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
	"fmt"
	"net/url"
	"strings"
	"testing"
)

func TestRedactDSNIn(t *testing.T) {
	const (
		urlDSN     = "postgres://hub:s3cr3t-pw-91@db.internal:5432/hub?sslmode=require"
		urlDSNRed  = "postgres://hub:xxxxx@db.internal:5432/hub?sslmode=require"
		kvDSN      = `host=db password='kv-s3cr3t' dbname="hub"`
		kvDSNRed   = `host=db password=xxxxx dbname="hub"`
		dollarDSN  = "postgres://hub:pa$$w0rd-77@db.internal/hub"
		dollarRed  = "postgres://hub:xxxxx@db.internal/hub"
		quoteDSN   = "postgres://hub:it's-s3cret@db/hub"
		quoteRed   = "postgres://hub:xxxxx@db/hub"
		socketDSN  = "postgres://hub:sock-s3cr3t@/hub?host=/cloudsql/p:r:i"
		queryPwDSN = "postgres://hub@db/hub?sslmode=disable&password=q-s3cr3t"
	)
	cases := []struct {
		name, driver, dsn, text, want string
	}{
		{"empty dsn", "postgres", "", "nothing " + urlDSN, "nothing " + urlDSN},
		{"sqlite path unchanged", "sqlite", "/var/lib/hub.db", "open /var/lib/hub.db: denied", "open /var/lib/hub.db: denied"},
		{"raw URL DSN", "postgres", urlDSN, "dial " + urlDSN + ": refused", "dial " + urlDSNRed + ": refused"},
		{"raw keyword DSN", "postgres", kvDSN, "open " + kvDSN + ": refused", "open " + kvDSNRed + ": refused"},
		{"quoted DSN", "postgres", kvDSN, fmt.Sprintf("open %q: refused", kvDSN), `open "` + kvDSNRed + `": refused`},
		{"password alone", "postgres", urlDSN, "auth failed for s3cr3t-pw-91", "auth failed for xxxxx"},
		{"password quoted", "postgres", kvDSN, fmt.Sprintf("bad value %q", "kv-s3cr3t"), `bad value "xxxxx"`},
		{"password inside another URL", "postgres", dollarDSN, "GET https://x/?p=" + url.QueryEscape("pa$$w0rd-77"), "GET https://x/?p=xxxxx"},
		{"socket URL password", "postgres", socketDSN, "pw sock-s3cr3t", "pw xxxxx"},
		{"query password", "postgres", queryPwDSN, "pw q-s3cr3t", "pw xxxxx"},
		{"short text untouched", "postgres", urlDSN, "no secret here", "no secret here"},
		// Environment dumps (env, export -p, declare -x, set).
		{"env dump, env(1)", "postgres", urlDSN,
			"HOME=/home/hub\nSCION_PREFLIGHT_DB=" + urlDSN + "\nPATH=/usr/bin\n",
			"HOME=/home/hub\nSCION_PREFLIGHT_DB=" + urlDSNRed + "\nPATH=/usr/bin\n"},
		{"env dump, declare -x escapes $", "postgres", dollarDSN,
			"declare -x HOME=\"/home/hub\"\ndeclare -x SCION_PREFLIGHT_DB=\"postgres://hub:pa\\$\\$w0rd-77@db.internal/hub\"\n",
			"declare -x HOME=\"/home/hub\"\ndeclare -x SCION_PREFLIGHT_DB=" + dollarRed + "\n"},
		{"env dump, set single-quotes", "postgres", quoteDSN,
			"SCION_PREFLIGHT_DB='postgres://hub:it'\\''s-s3cret@db/hub'\nTERM=xterm\n",
			"SCION_PREFLIGHT_DB=" + quoteRed + "\nTERM=xterm\n"},
		{"env dump, export with CRLF", "postgres", urlDSN,
			"export SCION_PREFLIGHT_DB='" + urlDSN + "'\r\nexport A=b\r\n",
			"export SCION_PREFLIGHT_DB=" + urlDSNRed + "\r\nexport A=b\r\n"},
		{"env dump, credential in another variable", "postgres", dollarDSN,
			"declare -x PGPASSWORD=\"pa\\$\\$w0rd-77\"\nUSER=hub\n",
			"declare -x PGPASSWORD=xxxxx\nUSER=hub\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactDSNIn(tc.text, tc.driver, tc.dsn)
			if got != tc.want {
				t.Errorf("RedactDSNIn:\n text %q\n got  %q\n want %q", tc.text, got, tc.want)
			}
		})
	}
}

// No rendering of a password survives, whatever form the text uses.
func TestRedactDSNIn_NeverLeaksThePassword(t *testing.T) {
	for _, pw := range []string{"s3cr3t-pw-91", "pa$$w0rd-77", "it's-s3cret", `q"uo\te-pw`, "sp ace-pw"} {
		dsn := "postgres://hub:" + url.UserPassword("hub", pw).String()[len("hub:"):] + "@db/hub"
		got, _ := url.Parse(dsn)
		if p, _ := got.User.Password(); p != pw {
			t.Fatalf("test DSN %q does not carry %q", dsn, pw)
		}
		shellDouble := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `\$`, "`", "\\`").Replace(dsn)
		shellSingle := strings.ReplaceAll(dsn, `'`, `'\''`)
		text := strings.Join([]string{
			"raw " + dsn,
			fmt.Sprintf("quoted %q", dsn),
			fmt.Sprintf("pw %q", pw),
			"escaped " + url.QueryEscape(pw) + " " + url.PathEscape(pw),
			"SCION_PREFLIGHT_DB=" + dsn,
			`declare -x SCION_PREFLIGHT_DB="` + shellDouble + `"`,
			`SCION_PREFLIGHT_DB='` + shellSingle + `'`,
		}, "\n")
		out := RedactDSNIn(text, "postgres", dsn)
		for _, form := range []string{pw, QuotedBody(pw), url.QueryEscape(pw), url.PathEscape(pw), url.UserPassword("u", pw).String()[2:]} {
			if strings.Contains(out, form) {
				t.Errorf("password %q leaked as %q in:\n%s", pw, form, out)
			}
		}
		if strings.Contains(out, shellDouble) || strings.Contains(out, shellSingle) {
			t.Errorf("a shell-quoted DSN leaked in:\n%s", out)
		}
	}
}

// QuotedBody is the inside of strconv.Quote.
func TestQuotedBody(t *testing.T) {
	for in, want := range map[string]string{"a": "a", `a"b`: `a\"b`, "a\nb": `a\nb`, `a\b`: `a\\b`} {
		if got := QuotedBody(in); got != want {
			t.Errorf("QuotedBody(%q) = %q, want %q", in, got, want)
		}
	}
}

// A secret shorter than 3 characters is not replaced on its own.
func TestRedactSecretForms_ShortSecretLeftAlone(t *testing.T) {
	if got := RedactSecretForms("ab cd", "ab"); got != "ab cd" {
		t.Errorf("RedactSecretForms with a 2-character secret = %q", got)
	}
	if got := RedactSecretForms("abc cd", "abc"); got != "xxxxx cd" {
		t.Errorf("RedactSecretForms with a 3-character secret = %q", got)
	}
}
