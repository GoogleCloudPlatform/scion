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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// getTemplatesPage requests /api/v1/templates with the given raw query
// string as user, and returns the decoded response alongside the raw
// recorder (for status-code assertions).
func getTemplatesPage(t *testing.T, srv *Server, user *store.User, rawQuery string) (*httptest.ResponseRecorder, ListTemplatesResponse) {
	t.Helper()
	rec := doRequestAsUser(t, srv, user, http.MethodGet, "/api/v1/templates?"+rawQuery, nil)
	var resp ListTemplatesResponse
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	}
	return rec, resp
}

// scopedCursorBindingForTemplatesTest reconstructs the exact binding
// listTemplatesV2 computes for a scope=user request, so tests can Open a
// real cursor directly. Kept in lockstep with listTemplatesV2's filter
// construction; if that handler's filter shape changes, update this too.
func scopedCursorBindingForTemplatesTest(user *store.User, scope, scopeID string) string {
	filter := store.TemplateFilter{Scope: scope, ScopeID: scopeID, Status: store.TemplateStatusActive}
	identity := NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, "api")
	return scopedCursorBinding("templates", filter, identity)
}
