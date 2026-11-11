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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// resolvedEndpointsConfig is the hub config after a restart with a public
// URL saved in the settings database and no operator pin.
func resolvedEndpointsConfig() ServerConfig {
	return ServerConfig{
		HubEndpoint: "https://file.example.com",
		HubEndpoints: &HubEndpoints{
			Operator:       "https://file.example.com",
			OperatorSource: "file",
			Public:         "https://db.example.com",
			PublicSource:   HubEndpointSourceDatabase,
		},
	}
}

// TestSavedPublicURL_ReachesAgentDispatch checks that after a restart a
// public URL saved in the settings database is the hub endpoint dispatched
// agents receive (ptone/scion#3905).
func TestSavedPublicURL_ReachesAgentDispatch(t *testing.T) {
	srv := &Server{
		store:       createTestStore(t),
		maintenance: NewMaintenanceState(false, ""),
		config:      resolvedEndpointsConfig(),
	}
	d := srv.CreateAuthenticatedDispatcher()
	req, err := d.buildCreateRequest(context.Background(), hubDefaultsDispatchAgent(), "test")
	if err != nil {
		t.Fatalf("buildCreateRequest: %v", err)
	}
	if req.HubEndpoint != "https://db.example.com" {
		t.Errorf("dispatch HubEndpoint = %q, want the saved public URL", req.HubEndpoint)
	}
}

// TestSavedPublicURL_ReachesInviteLinks checks that after a restart new
// invite links use a public URL saved in the settings database.
func TestSavedPublicURL_ReachesInviteLinks(t *testing.T) {
	s := createTestStore(t)
	srv := &Server{
		store:         s,
		config:        resolvedEndpointsConfig(),
		inviteService: NewInviteService(s),
		events:        noopEventPublisher{},
	}
	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/invites", strings.NewReader(`{"expiresIn":"1h"}`))
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
	if !strings.HasPrefix(resp.InviteURL, "https://db.example.com/invite?code=") {
		t.Errorf("InviteURL = %q, want it built on the saved public URL", resp.InviteURL)
	}
}
