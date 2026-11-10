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
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/decision"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/decisionanswer"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/decisionkeycounter"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Choice prompt decisions and answers (choices, ptone/scion#7514). The
// methods live on CompositeStore because each one writes a message and
// decision rows in one transaction.

// errDecisionRace marks a write that lost a race with a concurrent prompt
// or answer: a unique-index violation on decisions, decision_answers or
// decision_key_counters, or an active version replaced under us. The
// non-transactional entry points retry (CreateChoicePrompt, up to
// maxDecisionRaceRetries times) or re-read (RecordAnswer) once on it.
var errDecisionRace = errors.New("concurrent decision write")

// maxDecisionRaceRetries bounds CreateChoicePrompt's retries of a lost
// race: each retry reads the winner's rows, so n concurrent reposts of one
// key need up to n-1 retries for the last one.
const maxDecisionRaceRetries = 5

// markDecisionRace wraps a constraint violation as errDecisionRace and
// maps any other error as usual.
func markDecisionRace(err error) error {
	if ent.IsConstraintError(err) {
		return fmt.Errorf("%w: %v", errDecisionRace, err)
	}
	return mapError(err)
}

// CreateChoicePrompt implements store.DecisionStore.
func (c *CompositeStore) CreateChoicePrompt(ctx context.Context, in *store.ChoicePromptInput) (*store.ChoicePromptResult, error) {
	if err := validateChoicePromptInput(in); err != nil {
		return nil, err
	}
	if c.inTx {
		return c.createChoicePromptTx(ctx, in)
	}
	var res *store.ChoicePromptResult
	for attempt := 0; ; attempt++ {
		err := c.WithTx(ctx, func(tx store.Store) error {
			var err error
			res, err = tx.(*CompositeStore).createChoicePromptTx(ctx, in)
			return err
		})
		if err == nil {
			return res, nil
		}
		// The loser of a concurrent repost (or of the first use of the
		// agent's counter) retries on the winner's rows.
		if attempt < maxDecisionRaceRetries && (errors.Is(err, errDecisionRace) || errors.Is(err, store.ErrTransient)) {
			continue
		}
		if errors.Is(err, errDecisionRace) {
			return nil, store.ErrTransient
		}
		return nil, err
	}
}

func validateChoicePromptInput(in *store.ChoicePromptInput) error {
	if in == nil || in.Message == nil || in.Message.ID == "" || in.Message.ProjectID == "" {
		return fmt.Errorf("%w: a choice prompt needs a message with an ID and a project", store.ErrInvalidInput)
	}
	// Every ID is a UUID column; a malformed one is refused here, before
	// the transaction reserves numbers or replaces versions.
	for _, id := range []string{in.Message.ID, in.Message.ProjectID, in.AskerAgentID} {
		if _, err := parseUUID(id); err != nil {
			return err
		}
	}
	if len(in.Decisions) == 0 || len(in.Decisions) > messages.MaxDecisionsPerMessage {
		return fmt.Errorf("%w: a choice prompt carries 1 to %d decisions", store.ErrInvalidInput, messages.MaxDecisionsPerMessage)
	}
	seen := map[string]bool{}
	for i, d := range in.Decisions {
		if d.Key != "" {
			if !messages.ValidDecisionKey(d.Key) {
				return fmt.Errorf("%w: decision %d has an invalid key", store.ErrInvalidInput, i)
			}
			norm := messages.DecisionKeyNorm(d.Key)
			if seen[norm] {
				return fmt.Errorf("%w: decision key %q repeats in the message", store.ErrInvalidInput, d.Key)
			}
			seen[norm] = true
		}
		if d.Question == "" || len(d.Choices) < messages.MinChoicesPerDecision {
			return fmt.Errorf("%w: decision %d is not normalized", store.ErrInvalidInput, i)
		}
	}
	return nil
}

// keyState is what one key of a prompt finds in the agent's rows.
type keyState struct {
	maxVersion int
	active     *ent.Decision // the highest version, unless it is replaced
}

