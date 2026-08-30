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
	"fmt"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestProjectStore(t *testing.T) *ProjectStore {
	t.Helper()
	client := enttest.NewClient(t)
	return NewProjectStore(client)
}

func newProject(seq int) *store.Project {
	id := uuid.NewString()
	return &store.Project{
		ID:     id,
		Name:   "Project " + id[:8],
		Slug:   "project-" + id[:8],
		Labels: map[string]string{"seq": id[:4]},
	}
}

func TestProject_CreateGet(t *testing.T) {
	ps := newTestProjectStore(t)
	ctx := context.Background()

	p := newProject(1)
	p.GitRemote = "https://github.com/acme/repo.git"
	p.OwnerID = uuid.NewString()
	require.NoError(t, ps.CreateProject(ctx, p))
	assert.False(t, p.Created.IsZero())
	assert.False(t, p.Updated.IsZero())

	got, err := ps.GetProject(ctx, p.ID)
	require.NoError(t, err)
	assert.Equal(t, p.ID, got.ID)
	assert.Equal(t, p.Name, got.Name)
	assert.Equal(t, p.Slug, got.Slug)
	assert.Equal(t, "https://github.com/acme/repo.git", got.GitRemote)
	assert.Equal(t, store.ProjectTypeHubManaged, got.ProjectType) // computed default
}

func TestProject_CreateDuplicateSlug(t *testing.T) {
	ps := newTestProjectStore(t)
	ctx := context.Background()

	p1 := newProject(1)
	p1.Slug = "dup-slug"
	require.NoError(t, ps.CreateProject(ctx, p1))

	p2 := newProject(2)
	p2.Slug = "dup-slug"
	err := ps.CreateProject(ctx, p2)
	assert.ErrorIs(t, err, store.ErrAlreadyExists)
}

