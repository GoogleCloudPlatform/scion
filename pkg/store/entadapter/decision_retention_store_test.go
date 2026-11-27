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
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/decision"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/decisionanswer"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/decisionfacetentry"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TestDecisionRetentionStore_EligibilityMatrix proves every eligibility rule
// of design-8984 §3.2 on both SQLite and Postgres:
//   - pending (old): kept
//   - interpreted-awaiting (old): kept
//   - direct decided (old): purged with answer; chat message & key counter kept
//   - interpreted + direct confirm (old): purged with both answers
//   - interpreted + direct override (old): purged with both answers
//   - dismissed direct & dismissed over interpreted (old): purged with answers
//   - withdrawn (old): purged
//   - replaced chain (old v1 replaced by recent v2): v1 purged, v2 kept
//   - multi-decision message with one old settled + one open sibling: whole message kept
//   - old decision with recent answer / recent terminal timestamp: kept
//   - NULL chat_key (old settled): purged cleanly
//   - deleted-user answer (old settled): purged cleanly
//   - project omitted from cutoffByProject: kept
func TestDecisionRetentionStore_EligibilityMatrix(t *testing.T) {
	ctx := context.Background()
	cs := newTestCompositeStore(t)
	st := cs.DecisionRetention()

	now := time.Now().UTC().Truncate(time.Microsecond)
	cutoff := now.Add(-30 * 24 * time.Hour)
	oldTime := now.Add(-60 * 24 * time.Hour)
	recentTime := now.Add(-5 * 24 * time.Hour)

	proj := uuid.NewString()
	projHold := uuid.NewString()
	for i, pid := range []string{proj, projHold} {
		require.NoError(t, cs.CreateProject(ctx, &store.Project{
			ID: pid, Name: fmt.Sprintf("ret-p-%d", i), Slug: fmt.Sprintf("ret-p-%d", i), Created: oldTime,
		}))
	}
	user1 := uuid.NewString()
	userDel := uuid.NewString()
	for i, uid := range []string{user1, userDel} {
		require.NoError(t, cs.CreateUser(ctx, &store.User{
			ID: uid, Email: fmt.Sprintf("ret-u%d@test.com", i), DisplayName: fmt.Sprintf("User %d", i),
			Role: store.UserRoleMember, Status: "active", Created: oldTime,
		}))
	}
	agentID := uuid.NewString()
	agentHoldID := uuid.NewString()
	topic := "topic-" + uuid.NewString()
	choices := []messages.Choice{{ID: "1", Label: "Yes"}, {ID: "2", Label: "No"}}

	postPrompt := func(pid, aid, threadID string, update bool, keys ...string) (string, []string) {
		msgID := uuid.NewString()
		decs := make([]messages.Decision, len(keys))
		for i, k := range keys {
			decs[i] = messages.Decision{Key: k, Question: "Q " + k, Choices: choices}
		}
		res, err := cs.CreateDecisionPrompt(ctx, &store.DecisionPromptInput{
			Message: &store.Message{
				ID: msgID, ProjectID: pid, ThreadID: threadID,
				Sender: "agent:bot", SenderID: aid, Recipient: "user:" + user1, RecipientID: user1,
			},
			AskerAgentID:   aid,
			AskerAgentName: "bot",
			Decisions:      decs,
			Update:         update,
		})
		require.NoError(t, err)
		ids := make([]string, len(res.Decisions))
		for i, d := range res.Decisions {
			ids[i] = d.RowID
		}
		return msgID, ids
	}

	backdateDecision := func(id string, created time.Time, decided, replaced, withdrawn, dismissed *time.Time) {
		uid := uuid.MustParse(id)
		// created_at is immutable in Ent; update via SQL so both dialects accept it.
		db := clientDB(cs.client)
		q := "UPDATE decisions SET created_at = ? WHERE id = ?"
		if cs.isPostgres() {
			q = "UPDATE decisions SET created_at = $1 WHERE id = $2"
		}
		_, err := db.ExecContext(ctx, q, created.UTC(), uid)
		require.NoError(t, err)
		upd := cs.client.Decision.UpdateOneID(uid)
		changed := false
		if decided != nil {
			upd.SetDecidedAt(decided.UTC())
			changed = true
		}
		if replaced != nil {
			upd.SetReplacedAt(replaced.UTC())
			changed = true
		}
		if withdrawn != nil {
			upd.SetWithdrawnAt(withdrawn.UTC())
			changed = true
		}
		if dismissed != nil {
			upd.SetDismissedAt(dismissed.UTC())
			changed = true
		}
		if changed {
			require.NoError(t, upd.Exec(ctx))
		}
	}

	backdateAnswers := func(decRowID string, ts time.Time) {
		uid := uuid.MustParse(decRowID)
		db := clientDB(cs.client)
		q := "UPDATE decision_answers SET created_at = ? WHERE decision_row_id = ?"
		if cs.isPostgres() {
			q = "UPDATE decision_answers SET created_at = $1 WHERE decision_row_id = $2"
		}
		_, err := db.ExecContext(ctx, q, ts.UTC(), uid)
		require.NoError(t, err)
	}

	recordDirect := func(pid, decRowID, uid, choiceID, outcome string) {
		_, _, err := cs.RecordAnswer(ctx, &store.RecordAnswerInput{
			DecisionRowID:    decRowID,
			ChoiceID:         choiceID,
			AnsweredByUserID: uid,
			AnsweredByName:   "User 1",
			AnsweredByEmail:  "ret-u0@test.com",
			Provenance:       store.DecisionProvenanceDirect,
			Outcome:          outcome,
			Surface:          store.DecisionSurfaceNative,
			ResponseMessage: &store.Message{
				ID: uuid.NewString(), ProjectID: pid, ThreadID: topic,
				Sender: "user:" + uid, SenderID: uid, Recipient: "agent:bot", RecipientID: agentID,
			},
		})
		require.NoError(t, err)
	}

	recordInterp := func(pid, aid, decRowID, key, uid, choiceID string) {
		_, err := cs.RecordInterpretedAnswers(ctx, &store.RecordInterpretedInput{
			InterpretedByAgentID: aid,
			SourceMessageID:      uuid.NewString(),
			AnsweredByUserID:     uid,
			AnsweredByName:       "User 1",
			AnsweredByEmail:      "ret-u0@test.com",
			Surface:              store.DecisionSurfaceNative,
			Answers:              []store.InterpretedAnswerPair{{DecisionRowID: decRowID, Key: key, ChoiceID: choiceID}},
			ResponseMessage: &store.Message{
				ID: uuid.NewString(), ProjectID: pid, ThreadID: topic,
				Sender: "agent:bot", SenderID: aid, Recipient: "user:" + uid, RecipientID: uid,
			},
		})
		require.NoError(t, err)
	}

	// 1. Old pending -> MUST STAY.
	msgPending, decPending := postPrompt(proj, agentID, topic, false, "K-PEND")
	backdateDecision(decPending[0], oldTime, nil, nil, nil, nil)

	// 2. Old interpreted-awaiting -> MUST STAY.
	msgInterpWait, decInterpWait := postPrompt(proj, agentID, topic, false, "K-IWAIT")
	recordInterp(proj, agentID, decInterpWait[0], "K-IWAIT", user1, "1")
	backdateDecision(decInterpWait[0], oldTime, &oldTime, nil, nil, nil)
	backdateAnswers(decInterpWait[0], oldTime)

	// 3. Old direct decided (hub-assigned key to check decision_key_counters kept) -> MUST PURGE.
	msgDirect, decDirect := postPrompt(proj, agentID, topic, false, "")
	recordDirect(proj, decDirect[0], user1, "1", "")
	backdateDecision(decDirect[0], oldTime, &oldTime, nil, nil, nil)
	backdateAnswers(decDirect[0], oldTime)

	// 4. Old interpreted + direct confirm -> MUST PURGE.
	msgConfirm, decConfirm := postPrompt(proj, agentID, topic, false, "K-CONF")
	recordInterp(proj, agentID, decConfirm[0], "K-CONF", user1, "1")
	recordDirect(proj, decConfirm[0], user1, "1", store.DecisionOutcomeConfirm)
	backdateDecision(decConfirm[0], oldTime, &oldTime, nil, nil, nil)
	backdateAnswers(decConfirm[0], oldTime)

	// 5. Old interpreted + direct override -> MUST PURGE.
	msgOverride, decOverride := postPrompt(proj, agentID, topic, false, "K-OVER")
	recordInterp(proj, agentID, decOverride[0], "K-OVER", user1, "1")
	recordDirect(proj, decOverride[0], user1, "2", store.DecisionOutcomeOverride)
	backdateDecision(decOverride[0], oldTime, &oldTime, nil, nil, nil)
	backdateAnswers(decOverride[0], oldTime)

	// 6. Old dismissed (direct) -> MUST PURGE.
	msgDismiss, decDismiss := postPrompt(proj, agentID, topic, false, "K-DISM")
	backdateDecision(decDismiss[0], oldTime, nil, nil, nil, nil)
	_, err := cs.DismissDecisions(ctx, &store.DismissDecisionsInput{
		MessageID: msgDismiss,
		User:      store.DecisionAnswerer{UserID: user1, Name: "User 1", Email: "ret-u0@test.com"},
		Surface:   store.DecisionSurfaceNative,
		ResponseMessage: &store.Message{
			ID: uuid.NewString(), ProjectID: proj, ThreadID: topic,
			Sender: "user:" + user1, SenderID: user1, Recipient: "agent:bot", RecipientID: agentID,
		},
	})
	require.NoError(t, err)
	backdateDecision(decDismiss[0], oldTime, nil, nil, nil, &oldTime)
	backdateAnswers(decDismiss[0], oldTime)

	// 7. Old dismissed over interpreted -> MUST PURGE.
	msgDismissInterp, decDismissInterp := postPrompt(proj, agentID, topic, false, "K-DISMI")
	recordInterp(proj, agentID, decDismissInterp[0], "K-DISMI", user1, "1")
	backdateDecision(decDismissInterp[0], oldTime, &oldTime, nil, nil, nil)
	_, err = cs.DismissDecisions(ctx, &store.DismissDecisionsInput{
		MessageID: msgDismissInterp,
		User:      store.DecisionAnswerer{UserID: user1, Name: "User 1", Email: "ret-u0@test.com"},
		Surface:   store.DecisionSurfaceNative,
		ResponseMessage: &store.Message{
			ID: uuid.NewString(), ProjectID: proj, ThreadID: topic,
			Sender: "user:" + user1, SenderID: user1, Recipient: "agent:bot", RecipientID: agentID,
		},
	})
	require.NoError(t, err)
	backdateDecision(decDismissInterp[0], oldTime, &oldTime, nil, nil, &oldTime)
	backdateAnswers(decDismissInterp[0], oldTime)

	// 8. Old withdrawn -> MUST PURGE.
	msgWithdraw, decWithdraw := postPrompt(proj, agentID, topic, false, "K-WITH")
	_, err = cs.WithdrawDecisions(ctx, &store.WithdrawDecisionsInput{
		AskerAgentID: agentID, Keys: []string{"K-WITH"}, Reason: "obsolete",
	})
	require.NoError(t, err)
	backdateDecision(decWithdraw[0], oldTime, nil, nil, &oldTime, nil)

	// 9. Replaced chain: old v1 (replaced) + recent v2 (pending) -> v1 PURGED, v2 KEPT.
	msgRepV1, decRepV1 := postPrompt(proj, agentID, topic, false, "K-REP")
	msgRepV2, decRepV2 := postPrompt(proj, agentID, topic, true, "K-REP")
	backdateDecision(decRepV1[0], oldTime, nil, &oldTime, nil, nil)
	backdateDecision(decRepV2[0], recentTime, nil, nil, nil, nil)

	// 10. Multi-decision message: two old settled rows bracketing an old pending
	// sibling (K-MIX1 settled, K-MIX2 pending, K-MIX3 settled) -> WHOLE MESSAGE
	// KEPT deterministically regardless of first/last lock position.
	msgMixed, decMixed := postPrompt(proj, agentID, topic, false, "K-MIX1", "K-MIX2", "K-MIX3")
	recordDirect(proj, decMixed[0], user1, "1", "")
	backdateDecision(decMixed[0], oldTime, &oldTime, nil, nil, nil)
	backdateAnswers(decMixed[0], oldTime)
	backdateDecision(decMixed[1], oldTime, nil, nil, nil, nil)
	recordDirect(proj, decMixed[2], user1, "1", "")
	backdateDecision(decMixed[2], oldTime, &oldTime, nil, nil, nil)
	backdateAnswers(decMixed[2], oldTime)

	// 11. Old decision with recent answer -> KEPT.
	msgRecentAns, decRecentAns := postPrompt(proj, agentID, topic, false, "K-RANS")
	recordDirect(proj, decRecentAns[0], user1, "1", "")
	backdateDecision(decRecentAns[0], oldTime, &oldTime, nil, nil, nil)
	backdateAnswers(decRecentAns[0], recentTime)

	// 12. NULL chat_key (old settled) -> PURGED cleanly.
	msgNullChat, decNullChat := postPrompt(proj, agentID, "", false, "K-NULLCK")
	recordDirect(proj, decNullChat[0], user1, "1", "")
	backdateDecision(decNullChat[0], oldTime, &oldTime, nil, nil, nil)
	backdateAnswers(decNullChat[0], oldTime)

	// 13. Deleted-user answer (old settled) -> PURGED cleanly.
	msgDelUser, decDelUser := postPrompt(proj, agentID, topic, false, "K-DELU")
	recordDirect(proj, decDelUser[0], userDel, "1", "")
	backdateDecision(decDelUser[0], oldTime, &oldTime, nil, nil, nil)
	backdateAnswers(decDelUser[0], oldTime)
	require.NoError(t, clearDecisionAnswerUser(ctx, cs.client, userDel))

	// 14. Project omitted from cutoffByProject (hold) -> KEPT.
	msgHold, decHold := postPrompt(projHold, agentHoldID, topic, false, "K-HOLD")
	recordDirect(projHold, decHold[0], user1, "1", "")
	backdateDecision(decHold[0], oldTime, &oldTime, nil, nil, nil)
	backdateAnswers(decHold[0], oldTime)

	// Rebuild facet entries from the backdated timestamps so facet_entries.latest_at
	// reflects the backdated rows before purge.
	_, err = cs.client.DecisionFacetEntry.Delete().Exec(ctx)
	require.NoError(t, err)
	require.NoError(t, cs.client.Decision.Update().SetFacetIndexed(false).Exec(ctx))
	require.NoError(t, cs.client.DecisionAnswer.Update().SetFacetIndexed(false).Exec(ctx))
	drainDecisionIndex(t, ctx, cs, 100)

	allMsgs := []string{
		msgPending, msgInterpWait, msgDirect, msgConfirm, msgOverride,
		msgDismiss, msgDismissInterp, msgWithdraw, msgRepV1, msgRepV2,
		msgMixed, msgRecentAns, msgNullChat, msgDelUser, msgHold,
	}
	res, err := st.PurgeDecisionMessages(ctx, allMsgs, map[string]time.Time{proj: cutoff})
	require.NoError(t, err)

	// Expected purged: msgDirect, msgConfirm, msgOverride, msgDismiss, msgDismissInterp,
	// msgWithdraw, msgRepV1, msgNullChat, msgDelUser = 9 messages, 9 decisions.
	assert.Equal(t, 9, res.PurgedMessages)
	assert.Equal(t, 6, res.SkippedMessages)
	assert.Equal(t, int64(9), res.ByProject[proj].Decisions)
	// Answers: direct(1) + confirm(2) + override(2) + dismiss(1) + dismissInterp(2) +
	// withdraw(0) + repV1(0) + nullChat(1) + delUser(1) = 10 answers.
	assert.Equal(t, int64(10), res.ByProject[proj].Answers)
	assert.Zero(t, res.ByProject[projHold].Decisions)

	// Verify purged decisions are gone, while chat messages & key counters remain!
	purgedDecs := []string{
		decDirect[0], decConfirm[0], decOverride[0], decDismiss[0],
		decDismissInterp[0], decWithdraw[0], decRepV1[0], decNullChat[0], decDelUser[0],
	}
	for _, id := range purgedDecs {
		_, err := cs.GetDecision(ctx, id)
		assert.ErrorIs(t, err, store.ErrNotFound, "purged decision %s should be gone", id)
	}
	// Chat message for msgDirect must still exist!
	_, err = cs.GetMessage(ctx, msgDirect)
	assert.NoError(t, err, "prompt chat message must be kept")
	// decision_key_counters for agentID must still exist!
	counterExists, err := cs.client.DecisionKeyCounter.Query().Exist(ctx)
	require.NoError(t, err)
	assert.True(t, counterExists, "decision_key_counters must be kept")

	// Verify kept decisions still exist with their answers intact.
	keptDecs := []string{
		decPending[0], decInterpWait[0], decRepV2[0],
		decMixed[0], decMixed[1], decMixed[2], decRecentAns[0], decHold[0],
	}
	for _, id := range keptDecs {
		_, err := cs.GetDecision(ctx, id)
		assert.NoError(t, err, "ineligible decision %s must be kept", id)
	}

	// Facet summary must equal a full recompute after purge.
	assertFacetSummaryMatchesRecompute(t, ctx, cs.client)
}

