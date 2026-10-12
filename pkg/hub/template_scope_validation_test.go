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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TestCreateTemplateV2_ScopeValidation verifies that createTemplateV2 accepts
// only the canonical scopes ("", "global", "project", "user") and rejects
// anything else — including the removed legacy "grove" scope and arbitrary
// garbage — with 400 instead of silently storing it.
func TestCreateTemplateV2_ScopeValidation(t *testing.T) {
	srv, _ := testServer(t)

	valid := []struct {
		name    string
		scope   string
		scopeID string
	}{
		{"empty defaults to global", "", ""},
		{"global", store.TemplateScopeGlobal, ""},
		{"project", store.TemplateScopeProject, tid("scope-validation-project")},
		{"user", store.TemplateScopeUser, ""},
	}
	for _, tt := range valid {
		t.Run("accepts/"+tt.name, func(t *testing.T) {
			body := CreateTemplateRequest{
				Name:    "tmpl-" + tt.name,
				Harness: "antigravity",
				Scope:   tt.scope,
				ScopeID: tt.scopeID,
			}
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/templates", body)
			require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
		})
	}

	invalid := []string{"grove", "bogus"}
	for _, scope := range invalid {
		t.Run("rejects/"+scope, func(t *testing.T) {
			body := CreateTemplateRequest{
				Name:    "tmpl-invalid-" + scope,
				Harness: "antigravity",
				Scope:   scope,
			}
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/templates", body)
			assert.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
			assert.Contains(t, rec.Body.String(), "invalid scope", "body: %s", rec.Body.String())
		})
	}
}

// TestHandleTemplateClone_ScopeValidation verifies the same scope validation
// on the template clone create path. Notification subscription templates get
// their own, differently-scoped validation — see
// TestCreateNotificationSubscriptionTemplate_ScopeValidation in
// handlers_notifications_test.go — but template clone gets this treatment
// regardless: validation runs before the clone's destination-scope
// authorization switch, which has arms only for global, project and user;
// an unrecognized scope must be rejected before it reaches that switch, or
// it would skip every authorization check.
func TestHandleTemplateClone_ScopeValidation(t *testing.T) {
	srv, _ := testServer(t)

	// Create a source template to clone from.
	createBody := CreateTemplateRequest{
		Name:    "clone-source",
		Harness: "antigravity",
		Scope:   store.TemplateScopeGlobal,
	}
	createRec := doRequest(t, srv, http.MethodPost, "/api/v1/templates", createBody)
	require.Equal(t, http.StatusCreated, createRec.Code, "body: %s", createRec.Body.String())
	var created CreateTemplateResponse
	require.NoError(t, json.Unmarshal(createRec.Body.Bytes(), &created))
	require.NotNil(t, created.Template)

	valid := []struct {
		name    string
		scope   string
		scopeID string
	}{
		{"empty defaults to project", "", tid("scope-validation-clone-project")},
		{"global", store.TemplateScopeGlobal, ""},
		{"project", store.TemplateScopeProject, tid("scope-validation-clone-project-2")},
		{"user", store.TemplateScopeUser, ""},
	}
	for _, tt := range valid {
		t.Run("accepts/"+tt.name, func(t *testing.T) {
			body := CloneTemplateRequest{
				Name:    "clone-" + tt.name,
				Scope:   tt.scope,
				ScopeID: tt.scopeID,
			}
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/templates/"+created.Template.ID+"/clone", body)
			assert.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
		})
	}

	invalid := []string{"grove", "bogus"}
	for _, scope := range invalid {
		t.Run("rejects/"+scope, func(t *testing.T) {
			body := CloneTemplateRequest{
				Name:  "clone-invalid-" + scope,
				Scope: scope,
			}
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/templates/"+created.Template.ID+"/clone", body)
			assert.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
			assert.Contains(t, rec.Body.String(), "invalid scope", "body: %s", rec.Body.String())
		})
	}
}
