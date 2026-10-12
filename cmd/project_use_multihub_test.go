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
		return true
	}
	var loggedIn []string
	useLoginToHub = func(_ *cobra.Command, endpoint string) error {
		loggedIn = append(loggedIn, endpoint)
		storeTestLogin(t, endpoint)
		return nil
	}

	stdout, _, err := runScion(t, "use", "foo")
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
