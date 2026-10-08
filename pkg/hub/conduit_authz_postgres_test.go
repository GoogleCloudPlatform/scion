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
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
)

// Postgres (P-tier) tests of the user-stream re-check, criterion 16b: two
// hub nodes on one Postgres database, each with its own store connection
// and LISTEN/NOTIFY event publisher. Node B holds the user streams (it owns
// their authorization); revocations are committed through node A's API.
// They need SCION_TEST_POSTGRES_DSN and skip without it; the
// test-conduit-authz-postgres make target fails on a skip.

// notifyBound is the 16b NOTIFY-path bound (p99 ≤ 5s).
const notifyBound = 5 * time.Second

// withDSNParam returns dsn with a connection parameter set.
func withDSNParam(t *testing.T, dsn, key, value string) string {
	t.Helper()
	if !strings.Contains(dsn, "://") {
		return dsn + " " + key + "=" + value
	}
	u, err := url.Parse(dsn)
	require.NoError(t, err)
	q := u.Query()
	q.Set(key, value)
	u.RawQuery = q.Encode()
	return u.String()
}

// pgTwoNodes is the two-node fixture.
type pgTwoNodes struct {
	dsn        string // schema-scoped
	nodeA      *Server
	nodeB      *Server
	pubA, pubB *PostgresEventPublisher
	appB       string // application_name of node B's connections
	clk        *clock.Fake
	metrics    *metricWaiter
	authzB     *conduitStreamAuthz

	agent        *store.Agent
	attachRoleID string
	attachBind   map[string]string
}

func newPGTwoNodes(t *testing.T) *pgTwoNodes {
	t.Helper()
	base := requirePostgres(t)
	ctx := context.Background()

	schema := "conduit_authz_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	admin, err := pgx.Connect(ctx, base)
	require.NoError(t, err)
	_, err = admin.Exec(ctx, "CREATE SCHEMA "+schema)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
		_ = admin.Close(context.Background())
	})

	f := &pgTwoNodes{
		dsn:        withDSNParam(t, base, "search_path", schema),
		appB:       "conduit-authz-node-b-" + schema[len(schema)-8:],
		clk:        clock.NewFake(time.Date(2026, 10, 8, 2, 0, 0, 0, time.UTC)),
		metrics:    newMetricWaiter(),
		attachBind: map[string]string{},
	}
	openStore := func() store.Store {
		client, err := entc.OpenPostgres(f.dsn, entc.PoolConfig{MaxOpenConns: 8})
		require.NoError(t, err)
		s := entadapter.NewCompositeStore(client)
		require.NoError(t, s.Migrate(ctx))
		return s
	}
	f.nodeA, _ = testServerWithStore(t, openStore())
	f.nodeB, _ = testServerWithStore(t, openStore())
	for _, n := range []*Server{f.nodeA, f.nodeB} {
		setConduitExperiment(t, n, true)
	}

	f.pubA, err = NewPostgresEventPublisher(ctx, f.dsn, nil, nil)
	require.NoError(t, err)
	t.Cleanup(f.pubA.Close)
	f.pubB, err = NewPostgresEventPublisher(ctx, withDSNParam(t, f.dsn, "application_name", f.appB), nil, nil)
	require.NoError(t, err)
	t.Cleanup(f.pubB.Close)
	f.nodeA.SetEventPublisher(f.pubA)
	f.nodeB.SetEventPublisher(f.pubB)

	f.seed(t)

	var m conduitStreamAuthzMetrics = f.metrics
	f.nodeB.conduitAuthzMetrics.Store(&m)
	runCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	f.authzB = f.nodeB.startConduitStreamAuthz(runCtx, f.clk, -1)
	f.waitListening(t)
	return f
}

// seed creates the project, the agent and its owner through node A's
// store.
func (f *pgTwoNodes) seed(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	s := f.nodeA.store
	project := &store.Project{ID: tid("pg-conduit-project"), Name: "PG Conduit", Slug: "pg-conduit"}
	require.NoError(t, s.CreateProject(ctx, project))
	ownerID := tid("pg-conduit-owner")
	createTestUserWithProjectRole(t, s, ownerID, "owner@pg.test", project.ID, store.ProjectRoleMember)
	ensureHubMembership(ctx, s, ownerID)
	f.agent = &store.Agent{
		ID: tid("pg-conduit-agent"), Slug: "pg-conduit-agent", Name: "PG Agent",
		ProjectID: project.ID, OwnerID: ownerID, Ancestry: []string{ownerID},
		RuntimeBrokerID: "broker-1", Phase: string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, f.agent))
	rd, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name: "pg-conduit-attach", ScopeType: store.RoleScopeProject, Permissions: []string{"agent.attach"}})
	require.NoError(t, err)
	f.attachRoleID = rd.ID
}

