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
	"strings"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/decision"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/decisionanswer"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/decisionfacetentry"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/message"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/predicate"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

const (
	maxProjectDecisionFacetItems   = 500
	projectDecisionAgentsChunkSize = 500
	facetProbeChunkSize            = 500
)

type groupedProjectDecisionAgent struct {
	ProjectID    uuid.UUID `json:"project_id"`
	AskerAgentID uuid.UUID `json:"asker_agent_id"`
	Name         string    `json:"name"`
}

type groupedDecisionAgentName struct {
	AskerAgentID uuid.UUID `json:"asker_agent_id"`
	ProjectID    uuid.UUID `json:"project_id"`
	Name         string    `json:"asker_agent_name"`
	Latest       string    `json:"latest"`
}

type groupedDecisionAnswerer struct {
	AnsweredByUserID string `json:"answered_by_user_id"`
	Name             string `json:"name"`
	Email            string `json:"email"`
}

type groupedDecisionProject struct {
	ProjectID uuid.UUID `json:"project_id"`
	Latest    string    `json:"latest"`
}

// hasUnindexedDecisionFacets probes the facet_indexed=false partial indexes on
// decisions and decision_answers for any row in projectUUIDs not yet reflected
// in decision_facet_entries (design-scale §3.2 "Fallback"). Returns true iff
// at least one unindexed row exists in scope, signaling callers to use the
// legacy query.
func (c *CompositeStore) hasUnindexedDecisionFacets(ctx context.Context, projectUUIDs []uuid.UUID) (bool, error) {
	if len(projectUUIDs) == 0 {
		return false, nil
	}
	drv := c.client.Driver()
	d := entsql.Dialect(drv.Dialect())
	for start := 0; start < len(projectUUIDs); start += facetProbeChunkSize {
		end := start + facetProbeChunkSize
		if end > len(projectUUIDs) {
			end = len(projectUUIDs)
		}
		chunk := projectUUIDs[start:end]
		vals := make([]any, len(chunk))
		for i, u := range chunk {
			vals[i] = u
		}

		decSub := d.SelectExpr(entsql.Raw("1")).
			From(entsql.Table(decision.Table)).
			Where(entsql.And(
				entsql.P(func(b *entsql.Builder) {
					b.Ident(decision.FieldFacetIndexed)
					b.WriteString(" = false")
				}),
				entsql.In(decision.FieldProjectID, vals...),
			)).
			Limit(1)
		ansSub := d.SelectExpr(entsql.Raw("1")).
			From(entsql.Table(decisionanswer.Table)).
			Where(entsql.And(
				entsql.P(func(b *entsql.Builder) {
					b.Ident(decisionanswer.FieldFacetIndexed)
					b.WriteString(" = false")
				}),
				entsql.In(decisionanswer.FieldProjectID, vals...),
			)).
			Limit(1)

		q := d.SelectExpr(entsql.Raw("1")).From(decSub.As("d_unidx")).
			UnionAll(d.SelectExpr(entsql.Raw("1")).From(ansSub.As("a_unidx")))
		query, args := q.Query()
		rows := &entsql.Rows{}
		if err := drv.Query(ctx, query, args, rows); err != nil {
			return false, mapError(err)
		}
		found := rows.Next()
		rowErr := rows.Err()
		closeErr := rows.Close()
		if rowErr != nil {
			return false, mapError(rowErr)
		}
		if closeErr != nil {
			return false, mapError(closeErr)
		}
		if found {
			return true, nil
		}
	}
	return false, nil
}

// ListProjectDecisionAgents implements store.DecisionStore.
func (c *CompositeStore) ListProjectDecisionAgents(ctx context.Context, projectID string) ([]store.ProjectDecisionActor, error) {
	byProject, err := c.ListProjectDecisionAgentsForProjects(ctx, []string{projectID})
	if err != nil {
		return nil, err
	}
	return byProject[projectID], nil
}

// ListProjectDecisionAgentsForProjects implements store.DecisionStore.
// Reads decision_facet_entries when all rows in projectIDs are facet-indexed,
// falling back to listProjectDecisionAgentsForProjectsLegacy otherwise
// (design-scale §3.2).
func (c *CompositeStore) ListProjectDecisionAgentsForProjects(ctx context.Context, projectIDs []string) (map[string][]store.ProjectDecisionActor, error) {
	if len(projectIDs) == 0 {
		return nil, nil
	}
	projectUUIDs, err := parseProjectIDs(projectIDs)
	if err != nil {
		return nil, err
	}
	if len(projectUUIDs) == 0 {
		return nil, nil
	}

	unindexed, err := c.hasUnindexedDecisionFacets(ctx, projectUUIDs)
	if err != nil {
		return nil, err
	}
	if unindexed {
		return c.listProjectDecisionAgentsForProjectsLegacy(ctx, projectUUIDs)
	}

	type projActorKey struct {
		projectID string
		actorID   string
	}
	type newest struct {
		name   string
		latest time.Time
	}
	byProjActor := make(map[projActorKey]newest)
	for start := 0; start < len(projectUUIDs); start += projectDecisionAgentsChunkSize {
		end := start + projectDecisionAgentsChunkSize
		if end > len(projectUUIDs) {
			end = len(projectUUIDs)
		}
		rows, err := c.client.DecisionFacetEntry.Query().
			Where(
				decisionfacetentry.KindEQ(decisionfacetentry.KindAsker),
				decisionfacetentry.ProjectIDIn(projectUUIDs[start:end]...),
			).
			Select(
				decisionfacetentry.FieldProjectID,
				decisionfacetentry.FieldActorID,
				decisionfacetentry.FieldName,
				decisionfacetentry.FieldLatestAt,
			).
			All(ctx)
		if err != nil {
			return nil, mapError(err)
		}
		for _, r := range rows {
			k := projActorKey{projectID: r.ProjectID.String(), actorID: r.ActorID}
			cur, ok := byProjActor[k]
			if !ok || r.LatestAt.After(cur.latest) || (r.LatestAt.Equal(cur.latest) && r.Name > cur.name) {
				byProjActor[k] = newest{name: r.Name, latest: r.LatestAt}
			}
		}
	}

	out := make(map[string][]store.ProjectDecisionActor, len(projectUUIDs))
	for k, n := range byProjActor {
		out[k.projectID] = append(out[k.projectID], store.ProjectDecisionActor{
			ID:   k.actorID,
			Name: n.name,
		})
	}
	for pid := range out {
		sortProjectDecisionActors(out[pid])
	}
	return out, nil
}

