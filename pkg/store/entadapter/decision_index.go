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
	"fmt"
	"sort"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/decision"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/decisionanswer"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/decisionfacetentry"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/predicate"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/project"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/user"
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

// decisionFacetIndexedFalse emits "<table.facet_indexed> = false" so both
// SQLite and Postgres match the partial index WHERE facet_indexed = false.
func decisionFacetIndexedFalse() predicate.Decision {
	return func(s *entsql.Selector) {
		col := s.C(decision.FieldFacetIndexed)
		s.Where(entsql.P(func(b *entsql.Builder) {
			b.Ident(col)
			b.WriteString(" = false")
		}))
	}
}

// decisionAnswerFacetIndexedFalse emits "<table.facet_indexed> = false" so
// both SQLite and Postgres match the partial index WHERE facet_indexed = false.
func decisionAnswerFacetIndexedFalse() predicate.DecisionAnswer {
	return func(s *entsql.Selector) {
		col := s.C(decisionanswer.FieldFacetIndexed)
		s.Where(entsql.P(func(b *entsql.Builder) {
			b.Ident(col)
			b.WriteString(" = false")
		}))
	}
}

// directDecisionAnswerSubquery returns a correlated SELECT over
// decision_answers matching direct answer rows (including confirm, override,
// and dismiss outcomes) for the outer decision row.
func directDecisionAnswerSubquery(outer *entsql.Selector) *entsql.Selector {
	t := entsql.Table(decisionanswer.Table)
	return entsql.Select(t.C(decisionanswer.FieldID)).
		From(t).
		Where(entsql.And(
			entsql.ColumnsEQ(t.C(decisionanswer.FieldDecisionRowID), outer.C(decision.FieldID)),
			entsql.EQ(t.C(decisionanswer.FieldProvenance), decisionanswer.ProvenanceDirect),
		))
}

