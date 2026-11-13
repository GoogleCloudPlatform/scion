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

//go:build !no_sqlite

package entadapter

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/message"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// decisionFixture is one agent asking in one conversation.
type decisionFixture struct {
	cs             *CompositeStore
	projectID      string
	conversationID string
	agentID        string
}

func newDecisionFixture(t *testing.T) *decisionFixture {
	t.Helper()
	return &decisionFixture{
		cs:             newTestCompositeStore(t),
		projectID:      uuid.NewString(),
		conversationID: uuid.NewString(),
		agentID:        uuid.NewString(),
	}
}

// yesNo is a normalized decision with the given key ("" = hub-assigned).
func yesNo(key, question string) messages.Decision {
	return messages.Decision{
		Key:             key,
		Question:        question,
		DefaultChoiceID: "1",
		Choices:         []messages.Choice{{ID: "1", Label: "yes"}, {ID: "2", Label: "no"}},
	}
}

func (f *decisionFixture) prompt(intro string, update bool, ds ...messages.Decision) *store.ChoicePromptInput {
	return &store.ChoicePromptInput{
		Message: &store.Message{
			ID:             uuid.NewString(),
			ProjectID:      f.projectID,
			ConversationID: f.conversationID,
			Sender:         "agent:asker",
			SenderID:       f.agentID,
			Recipient:      "user:pat@example.com",
			RecipientID:    "user-1",
			AgentID:        f.agentID,
			DispatchState:  store.MessageDispatchDispatched,
		},
		Intro:          intro,
		AskerAgentID:   f.agentID,
		AskerAgentName: "asker",
		Decisions:      ds,
		Update:         update,
	}
}

func (f *decisionFixture) create(t *testing.T, update bool, ds ...messages.Decision) *store.ChoicePromptResult {
	t.Helper()
	res, err := f.cs.CreateChoicePrompt(context.Background(), f.prompt("", update, ds...))
	require.NoError(t, err)
	return res
}

func (f *decisionFixture) answer(rowID, choiceID, userID, idem string) *store.RecordAnswerInput {
	return &store.RecordAnswerInput{
		DecisionRowID:    rowID,
		ChoiceID:         choiceID,
		AnsweredByUserID: userID,
		AnsweredByName:   "Pat",
		AnsweredByEmail:  "pat@example.com",
		Provenance:       store.DecisionProvenanceDirect,
		Surface:          store.DecisionSurfaceNative,
		IdempotencyKey:   idem,
		ResponseMessage: &store.Message{
			ID:             uuid.NewString(),
			ProjectID:      f.projectID,
			ConversationID: f.conversationID,
			Sender:         "user:pat@example.com",
			SenderID:       userID,
			Recipient:      "agent:asker",
			RecipientID:    f.agentID,
			Msg:            "Pat chose yes",
			DispatchState:  store.MessageDispatchDispatched,
		},
	}
}

func TestCreateChoicePrompt_StoresMessageAndDecisions(t *testing.T) {
	f := newDecisionFixture(t)
	ctx := context.Background()
	in := f.prompt("Two things 🚀", false, yesNo("D234", "Ship it?"), yesNo("", "Which approach?"))
	res, err := f.cs.CreateChoicePrompt(ctx, in)
	require.NoError(t, err)
	require.Len(t, res.Decisions, 2)

	assert.Equal(t, "D234", res.Decisions[0].Key)
	assert.Equal(t, store.DecisionKeySourceAgent, res.Decisions[0].KeySource)
	assert.Equal(t, 1, res.Decisions[0].Version)
	assert.Nil(t, res.Decisions[0].Replaced)
	assert.Equal(t, "1", res.Decisions[1].Key)
	assert.Equal(t, store.DecisionKeySourceHub, res.Decisions[1].KeySource)

	// The message is stored as a choice prompt whose text is the fallback.
	msg, err := f.cs.GetMessage(ctx, in.Message.ID)
	require.NoError(t, err)
	assert.Equal(t, messages.TypeChoicePrompt, msg.Type)
	assert.Contains(t, msg.Msg, "**D234.** Ship it?")
	assert.Contains(t, msg.Msg, "**1.** Which approach?")
	assert.Equal(t, len("Two things 🚀\n\n"), res.IntroLength)
	assert.Equal(t, "Two things 🚀", messages.ChoiceIntro(msg.Msg, res.IntroLength))

	byMsg, err := f.cs.GetDecisionsByMessageIDs(ctx, []string{in.Message.ID, "not-a-uuid", uuid.NewString()})
	require.NoError(t, err)
	require.Len(t, byMsg, 1)
	ds := byMsg[in.Message.ID]
	require.Len(t, ds, 2)
	d0, d1 := ds[0], ds[1]
	assert.Equal(t, res.Decisions[0].RowID, d0.ID)
	assert.Equal(t, 0, d0.Position)
	assert.Equal(t, 1, d1.Position)
	assert.Equal(t, f.projectID, d0.ProjectID)
	assert.Equal(t, f.conversationID, d0.ConversationID)
	assert.Equal(t, f.agentID, d0.AskerAgentID)
	assert.Equal(t, "asker", d0.AskerAgentName)
	assert.Equal(t, "d234", d0.KeyNorm)
	assert.Equal(t, store.DecisionStatusPending, d0.Status)
	assert.Equal(t, "1", d0.DefaultChoiceID)
	assert.Equal(t, []messages.Choice{{ID: "1", Label: "yes"}, {ID: "2", Label: "no"}}, d0.Choices)
	assert.Equal(t, res.IntroLength, d0.IntroLength)
	assert.Equal(t, 0, d1.IntroLength, "the intro length is stored on the position 0 row only")
	assert.Equal(t, f.agentID+"/D234", d0.Ref())

	got, err := f.cs.GetDecision(ctx, d1.ID)
	require.NoError(t, err)
	assert.Equal(t, "1", got.Key)
	assert.Equal(t, store.DecisionKeySourceHub, got.KeySource)

	_, err = f.cs.GetDecision(ctx, uuid.NewString())
	assert.ErrorIs(t, err, store.ErrNotFound)
	_, err = f.cs.GetDecision(ctx, "nope")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestCreateChoicePrompt_HubNumbersAreNeverReused(t *testing.T) {
	f := newDecisionFixture(t)
	other := uuid.NewString()

	r1 := f.create(t, false, yesNo("", "a"), yesNo("", "b"))
	assert.Equal(t, []string{"1", "2"}, []string{r1.Decisions[0].Key, r1.Decisions[1].Key})

	// Replacing number 2 does not free it: the next number is 3.
	r2 := f.create(t, true, yesNo("2", "b again"))
	assert.Equal(t, "2", r2.Decisions[0].Key)
	assert.Equal(t, 2, r2.Decisions[0].Version)
	r3 := f.create(t, false, yesNo("", "c"))
	assert.Equal(t, "3", r3.Decisions[0].Key)

	// Another agent has its own counter.
	g := &decisionFixture{cs: f.cs, projectID: f.projectID, conversationID: f.conversationID, agentID: other}
	r4 := g.create(t, false, yesNo("", "x"))
	assert.Equal(t, "1", r4.Decisions[0].Key)

	// A prompt refused after its numbers were reserved (here for an
	// all-digit key never issued) rolls the reservation back.
	_, err := f.cs.CreateChoicePrompt(context.Background(), f.prompt("", false, yesNo("", "d"), yesNo("99", "e")))
	require.ErrorIs(t, err, store.ErrDecisionKeyNotIssued)
	r5 := f.create(t, false, yesNo("", "f"))
	assert.Equal(t, "4", r5.Decisions[0].Key)
}

func TestCreateChoicePrompt_RejectsUnissuedNumbers(t *testing.T) {
	f := newDecisionFixture(t)
	ctx := context.Background()

	_, err := f.cs.CreateChoicePrompt(ctx, f.prompt("", true, yesNo("1", "q")))
	var nie *store.DecisionKeyNotIssuedError
	require.ErrorAs(t, err, &nie)
	assert.ErrorIs(t, err, store.ErrDecisionKeyNotIssued)
	assert.Equal(t, []string{"1"}, nie.Keys)

	f.create(t, false, yesNo("", "first")) // issues 1
	// 1 is now issued; 2 is not, even in the same prompt that reserves 2.
	_, err = f.cs.CreateChoicePrompt(ctx, f.prompt("", true, yesNo("", "new"), yesNo("2", "q")))
	require.ErrorAs(t, err, &nie)
	assert.Equal(t, []string{"2"}, nie.Keys)

	res, err := f.cs.CreateChoicePrompt(ctx, f.prompt("", true, yesNo("1", "repost")))
	require.NoError(t, err)
	assert.Equal(t, 2, res.Decisions[0].Version)
	assert.Equal(t, store.DecisionKeySourceHub, res.Decisions[0].KeySource)

	// Nothing from the refused prompts was stored.
	n, err := f.cs.client.Message.Query().Where(message.TypeEQ(messages.TypeChoicePrompt)).Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, n)
}

