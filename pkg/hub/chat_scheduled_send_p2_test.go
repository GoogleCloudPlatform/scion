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

// Tests for scheduled send in native web chat, phase 2 (ptone/scion#3666):
// late cutoff, interrupted deliveries, send now, dismiss, retention,
// deletion with the conversation or sender, and audit records.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Store: phase 2 transitions (run on SQLite here and on Postgres in
// TestScheduledStore_Postgres)
// ---------------------------------------------------------------------------

func TestScheduledStore_SQLite_Phase2Transitions(t *testing.T) {
	sms, _ := openScheduledStorePair(t)
	testScheduledStorePhase2Transitions(t, sms[0])
}

// ---------------------------------------------------------------------------
// Late cutoff (missed) and restart with overdue rows
// ---------------------------------------------------------------------------

// After downtime, an overdue row is sent if it is at most an hour late.
func TestScheduledSend_OverdueWithinCutoff_Sent(t *testing.T) {
	f := newScheduledSendFixture(t)
	ctx := context.Background()
	m := f.insertScheduled(t, f.bob, "late-59", time.Now().Add(-59*time.Minute))

	assert.Equal(t, 1, f.srv.sweepScheduledMessages(ctx, time.Now().UTC()))
	row := f.row(t, f.bob, m.ID)
	assert.Equal(t, ScheduledMessageSent, row.Status)
	require.Len(t, f.topicMessages(t), 1)
}

// More than an hour late: failed as missed, nothing sent; Send now then
// delivers it through the full fire path.
func TestScheduledSend_OverdueBeyondCutoff_MissedThenSendNow(t *testing.T) {
	f := newScheduledSendFixture(t)
	ctx := context.Background()
	events, unsub := f.srv.events.Subscribe("user." + f.bob.ID + ".chat.scheduled")
	defer unsub()
	m := f.insertScheduled(t, f.bob, "late-61", time.Now().Add(-61*time.Minute))

	assert.Equal(t, 1, f.srv.sweepScheduledMessages(ctx, time.Now().UTC()))
	row := f.row(t, f.bob, m.ID)
	assert.Equal(t, ScheduledMessageFailed, row.Status)
	assert.Equal(t, ScheduledFailureMissed, row.FailureReason)
	assert.Empty(t, f.topicMessages(t), "a missed message is not sent")
	assert.Empty(t, f.dispatcher.getMessages())
	listed := f.list(t, f.bob)
	require.Len(t, listed, 1)
	assert.Equal(t, ScheduledFailureMissed, listed[0].FailureReason)

	rec := doRequestAsUser(t, f.srv, f.bob, http.MethodPost, f.rowPath(m.ID, "/send-now"), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp scheduledMessageResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, ScheduledMessagePending, resp.Status)
	assert.Empty(t, resp.FailureReason)
	assert.WithinDuration(t, time.Now(), resp.FireAt, 2*time.Second)

	time.Sleep(time.Until(resp.FireAt))
	assert.Equal(t, 1, f.srv.sweepScheduledMessages(ctx, time.Now().UTC()))
	row = f.row(t, f.bob, m.ID)
	assert.Equal(t, ScheduledMessageSent, row.Status)
	msgs := f.topicMessages(t)
	require.Len(t, msgs, 1)
	assert.Equal(t, f.bob.ID, msgs[0].SenderID)
	assert.Equal(t, []string{"sending", "failed", "requeued", "sending", "sent"}, scheduledEventActions(t, collectEvents(events)))
}

// Held while the experiment is off and turned back on later than the
// cutoff: missed, not sent.
func TestScheduledSend_HeldBeyondCutoff_Missed(t *testing.T) {
	f := newScheduledSendFixture(t)
	ctx := context.Background()
	m := f.insertScheduled(t, f.bob, "held", time.Now().Add(-2*time.Hour))
	setScheduledSendExperiment(t, f.srv, false)
	assert.Equal(t, 0, f.srv.sweepScheduledMessages(ctx, time.Now().UTC()))
	assert.Equal(t, ScheduledMessagePending, f.row(t, f.bob, m.ID).Status)
	setScheduledSendExperiment(t, f.srv, true)
	assert.Equal(t, 1, f.srv.sweepScheduledMessages(ctx, time.Now().UTC()))
	row := f.row(t, f.bob, m.ID)
	assert.Equal(t, ScheduledMessageFailed, row.Status)
	assert.Equal(t, ScheduledFailureMissed, row.FailureReason)
	assert.Empty(t, f.topicMessages(t))
}

