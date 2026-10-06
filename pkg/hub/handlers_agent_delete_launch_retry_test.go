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

// This file covers design §6 test case H-3: soft delete during a launch
// succeeds via its re-read retry when a terminal write lands in between.
package hub

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// deleteCountingEventPublisher counts PublishAgentDeleted calls, so a test
// can assert a concurrent double-delete only ever publishes once.
type deleteCountingEventPublisher struct {
	noopEventPublisher
	mu    sync.Mutex
	count int
}

func (p *deleteCountingEventPublisher) PublishAgentDeleted(_ context.Context, _, _ string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.count++
}

func (p *deleteCountingEventPublisher) Count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.count
}

func TestPerformAgentDelete_H3_RetriesOnVersionConflictFromLaunchTerminal(t *testing.T) {
	srv, s := testServer(t)
	srv.config.SoftDeleteRetention = 24 * time.Hour // force the soft-delete branch

	ctx := context.Background()
	project := &store.Project{
		ID: tid("h3-project"), Slug: "h3-project", Name: "H3 Project",
		GitRemote: "https://github.com/test/h3", Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))

	adminID := tid("h3-admin")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: adminID, Email: "h3-admin@test.com", DisplayName: "H3 Admin", Role: "member", Status: "active",
	}))
	// The owner relationship requires active project access
	// (ptone/scion#2141); the binding grants no permission itself.
	grantProjectAccessOnly(t, s, adminID, project.ID)
	agent := &store.Agent{
		ID: tid("h3-agent"), Slug: "h3-agent", Name: "H3 Agent",
		ProjectID: project.ID, Phase: string(state.PhaseCreated),
		StateVersion: 1, Created: time.Now(), Updated: time.Now(),
		CreatedBy: adminID,
		OwnerID:   adminID, // authz's resource-owner bypass authorizes ActionDelete for this caller.
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	// The caller's snapshot, as the deleteAgent HTTP handler would have read
	// it before calling performAgentDelete.
	stale, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)

	// A launch terminal report lands and bumps state_version, simulating the
	// race window between the delete handler's read and its own write
	// (design §3.3: "delete during a launch is expected").
	_, err = s.BeginLaunch(ctx, agent.ID, store.LaunchKindCreate, 5*time.Minute)
	require.NoError(t, err)
	launchID, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	_, _, err = s.ApplyLaunchReport(ctx, agent.ID, agent.RuntimeBrokerID, store.LaunchReport{
		LaunchID: launchID.LaunchID, InstanceID: "instance-1", State: store.LaunchReportStateFailed,
		Step: "pod_create", Message: "boom", ErrorCode: "runtime_error",
	})
	require.NoError(t, err)

	// stale.StateVersion no longer matches the row.
	after, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	require.NotEqual(t, stale.StateVersion, after.StateVersion, "the terminal report must have bumped state_version")

	admin := NewAuthenticatedUser(adminID, "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/agents/"+agent.ID, nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rec := httptest.NewRecorder()

	srv.performAgentDelete(rec, req, stale)

	assert.Equal(t, http.StatusNoContent, rec.Code, "the delete must succeed despite the stale snapshot, not surface the conflict: %s", rec.Body.String())

	// The failed launch report left the row an incomplete async create
	// (kind create, phase error, launch_error set). Under acceptance (bb) and
	// T1 §0c such a row is always hard-deleted, even with soft-delete
	// retention configured, so its name can be reused at once. Before the
	// delete engine (ptone/scion#2483) this test asserted a soft delete; the
	// stale-snapshot race it targets is kept by
	// TestPerformAgentDelete_H3_StaleSnapshotRaceOnPlainCreatedRow.
	_, err = s.GetAgent(ctx, agent.ID)
	assert.ErrorIs(t, err, store.ErrNotFound, "an incomplete create must be hard-deleted")
	_, err = s.GetAgentBySlug(ctx, project.ID, agent.Slug)
	assert.ErrorIs(t, err, store.ErrNotFound, "the slug must be free for reuse")
}

// TestPerformAgentDelete_H3_StaleSnapshotRaceOnPlainCreatedRow keeps H-3's
// original intent on a row that is not an incomplete create: a concurrent
// write bumps state_version between the caller's read and the delete, and
// the delete still completes exactly once — soft-deleted, one deleted event,
// one quota release.
func TestPerformAgentDelete_H3_StaleSnapshotRaceOnPlainCreatedRow(t *testing.T) {
	srv, s := testServer(t)
	srv.config.SoftDeleteRetention = 24 * time.Hour

	events := &deleteCountingEventPublisher{}
	srv.events = events

	ctx := context.Background()
	project := &store.Project{
		ID: tid("h3c-project"), Slug: "h3c-project", Name: "H3c Project",
		GitRemote: "https://github.com/test/h3c", Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	adminID := tid("h3c-admin")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: adminID, Email: "h3c-admin@test.com", DisplayName: "H3c Admin", Role: "member", Status: "active",
	}))
	// The owner relationship requires active project access
	// (ptone/scion#2141); the binding grants no permission itself.
	grantProjectAccessOnly(t, s, adminID, project.ID)
	agent := &store.Agent{
		ID: tid("h3c-agent"), Slug: "h3c-agent", Name: "H3c Agent",
		ProjectID: project.ID, Phase: string(state.PhaseCreated),
		StateVersion: 1, Created: time.Now(), Updated: time.Now(),
		CreatedBy: adminID, OwnerID: adminID,
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	stale, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)

	// A concurrent full-row write lands between the read and the delete.
	racer, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	racer.Message = "concurrent write"
	require.NoError(t, s.UpdateAgent(ctx, racer))
	require.NotEqual(t, stale.StateVersion, racer.StateVersion)

	releases := countQuotaReleases(t, srv)

	admin := NewAuthenticatedUser(adminID, "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/agents/"+agent.ID, nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rec := httptest.NewRecorder()
	srv.performAgentDelete(rec, req, stale)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	final, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.False(t, final.DeletedAt.IsZero(), "the agent must end up soft-deleted")
	assert.Equal(t, string(state.PhaseStopped), final.Phase)
	assert.Equal(t, "concurrent write", final.Message, "the delete must not overwrite the concurrent write")
	assert.Equal(t, 1, events.Count(), "exactly one deleted event")
	assert.Equal(t, 1, releases(), "exactly one quota release")

	// A redundant second delete on the same stale snapshot is a 204 no-op.
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodDelete, "/api/v1/agents/"+agent.ID, nil)
	srv.performAgentDelete(rec2, req2.WithContext(contextWithIdentity(req2.Context(), admin)), stale)
	assert.Equal(t, http.StatusNoContent, rec2.Code, rec2.Body.String())
	assert.Equal(t, 1, events.Count())
	assert.Equal(t, 1, releases())
}