func TestCreateChoicePrompt_RejectsExistingKeysWithoutUpdate(t *testing.T) {
	f := newDecisionFixture(t)
	ctx := context.Background()
	first := f.create(t, false, yesNo("D234", "Ship it?"), yesNo("ship-now", "Now?"))
	_, _, err := f.cs.RecordAnswer(ctx, f.answer(first.Decisions[0].RowID, "1", "user-1", ""))
	require.NoError(t, err)

	in := f.prompt("", false, yesNo("d234", "again"), yesNo("SHIP-NOW", "again"), yesNo("fresh", "new"))
	_, err = f.cs.CreateChoicePrompt(ctx, in)
	var kee *store.ChoiceKeyExistsError
	require.ErrorAs(t, err, &kee)
	assert.ErrorIs(t, err, store.ErrChoiceKeyExists)
	require.Len(t, kee.Existing, 2)
	assert.Equal(t, "D234", kee.Existing[0].Key, "keys are displayed as first given")
	assert.Equal(t, 1, kee.Existing[0].Version)
	assert.Equal(t, store.DecisionStatusDecided, kee.Existing[0].Status)
	require.NotNil(t, kee.Existing[0].Answer)
	assert.Equal(t, "yes", kee.Existing[0].Answer.ChoiceLabel)
	assert.Equal(t, store.DecisionStatusPending, kee.Existing[1].Status)
	assert.Nil(t, kee.Existing[1].Answer)
	assert.Equal(t,
		"D234 already exists (v1, decided by Pat: yes); ship-now already exists (v1, pending); pass --update to replace it, or use a new id",
		err.Error())

	// Nothing was stored: not the message, not the new key.
	_, err = f.cs.GetMessage(ctx, in.Message.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	byMsg, err := f.cs.GetDecisionsByMessageIDs(ctx, []string{in.Message.ID})
	require.NoError(t, err)
	assert.Empty(t, byMsg)
}

func TestCreateChoicePrompt_UpdateReplacesPendingAndDecided(t *testing.T) {
	f := newDecisionFixture(t)
	ctx := context.Background()
	v1 := f.create(t, false, yesNo("D1", "pending one"), yesNo("D2", "decided one"))
	ans, _, err := f.cs.RecordAnswer(ctx, f.answer(v1.Decisions[1].RowID, "2", "user-1", ""))
	require.NoError(t, err)

	v2 := f.create(t, true, yesNo("d1", "pending one, v2"), yesNo("D2", "decided one, v2"), yesNo("D3", "brand new"))
	require.Len(t, v2.Decisions, 3)

	p := v2.Decisions[0]
	assert.Equal(t, "D1", p.Key, "a repost keeps the key as first given")
	assert.Equal(t, 2, p.Version)
	require.NotNil(t, p.Replaced)
	assert.Equal(t, store.ReplacedDecision{RowID: v1.Decisions[0].RowID, Version: 1, Status: store.DecisionStatusPending}, *p.Replaced)

	d := v2.Decisions[1]
	assert.Equal(t, 2, d.Version)
	require.NotNil(t, d.Replaced)
	assert.Equal(t, store.DecisionStatusDecided, d.Replaced.Status)
	require.NotNil(t, d.Replaced.Answer)
	assert.Equal(t, "no", d.Replaced.Answer.ChoiceLabel)

	assert.Equal(t, 1, v2.Decisions[2].Version)
	assert.Nil(t, v2.Decisions[2].Replaced)

	// The old versions are replaced and point at the new ones; the decided
	// one keeps its answer row.
	old, err := f.cs.GetDecision(ctx, v1.Decisions[0].RowID)
	require.NoError(t, err)
	assert.Equal(t, store.DecisionStatusReplaced, old.Status)
	assert.Equal(t, p.RowID, old.ReplacedByID)
	assert.NotNil(t, old.ReplacedAt)

	oldDecided, err := f.cs.GetDecision(ctx, v1.Decisions[1].RowID)
	require.NoError(t, err)
	assert.Equal(t, store.DecisionStatusReplaced, oldDecided.Status)
	assert.NotNil(t, oldDecided.DecidedAt)
	require.Len(t, oldDecided.Answers, 1)
	assert.Equal(t, ans.ID, oldDecided.Answers[0].ID)

	// A replaced version can no longer be answered.
	_, _, err = f.cs.RecordAnswer(ctx, f.answer(v1.Decisions[0].RowID, "1", "user-1", ""))
	assert.ErrorIs(t, err, store.ErrDecisionReplaced)

	// v3 replaces v2, not v1.
	v3 := f.create(t, true, yesNo("D1", "v3"))
	assert.Equal(t, 3, v3.Decisions[0].Version)
	assert.Equal(t, p.RowID, v3.Decisions[0].Replaced.RowID)
}

func TestCreateChoicePrompt_PendingCap(t *testing.T) {
	f := newDecisionFixture(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		ds := make([]messages.Decision, 10)
		for j := range ds {
			ds[j] = yesNo(fmt.Sprintf("k%d-%d", i, j), "q")
		}
		f.create(t, false, ds...)
	}
	// 50 pending: a new key is refused, nothing is stored.
	in := f.prompt("", false, yesNo("one-more", "q"))
	_, err := f.cs.CreateChoicePrompt(ctx, in)
	require.ErrorIs(t, err, store.ErrTooManyPendingDecisions)
	_, err = f.cs.GetMessage(ctx, in.Message.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)

	// An --update of a pending key counts once, so it still fits.
	_, err = f.cs.CreateChoicePrompt(ctx, f.prompt("", true, yesNo("k0-0", "v2")))
	require.NoError(t, err)

	// Answering one frees a slot.
	byMsgRows, err := f.cs.client.Decision.Query().All(ctx)
	require.NoError(t, err)
	var pendingRow string
	for _, r := range byMsgRows {
		if r.Key == "k1-0" {
			pendingRow = r.ID.String()
		}
	}
	_, _, err = f.cs.RecordAnswer(ctx, f.answer(pendingRow, "1", "user-1", ""))
	require.NoError(t, err)
	_, err = f.cs.CreateChoicePrompt(ctx, f.prompt("", false, yesNo("one-more", "q")))
	require.NoError(t, err)
}

func TestCreateChoicePrompt_ConcurrentUpdatesGiveConsecutiveVersions(t *testing.T) {
	testConcurrentUpdates(t, 2)
}

// TestCreateChoicePrompt_ThreeConcurrentUpdates: the last of n concurrent
// reposts of one key needs n-1 retries, more than the single retry a
// two-way race needs.
func TestCreateChoicePrompt_ThreeConcurrentUpdates(t *testing.T) {
	testConcurrentUpdates(t, 3)
}

// testConcurrentUpdates races n --update reposts of one key: each gets
// its own next version and exactly one version stays active.
func testConcurrentUpdates(t *testing.T, n int) {
	t.Helper()
	f := newDecisionFixture(t)
	ctx := context.Background()
	f.create(t, false, yesNo("D1", "v1"))

	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make([]*store.ChoicePromptResult, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = f.cs.CreateChoicePrompt(ctx, f.prompt("", true, yesNo("D1", fmt.Sprintf("racer %d", i))))
		}(i)
	}
	close(start)
	wg.Wait()

	var versions, want []int
	for i := 0; i < n; i++ {
		require.NoError(t, errs[i], "racer %d", i)
		versions = append(versions, results[i].Decisions[0].Version)
		want = append(want, i+2)
	}
	sort.Ints(versions)
	assert.Equal(t, want, versions)

	rows, err := f.cs.client.Decision.Query().All(ctx)
	require.NoError(t, err)
	active := 0
	for _, r := range rows {
		if string(r.Status) != store.DecisionStatusReplaced {
			active++
			assert.Equal(t, n+1, r.Version)
		}
	}
	assert.Equal(t, 1, active, "exactly one version stays active")
}

func TestCreateChoicePrompt_ConcurrentHubNumbersAreDistinct(t *testing.T) {
	f := newDecisionFixture(t)
	ctx := context.Background()
	const n = 4
	var wg sync.WaitGroup
	start := make(chan struct{})
	keys := make([]string, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			res, err := f.cs.CreateChoicePrompt(ctx, f.prompt("", false, yesNo("", "q")))
			errs[i] = err
			if err == nil {
				keys[i] = res.Decisions[0].Key
			}
		}(i)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		require.NoError(t, err, "prompt %d", i)
	}
	sort.Strings(keys)
	assert.Equal(t, []string{"1", "2", "3", "4"}, keys)
}

