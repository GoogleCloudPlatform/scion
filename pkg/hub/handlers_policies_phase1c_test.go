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
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func makeAdminRequest(r *http.Request) *http.Request {
	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	return r.WithContext(contextWithIdentity(r.Context(), admin))
}

// TestCreatePolicy_SourceIPsRejection verifies that SourceIPs are rejected at create time.
func TestCreatePolicy_SourceIPsRejection(t *testing.T) {
	srv, _ := testServer(t)

	tests := []struct {
		name        string
		conditions  *store.PolicyConditions
		shouldAllow bool
		errorMsg    string
	}{
		{
			name:        "no conditions",
			conditions:  nil,
			shouldAllow: true,
		},
		{
			name: "empty SourceIPs",
			conditions: &store.PolicyConditions{
				SourceIPs: []string{},
			},
			shouldAllow: true,
		},
		{
			name: "SourceIPs present",
			conditions: &store.PolicyConditions{
				SourceIPs: []string{"192.168.1.0/24"},
			},
			shouldAllow: false,
			errorMsg:    "SourceIPs conditions are not currently enforced and cannot be set",
		},
		{
			name: "DelegatedFrom allowed",
			conditions: &store.PolicyConditions{
				DelegatedFrom: &store.DelegatedFromCondition{
					PrincipalType: "user",
					PrincipalID:   tid("user-1"),
				},
			},
			shouldAllow: true,
		},
		{
			name: "DelegatedFromGroup allowed",
			conditions: &store.PolicyConditions{
				DelegatedFromGroup: tid("group-1"),
			},
			shouldAllow: true,
		},
		{
			name: "SourceIPs with other conditions",
			conditions: &store.PolicyConditions{
				SourceIPs:          []string{"10.0.0.0/8"},
				DelegatedFromGroup: tid("group-1"),
			},
			shouldAllow: false,
			errorMsg:    "SourceIPs conditions are not currently enforced and cannot be set",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := CreatePolicyRequest{
				Name:         "Test Policy " + tt.name,
				ScopeType:    "hub",
				ResourceType: "agent",
				Actions:      []string{"read"},
				Effect:       "allow",
				Conditions:   tt.conditions,
			}

			body, err := json.Marshal(req)
			require.NoError(t, err)

			httpReq := httptest.NewRequest(http.MethodPost, "/api/v1/policies", bytes.NewReader(body))
			httpReq = makeAdminRequest(httpReq)
			w := httptest.NewRecorder()

			srv.handlePolicies(w, httpReq)

			if tt.shouldAllow {
				assert.Equal(t, http.StatusCreated, w.Code, "should create policy: %s", w.Body.String())

				// Verify created policy has PolicyKind set to explicit
				var resp store.Policy
				err := json.NewDecoder(w.Body).Decode(&resp)
				require.NoError(t, err)
				assert.Equal(t, store.PolicyKindExplicit, resp.PolicyKind, "user-created policies should be explicit")
			} else {
				assert.Equal(t, http.StatusBadRequest, w.Code, "should reject policy")
				assert.Contains(t, w.Body.String(), tt.errorMsg, "error message should mention SourceIPs")
			}
		})
	}
}

// TestUpdatePolicy_SourceIPsRejection verifies that SourceIPs are rejected at update time.
func TestUpdatePolicy_SourceIPsRejection(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a policy without SourceIPs
	policy := &store.Policy{
		ID:           tid("policy-1"),
		Name:         "Test Policy",
		ScopeType:    "hub",
		ResourceType: "agent",
		Actions:      []string{"read"},
		Effect:       "allow",
		PolicyKind:   store.PolicyKindExplicit,
	}
	require.NoError(t, s.CreatePolicy(ctx, policy))

	tests := []struct {
		name        string
		conditions  *store.PolicyConditions
		shouldAllow bool
		errorMsg    string
	}{
		{
			name:        "update without conditions",
			conditions:  nil,
			shouldAllow: true,
		},
		{
			name: "update with SourceIPs",
			conditions: &store.PolicyConditions{
				SourceIPs: []string{"172.16.0.0/12"},
			},
			shouldAllow: false,
			errorMsg:    "SourceIPs conditions are not currently enforced and cannot be set",
		},
		{
			name: "update with DelegatedFrom",
			conditions: &store.PolicyConditions{
				DelegatedFrom: &store.DelegatedFromCondition{
					PrincipalType: "user",
					PrincipalID:   tid("user-1"),
				},
			},
			shouldAllow: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := UpdatePolicyRequest{
				Conditions: tt.conditions,
			}

			body, err := json.Marshal(req)
			require.NoError(t, err)

			httpReq := httptest.NewRequest(http.MethodPatch, "/api/v1/policies/"+tid("policy-1"), bytes.NewReader(body))
			httpReq = makeAdminRequest(httpReq)
			w := httptest.NewRecorder()

			srv.handlePolicyRoutes(w, httpReq)

			if tt.shouldAllow {
				assert.Equal(t, http.StatusOK, w.Code, "should update policy: %s", w.Body.String())
			} else {
				assert.Equal(t, http.StatusBadRequest, w.Code, "should reject update")
				assert.Contains(t, w.Body.String(), tt.errorMsg, "error message should mention SourceIPs")
			}
		})
	}
}

// TestUpdatePolicy_PolicyKindPreservation verifies that PolicyKind is not changed by update.
func TestUpdatePolicy_PolicyKindPreservation(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a seeded policy (default kind)
	seededPolicy := &store.Policy{
		ID:           tid("policy-seeded"),
		Name:         "Seeded Policy",
		ScopeType:    "hub",
		ResourceType: "agent",
		Actions:      []string{"read"},
		Effect:       "allow",
		PolicyKind:   store.PolicyKindDefault,
		Origin:       store.PolicyOriginSeeded,
	}
	require.NoError(t, s.CreatePolicy(ctx, seededPolicy))

	// Update the policy (change description)
	req := UpdatePolicyRequest{
		Description: "Updated description",
	}

	body, err := json.Marshal(req)
	require.NoError(t, err)

	httpReq := httptest.NewRequest(http.MethodPatch, "/api/v1/policies/"+tid("policy-seeded"), bytes.NewReader(body))
	httpReq = makeAdminRequest(httpReq)
	w := httptest.NewRecorder()

	srv.handlePolicyRoutes(w, httpReq)

	require.Equal(t, http.StatusOK, w.Code, "should update policy: %s", w.Body.String())

	// Verify PolicyKind is still 'default'
	var resp store.Policy
	err = json.NewDecoder(w.Body).Decode(&resp)
	require.NoError(t, err)
	assert.Equal(t, store.PolicyKindDefault, resp.PolicyKind, "PolicyKind should be preserved as 'default'")
}