func TestProject_GetNotFound(t *testing.T) {
	ps := newTestProjectStore(t)
	_, err := ps.GetProject(context.Background(), uuid.NewString())
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestProject_GetBySlugCaseInsensitive(t *testing.T) {
	ps := newTestProjectStore(t)
	ctx := context.Background()

	p := newProject(1)
	p.Slug = "MixedCase-Slug"
	require.NoError(t, ps.CreateProject(ctx, p))

	got, err := ps.GetProjectBySlugCaseInsensitive(ctx, "mixedcase-slug")
	require.NoError(t, err)
	assert.Equal(t, p.ID, got.ID)

	// Exact (case-sensitive) lookup must not match a different case.
	_, err = ps.GetProjectBySlug(ctx, "mixedcase-slug")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestProject_GetByGitRemote(t *testing.T) {
	ps := newTestProjectStore(t)
	ctx := context.Background()

	remote := "https://github.com/acme/shared.git"
	for i := 0; i < 2; i++ {
		p := newProject(i)
		p.GitRemote = remote
		require.NoError(t, ps.CreateProject(ctx, p))
	}
	other := newProject(99)
	other.GitRemote = "https://github.com/acme/other.git"
	require.NoError(t, ps.CreateProject(ctx, other))

	got, err := ps.GetProjectsByGitRemote(ctx, remote)
	require.NoError(t, err)
	assert.Len(t, got, 2)

	none, err := ps.GetProjectsByGitRemote(ctx, "https://github.com/none.git")
	require.NoError(t, err)
	assert.Empty(t, none)
}

func TestProject_NextAvailableSlug(t *testing.T) {
	ps := newTestProjectStore(t)
	ctx := context.Background()

	slug, err := ps.NextAvailableSlug(ctx, "myproj")
	require.NoError(t, err)
	assert.Equal(t, "myproj", slug)

	p := newProject(1)
	p.Slug = "myproj"
	require.NoError(t, ps.CreateProject(ctx, p))

	slug, err = ps.NextAvailableSlug(ctx, "myproj")
	require.NoError(t, err)
	assert.Equal(t, "myproj-1", slug)

	p2 := newProject(2)
	p2.Slug = "myproj-1"
	require.NoError(t, ps.CreateProject(ctx, p2))

	slug, err = ps.NextAvailableSlug(ctx, "myproj")
	require.NoError(t, err)
	assert.Equal(t, "myproj-2", slug)
}

func TestProject_Update(t *testing.T) {
	ps := newTestProjectStore(t)
	ctx := context.Background()

	p := newProject(1)
	require.NoError(t, ps.CreateProject(ctx, p))

	p.Name = "Renamed"
	p.GitRemote = "https://github.com/acme/renamed.git"
	installID := int64(424242)
	p.GitHubInstallationID = &installID
	require.NoError(t, ps.UpdateProject(ctx, p))

	got, err := ps.GetProject(ctx, p.ID)
	require.NoError(t, err)
	assert.Equal(t, "Renamed", got.Name)
	assert.Equal(t, "https://github.com/acme/renamed.git", got.GitRemote)
	require.NotNil(t, got.GitHubInstallationID)
	assert.Equal(t, int64(424242), *got.GitHubInstallationID)
}

func TestProject_SharedDirsRoundTrip(t *testing.T) {
	ps := newTestProjectStore(t)
	ctx := context.Background()

	p := newProject(1)
	p.SharedDirs = []api.SharedDir{{Name: "build-cache", ReadOnly: true, InWorkspace: true}}
	require.NoError(t, ps.CreateProject(ctx, p))

	got, err := ps.GetProject(ctx, p.ID)
	require.NoError(t, err)
	require.Len(t, got.SharedDirs, 1)
	assert.Equal(t, "build-cache", got.SharedDirs[0].Name)
	assert.True(t, got.SharedDirs[0].ReadOnly)
	assert.True(t, got.SharedDirs[0].InWorkspace)
}

func TestProject_Delete(t *testing.T) {
	ps := newTestProjectStore(t)
	ctx := context.Background()

	p := newProject(1)
	require.NoError(t, ps.CreateProject(ctx, p))
	require.NoError(t, ps.DeleteProject(ctx, p.ID))

	_, err := ps.GetProject(ctx, p.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)

	assert.ErrorIs(t, ps.DeleteProject(ctx, p.ID), store.ErrNotFound)
}

func TestProject_ListFilters(t *testing.T) {
	ps := newTestProjectStore(t)
	ctx := context.Background()

	owner := uuid.NewString()
	pub := newProject(1)
	pub.OwnerID = owner
	require.NoError(t, ps.CreateProject(ctx, pub))

	priv := newProject(2)
	require.NoError(t, ps.CreateProject(ctx, priv))

	all, err := ps.ListProjects(ctx, store.ProjectFilter{}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 2, all.TotalCount)

	byOwner, err := ps.ListProjects(ctx, store.ProjectFilter{OwnerID: owner}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, byOwner.TotalCount)

	byName, err := ps.ListProjects(ctx, store.ProjectFilter{Name: pub.Name}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, byName.TotalCount)

	limited, err := ps.ListProjects(ctx, store.ProjectFilter{}, store.ListOptions{Limit: 1})
	require.NoError(t, err)
	assert.Len(t, limited.Items, 1)
	assert.Equal(t, 2, limited.TotalCount)
}

func TestProject_ComputedAgentCount(t *testing.T) {
	ps := newTestProjectStore(t)
	ctx := context.Background()

	p := newProject(1)
	require.NoError(t, ps.CreateProject(ctx, p))
	uid := uuid.MustParse(p.ID)

	for i := 0; i < 3; i++ {
		_, err := ps.client.Agent.Create().
			SetID(uuid.New()).
			SetName("agent").
			SetSlug("agent-" + uuid.NewString()[:8]).
			SetProjectID(uid).
			Save(ctx)
		require.NoError(t, err)
	}

	got, err := ps.GetProject(ctx, p.ID)
	require.NoError(t, err)
	assert.Equal(t, 3, got.AgentCount)
}

func TestProject_ProjectTypeLinked(t *testing.T) {
	ps := newTestProjectStore(t)
	ctx := context.Background()

	p := newProject(1)
	require.NoError(t, ps.CreateProject(ctx, p))

	// A contributor with a local path outside ~/.scion/projects/ marks it linked.
	require.NoError(t, ps.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  p.ID,
		BrokerID:   uuid.NewString(),
		BrokerName: "broker-1",
		LocalPath:  "/home/user/code/myrepo/.scion",
		Status:     store.BrokerStatusOnline,
	}))

	got, err := ps.GetProject(ctx, p.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ProjectTypeLinked, got.ProjectType)
}