// createChoicePromptTx is CreateChoicePrompt inside the transaction c runs
// in (c.client is the transaction's client).
func (c *CompositeStore) createChoicePromptTx(ctx context.Context, in *store.ChoicePromptInput) (*store.ChoicePromptResult, error) {
	client := c.client
	agentID, _ := parseUUID(in.AskerAgentID)
	now := time.Now().UTC()

	// 1. Hub numbers: reserve one per decision without a key. The counter
	// row's update locks it, so concurrent prompts of one agent take
	// disjoint numbers; a concurrent first use of the counter collides on
	// its primary key and retries.
	need := 0
	for _, d := range in.Decisions {
		if d.Key == "" {
			need++
		}
	}
	next, err := reserveDecisionNumbers(ctx, client, agentID, need)
	if err != nil {
		return nil, err
	}
	firstNumber := next - int64(need)

	// 2. An all-digit key must be a number the hub already issued to the
	// agent (below the numbers reserved just now).
	var notIssued []string
	for _, d := range in.Decisions {
		if d.Key != "" && messages.IsHubDecisionKey(d.Key) {
			var n int64
			if _, err := fmt.Sscan(d.Key, &n); err != nil || n >= firstNumber {
				notIssued = append(notIssued, d.Key)
			}
		}
	}
	if len(notIssued) > 0 {
		return nil, &store.DecisionKeyNotIssuedError{Keys: notIssued}
	}

	// 3. The existing versions of every key the prompt names.
	states, err := loadKeyStates(ctx, client, agentID, in.Decisions, c.isPostgres())
	if err != nil {
		return nil, err
	}
	if !in.Update {
		var existing []store.ExistingDecisionKey
		var activeIDs []uuid.UUID
		for _, d := range in.Decisions {
			if st, ok := states[messages.DecisionKeyNorm(d.Key)]; ok && d.Key != "" && st.active != nil {
				activeIDs = append(activeIDs, st.active.ID)
			}
		}
		answers, err := loadAnswers(ctx, client, activeIDs)
		if err != nil {
			return nil, err
		}
		for _, d := range in.Decisions {
			st, ok := states[messages.DecisionKeyNorm(d.Key)]
			if d.Key == "" || !ok {
				continue
			}
			ek := store.ExistingDecisionKey{Key: d.Key, Version: st.maxVersion, Status: store.DecisionStatusReplaced}
			if st.active != nil {
				ek.Key = st.active.Key
				ek.Status = string(st.active.Status)
				if st.active.Status == decision.StatusDecided {
					ek.Answer = effectiveAnswer(answers[st.active.ID])
				}
			}
			existing = append(existing, ek)
		}
		if len(existing) > 0 {
			return nil, &store.ChoiceKeyExistsError{Existing: existing}
		}
	}

	// 4. Keys, versions and replacements, in message order.
	decided := map[uuid.UUID][]*ent.DecisionAnswer{}
	if in.Update {
		var ids []uuid.UUID
		for _, st := range states {
			if st.active != nil && st.active.Status == decision.StatusDecided {
				ids = append(ids, st.active.ID)
			}
		}
		if decided, err = loadAnswers(ctx, client, ids); err != nil {
			return nil, err
		}
	}
	keyed := make([]messages.Decision, len(in.Decisions))
	results := make([]store.DecisionResult, len(in.Decisions))
	rowIDs := make([]uuid.UUID, len(in.Decisions))
	number := firstNumber
	for i, d := range in.Decisions {
		keyed[i] = d
		rowIDs[i] = uuid.New()
		r := store.DecisionResult{RowID: rowIDs[i].String(), Version: 1, KeySource: store.DecisionKeySourceAgent}
		switch {
		case d.Key == "":
			keyed[i].Key = fmt.Sprint(number)
			number++
			r.KeySource = store.DecisionKeySourceHub
		case messages.IsHubDecisionKey(d.Key):
			r.KeySource = store.DecisionKeySourceHub
		}
		if st, ok := states[messages.DecisionKeyNorm(keyed[i].Key)]; ok && d.Key != "" {
			r.Version = st.maxVersion + 1
			if st.active != nil {
				// Display the key as first given.
				keyed[i].Key = st.active.Key
				prev := &store.ReplacedDecision{RowID: st.active.ID.String(), Version: st.active.Version, Status: string(st.active.Status)}
				if st.active.Status == decision.StatusDecided {
					prev.Answer = effectiveAnswer(decided[st.active.ID])
				}
				n, err := client.Decision.Update().
					Where(decision.ID(st.active.ID), decision.StatusNEQ(decision.StatusReplaced)).
					SetStatus(decision.StatusReplaced).
					SetReplacedAt(now).
					SetReplacedByID(rowIDs[i]).
					Save(ctx)
				if err != nil {
					return nil, mapError(err)
				}
				if n != 1 {
					return nil, fmt.Errorf("%w: version %d of %s was replaced concurrently", errDecisionRace, st.active.Version, st.active.Key)
				}
				r.Replaced = prev
			}
		}
		r.Key = keyed[i].Key
		results[i] = r
	}

	// 5. The pending cap, counted after this prompt's replacements.
	pending, err := client.Decision.Query().
		Where(decision.AskerAgentID(agentID), decision.StatusEQ(decision.StatusPending)).
		Count(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	if pending+len(in.Decisions) > store.MaxPendingDecisionsPerAgent {
		return nil, fmt.Errorf("%w: you have %d pending decisions and this message adds %d; at most %d may be pending", store.ErrTooManyPendingDecisions, pending, len(in.Decisions), store.MaxPendingDecisionsPerAgent)
	}

	// 6. The message, with the markdown fallback as its text.
	msgText, introLen := messages.RenderChoiceFallback(in.Intro, keyed, in.AnswerURL)
	in.Message.Msg = msgText
	in.Message.Type = messages.TypeChoicePrompt
	if err := c.CreateMessage(ctx, in.Message); err != nil {
		return nil, err
	}
	messageID, _ := parseUUID(in.Message.ID)
	projectID, _ := parseUUID(in.Message.ProjectID)
	var conversationID *uuid.UUID
	if in.Message.ConversationID != "" {
		cid, err := parseUUID(in.Message.ConversationID)
		if err != nil {
			return nil, err
		}
		conversationID = &cid
	}

	// 7. The decision rows.
	creates := make([]*ent.DecisionCreate, len(keyed))
	for i, d := range keyed {
		cr := client.Decision.Create().
			SetID(rowIDs[i]).
			SetProjectID(projectID).
			SetNillableConversationID(conversationID).
			SetMessageID(messageID).
			SetPosition(i).
			SetAskerAgentID(agentID).
			SetAskerAgentName(in.AskerAgentName).
			SetKey(d.Key).
			SetKeyNorm(messages.DecisionKeyNorm(d.Key)).
			SetKeySource(decision.KeySource(results[i].KeySource)).
			SetVersion(results[i].Version).
			SetQuestion(d.Question).
			SetChoices(d.Choices).
			SetNillableDefaultChoiceID(nullableString(d.DefaultChoiceID)).
			SetCreatedAt(now)
		if i == 0 {
			cr.SetIntroLength(introLen)
		}
		creates[i] = cr
	}
	if _, err := client.Decision.CreateBulk(creates...).Save(ctx); err != nil {
		return nil, markDecisionRace(err)
	}
	return &store.ChoicePromptResult{Decisions: results, IntroLength: introLen}, nil
}

// reserveDecisionNumbers takes need numbers from the agent's counter and
// returns the counter's new next value; the reserved numbers are
// [next-need, next). With need 0 it only reads the counter (1 when the
// agent has none yet).
func reserveDecisionNumbers(ctx context.Context, client *ent.Client, agentID uuid.UUID, need int) (int64, error) {
	if need == 0 {
		row, err := client.DecisionKeyCounter.Get(ctx, agentID)
		if ent.IsNotFound(err) {
			return 1, nil
		}
		if err != nil {
			return 0, mapError(err)
		}
		return row.Next, nil
	}
	row, err := client.DecisionKeyCounter.UpdateOneID(agentID).AddNext(int64(need)).Save(ctx)
	if err == nil {
		return row.Next, nil
	}
	if !ent.IsNotFound(err) {
		return 0, mapError(err)
	}
	row, err = client.DecisionKeyCounter.Create().SetID(agentID).SetNext(int64(need) + 1).Save(ctx)
	if err != nil {
		return 0, markDecisionRace(err)
	}
	return row.Next, nil
}

// loadKeyStates reads, for every key the prompt names, its highest version
// and its active (not replaced) version. With lock (Postgres) it locks the
// rows it reads for the transaction, in key and version order: an answer
// in flight on the active row (recordAnswerTx locks it too) commits first,
// so the active row's status read here, and the answers read after it, are
// current when the repost replaces it.
func loadKeyStates(ctx context.Context, client *ent.Client, agentID uuid.UUID, ds []messages.Decision, lock bool) (map[string]*keyState, error) {
	var norms []string
	for _, d := range ds {
		if d.Key != "" {
			norms = append(norms, messages.DecisionKeyNorm(d.Key))
		}
	}
	states := map[string]*keyState{}
	if len(norms) == 0 {
		return states, nil
	}
	q := client.Decision.Query().
		Where(decision.AskerAgentID(agentID), decision.KeyNormIn(norms...)).
		Order(decision.ByKeyNorm(), decision.ByVersion())
	if lock {
		q = q.ForUpdate()
	}
	rows, err := q.All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	for _, r := range rows {
		st := states[r.KeyNorm]
		if st == nil {
			st = &keyState{}
			states[r.KeyNorm] = st
		}
		if r.Version > st.maxVersion {
			st.maxVersion = r.Version
			st.active = nil
			if r.Status != decision.StatusReplaced {
				st.active = r
			}
		}
	}
	return states, nil
}

// loadAnswers returns the answers of the given decision rows, oldest first.
func loadAnswers(ctx context.Context, client *ent.Client, rowIDs []uuid.UUID) (map[uuid.UUID][]*ent.DecisionAnswer, error) {
	out := map[uuid.UUID][]*ent.DecisionAnswer{}
	if len(rowIDs) == 0 {
		return out, nil
	}
	rows, err := client.DecisionAnswer.Query().
		Where(decisionanswer.DecisionRowIDIn(rowIDs...)).
		Order(decisionanswer.ByCreatedAt(), decisionanswer.ByID()).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	for _, r := range rows {
		out[r.DecisionRowID] = append(out[r.DecisionRowID], r)
	}
	return out, nil
}

// effectiveAnswer is the direct answer if present, else the interpreted
// one, as a store value.
func effectiveAnswer(rows []*ent.DecisionAnswer) *store.DecisionAnswer {
	d := &store.Decision{Answers: answersToStore(rows)}
	return d.EffectiveAnswer()
}

// GetDecision implements store.DecisionStore.
func (c *CompositeStore) GetDecision(ctx context.Context, rowID string) (*store.Decision, error) {
	uid, err := parseGetID(rowID)
	if err != nil {
		return nil, err
	}
	row, err := c.client.Decision.Get(ctx, uid)
	if err != nil {
		return nil, mapError(err)
	}
	answers, err := loadAnswers(ctx, c.client, []uuid.UUID{uid})
	if err != nil {
		return nil, err
	}
	d := decisionToStore(row)
	d.Answers = answersToStore(answers[uid])
	return d, nil
}

// GetDecisionsByMessageIDs implements store.DecisionStore.
func (c *CompositeStore) GetDecisionsByMessageIDs(ctx context.Context, messageIDs []string) (map[string][]*store.Decision, error) {
	out := map[string][]*store.Decision{}
	uids := make([]uuid.UUID, 0, len(messageIDs))
	for _, id := range messageIDs {
		if uid, err := uuid.Parse(id); err == nil {
			uids = append(uids, uid)
		}
	}
	if len(uids) == 0 {
		return out, nil
	}
	rows, err := c.client.Decision.Query().
		Where(decision.MessageIDIn(uids...)).
		Order(decision.ByMessageID(), decision.ByPosition()).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	rowIDs := make([]uuid.UUID, len(rows))
	for i, r := range rows {
		rowIDs[i] = r.ID
	}
	answers, err := loadAnswers(ctx, c.client, rowIDs)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		d := decisionToStore(r)
		d.Answers = answersToStore(answers[r.ID])
		out[d.MessageID] = append(out[d.MessageID], d)
	}
	return out, nil
}