// hasDirectDecisionAnswer matches decisions that have at least one direct
// answer row (including confirm, override, and dismiss outcomes).
func hasDirectDecisionAnswer() predicate.Decision {
	return func(s *entsql.Selector) {
		s.Where(entsql.Exists(directDecisionAnswerSubquery(s)))
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
// It probes the settled=false and facet_indexed=false partial indexes for any
// pending background indexing work.
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
	if exists {
		return true, nil
	}

	exists, err = c.client.Decision.Query().
		Where(decisionFacetIndexedFalse()).
		Limit(1).
		Exist(ctx)
	if err != nil {
		return false, mapError(err)
	}
	if exists {
		return true, nil
	}

	exists, err = c.client.DecisionAnswer.Query().
		Where(decisionAnswerFacetIndexedFalse()).
		Limit(1).
		Exist(ctx)
	if err != nil {
		return false, mapError(err)
	}
	return exists, nil
}

func parseIndexAfterID(afterID string) (uuid.UUID, error) {
	if afterID == "" {
		return uuid.Nil, nil
	}
	parsed, err := uuid.Parse(afterID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("%w: invalid after_id %q", store.ErrInvalidInput, afterID)
	}
	return parsed, nil
}

// SweepSettledDecisionsPage implements store.DecisionStore.
// It marks up to limit settled-eligible decisions with id > afterID (when
// afterID is non-empty) whose settled hint is still false as settled=true
// inside one transaction. Returns the number of rows updated and the highest
// decision ID scanned on this page (empty when no matching rows remain).
func (c *CompositeStore) SweepSettledDecisionsPage(ctx context.Context, afterID string, limit int) (int, string, error) {
	if limit <= 0 {
		limit = DefaultDecisionIndexPageSize
	}
	afterUUID, err := parseIndexAfterID(afterID)
	if err != nil {
		return 0, "", err
	}
	if c.inTx {
		return c.sweepSettledDecisionsPageTx(ctx, afterUUID, limit)
	}
	var (
		updated    int
		nextCursor string
	)
	err = c.WithTx(ctx, func(tx store.Store) error {
		var err error
		updated, nextCursor, err = tx.(*CompositeStore).sweepSettledDecisionsPageTx(ctx, afterUUID, limit)
		return err
	})
	return updated, nextCursor, err
}

// decisionIndexPageQuery builds the shared id-ordered, keyset-paginated
// decision page query for background indexer steps. Keyset pagination avoids
// quadratic skip-scans of SQLite's PK autoindex when ANALYZE has not been run.
// On Postgres it locks the selected rows with FOR UPDATE SKIP LOCKED so
// concurrent replicas' sweeps and live writers never deadlock (SQLSTATE 40P01)
// or stall on overlapping pages (design-scale §3.6).
func (c *CompositeStore) decisionIndexPageQuery(
	afterID uuid.UUID,
	limit int,
	preds ...predicate.Decision,
) *ent.DecisionQuery {
	q := c.client.Decision.Query().Where(preds...)
	if afterID != uuid.Nil {
		q = q.Where(decision.IDGT(afterID))
	}
	q = q.Order(ent.Asc(decision.FieldID))
	if limit > 0 {
		q = q.Limit(limit)
	}
	if c.isPostgres() {
		q = q.ForUpdate(entsql.WithLockAction(entsql.SkipLocked))
	}
	return q
}

// selectDecisionIDsForIndexPage selects up to limit decision IDs matching
// preds with id > afterID (when afterID != uuid.Nil) in deterministic
// ORDER BY id order, locking rows on Postgres via decisionIndexPageQuery.
func (c *CompositeStore) selectDecisionIDsForIndexPage(
	ctx context.Context,
	afterID uuid.UUID,
	limit int,
	preds ...predicate.Decision,
) ([]uuid.UUID, error) {
	ids, err := c.decisionIndexPageQuery(afterID, limit, preds...).IDs(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	return ids, nil
}

// selectDecisionsForIndexPage selects up to limit decision rows matching preds
// with id > afterID (when afterID != uuid.Nil) in deterministic ORDER BY id
// order, locking rows on Postgres via decisionIndexPageQuery.
func (c *CompositeStore) selectDecisionsForIndexPage(
	ctx context.Context,
	afterID uuid.UUID,
	limit int,
	preds ...predicate.Decision,
) ([]*ent.Decision, error) {
	rows, err := c.decisionIndexPageQuery(afterID, limit, preds...).All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	return rows, nil
}

func (c *CompositeStore) sweepSettledDecisionsPageTx(ctx context.Context, afterID uuid.UUID, limit int) (int, string, error) {
	ids, err := c.selectDecisionIDsForIndexPage(ctx, afterID, limit, decisionSettledFalse(), settledDecisionPredicate())
	if err != nil {
		return 0, "", err
	}
	if len(ids) == 0 {
		return 0, "", nil
	}
	nextCursor := ids[len(ids)-1].String()
	n, err := c.client.Decision.Update().
		Where(
			decision.IDIn(ids...),
			decisionSettledFalse(),
			settledDecisionPredicate(),
		).
		SetSettled(true).
		Save(ctx)
	if err != nil {
		return 0, "", mapError(err)
	}
	return n, nextCursor, nil
}

// IndexDecisionFacetsDecisionsPage implements store.DecisionStore.
// It locks up to limit unindexed decisions with id > afterID in id order via
// selectDecisionsForIndexPage (FOR UPDATE SKIP LOCKED on Postgres), CAS-flips
// facet_indexed=false -> true, and applies their asker contributions to
// decision_facet_entries in one transaction.
func (c *CompositeStore) IndexDecisionFacetsDecisionsPage(ctx context.Context, afterID string, limit int) (int, string, error) {
	if limit <= 0 {
		limit = DefaultDecisionIndexPageSize
	}
	afterUUID, err := parseIndexAfterID(afterID)
	if err != nil {
		return 0, "", err
	}
	if c.inTx {
		return c.indexDecisionFacetsDecisionsPageTx(ctx, afterUUID, limit)
	}
	var (
		indexed    int
		nextCursor string
	)
	err = c.WithTx(ctx, func(tx store.Store) error {
		var err error
		indexed, nextCursor, err = tx.(*CompositeStore).indexDecisionFacetsDecisionsPageTx(ctx, afterUUID, limit)
		return err
	})
	return indexed, nextCursor, err
}

func (c *CompositeStore) indexDecisionFacetsDecisionsPageTx(ctx context.Context, afterID uuid.UUID, limit int) (int, string, error) {
	rows, err := c.selectDecisionsForIndexPage(ctx, afterID, limit, decisionFacetIndexedFalse())
	if err != nil {
		return 0, "", err
	}
	if len(rows) == 0 {
		return 0, "", nil
	}
	nextCursor := rows[len(rows)-1].ID.String()

	ids := make([]uuid.UUID, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	n, err := c.client.Decision.Update().
		Where(
			decision.IDIn(ids...),
			decisionFacetIndexedFalse(),
		).
		SetFacetIndexed(true).
		Save(ctx)
	if err != nil {
		return 0, "", mapError(err)
	}
	if n != len(rows) {
		return 0, "", fmt.Errorf("decision facet indexer CAS mismatch: flipped %d of %d rows", n, len(rows))
	}

	deltas := make([]facetEntryDelta, 0, len(rows))
	for _, r := range rows {
		if r.ChatKey == nil || *r.ChatKey == "" {
			continue
		}
		deltas = append(deltas, facetEntryDelta{
			ProjectID: r.ProjectID,
			ChatKey:   *r.ChatKey,
			Kind:      decisionfacetentry.KindAsker,
			ActorID:   r.AskerAgentID.String(),
			Name:      r.AskerAgentName,
			LatestAt:  r.CreatedAt,
			Delta:     1,
		})
	}
	if err := applyDecisionFacetDeltas(ctx, c.client, deltas); err != nil {
		return 0, "", err
	}
	return len(rows), nextCursor, nil
}

// IndexDecisionFacetsAnswersPage implements store.DecisionStore.
// It locks the decisions owning up to limit unindexed decision_answers with
// id > afterID in id order (FOR UPDATE SKIP LOCKED on Postgres), processes
// direct answers before agent_interpreted siblings on each locked decision so
// all four mixed-order supersede cases are exact, CAS-flips
// facet_indexed=false -> true, and applies answerer deltas to
// decision_facet_entries in one transaction.
func (c *CompositeStore) IndexDecisionFacetsAnswersPage(ctx context.Context, afterID string, limit int) (int, string, error) {
	if limit <= 0 {
		limit = DefaultDecisionIndexPageSize
	}
	afterUUID, err := parseIndexAfterID(afterID)
	if err != nil {
		return 0, "", err
	}
	if c.inTx {
		return c.indexDecisionFacetsAnswersPageTx(ctx, afterUUID, limit)
	}
	var (
		indexed    int
		nextCursor string
	)
	err = c.WithTx(ctx, func(tx store.Store) error {
		var err error
		indexed, nextCursor, err = tx.(*CompositeStore).indexDecisionFacetsAnswersPageTx(ctx, afterUUID, limit)
		return err
	})
	return indexed, nextCursor, err
}

func (c *CompositeStore) indexDecisionFacetsAnswersPageTx(ctx context.Context, afterID uuid.UUID, limit int) (int, string, error) {
	ansQ := c.client.DecisionAnswer.Query().
		Where(decisionAnswerFacetIndexedFalse())
	if afterID != uuid.Nil {
		ansQ = ansQ.Where(decisionanswer.IDGT(afterID))
	}
	pageAnswers, err := ansQ.
		Order(ent.Asc(decisionanswer.FieldID)).
		Limit(limit).
		All(ctx)
	if err != nil {
		return 0, "", mapError(err)
	}
	if len(pageAnswers) == 0 {
		return 0, "", nil
	}
	nextCursor := pageAnswers[len(pageAnswers)-1].ID.String()

	// Collect unique decision_row_ids in deterministic ascending order.
	seenDec := make(map[uuid.UUID]bool, len(pageAnswers))
	decIDs := make([]uuid.UUID, 0, len(pageAnswers))
	for _, a := range pageAnswers {
		if !seenDec[a.DecisionRowID] {
			seenDec[a.DecisionRowID] = true
			decIDs = append(decIDs, a.DecisionRowID)
		}
	}
	sort.Slice(decIDs, func(i, j int) bool {
		return decIDs[i].String() < decIDs[j].String()
	})

	// Lock the page's decision rows in id order via the shared P1 locking helper
	// (FOR UPDATE SKIP LOCKED on Postgres) so concurrent live answer writers and
	// other indexer replicas serialize on the owning decision row.
	lockedDecs, err := c.selectDecisionsForIndexPage(ctx, uuid.Nil, 0, decision.IDIn(decIDs...))
	if err != nil {
		return 0, "", err
	}
	lockedDecByID := make(map[uuid.UUID]*ent.Decision, len(lockedDecs))
	lockedDecIDs := make([]uuid.UUID, len(lockedDecs))
	for i, d := range lockedDecs {
		lockedDecByID[d.ID] = d
		lockedDecIDs[i] = d.ID
	}

	// Distinguish genuinely missing decision rows (orphaned answers, which must
	// be marked facet_indexed=true with 0 contribution so they never pin a
	// project in fallback) from existing decision rows skipped by SKIP LOCKED.
	orphanDecIDs := make(map[uuid.UUID]bool)
	if len(lockedDecByID) < len(decIDs) {
		var missing []uuid.UUID
		for _, id := range decIDs {
			if _, ok := lockedDecByID[id]; !ok {
				missing = append(missing, id)
			}
		}
		if c.isPostgres() {
			existingIDs, err := c.client.Decision.Query().
				Where(decision.IDIn(missing...)).
				IDs(ctx)
			if err != nil {
				return 0, "", mapError(err)
			}
			existsSet := make(map[uuid.UUID]bool, len(existingIDs))
			for _, id := range existingIDs {
				existsSet[id] = true
			}
			for _, id := range missing {
				if !existsSet[id] {
					orphanDecIDs[id] = true
				}
			}
		} else {
			for _, id := range missing {
				orphanDecIDs[id] = true
			}
		}
	}

	var toFlip []uuid.UUID
	var deltas []facetEntryDelta

	// Orphaned answers (missing decision row): flip facet_indexed=true with 0 contribution.
	for _, a := range pageAnswers {
		if orphanDecIDs[a.DecisionRowID] {
			toFlip = append(toFlip, a.ID)
		}
	}

	// For every locked decision, load ALL of its answers (both direct and
	// agent_interpreted, indexed and unindexed) under the decision row lock and
	// process direct before agent_interpreted so all four mixed-order supersede
	// cases produce the exact summary delta.
	if len(lockedDecIDs) > 0 {
		allAnswers, err := c.client.DecisionAnswer.Query().
			Where(decisionanswer.DecisionRowIDIn(lockedDecIDs...)).
			All(ctx)
		if err != nil {
			return 0, "", mapError(err)
		}
		answersByDec := make(map[uuid.UUID][]*ent.DecisionAnswer, len(lockedDecIDs))
		for _, a := range allAnswers {
			answersByDec[a.DecisionRowID] = append(answersByDec[a.DecisionRowID], a)
		}

		for _, decID := range lockedDecIDs {
			dec := lockedDecByID[decID]
			var directAns, interpAns *ent.DecisionAnswer
			for _, a := range answersByDec[decID] {
				switch a.Provenance {
				case decisionanswer.ProvenanceDirect:
					directAns = a
				case decisionanswer.ProvenanceAgentInterpreted:
					interpAns = a
				}
			}
			hasChatKey := dec.ChatKey != nil && *dec.ChatKey != ""

			// 1. Direct answer first (if unindexed).
			if directAns != nil && !directAns.FacetIndexed {
				toFlip = append(toFlip, directAns.ID)
				if hasChatKey {
					if directAns.AnsweredByUserID != nil && *directAns.AnsweredByUserID != "" {
						deltas = append(deltas, facetEntryDelta{
							ProjectID: dec.ProjectID,
							ChatKey:   *dec.ChatKey,
							Kind:      decisionfacetentry.KindAnswerer,
							ActorID:   *directAns.AnsweredByUserID,
							Name:      directAns.AnsweredByName,
							Email:     directAns.AnsweredByEmail,
							LatestAt:  directAns.CreatedAt,
							Delta:     1,
						})
					}
					// If an interpreted sibling was already facet_indexed before
					// this direct answer existed, undo its +1 contribution.
					if interpAns != nil && interpAns.FacetIndexed &&
						interpAns.AnsweredByUserID != nil && *interpAns.AnsweredByUserID != "" {
						deltas = append(deltas, facetEntryDelta{
							ProjectID: dec.ProjectID,
							ChatKey:   *dec.ChatKey,
							Kind:      decisionfacetentry.KindAnswerer,
							ActorID:   *interpAns.AnsweredByUserID,
							Delta:     -1,
						})
					}
				}
			}

			// 2. Interpreted answer second (if unindexed).
			if interpAns != nil && !interpAns.FacetIndexed {
				toFlip = append(toFlip, interpAns.ID)
				// Contributes +1 iff no direct sibling exists (neither previously
				// indexed nor indexed in step 1 above).
				if directAns == nil && hasChatKey &&
					interpAns.AnsweredByUserID != nil && *interpAns.AnsweredByUserID != "" {
					deltas = append(deltas, facetEntryDelta{
						ProjectID: dec.ProjectID,
						ChatKey:   *dec.ChatKey,
						Kind:      decisionfacetentry.KindAnswerer,
						ActorID:   *interpAns.AnsweredByUserID,
						Name:      interpAns.AnsweredByName,
						Email:     interpAns.AnsweredByEmail,
						LatestAt:  interpAns.CreatedAt,
						Delta:     1,
					})
				}
			}
		}
	}

	if len(toFlip) == 0 {
		return 0, nextCursor, nil
	}

	n, err := c.client.DecisionAnswer.Update().
		Where(
			decisionanswer.IDIn(toFlip...),
			decisionAnswerFacetIndexedFalse(),
		).
		SetFacetIndexed(true).
		Save(ctx)
	if err != nil {
		return 0, "", mapError(err)
	}
	if n != len(toFlip) {
		return 0, "", fmt.Errorf("decision answer facet indexer CAS mismatch: flipped %d of %d rows", n, len(toFlip))
	}

	if err := applyDecisionFacetDeltas(ctx, c.client, deltas); err != nil {
		return 0, "", err
	}
	return len(toFlip), nextCursor, nil
}

const reconcileChunkSize = 500

// ReconcileDecisionFacets implements store.DecisionStore.
// It removes stale decision_facet_entries left behind when an older binary
// deleted a user or project without updating the summary table (design-scale
// §3.2, §3.6). Cost is O(distinct actors + distinct projects).
func (c *CompositeStore) ReconcileDecisionFacets(ctx context.Context) (int, error) {
	if c.inTx {
		return c.reconcileDecisionFacetsTx(ctx)
	}
	var deleted int
	err := c.WithTx(ctx, func(tx store.Store) error {
		var err error
		deleted, err = tx.(*CompositeStore).reconcileDecisionFacetsTx(ctx)
		return err
	})
	return deleted, err
}

func (c *CompositeStore) reconcileDecisionFacetsTx(ctx context.Context) (int, error) {
	totalDeleted := 0

	// 1. Stale answerer rows whose user was deleted (and whose answers no longer
	// reference that actor_id).
	actorIDs, err := c.client.DecisionFacetEntry.Query().
		Where(decisionfacetentry.KindEQ(decisionfacetentry.KindAnswerer)).
		Unique(true).
		Select(decisionfacetentry.FieldActorID).
		Strings(ctx)
	if err != nil {
		return 0, mapError(err)
	}
	if len(actorIDs) > 0 {
		existingUsers := make(map[string]bool, len(actorIDs))
		var validUUIDs []uuid.UUID
		for _, aid := range actorIDs {
			if u, perr := uuid.Parse(aid); perr == nil {
				validUUIDs = append(validUUIDs, u)
			}
		}
		for start := 0; start < len(validUUIDs); start += reconcileChunkSize {
			end := start + reconcileChunkSize
			if end > len(validUUIDs) {
				end = len(validUUIDs)
			}
			found, err := c.client.User.Query().
				Where(user.IDIn(validUUIDs[start:end]...)).
				IDs(ctx)
			if err != nil {
				return 0, mapError(err)
			}
			for _, u := range found {
				existingUsers[u.String()] = true
			}
		}

		var staleActors []string
		for _, aid := range actorIDs {
			if existingUsers[aid] {
				continue
			}
			// Guard against synthetic user IDs in store tests that still have
			// live decision_answers rows: an older-binary DeleteUser always
			// clears answered_by_user_id on decision_answers.
			hasAns, err := c.client.DecisionAnswer.Query().
				Where(decisionanswer.AnsweredByUserIDEQ(aid)).
				Limit(1).
				Exist(ctx)
			if err != nil {
				return 0, mapError(err)
			}
			if !hasAns {
				staleActors = append(staleActors, aid)
			}
		}
		for start := 0; start < len(staleActors); start += reconcileChunkSize {
			end := start + reconcileChunkSize
			if end > len(staleActors) {
				end = len(staleActors)
			}
			n, err := c.client.DecisionFacetEntry.Delete().
				Where(
					decisionfacetentry.KindEQ(decisionfacetentry.KindAnswerer),
					decisionfacetentry.ActorIDIn(staleActors[start:end]...),
				).
				Exec(ctx)
			if err != nil {
				return 0, mapError(err)
			}
			totalDeleted += n
		}
	}

	// 2. Stale project rows whose project was deleted (and whose decisions no
	// longer exist).
	var projRows []struct {
		ProjectID uuid.UUID `json:"project_id"`
	}
	err = c.client.DecisionFacetEntry.Query().
		Unique(true).
		Select(decisionfacetentry.FieldProjectID).
		Scan(ctx, &projRows)
	if err != nil {
		return 0, mapError(err)
	}
	if len(projRows) > 0 {
		projIDs := make([]uuid.UUID, len(projRows))
		for i, r := range projRows {
			projIDs[i] = r.ProjectID
		}
		existingProjs := make(map[uuid.UUID]bool, len(projIDs))
		for start := 0; start < len(projIDs); start += reconcileChunkSize {
			end := start + reconcileChunkSize
			if end > len(projIDs) {
				end = len(projIDs)
			}
			found, err := c.client.Project.Query().
				Where(project.IDIn(projIDs[start:end]...)).
				IDs(ctx)
			if err != nil {
				return 0, mapError(err)
			}
			for _, id := range found {
				existingProjs[id] = true
			}
		}

		var staleProjs []uuid.UUID
		for _, pid := range projIDs {
			if existingProjs[pid] {
				continue
			}
			hasDec, err := c.client.Decision.Query().
				Where(decision.ProjectIDEQ(pid)).
				Limit(1).
				Exist(ctx)
			if err != nil {
				return 0, mapError(err)
			}
			if !hasDec {
				staleProjs = append(staleProjs, pid)
			}
		}
		for start := 0; start < len(staleProjs); start += reconcileChunkSize {
			end := start + reconcileChunkSize
			if end > len(staleProjs) {
				end = len(staleProjs)
			}
			n, err := c.client.DecisionFacetEntry.Delete().
				Where(decisionfacetentry.ProjectIDIn(staleProjs[start:end]...)).
				Exec(ctx)
			if err != nil {
				return 0, mapError(err)
			}
			totalDeleted += n
		}
	}

	return totalDeleted, nil
}
