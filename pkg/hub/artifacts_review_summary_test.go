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
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/plaintext"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Review summary notifications (ptone/scion#6853).

const (
	summaryArtifactID = "6a2b3c4d-0000-4000-8000-000000000001"
	summaryProjectID  = "6a2b3c4d-0000-4000-8000-0000000000aa"
	// otherArtifactID and otherProjectID appear only inside the hostile
	// note; no field may take them.
	otherArtifactID = "deadbeef-0000-4000-8000-000000000666"
	otherProjectID  = "deadbeef-0000-4000-8000-000000000777"
)

// hostileReviewNote holds, in one note: ESC and CSI sequences, a C1
// control, bidi overrides and isolates, zero-width characters and a BOM,
// NUL and BEL, every kind of line break, Markdown and HTML, mention tokens,
// a URL, resource-like strings naming another artifact and project, invalid
// UTF-8, and more text than the excerpt cap.
var hostileReviewNote = "\x1b[31mRED\x1b[0m \x1b]8;;https://evil.example/\x07click\x1b]8;;\x07 \u009b2J" +
	"\u202egnp.exe\u202c \u2066iso\u2069 zero\u200bwidth\ufeff nul\x00bel\x07\r\n" +
	"line2\u2028line3\u2029line4\u0085line5\t\v\f " +
	"**bold** [link](https://evil.example/a) <img src=x onerror=alert(1)> <script>x</script> " +
	"@channel <!channel> <@U012345> @here " +
	"https://evil.example/projects/" + otherProjectID + "/artifacts/" + otherArtifactID + " " +
	"scion://artifact/" + otherArtifactID + " /projects/" + otherProjectID + "/artifacts/" + otherArtifactID + " " +
	"bad\xff\xfeutf8 " + strings.Repeat("filler ", 80) + "TAIL-NOT-SHOWN"

// hostileReviewExcerpt is the Excerpt built from hostileReviewNote. The
// web tray test (notification-tray-review-summary.test.ts) renders this
// exact string, so the two stay on one fixture.
const hostileReviewExcerpt = "[31mRED[0m ]8;;https://evil.example/click]8;; 2Jgnp.exe iso zerowidth nulbel line2 line3 " +
	"line4 line5 **bold** [link](https://evil.example/a) <img src=x onerror=alert(1)> " +
	"<script>x</script> @channel <!channel> <@U012345> @here https://evil.…"

// assertCleanText fails when s holds a rune cleanNotificationText must
// remove, or more than maxRunes runes.
func assertCleanText(t *testing.T, s string, maxRunes int) {
	t.Helper()
	assert.True(t, utf8.ValidString(s), "valid UTF-8: %q", s)
	assert.LessOrEqual(t, utf8.RuneCountInString(s), maxRunes, "%q", s)
	for _, r := range s {
		if r != ' ' && (unicode.IsSpace(r) || isDisplayUnsafeRune(r)) {
			t.Errorf("rune %U left in %q", r, s)
		}
	}
	assert.NotContains(t, s, "  ", "whitespace collapsed")
	assert.Equal(t, strings.TrimSpace(s), s, "trimmed")
}

// TestCleanNotificationText_UsesPlaintextClean pins the hub's strip to
// plaintext.Clean, whose own tests (pkg/util/plaintext) hold the table,
// the every-code-point kill test and the mutant set.
func TestCleanNotificationText_UsesPlaintextClean(t *testing.T) {
	for _, in := range []string{hostileReviewNote, "", "  a\u2028b\u202ec  ", strings.Repeat("x", 300)} {
		assert.Equal(t, plaintext.Clean(in, reviewExcerptMaxRunes), cleanNotificationText(in, reviewExcerptMaxRunes))
	}
	assert.Equal(t, hostileReviewExcerpt, cleanNotificationText(hostileReviewNote, reviewExcerptMaxRunes))
}

func summaryNotice(note string) artifacts.ReviewNotice {
	return artifacts.ReviewNotice{
		ArtifactID: summaryArtifactID, Seq: 3,
		ScopeKind: artifacts.ScopeKindProject, ScopeRef: summaryProjectID,
		OwnerKind: artifacts.PrincipalKindUser, OwnerRef: "owner",
		ReviewerKind: artifacts.PrincipalKindUser, ReviewerRef: "reviewer",
		Note: note, SubmitAuthorized: true,
	}
}