func TestScheduledSend_Missed(t *testing.T) {
	now := time.Now()
	assert.False(t, scheduledMissed(&ScheduledChatMessage{FireAt: now.Add(-scheduledLateCutoff)}, now))
	assert.True(t, scheduledMissed(&ScheduledChatMessage{FireAt: now.Add(-scheduledLateCutoff - time.Second)}, now))
	assert.False(t, scheduledMissed(&ScheduledChatMessage{FireAt: now.Add(time.Hour)}, now))
	assert.Equal(t, 60*time.Minute, scheduledLateCutoff)
}

// ---------------------------------------------------------------------------
// Interrupted deliveries
// ---------------------------------------------------------------------------

// A replica that stopped after claiming a row leaves it in sending. Upkeep
// marks it failed/interrupted once no delivery can still be running; it is
// never sent again on its own. Send now sends it.
func TestScheduledSend_KillMidSend_InterruptedNotResent(t *testing.T) {
	f := newScheduledSendFixture(t)
	ctx := context.Background()
	events, unsub := f.srv.events.Subscribe("user." + f.bob.ID + ".chat.scheduled")
	defer unsub()
	now := time.Now().UTC()
	m := f.insertScheduled(t, f.bob, "killed", now.Add(-10*time.Minute))
	recent := f.insertScheduled(t, f.alice, "in-delivery", now.Add(-time.Minute))
	// The killed replica claimed it long ago; another delivery is in progress.
	ok, err := f.sms.ClaimScheduledMessage(ctx, m.ID, now.Add(-scheduledStuckAfter()-time.Second))
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = f.sms.ClaimScheduledMessage(ctx, recent.ID, now.Add(-scheduledStuckAfter()+time.Minute))
	require.NoError(t, err)
	require.True(t, ok)

	f.srv.scheduledUpkeep(ctx, now)
	row := f.row(t, f.bob, m.ID)
	assert.Equal(t, ScheduledMessageFailed, row.Status)
	assert.Equal(t, ScheduledFailureInterrupted, row.FailureReason)
	assert.Equal(t, ScheduledMessageSending, f.row(t, f.alice, recent.ID).Status, "a delivery that may still run is left alone")
	assert.Equal(t, []string{"failed"}, scheduledEventActions(t, collectEvents(events)))

	// Never sent again by the sweeper.
	assert.Equal(t, 0, f.srv.sweepScheduledMessages(ctx, now.Add(time.Minute)))
	f.srv.scheduledUpkeep(ctx, now.Add(time.Minute))
	assert.Empty(t, f.topicMessages(t))
	assert.Equal(t, ScheduledMessageFailed, f.row(t, f.bob, m.ID).Status)

	rec := doRequestAsUser(t, f.srv, f.bob, http.MethodPost, f.rowPath(m.ID, "/send-now"), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	time.Sleep(time.Until(f.row(t, f.bob, m.ID).FireAt))
	assert.Equal(t, 1, f.srv.sweepScheduledMessages(ctx, time.Now().UTC()))
	assert.Equal(t, ScheduledMessageSent, f.row(t, f.bob, m.ID).Status)
	assert.Len(t, f.topicMessages(t), 1)
}

// No delivery still in progress can be marked interrupted: the threshold
// is longer than the delivery bound plus the final state write.
func TestScheduledSend_StuckThresholdExceedsDelivery(t *testing.T) {
	assert.Greater(t, scheduledStuckAfter(), scheduledClaimTimeout+scheduledDeliveryBudget+scheduledFinalizeTimeout)
}

// A delivery that outlived the stuck threshold (its row was marked
// interrupted, then sent now and claimed again) cannot finalize the new
// claim: the row keeps the new claim and no sent outcome is recorded or
// published for it.
func TestScheduledSend_StaleDeliveryDoesNotFinalizeNewClaim(t *testing.T) {
	f := newScheduledSendFixture(t)
	ctx := context.Background()
	rec := &recordingScheduledAuditor{}
	f.srv.SetAuditLogger(rec)
	now := time.Now().UTC()
	m := f.insertScheduled(t, f.bob, "stale", now.Add(-time.Minute))
	firstClaim := now.Add(-scheduledStuckAfter() - time.Second)
	mustApply(t)(f.sms.ClaimScheduledMessage(ctx, m.ID, firstClaim))
	stale := f.row(t, f.bob, m.ID) // what the slow delivery holds

	f.srv.scheduledUpkeep(ctx, now)
	require.Equal(t, ScheduledFailureInterrupted, f.row(t, f.bob, m.ID).FailureReason)
	r := doRequestAsUser(t, f.srv, f.bob, http.MethodPost, f.rowPath(m.ID, "/send-now"), nil)
	require.Equal(t, http.StatusOK, r.Code, r.Body.String())
	mustApply(t)(f.sms.ClaimScheduledMessage(ctx, m.ID, now))

	events, unsub := f.srv.events.Subscribe("user." + f.bob.ID + ".chat.scheduled")
	defer unsub()
	f.srv.fireScheduledMessage(ctx, f.sms, stale)
	row := f.row(t, f.bob, m.ID)
	assert.Equal(t, ScheduledMessageSending, row.Status, "the new claim is untouched")
	assert.Empty(t, row.MessageID)
	assert.NotContains(t, scheduledEventActions(t, collectEvents(events)), "sent")
	assert.NotContains(t, rec.actions(), "fire:sent:")
}

// ---------------------------------------------------------------------------
// Send now
// ---------------------------------------------------------------------------

// Send now is treated as a new schedule: same caller rules, same live
// conversation access check, same pending cap; only missed and
// interrupted rows qualify; another user's row is not found.
func TestScheduledSend_SendNowChecks(t *testing.T) {
	f := newScheduledSendFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()
	missed := f.insertScheduled(t, f.bob, "sn-missed", now.Add(-2*time.Hour))
	failRow(t, f.sms, missed.ID, ScheduledFailureMissed, now)
	noAccess := f.insertScheduled(t, f.bob, "sn-no-access", now.Add(-time.Minute))
	failRow(t, f.sms, noAccess.ID, ScheduledFailureNoAccess, now)
	pending := f.insertScheduled(t, f.bob, "sn-pending", now.Add(time.Hour))

	// Another user's row: 404, unchanged.
	rec := doRequestAsUser(t, f.srv, f.alice, http.MethodPost, f.rowPath(missed.ID, "/send-now"), nil)
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	assert.Equal(t, ScheduledMessageFailed, f.row(t, f.bob, missed.ID).Status)

	// Not missed or interrupted: 409.
	for _, id := range []string{noAccess.ID, pending.ID} {
		rec = doRequestAsUser(t, f.srv, f.bob, http.MethodPost, f.rowPath(id, "/send-now"), nil)
		assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	}

	// Wrong method.
	rec = doRequestAsUser(t, f.srv, f.bob, http.MethodGet, f.rowPath(missed.ID, "/send-now"), nil)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)

	// Scoped token: refused like create.
	bob := NewAuthenticatedUser(f.bob.ID, f.bob.Email, f.bob.DisplayName, f.bob.Role, string(ClientTypeWeb))
	scoped := NewScopedUserIdentity(bob, f.project.ID, []string{"project:read"})
	rec = doRequestAsIdentity(t, f.srv, scoped, http.MethodPost, f.rowPath(missed.ID, "/send-now"), nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	// Pending cap: 50 active already.
	for i := 0; i < scheduledMaxActivePerSender-1; i++ {
		f.insertScheduled(t, f.bob, fmt.Sprintf("sn-cap-%d", i), now.Add(time.Hour))
	}
	rec = doRequestAsUser(t, f.srv, f.bob, http.MethodPost, f.rowPath(missed.ID, "/send-now"), nil)
	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), ErrCodeScheduledLimit)
	ok, err := f.sms.CancelScheduledMessage(ctx, f.bob.ID, pending.ID, now)
	require.NoError(t, err)
	require.True(t, ok)

	// Lost access: refused by the live check, row unchanged.
	membersGroup, err := f.store.GetGroupBySlug(ctx, "project:"+f.project.Slug+":members")
	require.NoError(t, err)
	require.NoError(t, f.store.RemoveGroupMember(ctx, membersGroup.ID, store.GroupMemberTypeUser, f.bob.ID))
	_, err = f.store.DeleteRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, f.bob.ID)
	require.NoError(t, err)
	rec = doRequestAsUser(t, f.srv, f.bob, http.MethodPost, f.rowPath(missed.ID, "/send-now"), nil)
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	requireLiveSendAnswer(t, f.srv, f.bob, f.topicID, rec)
	assert.Equal(t, ScheduledMessageFailed, f.row(t, f.bob, missed.ID).Status)
}

