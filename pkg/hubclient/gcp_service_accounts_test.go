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
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Contract tests for the GCP service account service.
//
// This file did not exist until P5, and its absence is the whole reason
// TestGCPServiceAccounts_List_DecodesTheServersActualShape below describes a
// four-month-old break rather than a hypothetical one. Six methods, zero
// tests, so nothing pinned the client's decoding against the server's
// encoding and the two drifted apart in a commit that was itself a correct
// fix to the server.
//
// The bodies below are the shapes the hub really writes. When changing one,
// change it because pkg/hub changed, not to make a test pass.
//
// No live GCP: every response here is canned, which is the whole point — the
// service under test is an HTTP client and GCP is on the far side of the hub.

// saListBody is what both list handlers actually write:
// ListGCPServiceAccountsResponse, an OBJECT with an "items" array.
// pkg/hub/handlers_gcp_identity.go:301.
const saListBody = `{
  "items": [
    {"id":"sa-1","scope":"project","scopeId":"proj-1","email":"one@x.iam.gserviceaccount.com","verified":true,
     "_capabilities":{"actions":["read","assign"]}},
    {"id":"sa-2","scope":"hub","scopeId":"hub-instance","email":"two@x.iam.gserviceaccount.com","verified":false,
     "_capabilities":{"actions":["read"]}}
  ],
  "_capabilities":{"actions":["list"]}
}`

func saTestClient(t *testing.T, h http.HandlerFunc) (Client, func()) {
	t.Helper()
	srv := httptest.NewServer(h)
	c, err := New(srv.URL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, srv.Close
}

// THE REGRESSION. Broken since d65dc095 (2026-03-17), which changed the server
// from writing a bare array to writing {"items":[...]} in order to return
// capabilities — a correct change — and did not update the only Go client that
// decodes it. `scion project service-accounts list`
// (cmd/project_service_accounts.go:223) has returned a decode error, not a
// short list, ever since.
//
// It survived because List hand-rolled its own json.NewDecoder instead of
// using apiclient.DecodeResponse like every sibling method. That private copy
// of the contract is what went stale; Get, Create, Verify and Mint all rode
// the shared helper and were unaffected by the same server change.
func TestGCPServiceAccounts_List_DecodesTheServersActualShape(t *testing.T) {
	c, done := saTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(saListBody))
	})
	defer done()

	sas, err := c.GCPServiceAccounts().List(context.Background(), ListForProject("proj-1"))
	if err != nil {
		t.Fatalf("List against the server's real response shape must succeed, got: %v", err)
	}
	if len(sas) != 2 {
		t.Fatalf("expected 2 accounts, got %d", len(sas))
	}
	if sas[0].ID != "sa-1" || sas[1].ID != "sa-2" {
		t.Errorf("unexpected ids: %q, %q", sas[0].ID, sas[1].ID)
	}
	// Scope must survive decoding: every consumer that distinguishes a
	// hub-scoped account from a project-scoped one reads this field.
	if sas[1].Scope != "hub" {
		t.Errorf("hub-scoped account lost its scope in decode: %q", sas[1].Scope)
	}
}

// Delete swallowed every HTTP error status: it returned the transport error
// only, and a transport error is nil for a 403. So a refused deletion of a
// credential binding reported success to the caller, and the CLI printed a
// confirmation for a thing that still exists.
//
// Checked across three statuses because the bug is in the absence of any
// status check at all — a fix that special-cases one code would pass a
// single-status test while leaving the others silent.
func TestGCPServiceAccounts_Delete_ReportsHTTPErrors(t *testing.T) {
	for _, code := range []int{http.StatusForbidden, http.StatusNotFound, http.StatusInternalServerError} {
		c, done := saTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodDelete {
				t.Errorf("expected DELETE, got %s", r.Method)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			_, _ = w.Write([]byte(`{"error":{"code":"forbidden","message":"nope"}}`))
		})

		err := c.GCPServiceAccounts().Delete(context.Background(), ProjectScopedRef("proj-1", "sa-1"))
		if err == nil {
			t.Errorf("HTTP %d on delete must surface as an error; reporting success for a "+
				"credential binding that still exists is the dangerous direction to fail in", code)
		}
		done()
	}
}