// GetDecisionRowsByIDs implements store.DecisionStore.
func (c *CompositeStore) GetDecisionRowsByIDs(ctx context.Context, rowIDs []string) (map[string]*store.Decision, error) {
	out := map[string]*store.Decision{}
	uids := make([]uuid.UUID, 0, len(rowIDs))
	for _, id := range rowIDs {
		if uid, err := uuid.Parse(id); err == nil {
			uids = append(uids, uid)
		}
	}
	if len(uids) == 0 {
		return out, nil
	}
	rows, err := c.client.Decision.Query().
		Where(decision.IDIn(uids...)).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	for _, r := range rows {
		out[r.ID.String()] = decisionToStore(r)
	}
	return out, nil
}

// GetDecisionAnswersByResponseMessageIDs implements store.DecisionStore.
func (c *CompositeStore) GetDecisionAnswersByResponseMessageIDs(ctx context.Context, messageIDs []string) (map[string]*store.DecisionAnswer, error) {
	out := map[string]*store.DecisionAnswer{}
	uids := make([]uuid.UUID, 0, len(messageIDs))
	for _, id := range messageIDs {
		if uid, err := uuid.Parse(id); err == nil {
			uids = append(uids, uid)
		}
	}
	if len(uids) == 0 {
		return out, nil
	}
	rows, err := c.client.DecisionAnswer.Query().
		Where(decisionanswer.ResponseMessageIDIn(uids...)).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	for _, r := range rows {
		out[r.ResponseMessageID.String()] = answerToStore(r)
	}
	return out, nil
}

