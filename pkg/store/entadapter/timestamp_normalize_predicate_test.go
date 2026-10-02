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

package entadapter

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// TestNonCanonicalSQL_MatchesGoCanonicalForms checks the shared canonical
// predicate against the text Go itself produces, plus the non-canonical
// shapes the normalizer must select.
func TestNonCanonicalSQL_MatchesGoCanonicalForms(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec("CREATE TABLE t (v DATETIME)")
	require.NoError(t, err)

	nonCanonical := func(family columnFamily, v any) bool {
		t.Helper()
		_, err := db.Exec("DELETE FROM t")
		require.NoError(t, err)
		_, err = db.Exec("INSERT INTO t (v) VALUES (?)", v)
		require.NoError(t, err)
		var got bool
		require.NoError(t, db.QueryRow("SELECT EXISTS (SELECT 1 FROM t WHERE "+nonCanonicalSQL("v", family)+")").Scan(&got))
		return got
	}

	base := time.Date(2026, 10, 1, 4, 0, 0, 0, time.UTC)
	for _, frac := range []time.Duration{0, 500 * time.Millisecond, 250 * time.Millisecond,
		10 * time.Millisecond, 123456 * time.Microsecond, 1, 999999999} {
		ts := base.Add(frac)
		assert.False(t, nonCanonical(familyEnt, ts.String()), "ent canonical %q", ts.String())
		rfc := ts.Format(time.RFC3339Nano)
		assert.False(t, nonCanonical(familyWebchat, rfc), "webchat canonical %q", rfc)
		// Each family's canonical text is non-canonical for the other.
		assert.True(t, nonCanonical(familyEnt, rfc), "ent given %q", rfc)
		assert.True(t, nonCanonical(familyWebchat, ts.String()), "webchat given %q", ts.String())
	}

	for _, v := range []any{nil, ""} {
		assert.False(t, nonCanonical(familyEnt, v), "ent %v", v)
		assert.False(t, nonCanonical(familyWebchat, v), "webchat %v", v)
	}

	for _, v := range []string{
		"2026-10-01 04:00:00 +0000 UTC m=+0.5",
		"2026-10-01 04:00:00.50 +0000 UTC",
		"2026-10-01 04:00:00. +0000 UTC",
		"2026-10-01 04:00:00.5x +0000 UTC",
		"2026-10-01 13:00:00 +0900 JST",
		"2026-10-01 09:45:00 +0545 +0545",
		"2026-10-01 06:00:00 +0200 +0200",
		"2026-10-01 04:00:00+00:00",
		"garbage",
	} {
		assert.True(t, nonCanonical(familyEnt, v), "ent %q", v)
	}
	for _, v := range []string{
		"2026-10-01T04:00:00.50Z",
		"2026-10-01T04:00:00.Z",
		"2026-10-01T13:00:00+09:00",
		"2026-10-01 04:00:00+00:00",
		"2026-10-01 04:00:00",
		"garbage",
	} {
		assert.True(t, nonCanonical(familyWebchat, v), "webchat %q", v)
	}
}

// TestUnreadableSQL_MatchesFourDigitAbbreviations checks the four-digit
// probe: it matches the numeric-abbreviation shapes the driver cannot scan
// and nothing else.
func TestUnreadableSQL_MatchesFourDigitAbbreviations(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	match := func(v any) bool {
		t.Helper()
		var got bool
		require.NoError(t, db.QueryRow("SELECT COALESCE("+unreadableSQL("v")+", 0) FROM (SELECT ? AS v)", v).Scan(&got))
		return got
	}
	ktm := time.FixedZone("+0545", 5*3600+45*60)
	for _, v := range []string{
		"2026-10-01 09:45:00 +0545 +0545",
		"2026-10-01 09:45:00.125 +0545 +0545 m=+12.5",
		"2026-10-01 06:00:00 +0200 +0200",
		"2026-09-30 23:30:00 -0430 -0430",
		time.Date(2026, 10, 1, 9, 45, 0, 5, ktm).String(),
		// With a monotonic-clock reading, as time.Now() carries.
		time.Now().In(ktm).String(),
		time.Now().In(time.FixedZone("", 2*3600)).String(),
	} {
		assert.True(t, match(v), "%q", v)
	}
	for _, v := range []any{
		nil, "",
		"2026-10-01 04:00:00 +0000 UTC",
		"2026-10-01 13:00:00 +0900 JST m=+0.5",
		"2026-10-01 01:00:00 -0300 -03",
		"2026-10-01T09:45:00+05:45",
		"garbage +0545 +0545",
	} {
		assert.False(t, match(v), "%v", v)
	}
}

func TestIsSQLiteDialect(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	for d, want := range map[string]bool{"sqlite3": true, "sqlite": true, "postgres": false} {
		got, err := isSQLiteDialect(db, d)
		require.NoError(t, err, d)
		assert.Equal(t, want, got, d)
	}
	_, err = isSQLiteDialect(db, "mysql")
	assert.Error(t, err)
	_, err = isSQLiteDialect(nil, "sqlite3")
	assert.Error(t, err)
}
