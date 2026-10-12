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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/credentials"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeProjectHub is a fake hub with a fixed set of projects. It answers a
// project by ID, the slug, name and git remote filters, and the unfiltered
// listing one project per page. It counts the calls by ID and the
// listings.
type fakeProjectHub struct {
	srv   *httptest.Server
	gets  atomic.Int32
	lists atomic.Int32

	mu       sync.Mutex
	projects []hubclient.Project
	// authz are the Authorization headers of the project requests.
	authz []string
}

func newFakeProjectHub(t *testing.T, projects ...hubclient.Project) *fakeProjectHub {
	t.Helper()
	h := &fakeProjectHub{projects: projects}
	h.srv = httptest.NewServer(http.HandlerFunc(h.serve))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *fakeProjectHub) URL() string { return h.srv.URL }

func (h *fakeProjectHub) setProjects(projects ...hubclient.Project) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.projects = projects
}

func (h *fakeProjectHub) serve(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	projects := append([]hubclient.Project(nil), h.projects...)
	if strings.HasPrefix(r.URL.Path, "/api/v1/projects") {
		h.authz = append(h.authz, r.Header.Get("Authorization"))
	}
	h.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	q := r.URL.Query()
	switch {
	case r.URL.Path == "/healthz":
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	case strings.HasPrefix(r.URL.Path, "/api/v1/projects/"):
		h.gets.Add(1)
		id := strings.TrimPrefix(r.URL.Path, "/api/v1/projects/")
		for _, p := range projects {
			if p.ID == id {
				_ = json.NewEncoder(w).Encode(p)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"project not found"}}`))
	case r.URL.Path == "/api/v1/projects":
		h.lists.Add(1)
		var out []hubclient.Project
		filtered := q.Get("slug") != "" || q.Get("name") != "" || q.Get("gitRemote") != ""
		for _, p := range projects {
			switch {
			case q.Get("slug") != "" && p.Slug == q.Get("slug"),
				q.Get("name") != "" && p.Name == q.Get("name"),
				q.Get("gitRemote") != "" && p.GitRemote == q.Get("gitRemote"):
				out = append(out, p)
			}
		}
		resp := map[string]interface{}{}
		if !filtered {
			// One project per page, to exercise the paging.
			i, _ := strconv.Atoi(q.Get("cursor"))
			if i < len(projects) {
				out = projects[i : i+1]
			}
			if i+1 < len(projects) {
				resp["nextCursor"] = strconv.Itoa(i + 1)
			}
		}
		if out == nil {
			out = []hubclient.Project{}
		}
		resp["projects"] = out
		_ = json.NewEncoder(w).Encode(resp)
	default:
		http.NotFound(w, r)
	}
}

func (h *fakeProjectHub) calls() int32 { return h.gets.Load() + h.lists.Load() }

// authHeaders returns the distinct Authorization headers the hub received.
func (h *fakeProjectHub) authHeaders() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	for _, a := range h.authz {
		if !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	return out
}

// Projects named "foo" on two hubs.
var (
	prodFoo    = hubclient.Project{ID: "p-foo", Slug: "foo", Name: "Foo"}
	stagingFoo = hubclient.Project{ID: "s-foo", Slug: "foo", Name: "Foo"}
)

// setupTwoHubs registers two fake hubs, prod and staging, both with a
// project "foo", and logs in to both. No hub is current, and there is no
// terminal (see setupRegistryTest).
func setupTwoHubs(t *testing.T) (prod, staging *fakeProjectHub) {
	t.Helper()
	setupRegistryTest(t)
	writeHomeGlobalSettings(t, "")
	prod = newFakeProjectHub(t, prodFoo)
	staging = newFakeProjectHub(t, stagingFoo)
	require.NoError(t, config.UpdateCLIContext(func(c *config.CLIContext) error {
		if err := c.AddHub("prod", prod.URL()); err != nil {
			return err
		}
		return c.AddHub("staging", staging.URL())
	}))
	storeTestLogin(t, prod.URL())
	storeTestLogin(t, staging.URL())
	return prod, staging
}

// withTerminal makes 'scion use' see (or not see) a terminal.
func withTerminal(t *testing.T, tty bool) {
	t.Helper()
	orig := isInteractiveTerminal
	t.Cleanup(func() { isInteractiveTerminal = orig })
	isInteractiveTerminal = func() bool { return tty }
}

// stubPicker replaces the picker. It records the options it was shown and
// answers with choice; with choice < -1 a call fails the test.
func stubPicker(t *testing.T, choice int) *[][]string {
	t.Helper()
	orig := useChooseProject
	t.Cleanup(func() { useChooseProject = orig })
	var shown [][]string
	useChooseProject = func(title string, options []string) (int, error) {
		if choice < -1 {
			t.Errorf("the picker must not be called (title %q, options %v)", title, options)
		}
		shown = append(shown, options)
		return choice, nil
	}
	return &shown
}

func currentProjectOf(t *testing.T, hub string) string {
	t.Helper()
	c := readTestContext(t)
	h := c.Hubs[hub]
	require.NotNil(t, h, "hub %s", hub)
	if h.CurrentProject == nil {
		return ""
	}
	return h.CurrentProject.ID
}

// In a terminal, a project on two hubs is offered in the picker with both
// hubs, and the hub follows the chosen project.
func TestUseMultiHub_PickerOffersBoth(t *testing.T) {
	prod, staging := setupTwoHubs(t)
	withTerminal(t, true)
	shown := stubPicker(t, 1)

	stdout, _, err := runScion(t, "use", "foo")
	require.NoError(t, err)
	require.Len(t, *shown, 1)
	assert.Equal(t, []string{
		"foo on prod (" + prod.URL() + ")",
		"foo on staging (" + staging.URL() + ")",
	}, (*shown)[0])
	assert.Equal(t, "Now using project 'foo' on hub 'staging' ("+staging.URL()+").\n", stdout)
	assert.Equal(t, "staging", readTestContext(t).CurrentHub)
	assert.Equal(t, "s-foo", currentProjectOf(t, "staging"))
	assert.Equal(t, "", currentProjectOf(t, "prod"))
}

// Cancelling the picker selects nothing.
func TestUseMultiHub_PickerCancelled(t *testing.T) {
	setupTwoHubs(t)
	withTerminal(t, true)
	stubPicker(t, -1)
	_, _, err := runScion(t, "use", "foo")
	require.EqualError(t, err, "no project selected")
	assert.Empty(t, readTestContext(t).CurrentHub)
}

// --non-interactive never asks: it fails with one --hub line per hub, in
// text and in JSON.
func TestUseMultiHub_NonInteractiveAmbiguity(t *testing.T) {
	prod, staging := setupTwoHubs(t)
	withTerminal(t, true)
	stubPicker(t, -2)

	_, _, err := runScion(t, "--non-interactive", "use", "foo")
	require.Error(t, err)
	// The --hub column is aligned: "prod" is padded to the width of
	// "staging" plus three spaces.
	assert.Equal(t, "project 'foo' exists on several hubs; choose one with --hub:\n"+
		"  scion use foo --hub prod      ("+prod.URL()+", id p-foo)\n"+
		"  scion use foo --hub staging   ("+staging.URL()+", id s-foo)", err.Error())

	stdout, _, err := runScion(t, "--non-interactive", "use", "foo", "--format", "json")
	require.Error(t, err)
	var got map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(stdout), &got), stdout)
	assert.Equal(t, map[string]interface{}{
		"error": "ambiguous",
		"candidates": []interface{}{
			map[string]interface{}{"hub": "prod", "endpoint": prod.URL(), "id": "p-foo", "slug": "foo"},
			map[string]interface{}{"hub": "staging", "endpoint": staging.URL(), "id": "s-foo", "slug": "foo"},
		},
	}, got)
	assert.Empty(t, readTestContext(t).CurrentHub, "nothing is selected")
}

// --yes does not pick a candidate: without a terminal it is the ambiguity
// error, and in a terminal the picker still asks.
func TestUseMultiHub_YesDoesNotPick(t *testing.T) {
	setupTwoHubs(t)
	withTerminal(t, false)
	stubPicker(t, -2)
	_, _, err := runScion(t, "--yes", "use", "foo")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "project 'foo' exists on several hubs; choose one with --hub:")
	assert.Empty(t, readTestContext(t).CurrentHub)

	withTerminal(t, true)
	shown := stubPicker(t, 0)
	_, _, err = runScion(t, "--yes", "use", "foo")
	require.NoError(t, err)
	assert.Len(t, *shown, 1, "the picker asks even with --yes")
	assert.Equal(t, "prod", readTestContext(t).CurrentHub)
}

// --hub restricts the lookup to that hub, which picks the project.
func TestUseMultiHub_HubFlagPicks(t *testing.T) {
	prod, staging := setupTwoHubs(t)
	stubPicker(t, -2)
	stdout, _, err := runScion(t, "use", "foo", "--hub", "staging")
	require.NoError(t, err)
	assert.Equal(t, "Now using project 'foo' on hub 'staging' ("+staging.URL()+").\n", stdout)
	assert.Equal(t, "s-foo", currentProjectOf(t, "staging"))
	assert.Zero(t, prod.calls(), "the other hub is not asked")
}

// A name known on exactly one hub is checked with one call by ID on that
// hub: no listing, and no call to the other hubs.
func TestUseMultiHub_FastPathNoListing(t *testing.T) {
	prod, staging := setupTwoHubs(t)
	require.NoError(t, config.UpdateCLIContext(func(c *config.CLIContext) error {
		c.RememberProjects("prod", []config.KnownProject{{ID: "p-foo", Slug: "foo", Name: "Foo"}})
		return nil
	}))
	stdout, _, err := runScion(t, "use", "foo")
	require.NoError(t, err)
	assert.Equal(t, "Now using project 'foo' on hub 'prod' ("+prod.URL()+").\n", stdout)
	assert.Equal(t, int32(1), prod.gets.Load())
	assert.Zero(t, prod.lists.Load(), "no listing call")
	assert.Zero(t, staging.calls())
}

// With --refresh a known name is looked up on the hubs anyway.
func TestUseMultiHub_RefreshLooksUpKnownName(t *testing.T) {
	prod, staging := setupTwoHubs(t)
	staging.setProjects()
	require.NoError(t, config.UpdateCLIContext(func(c *config.CLIContext) error {
		c.RememberProjects("prod", []config.KnownProject{{ID: "p-foo", Slug: "foo"}})
		return nil
	}))
	_, _, err := runScion(t, "use", "foo", "--refresh")
	require.NoError(t, err)
	assert.Positive(t, prod.lists.Load(), "the hub is searched")
	assert.Positive(t, staging.lists.Load(), "every hub is searched")
	assert.Equal(t, "p-foo", currentProjectOf(t, "prod"))

	_, _, err = runScion(t, "use", "--refresh")
	require.EqualError(t, err, "--refresh needs a project")
}

// A known entry that its hub no longer has is dropped, and the name is
// looked up on the hubs again.
func TestUseMultiHub_StaleEntryDroppedAndLookedUp(t *testing.T) {
	prod, staging := setupTwoHubs(t)
	staging.setProjects()
	require.NoError(t, config.UpdateCLIContext(func(c *config.CLIContext) error {
		c.RememberProjects("prod", []config.KnownProject{{ID: "p-old", Slug: "foo"}})
		return nil
	}))
	stdout, _, err := runScion(t, "use", "foo")
	require.NoError(t, err)
	assert.Equal(t, "Now using project 'foo' on hub 'prod' ("+prod.URL()+").\n", stdout)
	assert.Equal(t, "p-foo", currentProjectOf(t, "prod"))
	for _, p := range readTestContext(t).Projects {
		assert.NotEqual(t, "p-old", p.ID, "the stale entry is dropped")
	}
	assert.Positive(t, staging.lists.Load(), "the lookup asks every hub")
}

// A stale known entry with no match left anywhere: the entry is still
// dropped, and the error names the hubs searched.
func TestUseMultiHub_StaleEntryNotFoundAnywhere(t *testing.T) {
	prod, staging := setupTwoHubs(t)
	prod.setProjects()
	staging.setProjects()
	require.NoError(t, config.UpdateCLIContext(func(c *config.CLIContext) error {
		c.RememberProjects("prod", []config.KnownProject{{ID: "p-old", Slug: "foo"}})
		return nil
	}))
	_, _, err := runScion(t, "use", "foo")
	require.EqualError(t, err, "project 'foo' not found on hubs: prod, staging")
	assert.Empty(t, readTestContext(t).Projects)
}

// One hub does not answer: a warning names it, and the other hub's result
// is used.
func TestUseMultiHub_OneHubUnreachable(t *testing.T) {
	prod, staging := setupTwoHubs(t)
	staging.srv.Close()
	stdout, stderr, err := runScion(t, "use", "foo")
	require.NoError(t, err)
	assert.Equal(t, "Now using project 'foo' on hub 'prod' ("+prod.URL()+").\n", stdout)
	assert.Contains(t, stderr, "Warning: skipped hubs: staging: unreachable: ")
}

// A known project whose hub does not answer is selected anyway, with a
// warning.
func TestUseMultiHub_KnownProjectHubUnreachable(t *testing.T) {
	prod, _ := setupTwoHubs(t)
	require.NoError(t, config.UpdateCLIContext(func(c *config.CLIContext) error {
		c.RememberProjects("prod", []config.KnownProject{{ID: "p-foo", Slug: "foo"}})
		return nil
	}))
	url := prod.URL()
	prod.srv.Close()
	stdout, stderr, err := runScion(t, "use", "foo")
	require.NoError(t, err)
	assert.Equal(t, "Now using project 'foo' on hub 'prod' ("+url+").\n", stdout)
	assert.Contains(t, stderr, "Warning: could not check project 'foo' on hub 'prod': ")
	assert.Equal(t, "p-foo", currentProjectOf(t, "prod"))
}

// A hub without a login is skipped and named; a known project on it is
// selected with the login hint, without calling it.
func TestUseMultiHub_NotLoggedIn(t *testing.T) {
	prod, _ := setupTwoHubs(t)
	// staging moves to a hub this machine has no login for.
	staging2 := newFakeProjectHub(t, stagingFoo)
	require.NoError(t, config.UpdateCLIContext(func(c *config.CLIContext) error {
		c.Hubs["staging"].Endpoint = staging2.URL()
		return nil
	}))

	stdout, stderr, err := runScion(t, "use", "foo")
	require.NoError(t, err)
	assert.Equal(t, "Now using project 'foo' on hub 'prod' ("+prod.URL()+").\n", stdout)
	assert.Contains(t, stderr, "Warning: skipped hubs: staging: not logged in")
	assert.Zero(t, staging2.calls())

	// A known project on the hub without a login: selected, with the hint.
	require.NoError(t, config.UpdateCLIContext(func(c *config.CLIContext) error {
		c.RememberProjects("staging", []config.KnownProject{{ID: "s-bar", Slug: "bar"}})
		return nil
	}))
	stdout, _, err = runScion(t, "use", "bar")
	require.NoError(t, err)
	assert.Equal(t, "Now using project 'bar' on hub 'staging' ("+staging2.URL()+").\n"+
		"Not logged in to hub 'staging'. Run: scion hub auth login --hub staging\n", stdout)
	assert.Zero(t, staging2.calls())

	// Not found on the logged-in hub: the error names the skipped hub and
	// its login command.
	_, _, err = runScion(t, "use", "nope")
	require.EqualError(t, err, "project 'nope' not found on hubs: prod (staging: not logged in)\n\n"+
		"Log in with: scion hub auth login --hub staging")
}

// In a terminal, when the only hub that could have the project is not
// logged in, 'scion use' offers the login and then looks the project up.
func TestUseMultiHub_LoginOffer(t *testing.T) {
	setupRegistryTest(t)
	writeHomeGlobalSettings(t, "")
	staging := newFakeProjectHub(t, stagingFoo)
	require.NoError(t, config.UpdateCLIContext(func(c *config.CLIContext) error {
		return c.AddHub("staging", staging.URL())
	}))
	withTerminal(t, true)

	origConfirm, origLogin := useConfirm, useLoginToHub
	t.Cleanup(func() { useConfirm, useLoginToHub = origConfirm, origLogin })
	var asked []string
	useConfirm = func(prompt string, defaultYes, auto bool) bool {
		asked = append(asked, prompt)
		assert.False(t, auto, "--yes never starts a login by itself")
		return true
	}
	var loggedIn []string
	useLoginToHub = func(_ *cobra.Command, endpoint string) error {
		loggedIn = append(loggedIn, endpoint)
		storeTestLogin(t, endpoint)
		return nil
	}

	stdout, _, err := runScion(t, "--yes", "use", "foo")
	require.NoError(t, err)
	assert.Equal(t, []string{"Log in to staging now?"}, asked)
	assert.Equal(t, []string{staging.URL()}, loggedIn)
	assert.Equal(t, "Now using project 'foo' on hub 'staging' ("+staging.URL()+").\n", stdout)

	// With --non-interactive no offer is made.
	require.NoError(t, credentials.Remove(staging.URL()))
	asked = nil
	_, _, err = runScion(t, "--non-interactive", "use", "bar")
	require.EqualError(t, err, "project 'bar' could not be looked up (staging: not logged in)\n\n"+
		"Log in with: scion hub auth login --hub staging")
	assert.Empty(t, asked)
}

// Switching hubs restores the project each hub remembers.
func TestUseMultiHub_SwitchRestoresEachHubsProject(t *testing.T) {
	prod, staging := setupTwoHubs(t)
	_, _, err := runScion(t, "use", "foo", "--hub", "prod")
	require.NoError(t, err)
	_, _, err = runScion(t, "use", "foo", "--hub", "staging")
	require.NoError(t, err)

	stdout, _, err := runScion(t, "use", "--hub", "prod")
	require.NoError(t, err)
	assert.Equal(t, "Now using project 'foo' on hub 'prod' ("+prod.URL()+").\n", stdout)
	assert.Equal(t, "prod", readTestContext(t).CurrentHub)

	stdout, _, err = runScion(t, "use", "--hub", "staging")
	require.NoError(t, err)
	assert.Equal(t, "Now using project 'foo' on hub 'staging' ("+staging.URL()+").\n", stdout)
	c := readTestContext(t)
	assert.Equal(t, "staging", c.CurrentHub)
	assert.Equal(t, "p-foo", c.Hubs["prod"].CurrentProject.ID)
	assert.Equal(t, "s-foo", c.Hubs["staging"].CurrentProject.ID)
}

// After an ambiguous lookup the candidates are known, so the next run is
// ambiguous without a lookup.
func TestUseMultiHub_KnownAmbiguityNeedsNoLookup(t *testing.T) {
	prod, staging := setupTwoHubs(t)
	_, _, err := runScion(t, "--non-interactive", "use", "foo")
	require.Error(t, err)
	before := prod.calls() + staging.calls()

	_, _, err = runScion(t, "--non-interactive", "use", "foo")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exists on several hubs")
	assert.Equal(t, before, prod.calls()+staging.calls(), "no hub call")
}

func TestUseMultiHub_HubTimeoutIsFiveSeconds(t *testing.T) {
	assert.Equal(t, 5*time.Second, useHubTimeout)
}

// withFanOutLocalHub decides whether the fake hubs count as local
// workstation hubs, where dev auth applies in a fan-out.
func withFanOutLocalHub(t *testing.T, local bool) {
	t.Helper()
	orig := fanOutLocalHub
	t.Cleanup(func() { fanOutLocalHub = orig })
	fanOutLocalHub = func(string) bool { return local }
}

// setupTwoHubsNoLogins is setupTwoHubs without the OAuth logins, with
// SCION_HUB_TOKEN and SCION_DEV_TOKEN set, and the fake hubs treated as
// remote hubs.
func setupTwoHubsNoLogins(t *testing.T) (prod, staging *fakeProjectHub) {
	t.Helper()
	setupRegistryTest(t)
	writeHomeGlobalSettings(t, "")
	prod = newFakeProjectHub(t, prodFoo)
	staging = newFakeProjectHub(t, stagingFoo)
	require.NoError(t, config.UpdateCLIContext(func(c *config.CLIContext) error {
		if err := c.AddHub("prod", prod.URL()); err != nil {
			return err
		}
		return c.AddHub("staging", staging.URL())
	}))
	t.Setenv("SCION_HUB_TOKEN", "scoped-token")
	t.Setenv("SCION_DEV_TOKEN", "scion_dev_test")
	withFanOutLocalHub(t, false)
	return prod, staging
}

// A lookup across hubs never sends a token that is not tied to a hub
// (SCION_HUB_TOKEN, the dev token of a remote hub): with no hub login, no
// hub is contacted and both are reported as not logged in.
func TestUseMultiHub_FanOutNeverSendsUnboundTokens(t *testing.T) {
	prod, staging := setupTwoHubsNoLogins(t)

	_, _, err := runScion(t, "use", "foo")
	require.EqualError(t, err, "project 'foo' could not be looked up (prod: not logged in; staging: not logged in)\n\n"+
		"Log in with: scion hub auth login --hub prod\n"+
		"Log in with: scion hub auth login --hub staging")
	assert.Zero(t, prod.calls())
	assert.Zero(t, staging.calls())

	_, stderr, err := runScion(t, "project", "list", "--refresh")
	require.NoError(t, err)
	assert.Contains(t, stderr, "Warning: not refreshed: prod: not logged in; staging: not logged in")
	assert.Zero(t, prod.calls())
	assert.Zero(t, staging.calls())
}

// A single hub chosen with --hub keeps the full credential chain, so
// SCION_HUB_TOKEN is used there, for the lookup and for the refresh.
func TestUseMultiHub_ExplicitHubUsesFullCredentialChain(t *testing.T) {
	prod, staging := setupTwoHubsNoLogins(t)

	stdout, _, err := runScion(t, "use", "foo", "--hub", "staging")
	require.NoError(t, err)
	assert.Equal(t, "Now using project 'foo' on hub 'staging' ("+staging.URL()+").\n", stdout)
	assert.Equal(t, []string{"Bearer scoped-token"}, staging.authHeaders())
	assert.Zero(t, prod.calls())

	_, _, err = runScion(t, "project", "list", "--refresh", "--hub", "prod")
	require.NoError(t, err)
	assert.Equal(t, []string{"Bearer scoped-token"}, prod.authHeaders())
}

// With an OAuth login on one hub only, a fan-out queries that hub, with
// its login, and skips the other.
func TestUseMultiHub_FanOutUsesOnlyHubLogins(t *testing.T) {
	prod, staging := setupTwoHubsNoLogins(t)
	storeTestLogin(t, prod.URL())

	stdout, stderr, err := runScion(t, "use", "foo")
	require.NoError(t, err)
	assert.Equal(t, "Now using project 'foo' on hub 'prod' ("+prod.URL()+").\n", stdout)
	assert.Contains(t, stderr, "Warning: skipped hubs: staging: not logged in")
	assert.Equal(t, []string{"Bearer tok"}, prod.authHeaders())
	assert.Zero(t, staging.calls())

	_, stderr, err = runScion(t, "project", "list", "--refresh")
	require.NoError(t, err)
	assert.Contains(t, stderr, "Warning: not refreshed: staging: not logged in")
	assert.Equal(t, []string{"Bearer tok"}, prod.authHeaders())
	assert.Zero(t, staging.calls())
}

// Dev auth counts in a fan-out only for a local workstation hub.
func TestUseMultiHub_FanOutDevAuthOnlyForLocalHubs(t *testing.T) {
	prod, staging := setupTwoHubsNoLogins(t)
	staging.setProjects()
	withFanOutLocalHub(t, true)

	_, _, err := runScion(t, "use", "foo")
	require.NoError(t, err)
	assert.Equal(t, []string{"Bearer scion_dev_test"}, prod.authHeaders())
	assert.Equal(t, []string{"Bearer scion_dev_test"}, staging.authHeaders())
}

// SCION_HUB_ENDPOINT restricts the lookup to that hub, even with other
// hubs registered.
func TestUseMultiHub_EnvHubRestrictsCandidates(t *testing.T) {
	prod, staging := setupTwoHubs(t)
	t.Setenv("SCION_HUB_ENDPOINT", staging.URL())
	stdout, _, err := runScion(t, "use", "foo")
	require.NoError(t, err)
	assert.Equal(t, "Now using project 'foo' on hub 'staging' ("+staging.URL()+").\n", stdout)
	assert.Zero(t, prod.calls())
}

// The hub in the settings joins the registered hubs when it has no entry.
func TestUseMultiHub_SettingsHubJoinsRegisteredHubs(t *testing.T) {
	prod, staging := setupTwoHubs(t)
	prod.setProjects()
	staging.setProjects()
	other := newFakeProjectHub(t, hubclient.Project{ID: "o-foo", Slug: "foo", Name: "Foo"})
	storeTestLogin(t, other.URL())
	writeHomeGlobalSettings(t, "hub:\n  endpoint: "+other.URL()+"\n")

	stdout, _, err := runScion(t, "use", "foo")
	require.NoError(t, err)
	assert.Equal(t, "Now using project 'foo' on hub 'local' ("+other.URL()+").\n", stdout)
	assert.Positive(t, prod.lists.Load(), "the registered hubs are searched too")
	assert.Positive(t, staging.lists.Load(), "the registered hubs are searched too")
	c := readTestContext(t)
	require.NotNil(t, c.Hubs["local"])
	assert.Equal(t, other.URL(), c.Hubs["local"].Endpoint)
}

// Each hub's lookup has its own timeout: a hub that never answers is
// reported unreachable, and the other hub's result is used.
func TestUseMultiHub_HangingHubTimesOut(t *testing.T) {
	prod, _ := setupTwoHubs(t)
	orig := useHubTimeout
	t.Cleanup(func() { useHubTimeout = orig })
	useHubTimeout = 100 * time.Millisecond

	hanging := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(hanging.Close)
	storeTestLogin(t, hanging.URL)
	require.NoError(t, config.UpdateCLIContext(func(c *config.CLIContext) error {
		c.Hubs["staging"].Endpoint = hanging.URL
		return nil
	}))

	start := time.Now()
	stdout, stderr, err := runScion(t, "use", "foo")
	require.NoError(t, err)
	assert.Less(t, time.Since(start), 5*time.Second)
	assert.Equal(t, "Now using project 'foo' on hub 'prod' ("+prod.URL()+").\n", stdout)
	assert.Contains(t, stderr, "Warning: skipped hubs: staging: unreachable: ")
}

// setupOneHubNoLogin registers one fake hub, prod, with a project "foo"
// and no login, with SCION_HUB_TOKEN and SCION_DEV_TOKEN set and the fake
// hubs treated as remote hubs.
func setupOneHubNoLogin(t *testing.T) *fakeProjectHub {
	t.Helper()
	setupRegistryTest(t)
	writeHomeGlobalSettings(t, "")
	prod := newFakeProjectHub(t, prodFoo)
	require.NoError(t, config.UpdateCLIContext(func(c *config.CLIContext) error {
		return c.AddHub("prod", prod.URL())
	}))
	t.Setenv("SCION_HUB_TOKEN", "scoped-token")
	t.Setenv("SCION_DEV_TOKEN", "scion_dev_test")
	withFanOutLocalHub(t, false)
	return prod
}

// The only registered hub is not the hub this invocation targets
// (SCION_HUB_ENDPOINT names another): it gets nothing that is not bound to
// it, from --refresh or from 'use'.
func TestUseMultiHub_LoneHubThatIsNotEffectiveGetsNoUnboundToken(t *testing.T) {
	prod := setupOneHubNoLogin(t)
	envHub := newFakeProjectHub(t)
	t.Setenv("SCION_HUB_ENDPOINT", envHub.URL())

	_, stderr, err := runScion(t, "project", "list", "--refresh")
	require.NoError(t, err)
	assert.Contains(t, stderr, "Warning: not refreshed: prod: not logged in")
	assert.Zero(t, prod.calls())

	_, _, err = runScion(t, "use", "foo")
	require.Error(t, err)
	assert.Zero(t, prod.calls(), "'use' searches only the environment's hub")
	assert.Equal(t, []string{"Bearer scoped-token"}, envHub.authHeaders(), "the hub the user chose gets the full chain")
}

// --hub naming the lone hub, or the lone hub being the effective hub, gives
// it the full credential chain.
func TestUseMultiHub_LoneHubChosenOrEffectiveGetsFullChain(t *testing.T) {
	t.Run("--hub", func(t *testing.T) {
		prod := setupOneHubNoLogin(t)
		t.Setenv("SCION_HUB_ENDPOINT", newFakeProjectHub(t).URL())
		_, _, err := runScion(t, "project", "list", "--refresh", "--hub", "prod")
		require.NoError(t, err)
		_, _, err = runScion(t, "use", "foo", "--hub", "prod")
		require.NoError(t, err)
		assert.Equal(t, []string{"Bearer scoped-token"}, prod.authHeaders())
	})
	t.Run("effective hub", func(t *testing.T) {
		prod := setupOneHubNoLogin(t)
		writeHomeGlobalSettings(t, "hub:\n  endpoint: "+prod.URL()+"/\n")
		_, _, err := runScion(t, "project", "list", "--refresh")
		require.NoError(t, err)
		assert.Positive(t, prod.lists.Load())
		_, _, err = runScion(t, "use", "foo")
		require.NoError(t, err)
		assert.Equal(t, []string{"Bearer scoped-token"}, prod.authHeaders())
	})
}

// The check of a known project follows the same rule: on a hub that is
// neither chosen nor effective, only that hub's own login counts, so with
// unbound tokens only the project is selected unchecked, with the login
// hint; on the effective hub the full chain is used.
func TestUseMultiHub_KnownProjectCheckFollowsCredentialRule(t *testing.T) {
	seed := func(t *testing.T) {
		t.Helper()
		require.NoError(t, config.UpdateCLIContext(func(c *config.CLIContext) error {
			c.RememberProjects("prod", []config.KnownProject{{ID: "p-foo", Slug: "foo", Name: "Foo"}})
			return nil
		}))
	}
	t.Run("not effective", func(t *testing.T) {
		prod, staging := setupTwoHubsNoLogins(t)
		seed(t)
		stdout, _, err := runScion(t, "use", "foo")
		require.NoError(t, err)
		assert.Equal(t, "Now using project 'foo' on hub 'prod' ("+prod.URL()+").\n"+
			"Not logged in to hub 'prod'. Run: scion hub auth login --hub prod\n", stdout)
		assert.Zero(t, prod.calls())
		assert.Zero(t, staging.calls())
	})
	t.Run("effective", func(t *testing.T) {
		prod, staging := setupTwoHubsNoLogins(t)
		seed(t)
		writeHomeGlobalSettings(t, "hub:\n  endpoint: "+prod.URL()+"\n")
		stdout, _, err := runScion(t, "use", "foo")
		require.NoError(t, err)
		assert.Equal(t, "Now using project 'foo' on hub 'prod' ("+prod.URL()+").\n", stdout)
		assert.Equal(t, []string{"Bearer scoped-token"}, prod.authHeaders())
		assert.Equal(t, int32(1), prod.gets.Load())
		assert.Zero(t, staging.calls())
	})
}

// In a fan-out only the hub entry's own transport applies: never the
// transport token from the environment, never hub.transport in settings.
func TestFanOutTransport_OnlyTheHubEntrysOwn(t *testing.T) {
	setupRegistryTest(t)
	writeHomeGlobalSettings(t, "hub:\n  transport:\n    mode: iap\n    audience: settings-audience\n")
	t.Setenv("SCION_TRANSPORT_TOKEN", "env-transport-token")
	origGCE, origADC := transportauth.IsOnGCEFunc, fanOutADCSource
	t.Cleanup(func() { transportauth.IsOnGCEFunc, fanOutADCSource = origGCE, origADC })
	transportauth.IsOnGCEFunc = func() bool { return false }
	var audiences []string
	fanOutADCSource = func(audience string) (transportauth.TokenSource, error) {
		audiences = append(audiences, audience)
		return nil, nil
	}
	require.NoError(t, config.UpdateCLIContext(func(c *config.CLIContext) error {
		if err := c.AddHub("plain", "https://plain.example.com"); err != nil {
			return err
		}
		if err := c.AddHub("iap", "https://iap.example.com"); err != nil {
			return err
		}
		c.Hubs["iap"].Transport = &config.HubTransportConfig{Mode: "iap", Audience: "entry-audience"}
		return nil
	}))

	src, _, err := fanOutTransport("https://plain.example.com")
	require.NoError(t, err)
	assert.Nil(t, src, "no entry transport: nothing, not the environment's or the settings' transport")
	assert.Empty(t, audiences)

	_, mode, err := fanOutTransport("https://iap.example.com")
	require.NoError(t, err)
	assert.Equal(t, []string{"entry-audience"}, audiences)
	assert.Equal(t, transportauth.ModeFromString("iap"), mode)
}
