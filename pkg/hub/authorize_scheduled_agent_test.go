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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAuthorizeScheduledDispatchAgentAuthoring_Precondition checks the
// scheduled authoring credential gate directly, through both authoring
// functions' shared rule: it requires an identity, admits the allowlisted
// (identity type, credential kind) pairs, and refuses every user access
// token and every other credential kind, including an empty or unknown one,
// with 403 and the GOV_PENDING session-only reason.
func TestAuthorizeScheduledDispatchAgentAuthoring_Precondition(t *testing.T) {
	srv := &Server{}
	user := NewAuthenticatedUser("gate-user", "gate-user@test.com", "Gate User", "member", "api")
	federated := NewFederatedUserIdentity("https://issuer.example", "fed-user", "fed@test.com", "Fed", "member", nil)
	agent := &agentIdentityWrapper{&AgentTokenClaims{Claims: jwt.Claims{Subject: "gate-agent"}, ProjectID: "project-1", Scopes: []AgentTokenScope{ScopeProjectRead, ScopeAgentCreate}}}
	scopes := []string{"scheduled_event:create", "agent:create"}
	noRecord := CredentialKind("")

	cases := []struct {
		name     string
		identity Identity
		kind     CredentialKind // noRecord: no credential record on the context
		want     int
	}{
		{"user, interactive session", user, CredentialKindInteractive, http.StatusOK},
		{"user, no credential record", user, noRecord, http.StatusOK},
		{"dev user, dev credential", NewDevUser(DevUserConfig{Username: "dev", DisplayName: "Dev"}), CredentialKindDev, http.StatusOK},
		{"user presented by a broker", user, CredentialKindBroker, http.StatusOK},
		{"federated user, federation credential", federated, CredentialKindFederation, http.StatusOK},
		{"agent, agent JWT", agent, CredentialKindAgentJWT, http.StatusOK},
		{"agent, no credential record", agent, noRecord, http.StatusOK},
		{"project-scoped UAT", NewScopedUserIdentity(user, "project-1", scopes), noRecord, http.StatusForbidden},
		{"hub-scoped UAT", NewScopedUserIdentity(user, "", scopes), noRecord, http.StatusForbidden},
		{"UAT credential on a plain user identity", user, CredentialKindUAT, http.StatusForbidden},
		{"scoped identity with an interactive credential record", NewScopedUserIdentity(user, "project-1", scopes), CredentialKindInteractive, http.StatusForbidden},
		{"user, hub delivery credential", user, CredentialKindHubDelivery, http.StatusForbidden},
		{"user, unknown credential kind", user, CredentialKind("bogus"), http.StatusForbidden},
		{"user, agent JWT credential", user, CredentialKindAgentJWT, http.StatusForbidden},
		{"agent, interactive credential", agent, CredentialKindInteractive, http.StatusForbidden},
		{"federated user, interactive credential", federated, CredentialKindInteractive, http.StatusForbidden},
		{"federated service", NewFederatedServiceIdentity("https://issuer.example", "svc", "svc@test.com", nil), CredentialKindFederation, http.StatusForbidden},
		{"no identity", nil, noRecord, http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			if tc.identity != nil {
				ctx = contextWithIdentity(ctx, tc.identity)
			}
			if tc.kind != noRecord {
				ctx = contextWithCredentialContext(ctx, CredentialContext{Kind: tc.kind})
			}
			req := httptest.NewRequest(http.MethodPost, "/", nil).WithContext(ctx)

			rec := httptest.NewRecorder()
			got := srv.authorizeScheduledDispatchAgentAuthoring(rec, req)
			assert.Equal(t, tc.want == http.StatusOK, got)
			assert.Equal(t, tc.want, rec.Code, rec.Body.String())
			assert.Equal(t, tc.want == http.StatusOK, scheduleAuthoringCredentialAllowed(req))
			if tc.want == http.StatusForbidden {
				var resp ErrorResponse
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
				assert.Equal(t, string(authzop.ReasonGovernancePending), resp.Error.Details["reason"])
				assert.Equal(t, sessionRequiredCredential, resp.Error.Details["credential"])
			}
		})
	}
}
