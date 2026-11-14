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

type groupedDecisionAnswerer struct {
	AnsweredByUserID string `json:"answered_by_user_id"`
	Name             string `json:"name"`
	Email            string `json:"email"`
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

// ListProjectOpenPrompts implements store.DecisionStore.
func (c *CompositeStore) ListProjectOpenPrompts(ctx context.Context, projectID string, chatKeys []string) ([]*store.ProjectDecisionPrompt, error) {
	projectUUID, err := parseGetID(projectID)
	if err != nil {
		return nil, err
	}
	keys := cleanChatKeys(chatKeys)
	if len(keys) == 0 {
		return nil, nil
	}

	// Find every non-settled decision row in (projectID, chatKeys):
	// status == pending, OR (status == decided AND NOT EXISTS direct answer).
	openRows, err := c.client.Decision.Query().
		Where(
			decision.ProjectID(projectUUID),
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

// ListProjectDecisionAnswers implements store.DecisionStore.
func (c *CompositeStore) ListProjectDecisionAnswers(
	ctx context.Context,
	projectID string,
	chatKeys []string,
	filter store.ProjectDecisionAnswerFilter,
	cursor string,
	limit int,
) ([]*store.ProjectDecisionAnswerEntry, string, error) {
	projectUUID, err := parseGetID(projectID)
	if err != nil {
		return nil, "", err
	}
	if limit < 0 || limit > store.MaxDecisionListLimit {
		return nil, "", fmt.Errorf("%w: limit must be between 0 and %d", store.ErrInvalidInput, store.MaxDecisionListLimit)
	}

	var agentUUID uuid.UUID
	if filter.AgentID != "" {
		agentUUID, err = parseUUID(filter.AgentID)
		if err != nil {
			return nil, "", err
		}
	}
	provPred, err := answerProvenancePredicate(filter.Provenance)
	if err != nil {
		return nil, "", err
	}
	if filter.From != nil && filter.To != nil && filter.From.After(*filter.To) {
		return nil, "", fmt.Errorf("%w: from must not be after to", store.ErrInvalidInput)
	}
	var cur decisionListCursor
	if cursor != "" {
		cur, err = decodeDecisionListCursor(cursor)
		if err != nil {
			return nil, "", err
		}
	}

	keys := cleanChatKeys(chatKeys)
	if limit == 0 || len(keys) == 0 {
		return nil, "", nil
	}

	q := c.client.DecisionAnswer.Query().
		Where(
			decisionanswer.ProjectID(projectUUID),
			decisionAnswerInChatKeys(projectUUID, keys),
		)
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
		return nil, "", mapError(err)
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
			return nil, "", mapError(err)
		}
		for _, ir := range interpRows {
			prevChoiceByRow[ir.DecisionRowID] = ir.ChoiceLabel
		}
	}

	existingMsgs, err := c.existingMessageIDSet(ctx, msgUUIDsToCheck)
	if err != nil {
		return nil, "", err
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
	return entries, nextCursor, nil
}

// ListProjectDecisionFacets implements store.DecisionStore.
func (c *CompositeStore) ListProjectDecisionFacets(
	ctx context.Context,
	projectID string,
	chatKeys []string,
) ([]store.ProjectDecisionActor, []store.ProjectDecisionActor, error) {
	projectUUID, err := parseGetID(projectID)
	if err != nil {
		return nil, nil, err
	}
	keys := cleanChatKeys(chatKeys)
	if len(keys) == 0 {
		return nil, nil, nil
	}

	var decRows []groupedDecisionAgent
	err = c.client.Decision.Query().
		Where(decision.ProjectID(projectUUID), decision.ChatKeyIn(keys...)).
		Order(ent.Asc(decision.FieldAskerAgentID)).
		Limit(maxProjectDecisionFacetItems).
		GroupBy(decision.FieldAskerAgentID).
		Aggregate(ent.As(ent.Max(decision.FieldAskerAgentName), "name")).
		Scan(ctx, &decRows)
	if err != nil {
		return nil, nil, mapError(err)
	}
	agents := make([]store.ProjectDecisionActor, 0, len(decRows))
	for _, r := range decRows {
		agents = append(agents, store.ProjectDecisionActor{ID: r.AskerAgentID.String(), Name: r.Name})
	}
	sortProjectDecisionActors(agents)

	var ansRows []groupedDecisionAnswerer
	err = c.client.DecisionAnswer.Query().
		Where(
			decisionanswer.ProjectID(projectUUID),
			decisionanswer.AnsweredByUserIDNotNil(),
			decisionanswer.AnsweredByUserIDNEQ(""),
			decisionAnswerInChatKeys(projectUUID, keys),
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
		return nil, nil, mapError(err)
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

	return agents, answerers, nil
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

func decisionAnswerInChatKeys(projectUUID uuid.UUID, keys []string) predicate.DecisionAnswer {
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
					entsql.EQ(t.C(decision.FieldProjectID), projectUUID),
					entsql.In(t.C(decision.FieldChatKey), vals...),
				)),
		))
	}
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