// TestDecisionRetentionStore_DifferentialFacets verifies design-8984 §3.4 & §5:
// after purging a subset of decisions (including cases where an asker hits 0,
// an answerer hits 0, and a surviving entry's newest contributor is purged so
// name/email/latest_at must be recomputed), the facet summary matches a full
// recompute AND summary-backed ListDecisionFacets / ProjectsWithDecisions /
// ListProjectDecisionAgentsForProjects equal the legacy queries.
func TestDecisionRetentionStore_DifferentialFacets(t *testing.T) {
	ctx := context.Background()
	cs := newTestCompositeStore(t)
	st := cs.DecisionRetention()

	now := time.Now().UTC().Truncate(time.Microsecond)
	cutoff := now.Add(-30 * 24 * time.Hour)
	t1 := now.Add(-90 * 24 * time.Hour)
	t2 := now.Add(-60 * 24 * time.Hour)
	t3 := now.Add(-10 * 24 * time.Hour)

	proj1 := uuid.NewString()
	proj2 := uuid.NewString()
	for i, pid := range []string{proj1, proj2} {
		require.NoError(t, cs.CreateProject(ctx, &store.Project{
			ID: pid, Name: fmt.Sprintf("diff-p-%d", i), Slug: fmt.Sprintf("diff-p-%d", i), Created: t1,
		}))
	}
	user1 := uuid.NewString()
	user2 := uuid.NewString()
	for i, uid := range []string{user1, user2} {
		require.NoError(t, cs.CreateUser(ctx, &store.User{
			ID: uid, Email: fmt.Sprintf("du%d@test.com", i), DisplayName: fmt.Sprintf("User %d", i),
			Role: store.UserRoleMember, Status: "active", Created: t1,
		}))
	}

	agentGone := uuid.NewString() // all its decisions purged -> asker row deleted at 0
	agentKeep := uuid.NewString() // newest decision purged (old decided), older kept (pending) -> name/latest_at recomputed!
	topic1 := "topic-1-" + uuid.NewString()
	topic2 := "topic-2-" + uuid.NewString()
	choices := []messages.Choice{{ID: "1", Label: "Yes"}, {ID: "2", Label: "No"}}

	backdate := func(decID string, created time.Time, decided *time.Time, ansTime *time.Time) {
		uid := uuid.MustParse(decID)
		db := clientDB(cs.client)
		qDec := "UPDATE decisions SET created_at = ? WHERE id = ?"
		qAns := "UPDATE decision_answers SET created_at = ? WHERE decision_row_id = ?"
		if cs.isPostgres() {
			qDec = "UPDATE decisions SET created_at = $1 WHERE id = $2"
			qAns = "UPDATE decision_answers SET created_at = $1 WHERE decision_row_id = $2"
		}
		_, err := db.ExecContext(ctx, qDec, created.UTC(), uid)
		require.NoError(t, err)
		if decided != nil {
			require.NoError(t, cs.client.Decision.UpdateOneID(uid).SetDecidedAt(decided.UTC()).Exec(ctx))
		}
		if ansTime != nil {
			_, err = db.ExecContext(ctx, qAns, ansTime.UTC(), uid)
			require.NoError(t, err)
		}
	}

	// Project 1, agentGone: single old settled decision answered by user2 -> both asker & answerer hit 0 and are deleted!
	msgGone := uuid.NewString()
	rGone, err := cs.CreateDecisionPrompt(ctx, &store.DecisionPromptInput{
		Message:        &store.Message{ID: msgGone, ProjectID: proj1, ThreadID: topic1, Sender: "agent:gone", SenderID: agentGone, Recipient: "user:" + user2, RecipientID: user2},
		AskerAgentID:   agentGone,
		AskerAgentName: "Agent Gone",
		Decisions:      []messages.Decision{{Key: "G1", Question: "Q", Choices: choices}},
	})
	require.NoError(t, err)
	_, _, err = cs.RecordAnswer(ctx, &store.RecordAnswerInput{
		DecisionRowID: rGone.Decisions[0].RowID, ChoiceID: "1",
		AnsweredByUserID: user2, AnsweredByName: "User Two", AnsweredByEmail: "u2@test.com",
		Provenance: store.DecisionProvenanceDirect, Surface: store.DecisionSurfaceNative,
		ResponseMessage: &store.Message{ID: uuid.NewString(), ProjectID: proj1, ThreadID: topic1, Sender: "user:" + user2, SenderID: user2, Recipient: "agent:gone", RecipientID: agentGone},
	})
	require.NoError(t, err)
	backdate(rGone.Decisions[0].RowID, t1, &t1, &t1)

	// Project 1, agentKeep:
	// - Decision A at t1 (pending -> KEPT), name "Agent Keep Old"
	// - Decision B at t2 (decided at t2 -> PURGED, newer than t1!), name "Agent Keep New"
	//   answered by user1 at t2 with name "User1 New" / "u1-new@test.com"
	// - Decision C at t1 (decided with recent answer at t3 -> KEPT)
	//   Wait: to test answerer recompute where the NEWEST answer is purged, we make:
	//   Decision C1 (created t1, decided t1, answer by user1 at t1, name "User1 Old", pending sibling -> KEPT!)
	//   Decision B (created t2, decided t2, answer by user1 at t2, name "User1 New" -> PURGED!)
	msgKeepOld := uuid.NewString()
	rKeepOld, err := cs.CreateDecisionPrompt(ctx, &store.DecisionPromptInput{
		Message:        &store.Message{ID: msgKeepOld, ProjectID: proj1, ThreadID: topic1, Sender: "agent:keep", SenderID: agentKeep, Recipient: "user:" + user1, RecipientID: user1},
		AskerAgentID:   agentKeep,
		AskerAgentName: "Agent Keep Old",
		Decisions: []messages.Decision{
			{Key: "KO1", Question: "Q1", Choices: choices},
			{Key: "KO2", Question: "Q2", Choices: choices},
		},
	})
	require.NoError(t, err)
	_, _, err = cs.RecordAnswer(ctx, &store.RecordAnswerInput{
		DecisionRowID: rKeepOld.Decisions[0].RowID, ChoiceID: "1",
		AnsweredByUserID: user1, AnsweredByName: "User1 Old", AnsweredByEmail: "u1-old@test.com",
		Provenance: store.DecisionProvenanceDirect, Surface: store.DecisionSurfaceNative,
		ResponseMessage: &store.Message{ID: uuid.NewString(), ProjectID: proj1, ThreadID: topic1, Sender: "user:" + user1, SenderID: user1, Recipient: "agent:keep", RecipientID: agentKeep},
	})
	require.NoError(t, err)
	backdate(rKeepOld.Decisions[0].RowID, t1, &t1, &t1)
	backdate(rKeepOld.Decisions[1].RowID, t1, nil, nil)

	msgKeepNewPurged := uuid.NewString()
	rKeepNew, err := cs.CreateDecisionPrompt(ctx, &store.DecisionPromptInput{
		Message:        &store.Message{ID: msgKeepNewPurged, ProjectID: proj1, ThreadID: topic1, Sender: "agent:keep", SenderID: agentKeep, Recipient: "user:" + user1, RecipientID: user1},
		AskerAgentID:   agentKeep,
		AskerAgentName: "Agent Keep New",
		Decisions:      []messages.Decision{{Key: "KN1", Question: "QN", Choices: choices}},
	})
	require.NoError(t, err)
	_, _, err = cs.RecordAnswer(ctx, &store.RecordAnswerInput{
		DecisionRowID: rKeepNew.Decisions[0].RowID, ChoiceID: "2",
		AnsweredByUserID: user1, AnsweredByName: "User1 New", AnsweredByEmail: "u1-new@test.com",
		Provenance: store.DecisionProvenanceDirect, Surface: store.DecisionSurfaceNative,
		ResponseMessage: &store.Message{ID: uuid.NewString(), ProjectID: proj1, ThreadID: topic1, Sender: "user:" + user1, SenderID: user1, Recipient: "agent:keep", RecipientID: agentKeep},
	})
	require.NoError(t, err)
	backdate(rKeepNew.Decisions[0].RowID, t2, &t2, &t2)

	// Project 2: one old settled decision (purged -> project 2 disappears from ProjectsWithDecisions!).
	msgProj2 := uuid.NewString()
	rProj2, err := cs.CreateDecisionPrompt(ctx, &store.DecisionPromptInput{
		Message:        &store.Message{ID: msgProj2, ProjectID: proj2, ThreadID: topic2, Sender: "agent:p2", SenderID: uuid.NewString(), Recipient: "user:" + user1, RecipientID: user1},
		AskerAgentID:   uuid.NewString(),
		AskerAgentName: "Agent P2",
		Decisions:      []messages.Decision{{Key: "P2", Question: "QP2", Choices: choices}},
	})
	require.NoError(t, err)
	_, _, err = cs.RecordAnswer(ctx, &store.RecordAnswerInput{
		DecisionRowID: rProj2.Decisions[0].RowID, ChoiceID: "1",
		AnsweredByUserID: user1, AnsweredByName: "User1 P2", AnsweredByEmail: "u1-p2@test.com",
		Provenance: store.DecisionProvenanceDirect, Surface: store.DecisionSurfaceNative,
		ResponseMessage: &store.Message{ID: uuid.NewString(), ProjectID: proj2, ThreadID: topic2, Sender: "user:" + user1, SenderID: user1, Recipient: "agent:p2", RecipientID: agentKeep},
	})
	require.NoError(t, err)
	backdate(rProj2.Decisions[0].RowID, t1, &t1, &t1)

	// Drain indexer so all rows are facet_indexed=true.
	drainDecisionIndex(t, ctx, cs, 100)
	// Backdate does not affect facet_entries latest_at if created_at was changed after insert;
	// rebuild facet entries cleanly from current backdated timestamps via delete + reindex.
	_, err = cs.client.DecisionFacetEntry.Delete().Exec(ctx)
	require.NoError(t, err)
	require.NoError(t, cs.client.Decision.Update().SetFacetIndexed(false).Exec(ctx))
	require.NoError(t, cs.client.DecisionAnswer.Update().SetFacetIndexed(false).Exec(ctx))
	drainDecisionIndex(t, ctx, cs, 100)
	assertFacetSummaryMatchesRecompute(t, ctx, cs.client)

	// Purge eligible messages.
	res, err := st.PurgeDecisionMessages(ctx, []string{msgGone, msgKeepOld, msgKeepNewPurged, msgProj2}, map[string]time.Time{
		proj1: cutoff,
		proj2: cutoff,
	})
	require.NoError(t, err)
	assert.Equal(t, 3, res.PurgedMessages)
	assert.Equal(t, 1, res.SkippedMessages)

	// 1. Exact summary table equality against full recompute.
	assertFacetSummaryMatchesRecompute(t, ctx, cs.client)

	// 2. Asker row for agentGone and project 2 must be DELETED (not left at ref_count=0).
	goneCount, err := cs.client.DecisionFacetEntry.Query().
		Where(decisionfacetentry.ActorIDEQ(agentGone)).
		Count(ctx)
	require.NoError(t, err)
	assert.Zero(t, goneCount, "asker facet row must be deleted when ref_count reaches 0")

	p2Count, err := cs.client.DecisionFacetEntry.Query().
		Where(decisionfacetentry.ProjectIDEQ(uuid.MustParse(proj2))).
		Count(ctx)
	require.NoError(t, err)
	assert.Zero(t, p2Count, "all facet rows for purged project must be deleted")

	// 3. Surviving agentKeep asker and user1 answerer must have recomputed name/email/latest_at = t1 ("Old").
	askerEntry, err := cs.client.DecisionFacetEntry.Query().
		Where(
			decisionfacetentry.ProjectIDEQ(uuid.MustParse(proj1)),
			decisionfacetentry.ChatKeyEQ(topic1),
			decisionfacetentry.KindEQ(decisionfacetentry.KindAsker),
			decisionfacetentry.ActorIDEQ(agentKeep),
		).Only(ctx)
	require.NoError(t, err)
	assert.Equal(t, "Agent Keep Old", askerEntry.Name)
	assert.True(t, t1.Equal(askerEntry.LatestAt.UTC().Truncate(time.Microsecond)))

	ansEntry, err := cs.client.DecisionFacetEntry.Query().
		Where(
			decisionfacetentry.ProjectIDEQ(uuid.MustParse(proj1)),
			decisionfacetentry.ChatKeyEQ(topic1),
			decisionfacetentry.KindEQ(decisionfacetentry.KindAnswerer),
			decisionfacetentry.ActorIDEQ(user1),
		).Only(ctx)
	require.NoError(t, err)
	assert.Equal(t, "User1 Old", ansEntry.Name)
	assert.Equal(t, "u1-old@test.com", ansEntry.Email)
	assert.True(t, t1.Equal(ansEntry.LatestAt.UTC().Truncate(time.Microsecond)))

	// 4. Differential check: summary-backed reads vs legacy queries.
	allProjs := []string{proj1, proj2}
	allProjUUIDs := []uuid.UUID{uuid.MustParse(proj1), uuid.MustParse(proj2)}
	allTopics := []string{topic1, topic2}

	gotAgents, gotAnswerers, gotTopics, err := cs.ListDecisionFacets(ctx, allProjs, allTopics)
	require.NoError(t, err)
	legAgents, legAnswerers, legTopics, err := cs.listDecisionFacetsLegacy(ctx, allProjUUIDs, allTopics)
	require.NoError(t, err)
	assert.Equal(t, legAgents, gotAgents, "ListDecisionFacets agents vs legacy")
	assert.Equal(t, legAnswerers, gotAnswerers, "ListDecisionFacets answerers vs legacy")
	assert.Equal(t, legTopics, gotTopics, "ListDecisionFacets topics vs legacy")

	gotProjs, err := cs.ProjectsWithDecisions(ctx, allProjs)
	require.NoError(t, err)
	legProjs, err := cs.projectsWithDecisionsLegacy(ctx, allProjUUIDs)
	require.NoError(t, err)
	assert.Equal(t, legProjs, gotProjs, "ProjectsWithDecisions vs legacy")
	assert.Equal(t, []string{proj1}, gotProjs, "project 2 should no longer have decisions")

	gotByProj, err := cs.ListProjectDecisionAgentsForProjects(ctx, allProjs)
	require.NoError(t, err)
	legByProj, err := cs.listProjectDecisionAgentsForProjectsLegacy(ctx, allProjUUIDs)
	require.NoError(t, err)
	assert.Equal(t, legByProj, gotByProj, "ListProjectDecisionAgentsForProjects vs legacy")
	_ = t3
}