// =============================================================================
// RuntimeBroker
// =============================================================================

func newBroker() *store.RuntimeBroker {
	id := uuid.NewString()
	return &store.RuntimeBroker{
		ID:      id,
		Name:    "broker-" + id[:8],
		Slug:    "broker-" + id[:8],
		Version: "1.0.0",
		Status:  store.BrokerStatusOnline,
	}
}

func TestBroker_CreateGet(t *testing.T) {
	ps := newTestProjectStore(t)
	ctx := context.Background()

	b := newBroker()
	b.Capabilities = &store.BrokerCapabilities{WebPTY: true, Sync: true}
	b.Profiles = []store.BrokerProfile{{Name: "docker-default", Type: "docker", Available: true}}
	b.AutoProvide = true
	require.NoError(t, ps.CreateRuntimeBroker(ctx, b))
	assert.False(t, b.Created.IsZero())

	got, err := ps.GetRuntimeBroker(ctx, b.ID)
	require.NoError(t, err)
	assert.Equal(t, b.Name, got.Name)
	assert.Equal(t, "1.0.0", got.Version)
	assert.Equal(t, store.BrokerStatusOnline, got.Status)
	assert.True(t, got.AutoProvide)
	require.NotNil(t, got.Capabilities)
	assert.True(t, got.Capabilities.WebPTY)
	require.Len(t, got.Profiles, 1)
	assert.Equal(t, "docker-default", got.Profiles[0].Name)
}