// TestPerformAgentDelete_H3_ConcurrentDoubleDeleteShortCircuits verifies that
// the retry's version-conflict branch is not specific to a launch terminal
// write. When the re-read finds the row already soft-deleted by
// another concurrent delete, it must adopt that row's DeletedAt (not
// overwrite it, which would shift the retention/purge window) and must not
// publish a second AgentDeleted event for the same delete.
func TestPerformAgentDelete_H3_ConcurrentDoubleDeleteShortCircuits(t *testing.T) {
	srv, s := testServer(t)
	srv.config.SoftDeleteRetention = 24 * time.Hour // force the soft-delete branch

	events := &deleteCountingEventPublisher{}
	srv.events = events

	ctx := context.Background()
	project := &store.Project{
		ID: tid("h3b-project"), Slug: "h3b-project", Name: "H3b Project",
		GitRemote: "https://github.com/test/h3b", Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))

	adminID := tid("h3b-admin")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: adminID, Email: "h3b-admin@test.com", DisplayName: "H3b Admin", Role: "member", Status: "active",
	}))
	// The owner relationship requires active project access
	// (ptone/scion#2141); the binding grants no permission itself.
	grantProjectAccessOnly(t, s, adminID, project.ID)
	agent := &store.Agent{
		ID: tid("h3b-agent"), Slug: "h3b-agent", Name: "H3b Agent",
		ProjectID: project.ID, Phase: string(state.PhaseCreated),
		StateVersion: 1, Created: time.Now(), Updated: time.Now(),
		CreatedBy: adminID,
		OwnerID:   adminID,
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	// Both concurrent callers read the same pre-delete state independently —
	// two separate GetAgent calls, like two separate HTTP requests would
	// make, rather than sharing one *store.Agent (UpdateAgent mutates its
	// argument's StateVersion in place on success, so sharing a pointer
	// across both calls would make the second call see the already-bumped
	// version and never hit the conflict this test targets).
	stale1, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	stale2, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)

	admin := NewAuthenticatedUser(adminID, "admin@example.com", "Admin", "admin", "cli")
	newDeleteReq := func() *http.Request {
		req := httptest.NewRequest(http.MethodDelete, "/api/v1/agents/"+agent.ID, nil)
		return req.WithContext(contextWithIdentity(req.Context(), admin))
	}

	// First caller's delete lands via the fast path (no conflict yet).
	rec1 := httptest.NewRecorder()
	srv.performAgentDelete(rec1, newDeleteReq(), stale1)
	require.Equal(t, http.StatusNoContent, rec1.Code, "first delete must succeed: %s", rec1.Body.String())

	firstDeleted, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	require.False(t, firstDeleted.DeletedAt.IsZero())
	require.Equal(t, 1, events.Count(), "the first delete must publish exactly once")

	// Second caller races on its own pre-delete snapshot: its first
	// UpdateAgent hits a version conflict (the first delete already bumped
	// state_version), and its re-read finds the row already soft-deleted.
	rec2 := httptest.NewRecorder()
	srv.performAgentDelete(rec2, newDeleteReq(), stale2)
	assert.Equal(t, http.StatusNoContent, rec2.Code, "a concurrent double delete must not surface a conflict to the second caller: %s", rec2.Body.String())

	final, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.True(t, final.DeletedAt.Equal(firstDeleted.DeletedAt), "the second delete must not overwrite the first delete's DeletedAt")
	assert.Equal(t, 1, events.Count(), "the second, redundant delete must not publish AgentDeleted again")
}

// quotaReleaseCountingStore counts project quota releases: every
// releaseAgentQuotas call looks up max_agents_per_project once.
type quotaReleaseCountingStore struct {
	store.Store
	mu    sync.Mutex
	count int
}

func (c *quotaReleaseCountingStore) GetLimitDefinitionByName(ctx context.Context, name string) (*store.LimitDefinition, error) {
	if name == "max_agents_per_project" {
		c.mu.Lock()
		c.count++
		c.mu.Unlock()
	}
	return c.Store.GetLimitDefinitionByName(ctx, name)
}

// countQuotaReleases swaps srv's quota service for one that counts
// releaseAgentQuotas calls and returns the counter.
func countQuotaReleases(t *testing.T, srv *Server) func() int {
	t.Helper()
	cs := &quotaReleaseCountingStore{Store: srv.store}
	srv.quotaService = &QuotaService{store: cs, logger: slog.Default()}
	return func() int {
		cs.mu.Lock()
		defer cs.mu.Unlock()
		return cs.count
	}
}
