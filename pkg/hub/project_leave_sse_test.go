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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Streams a user holds re-run their authorization when the user leaves a
// project (ptone/scion#7494): the web events stream (project chat) and the
// agent messages stream.

// leaveStreamRecorder is a streaming response writer safe to read from the
// test goroutine: the header map belongs to the handler, and the first
// Flush (after the handler set its headers) is recorded under the lock.
type leaveStreamRecorder struct {
	mu      sync.Mutex
	header  http.Header
	body    bytes.Buffer
	code    int
	flushed bool
}

func newLeaveStreamRecorder() *leaveStreamRecorder {
	return &leaveStreamRecorder{header: http.Header{}}
}

func (r *leaveStreamRecorder) Header() http.Header { return r.header }

func (r *leaveStreamRecorder) WriteHeader(code int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.code == 0 {
		r.code = code
	}
}

func (r *leaveStreamRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.code == 0 {
		r.code = http.StatusOK
	}
	return r.body.Write(p)
}

// Flush commits the headers with an implicit 200 when no status was
// written yet, as net/http's response writer does: a stream handler sets
// its headers and flushes without calling WriteHeader.
func (r *leaveStreamRecorder) Flush() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.code == 0 {
		r.code = http.StatusOK
	}
	r.flushed = true
}

func (r *leaveStreamRecorder) isFlushed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.flushed
}

func (r *leaveStreamRecorder) status() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.code
}

func (r *leaveStreamRecorder) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.body.String()
}

// leaveSSEStream is one open GET /events stream driven in a goroutine.
type leaveSSEStream struct {
	rec    *leaveStreamRecorder
	done   chan struct{}
	cancel context.CancelFunc
}

func (s *leaveSSEStream) closed() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

// openLeaveSSE opens ws.handleSSE for userID on subject and waits until the
// stream has flushed its headers (the subscription is installed first).
func openLeaveSSE(t *testing.T, ws *WebServer, userID, subject string) *leaveSSEStream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	s := &leaveSSEStream{rec: newLeaveStreamRecorder(), done: make(chan struct{}), cancel: cancel}
	t.Cleanup(func() { cancel(); <-s.done })
	go func() {
		defer close(s.done)
		ws.handleSSE(s.rec, sseAgentRequest(ctx, userID, "user", subject))
	}()
	require.Eventually(t, func() bool { return s.rec.isFlushed() || s.closed() }, 5*time.Second, 10*time.Millisecond)
	require.False(t, s.closed(), "the stream was refused: %s", s.rec.String())
	return s
}

// leaveSSEFixture is a project with a member who leaves and an admin who
// stays, both holding a web events stream on the project's chat subject.
type leaveSSEFixture struct {
	f       *mmrFixture
	pub     *ChannelEventPublisher
	ws      *WebServer
	subject string
}

func newLeaveSSEFixture(t *testing.T) *leaveSSEFixture {
	t.Helper()
	f := setupMMRFixture(t)
	enableProjectLeave(t, f.srv)
	pub := NewChannelEventPublisher()
	t.Cleanup(pub.Close)
	f.srv.SetEventPublisher(pub)
	return &leaveSSEFixture{
		f: f, pub: pub,
		ws:      &WebServer{store: f.store, events: pub, authzService: f.srv.authzService},
		subject: "project." + f.projectID + ".chat.message",
	}
}

// chat publishes a project chat message carrying marker.
func (x *leaveSSEFixture) chat(marker string) {
	x.pub.PublishRaw(x.subject, map[string]string{"id": marker, "msg": marker})
}

// TestProjectLeave_ChatStreamClosesOnLeave (6c follow-up, re-authorize on
// notify): the leaver's open chat stream closes with a reconnect hint after
// the leave and never receives a chat message posted after it; another
// member's stream stays open and receives it. Conduit is off here: the
// re-check does not depend on hub.conduit.
func TestProjectLeave_ChatStreamClosesOnLeave(t *testing.T) {
	x := newLeaveSSEFixture(t)
	leaver := openLeaveSSE(t, x.ws, x.f.member.ID, x.subject)
	control := openLeaveSSE(t, x.ws, x.f.admin.ID, x.subject)

	x.chat("before-leave")
	require.Eventually(t, func() bool {
		return strings.Contains(leaver.rec.String(), "before-leave") && strings.Contains(control.rec.String(), "before-leave")
	}, 5*time.Second, 10*time.Millisecond)

	decodeLeaveResult(t, leaveAs(t, x.f.srv, x.f.member, x.f.projectID, false))

	select {
	case <-leaver.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the leaver's chat stream was not closed")
	}
	assert.Contains(t, leaver.rec.String(), "event: reconnect")

	x.chat("after-leave")
	require.Eventually(t, func() bool { return strings.Contains(control.rec.String(), "after-leave") },
		5*time.Second, 10*time.Millisecond)
	assert.NotContains(t, leaver.rec.String(), "after-leave")
	assert.False(t, control.closed(), "another member's stream stays open")
	assert.NotContains(t, leaver.rec.String(), membershipChangedSubject, "the internal event is never written to a client")
	assert.NotContains(t, control.rec.String(), membershipChangedSubject)
}

