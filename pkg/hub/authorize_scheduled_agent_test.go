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

// TestAuthorizeScheduledDispatchAgentAuthoring_Precondition checks the
// dispatch_agent authoring precondition directly: it requires an identity and
// admits every credential shape, scoped UATs included, since a revision
// records the token's ceiling and every fire applies it. The schedule routes
// refuse a project-scoped UAT before this check, at the bearer gate's boundary
// eligibility stage (assertScheduledEventBoundaryIneligible).
func TestAuthorizeScheduledDispatchAgentAuthoring_Precondition(t *testing.T) {
	srv := &Server{}
	user := NewAuthenticatedUser("gate-user", "gate-user@test.com", "Gate User", "member", "api")
	scopes := []string{"scheduled_event:create", "agent:create"}

	cases := []struct {
		name     string
		identity Identity
		want     int
	}{
		{"unscoped user", user, http.StatusOK},
		{"project-scoped UAT", NewScopedUserIdentity(user, "project-1", scopes), http.StatusOK},
		{"hub-scoped UAT", NewScopedUserIdentity(user, "", scopes), http.StatusOK},
		{"no identity", nil, http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/", nil)
			if tc.identity != nil {
				req = req.WithContext(contextWithIdentity(context.Background(), tc.identity))
			}
			rec := httptest.NewRecorder()

			got := srv.authorizeScheduledDispatchAgentAuthoring(rec, req)
			assert.Equal(t, tc.want == http.StatusOK, got)
			assert.Equal(t, tc.want, rec.Code, rec.Body.String())
		})
	}
}