// TestBuildReviewSummaryNotification_HostileNote: a hostile note comes out
// stripped, single-line and capped, in the Excerpt alone. The message is
// the hub's own text naming the reviewer, and the resource fields and the
// project come from the notice, never from the note.
func TestBuildReviewSummaryNotification_HostileNote(t *testing.T) {
	n := buildReviewSummaryNotification(summaryNotice(hostileReviewNote), "owner-id", "Alice")

	assertCleanText(t, n.Excerpt, reviewExcerptMaxRunes)
	assert.Equal(t, hostileReviewExcerpt, n.Excerpt)
	assert.True(t, strings.HasSuffix(n.Excerpt, "…"), "an over-long note is cut with an ellipsis: %q", n.Excerpt)
	assert.NotContains(t, n.Excerpt, "TAIL-NOT-SHOWN")
	assert.True(t, strings.HasPrefix(n.Excerpt, "[31mRED[0m "), "%q", n.Excerpt)
	assert.Contains(t, n.Excerpt, "line2 line3 line4 line5", "line breaks become single spaces")
	assert.Contains(t, n.Excerpt, "<img src=x onerror=alert(1)>", "markup is kept as literal text; clients render it as text")
	assert.Contains(t, n.Excerpt, "@channel <!channel> <@U012345> @here", "mentions are kept as literal text")

	assert.Equal(t, "Review summary from Alice (v3 of your artifact):", n.Message)
	assert.Equal(t, store.NotificationKindArtifactReviewSummary, n.Kind)
	assert.Equal(t, store.NotificationStatusArtifactReviewSubmitted, n.Status)
	assert.Equal(t, store.SubscriberTypeUser, n.SubscriberType)
	assert.Equal(t, "owner-id", n.SubscriberID)
	assert.Equal(t, store.NotificationResourceArtifact, n.ResourceType)
	assert.Equal(t, summaryArtifactID, n.ResourceID)
	assert.Equal(t, summaryProjectID, n.ProjectID)
	assert.Equal(t, store.CreatorFallbackSubscriptionID, n.AgentID)
	assert.Equal(t, store.CreatorFallbackSubscriptionID, n.SubscriptionID)
}

// TestBuildReviewSummaryNotification_ResourceFieldsIgnoreNote is the
// resource-field immunity test: whatever URL or resource-like string the
// note holds, every field but Excerpt is the same as for a plain note.
func TestBuildReviewSummaryNotification_ResourceFieldsIgnoreNote(t *testing.T) {
	plain := buildReviewSummaryNotification(summaryNotice("Looks good."), "owner-id", "Alice")
	for _, note := range []string{
		hostileReviewNote,
		"scion://artifact/" + otherArtifactID,
		"/projects/" + otherProjectID + "/artifacts/" + otherArtifactID + "/v/9",
		"https://evil.example/projects/" + otherProjectID + "/artifacts/" + otherArtifactID,
		`{"resourceType":"agent","resourceId":"` + otherArtifactID + `","projectId":"` + otherProjectID + `"}`,
		"resourceId=" + otherArtifactID + "&kind=artifact_grant",
	} {
		n := buildReviewSummaryNotification(summaryNotice(note), "owner-id", "Alice")
		assert.Equal(t, plain.ResourceType, n.ResourceType, note)
		assert.Equal(t, plain.ResourceID, n.ResourceID, note)
		assert.Equal(t, plain.ProjectID, n.ProjectID, note)
		assert.Equal(t, plain.AgentID, n.AgentID, note)
		assert.Equal(t, plain.Kind, n.Kind, note)
		assert.Equal(t, plain.Status, n.Status, note)
		assert.Equal(t, plain.Message, n.Message, note)
		assert.NotContains(t, n.Message, otherArtifactID, note)
	}
}