func TestBroker_GetByName(t *testing.T) {
	ps := newTestProjectStore(t)
	ctx := context.Background()

	b := newBroker()
	b.Name = "MyBroker"
	require.NoError(t, ps.CreateRuntimeBroker(ctx, b))

	got, err := ps.GetRuntimeBrokerByName(ctx, "mybroker")
	require.NoError(t, err)
	assert.Equal(t, b.ID, got.ID)

	_, err = ps.GetRuntimeBrokerByName(ctx, "nonexistent")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestBroker_Update(t *testing.T) {
	ps := newTestProjectStore(t)
	ctx := context.Background()

	b := newBroker()
	require.NoError(t, ps.CreateRuntimeBroker(ctx, b))

	b.Name = "Renamed"
	b.Version = "2.0.0"
	b.Status = store.BrokerStatusDegraded
	require.NoError(t, ps.UpdateRuntimeBroker(ctx, b))

	got, err := ps.GetRuntimeBroker(ctx, b.ID)
	require.NoError(t, err)
	assert.Equal(t, "Renamed", got.Name)
	assert.Equal(t, "2.0.0", got.Version)
	assert.Equal(t, store.BrokerStatusDegraded, got.Status)
}

func TestBroker_UpdateBumpsLockVersion(t *testing.T) {
	ps := newTestProjectStore(t)
	ctx := context.Background()

	b := newBroker()
	require.NoError(t, ps.CreateRuntimeBroker(ctx, b))
	uid := uuid.MustParse(b.ID)

	before, err := ps.client.RuntimeBroker.Get(ctx, uid)
	require.NoError(t, err)

	b.Status = store.BrokerStatusOffline
	require.NoError(t, ps.UpdateRuntimeBroker(ctx, b))

	after, err := ps.client.RuntimeBroker.Get(ctx, uid)
	require.NoError(t, err)
	assert.Equal(t, before.LockVersion+1, after.LockVersion, "update must advance the lock_version CAS token")
}

func TestBroker_Heartbeat(t *testing.T) {
	ps := newTestProjectStore(t)
	ctx := context.Background()

	b := newBroker()
	b.Status = store.BrokerStatusOffline
	require.NoError(t, ps.CreateRuntimeBroker(ctx, b))
	uid := uuid.MustParse(b.ID)
	before, err := ps.client.RuntimeBroker.Get(ctx, uid)
	require.NoError(t, err)

	require.NoError(t, ps.UpdateRuntimeBrokerHeartbeat(ctx, b.ID, store.BrokerStatusOnline))

	got, err := ps.GetRuntimeBroker(ctx, b.ID)
	require.NoError(t, err)
	assert.Equal(t, store.BrokerStatusOnline, got.Status)
	assert.False(t, got.LastHeartbeat.IsZero())

	after, err := ps.client.RuntimeBroker.Get(ctx, uid)
	require.NoError(t, err)
	assert.Equal(t, before.LockVersion+1, after.LockVersion)
}

func TestBroker_HeartbeatNotFound(t *testing.T) {
	ps := newTestProjectStore(t)
	err := ps.UpdateRuntimeBrokerHeartbeat(context.Background(), uuid.NewString(), store.BrokerStatusOnline)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestBroker_Delete(t *testing.T) {
	ps := newTestProjectStore(t)
	ctx := context.Background()

	b := newBroker()
	require.NoError(t, ps.CreateRuntimeBroker(ctx, b))
	require.NoError(t, ps.DeleteRuntimeBroker(ctx, b.ID))
	_, err := ps.GetRuntimeBroker(ctx, b.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestBroker_ListFilters(t *testing.T) {
	ps := newTestProjectStore(t)
	ctx := context.Background()

	online := newBroker()
	online.Status = store.BrokerStatusOnline
	require.NoError(t, ps.CreateRuntimeBroker(ctx, online))

	offline := newBroker()
	offline.Status = store.BrokerStatusOffline
	require.NoError(t, ps.CreateRuntimeBroker(ctx, offline))

	all, err := ps.ListRuntimeBrokers(ctx, store.RuntimeBrokerFilter{}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 2, all.TotalCount)

	byStatus, err := ps.ListRuntimeBrokers(ctx, store.RuntimeBrokerFilter{Status: store.BrokerStatusOnline}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, byStatus.TotalCount)

	yes := true
	autoProvide := newBroker()
	autoProvide.AutoProvide = true
	require.NoError(t, ps.CreateRuntimeBroker(ctx, autoProvide))
	byAuto, err := ps.ListRuntimeBrokers(ctx, store.RuntimeBrokerFilter{AutoProvide: &yes}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, byAuto.TotalCount)
}

// =============================================================================
// ProjectProvider (contributors)
// =============================================================================

func TestProvider_UpsertAndGet(t *testing.T) {
	ps := newTestProjectStore(t)
	ctx := context.Background()

	projectID := uuid.NewString()
	brokerID := uuid.NewString()
	require.NoError(t, ps.CreateProject(ctx, &store.Project{ID: projectID, Name: "p", Slug: "p-" + projectID[:8]}))

	prov := &store.ProjectProvider{
		ProjectID:  projectID,
		BrokerID:   brokerID,
		BrokerName: "broker-a",
		LocalPath:  "/tmp/a",
		Status:     store.BrokerStatusOffline,
		LinkedBy:   uuid.NewString(),
	}
	require.NoError(t, ps.AddProjectProvider(ctx, prov))
	assert.False(t, prov.LinkedAt.IsZero(), "LinkedAt should be set when LinkedBy present")

	got, err := ps.GetProjectProvider(ctx, projectID, brokerID)
	require.NoError(t, err)
	assert.Equal(t, "broker-a", got.BrokerName)
	assert.Equal(t, "/tmp/a", got.LocalPath)
	assert.Equal(t, store.BrokerStatusOffline, got.Status)

	// Upsert (INSERT OR REPLACE): same (project, broker) updates in place.
	prov2 := &store.ProjectProvider{
		ProjectID:  projectID,
		BrokerID:   brokerID,
		BrokerName: "broker-a-renamed",
		LocalPath:  "/tmp/b",
		Status:     store.BrokerStatusOnline,
	}
	require.NoError(t, ps.AddProjectProvider(ctx, prov2))

	providers, err := ps.GetProjectProviders(ctx, projectID)
	require.NoError(t, err)
	require.Len(t, providers, 1, "upsert must not create a duplicate row")
	assert.Equal(t, "broker-a-renamed", providers[0].BrokerName)
	assert.Equal(t, store.BrokerStatusOnline, providers[0].Status)
}

func TestProvider_RemoveAndStatus(t *testing.T) {
	ps := newTestProjectStore(t)
	ctx := context.Background()

	projectID := uuid.NewString()
	brokerID := uuid.NewString()
	require.NoError(t, ps.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: projectID, BrokerID: brokerID, BrokerName: "b", Status: store.BrokerStatusOffline,
	}))

	require.NoError(t, ps.UpdateProviderStatus(ctx, projectID, brokerID, store.BrokerStatusOnline))
	got, err := ps.GetProjectProvider(ctx, projectID, brokerID)
	require.NoError(t, err)
	assert.Equal(t, store.BrokerStatusOnline, got.Status)
	assert.False(t, got.LastSeen.IsZero())

	require.NoError(t, ps.RemoveProjectProvider(ctx, projectID, brokerID))
	_, err = ps.GetProjectProvider(ctx, projectID, brokerID)
	assert.ErrorIs(t, err, store.ErrNotFound)

	assert.ErrorIs(t, ps.RemoveProjectProvider(ctx, projectID, brokerID), store.ErrNotFound)
	assert.ErrorIs(t, ps.UpdateProviderStatus(ctx, projectID, brokerID, store.BrokerStatusOnline), store.ErrNotFound)
}

func TestProvider_GetBrokerProjects(t *testing.T) {
	ps := newTestProjectStore(t)
	ctx := context.Background()

	brokerID := uuid.NewString()
	for i := 0; i < 2; i++ {
		require.NoError(t, ps.AddProjectProvider(ctx, &store.ProjectProvider{
			ProjectID: uuid.NewString(), BrokerID: brokerID, BrokerName: "b", Status: store.BrokerStatusOnline,
		}))
	}
	got, err := ps.GetBrokerProjects(ctx, brokerID)
	require.NoError(t, err)
	assert.Len(t, got, 2)
}

func TestProject_ListByBrokerID(t *testing.T) {
	ps := newTestProjectStore(t)
	ctx := context.Background()

	p := newProject(1)
	require.NoError(t, ps.CreateProject(ctx, p))
	brokerID := uuid.NewString()
	require.NoError(t, ps.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: p.ID, BrokerID: brokerID, BrokerName: "b", Status: store.BrokerStatusOnline,
	}))
	// A second project with no contributor for this broker.
	require.NoError(t, ps.CreateProject(ctx, newProject(2)))

	res, err := ps.ListProjects(ctx, store.ProjectFilter{BrokerID: brokerID}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, res.TotalCount)
	require.Len(t, res.Items, 1)
	assert.Equal(t, p.ID, res.Items[0].ID)
}