// TestProjectLeave_ChatStreamClosesOnLeave_MutantNoPublish (mutant): with
// the leave route's membership publish replaced by a no-op, the leaver's
// chat stream stays open and receives a post-leave message, so the test
// above would fail.
func TestProjectLeave_ChatStreamClosesOnLeave_MutantNoPublish(t *testing.T) {
	x := newLeaveSSEFixture(t)
	orig := projectLeavePublishMembership
	projectLeavePublishMembership = func(*Server, string, string) {}
	t.Cleanup(func() { projectLeavePublishMembership = orig })
	leaver := openLeaveSSE(t, x.ws, x.f.member.ID, x.subject)

	decodeLeaveResult(t, leaveAs(t, x.f.srv, x.f.member, x.f.projectID, false))
	x.chat("after-leave")
	require.Eventually(t, func() bool { return strings.Contains(leaver.rec.String(), "after-leave") },
		5*time.Second, 10*time.Millisecond, "without the publish the stream keeps delivering")
	assert.False(t, leaver.closed())
}

// TestProjectLeave_ChatStreamClosesOnLeave_MutantNoReauth (mutant): with
// the stream's re-authorization skipped (always allowed), the leaver's chat
// stream stays open after the membership event and receives a post-leave
// message, so the first test would fail.
func TestProjectLeave_ChatStreamClosesOnLeave_MutantNoReauth(t *testing.T) {
	x := newLeaveSSEFixture(t)
	orig := sseStreamStillAuthorized
	reauth := make(chan struct{}, 4)
	sseStreamStillAuthorized = func(*WebServer, *http.Request, []string) bool { reauth <- struct{}{}; return true }
	t.Cleanup(func() { sseStreamStillAuthorized = orig })
	leaver := openLeaveSSE(t, x.ws, x.f.member.ID, x.subject)

	decodeLeaveResult(t, leaveAs(t, x.f.srv, x.f.member, x.f.projectID, false))
	select {
	case <-reauth:
	case <-time.After(10 * time.Second):
		t.Fatal("the membership event never reached the stream")
	}
	x.chat("after-leave")
	require.Eventually(t, func() bool { return strings.Contains(leaver.rec.String(), "after-leave") },
		5*time.Second, 10*time.Millisecond, "without the re-check the stream keeps delivering")
	assert.False(t, leaver.closed())
}

// TestProjectLeave_ChatStreamReauthFailsClosed: a re-check that cannot
// decide ends the stream with the reconnect hint.
func TestProjectLeave_ChatStreamReauthFailsClosed(t *testing.T) {
	x := newLeaveSSEFixture(t)
	orig := sseStreamStillAuthorized
	sseStreamStillAuthorized = func(*WebServer, *http.Request, []string) bool { return false }
	t.Cleanup(func() { sseStreamStillAuthorized = orig })
	s := openLeaveSSE(t, x.ws, x.f.admin.ID, x.subject)

	x.f.srv.publishMembershipChanged(x.f.admin.ID, x.f.projectID)
	select {
	case <-s.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the stream was not closed")
	}
	assert.Contains(t, s.rec.String(), "event: reconnect")
}

// gatedLeaveRecorder is a leaveStreamRecorder whose Write blocks while the
// gate is armed, so a test can hold the handler inside a write while it
// queues more events behind it.
type gatedLeaveRecorder struct {
	*leaveStreamRecorder
	gateMu  sync.Mutex
	armed   bool
	gate    chan struct{}
	blocked chan struct{}
}

func newGatedLeaveRecorder() *gatedLeaveRecorder {
	return &gatedLeaveRecorder{leaveStreamRecorder: newLeaveStreamRecorder(), blocked: make(chan struct{}, 1)}
}

