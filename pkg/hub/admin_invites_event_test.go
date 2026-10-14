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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAdminInvitesCreate_ChangedEventCarriesNoCodeMaterial creates an invite
// through the admin handler and checks that the published
// admin.invite.changed event holds the id and action only: no code, and no
// code prefix, while the invite row returned to the admin still has the prefix.
func TestAdminInvitesCreate_ChangedEventCarriesNoCodeMaterial(t *testing.T) {
	s, err := newTestStore(t, ":memory:")
	if err != nil {
		if strings.Contains(err.Error(), "sqlite driver not registered") {
			t.Skip("Skipping test because sqlite driver is not registered (build with -tags sqlite to enable)")
		}
		t.Fatalf("failed to create test store: %v", err)
	}

	pub := NewChannelEventPublisher()
	ch, unsub := pub.Subscribe("admin.invite.changed")
	defer unsub()

	srv := &Server{
		store:         s,
		config:        ServerConfig{HubEndpoint: "https://hub.example.com"},
		inviteService: NewInviteService(s),
		events:        pub,
	}

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/invites", strings.NewReader(`{"expiresIn":"1h"}`))
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()

	srv.handleAdminInvites(rr, req)
	require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())

	var resp InviteCreateResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.NotNil(t, resp.Invite)
	require.NotEmpty(t, resp.Code)
	require.NotEmpty(t, resp.Invite.CodePrefix, "the invite row must still carry the code prefix")

	var evt Event
	select {
	case evt = <-ch:
	default:
		t.Fatal("no admin.invite.changed event published on invite create")
	}

	var payload map[string]any
	require.NoError(t, json.Unmarshal(evt.Data, &payload))
	assert.Equal(t, map[string]any{"action": "created", "inviteId": resp.Invite.ID}, payload)
	assert.NotContains(t, string(evt.Data), resp.Invite.CodePrefix, "event must not contain the code prefix")
	assert.NotContains(t, string(evt.Data), resp.Code, "event must not contain the code")
}