// newUser creates a project member holding a custom attach role.
func (f *pgTwoNodes) newUser(t *testing.T, name string) *AuthenticatedUser {
	t.Helper()
	ctx := context.Background()
	s := f.nodeA.store
	id := tid("pg-recheck-" + name)
	email := name + "@pg.test"
	createTestUserWithProjectRole(t, s, id, email, f.agent.ProjectID, store.ProjectRoleMember)
	ensureHubMembership(ctx, s, id)
	b, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: f.attachRoleID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: id,
		ScopeType: store.RoleScopeProject, ScopeID: f.agent.ProjectID, CreatedBy: "test",
	})
	require.NoError(t, err)
	f.attachBind[id] = b.ID
	return NewAuthenticatedUser(id, email, name, store.UserRoleMember, "api")
}

// open tracks a PTY stream on node B.
func (f *pgTwoNodes) open(t *testing.T, ident Identity, id uint32) *streamWatch {
	t.Helper()
	require.NoError(t, f.nodeB.authorizeConduitAction(context.Background(), ident, f.agent, ActionAttach))
	w := &streamWatch{closed: make(chan string, 1)}
	untrack := f.nodeB.trackConduitUserStream(&conduitUserStream{
		Kind: grant.StreamKindPTY, Identity: ident, AgentID: f.agent.ID, ProjectID: f.agent.ProjectID,
		SessionID: "sess-b", StreamID: id, Close: w.close,
	})
	t.Cleanup(untrack)
	return w
}

// waitListening returns once node B receives conduit events published by
// node A (its LISTEN connection covers the global channel).
func (f *pgTwoNodes) waitListening(t *testing.T) {
	t.Helper()
	ch, unsubscribe := f.pubB.Subscribe(conduitAuthzChangedSubject)
	defer unsubscribe()
	marker := conduitAuthzMatch{AgentID: "listen-probe-" + uuid.NewString()}
	deadline := time.After(30 * time.Second)
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		f.pubA.PublishRaw(conduitAuthzChangedSubject, marker)
		select {
		case evt := <-ch:
			if strings.Contains(string(evt.Data), marker.AgentID) {
				return
			}
		case <-tick.C:
		case <-deadline:
			t.Fatal("node B never received node A's events")
		}
	}
}

// closeWithin waits for w's close and checks it arrived within bound of
// start.
func closeWithin(t *testing.T, w *streamWatch, start time.Time, bound time.Duration) string {
	t.Helper()
	c := w.waitClosed(t)
	assert.LessOrEqual(t, time.Since(start), bound, "close took longer than %s", bound)
	return c
}

