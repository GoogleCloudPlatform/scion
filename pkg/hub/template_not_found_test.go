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
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
)

// ============================================================================
// templates: consistent not-found error mapping (align with harness configs)
//
// getTemplateV2, handleTemplateDownload and handleTemplateFiles each fetch
// the template before authorizing it. A missing ID used to fall through to
// the generic "Resource not found" mapping, while an inaccessible template
// produced "Template not found". This suite pins the fix: missing and
// inaccessible templates return the same not-found response across these
// read paths.
// ============================================================================

func TestTemplateNotFound_ConsistentAcrossGetDownloadAndFiles(t *testing.T) {
	srv, s, alice, carol, _ := setupTemplateScopeTest(t)
	tpl := createAuthzTestTemplate(t, s, "not-found-parity-template", store.TemplateScopeUser, alice.ID, alice.ID)
	missingID := tid("not-found-parity-missing")

	type route struct {
		name string
		path func(id string) string
	}
	routes := []route{
		{"get", func(id string) string { return "/api/v1/templates/" + id }},
		{"download", func(id string) string { return "/api/v1/templates/" + id + "/download" }},
		{"files", func(id string) string { return "/api/v1/templates/" + id + "/files" }},
	}

	for _, rt := range routes {
		t.Run(rt.name, func(t *testing.T) {
			otherRec := doRequestAsUser(t, srv, carol, http.MethodGet, rt.path(tpl.ID), nil)
			missingRec := doRequestAsUser(t, srv, carol, http.MethodGet, rt.path(missingID), nil)

			assert.Equal(t, http.StatusNotFound, otherRec.Code, "an inaccessible template must 404; got: %s", otherRec.Body.String())
			assert.Equal(t, http.StatusNotFound, missingRec.Code, "a nonexistent template must 404; got: %s", missingRec.Body.String())
			assert.JSONEq(t, otherRec.Body.String(), missingRec.Body.String(),
				"missing and inaccessible templates must return the same not-found response on %s", rt.name)
		})
	}
}

// TestTemplateNotFound_MissingID_UploadFinalizeValidate pins the same
// missing-ID mapping for upload, finalize and validate: each fetches the
// template with GetTemplate before doing anything else, including
// authorization, so a missing ID must 404 with "Template not found" the same
// way get/download/files do. Upload and finalize authorize with
// authorize(...ActionUpdate), which returns 403 rather than 404 for a
// template the caller cannot access — unlike the ActionRead paths above —
// so there is no not-found/inaccessible parity to compare here; this only
// pins the missing-ID mapping itself.
func TestTemplateNotFound_MissingID_UploadFinalizeValidate(t *testing.T) {
	srv, _, alice, _, _ := setupTemplateScopeTest(t)
	missingID := tid("not-found-missing-upload-finalize-validate")

	type route struct {
		name   string
		method string
		path   func(id string) string
	}
	routes := []route{
		{"upload", http.MethodPost, func(id string) string { return "/api/v1/templates/" + id + "/upload" }},
		{"finalize", http.MethodPost, func(id string) string { return "/api/v1/templates/" + id + "/finalize" }},
		{"validate", http.MethodGet, func(id string) string { return "/api/v1/templates/" + id + "/validate" }},
	}

	for _, rt := range routes {
		t.Run(rt.name, func(t *testing.T) {
			rec := doRequestAsUser(t, srv, alice, rt.method, rt.path(missingID), nil)
			assert.Equal(t, http.StatusNotFound, rec.Code, "a nonexistent template must 404 on %s; got: %s", rt.name, rec.Body.String())
			assert.JSONEq(t, `{"error":{"code":"not_found","message":"Template not found"}}`, rec.Body.String(),
				"a nonexistent template must map to the Template not-found body on %s", rt.name)
		})
	}
}
