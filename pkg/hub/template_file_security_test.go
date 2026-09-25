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

// Authorization and path handling for the template file sub-route.
//
// Two properties are pinned here, because the route had neither.
//
// Authorization: /api/v1/templates/ is classified RoutePolicy, which passes
// through the declarative guard by design and delegates enforcement to the
// handler. Every sibling template action honours that with an s.authorize
// call; the file sub-route did not, so any authenticated user could read,
// write and delete the files of a template they had no access to. Each case
// below pairs the file route with a control on an action that was always
// gated, so a harness that simply fails to authenticate anyone cannot make
// these tests pass.
//
// Path handling: the file path is concatenated into a storage object path and,
// on write, recorded in the template manifest. A literal "../" never reaches
// the handler — the Go mux cleans the request path and redirects — but
// r.URL.Path is percent-decoded before routing, so an encoded "..%2f" does.
// The encoded form is therefore the one worth testing; testing only the
// literal form would pass against the unfixed code.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// encodedTraversal is "../../../../tmp/pwned.txt" with the separators
// percent-encoded so it survives mux path cleaning.
const encodedTraversal = "..%2f..%2f..%2f..%2ftmp%2fpwned.txt"

func TestTemplateFileAuthz_NonMemberDenied(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
		body   any
		// wantCode is the expected denial status. Reads are 404 — not 403 —
		// per ptone/scion#1916: a read denial on this surface must be
		// indistinguishable from a nonexistent template (authorizeRead,
		// authorize.go). Writes remain 403.
		wantCode int
	}{
		{"read a file", http.MethodGet, "/files/CLAUDE.md", nil, http.StatusNotFound},
		{"list files", http.MethodGet, "/files", nil, http.StatusNotFound},
		{"write a file", http.MethodPut, "/files/CLAUDE.md", map[string]string{"content": "PWNED"}, http.StatusForbidden},
		{"delete a file", http.MethodDelete, "/files/CLAUDE.md", nil, http.StatusForbidden},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, alice, bob, project := setupTemplateAuthzTest(t)
			srv.SetStorage(newContentMockStorage("b"))
			tpl := createAuthzTestTemplate(t, s, "authz-files", store.TemplateScopeProject, project.ID, alice.ID)

			rec := doRequestAsUser(t, srv, bob, tc.method, "/api/v1/templates/"+tpl.ID+tc.path, tc.body)
			assert.Equal(t, tc.wantCode, rec.Code,
				"a non-member must not reach template files; got %d: %s", rec.Code, rec.Body.String())

			// Control: the same non-member on an action that was always gated.
			// If this is not 403 the harness is not exercising authorization at
			// all, and the assertion above would be meaningless.
			ctrl := doRequestAsUser(t, srv, bob, http.MethodPut, "/api/v1/templates/"+tpl.ID,
				store.Template{Name: "pwned", Status: "active"})
			require.Equal(t, http.StatusForbidden, ctrl.Code,
				"control: non-member should already be refused on plain template update")
		})
	}
}

// The owner must keep working — a fix that denies everyone would also pass the
// test above.
func TestTemplateFileAuthz_OwnerAllowed(t *testing.T) {
	srv, s, alice, _, project := setupTemplateAuthzTest(t)
	srv.SetStorage(newContentMockStorage("b"))
	tpl := createAuthzTestTemplate(t, s, "authz-owner", store.TemplateScopeProject, project.ID, alice.ID)

	rec := doRequestAsUser(t, srv, alice, http.MethodPut,
		"/api/v1/templates/"+tpl.ID+"/files/CLAUDE.md", map[string]string{"content": "# Mine"})
	assert.Equal(t, http.StatusOK, rec.Code,
		"the template owner must still be able to write its files: %s", rec.Body.String())
}

// A percent-encoded traversal must be refused before it reaches storage, and
// before it is recorded in the manifest. Run as the template owner, so the
// authorization gate is satisfied and the path check is what is under test.
func TestTemplateFile_RejectsEncodedTraversal(t *testing.T) {
	for _, tc := range []struct {
		name   string
		method string
		body   any
	}{
		{"write", http.MethodPut, map[string]string{"content": "PWNED"}},
		{"read", http.MethodGet, nil},
		{"delete", http.MethodDelete, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, alice, _, project := setupTemplateAuthzTest(t)
			stor := newContentMockStorage("b")
			srv.SetStorage(stor)
			tpl := createAuthzTestTemplate(t, s, "trav", store.TemplateScopeProject, project.ID, alice.ID)

			rec := doRequestAsUser(t, srv, alice, tc.method,
				"/api/v1/templates/"+tpl.ID+"/files/"+encodedTraversal, tc.body)
			assert.Equal(t, http.StatusBadRequest, rec.Code,
				"an encoded traversal must be refused; got %d: %s", rec.Code, rec.Body.String())

			for k := range stor.content {
				assert.NotContains(t, k, "..",
					"a traversing object path reached storage: %q", k)
			}

			updated, err := s.GetTemplate(t.Context(), tpl.ID)
			require.NoError(t, err)
			for _, f := range updated.Files {
				assert.NotContains(t, f.Path, "..",
					"a traversing path was recorded in the template manifest: %q", f.Path)
			}
		})
	}
}

// The literal form is cleaned by the mux long before the handler. Pinned so
// that nobody later reads the test above and concludes the mux was the
// protection: it is not, which is exactly why the encoded case exists.
func TestTemplateFile_LiteralTraversalNeverReachesHandler(t *testing.T) {
	srv, s, alice, _, project := setupTemplateAuthzTest(t)
	srv.SetStorage(newContentMockStorage("b"))
	tpl := createAuthzTestTemplate(t, s, "lit", store.TemplateScopeProject, project.ID, alice.ID)

	rec := doRequestAsUser(t, srv, alice, http.MethodPut,
		"/api/v1/templates/"+tpl.ID+"/files/../../../../tmp/pwned.txt",
		map[string]string{"content": "PWNED"})
	assert.Equal(t, http.StatusTemporaryRedirect, rec.Code,
		"expected the mux to clean and redirect a literal traversal, got %d", rec.Code)
}

// Ordinary nested paths must keep working; the validator must not be so strict
// that it breaks the documented layout.
func TestTemplateFile_AllowsNestedPaths(t *testing.T) {
	srv, _, stor := testTemplateFileServer(t)
	tmpl := createTestTemplate(t, srv.store, stor, map[string]string{"CLAUDE.md": "# Agent"})

	for _, p := range []string{"home/.bashrc", "a/b/c/deep.txt", "CLAUDE.md"} {
		req := httptest.NewRequest(http.MethodPut,
			"/api/v1/templates/"+tmpl.ID+"/files/"+p, strings.NewReader(`{"content":"ok"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+testDevToken)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		assert.Equal(t, http.StatusOK, rec.Code,
			"%q is a legitimate template path and must be accepted: %s", p, rec.Body.String())
	}

	for k := range stor.content {
		assert.NotContains(t, k, "..", "unexpected traversing key %q", k)
	}
}
