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
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/decision"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/decisionanswer"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/decisionkeycounter"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/predicate"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Decision prompt decisions and answers (ptone/scion#7514). The
// methods live on CompositeStore because each one writes a message and
// decision rows in one transaction.

// errDecisionRace marks a write that lost a race with a concurrent prompt
// or answer: a unique-index violation on decisions, decision_answers or
// decision_key_counters, or an active version replaced under us. The
// non-transactional entry points retry (CreateDecisionPrompt, up to
// maxDecisionRaceRetries times) or re-read (RecordAnswer) once on it.
var errDecisionRace = errors.New("concurrent decision write")

// maxDecisionRaceRetries bounds CreateDecisionPrompt's retries of a lost
// race: each retry reads the winner's rows, so n concurrent reposts of one
// key need up to n-1 retries for the last one.
const maxDecisionRaceRetries = 5

// markDecisionRace wraps a constraint violation as errDecisionRace, maps Ent
// field validation failures to store.ErrInvalidInput, and maps any other error
// as usual.
func markDecisionRace(err error) error {
	if ent.IsConstraintError(err) {
		return fmt.Errorf("%w: %v", errDecisionRace, err)
	}
	if ent.IsValidationError(err) {
		return fmt.Errorf("%w: %v", store.ErrInvalidInput, err)
	}
	return mapError(err)
}

// CreateDecisionPrompt implements store.DecisionStore.
func (c *CompositeStore) CreateDecisionPrompt(ctx context.Context, in *store.DecisionPromptInput) (*store.DecisionPromptResult, error) {
	if err := validateDecisionPromptInput(in); err != nil {
		return nil, err
	}
	if c.inTx {
		return c.createDecisionPromptTx(ctx, in)
	}
	var res *store.DecisionPromptResult
	for attempt := 0; ; attempt++ {
		err := c.WithTx(ctx, func(tx store.Store) error {
			var err error
			res, err = tx.(*CompositeStore).createDecisionPromptTx(ctx, in)
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

func validateDecisionPromptInput(in *store.DecisionPromptInput) error {
	if in == nil || in.Message == nil || in.Message.ID == "" || in.Message.ProjectID == "" {
		return fmt.Errorf("%w: a decision prompt needs a message with an ID and a project", store.ErrInvalidInput)
	}
	// Every ID is a UUID column; a malformed one is refused here, before
	// the transaction reserves numbers or replaces versions.
	for _, id := range []string{in.Message.ID, in.Message.ProjectID, in.AskerAgentID} {
		if _, err := parseUUID(id); err != nil {
			return err
		}
	}
	if len(in.Decisions) == 0 || len(in.Decisions) > messages.MaxDecisionsPerMessage {
		return fmt.Errorf("%w: a decision prompt carries 1 to %d decisions", store.ErrInvalidInput, messages.MaxDecisionsPerMessage)
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
		if d.Other != nil {
			if strings.TrimSpace(d.Other.Label) == "" {
				return fmt.Errorf("%w: decision %d has an empty other label", store.ErrInvalidInput, i)
			}
			if utf8.RuneCountInString(d.Other.Label) > messages.MaxChoiceLabelLen {
				return fmt.Errorf("%w: decision %d other label exceeds %d characters", store.ErrInvalidInput, i, messages.MaxChoiceLabelLen)
			}
			if utf8.RuneCountInString(d.Other.Description) > messages.MaxChoiceDescriptionLen {
				return fmt.Errorf("%w: decision %d other description exceeds %d characters", store.ErrInvalidInput, i, messages.MaxChoiceDescriptionLen)
			}
			if utf8.RuneCountInString(d.Other.Placeholder) > messages.MaxOtherPlaceholderLen {
				return fmt.Errorf("%w: decision %d other placeholder exceeds %d characters", store.ErrInvalidInput, i, messages.MaxOtherPlaceholderLen)
			}
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

// createDecisionPromptTx is CreateDecisionPrompt inside the transaction c runs
// in (c.client is the transaction's client).
func (c *CompositeStore) createDecisionPromptTx(ctx context.Context, in *store.DecisionPromptInput) (*store.DecisionPromptResult, error) {
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
				switch st.active.Status {
				case decision.StatusDecided:
					ek.Answer = effectiveAnswer(answers[st.active.ID])
				case decision.StatusDismissed:
					ek.Dismissal = dismissalAnswer(answers[st.active.ID])
				}
			}
			existing = append(existing, ek)
		}
		if len(existing) > 0 {
			return nil, &store.DecisionKeyExistsError{Existing: existing}
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
				var n int
				if store.DecisionStatusTerminal(string(st.active.Status)) {
					// A withdrawn or dismissed version keeps its terminal status when
					// --update re-asks the key as the next version, while replaced_by_id
					// links v1 -> v2 (design-8145 D7, design-8194 section 4.8).
					n, err = client.Decision.Update().
						Where(decision.ID(st.active.ID), decision.StatusIn(decision.StatusWithdrawn, decision.StatusDismissed), decision.ReplacedByIDIsNil()).
						SetReplacedAt(now).
						SetReplacedByID(rowIDs[i]).
						Save(ctx)
				} else {
					n, err = client.Decision.Update().
						Where(decision.ID(st.active.ID), decision.StatusNotIn(decision.StatusReplaced, decision.StatusWithdrawn, decision.StatusDismissed), decision.ReplacedByIDIsNil()).
						SetStatus(decision.StatusReplaced).
						SetReplacedAt(now).
						SetReplacedByID(rowIDs[i]).
						Save(ctx)
				}
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
	msgText, introLen := messages.RenderDecisionFallback(in.Intro, keyed, in.AnswerURL)
	in.Message.Msg = msgText
	in.Message.Type = messages.TypeDecisionPrompt
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
			SetNillableChatKey(nullableString(in.Message.ThreadID)).
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
		if d.Other != nil {
			cr.SetAllowOther(true).
				SetOtherLabel(d.Other.Label).
				SetNillableOtherDescription(nullableString(d.Other.Description)).
				SetNillableOtherPlaceholder(nullableString(d.Other.Placeholder))
		}
		if i == 0 {
			cr.SetIntroLength(introLen)
		}
		creates[i] = cr
	}
	if _, err := client.Decision.CreateBulk(creates...).Save(ctx); err != nil {
		return nil, markDecisionRace(err)
	}
	return &store.DecisionPromptResult{Decisions: results, IntroLength: introLen}, nil
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
			if r.Status != decision.StatusReplaced && r.ReplacedByID == nil {
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

func dismissalAnswer(rows []*ent.DecisionAnswer) *store.DecisionAnswer {
	d := &store.Decision{Answers: answersToStore(rows)}
	return d.Dismissal()
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
func (c *CompositeStore) GetDecisionAnswersByResponseMessageIDs(ctx context.Context, messageIDs []string) (map[string][]*store.DecisionAnswer, error) {
	out := map[string][]*store.DecisionAnswer{}
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
		id := r.ResponseMessageID.String()
		out[id] = append(out[id], answerToStore(r))
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
	if messages.IsReservedChoiceID(in.ChoiceID) && in.ChoiceID != messages.ChoiceIDOther {
		return fmt.Errorf("%w: choice %q is reserved", store.ErrInvalidInput, in.ChoiceID)
	}
	if _, verr := messages.NormalizeOtherText(in.ChoiceID, in.OtherText); verr != nil {
		return fmt.Errorf("%w: %s", store.ErrInvalidInput, verr.Error())
	}
	if err := decisionanswer.ProvenanceValidator(decisionanswer.Provenance(in.Provenance)); err != nil {
		return fmt.Errorf("%w: %v", store.ErrInvalidInput, err)
	}
	if in.Outcome != "" {
		if in.Outcome == store.DecisionOutcomeDismiss {
			return fmt.Errorf("%w: outcome %q is written only by DismissDecisions", store.ErrInvalidInput, in.Outcome)
		}
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
// same (choice_id, other_text), non-dismiss outcome, and same answerer.
func sameAnswer(row *ent.DecisionAnswer, in *store.RecordAnswerInput, otherText string) bool {
	if row.Outcome == decisionanswer.OutcomeDismiss {
		return false
	}
	if row.ChoiceID != in.ChoiceID {
		return false
	}
	rowOther := ""
	if row.OtherText != nil {
		rowOther = *row.OtherText
	}
	if rowOther != otherText {
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
	otherText, verr := messages.NormalizeOtherText(in.ChoiceID, in.OtherText)
	if verr != nil {
		return nil, false, fmt.Errorf("%w: %s", store.ErrInvalidInput, verr.Error())
	}
	rowID, err := parseGetID(in.DecisionRowID)
	if err != nil {
		return nil, false, err
	}
	row, err := c.client.DecisionAnswer.Query().
		Where(
			decisionanswer.DecisionRowID(rowID),
			decisionanswer.IdempotencyKey(in.IdempotencyKey),
			decisionanswer.OutcomeNEQ(decisionanswer.OutcomeDismiss),
		).
		Only(ctx)
	if ent.IsNotFound(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, mapError(err)
	}
	if !sameAnswer(row, in, otherText) {
		return nil, false, fmt.Errorf("%w: the idempotency key was used for a different answer", store.ErrDecisionAnswerConflict)
	}
	return answerToStore(row), true, nil
}

// recordAnswerTx is RecordAnswer inside the transaction c runs in.
func (c *CompositeStore) recordAnswerTx(ctx context.Context, in *store.RecordAnswerInput) (*store.DecisionAnswer, bool, error) {
	client := c.client
	otherText, verr := messages.NormalizeOtherText(in.ChoiceID, in.OtherText)
	if verr != nil {
		return nil, false, fmt.Errorf("%w: %s", store.ErrInvalidInput, verr.Error())
	}
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
	if store.DecisionStatusTerminal(string(row.Status)) {
		switch row.Status {
		case decision.StatusReplaced:
			return nil, false, fmt.Errorf("%w: version %d of %s was replaced", store.ErrDecisionReplaced, row.Version, row.Key)
		case decision.StatusDismissed:
			return nil, false, fmt.Errorf("%w: version %d of %s was dismissed", store.ErrDecisionDismissed, row.Version, row.Key)
		default:
			return nil, false, fmt.Errorf("%w: version %d of %s was withdrawn", store.ErrDecisionWithdrawn, row.Version, row.Key)
		}
	}
	var label string
	if in.ChoiceID == messages.ChoiceIDOther {
		if !row.AllowOther {
			return nil, false, fmt.Errorf("%w: %s has no choice %q", store.ErrInvalidInput, row.Key, in.ChoiceID)
		}
		if row.OtherLabel != nil && *row.OtherLabel != "" {
			label = *row.OtherLabel
		} else {
			label = messages.DefaultOtherLabel
		}
	} else {
		for _, ch := range row.Choices {
			if ch.ID == in.ChoiceID {
				label = ch.Label
			}
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
	if err := checkAnswerPreconditions(row, existing[rowID], in, otherText, outcome); err != nil {
		return nil, false, err
	}

	in.ResponseMessage.Type = messages.TypeDecisionResponse
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
		SetNillableOtherText(nullableString(otherText)).
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
		SetMatchedDefault(in.ChoiceID != messages.ChoiceIDOther && row.DefaultChoiceID != nil && *row.DefaultChoiceID == in.ChoiceID).
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
	if in.Surface != "" {
		if err := decisionanswer.SurfaceValidator(decisionanswer.Surface(in.Surface)); err != nil {
			return fmt.Errorf("%w: %v", store.ErrInvalidInput, err)
		}
	}
	seen := make(map[string]bool, len(in.Answers))
	for _, a := range in.Answers {
		if a.DecisionRowID == "" || a.ChoiceID == "" {
			return fmt.Errorf("%w: every answer needs a decision row id and a choice id", store.ErrInvalidInput)
		}
		if messages.IsReservedChoiceID(a.ChoiceID) && a.ChoiceID != messages.ChoiceIDOther {
			return fmt.Errorf("%w: choice %q is reserved", store.ErrInvalidInput, a.ChoiceID)
		}
		if _, verr := messages.NormalizeOtherText(a.ChoiceID, a.OtherText); verr != nil {
			return fmt.Errorf("%w: %s", store.ErrInvalidInput, verr.Error())
		}
		if seen[a.DecisionRowID] {
			return fmt.Errorf("%w: decision row %s is answered twice", store.ErrInvalidInput, a.DecisionRowID)
		}
		seen[a.DecisionRowID] = true
		if a.Outcome != "" {
			if a.Outcome == store.DecisionOutcomeDismiss {
				return fmt.Errorf("%w: outcome %q is written only by DismissDecisions", store.ErrInvalidInput, a.Outcome)
			}
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
	// Lock the prompt's rows on Postgres in the order a repost of a key
	// locks them (loadKeyStates: key, then version), so a set submit and a
	// repost never wait on each other in opposite orders; set submits and
	// one-click answers then serialize on these locks. The rows are put
	// back in position order after the read.
	q := client.Decision.Query().Where(decision.MessageID(msgID)).
		Order(ent.Asc(decision.FieldKeyNorm), ent.Asc(decision.FieldVersion))
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
	sort.Slice(rows, func(i, j int) bool { return rows[i].Position < rows[j].Position })
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
	otherTexts := make(map[string]string, len(in.Answers))
	for _, a := range in.Answers {
		r, ok := byID[a.DecisionRowID]
		if !ok {
			return nil, fmt.Errorf("%w: decision row %s is not in message %s", store.ErrInvalidInput, a.DecisionRowID, in.MessageID)
		}
		var label string
		if a.ChoiceID == messages.ChoiceIDOther {
			if !r.AllowOther {
				return nil, fmt.Errorf("%w: %s has no choice %q", store.ErrInvalidInput, r.Key, a.ChoiceID)
			}
			if r.OtherLabel != nil && *r.OtherLabel != "" {
				label = *r.OtherLabel
			} else {
				label = messages.DefaultOtherLabel
			}
		} else {
			for _, ch := range r.Choices {
				if ch.ID == a.ChoiceID {
					label = ch.Label
				}
			}
		}
		if label == "" {
			return nil, fmt.Errorf("%w: %s has no choice %q", store.ErrInvalidInput, r.Key, a.ChoiceID)
		}
		otherText, verr := messages.NormalizeOtherText(a.ChoiceID, a.OtherText)
		if verr != nil {
			return nil, fmt.Errorf("%w: %s", store.ErrInvalidInput, verr.Error())
		}
		labels[a.DecisionRowID] = label
		otherTexts[a.DecisionRowID] = otherText
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
		interpreted := false
		for _, e := range existing[r.ID] {
			if e.Provenance == decisionanswer.ProvenanceAgentInterpreted {
				interpreted = true
				want = store.DecisionOutcomeConfirm
				eOther := ""
				if e.OtherText != nil {
					eOther = *e.OtherText
				}
				if e.ChoiceID != a.ChoiceID || eOther != otherTexts[a.DecisionRowID] {
					want = store.DecisionOutcomeOverride
				}
			}
		}
		got := a.Outcome
		if got == "" {
			got = store.DecisionOutcomeAnswer
		}
		if got != want {
			// Over an interpreted answer the state changed after the
			// caller read it; on a row with none, a confirm or override
			// was never possible.
			if interpreted {
				reasons[a.DecisionRowID] = store.AnswerSetReasonDecided
			} else {
				reasons[a.DecisionRowID] = store.AnswerSetReasonNotActionable
			}
			continue
		}
		outcomes[a.DecisionRowID] = want
	}
	if len(reasons) > 0 {
		return nil, &store.AnswerSetChangedError{Reasons: reasons}
	}

	in.ResponseMessage.Type = messages.TypeDecisionResponse
	if err := c.CreateMessage(ctx, in.ResponseMessage); err != nil {
		return nil, err
	}
	responseID, _ := parseUUID(in.ResponseMessage.ID)
	batchID := uuid.New()
	now := time.Now().UTC()
	surface := in.Surface
	if surface == "" {
		surface = store.DecisionSurfaceNative
	}
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
			SetNillableOtherText(nullableString(otherTexts[a.DecisionRowID])).
			SetAnsweredByUserID(in.AnsweredByUserID).
			SetAnsweredByName(in.AnsweredByName).
			SetAnsweredByEmail(in.AnsweredByEmail).
			SetProvenance(decisionanswer.ProvenanceDirect).
			SetOutcome(decisionanswer.Outcome(outcomes[a.DecisionRowID])).
			SetSurface(decisionanswer.Surface(surface)).
			SetResponseMessageID(responseID).
			SetNillableIdempotencyKey(nullableString(in.IdempotencyKey)).
			SetBatchID(batchID).
			SetMatchedDefault(a.ChoiceID != messages.ChoiceIDOther && r.DefaultChoiceID != nil && *r.DefaultChoiceID == a.ChoiceID).
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

// replayableAnswerSet returns the set recorded under in's idempotency key by
// the same user on the prompt's rows (the key is scoped to the user and the
// prompt). ok is false when the user did not use the key there;
// ErrIdempotencyMismatch when it names other answers (other rows or
// choices). The per-row user check stays as a safety net.
func replayableAnswerSet(ctx context.Context, client *ent.Client, rows []*ent.Decision, in *store.RecordAnswerSetInput) (*store.AnswerSetResult, bool, error) {
	rowIDs := make([]uuid.UUID, len(rows))
	pos := make(map[uuid.UUID]int, len(rows))
	for i, r := range rows {
		rowIDs[i] = r.ID
		pos[r.ID] = i
	}
	found, err := client.DecisionAnswer.Query().
		Where(
			decisionanswer.DecisionRowIDIn(rowIDs...),
			decisionanswer.IdempotencyKey(in.IdempotencyKey),
			decisionanswer.BatchIDNotNil(),
			decisionanswer.AnsweredByUserID(in.AnsweredByUserID),
			decisionanswer.OutcomeNEQ(decisionanswer.OutcomeDismiss),
		).
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
	type setAnswerWant struct {
		choiceID  string
		otherText string
	}
	want := make(map[string]setAnswerWant, len(in.Answers))
	for _, a := range in.Answers {
		otherText, verr := messages.NormalizeOtherText(a.ChoiceID, a.OtherText)
		if verr != nil {
			return nil, false, fmt.Errorf("%w: %s", store.ErrInvalidInput, verr.Error())
		}
		want[a.DecisionRowID] = setAnswerWant{choiceID: a.ChoiceID, otherText: otherText}
	}
	batch := *found[0].BatchID
	for _, f := range found {
		if f.AnsweredByUserID == nil || *f.AnsweredByUserID != in.AnsweredByUserID || f.BatchID == nil || *f.BatchID != batch {
			return nil, false, mismatch
		}
		fOther := ""
		if f.OtherText != nil {
			fOther = *f.OtherText
		}
		if w, ok := want[f.DecisionRowID.String()]; !ok || w.choiceID != f.ChoiceID || w.otherText != fOther {
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
func checkAnswerPreconditions(row *ent.Decision, existing []*ent.DecisionAnswer, in *store.RecordAnswerInput, otherText, outcome string) error {
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
	interpretedOther := ""
	if interpreted.OtherText != nil {
		interpretedOther = *interpreted.OtherText
	}
	want := store.DecisionOutcomeConfirm
	if interpreted.ChoiceID != in.ChoiceID || interpretedOther != otherText {
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
		WithdrawnAt:    r.WithdrawnAt,
		DismissedAt:    r.DismissedAt,
	}
	if r.AllowOther {
		lbl := messages.DefaultOtherLabel
		if r.OtherLabel != nil && *r.OtherLabel != "" {
			lbl = *r.OtherLabel
		}
		opt := &messages.OtherOption{Label: lbl}
		if r.OtherDescription != nil {
			opt.Description = *r.OtherDescription
		}
		if r.OtherPlaceholder != nil {
			opt.Placeholder = *r.OtherPlaceholder
		}
		d.Other = opt
	}
	if r.WithdrawnReason != nil {
		d.WithdrawnReason = *r.WithdrawnReason
	}
	if r.ConversationID != nil {
		d.ConversationID = r.ConversationID.String()
	}
	if r.ChatKey != nil {
		d.ChatKey = *r.ChatKey
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
	if r.OtherText != nil {
		a.OtherText = *r.OtherText
	}
	if r.AnsweredByUserID != nil {
		a.AnsweredByUserID = *r.AnsweredByUserID
	}
	if r.DismissReason != nil {
		a.DismissReason = *r.DismissReason
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

type decisionListCursor struct {
	CreatedAt time.Time `json:"c"`
	ID        uuid.UUID `json:"id"`
}

func encodeDecisionListCursor(createdAt time.Time, id uuid.UUID) (string, error) {
	raw, err := json.Marshal(decisionListCursor{CreatedAt: createdAt.UTC(), ID: id})
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeDecisionListCursor(s string) (decisionListCursor, error) {
	var cur decisionListCursor
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return cur, fmt.Errorf("%w: invalid cursor", store.ErrInvalidInput)
	}
	if err := json.Unmarshal(raw, &cur); err != nil || cur.ID == uuid.Nil || cur.CreatedAt.IsZero() {
		return cur, fmt.Errorf("%w: invalid cursor", store.ErrInvalidInput)
	}
	return cur, nil
}

// ListAgentDecisions implements store.DecisionStore.
func (c *CompositeStore) ListAgentDecisions(ctx context.Context, agentID string, state store.DecisionListState, query string, cursor string, limit int) ([]*store.Decision, string, error) {
	agentUUID, err := parseGetID(agentID)
	if err != nil {
		return nil, "", err
	}
	if state == "" {
		state = store.DecisionListStatePending
	}
	if limit == 0 {
		limit = store.DefaultDecisionListLimit
	}
	if limit < 0 || limit > store.MaxDecisionListLimit {
		return nil, "", fmt.Errorf("%w: limit must be between 1 and %d", store.ErrInvalidInput, store.MaxDecisionListLimit)
	}

	q := c.client.Decision.Query().Where(decision.AskerAgentID(agentUUID))
	switch state {
	case store.DecisionListStatePending:
		q = q.Where(decision.StatusEQ(decision.StatusPending))
	case store.DecisionListStateDecided:
		q = q.Where(decision.StatusEQ(decision.StatusDecided))
	case store.DecisionListStateWithdrawn:
		q = q.Where(decision.StatusEQ(decision.StatusWithdrawn))
	case store.DecisionListStateDismissed:
		q = q.Where(decision.StatusEQ(decision.StatusDismissed))
	case store.DecisionListStateAll:
		// All statuses (pending, decided, replaced, withdrawn, dismissed).
	default:
		return nil, "", fmt.Errorf("%w: invalid decision state %q", store.ErrInvalidInput, state)
	}
	if query != "" {
		q = q.Where(agentDecisionMatchesQuery(query))
	}

	if cursor != "" {
		cur, err := decodeDecisionListCursor(cursor)
		if err != nil {
			return nil, "", err
		}
		q = q.Where(decision.Or(
			decision.CreatedAtLT(cur.CreatedAt),
			decision.And(
				decision.CreatedAtEQ(cur.CreatedAt),
				decision.IDLT(cur.ID),
			),
		))
	}

	rows, err := q.Order(ent.Desc(decision.FieldCreatedAt), ent.Desc(decision.FieldID)).
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

	rowIDs := make([]uuid.UUID, len(rows))
	for i, r := range rows {
		rowIDs[i] = r.ID
	}
	answers, err := loadAnswers(ctx, c.client, rowIDs)
	if err != nil {
		return nil, "", err
	}

	items := make([]*store.Decision, len(rows))
	for i, r := range rows {
		d := decisionToStore(r)
		d.Answers = answersToStore(answers[r.ID])
		items[i] = d
	}
	return items, nextCursor, nil
}

// agentDecisionMatchesQuery keeps decisions whose question text, key, or
// effective answer's choice label contains q case-insensitively (D12).
// ContainsFold escapes %, _ and \, so q matches literally (ILIKE on Postgres,
// LOWER() LIKE on SQLite). There is no conversation label or agent name arm:
// every row already belongs to the calling agent.
func agentDecisionMatchesQuery(q string) predicate.Decision {
	return decision.Or(
		decision.QuestionContainsFold(q),
		decision.KeyContainsFold(q),
		effectiveAnswerLabelContainsFold(q),
		dismissalReasonContainsFold(q),
	)
}

func dismissalReasonContainsFold(q string) predicate.Decision {
	return func(s *entsql.Selector) {
		ta := entsql.Table(decisionanswer.Table).As("da_dsm")
		s.Where(entsql.And(
			entsql.EQ(s.C(decision.FieldStatus), decision.StatusDismissed),
			entsql.Exists(
				entsql.Select(ta.C(decisionanswer.FieldID)).
					From(ta).
					Where(entsql.And(
						entsql.ColumnsEQ(ta.C(decisionanswer.FieldDecisionRowID), s.C(decision.FieldID)),
						entsql.EQ(ta.C(decisionanswer.FieldOutcome), decisionanswer.OutcomeDismiss),
						entsql.ContainsFold(ta.C(decisionanswer.FieldDismissReason), q),
					)),
			),
		))
	}
}

// effectiveAnswerLabelContainsFold matches decisions whose effective answer
// (Decision.EffectiveAnswer: the direct answer when present, otherwise the
// agent-interpreted answer, skipping dismissed decisions and outcome=dismiss
// rows) has a choice_label or other_text containing q case-insensitively.
func effectiveAnswerLabelContainsFold(q string) predicate.Decision {
	return func(s *entsql.Selector) {
		ta := entsql.Table(decisionanswer.Table).As("da_eff")
		tDir := entsql.Table(decisionanswer.Table).As("da_dir")
		noDirect := entsql.NotExists(
			entsql.Select(tDir.C(decisionanswer.FieldID)).
				From(tDir).
				Where(entsql.And(
					entsql.ColumnsEQ(tDir.C(decisionanswer.FieldDecisionRowID), s.C(decision.FieldID)),
					entsql.EQ(tDir.C(decisionanswer.FieldProvenance), decisionanswer.ProvenanceDirect),
				)),
		)
		s.Where(entsql.And(
			entsql.NEQ(s.C(decision.FieldStatus), decision.StatusDismissed),
			entsql.Exists(
				entsql.Select(ta.C(decisionanswer.FieldID)).
					From(ta).
					Where(entsql.And(
						entsql.ColumnsEQ(ta.C(decisionanswer.FieldDecisionRowID), s.C(decision.FieldID)),
						entsql.NEQ(ta.C(decisionanswer.FieldOutcome), decisionanswer.OutcomeDismiss),
						entsql.Or(
							entsql.ContainsFold(ta.C(decisionanswer.FieldChoiceLabel), q),
							entsql.ContainsFold(ta.C(decisionanswer.FieldOtherText), q),
						),
						entsql.Or(
							entsql.EQ(ta.C(decisionanswer.FieldProvenance), decisionanswer.ProvenanceDirect),
							entsql.And(
								entsql.EQ(ta.C(decisionanswer.FieldProvenance), decisionanswer.ProvenanceAgentInterpreted),
								noDirect,
							),
						),
					)),
			),
		))
	}
}

// GetAgentDecisionVersions implements store.DecisionStore.
func (c *CompositeStore) GetAgentDecisionVersions(ctx context.Context, agentID, key string) ([]*store.Decision, error) {
	agentUUID, err := parseGetID(agentID)
	if err != nil {
		return nil, err
	}
	if key == "" {
		return nil, fmt.Errorf("%w: decision key is empty", store.ErrNotFound)
	}
	norm := messages.DecisionKeyNorm(key)
	rows, err := c.client.Decision.Query().
		Where(decision.AskerAgentID(agentUUID), decision.KeyNormEQ(norm)).
		Order(ent.Asc(decision.FieldVersion)).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("%w: decision %q not found", store.ErrNotFound, key)
	}

	rowIDs := make([]uuid.UUID, len(rows))
	for i, r := range rows {
		rowIDs[i] = r.ID
	}
	answers, err := loadAnswers(ctx, c.client, rowIDs)
	if err != nil {
		return nil, err
	}

	out := make([]*store.Decision, len(rows))
	for i, r := range rows {
		d := decisionToStore(r)
		d.Answers = answersToStore(answers[r.ID])
		out[i] = d
	}
	return out, nil
}

// FindInterpretedReplay implements store.DecisionStore.
func (c *CompositeStore) FindInterpretedReplay(ctx context.Context, agentID, idempotencyKey, sourceMessageID string, pairs []store.InterpretedAnswerPair) (*store.AnswerSetResult, error) {
	if idempotencyKey == "" {
		return nil, nil
	}
	if len(idempotencyKey) > store.MaxDecisionIdempotencyKeyLen {
		return nil, fmt.Errorf("%w: the idempotency key is at most %d bytes", store.ErrInvalidInput, store.MaxDecisionIdempotencyKeyLen)
	}
	agentUUID, err := parseGetID(agentID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	res, ok, err := replayableInterpretedAnswers(ctx, c.client, agentUUID, idempotencyKey, sourceMessageID, pairs)
	if err != nil || !ok {
		return nil, err
	}
	return res, nil
}

// RecordInterpretedAnswers implements store.DecisionStore.
func (c *CompositeStore) RecordInterpretedAnswers(ctx context.Context, in *store.RecordInterpretedInput) (*store.AnswerSetResult, error) {
	if err := validateRecordInterpretedInput(in); err != nil {
		return nil, err
	}
	if c.inTx {
		return c.recordInterpretedAnswersTx(ctx, in)
	}
	origMsg := in.ResponseMessage.Msg
	var res *store.AnswerSetResult
	for attempt := 0; ; attempt++ {
		in.ResponseMessage.Msg = origMsg
		err := c.WithTx(ctx, func(tx store.Store) error {
			var err error
			res, err = tx.(*CompositeStore).recordInterpretedAnswersTx(ctx, in)
			return err
		})
		if err == nil {
			return res, nil
		}
		if attempt < maxDecisionRaceRetries && (errors.Is(err, errDecisionRace) || errors.Is(err, store.ErrTransient)) {
			continue
		}
		if errors.Is(err, errDecisionRace) {
			return nil, store.ErrTransient
		}
		return nil, err
	}
}

func validateRecordInterpretedInput(in *store.RecordInterpretedInput) error {
	if in == nil || in.ResponseMessage == nil || in.ResponseMessage.ID == "" || in.ResponseMessage.ProjectID == "" {
		return fmt.Errorf("%w: interpreted answers need a response message with an ID and a project", store.ErrInvalidInput)
	}
	for _, id := range []string{in.InterpretedByAgentID, in.SourceMessageID, in.ResponseMessage.ID, in.ResponseMessage.ProjectID} {
		if _, err := parseUUID(id); err != nil {
			return err
		}
	}
	if in.AnsweredByUserID == "" {
		return fmt.Errorf("%w: interpreted answers need the answering user", store.ErrInvalidInput)
	}
	if err := decisionanswer.SurfaceValidator(decisionanswer.Surface(in.Surface)); err != nil {
		return fmt.Errorf("%w: %v", store.ErrInvalidInput, err)
	}
	if len(in.Answers) == 0 || len(in.Answers) > messages.MaxDecisionsPerMessage {
		return fmt.Errorf("%w: interpreted answers require 1 to %d pairs", store.ErrInvalidInput, messages.MaxDecisionsPerMessage)
	}
	if len(in.IdempotencyKey) > store.MaxDecisionIdempotencyKeyLen {
		return fmt.Errorf("%w: the idempotency key is at most %d bytes", store.ErrInvalidInput, store.MaxDecisionIdempotencyKeyLen)
	}
	seenKeys := make(map[string]bool, len(in.Answers))
	seenRows := make(map[string]bool, len(in.Answers))
	for _, a := range in.Answers {
		if a.ChoiceID == "" || (a.Key == "" && a.DecisionRowID == "") {
			return fmt.Errorf("%w: every pair needs a decision key or row id and a choice id", store.ErrInvalidInput)
		}
		if messages.IsReservedChoiceID(a.ChoiceID) && a.ChoiceID != messages.ChoiceIDOther {
			return fmt.Errorf("%w: choice %q is reserved", store.ErrInvalidInput, a.ChoiceID)
		}
		if _, verr := messages.NormalizeOtherText(a.ChoiceID, a.OtherText); verr != nil {
			return fmt.Errorf("%w: %s", store.ErrInvalidInput, verr.Error())
		}
		if a.DecisionRowID != "" {
			if _, err := parseUUID(a.DecisionRowID); err != nil {
				return err
			}
			if seenRows[a.DecisionRowID] {
				return fmt.Errorf("%w: duplicate decision row %s", store.ErrInvalidInput, a.DecisionRowID)
			}
			seenRows[a.DecisionRowID] = true
		}
		if a.Key != "" {
			norm := messages.DecisionKeyNorm(a.Key)
			if seenKeys[norm] {
				return fmt.Errorf("%w: duplicate decision key %q", store.ErrInvalidInput, a.Key)
			}
			seenKeys[norm] = true
		}
	}
	return nil
}

type validatedInterpretedItem struct {
	row         *ent.Decision
	choiceID    string
	choiceLabel string
	otherText   string
}

func compareEntDecisionOrder(ri, rj *ent.Decision) bool {
	if ri.MessageID != rj.MessageID {
		if !ri.CreatedAt.Equal(rj.CreatedAt) {
			return ri.CreatedAt.Before(rj.CreatedAt)
		}
		return ri.MessageID.String() < rj.MessageID.String()
	}
	return ri.Position < rj.Position
}

func (c *CompositeStore) recordInterpretedAnswersTx(ctx context.Context, in *store.RecordInterpretedInput) (*store.AnswerSetResult, error) {
	client := c.client
	agentUUID, _ := parseUUID(in.InterpretedByAgentID)
	sourceUUID, _ := parseUUID(in.SourceMessageID)

	// 1. Check idempotency scoped to (InterpretedByAgentID, IdempotencyKey)
	// before row-status checks so retries after confirm/override/repost still
	// replay cleanly and reused keys with different inputs return 422.
	if in.IdempotencyKey != "" {
		if res, ok, err := replayableInterpretedAnswers(ctx, client, agentUUID, in.IdempotencyKey, in.SourceMessageID, in.Answers); err != nil || ok {
			return res, err
		}
	}

	// 2. Load and lock all target decision rows on Postgres in (key_norm,
	// version) order — the exact lock order of CreateDecisionPrompt and
	// RecordAnswerSet — so concurrent reposts, user answers, and interpreted
	// records serialize without deadlocking.
	var explicitUIDs []uuid.UUID
	var norms []string
	for _, a := range in.Answers {
		if a.DecisionRowID != "" {
			uid, _ := parseUUID(a.DecisionRowID)
			explicitUIDs = append(explicitUIDs, uid)
		}
		if a.Key != "" {
			norms = append(norms, messages.DecisionKeyNorm(a.Key))
		}
	}
	var matchPreds []predicate.Decision
	if len(explicitUIDs) > 0 {
		matchPreds = append(matchPreds, decision.IDIn(explicitUIDs...))
	}
	if len(norms) > 0 {
		matchPreds = append(matchPreds, decision.KeyNormIn(norms...))
	}
	q := client.Decision.Query().
		Where(decision.AskerAgentID(agentUUID), decision.Or(matchPreds...)).
		Order(ent.Asc(decision.FieldKeyNorm), ent.Asc(decision.FieldVersion))
	if c.isPostgres() {
		q = q.ForUpdate()
	}
	rows, err := q.All(ctx)
	if err != nil {
		return nil, mapError(err)
	}

	byID := make(map[string]*ent.Decision, len(rows))
	latestByNorm := make(map[string]*ent.Decision, len(rows))
	activeByNorm := make(map[string]*ent.Decision, len(rows))
	rowIDs := make([]uuid.UUID, len(rows))
	for i, r := range rows {
		byID[r.ID.String()] = r
		rowIDs[i] = r.ID
		if prev := latestByNorm[r.KeyNorm]; prev == nil || r.Version > prev.Version {
			latestByNorm[r.KeyNorm] = r
			if r.Status != decision.StatusReplaced && r.ReplacedByID == nil {
				activeByNorm[r.KeyNorm] = r
			} else {
				delete(activeByNorm, r.KeyNorm)
			}
		}
	}

	existing, err := loadAnswers(ctx, client, rowIDs)
	if err != nil {
		return nil, err
	}

	// 3. Validate all requested pairs before writing anything (all-or-nothing).
	var failures []store.InterpretedKeyFailure
	validated := make([]validatedInterpretedItem, 0, len(in.Answers))
	for _, a := range in.Answers {
		var row *ent.Decision
		if a.DecisionRowID != "" {
			row = byID[a.DecisionRowID]
		} else {
			norm := messages.DecisionKeyNorm(a.Key)
			row = activeByNorm[norm]
			if row == nil {
				row = latestByNorm[norm]
			}
		}
		dispKey := a.Key
		if row != nil {
			dispKey = row.Key
		} else if dispKey == "" {
			dispKey = a.DecisionRowID
		}
		if row == nil {
			failures = append(failures, store.InterpretedKeyFailure{
				Key:    dispKey,
				Reason: store.InterpretedReasonNotFound,
			})
			continue
		}
		dStore := decisionToStore(row)
		dStore.Answers = answersToStore(existing[row.ID])
		if store.DecisionStatusTerminal(string(row.Status)) {
			reason := store.InterpretedReasonWithdrawn
			switch row.Status {
			case decision.StatusReplaced:
				reason = store.InterpretedReasonReplaced
			case decision.StatusDismissed:
				reason = store.InterpretedReasonDismissed
			}
			failures = append(failures, store.InterpretedKeyFailure{
				Key:      dispKey,
				Reason:   reason,
				Decision: dStore,
			})
			continue
		}
		if len(existing[row.ID]) > 0 || row.Status != decision.StatusPending {
			failures = append(failures, store.InterpretedKeyFailure{
				Key:      dispKey,
				Reason:   store.InterpretedReasonAlreadyAnswered,
				Decision: dStore,
			})
			continue
		}
		otherText, verr := messages.NormalizeOtherText(a.ChoiceID, a.OtherText)
		if verr != nil {
			return nil, fmt.Errorf("%w: %s", store.ErrInvalidInput, verr.Error())
		}
		label := ""
		if a.ChoiceID == messages.ChoiceIDOther {
			if row.AllowOther {
				if row.OtherLabel != nil && *row.OtherLabel != "" {
					label = *row.OtherLabel
				} else {
					label = messages.DefaultOtherLabel
				}
			}
		} else {
			for _, ch := range row.Choices {
				if ch.ID == a.ChoiceID {
					label = ch.Label
					break
				}
			}
		}
		if label == "" {
			failures = append(failures, store.InterpretedKeyFailure{
				Key:      dispKey,
				Reason:   store.InterpretedReasonInvalidChoice,
				ChoiceID: a.ChoiceID,
				Decision: dStore,
			})
			continue
		}
		validated = append(validated, validatedInterpretedItem{
			row:         row,
			choiceID:    a.ChoiceID,
			choiceLabel: label,
			otherText:   otherText,
		})
	}
	if len(failures) > 0 {
		// On Postgres READ COMMITTED, two concurrent identical retries can both
		// miss the pre-lock check; once the winner commits and releases FOR
		// UPDATE locks, re-check idempotency here so the retry gets 200 replayed
		// (or 422 on mismatch) rather than 409 already_answered.
		if in.IdempotencyKey != "" {
			if res, ok, err := replayableInterpretedAnswers(ctx, client, agentUUID, in.IdempotencyKey, in.SourceMessageID, in.Answers); err != nil || ok {
				return res, err
			}
		}
		return nil, &store.InterpretedAnswersError{Failures: failures}
	}

	// Sort validated pairs into canonical prompt/position order so stored
	// response text, live SSE DecisionResponseSetView, history hydration, and
	// idempotency replay all share one deterministic order.
	sort.SliceStable(validated, func(i, j int) bool {
		return compareEntDecisionOrder(validated[i].row, validated[j].row)
	})
	formatted := make([]store.InterpretedFormattedItem, len(validated))
	for i, v := range validated {
		formatted[i] = store.InterpretedFormattedItem{
			Key:         v.row.Key,
			ChoiceID:    v.choiceID,
			ChoiceLabel: v.choiceLabel,
			OtherText:   v.otherText,
		}
	}

	// 4. Insert the single decision-response message, the DecisionAnswer rows,
	// and mark each Decision row decided.
	if in.FormatMessage != nil && in.ResponseMessage.Msg == "" {
		in.ResponseMessage.Msg = in.FormatMessage(formatted)
	}
	in.ResponseMessage.Type = messages.TypeDecisionResponse
	if err := c.CreateMessage(ctx, in.ResponseMessage); err != nil {
		return nil, err
	}
	responseUUID, _ := parseUUID(in.ResponseMessage.ID)

	var batchUUID *uuid.UUID
	batchStr := ""
	if len(validated) >= 2 {
		b := uuid.New()
		batchUUID = &b
		batchStr = b.String()
	}

	now := time.Now().UTC()
	res := &store.AnswerSetResult{
		BatchID: batchStr,
		Answers: make([]*store.DecisionAnswer, 0, len(validated)),
	}
	for _, v := range validated {
		saved, err := client.DecisionAnswer.Create().
			SetDecisionRowID(v.row.ID).
			SetProjectID(v.row.ProjectID).
			SetAskerAgentID(v.row.AskerAgentID).
			SetKey(v.row.Key).
			SetVersion(v.row.Version).
			SetQuestionText(v.row.Question).
			SetChoiceID(v.choiceID).
			SetChoiceLabel(v.choiceLabel).
			SetNillableOtherText(nullableString(v.otherText)).
			SetNillableAnsweredByUserID(nullableString(in.AnsweredByUserID)).
			SetAnsweredByName(in.AnsweredByName).
			SetAnsweredByEmail(in.AnsweredByEmail).
			SetProvenance(decisionanswer.ProvenanceAgentInterpreted).
			SetOutcome(decisionanswer.OutcomeAnswer).
			SetInterpretedByAgentID(agentUUID).
			SetSourceMessageID(sourceUUID).
			SetSurface(decisionanswer.Surface(in.Surface)).
			SetResponseMessageID(responseUUID).
			SetNillableIdempotencyKey(nullableString(in.IdempotencyKey)).
			SetNillableBatchID(batchUUID).
			SetMatchedDefault(v.choiceID != messages.ChoiceIDOther && v.row.DefaultChoiceID != nil && *v.row.DefaultChoiceID == v.choiceID).
			SetCreatedAt(now).
			Save(ctx)
		if err != nil {
			return nil, markDecisionRace(err)
		}
		n, err := client.Decision.Update().
			Where(decision.ID(v.row.ID), decision.StatusEQ(decision.StatusPending)).
			SetStatus(decision.StatusDecided).
			SetDecidedAt(now).
			Save(ctx)
		if err != nil {
			return nil, mapError(err)
		}
		if n != 1 {
			return nil, fmt.Errorf("%w: version %d of %s changed concurrently", errDecisionRace, v.row.Version, v.row.Key)
		}
		res.Answers = append(res.Answers, answerToStore(saved))
	}
	return res, nil
}

// replayableInterpretedAnswers returns the answers recorded under
// idempotencyKey by agentUUID when every target pair in pairs and
// sourceMessageID match, ordered canonically by (prompt CreatedAt, MessageID,
// Position). Returns ErrIdempotencyMismatch when the key was already used by
// the agent for different answers or a different source message.
func replayableInterpretedAnswers(ctx context.Context, client *ent.Client, agentUUID uuid.UUID, idempotencyKey, sourceMessageID string, pairs []store.InterpretedAnswerPair) (*store.AnswerSetResult, bool, error) {
	found, err := client.DecisionAnswer.Query().
		Where(
			decisionanswer.InterpretedByAgentID(agentUUID),
			decisionanswer.IdempotencyKey(idempotencyKey),
		).
		All(ctx)
	if err != nil {
		return nil, false, mapError(err)
	}
	if len(found) == 0 {
		return nil, false, nil
	}
	mismatch := fmt.Errorf("%w: the idempotency key was used for different interpreted answers", store.ErrIdempotencyMismatch)
	sourceUUID, serr := parseUUID(sourceMessageID)
	if serr != nil {
		return nil, false, mismatch
	}
	if len(found) != len(pairs) {
		return nil, false, mismatch
	}

	respID := found[0].ResponseMessageID
	var batchStr string
	if found[0].BatchID != nil {
		batchStr = found[0].BatchID.String()
	}
	if (len(found) >= 2) != (batchStr != "") {
		return nil, false, mismatch
	}

	byNorm := make(map[string]*ent.DecisionAnswer, len(found))
	byRowID := make(map[string]*ent.DecisionAnswer, len(found))
	foundRowIDs := make([]uuid.UUID, len(found))
	for i, f := range found {
		if f.Provenance != decisionanswer.ProvenanceAgentInterpreted {
			return nil, false, mismatch
		}
		if f.SourceMessageID == nil || *f.SourceMessageID != sourceUUID {
			return nil, false, mismatch
		}
		if f.ResponseMessageID != respID {
			return nil, false, mismatch
		}
		gotBatch := ""
		if f.BatchID != nil {
			gotBatch = f.BatchID.String()
		}
		if gotBatch != batchStr {
			return nil, false, mismatch
		}
		byNorm[messages.DecisionKeyNorm(f.Key)] = f
		byRowID[f.DecisionRowID.String()] = f
		foundRowIDs[i] = f.DecisionRowID
	}

	matched := make(map[uuid.UUID]bool, len(found))
	ordered := make([]*store.DecisionAnswer, len(pairs))
	for i, a := range pairs {
		var f *ent.DecisionAnswer
		if a.Key != "" {
			f = byNorm[messages.DecisionKeyNorm(a.Key)]
		} else {
			f = byRowID[a.DecisionRowID]
		}
		if f == nil || matched[f.ID] || f.ChoiceID != a.ChoiceID {
			return nil, false, mismatch
		}
		wantOther, verr := messages.NormalizeOtherText(a.ChoiceID, a.OtherText)
		if verr != nil {
			return nil, false, mismatch
		}
		gotOther := ""
		if f.OtherText != nil {
			gotOther = *f.OtherText
		}
		if gotOther != wantOther {
			return nil, false, mismatch
		}
		if a.DecisionRowID != "" && f.DecisionRowID.String() != a.DecisionRowID && a.Key == "" {
			return nil, false, mismatch
		}
		matched[f.ID] = true
		ordered[i] = answerToStore(f)
	}

	dRows, derr := client.Decision.Query().
		Where(decision.IDIn(foundRowIDs...)).
		All(ctx)
	if derr != nil {
		return nil, false, mapError(derr)
	}
	rowsByID := make(map[string]*ent.Decision, len(dRows))
	for _, r := range dRows {
		rowsByID[r.ID.String()] = r
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		ri, rj := rowsByID[ordered[i].DecisionRowID], rowsByID[ordered[j].DecisionRowID]
		if ri != nil && rj != nil {
			return compareEntDecisionOrder(ri, rj)
		}
		return ordered[i].ID < ordered[j].ID
	})

	return &store.AnswerSetResult{
		BatchID:  batchStr,
		Answers:  ordered,
		Replayed: true,
	}, true, nil
}

// WithdrawDecisions implements store.DecisionStore.
func (c *CompositeStore) WithdrawDecisions(ctx context.Context, in *store.WithdrawDecisionsInput) (*store.WithdrawDecisionsResult, error) {
	if err := validateWithdrawDecisionsInput(in); err != nil {
		return nil, err
	}
	if c.inTx {
		return c.withdrawDecisionsTx(ctx, in)
	}
	var res *store.WithdrawDecisionsResult
	for attempt := 0; ; attempt++ {
		err := c.WithTx(ctx, func(tx store.Store) error {
			var err error
			res, err = tx.(*CompositeStore).withdrawDecisionsTx(ctx, in)
			return err
		})
		if err == nil {
			return res, nil
		}
		if attempt < maxDecisionRaceRetries && (errors.Is(err, errDecisionRace) || errors.Is(err, store.ErrTransient)) {
			continue
		}
		if errors.Is(err, errDecisionRace) {
			return nil, store.ErrTransient
		}
		return nil, err
	}
}

func validateWithdrawDecisionsInput(in *store.WithdrawDecisionsInput) error {
	if in == nil {
		return fmt.Errorf("%w: withdraw input is required", store.ErrInvalidInput)
	}
	if _, err := parseUUID(in.AskerAgentID); err != nil {
		return err
	}
	if len(in.Keys) == 0 || len(in.Keys) > messages.MaxDecisionsPerMessage {
		return fmt.Errorf("%w: withdraw requires 1 to %d keys", store.ErrInvalidInput, messages.MaxDecisionsPerMessage)
	}
	seenNorms := make(map[string]bool, len(in.Keys))
	for _, rawKey := range in.Keys {
		k := strings.TrimSpace(rawKey)
		if k == "" {
			return fmt.Errorf("%w: decision key must be non-empty", store.ErrInvalidInput)
		}
		norm := messages.DecisionKeyNorm(k)
		if seenNorms[norm] {
			return fmt.Errorf("%w: duplicate decision key %q", store.ErrInvalidInput, k)
		}
		seenNorms[norm] = true
	}
	reason := strings.TrimSpace(in.Reason)
	if utf8.RuneCountInString(reason) > store.MaxDecisionWithdrawReasonRunes {
		return fmt.Errorf("%w: reason is at most %d characters", store.ErrInvalidInput, store.MaxDecisionWithdrawReasonRunes)
	}
	return nil
}

func (c *CompositeStore) withdrawDecisionsTx(ctx context.Context, in *store.WithdrawDecisionsInput) (*store.WithdrawDecisionsResult, error) {
	client := c.client
	agentUUID, _ := parseUUID(in.AskerAgentID)
	reason := strings.TrimSpace(in.Reason)

	trimmedKeys := make([]string, len(in.Keys))
	norms := make([]string, len(in.Keys))
	normSet := make(map[string]bool, len(in.Keys))
	for i, k := range in.Keys {
		trimmed := strings.TrimSpace(k)
		trimmedKeys[i] = trimmed
		norms[i] = messages.DecisionKeyNorm(trimmed)
		normSet[norms[i]] = true
	}

	// With WholeSet, peek the prompt message IDs of the requested keys' active
	// versions so one FOR UPDATE query locks both the named keys and their
	// sibling rows in canonical (key_norm, version) order (never deadlocking
	// with RecordAnswerSet, CreateDecisionPrompt, or RecordInterpretedAnswers).
	var peekedMsgIDs []uuid.UUID
	peekedMsgSet := map[uuid.UUID]bool{}
	if in.WholeSet {
		peekRows, err := client.Decision.Query().
			Where(decision.AskerAgentID(agentUUID), decision.KeyNormIn(norms...), decision.ReplacedByIDIsNil()).
			All(ctx)
		if err != nil {
			return nil, mapError(err)
		}
		for _, r := range peekRows {
			if !peekedMsgSet[r.MessageID] {
				peekedMsgSet[r.MessageID] = true
				peekedMsgIDs = append(peekedMsgIDs, r.MessageID)
			}
		}
	}

	// Lock target rows on Postgres in (key_norm, version) order — matching
	// CreateDecisionPrompt, RecordAnswerSet, and RecordInterpretedAnswers —
	// so concurrent answers, reposts, dismissals, and withdrawals serialize cleanly.
	matchPred := decision.KeyNormIn(norms...)
	if len(peekedMsgIDs) > 0 {
		matchPred = decision.Or(decision.KeyNormIn(norms...), decision.MessageIDIn(peekedMsgIDs...))
	}
	q := client.Decision.Query().
		Where(decision.AskerAgentID(agentUUID), matchPred).
		Order(ent.Asc(decision.FieldKeyNorm), ent.Asc(decision.FieldVersion))
	if c.isPostgres() {
		q = q.ForUpdate()
	}
	rows, err := q.All(ctx)
	if err != nil {
		return nil, mapError(err)
	}

	latestByNorm := make(map[string]*ent.Decision, len(norms))
	activeByNorm := make(map[string]*ent.Decision, len(norms))
	siblingsByMsg := make(map[uuid.UUID][]*ent.Decision)
	candidateIDs := make([]uuid.UUID, 0, len(rows))
	for _, r := range rows {
		if normSet[r.KeyNorm] {
			if prev := latestByNorm[r.KeyNorm]; prev == nil || r.Version > prev.Version {
				latestByNorm[r.KeyNorm] = r
				if r.Status != decision.StatusReplaced && r.ReplacedByID == nil {
					activeByNorm[r.KeyNorm] = r
				} else {
					delete(activeByNorm, r.KeyNorm)
				}
			}
		}
		if in.WholeSet {
			siblingsByMsg[r.MessageID] = append(siblingsByMsg[r.MessageID], r)
		}
		if r.Status != decision.StatusReplaced && r.ReplacedByID == nil {
			candidateIDs = append(candidateIDs, r.ID)
		}
	}

	// Under Postgres READ COMMITTED, if a concurrent --update replaced v1 with
	// v2 while FOR UPDATE waited on v1, EvalPlanQual returns updated v1 without
	// scanning newly inserted v2. Retry so the next transaction snapshots and
	// locks v2 (and its prompt's siblings when WholeSet is set).
	for _, norm := range norms {
		latest := latestByNorm[norm]
		if latest != nil && activeByNorm[norm] == nil && (latest.Status == decision.StatusReplaced || latest.ReplacedByID != nil) {
			return nil, fmt.Errorf("%w: %s was replaced concurrently", errDecisionRace, latest.Key)
		}
		if in.WholeSet {
			if active := activeByNorm[norm]; active != nil && !peekedMsgSet[active.MessageID] {
				return nil, fmt.Errorf("%w: %s moved to a newer prompt concurrently", errDecisionRace, active.Key)
			}
		}
	}

	existingAnswers, err := loadAnswers(ctx, client, candidateIDs)
	if err != nil {
		return nil, err
	}

	var failures []store.WithdrawKeyFailure
	var pendingRows []*ent.Decision
	var alreadyRows []*ent.Decision
	seenPending := make(map[uuid.UUID]bool, len(rows))
	seenAlready := make(map[uuid.UUID]bool, len(rows))
	var resolvedPromptIDs []uuid.UUID
	seenPromptIDs := make(map[uuid.UUID]bool, len(norms))

	for i, key := range trimmedKeys {
		row := activeByNorm[norms[i]]
		if row == nil {
			failures = append(failures, store.WithdrawKeyFailure{
				Key:    key,
				Reason: store.WithdrawReasonNotFound,
			})
			continue
		}
		dStore := decisionToStore(row)
		dStore.Answers = answersToStore(existingAnswers[row.ID])
		switch {
		case row.Status == decision.StatusWithdrawn:
			if !seenAlready[row.ID] {
				seenAlready[row.ID] = true
				alreadyRows = append(alreadyRows, row)
			}
			if !seenPromptIDs[row.MessageID] {
				seenPromptIDs[row.MessageID] = true
				resolvedPromptIDs = append(resolvedPromptIDs, row.MessageID)
			}
		case row.Status == decision.StatusDismissed:
			failures = append(failures, store.WithdrawKeyFailure{
				Key:      row.Key,
				Reason:   store.WithdrawReasonDismissed,
				Decision: dStore,
			})
		case len(existingAnswers[row.ID]) > 0 || row.Status == decision.StatusDecided:
			failures = append(failures, store.WithdrawKeyFailure{
				Key:      row.Key,
				Reason:   store.WithdrawReasonAlreadyAnswered,
				Decision: dStore,
			})
		case row.Status == decision.StatusPending:
			if !seenPending[row.ID] {
				seenPending[row.ID] = true
				pendingRows = append(pendingRows, row)
			}
			if !seenPromptIDs[row.MessageID] {
				seenPromptIDs[row.MessageID] = true
				resolvedPromptIDs = append(resolvedPromptIDs, row.MessageID)
			}
		default:
			failures = append(failures, store.WithdrawKeyFailure{
				Key:      row.Key,
				Reason:   store.WithdrawReasonNotFound,
				Decision: dStore,
			})
		}
	}
	if len(failures) > 0 {
		return nil, &store.WithdrawDecisionsError{Failures: failures}
	}

	if in.WholeSet {
		for _, msgID := range resolvedPromptIDs {
			for _, sib := range siblingsByMsg[msgID] {
				if seenPending[sib.ID] || seenAlready[sib.ID] {
					continue
				}
				if sib.Status == decision.StatusPending && sib.ReplacedByID == nil && len(existingAnswers[sib.ID]) == 0 {
					seenPending[sib.ID] = true
					pendingRows = append(pendingRows, sib)
				}
			}
		}
	}

	sort.SliceStable(pendingRows, func(i, j int) bool {
		return compareEntDecisionOrder(pendingRows[i], pendingRows[j])
	})
	sort.SliceStable(alreadyRows, func(i, j int) bool {
		return compareEntDecisionOrder(alreadyRows[i], alreadyRows[j])
	})

	alreadyOut := make([]*store.Decision, len(alreadyRows))
	for i, r := range alreadyRows {
		d := decisionToStore(r)
		d.Answers = answersToStore(existingAnswers[r.ID])
		alreadyOut[i] = d
	}

	// Pure retry: every target is already withdrawn, so store nothing and keep
	// the original withdrawal timestamp and reason.
	if len(pendingRows) == 0 {
		return &store.WithdrawDecisionsResult{
			Withdrawn:        []*store.Decision{},
			AlreadyWithdrawn: alreadyOut,
		}, nil
	}

	// Truncate to microsecond precision (Postgres TIMESTAMPTZ resolution) so the
	// WithdrawnAt returned in the first response matches subsequent DB reads and
	// idempotent retries on both SQLite and Postgres.
	now := time.Now().UTC().Truncate(time.Microsecond)
	withdrawnOut := make([]*store.Decision, len(pendingRows))
	for i, r := range pendingRows {
		upd := client.Decision.Update().
			Where(decision.ID(r.ID), decision.StatusEQ(decision.StatusPending)).
			SetStatus(decision.StatusWithdrawn).
			SetWithdrawnAt(now)
		if reason != "" {
			upd.SetWithdrawnReason(reason)
		}
		n, err := upd.Save(ctx)
		if err != nil {
			return nil, mapError(err)
		}
		if n != 1 {
			return nil, fmt.Errorf("%w: version %d of %s changed concurrently", errDecisionRace, r.Version, r.Key)
		}
		d := decisionToStore(r)
		d.Status = store.DecisionStatusWithdrawn
		nowCopy := now
		d.WithdrawnAt = &nowCopy
		d.WithdrawnReason = reason
		d.Answers = answersToStore(existingAnswers[r.ID])
		withdrawnOut[i] = d
	}

	return &store.WithdrawDecisionsResult{
		Withdrawn:        withdrawnOut,
		AlreadyWithdrawn: alreadyOut,
	}, nil
}

// DismissDecisions implements store.DecisionStore.
func (c *CompositeStore) DismissDecisions(ctx context.Context, in *store.DismissDecisionsInput) (*store.DismissDecisionsResult, error) {
	if err := validateDismissDecisionsInput(in); err != nil {
		return nil, err
	}
	if c.inTx {
		return c.dismissDecisionsTx(ctx, in)
	}
	origMsg := in.ResponseMessage.Msg
	var res *store.DismissDecisionsResult
	for attempt := 0; ; attempt++ {
		in.ResponseMessage.Msg = origMsg
		err := c.WithTx(ctx, func(tx store.Store) error {
			var err error
			res, err = tx.(*CompositeStore).dismissDecisionsTx(ctx, in)
			return err
		})
		if err == nil {
			return res, nil
		}
		if attempt < maxDecisionRaceRetries && (errors.Is(err, errDecisionRace) || errors.Is(err, store.ErrTransient)) {
			continue
		}
		if errors.Is(err, errDecisionRace) {
			return nil, store.ErrTransient
		}
		return nil, err
	}
}

func validateDismissDecisionsInput(in *store.DismissDecisionsInput) error {
	if in == nil || in.ResponseMessage == nil || in.ResponseMessage.ID == "" || in.ResponseMessage.ProjectID == "" {
		return fmt.Errorf("%w: dismiss requires a response message with an ID and a project", store.ErrInvalidInput)
	}
	for _, id := range []string{in.MessageID, in.ResponseMessage.ID, in.ResponseMessage.ProjectID} {
		if _, err := parseUUID(id); err != nil {
			return err
		}
	}
	if strings.TrimSpace(in.User.UserID) == "" {
		return fmt.Errorf("%w: dismiss requires the dismissing user", store.ErrInvalidInput)
	}
	if len(in.RowIDs) > messages.MaxDecisionsPerMessage {
		return fmt.Errorf("%w: dismiss accepts at most %d decision row ids", store.ErrInvalidInput, messages.MaxDecisionsPerMessage)
	}
	seenIDs := make(map[string]bool, len(in.RowIDs))
	for _, rawID := range in.RowIDs {
		trimmed := strings.TrimSpace(rawID)
		if trimmed == "" {
			return fmt.Errorf("%w: decision row id must be non-empty", store.ErrInvalidInput)
		}
		if _, err := parseUUID(trimmed); err != nil {
			return err
		}
		if seenIDs[trimmed] {
			return fmt.Errorf("%w: duplicate decision row id %q", store.ErrInvalidInput, trimmed)
		}
		seenIDs[trimmed] = true
	}
	if len(in.IdempotencyKey) > store.MaxDecisionIdempotencyKeyLen {
		return fmt.Errorf("%w: idempotency key is at most %d bytes", store.ErrInvalidInput, store.MaxDecisionIdempotencyKeyLen)
	}
	surface := strings.TrimSpace(in.Surface)
	if surface == "" {
		surface = store.DecisionSurfaceNative
	}
	if surface != store.DecisionSurfaceNative && surface != store.DecisionSurfaceDecisionsPage {
		return fmt.Errorf("%w: invalid dismiss surface %q", store.ErrInvalidInput, in.Surface)
	}
	reason := strings.TrimSpace(in.Reason)
	if utf8.RuneCountInString(reason) > store.MaxDecisionDismissReasonRunes {
		return fmt.Errorf("%w: reason is at most %d characters", store.ErrInvalidInput, store.MaxDecisionDismissReasonRunes)
	}
	return nil
}

func (c *CompositeStore) dismissDecisionsTx(ctx context.Context, in *store.DismissDecisionsInput) (*store.DismissDecisionsResult, error) {
	client := c.client
	msgID, err := parseGetID(in.MessageID)
	if err != nil {
		return nil, err
	}

	rawNow := time.Now().UTC()
	if in.Now != nil {
		rawNow = in.Now().UTC()
	}
	// Truncate stored timestamps to microsecond precision so in-memory responses
	// match subsequent Postgres TIMESTAMPTZ reads byte-for-byte.
	writeNow := rawNow.Truncate(time.Microsecond)
	reason := strings.TrimSpace(in.Reason)
	surface := strings.TrimSpace(in.Surface)
	if surface == "" {
		surface = store.DecisionSurfaceNative
	}

	// Lock the prompt's rows on Postgres in (key_norm, version) order — the
	// same order used by CreateDecisionPrompt, RecordAnswerSet,
	// RecordInterpretedAnswers, and WithdrawDecisions — so concurrent races
	// serialize without deadlocking. Put rows back in prompt position order
	// after reading.
	q := client.Decision.Query().
		Where(decision.MessageID(msgID)).
		Order(ent.Asc(decision.FieldKeyNorm), ent.Asc(decision.FieldVersion))
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
	sort.Slice(rows, func(i, j int) bool { return rows[i].Position < rows[j].Position })

	byID := make(map[string]*ent.Decision, len(rows))
	rowIDs := make([]uuid.UUID, len(rows))
	for i, r := range rows {
		byID[r.ID.String()] = r
		rowIDs[i] = r.ID
	}

	requestedSet := make(map[string]bool, len(in.RowIDs))
	for _, rawID := range in.RowIDs {
		id := strings.TrimSpace(rawID)
		if _, ok := byID[id]; !ok {
			return nil, fmt.Errorf("%w: decision row %s is not in message %s", store.ErrInvalidInput, id, in.MessageID)
		}
		requestedSet[id] = true
	}

	existingAnswers, err := loadAnswers(ctx, client, rowIDs)
	if err != nil {
		return nil, err
	}

	// Replay check runs before state and minimum-age checks so retries always
	// receive the stored dismissal (or 422 ErrIdempotencyMismatch when reused
	// for a different row set or reason).
	if in.IdempotencyKey != "" {
		if res, ok, err := replayableDismissal(ctx, client, rows, existingAnswers, requestedSet, reason, in); err != nil || ok {
			return res, err
		}
	}

	minAge := dismissMinAge(in)

	var toDismiss []*ent.Decision
	var alreadyRows []*ent.Decision
	var earliestAvailableAt time.Time
	var latestTooRecentAt time.Time
	reasons := make(map[string]string)

	if len(requestedSet) > 0 {
		// Explicit RowIDs: all-or-nothing. Every requested row must either be
		// already dismissed (idempotent no-op) or pass DecisionActionableReason
		// for the user and meet minAge (DecisionDismissMinAge in production).
		for _, r := range rows {
			id := r.ID.String()
			if !requestedSet[id] {
				continue
			}
			dStore := decisionToStore(r)
			dStore.Answers = answersToStore(existingAnswers[r.ID])
			if r.Status == decision.StatusDismissed {
				alreadyRows = append(alreadyRows, r)
				continue
			}
			actReason := store.DecisionActionableReason(dStore, in.User.UserID)
			if actReason != "" {
				reasons[id] = actReason
				continue
			}
			avail := r.CreatedAt.Add(minAge)
			if rawNow.Before(avail) {
				reasons[id] = store.AnswerSetReasonTooRecent
				if avail.After(latestTooRecentAt) {
					latestTooRecentAt = avail
				}
				continue
			}
			toDismiss = append(toDismiss, r)
		}
		if len(reasons) > 0 {
			return nil, &store.DismissChangedError{Reasons: reasons, AvailableAt: latestTooRecentAt}
		}
	} else {
		// Empty RowIDs: dismiss every open row of the prompt waiting on the user
		// (pending or agent_interpreted naming the user, D6) that has reached
		// minAge (DecisionDismissMinAge in production); too-recent open rows are
		// skipped, and if none remain eligible, return DecisionTooRecentError
		// with the earliest time (design-8194 section 4.4a).
		for _, r := range rows {
			dStore := decisionToStore(r)
			dStore.Answers = answersToStore(existingAnswers[r.ID])
			if r.Status == decision.StatusDismissed {
				alreadyRows = append(alreadyRows, r)
				continue
			}
			actReason := store.DecisionActionableReason(dStore, in.User.UserID)
			if actReason == "" {
				avail := r.CreatedAt.Add(minAge)
				if rawNow.Before(avail) {
					if earliestAvailableAt.IsZero() || avail.Before(earliestAvailableAt) {
						earliestAvailableAt = avail
					}
					continue
				}
				toDismiss = append(toDismiss, r)
				continue
			}
			reasons[dStore.ID] = actReason
		}
	}

	alreadyOut := make([]*store.Decision, len(alreadyRows))
	for i, r := range alreadyRows {
		d := decisionToStore(r)
		d.Answers = answersToStore(existingAnswers[r.ID])
		alreadyOut[i] = d
	}

	if len(toDismiss) == 0 {
		if !earliestAvailableAt.IsZero() {
			return nil, &store.DecisionTooRecentError{AvailableAt: earliestAvailableAt}
		}
		if len(alreadyOut) > 0 {
			return &store.DismissDecisionsResult{
				Dismissed:        []*store.Decision{},
				AlreadyDismissed: alreadyOut,
			}, nil
		}
		return nil, &store.DismissChangedError{Reasons: reasons}
	}

	if in.FormatMessage != nil && in.ResponseMessage.Msg == "" {
		preview := make([]*store.Decision, len(toDismiss))
		for i, r := range toDismiss {
			d := decisionToStore(r)
			d.Answers = answersToStore(existingAnswers[r.ID])
			preview[i] = d
		}
		in.ResponseMessage.Msg = in.FormatMessage(preview)
	}
	in.ResponseMessage.Type = messages.TypeDecisionResponse
	if in.ResponseMessage.CreatedAt.IsZero() {
		in.ResponseMessage.CreatedAt = writeNow
	} else {
		in.ResponseMessage.CreatedAt = in.ResponseMessage.CreatedAt.UTC().Truncate(time.Microsecond)
	}
	if err := c.CreateMessage(ctx, in.ResponseMessage); err != nil {
		return nil, err
	}

	responseUUID, _ := parseUUID(in.ResponseMessage.ID)
	batchUUID := uuid.New()
	dismissedOut := make([]*store.Decision, len(toDismiss))

	for i, r := range toDismiss {
		savedAns, err := client.DecisionAnswer.Create().
			SetDecisionRowID(r.ID).
			SetProjectID(r.ProjectID).
			SetAskerAgentID(r.AskerAgentID).
			SetKey(r.Key).
			SetVersion(r.Version).
			SetQuestionText(r.Question).
			SetChoiceID(messages.ChoiceIDDismissed).
			SetChoiceLabel(messages.ChoiceLabelDismissed).
			SetNillableAnsweredByUserID(nullableString(in.User.UserID)).
			SetAnsweredByName(in.User.Name).
			SetAnsweredByEmail(in.User.Email).
			SetProvenance(decisionanswer.ProvenanceDirect).
			SetOutcome(decisionanswer.OutcomeDismiss).
			SetNillableDismissReason(nullableString(reason)).
			SetSurface(decisionanswer.Surface(surface)).
			SetResponseMessageID(responseUUID).
			SetNillableIdempotencyKey(nullableString(in.IdempotencyKey)).
			SetBatchID(batchUUID).
			SetMatchedDefault(false).
			SetCreatedAt(writeNow).
			Save(ctx)
		if err != nil {
			return nil, markDecisionRace(err)
		}

		// Transition pending or decided (interpreted awaiting confirm, D6) to
		// dismissed while keeping any prior agent_interpreted answer row in
		// decision_answers.
		n, err := client.Decision.Update().
			Where(decision.ID(r.ID), decision.StatusEQ(r.Status)).
			SetStatus(decision.StatusDismissed).
			SetDismissedAt(writeNow).
			Save(ctx)
		if err != nil {
			return nil, mapError(err)
		}
		if n != 1 {
			return nil, fmt.Errorf("%w: version %d of %s changed concurrently", errDecisionRace, r.Version, r.Key)
		}

		d := decisionToStore(r)
		d.Status = store.DecisionStatusDismissed
		nowCopy := writeNow
		d.DismissedAt = &nowCopy
		ansStore := answerToStore(savedAns)
		ansStore.CreatedAt = writeNow
		d.Answers = append(answersToStore(existingAnswers[r.ID]), ansStore)
		dismissedOut[i] = d
	}

	return &store.DismissDecisionsResult{
		Dismissed:        dismissedOut,
		AlreadyDismissed: alreadyOut,
		BatchID:          batchUUID.String(),
	}, nil
}

// replayableDismissal checks whether in.IdempotencyKey was already used by
// in.User.UserID on this prompt's rows. Returns (res, true, nil) on an exact
// replay of the same row set and trimmed reason, (nil, false, ErrIdempotencyMismatch)
// when reused for different rows/reason/answers, or (nil, false, nil) when unused.
func replayableDismissal(
	ctx context.Context,
	client *ent.Client,
	rows []*ent.Decision,
	existingAnswers map[uuid.UUID][]*ent.DecisionAnswer,
	requestedSet map[string]bool,
	reason string,
	in *store.DismissDecisionsInput,
) (*store.DismissDecisionsResult, bool, error) {
	rowIDs := make([]uuid.UUID, len(rows))
	for i, r := range rows {
		rowIDs[i] = r.ID
	}
	found, err := client.DecisionAnswer.Query().
		Where(
			decisionanswer.DecisionRowIDIn(rowIDs...),
			decisionanswer.IdempotencyKey(in.IdempotencyKey),
			decisionanswer.BatchIDNotNil(),
			decisionanswer.AnsweredByUserID(in.User.UserID),
		).
		All(ctx)
	if err != nil {
		return nil, false, mapError(err)
	}
	if len(found) == 0 {
		return nil, false, nil
	}

	mismatch := fmt.Errorf("%w: the idempotency key was used for another dismissal or answer set", store.ErrIdempotencyMismatch)
	batch := *found[0].BatchID
	recordedAt := found[0].CreatedAt
	foundByRow := make(map[uuid.UUID]*ent.DecisionAnswer, len(found))
	for _, f := range found {
		if f.Outcome != decisionanswer.OutcomeDismiss ||
			f.AnsweredByUserID == nil || *f.AnsweredByUserID != in.User.UserID ||
			f.BatchID == nil || *f.BatchID != batch ||
			derefString(f.DismissReason) != reason {
			return nil, false, mismatch
		}
		foundByRow[f.DecisionRowID] = f
	}

	var dismissedOut []*store.Decision
	var alreadyOut []*store.Decision

	if len(requestedSet) > 0 {
		for _, f := range found {
			if !requestedSet[f.DecisionRowID.String()] {
				return nil, false, mismatch
			}
		}
		for _, r := range rows {
			if !requestedSet[r.ID.String()] {
				continue
			}
			dStore := decisionToStore(r)
			dStore.Answers = answersToStore(existingAnswers[r.ID])
			if _, inBatch := foundByRow[r.ID]; inBatch {
				dismissedOut = append(dismissedOut, dStore)
				continue
			}
			// A requested row not in this batch is valid only if it was already
			// dismissed in an earlier call (not still open, decided, or dismissed
			// after recordedAt).
			dsm := dStore.Dismissal()
			if r.Status != decision.StatusDismissed || dsm == nil || dsm.BatchID == batch.String() || dsm.CreatedAt.After(recordedAt) {
				return nil, false, mismatch
			}
			alreadyOut = append(alreadyOut, dStore)
		}
	} else {
		for _, r := range rows {
			dStore := decisionToStore(r)
			dStore.Answers = answersToStore(existingAnswers[r.ID])
			if _, inBatch := foundByRow[r.ID]; inBatch {
				dismissedOut = append(dismissedOut, dStore)
				continue
			}
			if r.Status == decision.StatusDismissed {
				if dsm := dStore.Dismissal(); dsm != nil && !dsm.CreatedAt.After(recordedAt) {
					alreadyOut = append(alreadyOut, dStore)
				}
				continue
			}
			// If another row of the prompt is still waiting on the user and was
			// already >= minAge old at recordedAt, the original call under
			// this key must have been an explicit subset rather than empty RowIDs.
			if store.DecisionActionableReason(dStore, in.User.UserID) == "" &&
				!recordedAt.Before(r.CreatedAt.Add(dismissMinAge(in)).Truncate(time.Microsecond)) {
				return nil, false, mismatch
			}
		}
	}

	if dismissedOut == nil {
		dismissedOut = []*store.Decision{}
	}
	if alreadyOut == nil {
		alreadyOut = []*store.Decision{}
	}
	return &store.DismissDecisionsResult{
		Dismissed:        dismissedOut,
		AlreadyDismissed: alreadyOut,
		BatchID:          batch.String(),
		Replayed:         true,
	}, true, nil
}

func dismissMinAge(in *store.DismissDecisionsInput) time.Duration {
	if in != nil && in.MinAge > 0 {
		return in.MinAge
	}
	return store.DecisionDismissMinAge
}