func (c *CompositeStore) listProjectDecisionAgentsForProjectsLegacy(ctx context.Context, projectUUIDs []uuid.UUID) (map[string][]store.ProjectDecisionActor, error) {
	out := make(map[string][]store.ProjectDecisionActor, len(projectUUIDs))
	for start := 0; start < len(projectUUIDs); start += projectDecisionAgentsChunkSize {
		end := start + projectDecisionAgentsChunkSize
		if end > len(projectUUIDs) {
			end = len(projectUUIDs)
		}
		var rows []groupedProjectDecisionAgent
		err := c.client.Decision.Query().
			Where(decision.ProjectIDIn(projectUUIDs[start:end]...)).
			Order(ent.Asc(decision.FieldProjectID), ent.Asc(decision.FieldAskerAgentID)).
			GroupBy(decision.FieldProjectID, decision.FieldAskerAgentID).
			Aggregate(ent.As(ent.Max(decision.FieldAskerAgentName), "name")).
			Scan(ctx, &rows)
		if err != nil {
			return nil, mapError(err)
		}
		for _, r := range rows {
			pid := r.ProjectID.String()
			out[pid] = append(out[pid], store.ProjectDecisionActor{
				ID:   r.AskerAgentID.String(),
				Name: r.Name,
			})
		}
	}
	for pid := range out {
		sortProjectDecisionActors(out[pid])
	}
	return out, nil
}

// ListOpenPrompts implements store.DecisionStore.
func (c *CompositeStore) ListOpenPrompts(ctx context.Context, projectIDs, chatKeys []string) ([]*store.ProjectDecisionPrompt, error) {
	projectUUIDs, err := parseProjectIDs(projectIDs)
	if err != nil {
		return nil, err
	}
	keys := cleanChatKeys(chatKeys)
	if len(keys) == 0 || len(projectUUIDs) == 0 {
		return nil, nil
	}

	// Find every non-settled decision row in (projectIDs, chatKeys):
	// settled = false AND (status == pending, OR (status == decided AND NOT
	// EXISTS direct answer)). The settled = false hint narrows the lookup to
	// the partial index (project_id, chat_key) WHERE settled = false while the
	// exact open predicate preserves correctness for rows written by an older
	// binary before the background indexer sweeps them.
	openRows, err := c.client.Decision.Query().
		Where(
			decisionSettledFalse(),
			decision.ProjectIDIn(projectUUIDs...),
			decisionChatKeyIn(len(projectUUIDs), keys),
			decision.Or(
				decision.StatusEQ(decision.StatusPending),
				decision.And(
					decision.StatusEQ(decision.StatusDecided),
					noDirectDecisionAnswer(),
				),
			),
		).
		Select(decision.FieldMessageID).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	if len(openRows) == 0 {
		return nil, nil
	}

	seenMsg := make(map[uuid.UUID]bool, len(openRows))
	msgUUIDs := make([]uuid.UUID, 0, len(openRows))
	msgIDStrings := make([]string, 0, len(openRows))
	for _, r := range openRows {
		if !seenMsg[r.MessageID] {
			seenMsg[r.MessageID] = true
			msgUUIDs = append(msgUUIDs, r.MessageID)
			msgIDStrings = append(msgIDStrings, r.MessageID.String())
		}
	}

	byMsg, err := c.GetDecisionsByMessageIDs(ctx, msgIDStrings)
	if err != nil {
		return nil, err
	}

	existingMsgs, err := c.existingMessageIDSet(ctx, msgUUIDs)
	if err != nil {
		return nil, err
	}

	prompts := make([]*store.ProjectDecisionPrompt, 0, len(msgIDStrings))
	for _, msgID := range msgIDStrings {
		ds := byMsg[msgID]
		if len(ds) == 0 {
			continue
		}
		first := ds[0]
		prompts = append(prompts, &store.ProjectDecisionPrompt{
			ProjectID:              first.ProjectID,
			MessageID:              msgID,
			ChatKey:                first.ChatKey,
			AskerAgentID:           first.AskerAgentID,
			AskerAgentName:         first.AskerAgentName,
			CreatedAt:              first.CreatedAt,
			PromptExistsInMessages: existingMsgs[msgID],
			Decisions:              ds,
		})
	}

	sort.Slice(prompts, func(i, j int) bool {
		if !prompts[i].CreatedAt.Equal(prompts[j].CreatedAt) {
			return prompts[i].CreatedAt.After(prompts[j].CreatedAt)
		}
		return prompts[i].MessageID > prompts[j].MessageID
	})
	return prompts, nil
}