func TestRecordAnswer_RecordsSnapshotAndDecides(t *testing.T) {
	f := newDecisionFixture(t)
	ctx := context.Background()
	res := f.create(t, false, yesNo("D234", "Ship the **feature** now?"))
	in := f.answer(res.Decisions[0].RowID, "1", "user-1", "")
	in.AuthorizingUserID = "issuer-1"
	in.DelegationGrantID = "grant-1"
	a, replayed, err := f.cs.RecordAnswer(ctx, in)
	require.NoError(t, err)
	assert.False(t, replayed)

	assert.Equal(t, res.Decisions[0].RowID, a.DecisionRowID)
	assert.Equal(t, f.projectID, a.ProjectID)
	assert.Equal(t, f.agentID, a.AskerAgentID)
	assert.Equal(t, "D234", a.Key)
	assert.Equal(t, 1, a.Version)
	assert.Equal(t, "Ship the **feature** now?", a.QuestionText)
	assert.Equal(t, "1", a.ChoiceID)
	assert.Equal(t, "yes", a.ChoiceLabel, "the label comes from the stored row")
	assert.Equal(t, "user-1", a.AnsweredByUserID)
	assert.Equal(t, "Pat", a.AnsweredByName)
	assert.Equal(t, "pat@example.com", a.AnsweredByEmail)
	assert.Equal(t, store.DecisionProvenanceDirect, a.Provenance)
	assert.Equal(t, store.DecisionOutcomeAnswer, a.Outcome)
	assert.Equal(t, store.DecisionSurfaceNative, a.Surface)
	assert.Equal(t, in.ResponseMessage.ID, a.ResponseMessageID)
	assert.Equal(t, "issuer-1", a.AuthorizingUserID)
	assert.Equal(t, "grant-1", a.DelegationGrantID)

	resp, err := f.cs.GetMessage(ctx, in.ResponseMessage.ID)
	require.NoError(t, err)
	assert.Equal(t, messages.TypeChoiceResponse, resp.Type)

	d, err := f.cs.GetDecision(ctx, res.Decisions[0].RowID)
	require.NoError(t, err)
	assert.Equal(t, store.DecisionStatusDecided, d.Status)
	require.NotNil(t, d.DecidedAt)
	require.Len(t, d.Answers, 1)
	assert.Equal(t, a.ID, d.EffectiveAnswer().ID)

	// A second direct answer is a conflict; nothing more is stored.
	again := f.answer(res.Decisions[0].RowID, "2", "user-2", "")
	_, _, err = f.cs.RecordAnswer(ctx, again)
	assert.ErrorIs(t, err, store.ErrDecisionAnswerConflict)
	_, err = f.cs.GetMessage(ctx, again.ResponseMessage.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestRecordAnswer_InvalidInput(t *testing.T) {
	f := newDecisionFixture(t)
	ctx := context.Background()
	res := f.create(t, false, yesNo("D1", "q"))
	row := res.Decisions[0].RowID

	cases := map[string]func(in *store.RecordAnswerInput){
		"unknown choice":          func(in *store.RecordAnswerInput) { in.ChoiceID = "9" },
		"no choice":               func(in *store.RecordAnswerInput) { in.ChoiceID = "" },
		"bad provenance":          func(in *store.RecordAnswerInput) { in.Provenance = "robot" },
		"bad surface":             func(in *store.RecordAnswerInput) { in.Surface = "fax" },
		"bad outcome":             func(in *store.RecordAnswerInput) { in.Outcome = "maybe" },
		"no response message":     func(in *store.RecordAnswerInput) { in.ResponseMessage = nil },
		"interpreted without src": func(in *store.RecordAnswerInput) { in.Provenance = store.DecisionProvenanceAgentInterpreted },
		"direct with source":      func(in *store.RecordAnswerInput) { in.SourceMessageID = uuid.NewString() },
		"long idempotency key":    func(in *store.RecordAnswerInput) { in.IdempotencyKey = strings.Repeat("k", 129) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			in := f.answer(row, "1", "user-1", "")
			mutate(in)
			_, _, err := f.cs.RecordAnswer(ctx, in)
			assert.ErrorIs(t, err, store.ErrInvalidInput)
		})
	}
	_, _, err := f.cs.RecordAnswer(ctx, f.answer(uuid.NewString(), "1", "user-1", ""))
	assert.ErrorIs(t, err, store.ErrNotFound)

	d, err := f.cs.GetDecision(ctx, row)
	require.NoError(t, err)
	assert.Equal(t, store.DecisionStatusPending, d.Status, "no failed answer changed the status")
}

func TestRecordAnswer_InterpretedThenDirectKeepsDecidedAt(t *testing.T) {
	f := newDecisionFixture(t)
	ctx := context.Background()
	res := f.create(t, false, yesNo("D1", "q"))
	row := res.Decisions[0].RowID

	interp := f.answer(row, "1", "user-1", "")
	interp.Provenance = store.DecisionProvenanceAgentInterpreted
	interp.InterpretedByAgentID = f.agentID
	interp.SourceMessageID = uuid.NewString()
	interp.Surface = store.DecisionSurfaceSlack
	ia, _, err := f.cs.RecordAnswer(ctx, interp)
	require.NoError(t, err)
	assert.Equal(t, f.agentID, ia.InterpretedByAgentID)
	assert.Equal(t, interp.SourceMessageID, ia.SourceMessageID)

	first, err := f.cs.GetDecision(ctx, row)
	require.NoError(t, err)
	require.NotNil(t, first.DecidedAt)

	direct := f.answer(row, "2", "user-1", "")
	direct.Outcome = store.DecisionOutcomeOverride
	da, _, err := f.cs.RecordAnswer(ctx, direct)
	require.NoError(t, err)

	d, err := f.cs.GetDecision(ctx, row)
	require.NoError(t, err)
	assert.Equal(t, first.DecidedAt.UnixNano(), d.DecidedAt.UnixNano(), "only the pending-to-decided transition sets decided_at")
	require.Len(t, d.Answers, 2)
	assert.Equal(t, da.ID, d.EffectiveAnswer().ID, "the direct answer is effective")
	assert.Equal(t, store.DecisionOutcomeOverride, d.EffectiveAnswer().Outcome)
}

func TestRecordAnswer_IdempotentReplay(t *testing.T) {
	f := newDecisionFixture(t)
	ctx := context.Background()
	res := f.create(t, false, yesNo("D1", "q"))
	row := res.Decisions[0].RowID

	a1, replayed, err := f.cs.RecordAnswer(ctx, f.answer(row, "1", "user-1", "idem-1"))
	require.NoError(t, err)
	assert.False(t, replayed)
	assert.Equal(t, "idem-1", a1.IdempotencyKey)

	retry := f.answer(row, "1", "user-1", "idem-1")
	a2, replayed, err := f.cs.RecordAnswer(ctx, retry)
	require.NoError(t, err)
	assert.True(t, replayed)
	assert.Equal(t, a1.ID, a2.ID)
	_, err = f.cs.GetMessage(ctx, retry.ResponseMessage.ID)
	assert.ErrorIs(t, err, store.ErrNotFound, "a replay stores no second response message")

	// The same key with another choice or another user is a conflict.
	_, _, err = f.cs.RecordAnswer(ctx, f.answer(row, "2", "user-1", "idem-1"))
	assert.ErrorIs(t, err, store.ErrDecisionAnswerConflict)
	_, _, err = f.cs.RecordAnswer(ctx, f.answer(row, "1", "user-2", "idem-1"))
	assert.ErrorIs(t, err, store.ErrDecisionAnswerConflict)

	// A replay still works after the version is replaced.
	f.create(t, true, yesNo("D1", "v2"))
	a3, replayed, err := f.cs.RecordAnswer(ctx, f.answer(row, "1", "user-1", "idem-1"))
	require.NoError(t, err)
	assert.True(t, replayed)
	assert.Equal(t, a1.ID, a3.ID)
}

func TestRecordAnswer_ConcurrentAnswersHaveOneWinner(t *testing.T) {
	f := newDecisionFixture(t)
	ctx := context.Background()
	res := f.create(t, false, yesNo("D1", "q"))
	row := res.Decisions[0].RowID

	const n = 2
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, _, errs[i] = f.cs.RecordAnswer(ctx, f.answer(row, fmt.Sprint(i+1), fmt.Sprintf("user-%d", i), ""))
		}(i)
	}
	close(start)
	wg.Wait()

	wins, conflicts := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, store.ErrDecisionAnswerConflict):
			conflicts++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	assert.Equal(t, 1, wins)
	assert.Equal(t, 1, conflicts)

	d, err := f.cs.GetDecision(ctx, row)
	require.NoError(t, err)
	assert.Len(t, d.Answers, 1)
	assert.Equal(t, store.DecisionStatusDecided, d.Status)
}

func TestDecisionRows_SurvivePromptMessageDeletion(t *testing.T) {
	f := newDecisionFixture(t)
	ctx := context.Background()
	in := f.prompt("", false, yesNo("D1", "q"))
	res, err := f.cs.CreateChoicePrompt(ctx, in)
	require.NoError(t, err)
	ans := f.answer(res.Decisions[0].RowID, "2", "user-1", "")
	_, _, err = f.cs.RecordAnswer(ctx, ans)
	require.NoError(t, err)

	// Purge both messages, as chat retention would.
	for _, id := range []string{in.Message.ID, ans.ResponseMessage.ID} {
		require.NoError(t, f.cs.client.Message.DeleteOneID(uuid.MustParse(id)).Exec(ctx))
	}

	d, err := f.cs.GetDecision(ctx, res.Decisions[0].RowID)
	require.NoError(t, err)
	assert.Equal(t, "q", d.Question)
	require.Len(t, d.Answers, 1)
	assert.Equal(t, "no", d.Answers[0].ChoiceLabel)
	assert.Equal(t, "q", d.Answers[0].QuestionText)
	byMsg, err := f.cs.GetDecisionsByMessageIDs(ctx, []string{in.Message.ID})
	require.NoError(t, err)
	assert.Len(t, byMsg[in.Message.ID], 1)
}

func TestCreateChoicePrompt_JoinsCallerTransaction(t *testing.T) {
	f := newDecisionFixture(t)
	ctx := context.Background()
	in := f.prompt("", false, yesNo("D1", "q"))
	boom := errors.New("caller rolls back")
	err := f.cs.WithTx(ctx, func(tx store.Store) error {
		if _, err := tx.CreateChoicePrompt(ctx, in); err != nil {
			return err
		}
		return boom
	})
	require.ErrorIs(t, err, boom)
	_, err = f.cs.GetMessage(ctx, in.Message.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	byMsg, err := f.cs.GetDecisionsByMessageIDs(ctx, []string{in.Message.ID})
	require.NoError(t, err)
	assert.Empty(t, byMsg)
	// The counter did not move either: D1 is still free.
	f.create(t, false, yesNo("D1", "q"))
}

func TestCreateChoicePrompt_InvalidInput(t *testing.T) {
	f := newDecisionFixture(t)
	ctx := context.Background()
	cases := map[string]func(in *store.ChoicePromptInput){
		"no message":          func(in *store.ChoicePromptInput) { in.Message = nil },
		"no project":          func(in *store.ChoicePromptInput) { in.Message.ProjectID = "" },
		"bad message id":      func(in *store.ChoicePromptInput) { in.Message.ID = "message-1" },
		"bad project id":      func(in *store.ChoicePromptInput) { in.Message.ProjectID = "project-1" },
		"bad agent":           func(in *store.ChoicePromptInput) { in.AskerAgentID = "agent-1" },
		"no decisions":        func(in *store.ChoicePromptInput) { in.Decisions = nil },
		"invalid key":         func(in *store.ChoicePromptInput) { in.Decisions[0].Key = "a b" },
		"unnormalized choice": func(in *store.ChoicePromptInput) { in.Decisions[0].Choices = in.Decisions[0].Choices[:1] },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			in := f.prompt("", false, yesNo("D1", "q"))
			mutate(in)
			_, err := f.cs.CreateChoicePrompt(ctx, in)
			assert.ErrorIs(t, err, store.ErrInvalidInput)
		})
	}
}