// =============================================================================
// ProjectSyncState
// =============================================================================

func TestSyncState_Upsert(t *testing.T) {
	ps := newTestProjectStore(t)
	ctx := context.Background()

	projectID := uuid.NewString()
	now := time.Now().UTC().Truncate(time.Second)
	state := &store.ProjectSyncState{
		ProjectID:     projectID,
		BrokerID:      "", // hub-native, project-wide
		LastSyncTime:  &now,
		LastCommitSHA: "abc123",
		FileCount:     10,
		TotalBytes:    2048,
	}
	require.NoError(t, ps.UpsertProjectSyncState(ctx, state))

	got, err := ps.GetProjectSyncState(ctx, projectID, "")
	require.NoError(t, err)
	assert.Equal(t, "abc123", got.LastCommitSHA)
	assert.Equal(t, 10, got.FileCount)
	assert.Equal(t, int64(2048), got.TotalBytes)
	require.NotNil(t, got.LastSyncTime)

	// Upsert again on the same key updates in place.
	state.FileCount = 20
	state.LastCommitSHA = "def456"
	require.NoError(t, ps.UpsertProjectSyncState(ctx, state))

	states, err := ps.ListProjectSyncStates(ctx, projectID)
	require.NoError(t, err)
	require.Len(t, states, 1)
	assert.Equal(t, 20, states[0].FileCount)
	assert.Equal(t, "def456", states[0].LastCommitSHA)
}