func TestGCPServiceAccounts_Get(t *testing.T) {
	c, done := saTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		// A project-scoped ref must keep the NESTED address: the flat by-id
		// route 404s project-scoped accounts by design.
		want := "/api/v1/projects/proj-1/gcp-service-accounts/sa-1"
		if r.URL.Path != want {
			t.Errorf("expected path %s, got %s", want, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"sa-1","scope":"project","email":"one@x.iam.gserviceaccount.com"}`))
	})
	defer done()

	sa, err := c.GCPServiceAccounts().Get(context.Background(), ProjectScopedRef("proj-1", "sa-1"))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if sa.ID != "sa-1" {
		t.Errorf("expected sa-1, got %q", sa.ID)
	}
}

func TestGCPServiceAccounts_Get_ReportsHTTPErrors(t *testing.T) {
	c, done := saTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"no"}}`))
	})
	defer done()

	if _, err := c.GCPServiceAccounts().Get(context.Background(), ProjectScopedRef("proj-1", "sa-1")); err == nil {
		t.Error("a 404 must surface as an error rather than a zero-valued account")
	}
}

func TestGCPServiceAccounts_Create(t *testing.T) {
	c, done := saTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/api/v1/gcp-service-accounts" {
			t.Errorf("create goes to the flat route so that scope can be named: got %s", r.URL.Path)
		}
		if got, want := r.URL.Query().Get("scopeId"), "proj-1"; got != want {
			t.Errorf("expected scopeId %q, got %q", want, got)
		}
		var got CreateGCPServiceAccountRequest
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if got.Email != "new@x.iam.gserviceaccount.com" {
			t.Errorf("unexpected email on the wire: %q", got.Email)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"sa-new","scope":"project","email":"new@x.iam.gserviceaccount.com"}`))
	})
	defer done()

	sa, err := c.GCPServiceAccounts().Create(context.Background(),
		&CreateGCPServiceAccountRequest{
			Scope: store.ScopeProject, ScopeID: "proj-1",
			Email: "new@x.iam.gserviceaccount.com", ProjectID: "gcp-proj",
		})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sa.ID != "sa-new" {
		t.Errorf("expected sa-new, got %q", sa.ID)
	}
}

func TestGCPServiceAccounts_Verify(t *testing.T) {
	c, done := saTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		want := "/api/v1/projects/proj-1/gcp-service-accounts/sa-1/verify"
		if r.URL.Path != want {
			t.Errorf("expected path %s, got %s", want, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"sa-1","verified":true,"verificationStatus":"verified"}`))
	})
	defer done()

	sa, err := c.GCPServiceAccounts().Verify(context.Background(), ProjectScopedRef("proj-1", "sa-1"))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !sa.Verified {
		t.Error("expected verified=true to survive decoding")
	}
}

