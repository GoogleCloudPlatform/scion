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

package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// Coverage for `scion service-accounts` (P5 item B) and for the promise that
// `scion project service-accounts` still works underneath the reshaped client.
//
// No live GCP. The Hub is an httptest server and GCP is on the far side of it,
// so nothing here can reach Google even if it wanted to.

// saCLITestState captures the package-level vars these commands read, because
// --global is a root persistent flag and therefore global state.
type saCLITestState struct {
	home        string
	globalMode  bool
	noHub       bool
	projectPath string
	outputJSON  bool
	assignable  bool
	hubEndpoint string
}

func saveSACLIState() saCLITestState {
	return saCLITestState{
		home:        os.Getenv("HOME"),
		globalMode:  globalMode,
		noHub:       noHub,
		projectPath: projectPath,
		outputJSON:  saOutputJSON,
		assignable:  saGlobalListAssignable,
		hubEndpoint: os.Getenv("SCION_HUB_ENDPOINT"),
	}
}

func (s saCLITestState) restore() {
	_ = os.Setenv("HOME", s.home)
	globalMode = s.globalMode
	noHub = s.noHub
	projectPath = s.projectPath
	saOutputJSON = s.outputJSON
	saGlobalListAssignable = s.assignable
	if s.hubEndpoint == "" {
		_ = os.Unsetenv("SCION_HUB_ENDPOINT")
	} else {
		_ = os.Setenv("SCION_HUB_ENDPOINT", s.hubEndpoint)
	}
}

// saCLIHub stands up a fake Hub and points a fresh project at it, recording
// the query of the last request so a test can assert what was ASKED.
func saCLIHub(t *testing.T, body string) *url.Values {
	t.Helper()

	seen := &url.Values{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	tmpHome := t.TempDir()
	_ = os.Setenv("HOME", tmpHome)
	// SCION_HUB_ENDPOINT would override the settings file via the koanf env
	// provider and silently send these requests somewhere real.
	_ = os.Unsetenv("SCION_HUB_ENDPOINT")
	noHub = false

	projectDir := filepath.Join(tmpHome, "project", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))
	settings := map[string]interface{}{
		"project_id": "proj-local",
		"hub": map[string]interface{}{
			"enabled":   true,
			"endpoint":  srv.URL,
			"projectId": "scion-proj-1",
		},
	}
	data, err := json.Marshal(settings)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "settings.json"), data, 0644))
	projectPath = projectDir

	return seen
}

const saCLIHubScopedBody = `{
  "items": [
    {"id":"hub-sa-1","scope":"hub","scopeId":"hub-instance","email":"shared@x.iam.gserviceaccount.com",
     "projectId":"gcp-proj","verified":true,"_capabilities":{"actions":["read"]}}
  ]
}`

// THE BRIEF'S CLI REQUIREMENT: `scion service-accounts list --global` lists
// hub-scoped accounts.
//
// Both halves are asserted, and the first is the one that matters: the command
// must ASK for scope=hub. A test that only checked the rows came back would
// still pass if the command asked for the wrong scope and the fake answered
// anyway -- which is exactly what a fake always does.
func TestSAScopedList_Global_AsksForHubScope(t *testing.T) {
	orig := saveSACLIState()
	defer orig.restore()

	seen := saCLIHub(t, saCLIHubScopedBody)
	globalMode = true
	saOutputJSON = false
	saGlobalListAssignable = false

	require.NoError(t, runSAScopedList(nil, nil))

	require.Equal(t, store.ScopeHub, seen.Get("scope"),
		"--global on service-accounts must ask the Hub for hub scope")
	// Absent, not empty: the Hub resolves its own ID and rejects a
	// client-supplied one, so naming a hub would be asking a hub to answer for
	// a hub.
	require.NotContains(t, *seen, "scopeId",
		"hub scope must not send a scopeId")
}

