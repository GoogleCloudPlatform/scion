//go:build !no_sqlite

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

package hub

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/stretchr/testify/require"
)

// decisionsLifecycleGoldenPath pins the HTTP response bodies (success and
// every 409 conflict code) and chat.decision.updated event payloads for the
// decision lifecycle so the P5a message-action extraction preserves every
// wire detail byte for byte.
const decisionsLifecycleGoldenPath = "testdata/decisions-lifecycle-golden.json"

type goldenCapture struct {
	Status  int           `json:"status,omitempty"`
	Body    any           `json:"body,omitempty"`
	Subject string        `json:"subject,omitempty"`
	Events  []goldenEvent `json:"events,omitempty"`
}

type goldenEvent struct {
	Subject string `json:"subject"`
	Payload any    `json:"payload"`
}

var (
	goldenUUIDRe      = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	goldenTimestampRe = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})`)
)

// goldenNormalizer replaces fixture IDs, dynamic UUIDs (consistently per case),
// and RFC3339 timestamps with stable placeholders while keeping every JSON key,
// status code, error code, message, and structure intact.
type goldenNormalizer struct {
	fixed  map[string]string
	seen   map[string]string
	nextID int
}

func newGoldenNormalizer(f *decisionsFixture, dmKey string) *goldenNormalizer {
	fixed := map[string]string{
		f.agent.ID:   "<AGENT_ID>",
		f.bob.ID:     "<USER_BOB_ID>",
		f.alice.ID:   "<USER_ALICE_ID>",
		f.project.ID: "<PROJECT_ID>",
		f.topicID:    "<TOPIC_ID>",
	}
	if dmKey != "" {
		fixed[dmKey] = "<DM_KEY>"
	}
	return &goldenNormalizer{
		fixed: fixed,
		seen:  make(map[string]string),
	}
}

func (n *goldenNormalizer) normalizeString(s string) string {
	// Replace longer fixed keys first (e.g. dmKey before constituent UUIDs).
	for k, v := range n.fixed {
		if strings.HasPrefix(k, "dm:") && k != "" {
			s = strings.ReplaceAll(s, k, v)
		}
	}
	for k, v := range n.fixed {
		if !strings.HasPrefix(k, "dm:") && k != "" {
			s = strings.ReplaceAll(s, k, v)
		}
	}
	s = goldenTimestampRe.ReplaceAllString(s, "<TIMESTAMP>")
	s = goldenUUIDRe.ReplaceAllStringFunc(s, func(u string) string {
		if rep, ok := n.seen[u]; ok {
			return rep
		}
		n.nextID++
		rep := fmt.Sprintf("<UUID_%d>", n.nextID)
		n.seen[u] = rep
		return rep
	})
	return s
}

func (n *goldenNormalizer) normalizeValue(v any) any {
	switch x := v.(type) {
	case string:
		return n.normalizeString(x)
	case []any:
		out := make([]any, len(x))
		for i, elem := range x {
			out[i] = n.normalizeValue(elem)
		}
		return out
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make(map[string]any, len(x))
		for _, k := range keys {
			nk := n.normalizeString(k)
			out[nk] = n.normalizeValue(x[k])
		}
		return out
	default:
		return v
	}
}

func (n *goldenNormalizer) decodeBody(t *testing.T, raw []byte) any {
	t.Helper()
	var parsed any
	require.NoError(t, json.Unmarshal(raw, &parsed), "raw body: %s", string(raw))
	return n.normalizeValue(parsed)
}

func (n *goldenNormalizer) decodeEvents(t *testing.T, subject string, evts []Event) []goldenEvent {
	t.Helper()
	out := make([]goldenEvent, 0, len(evts))
	for _, e := range evts {
		var parsed any
		require.NoError(t, json.Unmarshal(e.Data, &parsed), "raw event: %s", string(e.Data))
		out = append(out, goldenEvent{
			Subject: n.normalizeString(subject),
			Payload: n.normalizeValue(parsed),
		})
	}
	return out
}

func TestDecisionsLifecycle_Golden(t *testing.T) {
	captures := map[string]goldenCapture{}

	// 1. Single-decision answer (DM), 409 already_answered, and 409 decision_key_exists (decided).
	{
		f := newDecisionsFixture(t)
		dmKey, err := messages.DMConversationKey("agent", f.agent.ID, "user", f.bob.ID)
		require.NoError(t, err)
		dmSubject := "user." + f.bob.ID + ".chat.decision.updated"

		resp := f.sendPromptToBob(t, oneDecision, false)
		rowID := resp.Decisions[0].RowID

		// 409 decision_key_exists (pending).
		normPending := newGoldenNormalizer(f, dmKey)
		rrExistsPending := f.sendAsAgent(t, OutboundMessageRequest{
			Recipient: "user:" + f.bob.Email,
			Msg:       "dup",
			Decisions: decisionInputs(t, oneDecision),
		})
		require.Equal(t, http.StatusConflict, rrExistsPending.Code)
		captures["send_409_decision_key_exists_pending"] = goldenCapture{
			Status: rrExistsPending.Code,
			Body:   normPending.decodeBody(t, rrExistsPending.Body.Bytes()),
		}

		// 200 answer (DM) + chat.decision.updated on user subject.
		normAns := newGoldenNormalizer(f, dmKey)
		updCh, unsub := f.srv.events.Subscribe(dmSubject)
		rrAns := f.answer(t, f.bob, rowID, map[string]interface{}{"choice_id": "1"})
		require.Equal(t, http.StatusOK, rrAns.Code, rrAns.Body.String())
		evts := collectEvents(updCh)
		unsub()
		require.Len(t, evts, 1)
		captures["answer_single_dm_ok"] = goldenCapture{
			Status: rrAns.Code,
			Body:   normAns.decodeBody(t, rrAns.Body.Bytes()),
			Events: normAns.decodeEvents(t, dmSubject, evts),
		}

		// 409 already_answered.
		normAlready := newGoldenNormalizer(f, dmKey)
		rrAlready := f.answer(t, f.bob, rowID, map[string]interface{}{"choice_id": "2"})
		require.Equal(t, http.StatusConflict, rrAlready.Code)
		captures["answer_409_already_answered"] = goldenCapture{
			Status: rrAlready.Code,
			Body:   normAlready.decodeBody(t, rrAlready.Body.Bytes()),
		}

		// 409 decision_key_exists (decided).
		normExistsDecided := newGoldenNormalizer(f, dmKey)
		rrExistsDecided := f.sendAsAgent(t, OutboundMessageRequest{
			Recipient: "user:" + f.bob.Email,
			Msg:       "dup decided",
			Decisions: decisionInputs(t, oneDecision),
		})
		require.Equal(t, http.StatusConflict, rrExistsDecided.Code)
		captures["send_409_decision_key_exists_decided"] = goldenCapture{
			Status: rrExistsDecided.Code,
			Body:   normExistsDecided.decodeBody(t, rrExistsDecided.Body.Bytes()),
		}

		// 409 interpreted already_answered.
		normInterp := newGoldenNormalizer(f, dmKey)
		reply := f.bobDMReply(t, resp.MessageID, "D234: yes")
		rrInterp := f.agentDecisionReq(t, f.agent, http.MethodPost, "/api/v1/agent/decisions/interpreted", RecordInterpretedRequest{
			SourceMessageID: reply.ID,
			Answers:         []InterpretedAnswerInput{{Key: "D234", ChoiceID: "1"}},
		})
		require.Equal(t, http.StatusConflict, rrInterp.Code)
		captures["interpreted_409_already_answered"] = goldenCapture{
			Status: rrInterp.Code,
			Body:   normInterp.decodeBody(t, rrInterp.Body.Bytes()),
		}

		// 409 withdraw_failed (already_answered).
		normWdFail := newGoldenNormalizer(f, dmKey)
		rrWdFail := f.agentDecisionReq(t, f.agent, http.MethodPost, "/api/v1/agent/decisions/withdraw", WithdrawDecisionsRequest{
			Keys: []string{"D234"},
		})
		require.Equal(t, http.StatusConflict, rrWdFail.Code)
		captures["withdraw_409_failed_answered"] = goldenCapture{
			Status: rrWdFail.Code,
			Body:   normWdFail.decodeBody(t, rrWdFail.Body.Bytes()),
		}
	}

	// 2. Topic answer + chat.decision.updated on project subject.
	{
		f := newDecisionsFixture(t)
		projSubject := "project." + f.project.ID + ".chat.decision.updated"
		_, res := f.topicPrompt(t, oneDecision, false)
		rowID := res.Decisions[0].RowID

		normTopic := newGoldenNormalizer(f, "")
		updCh, unsub := f.srv.events.Subscribe(projSubject)
		rrAns := f.answer(t, f.alice, rowID, map[string]interface{}{"choice_id": "1"})
		require.Equal(t, http.StatusOK, rrAns.Code, rrAns.Body.String())
		evts := collectEvents(updCh)
		unsub()
		require.Len(t, evts, 1)
		captures["answer_single_topic_ok"] = goldenCapture{
			Status: rrAns.Code,
			Body:   normTopic.decodeBody(t, rrAns.Body.Bytes()),
			Events: normTopic.decodeEvents(t, projSubject, evts),
		}
	}

	// 3. Replace (--update) + chat.decision.updated + 409 replaced.
	{
		f := newDecisionsFixture(t)
		dmKey, err := messages.DMConversationKey("agent", f.agent.ID, "user", f.bob.ID)
		require.NoError(t, err)
		dmSubject := "user." + f.bob.ID + ".chat.decision.updated"

		v1 := f.sendPromptToBob(t, oneDecision, false)
		v1RowID := v1.Decisions[0].RowID

		normRep := newGoldenNormalizer(f, dmKey)
		updCh, unsub := f.srv.events.Subscribe(dmSubject)
		rrRep := f.sendAsAgent(t, OutboundMessageRequest{
			Recipient:       "user:" + f.bob.Email,
			Msg:             "Updated question",
			Decisions:       decisionInputs(t, `[{"id":"D234","question":"Ship v2 now?","choices":["yes","no"],"default":"yes"}]`),
			DecisionsUpdate: true,
		})
		require.Equal(t, http.StatusOK, rrRep.Code, rrRep.Body.String())
		evts := collectEvents(updCh)
		unsub()
		require.Len(t, evts, 1)
		captures["replace_update_ok"] = goldenCapture{
			Status: rrRep.Code,
			Body:   normRep.decodeBody(t, rrRep.Body.Bytes()),
			Events: normRep.decodeEvents(t, dmSubject, evts),
		}

		normAnsRep := newGoldenNormalizer(f, dmKey)
		rrAnsRep := f.answer(t, f.bob, v1RowID, map[string]interface{}{"choice_id": "1"})
		require.Equal(t, http.StatusConflict, rrAnsRep.Code)
		captures["answer_409_replaced"] = goldenCapture{
			Status: rrAnsRep.Code,
			Body:   normAnsRep.decodeBody(t, rrAnsRep.Body.Bytes()),
		}
	}

	// 4. Withdraw (DM + topic), idempotent replay, and 409 withdrawn.
	{
		f := newDecisionsFixture(t)
		dmKey, err := messages.DMConversationKey("agent", f.agent.ID, "user", f.bob.ID)
		require.NoError(t, err)
		dmSubject := "user." + f.bob.ID + ".chat.decision.updated"
		projSubject := "project." + f.project.ID + ".chat.decision.updated"

		dmResp := f.sendPromptToBob(t, oneDecision, false)
		dmRowID := dmResp.Decisions[0].RowID

		normWdDM := newGoldenNormalizer(f, dmKey)
		dmCh, unsubDM := f.srv.events.Subscribe(dmSubject)
		rrWd := f.agentDecisionReq(t, f.agent, http.MethodPost, "/api/v1/agent/decisions/withdraw", WithdrawDecisionsRequest{
			Keys:   []string{"D234"},
			Reason: "no longer needed",
		})
		require.Equal(t, http.StatusOK, rrWd.Code, rrWd.Body.String())
		evts := collectEvents(dmCh)
		unsubDM()
		require.Len(t, evts, 1)
		captures["withdraw_dm_ok"] = goldenCapture{
			Status: rrWd.Code,
			Body:   normWdDM.decodeBody(t, rrWd.Body.Bytes()),
			Events: normWdDM.decodeEvents(t, dmSubject, evts),
		}

		// Idempotent withdraw replay -> alreadyWithdrawn.
		normWdAgain := newGoldenNormalizer(f, dmKey)
		rrWdAgain := f.agentDecisionReq(t, f.agent, http.MethodPost, "/api/v1/agent/decisions/withdraw", WithdrawDecisionsRequest{
			Keys:   []string{"D234"},
			Reason: "no longer needed",
		})
		require.Equal(t, http.StatusOK, rrWdAgain.Code)
		captures["withdraw_idempotent_ok"] = goldenCapture{
			Status: rrWdAgain.Code,
			Body:   normWdAgain.decodeBody(t, rrWdAgain.Body.Bytes()),
		}

		// 409 withdrawn on answer.
		normAnsWd := newGoldenNormalizer(f, dmKey)
		rrAnsWd := f.answer(t, f.bob, dmRowID, map[string]interface{}{"choice_id": "1"})
		require.Equal(t, http.StatusConflict, rrAnsWd.Code)
		captures["answer_409_withdrawn"] = goldenCapture{
			Status: rrAnsWd.Code,
			Body:   normAnsWd.decodeBody(t, rrAnsWd.Body.Bytes()),
		}

		// Topic withdraw event.
		_, topicRes := f.topicPrompt(t, `[{"id":"T1","question":"Topic Q?","choices":["yes","no"]}]`, false)
		_ = topicRes
		normWdTopic := newGoldenNormalizer(f, dmKey)
		projCh, unsubProj := f.srv.events.Subscribe(projSubject)
		rrWdTopic := f.agentDecisionReq(t, f.agent, http.MethodPost, "/api/v1/agent/decisions/withdraw", WithdrawDecisionsRequest{
			Keys:   []string{"T1"},
			Reason: "superseded",
		})
		require.Equal(t, http.StatusOK, rrWdTopic.Code, rrWdTopic.Body.String())
		projEvts := collectEvents(projCh)
		unsubProj()
		require.Len(t, projEvts, 1)
		captures["withdraw_topic_ok"] = goldenCapture{
			Status: rrWdTopic.Code,
			Body:   normWdTopic.decodeBody(t, rrWdTopic.Body.Bytes()),
			Events: normWdTopic.decodeEvents(t, projSubject, projEvts),
		}
	}

	// 5. Dismiss: 409 too_recent, 200 dismiss + chat.decision.updated, 409 dismissed, 409 dismiss_changed.
	{
		f := newDecisionsFixture(t)
		projSubject := "project." + f.project.ID + ".chat.decision.updated"

		msg, res := f.topicPrompt(t, oneDecision, false)
		rowID := res.Decisions[0].RowID
		dismissPath := "/api/v1/decision-prompts/" + msg.ID + "/dismiss"

		// 409 too_recent (immediately after creation).
		normTooRecent := newGoldenNormalizer(f, "")
		rrEarly := doRequestAsUser(t, f.srv, f.alice, http.MethodPost, dismissPath, map[string]interface{}{
			"reason": "too early",
		})
		require.Equal(t, http.StatusConflict, rrEarly.Code, rrEarly.Body.String())
		captures["dismiss_409_too_recent"] = goldenCapture{
			Status: rrEarly.Code,
			Body:   normTooRecent.decodeBody(t, rrEarly.Body.Bytes()),
		}

		// Advance clock past 5-minute minimum -> 200 dismiss + chat.decision.updated.
		f.srv.decisionClock = func() time.Time { return time.Now().UTC().Add(10 * time.Minute) }
		normDsm := newGoldenNormalizer(f, "")
		projCh, unsubProj := f.srv.events.Subscribe(projSubject)
		rrDsm := doRequestAsUser(t, f.srv, f.alice, http.MethodPost, dismissPath, map[string]interface{}{
			"reason": "out of scope",
		})
		require.Equal(t, http.StatusOK, rrDsm.Code, rrDsm.Body.String())
		evts := collectEvents(projCh)
		unsubProj()
		require.Len(t, evts, 1)
		captures["dismiss_topic_ok"] = goldenCapture{
			Status: rrDsm.Code,
			Body:   normDsm.decodeBody(t, rrDsm.Body.Bytes()),
			Events: normDsm.decodeEvents(t, projSubject, evts),
		}

		// 409 dismissed on answer.
		normAnsDsm := newGoldenNormalizer(f, "")
		rrAnsDsm := f.answer(t, f.alice, rowID, map[string]interface{}{"choice_id": "1"})
		require.Equal(t, http.StatusConflict, rrAnsDsm.Code)
		captures["answer_409_dismissed"] = goldenCapture{
			Status: rrAnsDsm.Code,
			Body:   normAnsDsm.decodeBody(t, rrAnsDsm.Body.Bytes()),
		}

		// 409 dismiss_changed (explicitly targeting an already-answered decision).
		msg2, res2 := f.topicPrompt(t, `[{"id":"D-ANS","question":"Already answered?","choices":["yes","no"]}]`, false)
		rrAns2 := f.answer(t, f.alice, res2.Decisions[0].RowID, map[string]interface{}{"choice_id": "1"})
		require.Equal(t, http.StatusOK, rrAns2.Code)

		normChanged := newGoldenNormalizer(f, "")
		rrChanged := doRequestAsUser(t, f.srv, f.alice, http.MethodPost, "/api/v1/decision-prompts/"+msg2.ID+"/dismiss", map[string]interface{}{
			"decisionIds": []string{res2.Decisions[0].RowID},
		})
		require.Equal(t, http.StatusConflict, rrChanged.Code, rrChanged.Body.String())
		captures["dismiss_409_dismiss_changed"] = goldenCapture{
			Status: rrChanged.Code,
			Body:   normChanged.decodeBody(t, rrChanged.Body.Bytes()),
		}
	}

	// 6. Set prompt: 409 batch_required, 409 answer_set_changed, 200 set answer + chat.decision.updated.
	{
		f := newDecisionsFixture(t)
		dmKey, err := messages.DMConversationKey("agent", f.agent.ID, "user", f.bob.ID)
		require.NoError(t, err)
		dmSubject := "user." + f.bob.ID + ".chat.decision.updated"

		resp := f.sendPromptToBob(t, twoDecisions, false)
		row0 := resp.Decisions[0].RowID
		row1 := resp.Decisions[1].RowID

		// 409 batch_required.
		normBatchReq := newGoldenNormalizer(f, dmKey)
		rrBatchReq := f.answer(t, f.bob, row0, map[string]interface{}{"choice_id": "1"})
		require.Equal(t, http.StatusConflict, rrBatchReq.Code)
		captures["answer_409_batch_required"] = goldenCapture{
			Status: rrBatchReq.Code,
			Body:   normBatchReq.decodeBody(t, rrBatchReq.Body.Bytes()),
		}

		// 409 answer_set_changed (omitting second actionable row).
		normSetChanged := newGoldenNormalizer(f, dmKey)
		rrSetChanged := doRequestAsUser(t, f.srv, f.bob, http.MethodPost, "/api/v1/decision-prompts/"+resp.MessageID+"/answers", map[string]interface{}{
			"answers": []map[string]interface{}{
				{"decision_row_id": row0, "choice_id": "1"},
			},
		})
		require.Equal(t, http.StatusConflict, rrSetChanged.Code)
		captures["answer_set_409_changed"] = goldenCapture{
			Status: rrSetChanged.Code,
			Body:   normSetChanged.decodeBody(t, rrSetChanged.Body.Bytes()),
		}

		// 200 set answer + 2 chat.decision.updated events.
		normSetOK := newGoldenNormalizer(f, dmKey)
		dmCh, unsubDM := f.srv.events.Subscribe(dmSubject)
		rrSetOK := doRequestAsUser(t, f.srv, f.bob, http.MethodPost, "/api/v1/decision-prompts/"+resp.MessageID+"/answers", map[string]interface{}{
			"answers": []map[string]interface{}{
				{"decision_row_id": row0, "choice_id": "1"},
				{"decision_row_id": row1, "choice_id": "a"},
			},
		})
		require.Equal(t, http.StatusOK, rrSetOK.Code, rrSetOK.Body.String())
		evts := collectEvents(dmCh)
		unsubDM()
		require.Len(t, evts, 2)
		captures["answer_set_ok"] = goldenCapture{
			Status: rrSetOK.Code,
			Body:   normSetOK.decodeBody(t, rrSetOK.Body.Bytes()),
			Events: normSetOK.decodeEvents(t, dmSubject, evts),
		}
	}

	// 7. 409 too_many_pending_decisions.
	{
		f := newDecisionsFixture(t)
		dmKey, err := messages.DMConversationKey("agent", f.agent.ID, "user", f.bob.ID)
		require.NoError(t, err)
		for batch := 0; batch < 5; batch++ {
			decs := make([]messages.DecisionInput, 10)
			for i := 0; i < 10; i++ {
				decs[i] = messages.DecisionInput{
					ID:       fmt.Sprintf("P%d_%d", batch, i),
					Question: "Cap test?",
					Choices:  []messages.ChoiceInput{{Label: "yes"}, {Label: "no"}},
				}
			}
			rr := f.sendAsAgent(t, OutboundMessageRequest{
				Recipient: "user:" + f.bob.Email,
				Msg:       "cap fill",
				Decisions: decs,
			})
			require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		}
		normCap := newGoldenNormalizer(f, dmKey)
		rrCap := f.sendAsAgent(t, OutboundMessageRequest{
			Recipient: "user:" + f.bob.Email,
			Msg:       "one too many",
			Decisions: decisionInputs(t, `[{"id":"OVER","question":"Over cap?","choices":["yes","no"]}]`),
		})
		require.Equal(t, http.StatusConflict, rrCap.Code, rrCap.Body.String())
		captures["send_409_too_many_pending"] = goldenCapture{
			Status: rrCap.Code,
			Body:   normCap.decodeBody(t, rrCap.Body.Bytes()),
		}
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	require.NoError(t, enc.Encode(captures))
	gotBytes := buf.Bytes()

	if os.Getenv("UPDATE_DECISIONS_GOLDEN") == "1" {
		require.NoError(t, os.MkdirAll(filepath.Dir(decisionsLifecycleGoldenPath), 0o755))
		require.NoError(t, os.WriteFile(decisionsLifecycleGoldenPath, gotBytes, 0o644))
		return
	}

	wantBytes, err := os.ReadFile(decisionsLifecycleGoldenPath)
	require.NoError(t, err, "missing golden file %s; run with UPDATE_DECISIONS_GOLDEN=1 to generate", decisionsLifecycleGoldenPath)
	require.Equal(t, string(wantBytes), string(gotBytes), "decision lifecycle golden mismatch")
}