// deleteProjectDecisions removes a project's decisions and answers, and the
// key counters of the agents deleted with it, inside the project delete's
// transaction. The tables carry project_id as a plain column with no
// foreign key, so the cascade is explicit. Chat retention never touches
// them; only project deletion does.
//
// Only the counters of agentIDs (the agents deleted with the project) go.
// The counter of an agent hard-deleted earlier, outside a project delete,
// stays as an orphan row: that is accepted, as decided in the choices
// design. It is one small row per such agent, and no agent ID is reused.
func deleteProjectDecisions(ctx context.Context, client *ent.Client, projectID uuid.UUID, agentIDs []uuid.UUID) error {
	if _, err := client.DecisionAnswer.Delete().
		Where(decisionanswer.ProjectIDEQ(projectID)).Exec(ctx); err != nil {
		return mapError(err)
	}
	if _, err := client.Decision.Delete().
		Where(decision.ProjectIDEQ(projectID)).Exec(ctx); err != nil {
		return mapError(err)
	}
	if len(agentIDs) > 0 {
		if _, err := client.DecisionKeyCounter.Delete().
			Where(decisionkeycounter.IDIn(agentIDs...)).Exec(ctx); err != nil {
			return mapError(err)
		}
	}
	return nil
}

// clearDecisionAnswerUser clears answered_by_user_id on a deleted user's
// answers; the name and email snapshots stay (design choices v4, O2).
func clearDecisionAnswerUser(ctx context.Context, client *ent.Client, userID string) error {
	if err := client.DecisionAnswer.Update().
		Where(decisionanswer.AnsweredByUserID(userID)).
		ClearAnsweredByUserID().Exec(ctx); err != nil {
		return mapError(err)
	}
	return nil
}

// RecordAnswer implements store.DecisionStore.
func (c *CompositeStore) RecordAnswer(ctx context.Context, in *store.RecordAnswerInput) (*store.DecisionAnswer, bool, error) {
	if err := validateRecordAnswerInput(in); err != nil {
		return nil, false, err
	}
	if c.inTx {
		return c.recordAnswerTx(ctx, in)
	}
	var (
		answer   *store.DecisionAnswer
		replayed bool
	)
	err := c.WithTx(ctx, func(tx store.Store) error {
		var err error
		answer, replayed, err = tx.(*CompositeStore).recordAnswerTx(ctx, in)
		return err
	})
	if err == nil {
		return answer, replayed, nil
	}
	if !errors.Is(err, errDecisionRace) {
		return nil, false, err
	}
	// A concurrent answer won. It is ours to replay only if it carries
	// the same idempotency key, choice and answerer.
	if in.IdempotencyKey != "" {
		if a, ok, rerr := c.replayableAnswer(ctx, in); rerr != nil {
			return nil, false, rerr
		} else if ok {
			return a, true, nil
		}
	}
	return nil, false, fmt.Errorf("%w: the decision was answered concurrently", store.ErrDecisionAnswerConflict)
}

func validateRecordAnswerInput(in *store.RecordAnswerInput) error {
	if in == nil || in.ResponseMessage == nil || in.ResponseMessage.ID == "" || in.ResponseMessage.ProjectID == "" {
		return fmt.Errorf("%w: an answer needs a response message with an ID and a project", store.ErrInvalidInput)
	}
	if in.ChoiceID == "" {
		return fmt.Errorf("%w: an answer needs a choice id", store.ErrInvalidInput)
	}
	if err := decisionanswer.ProvenanceValidator(decisionanswer.Provenance(in.Provenance)); err != nil {
		return fmt.Errorf("%w: %v", store.ErrInvalidInput, err)
	}
	if in.Outcome != "" {
		if err := decisionanswer.OutcomeValidator(decisionanswer.Outcome(in.Outcome)); err != nil {
			return fmt.Errorf("%w: %v", store.ErrInvalidInput, err)
		}
	}
	if err := decisionanswer.SurfaceValidator(decisionanswer.Surface(in.Surface)); err != nil {
		return fmt.Errorf("%w: %v", store.ErrInvalidInput, err)
	}
	interpreted := in.Provenance == store.DecisionProvenanceAgentInterpreted
	if interpreted != (in.InterpretedByAgentID != "") || interpreted != (in.SourceMessageID != "") {
		return fmt.Errorf("%w: interpreted_by_agent_id and source_message_id are set exactly for an agent_interpreted answer", store.ErrInvalidInput)
	}
	if len(in.IdempotencyKey) > store.MaxDecisionIdempotencyKeyLen {
		return fmt.Errorf("%w: the idempotency key is at most %d bytes", store.ErrInvalidInput, store.MaxDecisionIdempotencyKeyLen)
	}
	return nil
}

