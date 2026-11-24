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
	"context"

	entsql "entgo.io/ent/dialect/sql"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/decision"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/decisionanswer"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/predicate"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// DefaultDecisionIndexPageSize is the bounded page size used by background
// decision indexing sweeps (design-scale §3.6).
const DefaultDecisionIndexPageSize = 500

// decisionSettledFalse emits the literal predicate "<table.settled> = false"
// so both SQLite and Postgres match the partial index WHERE settled = false.
// Ent's generated decision.Settled(false) emits "NOT decisions.settled", which
// SQLite's syntactic partial-index matcher does not recognize.
func decisionSettledFalse() predicate.Decision {
	return func(s *entsql.Selector) {
		col := s.C(decision.FieldSettled)
		s.Where(entsql.P(func(b *entsql.Builder) {
			b.Ident(col)
			b.WriteString(" = false")
		}))
	}
}

// hasDirectDecisionAnswer matches decisions that have at least one direct
// answer row (including confirm, override, and dismiss outcomes).
func hasDirectDecisionAnswer() predicate.Decision {
	return func(s *entsql.Selector) {
		t := entsql.Table(decisionanswer.Table)
		s.Where(entsql.Exists(
			entsql.Select(t.C(decisionanswer.FieldID)).
				From(t).
				Where(entsql.And(
					entsql.ColumnsEQ(t.C(decisionanswer.FieldDecisionRowID), s.C(decision.FieldID)),
					entsql.EQ(t.C(decisionanswer.FieldProvenance), decisionanswer.ProvenanceDirect),
				)),
		))
	}
}

// settledDecisionPredicate is the exact logical complement of the open-prompt
// predicate: a decision is settled iff its status is terminal (replaced,
// withdrawn, dismissed) or it is decided with at least one direct answer.
func settledDecisionPredicate() predicate.Decision {
	return decision.Or(
		decision.StatusIn(decision.StatusReplaced, decision.StatusWithdrawn, decision.StatusDismissed),
		decision.And(
			decision.StatusEQ(decision.StatusDecided),
			hasDirectDecisionAnswer(),
		),
	)
}

// HasPendingDecisionIndexWork implements store.DecisionStore.
// It probes the settled=false partial index for any settled-eligible row not
// yet marked settled=true. P4 extends this probe with facet_indexed=false
// checks.
func (c *CompositeStore) HasPendingDecisionIndexWork(ctx context.Context) (bool, error) {
	exists, err := c.client.Decision.Query().
		Where(
			decisionSettledFalse(),
			settledDecisionPredicate(),
		).
		Limit(1).
		Exist(ctx)
	if err != nil {
		return false, mapError(err)
	}
	return exists, nil
}

// SweepSettledDecisionsPage implements store.DecisionStore.
// It marks up to limit settled-eligible decisions whose settled hint is still
// false as settled=true inside one transaction. The update is monotone
// (false -> true only) and re-evaluates the exact settled predicate, so it is
// safe against concurrent writers and multi-replica indexers.
func (c *CompositeStore) SweepSettledDecisionsPage(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		limit = DefaultDecisionIndexPageSize
	}
	if c.inTx {
		return c.sweepSettledDecisionsPageTx(ctx, limit)
	}
	var updated int
	err := c.WithTx(ctx, func(tx store.Store) error {
		var err error
		updated, err = tx.(*CompositeStore).sweepSettledDecisionsPageTx(ctx, limit)
		return err
	})
	return updated, err
}

func (c *CompositeStore) sweepSettledDecisionsPageTx(ctx context.Context, limit int) (int, error) {
	ids, err := c.client.Decision.Query().
		Where(
			decisionSettledFalse(),
			settledDecisionPredicate(),
		).
		Limit(limit).
		IDs(ctx)
	if err != nil {
		return 0, mapError(err)
	}
	if len(ids) == 0 {
		return 0, nil
	}
	n, err := c.client.Decision.Update().
		Where(
			decision.IDIn(ids...),
			decisionSettledFalse(),
			settledDecisionPredicate(),
		).
		SetSettled(true).
		Save(ctx)
	if err != nil {
		return 0, mapError(err)
	}
	return n, nil
}
