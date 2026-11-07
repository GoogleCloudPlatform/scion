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
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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

func (r *leaveStreamRecorder) Flush() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.flushed = true
}

func (r *leaveStreamRecorder) isFlushed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.flushed
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
// the internal membership subject, nor to the hub namespace.
func TestSSE_InternalMembershipSubjectRefused(t *testing.T) {
	x := newLeaveSSEFixture(t)
	for _, subject := range []string{membershipChangedSubject, "hub.membership.>", "hub.>"} {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		rec := httptest.NewRecorder()
		x.ws.handleSSE(rec, sseAgentRequest(ctx, x.f.member.ID, "user", subject))
		assert.Equal(t, http.StatusForbidden, rec.Code, "subject %q: %s", subject, rec.Body.String())
	}
	assert.NotEmpty(t, x.ws.authorizeSSESubjects(sseAgentRequest(context.Background(), x.f.member.ID, "user", membershipChangedSubject),
		[]string{membershipChangedSubject}))
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