// TestDecisionRetentionStore_CandidateScanPlan verifies that
// SelectDecisionRetentionCandidates uses the partial index
// decision_settled_created_at_id on both SQLite and Postgres.
func TestDecisionRetentionStore_CandidateScanPlan(t *testing.T) {
	ctx := context.Background()
	cs := newTestCompositeStore(t)
	st := NewDecisionRetentionStore(cs)

	// Seed enough rows so Postgres planner prefers the partial index.
	projID := uuid.New()
	agentID := uuid.New()
	base := time.Now().UTC().Add(-90 * 24 * time.Hour).Truncate(time.Microsecond)
	choices := []messages.Choice{{ID: "1", Label: "Yes"}}

	tx, err := cs.client.Tx(ctx)
	require.NoError(t, err)
	for i := 0; i < 200; i++ {
		dc := tx.Decision.Create().
			SetProjectID(projID).
			SetMessageID(uuid.New()).
			SetPosition(0).
			SetAskerAgentID(agentID).
			SetAskerAgentName("bot").
			SetKey(fmt.Sprintf("P%d", i)).
			SetKeyNorm(fmt.Sprintf("p%d", i)).
			SetKeySource(decision.KeySourceAgent).
			SetVersion(1).
			SetQuestion("Q").
			SetChoices(choices).
			SetCreatedAt(base.Add(time.Duration(i) * time.Minute))
		if i < 20 {
			dc.SetStatus(decision.StatusWithdrawn).SetSettled(true).SetWithdrawnAt(base)
		} else {
			dc.SetStatus(decision.StatusPending).SetSettled(false)
		}
		require.NoError(t, dc.Exec(ctx))
	}
	require.NoError(t, tx.Commit())

	cutoff := time.Now().UTC().Add(-30 * 24 * time.Hour)
	for _, after := range []store.DecisionRetentionPosition{
		{},
		{CreatedAt: base.Add(5 * time.Minute), ID: uuid.Nil.String()},
	} {
		q, args := st.candidateScanQuery(cutoff, after, 50)
		if st.pg {
			_, err := st.db.ExecContext(ctx, "ANALYZE decisions")
			require.NoError(t, err)
			// Disable seqscan inside a local tx so Postgres proves the query
			// satisfies the partial index predicate even on a small test table.
			ptx, err := st.db.BeginTx(ctx, nil)
			require.NoError(t, err)
			_, err = ptx.ExecContext(ctx, "SET LOCAL enable_seqscan = off")
			require.NoError(t, err)
			rows, err := ptx.QueryContext(ctx, "EXPLAIN "+q, args...)
			require.NoError(t, err)
			var lines []string
			for rows.Next() {
				var line string
				require.NoError(t, rows.Scan(&line))
				lines = append(lines, line)
			}
			require.NoError(t, rows.Close())
			_ = ptx.Rollback()
			plan := strings.Join(lines, "\n")
			t.Logf("Postgres plan (after=%v):\n%s", !after.IsStart(), plan)
			assert.Contains(t, plan, "decision_settled_created_at_id", "Postgres plan must use partial index")
		} else {
			rows, err := st.db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+q, args...)
			require.NoError(t, err)
			var details []string
			for rows.Next() {
				var id, parent, notused int
				var detail string
				require.NoError(t, rows.Scan(&id, &parent, &notused, &detail))
				details = append(details, detail)
			}
			require.NoError(t, rows.Close())
			plan := strings.Join(details, " | ")
			t.Logf("SQLite plan (after=%v): %s", !after.IsStart(), plan)
			assert.Contains(t, plan, "decision_settled_created_at_id", "SQLite plan must use partial index")
		}
	}
}

