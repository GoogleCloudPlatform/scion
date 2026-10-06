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

package bridge

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/internal/state"
	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// The auto-provision fallback adopts an agent by name after a failed create
// (a concurrent create may have made it). It must not adopt an agent that is
// being deleted: not after the hub answered the create delete_in_progress,
// and not a listed row whose deletion view reads deleting or that is
// soft-deleted. A failed delete leaves the agent live, so it is still
// adopted (ptone/scion#3455).

const resolveTestSlug = "auto-agent"

// newAutoProvisionBridge is newFollowUpTestBridge with auto-provisioning on
// for proj-1.
func newAutoProvisionBridge(t *testing.T, agents *mockAgentService) *Bridge {
	t.Helper()
	store, err := state.NewSQLite(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("state.New: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := &Config{
		Hub:      HubConfig{User: "test-user"},
		Timeouts: TimeoutConfig{SendMessage: 2 * time.Second},
		Projects: []ProjectConfig{{Slug: "proj-1", AutoProvision: true, DefaultTemplate: "default", ExposedAgents: []string{resolveTestSlug}}},
		Bridge:   BridgeConfig{ExternalURL: "https://test.example.com"},
	}
	b := New(store, &mockHubClient{agents: agents}, nil, cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { b.Shutdown() })
	return b
}

func deletingView() *hubclient.DeletionInfo {
	return &hubclient.DeletionInfo{State: hubclient.DeletionStateDeleting, Claim: 1, StartedAt: time.Now()}
}

func failedView() *hubclient.DeletionInfo {
	return &hubclient.DeletionInfo{State: hubclient.DeletionStateFailed, Code: "runtime_error", Claim: 1, StartedAt: time.Now()}
}

// listing answers the first List with first and every later one with retry.
func listing(first, retry []hubclient.Agent) (func(context.Context, *hubclient.ListAgentsOptions) (*hubclient.ListAgentsResponse, error), *int) {
	calls := 0
	return func(context.Context, *hubclient.ListAgentsOptions) (*hubclient.ListAgentsResponse, error) {
		calls++
		if calls == 1 {
			return &hubclient.ListAgentsResponse{Agents: first}, nil
		}
		return &hubclient.ListAgentsResponse{Agents: retry}, nil
	}, &calls
}

func failCreate(err error) (func(context.Context, *hubclient.CreateAgentRequest) (*hubclient.CreateAgentResponse, error), *int) {
	calls := 0
	return func(context.Context, *hubclient.CreateAgentRequest) (*hubclient.CreateAgentResponse, error) {
		calls++
		return nil, err
	}, &calls
}

func deleteInProgressErr() error {
	return &apiclient.APIError{StatusCode: http.StatusConflict, Code: "delete_in_progress", Message: "agent was deleted while it was being created"}
}

// A create answered delete_in_progress is not followed by adopt-by-name,
// even when the list now returns the (deleting) agent.
func TestResolveContext_DeleteInProgressCreate_DoesNotAdopt(t *testing.T) {
	for _, row := range []hubclient.Agent{
		{ID: "a-1", Slug: resolveTestSlug, Name: resolveTestSlug, ProjectID: "p-1", Deletion: deletingView()},
		// Even a row that reads live: the 409 says this create's agent is
		// gone or going.
		{ID: "a-1", Slug: resolveTestSlug, Name: resolveTestSlug, ProjectID: "p-1"},
	} {
		listFn, listCalls := listing(nil, []hubclient.Agent{row})
		createFn, _ := failCreate(deleteInProgressErr())
		b := newAutoProvisionBridge(t, &mockAgentService{listFn: listFn, createFn: createFn})

		got, err := b.resolveContext(context.Background(), "proj-1", resolveTestSlug, "")
		if err == nil {
			t.Fatalf("resolveContext adopted %q after delete_in_progress", got.AgentID)
		}
		if !isDeleteInProgress(err) {
			t.Errorf("error does not carry the hub's delete_in_progress: %v", err)
		}
		if *listCalls != 1 {
			t.Errorf("List calls = %d, want 1 (no re-list after delete_in_progress)", *listCalls)
		}
	}
}

// Other create errors keep the adopt-by-name fallback.
func TestResolveContext_OtherCreateError_StillAdopts(t *testing.T) {
	listFn, _ := listing(nil, []hubclient.Agent{{ID: "a-2", Slug: resolveTestSlug, Name: resolveTestSlug, ProjectID: "p-1"}})
	createFn, _ := failCreate(&apiclient.APIError{StatusCode: http.StatusConflict, Code: "conflict", Message: "already exists"})
	b := newAutoProvisionBridge(t, &mockAgentService{listFn: listFn, createFn: createFn})

	got, err := b.resolveContext(context.Background(), "proj-1", resolveTestSlug, "")
	if err != nil {
		t.Fatalf("resolveContext: %v", err)
	}
	if got.AgentID != "a-2" {
		t.Errorf("AgentID = %q, want a-2 (adopted by name)", got.AgentID)
	}
}

// The fallback skips a row that is being deleted (deletion view deleting, or
// soft-deleted) and adopts one whose delete failed.
func TestResolveContext_Fallback_SkipsDeletingRows(t *testing.T) {
	cases := []struct {
		name  string
		row   hubclient.Agent
		adopt bool
	}{
		{"deleting", hubclient.Agent{ID: "a-3", Slug: resolveTestSlug, Name: resolveTestSlug, Deletion: deletingView()}, false},
		{"soft-deleted", hubclient.Agent{ID: "a-3", Slug: resolveTestSlug, Name: resolveTestSlug, DeletedAt: time.Now()}, false},
		{"delete failed", hubclient.Agent{ID: "a-3", Slug: resolveTestSlug, Name: resolveTestSlug, Deletion: failedView()}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			createErr := errors.New("hub unavailable")
			listFn, _ := listing(nil, []hubclient.Agent{tc.row})
			createFn, _ := failCreate(createErr)
			b := newAutoProvisionBridge(t, &mockAgentService{listFn: listFn, createFn: createFn})

			got, err := b.resolveContext(context.Background(), "proj-1", resolveTestSlug, "")
			if tc.adopt {
				if err != nil {
					t.Fatalf("resolveContext: %v", err)
				}
				if got.AgentID != "a-3" {
					t.Errorf("AgentID = %q, want a-3", got.AgentID)
				}
				return
			}
			if err == nil {
				t.Fatalf("resolveContext adopted %q, which is being deleted", got.AgentID)
			}
			if !errors.Is(err, createErr) {
				t.Errorf("error = %v, want the create error", err)
			}
		})
	}
}

