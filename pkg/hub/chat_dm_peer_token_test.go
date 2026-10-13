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

// TestDMPeerToken_HubTokenRefusedAtUserPeerCheck pins that a hub-boundary
// user access token holding user:read, alone or with inbox:write, is
// refused at the user DM peer check: it gets the same answer as a missing
// thread, and nothing is stored.
func TestDMPeerToken_HubTokenRefusedAtUserPeerCheck(t *testing.T) {
	for _, scopes := range [][]string{{"user:read"}, {"user:read", "inbox:write"}} {
		e := setupDMPeerEnv(t)
		want := e.nonParticipantRefusal(t, e.send)
		key := userDMKey(t, e.alice.ID, e.bob.ID)
		token := mintHubConfigToken(t, e.srv, e.alice.ID, hubBoundary(), scopes...)
		rr := doRequestWithUAT(t, e.srv, token, http.MethodPost,
			"/api/v1/chat/conversations/"+key+"/messages", map[string]string{"content": "hello"})
		require.Equal(t, want, refusalOf(rr), "scopes %v", scopes)
		e.assertNothingStored(t, e.alice, key)
	}
}