// BACKWARDS COMPATIBILITY, which is a brief requirement and not a courtesy:
// `scion project service-accounts list` still works after the accessor reshape,
// and it still asks the narrow question.
func TestProjectSAList_StillWorksAndStaysNarrow(t *testing.T) {
	orig := saveSACLIState()
	defer orig.restore()

	seen := saCLIHub(t, `{"items":[]}`)
	globalMode = false
	saOutputJSON = false

	require.NoError(t, runSAList(nil, nil))

	require.Equal(t, store.ScopeProject, seen.Get("scope"))
	require.Equal(t, "scion-proj-1", seen.Get("scopeId"),
		"the route names the SCION project, not the GCP one")
	require.NotContains(t, *seen, "includeHubScoped",
		"the nested command answers 'what is registered to this project'; widening it "+
			"here would change what the command means")
}

// THE THREE QUESTIONS THIS GROUP CAN ASK ARE THREE DIFFERENT QUESTIONS, pinned
// as ONE test asserting they differ rather than three each pinning a query
// string. Three separate tests all stay green if a later change collapses two
// of the branches into each other, because each would still describe something
// the command really does.
func TestSAScopedList_ThreeScopesDivergeOnTheWire(t *testing.T) {
	orig := saveSACLIState()
	defer orig.restore()

	ask := func(global, assignable bool) string {
		seen := saCLIHub(t, `{"items":[]}`)
		globalMode = global
		saGlobalListAssignable = assignable
		saOutputJSON = false
		require.NoError(t, runSAScopedList(nil, nil))
		return seen.Encode()
	}

	hubOnly := ask(true, false)
	projectOnly := ask(false, false)
	assignable := ask(false, true)

	require.NotEqual(t, hubOnly, projectOnly)
	require.NotEqual(t, projectOnly, assignable)
	require.NotEqual(t, hubOnly, assignable)

	// And the specific difference between the last two, because it is the one
	// that goes wrong silently: a picker given the narrow set omits hub-scoped
	// accounts the user IS permitted to assign, and their absence reads as "no
	// such account".
	require.Contains(t, assignable, "includeHubScoped=true")
	require.NotContains(t, projectOnly, "includeHubScoped")
}

// --assignable with --global is refused BEFORE any request, because the two
// together do not describe a set. Asserting only that an error came back would
// also pass for a command that sent an incoherent request and relayed the Hub's
// complaint about it, which is a different bug wearing the same error.
func TestSAScopedList_AssignableWithGlobalIsRefusedLocally(t *testing.T) {
	orig := saveSACLIState()
	defer orig.restore()

	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	defer srv.Close()

	tmpHome := t.TempDir()
	_ = os.Setenv("HOME", tmpHome)
	_ = os.Unsetenv("SCION_HUB_ENDPOINT")
	noHub = false
	projectDir := filepath.Join(tmpHome, "project", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))
	data, err := json.Marshal(map[string]interface{}{
		"project_id": "proj-local",
		"hub":        map[string]interface{}{"enabled": true, "endpoint": srv.URL, "projectId": "scion-proj-1"},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "settings.json"), data, 0644))
	projectPath = projectDir

	globalMode = true
	saGlobalListAssignable = true

	require.Error(t, runSAScopedList(nil, nil))
	require.False(t, called, "an incoherent scope combination must not reach the Hub")
}

// ONE FLAG, TWO VOCABULARIES, AND THEY MUST NOT CONVERGE.
//
// --global is a PRESENTATION word, chosen so that `scion service-accounts` and
// `scion templates` read alike. The domain words underneath differ: templates
// really have a scope called "global", and service accounts have one called
// "hub", and pkg/hubclient refuses "global" outright so the two cannot be
// silently interchanged.
//
// ONE TEST, NOT TWO. Two tests -- "--global means hub for SAs" and "--global
// means global for templates" -- both stay green if someone later unifies the
// vocabularies, because each would still be describing a real mapping. A test
// whose subject is the DIVERGENCE fails the moment they meet.
func TestGlobalFlag_MapsToDifferentScopesInEachVocabulary(t *testing.T) {
	orig := saveSACLIState()
	defer orig.restore()

	globalMode = true
	saScope, templateScope := saScopeFromGlobalFlag(), templateScopeFromGlobalFlag()

	require.NotEqual(t, saScope, templateScope,
		"one CLI flag, two domain vocabularies: collapsing them makes it impossible to ever "+
			"again find a site that conflated template-global with service-account-hub")
	require.Equal(t, store.ScopeHub, saScope)
	require.Equal(t, "global", templateScope)

	// Without the flag both say "project" -- the vocabularies overlap here, and
	// that overlap is real rather than a normalisation.
	globalMode = false
	require.Equal(t, store.ScopeProject, saScopeFromGlobalFlag())
	require.Equal(t, "project", templateScopeFromGlobalFlag())
}