func TestGetDecisionAnswersByResponseMessageIDs(t *testing.T) {
	f := newDecisionFixture(t)
	ctx := context.Background()
	res := f.create(t, false, yesNo("D1", "One?"), yesNo("D2", "Two?"))
	in := f.answer(res.Decisions[0].RowID, "2", "user-1", "")
	a, _, err := f.cs.RecordAnswer(ctx, in)
	require.NoError(t, err)

	got, err := f.cs.GetDecisionAnswersByResponseMessageIDs(ctx, []string{in.ResponseMessage.ID, uuid.NewString(), "not-a-uuid"})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Len(t, got[in.ResponseMessage.ID], 1, "a one-click response has one answer")
	assert.Equal(t, a.ID, got[in.ResponseMessage.ID][0].ID)
	assert.Equal(t, "no", got[in.ResponseMessage.ID][0].ChoiceLabel)
	assert.Equal(t, res.Decisions[0].RowID, got[in.ResponseMessage.ID][0].DecisionRowID)

	empty, err := f.cs.GetDecisionAnswersByResponseMessageIDs(ctx, nil)
	require.NoError(t, err)
	assert.Empty(t, empty)
}

// Deleting a project removes its decisions and answers and the key
// counters of the agents deleted with it; another project's rows stay.
func TestDeleteProject_RemovesDecisions(t *testing.T) {
	f := newDecisionFixture(t)
	ctx := context.Background()
	require.NoError(t, f.cs.CreateProject(ctx, &store.Project{ID: f.projectID, Name: "dp-" + f.projectID[:8], Slug: "dp-" + f.projectID[:8]}))
	a := makeAgent(f.projectID, "dp-agent-"+f.projectID[:8])
	require.NoError(t, f.cs.CreateAgent(ctx, a))
	f.agentID = a.ID
	res := f.create(t, false, yesNo("", "Numbered?"), yesNo("D1", "Keyed?"))
	_, _, err := f.cs.RecordAnswer(ctx, f.answer(res.Decisions[1].RowID, "1", "user-1", ""))
	require.NoError(t, err)

	other := newDecisionFixture(t)
	other.cs = f.cs
	otherRes := other.create(t, false, yesNo("D9", "Other project?"))

	require.NoError(t, f.cs.DeleteProject(ctx, f.projectID))

	for _, d := range res.Decisions {
		_, err := f.cs.GetDecision(ctx, d.RowID)
		assert.ErrorIs(t, err, store.ErrNotFound)
	}
	answers, err := f.cs.client.DecisionAnswer.Query().All(ctx)
	require.NoError(t, err)
	assert.Empty(t, answers)
	n, err := f.cs.client.DecisionKeyCounter.Query().Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, n, "the deleted agent's counter is gone")
	_, err = f.cs.GetDecision(ctx, otherRes.Decisions[0].RowID)
	assert.NoError(t, err, "another project's decision stays")
}

// Deleting a user clears answered_by_user_id on their answers and keeps the
// name and email snapshots.
func TestDeleteUser_ClearsAnswerUserKeepsSnapshots(t *testing.T) {
	f := newDecisionFixture(t)
	ctx := context.Background()
	userID := uuid.NewString()
	require.NoError(t, f.cs.CreateUser(ctx, &store.User{ID: userID, Email: "pat@example.com", Role: store.UserRoleMember, Status: store.UserStatusActive}))
	res := f.create(t, false, yesNo("D1", "Keep?"))
	_, _, err := f.cs.RecordAnswer(ctx, f.answer(res.Decisions[0].RowID, "1", userID, ""))
	require.NoError(t, err)

	require.NoError(t, f.cs.DeleteUser(ctx, userID))

	d, err := f.cs.GetDecision(ctx, res.Decisions[0].RowID)
	require.NoError(t, err)
	require.Len(t, d.Answers, 1)
	assert.Empty(t, d.Answers[0].AnsweredByUserID)
	assert.Equal(t, "Pat", d.Answers[0].AnsweredByName)
	assert.Equal(t, "pat@example.com", d.Answers[0].AnsweredByEmail)
}

// TestDeleteUser_FailedDeleteKeepsAnswerUser: the clearing of the user id
// on the answers and the user delete are one transaction, so a delete
// that fails (here: no such user) leaves the answers as they were.
func TestDeleteUser_FailedDeleteKeepsAnswerUser(t *testing.T) {
	f := newDecisionFixture(t)
	ctx := context.Background()
	userID := uuid.NewString() // no user row: the delete fails
	res := f.create(t, false, yesNo("D1", "Keep?"))
	_, _, err := f.cs.RecordAnswer(ctx, f.answer(res.Decisions[0].RowID, "1", userID, ""))
	require.NoError(t, err)

	err = f.cs.DeleteUser(ctx, userID)
	require.ErrorIs(t, err, store.ErrNotFound)

	d, err := f.cs.GetDecision(ctx, res.Decisions[0].RowID)
	require.NoError(t, err)
	require.Len(t, d.Answers, 1)
	assert.Equal(t, userID, d.Answers[0].AnsweredByUserID, "the failed delete rolled back the clear")
}

// interpretedAnswer is an agent_interpreted answer of userID.
func (f *decisionFixture) interpretedAnswer(rowID, choiceID, userID string) *store.RecordAnswerInput {
	in := f.answer(rowID, choiceID, userID, "")
	in.Provenance = store.DecisionProvenanceAgentInterpreted
	in.InterpretedByAgentID = f.agentID
	in.SourceMessageID = uuid.NewString()
	in.Surface = store.DecisionSurfaceSlack
	return in
}

// TestRecordAnswer_OutcomeNeedsInterpretedAnswer: confirm and override
// exist only over an agent_interpreted answer.
func TestRecordAnswer_OutcomeNeedsInterpretedAnswer(t *testing.T) {
	f := newDecisionFixture(t)
	ctx := context.Background()
	row := f.create(t, false, yesNo("D1", "q")).Decisions[0].RowID

	for _, outcome := range []string{store.DecisionOutcomeConfirm, store.DecisionOutcomeOverride} {
		in := f.answer(row, "1", "user-1", "")
		in.Outcome = outcome
		_, _, err := f.cs.RecordAnswer(ctx, in)
		assert.ErrorIs(t, err, store.ErrDecisionOutcomeMismatch, outcome)
	}
	// An interpreted answer is itself outcome answer: anything else is
	// malformed input, not a state mismatch.
	interp := f.interpretedAnswer(row, "1", "user-1")
	interp.Outcome = store.DecisionOutcomeConfirm
	_, _, err := f.cs.RecordAnswer(ctx, interp)
	assert.ErrorIs(t, err, store.ErrInvalidInput)

	d, err := f.cs.GetDecision(ctx, row)
	require.NoError(t, err)
	assert.Equal(t, store.DecisionStatusPending, d.Status)
	assert.Empty(t, d.Answers)

	// Positive control: outcome answer is recorded.
	_, _, err = f.cs.RecordAnswer(ctx, f.answer(row, "1", "user-1", ""))
	require.NoError(t, err)
}

// TestRecordAnswer_OverInterpretedOnlyByRecordedAnswerer: the store itself
// refuses a direct answer over an agent_interpreted one from any other
// user, with or without a fresh idempotency key, and checks its outcome.
func TestRecordAnswer_OverInterpretedOnlyByRecordedAnswerer(t *testing.T) {
	f := newDecisionFixture(t)
	ctx := context.Background()
	row := f.create(t, false, yesNo("D1", "q")).Decisions[0].RowID
	_, _, err := f.cs.RecordAnswer(ctx, f.interpretedAnswer(row, "1", "user-1"))
	require.NoError(t, err)

	for name, in := range map[string]*store.RecordAnswerInput{
		"third user, no key":         f.answer(row, "2", "user-3", ""),
		"third user, fresh key":      f.answer(row, "2", "user-3", "fresh-key"),
		"third user, as a confirm":   withOutcome(f.answer(row, "1", "user-3", "k2"), store.DecisionOutcomeConfirm),
		"third user, as an override": withOutcome(f.answer(row, "2", "user-3", ""), store.DecisionOutcomeOverride),
	} {
		_, _, err := f.cs.RecordAnswer(ctx, in)
		assert.ErrorIs(t, err, store.ErrNotRecordedAnswerer, name)
	}
	// The recorded answerer, with the wrong outcome.
	_, _, err = f.cs.RecordAnswer(ctx, f.answer(row, "1", "user-1", ""))
	assert.ErrorIs(t, err, store.ErrDecisionOutcomeMismatch, "outcome answer over an interpreted answer")
	_, _, err = f.cs.RecordAnswer(ctx, withOutcome(f.answer(row, "2", "user-1", ""), store.DecisionOutcomeConfirm))
	assert.ErrorIs(t, err, store.ErrDecisionOutcomeMismatch, "confirm with another choice")

	d, err := f.cs.GetDecision(ctx, row)
	require.NoError(t, err)
	require.Len(t, d.Answers, 1, "no refused answer was stored")

	// Positive control: the recorded answerer confirms.
	a, _, err := f.cs.RecordAnswer(ctx, withOutcome(f.answer(row, "1", "user-1", ""), store.DecisionOutcomeConfirm))
	require.NoError(t, err)
	assert.Equal(t, store.DecisionOutcomeConfirm, a.Outcome)
}