// TestDecisionRetentionStore_CursorAndRuns verifies cursor persistence in
// hub_settings and run row CRUD, counts-only JSON, and 365-day pruning.
func TestDecisionRetentionStore_CursorAndRuns(t *testing.T) {
	ctx := context.Background()
	cs := newTestCompositeStore(t)
	st := cs.DecisionRetention()

	// Empty cursor on fresh DB.
	cur, err := st.GetDecisionRetentionCursor(ctx)
	require.NoError(t, err)
	assert.True(t, cur.Position.IsStart())
	assert.Nil(t, cur.ResetAt)

	now := time.Now().UTC().Truncate(time.Microsecond)
	resetAt := now.Add(-2 * time.Hour)
	cur = store.DecisionRetentionCursor{
		Position:  store.DecisionRetentionPosition{CreatedAt: now.Add(-40 * 24 * time.Hour), ID: uuid.NewString()},
		ResetAt:   &resetAt,
		UpdatedAt: now,
	}
	require.NoError(t, st.SaveDecisionRetentionCursor(ctx, cur))

	gotCur, err := st.GetDecisionRetentionCursor(ctx)
	require.NoError(t, err)
	assert.Equal(t, cur.Position.ID, gotCur.Position.ID)
	assert.True(t, cur.Position.CreatedAt.Equal(gotCur.Position.CreatedAt))
	require.NotNil(t, gotCur.ResetAt)
	assert.True(t, resetAt.Equal(*gotCur.ResetAt))

	// Run CRUD + counts-only verification.
	projID := uuid.NewString()
	fin := now.Add(time.Second)
	run := &store.DecisionRetentionRun{
		Trigger:    store.DecisionRetentionTriggerSweep,
		StartedAt:  now,
		FinishedAt: &fin,
		Status:     store.DecisionRetentionStatusOK,
		Policy:     store.DecisionRetentionPolicy{RetentionDays: 30, Cutoff: now.Add(-30 * 24 * time.Hour)},
		Counts:     map[string]store.DecisionRetentionCounts{projID: {Decisions: 5, Answers: 4}},
	}
	require.NoError(t, st.SaveDecisionRetentionRun(ctx, run))
	require.NotEmpty(t, run.ID)

	latest, err := st.LatestDecisionRetentionRun(ctx, store.DecisionRetentionTriggerSweep, now.Add(-time.Hour))
	require.NoError(t, err)
	require.NotNil(t, latest)
	assert.Equal(t, run.ID, latest.ID)
	assert.Equal(t, int64(5), latest.Counts[projID].Decisions)
	assert.Equal(t, int64(4), latest.Counts[projID].Answers)

	// Verify raw DB JSON has counts only (no decision/answer IDs or content).
	entRow, err := cs.client.DecisionRetentionRun.Get(ctx, uuid.MustParse(run.ID))
	require.NoError(t, err)
	var countsMap map[string]map[string]any
	require.NoError(t, json.Unmarshal(entRow.Counts, &countsMap))
	assert.Len(t, countsMap[projID], 2)
	assert.Contains(t, countsMap[projID], "decisions")
	assert.Contains(t, countsMap[projID], "answers")

	// Insert an old run (> 365 days ago) and verify PruneDecisionRetentionRuns deletes only it.
	oldRun := &store.DecisionRetentionRun{
		Trigger:   store.DecisionRetentionTriggerSweep,
		StartedAt: now.Add(-400 * 24 * time.Hour),
		Status:    store.DecisionRetentionStatusOK,
	}
	require.NoError(t, st.SaveDecisionRetentionRun(ctx, oldRun))

	pruned, err := st.PruneDecisionRetentionRuns(ctx, now.Add(-365*24*time.Hour))
	require.NoError(t, err)
	assert.Equal(t, 1, pruned)

	runs, err := st.ListDecisionRetentionRuns(ctx, store.DecisionRetentionRunFilter{Limit: 10})
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, run.ID, runs[0].ID)

	// Verify unused import guard.
	_ = decisionanswer.Table
}

