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
	"strings"
	"testing"
)

func TestRedactDatabaseURL(t *testing.T) {
	const fakePassword = "hunter2fake"

	tests := []struct {
		name   string
		driver string
		dsn    string
		want   string
	}{
		{
			name:   "empty dsn",
			driver: "postgres",
			dsn:    "",
			want:   "",
		},
		{
			name:   "sqlite path unchanged",
			driver: "sqlite",
			dsn:    "/data/hub.db",
			want:   "/data/hub.db",
		},
		{
			name:   "sqlite driver case-insensitive",
			driver: "SQLite",
			dsn:    "hub.db",
			want:   "hub.db",
		},
		{
			name:   "sqlite empty url stays empty",
			driver: "sqlite",
			dsn:    "",
			want:   "",
		},
		{
			name:   "postgres url with plain password",
			driver: "postgres",
			dsn:    "postgres://scion:" + fakePassword + "@db.example.com:5432/scion?sslmode=require",
			want:   "postgres://scion:xxxxx@db.example.com:5432/scion?sslmode=require",
		},
		{
			name:   "postgres url with url-encoded password",
			driver: "postgres",
			dsn:    "postgres://scion:" + strings.ReplaceAll(fakePassword, "2", "%402") + "@db.example.com/scion",
			want:   "postgres://scion:xxxxx@db.example.com/scion",
		},
		{
			name:   "postgres url with cloudsql host query param",
			driver: "postgres",
			dsn:    "postgres://scion:" + fakePassword + "@/scion?host=/cloudsql/proj:region:instance&sslmode=disable",
			want:   "postgres://scion:xxxxx@/scion?host=/cloudsql/proj:region:instance&sslmode=disable",
		},
		{
			name:   "postgres url with password only in query string",
			driver: "postgres",
			dsn:    "postgres://db.example.com/scion?password=" + fakePassword + "&sslmode=disable",
			want:   "postgres://db.example.com/scion?password=xxxxx&sslmode=disable",
		},
		{
			name:   "postgres url with sslpassword in query string",
			driver: "postgres",
			dsn:    "postgres://db.example.com/scion?sslcert=/etc/certs/client.crt&sslpassword=" + fakePassword,
			want:   "postgres://db.example.com/scion?sslcert=/etc/certs/client.crt&sslpassword=xxxxx",
		},
		{
			name:   "postgres url with no credentials",
			driver: "postgres",
			dsn:    "postgres://db.example.com:5432/scion?sslmode=disable",
			want:   "postgres://db.example.com:5432/scion?sslmode=disable",
		},
		{
			name:   "libpq keyword value form",
			driver: "postgres",
			dsn:    "host=10.0.0.5 port=5432 user=scion password=" + fakePassword + " dbname=scion sslmode=disable",
			want:   "host=10.0.0.5 port=5432 user=scion password=xxxxx dbname=scion sslmode=disable",
		},
		{
			name:   "libpq form with cloudsql unix socket host",
			driver: "postgres",
			dsn:    "host=/cloudsql/proj:region:instance user=scion password=" + fakePassword + " dbname=scion",
			want:   "host=/cloudsql/proj:region:instance user=scion password=xxxxx dbname=scion",
		},
		{
			name:   "libpq form with single-quoted password",
			driver: "postgres",
			dsn:    "host=localhost user=scion password='" + fakePassword + " with space' dbname=scion",
			want:   "host=localhost user=scion password=xxxxx dbname=scion",
		},
		{
			name:   "libpq form with double-quoted password",
			driver: "postgres",
			dsn:    `host=localhost user=scion password="` + fakePassword + ` with space" dbname=scion`,
			want:   "host=localhost user=scion password=xxxxx dbname=scion",
		},
		{
			name:   "libpq form with sslpassword",
			driver: "postgres",
			dsn:    "host=localhost user=scion sslpassword=" + fakePassword + " dbname=scion",
			want:   "host=localhost user=scion sslpassword=xxxxx dbname=scion",
		},
		{
			// libpq's unquoted value grammar has no escape for "=": an
			// unquoted password runs up to the next whitespace and may
			// itself contain "=". The narrower [^=\s]+ alternative some
			// reviewers suggest would stop at the first "=" and leak the
			// tail, e.g. password=ab=cd would redact to password=xxxxx=cd.
			// \S+ is required to mask the value in full.
			name:   "libpq unquoted password containing = is masked in full",
			driver: "postgres",
			dsn:    "host=localhost user=scion password=" + fakePassword + "=tail dbname=scion",
			want:   "host=localhost user=scion password=xxxxx dbname=scion",
		},
		{
			// Same leak risk as above, but for a credential carried in a
			// URL query parameter rather than the libpq keyword/value form.
			name:   "url query password containing = is masked in full, sslmode preserved",
			driver: "postgres",
			dsn:    "postgres://db.example.com/scion?password=" + fakePassword + "=tail&sslmode=disable",
			want:   "postgres://db.example.com/scion?password=xxxxx&sslmode=disable",
		},
		{
			// pgx's parseKeywordValueSettings (pgconn/config.go:627-662) and
			// libpq's conninfo_parse agree: after "=" the parser trims
			// leading whitespace, then reads an unquoted value up to the
			// next whitespace, with "=" permitted inside that value. So for
			// "password= dbname=scion" the driver itself parses the
			// password as the literal string "dbname=scion" -- there is no
			// separate dbname keyword here. Masking the whole span is
			// correct, not a bug: a narrower pattern that stopped at the
			// first "=" would incorrectly leave "dbname=scion" visible even
			// though the driver treats it as (part of) the password.
			name:   "libpq empty-looking password consumes trailing dbname per driver grammar",
			driver: "postgres",
			dsn:    "host=localhost password= dbname=scion",
			want:   "host=localhost password=xxxxx",
		},
		{
			name:   "unparseable dsn is fully masked",
			driver: "postgres",
			dsn:    "definitely not a connection string",
			want:   redactedPlaceholder,
		},
		{
			name:   "malformed url falls back to placeholder",
			driver: "postgres",
			dsn:    "postgres://[::1",
			want:   redactedPlaceholder,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RedactDatabaseURL(tt.driver, tt.dsn)
			if got != tt.want {
				t.Errorf("RedactDatabaseURL(%q, %q) = %q, want %q", tt.driver, tt.dsn, got, tt.want)
			}
			if tt.dsn != "" && strings.Contains(got, fakePassword) {
				t.Errorf("RedactDatabaseURL(%q, %q) = %q leaked the password", tt.driver, tt.dsn, got)
			}
		})
	}
}

// TestRedactDatabaseURL_NeverLeaksPassword is a defense-in-depth sweep: for
// every non-trivial dsn shape tested above, the raw password substring must
// never appear in the redacted output, regardless of what exact masking
// convention is used.
func TestRedactDatabaseURL_NeverLeaksPassword(t *testing.T) {
	const fakePassword = "supersecretvalue"
	dsns := []struct {
		driver string
		dsn    string
	}{
		{"postgres", "postgres://scion:" + fakePassword + "@db.example.com/scion"},
		{"postgres", "postgresql://scion:" + fakePassword + "@db.example.com/scion?sslmode=require"},
		{"postgres", "postgres://db.example.com/scion?password=" + fakePassword},
		{"postgres", "host=localhost user=scion password=" + fakePassword + " dbname=scion"},
		{"postgres", "host=localhost user=scion password='" + fakePassword + "' dbname=scion"},
		{"", "postgres://scion:" + fakePassword + "@db.example.com/scion"},
	}
	for _, d := range dsns {
		got := RedactDatabaseURL(d.driver, d.dsn)
		if strings.Contains(got, fakePassword) {
			t.Errorf("RedactDatabaseURL(%q, %q) = %q leaked the password", d.driver, d.dsn, got)
		}
	}
}