func TestBuildReviewSummaryNotification_Shapes(t *testing.T) {
	n := buildReviewSummaryNotification(summaryNotice(" \n\u200b "), "owner-id", "an agent")
	assert.Empty(t, n.Excerpt, "a note that cleans to nothing gives no excerpt")
	assert.Equal(t, "Review (v3) of your artifact submitted by an agent.", n.Message)

	notice := summaryNotice("ok")
	notice.ScopeKind = artifacts.ScopeKindMessage
	notice.ScopeRef = "msg-1"
	assert.Equal(t, store.CreatorFallbackSubscriptionID, buildReviewSummaryNotification(notice, "o", "A").ProjectID,
		"an artifact not homed in a project names no project")
	notice = summaryNotice("ok")
	notice.ScopeRef = strings.ToUpper(summaryProjectID)
	assert.Equal(t, store.CreatorFallbackSubscriptionID, buildReviewSummaryNotification(notice, "o", "A").ProjectID,
		"a project ref that is not a canonical UUID is not used")
}

// summaryFixture is a server with a user owner, a reviewer whose display
// name is hostile, and a third user, all on the real store, with every
// published event captured.
type summaryFixture struct {
	srv                    *Server
	st                     store.Store
	owner, reviewer, other *store.User
	all                    <-chan Event
	dispatcher             *recordingDispatcher
}

func newSummaryFixture(t *testing.T) *summaryFixture {
	t.Helper()
	srv, st := testServer(t)
	f := &summaryFixture{srv: srv, st: st, dispatcher: &recordingDispatcher{}}
	srv.SetDispatcher(f.dispatcher)
	f.owner = createTestUser(t, st, "rs-owner", "rs-owner@test.com")
	f.other = createTestUser(t, st, "rs-other", "rs-other@test.com")
	f.reviewer = &store.User{ID: tid("rs-reviewer"), Email: "rs-reviewer@test.com",
		DisplayName: "\u202eMallory\x1b[2J\nAdmin", Role: store.UserRoleMember, Status: "active", Created: time.Now()}
	require.NoError(t, st.CreateUser(context.Background(), f.reviewer))
	pub := NewChannelEventPublisher()
	t.Cleanup(pub.Close)
	all, unsub := pub.Subscribe(">")
	t.Cleanup(unsub)
	srv.events = pub
	f.all = all
	return f
}

func (f *summaryFixture) notice(note string) artifacts.ReviewNotice {
	n := summaryNotice(note)
	n.OwnerRef = f.owner.ID
	n.ReviewerRef = f.reviewer.ID
	return n
}

func (f *summaryFixture) notifications(t *testing.T, u *store.User) []store.Notification {
	t.Helper()
	list, err := f.st.GetNotifications(context.Background(), store.SubscriberTypeUser, u.ID, false)
	require.NoError(t, err)
	return list
}

// awaitNotificationEvents waits up to 5 seconds for n notification events
// and returns them.
func (f *summaryFixture) awaitNotificationEvents(t *testing.T, n int) []Event {
	t.Helper()
	var out []Event
	require.Eventually(t, func() bool {
		out = append(out, f.published()...)
		return len(out) >= n
	}, 5*time.Second, 5*time.Millisecond)
	require.Len(t, out, n)
	return out
}

// published drains every event published so far and returns the
// notification events among them: the broadcast notification.* subjects
// and every *.notification subject (user and project).
func (f *summaryFixture) published() []Event {
	var out []Event
	for {
		select {
		case e := <-f.all:
			if strings.HasPrefix(e.Subject, "notification.") || strings.HasSuffix(e.Subject, ".notification") {
				out = append(out, e)
			}
		default:
			return out
		}
	}
}