func (g *gatedLeaveRecorder) Write(p []byte) (int, error) {
	g.gateMu.Lock()
	armed, gate := g.armed, g.gate
	g.gateMu.Unlock()
	if armed {
		select {
		case g.blocked <- struct{}{}:
		default:
		}
		<-gate
	}
	return g.leaveStreamRecorder.Write(p)
}

// arm makes every later Write block until release.
func (g *gatedLeaveRecorder) arm() {
	g.gateMu.Lock()
	defer g.gateMu.Unlock()
	g.armed, g.gate = true, make(chan struct{})
}

// release unblocks the held Write and every later one.
func (g *gatedLeaveRecorder) release() {
	g.gateMu.Lock()
	defer g.gateMu.Unlock()
	g.armed = false
	close(g.gate)
}

// TestProjectLeave_EventAfterMembershipChangeNeverReachesLeaver (review
// S1): when the membership-changed event and a later event are both
// queued, the stream re-checks the membership first, so the later event
// is never written to the leaver. The handler is held inside the write of
// an earlier event while both are queued behind it, which is where
// select's random choice could otherwise let the later event through; the
// round is repeated so a random pick would fail it.
func TestProjectLeave_EventAfterMembershipChangeNeverReachesLeaver(t *testing.T) {
	x := newLeaveSSEFixture(t)
	orig := sseStreamStillAuthorized
	sseStreamStillAuthorized = func(*WebServer, *http.Request, []string) bool { return false }
	t.Cleanup(func() { sseStreamStillAuthorized = orig })

	for i := 0; i < 20; i++ {
		g := newGatedLeaveRecorder()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			x.ws.handleSSE(g, sseAgentRequest(ctx, x.f.member.ID, "user", x.subject))
		}()
		require.Eventually(t, func() bool { return g.isFlushed() }, 5*time.Second, 10*time.Millisecond)

		g.arm()
		hold, after := fmt.Sprintf("hold-%d", i), fmt.Sprintf("after-%d", i)
		x.chat(hold)
		select {
		case <-g.blocked:
		case <-time.After(5 * time.Second):
			g.release()
			cancel()
			<-done
			t.Fatalf("round %d: the stream never started writing the held event", i)
		}
		// Both queued behind the held write: the leaver's membership change,
		// then a chat message published right after it.
		x.f.srv.publishMembershipChanged(x.f.member.ID, x.f.projectID)
		x.chat(after)
		g.release()

		select {
		case <-done:
		case <-time.After(10 * time.Second):
			cancel()
			<-done
			t.Fatalf("round %d: the leaver's stream was not closed", i)
		}
		cancel()
		body := g.String()
		require.Contains(t, body, hold, "round %d", i)
		require.Contains(t, body, "event: reconnect", "round %d", i)
		require.NotContains(t, body, after, "round %d: an event published after the membership change reached the leaver", i)
	}
}

// TestProjectLeave_MembershipEventOfOtherUserKeepsStream: an event naming
// another user does not re-check or close the stream.
func TestProjectLeave_MembershipEventOfOtherUserKeepsStream(t *testing.T) {
	x := newLeaveSSEFixture(t)
	orig := sseStreamStillAuthorized
	sseStreamStillAuthorized = func(*WebServer, *http.Request, []string) bool { return false }
	t.Cleanup(func() { sseStreamStillAuthorized = orig })
	s := openLeaveSSE(t, x.ws, x.f.admin.ID, x.subject)

	x.f.srv.publishMembershipChanged(x.f.member.ID, x.f.projectID)
	x.chat("still-here")
	require.Eventually(t, func() bool { return strings.Contains(s.rec.String(), "still-here") },
		5*time.Second, 10*time.Millisecond)
	assert.False(t, s.closed())
}

