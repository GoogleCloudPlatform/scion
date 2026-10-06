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
	"strconv"
	"strings"
	"time"

	"entgo.io/ent/dialect"
)

// rebindForDialect rewrites the "?" placeholders of a raw test query to
// Postgres' "$1, $2, ..." when dialectName is Postgres, and returns it
// unchanged otherwise. Postgres rejects "?" with a syntax error at the next
// token. Raw SQL in tests that run through enttest.NewClient (and therefore
// on Postgres in CI) must go through this. The query must not contain a
// literal "?" outside placeholders.
func rebindForDialect(dialectName, query string) string {
	if dialectName != dialect.Postgres {
		return query
	}
	var b strings.Builder
	n := 0
	for _, r := range query {
		if r == '?' {
			n++
			b.WriteString("$" + strconv.Itoa(n))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// sameInstantAtStoredPrecision reports whether got, a time read back from the
// store, is the instant want that was written. SQLite keeps nanoseconds, so
// that is exact equality there. Postgres keeps microseconds; whether the
// sub-microsecond part is truncated (pgx binary encoding) or rounded (a
// text-format bind) depends on the driver path, so exactly those two values
// are accepted as well, and nothing else.
func sameInstantAtStoredPrecision(want, got time.Time) bool {
	return got.Equal(want) ||
		got.Equal(want.Truncate(time.Microsecond)) ||
		got.Equal(want.Round(time.Microsecond))
}