// TestNotifyArtifactReviewSummary_OwnerOnly: the review summary is stored
// for the artifact's owner alone, published on the owner's own subject
// alone, read back only by the owner through the notifications API, and
// handed to no dispatcher (no agent, broker or chat plugin delivery).
func TestNotifyArtifactReviewSummary_OwnerOnly(t *testing.T) {
	f := newSummaryFixture(t)
	f.srv.notifyArtifactReview(context.Background(), f.notice(hostileReviewNote))

	mine := f.notifications(t, f.owner)
	require.Len(t, mine, 1)
	n := mine[0]
	assert.Equal(t, store.NotificationKindArtifactReviewSummary, n.Kind)
	assert.Equal(t, summaryArtifactID, n.ResourceID)
	assert.Equal(t, summaryProjectID, n.ProjectID)
	assertCleanText(t, n.Excerpt, reviewExcerptMaxRunes)
	assert.NotEmpty(t, n.Excerpt)
	// The reviewer's hostile display name is cleaned in the attribution.
	assert.Equal(t, "Review summary from Mallory[2J Admin (v3 of your artifact):", n.Message)
	assert.Empty(t, f.notifications(t, f.reviewer), "the reviewer gets nothing")
	assert.Empty(t, f.notifications(t, f.other), "no other user gets anything")

	evts := f.awaitNotificationEvents(t, 1)
	assert.Equal(t, "user."+f.owner.ID+".notification", evts[0].Subject)
	assert.Empty(t, f.published(), "one notification event only")
	var evt UserNotificationEvent
	require.NoError(t, json.Unmarshal(evts[0].Data, &evt))
	assert.Equal(t, n.Excerpt, evt.Excerpt)
	assert.Equal(t, n.ResourceID, evt.ResourceID)
	assert.Equal(t, f.owner.ID, evt.SubscriberID)

	assert.Empty(t, f.dispatcher.getCalls(), "nothing is dispatched to an agent, broker or plugin")

	// The per-recipient read path: the owner lists it with its excerpt;
	// another user does not see it.
	rec := userArtifactRequest(t, f.srv, f.owner, http.MethodGet, "/api/v1/notifications", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var listed []store.Notification
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &listed))
	require.Len(t, listed, 1)
	assert.Equal(t, n.Excerpt, listed[0].Excerpt)
	assert.Equal(t, store.NotificationKindArtifactReviewSummary, listed[0].Kind)
	rec = userArtifactRequest(t, f.srv, f.other, http.MethodGet, "/api/v1/notifications", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), n.ID)
}

// summaryRecordingChannel is a notification channel (the path chat
// integrations such as Slack, Telegram, Discord or email use without a
// message broker) that records what it is given.
type summaryRecordingChannel struct {
	mu   sync.Mutex
	msgs []*messages.StructuredMessage
}

func (c *summaryRecordingChannel) Name() string    { return "recording" }
func (c *summaryRecordingChannel) Validate() error { return nil }
func (c *summaryRecordingChannel) Deliver(_ context.Context, msg *messages.StructuredMessage) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msgs = append(c.msgs, msg)
	return nil
}

func (c *summaryRecordingChannel) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.msgs)
}

// TestNotifyArtifactReviewSummary_NoExternalDelivery: with a message broker
// (whose bus feeds the chat plugins) and a notification channel registry
// both configured on the server, a review summary reaches neither; only the
// owner's in-app row and subject carry it. The control at the end shows
// both spies are wired: a direct publish and dispatch reach them.
func TestNotifyArtifactReviewSummary_NoExternalDelivery(t *testing.T) {
	f := newSummaryFixture(t)
	bus := &spyBrokerBus{}
	f.srv.SetMessageBrokerProxy(NewMessageBrokerProxy(bus, f.st, f.srv.events,
		func() AgentDispatcher { return f.dispatcher }, slog.Default()))
	ch := &summaryRecordingChannel{}
	reg := &ChannelRegistry{entries: []channelEntry{{channel: ch, config: ChannelConfig{Type: "recording"}}}, log: slog.Default()}
	f.srv.SetChannelRegistry(reg)

	// The user-owner path is synchronous: notifyArtifactReview returns
	// after the row is stored and the owner's event published. The event is
	// awaited anyway before the negative checks.
	f.srv.notifyArtifactReview(context.Background(), f.notice(hostileReviewNote))
	evts := f.awaitNotificationEvents(t, 1)
	assert.Equal(t, "user."+f.owner.ID+".notification", evts[0].Subject)
	require.Len(t, f.notifications(t, f.owner), 1, "the in-app row is written")
	assert.Empty(t, bus.getEvents(), "nothing is published on the message broker bus")
	assert.Zero(t, ch.count(), "nothing is handed to a notification channel")
	assert.Empty(t, f.dispatcher.getCalls(), "nothing is dispatched to an agent")
	assert.Empty(t, f.published(), "no further notification event")

	// Control: the spies receive what is sent to them directly.
	probe := messages.NewSystemMessage("system", "user:"+f.owner.ID, "probe", "")
	require.NoError(t, f.srv.GetMessageBrokerProxy().PublishUserMessage(context.Background(), summaryProjectID, f.owner.ID, probe))
	f.srv.currentChannelRegistry().Dispatch(context.Background(), probe)
	assert.Len(t, bus.getEvents(), 1)
	assert.Equal(t, 1, ch.count())
}