// TestSSE_InternalMembershipSubjectRefused: a client cannot subscribe to
// the internal membership subjects (its own per-user subject, another
// user's, or the prefix), nor to the hub namespace.
func TestSSE_InternalMembershipSubjectRefused(t *testing.T) {
	x := newLeaveSSEFixture(t)
	for _, subject := range []string{
		membershipChangedSubject, membershipChangedSubjectFor(x.f.member.ID), membershipChangedSubjectFor(x.f.admin.ID),
		membershipChangedSubject + "." + strings.Repeat("0123456789abcdef", 4),
		"hub.membership.changed.*", "hub.membership.changed.>", "hub.membership.>", "hub.>",
	} {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		rec := httptest.NewRecorder()
		x.ws.handleSSE(rec, sseAgentRequest(ctx, x.f.member.ID, "user", subject))
		assert.Equal(t, http.StatusForbidden, rec.Code, "subject %q: %s", subject, rec.Body.String())
	}
	assert.NotEmpty(t, x.ws.authorizeSSESubjects(sseAgentRequest(context.Background(), x.f.member.ID, "user", membershipChangedSubject),
		[]string{membershipChangedSubject}))
	own := membershipChangedSubjectFor(x.f.member.ID)
	assert.NotEmpty(t, x.ws.authorizeSSESubjects(sseAgentRequest(context.Background(), x.f.member.ID, "user", own),
		[]string{own}), "the caller's own per-user subject is refused too")
}