// TestRecordAnswer_InterpretedAfterDirectRefused: an agent_interpreted
// answer needs a version with no answer of either kind, so one after a
// direct answer is refused and nothing is stored.
func TestRecordAnswer_InterpretedAfterDirectRefused(t *testing.T) {
	f := newDecisionFixture(t)
	ctx := context.Background()
	row := f.create(t, false, yesNo("D1", "q")).Decisions[0].RowID
	_, _, err := f.cs.RecordAnswer(ctx, f.answer(row, "1", "user-1", ""))
	require.NoError(t, err)

	for name, in := range map[string]*store.RecordAnswerInput{
		"same user, same choice":    f.interpretedAnswer(row, "1", "user-1"),
		"same user, another choice": f.interpretedAnswer(row, "2", "user-1"),
		"another user":              f.interpretedAnswer(row, "2", "user-2"),
	} {
		_, _, err := f.cs.RecordAnswer(ctx, in)
		assert.ErrorIs(t, err, store.ErrDecisionAnswerConflict, name)
	}

	d, err := f.cs.GetDecision(ctx, row)
	require.NoError(t, err)
	require.Len(t, d.Answers, 1, "no interpreted answer was stored")
	assert.Equal(t, store.DecisionProvenanceDirect, d.Answers[0].Provenance)

	// Positive control: an interpreted answer to an unanswered version.
	row2 := f.create(t, false, yesNo("D2", "q")).Decisions[0].RowID
	_, _, err = f.cs.RecordAnswer(ctx, f.interpretedAnswer(row2, "1", "user-1"))
	require.NoError(t, err)
}

func withOutcome(in *store.RecordAnswerInput, outcome string) *store.RecordAnswerInput {
	in.Outcome = outcome
	return in
}

func TestGetDecisionRowsByIDs(t *testing.T) {
	f := newDecisionFixture(t)
	ctx := context.Background()
	res := f.create(t, false, yesNo("D1", "a"), yesNo("D2", "b"))
	a, b := res.Decisions[0].RowID, res.Decisions[1].RowID

	rows, err := f.cs.GetDecisionRowsByIDs(ctx, []string{a, b, "not-a-uuid", uuid.NewString()})
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, "D1", rows[a].Key)
	assert.Equal(t, "D2", rows[b].Key)
	assert.Empty(t, rows[a].Answers, "rows only")

	none, err := f.cs.GetDecisionRowsByIDs(ctx, nil)
	require.NoError(t, err)
	assert.Empty(t, none)
}

// ---- set submits (design choices v5, PR A2) ----

// threeDecisions is a set prompt: D1 and D3 recommend choice 1, D2 has no
// recommended choice.
func threeDecisions() []messages.Decision {
	d2 := yesNo("D2", "second?")
	d2.DefaultChoiceID = ""
	return []messages.Decision{yesNo("D1", "first?"), d2, yesNo("D3", "third?")}
}

// createSet stores a set prompt and returns its message id and row ids in
// position order.
func (f *decisionFixture) createSet(t *testing.T, ds ...messages.Decision) (string, []string) {
	t.Helper()
	in := f.prompt("", false, ds...)
	res, err := f.cs.CreateChoicePrompt(context.Background(), in)
	require.NoError(t, err)
	ids := make([]string, len(res.Decisions))
	for i, d := range res.Decisions {
		ids[i] = d.RowID
	}
	return in.Message.ID, ids
}

// answerSet is a set submit of userID with the given row and choice pairs.
func (f *decisionFixture) answerSet(msgID, userID, idem string, pairs ...string) *store.RecordAnswerSetInput {
	in := &store.RecordAnswerSetInput{
		MessageID:        msgID,
		AnsweredByUserID: userID,
		AnsweredByName:   "Pat",
		AnsweredByEmail:  "pat@example.com",
		IdempotencyKey:   idem,
		ResponseMessage: &store.Message{
			ID:             uuid.NewString(),
			ProjectID:      f.projectID,
			ConversationID: f.conversationID,
			Sender:         "user:pat@example.com",
			SenderID:       userID,
			Recipient:      "agent:asker",
			RecipientID:    f.agentID,
			AgentID:        f.agentID,
			Msg:            "Pat answered decisions",
			DispatchState:  store.MessageDispatchDispatched,
		},
	}
	for i := 0; i+1 < len(pairs); i += 2 {
		in.Answers = append(in.Answers, store.AnswerSetItem{DecisionRowID: pairs[i], ChoiceID: pairs[i+1]})
	}
	return in
}

// TestRecordAnswerSet_RecordsAllWithOneBatch: one transaction records an
// answer per row in position order with one batch id, matched_default from
// the recommended choice, one response message, and every row decided.
func TestRecordAnswerSet_RecordsAllWithOneBatch(t *testing.T) {
	f := newDecisionFixture(t)
	ctx := context.Background()
	msgID, rows := f.createSet(t, threeDecisions()...)

	in := f.answerSet(msgID, "user-1", "", rows[2], "2", rows[0], "1", rows[1], "2")
	res, err := f.cs.RecordAnswerSet(ctx, in)
	require.NoError(t, err)
	require.NotEmpty(t, res.BatchID)
	assert.False(t, res.Replayed)
	require.Len(t, res.Answers, 3)
	for i, a := range res.Answers {
		assert.Equal(t, rows[i], a.DecisionRowID, "position order")
		assert.Equal(t, res.BatchID, a.BatchID)
		assert.Equal(t, in.ResponseMessage.ID, a.ResponseMessageID)
		assert.Equal(t, store.DecisionProvenanceDirect, a.Provenance)
		assert.Equal(t, store.DecisionOutcomeAnswer, a.Outcome)
		assert.Equal(t, store.DecisionSurfaceNative, a.Surface)
	}
	assert.True(t, res.Answers[0].MatchedDefault, "D1 answered with its recommended choice")
	assert.False(t, res.Answers[1].MatchedDefault, "D2 has no recommended choice")
	assert.False(t, res.Answers[2].MatchedDefault, "D3 answered against its recommended choice")
	assert.Equal(t, "no", res.Answers[2].ChoiceLabel, "the label comes from the stored row")

	for _, id := range rows {
		d, err := f.cs.GetDecision(ctx, id)
		require.NoError(t, err)
		assert.Equal(t, store.DecisionStatusDecided, d.Status)
		require.Len(t, d.Answers, 1)
		assert.Equal(t, res.BatchID, d.Answers[0].BatchID)
	}
	m, err := f.cs.GetMessage(ctx, in.ResponseMessage.ID)
	require.NoError(t, err)
	assert.Equal(t, messages.TypeChoiceResponse, m.Type)
}

// TestRecordAnswerSet_ChangedSetStoresNothing: every difference from the
// actionable set is refused with a reason per row, and nothing is stored.
func TestRecordAnswerSet_ChangedSetStoresNothing(t *testing.T) {
	ctx := context.Background()
	cases := map[string]struct {
		setup  func(f *decisionFixture, rows []string)
		pairs  func(rows []string) []string
		reason func(rows []string) map[string]string
	}{
		"missing row": {
			setup:  func(*decisionFixture, []string) {},
			pairs:  func(r []string) []string { return []string{r[0], "1", r[1], "1"} },
			reason: func(r []string) map[string]string { return map[string]string{r[2]: store.AnswerSetReasonMissing} },
		},
		"decided row": {
			setup: func(f *decisionFixture, r []string) {
				_, _, err := f.cs.RecordAnswer(ctx, f.answer(r[0], "1", "user-2", ""))
				require.NoError(t, err)
			},
			pairs:  func(r []string) []string { return []string{r[0], "1", r[1], "1", r[2], "1"} },
			reason: func(r []string) map[string]string { return map[string]string{r[0]: store.AnswerSetReasonDecided} },
		},
		"foreign interpreted row": {
			setup: func(f *decisionFixture, r []string) {
				_, _, err := f.cs.RecordAnswer(ctx, f.interpretedAnswer(r[1], "1", "user-2"))
				require.NoError(t, err)
			},
			pairs: func(r []string) []string { return []string{r[0], "1", r[1], "1", r[2], "1"} },
			reason: func(r []string) map[string]string {
				return map[string]string{r[1]: store.AnswerSetReasonNotRecordedAnswerer}
			},
		},
		"replaced row": {
			setup: func(f *decisionFixture, r []string) {
				f.create(t, true, yesNo("D3", "third, again?"))
			},
			pairs:  func(r []string) []string { return []string{r[0], "1", r[1], "1", r[2], "1"} },
			reason: func(r []string) map[string]string { return map[string]string{r[2]: store.AnswerSetReasonReplaced} },
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newDecisionFixture(t)
			msgID, rows := f.createSet(t, threeDecisions()...)
			tc.setup(f, rows)
			before := map[string]int{}
			for _, id := range rows {
				d, err := f.cs.GetDecision(ctx, id)
				require.NoError(t, err)
				before[id] = len(d.Answers)
			}

			in := f.answerSet(msgID, "user-1", "", tc.pairs(rows)...)
			_, err := f.cs.RecordAnswerSet(ctx, in)
			require.ErrorIs(t, err, store.ErrAnswerSetChanged)
			var changed *store.AnswerSetChangedError
			require.ErrorAs(t, err, &changed)
			assert.Equal(t, tc.reason(rows), changed.Reasons)

			for _, id := range rows {
				d, err := f.cs.GetDecision(ctx, id)
				require.NoError(t, err)
				assert.Len(t, d.Answers, before[id], "no answer stored")
			}
			_, err = f.cs.GetMessage(ctx, in.ResponseMessage.ID)
			assert.ErrorIs(t, err, store.ErrNotFound, "no response message stored")
		})
	}
}

// TestRecordAnswerSet_SmallerSetAfterUpdate: after an --update repost of one
// key, the old prompt's remaining decisions submit as a smaller set.
func TestRecordAnswerSet_SmallerSetAfterUpdate(t *testing.T) {
	f := newDecisionFixture(t)
	ctx := context.Background()
	msgID, rows := f.createSet(t, threeDecisions()...)
	f.create(t, true, yesNo("D2", "second, again?"))

	res, err := f.cs.RecordAnswerSet(ctx, f.answerSet(msgID, "user-1", "", rows[0], "1", rows[2], "1"))
	require.NoError(t, err)
	require.Len(t, res.Answers, 2)
	assert.Equal(t, rows[0], res.Answers[0].DecisionRowID)
	assert.Equal(t, rows[2], res.Answers[1].DecisionRowID)
}

