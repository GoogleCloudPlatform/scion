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

package hub

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/managedagent"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pendingLogBuffer is a concurrency-safe sink for a JSON slog handler.
type pendingLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *pendingLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// records returns every JSON record whose msg equals msg, plus the raw line
// of each, in write order.
func (b *pendingLogBuffer) records(t *testing.T, msg string) ([]map[string]any, []string) {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	var recs []map[string]any
	var lines []string
	sc := bufio.NewScanner(bytes.NewReader(b.buf.Bytes()))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		var rec map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &rec), "log line is not JSON: %s", line)
		if rec["msg"] == msg {
			recs = append(recs, rec)
			lines = append(lines, line)
		}
	}
	require.NoError(t, sc.Err())
	return recs, lines
}

func newPendingLogCapture() (*slog.Logger, *pendingLogBuffer) {
	b := &pendingLogBuffer{}
	return slog.New(slog.NewJSONHandler(b, &slog.HandlerOptions{Level: slog.LevelDebug})), b
}

func pendingLogKeys(rec map[string]any) []string {
	keys := make([]string, 0, len(rec))
	for k := range rec {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// TestMessagePendingLog_AgentDM pins the entered-pending line written by
// ExecuteAgentDM when it persists the row "pending" (ptone/scion#4827): one
// line per message, with message_id, sender, recipient, type and
// creation_path, and nothing else — in particular never the body.
func TestMessagePendingLog_AgentDM(t *testing.T) {
	srv, _, _, sender, target, _, _ := deliverySetup(t)
	logger, logs := newPendingLogCapture()
	srv.messageLog = logger

	const body = "pending-log-body-must-not-appear-3f9c"
	result, dmErr := srv.ExecuteAgentDM(context.Background(), deliveryDMInput(sender, target, body))
	require.Nil(t, dmErr)
	require.Equal(t, AgentDMAccepted, result.Outcome)

	recs, lines := logs.records(t, store.MessageEnteredPendingLogMsg)
	require.Len(t, recs, 1, "exactly one entered-pending line per agent DM")
	rec := recs[0]
	assert.Equal(t, "INFO", rec["level"])
	assert.Equal(t, result.MessageID, rec["message_id"])
	assert.Equal(t, "agent:"+sender.Slug, rec["sender"])
	assert.Equal(t, "agent:"+target.Slug, rec["recipient"])
	assert.Equal(t, pendingPathAgentDM, rec["creation_path"])
	assert.Equal(t, messages.TypeInstruction, rec["type"])
	wantKeys := []string{"creation_path", "level", "message_id", "msg", "recipient", "sender", "time", "type"}
	assert.Equal(t, wantKeys, pendingLogKeys(rec), "the line carries only identifiers and routing fields")
	assert.NotContains(t, lines[0], body, "the message body must never be logged")
}

// TestMessagePendingLog_NotLoggedForOtherStates pins that the helper logs a
// row only when its stored state is pending.
func TestMessagePendingLog_NotLoggedForOtherStates(t *testing.T) {
	logger, logs := newPendingLogCapture()
	srv := &Server{messageLog: logger}
	for _, state := range []string{
		store.MessageDispatchDispatched, store.MessageDispatchDeferred,
		store.MessageDispatchFailed, store.MessageDispatchNoRecipient, "",
	} {
		srv.logMessageEnteredPending(pendingPathAgentDM, &store.Message{ID: uuid.NewString(), DispatchState: state})
	}
	srv.logMessageEnteredPending(pendingPathAgentDM, nil)
	recs, _ := logs.records(t, store.MessageEnteredPendingLogMsg)
	assert.Empty(t, recs)
}

// TestMessagePendingLog_TypeIsBounded pins that the line carries a known
// message type as is and maps any other value (the outbound DM type is caller
// free text) to messages.LogTypeOther.
func TestMessagePendingLog_TypeIsBounded(t *testing.T) {
	logger, logs := newPendingLogCapture()
	srv := &Server{messageLog: logger}
	for _, typ := range []string{messages.TypeMention, "caller free text 4827", ""} {
		srv.logMessageEnteredPending(pendingPathAgentDM, &store.Message{ID: uuid.NewString(), Type: typ, DispatchState: store.MessageDispatchPending})
	}
	recs, _ := logs.records(t, store.MessageEnteredPendingLogMsg)
	require.Len(t, recs, 3)
	assert.Equal(t, messages.TypeMention, recs[0]["type"], "a known type is logged as is")
	assert.Equal(t, messages.LogTypeOther, recs[1]["type"], "free text is logged as other")
	assert.Equal(t, messages.LogTypeOther, recs[2]["type"], "an empty type is logged as other")
}

// seedStuckPending creates n agent-addressed rows left pending well past the
// stuck threshold (but inside the expiry TTL), and returns their IDs.
func seedStuckPending(t *testing.T, cs store.Store, n int) []string {
	t.Helper()
	ctx := context.Background()
	proj := &store.Project{
		ID: uuid.NewString(), Name: "p", Slug: "p-" + uuid.NewString()[:8],
		OwnerID: uuid.NewString(),
	}
	require.NoError(t, cs.CreateProject(ctx, proj))
	ids := make([]string, 0, n)
	base := time.Now().Add(-time.Hour)
	for i := 0; i < n; i++ {
		m := &store.Message{
			ID: uuid.NewString(), ProjectID: proj.ID,
			Sender: "agent:s", Recipient: "agent:r", RecipientID: uuid.NewString(),
			Msg: "stuck", DispatchState: store.MessageDispatchPending,
			CreatedAt: base.Add(time.Duration(i) * time.Second),
		}
		require.NoError(t, cs.CreateMessage(ctx, m))
		ids = append(ids, m.ID)
	}
	return ids
}

func pendingLogIDs(t *testing.T, v any) []string {
	t.Helper()
	raw, ok := v.([]any)
	require.True(t, ok, "message_ids must be a JSON array, got %T", v)
	out := make([]string, 0, len(raw))
	for _, e := range raw {
		s, ok := e.(string)
		require.True(t, ok)
		out = append(out, s)
	}
	return out
}

// TestBrokerMessageSweep_StuckLineNamesMessageIDs pins that the stuck-pending
// sweep line names the stuck rows (ptone/scion#4827).
func TestBrokerMessageSweep_StuckLineNamesMessageIDs(t *testing.T) {
	srv, cs := newSweepTestServer(t)
	logger, logs := newPendingLogCapture()
	srv.agentLifecycleLog = logger
	ids := seedStuckPending(t, cs, 2)

	srv.brokerMessageSweepHandler()(context.Background())

	recs, _ := logs.records(t, "sweep: stuck pending messages detected")
	require.Len(t, recs, 1)
	assert.EqualValues(t, 2, recs[0]["count"])
	assert.Equal(t, ids, pendingLogIDs(t, recs[0]["message_ids"]), "oldest first")
	assert.Equal(t, false, recs[0]["message_ids_truncated"])
	assert.Equal(t, false, recs[0]["count_capped"])
}

// TestBrokerMessageSweep_StuckLineCapsMessageIDs pins the cap on the IDs the
// sweep line names: the oldest stuckMessageLogIDCap rows, with
// message_ids_truncated set and count still reporting every stuck row while
// that number is at most stuckMessageCountCap.
func TestBrokerMessageSweep_StuckLineCapsMessageIDs(t *testing.T) {
	srv, cs := newSweepTestServer(t)
	logger, logs := newPendingLogCapture()
	srv.agentLifecycleLog = logger
	require.Equal(t, 20, stuckMessageLogIDCap)
	ids := seedStuckPending(t, cs, stuckMessageLogIDCap+3)

	srv.brokerMessageSweepHandler()(context.Background())

	recs, lines := logs.records(t, "sweep: stuck pending messages detected")
	require.Len(t, recs, 1)
	assert.EqualValues(t, stuckMessageLogIDCap+3, recs[0]["count"])
	assert.Equal(t, ids[:stuckMessageLogIDCap], pendingLogIDs(t, recs[0]["message_ids"]))
	assert.Equal(t, true, recs[0]["message_ids_truncated"])
	for _, id := range ids[stuckMessageLogIDCap:] {
		assert.False(t, strings.Contains(lines[0], id), "IDs past the cap are not on the line")
	}
}

// TestBrokerMessageSweep_StuckCountIsBounded pins that the sweep reads at
// most stuckMessageCountCap+1 stuck rows rather than counting the whole
// backlog (ptone/scion#6648): past the cap the line and the gauge report the
// cap, with count_capped set.
func TestBrokerMessageSweep_StuckCountIsBounded(t *testing.T) {
	srv, cs := newSweepTestServer(t)
	logger, logs := newPendingLogCapture()
	srv.agentLifecycleLog = logger
	orig := stuckMessageCountCap
	stuckMessageCountCap = 3
	t.Cleanup(func() { stuckMessageCountCap = orig })
	ids := seedStuckPending(t, cs, 5)

	srv.brokerMessageSweepHandler()(context.Background())

	recs, _ := logs.records(t, "sweep: stuck pending messages detected")
	require.Len(t, recs, 1)
	assert.EqualValues(t, 3, recs[0]["count"])
	assert.Equal(t, true, recs[0]["count_capped"])
	assert.Equal(t, ids[:3], pendingLogIDs(t, recs[0]["message_ids"]), "the oldest counted rows")
	assert.Equal(t, true, recs[0]["message_ids_truncated"])
}

// cancelCallerDispatcher cancels the caller's context while the dispatch is
// in flight, the way a client that disconnects (or a sender agent that is
// being deleted) does, and then behaves like a transport that honors its
// context: a cancelled or expired context fails the call with a wrapped
// context error, as the control channel does.
type cancelCallerDispatcher struct {
	*recordingDispatcher
	cancel   context.CancelFunc
	deadline time.Time
	hasDL    bool
}

func (d *cancelCallerDispatcher) DispatchAgentMessage(ctx context.Context, agent *store.Agent, message string, interrupt bool, sm *messages.StructuredMessage) error {
	d.deadline, d.hasDL = ctx.Deadline()
	d.cancel()
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("control channel request failed: %w", err)
	}
	return d.recordingDispatcher.DispatchAgentMessage(ctx, agent, message, interrupt, sm)
}

// TestAgentDM_CallerCancelMidDispatch_RowNotLeftPending reproduces
// ptone/scion#4827: the caller's request was cancelled while the broker
// dispatch was in flight, the dispatch failed with "context canceled", and
// ExecuteAgentDM left the persisted row pending for the 24h TTL. The dispatch
// now runs detached from the caller's cancellation, so it completes and the
// row ends dispatched.
func TestAgentDM_CallerCancelMidDispatch_RowNotLeftPending(t *testing.T) {
	srv, s, _, sender, target, _, _ := deliverySetup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := &cancelCallerDispatcher{recordingDispatcher: &recordingDispatcher{}, cancel: cancel}
	srv.SetDispatcher(d)

	result, dmErr := srv.ExecuteAgentDM(ctx, deliveryDMInput(sender, target, "caller-cancel-4827"))
	require.Nil(t, dmErr)
	require.NotNil(t, result)
	assert.Equal(t, AgentDMAccepted, result.Outcome, "a caller cancel must not make the dispatch ambiguous")
	msg, err := s.GetMessage(context.Background(), result.MessageID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchDispatched, msg.DispatchState, "the row must not be left pending")
	assert.Len(t, d.getCalls(), 1, "the dispatch completed exactly once")
}

// TestAgentDM_DispatchKeepsCallerDeadline pins that the detached dispatch
// still honors the caller's deadline (the mention fan-out bounds each copy
// this way), so only a deadline can leave a row ambiguous.
func TestAgentDM_DispatchKeepsCallerDeadline(t *testing.T) {
	srv, _, _, sender, target, _, _ := deliverySetup(t)
	want := time.Now().Add(10 * time.Second)
	ctx, cancel := context.WithDeadline(context.Background(), want)
	defer cancel()
	d := &cancelCallerDispatcher{recordingDispatcher: &recordingDispatcher{}, cancel: func() {}}
	srv.SetDispatcher(d)

	_, dmErr := srv.ExecuteAgentDM(ctx, deliveryDMInput(sender, target, "caller-deadline-4827"))
	require.Nil(t, dmErr)
	require.True(t, d.hasDL, "the dispatch context must carry a deadline")
	assert.True(t, d.deadline.Equal(want), "the dispatch deadline must be the caller's (got %s, want %s)", d.deadline, want)
}

// TestMentionFanout_NotFoundLeavesNoPendingRow pins that an agent message
// mentioning a name that resolves to no agent (status not_found) writes no
// message row at all, so it cannot leave a pending one (ptone/scion#4827).
func TestMentionFanout_NotFoundLeavesNoPendingRow(t *testing.T) {
	srv, s, _, sender, target, _, _ := deliverySetup(t)
	ctx := context.Background()
	in := agentMentionFanoutInput{
		Sender:          sender,
		SenderIdent:     GetAgentIdentityFromContext(agentCtx(ctx, sender)),
		Primary:         target,
		Msg:             "cc @nobody-by-this-name-4827",
		Type:            messages.TypeInstruction,
		ParentMessageID: uuid.NewString(),
	}
	results := srv.fanOutAgentMentions(ctx, in)
	require.Len(t, results, 1)
	assert.Equal(t, "not_found", results[0].Status)
	n, err := s.CountStuckPendingMessages(ctx, time.Now().Add(time.Hour))
	require.NoError(t, err)
	assert.Equal(t, 0, n, "a not_found mention must leave no pending row")
	page, err := s.ListMessages(ctx, store.MessageFilter{AgentID: sender.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, page.Items, "a not_found mention writes no message row")
}

// TestDispatchContext_DetachesCancelKeepsCap pins dispatchContext: a parent
// that is already cancelled does not cancel the dispatch, and a parent with
// no deadline still gets the cap.
func TestDispatchContext_DetachesCancelKeepsCap(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	cancel()
	ctx, dcancel := dispatchContext(parent, time.Minute)
	defer dcancel()
	assert.NoError(t, ctx.Err(), "the caller's cancellation must not reach the dispatch")
	dl, ok := ctx.Deadline()
	require.True(t, ok, "the dispatch must be capped")
	assert.WithinDuration(t, time.Now().Add(time.Minute), dl, 5*time.Second)
}

// pendlogManagedBackend is a managed backend whose cancel can be slow and
// whose create can finish just as the send deadline expires.
type pendlogManagedBackend struct {
	// slowCancel makes CancelInteraction block until its context ends.
	slowCancel bool
	// createAtDeadline makes CreateInteraction succeed only once its
	// context has ended (the create landed as the deadline hit).
	createAtDeadline bool
	createCtxErr     error
	created          bool
	// cancelDeadline is the deadline CancelInteraction saw.
	cancelDeadline time.Time
	// createDeadline is the deadline CreateInteraction saw.
	createDeadline time.Time
	// calls records the order in which the cancel returned and the create ran.
	calls []string
}

func (b *pendlogManagedBackend) Name() string { return "pendlog-test" }
func (b *pendlogManagedBackend) CreateAgent(context.Context, managedagent.CreateAgentConfig) (string, error) {
	return "", errors.New("not used")
}
func (b *pendlogManagedBackend) DeleteAgent(context.Context, string) error { return nil }
func (b *pendlogManagedBackend) CreateInteraction(ctx context.Context, _ managedagent.InteractionRequest) (*managedagent.InteractionHandle, error) {
	b.createDeadline, _ = ctx.Deadline()
	b.calls = append(b.calls, "create")
	if b.createAtDeadline {
		<-ctx.Done()
	} else if err := ctx.Err(); err != nil {
		b.createCtxErr = err
		return nil, err
	}
	b.created = true
	return &managedagent.InteractionHandle{InteractionID: "interaction-4827"}, nil
}
func (b *pendlogManagedBackend) GetInteraction(context.Context, string) (*managedagent.InteractionState, error) {
	return nil, errors.New("not used")
}
func (b *pendlogManagedBackend) CancelInteraction(ctx context.Context, _ string) error {
	b.cancelDeadline, _ = ctx.Deadline()
	defer func() { b.calls = append(b.calls, "cancel-returned") }()
	if b.slowCancel {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}
func (b *pendlogManagedBackend) StreamInteraction(context.Context, string, string) (io.ReadCloser, error) {
	return nil, errors.New("not used")
}

// TestManagedAgentMessage_SlowCancelLeavesCreateItsBudget pins that the
// best-effort cancel before an interrupt send has its own short bound, so a
// cancel that hangs cannot spend the send budget and fail the create (which
// would leave the agent DM row pending).
func TestManagedAgentMessage_SlowCancelLeavesCreateItsBudget(t *testing.T) {
	srv, _, _, _, target, _, _ := deliverySetup(t)
	prevCancel := managedInterruptCancelTimeout
	managedInterruptCancelTimeout = 50 * time.Millisecond
	t.Cleanup(func() { managedInterruptCancelTimeout = prevCancel })
	b := &pendlogManagedBackend{slowCancel: true}
	useManagedBackend(t, b)
	if target.Annotations == nil {
		target.Annotations = map[string]string{}
	}
	target.Annotations[annotationInteractionID] = "running-4827"

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := srv.managedAgentMessage(ctx, target, "interrupt-4827", true)
	require.NoError(t, err, "a slow cancel must not fail the send")
	assert.True(t, b.created, "the create must run with budget left")
	assert.Equal(t, []string{"cancel-returned", "create"}, b.calls, "the create runs after the bounded cancel returned")
	assert.True(t, b.cancelDeadline.Before(b.createDeadline), "the cancel's deadline must come before the create's")
}

// TestManagedAgentMessage_AnnotationSurvivesDeadline pins that the
// interaction ID is recorded on a detached finalization context, so a create
// that finishes as the send deadline expires still chains the next one.
func TestManagedAgentMessage_AnnotationSurvivesDeadline(t *testing.T) {
	srv, s, _, _, target, _, _ := deliverySetup(t)
	b := &pendlogManagedBackend{createAtDeadline: true}
	useManagedBackend(t, b)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	require.NoError(t, srv.managedAgentMessage(ctx, target, "deadline-4827", false))
	got, err := s.GetAgent(context.Background(), target.ID)
	require.NoError(t, err)
	assert.Equal(t, "interaction-4827", got.Annotations[annotationInteractionID], "the interaction ID must be persisted")
}