// TestDecisionRetentionStore_CandidateCursorTieBreak verifies that
// SelectDecisionRetentionCandidates walks tied created_at rows in exact
// (created_at, id) order without skipping or duplicating rows when paginated
// with limit=1 and limit=2.
func TestDecisionRetentionStore_CandidateCursorTieBreak(t *testing.T) {
	ctx := context.Background()
	cs := newTestCompositeStore(t)
	st := cs.DecisionRetention()

	projID := uuid.New()
	agentID := uuid.New()
	now := time.Now().UTC().Truncate(time.Microsecond)
	cutoff := now.Add(-30 * 24 * time.Hour)
	tEarlier := now.Add(-70 * 24 * time.Hour)
	tTied := now.Add(-60 * 24 * time.Hour)
	tLater := now.Add(-50 * 24 * time.Hour)
	tAfterCutoff := now.Add(-10 * 24 * time.Hour)
	choices := []messages.Choice{{ID: "1", Label: "Yes"}}

	type seeded struct {
		id        string
		createdAt time.Time
	}
	var expected []seeded

	insertRow := func(key string, created time.Time, settled bool) string {
		id := uuid.New()
		dc := cs.client.Decision.Create().
			SetID(id).
			SetProjectID(projID).
			SetMessageID(uuid.New()).
			SetPosition(0).
			SetAskerAgentID(agentID).
			SetAskerAgentName("bot").
			SetKey(key).
			SetKeyNorm(strings.ToLower(key)).
			SetKeySource(decision.KeySourceAgent).
			SetVersion(1).
			SetQuestion("Q " + key).
			SetChoices(choices).
			SetCreatedAt(created).
			SetSettled(settled).
			SetFacetIndexed(true)
		if settled {
			dc.SetStatus(decision.StatusWithdrawn).SetWithdrawnAt(created)
		} else {
			dc.SetStatus(decision.StatusPending)
		}
		require.NoError(t, dc.Exec(ctx))
		return id.String()
	}

	// Seed 1 earlier, 3 tied on the exact same created_at, and 1 later (all < cutoff).
	expected = append(expected, seeded{insertRow("E1", tEarlier, true), tEarlier})
	for i := 0; i < 3; i++ {
		expected = append(expected, seeded{insertRow(fmt.Sprintf("T%d", i), tTied, true), tTied})
	}
	expected = append(expected, seeded{insertRow("L1", tLater, true), tLater})
	// Noise rows that must never be returned: unsettled at tTied, and settled >= cutoff.
	insertRow("UNSETTLED", tTied, false)
	insertRow("RECENT", tAfterCutoff, true)

	sort.Slice(expected, func(i, j int) bool {
		if !expected[i].createdAt.Equal(expected[j].createdAt) {
			return expected[i].createdAt.Before(expected[j].createdAt)
		}
		return expected[i].id < expected[j].id
	})
	wantIDs := make([]string, len(expected))
	for i, e := range expected {
		wantIDs[i] = e.id
	}

	for _, limit := range []int{1, 2} {
		t.Run(fmt.Sprintf("limit=%d", limit), func(t *testing.T) {
			var after store.DecisionRetentionPosition
			var gotIDs []string
			seen := make(map[string]bool)
			for guard := 0; guard < 20; guard++ {
				page, err := st.SelectDecisionRetentionCandidates(ctx, cutoff, after, limit)
				require.NoError(t, err)
				if len(page) == 0 {
					break
				}
				assert.LessOrEqual(t, len(page), limit)
				for _, c := range page {
					assert.False(t, seen[c.ID], "duplicate candidate id %s", c.ID)
					seen[c.ID] = true
					gotIDs = append(gotIDs, c.ID)
				}
				last := page[len(page)-1]
				after = store.DecisionRetentionPosition{CreatedAt: last.CreatedAt, ID: last.ID}
			}
			assert.Equal(t, wantIDs, gotIDs, "walk order must match (created_at, id) exactly")
		})
	}
}