// TestRecordAnswerSet_ShapeErrors: malformed submits are invalid input and
// store nothing.
func TestRecordAnswerSet_ShapeErrors(t *testing.T) {
	f := newDecisionFixture(t)
	ctx := context.Background()
	msgID, rows := f.createSet(t, threeDecisions()...)
	otherMsg, otherRows := f.createSet(t, yesNo("E1", "other?"), yesNo("E2", "other two?"))
	_ = otherMsg

	cases := map[string][]string{
		"empty":          nil,
		"duplicate row":  {rows[0], "1", rows[0], "2", rows[1], "1", rows[2], "1"},
		"row not in M":   {rows[0], "1", rows[1], "1", rows[2], "1", otherRows[0], "1"},
		"unknown choice": {rows[0], "9", rows[1], "1", rows[2], "1"},
	}
	for name, pairs := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := f.cs.RecordAnswerSet(ctx, f.answerSet(msgID, "user-1", "", pairs...))
			assert.ErrorIs(t, err, store.ErrInvalidInput)
		})
	}
	_, err := f.cs.RecordAnswerSet(ctx, f.answerSet(uuid.NewString(), "user-1", "", rows[0], "1"))
	assert.ErrorIs(t, err, store.ErrNotFound, "a message with no decisions")
	for _, id := range rows {
		d, err := f.cs.GetDecision(ctx, id)
		require.NoError(t, err)
		assert.Empty(t, d.Answers)
	}
}

// TestRecordAnswerSet_IdempotencyPerBatch: the same key and answers replay
// the recorded set; the same key with other answers is a mismatch.
func TestRecordAnswerSet_IdempotencyPerBatch(t *testing.T) {
	f := newDecisionFixture(t)
	ctx := context.Background()
	msgID, rows := f.createSet(t, threeDecisions()...)
	pairs := []string{rows[0], "1", rows[1], "2", rows[2], "1"}

	first, err := f.cs.RecordAnswerSet(ctx, f.answerSet(msgID, "user-1", "k1", pairs...))
	require.NoError(t, err)
	again, err := f.cs.RecordAnswerSet(ctx, f.answerSet(msgID, "user-1", "k1", pairs...))
	require.NoError(t, err)
	assert.True(t, again.Replayed)
	assert.Equal(t, first.BatchID, again.BatchID)
	require.Len(t, again.Answers, 3)
	assert.Equal(t, rows[0], again.Answers[0].DecisionRowID, "replay in position order")

	_, err = f.cs.RecordAnswerSet(ctx, f.answerSet(msgID, "user-1", "k1", rows[0], "2", rows[1], "2", rows[2], "1"))
	assert.ErrorIs(t, err, store.ErrIdempotencyMismatch)
	// The key is scoped to the user: another user reusing it is not a
	// mismatch but a fresh submit, here of a fully decided set.
	_, err = f.cs.RecordAnswerSet(ctx, f.answerSet(msgID, "user-2", "k1", pairs...))
	assert.NotErrorIs(t, err, store.ErrIdempotencyMismatch, "another user's key is not a mismatch")
	var changed *store.AnswerSetChangedError
	require.ErrorAs(t, err, &changed)
	for _, id := range rows {
		assert.Equal(t, store.AnswerSetReasonDecided, changed.Reasons[id])
	}
}

// TestRecordAnswerSet_ConfirmOwnInterpreted: a set answer equal to the
// user's own interpreted answer is a confirm; the row stays decided with
// both answers, and matched_default follows the recommended choice.
func TestRecordAnswerSet_ConfirmOwnInterpreted(t *testing.T) {
	f := newDecisionFixture(t)
	ctx := context.Background()
	msgID, rows := f.createSet(t, threeDecisions()...)
	_, _, err := f.cs.RecordAnswer(ctx, f.interpretedAnswer(rows[0], "1", "user-1"))
	require.NoError(t, err)

	in := f.answerSet(msgID, "user-1", "", rows[0], "1", rows[1], "1", rows[2], "1")
	in.Answers[0].Outcome = store.DecisionOutcomeConfirm
	res, err := f.cs.RecordAnswerSet(ctx, in)
	require.NoError(t, err)
	assert.Equal(t, store.DecisionOutcomeConfirm, res.Answers[0].Outcome)
	assert.True(t, res.Answers[0].MatchedDefault, "D1 confirmed with its recommended choice")

	d, err := f.cs.GetDecision(ctx, rows[0])
	require.NoError(t, err)
	assert.Equal(t, store.DecisionStatusDecided, d.Status)
	assert.Len(t, d.Answers, 2, "the interpreted answer and the confirm")
}

// TestRecordAnswerSet_OutcomeOnPendingRow: a confirm or override on a row
// with no interpreted answer is not actionable as asked.
func TestRecordAnswerSet_OutcomeOnPendingRow(t *testing.T) {
	f := newDecisionFixture(t)
	ctx := context.Background()
	msgID, rows := f.createSet(t, threeDecisions()...)
	in := f.answerSet(msgID, "user-1", "", rows[0], "1", rows[1], "1", rows[2], "1")
	in.Answers[1].Outcome = store.DecisionOutcomeConfirm
	_, err := f.cs.RecordAnswerSet(ctx, in)
	var changed *store.AnswerSetChangedError
	require.ErrorAs(t, err, &changed)
	assert.Equal(t, map[string]string{rows[1]: store.AnswerSetReasonNotActionable}, changed.Reasons)
}

// TestRecordAnswerSet_OwnInterpretedRows: a row with the user's own
// interpreted answer is actionable, as a confirm or an override, and a
// stale expected outcome is a changed set.
func TestRecordAnswerSet_OwnInterpretedRows(t *testing.T) {
	f := newDecisionFixture(t)
	ctx := context.Background()
	msgID, rows := f.createSet(t, threeDecisions()...)
	_, _, err := f.cs.RecordAnswer(ctx, f.interpretedAnswer(rows[0], "1", "user-1"))
	require.NoError(t, err)

	stale := f.answerSet(msgID, "user-1", "", rows[0], "1", rows[1], "1", rows[2], "1")
	_, err = f.cs.RecordAnswerSet(ctx, stale)
	var changed *store.AnswerSetChangedError
	require.ErrorAs(t, err, &changed, "outcome answer over an interpreted answer is stale")
	assert.Equal(t, map[string]string{rows[0]: store.AnswerSetReasonDecided}, changed.Reasons)

	in := f.answerSet(msgID, "user-1", "", rows[0], "2", rows[1], "1", rows[2], "1")
	in.Answers[0].Outcome = store.DecisionOutcomeOverride
	res, err := f.cs.RecordAnswerSet(ctx, in)
	require.NoError(t, err)
	assert.Equal(t, store.DecisionOutcomeOverride, res.Answers[0].Outcome)
	assert.Equal(t, store.DecisionOutcomeAnswer, res.Answers[1].Outcome)
}

// TestRecordAnswer_MatchedDefault: a one-click answer also records whether
// it chose the recommended choice.
func TestRecordAnswer_MatchedDefault(t *testing.T) {
	f := newDecisionFixture(t)
	ctx := context.Background()
	res := f.create(t, false, yesNo("D1", "q"), yesNo("D2", "q2"))
	a, _, err := f.cs.RecordAnswer(ctx, f.answer(res.Decisions[0].RowID, "1", "user-1", ""))
	require.NoError(t, err)
	assert.True(t, a.MatchedDefault)
	assert.Empty(t, a.BatchID, "a one-click answer has no batch")
	b, _, err := f.cs.RecordAnswer(ctx, f.answer(res.Decisions[1].RowID, "2", "user-1", ""))
	require.NoError(t, err)
	assert.False(t, b.MatchedDefault)
}

// ---- agent decision list, show, and interpreted answers (PR 2A) ----

func (f *decisionFixture) interpretedBatch(sourceMsgID, userID, idem string, pairs ...store.InterpretedAnswerPair) *store.RecordInterpretedInput {
	return &store.RecordInterpretedInput{
		InterpretedByAgentID: f.agentID,
		SourceMessageID:      sourceMsgID,
		AnsweredByUserID:     userID,
		AnsweredByName:       "Pat",
		AnsweredByEmail:      "pat@example.com",
		Surface:              store.DecisionSurfaceNative,
		Answers:              pairs,
		IdempotencyKey:       idem,
		ResponseMessage: &store.Message{
			ID:             uuid.NewString(),
			ProjectID:      f.projectID,
			ConversationID: f.conversationID,
			Sender:         "agent:asker",
			SenderID:       f.agentID,
			Recipient:      "user:pat@example.com",
			RecipientID:    userID,
			AgentID:        f.agentID,
			DispatchState:  store.MessageDispatchDispatched,
		},
		FormatMessage: func(items []store.InterpretedFormattedItem) string {
			parts := make([]string, len(items))
			for i, it := range items {
				parts[i] = it.Key + "=" + it.ChoiceLabel
			}
			return "interpreted: " + strings.Join(parts, ", ")
		},
	}
}

