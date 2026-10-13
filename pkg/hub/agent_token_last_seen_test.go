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
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// credLastSeenRecorder records last_seen_at writes and answers them with
// err; with no err it forwards them to the wrapped store.
type credLastSeenRecorder struct {
	store.AgentCredentialStore
	mu  sync.Mutex
	err error
	got []time.Time
}

func (r *credLastSeenRecorder) UpdateAgentCredentialLastSeen(ctx context.Context, id string, at time.Time) error {
	r.mu.Lock()
	r.got = append(r.got, at)
	err := r.err
	r.mu.Unlock()
	if err != nil {
		return err
	}
	return r.AgentCredentialStore.UpdateAgentCredentialLastSeen(ctx, id, at)
}

func (r *credLastSeenRecorder) setErr(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.err = err
}

func (r *credLastSeenRecorder) writes() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.got)
}

// TestAgentCredentialLastSeenDue pins the interval on a fixed clock: a
// credential never seen, or seen at least the interval ago, is due; one
// seen more recently is not.
func TestAgentCredentialLastSeenDue(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	at := func(ago time.Duration) *time.Time { v := now.Add(-ago); return &v }
	assert.True(t, agentCredentialLastSeenDue(nil, now), "never seen")
	assert.False(t, agentCredentialLastSeenDue(at(0), now), "just seen")
	assert.False(t, agentCredentialLastSeenDue(at(agentCredentialLastSeenInterval-time.Nanosecond), now), "within the interval")
	assert.True(t, agentCredentialLastSeenDue(at(agentCredentialLastSeenInterval), now), "the interval has passed")
	assert.True(t, agentCredentialLastSeenDue(at(time.Hour), now), "long ago")
}

// lastSeenVerifyFixture is a credential test server whose agent-token
// verification writes last_seen_at through a recorder. Requests carry the
// spawn seam, so a write runs inline before the response.
type lastSeenVerifyFixture struct {
	srv    *Server
	store  store.Store
	rec    *credLastSeenRecorder
	token  string
	credID string
}

func newLastSeenVerifyFixture(t *testing.T, name string) *lastSeenVerifyFixture {
	t.Helper()
	srv, s, _, project := setupCredentialTestServer(t)
	agentID := tid("agent-last-seen-" + name)
	createCredTestAgent(t, s, agentID, project.ID, tid("user-cred-test"))
	token, err := srv.GenerateAgentToken(agentID, project.ID, nil, AgentRoleFull, nil)
	require.NoError(t, err)
	claims, err := srv.agentTokenService.ValidateAgentToken(token)
	require.NoError(t, err)
	cred, err := s.GetAgentCredentialByJTIHash(context.Background(), hashJTI(claims.ID))
	require.NoError(t, err)
	rec := &credLastSeenRecorder{AgentCredentialStore: s}
	srv.authConfig.CredentialStore = rec
	return &lastSeenVerifyFixture{srv: srv, store: s, rec: rec, token: token, credID: cred.ID}
}

// verify sends one agent-token request and returns its status code, which
// must be neither the revoked 401 nor the unevaluated 503.
func (f *lastSeenVerifyFixture) verify(t *testing.T) int {
	t.Helper()
	ctx := context.WithValue(context.Background(), agentCredentialLastSeenSpawnKey{}, func(write func()) { write() })
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/"+tid("agent-last-seen-target"), nil).WithContext(ctx)
	req.Header.Set("X-Scion-Agent-Token", f.token)
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	require.NotEqual(t, http.StatusUnauthorized, rec.Code, "body: %s", rec.Body.String())
	require.NotEqual(t, http.StatusServiceUnavailable, rec.Code, "body: %s", rec.Body.String())
	return rec.Code
}

// lastSeen reads the credential row's last_seen_at.
func (f *lastSeenVerifyFixture) lastSeen(t *testing.T) *time.Time {
	t.Helper()
	cred, err := f.store.GetAgentCredentialByID(context.Background(), f.credID)
	require.NoError(t, err)
	return cred.LastSeenAt
}

// age sets the row's last_seen_at to ago before now, as if that much
// time had passed since the last write.
func (f *lastSeenVerifyFixture) age(t *testing.T, ago time.Duration) {
	t.Helper()
	require.NoError(t, f.store.UpdateAgentCredentialLastSeen(context.Background(), f.credID, time.Now().Add(-ago)))
}

// TestAgentTokenVerify_LastSeenOncePerInterval: the first verify writes
// last_seen_at; a second, with the row seen within the interval, does
// not; once the row is older than the interval, the next verify writes.
func TestAgentTokenVerify_LastSeenOncePerInterval(t *testing.T) {
	f := newLastSeenVerifyFixture(t, "interval")
	require.Nil(t, f.lastSeen(t), "a new credential was never seen")

	f.verify(t)
	require.Equal(t, 1, f.rec.writes(), "the first verify writes")
	first := f.lastSeen(t)
	require.NotNil(t, first)

	f.verify(t)
	assert.Equal(t, 1, f.rec.writes(), "no write within the interval")

	f.age(t, agentCredentialLastSeenInterval/2)
	f.verify(t)
	assert.Equal(t, 1, f.rec.writes(), "no write while the row is younger than the interval")

	f.age(t, 2*agentCredentialLastSeenInterval)
	before := f.lastSeen(t)
	f.verify(t)
	assert.Equal(t, 2, f.rec.writes(), "one write once the interval has passed")
	after := f.lastSeen(t)
	require.NotNil(t, after)
	assert.True(t, after.After(*before), "last_seen_at moved forward: %v -> %v", *before, *after)
}

// TestAgentTokenVerify_LastSeenWriteFailureRetried: a failed write gives
// the same response, and leaves the row unwritten, so the next request
// writes again.
func TestAgentTokenVerify_LastSeenWriteFailureRetried(t *testing.T) {
	f := newLastSeenVerifyFixture(t, "write-failure")
	f.rec.setErr(errors.New("last_seen write failed for testing"))
	failedCode := f.verify(t)
	require.Equal(t, 1, f.rec.writes(), "the write was attempted")
	assert.Nil(t, f.lastSeen(t), "the failed write left the row unwritten")

	f.rec.setErr(nil)
	okCode := f.verify(t)
	assert.Equal(t, okCode, failedCode, "a failed write does not change the response")
	assert.Equal(t, 2, f.rec.writes(), "the next request retries the write")
	assert.NotNil(t, f.lastSeen(t))

	f.verify(t)
	assert.Equal(t, 2, f.rec.writes(), "no write within the interval after the retry")
}

// TestTouchAgentCredentialLastSeen_NilWritesNothing: a nil store or a nil
// credential writes nothing and does not panic.
func TestTouchAgentCredentialLastSeen_NilWritesNothing(t *testing.T) {
	spawned := 0
	ctx := context.WithValue(context.Background(), agentCredentialLastSeenSpawnKey{}, func(write func()) {
		spawned++
		write()
	})
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	rec := &credLastSeenRecorder{}
	assert.NotPanics(t, func() {
		touchAgentCredentialLastSeen(ctx, nil, &store.AgentCredential{ID: "cred-1"}, now)
		touchAgentCredentialLastSeen(ctx, rec, nil, now)
		touchAgentCredentialLastSeen(ctx, nil, nil, now)
	})
	assert.Zero(t, spawned, "no write was started")
	assert.Zero(t, rec.writes())
}