// The first lookup skips a row that is being deleted too: with
// auto-provisioning the bridge creates a fresh agent instead of routing to
// the deleting one, and a failed delete is used as is.
func TestResolveContext_FirstLookup_SkipsDeletingRows(t *testing.T) {
	cases := []struct {
		name    string
		row     hubclient.Agent
		creates bool
	}{
		{"deleting", hubclient.Agent{ID: "a-4", Slug: resolveTestSlug, Name: resolveTestSlug, Deletion: deletingView()}, true},
		{"soft-deleted", hubclient.Agent{ID: "a-4", Slug: resolveTestSlug, Name: resolveTestSlug, DeletedAt: time.Now()}, true},
		{"delete failed", hubclient.Agent{ID: "a-4", Slug: resolveTestSlug, Name: resolveTestSlug, Deletion: failedView()}, false},
		{"no delete", hubclient.Agent{ID: "a-4", Slug: resolveTestSlug, Name: resolveTestSlug}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			listFn, _ := listing([]hubclient.Agent{tc.row}, nil)
			creates := 0
			createFn := func(context.Context, *hubclient.CreateAgentRequest) (*hubclient.CreateAgentResponse, error) {
				creates++
				return &hubclient.CreateAgentResponse{Agent: &hubclient.Agent{ID: "a-new", ProjectID: "p-1"}}, nil
			}
			b := newAutoProvisionBridge(t, &mockAgentService{listFn: listFn, createFn: createFn})

			got, err := b.resolveContext(context.Background(), "proj-1", resolveTestSlug, "")
			if err != nil {
				t.Fatalf("resolveContext: %v", err)
			}
			want := "a-4"
			if tc.creates {
				want = "a-new"
			}
			if got.AgentID != want {
				t.Errorf("AgentID = %q, want %q", got.AgentID, want)
			}
			if (creates == 1) != tc.creates {
				t.Errorf("creates = %d, want create=%v", creates, tc.creates)
			}
		})
	}
}
