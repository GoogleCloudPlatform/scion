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

	entsql "entgo.io/ent/dialect/sql"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/predicate"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/agentsort"
)

// agentSortOrder returns the ent OrderOption implementing the total order
// for (sortKey, dir), used by ListAgents' sorted-mode branch:
//
//	created: created <dir>, id DESC
//	updated: COALESCE(last_activity_event, updated) <dir>, created DESC, id DESC
//
// The tie-break columns are always DESC regardless of dir, which
// is what makes both directions show ties in the same created-desc order.
// Callers (ListAgents) have already validated sortKey/dir; an unrecognized
// pair here would simply produce no ORDER BY term, so that validation is not
// repeated — this function is not itself part of the fail-closed contract.
func agentSortOrder(sortKey, dir string) agent.OrderOption {
	return func(s *entsql.Selector) {
		s.OrderExpr(entsql.Raw(agentSortOrderExpr(sortKey, dir, s)))
	}
}

// agentSortOrderExpr builds the raw ORDER BY expression text for sortKey/dir
// against the columns of selector s (qualified and normalized via
// timeColumnExpr).
func agentSortOrderExpr(sortKey, dir string, s *entsql.Selector) string {
	dirSQL := "DESC"
	if dir == agentsort.Asc {
		dirSQL = "ASC"
	}
	id := s.C(agent.FieldID)
	created := timeColumnExpr(s, agent.FieldCreated)
	if sortKey == agentsort.Created {
		return fmt.Sprintf("%s %s, %s DESC", created, dirSQL, id)
	}
	key := agentSortKeyExpr(s)
	return fmt.Sprintf("%s %s, %s DESC, %s DESC", key, dirSQL, created, id)
}

// agentSortKeyExpr is the SQL for sort=updated's position key K:
// COALESCE(last_activity_event, updated), both normalized via
// timeColumnExpr. The store writes a zero LastActivityEvent as NULL
// (entAgentToMember and buildAgentUpdate never write a zero time.Time to
// that column), so COALESCE falls through to updated exactly when the
// client's own "0001 means none" rule would; timeColumnExpr's CASE
// expression propagates a NULL input through unchanged (instr(NULL, ...) is
// NULL, which is not > 0, so the ELSE branch -- the column itself, NULL --
// is returned), so COALESCE still sees NULL for "no last activity".
func agentSortKeyExpr(s *entsql.Selector) string {
	return fmt.Sprintf("COALESCE(%s, %s)", timeColumnExpr(s, agent.FieldLastActivityEvent), timeColumnExpr(s, agent.FieldUpdated))
}

// agentAfterCursor returns the keyset predicate for "the next row strictly
// after cur in the (sortKey, dir) walk":
//
//	updated: (K ⋚ k) OR (K = k AND created < c) OR (K = k AND created = c AND id < i)
//	created: (created ⋚ c) OR (created = c AND id < i)
//
// where ⋚ is "<" for dir=desc and ">" for dir=asc (the primary key follows
// dir; the tie-break comparisons are always strictly "<" because the
// tie-break columns always sort DESC regardless of dir — see
// agentSortOrderExpr). K/created are compared via timeColumnExpr
// (normalized on SQLite, bare on Postgres) against cur's timestamps
// converted by timeArg to the matching bound form.
func agentAfterCursor(sortKey, dir string, cur store.AgentCursor) predicate.Agent {
	return func(s *entsql.Selector) {
		primaryOp := "<"
		if dir == agentsort.Asc {
			primaryOp = ">"
		}
		id := s.C(agent.FieldID)
		created := timeColumnExpr(s, agent.FieldCreated)
		dialectName := s.Dialect()
		createdArg := timeArg(dialectName, cur.Created)

		if sortKey == agentsort.Created {
			s.Where(entsql.P(func(b *entsql.Builder) {
				b.WriteString("((").WriteString(created).WriteString(" ").WriteString(primaryOp).WriteString(" ").Arg(createdArg).
					WriteString(") OR (").WriteString(created).WriteString(" = ").Arg(createdArg).
					WriteString(" AND ").WriteString(id).WriteString(" < ").Arg(cur.ID).WriteString("))")
			}))
			return
		}

		key := agentSortKeyExpr(s)
		kArg := timeArg(dialectName, cur.K)
		s.Where(entsql.P(func(b *entsql.Builder) {
			b.WriteString("((").WriteString(key).WriteString(" ").WriteString(primaryOp).WriteString(" ").Arg(kArg).
				WriteString(") OR (").WriteString(key).WriteString(" = ").Arg(kArg).
				WriteString(" AND ").WriteString(created).WriteString(" < ").Arg(createdArg).
				WriteString(") OR (").WriteString(key).WriteString(" = ").Arg(kArg).
				WriteString(" AND ").WriteString(created).WriteString(" = ").Arg(createdArg).
				WriteString(" AND ").WriteString(id).WriteString(" < ").Arg(cur.ID).WriteString("))")
		}))
	}
}