// sameAnswer reports whether an existing row is the answer in describes:
// same choice and same answerer.
func sameAnswer(row *ent.DecisionAnswer, in *store.RecordAnswerInput) bool {
	if row.ChoiceID != in.ChoiceID {
		return false
	}
	if in.AnsweredByUserID != "" || row.AnsweredByUserID != nil {
		return row.AnsweredByUserID != nil && *row.AnsweredByUserID == in.AnsweredByUserID
	}
	return row.AnsweredByEmail == in.AnsweredByEmail
}

// replayableAnswer returns the row recorded under in's idempotency key when
// it is the same answer. ok is false when there is no such row;
// ErrDecisionAnswerConflict when the key names a different answer.
func (c *CompositeStore) replayableAnswer(ctx context.Context, in *store.RecordAnswerInput) (*store.DecisionAnswer, bool, error) {
	rowID, err := parseGetID(in.DecisionRowID)
	if err != nil {
		return nil, false, err
	}
	row, err := c.client.DecisionAnswer.Query().
		Where(decisionanswer.DecisionRowID(rowID), decisionanswer.IdempotencyKey(in.IdempotencyKey)).
		Only(ctx)
	if ent.IsNotFound(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, mapError(err)
	}
	if !sameAnswer(row, in) {
		return nil, false, fmt.Errorf("%w: the idempotency key was used for a different answer", store.ErrDecisionAnswerConflict)
	}
	return answerToStore(row), true, nil
}

// recordAnswerTx is RecordAnswer inside the transaction c runs in.
func (c *CompositeStore) recordAnswerTx(ctx context.Context, in *store.RecordAnswerInput) (*store.DecisionAnswer, bool, error) {
	client := c.client
	rowID, err := parseGetID(in.DecisionRowID)
	if err != nil {
		return nil, false, err
	}
	// Lock the row on Postgres: a concurrent repost of the key (which
	// locks it in loadKeyStates) either finishes first, and this answer
	// sees the version replaced, or waits and then sees it decided.
	q := client.Decision.Query().Where(decision.ID(rowID))
	if c.isPostgres() {
		q = q.ForUpdate()
	}
	row, err := q.Only(ctx)
	if err != nil {
		return nil, false, mapError(err)
	}
	// A replay is answered first, so a retry after the version was
	// replaced still gets the answer it recorded.
	if in.IdempotencyKey != "" {
		if a, ok, err := c.replayableAnswer(ctx, in); err != nil || ok {
			return a, ok, err
		}
	}
	if row.Status == decision.StatusReplaced {
		return nil, false, fmt.Errorf("%w: version %d of %s was replaced", store.ErrDecisionReplaced, row.Version, row.Key)
	}
	var label string
	for _, ch := range row.Choices {
		if ch.ID == in.ChoiceID {
			label = ch.Label
		}
	}
	if label == "" {
		return nil, false, fmt.Errorf("%w: %s has no choice %q", store.ErrInvalidInput, row.Key, in.ChoiceID)
	}
	existing, err := loadAnswers(ctx, client, []uuid.UUID{rowID})
	if err != nil {
		return nil, false, err
	}
	outcome := in.Outcome
	if outcome == "" {
		outcome = store.DecisionOutcomeAnswer
	}
	if err := checkAnswerPreconditions(row, existing[rowID], in, outcome); err != nil {
		return nil, false, err
	}

	in.ResponseMessage.Type = messages.TypeChoiceResponse
	if err := c.CreateMessage(ctx, in.ResponseMessage); err != nil {
		return nil, false, err
	}
	responseID, _ := parseUUID(in.ResponseMessage.ID)
	now := time.Now().UTC()
	cr := client.DecisionAnswer.Create().
		SetDecisionRowID(rowID).
		SetProjectID(row.ProjectID).
		SetAskerAgentID(row.AskerAgentID).
		SetKey(row.Key).
		SetVersion(row.Version).
		SetQuestionText(row.Question).
		SetChoiceID(in.ChoiceID).
		SetChoiceLabel(label).
		SetNillableAnsweredByUserID(nullableString(in.AnsweredByUserID)).
		SetAnsweredByName(in.AnsweredByName).
		SetAnsweredByEmail(in.AnsweredByEmail).
		SetProvenance(decisionanswer.Provenance(in.Provenance)).
		SetOutcome(decisionanswer.Outcome(outcome)).
		SetSurface(decisionanswer.Surface(in.Surface)).
		SetResponseMessageID(responseID).
		SetNillableAuthorizingUserID(nullableString(in.AuthorizingUserID)).
		SetNillableDelegationGrantID(nullableString(in.DelegationGrantID)).
		SetNillableIdempotencyKey(nullableString(in.IdempotencyKey)).
		SetMatchedDefault(row.DefaultChoiceID != nil && *row.DefaultChoiceID == in.ChoiceID).
		SetCreatedAt(now)
	if in.InterpretedByAgentID != "" {
		aid, err := parseUUID(in.InterpretedByAgentID)
		if err != nil {
			return nil, false, err
		}
		cr.SetInterpretedByAgentID(aid)
	}
	if in.SourceMessageID != "" {
		sid, err := parseUUID(in.SourceMessageID)
		if err != nil {
			return nil, false, err
		}
		cr.SetSourceMessageID(sid)
	}
	saved, err := cr.Save(ctx)
	if err != nil {
		return nil, false, markDecisionRace(err)
	}
	// Only the pending-to-decided transition sets decided_at; a later
	// answer (a direct one after an interpreted one) leaves it.
	// It must change exactly the row read above: none changed means a
	// concurrent repost replaced the version, and the whole answer rolls
	// back.
	if row.Status == decision.StatusPending {
		n, err := client.Decision.Update().
			Where(decision.ID(rowID), decision.StatusEQ(decision.StatusPending)).
			SetStatus(decision.StatusDecided).
			SetDecidedAt(now).
			Save(ctx)
		if err != nil {
			return nil, false, mapError(err)
		}
		if n != 1 {
			return nil, false, fmt.Errorf("%w: version %d of %s was replaced concurrently", store.ErrDecisionReplaced, row.Version, row.Key)
		}
	}
	return answerToStore(saved), false, nil
}