// TestDecisionRetentionStore_SkipsUnindexedFacetRows verifies on both SQLite
// and Postgres that an old settled message with any facet_indexed=false
// decision or answer row is skipped by PurgeDecisionMessages, while a fully
// indexed sibling message is purged and decision_facet_entries matches a full
// recompute before and after purge.
func TestDecisionRetentionStore_SkipsUnindexedFacetRows(t *testing.T) {
	ctx := context.Background()
	cs := newTestCompositeStore(t)
	st := cs.DecisionRetention()

	now := time.Now().UTC().Truncate(time.Microsecond)
	cutoff := now.Add(-30 * 24 * time.Hour)
	oldTime := now.Add(-60 * 24 * time.Hour)

	proj := uuid.NewString()
	require.NoError(t, cs.CreateProject(ctx, &store.Project{
		ID: proj, Name: "ret-unidx", Slug: "ret-unidx", Created: oldTime,
	}))
	userID := uuid.NewString()
	require.NoError(t, cs.CreateUser(ctx, &store.User{
		ID: userID, Email: "unidx@test.com", DisplayName: "Unidx User",
		Role: store.UserRoleMember, Status: "active", Created: oldTime,
	}))
	agentID := uuid.NewString()
	topic := "topic-unidx-" + uuid.NewString()
	choices := []messages.Choice{{ID: "1", Label: "Yes"}, {ID: "2", Label: "No"}}

	seed := func(key string) (msgID, decID string) {
		msgID = uuid.NewString()
		res, err := cs.CreateDecisionPrompt(ctx, &store.DecisionPromptInput{
			Message: &store.Message{
				ID: msgID, ProjectID: proj, ThreadID: topic,
				Sender: "agent:bot", SenderID: agentID, Recipient: "user:" + userID, RecipientID: userID,
			},
			AskerAgentID:   agentID,
			AskerAgentName: "bot",
			Decisions:      []messages.Decision{{Key: key, Question: "Q " + key, Choices: choices}},
		})
		require.NoError(t, err)
		decID = res.Decisions[0].RowID
		_, _, err = cs.RecordAnswer(ctx, &store.RecordAnswerInput{
			DecisionRowID:    decID,
			ChoiceID:         "1",
			AnsweredByUserID: userID,
			AnsweredByName:   "Unidx User",
			AnsweredByEmail:  "unidx@test.com",
			Provenance:       store.DecisionProvenanceDirect,
			Surface:          store.DecisionSurfaceNative,
			ResponseMessage: &store.Message{
				ID: uuid.NewString(), ProjectID: proj, ThreadID: topic,
				Sender: "user:" + userID, SenderID: userID, Recipient: "agent:bot", RecipientID: agentID,
			},
		})
		require.NoError(t, err)
		uid := uuid.MustParse(decID)
		db := clientDB(cs.client)
		qDec := "UPDATE decisions SET created_at = ? WHERE id = ?"
		qAns := "UPDATE decision_answers SET created_at = ? WHERE decision_row_id = ?"
		if cs.isPostgres() {
			qDec = "UPDATE decisions SET created_at = $1 WHERE id = $2"
			qAns = "UPDATE decision_answers SET created_at = $1 WHERE decision_row_id = $2"
		}
		_, err = db.ExecContext(ctx, qDec, oldTime, uid)
		require.NoError(t, err)
		require.NoError(t, cs.client.Decision.UpdateOneID(uid).SetDecidedAt(oldTime).Exec(ctx))
		_, err = db.ExecContext(ctx, qAns, oldTime, uid)
		require.NoError(t, err)
		return msgID, decID
	}

	msgIndexed, decIndexed := seed("IDX1")
	msgUnindexedDec, decUnindexedDec := seed("UIDX-DEC")
	msgUnindexedAns, decUnindexedAns := seed("UIDX-ANS")

	// Rebuild facet entries cleanly so only the rows we want indexed have
	// facet_indexed=true and matching decision_facet_entries rows.
	_, err := cs.client.DecisionFacetEntry.Delete().Exec(ctx)
	require.NoError(t, err)
	require.NoError(t, cs.client.Decision.Update().SetFacetIndexed(false).Exec(ctx))
	require.NoError(t, cs.client.DecisionAnswer.Update().SetFacetIndexed(false).Exec(ctx))
	// Index everything first, then unindex decUnindexedDec (decision) and
	// decUnindexedAns (answer) and rebuild summary via targeted reindex.
	drainDecisionIndex(t, ctx, cs, 100)

	// Re-create exact partial-index state:
	// - msgIndexed: decision=true, answer=true
	// - msgUnindexedDec: decision=false, answer=true
	// - msgUnindexedAns: decision=true, answer=false
	_, err = cs.client.DecisionFacetEntry.Delete().Exec(ctx)
	require.NoError(t, err)
	require.NoError(t, cs.client.Decision.Update().SetFacetIndexed(false).Exec(ctx))
	require.NoError(t, cs.client.DecisionAnswer.Update().SetFacetIndexed(false).Exec(ctx))
	// Index only the two decisions that should be indexed (temporarily mark
	// decUnindexedDec=true so decisions page skips it, then flip it back).
	require.NoError(t, cs.client.Decision.UpdateOneID(uuid.MustParse(decUnindexedDec)).SetFacetIndexed(true).Exec(ctx))
	_, _, err = cs.IndexDecisionFacetsDecisionsPage(ctx, "", 100)
	require.NoError(t, err)
	require.NoError(t, cs.client.Decision.UpdateOneID(uuid.MustParse(decUnindexedDec)).SetFacetIndexed(false).Exec(ctx))
	// Index only the two answers that should be indexed (temporarily mark
	// decUnindexedAns's answer=true so answers page skips it, then flip it back).
	require.NoError(t, cs.client.DecisionAnswer.Update().
		Where(decisionanswer.DecisionRowIDEQ(uuid.MustParse(decUnindexedAns))).
		SetFacetIndexed(true).Exec(ctx))
	_, _, err = cs.IndexDecisionFacetsAnswersPage(ctx, "", 100)
	require.NoError(t, err)
	require.NoError(t, cs.client.DecisionAnswer.Update().
		Where(decisionanswer.DecisionRowIDEQ(uuid.MustParse(decUnindexedAns))).
		SetFacetIndexed(false).Exec(ctx))

	// Before purge: 2 indexed decisions (msgIndexed, msgUnindexedAns) and
	// 2 indexed answers (msgIndexed, msgUnindexedDec) are in decision_facet_entries.
	// Purge all three: only msgIndexed is eligible.
	res, err := st.PurgeDecisionMessages(ctx, []string{msgIndexed, msgUnindexedDec, msgUnindexedAns}, map[string]time.Time{proj: cutoff})
	require.NoError(t, err)
	assert.Equal(t, 1, res.PurgedMessages)
	assert.Equal(t, 2, res.SkippedMessages)

	_, err = cs.GetDecision(ctx, decIndexed)
	assert.ErrorIs(t, err, store.ErrNotFound, "indexed message must be purged")
	_, err = cs.GetDecision(ctx, decUnindexedDec)
	assert.NoError(t, err, "message with facet_indexed=false decision must be skipped")
	_, err = cs.GetDecision(ctx, decUnindexedAns)
	assert.NoError(t, err, "message with facet_indexed=false answer must be skipped")

	// Summary ref_counts must now be 1 asker and 1 answerer (the indexed row
	// from msgUnindexedAns and msgUnindexedDec respectively).
	askerRow, err := cs.client.DecisionFacetEntry.Query().
		Where(decisionfacetentry.KindEQ(decisionfacetentry.KindAsker), decisionfacetentry.ActorIDEQ(agentID)).
		Only(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, askerRow.RefCount)
	ansRow, err := cs.client.DecisionFacetEntry.Query().
		Where(decisionfacetentry.KindEQ(decisionfacetentry.KindAnswerer), decisionfacetentry.ActorIDEQ(userID)).
		Only(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, ansRow.RefCount)

	// Once the indexer catches up, summary equals a full recompute, and both
	// remaining messages become eligible and purge cleanly.
	drainDecisionIndex(t, ctx, cs, 100)
	assertFacetSummaryMatchesRecompute(t, ctx, cs.client)

	res2, err := st.PurgeDecisionMessages(ctx, []string{msgUnindexedDec, msgUnindexedAns}, map[string]time.Time{proj: cutoff})
	require.NoError(t, err)
	assert.Equal(t, 2, res2.PurgedMessages)
	assert.Equal(t, 0, res2.SkippedMessages)
	assertFacetSummaryMatchesRecompute(t, ctx, cs.client)
}