func TestSyncState_PerBroker(t *testing.T) {
	ps := newTestProjectStore(t)
	ctx := context.Background()

	projectID := uuid.NewString()
	brokerID := uuid.NewString()
	require.NoError(t, ps.UpsertProjectSyncState(ctx, &store.ProjectSyncState{ProjectID: projectID, BrokerID: "", FileCount: 1}))
	require.NoError(t, ps.UpsertProjectSyncState(ctx, &store.ProjectSyncState{ProjectID: projectID, BrokerID: brokerID, FileCount: 2}))

	states, err := ps.ListProjectSyncStates(ctx, projectID)
	require.NoError(t, err)
	assert.Len(t, states, 2)

	perBroker, err := ps.GetProjectSyncState(ctx, projectID, brokerID)
	require.NoError(t, err)
	assert.Equal(t, 2, perBroker.FileCount)
}

func TestSyncState_DeleteAndNotFound(t *testing.T) {
	ps := newTestProjectStore(t)
	ctx := context.Background()

	projectID := uuid.NewString()
	_, err := ps.GetProjectSyncState(ctx, projectID, "")
	assert.ErrorIs(t, err, store.ErrNotFound)

	require.NoError(t, ps.UpsertProjectSyncState(ctx, &store.ProjectSyncState{ProjectID: projectID, BrokerID: "", FileCount: 1}))
	require.NoError(t, ps.DeleteProjectSyncState(ctx, projectID, ""))
	assert.ErrorIs(t, ps.DeleteProjectSyncState(ctx, projectID, ""), store.ErrNotFound)
}

