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

package entadapter

import (
	"fmt"
	"strings"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/predicate"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/project"
)

// labelContains returns an Ent predicate restricting results to agents whose
// `labels` JSON object contains the given key-value pair.
//
//	SQLite:   json_extract(labels, '$."key"') = ?
//	Postgres: labels @> '{"key":"value"}'::jsonb
//
// The Postgres path embeds both key and value into the @> operand literal
// (after quoting) to use the GIN-indexable containment operator. The SQLite
// path uses json_extract with a parameterised value to avoid SQL injection.
//
// The SQLite json_path uses quoted-member syntax ($."key") so that keys
// containing dots (e.g. "scion.dev/role") are treated as a single top-level
// key rather than nested object traversal.
func labelContains(key, value string) predicate.Agent {
	return func(s *entsql.Selector) {
		col := s.C(agent.FieldLabels)
		switch s.Dialect() {
		case dialect.Postgres:
			s.Where(entsql.P(func(b *entsql.Builder) {
				b.WriteString(col).
					WriteString(" @> ").
					Arg(fmt.Sprintf(`{%q:%q}`, key, value)).
					WriteString("::jsonb")
			}))
		default: // SQLite
			s.Where(entsql.P(func(b *entsql.Builder) {
				escapedKey := strings.ReplaceAll(key, `"`, `\"`)
				b.WriteString("json_extract(").
					WriteString(col).
					WriteString(", ").
					Arg(fmt.Sprintf(`$."%s"`, escapedKey)).
					WriteString(") = ").
					Arg(value)
			}))
		}
	}
}

// projectLabelContains returns an Ent predicate restricting results to
// projects whose `labels` JSON object contains the given key-value pair.
func projectLabelContains(key, value string) predicate.Project {
	return func(s *entsql.Selector) {
		col := s.C(project.FieldLabels)
		switch s.Dialect() {
		case dialect.Postgres:
			s.Where(entsql.P(func(b *entsql.Builder) {
				b.WriteString(col).
					WriteString(" @> ").
					Arg(fmt.Sprintf(`{%q:%q}`, key, value)).
					WriteString("::jsonb")
			}))
		default: // SQLite
			s.Where(entsql.P(func(b *entsql.Builder) {
				escapedKey := strings.ReplaceAll(key, `"`, `\"`)
				b.WriteString("json_extract(").
					WriteString(col).
					WriteString(", ").
					Arg(fmt.Sprintf(`$."%s"`, escapedKey)).
					WriteString(") = ").
					Arg(value)
			}))
		}
	}
}

// projectLabelNotContains returns an Ent predicate restricting results to
// projects whose `labels` JSON object does NOT contain the given key-value pair
// (label absent OR value differs).
func projectLabelNotContains(key, value string) predicate.Project {
	return func(s *entsql.Selector) {
		col := s.C(project.FieldLabels)
		switch s.Dialect() {
		case dialect.Postgres:
			s.Where(entsql.P(func(b *entsql.Builder) {
				b.WriteString("NOT (").
					WriteString(col).
					WriteString(" @> ").
					Arg(fmt.Sprintf(`{%q:%q}`, key, value)).
					WriteString("::jsonb)").
					WriteString(" OR ").
					WriteString(col).
					WriteString(" IS NULL")
			}))
		default: // SQLite
			s.Where(entsql.P(func(b *entsql.Builder) {
				escapedKey := strings.ReplaceAll(key, `"`, `\"`)
				b.WriteString("(json_extract(").
					WriteString(col).
					WriteString(", ").
					Arg(fmt.Sprintf(`$."%s"`, escapedKey)).
					WriteString(") IS NULL OR json_extract(").
					WriteString(col).
					WriteString(", ").
					Arg(fmt.Sprintf(`$."%s"`, escapedKey)).
					WriteString(") != ").
					Arg(value).
					WriteString(")")
			}))
		}
	}
}