// TestDecisionRetentionStore_PerProjectWindowsPreviewAndDifferentialFacets
// proves on BOTH SQLite and Postgres that:
//   - per-project cutoffs in cutoffByProject purge each project according to its
//     own window (30d default, 60d override keeps 45d and purges 75d, omitted
//     project = keep_forever keeps 90d);
//   - PreviewDecisionMessages applies the exact same facet_indexed=false skip
//     as PurgeDecisionMessages and returns identical per-project counts without
//     deleting any rows;
//   - after purge under mixed per-project windows, summary-backed facets,
//     ProjectsWithDecisions, and ListProjectDecisionAgentsForProjects equal
//     the legacy queries on the remaining rows.
func TestDecisionRetentionStore_PerProjectWindowsPreviewAndDifferentialFacets(t *testing.T) {
	ctx := context.Background()
	cs := newTestCompositeStore(t)
	st := cs.DecisionRetention()

	now := time.Now().UTC().Truncate(time.Microsecond)
	oldTime := now.Add(-100 * 24 * time.Hour)

	projDefault := uuid.NewString()
	projLong := uuid.NewString()
	projHold := uuid.NewString()
	for i, pid := range []string{projDefault, projLong, projHold} {
		require.NoError(t, cs.CreateProject(ctx, &store.Project{
			ID: pid, Name: fmt.Sprintf("ret-mix-%d", i), Slug: fmt.Sprintf("ret-mix-%d", i), Created: oldTime,
		}))
	}
	userID := uuid.NewString()
	require.NoError(t, cs.CreateUser(ctx, &store.User{
		ID: userID, Email: "ret-mix@test.com", DisplayName: "Mix User",
		Role: store.UserRoleMember, Status: "active", Created: oldTime,
	}))

	choices := []messages.Choice{{ID: "1", Label: "Yes"}, {ID: "2", Label: "No"}}
	seedSettled := func(pid, agentID, agentName, topic, key string, ageDays int) (msgID, decID string) {
		ts := now.Add(-time.Duration(ageDays) * 24 * time.Hour)
		msgID = uuid.NewString()
		res, err := cs.CreateDecisionPrompt(ctx, &store.DecisionPromptInput{
			Message: &store.Message{
				ID: msgID, ProjectID: pid, ThreadID: topic,
				Sender: "agent:" + agentName, SenderID: agentID, Recipient: "user:" + userID, RecipientID: userID,
			},
			AskerAgentID:   agentID,
			AskerAgentName: agentName,
			Decisions:      []messages.Decision{{Key: key, Question: "Q " + key, Choices: choices}},
		})
		require.NoError(t, err)
		decID = res.Decisions[0].RowID
		_, _, err = cs.RecordAnswer(ctx, &store.RecordAnswerInput{
			DecisionRowID:    decID,
			ChoiceID:         "1",
			AnsweredByUserID: userID,
			AnsweredByName:   "Mix User",
			AnsweredByEmail:  "ret-mix@test.com",
			Provenance:       store.DecisionProvenanceDirect,
			Surface:          store.DecisionSurfaceNative,
			ResponseMessage: &store.Message{
				ID: uuid.NewString(), ProjectID: pid, ThreadID: topic,
				Sender: "user:" + userID, SenderID: userID, Recipient: "agent:" + agentName, RecipientID: agentID,
			},
		})
		require.NoError(t, err)

		uid := uuid.MustParse(decID)
		db := clientDB(cs.client)
		qDec := "UPDATE decisions SET created_at = ?, decided_at = ? WHERE id = ?"
		qAns := "UPDATE decision_answers SET created_at = ? WHERE decision_row_id = ?"
		if cs.isPostgres() {
			qDec = "UPDATE decisions SET created_at = $1, decided_at = $2 WHERE id = $3"
			qAns = "UPDATE decision_answers SET created_at = $1 WHERE decision_row_id = $2"
		}
		_, err = db.ExecContext(ctx, qDec, ts, ts, uid)
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, qAns, ts, uid)
		require.NoError(t, err)
		return msgID, decID
	}

	topicDef := "topic-def-" + uuid.NewString()
	topicLong := "topic-long-" + uuid.NewString()
	topicHold := "topic-hold-" + uuid.NewString()

	agentDefOld := uuid.NewString()
	agentDefNew := uuid.NewString()
	agentLong := uuid.NewString()
	agentHold := uuid.NewString()

	// projDefault (window 30d): 45d purged, 15d kept, plus one unindexed 45d skipped
	mDef45, dDef45 := seedSettled(projDefault, agentDefOld, "DefOld", topicDef, "DEF-45", 45)
	mDef15, dDef15 := seedSettled(projDefault, agentDefNew, "DefNew", topicDef, "DEF-15", 15)
	mDefUnidx, dDefUnidx := seedSettled(projDefault, agentDefNew, "DefNew", topicDef, "DEF-UNIDX", 45)
	// projLong (window 60d): 75d purged, 45d kept
	mLong75, dLong75 := seedSettled(projLong, agentLong, "LongBot", topicLong, "LONG-75", 75)
	mLong45, dLong45 := seedSettled(projLong, agentLong, "LongBot", topicLong, "LONG-45", 45)
	// projHold (keep_forever / omitted): 90d kept
	mHold90, dHold90 := seedSettled(projHold, agentHold, "HoldBot", topicHold, "HOLD-90", 90)

	// Rebuild facet entries from the back-dated timestamps so latest_at matches created_at.
	_, err := cs.client.DecisionFacetEntry.Delete().Exec(ctx)
	require.NoError(t, err)
	require.NoError(t, cs.client.Decision.Update().SetFacetIndexed(false).Exec(ctx))
	require.NoError(t, cs.client.DecisionAnswer.Update().SetFacetIndexed(false).Exec(ctx))
	drainDecisionIndex(t, ctx, cs, 100)
	assertFacetSummaryMatchesRecompute(t, ctx, cs.client)

	// Flip dDefUnidx to facet_indexed=false to prove PreviewDecisionMessages skips
	// unindexed rows identically to PurgeDecisionMessages.
	require.NoError(t, cs.client.Decision.UpdateOneID(uuid.MustParse(dDefUnidx)).SetFacetIndexed(false).Exec(ctx))

	allMsgs := []string{mDef45, mDef15, mDefUnidx, mLong75, mLong45, mHold90}
	cutoffByProject := map[string]time.Time{
		projDefault: now.Add(-30 * 24 * time.Hour),
		projLong:    now.Add(-60 * 24 * time.Hour),
		// projHold intentionally omitted (keep_forever)
	}

	// 1. Preview returns exact per-project counts (skipping unindexed row) and deletes NOTHING.
	previewRes, err := st.PreviewDecisionMessages(ctx, allMsgs, cutoffByProject)
	require.NoError(t, err)
	assert.Equal(t, 2, previewRes.PurgedMessages)
	assert.Equal(t, 4, previewRes.SkippedMessages)
	assert.Equal(t, store.DecisionRetentionCounts{Decisions: 1, Answers: 1}, previewRes.ByProject[projDefault])
	assert.Equal(t, store.DecisionRetentionCounts{Decisions: 1, Answers: 1}, previewRes.ByProject[projLong])
	assert.Zero(t, previewRes.ByProject[projHold])

	// Confirm nothing was deleted by preview.
	for _, id := range []string{dDef45, dDef15, dDefUnidx, dLong75, dLong45, dHold90} {
		_, err := cs.GetDecision(ctx, id)
		require.NoError(t, err, "preview must not delete decision %s", id)
	}

	// 2. Purge with the same inputs produces identical counts.
	purgeRes, err := st.PurgeDecisionMessages(ctx, allMsgs, cutoffByProject)
	require.NoError(t, err)
	assert.Equal(t, previewRes, purgeRes, "preview counts must equal purge counts (including unindexed skip)")

	// Verify exact kept vs purged rows.
	for _, gone := range []string{dDef45, dLong75} {
		_, err := cs.GetDecision(ctx, gone)
		assert.ErrorIs(t, err, store.ErrNotFound, "decision %s should be purged", gone)
	}
	for _, kept := range []string{dDef15, dDefUnidx, dLong45, dHold90} {
		_, err := cs.GetDecision(ctx, kept)
		assert.NoError(t, err, "decision %s should be kept", kept)
	}

	// Restore dDefUnidx facet_indexed=true (its facet entry was already counted
	// during the initial drain above) before running the full differential check.
	require.NoError(t, cs.client.Decision.UpdateOneID(uuid.MustParse(dDefUnidx)).SetFacetIndexed(true).Exec(ctx))

	// 3. Differential facet check across all 3 projects after mixed-window purge.
	assertFacetSummaryMatchesRecompute(t, ctx, cs.client)
	allProjects := []string{projDefault, projLong, projHold}
	allProjectUUIDs := []uuid.UUID{uuid.MustParse(projDefault), uuid.MustParse(projLong), uuid.MustParse(projHold)}
	allTopics := []string{topicDef, topicLong, topicHold}

	sumAgents, sumAnswerers, sumConvs, err := cs.ListDecisionFacets(ctx, allProjects, allTopics)
	require.NoError(t, err)
	legAgents, legAnswerers, legConvs, err := cs.listDecisionFacetsLegacy(ctx, allProjectUUIDs, allTopics)
	require.NoError(t, err)
	assert.ElementsMatch(t, legAgents, sumAgents, "summary agents must equal legacy after mixed-window purge")
	assert.ElementsMatch(t, legAnswerers, sumAnswerers, "summary answerers must equal legacy after mixed-window purge")
	assert.ElementsMatch(t, legConvs, sumConvs, "summary conversations must equal legacy after mixed-window purge")

	sumProjs, err := cs.ProjectsWithDecisions(ctx, allProjects)
	require.NoError(t, err)
	legProjs, err := cs.projectsWithDecisionsLegacy(ctx, allProjectUUIDs)
	require.NoError(t, err)
	assert.Equal(t, legProjs, sumProjs, "ProjectsWithDecisions must equal legacy after mixed-window purge")

	sumByProj, err := cs.ListProjectDecisionAgentsForProjects(ctx, allProjects)
	require.NoError(t, err)
	legByProj, err := cs.listProjectDecisionAgentsForProjectsLegacy(ctx, allProjectUUIDs)
	require.NoError(t, err)
	assert.Equal(t, legByProj, sumByProj, "ListProjectDecisionAgentsForProjects must equal legacy after mixed-window purge")
}