// RecordAnswerSet implements store.DecisionStore.
func (c *CompositeStore) RecordAnswerSet(ctx context.Context, in *store.RecordAnswerSetInput) (*store.AnswerSetResult, error) {
	if err := validateRecordAnswerSetInput(in); err != nil {
		return nil, err
	}
	if c.inTx {
		return c.recordAnswerSetTx(ctx, in)
	}
	var res *store.AnswerSetResult
	for attempt := 0; ; attempt++ {
		err := c.WithTx(ctx, func(tx store.Store) error {
			var err error
			res, err = tx.(*CompositeStore).recordAnswerSetTx(ctx, in)
			return err
		})
		if err == nil {
			return res, nil
		}
		// The loser of a concurrent answer or repost retries: the next
		// attempt sees the winner's rows and reports the changed set (or
		// replays its own key).
		if attempt < maxDecisionRaceRetries && (errors.Is(err, errDecisionRace) || errors.Is(err, store.ErrTransient)) {
			continue
		}
		if errors.Is(err, errDecisionRace) {
			return nil, store.ErrTransient
		}
		return nil, err
	}
}

func validateRecordAnswerSetInput(in *store.RecordAnswerSetInput) error {
	if in == nil || in.ResponseMessage == nil || in.ResponseMessage.ID == "" || in.ResponseMessage.ProjectID == "" {
		return fmt.Errorf("%w: a set submit needs a response message with an ID and a project", store.ErrInvalidInput)
	}
	if in.AnsweredByUserID == "" {
		return fmt.Errorf("%w: a set submit needs the answering user", store.ErrInvalidInput)
	}
	if len(in.Answers) == 0 {
		return fmt.Errorf("%w: a set submit needs at least one answer", store.ErrInvalidInput)
	}
	if len(in.IdempotencyKey) > store.MaxDecisionIdempotencyKeyLen {
		return fmt.Errorf("%w: the idempotency key is at most %d bytes", store.ErrInvalidInput, store.MaxDecisionIdempotencyKeyLen)
	}
	seen := make(map[string]bool, len(in.Answers))
	for _, a := range in.Answers {
		if a.DecisionRowID == "" || a.ChoiceID == "" {
			return fmt.Errorf("%w: every answer needs a decision row id and a choice id", store.ErrInvalidInput)
		}
		if seen[a.DecisionRowID] {
			return fmt.Errorf("%w: decision row %s is answered twice", store.ErrInvalidInput, a.DecisionRowID)
		}
		seen[a.DecisionRowID] = true
		if a.Outcome != "" {
			if err := decisionanswer.OutcomeValidator(decisionanswer.Outcome(a.Outcome)); err != nil {
				return fmt.Errorf("%w: %v", store.ErrInvalidInput, err)
			}
		}
	}
	return nil
}