// After Send now, the fire-time checks run again: access lost between Send
// now and delivery fails the row as no_access, nothing sent.
func TestScheduledSend_SendNowRunsFireChecks(t *testing.T) {
	f := newScheduledSendFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()
	m := f.insertScheduled(t, f.bob, "sn-fire", now.Add(-2*time.Hour))
	failRow(t, f.sms, m.ID, ScheduledFailureMissed, now)
	rec := doRequestAsUser(t, f.srv, f.bob, http.MethodPost, f.rowPath(m.ID, "/send-now"), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	u, err := f.store.GetUser(ctx, f.bob.ID)
	require.NoError(t, err)
	u.Status = store.UserStatusSuspended
	require.NoError(t, f.store.UpdateUser(ctx, u))

	time.Sleep(time.Until(f.row(t, f.bob, m.ID).FireAt))
	assert.Equal(t, 1, f.srv.sweepScheduledMessages(ctx, time.Now().UTC()))
	row := f.row(t, f.bob, m.ID)
	assert.Equal(t, ScheduledMessageFailed, row.Status)
	assert.Equal(t, ScheduledFailureSenderInactive, row.FailureReason)
	assert.Empty(t, f.topicMessages(t))
}

// ---------------------------------------------------------------------------
// Dismiss
// ---------------------------------------------------------------------------

func TestScheduledSend_Dismiss(t *testing.T) {
	f := newScheduledSendFixture(t)
	events, unsub := f.srv.events.Subscribe("user." + f.bob.ID + ".chat.scheduled")
	defer unsub()
	now := time.Now().UTC()
	failed := f.insertScheduled(t, f.bob, "dismiss-failed", now.Add(-time.Minute))
	failRow(t, f.sms, failed.ID, ScheduledFailureNoAccess, now)
	pending := f.insertScheduled(t, f.bob, "dismiss-pending", now.Add(time.Hour))

	rec := doRequestAsUser(t, f.srv, f.alice, http.MethodPost, f.rowPath(failed.ID, "/dismiss"), nil)
	assert.Equal(t, http.StatusNotFound, rec.Code, "another user's row")
	rec = doRequestAsUser(t, f.srv, f.bob, http.MethodPost, f.rowPath(pending.ID, "/dismiss"), nil)
	assert.Equal(t, http.StatusConflict, rec.Code, "a pending row is cancelled, not dismissed")
	rec = doRequestAsUser(t, f.srv, f.bob, http.MethodDelete, f.rowPath(failed.ID, "/dismiss"), nil)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)

	rec = doRequestAsUser(t, f.srv, f.bob, http.MethodPost, f.rowPath(failed.ID, "/dismiss"), nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.Equal(t, ScheduledMessageCancelled, f.row(t, f.bob, failed.ID).Status)
	for _, m := range f.list(t, f.bob) {
		assert.NotEqual(t, failed.ID, m.ID, "no longer listed")
	}
	rec = doRequestAsUser(t, f.srv, f.bob, http.MethodPost, f.rowPath(failed.ID, "/dismiss"), nil)
	assert.Equal(t, http.StatusNoContent, rec.Code, "dismissing again is a no-op")
	assert.Equal(t, []string{"dismissed"}, scheduledEventActions(t, collectEvents(events)))
	assert.Empty(t, f.topicMessages(t))
}

// The new routes are behind the experiment like the others.
func TestScheduledSend_Phase2RoutesExperimentOff(t *testing.T) {
	f := newScheduledSendFixture(t)
	now := time.Now().UTC()
	m := f.insertScheduled(t, f.bob, "off", now.Add(-2*time.Hour))
	failRow(t, f.sms, m.ID, ScheduledFailureMissed, now)
	setScheduledSendExperiment(t, f.srv, false)
	for _, suffix := range []string{"/send-now", "/dismiss"} {
		rec := doRequestAsUser(t, f.srv, f.bob, http.MethodPost, f.rowPath(m.ID, suffix), nil)
		assert.Equal(t, http.StatusNotFound, rec.Code, suffix)
	}
	assert.Equal(t, ScheduledMessageFailed, f.row(t, f.bob, m.ID).Status)
}

// ---------------------------------------------------------------------------
// Retention and deletion
// ---------------------------------------------------------------------------

func TestScheduledSend_UpkeepPurgesOldRows(t *testing.T) {
	f := newScheduledSendFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()
	old := f.insertScheduled(t, f.bob, "purge-old", now.Add(-9*24*time.Hour))
	ok, err := f.sms.ClaimScheduledMessage(ctx, old.ID, now.Add(-8*24*time.Hour))
	require.NoError(t, err)
	require.True(t, ok)
	mustApply(t)(f.sms.MarkScheduledMessageSent(ctx, old.ID, "m-old", now.Add(-8*24*time.Hour), now.Add(-8*24*time.Hour)))

	f.srv.scheduledUpkeep(ctx, now)
	got, err := f.sms.GetScheduledMessage(ctx, f.bob.ID, old.ID)
	require.NoError(t, err)
	assert.Nil(t, got, "purged on the first pass")

	// The next purge waits for the purge interval.
	second := f.insertScheduled(t, f.bob, "purge-second", now.Add(-9*24*time.Hour))
	require.NoError(t, func() error {
		ok, err := f.sms.CancelScheduledMessage(ctx, f.bob.ID, second.ID, now.Add(-8*24*time.Hour))
		if err == nil && !ok {
			err = fmt.Errorf("not cancelled")
		}
		return err
	}())
	f.srv.scheduledUpkeep(ctx, now.Add(time.Minute))
	got, err = f.sms.GetScheduledMessage(ctx, f.bob.ID, second.ID)
	require.NoError(t, err)
	assert.NotNil(t, got, "not purged again within the interval")
	f.srv.scheduledUpkeep(ctx, now.Add(scheduledPurgeInterval))
	got, err = f.sms.GetScheduledMessage(ctx, f.bob.ID, second.ID)
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestScheduledSend_TopicDelete_DeletesScheduled(t *testing.T) {
	f := newScheduledSendFixture(t)
	ctx := context.Background()
	// A topic can be deleted only while the project has another.
	require.NoError(t, f.wcs.CreateTopic(ctx, WebChatTopic{
		ID: tid("sched-topic-2"), ProjectID: f.project.ID, Name: "other", CreatedBy: f.alice.ID, CreatedAt: time.Now().UTC(),
	}))
	m := f.schedule(t, f.bob, "doomed", time.Now().Add(time.Hour))
	a := f.schedule(t, f.alice, "doomed too", time.Now().Add(time.Hour))

	rec := doRequestAsUser(t, f.srv, f.bob, http.MethodDelete, "/api/v1/chat/topics/"+f.topicID, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	for _, c := range []struct {
		user *store.User
		id   string
	}{{f.bob, m.ID}, {f.alice, a.ID}} {
		got, err := f.sms.GetScheduledMessage(ctx, c.user.ID, c.id)
		require.NoError(t, err)
		assert.Nil(t, got)
	}
}

func TestScheduledSend_UserDelete_DeletesScheduled(t *testing.T) {
	f := newScheduledSendFixture(t)
	ctx := context.Background()
	m := f.schedule(t, f.bob, "from a deleted user", time.Now().Add(time.Hour))
	a := f.schedule(t, f.alice, "from alice", time.Now().Add(time.Hour))
	// The post-delete cleanup both user delete paths run.
	f.srv.removeUserScopedData(ctx, f.bob.ID)
	got, err := f.sms.GetScheduledMessage(ctx, f.bob.ID, m.ID)
	require.NoError(t, err)
	assert.Nil(t, got)
	got, err = f.sms.GetScheduledMessage(ctx, f.alice.ID, a.ID)
	require.NoError(t, err)
	assert.NotNil(t, got, "other users' messages are kept")
}

// ---------------------------------------------------------------------------
// Audit
// ---------------------------------------------------------------------------

type recordingScheduledAuditor struct {
	plainAuditLogger
	mu     sync.Mutex
	events []ChatScheduledMessageEvent
}

func (r *recordingScheduledAuditor) LogChatScheduledMessageEvent(_ context.Context, e *ChatScheduledMessageEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, *e)
	return nil
}

func (r *recordingScheduledAuditor) actions() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, e := range r.events {
		out = append(out, e.Action+":"+e.Status+":"+e.FailureReason)
	}
	return out
}

// Every change is recorded through the hub audit logger with the sender as
// principal and scheduled-send as executor, without the message text.
func TestScheduledSend_AuditThroughAuditLogger(t *testing.T) {
	f := newScheduledSendFixture(t)
	ctx := context.Background()
	rec := &recordingScheduledAuditor{}
	f.srv.SetAuditLogger(rec)

	cancelled := f.schedule(t, f.bob, "audit cancel secret-text", time.Now().Add(time.Hour))
	r := doRequestAsUser(t, f.srv, f.bob, http.MethodDelete, f.rowPath(cancelled.ID, ""), nil)
	require.Equal(t, http.StatusNoContent, r.Code, r.Body.String())

	missed := f.insertScheduled(t, f.bob, "audit-missed", time.Now().Add(-2*time.Hour))
	assert.Equal(t, 1, f.srv.sweepScheduledMessages(ctx, time.Now().UTC()))
	r = doRequestAsUser(t, f.srv, f.bob, http.MethodPost, f.rowPath(missed.ID, "/send-now"), nil)
	require.Equal(t, http.StatusOK, r.Code, r.Body.String())
	time.Sleep(time.Until(f.row(t, f.bob, missed.ID).FireAt))
	assert.Equal(t, 1, f.srv.sweepScheduledMessages(ctx, time.Now().UTC()))

	stuck := f.insertScheduled(t, f.bob, "audit-stuck", time.Now().Add(-10*time.Minute))
	ok, err := f.sms.ClaimScheduledMessage(ctx, stuck.ID, time.Now().Add(-scheduledStuckAfter()-time.Minute))
	require.NoError(t, err)
	require.True(t, ok)
	f.srv.scheduledUpkeep(ctx, time.Now().UTC())
	r = doRequestAsUser(t, f.srv, f.bob, http.MethodPost, f.rowPath(stuck.ID, "/dismiss"), nil)
	require.Equal(t, http.StatusNoContent, r.Code, r.Body.String())

	assert.Equal(t, []string{
		"create:pending:",
		"cancel:cancelled:",
		"fire:failed:missed",
		"send_now:pending:",
		"fire:sent:",
		"interrupted:failed:interrupted",
		"dismiss:cancelled:interrupted",
	}, rec.actions())
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, e := range rec.events {
		assert.Equal(t, f.bob.ID, e.SenderUserID)
		assert.Equal(t, scheduledSendClientType, e.Executor)
		assert.Equal(t, "scheduled_message:"+e.ScheduledMessageID, e.ExecutorID)
		assert.Equal(t, f.topicID, e.ConversationKey)
		assert.False(t, strings.Contains(fmt.Sprintf("%+v", e), "secret-text"), "no message text in the audit record")
	}
	assert.NotEmpty(t, rec.events[0].CredentialKind, "request-driven actions carry the credential kind")
}

// Without an audit logger that records scheduled-message events, the
// record goes to the structured log with the same fields and no text.
func TestScheduledSend_AuditFallbackToStructuredLog(t *testing.T) {
	f := newScheduledSendFixture(t)
	f.srv.SetAuditLogger(plainAuditLogger{})
	logs := captureSlogDefault(t)
	sm := f.schedule(t, f.bob, "fallback secret-text", time.Now().Add(time.Hour))
	out := logs.String()
	assert.Contains(t, out, "audit_action=chat.scheduled.create")
	assert.Contains(t, out, "principal_id="+f.bob.ID)
	assert.Contains(t, out, "scheduled_message_id="+sm.ID)
	assert.Contains(t, out, "executor=scheduled-send")
	assert.NotContains(t, out, "secret-text")
}

// Send now spends the sender's chat send allowance like a new schedule.
func TestScheduledSend_SendNowRateLimited(t *testing.T) {
	f := newScheduledSendFixture(t)
	now := time.Now().UTC()
	m := f.insertScheduled(t, f.bob, "rl", now.Add(-2*time.Hour))
	failRow(t, f.sms, m.ID, ScheduledFailureMissed, now)
	fakeNow := time.Now()
	f.srv.chatSendLimiter = newChatSendLimiterWithRates(map[chatSenderClass]float64{
		chatSenderHuman: 1,
		chatSenderAgent: chatSendAgentRatePerMinute,
	}, func() time.Time { return fakeNow })
	require.True(t, f.srv.chatSendLimiter.Allow(f.bob.ID, chatSenderHuman).Allowed, "spend the only send")

	rec := doRequestAsUser(t, f.srv, f.bob, http.MethodPost, f.rowPath(m.ID, "/send-now"), nil)
	assert.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
	row := f.row(t, f.bob, m.ID)
	assert.Equal(t, ScheduledMessageFailed, row.Status)
	assert.Equal(t, ScheduledFailureMissed, row.FailureReason)
}

func TestScheduledStore_SQLite_ConversationIndex(t *testing.T) {
	_, dsn := openScheduledStorePair(t)
	db, err := sql.Open("sqlite3", dsn)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = ?`,
		"idx_webchat_scheduled_message_conversation").Scan(&n))
	assert.Equal(t, 1, n)
}