func TestListAgentDecisions(t *testing.T) {
	f := newDecisionFixture(t)
	ctx := context.Background()

	// Prompt 1: D1 (later replaced by v2) and D2 (later decided).
	p1 := f.create(t, false, yesNo("D1", "q1"), yesNo("D2", "q2"))
	_, _, err := f.cs.RecordAnswer(ctx, f.answer(p1.Decisions[1].RowID, "2", "user-1", ""))
	require.NoError(t, err)

	// Prompt 2: D1 v2 (pending) + D3, D4 (same prompt -> identical CreatedAt ties).
	p2 := f.create(t, true, yesNo("D1", "q1 v2"), yesNo("D3", "q3"), yesNo("D4", "q4"))
	require.Len(t, p2.Decisions, 3)

	// Another agent's decision must never appear in f.agentID's list.
	otherAgent := uuid.NewString()
	otherIn := f.prompt("", false, yesNo("OTHER", "other?"))
	otherIn.AskerAgentID = otherAgent
	otherIn.Message.SenderID = otherAgent
	otherIn.Message.AgentID = otherAgent
	_, err = f.cs.CreateChoicePrompt(ctx, otherIn)
	require.NoError(t, err)

	// Default state ("") == pending: D1 v2, D3, D4 (3 rows).
	pending, cur, err := f.cs.ListAgentDecisions(ctx, f.agentID, "", "", 0)
	require.NoError(t, err)
	assert.Empty(t, cur)
	require.Len(t, pending, 3)
	for _, d := range pending {
		assert.Equal(t, store.DecisionStatusPending, d.Status)
		assert.Equal(t, f.agentID, d.AskerAgentID)
	}

	// Decided: D2 with its answer loaded.
	decided, cur, err := f.cs.ListAgentDecisions(ctx, f.agentID, store.DecisionListStateDecided, "", 50)
	require.NoError(t, err)
	assert.Empty(t, cur)
	require.Len(t, decided, 1)
	assert.Equal(t, "D2", decided[0].Key)
	require.Len(t, decided[0].Answers, 1)
	assert.Equal(t, "no", decided[0].Answers[0].ChoiceLabel)

	// All with limit=2 paginates across all 5 versions (including replaced D1 v1)
	// and handles identical CreatedAt ties without duplicates or drops.
	var seenIDs []string
	pageCursor := ""
	for {
		page, next, err := f.cs.ListAgentDecisions(ctx, f.agentID, store.DecisionListStateAll, pageCursor, 2)
		require.NoError(t, err)
		for _, d := range page {
			seenIDs = append(seenIDs, d.ID)
		}
		if next == "" {
			break
		}
		pageCursor = next
	}
	require.Len(t, seenIDs, 5)
	uniq := map[string]bool{}
	for _, id := range seenIDs {
		uniq[id] = true
	}
	assert.Len(t, uniq, 5, "cursor pagination across timestamp ties must visit each row once")

	// Validation errors: bad state, bad limit, bad cursor.
	_, _, err = f.cs.ListAgentDecisions(ctx, f.agentID, "bogus", "", 10)
	assert.ErrorIs(t, err, store.ErrInvalidInput)
	_, _, err = f.cs.ListAgentDecisions(ctx, f.agentID, store.DecisionListStatePending, "", 201)
	assert.ErrorIs(t, err, store.ErrInvalidInput)
	_, _, err = f.cs.ListAgentDecisions(ctx, f.agentID, store.DecisionListStatePending, "not-a-cursor", 10)
	assert.ErrorIs(t, err, store.ErrInvalidInput)
}

func TestGetAgentDecisionVersions(t *testing.T) {
	f := newDecisionFixture(t)
	ctx := context.Background()

	// v1 gets an interpreted answer then a direct override -> 2 answer rows.
	v1 := f.create(t, false, yesNo("D234", "ship v1?")).Decisions[0].RowID
	_, _, err := f.cs.RecordAnswer(ctx, f.interpretedAnswer(v1, "1", "user-1"))
	require.NoError(t, err)
	overrideIn := f.answer(v1, "2", "user-1", "")
	overrideIn.Outcome = store.DecisionOutcomeOverride
	_, _, err = f.cs.RecordAnswer(ctx, overrideIn)
	require.NoError(t, err)

	// Repost with --update -> v2 pending.
	f.create(t, true, yesNo("D234", "ship v2?"))

	// Case-insensitive lookup returns [v1, v2] oldest-first with BOTH answers on v1.
	versions, err := f.cs.GetAgentDecisionVersions(ctx, f.agentID, "d234")
	require.NoError(t, err)
	require.Len(t, versions, 2)
	assert.Equal(t, 1, versions[0].Version)
	assert.Equal(t, store.DecisionStatusReplaced, versions[0].Status)
	require.Len(t, versions[0].Answers, 2)
	assert.Equal(t, store.DecisionProvenanceAgentInterpreted, versions[0].Answers[0].Provenance)
	assert.Equal(t, store.DecisionProvenanceDirect, versions[0].Answers[1].Provenance)
	assert.Equal(t, store.DecisionOutcomeOverride, versions[0].Answers[1].Outcome)
	assert.Equal(t, 2, versions[1].Version)
	assert.Equal(t, store.DecisionStatusPending, versions[1].Status)

	// Unknown key or another agent's lookup -> ErrNotFound.
	_, err = f.cs.GetAgentDecisionVersions(ctx, f.agentID, "NOPE")
	assert.ErrorIs(t, err, store.ErrNotFound)
	_, err = f.cs.GetAgentDecisionVersions(ctx, uuid.NewString(), "D234")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestRecordInterpretedAnswers_SingleAndMultiPromptPartialSet(t *testing.T) {
	f := newDecisionFixture(t)
	ctx := context.Background()
	srcMsgID := uuid.NewString()

	// Single pair: BatchID is empty, MatchedDefault true when matching default.
	p0 := f.create(t, false, yesNo("SOLO", "ship solo?"))
	singleIn := f.interpretedBatch(srcMsgID, "user-1", "idem-solo",
		store.InterpretedAnswerPair{DecisionRowID: p0.Decisions[0].RowID, Key: "SOLO", ChoiceID: "1"},
	)
	singleRes, err := f.cs.RecordInterpretedAnswers(ctx, singleIn)
	require.NoError(t, err)
	assert.Empty(t, singleRes.BatchID, "1 pair must have empty batch_id")
	assert.False(t, singleRes.Replayed)
	require.Len(t, singleRes.Answers, 1)
	assert.Equal(t, store.DecisionProvenanceAgentInterpreted, singleRes.Answers[0].Provenance)
	assert.Equal(t, store.DecisionOutcomeAnswer, singleRes.Answers[0].Outcome)
	assert.True(t, singleRes.Answers[0].MatchedDefault)
	assert.Equal(t, srcMsgID, singleRes.Answers[0].SourceMessageID)
	assert.Equal(t, f.agentID, singleRes.Answers[0].InterpretedByAgentID)
	assert.Equal(t, "user-1", singleRes.Answers[0].AnsweredByUserID)
	assert.Equal(t, "Pat", singleRes.Answers[0].AnsweredByName)
	assert.Equal(t, "pat@example.com", singleRes.Answers[0].AnsweredByEmail)

	soloStored, err := f.cs.GetDecision(ctx, p0.Decisions[0].RowID)
	require.NoError(t, err)
	require.Len(t, soloStored.Answers, 1)
	assert.Equal(t, "user-1", soloStored.Answers[0].AnsweredByUserID)
	assert.Equal(t, "Pat", soloStored.Answers[0].AnsweredByName)
	assert.Equal(t, "pat@example.com", soloStored.Answers[0].AnsweredByEmail)

	msg, err := f.cs.GetMessage(ctx, singleIn.ResponseMessage.ID)
	require.NoError(t, err)
	assert.Equal(t, messages.TypeChoiceResponse, msg.Type)
	assert.Equal(t, "interpreted: SOLO=yes", msg.Msg)

	// Multi-prompt + partial set: prompt A has (D234, D999), prompt B has (17).
	// Pass pairs in REVERSE prompt order (hubKey first, d234 second): both
	// stored Msg and returned Answers must be canonically ordered [D234, hubKey].
	pA := f.create(t, false, yesNo("D234", "first?"), yesNo("D999", "leave pending?"))
	time.Sleep(2 * time.Millisecond)
	pB := f.create(t, false, yesNo("", "hub key?"))
	hubKey := pB.Decisions[0].Key

	batchIn := f.interpretedBatch(srcMsgID, "user-1", "idem-batch",
		store.InterpretedAnswerPair{Key: hubKey, ChoiceID: "1"},
		store.InterpretedAnswerPair{Key: "d234", ChoiceID: "2"},
	)
	batchRes, err := f.cs.RecordInterpretedAnswers(ctx, batchIn)
	require.NoError(t, err)
	assert.NotEmpty(t, batchRes.BatchID, "2+ pairs share a batch_id")
	require.Len(t, batchRes.Answers, 2)
	assert.Equal(t, "D234", batchRes.Answers[0].Key, "earlier prompt comes first")
	assert.Equal(t, hubKey, batchRes.Answers[1].Key, "later prompt comes second")
	assert.Equal(t, batchRes.BatchID, batchRes.Answers[0].BatchID)
	assert.Equal(t, batchRes.BatchID, batchRes.Answers[1].BatchID)
	assert.False(t, batchRes.Answers[0].MatchedDefault)
	assert.True(t, batchRes.Answers[1].MatchedDefault)

	batchMsg, err := f.cs.GetMessage(ctx, batchIn.ResponseMessage.ID)
	require.NoError(t, err)
	assert.Equal(t, "interpreted: D234=no, "+hubKey+"=yes", batchMsg.Msg)

	// Verify D999 is still pending while D234 and hubKey are decided.
	d999, err := f.cs.GetDecision(ctx, pA.Decisions[1].RowID)
	require.NoError(t, err)
	assert.Equal(t, store.DecisionStatusPending, d999.Status)
	assert.Empty(t, d999.Answers)
}

func TestRecordInterpretedAnswers_AllOrNothingRollback(t *testing.T) {
	f := newDecisionFixture(t)
	ctx := context.Background()
	srcMsgID := uuid.NewString()

	p := f.create(t, false, yesNo("K1", "pending?"), yesNo("K2", "already answered?"), yesNo("K3", "bad choice?"))
	_, _, err := f.cs.RecordAnswer(ctx, f.answer(p.Decisions[1].RowID, "1", "user-1", ""))
	require.NoError(t, err)

	beforeMsgs, err := f.cs.client.Message.Query().Where(message.TypeEQ(messages.TypeChoiceResponse)).Count(ctx)
	require.NoError(t, err)

	// K1 is valid, K2 is already_answered, K3 has invalid_choice "99".
	in := f.interpretedBatch(srcMsgID, "user-1", "",
		store.InterpretedAnswerPair{Key: "K1", ChoiceID: "1"},
		store.InterpretedAnswerPair{Key: "K2", ChoiceID: "1"},
		store.InterpretedAnswerPair{Key: "K3", ChoiceID: "99"},
	)
	_, err = f.cs.RecordInterpretedAnswers(ctx, in)
	var iaErr *store.InterpretedAnswersError
	require.ErrorAs(t, err, &iaErr)
	require.Len(t, iaErr.Failures, 2, "both failing keys reported together")
	assert.Equal(t, "K2", iaErr.Failures[0].Key)
	assert.Equal(t, store.InterpretedReasonAlreadyAnswered, iaErr.Failures[0].Reason)
	require.NotNil(t, iaErr.Failures[0].Decision)
	require.Len(t, iaErr.Failures[0].Decision.Answers, 1)
	assert.Equal(t, "K3", iaErr.Failures[1].Key)
	assert.Equal(t, store.InterpretedReasonInvalidChoice, iaErr.Failures[1].Reason)
	assert.Equal(t, "99", iaErr.Failures[1].ChoiceID)

	// K1 is still untouched, and 0 new choice-response messages were stored.
	k1, err := f.cs.GetDecision(ctx, p.Decisions[0].RowID)
	require.NoError(t, err)
	assert.Equal(t, store.DecisionStatusPending, k1.Status)
	assert.Empty(t, k1.Answers)

	afterMsgs, err := f.cs.client.Message.Query().Where(message.TypeEQ(messages.TypeChoiceResponse)).Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, beforeMsgs, afterMsgs, "0 choice-response messages committed on rollback")
}