// appliedConfigHarnessConfigEquals returns an Ent predicate restricting
// results to agents whose `applied_config` JSON document has a top-level
// "harnessConfig" key equal to value.
//
// Unlike labels and ancestry, applied_config is stored as a plain Ent
// field.Text column (pkg/ent/schema/agent.go), not field.JSON — it is kept
// decoupled from the store package's struct definition. Ent/the schema never
// validates that the column actually holds well-formed JSON (unlike a real
// jsonb column, which Postgres rejects invalid input for at write time), and
// pkg/store/entadapter/agent_store.go's own marshalAppliedConfig /
// parseAppliedConfig tolerate and log malformed rows rather than failing —
// so corrupt applied_config values are a known, expected condition, not a
// hypothetical one (ptone/scion#2146 review R1-4).
//
// A naive `col::jsonb ->> 'harnessConfig'` (or SQLite's plain json_extract)
// throws on the first malformed row it touches, turning one corrupt agent —
// possibly in a project the caller can't even see — into a 500 for every
// harnessConfig-filtered query. The two dialects take different, but each
// version-independent, guards:
//
//	SQLite:   col IS NOT NULL
//	          AND CASE WHEN json_valid(col) THEN json_extract(col, '$.harnessConfig') END = ?
//	Postgres: col IS NOT NULL
//	          AND strpos(col, '"harnessConfig":' || to_json($1::text)::text) > 0
//	          AND (strpos(col, '"env":') = 0
//	               OR strpos(col, '"harnessConfig":' || to_json($2::text)::text) < strpos(col, '"env":'))
//
// SQLite's json_valid/json_extract genuinely parses the document, so it has
// no false-positive risk and no version dependency (json_valid has shipped
// since SQLite 3.38, and this codebase already depends on JSON1 elsewhere).
//
// Postgres has no version-independent "try cast" expression before
// pg_input_is_valid (added in PostgreSQL 16) — this repo documents no
// minimum Postgres version, and evidence points at deployments as old as
// PG15 (ptone/scion#2146 review R2), so an earlier draft that used
// pg_input_is_valid was rejected as a hidden version floor. strpos/to_json
// avoid that: both operate on plain text, never throw on malformed input
// regardless of version, and to_json($n::text) produces the exact quoted,
// escaped bytes Go's encoding/json would write for a plain string value
// (matching what marshalAppliedConfig actually stores) — so a corrupt row
// simply fails the substring match instead of erroring, on every supported
// Postgres version, with no CASE/validity indirection needed at all.
//
// The second condition guards the one real false-positive source: applied
// config has a single later-declared, user-controlled nested map field,
// Env (`AgentAppliedConfig.Env map[string]string`), whose keys are
// arbitrary — an env var literally named "harnessConfig" with a matching
// value would otherwise also match the substring search. Every other field
// has a fixed, non-"harnessConfig" JSON key name, so Env is the only
// possible source of a false positive. Go's encoding/json marshals struct
// fields in declaration order, and Env is declared after HarnessConfig, so
// the *first* occurrence of the search string can only be the real
// top-level key: if it occurs before "env": starts (or "env" is absent
// entirely), it is the top-level field; if the only occurrence is at or
// after "env": starts, it is nested inside Env and must not match. This
// holds regardless of which earlier fields (e.g. Image) happen to be
// present, unlike anchoring to a fixed byte offset.
//
// Known, accepted narrow gap: Go's encoding/json HTML-escapes '<', '>', and
// '&' in string values by default; Postgres's to_json does not. A harness
// config name containing one of those characters would not match. Harness
// config names are validated slugs elsewhere in the system and never
// contain them in practice.
//
// The whole fragment is parenthesized so it composes safely if a future
// caller wraps it in Or/Not (it is only ever ANDed today).
func appliedConfigHarnessConfigEquals(value string) predicate.Agent {
	return func(s *entsql.Selector) {
		col := s.C(agent.FieldAppliedConfig)
		switch s.Dialect() {
		case dialect.Postgres:
			s.Where(entsql.P(func(b *entsql.Builder) {
				b.WriteString("(").
					WriteString(col).
					WriteString(" IS NOT NULL AND strpos(").
					WriteString(col).
					WriteString(`, '"harnessConfig":' || to_json(`).
					Arg(value).
					WriteString("::text)::text) > 0 AND (strpos(").
					WriteString(col).
					WriteString(`, '"env":') = 0 OR strpos(`).
					WriteString(col).
					WriteString(`, '"harnessConfig":' || to_json(`).
					Arg(value).
					WriteString("::text)::text) < strpos(").
					WriteString(col).
					WriteString(`, '"env":')))`)
			}))
		default: // SQLite
			s.Where(entsql.P(func(b *entsql.Builder) {
				b.WriteString("(").
					WriteString(col).
					WriteString(" IS NOT NULL AND CASE WHEN json_valid(").
					WriteString(col).
					WriteString(") THEN json_extract(").
					WriteString(col).
					WriteString(", '$.harnessConfig') END = ").
					Arg(value).
					WriteString(")")
			}))
		}
	}
}

// ancestryContains returns an Ent predicate restricting results to agents whose
// `ancestry` JSON array contains principalID.
//
// JSON-array membership has no portable SQL spelling, so this is the one agent
// query that must dialect-switch its raw fragment. Both dialects expand the
// stored JSON array into a row set and test for membership inside a correlated
// EXISTS subquery, which composes cleanly with the surrounding typed Ent query
// (soft-delete predicate, ordering, pagination, COUNT):
//
//	SQLite:   EXISTS (SELECT 1 FROM json_each(ancestry)
//	                  WHERE json_each.value = ?)
//	Postgres: EXISTS (SELECT 1 FROM jsonb_array_elements_text(ancestry) AS elem
//	                  WHERE elem = $n)
//
// Two dialect details are load-bearing:
//
//   - Function name: Ent stores field.TypeJSON as `jsonb` on Postgres, so the
//     set-returning function must be jsonb_array_elements_text (the json_*
//     variant only accepts the `json` type).
//   - Bind parameter: the fragment is emitted through Builder.Arg, not as a
//     literal "?" via ExprP. ExprP writes raw text verbatim and does NOT rebind
//     "?" to Postgres' "$n" syntax, which produced a syntax error against
//     Postgres. Builder.Arg emits the dialect-correct placeholder ("?" on
//     SQLite, "$n" on Postgres) and tracks the argument index.
//
// The dialect is read from the live selector via Builder.Dialect(), so the same
// store works against either backend with no external configuration.
//
// The ancestry IS NOT NULL guard short-circuits agents with no recorded
// lineage and keeps Postgres from invoking the set-returning function on a NULL
// input.
func ancestryContains(principalID string) predicate.Agent {
	return func(s *entsql.Selector) {
		col := s.C(agent.FieldAncestry)
		switch s.Dialect() {
		case dialect.Postgres:
			s.Where(entsql.P(func(b *entsql.Builder) {
				b.WriteString(col).
					WriteString(" IS NOT NULL AND EXISTS (SELECT 1 FROM jsonb_array_elements_text(").
					WriteString(col).
					WriteString(") AS elem WHERE elem = ").
					Arg(principalID).
					WriteString(")")
			}))
		default: // SQLite and any other backend providing json_each().
			s.Where(entsql.P(func(b *entsql.Builder) {
				b.WriteString(col).
					WriteString(" IS NOT NULL AND EXISTS (SELECT 1 FROM json_each(").
					WriteString(col).
					WriteString(") WHERE json_each.value = ").
					Arg(principalID).
					WriteString(")")
			}))
		}
	}
}
