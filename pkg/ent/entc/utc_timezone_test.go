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

//go:build !no_sqlite

package entc

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/user"
)

// TestWithUTCTimezone covers the DSN rewrite for every shape OpenSQLite and
// OpenSQLiteReadOnly accept: a bare path, an in-memory name, a "file:" URI
// with no query, a "file:" URI with an existing query, and a DSN that
// already sets "_timezone" to something else. modernc.org/sqlite honours
// only the first "_timezone" value for a key (tz-refactor design §2.1.2), so
// an operator-supplied value must be replaced, not merely appended to.
func TestWithUTCTimezone(t *testing.T) {
	tests := []struct {
		name        string
		dsn         string
		wantBase    string
		wantOptions map[string]string
	}{
		{
			name:        "bare path",
			dsn:         "test.db",
			wantBase:    "test.db",
			wantOptions: map[string]string{"_timezone": "UTC"},
		},
		{
			name:        "in-memory name",
			dsn:         ":memory:",
			wantBase:    ":memory:",
			wantOptions: map[string]string{"_timezone": "UTC"},
		},
		{
			name:        "file URI with no query",
			dsn:         "file:test.db",
			wantBase:    "file:test.db",
			wantOptions: map[string]string{"_timezone": "UTC"},
		},
		{
			name:        "file URI with an existing query",
			dsn:         "file:test.db?mode=memory&cache=shared",
			wantBase:    "file:test.db",
			wantOptions: map[string]string{"mode": "memory", "cache": "shared", "_timezone": "UTC"},
		},
		{
			name:        "operator-supplied _timezone is replaced",
			dsn:         "file:test.db?cache=shared&_timezone=America%2FNew_York",
			wantBase:    "file:test.db",
			wantOptions: map[string]string{"cache": "shared", "_timezone": "UTC"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := withUTCTimezone(tc.dsn)

			base, rawQuery, found := strings.Cut(got, "?")
			require.True(t, found, "rewritten dsn must carry a query string: %q", got)
			assert.Equal(t, tc.wantBase, base)

			values, err := url.ParseQuery(rawQuery)
			require.NoError(t, err)
			require.Len(t, values, len(tc.wantOptions), "unexpected option set: %v", values)
			for k, v := range tc.wantOptions {
				assert.Equal(t, v, values.Get(k), "option %q", k)
			}
		})
	}
}

// TestWithUTCTimezone_MalformedQueryIsNotMasked covers review finding R1-1:
// a query string url.ParseQuery can't parse must not be silently replaced
// with a bare "_timezone=UTC", which would drop every other option (e.g.
// "mode=memory", turning an in-memory database into an on-disk file) and
// mask the error modernc's own url.ParseQuery call would otherwise surface
// at open time. withUTCTimezone must return dsn unchanged in that case.
func TestWithUTCTimezone_MalformedQueryIsNotMasked(t *testing.T) {
	tests := []struct {
		name string
		dsn  string
	}{
		{
			name: "invalid percent-encoding",
			dsn:  "file:/d/h.db?mode=ro&_pragma=foo%zz",
		},
		{
			name: "semicolon breaks url.ParseQuery",
			dsn:  "file:/data/hub.db?mode=ro&_pragma=busy_timeout(5000);x",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, rawQuery, _ := strings.Cut(tc.dsn, "?")
			_, err := url.ParseQuery(rawQuery)
			require.Error(t, err, "test fixture must actually be malformed")

			got := withUTCTimezone(tc.dsn)
			assert.Equal(t, tc.dsn, got, "a malformed query must be returned unchanged, not masked")
		})
	}
}

// sqlDBFromClient reaches through the ent.Client's dialect.Driver to the
// underlying *sql.DB, so tests can read a column's raw stored text with
// CAST(col AS TEXT) — the only way to see modernc's stored bytes without the
// driver re-parsing them into a Go time.Time first (tz-refactor design
// §2.1.2, U3's reading approach).
func sqlDBFromClient(t *testing.T, client *ent.Client) *sql.DB {
	t.Helper()
	drv, ok := client.Driver().(*entsql.Driver)
	require.True(t, ok, "expected *entsql.Driver, got %T", client.Driver())
	return drv.DB()
}