func TestGCPServiceAccounts_Mint(t *testing.T) {
	c, done := saTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		want := "/api/v1/projects/proj-1/gcp-service-accounts/mint"
		if r.URL.Path != want {
			t.Errorf("expected path %s, got %s", want, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"sa-minted","managed":true,"managedBy":"scion"}`))
	})
	defer done()

	sa, err := c.GCPServiceAccounts().Mint(context.Background(), "proj-1",
		&MintGCPServiceAccountRequest{AccountID: "minted"})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if !sa.Managed {
		t.Error("expected managed=true to survive decoding")
	}
}

// ---------------------------------------------------------------------------
// P5 item A: the accessor is no longer pinned to a project.
//
// The tests below are about ADDRESSING and SCOPE SELECTION -- what this client
// asks the Hub for. They are deliberately not about what the Hub answers: the
// authorization behaviour of each scope is pinned in pkg/hub against a real
// store, and re-asserting it here against a canned response would only pin this
// file's fixtures to themselves.
// ---------------------------------------------------------------------------

// hubSAListBody is the hub-wide list shape: parentless accounts, so no
// scopeId-bearing project, and capabilities that are read-only for the ordinary
// caller who can nonetheless see them.
const hubSAListBody = `{
  "items": [
    {"id":"hub-sa-1","scope":"hub","scopeId":"hub-instance","email":"shared@x.iam.gserviceaccount.com",
     "verified":true,"_capabilities":{"actions":["read"]}},
    {"id":"hub-sa-2","scope":"hub","scopeId":"hub-instance","email":"shared2@x.iam.gserviceaccount.com",
     "verified":false,"_capabilities":{"actions":["read","delete"]}}
  ]
}`

// saQueryRecorder serves body and records the query of the last request, so a
// test can assert what was ASKED rather than only what came back.
func saQueryRecorder(body string) (http.HandlerFunc, *url.Values) {
	var seen url.Values
	return func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}, &seen
}

// THE BRIEF'S FIRST REQUIREMENT: GCPServiceAccounts() returns hub-scoped
// accounts when queried hub-wide. Under the old project-pinned accessor there
// was no argument that could express this request at all -- hub-scoped accounts
// have no project to pin to -- so this is the test that would not have compiled.
func TestGCPServiceAccounts_List_HubScoped(t *testing.T) {
	h, seen := saQueryRecorder(hubSAListBody)
	c, done := saTestClient(t, h)
	defer done()

	sas, err := c.GCPServiceAccounts().List(context.Background(), ListHubScoped())
	if err != nil {
		t.Fatalf("hub-wide list: %v", err)
	}

	if got := seen.Get("scope"); got != store.ScopeHub {
		t.Errorf("expected scope=%q on the wire, got %q", store.ScopeHub, got)
	}
	// Absent, not empty-valued: the Hub resolves its own ID, and a request that
	// named one would be asking a hub to answer for a hub.
	if _, present := (*seen)["scopeId"]; present {
		t.Errorf("hub scope must not send scopeId, got %q", seen.Get("scopeId"))
	}

	if len(sas) != 2 {
		t.Fatalf("expected 2 hub-scoped accounts, got %d", len(sas))
	}
	for _, sa := range sas {
		if !sa.IsHubScoped() {
			t.Errorf("account %s decoded as scope %q, not hub-scoped", sa.ID, sa.Scope)
		}
	}
}

// THE THREE LIST CASES DIFFER ON THE WIRE, PINNED AS ONE TEST.
//
// One test rather than three, because the property that matters is that the
// three requests are DISTINCT. Three separate tests each asserting one expected
// query string can all be made green again by a refactor that collapses two of
// the constructors into each other; a single test asserting they differ cannot.
func TestGCPServiceAccounts_List_ThreeScopesAskThreeDifferentQuestions(t *testing.T) {
	ask := func(opts *ListGCPServiceAccountsOptions) string {
		t.Helper()
		h, seen := saQueryRecorder(`{"items":[]}`)
		c, done := saTestClient(t, h)
		defer done()
		if _, err := c.GCPServiceAccounts().List(context.Background(), opts); err != nil {
			t.Fatalf("List: %v", err)
		}
		return seen.Encode()
	}

	hubOnly := ask(ListHubScoped())
	projectOnly := ask(ListForProject("proj-1"))
	union := ask(ListForProjectIncludingHubScoped("proj-1"))

	if hubOnly == projectOnly || projectOnly == union || hubOnly == union {
		t.Fatalf("the three list scopes must be distinguishable on the wire; got\n"+
			"  hub-only:     %s\n  project-only: %s\n  union:        %s",
			hubOnly, projectOnly, union)
	}

	// And the specific difference between the last two, because it is the one a
	// caller gets wrong silently: the management view and the picker view differ
	// only by this flag, and confusing them shows a plain member accounts they
	// cannot manage (or hides accounts they could be assigned).
	if !strings.Contains(union, "includeHubScoped=true") {
		t.Errorf("the picker's list must widen to hub-scoped accounts: %s", union)
	}
	if strings.Contains(projectOnly, "includeHubScoped") {
		t.Errorf("the management list must not widen: %s", projectOnly)
	}
}

// Scope selection is refused in the CLIENT for combinations the Hub has no
// address for -- and the observable is that NO REQUEST IS MADE. Asserting only
// that an error came back would pass for a client that sent a malformed request
// and reported the Hub's complaint about it, which is a different bug wearing
// the same error.
func TestGCPServiceAccounts_List_RejectsIncoherentScopeWithoutCalling(t *testing.T) {
	cases := []struct {
		name string
		opts *ListGCPServiceAccountsOptions
	}{
		{"nil options", nil},
		{"no scope", &ListGCPServiceAccountsOptions{}},
		{"unknown scope", &ListGCPServiceAccountsOptions{Scope: "global"}},
		{"project without id", &ListGCPServiceAccountsOptions{Scope: store.ScopeProject}},
		{"hub with a caller-supplied hub id", &ListGCPServiceAccountsOptions{
			Scope: store.ScopeHub, ScopeID: "hub-instance"}},
		{"hub widened to include itself", &ListGCPServiceAccountsOptions{
			Scope: store.ScopeHub, IncludeHubScoped: true}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			c, done := saTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				called = true
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"items":[]}`))
			})
			defer done()

			if _, err := c.GCPServiceAccounts().List(context.Background(), tc.opts); err == nil {
				t.Error("expected an error")
			}
			if called {
				t.Error("the client must not send a request it cannot describe; " +
					"a Hub-side complaint is a different failure from a client-side refusal")
			}
		})
	}
}

