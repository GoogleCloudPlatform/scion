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

package identity

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/extras/scion-chat-app/internal/state"
	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// stubUserLookup returns a fixed ChatUserInfo.
type stubUserLookup struct {
	info *ChatUserInfo
	err  error
}

func (s *stubUserLookup) GetUser(ctx context.Context, userID string) (*ChatUserInfo, error) {
	return s.info, s.err
}

func newTestMapper(t *testing.T, users []hubclient.User) *Mapper {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	store, err := state.New(dbPath)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	// Use a real TokenMinter with a dummy 32-byte key so the fixture is valid
	// for every code path through Mapper, including ClientFor which calls
	// MintToken. A nil minter would compile but panic on the impersonation path.
	minter, err := NewTokenMinter(make([]byte, 32))
	if err != nil {
		t.Fatalf("failed to create test minter: %v", err)
	}

	client := &fakeHubClient{users: users}
	return NewMapper(store, client, "http://hub.test", minter, slog.Default())
}

func TestAutoRegister_EmailMatch(t *testing.T) {
	mapper := newTestMapper(t, []hubclient.User{
		{ID: "hub-1", Email: "alice@example.com"},
		{ID: "hub-2", Email: "bob@example.com"},
	})

	mapping, err := mapper.AutoRegister(context.Background(), &ChatUserInfo{
		PlatformID: "users/12345",
		Email:      "alice@example.com",
	}, "google_chat")
	if err != nil {
		t.Fatalf("AutoRegister failed: %v", err)
	}
	if mapping == nil {
		t.Fatal("expected mapping, got nil")
	}
	if mapping.HubUserID != "hub-1" {
		t.Errorf("HubUserID = %q, want %q", mapping.HubUserID, "hub-1")
	}
	if mapping.HubUserEmail != "alice@example.com" {
		t.Errorf("HubUserEmail = %q, want %q", mapping.HubUserEmail, "alice@example.com")
	}
	if mapping.RegisteredBy != "auto" {
		t.Errorf("RegisteredBy = %q, want %q", mapping.RegisteredBy, "auto")
	}
}

func TestAutoRegister_NoEmailMatch(t *testing.T) {
	mapper := newTestMapper(t, []hubclient.User{
		{ID: "hub-1", Email: "alice@example.com"},
	})

	mapping, err := mapper.AutoRegister(context.Background(), &ChatUserInfo{
		PlatformID: "users/12345",
		Email:      "unknown@example.com",
	}, "google_chat")
	if err != nil {
		t.Fatalf("AutoRegister failed: %v", err)
	}
	if mapping != nil {
		t.Errorf("expected nil mapping for unmatched email, got %+v", mapping)
	}
}

func TestAutoRegister_EmptyEmail(t *testing.T) {
	mapper := newTestMapper(t, []hubclient.User{
		{ID: "hub-1", Email: "alice@example.com"},
	})

	mapping, err := mapper.AutoRegister(context.Background(), &ChatUserInfo{
		PlatformID: "users/12345",
		Email:      "",
	}, "google_chat")
	if err != nil {
		t.Fatalf("AutoRegister failed: %v", err)
	}
	if mapping != nil {
		t.Errorf("expected nil mapping for empty email, got %+v", mapping)
	}
}

func TestResolveOrAutoRegister_WithEventEmail(t *testing.T) {
	mapper := newTestMapper(t, []hubclient.User{
		{ID: "hub-42", Email: "dev@company.com"},
	})

	// Simulate what eventUserLookup does — returns the email from the event.
	lookup := &stubUserLookup{
		info: &ChatUserInfo{
			PlatformID: "users/99",
			Email:      "dev@company.com",
		},
	}

	mapping, err := mapper.ResolveOrAutoRegister(context.Background(), lookup, "users/99", "google_chat")
	if err != nil {
		t.Fatalf("ResolveOrAutoRegister failed: %v", err)
	}
	if mapping == nil {
		t.Fatal("expected mapping, got nil")
	}
	if mapping.HubUserID != "hub-42" {
		t.Errorf("HubUserID = %q, want %q", mapping.HubUserID, "hub-42")
	}
	if mapping.RegisteredBy != "auto" {
		t.Errorf("RegisteredBy = %q, want %q", mapping.RegisteredBy, "auto")
	}

	// Second call should resolve from DB without needing the lookup.
	mapping2, err := mapper.ResolveOrAutoRegister(context.Background(), lookup, "users/99", "google_chat")
	if err != nil {
		t.Fatalf("second ResolveOrAutoRegister failed: %v", err)
	}
	if mapping2 == nil {
		t.Fatal("expected mapping on second call, got nil")
	}
	if mapping2.HubUserID != "hub-42" {
		t.Errorf("second call HubUserID = %q, want %q", mapping2.HubUserID, "hub-42")
	}
}

func TestResolveOrAutoRegister_NoEmail_ReturnsNil(t *testing.T) {
	mapper := newTestMapper(t, []hubclient.User{
		{ID: "hub-1", Email: "alice@example.com"},
	})

	// Simulate a lookup that returns no email (like the old GetUser stub).
	lookup := &stubUserLookup{
		info: &ChatUserInfo{
			PlatformID: "users/99",
		},
	}

	mapping, err := mapper.ResolveOrAutoRegister(context.Background(), lookup, "users/99", "google_chat")
	if err != nil {
		t.Fatalf("ResolveOrAutoRegister failed: %v", err)
	}
	if mapping != nil {
		t.Errorf("expected nil mapping when no email provided, got %+v", mapping)
	}
}

// fakeHubClient implements the subset of hubclient.Client needed by identity.Mapper.
type fakeHubClient struct {
	hubclient.Client
	users []hubclient.User
}

func (f *fakeHubClient) Users() hubclient.UserService {
	return &fakeUserService{users: f.users}
}

type fakeUserService struct {
	hubclient.UserService
	users []hubclient.User
}

func (f *fakeUserService) List(ctx context.Context, opts *apiclient.PageOptions) (*hubclient.ListUsersResponse, error) {
	return &hubclient.ListUsersResponse{Users: f.users}, nil
}