// queryRawText returns the raw, unconverted text stored in a column, scanned
// as a string rather than a time.Time.
func queryRawText(t *testing.T, db *sql.DB, query string, args ...any) string {
	t.Helper()
	var s string
	require.NoError(t, db.QueryRow(query, args...).Scan(&s))
	return s
}

// TestOpenSQLite_BindsCanonicalUTC asserts that a bare time.Now() bind (via
// the ent-generated Default(time.Now) on User.created) and an explicit
// non-UTC-located SetLastLogin are both stored as canonical "... +0000 UTC"
// text with no trailing " m=..." monotonic reading, regardless of the
// process's time.Local. Run this under TZ=Asia/Tokyo and TZ=Asia/Kathmandu
// in addition to the default CI zone (tz-refactor dev-common.md).
func TestOpenSQLite_BindsCanonicalUTC(t *testing.T) {
	client := newTestClient(t)
	ctx := context.Background()

	tokyo, err := time.LoadLocation("Asia/Tokyo")
	require.NoError(t, err)
	explicit := time.Date(2026, 6, 1, 9, 0, 0, 0, tokyo) // 2026-06-01T00:00:00Z

	u, err := client.User.Create().
		SetEmail("utc-bind@example.com").
		SetDisplayName("UTC Bind").
		SetLastLogin(explicit).
		Save(ctx)
	require.NoError(t, err)

	// The value ent echoes back is already normalised by the hook.
	assert.Equal(t, time.UTC, u.Created.Location())
	require.NotNil(t, u.LastLogin)
	assert.Equal(t, time.UTC, u.LastLogin.Location())
	assert.True(t, explicit.Equal(*u.LastLogin))

	db := sqlDBFromClient(t, client)
	createdText := queryRawText(t, db, `SELECT CAST(created AS TEXT) FROM users WHERE id = ?`, u.ID)
	lastLoginText := queryRawText(t, db, `SELECT CAST(last_login AS TEXT) FROM users WHERE id = ?`, u.ID)

	for _, text := range []string{createdText, lastLoginText} {
		assert.True(t, strings.HasSuffix(text, "+0000 UTC"), "want canonical UTC suffix, got %q", text)
		assert.NotContains(t, text, "m=", "bound value must not carry a monotonic reading: %q", text)
	}
	assert.Equal(t, "2026-06-01 00:00:00 +0000 UTC", lastLoginText)
}

// TestOpenSQLite_LegacyRowReadBack asserts AC2 of tz-refactor task 2: a
// legacy row written with a non-UTC zone suffix (the Time.String() text a
// pre-fix process would have bound) reads back through ent as the correct
// UTC instant once the client is opened with the "_timezone=UTC" DSN option.
// The legacy text is inserted with a raw string bind that bypasses both the
// DSN option (which only canonicalises time.Time binds) and the mutation
// hook (which never sees raw SQL), so this is a faithful simulation of a
// row from before this task's fix, regardless of the host's TZ.
func TestOpenSQLite_LegacyRowReadBack(t *testing.T) {
	tests := []struct {
		name   string
		legacy string // Time.String()-shaped text, as a pre-fix bind would have produced
		wantZ  string // the same instant, RFC3339Nano UTC
	}{
		{
			name:   "JST alphabetic abbreviation",
			legacy: "2026-10-01 13:00:00 +0900 JST",
			wantZ:  "2026-10-01T04:00:00Z",
		},
		{
			name:   "JST with a monotonic reading",
			legacy: "2026-10-01 23:43:43.309458928 +0900 JST m=+0.088686566",
			wantZ:  "2026-10-01T14:43:43.309458928Z",
		},
	}

	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := newTestClient(t)
			ctx := context.Background()

			u, err := client.User.Create().
				SetEmail(fmt.Sprintf("legacy-row-%d@example.com", i)).
				SetDisplayName("Legacy Row").
				Save(ctx)
			require.NoError(t, err)

			db := sqlDBFromClient(t, client)
			_, err = db.Exec(`UPDATE users SET last_login = ? WHERE id = ?`, tc.legacy, u.ID)
			require.NoError(t, err)

			fetched, err := client.User.Get(ctx, u.ID)
			require.NoError(t, err)
			require.NotNil(t, fetched.LastLogin)

			wantInstant, err := time.Parse(time.RFC3339Nano, tc.wantZ)
			require.NoError(t, err)

			assert.Equal(t, time.UTC, fetched.LastLogin.Location())
			assert.True(t, wantInstant.Equal(*fetched.LastLogin), "got %s, want instant %s", fetched.LastLogin, wantInstant)
		})
	}
}