func TestRecordInterpretedAnswers_Idempotency(t *testing.T) {
	f := newDecisionFixture(t)
	ctx := context.Background()
	srcMsgID := uuid.NewString()

	p := f.create(t, false, yesNo("D1", "first?"), yesNo("D2", "second?"), yesNo("D3", "third?"))
	first := f.interpretedBatch(srcMsgID, "user-1", "idem-1",
		store.InterpretedAnswerPair{DecisionRowID: p.Decisions[0].RowID, Key: "D1", ChoiceID: "1"},
		store.InterpretedAnswerPair{DecisionRowID: p.Decisions[1].RowID, Key: "D2", ChoiceID: "2"},
	)
	res1, err := f.cs.RecordInterpretedAnswers(ctx, first)
	require.NoError(t, err)
	assert.False(t, res1.Replayed)

	// User confirms D1 and agent reposts D2 (--update v2), then agent retries idem-1:
	// replay still returns 200 with original batch_id and response_message_id!
	confirmIn := f.answer(p.Decisions[0].RowID, "1", "user-1", "")
	confirmIn.Outcome = store.DecisionOutcomeConfirm
	_, _, err = f.cs.RecordAnswer(ctx, confirmIn)
	require.NoError(t, err)
	f.create(t, true, yesNo("D2", "second v2?"))

	retry := f.interpretedBatch(srcMsgID, "user-1", "idem-1",
		store.InterpretedAnswerPair{Key: "D1", ChoiceID: "1"},
		store.InterpretedAnswerPair{Key: "D2", ChoiceID: "2"},
	)
	res2, err := f.cs.RecordInterpretedAnswers(ctx, retry)
	require.NoError(t, err)
	assert.True(t, res2.Replayed)
	assert.Equal(t, res1.BatchID, res2.BatchID)
	assert.Equal(t, first.ResponseMessage.ID, res2.Answers[0].ResponseMessageID)

	// Different choice -> ErrIdempotencyMismatch.
	diffChoice := f.interpretedBatch(srcMsgID, "user-1", "idem-1",
		store.InterpretedAnswerPair{Key: "D1", ChoiceID: "2"},
		store.InterpretedAnswerPair{Key: "D2", ChoiceID: "2"},
	)
	_, err = f.cs.RecordInterpretedAnswers(ctx, diffChoice)
	assert.ErrorIs(t, err, store.ErrIdempotencyMismatch)

	// Different key -> ErrIdempotencyMismatch.
	diffKey := f.interpretedBatch(srcMsgID, "user-1", "idem-1",
		store.InterpretedAnswerPair{Key: "D3", ChoiceID: "1"},
	)
	_, err = f.cs.RecordInterpretedAnswers(ctx, diffKey)
	assert.ErrorIs(t, err, store.ErrIdempotencyMismatch)

	// Different source_message_id -> ErrIdempotencyMismatch.
	diffSrc := f.interpretedBatch(uuid.NewString(), "user-1", "idem-1",
		store.InterpretedAnswerPair{Key: "D1", ChoiceID: "1"},
		store.InterpretedAnswerPair{Key: "D2", ChoiceID: "2"},
	)
	_, err = f.cs.RecordInterpretedAnswers(ctx, diffSrc)
	assert.ErrorIs(t, err, store.ErrIdempotencyMismatch)

	// Read-only FindInterpretedReplay: unused key -> (nil, nil); exact match
	// (even with reversed pair order) -> Replayed=true in canonical [D1, D2]
	// order; malformed source_message_id or mismatched choice -> ErrIdempotencyMismatch.
	miss, err := f.cs.FindInterpretedReplay(ctx, f.agentID, "unused-key", srcMsgID, []store.InterpretedAnswerPair{
		{Key: "D1", ChoiceID: "1"},
	})
	require.NoError(t, err)
	assert.Nil(t, miss)

	roReplay, err := f.cs.FindInterpretedReplay(ctx, f.agentID, "idem-1", srcMsgID, []store.InterpretedAnswerPair{
		{Key: "D2", ChoiceID: "2"},
		{Key: "D1", ChoiceID: "1"},
	})
	require.NoError(t, err)
	require.NotNil(t, roReplay)
	assert.True(t, roReplay.Replayed)
	assert.Equal(t, res1.BatchID, roReplay.BatchID)
	require.Len(t, roReplay.Answers, 2)
	assert.Equal(t, "D1", roReplay.Answers[0].Key)
	assert.Equal(t, "D2", roReplay.Answers[1].Key)

	_, err = f.cs.FindInterpretedReplay(ctx, f.agentID, "idem-1", "not-a-uuid", []store.InterpretedAnswerPair{
		{Key: "D1", ChoiceID: "1"},
		{Key: "D2", ChoiceID: "2"},
	})
	assert.ErrorIs(t, err, store.ErrIdempotencyMismatch)
}

func TestRecordInterpretedAnswers_ConcurrentRaceWithDirectAnswer(t *testing.T) {
	f := newDecisionFixture(t)
	ctx := context.Background()

	for round := 0; round < 10; round++ {
		key := fmt.Sprintf("RACE-%d", round)
		rowID := f.create(t, false, yesNo(key, "race?")).Decisions[0].RowID
		srcMsgID := uuid.NewString()

		start := make(chan struct{})
		var wg sync.WaitGroup
		var interpErr, directErr error

		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, interpErr = f.cs.RecordInterpretedAnswers(ctx, f.interpretedBatch(srcMsgID, "user-1", "",
				store.InterpretedAnswerPair{DecisionRowID: rowID, Key: key, ChoiceID: "1"},
			))
		}()
		go func() {
			defer wg.Done()
			<-start
			_, _, directErr = f.cs.RecordAnswer(ctx, f.answer(rowID, "2", "user-1", ""))
		}()
		close(start)
		wg.Wait()

		// Exactly one of the two concurrent pending-row writes succeeds; the loser
		// sees the winner's answer and fails cleanly with conflict/mismatch.
		assert.NotEqual(t, interpErr == nil, directErr == nil,
			"round %d: interpErr=%v directErr=%v", round, interpErr, directErr)

		d, err := f.cs.GetDecision(ctx, rowID)
		require.NoError(t, err)
		assert.Equal(t, store.DecisionStatusDecided, d.Status)
		assert.Len(t, d.Answers, 1)
	}
}

func TestRecordInterpretedAnswers_KeyOnlyPairIsAgentScoped(t *testing.T) {
	f := newDecisionFixture(t)
	ctx := context.Background()
	otherAgent := uuid.NewString()
	otherIn := f.prompt("", false, yesNo("SHARED", "other agent asks"))
	otherIn.AskerAgentID = otherAgent
	otherIn.Message.SenderID = otherAgent
	otherIn.Message.AgentID = otherAgent
	otherRes, err := f.cs.CreateChoicePrompt(ctx, otherIn)
	require.NoError(t, err)

	_, err = f.cs.RecordInterpretedAnswers(ctx, f.interpretedBatch(uuid.NewString(), "user-1", "",
		store.InterpretedAnswerPair{Key: "SHARED", ChoiceID: "1"},
	))
	var iaErr *store.InterpretedAnswersError
	require.ErrorAs(t, err, &iaErr)
	require.Len(t, iaErr.Failures, 1)
	assert.Equal(t, store.InterpretedReasonNotFound, iaErr.Failures[0].Reason)

	d, err := f.cs.GetDecision(ctx, otherRes.Decisions[0].RowID)
	require.NoError(t, err)
	assert.Equal(t, store.DecisionStatusPending, d.Status)
	assert.Empty(t, d.Answers)
}

func TestRecordInterpretedAnswers_ReplacedVersion(t *testing.T) {
	f := newDecisionFixture(t)
	ctx := context.Background()
	v1 := f.create(t, false, yesNo("REP", "v1?")).Decisions[0].RowID
	f.create(t, true, yesNo("REP", "v2?"))

	_, err := f.cs.RecordInterpretedAnswers(ctx, f.interpretedBatch(uuid.NewString(), "user-1", "",
		store.InterpretedAnswerPair{DecisionRowID: v1, Key: "REP", ChoiceID: "1"},
	))
	var iaErr *store.InterpretedAnswersError
	require.ErrorAs(t, err, &iaErr)
	require.Len(t, iaErr.Failures, 1)
	assert.Equal(t, store.InterpretedReasonReplaced, iaErr.Failures[0].Reason)
	require.NotNil(t, iaErr.Failures[0].Decision)
	assert.NotEmpty(t, iaErr.Failures[0].Decision.ReplacedByID)
}