// recordAnswerSetTx is RecordAnswerSet inside the transaction c runs in.
func (c *CompositeStore) recordAnswerSetTx(ctx context.Context, in *store.RecordAnswerSetInput) (*store.AnswerSetResult, error) {
	client := c.client
	msgID, err := parseGetID(in.MessageID)
	if err != nil {
		return nil, err
	}
	// Lock the prompt's rows on Postgres, in position order: concurrent
	// set submits, one-click answers and reposts of a key serialize here.
	q := client.Decision.Query().Where(decision.MessageID(msgID)).Order(ent.Asc(decision.FieldPosition))
	if c.isPostgres() {
		q = q.ForUpdate()
	}
	rows, err := q.All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("%w: message %s has no decisions", store.ErrNotFound, in.MessageID)
	}
	byID := make(map[string]*ent.Decision, len(rows))
	rowIDs := make([]uuid.UUID, len(rows))
	for i, r := range rows {
		byID[r.ID.String()] = r
		rowIDs[i] = r.ID
	}

	// A replay is answered first, so a retry after the set changed still
	// gets the answers it recorded.
	if in.IdempotencyKey != "" {
		if res, ok, err := replayableAnswerSet(ctx, client, rows, in); err != nil || ok {
			return res, err
		}
	}

	// Shape: every requested row is in the prompt and names one of its
	// choices.
	labels := make(map[string]string, len(in.Answers))
	for _, a := range in.Answers {
		r, ok := byID[a.DecisionRowID]
		if !ok {
			return nil, fmt.Errorf("%w: decision row %s is not in message %s", store.ErrInvalidInput, a.DecisionRowID, in.MessageID)
		}
		label := ""
		for _, ch := range r.Choices {
			if ch.ID == a.ChoiceID {
				label = ch.Label
			}
		}
		if label == "" {
			return nil, fmt.Errorf("%w: %s has no choice %q", store.ErrInvalidInput, r.Key, a.ChoiceID)
		}
		labels[a.DecisionRowID] = label
	}

	// The actionable set (design choices v5, 4.1) must equal the request.
	existing, err := loadAnswers(ctx, client, rowIDs)
	if err != nil {
		return nil, err
	}
	requested := make(map[string]store.AnswerSetItem, len(in.Answers))
	for _, a := range in.Answers {
		requested[a.DecisionRowID] = a
	}
	reasons := map[string]string{}
	for _, r := range rows {
		d := decisionToStore(r)
		d.Answers = answersToStore(existing[r.ID])
		reason := store.DecisionActionableReason(d, in.AnsweredByUserID)
		_, asked := requested[d.ID]
		switch {
		case asked && reason != "":
			reasons[d.ID] = reason
		case !asked && reason == "":
			reasons[d.ID] = store.AnswerSetReasonMissing
		}
	}
	// Each outcome must be the one the row's answers give: a direct
	// answer over the user's own interpreted one is a confirm (same
	// choice) or an override (another choice); every other answer is
	// outcome answer. A difference means the state changed after the
	// caller read it.
	outcomes := make(map[string]string, len(in.Answers))
	for _, a := range in.Answers {
		if _, bad := reasons[a.DecisionRowID]; bad {
			continue
		}
		r := byID[a.DecisionRowID]
		want := store.DecisionOutcomeAnswer
		for _, e := range existing[r.ID] {
			if e.Provenance == decisionanswer.ProvenanceAgentInterpreted {
				want = store.DecisionOutcomeConfirm
				if e.ChoiceID != a.ChoiceID {
					want = store.DecisionOutcomeOverride
				}
			}
		}
		got := a.Outcome
		if got == "" {
			got = store.DecisionOutcomeAnswer
		}
		if got != want {
			reasons[a.DecisionRowID] = store.AnswerSetReasonDecided
			continue
		}
		outcomes[a.DecisionRowID] = want
	}
	if len(reasons) > 0 {
		return nil, &store.AnswerSetChangedError{Reasons: reasons}
	}

	in.ResponseMessage.Type = messages.TypeChoiceResponse
	if err := c.CreateMessage(ctx, in.ResponseMessage); err != nil {
		return nil, err
	}
	responseID, _ := parseUUID(in.ResponseMessage.ID)
	batchID := uuid.New()
	now := time.Now().UTC()
	res := &store.AnswerSetResult{BatchID: batchID.String()}
	for _, r := range rows {
		a, ok := requested[r.ID.String()]
		if !ok {
			continue
		}
		saved, err := client.DecisionAnswer.Create().
			SetDecisionRowID(r.ID).
			SetProjectID(r.ProjectID).
			SetAskerAgentID(r.AskerAgentID).
			SetKey(r.Key).
			SetVersion(r.Version).
			SetQuestionText(r.Question).
			SetChoiceID(a.ChoiceID).
			SetChoiceLabel(labels[a.DecisionRowID]).
			SetAnsweredByUserID(in.AnsweredByUserID).
			SetAnsweredByName(in.AnsweredByName).
			SetAnsweredByEmail(in.AnsweredByEmail).
			SetProvenance(decisionanswer.ProvenanceDirect).
			SetOutcome(decisionanswer.Outcome(outcomes[a.DecisionRowID])).
			SetSurface(decisionanswer.SurfaceNative).
			SetResponseMessageID(responseID).
			SetNillableIdempotencyKey(nullableString(in.IdempotencyKey)).
			SetBatchID(batchID).
			SetMatchedDefault(r.DefaultChoiceID != nil && *r.DefaultChoiceID == a.ChoiceID).
			SetCreatedAt(now).
			Save(ctx)
		if err != nil {
			return nil, markDecisionRace(err)
		}
		// Only the pending-to-decided transition sets decided_at. It must
		// change exactly the row read above; none changed means a
		// concurrent write won, and the whole set rolls back and retries.
		if r.Status == decision.StatusPending {
			n, err := client.Decision.Update().
				Where(decision.ID(r.ID), decision.StatusEQ(decision.StatusPending)).
				SetStatus(decision.StatusDecided).
				SetDecidedAt(now).
				Save(ctx)
			if err != nil {
				return nil, mapError(err)
			}
			if n != 1 {
				return nil, fmt.Errorf("%w: version %d of %s changed concurrently", errDecisionRace, r.Version, r.Key)
			}
		}
		res.Answers = append(res.Answers, answerToStore(saved))
	}
	return res, nil
}

// replayableAnswerSet returns the set recorded under in's idempotency key on
// the prompt's rows. ok is false when the key was not used there;
// ErrIdempotencyMismatch when it names other answers (other rows or
// choices, or another user).
func replayableAnswerSet(ctx context.Context, client *ent.Client, rows []*ent.Decision, in *store.RecordAnswerSetInput) (*store.AnswerSetResult, bool, error) {
	rowIDs := make([]uuid.UUID, len(rows))
	pos := make(map[uuid.UUID]int, len(rows))
	for i, r := range rows {
		rowIDs[i] = r.ID
		pos[r.ID] = i
	}
	found, err := client.DecisionAnswer.Query().
		Where(decisionanswer.DecisionRowIDIn(rowIDs...), decisionanswer.IdempotencyKey(in.IdempotencyKey), decisionanswer.BatchIDNotNil()).
		All(ctx)
	if err != nil {
		return nil, false, mapError(err)
	}
	if len(found) == 0 {
		return nil, false, nil
	}
	mismatch := fmt.Errorf("%w: the idempotency key was used for other answers", store.ErrIdempotencyMismatch)
	if len(found) != len(in.Answers) {
		return nil, false, mismatch
	}
	want := make(map[string]string, len(in.Answers))
	for _, a := range in.Answers {
		want[a.DecisionRowID] = a.ChoiceID
	}
	batch := *found[0].BatchID
	for _, f := range found {
		if f.AnsweredByUserID == nil || *f.AnsweredByUserID != in.AnsweredByUserID || f.BatchID == nil || *f.BatchID != batch {
			return nil, false, mismatch
		}
		if choice, ok := want[f.DecisionRowID.String()]; !ok || choice != f.ChoiceID {
			return nil, false, mismatch
		}
	}
	sort.Slice(found, func(i, j int) bool { return pos[found[i].DecisionRowID] < pos[found[j].DecisionRowID] })
	return &store.AnswerSetResult{BatchID: batch.String(), Answers: answersToStore(found), Replayed: true}, true, nil
}