// "global" is the TEMPLATE vocabulary. Service accounts use "hub", and the two
// are kept spelled differently on purpose because they mean different things.
// This pins the non-normalisation: if someone later teaches the client to
// accept "global" as an alias, this test says out loud what that would erase.
func TestGCPServiceAccounts_ScopeVocabularyIsNotTheTemplateVocabulary(t *testing.T) {
	if store.ScopeHub == "global" {
		t.Fatalf("store.ScopeHub is now %q; this file's premise moved", store.ScopeHub)
	}
	if _, err := gcpSAScopeQuery("global", "", false); err == nil {
		t.Error(`"global" is the template scope, not the service-account scope; ` +
			`accepting it here would make two different vocabularies look like one`)
	}
}

// THE TWO BY-ID ADDRESSES ARE DIFFERENT ADDRESSES, pinned as one test for the
// same reason as the list scopes above: what matters is that a hub-scoped ref
// and a project-scoped ref do not resolve to the same place. They serve
// disjoint sets on the Hub -- a project-scoped account 404s on the flat route
// by design -- so a collapse here would turn a routing choice into a 404 that
// reads like a missing record.
func TestGCPServiceAccounts_RefChoosesFlatOrNestedAddress(t *testing.T) {
	hub := HubScopedRef("sa-1").byIDPath()
	project := ProjectScopedRef("proj-1", "sa-1").byIDPath()

	if hub == project {
		t.Fatalf("hub-scoped and project-scoped refs must address different routes, both got %s", hub)
	}
	if want := "/api/v1/gcp-service-accounts/sa-1"; hub != want {
		t.Errorf("hub-scoped ref: expected the parentless route %s, got %s", want, hub)
	}
	if want := "/api/v1/projects/proj-1/gcp-service-accounts/sa-1"; project != want {
		t.Errorf("project-scoped ref: expected the nested route %s, got %s", want, project)
	}
}

// REF() IS THE ADDRESS THE ACCOUNT ALREADY KNOWS.
//
// The trap this closes is not carelessness, it is care (sa-arch). A
// GCPServiceAccount carries two project-ish fields -- ScopeID, the Scion
// project in the route, and ProjectID, the GCP project the account lives in --
// so ProjectScopedRef(sa.ProjectID, sa.ID) compiles, reads correctly, matches
// the parameter by NAME, and routes to a project that does not exist in Scion.
//
// Pinned as ONE test asserting Ref() and the ProjectID spelling DISAGREE, not
// as two tests each pinning one address: two such tests both stay green if
// someone "simplifies" Ref() to use ProjectID, since each would still describe
// a real address.
func TestGCPServiceAccount_Ref_UsesScopeIDNotTheGCPProject(t *testing.T) {
	sa := &GCPServiceAccount{
		ID:        "sa-1",
		Scope:     store.ScopeProject,
		ScopeID:   "scion-proj",  // the Scion project: belongs in the route
		ProjectID: "my-gcp-proj", // the GCP project: does not
	}

	fromScope := sa.Ref().byIDPath()
	fromGCPProject := ProjectScopedRef(sa.ProjectID, sa.ID).byIDPath()

	if fromScope == fromGCPProject {
		t.Fatalf("the two project-ish fields must not address the same route, both got %s", fromScope)
	}
	if want := "/api/v1/projects/scion-proj/gcp-service-accounts/sa-1"; fromScope != want {
		t.Errorf("Ref() must route by ScopeID: expected %s, got %s", want, fromScope)
	}
}

func TestGCPServiceAccount_Ref_ParentlessScopesGoFlat(t *testing.T) {
	const flat = "/api/v1/gcp-service-accounts/sa-1"

	for _, scope := range []string{store.ScopeHub, store.ScopeUser} {
		sa := &GCPServiceAccount{ID: "sa-1", Scope: scope, ScopeID: "whatever", ProjectID: "gcp-proj"}
		if got := sa.Ref().byIDPath(); got != flat {
			t.Errorf("%s scope is parentless and belongs on the flat route: got %s", scope, got)
		}
	}

	// A project-scoped account with no ScopeID is a malformed record, not a
	// caller error. It goes flat and 404s, which beats asking the Hub to parse
	// /api/v1/projects//gcp-service-accounts/sa-1.
	malformed := &GCPServiceAccount{ID: "sa-1", Scope: store.ScopeProject, ProjectID: "gcp-proj"}
	if got := malformed.Ref().byIDPath(); got != flat {
		t.Errorf("a project-scoped account with no ScopeID must not build an empty path segment: got %s", got)
	}
}

