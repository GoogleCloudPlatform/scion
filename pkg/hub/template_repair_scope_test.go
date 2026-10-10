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
	"log/slog"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// Tests for ptone/scion#3130: template hash-mismatch repair must act on the
// record the agent was stamped with (by ID), or, for agents without a stamped
// ID, on the project-scoped then global record, never on an arbitrary
// same-named template in another scope.

type templateRepairFixture struct {
	srv      *Server
	store    store.Store
	projectA *store.Project
	projectB *store.Project
	global   *store.Template
	inA      *store.Template
}

func newTemplateRepairFixture(t *testing.T) *templateRepairFixture {
	t.Helper()
	ctx := context.Background()
	s, err := newTestStore(t, ":memory:")
	require.NoError(t, err)
	require.NoError(t, s.Migrate(ctx))

	srv := &Server{store: s, resourceLog: logging.Subsystem("hub.resources")}
	stor := newMockStorage("test-bucket")
	srv.SetStorage(stor)

	f := &templateRepairFixture{srv: srv, store: s}
	for _, p := range []**store.Project{&f.projectA, &f.projectB} {
		name := "tmpl-repair-proj-a"
		if p == &f.projectB {
			name = "tmpl-repair-proj-b"
		}
		proj := &store.Project{ID: tid(name + t.Name()), Name: name, Slug: name}
		require.NoError(t, s.CreateProject(ctx, proj))
		*p = proj
	}

	mk := func(id, scope, scopeID, storagePath string) *store.Template {
		tmpl := &store.Template{
			ID:          tid(id + t.Name()),
			Name:        "worker",
			Slug:        "worker",
			Harness:     "claude",
			Scope:       scope,
			ScopeID:     scopeID,
			StoragePath: storagePath,
			Files:       []store.TemplateFile{{Path: "scion-agent.yaml", Hash: repairStaleHash}},
			ContentHash: "stale-content",
			Status:      store.TemplateStatusActive,
		}
		require.NoError(t, s.CreateTemplate(ctx, tmpl))
		// Storage holds the real content for every record, so a repair of
		// either record would succeed; which DB row changed tells them apart.
		_, err := stor.Upload(ctx, storagePath+"/scion-agent.yaml", nil, storage.UploadOptions{
			Metadata: map[string]string{"sha256": repairStorageHash},
		})
		require.NoError(t, err)
		return tmpl
	}
	// Global first, then project A: a "newest by name" lookup would pick the
	// project-A record for everyone.
	f.global = mk("tmpl-global", store.TemplateScopeGlobal, "", "tmpl/global/worker")
	// Wait only until the clock has moved past the global record's creation
	// time, so the project-A record is strictly newer without a fixed sleep.
	for !time.Now().After(f.global.Created) {
		runtime.Gosched()
	}
	f.inA = mk("tmpl-proj-a", store.TemplateScopeProject, f.projectA.ID, "tmpl/project-a/worker")
	return f
}

func (f *templateRepairFixture) repaired(t *testing.T, tmpl *store.Template) bool {
	t.Helper()
	got, err := f.store.GetTemplate(context.Background(), tmpl.ID)
	require.NoError(t, err)
	require.Len(t, got.Files, 1)
	return got.Files[0].Hash == repairStorageHash
}

func TestTemplateRepair_NameOnlyOtherProjectRepairsGlobal(t *testing.T) {
	f := newTemplateRepairFixture(t)
	err := f.srv.syncTemplateFromStorage(context.Background(), TemplateRepairRef{
		Name: "worker", ProjectID: f.projectB.ID,
	})
	require.NoError(t, err)
	assert.True(t, f.repaired(t, f.global), "project B has no 'worker'; the global record must be repaired")
	assert.False(t, f.repaired(t, f.inA), "project A's record must not be touched by a project-B agent")
}

func TestTemplateRepair_NameOnlyProjectARepairsProjectA(t *testing.T) {
	f := newTemplateRepairFixture(t)
	err := f.srv.syncTemplateFromStorage(context.Background(), TemplateRepairRef{
		Name: "worker", ProjectID: f.projectA.ID,
	})
	require.NoError(t, err)
	assert.True(t, f.repaired(t, f.inA))
	assert.False(t, f.repaired(t, f.global))
}