// TestDecisionRetentionStore_DeleteProjectPrunesOverride verifies L1
// (ptone/scion#9439) at the store layer on both SQLite and Postgres:
//  1. Deleting a project with no override is a no-op (no revision bump).
//  2. Deleting a project with an override atomically prunes its key from
//     decision_retention in hub_settings, bumps section and settings revisions,
//     and leaves other projects' overrides and keep_forever holds untouched.
//  3. Re-creating a project with the exact same UUID starts clean (sees hub
//     defaults, not the deleted project's override).
//  4. Deleting the last override leaves project_overrides empty and valid.
//  5. A malformed decision_retention document is left untouched (fail closed).
func TestDecisionRetentionStore_DeleteProjectPrunesOverride(t *testing.T) {
	ctx := context.Background()
	cs := newTestCompositeStore(t)
	now := time.Now().UTC()

	pKeep := uuid.NewString()
	pDays := uuid.NewString()
	pDel := uuid.NewString()
	pNoOverride := uuid.NewString()
	for i, pid := range []string{pKeep, pDays, pDel, pNoOverride} {
		require.NoError(t, cs.CreateProject(ctx, &store.Project{
			ID: pid, Name: fmt.Sprintf("prune-p-%d", i), Slug: fmt.Sprintf("prune-p-%d", i), Created: now,
		}))
	}

	doc := json.RawMessage(fmt.Sprintf(`{
		"enabled": true,
		"retention_days": 90,
		"project_overrides": {
			%q: {"mode": "keep_forever"},
			%q: {"mode": "days", "days": 60},
			%q: {"mode": "days", "days": 30}
		}
	}`, pKeep, pDays, pDel))
	saved, err := cs.UpsertHubSetting(ctx, opsettings.DecisionRetentionSectionName, doc, "admin@test.com", 0, "managed")
	require.NoError(t, err)
	revBefore := saved.Revision
	settingsRevBefore := saved.SettingsRevision

	// 1. Deleting a project without an override must be a no-op.
	require.NoError(t, cs.DeleteProject(ctx, pNoOverride))
	row, err := cs.GetHubSetting(ctx, opsettings.DecisionRetentionSectionName)
	require.NoError(t, err)
	assert.Equal(t, revBefore, row.Revision, "deleting a project without an override must not bump section revision")
	revRow, err := cs.GetHubSetting(ctx, store.HubSettingsRevisionSection)
	require.NoError(t, err)
	assert.Equal(t, settingsRevBefore, revRow.Revision, "deleting a project without an override must not bump settings revision")

	// 2. Deleting pDel prunes its override and preserves pKeep (keep_forever) and pDays (60d).
	require.NoError(t, cs.DeleteProject(ctx, pDel))
	row, err = cs.GetHubSetting(ctx, opsettings.DecisionRetentionSectionName)
	require.NoError(t, err)
	assert.Equal(t, revBefore+1, row.Revision)
	assert.Equal(t, "project-delete", row.UpdatedBy)
	revRow, err = cs.GetHubSetting(ctx, store.HubSettingsRevisionSection)
	require.NoError(t, err)
	assert.Greater(t, revRow.Revision, settingsRevBefore, "pruning override must bump operational settings revision")

	cfg, err := opsettings.ParseDecisionRetentionDoc(row.Value)
	require.NoError(t, err)
	assert.True(t, cfg.Enabled)
	assert.Equal(t, 90, cfg.RetentionDays)
	assert.NotContains(t, cfg.ProjectOverrides, pDel, "deleted project override must be pruned")
	require.Contains(t, cfg.ProjectOverrides, pKeep)
	assert.Equal(t, opsettings.DecisionRetentionOverrideModeKeepForever, cfg.ProjectOverrides[pKeep].Mode)
	require.Contains(t, cfg.ProjectOverrides, pDays)
	require.NotNil(t, cfg.ProjectOverrides[pDays].Days)
	assert.Equal(t, 60, *cfg.ProjectOverrides[pDays].Days)

	// 3. Re-creating a project with the exact same UUID pDel sees hub default (90d), not 30d.
	require.NoError(t, cs.CreateProject(ctx, &store.Project{
		ID: pDel, Name: "prune-p-recreated", Slug: "prune-p-recreated", Created: now,
	}))
	row, err = cs.GetHubSetting(ctx, opsettings.DecisionRetentionSectionName)
	require.NoError(t, err)
	cfg, err = opsettings.ParseDecisionRetentionDoc(row.Value)
	require.NoError(t, err)
	days, ok := cfg.EffectiveDays(pDel)
	assert.True(t, ok)
	assert.Equal(t, 90, days, "re-created same-UUID project must inherit hub default, not deleted project's 30d override")
	_, held := cfg.EffectiveDays(pKeep)
	assert.False(t, held, "keep_forever hold on pKeep must remain intact")

	// 4. Deleting remaining overrides leaves project_overrides empty and valid.
	require.NoError(t, cs.DeleteProject(ctx, pKeep))
	require.NoError(t, cs.DeleteProject(ctx, pDays))
	row, err = cs.GetHubSetting(ctx, opsettings.DecisionRetentionSectionName)
	require.NoError(t, err)
	cfg, err = opsettings.ParseDecisionRetentionDoc(row.Value)
	require.NoError(t, err)
	assert.Empty(t, cfg.ProjectOverrides)
	assert.True(t, cfg.Enabled)
	assert.Equal(t, 90, cfg.RetentionDays)

	// 5. Malformed stored JSON is left untouched (fail closed) on project delete.
	pMalformed := uuid.NewString()
	require.NoError(t, cs.CreateProject(ctx, &store.Project{
		ID: pMalformed, Name: "prune-p-bad", Slug: "prune-p-bad", Created: now,
	}))
	badDoc := json.RawMessage(`{"unknown_field": true}`)
	badRow, err := cs.UpsertHubSetting(ctx, opsettings.DecisionRetentionSectionName, badDoc, "test", row.Revision, "managed")
	require.NoError(t, err)
	require.NoError(t, cs.DeleteProject(ctx, pMalformed))
	afterBad, err := cs.GetHubSetting(ctx, opsettings.DecisionRetentionSectionName)
	require.NoError(t, err)
	assert.Equal(t, badRow.Revision, afterBad.Revision, "malformed decision_retention doc must not be rewritten on project delete")
}
