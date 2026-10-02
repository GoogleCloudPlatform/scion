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
