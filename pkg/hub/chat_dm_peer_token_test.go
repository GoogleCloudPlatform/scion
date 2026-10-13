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
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// Tests for the user DM peer check under a user access token: the token
// reads the peer through the hub-level user record (user:read, hub
// boundary). A refused send gets the same answer as a missing thread and
// stores nothing.

// TestDMPeerToken_HubTokenWithUserReadReachesPeer pins that a hub-boundary
// token carrying user:read passes the peer check and sends the user DM.
func TestDMPeerToken_HubTokenWithUserReadReachesPeer(t *testing.T) {
	e := setupDMPeerEnv(t)
	key := userDMKey(t, e.alice.ID, e.bob.ID)
	token := mintHubConfigToken(t, e.srv, e.alice.ID, hubBoundary(), "user:read", "inbox:write")
	rr := doRequestWithUAT(t, e.srv, token, http.MethodPost,
		"/api/v1/chat/conversations/"+key+"/messages", map[string]string{"content": "hello"})
	require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())
}

// TestDMPeerToken_WithoutUserReadRefused pins that a hub-boundary token
// without user:read is refused at the peer check.
func TestDMPeerToken_WithoutUserReadRefused(t *testing.T) {
	e := setupDMPeerEnv(t)
	want := e.nonParticipantRefusal(t, e.send)
	key := userDMKey(t, e.alice.ID, e.bob.ID)
	token := mintHubConfigToken(t, e.srv, e.alice.ID, hubBoundary(), "inbox:write")
	rr := doRequestWithUAT(t, e.srv, token, http.MethodPost,
		"/api/v1/chat/conversations/"+key+"/messages", map[string]string{"content": "hello"})
	require.Equal(t, want, refusalOf(rr))
	e.assertNothingStored(t, e.alice, key)
}

// TestDMPeerToken_ProjectBoundaryRefused pins that a project-boundary
// credential carrying user:read is refused at the peer check: a hub-level
// user record is outside a project boundary.
func TestDMPeerToken_ProjectBoundaryRefused(t *testing.T) {
	e := setupDMPeerEnv(t)
	want := e.nonParticipantRefusal(t, e.send)
	key := userDMKey(t, e.alice.ID, e.bob.ID)
	alice := NewAuthenticatedUser(e.alice.ID, e.alice.Email, e.alice.DisplayName, e.alice.Role, "api")
	credential := NewScopedUserIdentityWithBoundaryAndDecoration(alice, projectBoundary(e.project.ID),
		[]string{"user:read", "inbox:write"}, tid("dm-peer-project-cred"), bearerCeiling(t, "user:read", "inbox:write"), nil)
	rr := doRequestAsIdentity(t, e.srv, credential, http.MethodPost,
		"/api/v1/chat/conversations/"+key+"/messages", map[string]string{"content": "hello"})
	require.Equal(t, want, refusalOf(rr))
	e.assertNothingStored(t, e.alice, key)
}