// checkAnswerPreconditions enforces in the store what the answer endpoint
// checks before it calls RecordAnswer, so no caller can skip it: one
// answer per provenance; an agent_interpreted answer only to a version
// with no answer of either kind; a direct answer over an agent_interpreted
// one comes only from the user it records, as a confirm (same choice) or
// an override (another choice); every other answer is outcome answer.
func checkAnswerPreconditions(row *ent.Decision, existing []*ent.DecisionAnswer, in *store.RecordAnswerInput, outcome string) error {
	if in.Provenance == store.DecisionProvenanceAgentInterpreted && len(existing) > 0 {
		return fmt.Errorf("%w: version %d of %s is already answered", store.ErrDecisionAnswerConflict, row.Version, row.Key)
	}
	var interpreted *ent.DecisionAnswer
	for _, a := range existing {
		if string(a.Provenance) == in.Provenance {
			return fmt.Errorf("%w: version %d of %s already has a %s answer", store.ErrDecisionAnswerConflict, row.Version, row.Key, in.Provenance)
		}
		if a.Provenance == decisionanswer.ProvenanceAgentInterpreted {
			interpreted = a
		}
	}
	if in.Provenance != store.DecisionProvenanceDirect {
		if outcome != store.DecisionOutcomeAnswer {
			return fmt.Errorf("%w: an agent_interpreted answer is outcome answer, not %s", store.ErrInvalidInput, outcome)
		}
		return nil
	}
	if interpreted == nil {
		if outcome != store.DecisionOutcomeAnswer {
			return fmt.Errorf("%w: outcome %s needs an agent_interpreted answer to confirm or override", store.ErrDecisionOutcomeMismatch, outcome)
		}
		return nil
	}
	if interpreted.AnsweredByUserID == nil || *interpreted.AnsweredByUserID != in.AnsweredByUserID {
		return fmt.Errorf("%w: version %d of %s", store.ErrNotRecordedAnswerer, row.Version, row.Key)
	}
	want := store.DecisionOutcomeConfirm
	if interpreted.ChoiceID != in.ChoiceID {
		want = store.DecisionOutcomeOverride
	}
	if outcome != want {
		return fmt.Errorf("%w: an answer over the agent_interpreted one is outcome %s, not %s", store.ErrDecisionOutcomeMismatch, want, outcome)
	}
	return nil
}

func decisionToStore(r *ent.Decision) *store.Decision {
	d := &store.Decision{
		ID:             r.ID.String(),
		ProjectID:      r.ProjectID.String(),
		MessageID:      r.MessageID.String(),
		Position:       r.Position,
		AskerAgentID:   r.AskerAgentID.String(),
		AskerAgentName: r.AskerAgentName,
		Key:            r.Key,
		KeyNorm:        r.KeyNorm,
		KeySource:      string(r.KeySource),
		Version:        r.Version,
		Question:       r.Question,
		Choices:        r.Choices,
		IntroLength:    r.IntroLength,
		Status:         string(r.Status),
		CreatedAt:      r.CreatedAt,
		DecidedAt:      r.DecidedAt,
		ReplacedAt:     r.ReplacedAt,
	}
	if r.ConversationID != nil {
		d.ConversationID = r.ConversationID.String()
	}
	if r.DefaultChoiceID != nil {
		d.DefaultChoiceID = *r.DefaultChoiceID
	}
	if r.ReplacedByID != nil {
		d.ReplacedByID = r.ReplacedByID.String()
	}
	return d
}

func answersToStore(rows []*ent.DecisionAnswer) []*store.DecisionAnswer {
	if len(rows) == 0 {
		return nil
	}
	out := make([]*store.DecisionAnswer, len(rows))
	for i, r := range rows {
		out[i] = answerToStore(r)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

func answerToStore(r *ent.DecisionAnswer) *store.DecisionAnswer {
	a := &store.DecisionAnswer{
		ID:                r.ID.String(),
		DecisionRowID:     r.DecisionRowID.String(),
		ProjectID:         r.ProjectID.String(),
		AskerAgentID:      r.AskerAgentID.String(),
		Key:               r.Key,
		Version:           r.Version,
		QuestionText:      r.QuestionText,
		ChoiceID:          r.ChoiceID,
		ChoiceLabel:       r.ChoiceLabel,
		AnsweredByName:    r.AnsweredByName,
		AnsweredByEmail:   r.AnsweredByEmail,
		Provenance:        string(r.Provenance),
		Outcome:           string(r.Outcome),
		Surface:           string(r.Surface),
		ResponseMessageID: r.ResponseMessageID.String(),
		CreatedAt:         r.CreatedAt,
	}
	if r.AnsweredByUserID != nil {
		a.AnsweredByUserID = *r.AnsweredByUserID
	}
	if r.InterpretedByAgentID != nil {
		a.InterpretedByAgentID = r.InterpretedByAgentID.String()
	}
	if r.SourceMessageID != nil {
		a.SourceMessageID = r.SourceMessageID.String()
	}
	if r.AuthorizingUserID != nil {
		a.AuthorizingUserID = *r.AuthorizingUserID
	}
	if r.DelegationGrantID != nil {
		a.DelegationGrantID = *r.DelegationGrantID
	}
	if r.IdempotencyKey != nil {
		a.IdempotencyKey = *r.IdempotencyKey
	}
	if r.BatchID != nil {
		a.BatchID = r.BatchID.String()
	}
	a.MatchedDefault = r.MatchedDefault
	return a
}