// ListDecisionAnswers implements store.DecisionStore.
func (c *CompositeStore) ListDecisionAnswers(
	ctx context.Context,
	projectIDs []string,
	chatKeys []string,
	filter store.ProjectDecisionAnswerFilter,
	cursor string,
	limit int,
) ([]*store.ProjectDecisionAnswerEntry, string, error) {
	projectUUIDs, err := parseProjectIDs(projectIDs)
	if err != nil {
		return nil, "", err
	}
	if limit < 0 || limit > store.MaxDecisionListLimit {
		return nil, "", fmt.Errorf("%w: limit must be between 0 and %d", store.ErrInvalidInput, store.MaxDecisionListLimit)
	}
	agentUUID, provPred, err := validateDecisionAnswerFilter(filter)
	if err != nil {
		return nil, "", err
	}
	var cur decisionListCursor
	if cursor != "" {
		cur, err = decodeDecisionListCursor(cursor)
		if err != nil {
			return nil, "", err
		}
	}

	keys := cleanChatKeys(chatKeys)
	if limit == 0 || len(keys) == 0 || len(projectUUIDs) == 0 {
		return nil, "", nil
	}

	q := c.client.DecisionAnswer.Query().
		Where(
			decisionanswer.ProjectIDIn(projectUUIDs...),
			decisionAnswerInChatKeys(keys),
			notSupersededInterpretedAnswer(),
		)
	q = applyDecisionAnswerFilter(q, filter, agentUUID, provPred)
	if cursor != "" {
		q = q.Where(decisionanswer.Or(
			decisionanswer.CreatedAtLT(cur.CreatedAt),
			decisionanswer.And(
				decisionanswer.CreatedAtEQ(cur.CreatedAt),
				decisionanswer.IDLT(cur.ID),
			),
		))
	}

	rows, err := q.Order(ent.Desc(decisionanswer.FieldCreatedAt), ent.Desc(decisionanswer.FieldID)).
		Limit(limit + 1).
		All(ctx)
	if err != nil {
		return nil, "", mapError(err)
	}

	var nextCursor string
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[len(rows)-1]
		nextCursor, err = encodeDecisionListCursor(last.CreatedAt, last.ID)
		if err != nil {
			return nil, "", fmt.Errorf("encode cursor: %w", err)
		}
	}
	if len(rows) == 0 {
		return nil, "", nil
	}

	entries, err := c.enrichDecisionAnswerRows(ctx, rows)
	if err != nil {
		return nil, "", err
	}
	return entries, nextCursor, nil
}

// ProjectsWithDecisions implements store.DecisionStore.
// Reads decision_facet_entries when all rows in projectIDs are facet-indexed,
// falling back to projectsWithDecisionsLegacy otherwise (design-scale §3.2).
func (c *CompositeStore) ProjectsWithDecisions(ctx context.Context, projectIDs []string) ([]string, error) {
	if len(projectIDs) == 0 {
		return nil, nil
	}
	projectUUIDs, err := parseProjectIDs(projectIDs)
	if err != nil {
		return nil, err
	}
	if len(projectUUIDs) == 0 {
		return nil, nil
	}

	unindexed, err := c.hasUnindexedDecisionFacets(ctx, projectUUIDs)
	if err != nil {
		return nil, err
	}
	if unindexed {
		return c.projectsWithDecisionsLegacy(ctx, projectUUIDs)
	}

	type projectLatest struct {
		id     string
		latest time.Time
	}
	byProj := make(map[string]time.Time, len(projectUUIDs))
	for start := 0; start < len(projectUUIDs); start += facetProbeChunkSize {
		end := start + facetProbeChunkSize
		if end > len(projectUUIDs) {
			end = len(projectUUIDs)
		}
		var rows []groupedDecisionProject
		err = c.client.DecisionFacetEntry.Query().
			Where(
				decisionfacetentry.KindEQ(decisionfacetentry.KindAsker),
				decisionfacetentry.ProjectIDIn(projectUUIDs[start:end]...),
			).
			GroupBy(decisionfacetentry.FieldProjectID).
			Aggregate(ent.As(ent.Max(decisionfacetentry.FieldLatestAt), "latest")).
			Scan(ctx, &rows)
		if err != nil {
			return nil, mapError(err)
		}
		for _, r := range rows {
			pid := r.ProjectID.String()
			ts := parseAggregateTime(r.Latest)
			if cur, ok := byProj[pid]; !ok || ts.After(cur) {
				byProj[pid] = ts
			}
		}
	}
	if len(byProj) == 0 {
		return nil, nil
	}

	ordered := make([]projectLatest, 0, len(byProj))
	for pid, ts := range byProj {
		ordered = append(ordered, projectLatest{id: pid, latest: ts})
	}
	sort.Slice(ordered, func(i, j int) bool {
		if !ordered[i].latest.Equal(ordered[j].latest) {
			return ordered[i].latest.After(ordered[j].latest)
		}
		return ordered[i].id < ordered[j].id
	})

	out := make([]string, len(ordered))
	for i, p := range ordered {
		out[i] = p.id
	}
	return out, nil
}

func (c *CompositeStore) projectsWithDecisionsLegacy(ctx context.Context, projectUUIDs []uuid.UUID) ([]string, error) {
	var rows []groupedDecisionProject
	err := c.client.Decision.Query().
		Where(decision.ProjectIDIn(projectUUIDs...)).
		GroupBy(decision.FieldProjectID).
		Aggregate(ent.As(ent.Max(decision.FieldCreatedAt), "latest")).
		Scan(ctx, &rows)
	if err != nil {
		return nil, mapError(err)
	}
	if len(rows) == 0 {
		return nil, nil
	}

	type projectLatest struct {
		id     string
		latest time.Time
	}
	ordered := make([]projectLatest, len(rows))
	for i, r := range rows {
		ordered[i] = projectLatest{
			id:     r.ProjectID.String(),
			latest: parseAggregateTime(r.Latest),
		}
	}
	sort.Slice(ordered, func(i, j int) bool {
		if !ordered[i].latest.Equal(ordered[j].latest) {
			return ordered[i].latest.After(ordered[j].latest)
		}
		return ordered[i].id < ordered[j].id
	})

	out := make([]string, len(ordered))
	for i, p := range ordered {
		out[i] = p.id
	}
	return out, nil
}