// THE CONSTRUCTORS ARE THE ONLY WAY IN, and that is enforced by the compiler
// rather than by the doc comment above them.
//
// An optional lever is a comment (sa-arch). With unexported fields,
// GCPServiceAccountRef{ID: x} outside this package does not compile, so the
// zero-value trap -- a forgotten project and a deliberate hub-scoped ref being
// byte-identical structs -- cannot be written by accident.
//
// A compile failure in another package is not something a test in this one can
// observe, so this asserts the property that produces it.
func TestGCPServiceAccountRef_HasNoExportedFields(t *testing.T) {
	rt := reflect.TypeOf(GCPServiceAccountRef{})
	for i := 0; i < rt.NumField(); i++ {
		if f := rt.Field(i); f.IsExported() {
			t.Errorf("GCPServiceAccountRef.%s is exported: external callers can now build a ref "+
				"by field assignment, and the named constructors stop being the only way in", f.Name)
		}
	}
}

func TestGCPServiceAccounts_Get_HubScopedUsesFlatRoute(t *testing.T) {
	var path string
	c, done := saTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"hub-sa-1","scope":"hub","scopeId":"hub-instance",
			"_capabilities":{"actions":["read","delete"]}}`))
	})
	defer done()

	sa, err := c.GCPServiceAccounts().Get(context.Background(), HubScopedRef("hub-sa-1"))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if want := "/api/v1/gcp-service-accounts/hub-sa-1"; path != want {
		t.Errorf("expected %s, got %s", want, path)
	}
	if !sa.IsHubScoped() {
		t.Errorf("expected a hub-scoped account, got scope %q", sa.Scope)
	}
}

// Capabilities must survive decoding PER ITEM, not only at the top level.
//
// Every affordance in the CLI and the web UI is supposed to be rendered from
// this field rather than from the fact that a row exists. If it silently
// decodes to nil the fail-closed default hides every button, which is a visible
// bug; the dangerous direction is the opposite one, so the nil case is pinned
// too.
func TestGCPServiceAccounts_CapabilitiesDecodePerItem(t *testing.T) {
	c, done := saTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(hubSAListBody))
	})
	defer done()

	sas, err := c.GCPServiceAccounts().List(context.Background(), ListHubScoped())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(sas) != 2 {
		t.Fatalf("expected 2 accounts, got %d", len(sas))
	}

	// Same list, same scope, DIFFERENT authority. Two rows a caller can equally
	// see, only one of which it may delete -- which is exactly why existence is
	// not the thing to render a Delete button from.
	if !sas[0].Capabilities.Can("read") {
		t.Error("hub-sa-1 should decode a read capability")
	}
	if sas[0].Capabilities.Can("delete") {
		t.Error("hub-sa-1 must not gain a delete capability the Hub did not send")
	}
	if !sas[1].Capabilities.Can("delete") {
		t.Error("hub-sa-2 should decode a delete capability")
	}
}

func TestGCPServiceAccountCapabilities_AbsentMeansNo(t *testing.T) {
	c, done := saTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// A surface that does not compute capabilities, e.g. the nested by-id
		// read.
		_, _ = w.Write([]byte(`{"id":"sa-1","scope":"project"}`))
	})
	defer done()

	sa, err := c.GCPServiceAccounts().Get(context.Background(), ProjectScopedRef("proj-1", "sa-1"))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if sa.Capabilities != nil {
		t.Fatalf("expected no capabilities block, got %+v", sa.Capabilities)
	}
	// Nil-safe and false: unknown authority must not read as permission.
	if sa.Capabilities.Can("delete") {
		t.Error("an account with no capabilities block must not report delete")
	}
}

// IsHubScoped reads SCOPE and nothing else.
//
// The tempting alternative -- comparing ScopeID against the hub's ID -- breaks
// the first time a hub is redeployed under a different hostname, and breaks
// silently: every hub-scoped account simply stops being recognised as one.
func TestGCPServiceAccount_IsHubScoped_IgnoresScopeID(t *testing.T) {
	renamed := &GCPServiceAccount{Scope: store.ScopeHub, ScopeID: "some-other-hub-id"}
	if !renamed.IsHubScoped() {
		t.Error("a hub-scoped account must stay hub-scoped regardless of which hub instance registered it")
	}
	if (&GCPServiceAccount{Scope: store.ScopeProject, ScopeID: "proj-1"}).IsHubScoped() {
		t.Error("a project-scoped account must not report as hub-scoped")
	}
	var nilSA *GCPServiceAccount
	if nilSA.IsHubScoped() {
		t.Error("nil must not report as hub-scoped")
	}
}

// A 204 is no accounts, not a nil dereference. The Hub does not send one today;
// the point is that the client survives if it starts to, since ranging over the
// result is what every caller does next.
func TestGCPServiceAccounts_List_EmptyResponseIsAnEmptyList(t *testing.T) {
	c, done := saTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	defer done()

	sas, err := c.GCPServiceAccounts().List(context.Background(), ListHubScoped())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(sas) != 0 {
		t.Errorf("expected no accounts, got %d", len(sas))
	}
}

// CREATION AT HUB SCOPE IS THE HUB'S REFUSAL TO MAKE, NOT THIS CLIENT'S.
//
// The request is representable here on purpose. The Hub holds hub-scoped
// creation closed at handlers_gcp_identity_scoped.go with a message that
// explains itself; a client that rejected it first would report a different and
// less true reason, and would keep reporting it after the hold is lifted. So
// the assertion is that the request REACHES the server carrying scope=hub, and
// that the server's refusal is what the caller sees.
func TestGCPServiceAccounts_Create_HubScopeReachesTheServersRefusal(t *testing.T) {
	var seenScope string
	reached := false
	c, done := saTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		reached = true
		seenScope = r.URL.Query().Get("scope")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"invalid_request",` +
			`"message":"hub-scoped service account creation is not enabled on this hub"}}`))
	})
	defer done()

	_, err := c.GCPServiceAccounts().Create(context.Background(), &CreateGCPServiceAccountRequest{
		Scope: store.ScopeHub,
		Email: "shared@x.iam.gserviceaccount.com",
	})
	if err == nil {
		t.Fatal("the Hub's refusal must surface as an error")
	}
	if !reached {
		t.Fatal("the client must not pre-empt the Hub's hold on hub-scoped creation: " +
			"a client-side refusal would outlive the server-side one")
	}
	if seenScope != store.ScopeHub {
		t.Errorf("expected scope=%q on the wire, got %q", store.ScopeHub, seenScope)
	}
	// STATUS AND CODE, NOT PROSE.
	//
	// I originally pinned the Hub's exact 400 text here so the test would break
	// if it changed. sa-arch: it will change, and the break would not be
	// informative -- a hub dev rewording that string sees a test fail in another
	// module and repairs it by pasting the new string, restoring green having
	// verified nothing. THE EVENT WORTH CATCHING IS THE HOLD BEING LIFTED, and
	// that arrives as a 2xx where a 400 was expected. Status and code catch it
	// exactly. Pin prose only where the prose IS the contract; here the HOLD is
	// the contract and the wording is not.
	var apiErr *apiclient.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected an *apiclient.APIError so callers can branch on it, got %T: %v", err, err)
	}
	if apiErr.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 while the hold stands, got %d", apiErr.StatusCode)
	}
	if apiErr.Code != "invalid_request" {
		t.Errorf("expected code invalid_request, got %q", apiErr.Code)
	}
	// A refusal the caller cannot print is a refusal the user cannot act on.
	if apiErr.Message == "" {
		t.Error("the Hub's refusal must carry a message, whatever it says")
	}
}

// Mint is project-scoped by nature and there is no hub-scoped form of it, so
// the missing project is a client-side refusal -- and again the observable is
// that nothing was sent, since a request to /api/v1/projects//gcp-service-
// accounts/mint would be a differently-shaped bug.
func TestGCPServiceAccounts_Mint_RequiresAProjectWithoutCalling(t *testing.T) {
	called := false
	c, done := saTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
	})
	defer done()

	if _, err := c.GCPServiceAccounts().Mint(context.Background(), "",
		&MintGCPServiceAccountRequest{AccountID: "x"}); err == nil {
		t.Error("minting without a project must fail: minting draws on a project's quota")
	}
	if called {
		t.Error("no request should have been sent for an unaddressable mint")
	}
}
