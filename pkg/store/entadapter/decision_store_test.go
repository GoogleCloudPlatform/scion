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

func (f *decisionFixture) prompt(intro string, update bool, ds ...messages.Decision) *store.DecisionPromptInput {
	return &store.DecisionPromptInput{
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

func (f *decisionFixture) create(t *testing.T, update bool, ds ...messages.Decision) *store.DecisionPromptResult {
	t.Helper()
	res, err := f.cs.CreateDecisionPrompt(context.Background(), f.prompt("", update, ds...))
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

func TestCreateDecisionPrompt_StoresMessageAndDecisions(t *testing.T) {
	f := newDecisionFixture(t)
	ctx := context.Background()
	in := f.prompt("Two things 🚀", false, yesNo("D234", "Ship it?"), yesNo("", "Which approach?"))
	res, err := f.cs.CreateDecisionPrompt(ctx, in)
	require.NoError(t, err)
	require.Len(t, res.Decisions, 2)

	assert.Equal(t, "D234", res.Decisions[0].Key)
	assert.Equal(t, store.DecisionKeySourceAgent, res.Decisions[0].KeySource)
	assert.Equal(t, 1, res.Decisions[0].Version)
	assert.Nil(t, res.Decisions[0].Replaced)
	assert.Equal(t, "1", res.Decisions[1].Key)
	assert.Equal(t, store.DecisionKeySourceHub, res.Decisions[1].KeySource)

	// The message is stored as a decision prompt whose text is the fallback.
	msg, err := f.cs.GetMessage(ctx, in.Message.ID)
	require.NoError(t, err)
	assert.Equal(t, messages.TypeDecisionPrompt, msg.Type)
	assert.Contains(t, msg.Msg, "**D234.** Ship it?")
	assert.Contains(t, msg.Msg, "**1.** Which approach?")
	assert.Equal(t, len("Two things 🚀\n\n"), res.IntroLength)
	assert.Equal(t, "Two things 🚀", messages.DecisionIntro(msg.Msg, res.IntroLength))

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

func TestCreateDecisionPrompt_HubNumbersAreNeverReused(t *testing.T) {
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
	_, err := f.cs.CreateDecisionPrompt(context.Background(), f.prompt("", false, yesNo("", "d"), yesNo("99", "e")))
	require.ErrorIs(t, err, store.ErrDecisionKeyNotIssued)
	r5 := f.create(t, false, yesNo("", "f"))
	assert.Equal(t, "4", r5.Decisions[0].Key)
}

func TestCreateDecisionPrompt_RejectsUnissuedNumbers(t *testing.T) {
	f := newDecisionFixture(t)
	ctx := context.Background()

	_, err := f.cs.CreateDecisionPrompt(ctx, f.prompt("", true, yesNo("1", "q")))
	var nie *store.DecisionKeyNotIssuedError
	require.ErrorAs(t, err, &nie)
	assert.ErrorIs(t, err, store.ErrDecisionKeyNotIssued)
	assert.Equal(t, []string{"1"}, nie.Keys)

	f.create(t, false, yesNo("", "first")) // issues 1
	// 1 is now issued; 2 is not, even in the same prompt that reserves 2.
	_, err = f.cs.CreateDecisionPrompt(ctx, f.prompt("", true, yesNo("", "new"), yesNo("2", "q")))
	require.ErrorAs(t, err, &nie)
	assert.Equal(t, []string{"2"}, nie.Keys)

	res, err := f.cs.CreateDecisionPrompt(ctx, f.prompt("", true, yesNo("1", "repost")))
	require.NoError(t, err)
	assert.Equal(t, 2, res.Decisions[0].Version)
	assert.Equal(t, store.DecisionKeySourceHub, res.Decisions[0].KeySource)

	// Nothing from the refused prompts was stored.
	n, err := f.cs.client.Message.Query().Where(message.TypeEQ(messages.TypeDecisionPrompt)).Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, n)
}

func TestCreateDecisionPrompt_RejectsExistingKeysWithoutUpdate(t *testing.T) {
	f := newDecisionFixture(t)
	ctx := context.Background()
	first := f.create(t, false, yesNo("D234", "Ship it?"), yesNo("ship-now", "Now?"))
	_, _, err := f.cs.RecordAnswer(ctx, f.answer(first.Decisions[0].RowID, "1", "user-1", ""))
	require.NoError(t, err)

	in := f.prompt("", false, yesNo("d234", "again"), yesNo("SHIP-NOW", "again"), yesNo("fresh", "new"))
	_, err = f.cs.CreateDecisionPrompt(ctx, in)
	var kee *store.DecisionKeyExistsError
	require.ErrorAs(t, err, &kee)
	assert.ErrorIs(t, err, store.ErrDecisionKeyExists)
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

func TestCreateDecisionPrompt_UpdateReplacesPendingAndDecided(t *testing.T) {
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

func TestCreateDecisionPrompt_PendingCap(t *testing.T) {
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
	_, err := f.cs.CreateDecisionPrompt(ctx, in)
	require.ErrorIs(t, err, store.ErrTooManyPendingDecisions)
	_, err = f.cs.GetMessage(ctx, in.Message.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)

	// An --update of a pending key counts once, so it still fits.
	_, err = f.cs.CreateDecisionPrompt(ctx, f.prompt("", true, yesNo("k0-0", "v2")))
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
	_, err = f.cs.CreateDecisionPrompt(ctx, f.prompt("", false, yesNo("one-more", "q")))
	require.NoError(t, err)
}

func TestCreateDecisionPrompt_ConcurrentUpdatesGiveConsecutiveVersions(t *testing.T) {
	testConcurrentUpdates(t, 2)
}

// TestCreateDecisionPrompt_ThreeConcurrentUpdates: the last of n concurrent
// reposts of one key needs n-1 retries, more than the single retry a
// two-way race needs.
func TestCreateDecisionPrompt_ThreeConcurrentUpdates(t *testing.T) {
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
	results := make([]*store.DecisionPromptResult, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = f.cs.CreateDecisionPrompt(ctx, f.prompt("", true, yesNo("D1", fmt.Sprintf("racer %d", i))))
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

func TestCreateDecisionPrompt_ConcurrentHubNumbersAreDistinct(t *testing.T) {
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
			res, err := f.cs.CreateDecisionPrompt(ctx, f.prompt("", false, yesNo("", "q")))
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
	assert.Equal(t, messages.TypeDecisionResponse, resp.Type)

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
	res, err := f.cs.CreateDecisionPrompt(ctx, in)
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

func TestCreateDecisionPrompt_JoinsCallerTransaction(t *testing.T) {
	f := newDecisionFixture(t)
	ctx := context.Background()
	in := f.prompt("", false, yesNo("D1", "q"))
	boom := errors.New("caller rolls back")
	err := f.cs.WithTx(ctx, func(tx store.Store) error {
		if _, err := tx.CreateDecisionPrompt(ctx, in); err != nil {
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

func TestCreateDecisionPrompt_InvalidInput(t *testing.T) {
	f := newDecisionFixture(t)
	ctx := context.Background()
	cases := map[string]func(in *store.DecisionPromptInput){
		"no message":          func(in *store.DecisionPromptInput) { in.Message = nil },
		"no project":          func(in *store.DecisionPromptInput) { in.Message.ProjectID = "" },
		"bad message id":      func(in *store.DecisionPromptInput) { in.Message.ID = "message-1" },
		"bad project id":      func(in *store.DecisionPromptInput) { in.Message.ProjectID = "project-1" },
		"bad agent":           func(in *store.DecisionPromptInput) { in.AskerAgentID = "agent-1" },
		"no decisions":        func(in *store.DecisionPromptInput) { in.Decisions = nil },
		"invalid key":         func(in *store.DecisionPromptInput) { in.Decisions[0].Key = "a b" },
		"unnormalized choice": func(in *store.DecisionPromptInput) { in.Decisions[0].Choices = in.Decisions[0].Choices[:1] },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			in := f.prompt("", false, yesNo("D1", "q"))
			mutate(in)
			_, err := f.cs.CreateDecisionPrompt(ctx, in)
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
	res, err := f.cs.CreateDecisionPrompt(context.Background(), in)
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
	assert.Equal(t, messages.TypeDecisionResponse, m.Type)
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
	_, err = f.cs.CreateDecisionPrompt(ctx, otherIn)
	require.NoError(t, err)

	// Default state ("") == pending: D1 v2, D3, D4 (3 rows).
	pending, cur, err := f.cs.ListAgentDecisions(ctx, f.agentID, "", "", "", 0)
	require.NoError(t, err)
	assert.Empty(t, cur)
	require.Len(t, pending, 3)
	for _, d := range pending {
		assert.Equal(t, store.DecisionStatusPending, d.Status)
		assert.Equal(t, f.agentID, d.AskerAgentID)
	}

	// Decided: D2 with its answer loaded.
	decided, cur, err := f.cs.ListAgentDecisions(ctx, f.agentID, store.DecisionListStateDecided, "", "", 50)
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
		page, next, err := f.cs.ListAgentDecisions(ctx, f.agentID, store.DecisionListStateAll, "", pageCursor, 2)
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
	_, _, err = f.cs.ListAgentDecisions(ctx, f.agentID, "bogus", "", "", 10)
	assert.ErrorIs(t, err, store.ErrInvalidInput)
	_, _, err = f.cs.ListAgentDecisions(ctx, f.agentID, store.DecisionListStatePending, "", "", 201)
	assert.ErrorIs(t, err, store.ErrInvalidInput)
	_, _, err = f.cs.ListAgentDecisions(ctx, f.agentID, store.DecisionListStatePending, "", "not-a-cursor", 10)
	assert.ErrorIs(t, err, store.ErrInvalidInput)
}

func TestListAgentDecisions_Search(t *testing.T) {
	f := newDecisionFixture(t)
	ctx := context.Background()

	// 1. Question-only match (pending).
	qRow := f.create(t, false, messages.Decision{
		Key:      "k-q",
		Question: "Run the BACKFILL on staging first?",
		Choices:  []messages.Choice{{ID: "1", Label: "CanaryOpt"}, {ID: "2", Label: "Skip"}},
	}).Decisions[0].RowID
	_ = qRow

	// 2. Key-only match (pending).
	keyRow := f.create(t, false, yesNo("MIGRATE-DB", "Proceed now?")).Decisions[0].RowID

	// 3. Direct effective answer label match (decided).
	dirRow := f.create(t, false, messages.Decision{
		Key:      "k-dir",
		Question: "Pick window?",
		Choices:  []messages.Choice{{ID: "a", Label: "Overnight Window"}, {ID: "b", Label: "Lunch"}},
	}).Decisions[0].RowID
	_, _, err := f.cs.RecordAnswer(ctx, f.answer(dirRow, "a", "user-1", ""))
	require.NoError(t, err)

	// 4. Unconfirmed agent_interpreted effective answer label match (decided).
	interpRow := f.create(t, false, messages.Decision{
		Key:      "k-interp",
		Question: "Pick region?",
		Choices:  []messages.Choice{{ID: "1", Label: "eu-central"}, {ID: "2", Label: "us-east"}},
	}).Decisions[0].RowID
	_, _, err = f.cs.RecordAnswer(ctx, f.interpretedAnswer(interpRow, "1", "user-1"))
	require.NoError(t, err)

	// 5. Overridden agent_interpreted answer: interpreted as "AlphaOld", then
	// overridden directly to "BetaNew". Effective answer is "BetaNew", so
	// searching "AlphaOld" must NOT match while "BetaNew" MUST match (D12).
	overRow := f.create(t, false, messages.Decision{
		Key:      "k-over",
		Question: "Which lane?",
		Choices:  []messages.Choice{{ID: "1", Label: "AlphaOld"}, {ID: "2", Label: "BetaNew"}},
	}).Decisions[0].RowID
	_, _, err = f.cs.RecordAnswer(ctx, f.interpretedAnswer(overRow, "1", "user-1"))
	require.NoError(t, err)
	overrideIn := f.answer(overRow, "2", "user-1", "")
	overrideIn.Outcome = store.DecisionOutcomeOverride
	_, _, err = f.cs.RecordAnswer(ctx, overrideIn)
	require.NoError(t, err)

	// 6. Replaced version with an answer, plus v2 pending: state=all finds v1
	// by its effective answer label, while state=pending and state=decided do not.
	repV1 := f.create(t, false, messages.Decision{
		Key:      "k-rep",
		Question: "Proceed step?",
		Choices:  []messages.Choice{{ID: "1", Label: "LegacyTrack"}, {ID: "2", Label: "Modern"}},
	}).Decisions[0].RowID
	_, _, err = f.cs.RecordAnswer(ctx, f.answer(repV1, "1", "user-1", ""))
	require.NoError(t, err)
	repV2 := f.create(t, true, yesNo("k-rep", "Proceed step v2?")).Decisions[0].RowID

	// 7. Wildcard literals (%, _, \).
	pctRow := f.create(t, false, yesNo("k-pct", `Mark 50%_done\now?`)).Decisions[0].RowID
	f.create(t, false, yesNo("k-nopct", "Mark 50 percent xdoneznow?"))

	// 8. Another agent's matching decision never leaks.
	otherAgent := uuid.NewString()
	otherIn := f.prompt("", false, yesNo("MIGRATE-DB", "Run the BACKFILL on staging first?"))
	otherIn.AskerAgentID = otherAgent
	otherIn.Message.SenderID = otherAgent
	otherIn.Message.AgentID = otherAgent
	_, err = f.cs.CreateDecisionPrompt(ctx, otherIn)
	require.NoError(t, err)

	rowIDs := func(state store.DecisionListState, q string) []string {
		t.Helper()
		rows, _, err := f.cs.ListAgentDecisions(ctx, f.agentID, state, q, "", 50)
		require.NoError(t, err)
		out := make([]string, len(rows))
		for i, r := range rows {
			out[i] = r.ID
		}
		return out
	}

	t.Run("each arm alone, case-insensitive, effective answer only", func(t *testing.T) {
		assert.Equal(t, []string{qRow}, rowIDs(store.DecisionListStateAll, "backfill"), "question text")
		assert.Equal(t, []string{keyRow}, rowIDs(store.DecisionListStateAll, "migrate-db"), "decision key")
		assert.Equal(t, []string{dirRow}, rowIDs(store.DecisionListStateAll, "overnight WINDOW"), "direct answer label")
		assert.Equal(t, []string{interpRow}, rowIDs(store.DecisionListStateAll, "EU-CENTRAL"), "interpreted effective answer label")
		assert.Equal(t, []string{overRow}, rowIDs(store.DecisionListStateAll, "betanew"), "overriding direct answer label")
		assert.Empty(t, rowIDs(store.DecisionListStateAll, "alphaold"), "overridden interpreted label is not the effective answer")
		assert.Empty(t, rowIDs(store.DecisionListStateAll, "canaryopt"), "unchosen choice label does not match")
		assert.Empty(t, rowIDs(store.DecisionListStateAll, "asker"), "asker agent name is not an arm on the agent endpoint")
	})

	t.Run("wildcards match literally", func(t *testing.T) {
		assert.Equal(t, []string{pctRow}, rowIDs(store.DecisionListStateAll, "%_"))
		assert.Equal(t, []string{pctRow}, rowIDs(store.DecisionListStateAll, "50%"))
		assert.Equal(t, []string{pctRow}, rowIDs(store.DecisionListStateAll, `done\now`))
		assert.Empty(t, rowIDs(store.DecisionListStateAll, "50_p"))
	})

	t.Run("combined with state filter and cursor paging", func(t *testing.T) {
		assert.Equal(t, []string{repV1}, rowIDs(store.DecisionListStateAll, "legacytrack"))
		assert.Empty(t, rowIDs(store.DecisionListStatePending, "legacytrack"))
		assert.Empty(t, rowIDs(store.DecisionListStateDecided, "legacytrack"))

		assert.ElementsMatch(t, []string{keyRow, repV2}, rowIDs(store.DecisionListStatePending, "proceed"))
		assert.Equal(t, []string{repV1}, rowIDs(store.DecisionListStateAll, "legacytrack"))

		// Cursor paging over the 3 "proceed" rows in state=all, limit=2.
		var paged []string
		cur := ""
		for page := 0; page < 5; page++ {
			rows, next, err := f.cs.ListAgentDecisions(ctx, f.agentID, store.DecisionListStateAll, "proceed", cur, 2)
			require.NoError(t, err)
			for _, r := range rows {
				paged = append(paged, r.ID)
			}
			if next == "" {
				break
			}
			cur = next
		}
		assert.Equal(t, []string{repV2, repV1, keyRow}, paged)
	})
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
	assert.Equal(t, messages.TypeDecisionResponse, msg.Type)
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

	beforeMsgs, err := f.cs.client.Message.Query().Where(message.TypeEQ(messages.TypeDecisionResponse)).Count(ctx)
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

	// K1 is still untouched, and 0 new decision-response messages were stored.
	k1, err := f.cs.GetDecision(ctx, p.Decisions[0].RowID)
	require.NoError(t, err)
	assert.Equal(t, store.DecisionStatusPending, k1.Status)
	assert.Empty(t, k1.Answers)

	afterMsgs, err := f.cs.client.Message.Query().Where(message.TypeEQ(messages.TypeDecisionResponse)).Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, beforeMsgs, afterMsgs, "0 decision-response messages committed on rollback")
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
	otherRes, err := f.cs.CreateDecisionPrompt(ctx, otherIn)
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

func TestWithdrawDecisions_RowStatesValidationAndIdempotency(t *testing.T) {
	f := newDecisionFixture(t)
	ctx := context.Background()

	p := f.create(t, false,
		yesNo("D234", "Ship with reason?"),
		yesNo("D235", "Ship without reason?"),
		yesNo("D236", "Direct answered?"),
		yesNo("D237", "Interpreted awaiting confirm?"),
	)
	rowReason := p.Decisions[0].RowID
	rowNoReason := p.Decisions[1].RowID
	rowDirect := p.Decisions[2].RowID
	rowInterp := p.Decisions[3].RowID

	// Answer D236 directly and D237 via agent_interpreted (awaiting confirm, D6).
	_, _, err := f.cs.RecordAnswer(ctx, f.answer(rowDirect, "1", "user-1", ""))
	require.NoError(t, err)
	_, err = f.cs.RecordInterpretedAnswers(ctx, f.interpretedBatch(uuid.NewString(), "user-1", "",
		store.InterpretedAnswerPair{DecisionRowID: rowInterp, Key: "D237", ChoiceID: "2"},
	))
	require.NoError(t, err)

	// Input validation rejects malformed inputs before touching rows.
	for _, bad := range []*store.WithdrawDecisionsInput{
		nil,
		{AskerAgentID: "not-a-uuid", Keys: []string{"D234"}},
		{AskerAgentID: f.agentID, Keys: nil},
		{AskerAgentID: f.agentID, Keys: []string{"   "}},
		{AskerAgentID: f.agentID, Keys: []string{"D234", "d234"}},
		{AskerAgentID: f.agentID, Keys: []string{"k1", "k2", "k3", "k4", "k5", "k6", "k7", "k8", "k9", "k10", "k11"}},
		{AskerAgentID: f.agentID, Keys: []string{"D234"}, Reason: strings.Repeat("é", store.MaxDecisionWithdrawReasonRunes+1)},
	} {
		_, err := f.cs.WithdrawDecisions(ctx, bad)
		require.ErrorIs(t, err, store.ErrInvalidInput, "input=%+v", bad)
	}

	// Withdraw pending D234 (case-insensitive key lookup, trimmed reason).
	res1, err := f.cs.WithdrawDecisions(ctx, &store.WithdrawDecisionsInput{
		AskerAgentID: f.agentID,
		Keys:         []string{"  d234  "},
		Reason:       "  CI found a regression — café  ",
	})
	require.NoError(t, err)
	require.Len(t, res1.Withdrawn, 1)
	assert.Empty(t, res1.AlreadyWithdrawn)
	assert.Equal(t, "D234", res1.Withdrawn[0].Key)
	assert.Equal(t, store.DecisionStatusWithdrawn, res1.Withdrawn[0].Status)
	require.NotNil(t, res1.Withdrawn[0].WithdrawnAt)
	assert.Equal(t, "CI found a regression — café", res1.Withdrawn[0].WithdrawnReason)
	firstWithdrawnAt := *res1.Withdrawn[0].WithdrawnAt
	assert.Zero(t, firstWithdrawnAt.Nanosecond()%int(time.Microsecond),
		"withdrawn_at is microsecond-aligned so first response and Postgres TIMESTAMPTZ reads agree")

	// Idempotent retry with a DIFFERENT reason reports AlreadyWithdrawn and
	// preserves the original timestamp and reason (first withdrawal wins).
	time.Sleep(2 * time.Millisecond)
	resRetry, err := f.cs.WithdrawDecisions(ctx, &store.WithdrawDecisionsInput{
		AskerAgentID: f.agentID,
		Keys:         []string{"D234"},
		Reason:       "Second reason should be ignored",
	})
	require.NoError(t, err)
	assert.Empty(t, resRetry.Withdrawn)
	require.Len(t, resRetry.AlreadyWithdrawn, 1)
	assert.Equal(t, "D234", resRetry.AlreadyWithdrawn[0].Key)
	assert.Equal(t, "CI found a regression — café", resRetry.AlreadyWithdrawn[0].WithdrawnReason)
	require.NotNil(t, resRetry.AlreadyWithdrawn[0].WithdrawnAt)
	assert.True(t, firstWithdrawnAt.Equal(*resRetry.AlreadyWithdrawn[0].WithdrawnAt))

	gotReason, err := f.cs.GetDecision(ctx, rowReason)
	require.NoError(t, err)
	assert.Equal(t, store.DecisionStatusWithdrawn, gotReason.Status)
	assert.Equal(t, "CI found a regression — café", gotReason.WithdrawnReason)

	// 500 multi-byte runes are accepted; empty reason stores empty WithdrawnReason.
	maxRuneReason := strings.Repeat("☕", store.MaxDecisionWithdrawReasonRunes)
	resNoReason, err := f.cs.WithdrawDecisions(ctx, &store.WithdrawDecisionsInput{
		AskerAgentID: f.agentID,
		Keys:         []string{"D235"},
		Reason:       "   ",
	})
	require.NoError(t, err)
	require.Len(t, resNoReason.Withdrawn, 1)
	assert.Empty(t, resNoReason.Withdrawn[0].WithdrawnReason)
	gotNoReason, err := f.cs.GetDecision(ctx, rowNoReason)
	require.NoError(t, err)
	assert.Equal(t, store.DecisionStatusWithdrawn, gotNoReason.Status)
	assert.Empty(t, gotNoReason.WithdrawnReason)

	pMax := f.create(t, false, yesNo("DMAX", "500 rune reason?"))
	resMax, err := f.cs.WithdrawDecisions(ctx, &store.WithdrawDecisionsInput{
		AskerAgentID: f.agentID,
		Keys:         []string{"DMAX"},
		Reason:       maxRuneReason,
	})
	require.NoError(t, err)
	require.Len(t, resMax.Withdrawn, 1)
	assert.Equal(t, maxRuneReason, resMax.Withdrawn[0].WithdrawnReason)
	_ = pMax

	// Unknown key -> decision_not_found; another agent's key -> decision_not_found.
	otherAgent := uuid.NewString()
	otherPrompt := f.prompt("", false, yesNo("OTHER-KEY", "Other agent asks"))
	otherPrompt.AskerAgentID = otherAgent
	otherPrompt.Message.SenderID = otherAgent
	otherPrompt.Message.AgentID = otherAgent
	_, err = f.cs.CreateDecisionPrompt(ctx, otherPrompt)
	require.NoError(t, err)

	for _, missing := range []string{"NOPE", "OTHER-KEY"} {
		_, err := f.cs.WithdrawDecisions(ctx, &store.WithdrawDecisionsInput{
			AskerAgentID: f.agentID,
			Keys:         []string{missing},
		})
		var wdErr *store.WithdrawDecisionsError
		require.ErrorAs(t, err, &wdErr, "key=%s", missing)
		require.ErrorIs(t, err, store.ErrWithdrawDecisionsFailed)
		require.Len(t, wdErr.Failures, 1)
		assert.Equal(t, missing, wdErr.Failures[0].Key)
		assert.Equal(t, store.WithdrawReasonNotFound, wdErr.Failures[0].Reason)
		assert.Nil(t, wdErr.Failures[0].Decision)
	}

	// Direct answered (D236) and interpreted awaiting confirm (D237, D6) -> already_answered.
	for _, answeredKey := range []string{"D236", "D237"} {
		_, err := f.cs.WithdrawDecisions(ctx, &store.WithdrawDecisionsInput{
			AskerAgentID: f.agentID,
			Keys:         []string{answeredKey},
		})
		var wdErr *store.WithdrawDecisionsError
		require.ErrorAs(t, err, &wdErr, "key=%s", answeredKey)
		require.Len(t, wdErr.Failures, 1)
		assert.Equal(t, answeredKey, wdErr.Failures[0].Key)
		assert.Equal(t, store.WithdrawReasonAlreadyAnswered, wdErr.Failures[0].Reason)
		require.NotNil(t, wdErr.Failures[0].Decision)
		require.NotEmpty(t, wdErr.Failures[0].Decision.Answers)
	}
}

func TestWithdrawDecisions_RecordAnswerRefusalAndReplay(t *testing.T) {
	f := newDecisionFixture(t)
	ctx := context.Background()

	rowID := f.create(t, false, yesNo("D-ANS", "Answer after withdraw?")).Decisions[0].RowID
	_, err := f.cs.WithdrawDecisions(ctx, &store.WithdrawDecisionsInput{
		AskerAgentID: f.agentID,
		Keys:         []string{"D-ANS"},
		Reason:       "Obsolete",
	})
	require.NoError(t, err)

	ansIn := f.answer(rowID, "1", "user-1", "idem-late")
	_, _, err = f.cs.RecordAnswer(ctx, ansIn)
	require.ErrorIs(t, err, store.ErrDecisionWithdrawn)

	d, err := f.cs.GetDecision(ctx, rowID)
	require.NoError(t, err)
	assert.Equal(t, store.DecisionStatusWithdrawn, d.Status)
	assert.Empty(t, d.Answers, "refused answer stored nothing")
	_, err = f.cs.GetMessage(ctx, ansIn.ResponseMessage.ID)
	assert.ErrorIs(t, err, store.ErrNotFound, "refused answer stored no response message")

	// Idempotency replay check runs BEFORE terminal-status refusal (design 4.1):
	// if an answer was recorded under an idempotency key before the row transitioned
	// to withdrawn, retrying with that idempotency key still replays the answer.
	replayRowID := f.create(t, false, yesNo("D-REPLAY", "Replay before withdraw?")).Decisions[0].RowID
	origAns, replayed, err := f.cs.RecordAnswer(ctx, f.answer(replayRowID, "1", "user-1", "idem-prior"))
	require.NoError(t, err)
	assert.False(t, replayed)

	replayUUID, err := uuid.Parse(replayRowID)
	require.NoError(t, err)
	require.NoError(t, f.cs.client.Decision.UpdateOneID(replayUUID).
		SetStatus("withdrawn").
		SetWithdrawnAt(time.Now().UTC()).
		Exec(ctx))

	gotReplay, replayed, err := f.cs.RecordAnswer(ctx, f.answer(replayRowID, "1", "user-1", "idem-prior"))
	require.NoError(t, err)
	assert.True(t, replayed)
	assert.Equal(t, origAns.ID, gotReplay.ID)
}

func TestWithdrawDecisions_PendingCapAndUpdateAfterWithdraw(t *testing.T) {
	f := newDecisionFixture(t)
	ctx := context.Background()

	// Fill all 50 pending slots across 5 prompts of 10 decisions each.
	for b := 0; b < 5; b++ {
		ds := make([]messages.Decision, 10)
		for i := 0; i < 10; i++ {
			ds[i] = yesNo(fmt.Sprintf("CAP-%d-%d", b, i), "Cap check?")
		}
		f.create(t, false, ds...)
	}

	// 51st pending decision is refused.
	_, err := f.cs.CreateDecisionPrompt(ctx, f.prompt("", false, yesNo("CAP-OVER", "Too many?")))
	require.ErrorIs(t, err, store.ErrTooManyPendingDecisions)

	// Withdrawing one pending decision frees capacity (cap counts pending only).
	_, err = f.cs.WithdrawDecisions(ctx, &store.WithdrawDecisionsInput{
		AskerAgentID: f.agentID,
		Keys:         []string{"CAP-0-0"},
		Reason:       "No longer needed",
	})
	require.NoError(t, err)

	overRes, err := f.cs.CreateDecisionPrompt(ctx, f.prompt("", false, yesNo("CAP-OVER", "Fits now?")))
	require.NoError(t, err)
	require.Len(t, overRes.Decisions, 1)

	// Re-asking CAP-0-0 without --update is refused with "(v1, withdrawn)" (D7).
	_, err = f.cs.CreateDecisionPrompt(ctx, f.prompt("", false, yesNo("CAP-0-0", "Re-ask without update?")))
	var existsErr *store.DecisionKeyExistsError
	require.ErrorAs(t, err, &existsErr)
	assert.Contains(t, existsErr.Error(), "CAP-0-0 already exists (v1, withdrawn)")

	// Free one more slot so --update adding a new pending v2 stays within 50.
	_, err = f.cs.WithdrawDecisions(ctx, &store.WithdrawDecisionsInput{
		AskerAgentID: f.agentID,
		Keys:         []string{"CAP-0-1"},
	})
	require.NoError(t, err)

	// Re-asking CAP-0-0 WITH --update creates v2 (pending), leaves v1 withdrawn,
	// and links v1.ReplacedByID -> v2.RowID (D7, design section 10).
	v2Res, err := f.cs.CreateDecisionPrompt(ctx, f.prompt("", true, yesNo("CAP-0-0", "Re-ask v2?")))
	require.NoError(t, err)
	require.Len(t, v2Res.Decisions, 1)
	assert.Equal(t, 2, v2Res.Decisions[0].Version)
	require.NotNil(t, v2Res.Decisions[0].Replaced)
	assert.Equal(t, 1, v2Res.Decisions[0].Replaced.Version)
	assert.Equal(t, store.DecisionStatusWithdrawn, v2Res.Decisions[0].Replaced.Status)

	versions, err := f.cs.GetAgentDecisionVersions(ctx, f.agentID, "CAP-0-0")
	require.NoError(t, err)
	require.Len(t, versions, 2)
	assert.Equal(t, store.DecisionStatusWithdrawn, versions[0].Status, "v1 stays withdrawn")
	assert.Equal(t, "No longer needed", versions[0].WithdrawnReason)
	require.NotNil(t, versions[0].WithdrawnAt)
	assert.Equal(t, v2Res.Decisions[0].RowID, versions[0].ReplacedByID, "v1 links to v2")
	assert.Equal(t, store.DecisionStatusPending, versions[1].Status)
}

func TestWithdrawDecisions_MultiKeyAllOrNothingWholeSetAndRefusals(t *testing.T) {
	f := newDecisionFixture(t)
	ctx := context.Background()

	// 1. Multi-key across prompts + all-or-nothing failure (D8) + mixed idempotency.
	p1 := f.create(t, false,
		yesNo("MK-1", "First pending?"),
		yesNo("MK-ANS", "Answered directly?"),
		yesNo("MK-INT", "Interpreted awaiting confirm?"),
	)
	p2 := f.create(t, false,
		yesNo("MK-2", "Second pending?"),
		yesNo("MK-3", "Third pending?"),
	)
	_, _, err := f.cs.RecordAnswer(ctx, f.answer(p1.Decisions[1].RowID, "1", "user-1", ""))
	require.NoError(t, err)
	_, err = f.cs.RecordInterpretedAnswers(ctx, f.interpretedBatch(uuid.NewString(), "user-1", "",
		store.InterpretedAnswerPair{DecisionRowID: p1.Decisions[2].RowID, Key: "MK-INT", ChoiceID: "2"},
	))
	require.NoError(t, err)

	// Mixed pending + direct-answered + interpreted-awaiting-confirm (D6) + unknown -> all-or-nothing failure.
	_, err = f.cs.WithdrawDecisions(ctx, &store.WithdrawDecisionsInput{
		AskerAgentID: f.agentID,
		Keys:         []string{"MK-1", "MK-ANS", "MK-INT", "MK-MISSING"},
		Reason:       "Should not write",
	})
	var wdErr *store.WithdrawDecisionsError
	require.ErrorAs(t, err, &wdErr)
	require.Len(t, wdErr.Failures, 3)
	assert.Equal(t, "MK-ANS", wdErr.Failures[0].Key)
	assert.Equal(t, store.WithdrawReasonAlreadyAnswered, wdErr.Failures[0].Reason)
	assert.Equal(t, "MK-INT", wdErr.Failures[1].Key)
	assert.Equal(t, store.WithdrawReasonAlreadyAnswered, wdErr.Failures[1].Reason)
	assert.Equal(t, "MK-MISSING", wdErr.Failures[2].Key)
	assert.Equal(t, store.WithdrawReasonNotFound, wdErr.Failures[2].Reason)

	mk1, err := f.cs.GetDecision(ctx, p1.Decisions[0].RowID)
	require.NoError(t, err)
	assert.Equal(t, store.DecisionStatusPending, mk1.Status, "all-or-nothing left MK-1 pending")

	// Withdraw MK-2 first with its own reason, then withdraw [MK-3, MK-2, MK-1] with a new reason:
	// MK-1 and MK-3 become Withdrawn (in canonical prompt/position order: MK-1 then MK-3),
	// while MK-2 is reported in AlreadyWithdrawn with its original reason preserved.
	resFirst, err := f.cs.WithdrawDecisions(ctx, &store.WithdrawDecisionsInput{
		AskerAgentID: f.agentID,
		Keys:         []string{"MK-2"},
		Reason:       "Original reason for MK-2",
	})
	require.NoError(t, err)
	require.Len(t, resFirst.Withdrawn, 1)
	mk2At := *resFirst.Withdrawn[0].WithdrawnAt

	time.Sleep(2 * time.Millisecond)
	resMixed, err := f.cs.WithdrawDecisions(ctx, &store.WithdrawDecisionsInput{
		AskerAgentID: f.agentID,
		Keys:         []string{"MK-3", "MK-2", "MK-1"},
		Reason:       "Batch reason",
	})
	require.NoError(t, err)
	require.Len(t, resMixed.Withdrawn, 2)
	assert.Equal(t, "MK-1", resMixed.Withdrawn[0].Key)
	assert.Equal(t, "MK-3", resMixed.Withdrawn[1].Key)
	assert.Equal(t, "Batch reason", resMixed.Withdrawn[0].WithdrawnReason)
	assert.Equal(t, "Batch reason", resMixed.Withdrawn[1].WithdrawnReason)
	require.Len(t, resMixed.AlreadyWithdrawn, 1)
	assert.Equal(t, "MK-2", resMixed.AlreadyWithdrawn[0].Key)
	assert.Equal(t, "Original reason for MK-2", resMixed.AlreadyWithdrawn[0].WithdrawnReason)
	assert.True(t, mk2At.Equal(*resMixed.AlreadyWithdrawn[0].WithdrawnAt))
	_ = p2

	// 2. WholeSet (--set): withdraws every pending sibling of the key's prompt,
	// skipping direct-answered, interpreted-awaiting-confirm (D6), and replaced siblings.
	setMsgID, setRows := f.createSet(t,
		yesNo("WS-1", "Pending target?"),
		yesNo("WS-DIRECT", "Direct answered sibling?"),
		yesNo("WS-INTERP", "Interpreted awaiting confirm sibling?"),
		yesNo("WS-REPL", "Replaced sibling?"),
		yesNo("WS-5", "Pending sibling?"),
	)
	_, _, err = f.cs.RecordAnswer(ctx, f.answer(setRows[1], "1", "user-1", ""))
	require.NoError(t, err)
	_, err = f.cs.RecordInterpretedAnswers(ctx, f.interpretedBatch(uuid.NewString(), "user-1", "",
		store.InterpretedAnswerPair{DecisionRowID: setRows[2], Key: "WS-INTERP", ChoiceID: "1"},
	))
	require.NoError(t, err)
	// Replace WS-REPL onto a newer prompt so setRows[3] has status=replaced on setMsgID.
	f.create(t, true, yesNo("WS-REPL", "WS-REPL v2 on another prompt"))

	// Explicitly naming an answered row with WholeSet=true still fails all-or-nothing.
	_, err = f.cs.WithdrawDecisions(ctx, &store.WithdrawDecisionsInput{
		AskerAgentID: f.agentID,
		Keys:         []string{"WS-INTERP"},
		WholeSet:     true,
		Reason:       "Must fail",
	})
	require.ErrorAs(t, err, &wdErr)
	assert.Equal(t, store.WithdrawReasonAlreadyAnswered, wdErr.Failures[0].Reason)

	// Naming pending WS-1 with WholeSet=true withdraws WS-1 and WS-5, skipping WS-DIRECT, WS-INTERP, and WS-REPL v1.
	resSet, err := f.cs.WithdrawDecisions(ctx, &store.WithdrawDecisionsInput{
		AskerAgentID: f.agentID,
		Keys:         []string{"WS-1"},
		WholeSet:     true,
		Reason:       "Retiring rest of set",
	})
	require.NoError(t, err)
	require.Len(t, resSet.Withdrawn, 2)
	assert.Empty(t, resSet.AlreadyWithdrawn)
	assert.Equal(t, "WS-1", resSet.Withdrawn[0].Key)
	assert.Equal(t, "WS-5", resSet.Withdrawn[1].Key)

	// Repeating WholeSet=true on WS-1 reports WS-1 in AlreadyWithdrawn and writes nothing.
	resSetRetry, err := f.cs.WithdrawDecisions(ctx, &store.WithdrawDecisionsInput{
		AskerAgentID: f.agentID,
		Keys:         []string{"WS-1"},
		WholeSet:     true,
		Reason:       "Ignored retry reason",
	})
	require.NoError(t, err)
	assert.Empty(t, resSetRetry.Withdrawn)
	require.Len(t, resSetRetry.AlreadyWithdrawn, 1)
	assert.Equal(t, "WS-1", resSetRetry.AlreadyWithdrawn[0].Key)
	assert.Equal(t, "Retiring rest of set", resSetRetry.AlreadyWithdrawn[0].WithdrawnReason)

	// 3. Set submit (RecordAnswerSet) refuses withdrawn rows with AnswerSetReasonWithdrawn,
	// while allowing the remaining actionable row (confirming WS-INTERP) to be submitted.
	badSetIn := f.answerSet(setMsgID, "user-1", "", setRows[0], "1", setRows[2], "1")
	badSetIn.Answers[1].Outcome = store.DecisionOutcomeConfirm
	_, err = f.cs.RecordAnswerSet(ctx, badSetIn)
	var setChanged *store.AnswerSetChangedError
	require.ErrorAs(t, err, &setChanged)
	assert.Equal(t, map[string]string{setRows[0]: store.AnswerSetReasonWithdrawn}, setChanged.Reasons)
	_, err = f.cs.GetMessage(ctx, badSetIn.ResponseMessage.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)

	goodSetIn := f.answerSet(setMsgID, "user-1", "", setRows[2], "1")
	goodSetIn.Answers[0].Outcome = store.DecisionOutcomeConfirm
	goodSetRes, err := f.cs.RecordAnswerSet(ctx, goodSetIn)
	require.NoError(t, err)
	require.Len(t, goodSetRes.Answers, 1)
	assert.Equal(t, store.DecisionOutcomeConfirm, goodSetRes.Answers[0].Outcome)

	// 4. RecordInterpretedAnswers refuses withdrawn keys with InterpretedReasonWithdrawn (all-or-nothing).
	interpOpen := f.create(t, false, yesNo("INT-OPEN", "Still pending?"))
	interpBatchIn := f.interpretedBatch(uuid.NewString(), "user-1", "",
		store.InterpretedAnswerPair{DecisionRowID: interpOpen.Decisions[0].RowID, Key: "INT-OPEN", ChoiceID: "1"},
		store.InterpretedAnswerPair{DecisionRowID: setRows[0], Key: "WS-1", ChoiceID: "1"},
	)
	_, err = f.cs.RecordInterpretedAnswers(ctx, interpBatchIn)
	var iaErr *store.InterpretedAnswersError
	require.ErrorAs(t, err, &iaErr)
	require.Len(t, iaErr.Failures, 1)
	assert.Equal(t, "WS-1", iaErr.Failures[0].Key)
	assert.Equal(t, store.InterpretedReasonWithdrawn, iaErr.Failures[0].Reason)
	require.NotNil(t, iaErr.Failures[0].Decision)
	assert.Equal(t, store.DecisionStatusWithdrawn, iaErr.Failures[0].Decision.Status)

	openAfter, err := f.cs.GetDecision(ctx, interpOpen.Decisions[0].RowID)
	require.NoError(t, err)
	assert.Equal(t, store.DecisionStatusPending, openAfter.Status, "all-or-nothing left INT-OPEN pending")

	// 5. ListAgentDecisions with DecisionListStateWithdrawn and DecisionListStateAll.
	withdrawnList, nextCur, err := f.cs.ListAgentDecisions(ctx, f.agentID, store.DecisionListStateWithdrawn, "", "", 50)
	require.NoError(t, err)
	assert.Empty(t, nextCur)
	require.Len(t, withdrawnList, 5) // MK-1, MK-2, MK-3, WS-1, WS-5
	for _, item := range withdrawnList {
		assert.Equal(t, store.DecisionStatusWithdrawn, item.Status)
		assert.NotNil(t, item.WithdrawnAt)
	}

	filteredWithdrawn, _, err := f.cs.ListAgentDecisions(ctx, f.agentID, store.DecisionListStateWithdrawn, "WS-", "", 50)
	require.NoError(t, err)
	require.Len(t, filteredWithdrawn, 2)

	allList, _, err := f.cs.ListAgentDecisions(ctx, f.agentID, store.DecisionListStateAll, "", "", 50)
	require.NoError(t, err)
	seenStatuses := map[string]bool{}
	for _, item := range allList {
		seenStatuses[item.Status] = true
	}
	assert.True(t, seenStatuses[store.DecisionStatusPending])
	assert.True(t, seenStatuses[store.DecisionStatusDecided])
	assert.True(t, seenStatuses[store.DecisionStatusReplaced])
	assert.True(t, seenStatuses[store.DecisionStatusWithdrawn])
}

func (f *decisionFixture) dismissInput(msgID, userID, reason string, now func() time.Time) *store.DismissDecisionsInput {
	return &store.DismissDecisionsInput{
		MessageID: msgID,
		Reason:    reason,
		Surface:   store.DecisionSurfaceDecisionsPage,
		User: store.DecisionAnswerer{
			UserID: userID,
			Name:   "Pat",
			Email:  "pat@example.com",
		},
		ResponseMessage: &store.Message{
			ID:             uuid.NewString(),
			ProjectID:      f.projectID,
			ConversationID: f.conversationID,
			Sender:         "user:pat@example.com",
			SenderID:       userID,
			Recipient:      "agent:asker",
			RecipientID:    f.agentID,
			DispatchState:  store.MessageDispatchDispatched,
		},
		FormatMessage: func(ds []*store.Decision) string {
			keys := make([]string, len(ds))
			for i, d := range ds {
				keys[i] = d.Key
			}
			return "Pat dismissed " + strings.Join(keys, ", ")
		},
		Now: now,
	}
}

func TestDismissDecisions_WholePromptAndMinimumAge(t *testing.T) {
	f := newDecisionFixture(t)
	ctx := context.Background()

	promptIn := f.prompt("", false, yesNo("DSM-1", "Ship v0.9 today?"), yesNo("DSM-2", "Which approach?"))
	promptIn.Message.ThreadID = "topic:general"
	createdPrompt, err := f.cs.CreateDecisionPrompt(ctx, promptIn)
	require.NoError(t, err)
	msgID := promptIn.Message.ID
	rows := []string{createdPrompt.Decisions[0].RowID, createdPrompt.Decisions[1].RowID}
	d0, err := f.cs.GetDecision(ctx, rows[0])
	require.NoError(t, err)
	createdAt := d0.CreatedAt
	availAt := createdAt.Add(store.DecisionDismissMinAge)

	// Input validation refusals.
	for _, bad := range []*store.DismissDecisionsInput{
		nil,
		{MessageID: msgID, User: store.DecisionAnswerer{UserID: "u1"}},
		func() *store.DismissDecisionsInput {
			in := f.dismissInput(msgID, "", "", nil)
			return in
		}(),
		func() *store.DismissDecisionsInput {
			in := f.dismissInput(msgID, "user-1", strings.Repeat("a", store.MaxDecisionDismissReasonRunes+1), nil)
			return in
		}(),
		func() *store.DismissDecisionsInput {
			in := f.dismissInput(msgID, "user-1", "", nil)
			in.Surface = "bogus"
			return in
		}(),
		func() *store.DismissDecisionsInput {
			in := f.dismissInput(msgID, "user-1", "", nil)
			in.RowIDs = []string{uuid.NewString()}
			return in
		}(),
		func() *store.DismissDecisionsInput {
			in := f.dismissInput(msgID, "user-1", "", nil)
			in.RowIDs = []string{rows[0], rows[0]}
			return in
		}(),
		func() *store.DismissDecisionsInput {
			in := f.dismissInput(msgID, "user-1", "", nil)
			in.RowIDs = []string{"not-a-uuid"}
			return in
		}(),
		func() *store.DismissDecisionsInput {
			in := f.dismissInput(msgID, "user-1", "", nil)
			in.IdempotencyKey = strings.Repeat("k", store.MaxDecisionIdempotencyKeyLen+1)
			return in
		}(),
	} {
		_, err := f.cs.DismissDecisions(ctx, bad)
		require.ErrorIs(t, err, store.ErrInvalidInput)
	}

	// Unknown message ID -> ErrNotFound.
	_, err = f.cs.DismissDecisions(ctx, f.dismissInput(uuid.NewString(), "user-1", "", func() time.Time { return availAt }))
	require.ErrorIs(t, err, store.ErrNotFound)

	// 4m59s after creation -> DecisionTooRecentError with exact AvailableAt, nothing stored.
	earlyIn := f.dismissInput(msgID, "user-1", "Too early", func() time.Time {
		return createdAt.Add(4*time.Minute + 59*time.Second)
	})
	_, err = f.cs.DismissDecisions(ctx, earlyIn)
	var tooRecent *store.DecisionTooRecentError
	require.ErrorAs(t, err, &tooRecent)
	require.ErrorIs(t, err, store.ErrDecisionTooRecent)
	assert.True(t, availAt.Equal(tooRecent.AvailableAt), "expected %v, got %v", availAt, tooRecent.AvailableAt)
	_, err = f.cs.GetMessage(ctx, earlyIn.ResponseMessage.ID)
	assert.ErrorIs(t, err, store.ErrNotFound, "early attempt stored no response message")

	// Open prompts still include this prompt before dismissal.
	openBefore, err := f.cs.ListOpenPrompts(ctx, []string{f.projectID}, []string{"topic:general"})
	require.NoError(t, err)
	require.Len(t, openBefore, 1)

	// Exactly 5m00s -> dismisses both rows in prompt position order in one transaction.
	okIn := f.dismissInput(msgID, "user-1", "  Stale: release already shipped — café ☕  ", func() time.Time {
		return availAt
	})
	res, err := f.cs.DismissDecisions(ctx, okIn)
	require.NoError(t, err)
	require.Len(t, res.Dismissed, 2)
	assert.Empty(t, res.AlreadyDismissed)
	require.NotEmpty(t, res.BatchID)
	assert.Equal(t, "DSM-1", res.Dismissed[0].Key)
	assert.Equal(t, "DSM-2", res.Dismissed[1].Key)

	storedMsg, err := f.cs.GetMessage(ctx, okIn.ResponseMessage.ID)
	require.NoError(t, err)
	assert.Equal(t, messages.TypeDecisionResponse, storedMsg.Type)
	assert.Equal(t, "Pat dismissed DSM-1, DSM-2", storedMsg.Msg)

	for i, rowID := range rows {
		got, err := f.cs.GetDecision(ctx, rowID)
		require.NoError(t, err)
		assert.Equal(t, store.DecisionStatusDismissed, got.Status)
		require.NotNil(t, got.DismissedAt)
		assert.True(t, availAt.Truncate(time.Microsecond).Equal(*got.DismissedAt))
		assert.Equal(t, res.Dismissed[i].DismissedAt.UTC(), got.DismissedAt.UTC())
		assert.Nil(t, got.EffectiveAnswer(), "EffectiveAnswer must skip outcome=dismiss and return nil on dismissed rows")
		dsm := got.Dismissal()
		require.NotNil(t, dsm)
		assert.Equal(t, res.Dismissed[i].Dismissal().CreatedAt.UTC(), dsm.CreatedAt.UTC())
		assert.Equal(t, messages.ChoiceIDDismissed, dsm.ChoiceID)
		assert.Equal(t, messages.ChoiceLabelDismissed, dsm.ChoiceLabel)
		assert.Equal(t, store.DecisionOutcomeDismiss, dsm.Outcome)
		assert.Equal(t, store.DecisionProvenanceDirect, dsm.Provenance)
		assert.Equal(t, "Stale: release already shipped — café ☕", dsm.DismissReason)
		assert.Equal(t, store.DecisionSurfaceDecisionsPage, dsm.Surface)
		assert.Equal(t, okIn.ResponseMessage.ID, dsm.ResponseMessageID)
		assert.Equal(t, res.BatchID, dsm.BatchID)
		assert.False(t, dsm.MatchedDefault, "row %d matched_default must be false on dismissal", i)
	}

	// Open prompts omit a wholly-dismissed prompt.
	openAfter, err := f.cs.ListOpenPrompts(ctx, []string{f.projectID}, []string{"topic:general"})
	require.NoError(t, err)
	assert.Empty(t, openAfter)

	// Repeating whole-prompt dismiss returns AlreadyDismissed and stores nothing new.
	retryIn := f.dismissInput(msgID, "user-1", "Second reason ignored", func() time.Time {
		return availAt.Add(time.Minute)
	})
	retryRes, err := f.cs.DismissDecisions(ctx, retryIn)
	require.NoError(t, err)
	assert.Empty(t, retryRes.Dismissed)
	require.Len(t, retryRes.AlreadyDismissed, 2)
	assert.Equal(t, "Stale: release already shipped — café ☕", retryRes.AlreadyDismissed[0].Dismissal().DismissReason)
	_, err = f.cs.GetMessage(ctx, retryIn.ResponseMessage.ID)
	assert.ErrorIs(t, err, store.ErrNotFound, "pure alreadyDismissed retry stores no message")
}

func TestDismissDecisions_AnswerRefusalReservedSentinelAndUpdateClock(t *testing.T) {
	f := newDecisionFixture(t)
	ctx := context.Background()

	// Reserved sentinel $dismissed and outcome=dismiss are rejected by RecordAnswer / RecordAnswerSet.
	openPrompt := f.create(t, false, yesNo("D-RES-1", "Reserved check?"))
	openRow := openPrompt.Decisions[0].RowID
	badChoice := f.answer(openRow, messages.ChoiceIDDismissed, "user-1", "")
	_, _, err := f.cs.RecordAnswer(ctx, badChoice)
	require.ErrorIs(t, err, store.ErrInvalidInput)

	badOutcome := f.answer(openRow, "1", "user-1", "")
	badOutcome.Outcome = store.DecisionOutcomeDismiss
	_, _, err = f.cs.RecordAnswer(ctx, badOutcome)
	require.ErrorIs(t, err, store.ErrInvalidInput)

	setMsgID, setRows := f.createSet(t, yesNo("D-RES-2", "Set 1?"), yesNo("D-RES-3", "Set 2?"))
	_, err = f.cs.RecordAnswerSet(ctx, f.answerSet(setMsgID, "user-1", "", setRows[0], messages.ChoiceIDDismissed, setRows[1], "1"))
	require.ErrorIs(t, err, store.ErrInvalidInput)

	// Dismiss D-RES-1 at +5m, then verify RecordAnswer returns ErrDecisionDismissed.
	dOpen, err := f.cs.GetDecision(ctx, openRow)
	require.NoError(t, err)
	t5m := dOpen.CreatedAt.Add(store.DecisionDismissMinAge)
	_, err = f.cs.DismissDecisions(ctx, f.dismissInput(dOpen.MessageID, "user-1", "", func() time.Time { return t5m }))
	require.NoError(t, err)

	lateAns := f.answer(openRow, "1", "user-1", "late-key")
	_, _, err = f.cs.RecordAnswer(ctx, lateAns)
	require.ErrorIs(t, err, store.ErrDecisionDismissed)
	_, err = f.cs.GetMessage(ctx, lateAns.ResponseMessage.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)

	// Idempotency replay runs BEFORE ErrDecisionDismissed check.
	replayPrompt := f.create(t, false, yesNo("D-REPLAY-DSM", "Replay check?"))
	replayRow := replayPrompt.Decisions[0].RowID
	origAns, replayed, err := f.cs.RecordAnswer(ctx, f.answer(replayRow, "1", "user-1", "idem-before-dsm"))
	require.NoError(t, err)
	assert.False(t, replayed)

	replayUUID, err := uuid.Parse(replayRow)
	require.NoError(t, err)
	require.NoError(t, f.cs.client.Decision.UpdateOneID(replayUUID).
		SetStatus("dismissed").
		SetDismissedAt(t5m).
		Exec(ctx))

	gotReplay, replayed, err := f.cs.RecordAnswer(ctx, f.answer(replayRow, "1", "user-1", "idem-before-dsm"))
	require.NoError(t, err)
	assert.True(t, replayed)
	assert.Equal(t, origAns.ID, gotReplay.ID)

	// --update after dismissal creates v2 (pending), leaves v1 dismissed with ReplacedByID -> v2,
	// and resets the 5-minute clock on v2.
	v2Res, err := f.cs.CreateDecisionPrompt(ctx, f.prompt("", true, yesNo("D-RES-1", "Re-asked v2?")))
	require.NoError(t, err)
	require.Len(t, v2Res.Decisions, 1)
	require.NotNil(t, v2Res.Decisions[0].Replaced)
	assert.Equal(t, store.DecisionStatusDismissed, v2Res.Decisions[0].Replaced.Status)

	versions, err := f.cs.GetAgentDecisionVersions(ctx, f.agentID, "D-RES-1")
	require.NoError(t, err)
	require.Len(t, versions, 2)
	assert.Equal(t, store.DecisionStatusDismissed, versions[0].Status, "v1 stays dismissed")
	assert.Equal(t, v2Res.Decisions[0].RowID, versions[0].ReplacedByID)
	assert.Equal(t, store.DecisionStatusPending, versions[1].Status)

	// v2 has its own fresh CreatedAt, so dismissing v2 immediately fails with DecisionTooRecentError.
	_, err = f.cs.DismissDecisions(ctx, f.dismissInput(versions[1].MessageID, "user-1", "", func() time.Time {
		return versions[1].CreatedAt.Add(time.Minute)
	}))
	require.ErrorIs(t, err, store.ErrDecisionTooRecent)

	// Dismissing an already-decided prompt returns DismissChangedError with reason "decided".
	_, _, err = f.cs.RecordAnswer(ctx, f.answer(versions[1].ID, "1", "user-1", ""))
	require.NoError(t, err)
	_, err = f.cs.DismissDecisions(ctx, f.dismissInput(versions[1].MessageID, "user-1", "", func() time.Time {
		return versions[1].CreatedAt.Add(store.DecisionDismissMinAge)
	}))
	var changedErr *store.DismissChangedError
	require.ErrorAs(t, err, &changedErr)
	require.ErrorIs(t, err, store.ErrDismissChanged)
	assert.Equal(t, store.AnswerSetReasonDecided, changedErr.Reasons[versions[1].ID])
}

func TestDismissDecisions_FreesPendingCapacity(t *testing.T) {
	f := newDecisionFixture(t)
	ctx := context.Background()

	var firstMsgID string
	var firstCreatedAt time.Time
	for b := 0; b < 5; b++ {
		ds := make([]messages.Decision, 10)
		for i := 0; i < 10; i++ {
			ds[i] = yesNo(fmt.Sprintf("DCAP-%d-%d", b, i), "Cap check?")
		}
		p := f.create(t, false, ds...)
		if b == 0 {
			d0, err := f.cs.GetDecision(ctx, p.Decisions[0].RowID)
			require.NoError(t, err)
			firstMsgID = d0.MessageID
			firstCreatedAt = d0.CreatedAt
		}
	}

	_, err := f.cs.CreateDecisionPrompt(ctx, f.prompt("", false, yesNo("DCAP-OVER", "Too many?")))
	require.ErrorIs(t, err, store.ErrTooManyPendingDecisions)

	_, err = f.cs.DismissDecisions(ctx, f.dismissInput(firstMsgID, "user-1", "Unblock cap", func() time.Time {
		return firstCreatedAt.Add(store.DecisionDismissMinAge)
	}))
	require.NoError(t, err)

	overRes, err := f.cs.CreateDecisionPrompt(ctx, f.prompt("", false, yesNo("DCAP-OVER", "Fits now?")))
	require.NoError(t, err)
	require.Len(t, overRes.Decisions, 1)
}

func (f *decisionFixture) createTopicSet(t *testing.T, ds ...messages.Decision) (string, []string) {
	t.Helper()
	in := f.prompt("", false, ds...)
	in.Message.ThreadID = "topic:general"
	res, err := f.cs.CreateDecisionPrompt(context.Background(), in)
	require.NoError(t, err)
	ids := make([]string, len(res.Decisions))
	for i, d := range res.Decisions {
		ids[i] = d.RowID
	}
	return in.Message.ID, ids
}

func TestDismissDecisions_SubsetsAllOrNothingAndEmptyRowIDs(t *testing.T) {
	f := newDecisionFixture(t)
	ctx := context.Background()

	msgID, rows := f.createTopicSet(t,
		yesNo("DSM-SUB-1", "First?"),
		yesNo("DSM-SUB-2", "Second?"),
		yesNo("DSM-SUB-3", "Third?"),
	)
	d0, err := f.cs.GetDecision(ctx, rows[0])
	require.NoError(t, err)
	createdAt := d0.CreatedAt
	t5m := createdAt.Add(store.DecisionDismissMinAge)

	// Explicit subset before 5 minutes -> DismissChangedError with reason "too_recent", nothing stored.
	earlySub := f.dismissInput(msgID, "user-1", "Too early subset", func() time.Time {
		return createdAt.Add(4*time.Minute + 59*time.Second)
	})
	earlySub.RowIDs = []string{rows[0], rows[2]}
	_, err = f.cs.DismissDecisions(ctx, earlySub)
	var changedErr *store.DismissChangedError
	require.ErrorAs(t, err, &changedErr)
	assert.Equal(t, map[string]string{
		rows[0]: store.AnswerSetReasonTooRecent,
		rows[2]: store.AnswerSetReasonTooRecent,
	}, changedErr.Reasons)
	assert.WithinDuration(t, t5m, changedErr.AvailableAt, time.Second, "too-recent subset reports when it becomes dismissable")
	_, err = f.cs.GetMessage(ctx, earlySub.ResponseMessage.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)

	// Explicit subset at +5m -> dismisses rows[0] and rows[2] in prompt position order; leaves rows[1] pending.
	subIn := f.dismissInput(msgID, "user-1", "Subset dismiss", func() time.Time { return t5m })
	subIn.RowIDs = []string{rows[2], rows[0]} // out-of-order input returns prompt position order
	subRes, err := f.cs.DismissDecisions(ctx, subIn)
	require.NoError(t, err)
	require.Len(t, subRes.Dismissed, 2)
	assert.Equal(t, "DSM-SUB-1", subRes.Dismissed[0].Key)
	assert.Equal(t, "DSM-SUB-3", subRes.Dismissed[1].Key)
	assert.Empty(t, subRes.AlreadyDismissed)

	stillOpen, err := f.cs.GetDecision(ctx, rows[1])
	require.NoError(t, err)
	assert.Equal(t, store.DecisionStatusPending, stillOpen.Status)

	// Prompt remains in ListOpenPrompts because rows[1] is still pending.
	openPrompts, err := f.cs.ListOpenPrompts(ctx, []string{f.projectID}, []string{"topic:general"})
	require.NoError(t, err)
	require.Len(t, openPrompts, 1)

	// Answer rows[1] directly, then try explicit subset [rows[0], rows[1]]:
	// rows[0] is already dismissed (allowed), rows[1] is decided (refused) -> all-or-nothing DismissChangedError!
	_, err = f.cs.RecordAnswerSet(ctx, f.answerSet(msgID, "user-2", "", rows[1], "1"))
	require.NoError(t, err)

	mixedIn := f.dismissInput(msgID, "user-1", "Should fail", func() time.Time { return t5m.Add(time.Minute) })
	mixedIn.RowIDs = []string{rows[0], rows[1]}
	_, err = f.cs.DismissDecisions(ctx, mixedIn)
	require.ErrorAs(t, err, &changedErr)
	assert.Equal(t, map[string]string{rows[1]: store.AnswerSetReasonDecided}, changedErr.Reasons)

	// Explicit subset naming only already-dismissed rows -> 200 idempotent AlreadyDismissed, nothing stored.
	repeatSub := f.dismissInput(msgID, "user-1", "Ignored", func() time.Time { return t5m.Add(2 * time.Minute) })
	repeatSub.RowIDs = []string{rows[0], rows[2]}
	repeatRes, err := f.cs.DismissDecisions(ctx, repeatSub)
	require.NoError(t, err)
	assert.Empty(t, repeatRes.Dismissed)
	require.Len(t, repeatRes.AlreadyDismissed, 2)
	_, err = f.cs.GetMessage(ctx, repeatSub.ResponseMessage.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)

	// Empty RowIDs skips too-recent open rows when another open row is >= 5m,
	// and returns DecisionTooRecentError with earliest AvailableAt once only too-recent rows remain.
	pAge := f.create(t, false, yesNo("DSM-AGE-1", "Mature?"))
	dAge0, err := f.cs.GetDecision(ctx, pAge.Decisions[0].RowID)
	require.NoError(t, err)
	msg2 := dAge0.MessageID
	msg2UUID, err := uuid.Parse(msg2)
	require.NoError(t, err)
	projUUID, err := uuid.Parse(f.projectID)
	require.NoError(t, err)
	agentUUID, err := uuid.Parse(f.agentID)
	require.NoError(t, err)
	freshCreatedAt := dAge0.CreatedAt.Add(3 * time.Minute).Truncate(time.Microsecond)
	_, err = f.cs.client.Decision.Create().
		SetID(uuid.New()).
		SetProjectID(projUUID).
		SetChatKey("topic:general").
		SetMessageID(msg2UUID).
		SetPosition(1).
		SetAskerAgentID(agentUUID).
		SetAskerAgentName("architect").
		SetKey("DSM-AGE-2").
		SetKeyNorm("dsm-age-2").
		SetKeySource(store.DecisionKeySourceAgent).
		SetVersion(1).
		SetQuestion("Fresh?").
		SetChoices(dAge0.Choices).
		SetCreatedAt(freshCreatedAt).
		Save(ctx)
	require.NoError(t, err)

	tAge := dAge0.CreatedAt.Add(store.DecisionDismissMinAge)
	partialRes, err := f.cs.DismissDecisions(ctx, f.dismissInput(msg2, "user-1", "Mature only", func() time.Time { return tAge }))
	require.NoError(t, err)
	require.Len(t, partialRes.Dismissed, 1)
	assert.Equal(t, "DSM-AGE-1", partialRes.Dismissed[0].Key)

	// Now only DSM-AGE-2 is open and still < 5m old -> DecisionTooRecentError with DSM-AGE-2's AvailableAt.
	_, err = f.cs.DismissDecisions(ctx, f.dismissInput(msg2, "user-1", "Too soon for second", func() time.Time { return tAge }))
	var tooRecent *store.DecisionTooRecentError
	require.ErrorAs(t, err, &tooRecent)
	assert.True(t, freshCreatedAt.Add(store.DecisionDismissMinAge).Equal(tooRecent.AvailableAt))
}

func TestDismissDecisions_IdempotencyReplayAndMismatch(t *testing.T) {
	f := newDecisionFixture(t)
	ctx := context.Background()

	msgID, rows := f.createSet(t, yesNo("DSM-IDEM-1", "Q1?"), yesNo("DSM-IDEM-2", "Q2?"))
	d0, err := f.cs.GetDecision(ctx, rows[0])
	require.NoError(t, err)
	t5m := d0.CreatedAt.Add(store.DecisionDismissMinAge)

	firstIn := f.dismissInput(msgID, "user-1", "  Same trimmed reason  ", func() time.Time { return t5m })
	firstIn.RowIDs = []string{rows[0]}
	firstIn.IdempotencyKey = "idem-key-1"
	firstRes, err := f.cs.DismissDecisions(ctx, firstIn)
	require.NoError(t, err)
	assert.False(t, firstRes.Replayed)
	require.Len(t, firstRes.Dismissed, 1)

	// Exact retry with same key, row set, and trimmed reason replays without storing a new message.
	retryIn := f.dismissInput(msgID, "user-1", "Same trimmed reason", func() time.Time { return t5m.Add(time.Minute) })
	retryIn.RowIDs = []string{rows[0]}
	retryIn.IdempotencyKey = "idem-key-1"
	retryRes, err := f.cs.DismissDecisions(ctx, retryIn)
	require.NoError(t, err)
	assert.True(t, retryRes.Replayed)
	assert.Equal(t, firstRes.BatchID, retryRes.BatchID)
	require.Len(t, retryRes.Dismissed, 1)
	assert.Equal(t, firstIn.ResponseMessage.ID, retryRes.Dismissed[0].Dismissal().ResponseMessageID)
	_, err = f.cs.GetMessage(ctx, retryIn.ResponseMessage.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)

	// Mismatch on reason -> ErrIdempotencyMismatch.
	diffReasonIn := f.dismissInput(msgID, "user-1", "Different reason", func() time.Time { return t5m.Add(time.Minute) })
	diffReasonIn.RowIDs = []string{rows[0]}
	diffReasonIn.IdempotencyKey = "idem-key-1"
	_, err = f.cs.DismissDecisions(ctx, diffReasonIn)
	require.ErrorIs(t, err, store.ErrIdempotencyMismatch)

	// Mismatch on explicit row set -> ErrIdempotencyMismatch.
	diffRowsIn := f.dismissInput(msgID, "user-1", "Same trimmed reason", func() time.Time { return t5m.Add(time.Minute) })
	diffRowsIn.RowIDs = []string{rows[0], rows[1]}
	diffRowsIn.IdempotencyKey = "idem-key-1"
	_, err = f.cs.DismissDecisions(ctx, diffRowsIn)
	require.ErrorIs(t, err, store.ErrIdempotencyMismatch)

	// Mismatch calling empty RowIDs under a key that only dismissed a subset while rows[1] was eligible -> ErrIdempotencyMismatch.
	emptyRowsIn := f.dismissInput(msgID, "user-1", "Same trimmed reason", func() time.Time { return t5m.Add(time.Minute) })
	emptyRowsIn.IdempotencyKey = "idem-key-1"
	_, err = f.cs.DismissDecisions(ctx, emptyRowsIn)
	require.ErrorIs(t, err, store.ErrIdempotencyMismatch)

	// Another user using the same key string on the same prompt does NOT collide with user-1's key.
	otherUserIn := f.dismissInput(msgID, "user-2", "User 2 dismisses Q2", func() time.Time { return t5m.Add(time.Minute) })
	otherUserIn.RowIDs = []string{rows[1]}
	otherUserIn.IdempotencyKey = "idem-key-1"
	otherRes, err := f.cs.DismissDecisions(ctx, otherUserIn)
	require.NoError(t, err)
	assert.False(t, otherRes.Replayed)
	require.Len(t, otherRes.Dismissed, 1)
	assert.Equal(t, "DSM-IDEM-2", otherRes.Dismissed[0].Key)
}

func TestDismissDecisions_InterpretedWithdrawReuseMadeAndAgentList(t *testing.T) {
	f := newDecisionFixture(t)
	ctx := context.Background()

	// 1. D6: interpreted-awaiting-confirm row can be dismissed by the named user,
	// keeping the interpreted row in decision_answers and transitioning status to dismissed.
	msgID, rows := f.createTopicSet(t, yesNo("D-INT-1", "Ship today?"), yesNo("D-INT-2", "Run migration?"))
	d0, err := f.cs.GetDecision(ctx, rows[0])
	require.NoError(t, err)
	t5m := d0.CreatedAt.Add(store.DecisionDismissMinAge)

	srcMsg := &store.Message{
		ID:            uuid.NewString(),
		ProjectID:     f.projectID,
		ThreadID:      "topic:general",
		Sender:        "user:user-1",
		SenderID:      "user-1",
		Recipient:     "group:topic",
		RecipientID:   "topic:general",
		Msg:           "looks good",
		DispatchState: store.MessageDispatchDispatched,
		CreatedAt:     d0.CreatedAt.Add(time.Minute),
	}
	require.NoError(t, f.cs.CreateMessage(ctx, srcMsg))

	_, err = f.cs.RecordInterpretedAnswers(ctx, &store.RecordInterpretedInput{
		InterpretedByAgentID: f.agentID,
		SourceMessageID:      srcMsg.ID,
		AnsweredByUserID:     "user-1",
		AnsweredByName:       "Pat",
		AnsweredByEmail:      "pat@example.com",
		Surface:              store.DecisionSurfaceNative,
		Answers:              []store.InterpretedAnswerPair{{Key: "D-INT-1", ChoiceID: "1"}},
		ResponseMessage: &store.Message{
			ID:            uuid.NewString(),
			ProjectID:     f.projectID,
			ThreadID:      "topic:general",
			Sender:        "agent:architect",
			SenderID:      f.agentID,
			Recipient:     "group:topic",
			RecipientID:   "topic:general",
			Msg:           "interpreted D-INT-1",
			DispatchState: store.MessageDispatchDispatched,
			CreatedAt:     d0.CreatedAt.Add(2 * time.Minute),
		},
	})
	require.NoError(t, err)

	// Another user cannot dismiss an interpreted row naming user-1 -> not_recorded_answerer.
	wrongUserIn := f.dismissInput(msgID, "user-2", "Not mine", func() time.Time { return t5m })
	wrongUserIn.RowIDs = []string{rows[0]}
	_, err = f.cs.DismissDecisions(ctx, wrongUserIn)
	var changedErr *store.DismissChangedError
	require.ErrorAs(t, err, &changedErr)
	assert.Equal(t, store.AnswerSetReasonNotRecordedAnswerer, changedErr.Reasons[rows[0]])
	assert.True(t, changedErr.AvailableAt.IsZero(), "no too-recent row: no availableAt")

	// Named user still cannot dismiss before version age reaches 5m (age counts from version creation, 4.4a).
	tooSoonInterp := f.dismissInput(msgID, "user-1", "Too soon", func() time.Time {
		return d0.CreatedAt.Add(4*time.Minute + 59*time.Second)
	})
	tooSoonInterp.RowIDs = []string{rows[0]}
	_, err = f.cs.DismissDecisions(ctx, tooSoonInterp)
	require.ErrorAs(t, err, &changedErr)
	assert.Equal(t, store.AnswerSetReasonTooRecent, changedErr.Reasons[rows[0]])

	// Named user dismisses rows[0] at +5m -> status becomes dismissed, interpreted row stays in Answers!
	dsmInterpIn := f.dismissInput(msgID, "user-1", "Misread reply — xylophone check", func() time.Time { return t5m })
	dsmInterpIn.RowIDs = []string{rows[0]}
	dsmInterpRes, err := f.cs.DismissDecisions(ctx, dsmInterpIn)
	require.NoError(t, err)
	require.Len(t, dsmInterpRes.Dismissed, 1)

	gotInterp, err := f.cs.GetDecision(ctx, rows[0])
	require.NoError(t, err)
	assert.Equal(t, store.DecisionStatusDismissed, gotInterp.Status)
	assert.Nil(t, gotInterp.EffectiveAnswer())
	require.NotNil(t, gotInterp.Dismissal())
	require.Len(t, gotInterp.Answers, 2, "interpreted answer row must remain in the log alongside the dismissal")
	assert.Equal(t, store.DecisionProvenanceAgentInterpreted, gotInterp.Answers[0].Provenance)
	assert.Equal(t, store.DecisionOutcomeDismiss, gotInterp.Answers[1].Outcome)

	// 2. Set submit including dismissed row fails with reason "dismissed"; submitting remaining row succeeds!
	_, err = f.cs.RecordAnswerSet(ctx, f.answerSet(msgID, "user-1", "", rows[0], "1", rows[1], "2"))
	var setChanged *store.AnswerSetChangedError
	require.ErrorAs(t, err, &setChanged)
	assert.Equal(t, map[string]string{rows[0]: store.AnswerSetReasonDismissed}, setChanged.Reasons)

	_, err = f.cs.RecordAnswerSet(ctx, f.answerSet(msgID, "user-1", "", rows[1], "2"))
	require.NoError(t, err)

	// 3. Agent interpreted answer on a dismissed key fails with per-key "dismissed".
	_, err = f.cs.RecordInterpretedAnswers(ctx, &store.RecordInterpretedInput{
		InterpretedByAgentID: f.agentID,
		SourceMessageID:      srcMsg.ID,
		AnsweredByUserID:     "user-1",
		AnsweredByName:       "Pat",
		Surface:              store.DecisionSurfaceNative,
		Answers:              []store.InterpretedAnswerPair{{Key: "D-INT-1", ChoiceID: "2"}},
		ResponseMessage: &store.Message{
			ID:            uuid.NewString(),
			ProjectID:     f.projectID,
			ThreadID:      "topic:general",
			Sender:        "agent:architect",
			SenderID:      f.agentID,
			Recipient:     "group:topic",
			RecipientID:   "topic:general",
			Msg:           "should not store",
			DispatchState: store.MessageDispatchDispatched,
		},
	})
	var iaErr *store.InterpretedAnswersError
	require.ErrorAs(t, err, &iaErr)
	require.Len(t, iaErr.Failures, 1)
	assert.Equal(t, "D-INT-1", iaErr.Failures[0].Key)
	assert.Equal(t, store.InterpretedReasonDismissed, iaErr.Failures[0].Reason)

	// 4. Withdraw interaction (section 4.8): explicit dismissed key fails all-or-nothing with "dismissed";
	// WholeSet (--set) from a pending sibling skips dismissed siblings!
	wdMsgID, wdRows := f.createTopicSet(t, yesNo("D-WD-1", "Obsolete 1?"), yesNo("D-WD-2", "Obsolete 2?"))
	dWd0, err := f.cs.GetDecision(ctx, wdRows[0])
	require.NoError(t, err)
	dsmWdIn := f.dismissInput(wdMsgID, "user-1", "User dismissed first", func() time.Time {
		return dWd0.CreatedAt.Add(store.DecisionDismissMinAge)
	})
	dsmWdIn.RowIDs = []string{wdRows[0]}
	_, err = f.cs.DismissDecisions(ctx, dsmWdIn)
	require.NoError(t, err)

	_, err = f.cs.WithdrawDecisions(ctx, &store.WithdrawDecisionsInput{
		AskerAgentID: f.agentID,
		Keys:         []string{"D-WD-1", "D-WD-2"},
		Reason:       "Try withdraw both",
	})
	var wdErr *store.WithdrawDecisionsError
	require.ErrorAs(t, err, &wdErr)
	require.Len(t, wdErr.Failures, 1)
	assert.Equal(t, "D-WD-1", wdErr.Failures[0].Key)
	assert.Equal(t, store.WithdrawReasonDismissed, wdErr.Failures[0].Reason)

	// WholeSet on pending D-WD-2 withdraws D-WD-2 and skips dismissed sibling D-WD-1!
	wdSetRes, err := f.cs.WithdrawDecisions(ctx, &store.WithdrawDecisionsInput{
		AskerAgentID: f.agentID,
		Keys:         []string{"D-WD-2"},
		WholeSet:     true,
		Reason:       "Withdraw remaining open",
	})
	require.NoError(t, err)
	require.Len(t, wdSetRes.Withdrawn, 1)
	assert.Equal(t, "D-WD-2", wdSetRes.Withdrawn[0].Key)

	gotWd0, err := f.cs.GetDecision(ctx, wdRows[0])
	require.NoError(t, err)
	assert.Equal(t, store.DecisionStatusDismissed, gotWd0.Status, "dismissed sibling stays dismissed")

	// 5. Reusing a dismissed key without --update gives hint "D-WD-1 already exists (v1, dismissed by Pat)".
	_, err = f.cs.CreateDecisionPrompt(ctx, f.prompt("", false, yesNo("D-WD-1", "Reuse without update?")))
	var keyExists *store.DecisionKeyExistsError
	require.ErrorAs(t, err, &keyExists)
	require.Len(t, keyExists.Existing, 1)
	assert.Equal(t, "D-WD-1 already exists (v1, dismissed by Pat)", keyExists.Existing[0].Hint())

	// 6. Made log (ListDecisionAnswers): provenance=dismissed includes only dismiss rows;
	// other provenances exclude dismiss rows; Query matches dismiss_reason!
	dsmEntries, _, err := f.cs.ListDecisionAnswers(ctx, []string{f.projectID}, []string{"topic:general"}, store.ProjectDecisionAnswerFilter{
		Provenance: store.ProjectDecisionProvenanceDismissed,
	}, "", 50)
	require.NoError(t, err)
	require.Len(t, dsmEntries, 2)
	for _, e := range dsmEntries {
		assert.Equal(t, store.DecisionOutcomeDismiss, e.Answer.Outcome)
	}

	for _, otherProv := range []string{
		store.ProjectDecisionProvenanceDirect,
		store.ProjectDecisionProvenanceInterpreted,
		store.ProjectDecisionProvenanceConfirmed,
		store.ProjectDecisionProvenanceOverridden,
	} {
		entries, _, err := f.cs.ListDecisionAnswers(ctx, []string{f.projectID}, []string{"topic:general"}, store.ProjectDecisionAnswerFilter{
			Provenance: otherProv,
		}, "", 50)
		require.NoError(t, err)
		for _, e := range entries {
			assert.NotEqual(t, store.DecisionOutcomeDismiss, e.Answer.Outcome, "provenance=%s must exclude dismiss rows", otherProv)
		}
	}

	reasonEntries, _, err := f.cs.ListDecisionAnswers(ctx, []string{f.projectID}, []string{"topic:general"}, store.ProjectDecisionAnswerFilter{
		Query: "xylophone",
	}, "", 50)
	require.NoError(t, err)
	require.Len(t, reasonEntries, 1)
	assert.Equal(t, "D-INT-1", reasonEntries[0].Decision.Key)
	assert.Equal(t, "Misread reply — xylophone check", reasonEntries[0].Answer.DismissReason)

	// 7. Agent ListAgentDecisions (DecisionListStateDismissed + query) and GetAgentDecisionVersions.
	dsmList, _, err := f.cs.ListAgentDecisions(ctx, f.agentID, store.DecisionListStateDismissed, "", "", 50)
	require.NoError(t, err)
	require.Len(t, dsmList, 2)
	assert.Equal(t, "D-WD-1", dsmList[0].Key)
	assert.Equal(t, "D-INT-1", dsmList[1].Key)

	dsmSearch, _, err := f.cs.ListAgentDecisions(ctx, f.agentID, store.DecisionListStateDismissed, "xylophone", "", 50)
	require.NoError(t, err)
	require.Len(t, dsmSearch, 1)
	assert.Equal(t, "D-INT-1", dsmSearch[0].Key)
}

func TestRecordAnswer_AllowOther_RoundTripIdempotencyTerminalAndUserDeletion(t *testing.T) {
	f := newDecisionFixture(t)
	ctx := context.Background()

	// Create a real user row so we can verify DeleteUser preserves other_text (D6).
	userID := uuid.NewString()
	require.NoError(t, f.cs.CreateUser(ctx, &store.User{
		ID:          userID,
		Email:       "alice@example.com",
		DisplayName: "Alice",
		Role:        store.UserRoleMember,
		Status:      "active",
	}))

	// 1. Decision with 1 fixed choice + Other round-trips all fields; decision without Other has Other == nil.
	withOther := messages.Decision{
		Key:             "region",
		Question:        "Which region for the canary?",
		DefaultChoiceID: "1",
		Choices:         []messages.Choice{{ID: "1", Label: "us-central1"}},
		Other: &messages.OtherOption{
			Label:       "Another region",
			Description: "Any GCP region code",
			Placeholder: "e.g. europe-west4",
		},
	}
	resOther := f.create(t, false, withOther)
	rowOther := resOther.Decisions[0].RowID

	gotOther, err := f.cs.GetDecision(ctx, rowOther)
	require.NoError(t, err)
	require.NotNil(t, gotOther.Other)
	assert.Equal(t, *withOther.Other, *gotOther.Other)
	assert.Len(t, gotOther.Choices, 1)

	byMsg, err := f.cs.GetDecisionsByMessageIDs(ctx, []string{gotOther.MessageID})
	require.NoError(t, err)
	require.Len(t, byMsg[gotOther.MessageID], 1)
	require.NotNil(t, byMsg[gotOther.MessageID][0].Other)
	assert.Equal(t, *withOther.Other, *byMsg[gotOther.MessageID][0].Other)

	byRow, err := f.cs.GetDecisionRowsByIDs(ctx, []string{rowOther})
	require.NoError(t, err)
	require.NotNil(t, byRow[rowOther].Other)
	assert.Equal(t, *withOther.Other, *byRow[rowOther].Other)

	resNoOther := f.create(t, false, yesNo("no-other", "Plain choice?"))
	rowNoOther := resNoOther.Decisions[0].RowID
	gotNoOther, err := f.cs.GetDecision(ctx, rowNoOther)
	require.NoError(t, err)
	assert.Nil(t, gotNoOther.Other)

	// 2. Validation rejections on RecordAnswer.
	badNoOther := f.answer(rowNoOther, messages.ChoiceIDOther, userID, "")
	badNoOther.OtherText = "custom"
	_, _, err = f.cs.RecordAnswer(ctx, badNoOther)
	assert.ErrorIs(t, err, store.ErrInvalidInput, "$other on decision without AllowOther must fail")

	for name, mutate := range map[string]func(in *store.RecordAnswerInput){
		"empty text":  func(in *store.RecordAnswerInput) { in.ChoiceID = messages.ChoiceIDOther; in.OtherText = "" },
		"spaces only": func(in *store.RecordAnswerInput) { in.ChoiceID = messages.ChoiceIDOther; in.OtherText = "   " },
		"newline":     func(in *store.RecordAnswerInput) { in.ChoiceID = messages.ChoiceIDOther; in.OtherText = "a\nb" },
		"tab":         func(in *store.RecordAnswerInput) { in.ChoiceID = messages.ChoiceIDOther; in.OtherText = "a\tb" },
		"bidi":        func(in *store.RecordAnswerInput) { in.ChoiceID = messages.ChoiceIDOther; in.OtherText = "a\u202eb" },
		"201 runes": func(in *store.RecordAnswerInput) {
			in.ChoiceID = messages.ChoiceIDOther
			in.OtherText = strings.Repeat("é", 201)
		},
		"text on fixed":       func(in *store.RecordAnswerInput) { in.ChoiceID = "1"; in.OtherText = "extra" },
		"space text on fixed": func(in *store.RecordAnswerInput) { in.ChoiceID = "1"; in.OtherText = " " },
	} {
		t.Run(name, func(t *testing.T) {
			in := f.answer(rowOther, "1", userID, "")
			mutate(in)
			_, _, err := f.cs.RecordAnswer(ctx, in)
			assert.ErrorIs(t, err, store.ErrInvalidInput)
		})
	}

	// 3. RecordAnswer with $other stores label snapshot, trimmed text, and MatchedDefault=false.
	ansIn := f.answer(rowOther, messages.ChoiceIDOther, userID, "idem-other-1")
	ansIn.AnsweredByName = "Alice"
	ansIn.AnsweredByEmail = "alice@example.com"
	ansIn.OtherText = "  europe-west4 canary  "
	a1, replayed, err := f.cs.RecordAnswer(ctx, ansIn)
	require.NoError(t, err)
	assert.False(t, replayed)
	assert.Equal(t, messages.ChoiceIDOther, a1.ChoiceID)
	assert.Equal(t, "Another region", a1.ChoiceLabel)
	assert.Equal(t, "europe-west4 canary", a1.OtherText)
	assert.False(t, a1.MatchedDefault)

	storedDec, err := f.cs.GetDecision(ctx, rowOther)
	require.NoError(t, err)
	require.NotNil(t, storedDec.EffectiveAnswer())
	assert.Equal(t, "europe-west4 canary", storedDec.EffectiveAnswer().OtherText)

	byResp, err := f.cs.GetDecisionAnswersByResponseMessageIDs(ctx, []string{a1.ResponseMessageID})
	require.NoError(t, err)
	require.Len(t, byResp[a1.ResponseMessageID], 1)
	assert.Equal(t, "europe-west4 canary", byResp[a1.ResponseMessageID][0].OtherText)

	// 4. Idempotency: same key + same (trimmed) text replays; same key + different text conflicts.
	retrySame := f.answer(rowOther, messages.ChoiceIDOther, userID, "idem-other-1")
	retrySame.OtherText = "europe-west4 canary"
	a2, replayed, err := f.cs.RecordAnswer(ctx, retrySame)
	require.NoError(t, err)
	assert.True(t, replayed)
	assert.Equal(t, a1.ID, a2.ID)
	assert.Equal(t, "europe-west4 canary", a2.OtherText)

	retryDiffText := f.answer(rowOther, messages.ChoiceIDOther, userID, "idem-other-1")
	retryDiffText.OtherText = "us-east4"
	_, _, err = f.cs.RecordAnswer(ctx, retryDiffText)
	assert.ErrorIs(t, err, store.ErrDecisionAnswerConflict)

	retryFixed := f.answer(rowOther, "1", userID, "idem-other-1")
	_, _, err = f.cs.RecordAnswer(ctx, retryFixed)
	assert.ErrorIs(t, err, store.ErrDecisionAnswerConflict)

	// 5. User deletion keeps other_text snapshot while clearing answered_by_user_id (D6).
	require.NoError(t, f.cs.DeleteUser(ctx, userID))
	afterDel, err := f.cs.GetDecision(ctx, rowOther)
	require.NoError(t, err)
	require.NotNil(t, afterDel.EffectiveAnswer())
	assert.Empty(t, afterDel.EffectiveAnswer().AnsweredByUserID)
	assert.Equal(t, "Alice", afterDel.EffectiveAnswer().AnsweredByName)
	assert.Equal(t, "europe-west4 canary", afterDel.EffectiveAnswer().OtherText)

	// 6. Terminal statuses (replaced, withdrawn) refuse $other answers first.
	repV1 := f.create(t, false, messages.Decision{
		Key:      "rep-other",
		Question: "Replace me?",
		Choices:  []messages.Choice{{ID: "1", Label: "Yes"}},
		Other:    &messages.OtherOption{Label: "Other"},
	}).Decisions[0].RowID
	// Replace v1 with v2 that drops Other.
	repV2 := f.create(t, true, yesNo("rep-other", "Replaced without Other?")).Decisions[0].RowID
	gotRepV2, err := f.cs.GetDecision(ctx, repV2)
	require.NoError(t, err)
	assert.Nil(t, gotRepV2.Other)

	ansReplaced := f.answer(repV1, messages.ChoiceIDOther, "user-2", "")
	ansReplaced.OtherText = "too late"
	_, _, err = f.cs.RecordAnswer(ctx, ansReplaced)
	assert.ErrorIs(t, err, store.ErrDecisionReplaced)

	wdRow := f.create(t, false, messages.Decision{
		Key:      "wd-other",
		Question: "Withdraw me?",
		Choices:  []messages.Choice{{ID: "1", Label: "Yes"}},
		Other:    &messages.OtherOption{Label: "Other"},
	}).Decisions[0].RowID
	_, err = f.cs.WithdrawDecisions(ctx, &store.WithdrawDecisionsInput{
		AskerAgentID: f.agentID,
		Keys:         []string{"wd-other"},
	})
	require.NoError(t, err)
	ansWithdrawn := f.answer(wdRow, messages.ChoiceIDOther, "user-2", "")
	ansWithdrawn.OtherText = "too late"
	_, _, err = f.cs.RecordAnswer(ctx, ansWithdrawn)
	assert.ErrorIs(t, err, store.ErrDecisionWithdrawn)

	// Dismissed rows refuse $other with ErrDecisionDismissed (even if reusing
	// the dismissal's idempotency key), and $dismissed is refused on RecordAnswer.
	badReserved := f.answer(rowOther, messages.ChoiceIDDismissed, "user-2", "")
	_, _, err = f.cs.RecordAnswer(ctx, badReserved)
	assert.ErrorIs(t, err, store.ErrInvalidInput)

	dsmRes := f.create(t, false, messages.Decision{
		Key:      "dsm-other",
		Question: "Dismiss me?",
		Choices:  []messages.Choice{{ID: "1", Label: "Yes"}},
		Other:    &messages.OtherOption{Label: "Other"},
	})
	dsmRow := dsmRes.Decisions[0].RowID
	dsmDec, err := f.cs.GetDecision(ctx, dsmRow)
	require.NoError(t, err)
	dsmIn := f.dismissInput(dsmDec.MessageID, "user-2", "No longer relevant", func() time.Time {
		return dsmDec.CreatedAt.Add(store.DecisionDismissMinAge)
	})
	dsmIn.IdempotencyKey = "shared-dsm-key"
	_, err = f.cs.DismissDecisions(ctx, dsmIn)
	require.NoError(t, err)

	ansDismissed := f.answer(dsmRow, messages.ChoiceIDOther, "user-2", "shared-dsm-key")
	ansDismissed.OtherText = "too late"
	_, _, err = f.cs.RecordAnswer(ctx, ansDismissed)
	assert.ErrorIs(t, err, store.ErrDecisionDismissed)

	// 7. Multibyte at-limit Other fields (100 Cyrillic runes label, 300 CJK runes
	// description, 100 CJK runes placeholder) round-trip cleanly via MaxRuneLen,
	// and un-normalized over-limit inputs fail as ErrInvalidInput (never raw Ent ValidationError).
	cyrillicLabel100 := strings.Repeat("д", messages.MaxChoiceLabelLen)
	cjkDesc300 := strings.Repeat("東", messages.MaxChoiceDescriptionLen)
	cjkPlaceholder100 := strings.Repeat("東", messages.MaxOtherPlaceholderLen)
	multibyteDec := messages.Decision{
		Key:      "multibyte-other",
		Question: "Multibyte Other limits?",
		Choices:  []messages.Choice{{ID: "1", Label: "Yes"}},
		Other: &messages.OtherOption{
			Label:       cyrillicLabel100,
			Description: cjkDesc300,
			Placeholder: cjkPlaceholder100,
		},
	}
	mbRes := f.create(t, false, multibyteDec)
	gotMB, err := f.cs.GetDecision(ctx, mbRes.Decisions[0].RowID)
	require.NoError(t, err)
	require.NotNil(t, gotMB.Other)
	assert.Equal(t, cyrillicLabel100, gotMB.Other.Label)
	assert.Equal(t, cjkDesc300, gotMB.Other.Description)
	assert.Equal(t, cjkPlaceholder100, gotMB.Other.Placeholder)

	for name, badOther := range map[string]*messages.OtherOption{
		"label 101 Cyrillic":  {Label: strings.Repeat("д", messages.MaxChoiceLabelLen+1)},
		"desc 301 CJK":        {Label: "Other", Description: strings.Repeat("東", messages.MaxChoiceDescriptionLen+1)},
		"placeholder 101 CJK": {Label: "Other", Placeholder: strings.Repeat("東", messages.MaxOtherPlaceholderLen+1)},
	} {
		t.Run(name, func(t *testing.T) {
			pIn := f.prompt("", false, messages.Decision{
				Key:      "bad-mb",
				Question: "Too long?",
				Choices:  []messages.Choice{{ID: "1", Label: "Yes"}},
				Other:    badOther,
			})
			_, err := f.cs.CreateDecisionPrompt(ctx, pIn)
			require.ErrorIs(t, err, store.ErrInvalidInput)
		})
	}
}

func TestRecordAnswerSet_AllowOther_AtomicIdempotencyAndDismiss(t *testing.T) {
	f := newDecisionFixture(t)
	ctx := context.Background()

	// 3-question set: row 0 has object Other, row 1 has no Other, row 2 has string Other with 1 fixed choice.
	pIn := f.prompt("Three decisions", false,
		messages.Decision{
			Key:             "region",
			Question:        "Which region for the canary?",
			DefaultChoiceID: "1",
			Choices:         []messages.Choice{{ID: "1", Label: "us-central1"}, {ID: "2", Label: "europe-west1"}},
			Other:           &messages.OtherOption{Label: "Another region", Placeholder: "e.g. europe-west4"},
		},
		yesNo("notify", "Notify the release channel?"),
		messages.Decision{
			Key:      "proceed",
			Question: "Tag v0.9 now?",
			Choices:  []messages.Choice{{ID: "1", Label: "Yes"}},
			Other:    &messages.OtherOption{Label: "Something else"},
		},
	)
	res, err := f.cs.CreateDecisionPrompt(ctx, pIn)
	require.NoError(t, err)
	require.Len(t, res.Decisions, 3)
	msgID := pIn.Message.ID
	r0, r1, r2 := res.Decisions[0].RowID, res.Decisions[1].RowID, res.Decisions[2].RowID

	makeSetInput := func(idem string, items []store.AnswerSetItem) *store.RecordAnswerSetInput {
		return &store.RecordAnswerSetInput{
			MessageID:        msgID,
			AnsweredByUserID: "user-1",
			AnsweredByName:   "Alice",
			AnsweredByEmail:  "alice@example.com",
			Surface:          store.DecisionSurfaceNative,
			Answers:          items,
			ResponseMessage: &store.Message{
				ID:        uuid.NewString(),
				ProjectID: f.projectID,
				Sender:    "user:alice@example.com",
				SenderID:  "user-1",
				Recipient: "agent:dev",
				Msg:       "set response",
				Type:      messages.TypeDecisionResponse,
				AgentID:   f.agentID,
				Channel:   "web",
				ThreadID:  "topic:general",
			},
			IdempotencyKey: idem,
		}
	}

	// 1. Atomic validation rejections: nothing stored when any row fails.
	badCases := map[string][]store.AnswerSetItem{
		"$other on non-Other row": {
			{DecisionRowID: r0, ChoiceID: "1"},
			{DecisionRowID: r1, ChoiceID: messages.ChoiceIDOther, OtherText: "maybe"},
			{DecisionRowID: r2, ChoiceID: "1"},
		},
		"empty other_text on $other row": {
			{DecisionRowID: r0, ChoiceID: messages.ChoiceIDOther, OtherText: "   "},
			{DecisionRowID: r1, ChoiceID: "1"},
			{DecisionRowID: r2, ChoiceID: "1"},
		},
		"newline in other_text": {
			{DecisionRowID: r0, ChoiceID: messages.ChoiceIDOther, OtherText: "a\nb"},
			{DecisionRowID: r1, ChoiceID: "1"},
			{DecisionRowID: r2, ChoiceID: "1"},
		},
		"201 runes in other_text": {
			{DecisionRowID: r0, ChoiceID: messages.ChoiceIDOther, OtherText: strings.Repeat("é", 201)},
			{DecisionRowID: r1, ChoiceID: "1"},
			{DecisionRowID: r2, ChoiceID: "1"},
		},
		"other_text on fixed choice row": {
			{DecisionRowID: r0, ChoiceID: "1", OtherText: "not allowed"},
			{DecisionRowID: r1, ChoiceID: "1"},
			{DecisionRowID: r2, ChoiceID: "1"},
		},
		"reserved $dismissed choice": {
			{DecisionRowID: r0, ChoiceID: messages.ChoiceIDDismissed},
			{DecisionRowID: r1, ChoiceID: "1"},
			{DecisionRowID: r2, ChoiceID: "1"},
		},
	}
	for name, items := range badCases {
		t.Run(name, func(t *testing.T) {
			_, err := f.cs.RecordAnswerSet(ctx, makeSetInput("", items))
			require.ErrorIs(t, err, store.ErrInvalidInput)
			for _, id := range []string{r0, r1, r2} {
				d, gerr := f.cs.GetDecision(ctx, id)
				require.NoError(t, gerr)
				assert.Equal(t, store.DecisionStatusPending, d.Status)
				assert.Empty(t, d.Answers)
			}
		})
	}

	// 2. Valid set submit with Other on rows 0 and 2, fixed choice on row 1.
	validItems := []store.AnswerSetItem{
		{DecisionRowID: r0, ChoiceID: messages.ChoiceIDOther, OtherText: "  europe-west4 canary  "},
		{DecisionRowID: r1, ChoiceID: "1"},
		{DecisionRowID: r2, ChoiceID: messages.ChoiceIDOther, OtherText: "Wait until Monday"},
	}
	setRes, err := f.cs.RecordAnswerSet(ctx, makeSetInput("set-other-idem-1", validItems))
	require.NoError(t, err)
	assert.False(t, setRes.Replayed)
	require.NotEmpty(t, setRes.BatchID)
	require.Len(t, setRes.Answers, 3)

	assert.Equal(t, messages.ChoiceIDOther, setRes.Answers[0].ChoiceID)
	assert.Equal(t, "Another region", setRes.Answers[0].ChoiceLabel)
	assert.Equal(t, "europe-west4 canary", setRes.Answers[0].OtherText)
	assert.False(t, setRes.Answers[0].MatchedDefault)

	assert.Equal(t, "1", setRes.Answers[1].ChoiceID)
	assert.Empty(t, setRes.Answers[1].OtherText)

	assert.Equal(t, messages.ChoiceIDOther, setRes.Answers[2].ChoiceID)
	assert.Equal(t, "Something else", setRes.Answers[2].ChoiceLabel)
	assert.Equal(t, "Wait until Monday", setRes.Answers[2].OtherText)
	assert.False(t, setRes.Answers[2].MatchedDefault)

	// 3. Idempotency: same key + same (trimmed) text replays; different text -> ErrIdempotencyMismatch.
	replayItems := []store.AnswerSetItem{
		{DecisionRowID: r0, ChoiceID: messages.ChoiceIDOther, OtherText: "europe-west4 canary"},
		{DecisionRowID: r1, ChoiceID: "1"},
		{DecisionRowID: r2, ChoiceID: messages.ChoiceIDOther, OtherText: "  Wait until Monday "},
	}
	replayed, err := f.cs.RecordAnswerSet(ctx, makeSetInput("set-other-idem-1", replayItems))
	require.NoError(t, err)
	assert.True(t, replayed.Replayed)
	assert.Equal(t, setRes.BatchID, replayed.BatchID)
	require.Len(t, replayed.Answers, 3)
	assert.Equal(t, "europe-west4 canary", replayed.Answers[0].OtherText)
	assert.Equal(t, "Wait until Monday", replayed.Answers[2].OtherText)

	diffTextItems := []store.AnswerSetItem{
		{DecisionRowID: r0, ChoiceID: messages.ChoiceIDOther, OtherText: "asia-east1"},
		{DecisionRowID: r1, ChoiceID: "1"},
		{DecisionRowID: r2, ChoiceID: messages.ChoiceIDOther, OtherText: "Wait until Monday"},
	}
	_, err = f.cs.RecordAnswerSet(ctx, makeSetInput("set-other-idem-1", diffTextItems))
	assert.ErrorIs(t, err, store.ErrIdempotencyMismatch)

	// 4. Dismissed set rows do not match replayableAnswerSet even if idempotency_key is reused.
	pDsm := f.prompt("Dismiss set", false,
		messages.Decision{
			Key:      "dsm-s1",
			Question: "First?",
			Choices:  []messages.Choice{{ID: "1", Label: "Yes"}},
			Other:    &messages.OtherOption{Label: "Other"},
		},
		yesNo("dsm-s2", "Second?"),
	)
	dsmRes, err := f.cs.CreateDecisionPrompt(ctx, pDsm)
	require.NoError(t, err)
	dsmDec0, err := f.cs.GetDecision(ctx, dsmRes.Decisions[0].RowID)
	require.NoError(t, err)
	dsmIn := f.dismissInput(pDsm.Message.ID, "user-1", "No longer needed", func() time.Time {
		return dsmDec0.CreatedAt.Add(store.DecisionDismissMinAge)
	})
	dsmIn.IdempotencyKey = "shared-set-dsm-key"
	_, err = f.cs.DismissDecisions(ctx, dsmIn)
	require.NoError(t, err)

	_, err = f.cs.RecordAnswerSet(ctx, &store.RecordAnswerSetInput{
		MessageID:        pDsm.Message.ID,
		AnsweredByUserID: "user-1",
		AnsweredByName:   "Alice",
		AnsweredByEmail:  "alice@example.com",
		Surface:          store.DecisionSurfaceNative,
		Answers: []store.AnswerSetItem{
			{DecisionRowID: dsmRes.Decisions[0].RowID, ChoiceID: messages.ChoiceIDOther, OtherText: "too late"},
			{DecisionRowID: dsmRes.Decisions[1].RowID, ChoiceID: "1"},
		},
		ResponseMessage: &store.Message{
			ID: uuid.NewString(), ProjectID: f.projectID, Sender: "user:alice@example.com",
			SenderID: "user-1", Recipient: "agent:dev", Msg: "late", Type: messages.TypeDecisionResponse,
			AgentID: f.agentID, Channel: "web", ThreadID: "topic:general",
		},
		IdempotencyKey: "shared-set-dsm-key",
	})
	var changedErr *store.AnswerSetChangedError
	require.ErrorAs(t, err, &changedErr)
	assert.Equal(t, store.AnswerSetReasonDismissed, changedErr.Reasons[dsmRes.Decisions[0].RowID])
	assert.Equal(t, store.AnswerSetReasonDismissed, changedErr.Reasons[dsmRes.Decisions[1].RowID])
}

func TestRecordInterpretedAnswers_AllowOther_ReplayConfirmOverrideAndSearch(t *testing.T) {
	f := newDecisionFixture(t)
	ctx := context.Background()

	mkSrc := func() *store.Message {
		m := &store.Message{
			ID:            uuid.NewString(),
			ProjectID:     f.projectID,
			ThreadID:      "topic:general",
			Sender:        "user:user-1",
			SenderID:      "user-1",
			Recipient:     "group:topic",
			RecipientID:   "topic:general",
			Msg:           "use europe-west4",
			DispatchState: store.MessageDispatchDispatched,
			CreatedAt:     time.Now().UTC(),
		}
		require.NoError(t, f.cs.CreateMessage(ctx, m))
		return m
	}
	mkResp := func() *store.Message {
		return &store.Message{
			ID:            uuid.NewString(),
			ProjectID:     f.projectID,
			ThreadID:      "topic:general",
			Sender:        "agent:architect",
			SenderID:      f.agentID,
			Recipient:     "group:topic",
			RecipientID:   "topic:general",
			DispatchState: store.MessageDispatchDispatched,
			CreatedAt:     time.Now().UTC(),
		}
	}

	// Create 4 decisions: 3 with Other enabled, 1 without.
	withOther := func(key, q string) messages.Decision {
		return messages.Decision{
			Key:             key,
			Question:        q,
			DefaultChoiceID: "1",
			Choices:         []messages.Choice{{ID: "1", Label: "us-central1"}},
			Other: &messages.OtherOption{
				Label:       "Another region",
				Placeholder: "e.g. europe-west4",
			},
		}
	}
	rowConfirm := f.create(t, false, withOther("reg-confirm", "Region for confirm?")).Decisions[0].RowID
	rowOverride := f.create(t, false, withOther("reg-override", "Region for override?")).Decisions[0].RowID
	rowFixedToOther := f.create(t, false, withOther("reg-fixed-other", "Region fixed to other?")).Decisions[0].RowID
	f.create(t, false, yesNo("no-other-interp", "No other here?"))

	src := mkSrc()

	// 1. Input validation rejects reserved $dismissed, invalid/empty/too-long other_text, and other_text on fixed.
	for name, pair := range map[string]store.InterpretedAnswerPair{
		"reserved $dismissed": {Key: "reg-confirm", ChoiceID: messages.ChoiceIDDismissed},
		"empty other_text":    {Key: "reg-confirm", ChoiceID: messages.ChoiceIDOther, OtherText: "   "},
		"newline other_text":  {Key: "reg-confirm", ChoiceID: messages.ChoiceIDOther, OtherText: "a\nb"},
		"too long other_text": {Key: "reg-confirm", ChoiceID: messages.ChoiceIDOther, OtherText: strings.Repeat("x", 201)},
		"text on fixed":       {Key: "reg-confirm", ChoiceID: "1", OtherText: "extra"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := f.cs.RecordInterpretedAnswers(ctx, &store.RecordInterpretedInput{
				InterpretedByAgentID: f.agentID,
				SourceMessageID:      src.ID,
				AnsweredByUserID:     "user-1",
				AnsweredByName:       "Alice",
				Surface:              store.DecisionSurfaceSlack,
				Answers:              []store.InterpretedAnswerPair{pair},
				ResponseMessage:      mkResp(),
			})
			require.ErrorIs(t, err, store.ErrInvalidInput)
		})
	}

	// 2. $other on decision without AllowOther returns InterpretedReasonInvalidChoice.
	_, err := f.cs.RecordInterpretedAnswers(ctx, &store.RecordInterpretedInput{
		InterpretedByAgentID: f.agentID,
		SourceMessageID:      src.ID,
		AnsweredByUserID:     "user-1",
		AnsweredByName:       "Alice",
		Surface:              store.DecisionSurfaceSlack,
		Answers:              []store.InterpretedAnswerPair{{Key: "no-other-interp", ChoiceID: messages.ChoiceIDOther, OtherText: "custom"}},
		ResponseMessage:      mkResp(),
	})
	var iaErr *store.InterpretedAnswersError
	require.ErrorAs(t, err, &iaErr)
	require.Len(t, iaErr.Failures, 1)
	assert.Equal(t, store.InterpretedReasonInvalidChoice, iaErr.Failures[0].Reason)

	// 3. Record interpreted Other on reg-confirm and reg-override, and interpreted fixed on reg-fixed-other.
	var formattedItems []store.InterpretedFormattedItem
	recRes, err := f.cs.RecordInterpretedAnswers(ctx, &store.RecordInterpretedInput{
		InterpretedByAgentID: f.agentID,
		SourceMessageID:      src.ID,
		AnsweredByUserID:     "user-1",
		AnsweredByName:       "Alice",
		AnsweredByEmail:      "alice@example.com",
		Surface:              store.DecisionSurfaceSlack,
		IdempotencyKey:       "interp-other-idem-1",
		Answers: []store.InterpretedAnswerPair{
			{Key: "reg-confirm", ChoiceID: messages.ChoiceIDOther, OtherText: "  europe-west4  "},
			{Key: "reg-override", ChoiceID: messages.ChoiceIDOther, OtherText: "asia-northeast1"},
		},
		ResponseMessage: mkResp(),
		FormatMessage: func(items []store.InterpretedFormattedItem) string {
			formattedItems = items
			return "formatted interpreted other"
		},
	})
	require.NoError(t, err)
	assert.False(t, recRes.Replayed)
	require.Len(t, recRes.Answers, 2)
	assert.Equal(t, messages.ChoiceIDOther, recRes.Answers[0].ChoiceID)
	assert.Equal(t, "Another region", recRes.Answers[0].ChoiceLabel)
	assert.Equal(t, "europe-west4", recRes.Answers[0].OtherText)
	assert.False(t, recRes.Answers[0].MatchedDefault)
	require.Len(t, formattedItems, 2)
	assert.Equal(t, "europe-west4", formattedItems[0].OtherText)
	assert.Equal(t, "asia-northeast1", formattedItems[1].OtherText)

	// 4. Idempotency replay compares (choice_id, other_text): same text replays, different text -> ErrIdempotencyMismatch.
	replaySame, err := f.cs.FindInterpretedReplay(ctx, f.agentID, "interp-other-idem-1", src.ID, []store.InterpretedAnswerPair{
		{Key: "reg-confirm", ChoiceID: messages.ChoiceIDOther, OtherText: "europe-west4"},
		{Key: "reg-override", ChoiceID: messages.ChoiceIDOther, OtherText: "  asia-northeast1  "},
	})
	require.NoError(t, err)
	require.NotNil(t, replaySame)
	assert.True(t, replaySame.Replayed)
	require.Len(t, replaySame.Answers, 2)

	_, err = f.cs.FindInterpretedReplay(ctx, f.agentID, "interp-other-idem-1", src.ID, []store.InterpretedAnswerPair{
		{Key: "reg-confirm", ChoiceID: messages.ChoiceIDOther, OtherText: "europe-west4"},
		{Key: "reg-override", ChoiceID: messages.ChoiceIDOther, OtherText: "us-east1"},
	})
	require.ErrorIs(t, err, store.ErrIdempotencyMismatch)

	// 5. ExistingDecisionKey.Hint includes OtherText.
	_, err = f.cs.CreateDecisionPrompt(ctx, f.prompt("", false, withOther("reg-confirm", "Duplicate?")))
	var existsErr *store.DecisionKeyExistsError
	require.ErrorAs(t, err, &existsErr)
	require.Len(t, existsErr.Existing, 1)
	assert.Contains(t, existsErr.Existing[0].Hint(), `Another region "europe-west4"`)

	// 6. Direct confirm vs override on interpreted Other.
	// Confirm with different text fails ErrDecisionOutcomeMismatch.
	badConfirm := f.answer(rowConfirm, messages.ChoiceIDOther, "user-1", "")
	badConfirm.OtherText = "europe-west4-changed"
	badConfirm.Outcome = store.DecisionOutcomeConfirm
	_, _, err = f.cs.RecordAnswer(ctx, badConfirm)
	require.ErrorIs(t, err, store.ErrDecisionOutcomeMismatch)

	// Confirm with same text succeeds.
	okConfirm := f.answer(rowConfirm, messages.ChoiceIDOther, "user-1", "")
	okConfirm.OtherText = "europe-west4"
	okConfirm.Outcome = store.DecisionOutcomeConfirm
	confAns, _, err := f.cs.RecordAnswer(ctx, okConfirm)
	require.NoError(t, err)
	assert.Equal(t, store.DecisionOutcomeConfirm, confAns.Outcome)
	assert.Equal(t, "europe-west4", confAns.OtherText)

	// Override with same text fails ErrDecisionOutcomeMismatch; with different text succeeds.
	badOverride := f.answer(rowOverride, messages.ChoiceIDOther, "user-1", "")
	badOverride.OtherText = "asia-northeast1"
	badOverride.Outcome = store.DecisionOutcomeOverride
	_, _, err = f.cs.RecordAnswer(ctx, badOverride)
	require.ErrorIs(t, err, store.ErrDecisionOutcomeMismatch)

	okOverride := f.answer(rowOverride, messages.ChoiceIDOther, "user-1", "")
	okOverride.OtherText = "me-central2"
	okOverride.Outcome = store.DecisionOutcomeOverride
	overAns, _, err := f.cs.RecordAnswer(ctx, okOverride)
	require.NoError(t, err)
	assert.Equal(t, store.DecisionOutcomeOverride, overAns.Outcome)
	assert.Equal(t, "me-central2", overAns.OtherText)

	// Interpreted fixed choice overridden by $other succeeds.
	_, err = f.cs.RecordInterpretedAnswers(ctx, &store.RecordInterpretedInput{
		InterpretedByAgentID: f.agentID,
		SourceMessageID:      src.ID,
		AnsweredByUserID:     "user-1",
		AnsweredByName:       "Alice",
		Surface:              store.DecisionSurfaceSlack,
		Answers:              []store.InterpretedAnswerPair{{Key: "reg-fixed-other", ChoiceID: "1"}},
		ResponseMessage:      mkResp(),
	})
	require.NoError(t, err)

	fixedToOther := f.answer(rowFixedToOther, messages.ChoiceIDOther, "user-1", "")
	fixedToOther.OtherText = "southamerica-east1"
	fixedToOther.Outcome = store.DecisionOutcomeOverride
	ftoAns, _, err := f.cs.RecordAnswer(ctx, fixedToOther)
	require.NoError(t, err)
	assert.Equal(t, store.DecisionOutcomeOverride, ftoAns.Outcome)
	assert.Equal(t, "southamerica-east1", ftoAns.OtherText)

	// 7. ListAgentDecisions search (-q) matches effective answer's other_text, not superseded interpreted text.
	foundNew, _, err := f.cs.ListAgentDecisions(ctx, f.agentID, store.DecisionListStateDecided, "ME-CENTRAL2", "", 50)
	require.NoError(t, err)
	require.Len(t, foundNew, 1)
	assert.Equal(t, "reg-override", foundNew[0].Key)

	notFoundOld, _, err := f.cs.ListAgentDecisions(ctx, f.agentID, store.DecisionListStateDecided, "asia-northeast1", "", 50)
	require.NoError(t, err)
	assert.Empty(t, notFoundOld, "superseded interpreted other_text must not match effective-answer search")
}

func TestRecordAnswerSet_InterpretedOther_ConfirmAndOverride(t *testing.T) {
	f := newDecisionFixture(t)
	ctx := context.Background()

	pIn := f.prompt("Two Other decisions", false,
		messages.Decision{
			Key:      "reg-set",
			Question: "Which region?",
			Choices:  []messages.Choice{{ID: "1", Label: "us-central1"}},
			Other:    &messages.OtherOption{Label: "Another region"},
		},
		messages.Decision{
			Key:      "win-set",
			Question: "When?",
			Choices:  []messages.Choice{{ID: "1", Label: "Now"}},
			Other:    &messages.OtherOption{Label: "Other"},
		},
	)
	created, err := f.cs.CreateDecisionPrompt(ctx, pIn)
	require.NoError(t, err)
	require.Len(t, created.Decisions, 2)
	r0, r1 := created.Decisions[0].RowID, created.Decisions[1].RowID

	src := &store.Message{
		ID:            uuid.NewString(),
		ProjectID:     f.projectID,
		Sender:        "user:user-1",
		SenderID:      "user-1",
		Recipient:     "group:topic",
		RecipientID:   "topic:general",
		Msg:           "europe-west4, after 6 PM",
		ThreadID:      "topic:general",
		CreatedAt:     time.Now().UTC().Add(time.Second),
		DispatchState: store.MessageDispatchDispatched,
	}
	require.NoError(t, f.cs.CreateMessage(ctx, src))

	_, err = f.cs.RecordInterpretedAnswers(ctx, &store.RecordInterpretedInput{
		InterpretedByAgentID: f.agentID,
		SourceMessageID:      src.ID,
		AnsweredByUserID:     "user-1",
		AnsweredByName:       "Alice",
		AnsweredByEmail:      "alice@test.com",
		Surface:              store.DecisionSurfaceNative,
		Answers: []store.InterpretedAnswerPair{
			{DecisionRowID: r0, Key: "reg-set", ChoiceID: messages.ChoiceIDOther, OtherText: "europe-west4"},
			{DecisionRowID: r1, Key: "win-set", ChoiceID: messages.ChoiceIDOther, OtherText: "after 6 PM"},
		},
		ResponseMessage: &store.Message{
			ID: uuid.NewString(), ProjectID: f.projectID, Sender: "agent:dev", SenderID: f.agentID,
			Recipient: "group:topic", RecipientID: "topic:general", Msg: "interpreted", Type: messages.TypeDecisionResponse,
			AgentID: f.agentID, Channel: "web", ThreadID: "topic:general", DispatchState: store.MessageDispatchDispatched,
		},
		FormatMessage: func(items []store.InterpretedFormattedItem) string { return "interpreted" },
	})
	require.NoError(t, err)

	// Mismatched Outcome vs Other text (Confirm with changed text, Override with same text) is rejected.
	_, err = f.cs.RecordAnswerSet(ctx, &store.RecordAnswerSetInput{
		MessageID:        pIn.Message.ID,
		AnsweredByUserID: "user-1",
		AnsweredByName:   "Alice",
		AnsweredByEmail:  "alice@test.com",
		Surface:          store.DecisionSurfaceNative,
		Answers: []store.AnswerSetItem{
			{DecisionRowID: r0, ChoiceID: messages.ChoiceIDOther, OtherText: "europe-west4", Outcome: store.DecisionOutcomeOverride},
			{DecisionRowID: r1, ChoiceID: messages.ChoiceIDOther, OtherText: "after 8 PM", Outcome: store.DecisionOutcomeConfirm},
		},
		ResponseMessage: &store.Message{
			ID: uuid.NewString(), ProjectID: f.projectID, Sender: "user:alice@test.com", SenderID: "user-1",
			Recipient: "agent:dev", RecipientID: f.agentID, Msg: "answered", Type: messages.TypeDecisionResponse,
			AgentID: f.agentID, Channel: "web", ThreadID: "topic:general",
		},
	})
	var changedErr *store.AnswerSetChangedError
	require.ErrorAs(t, err, &changedErr)
	assert.Equal(t, store.AnswerSetReasonDecided, changedErr.Reasons[r0])
	assert.Equal(t, store.AnswerSetReasonDecided, changedErr.Reasons[r1])

	// Confirm r0 with identical Other text and override r1 with different Other text.
	setRes, err := f.cs.RecordAnswerSet(ctx, &store.RecordAnswerSetInput{
		MessageID:        pIn.Message.ID,
		AnsweredByUserID: "user-1",
		AnsweredByName:   "Alice",
		AnsweredByEmail:  "alice@test.com",
		Surface:          store.DecisionSurfaceNative,
		Answers: []store.AnswerSetItem{
			{DecisionRowID: r0, ChoiceID: messages.ChoiceIDOther, OtherText: "europe-west4", Outcome: store.DecisionOutcomeConfirm},
			{DecisionRowID: r1, ChoiceID: messages.ChoiceIDOther, OtherText: "after 8 PM", Outcome: store.DecisionOutcomeOverride},
		},
		ResponseMessage: &store.Message{
			ID: uuid.NewString(), ProjectID: f.projectID, Sender: "user:alice@test.com", SenderID: "user-1",
			Recipient: "agent:dev", RecipientID: f.agentID, Msg: "answered", Type: messages.TypeDecisionResponse,
			AgentID: f.agentID, Channel: "web", ThreadID: "topic:general",
		},
	})
	require.NoError(t, err)
	require.Len(t, setRes.Answers, 2)
	assert.Equal(t, store.DecisionOutcomeConfirm, setRes.Answers[0].Outcome)
	assert.Equal(t, "europe-west4", setRes.Answers[0].OtherText)
	assert.Equal(t, store.DecisionOutcomeOverride, setRes.Answers[1].Outcome)
	assert.Equal(t, "after 8 PM", setRes.Answers[1].OtherText)

	d0, err := f.cs.GetDecision(ctx, r0)
	require.NoError(t, err)
	require.Len(t, d0.Answers, 2)
	assert.Equal(t, store.DecisionOutcomeConfirm, d0.EffectiveAnswer().Outcome)

	d1, err := f.cs.GetDecision(ctx, r1)
	require.NoError(t, err)
	require.Len(t, d1.Answers, 2)
	assert.Equal(t, store.DecisionOutcomeOverride, d1.EffectiveAnswer().Outcome)
	assert.Equal(t, "after 6 PM", d1.Answers[0].OtherText)
	assert.Equal(t, "after 8 PM", d1.Answers[1].OtherText)
}