const unsettledProjectsChunkSize = 500

// ProjectsWithUnsettledDecisions implements store.DecisionStore.
func (c *CompositeStore) ProjectsWithUnsettledDecisions(ctx context.Context, projectIDs []string) (map[string]bool, error) {
	if len(projectIDs) == 0 {
		return nil, nil
	}
	projectUUIDs, err := parseProjectIDs(projectIDs)
	if err != nil {
		return nil, err
	}
	if len(projectUUIDs) == 0 {
		return nil, nil
	}

	out := make(map[string]bool)
	for start := 0; start < len(projectUUIDs); start += unsettledProjectsChunkSize {
		end := start + unsettledProjectsChunkSize
		if end > len(projectUUIDs) {
			end = len(projectUUIDs)
		}
		var rows []struct {
			ProjectID uuid.UUID `json:"project_id"`
		}
		err := c.client.Decision.Query().
			Where(
				decisionSettledFalse(),
				decision.ProjectIDIn(projectUUIDs[start:end]...),
			).
			Unique(true).
			Select(decision.FieldProjectID).
			Scan(ctx, &rows)
		if err != nil {
			return nil, mapError(err)
		}
		for _, r := range rows {
			out[r.ProjectID.String()] = true
		}
	}
	return out, nil
}

// ListDecisionAnswersPerProject implements store.DecisionStore using the
// design R5 shape: one bounded LIMIT perProject query per target project
// scoped to that project's own readable keys (served by
// INDEX(project_id, created_at)), followed by a single batched enrichment
// pass over the union of returned rows.
func (c *CompositeStore) ListDecisionAnswersPerProject(
	ctx context.Context,
	projectIDs []string,
	chatKeysByProject map[string][]string,
	filter store.ProjectDecisionAnswerFilter,
	perProject int,
) (map[string][]*store.ProjectDecisionAnswerEntry, error) {
	projectUUIDs, err := parseProjectIDs(projectIDs)
	if err != nil {
		return nil, err
	}
	if perProject < 0 || perProject > store.MaxDecisionListLimit {
		return nil, fmt.Errorf("%w: perProject must be between 0 and %d", store.ErrInvalidInput, store.MaxDecisionListLimit)
	}
	agentUUID, provPred, err := validateDecisionAnswerFilter(filter)
	if err != nil {
		return nil, err
	}
	if perProject == 0 || len(projectUUIDs) == 0 || len(chatKeysByProject) == 0 {
		return nil, nil
	}

	cleanedQueryKeys := cleanChatKeys(filter.QueryChatKeys)
	allRows := make([]*ent.DecisionAnswer, 0, len(projectUUIDs)*perProject)
	for _, pUUID := range projectUUIDs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		pid := pUUID.String()
		projKeys := cleanChatKeys(chatKeysByProject[pid])
		if len(projKeys) == 0 {
			continue
		}
		projFilter := filter
		if filter.Query != "" && len(cleanedQueryKeys) > 0 {
			projKeySet := make(map[string]bool, len(projKeys))
			for _, k := range projKeys {
				projKeySet[k] = true
			}
			var qKeys []string
			for _, qk := range cleanedQueryKeys {
				if projKeySet[qk] {
					qKeys = append(qKeys, qk)
				}
			}
			projFilter.QueryChatKeys = qKeys
		}

		q := c.client.DecisionAnswer.Query().
			Where(
				decisionanswer.ProjectIDEQ(pUUID),
				decisionAnswerInChatKeys(projKeys),
				notSupersededInterpretedAnswer(),
			)
		q = applyDecisionAnswerFilter(q, projFilter, agentUUID, provPred)
		rows, err := q.Order(ent.Desc(decisionanswer.FieldCreatedAt), ent.Desc(decisionanswer.FieldID)).
			Limit(perProject).
			All(ctx)
		if err != nil {
			return nil, mapError(err)
		}
		allRows = append(allRows, rows...)
	}
	if len(allRows) == 0 {
		return nil, nil
	}

	entries, err := c.enrichDecisionAnswerRows(ctx, allRows)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]*store.ProjectDecisionAnswerEntry, len(projectUUIDs))
	for _, entry := range entries {
		pid := entry.Decision.ProjectID
		out[pid] = append(out[pid], entry)
	}
	return out, nil
}

func validateDecisionAnswerFilter(filter store.ProjectDecisionAnswerFilter) (uuid.UUID, predicate.DecisionAnswer, error) {
	var agentUUID uuid.UUID
	var err error
	if filter.AgentID != "" {
		agentUUID, err = parseUUID(filter.AgentID)
		if err != nil {
			return uuid.Nil, nil, err
		}
	}
	provPred, err := answerProvenancePredicate(filter.Provenance)
	if err != nil {
		return uuid.Nil, nil, err
	}
	if filter.From != nil && filter.To != nil && filter.From.After(*filter.To) {
		return uuid.Nil, nil, fmt.Errorf("%w: from must not be after to", store.ErrInvalidInput)
	}
	return agentUUID, provPred, nil
}