// TestOpenSQLite_ThresholdBindMatchesCanonicalCount is the 5-vs-3 fixture
// from tz-refactor design §2.1.2: five rows are written at distinct instants
// through the canonicalising client, and a "last_login < threshold"
// predicate bound with a time.Time located in Asia/Kathmandu (a zone with a
// four-digit numeric offset, +0545) must still match exactly the rows whose
// instant is before the threshold — not whatever a raw Kathmandu-zoned
// wall-clock text comparison would produce. The DSN option canonicalises
// every SQLite bind, hooked or not, which is what makes a non-UTC-located
// threshold safe to use directly.
func TestOpenSQLite_ThresholdBindMatchesCanonicalCount(t *testing.T) {
	client := newTestClient(t)
	ctx := context.Background()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		_, err := client.User.Create().
			SetEmail(fmt.Sprintf("threshold-fixture-%d@example.com", i)).
			SetDisplayName("Threshold Fixture").
			SetLastLogin(base.Add(time.Duration(i) * time.Hour)).
			Save(ctx)
		require.NoError(t, err)
	}

	kathmandu, err := time.LoadLocation("Asia/Kathmandu")
	require.NoError(t, err)
	// Rows 0,1,2 (three rows) are strictly before base+3h; build the
	// threshold as that same instant but located in Kathmandu, so its bound
	// text would be a numeric-abbreviation wall clock if the driver did not
	// canonicalise it.
	threshold := base.Add(3 * time.Hour).In(kathmandu)

	n, err := client.User.Query().Where(user.LastLoginLT(threshold)).Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, 3, n)
}

// TestHubSetting_HookCoversCreateAndUpdate exercises the mutation hook on
// both the create and the update path for a schema with an
// UpdateDefault(time.Now) field (HubSetting.update_time), which is the path
// tz-refactor task 2 is defending on Postgres (field.Time maps to timestamptz there,
// so the hook's only observable effect is the echoed-back value;
// tz-refactor design §2.1.2).
func TestHubSetting_HookCoversCreateAndUpdate(t *testing.T) {
	client := newTestClient(t)
	ctx := context.Background()

	hs, err := client.HubSetting.Create().
		SetSection("tz-refactor-test").
		SetValue(json.RawMessage(`{}`)).
		Save(ctx)
	require.NoError(t, err)
	assert.Equal(t, time.UTC, hs.CreateTime.Location())
	assert.Equal(t, time.UTC, hs.UpdateTime.Location())

	updated, err := client.HubSetting.UpdateOneID(hs.ID).
		SetValue(json.RawMessage(`{"k":"v"}`)).
		Save(ctx)
	require.NoError(t, err)
	assert.Equal(t, time.UTC, updated.UpdateTime.Location())

	db := sqlDBFromClient(t, client)
	text := queryRawText(t, db, `SELECT CAST(update_time AS TEXT) FROM hub_settings WHERE id = ?`, hs.ID)
	assert.True(t, strings.HasSuffix(text, "+0000 UTC"), "want canonical UTC suffix, got %q", text)
	assert.NotContains(t, text, "m=")
}
