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
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestAuthorizeScheduledDispatchAgentAuthoring_HubScopedUATDenied checks the
// dispatch_agent authoring gate directly for every scoped UAT shape: a
// project-scoped and a hub-scoped (empty project) UAT are both denied, and
// the same unscoped user is allowed.
func TestAuthorizeScheduledDispatchAgentAuthoring_HubScopedUATDenied(t *testing.T) {
	srv := &Server{}
	user := NewAuthenticatedUser("gate-user", "gate-user@test.com", "Gate User", "member", "api")
	scopes := []string{"scheduled_event:create", "agent:create"}

	cases := []struct {
		name     string
		identity Identity
		allowed  bool
	}{
		{"unscoped user allowed", user, true},
		{"project-scoped UAT denied", NewScopedUserIdentity(user, "project-1", scopes), false},
		{"hub-scoped UAT denied", NewScopedUserIdentity(user, "", scopes), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/", nil)
			req = req.WithContext(contextWithIdentity(context.Background(), tc.identity))
			rec := httptest.NewRecorder()

			got := srv.authorizeScheduledDispatchAgentAuthoring(rec, req)
			assert.Equal(t, tc.allowed, got)
			if !tc.allowed {
				assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
				assert.Contains(t, rec.Body.String(),
					"scheduled agent creation requires a credential whose scope can be applied at execution time")
			}
		})
	}
}
