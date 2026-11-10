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
	"bytes"
	"context"
	"encoding/json"
	"strings"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/predicate"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// The recorded native Cloud Run decision (AgentAppliedConfig.NativeCloudRun,
// the applied_config JSON key "nativeCloudRun") is a store invariant: only
// SetAgentNativeCloudRun changes it. Every other write of applied_config
// keeps the value the row holds, whatever the caller's copy carries:
//
//   - a whole-document write (UpdateAgent through buildAgentUpdate, and
//     UpdateAgentConfig) builds its document with appliedConfigForWrite,
//     which reads the row's value and returns a predicate the write adds,
//     so the write applies only while the row still holds that value (a
//     SetAgentNativeCloudRun between the read and the write makes it match
//     no row: ErrVersionConflict, and the caller's retry reads again);
//   - CreateAgent never writes it (a new row has no recorded decision);
//   - the key edits (SetAgentRuntimeTarget, ClearAgentRuntimeTarget) and the
//     writes that start from the row's own document (the launch report, the
//     agent-role backfill) carry its value through.
//
// TestAppliedConfigWritersPreserveNativeCloudRun enumerates every
// applied_config writer of this package, so a new one has to be classified.

// nativeCloudRunKey is the applied_config JSON key of
// AgentAppliedConfig.NativeCloudRun.
const nativeCloudRunKey = "nativeCloudRun"

// rowNativeCloudRun reports whether a stored applied_config document
// records the native decision: it is a JSON object whose top-level key
// spelled exactly nativeCloudRun holds the boolean true. Key case matters,
// as it does in SQL (a struct decode would match keys in any case). An
// empty or invalid document, or any other value, does not.
func rowNativeCloudRun(raw string) bool {
	if raw == "" {
		return false
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return false
	}
	return bytes.Equal(bytes.TrimSpace(doc[nativeCloudRunKey]), []byte("true"))
}

// marshalAppliedConfigNative is marshalAppliedConfig with NativeCloudRun set
// to native on a copy of cfg (the caller's struct is not changed). A nil cfg
// stays empty unless native, which keeps the key alone.
func marshalAppliedConfigNative(cfg *store.AgentAppliedConfig, native bool) string {
	if cfg == nil {
		if !native {
			return ""
		}
		return marshalAppliedConfig(&store.AgentAppliedConfig{NativeCloudRun: true})
	}
	c := *cfg
	c.NativeCloudRun = native
	return marshalAppliedConfig(&c)
}

// appliedConfigForWrite is the one place a whole-document applied_config
// write is built. It reads the row's recorded native decision (locked with
// FOR UPDATE when lock is set, inside the caller's transaction) and returns
// the document to write, carrying that value whatever cfg says, the value
// itself, and the predicate the write must add (appliedNativeCloudRunIs) so
// it applies only while the row still holds it. A missing row reads as not
// native; the write then matches no row and reports that.
func appliedConfigForWrite(ctx context.Context, ac *ent.AgentClient, uid uuid.UUID, cfg *store.AgentAppliedConfig, lock bool) (raw string, native bool, keep predicate.Agent, err error) {
	q := ac.Query().Where(agent.IDEQ(uid))
	if lock {
		q = q.ForUpdate()
	}
	row, err := q.Select(agent.FieldAppliedConfig).Only(ctx)
	stored := ""
	switch {
	case err == nil:
		stored = row.AppliedConfig
		native = rowNativeCloudRun(stored)
	case ent.IsNotFound(err):
	default:
		return "", false, nil, mapError(err)
	}
	return marshalAppliedConfigNative(cfg, native), native, appliedConfigKeep(stored, native), nil
}

// appliedConfigKeep is the predicate a whole-document write adds after
// reading raw (the row's applied_config) and native (rowNativeCloudRun of
// it). For an empty or valid JSON document it is appliedNativeCloudRunIs,
// so writes that only race other keys still apply. For a document that is
// not valid JSON, or that holds any \u escape, it is
// appliedConfigUnchanged (appliedConfigKeepUsesJSON): no JSON parse runs
// in SQL (a Postgres cast of such a document would fail the write), and
// native is what rowNativeCloudRun read in Go. The hub's
// writers only write valid JSON, so a document that becomes valid between
// the read and the write makes the write conflict and the caller read
// again.
func appliedConfigKeep(raw string, native bool) predicate.Agent {
	if !appliedConfigKeepUsesJSON(raw) {
		return appliedConfigUnchanged(raw)
	}
	return appliedNativeCloudRunIs(native)
}

// appliedConfigKeepUsesJSON reports whether appliedConfigKeep uses the JSON
// predicate for raw: an empty document, or a valid JSON document with no
// \u escape at all. A Postgres jsonb cast rejects some escapes Go accepts
// (\u0000, a lone surrogate), so any document with an escape is matched by
// its exact text instead, as an invalid one is. The hub's marshal writes an
// escape for control characters, <, > and & (HTML escaping) and invalid
// UTF-8, which are common in task text. The text match is always correct;
// the whole-row writes also compare state_version.
func appliedConfigKeepUsesJSON(raw string) bool {
	if raw == "" {
		return true
	}
	return json.Valid([]byte(raw)) && !strings.Contains(raw, `\u`)
}

// appliedNativeCloudRunIs returns an Ent predicate restricting a write to
// rows whose applied_config records the native decision exactly when native
// is true: the top-level nativeCloudRun key holds the JSON boolean true. An
// empty or NULL document, a document that is not an object, and any other
// value (false, a string, a number) record none, as in rowNativeCloudRun.
// applied_config is a text column, so Postgres casts it; the CASE keeps a
// NULL or empty document from reaching the cast, and appliedConfigKeep
// never uses this predicate for a document that is not valid JSON.
func appliedNativeCloudRunIs(native bool) predicate.Agent {
	return func(s *entsql.Selector) {
		col := s.C(agent.FieldAppliedConfig)
		switch s.Dialect() {
		case dialect.Postgres:
			s.Where(entsql.P(func(b *entsql.Builder) {
				b.WriteString("(CASE WHEN ").
					WriteString(col).
					WriteString(" IS NULL OR ").
					WriteString(col).
					WriteString(" = '' THEN false ELSE COALESCE((").
					WriteString(col).
					WriteString("::jsonb -> '" + nativeCloudRunKey + "') = 'true'::jsonb, false) END) = ").
					Arg(native)
			}))
		default: // SQLite
			want := 0
			if native {
				want = 1
			}
			s.Where(entsql.P(func(b *entsql.Builder) {
				b.WriteString("(CASE WHEN json_valid(").
					WriteString(col).
					WriteString(") THEN COALESCE(json_type(").
					WriteString(col).
					WriteString(", '$." + nativeCloudRunKey + "') = 'true', 0) ELSE 0 END) = ").
					Arg(want)
			}))
		}
	}
}