// TestListProjects_CursorPagination verifies ListProjects honors ListOptions.Cursor
// and enumerates every project across pages with no gaps or duplicates. Before the
// keyset-pagination fix the cursor was ignored and NextCursor was never set, so a
// caller could only ever see the first (default 50-row) page.
func TestListProjects_CursorPagination(t *testing.T) {
	ps := newTestProjectStore(t)
	ctx := context.Background()

	const total = 125 // more than two pages at pageSize=50
	created := make(map[string]bool, total)
	for i := 0; i < total; i++ {
		p := newProject(i)
		require.NoError(t, ps.CreateProject(ctx, p))
		created[p.ID] = true
	}

	// Use an explicit page size (50) to exercise cursor across multiple pages.
	const pageSize = 50
	first, err := ps.ListProjects(ctx, store.ProjectFilter{}, store.ListOptions{Limit: pageSize})
	require.NoError(t, err)
	assert.LessOrEqual(t, len(first.Items), pageSize, "page must cap at requested limit")
	assert.NotEmpty(t, first.NextCursor, "more pages exist, so NextCursor must be set")

	// Walking the cursor must enumerate every project exactly once.
	seen := make(map[string]bool, total)
	cursor := ""
	for pages := 0; ; pages++ {
		require.LessOrEqual(t, pages, total, "pagination did not terminate")
		page, err := ps.ListProjects(ctx, store.ProjectFilter{}, store.ListOptions{Limit: pageSize, Cursor: cursor})
		require.NoError(t, err)
		for _, p := range page.Items {
			require.False(t, seen[p.ID], "duplicate project across pages: %s", p.ID)
			seen[p.ID] = true
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}

	assert.Len(t, seen, total, "cursor pagination must enumerate every project")
	for id := range created {
		assert.True(t, seen[id], "project missing from pagination: %s", id)
	}
}

// =============================================================================
// AuthorizedProjectIDs fail-closed filter (R1 — LS1 review)
// =============================================================================

// TestProjectStore_AuthorizedProjectIDs verifies that the AuthorizedProjectIDs
// store filter fails closed: empty sets, invalid UUIDs, and nil each behave as
// documented, ensuring scope-aware authorization cannot leak projects.
func TestProjectStore_AuthorizedProjectIDs(t *testing.T) {
	ctx := context.Background()

	setup := func(t *testing.T) (*ProjectStore, string, string) {
		t.Helper()
		ps := newTestProjectStore(t)

		pA := &store.Project{ID: uuid.NewString(), Name: "proj-a", Slug: "proj-a-" + uuid.NewString()[:8]}
		pB := &store.Project{ID: uuid.NewString(), Name: "proj-b", Slug: "proj-b-" + uuid.NewString()[:8]}
		require.NoError(t, ps.CreateProject(ctx, pA))
		require.NoError(t, ps.CreateProject(ctx, pB))

		return ps, pA.ID, pB.ID
	}

	t.Run("nil applies no filter (all projects returned)", func(t *testing.T) {
		ps, _, _ := setup(t)
		result, err := ps.ListProjects(ctx, store.ProjectFilter{
			AuthorizedProjectIDs: nil,
		}, store.ListOptions{})
		require.NoError(t, err)
		assert.Equal(t, 2, result.TotalCount, "nil AuthorizedProjectIDs must not filter")
	})

	t.Run("empty non-nil returns zero results", func(t *testing.T) {
		ps, _, _ := setup(t)
		result, err := ps.ListProjects(ctx, store.ProjectFilter{
			AuthorizedProjectIDs: []string{},
		}, store.ListOptions{})
		require.NoError(t, err)
		assert.Equal(t, 0, result.TotalCount, "empty AuthorizedProjectIDs must return zero projects")
		assert.Empty(t, result.Items)
	})

	t.Run("valid UUID returns only matching project", func(t *testing.T) {
		ps, projA, _ := setup(t)
		result, err := ps.ListProjects(ctx, store.ProjectFilter{
			AuthorizedProjectIDs: []string{projA},
		}, store.ListOptions{})
		require.NoError(t, err)
		assert.Equal(t, 1, result.TotalCount, "should return only proj-a")
		require.Len(t, result.Items, 1)
		assert.Equal(t, projA, result.Items[0].ID)
	})

	t.Run("invalid UUID returns zero results (fail closed)", func(t *testing.T) {
		ps, _, _ := setup(t)
		result, err := ps.ListProjects(ctx, store.ProjectFilter{
			AuthorizedProjectIDs: []string{"not-a-uuid"},
		}, store.ListOptions{})
		require.NoError(t, err)
		assert.Equal(t, 0, result.TotalCount, "invalid UUID must fail closed — no projects visible")
		assert.Empty(t, result.Items)
	})

	t.Run("mix of valid and invalid UUIDs returns only valid matches", func(t *testing.T) {
		ps, projA, _ := setup(t)
		result, err := ps.ListProjects(ctx, store.ProjectFilter{
			AuthorizedProjectIDs: []string{projA, "garbage"},
		}, store.ListOptions{})
		require.NoError(t, err)
		assert.Equal(t, 1, result.TotalCount, "only the valid UUID should match")
		require.Len(t, result.Items, 1)
		assert.Equal(t, projA, result.Items[0].ID)
	})

	t.Run("multiple valid UUIDs returns both projects", func(t *testing.T) {
		ps, projA, projB := setup(t)
		result, err := ps.ListProjects(ctx, store.ProjectFilter{
			AuthorizedProjectIDs: []string{projA, projB},
		}, store.ListOptions{})
		require.NoError(t, err)
		assert.Equal(t, 2, result.TotalCount, "both projects should be returned")
	})
}

// TestListProjects_MaxLimit verifies that ListProjects with a Limit exceeding
// 1000 is capped at maxProjectListLimit=1000.
func TestListProjects_MaxLimit(t *testing.T) {
	ps := newTestProjectStore(t)
	ctx := context.Background()

	// Create 1010 projects — just over the max limit.
	const total = 1010
	for i := 0; i < total; i++ {
		p := &store.Project{
			ID:   uuid.NewString(),
			Name: fmt.Sprintf("proj-%04d", i),
			Slug: fmt.Sprintf("proj-%04d", i),
		}
		require.NoError(t, ps.CreateProject(ctx, p))
	}

	// Requesting a limit above the max (2000) should cap at 1000.
	result, err := ps.ListProjects(ctx, store.ProjectFilter{}, store.ListOptions{Limit: 2000})
	require.NoError(t, err)
	assert.Equal(t, total, result.TotalCount, "TotalCount should reflect all projects")
	assert.Len(t, result.Items, 1000, "Limit>1000 must be capped at maxProjectListLimit=1000")
	assert.NotEmpty(t, result.NextCursor, "more projects exist, so NextCursor must be set")
}