// The by-id address follows the same flag, and hub scope must reach the FLAT
// route: the nested one would have to borrow an unrelated project's ID to name
// an account that belongs to no project.
func TestSAScopedShow_GlobalUsesTheFlatRoute(t *testing.T) {
	orig := saveSACLIState()
	defer orig.restore()

	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"hub-sa-1","scope":"hub","email":"shared@x.iam.gserviceaccount.com",
			"_capabilities":{"actions":["read"]}}`))
	}))
	defer srv.Close()

	tmpHome := t.TempDir()
	_ = os.Setenv("HOME", tmpHome)
	_ = os.Unsetenv("SCION_HUB_ENDPOINT")
	noHub = false
	projectDir := filepath.Join(tmpHome, "project", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))
	data, err := json.Marshal(map[string]interface{}{
		"project_id": "proj-local",
		"hub":        map[string]interface{}{"enabled": true, "endpoint": srv.URL, "projectId": "scion-proj-1"},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "settings.json"), data, 0644))
	projectPath = projectDir

	globalMode = true
	saOutputJSON = false

	require.NoError(t, runSAScopedShow(nil, []string{"hub-sa-1"}))
	require.Equal(t, "/api/v1/gcp-service-accounts/hub-sa-1", path)
}

// AT HUB SCOPE THERE IS NO PROJECT TO REQUIRE. This is the point of the group:
// a caller outside any linked project must still be able to reach hub-scoped
// accounts. Same unlinked settings, two flags, opposite outcomes -- pinned
// together so that "make resolveSAScope consistent" reads as a behaviour change.
func TestResolveSAScope_HubScopeNeedsNoLinkedProject(t *testing.T) {
	orig := saveSACLIState()
	defer orig.restore()

	tmpHome := t.TempDir()
	_ = os.Setenv("HOME", tmpHome)
	_ = os.Unsetenv("SCION_HUB_ENDPOINT")
	noHub = false

	projectDir := filepath.Join(tmpHome, "project", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))
	// Deliberately NO hub.projectId: a project that has not been linked.
	data, err := json.Marshal(map[string]interface{}{
		"project_id": "proj-local",
		"hub":        map[string]interface{}{"enabled": true, "endpoint": "http://127.0.0.1:1"},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "settings.json"), data, 0644))
	projectPath = projectDir

	globalMode = true
	sc, err := resolveSAScope()
	require.NoError(t, err, "hub scope must not require a linked project")
	require.Equal(t, store.ScopeHub, sc.scope)
	require.Empty(t, sc.scopeID, "hub scope sends no scopeId")

	globalMode = false
	_, err = resolveSAScope()
	require.Error(t, err, "project scope still requires a linked project")
}

// THE REF FOLLOWS THE SCOPE, and the project branch uses the SCION project.
// Pinned as one test asserting the two addresses DIFFER, because if they ever
// collapse the failure is a 404 that reads like a missing record rather than
// like a routing mistake.
func TestSAScopeContext_RefFollowsScope(t *testing.T) {
	hub := (&saScopeContext{scope: store.ScopeHub}).ref("sa-1")
	project := (&saScopeContext{scope: store.ScopeProject, scopeID: "scion-proj-1"}).ref("sa-1")

	require.NotEqual(t, hub, project,
		"hub scope is parentless and must not be routed through a project it does not belong to")
}