// TestSSE_NoSessionDeniedWithNoMembershipSubscription (review round 4):
// with no session user the web events stream has no membership subject;
// it subscribes to nothing and denies every subject.
func TestSSE_NoSessionDeniedWithNoMembershipSubscription(t *testing.T) {
	x := newLeaveSSEFixture(t)
	pub := &sseCountingPublisher{ChannelEventPublisher: NewChannelEventPublisher()}
	t.Cleanup(pub.Close)
	ws := &WebServer{store: x.f.store, events: pub, authzService: x.f.srv.authzService}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodGet, "/events?sub="+x.subject, nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	ws.handleSSE(rec, req)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	var body struct {
		Denied []string `json:"denied_subjects"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, []string{x.subject}, body.Denied)
	assert.Zero(t, pub.subscriptions, "no subscription, the membership one included")
}

// TestMembershipChangedSubjectFor: every non-empty user ID, federated
// (<issuer>:<sub>) and awkward ones included, gets one deterministic
// subject whose last token is the 64-hex SHA-256 of the ID, so it is a
// single valid token with no ID text; distinct IDs get distinct subjects;
// an empty ID gets none.
func TestMembershipChangedSubjectFor(t *testing.T) {
	assert.Empty(t, membershipChangedSubjectFor(""))
	ids := []string{"u-1", "U-1", "a.b", "a*", "a>", "a b", "a\tb", "a\nb", "a\rb", "a\x00b", "a\x7fb", "a/b", "a\u00e9b",
		"https://issuer.example:sub", "https://issuer.example:sub/a.b\tc"}
	seen := map[string]string{}
	for _, id := range ids {
		subject := membershipChangedSubjectFor(id)
		assert.Equal(t, subject, membershipChangedSubjectFor(id), "%q: deterministic", id)
		require.True(t, strings.HasPrefix(subject, membershipChangedSubject+"."), "%q: %q", id, subject)
		token := strings.TrimPrefix(subject, membershipChangedSubject+".")
		assert.Len(t, token, 64, "%q", id)
		for _, c := range token {
			assert.True(t, (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f'), "%q: token %q is lowercase hex", id, token)
		}
		assert.NotContains(t, subject, id, "%q: no ID text in the subject", id)
		if prev, dup := seen[subject]; dup {
			t.Errorf("%q and %q share a subject", prev, id)
		}
		seen[subject] = id
	}
}

// TestProjectLeave_OtherUsersChangesDoNotCrowdOutLeaver (review round 2,
// security note): each stream subscribes only to its own user's
// membership subject, so a burst of other users' membership changes never
// fills the leaver's queue. The leaver's stream is held inside a write
// while more other-user changes than the subscription buffer holds are
// published, then the leaver's own change; on release the stream still
// re-checks and ends. With one shared subject the burst would fill the
// buffer and the leaver's change would be dropped.
func TestProjectLeave_OtherUsersChangesDoNotCrowdOutLeaver(t *testing.T) {
	x := newLeaveSSEFixture(t)
	orig := sseStreamStillAuthorized
	sseStreamStillAuthorized = func(*WebServer, *http.Request, []string) bool { return false }
	t.Cleanup(func() { sseStreamStillAuthorized = orig })

	g := newGatedLeaveRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	t.Cleanup(func() { cancel(); <-done })
	go func() {
		defer close(done)
		x.ws.handleSSE(g, sseAgentRequest(ctx, x.f.member.ID, "user", x.subject))
	}()
	require.Eventually(t, g.isFlushed, 5*time.Second, 10*time.Millisecond)

	g.arm()
	x.chat("hold")
	select {
	case <-g.blocked:
	case <-time.After(5 * time.Second):
		g.release()
		t.Fatal("the stream never started writing the held event")
	}
	for i := 0; i < 100; i++ { // more than the 64-event subscription buffer
		x.f.srv.publishMembershipChanged(tid(fmt.Sprintf("%s-other-%d", t.Name(), i)), x.f.projectID)
	}
	x.f.srv.publishMembershipChanged(x.f.member.ID, x.f.projectID)
	g.release()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the leaver's stream was not closed: its membership change was crowded out")
	}
	assert.Contains(t, g.String(), "event: reconnect")
}

// TestProjectLeave_AgentMessagesStreamEndsOnLeave: the leaver's session
// stream of an agent's messages ends after the leave; a member who stays
// keeps theirs.
func TestProjectLeave_AgentMessagesStreamEndsOnLeave(t *testing.T) {
	x := newLeaveSSEFixture(t)
	ctx := context.Background()
	agent := &store.Agent{
		ID: tid(t.Name() + "-agent"), Slug: "leave-msg-agent", Name: "Leave Msg Agent",
		ProjectID: x.f.projectID, OwnerID: x.f.owner.ID, CreatedBy: x.f.owner.ID, Ancestry: []string{x.f.owner.ID},
		Phase: "running", RuntimeBrokerID: tid(t.Name() + "-broker"), StateVersion: 1,
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, x.f.store.CreateAgent(ctx, agent))

	open := func(u *store.User) (*leaveStreamRecorder, chan struct{}) {
		identity := NewAuthenticatedUser(u.ID, u.Email, u.DisplayName, u.Role, "web")
		sctx, cancel := context.WithCancel(ctx)
		rctx := contextWithCredentialContext(context.WithValue(contextWithIdentity(sctx, identity), userContextKey{}, identity),
			CredentialContext{Kind: CredentialKindInteractive, ID: "test-session"})
		req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/"+agent.ID+"/messages/stream", nil).WithContext(rctx)
		rec := newLeaveStreamRecorder()
		done := make(chan struct{})
		t.Cleanup(func() { cancel(); <-done })
		go func() {
			defer close(done)
			x.f.srv.mux.ServeHTTP(rec, req)
		}()
		require.Eventually(t, rec.isFlushed, 5*time.Second, 10*time.Millisecond, "stream not opened")
		return rec, done
	}
	leaverRec, leaverDone := open(x.f.member)
	_, controlDone := open(x.f.admin)

	decodeLeaveResult(t, leaveAs(t, x.f.srv, x.f.member, x.f.projectID, false))
	select {
	case <-leaverDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the leaver's agent messages stream did not end")
	}
	assert.Contains(t, leaverRec.String(), "stream ended")
	select {
	case <-controlDone:
		t.Fatal("another member's agent messages stream ended")
	case <-time.After(200 * time.Millisecond):
	}
}

// TestProjectLeave_AgentMessageAfterMembershipChangeNeverReachesLeaver
// (review round 2, the agent messages twin of
// TestProjectLeave_EventAfterMembershipChangeNeverReachesLeaver): the
// leaver's agent messages stream is held inside the write of one message
// while the leave publishes its membership change and a second message is
// published behind it. On release, the stream re-checks the queued
// membership change before writing the second message, so the second
// message is never written and the stream ends with the credential
// error. A real leave each round (the member is added back between
// rounds); 20 rounds, so a random pick between the two ready cases would
// fail it.
//
// The stream drains the queued membership change before any filter
// (handlers_messages.go, the drain precedes the participant and
// conversation filters), so the result does not depend on the
// conversation envelope switch. The main case runs the default config
// (switch on) with the messages on the leaver's DM conversation with the
// agent, as real users send them; a second case turns the switch off,
// where the participant filter alone decides.
func TestProjectLeave_AgentMessageAfterMembershipChangeNeverReachesLeaver(t *testing.T) {
	t.Run("default config, DM conversation", func(t *testing.T) {
		x := newLeaveSSEFixture(t)
		agent := leaveOrderAgent(t, x)
		dmKey, err := messages.DMConversationKey("agent", agent.ID, "user", x.f.member.ID)
		require.NoError(t, err)
		conv, err := x.f.store.UpsertConversationByExternalRef(context.Background(), &store.Conversation{
			Kind: "direct", Surface: "native", ExternalRef: dmKey, DriftState: "active",
		})
		require.NoError(t, err)
		leaveAgentMessageOrderRounds(t, x, agent, conv.ID)
	})
	t.Run("envelope switch off, participant filter only", func(t *testing.T) {
		x := newLeaveSSEFixture(t)
		settings := newFakeHubSettingStore()
		overrides, err := json.Marshal(map[string]map[string]bool{"overrides": {experiments.ProjectSelfLeave: true}})
		require.NoError(t, err)
		settings.seed("experiments", json.RawMessage(overrides))
		settings.seed("messaging", json.RawMessage(`{"conversation_envelope_switch":false}`))
		ops := NewOperationalSettings(settings, emptyKoanf(), emptyKoanf())
		_, err = ops.Refresh(context.Background())
		require.NoError(t, err)
		x.f.srv.SetOperationalSettings(ops)
		leaveAgentMessageOrderRounds(t, x, leaveOrderAgent(t, x), "")
	})
}

// leaveOrderAgent creates the owner's running agent in the fixture project.
func leaveOrderAgent(t *testing.T, x *leaveSSEFixture) *store.Agent {
	t.Helper()
	agent := &store.Agent{
		ID: tid(t.Name() + "-agent"), Slug: "leave-order-agent", Name: "Leave Order Agent",
		ProjectID: x.f.projectID, OwnerID: x.f.owner.ID, CreatedBy: x.f.owner.ID, Ancestry: []string{x.f.owner.ID},
		Phase: "running", RuntimeBrokerID: tid(t.Name() + "-broker"), StateVersion: 1,
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, x.f.store.CreateAgent(context.Background(), agent))
	return agent
}

// leaveAgentMessageOrderRounds runs the held-write rounds of the agent
// messages ordering test. Each message is to the member, on convID when
// it is set.
func leaveAgentMessageOrderRounds(t *testing.T, x *leaveSSEFixture, agent *store.Agent, convID string) {
	t.Helper()
	x.f.srv.membershipService.onMembershipLoss = nil
	ctx := context.Background()
	message := func(marker string) {
		x.pub.PublishRaw("agent."+agent.ID+".message", UserMessageEvent{
			ID: marker, ProjectID: x.f.projectID, SenderID: x.f.owner.ID, RecipientID: x.f.member.ID,
			Msg: marker, AgentID: agent.ID, Type: "instruction", ConversationID: convID,
		})
	}

	for i := 0; i < 20; i++ {
		if i > 0 {
			grpBind(t, x.f.store, store.RoleBindingPrincipalUser, x.f.member.ID, x.f.memberRD.ID, x.f.projectID)
		}
		identity := NewAuthenticatedUser(x.f.member.ID, x.f.member.Email, x.f.member.DisplayName, x.f.member.Role, "web")
		sctx, cancel := context.WithCancel(ctx)
		rctx := contextWithCredentialContext(context.WithValue(contextWithIdentity(sctx, identity), userContextKey{}, identity),
			CredentialContext{Kind: CredentialKindInteractive, ID: "test-session"})
		req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/"+agent.ID+"/messages/stream", nil).WithContext(rctx)
		g := newGatedLeaveRecorder()
		done := make(chan struct{})
		go func() {
			defer close(done)
			x.f.srv.mux.ServeHTTP(g, req)
		}()
		require.Eventually(t, g.isFlushed, 5*time.Second, 10*time.Millisecond, "round %d: stream not opened", i)

		g.arm()
		hold, after := fmt.Sprintf("hold-%d", i), fmt.Sprintf("after-%d", i)
		message(hold)
		select {
		case <-g.blocked:
		case <-time.After(5 * time.Second):
			g.release()
			cancel()
			<-done
			t.Fatalf("round %d: the stream never started writing the held message", i)
		}
		// Both queued behind the held write: the leave's membership change,
		// then a message published right after it.
		decodeLeaveResult(t, leaveAs(t, x.f.srv, x.f.member, x.f.projectID, false))
		message(after)
		g.release()

		select {
		case <-done:
		case <-time.After(10 * time.Second):
			cancel()
			<-done
			t.Fatalf("round %d: the leaver's agent messages stream did not end", i)
		}
		cancel()
		body := g.String()
		require.Contains(t, body, hold, "round %d", i)
		require.Contains(t, body, "stream ended", "round %d", i)
		require.NotContains(t, body, after, "round %d: a message published after the membership change reached the leaver", i)
	}
}

// TestProjectLeave_ChangeDuringWebStreamSetupClosesStream (review round 3,
// SR-1): a membership change published after the web events stream
// authorized its subjects, but before its loop starts, is not missed. The
// stream subscribes to the caller's membership subject before it
// authorizes, so the change is queued, re-checked when the loop starts,
// and the stream closes with the reconnect hint. Had the subscription come
// after the authorization, the change would be lost and the stream would
// stay open.
func TestProjectLeave_ChangeDuringWebStreamSetupClosesStream(t *testing.T) {
	x := newLeaveSSEFixture(t)
	orig := sseStreamStillAuthorized
	sseStreamStillAuthorized = func(*WebServer, *http.Request, []string) bool { return false }
	t.Cleanup(func() { sseStreamStillAuthorized = orig })
	streamAuthorizedHook = func(kind string) {
		if kind == streamKindWebEvents {
			x.f.srv.publishMembershipChanged(x.f.member.ID, x.f.projectID)
		}
	}
	t.Cleanup(func() { streamAuthorizedHook = nil })

	rec := newLeaveStreamRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		x.ws.handleSSE(rec, sseAgentRequest(ctx, x.f.member.ID, "user", x.subject))
	}()
	t.Cleanup(func() { cancel(); <-done })
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the stream stayed open: the change published during setup was missed")
	}
	assert.Contains(t, rec.String(), "event: reconnect")
}

// TestProjectLeave_LeaveDuringAgentStreamSetupClosesStream (review round 3,
// SR-1): a leave made after the agent messages stream authorized the
// caller, but before its loop starts, is not missed. The stream subscribes
// to the caller's membership subject before it authorizes, so the leave's
// change is queued, re-checked when the loop starts, and the stream ends
// with the credential error.
func TestProjectLeave_LeaveDuringAgentStreamSetupClosesStream(t *testing.T) {
	x := newLeaveSSEFixture(t)
	x.f.srv.membershipService.onMembershipLoss = nil
	agent := leaveOrderAgent(t, x)
	// A real leave. The request is prepared here, on the test goroutine;
	// the hook runs only the returned func on the handler's goroutine, and
	// that func makes no assertion.
	leave := leaveRequestFunc(t, x.f.srv, x.f.member, x.f.projectID)
	leaveCode := 0
	streamAuthorizedHook = func(kind string) {
		if kind == streamKindAgentMessages {
			leaveCode = leave()
		}
	}
	t.Cleanup(func() { streamAuthorizedHook = nil })

	identity := NewAuthenticatedUser(x.f.member.ID, x.f.member.Email, x.f.member.DisplayName, x.f.member.Role, "web")
	sctx, cancel := context.WithCancel(context.Background())
	rctx := contextWithCredentialContext(context.WithValue(contextWithIdentity(sctx, identity), userContextKey{}, identity),
		CredentialContext{Kind: CredentialKindInteractive, ID: "test-session"})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/"+agent.ID+"/messages/stream", nil).WithContext(rctx)
	rec := newLeaveStreamRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		x.f.srv.mux.ServeHTTP(rec, req)
	}()
	t.Cleanup(func() { cancel(); <-done })
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the stream stayed open: the leave made during setup was missed")
	}
	assert.Equal(t, http.StatusOK, leaveCode, "the leave in the setup window succeeded")
	assert.Contains(t, rec.String(), "stream ended")
}

// leaveOddUserID is a user ID that is no single subject token: it holds
// ':', '/', '.' and a tab, as a federated <issuer>:<sub> ID can.
const leaveOddUserID = "https://issuer.example:sub/a.b\tc"

// TestSSE_OddUserIDStreamFollowsItsMembershipSubject (review round 4): a
// session user whose ID is no single subject token opens the web events
// stream and holds exactly one membership subscription, on
// membershipChangedSubjectFor(its ID). A membership change published for
// a different ID leaves the stream open; one published for its own ID,
// through the publisher the leave uses, re-checks and closes it.
func TestSSE_OddUserIDStreamFollowsItsMembershipSubject(t *testing.T) {
	other := leaveOddUserID + "x"
	own := membershipChangedSubjectFor(leaveOddUserID)
	require.NotEqual(t, own, membershipChangedSubjectFor(other))
	projectID, agentID := tid("sse-odd-project"), tid("sse-odd-agent")
	s := &sseAgentStore{mockAuthzStore: sseAgentProjectAccess(leaveOddUserID, projectID), agents: map[string]*store.Agent{
		agentID: {ID: agentID, ProjectID: projectID, OwnerID: leaveOddUserID},
	}}
	pub := &sseCountingPublisher{ChannelEventPublisher: NewChannelEventPublisher()}
	t.Cleanup(pub.Close)
	ws := &WebServer{store: s, events: pub, authzService: NewAuthzService(s, slog.Default())}
	srv := &Server{events: pub}
	orig := sseStreamStillAuthorized
	sseStreamStillAuthorized = func(*WebServer, *http.Request, []string) bool { return false }
	t.Cleanup(func() { sseStreamStillAuthorized = orig })

	stream := openLeaveSSE(t, ws, leaveOddUserID, "agent."+agentID+".>")
	// The subscriptions are installed before the flush openLeaveSSE waited
	// for, under the recorder's lock, so reading them here is ordered.
	var mine int
	for _, p := range pub.patterns {
		if len(p) == 1 && strings.HasPrefix(p[0], membershipChangedSubject) {
			assert.Equal(t, own, p[0], "the only membership subscription is the caller's own")
			mine++
		}
	}
	assert.Equal(t, 1, mine, "exactly one membership subscription")
	assert.Len(t, pub.clientSubscriptions(leaveOddUserID), 1, "exactly one client subscription")

	srv.publishMembershipChanged(other, projectID)
	assert.Never(t, stream.closed, 300*time.Millisecond, 10*time.Millisecond, "another ID's change closed the stream")

	srv.publishMembershipChanged(leaveOddUserID, projectID)
	require.Eventually(t, stream.closed, 5*time.Second, 10*time.Millisecond, "the caller's own change did not close the stream")
	assert.Contains(t, stream.rec.String(), "event: reconnect")
}

// openFedAgentStream opens the agent messages stream of agentID for fed,
// as the /api/v1 middleware would after a federated token, and waits for
// the headers. It returns the recorder and a channel closed when the
// handler returns.
func openFedAgentStream(t *testing.T, srv *Server, fed *FederatedUserIdentity, agentID string) (*leaveStreamRecorder, chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	rctx := contextWithCredentialContext(contextWithIdentity(ctx, fed), credentialContextForIdentity(fed))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/"+agentID+"/messages/stream", nil).WithContext(rctx)
	rec := newLeaveStreamRecorder()
	done := make(chan struct{})
	t.Cleanup(func() { cancel(); <-done })
	go func() {
		defer close(done)
		srv.handleAgentMessagesStream(rec, req, agentID)
	}()
	require.Eventually(t, func() bool {
		select {
		case <-done:
			return true
		default:
			return rec.isFlushed()
		}
	}, 5*time.Second, 10*time.Millisecond)
	return rec, done
}

// TestAgentMessagesStream_FederatedUserOpensAndFollowsMembership (review
// round 4): a federated user (<issuer>:<sub>, here with '/', '.' and a tab
// in the subject) holding read on an agent through a project binding opens
// /api/v1/agents/{id}/messages/stream: 200 and the event stream. A
// membership change published for a different ID leaves the stream open.
// After the binding is removed, a change published for the federated ID
// re-checks the stream and ends it.
func TestAgentMessagesStream_FederatedUserOpensAndFollowsMembership(t *testing.T) {
	f := newFedFixture(t, "stream")
	pub := NewChannelEventPublisher()
	t.Cleanup(pub.Close)
	f.srv.SetEventPublisher(pub)
	fed := NewFederatedUserIdentity(fedTestIssuer, "sub/a.b\tc", "fed-stream@idp.example", "Fed", "viewer", nil)
	require.Contains(t, fed.ID(), ":")
	binding := f.grantFed(t, fed.ID(), f.projectID, nil)
	agent := f.agent(t, "fedstream", fed.ID())
	require.True(t, decidePerm(f.srv.authzService, fed, agentResource(agent), ActionRead, "agent.read", false).Allowed,
		"the federated user holds agent.read")

	rec, done := openFedAgentStream(t, f.srv, fed, agent.ID)
	select {
	case <-done:
		t.Fatalf("the stream was refused: %d %s", rec.status(), rec.String())
	default:
	}
	assert.Equal(t, http.StatusOK, rec.status())
	assert.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"))

	f.srv.publishMembershipChanged(fed.ID()+"x", f.projectID)
	assert.Never(t, func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	}, 300*time.Millisecond, 10*time.Millisecond, "another ID's change ended the stream")

	f.fed.removeBinding(binding.ID)
	f.srv.publishMembershipChanged(fed.ID(), f.projectID)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the federated user's own change did not end the stream")
	}
	assert.Contains(t, rec.String(), "stream ended")
}