func decisionAnswerFilterPredicates(
	filter store.ProjectDecisionAnswerFilter,
	agentUUID uuid.UUID,
	provPred predicate.DecisionAnswer,
) []predicate.DecisionAnswer {
	var preds []predicate.DecisionAnswer
	if filter.Query != "" {
		preds = append(preds, decisionAnswerMatchesQuery(filter.Query, cleanChatKeys(filter.QueryChatKeys)))
	}
	if filter.AgentID != "" {
		preds = append(preds, decisionanswer.AskerAgentID(agentUUID))
	}
	if filter.UserID != "" {
		preds = append(preds, decisionanswer.AnsweredByUserID(filter.UserID))
	}
	if provPred != nil {
		preds = append(preds, provPred)
	}
	if filter.From != nil {
		preds = append(preds, decisionanswer.CreatedAtGTE(filter.From.UTC()))
	}
	if filter.To != nil {
		preds = append(preds, decisionanswer.CreatedAtLTE(filter.To.UTC()))
	}
	return preds
}

func applyDecisionAnswerFilter(
	q *ent.DecisionAnswerQuery,
	filter store.ProjectDecisionAnswerFilter,
	agentUUID uuid.UUID,
	provPred predicate.DecisionAnswer,
) *ent.DecisionAnswerQuery {
	if preds := decisionAnswerFilterPredicates(filter, agentUUID, provPred); len(preds) > 0 {
		q = q.Where(preds...)
	}
	return q
}