// TestNotifyArtifactReviewSummary_Skipped: no summary is written for a
// review the finalize request did not authorize, by the owner, of an agent's artifact,
// for an owner that does not exist, or for an artifact id that is not a
// canonical UUID.
func TestNotifyArtifactReviewSummary_Skipped(t *testing.T) {
	f := newSummaryFixture(t)
	for name, mutate := range map[string]func(*artifacts.ReviewNotice){
		"not authorized at submit": func(n *artifacts.ReviewNotice) { n.SubmitAuthorized = false },
		"owner's own review": func(n *artifacts.ReviewNotice) {
			n.ReviewerKind, n.ReviewerRef = n.OwnerKind, n.OwnerRef
		},
		"unknown owner":      func(n *artifacts.ReviewNotice) { n.OwnerRef = tid("rs-nobody") },
		"non-canonical id":   func(n *artifacts.ReviewNotice) { n.ArtifactID = strings.ToUpper(summaryArtifactID) },
		"resource-like id":   func(n *artifacts.ReviewNotice) { n.ArtifactID = "../" + summaryArtifactID },
		"unknown owner kind": func(n *artifacts.ReviewNotice) { n.OwnerKind = "" },
	} {
		t.Run(name, func(t *testing.T) {
			n := f.notice("Looks good.")
			mutate(&n)
			f.srv.notifyArtifactReview(context.Background(), n)
			assert.Empty(t, f.notifications(t, f.owner))
			assert.Empty(t, f.published())
		})
	}
}

func TestReviewerLabel(t *testing.T) {
	f := newSummaryFixture(t)
	ctx := context.Background()
	assert.Equal(t, "Mallory[2J Admin", f.srv.reviewerLabel(ctx, artifacts.PrincipalKindUser, f.reviewer.ID))
	assert.Equal(t, "rs-owner@test.com", f.srv.reviewerLabel(ctx, artifacts.PrincipalKindUser, f.owner.ID))
	assert.Equal(t, "a user", f.srv.reviewerLabel(ctx, artifacts.PrincipalKindUser, tid("rs-nobody")))
	assert.Equal(t, "an agent", f.srv.reviewerLabel(ctx, artifacts.PrincipalKindAgent, "missing-agent"))
	assert.Equal(t, "a reviewer", f.srv.reviewerLabel(ctx, "", "x"))

	long := &store.User{ID: tid("rs-long"), Email: "rs-long@test.com", DisplayName: strings.Repeat("n", 100),
		Role: store.UserRoleMember, Status: "active", Created: time.Now()}
	require.NoError(t, f.st.CreateUser(ctx, long))
	got := f.srv.reviewerLabel(ctx, artifacts.PrincipalKindUser, long.ID)
	assert.Equal(t, reviewerLabelMaxRunes, utf8.RuneCountInString(got))
	assert.True(t, strings.HasSuffix(got, "…"))
}

// TestReviewSummaryEvents_ExcerptOnUserSubjectOnly: a review summary row
// published on the broadcast subjects (which no writer does for this kind)
// still carries no excerpt there; only the owner's subject has it.
func TestReviewSummaryEvents_ExcerptOnUserSubjectOnly(t *testing.T) {
	pub := NewChannelEventPublisher()
	defer pub.Close()
	broadcast, unsubB := pub.Subscribe("notification.>")
	defer unsubB()
	project, unsubP := pub.Subscribe("project." + summaryProjectID + ".notification")
	defer unsubP()
	user, unsubU := pub.Subscribe("user.owner-id.notification")
	defer unsubU()

	n := buildReviewSummaryNotification(summaryNotice("Looks good."), "owner-id", "Alice")
	ctx := context.Background()
	pub.PublishNotification(ctx, n)
	for name, ch := range map[string]<-chan Event{"broadcast": broadcast, "project": project} {
		keys := receiveRaw(t, ch)
		assert.NotContains(t, keys, "excerpt", name)
	}
	pub.PublishUserNotification(ctx, n)
	keys := receiveRaw(t, user)
	assert.JSONEq(t, `"Looks good."`, string(keys["excerpt"]))
}