// TestConduitAuthzPostgres_NotifyAcrossNodes (R1, R4): U1's attach is
// removed through node A's API; node B closes U1's stream with 4401
// authz_expired within the notify bound, trigger=notify. U2's stream to
// the same agent stays open.
func TestConduitAuthzPostgres_NotifyAcrossNodes(t *testing.T) {
	f := newPGTwoNodes(t)
	u1, u2 := f.newUser(t, "u1"), f.newUser(t, "u2")
	w1 := f.open(t, u1, 1)
	w2 := f.open(t, u2, 3)

	start := time.Now()
	rec := doRequest(t, f.nodeA, http.MethodDelete, "/api/v1/admin/role-bindings/"+f.attachBind[u1.ID()], nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	assert.Equal(t, "4401 authz_expired", closeWithin(t, w1, start, notifyBound))
	f.metrics.wait(t, "notify/closed/pty", 1)
	w2.assertOpen(t)
	assert.Equal(t, 1, f.authzB.Len())
}

// TestConduitAuthzPostgres_UserSuspendDeleteAndTokenRevoke (R5): (a)
// suspending U1, and separately deleting U2, through node A closes their
// streams on node B with 4401, trigger=notify; (b) revoking the access
// token U3 opened its stream with, while U3 stays active and permitted,
// runs a notify re-check that passes and leaves the stream open.
func TestConduitAuthzPostgres_UserSuspendDeleteAndTokenRevoke(t *testing.T) {
	f := newPGTwoNodes(t)
	u1, u2, u3 := f.newUser(t, "u1"), f.newUser(t, "u2"), f.newUser(t, "u3")
	w1 := f.open(t, u1, 1)
	w2 := f.open(t, u2, 3)

	start := time.Now()
	rec := doRequest(t, f.nodeA, http.MethodPatch, "/api/v1/users/"+u1.ID(), map[string]string{"status": store.UserStatusSuspended})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "4401 authz_expired", closeWithin(t, w1, start, notifyBound))
	w2.assertOpen(t)

	start = time.Now()
	rec = doRequest(t, f.nodeA, http.MethodDelete, "/api/v1/users/"+u2.ID(), nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.Equal(t, "4401 authz_expired", closeWithin(t, w2, start, notifyBound))
	f.metrics.wait(t, "notify/closed/pty", 2)

	key, tok, err := f.nodeA.uatService.CreateToken(rs4MintContext(u3.ID()), u3.ID(), "stream", f.agent.ProjectID, []string{"agent:attach"}, nil)
	require.NoError(t, err)
	ident, err := f.nodeB.uatService.ValidateToken(context.Background(), key)
	require.NoError(t, err)
	w3 := f.open(t, ident, 5)
	require.NoError(t, f.nodeA.uatService.RevokeToken(rs4MintContext(u3.ID()), u3.ID(), tok.ID))
	// The handler publishes after the service call; do the same here.
	f.nodeA.publishConduitAuthzChanged(conduitAuthzMatch{UserID: u3.ID()})
	f.metrics.wait(t, "notify/passed/pty", 1)
	w3.assertOpen(t)
}

// TestConduitAuthzPostgres_ListenGapResync (R8, blocking): node B's LISTEN
// connection is dropped and held down; U1's attach is removed through
// node A during the gap, so its notification is lost; when the listener
// reconnects, the resync closes U1's stream with 4401, trigger=resync,
// within the backstop bound (sweep interval + 10s).
func TestConduitAuthzPostgres_ListenGapResync(t *testing.T) {
	f := newPGTwoNodes(t)
	u1, u2 := f.newUser(t, "u1"), f.newUser(t, "u2")
	w1 := f.open(t, u1, 1)
	w2 := f.open(t, u2, 3)

	// Hold node B's listener down once its connection is dropped.
	release := make(chan struct{})
	held := make(chan struct{}, 1)
	var once sync.Once
	f.pubB.mu.Lock()
	f.pubB.testHookBeforeConnect = func() {
		once.Do(func() {
			held <- struct{}{}
			<-release
		})
	}
	f.pubB.mu.Unlock()
	ctx := context.Background()
	_, err := f.pubA.pool.Exec(ctx,
		`SELECT pg_terminate_backend(pid) FROM pg_stat_activity
		 WHERE application_name = $1 AND query ILIKE 'LISTEN %'`, f.appB)
	require.NoError(t, err)
	select {
	case <-held:
	case <-time.After(30 * time.Second):
		t.Fatal("node B's listener did not drop")
	}

	start := time.Now()
	rec := doRequest(t, f.nodeA, http.MethodDelete, "/api/v1/admin/role-bindings/"+f.attachBind[u1.ID()], nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	w1.assertOpen(t)

	close(release)
	assert.Equal(t, "4401 authz_expired", closeWithin(t, w1, start, defaultConduitAuthzRecheckInterval+10*time.Second))
	f.metrics.wait(t, "resync/closed/pty", 1)
	assert.NotContains(t, f.metrics.list(), "notify/closed/pty", "the revocation's notification should have been missed")
	w2.assertOpen(t)
}

// TestConduitAuthzPostgres_BulkRevocation (R10): 50 streams across 10
// users on node B; 5 users are suspended through node A. Exactly their
// 25 streams close, each within the notify bound with trigger=notify;
// the other streams are not re-checked at all.
func TestConduitAuthzPostgres_BulkRevocation(t *testing.T) {
	f := newPGTwoNodes(t)
	type userStreams struct {
		user    *AuthenticatedUser
		streams []*streamWatch
	}
	var all []userStreams
	var id uint32 = 1
	for i := 0; i < 10; i++ {
		us := userStreams{user: f.newUser(t, fmt.Sprintf("bulk%d", i))}
		for j := 0; j < 5; j++ {
			us.streams = append(us.streams, f.open(t, us.user, id))
			id += 2
		}
		all = append(all, us)
	}

	start := time.Now()
	for _, us := range all[:5] {
		rec := doRequest(t, f.nodeA, http.MethodPatch, "/api/v1/users/"+us.user.ID(), map[string]string{"status": store.UserStatusSuspended})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}
	for _, us := range all[:5] {
		for _, w := range us.streams {
			assert.Equal(t, "4401 authz_expired", closeWithin(t, w, start, notifyBound))
		}
	}
	f.metrics.wait(t, "notify/closed/pty", 25)
	for _, us := range all[5:] {
		for _, w := range us.streams {
			w.assertOpen(t)
		}
	}
	for _, m := range f.metrics.list() {
		assert.Equal(t, "notify/closed/pty", m, "an unaffected stream was re-checked")
	}
	assert.Equal(t, 25, f.authzB.Len())
}
