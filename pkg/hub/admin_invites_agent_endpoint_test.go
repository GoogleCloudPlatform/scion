//go:build !no_sqlite

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

package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAdminInvitesCreate_UsesHubEndpointNotAgentEndpoint proves that the
// agent-endpoint override, used only for the SCION_HUB_ENDPOINT value
// injected into agents, has no effect on admin-generated invite links: they
// must keep using the Hub's regular (public) endpoint.
func TestAdminInvitesCreate_UsesHubEndpointNotAgentEndpoint(t *testing.T) {
	s, err := newTestStore(":memory:")
	if err != nil {
		if strings.Contains(err.Error(), "sqlite driver not registered") {
			t.Skip("Skipping test because sqlite driver is not registered (build with -tags sqlite to enable)")
		}
		t.Fatalf("failed to create test store: %v", err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("failed to migrate test store: %v", err)
	}

	srv := &Server{
		store: s,
		config: ServerConfig{
			HubEndpoint:   "https://hub.example.com",
			AgentEndpoint: "http://192.0.2.10:8080",
		},
		inviteService: NewInviteService(s),
		events:        noopEventPublisher{},
	}

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	body := strings.NewReader(`{"expiresIn":"1h"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/invites", body)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()

	srv.handleAdminInvites(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp InviteCreateResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	if !strings.HasPrefix(resp.InviteURL, "https://hub.example.com/invite?code=") {
		t.Errorf("InviteURL = %q, want it to use HubEndpoint (https://hub.example.com), not AgentEndpoint", resp.InviteURL)
	}
}