func (c *CompositeStore) enrichDecisionAnswerRows(ctx context.Context, rows []*ent.DecisionAnswer) ([]*store.ProjectDecisionAnswerEntry, error) {
	decisionRowUUIDs := make([]uuid.UUID, 0, len(rows))
	seenDec := make(map[uuid.UUID]bool, len(rows))
	var siblingRowUUIDs []uuid.UUID
	seenSibling := make(map[uuid.UUID]bool)
	var msgUUIDsToCheck []uuid.UUID
	seenMsgCheck := make(map[uuid.UUID]bool)

	for _, r := range rows {
		if !seenDec[r.DecisionRowID] {
			seenDec[r.DecisionRowID] = true
			decisionRowUUIDs = append(decisionRowUUIDs, r.DecisionRowID)
		}
		if r.Provenance == decisionanswer.ProvenanceDirect && r.Outcome != decisionanswer.OutcomeAnswer && !seenSibling[r.DecisionRowID] {
			seenSibling[r.DecisionRowID] = true
			siblingRowUUIDs = append(siblingRowUUIDs, r.DecisionRowID)
		}
		if r.SourceMessageID != nil && !seenMsgCheck[*r.SourceMessageID] {
			seenMsgCheck[*r.SourceMessageID] = true
			msgUUIDsToCheck = append(msgUUIDsToCheck, *r.SourceMessageID)
		}
	}

	decRows, err := c.client.Decision.Query().
		Where(decision.IDIn(decisionRowUUIDs...)).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	decByID := make(map[uuid.UUID]*store.Decision, len(decRows))
	for _, dr := range decRows {
		decByID[dr.ID] = decisionToStore(dr)
		if !seenMsgCheck[dr.MessageID] {
			seenMsgCheck[dr.MessageID] = true
			msgUUIDsToCheck = append(msgUUIDsToCheck, dr.MessageID)
		}
	}

	interpByRow := make(map[uuid.UUID]*store.DecisionAnswer, len(siblingRowUUIDs))
	if len(siblingRowUUIDs) > 0 {
		interpRows, err := c.client.DecisionAnswer.Query().
			Where(
				decisionanswer.DecisionRowIDIn(siblingRowUUIDs...),
				decisionanswer.ProvenanceEQ(decisionanswer.ProvenanceAgentInterpreted),
			).
			All(ctx)
		if err != nil {
			return nil, mapError(err)
		}
		for _, ir := range interpRows {
			interpByRow[ir.DecisionRowID] = answerToStore(ir)
		}
	}

	existingMsgs, err := c.existingMessageIDSet(ctx, msgUUIDsToCheck)
	if err != nil {
		return nil, err
	}

	entries := make([]*store.ProjectDecisionAnswerEntry, 0, len(rows))
	for _, r := range rows {
		ans := answerToStore(r)
		d := decByID[r.DecisionRowID]
		if d == nil {
			continue
		}
		entry := &store.ProjectDecisionAnswerEntry{
			Answer:                 ans,
			Decision:               d,
			PromptExistsInMessages: existingMsgs[d.MessageID],
			SourceExistsInMessages: ans.SourceMessageID != "" && existingMsgs[ans.SourceMessageID],
		}
		if ans.Provenance == store.DecisionProvenanceDirect {
			if interp := interpByRow[r.DecisionRowID]; interp != nil {
				entry.InterpretedAnswer = interp
				if ans.Outcome == store.DecisionOutcomeOverride {
					entry.PreviousChoiceLabel = interp.ChoiceLabel
					entry.PreviousOtherText = interp.OtherText
				}
			}
		} else if ans.Provenance == store.DecisionProvenanceAgentInterpreted && !store.DecisionStatusTerminal(d.Status) {
			entry.AwaitingConfirmation = true
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// ListDecisionFacets implements store.DecisionStore.
// Reads decision_facet_entries when all rows in projectIDs are facet-indexed,
// falling back to listDecisionFacetsLegacy otherwise (design-scale §3.2).
func (c *CompositeStore) ListDecisionFacets(
	ctx context.Context,
	projectIDs []string,
	chatKeys []string,
) ([]store.ProjectDecisionActor, []store.ProjectDecisionActor, []string, error) {
	projectUUIDs, err := parseProjectIDs(projectIDs)
	if err != nil {
		return nil, nil, nil, err
	}
	keys := cleanChatKeys(chatKeys)
	if len(keys) == 0 || len(projectUUIDs) == 0 {
		return nil, nil, nil, nil
	}

	unindexed, err := c.hasUnindexedDecisionFacets(ctx, projectUUIDs)
	if err != nil {
		return nil, nil, nil, err
	}
	if unindexed {
		return c.listDecisionFacetsLegacy(ctx, projectUUIDs, keys)
	}

	rows, err := c.client.DecisionFacetEntry.Query().
		Where(
			decisionfacetentry.ProjectIDIn(projectUUIDs...),
			decisionFacetEntryChatKeyIn(len(projectUUIDs), keys),
			decisionfacetentry.Or(
				decisionfacetentry.KindEQ(decisionfacetentry.KindAsker),
				decisionfacetentry.RefCountGT(0),
			),
		).
		Select(
			decisionfacetentry.FieldKind,
			decisionfacetentry.FieldActorID,
			decisionfacetentry.FieldProjectID,
			decisionfacetentry.FieldChatKey,
			decisionfacetentry.FieldName,
			decisionfacetentry.FieldEmail,
			decisionfacetentry.FieldLatestAt,
		).
		All(ctx)
	if err != nil {
		return nil, nil, nil, mapError(err)
	}

	type newestAgent struct {
		name      string
		projectID string
		latest    time.Time
	}
	type newestAnswerer struct {
		name   string
		email  string
		latest time.Time
	}

	byAgent := make(map[string]newestAgent)
	byAnswerer := make(map[string]newestAnswerer)
	seenConv := make(map[string]bool)
	var convKeys []string

	for _, r := range rows {
		switch r.Kind {
		case decisionfacetentry.KindAsker:
			cur, ok := byAgent[r.ActorID]
			if !ok || r.LatestAt.After(cur.latest) || (r.LatestAt.Equal(cur.latest) && r.Name > cur.name) {
				byAgent[r.ActorID] = newestAgent{
					name:      r.Name,
					projectID: r.ProjectID.String(),
					latest:    r.LatestAt,
				}
			}
			if !seenConv[r.ChatKey] {
				seenConv[r.ChatKey] = true
				convKeys = append(convKeys, r.ChatKey)
			}
		case decisionfacetentry.KindAnswerer:
			cur, ok := byAnswerer[r.ActorID]
			if !ok || facetDeltaNewer(r.LatestAt, r.Name, r.Email, cur.latest, cur.name, cur.email) {
				byAnswerer[r.ActorID] = newestAnswerer{
					name:   r.Name,
					email:  r.Email,
					latest: r.LatestAt,
				}
			}
		}
	}

	agents := make([]store.ProjectDecisionActor, 0, len(byAgent))
	for id, n := range byAgent {
		agents = append(agents, store.ProjectDecisionActor{
			ID:        id,
			Name:      n.name,
			ProjectID: n.projectID,
		})
	}
	sortProjectDecisionActors(agents)

	// Cap answerers at maxProjectDecisionFacetItems ordered by user ID ASC
	// before display-name sorting, matching listDecisionFacetsLegacy.
	answererIDs := make([]string, 0, len(byAnswerer))
	for id := range byAnswerer {
		answererIDs = append(answererIDs, id)
	}
	sort.Strings(answererIDs)
	if len(answererIDs) > maxProjectDecisionFacetItems {
		answererIDs = answererIDs[:maxProjectDecisionFacetItems]
	}
	answerers := make([]store.ProjectDecisionActor, 0, len(answererIDs))
	for _, id := range answererIDs {
		a := byAnswerer[id]
		name := a.name
		if name == "" {
			name = a.email
		}
		answerers = append(answerers, store.ProjectDecisionActor{ID: id, Name: name})
	}
	sortProjectDecisionActors(answerers)

	return agents, answerers, convKeys, nil
}

// decisionFacetEntryChatKeyIn applies the same SQLite +chat_key planner hint
// as decisionChatKeyIn when querying multiple projects so SQLite seeks
// project_id on the index instead of doing a Cartesian project x chat_key loop.
func decisionFacetEntryChatKeyIn(projectCount int, keys []string) predicate.DecisionFacetEntry {
	if projectCount <= 1 {
		return decisionfacetentry.ChatKeyIn(keys...)
	}
	vals := make([]any, len(keys))
	for i, k := range keys {
		vals[i] = k
	}
	return func(s *entsql.Selector) {
		if s.Dialect() != dialect.SQLite {
			decisionfacetentry.ChatKeyIn(keys...)(s)
			return
		}
		col := s.C(decisionfacetentry.FieldChatKey)
		s.Where(entsql.P(func(b *entsql.Builder) {
			b.WriteByte('+')
			b.Ident(col)
			b.WriteString(" IN (")
			b.Args(vals...)
			b.WriteByte(')')
		}))
	}
}

func (c *CompositeStore) listDecisionFacetsLegacy(
	ctx context.Context,
	projectUUIDs []uuid.UUID,
	keys []string,
) ([]store.ProjectDecisionActor, []store.ProjectDecisionActor, []string, error) {
	agents, err := c.decisionAgentFacet(ctx, projectUUIDs, keys)
	if err != nil {
		return nil, nil, nil, err
	}

	var ansRows []groupedDecisionAnswerer
	err = c.client.DecisionAnswer.Query().
		Where(
			decisionanswer.ProjectIDIn(projectUUIDs...),
			decisionanswer.AnsweredByUserIDNotNil(),
			decisionanswer.AnsweredByUserIDNEQ(""),
			decisionAnswerInChatKeys(keys),
			notSupersededInterpretedAnswer(),
		).
		Order(ent.Asc(decisionanswer.FieldAnsweredByUserID)).
		Limit(maxProjectDecisionFacetItems).
		GroupBy(decisionanswer.FieldAnsweredByUserID).
		Aggregate(
			ent.As(ent.Max(decisionanswer.FieldAnsweredByName), "name"),
			ent.As(ent.Max(decisionanswer.FieldAnsweredByEmail), "email"),
		).
		Scan(ctx, &ansRows)
	if err != nil {
		return nil, nil, nil, mapError(err)
	}
	answerers := make([]store.ProjectDecisionActor, 0, len(ansRows))
	for _, r := range ansRows {
		if r.AnsweredByUserID == "" {
			continue
		}
		name := r.Name
		if name == "" {
			name = r.Email
		}
		answerers = append(answerers, store.ProjectDecisionActor{ID: r.AnsweredByUserID, Name: name})
	}
	sortProjectDecisionActors(answerers)

	// Distinct chat keys with at least one decision. The result is a subset
	// of keys, so its size is bounded by the caller's readable set.
	convKeys, err := c.client.Decision.Query().
		Where(decision.ProjectIDIn(projectUUIDs...), decisionChatKeyIn(len(projectUUIDs), keys)).
		Unique(true).
		Select(decision.FieldChatKey).
		Strings(ctx)
	if err != nil {
		return nil, nil, nil, mapError(err)
	}

	return agents, answerers, convKeys, nil
}

// decisionAgentFacet returns the distinct asking agents of decisions in
// (projectUUIDs, keys), each named by the asker_agent_name snapshot of its
// newest decision and carrying its project_id.
func (c *CompositeStore) decisionAgentFacet(ctx context.Context, projectUUIDs []uuid.UUID, keys []string) ([]store.ProjectDecisionActor, error) {
	var rows []groupedDecisionAgentName
	err := c.client.Decision.Query().
		Where(decision.ProjectIDIn(projectUUIDs...), decisionChatKeyIn(len(projectUUIDs), keys)).
		GroupBy(decision.FieldAskerAgentID, decision.FieldProjectID, decision.FieldAskerAgentName).
		Aggregate(ent.As(ent.Max(decision.FieldCreatedAt), "latest")).
		Scan(ctx, &rows)
	if err != nil {
		return nil, mapError(err)
	}
	type newest struct {
		name      string
		projectID string
		latest    time.Time
	}
	byAgent := make(map[uuid.UUID]newest, len(rows))
	for _, r := range rows {
		latest := parseAggregateTime(r.Latest)
		cur, ok := byAgent[r.AskerAgentID]
		// Ties on the timestamp keep the greater name, so the result is
		// deterministic.
		if !ok || latest.After(cur.latest) || (latest.Equal(cur.latest) && r.Name > cur.name) {
			byAgent[r.AskerAgentID] = newest{name: r.Name, projectID: r.ProjectID.String(), latest: latest}
		}
	}
	out := make([]store.ProjectDecisionActor, 0, len(byAgent))
	for id, n := range byAgent {
		out = append(out, store.ProjectDecisionActor{ID: id.String(), Name: n.name, ProjectID: n.projectID})
	}
	sortProjectDecisionActors(out)
	return out, nil
}

func noDirectDecisionAnswer() predicate.Decision {
	return func(s *entsql.Selector) {
		s.Where(entsql.NotExists(directDecisionAnswerSubquery(s)))
	}
}

// notSupersededInterpretedAnswer excludes an agent_interpreted row when a
// direct sibling (answer, confirm, override, or dismiss) exists for the same
// decision_row_id (design-8443 §4.2, D2). Uses the unique index
// (decision_row_id, provenance).
func notSupersededInterpretedAnswer() predicate.DecisionAnswer {
	return func(s *entsql.Selector) {
		t := entsql.Table(decisionanswer.Table).As("da_dir")
		s.Where(entsql.Not(entsql.And(
			entsql.EQ(s.C(decisionanswer.FieldProvenance), decisionanswer.ProvenanceAgentInterpreted),
			entsql.Exists(
				entsql.Select(t.C(decisionanswer.FieldID)).
					From(t).
					Where(entsql.And(
						entsql.ColumnsEQ(t.C(decisionanswer.FieldDecisionRowID), s.C(decisionanswer.FieldDecisionRowID)),
						entsql.EQ(t.C(decisionanswer.FieldProvenance), decisionanswer.ProvenanceDirect),
					)),
			),
		)))
	}
}

// decisionChatKeyIn filters decisions to keys. On SQLite, when querying
// multiple projects alongside decision.ProjectIDIn, prefixing +chat_key
// prevents SQLite's planner from turning INDEX(project_id, chat_key, status)
// into a Cartesian 200x1000=200,000 B-tree seek loop while still seeking
// project_id via the index and testing chat_key in an ephemeral set.
func decisionChatKeyIn(projectCount int, keys []string) predicate.Decision {
	if projectCount <= 1 {
		return decision.ChatKeyIn(keys...)
	}
	vals := make([]any, len(keys))
	for i, k := range keys {
		vals[i] = k
	}
	return func(s *entsql.Selector) {
		if s.Dialect() != dialect.SQLite {
			decision.ChatKeyIn(keys...)(s)
			return
		}
		col := s.C(decision.FieldChatKey)
		s.Where(entsql.P(func(b *entsql.Builder) {
			b.WriteByte('+')
			b.Ident(col)
			b.WriteString(" IN (")
			b.Args(vals...)
			b.WriteByte(')')
		}))
	}
}

func decisionAnswerInChatKeys(keys []string) predicate.DecisionAnswer {
	vals := make([]any, len(keys))
	for i, k := range keys {
		vals[i] = k
	}
	return func(s *entsql.Selector) {
		t := entsql.Table(decision.Table)
		s.Where(entsql.Exists(
			entsql.Select(t.C(decision.FieldID)).
				From(t).
				Where(entsql.And(
					entsql.ColumnsEQ(t.C(decision.FieldID), s.C(decisionanswer.FieldDecisionRowID)),
					entsql.ColumnsEQ(t.C(decision.FieldProjectID), s.C(decisionanswer.FieldProjectID)),
					entsql.In(t.C(decision.FieldChatKey), vals...),
				)),
		))
	}
}

// decisionAnswerMatchesQuery keeps answers whose question text, choice label
// or key snapshot contains q case-insensitively, or whose decision row's
// asker_agent_name contains q or whose chat_key is in labelKeys. ContainsFold
// escapes %, _ and \, so q matches literally (ILIKE on Postgres, LOWER() LIKE
// on SQLite).
func decisionAnswerMatchesQuery(q string, labelKeys []string) predicate.DecisionAnswer {
	rowArms := []predicate.Decision{decision.AskerAgentNameContainsFold(q)}
	if len(labelKeys) > 0 {
		rowArms = append(rowArms, decision.ChatKeyIn(labelKeys...))
	}
	rowMatches := decision.Or(rowArms...)
	return decisionanswer.Or(
		decisionanswer.QuestionTextContainsFold(q),
		decisionanswer.ChoiceLabelContainsFold(q),
		decisionanswer.OtherTextContainsFold(q),
		decisionanswer.DismissReasonContainsFold(q),
		decisionanswer.KeyContainsFold(q),
		func(s *entsql.Selector) {
			t := entsql.Table(decision.Table)
			sub := entsql.Select(t.C(decision.FieldID)).From(t)
			rowMatches(sub)
			sub.Where(entsql.ColumnsEQ(t.C(decision.FieldID), s.C(decisionanswer.FieldDecisionRowID)))
			s.Where(entsql.Exists(sub))
		},
	)
}

func answerProvenancePredicate(raw string) (predicate.DecisionAnswer, error) {
	switch raw {
	case "":
		return nil, nil
	case store.ProjectDecisionProvenanceDirect:
		return decisionanswer.And(
			decisionanswer.ProvenanceEQ(decisionanswer.ProvenanceDirect),
			decisionanswer.OutcomeEQ(decisionanswer.OutcomeAnswer),
		), nil
	case store.ProjectDecisionProvenanceInterpreted:
		return decisionanswer.ProvenanceEQ(decisionanswer.ProvenanceAgentInterpreted), nil
	case store.ProjectDecisionProvenanceConfirmed:
		return decisionanswer.And(
			decisionanswer.ProvenanceEQ(decisionanswer.ProvenanceDirect),
			decisionanswer.OutcomeEQ(decisionanswer.OutcomeConfirm),
		), nil
	case store.ProjectDecisionProvenanceOverridden:
		return decisionanswer.And(
			decisionanswer.ProvenanceEQ(decisionanswer.ProvenanceDirect),
			decisionanswer.OutcomeEQ(decisionanswer.OutcomeOverride),
		), nil
	case store.ProjectDecisionProvenanceDismissed:
		return decisionanswer.And(
			decisionanswer.ProvenanceEQ(decisionanswer.ProvenanceDirect),
			decisionanswer.OutcomeEQ(decisionanswer.OutcomeDismiss),
		), nil
	default:
		return nil, fmt.Errorf("%w: invalid provenance filter %q", store.ErrInvalidInput, raw)
	}
}

func (c *CompositeStore) existingMessageIDSet(ctx context.Context, ids []uuid.UUID) (map[string]bool, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	found, err := c.client.Message.Query().
		Where(message.IDIn(ids...)).
		IDs(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	out := make(map[string]bool, len(found))
	for _, id := range found {
		out[id.String()] = true
	}
	return out, nil
}

// parseAggregateTime parses a MAX(timestamp) aggregate scanned as text:
// Postgres yields a time.Time that database/sql formats as RFC3339Nano, and
// SQLite yields the stored text (the driver writes time.Time.String()
// form), which loses its column type in an aggregate. An unparseable value
// is the zero time.
func parseAggregateTime(s string) time.Time {
	for _, layout := range []string{
		time.RFC3339Nano,
		"2006-01-02 15:04:05.999999999 -0700 MST",
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05.999999999Z07:00",
		"2006-01-02 15:04:05.999999999",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// parseProjectIDs parses and de-duplicates project UUIDs.
func parseProjectIDs(ids []string) ([]uuid.UUID, error) {
	out := make([]uuid.UUID, 0, len(ids))
	seen := make(map[uuid.UUID]bool, len(ids))
	for _, id := range ids {
		u, err := parseGetID(id)
		if err != nil {
			return nil, err
		}
		if !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	return out, nil
}

func cleanChatKeys(keys []string) []string {
	if len(keys) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(keys))
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		k = strings.TrimSpace(k)
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, k)
	}
	return out
}

func sortProjectDecisionActors(actors []store.ProjectDecisionActor) {
	sort.Slice(actors, func(i, j int) bool {
		ni := strings.ToLower(actors[i].Name)
		nj := strings.ToLower(actors[j].Name)
		if ni != nj {
			return ni < nj
		}
		return actors[i].ID < actors[j].ID
	})
}
