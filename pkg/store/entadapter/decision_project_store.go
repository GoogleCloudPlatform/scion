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

	entsql "entgo.io/ent/dialect/sql"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/decision"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/decisionanswer"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/message"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/predicate"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

const maxProjectDecisionFacetItems = 500

type groupedDecisionAgent struct {
	AskerAgentID uuid.UUID `json:"asker_agent_id"`
	Name         string    `json:"name"`
}

type groupedDecisionAgentName struct {
	AskerAgentID uuid.UUID `json:"asker_agent_id"`
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

// ListProjectDecisionAgents implements store.DecisionStore.
func (c *CompositeStore) ListProjectDecisionAgents(ctx context.Context, projectID string) ([]store.ProjectDecisionActor, error) {
	projectUUID, err := parseGetID(projectID)
	if err != nil {
		return nil, err
	}

	var rows []groupedDecisionAgent
	err = c.client.Decision.Query().
		Where(decision.ProjectID(projectUUID)).
		Order(ent.Asc(decision.FieldAskerAgentID)).
		GroupBy(decision.FieldAskerAgentID).
		Aggregate(ent.As(ent.Max(decision.FieldAskerAgentName), "name")).
		Scan(ctx, &rows)
	if err != nil {
		return nil, mapError(err)
	}

	out := make([]store.ProjectDecisionActor, 0, len(rows))
	for _, r := range rows {
		out = append(out, store.ProjectDecisionActor{
			ID:   r.AskerAgentID.String(),
			Name: r.Name,
		})
	}
	sortProjectDecisionActors(out)
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
	// status == pending, OR (status == decided AND NOT EXISTS direct answer).
	openRows, err := c.client.Decision.Query().
		Where(
			decision.ProjectIDIn(projectUUIDs...),
			decision.ChatKeyIn(keys...),
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
			decisionAnswerInChatKeys(projectUUIDs, keys),
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

	var rows []groupedDecisionProject
	err = c.client.Decision.Query().
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

// ListDecisionAnswersPerProject implements store.DecisionStore.
func (c *CompositeStore) ListDecisionAnswersPerProject(
	ctx context.Context,
	projectIDs []string,
	chatKeys []string,
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

	keys := cleanChatKeys(chatKeys)
	if perProject == 0 || len(keys) == 0 || len(projectUUIDs) == 0 {
		return nil, nil
	}

	var allRows []*ent.DecisionAnswer
	for _, pid := range projectUUIDs {
		singleProject := []uuid.UUID{pid}
		q := c.client.DecisionAnswer.Query().
			Where(
				decisionanswer.ProjectID(pid),
				decisionAnswerInChatKeys(singleProject, keys),
			)
		q = applyDecisionAnswerFilter(q, filter, agentUUID, provPred)
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

func applyDecisionAnswerFilter(
	q *ent.DecisionAnswerQuery,
	filter store.ProjectDecisionAnswerFilter,
	agentUUID uuid.UUID,
	provPred predicate.DecisionAnswer,
) *ent.DecisionAnswerQuery {
	if filter.Query != "" {
		q = q.Where(decisionAnswerMatchesQuery(filter.Query, cleanChatKeys(filter.QueryChatKeys)))
	}
	if filter.AgentID != "" {
		q = q.Where(decisionanswer.AskerAgentID(agentUUID))
	}
	if filter.UserID != "" {
		q = q.Where(decisionanswer.AnsweredByUserID(filter.UserID))
	}
	if provPred != nil {
		q = q.Where(provPred)
	}
	if filter.From != nil {
		q = q.Where(decisionanswer.CreatedAtGTE(filter.From.UTC()))
	}
	if filter.To != nil {
		q = q.Where(decisionanswer.CreatedAtLTE(filter.To.UTC()))
	}
	return q
}

func (c *CompositeStore) enrichDecisionAnswerRows(ctx context.Context, rows []*ent.DecisionAnswer) ([]*store.ProjectDecisionAnswerEntry, error) {
	decisionRowUUIDs := make([]uuid.UUID, 0, len(rows))
	seenDec := make(map[uuid.UUID]bool, len(rows))
	var overrideRowUUIDs []uuid.UUID
	seenOverride := make(map[uuid.UUID]bool)
	var msgUUIDsToCheck []uuid.UUID
	seenMsgCheck := make(map[uuid.UUID]bool)

	for _, r := range rows {
		if !seenDec[r.DecisionRowID] {
			seenDec[r.DecisionRowID] = true
			decisionRowUUIDs = append(decisionRowUUIDs, r.DecisionRowID)
		}
		if r.Outcome == decisionanswer.OutcomeOverride && !seenOverride[r.DecisionRowID] {
			seenOverride[r.DecisionRowID] = true
			overrideRowUUIDs = append(overrideRowUUIDs, r.DecisionRowID)
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

	prevChoiceByRow := make(map[uuid.UUID]string)
	if len(overrideRowUUIDs) > 0 {
		interpRows, err := c.client.DecisionAnswer.Query().
			Where(
				decisionanswer.DecisionRowIDIn(overrideRowUUIDs...),
				decisionanswer.ProvenanceEQ(decisionanswer.ProvenanceAgentInterpreted),
			).
			All(ctx)
		if err != nil {
			return nil, mapError(err)
		}
		for _, ir := range interpRows {
			prevChoiceByRow[ir.DecisionRowID] = ir.ChoiceLabel
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
		if ans.Outcome == store.DecisionOutcomeOverride {
			entry.PreviousChoiceLabel = prevChoiceByRow[r.DecisionRowID]
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// ListDecisionFacets implements store.DecisionStore.
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
			decisionAnswerInChatKeys(projectUUIDs, keys),
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
		Where(decision.ProjectIDIn(projectUUIDs...), decision.ChatKeyIn(keys...)).
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
// newest decision.
func (c *CompositeStore) decisionAgentFacet(ctx context.Context, projectUUIDs []uuid.UUID, keys []string) ([]store.ProjectDecisionActor, error) {
	var rows []groupedDecisionAgentName
	err := c.client.Decision.Query().
		Where(decision.ProjectIDIn(projectUUIDs...), decision.ChatKeyIn(keys...)).
		GroupBy(decision.FieldAskerAgentID, decision.FieldAskerAgentName).
		Aggregate(ent.As(ent.Max(decision.FieldCreatedAt), "latest")).
		Scan(ctx, &rows)
	if err != nil {
		return nil, mapError(err)
	}
	type newest struct {
		name   string
		latest time.Time
	}
	byAgent := make(map[uuid.UUID]newest, len(rows))
	for _, r := range rows {
		latest := parseAggregateTime(r.Latest)
		cur, ok := byAgent[r.AskerAgentID]
		// Ties on the timestamp keep the greater name, so the result is
		// deterministic.
		if !ok || latest.After(cur.latest) || (latest.Equal(cur.latest) && r.Name > cur.name) {
			byAgent[r.AskerAgentID] = newest{name: r.Name, latest: latest}
		}
	}
	out := make([]store.ProjectDecisionActor, 0, len(byAgent))
	for id, n := range byAgent {
		out = append(out, store.ProjectDecisionActor{ID: id.String(), Name: n.name})
	}
	sortProjectDecisionActors(out)
	return out, nil
}

func noDirectDecisionAnswer() predicate.Decision {
	return func(s *entsql.Selector) {
		t := entsql.Table(decisionanswer.Table)
		s.Where(entsql.NotExists(
			entsql.Select(t.C(decisionanswer.FieldID)).
				From(t).
				Where(entsql.And(
					entsql.ColumnsEQ(t.C(decisionanswer.FieldDecisionRowID), s.C(decision.FieldID)),
					entsql.EQ(t.C(decisionanswer.FieldProvenance), decisionanswer.ProvenanceDirect),
				)),
		))
	}
}

func decisionAnswerInChatKeys(projectUUIDs []uuid.UUID, keys []string) predicate.DecisionAnswer {
	vals := make([]any, len(keys))
	for i, k := range keys {
		vals[i] = k
	}
	projectVals := make([]any, len(projectUUIDs))
	for i, id := range projectUUIDs {
		projectVals[i] = id
	}
	return func(s *entsql.Selector) {
		t := entsql.Table(decision.Table)
		s.Where(entsql.Exists(
			entsql.Select(t.C(decision.FieldID)).
				From(t).
				Where(entsql.And(
					entsql.ColumnsEQ(t.C(decision.FieldID), s.C(decisionanswer.FieldDecisionRowID)),
					entsql.In(t.C(decision.FieldProjectID), projectVals...),
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
