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

package hubclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestProjectService_AssignableRoles pins the route and the decoding of
// the assignable-roles list, including a refused role's structured fields.
func TestProjectService_AssignableRoles(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/api/v1/projects/p1/members/assignable-roles", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[
 {"id":"r1","name":"project-member","description":"d","roleKind":"builtin","grantable":true,"reason":""},
 {"id":"r2","name":"project-owner","description":"","roleKind":"builtin","grantable":false,"reason":"no","denialCode":"target_role_protected","details":{"roleDefinitionId":"r2"}}
]}`))
	}))
	defer server.Close()

	client, err := New(server.URL)
	require.NoError(t, err)
	roles, err := client.Projects().AssignableRoles(context.Background(), "p1")
	require.NoError(t, err)
	require.Len(t, roles, 2)
	assert.Equal(t, AssignableProjectRole{ID: "r1", Name: "project-member", Description: "d", RoleKind: "builtin", Grantable: true}, roles[0])
	assert.False(t, roles[1].Grantable)
	assert.Equal(t, "target_role_protected", roles[1].DenialCode)
	assert.Equal(t, "r2", roles[1].Details["roleDefinitionId"])
}