func TestTemplateRepair_StampedIDWinsOverName(t *testing.T) {
	f := newTemplateRepairFixture(t)
	// A global-stamped agent living in project A: the ID wins over the
	// project-first name fallback.
	err := f.srv.syncTemplateFromStorage(context.Background(), TemplateRepairRef{
		ID: f.global.ID, Name: "worker", ProjectID: f.projectA.ID,
	})
	require.NoError(t, err)
	assert.True(t, f.repaired(t, f.global))
	assert.False(t, f.repaired(t, f.inA), "the stamped ID is authoritative over the name fallback")
}

func TestTemplateRepair_StaleIDDoesNotFallBackToName(t *testing.T) {
	f := newTemplateRepairFixture(t)
	err := f.srv.syncTemplateFromStorage(context.Background(), TemplateRepairRef{
		ID: tid("deleted-template"), Name: "worker", ProjectID: f.projectA.ID,
	})
	require.ErrorContains(t, err, "not found")
	assert.False(t, f.repaired(t, f.global))
	assert.False(t, f.repaired(t, f.inA))
}

// Startup sync must repair each listed record by its own ID: both same-named
// records are stale, and a name-based call would resolve both to one record.
func TestSyncAllTemplatesFromStorage_RepairsEachSameNamedRecordByID(t *testing.T) {
	f := newTemplateRepairFixture(t)

	f.srv.SyncAllTemplatesFromStorage(context.Background())

	assert.True(t, f.repaired(t, f.global), "the global record must be repaired")
	assert.True(t, f.repaired(t, f.inA), "the project-A record must be repaired by its own ID")
}

// The dispatcher must hand the stamped ID, the template reference and the
// agent's project to the repair callback.
func TestHTTPDispatcher_RepairTemplatePassesIDAndProject(t *testing.T) {
	d := NewHTTPAgentDispatcherWithClient(nil, nil, false, slog.Default())
	var got TemplateRepairRef
	d.SetTemplateRepairer(func(_ context.Context, ref TemplateRepairRef) error {
		got = ref
		return nil
	})
	agent := &store.Agent{
		Slug:          "a",
		ProjectID:     "proj-1",
		Template:      "worker",
		AppliedConfig: &store.AgentAppliedConfig{TemplateID: "tmpl-id-1"},
	}
	err := d.repairTemplate(context.Background(), agent)
	require.NoError(t, err)
	assert.Equal(t, TemplateRepairRef{ID: "tmpl-id-1", Name: "worker", ProjectID: "proj-1"}, got)
}

// With no stamped template ID (no applied config, or an empty TemplateID),
// the dispatcher falls back to a name-only reference scoped to the agent's
// project.
func TestHTTPDispatcher_RepairTemplateNameOnlyFallback(t *testing.T) {
	for _, tc := range []struct {
		name    string
		applied *store.AgentAppliedConfig
	}{
		{name: "no applied config"},
		{name: "empty template ID", applied: &store.AgentAppliedConfig{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := NewHTTPAgentDispatcherWithClient(nil, nil, false, slog.Default())
			var got TemplateRepairRef
			d.SetTemplateRepairer(func(_ context.Context, ref TemplateRepairRef) error {
				got = ref
				return nil
			})
			agent := &store.Agent{
				Slug:          "a",
				ProjectID:     "proj-1",
				Template:      "worker",
				AppliedConfig: tc.applied,
			}
			require.NoError(t, d.repairTemplate(context.Background(), agent))
			assert.Equal(t, TemplateRepairRef{Name: "worker", ProjectID: "proj-1"}, got)
		})
	}
}

func TestHTTPDispatcher_RepairTemplateNoReference(t *testing.T) {
	d := NewHTTPAgentDispatcherWithClient(nil, nil, false, slog.Default())
	d.SetTemplateRepairer(func(context.Context, TemplateRepairRef) error {
		return errors.New("must not be called")
	})
	err := d.repairTemplate(context.Background(), &store.Agent{Slug: "a"})
	require.ErrorContains(t, err, "no template reference")
}
