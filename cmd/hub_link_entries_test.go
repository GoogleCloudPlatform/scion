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
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	linkTestHub       = "https://linked.example.com"
	linkTestHubID     = "dir-hub-id"
	linkTestPinnedHub = "https://pinned.example.com"
)

// makeLinkedDirProject creates a directory project linked to a hub project
// (hub.linked and hub.projectId in its settings), with hub.endpoint set to
// endpoint ("" for none), and moves into it. It returns the .scion path.
func makeLinkedDirProject(t *testing.T, endpoint string) string {
	t.Helper()
	root := t.TempDir()
	dotScion := filepath.Join(root, ".scion")
	require.NoError(t, os.MkdirAll(dotScion, 0o755))
	settings := "project_id: dir-project-id\nhub:\n  linked: true\n  projectId: " + linkTestHubID + "\n"
	if endpoint != "" {
		settings += "  endpoint: " + endpoint + "\n"
	}
	require.NoError(t, os.WriteFile(filepath.Join(dotScion, "settings.yaml"), []byte(settings), 0o644))
	t.Chdir(root)
	return dotScion
}

// saveLinkEntry records a link entry for the project at dotScion on the hub
// at endpoint, adding that hub as hubName when it has no entry.
func saveLinkEntry(t *testing.T, dotScion, endpoint, hubName string) {
	t.Helper()
	require.NoError(t, config.UpdateCLIContext(func(c *config.CLIContext) error {
		name, _ := c.EnsureHub(endpoint, hubName)
		return c.SetLinkEntry(name, config.KnownProject{ID: linkTestHubID, Slug: "bar"}, config.LinkPathForProject(dotScion), time.Now())
	}))
}

// TestResolveInvocationTarget_LinkEntry: a directory project with a link
// entry resolves to the link entry's hub, also after the current hub was
// switched to another hub and when its settings pin another endpoint. The
// flag and the environment still win. The hub from a link entry is not a
// --hub flag (it is never written to settings), and no context project is
// injected in the directory.
func TestResolveInvocationTarget_LinkEntry(t *testing.T) {
	tests := []struct {
		name          string
		pinned        string
		flag          string
		env           string
		wantHub       string
		wantHubSource string
	}{
		{name: "after a hub switch the link entry's hub is used", wantHub: linkTestHub, wantHubSource: targetSourceDirectory},
		{name: "the link entry beats an endpoint pinned in settings", pinned: linkTestPinnedHub, wantHub: linkTestHub, wantHubSource: targetSourceDirectory},
		{name: "the --hub flag beats the link entry", flag: "https://flag.example.com", wantHub: "https://flag.example.com"},
		{name: "the environment beats the link entry", env: "https://env.example.com"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setupContextTest(t)
			t.Setenv("SCION_HUB_ENDPOINT", tc.env)
			// The current hub is another hub, with a remembered project.
			saveTestContext(t, ctxTestHub, ctxTestProjectID)
			dotScion := makeLinkedDirProject(t, tc.pinned)
			saveLinkEntry(t, dotScion, linkTestHub, "linked")
			hubEndpoint = tc.flag

			require.NoError(t, resolveInvocationTarget(findCommand(t, "list"), ModeHuman))
			assert.Equal(t, tc.wantHub, hubEndpoint)
			assert.Equal(t, tc.wantHubSource, hubSource)
			assert.Empty(t, projectPath, "no context project is injected in a directory project")
			assert.Equal(t, tc.flag, hubFlagEndpoint(), "only an explicit --hub counts as the flag")
		})
	}
}

// TestResolveInvocationTarget_NoLinkEntryKeepsSettings: without a link
// entry, a directory project that pins an endpoint in its settings keeps
// resolving through its settings, not the context hub.
func TestResolveInvocationTarget_NoLinkEntryKeepsSettings(t *testing.T) {
	setupContextTest(t)
	saveTestContext(t, ctxTestHub, ctxTestProjectID)
	makeLinkedDirProject(t, linkTestPinnedHub)

	require.NoError(t, resolveInvocationTarget(findCommand(t, "list"), ModeHuman))
	assert.Empty(t, hubEndpoint, "the settings endpoint applies (read later by GetHubEndpoint)")
	assert.Empty(t, hubSource)
}

// TestUseShow_DirectoryLink: bare 'scion use' in a linked directory reports
// the hub source as the directory link.
func TestUseShow_DirectoryLink(t *testing.T) {
	setupUseTest(t)
	saveTestContext(t, ctxTestHub, ctxTestProjectID)
	dotScion := makeLinkedDirProject(t, "")
	saveLinkEntry(t, dotScion, linkTestHub, "linked")

	stdout, _, err := runScion(t, "use", "--format", "json")
	require.NoError(t, err)
	var got struct {
		Effective struct {
			Hub struct {
				Name, Endpoint, Source string
			} `json:"hub"`
		} `json:"effective"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &got), stdout)
	assert.Equal(t, "directory", got.Effective.Hub.Source)
	assert.Equal(t, linkTestHub, got.Effective.Hub.Endpoint)

	stdout, _, err = runScion(t, "use")
	require.NoError(t, err)
	assert.Contains(t, stdout, "(directory link)")
}

// TestHubFlagMismatchWarning: --hub naming a hub other than the one the
// directory project is linked to warns once on stderr, for commands that
// act on the directory's project.
func TestHubFlagMismatchWarning(t *testing.T) {
	const tail = "; the directory's project may not exist there.\n"
	tests := []struct {
		name      string
		linkEntry bool
		pinned    string
		flag      string
		cmd       []string
		project   string
		want      string
	}{
		{
			name: "link entry, unregistered URL", linkEntry: true, flag: "https://other.example.com", cmd: []string{"list"},
			want: "Warning: this directory is linked to hub 'linked' (" + linkTestHub + "), but --hub selects https://other.example.com" + tail,
		},
		{
			name: "link entry, nickname", linkEntry: true, flag: "prod", cmd: []string{"list"},
			want: "Warning: this directory is linked to hub 'linked' (" + linkTestHub + "), but --hub selects 'prod' (" + ctxTestHub + ")" + tail,
		},
		{
			name: "link entry, registered URL", linkEntry: true, flag: ctxTestHub, cmd: []string{"list"},
			want: "Warning: this directory is linked to hub 'linked' (" + linkTestHub + "), but --hub selects 'prod' (" + ctxTestHub + ")" + tail,
		},
		{
			name: "endpoint pinned in settings, no link entry", pinned: linkTestPinnedHub, flag: "https://other.example.com", cmd: []string{"list"},
			want: "Warning: this directory is linked to hub " + linkTestPinnedHub + ", but --hub selects https://other.example.com" + tail,
		},
		{name: "the same hub: no warning", linkEntry: true, flag: linkTestHub + "/", cmd: []string{"list"}},
		{name: "not a project command: no warning", linkEntry: true, flag: "https://other.example.com", cmd: []string{"config", "list"}},
		{name: "--project given: no warning", linkEntry: true, flag: "https://other.example.com", cmd: []string{"list"}, project: "some-slug"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setupContextTest(t)
			saveTestContext(t, ctxTestHub, ctxTestProjectID)
			dotScion := makeLinkedDirProject(t, tc.pinned)
			if tc.linkEntry {
				saveLinkEntry(t, dotScion, linkTestHub, "linked")
			}
			hubEndpoint, projectPath = tc.flag, tc.project
			c := findCommand(t, tc.cmd...)
			var stderr bytes.Buffer
			c.SetErr(&stderr)
			t.Cleanup(func() { c.SetErr(nil) })

			require.NoError(t, resolveInvocationTarget(c, ModeHuman))
			assert.Equal(t, tc.want, stderr.String())
		})
	}
}

// TestHubFlagMismatchWarning_UnlinkedProject: a directory project that is
// not linked gets no warning, whatever --hub says.
func TestHubFlagMismatchWarning_UnlinkedProject(t *testing.T) {
	setupContextTest(t)
	saveTestContext(t, ctxTestHub, ctxTestProjectID)
	makeDirProject(t, linkTestPinnedHub)
	hubEndpoint = "https://other.example.com"
	c := findCommand(t, "list")
	var stderr bytes.Buffer
	c.SetErr(&stderr)
	t.Cleanup(func() { c.SetErr(nil) })

	require.NoError(t, resolveInvocationTarget(c, ModeHuman))
	assert.Empty(t, stderr.String())
}

// TestHubUnlink_RemovesLinkEntry: 'scion hub unlink' removes the project's
// link entry, keeps other entries and leaves the current context alone.
func TestHubUnlink_RemovesLinkEntry(t *testing.T) {
	setupContextTest(t)
	origYes := autoConfirm
	t.Cleanup(func() { autoConfirm = origYes })
	saveTestContext(t, ctxTestHub, ctxTestProjectID)
	dotScion := makeLinkedDirProject(t, "")
	saveLinkEntry(t, dotScion, linkTestHub, "linked")
	other := filepath.Join(t.TempDir(), ".scion")
	saveLinkEntry(t, other, linkTestHub, "linked")
	projectPath, autoConfirm = dotScion, true

	var runErr error
	_ = captureStdout(t, func() { runErr = runHubUnlink(hubUnlinkCmd, nil) })
	require.NoError(t, runErr)

	c := readTestContext(t)
	entry, _ := c.LinkEntryFor(config.LinkPathForProject(dotScion))
	assert.Nil(t, entry, "the project's link entry is removed")
	otherEntry, _ := c.LinkEntryFor(config.LinkPathForProject(other))
	assert.NotNil(t, otherEntry, "another directory's link entry is kept")
	assert.Equal(t, "prod", c.CurrentHub, "the current hub is not touched")
	require.NotNil(t, c.Hubs["prod"].CurrentProject)
	assert.Equal(t, ctxTestProjectID, c.Hubs["prod"].CurrentProject.ID, "the current project is not touched")
}

// TestHubUnlink_NotLinkedRemovesLeftoverEntry: unlink on a project that is
// already unlinked still removes a link entry left behind for it.
func TestHubUnlink_NotLinkedRemovesLeftoverEntry(t *testing.T) {
	setupContextTest(t)
	origYes := autoConfirm
	t.Cleanup(func() { autoConfirm = origYes })
	root := t.TempDir()
	dotScion := filepath.Join(root, ".scion")
	require.NoError(t, os.MkdirAll(dotScion, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dotScion, "settings.yaml"),
		[]byte("project_id: dir-project-id\nhub:\n  linked: false\n"), 0o644))
	t.Chdir(root)
	saveLinkEntry(t, dotScion, linkTestHub, "linked")
	projectPath, autoConfirm = dotScion, true

	var runErr error
	stdout := captureStdout(t, func() { runErr = runHubUnlink(hubUnlinkCmd, nil) })
	require.NoError(t, runErr)
	assert.Contains(t, stdout, "This project is not linked to the Hub.")
	entry, _ := readTestContext(t).LinkEntryFor(config.LinkPathForProject(dotScion))
	assert.Nil(t, entry, "the leftover link entry is removed")
}

// TestRegisterLocalHub covers what 'scion server start' does to the hub
// list: it registers the local hub once, and makes it current only when
// no hub is current and the global hub.endpoint is unset or names the
// local hub.
func TestRegisterLocalHub(t *testing.T) {
	const local = "http://127.0.0.1:8080"
	const registered = "Registered hub 'local' (" + local + "); rename with 'scion hub rename'.\n"

	t.Run("empty hub list: registered and current, then idempotent", func(t *testing.T) {
		setupContextTest(t)
		var out, errOut bytes.Buffer
		registerLocalHub(&out, &errOut, local)
		assert.Equal(t, registered, out.String())
		assert.Empty(t, errOut.String())
		c := readTestContext(t)
		require.NotNil(t, c.Hubs["local"])
		assert.Equal(t, local, c.Hubs["local"].Endpoint)
		assert.Equal(t, "local", c.CurrentHub)

		before := readFileT(t, contextFilePath())
		out.Reset()
		registerLocalHub(&out, &errOut, local+"/")
		assert.Empty(t, out.String(), "a second start prints nothing")
		assert.Equal(t, before, readFileT(t, contextFilePath()), "a second start does not rewrite the file")
		assert.Len(t, readTestContext(t).Hubs, 1)
	})

	t.Run("global hub.endpoint is the local hub: registered and current", func(t *testing.T) {
		setupContextTest(t)
		writeHomeGlobalSettings(t, "hub:\n  endpoint: "+local+"/\n")
		var out, errOut bytes.Buffer
		registerLocalHub(&out, &errOut, local)
		assert.Equal(t, registered, out.String())
		assert.Equal(t, "local", readTestContext(t).CurrentHub)
	})

	t.Run("global hub.endpoint names a remote hub: registered, not current", func(t *testing.T) {
		setupContextTest(t)
		writeHomeGlobalSettings(t, "hub:\n  endpoint: https://remote.example.com\n")
		var out, errOut bytes.Buffer
		registerLocalHub(&out, &errOut, local)
		assert.Equal(t, registered, out.String(), "the local hub is still registered")
		c := readTestContext(t)
		require.NotNil(t, c.Hubs["local"])
		assert.Empty(t, c.CurrentHub, "the remote hub of the global settings keeps applying outside projects")
		assert.Equal(t, "https://remote.example.com", settingsEndpoint(t, filepath.Join(os.Getenv("HOME"), ".scion")), "global settings unchanged")
	})

	t.Run("another hub is current: registered, not current", func(t *testing.T) {
		setupContextTest(t)
		saveTestContext(t, ctxTestHub, ctxTestProjectID)
		var out, errOut bytes.Buffer
		registerLocalHub(&out, &errOut, local)
		assert.Equal(t, registered, out.String())
		c := readTestContext(t)
		assert.NotNil(t, c.Hubs["local"])
		assert.Equal(t, "prod", c.CurrentHub)
	})

	t.Run("endpoint already registered under another name: made current, no new entry", func(t *testing.T) {
		setupContextTest(t)
		require.NoError(t, config.UpdateCLIContext(func(c *config.CLIContext) error {
			return c.AddHub("mine", local)
		}))
		var out, errOut bytes.Buffer
		registerLocalHub(&out, &errOut, local)
		assert.Empty(t, out.String())
		c := readTestContext(t)
		assert.Len(t, c.Hubs, 1)
		assert.Equal(t, "mine", c.CurrentHub)
	})

	t.Run("inside an agent: nothing written", func(t *testing.T) {
		setupContextTest(t)
		t.Setenv("SCION_HOST_UID", "1000")
		var out, errOut bytes.Buffer
		registerLocalHub(&out, &errOut, local)
		assert.Empty(t, out.String())
		_, err := os.Stat(contextFilePath())
		assert.True(t, os.IsNotExist(err), "no context.yaml is written")
	})
}

// TestPrintWorkstationQuickstart_RegistersLocalHub: the workstation start
// registers the local hub in the hub list and no longer writes the global
// hub.endpoint.
func TestPrintWorkstationQuickstart_RegistersLocalHub(t *testing.T) {
	dir := t.TempDir()
	stubQuickstartProbes(t, true, onboardingStatusResult{Complete: true, EmbeddedBrokerID: "b1"}, nil)
	t.Setenv("SCION_CLI_MODE", string(ModeHuman))

	out := captureStdout(t, func() {
		printWorkstationQuickstart(false, dir, "0.0.0.0", 9090, true, false, true, true)
	})

	assert.Contains(t, out, "Registered hub 'local' (http://127.0.0.1:9090); rename with 'scion hub rename'.")
	assert.NotContains(t, out, "Configured hub endpoint")
	assert.Empty(t, settingsEndpoint(t, dir), "the global hub.endpoint is not written")
	c := readTestContext(t)
	require.NotNil(t, c.Hubs["local"])
	assert.Equal(t, "http://127.0.0.1:9090", c.Hubs["local"].Endpoint)
	assert.Equal(t, "local", c.CurrentHub)
}

// TestContextLinkRecorder: the registration step records a link entry only
// when the hub came from the current context.
func TestContextLinkRecorder(t *testing.T) {
	setupContextTest(t)
	saveTestContext(t, ctxTestHub, ctxTestProjectID)
	dotScion := makeLinkedDirProject(t, "")

	hubSource = ""
	assert.Nil(t, contextLinkRecorder(os.Stderr, os.Stderr), "no recorder for a hub that is not from the context")
	hubSource = targetSourceDirectory
	assert.Nil(t, contextLinkRecorder(os.Stderr, os.Stderr), "no recorder for a hub from a link entry")

	hubSource = targetSourceContext
	record := contextLinkRecorder(os.Stderr, os.Stderr)
	require.NotNil(t, record)
	record(ctxTestHub, "new-hub-id", dotScion, false)
	entry, hub := readTestContext(t).LinkEntryFor(config.LinkPathForProject(dotScion))
	require.NotNil(t, entry)
	assert.Equal(t, "new-hub-id", entry.ID)
	assert.Equal(t, "prod", entry.Hub)
	assert.Equal(t, ctxTestHub, hub.Endpoint)

	// After a hub switch, the directory still resolves to that hub.
	require.NoError(t, config.UpdateCLIContext(func(c *config.CLIContext) error {
		name, _ := c.EnsureHub("https://other.example.com", "other")
		c.CurrentHub = name
		return nil
	}))
	resetContextTargets()
	require.NoError(t, resolveInvocationTarget(findCommand(t, "list"), ModeHuman))
	assert.Equal(t, ctxTestHub, hubEndpoint)
	assert.Equal(t, targetSourceDirectory, hubSource)
}

// TestRecordLinkEntry_NewHubIsAnnounced: a link to a hub that has no entry
// adds one and announces it; a known hub is not announced.
func TestRecordLinkEntry_NewHubIsAnnounced(t *testing.T) {
	setupContextTest(t)
	saveTestContext(t, ctxTestHub, "")
	dotScion := makeLinkedDirProject(t, "")

	var out, errOut bytes.Buffer
	recordLinkEntry(&out, &errOut, ctxTestHub, "id-1", "foo", "Foo", dotScion)
	assert.Empty(t, out.String(), "a known hub is not announced")

	recordLinkEntry(&out, &errOut, "https://new.example.com/", "id-2", "foo", "Foo", dotScion)
	assert.Equal(t, "Registered hub 'new' (https://new.example.com); rename with 'scion hub rename'.\n", out.String())
	assert.Empty(t, errOut.String())
	entry, _ := readTestContext(t).LinkEntryFor(config.LinkPathForProject(dotScion))
	require.NotNil(t, entry)
	assert.Equal(t, "new", entry.Hub, "re-linking the directory replaces its link entry")
	assert.Equal(t, "id-2", entry.ID)
}

// statusContextHub is a fake hub for hub status: health, an authenticated
// user, the project ctxTestProjectID ("Foo") when known is true, and no
// brokers.
func statusContextHub(t *testing.T, known bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/healthz":
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		case r.URL.Path == "/api/v1/auth/me":
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "u1", "email": "alice@example.com", "displayName": "Alice", "role": "member"})
		case r.URL.Path == "/api/v1/projects/"+ctxTestProjectID && known:
			_ = json.NewEncoder(w).Encode(map[string]string{"id": ctxTestProjectID, "name": "Foo", "slug": "foo"})
		case r.URL.Path == "/api/v1/runtime-brokers":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"brokers": []interface{}{}})
		default:
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"error": map[string]interface{}{"code": "not_found", "message": "not found"},
			})
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// runHubStatusForTest resolves the target of 'scion hub status' and runs
// it, returning stdout.
func runHubStatusForTest(t *testing.T) string {
	t.Helper()
	status := findCommand(t, "hub", "status")
	require.NoError(t, resolveInvocationTarget(status, ModeHuman))
	var runErr error
	out := captureStdout(t, func() { runErr = runHubStatus(status, nil) })
	require.NoError(t, runErr)
	return out
}

// TestHubStatus_ContextProject: outside any project directory, hub status
// shows the hub's nickname and the project selected with 'scion use', not
// the global project with a link hint.
func TestHubStatus_ContextProject(t *testing.T) {
	setupContextTest(t)
	t.Setenv("SCION_DEV_TOKEN", "scion_dev_test")
	srv := statusContextHub(t, true)
	saveTestContext(t, srv.URL, ctxTestProjectID)

	out := runHubStatusForTest(t)
	assert.Contains(t, out, "Hub:        prod\n")
	assert.Contains(t, out, "            (current context)\n")
	assert.Contains(t, out, "Project:    foo\n")
	assert.Contains(t, out, "Type:       hub project (current context)\n")
	assert.Contains(t, out, "Hub Project:  Foo (ID: "+ctxTestProjectID+")\n")
	assert.NotContains(t, out, "Linked: no")
	assert.NotContains(t, out, "hub link")
	assert.NotContains(t, out, "user global")
}

// TestHubStatus_ContextProjectNotFound: a context project the hub does not
// know gets the not-found line that points to 'scion use'.
func TestHubStatus_ContextProjectNotFound(t *testing.T) {
	setupContextTest(t)
	t.Setenv("SCION_DEV_TOKEN", "scion_dev_test")
	srv := statusContextHub(t, false)
	saveTestContext(t, srv.URL, ctxTestProjectID)

	out := runHubStatusForTest(t)
	assert.Contains(t, out, "The current project 'foo' (hub 'prod') was not found; run 'scion use' to pick another.\n")
	assert.NotContains(t, out, "Linked: no")
}

// TestHubStatusJSON_ContextProject: the JSON projectContext describes the
// context project, with the hub nickname at the top level.
func TestHubStatusJSON_ContextProject(t *testing.T) {
	setupContextTest(t)
	t.Setenv("SCION_DEV_TOKEN", "scion_dev_test")
	srv := statusContextHub(t, true)
	saveTestContext(t, srv.URL, ctxTestProjectID)
	outputFormat = "json"

	out := runHubStatusForTest(t)
	var got map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(out), &got), out)
	assert.Equal(t, "prod", got["hub"])
	assert.Equal(t, "context", got["endpointSource"])
	pc, ok := got["projectContext"].(map[string]interface{})
	require.True(t, ok, "projectContext: %v", got["projectContext"])
	assert.Equal(t, "foo", pc["name"])
	assert.Equal(t, "hub project", pc["type"])
	assert.Equal(t, "context", pc["source"])
	assert.Equal(t, ctxTestProjectID, pc["hubProjectId"])
	assert.Equal(t, "Foo", pc["hubProjectName"])
	_, hasLinked := pc["linked"]
	assert.False(t, hasLinked, "no linked key for a context project")
}

// TestHubStatus_DirectoryLinkSource: in a linked directory, hub status
// names the link entry's hub and the directory link as the source.
func TestHubStatus_DirectoryLinkSource(t *testing.T) {
	setupContextTest(t)
	saveTestContext(t, ctxTestHub, ctxTestProjectID)
	dotScion := makeLinkedDirProject(t, "")
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	saveLinkEntry(t, dotScion, url, "linked")
	outputFormat = "json"

	out := runHubStatusForTest(t)
	var got map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(out), &got), out)
	assert.Equal(t, "linked", got["hub"])
	assert.Equal(t, url, got["endpoint"])
	assert.Equal(t, "directory", got["endpointSource"])
}

// TestCompletion_HubNicknamesAndKnownProjects: --hub completes nicknames,
// and 'use <project>' completes known slugs (filtered by --hub), offline.
func TestCompletion_HubNicknamesAndKnownProjects(t *testing.T) {
	setupContextTest(t)
	require.NoError(t, config.UpdateCLIContext(func(c *config.CLIContext) error {
		if err := c.AddHub("prod", ctxTestHub); err != nil {
			return err
		}
		if err := c.AddHub("local", "http://127.0.0.1:8080"); err != nil {
			return err
		}
		c.Projects = []config.KnownProject{
			{Hub: "prod", ID: "1", Slug: "foo", Source: config.KnownProjectSourceUse},
			{Hub: "local", ID: "2", Slug: "bar", Source: config.KnownProjectSourceLink, LocalPath: "/src/bar"},
			{Hub: "local", ID: "3", Slug: "foo", Source: config.KnownProjectSourceRefresh},
			{Hub: "prod", ID: "4", Source: config.KnownProjectSourceRefresh},
		}
		return nil
	}))

	names, dir := completeHubNames(nil, nil, "")
	assert.Equal(t, []string{"local", "prod"}, names)
	assert.Equal(t, cobra.ShellCompDirectiveNoFileComp, dir)
	names, _ = completeHubNames(nil, nil, "p")
	assert.Equal(t, []string{"prod"}, names)

	slugs, dir := completeUseProjects(nil, nil, "")
	assert.Equal(t, []string{"bar", "foo"}, slugs)
	assert.Equal(t, cobra.ShellCompDirectiveNoFileComp, dir)
	slugs, _ = completeUseProjects(nil, nil, "f")
	assert.Equal(t, []string{"foo"}, slugs)
	slugs, _ = completeUseProjects(nil, []string{"foo"}, "")
	assert.Empty(t, slugs, "only one project is taken")

	hubEndpoint = "local"
	slugs, _ = completeUseProjects(nil, nil, "")
	assert.Equal(t, []string{"bar", "foo"}, slugs)
	hubEndpoint = ctxTestHub
	slugs, _ = completeUseProjects(nil, nil, "")
	assert.Equal(t, []string{"foo"}, slugs, "a --hub URL filters by its entry")
	hubEndpoint = "nope"
	slugs, _ = completeUseProjects(nil, nil, "")
	assert.Empty(t, slugs, "an unknown --hub offers nothing")

	// The completion functions are registered on --hub and on both use
	// commands.
	assert.NotNil(t, projectUseCmd.ValidArgsFunction)
	assert.NotNil(t, useCmd.ValidArgsFunction)
	assert.Error(t, rootCmd.RegisterFlagCompletionFunc("hub", completeHubNames),
		"--hub already has a completion function")
}

// TestCompletion_InsideAgentOffersNothing: inside an agent the hub list is
// not used for completion either.
func TestCompletion_InsideAgentOffersNothing(t *testing.T) {
	setupContextTest(t)
	saveTestContext(t, ctxTestHub, ctxTestProjectID)
	t.Setenv("SCION_HOST_UID", "1000")
	names, _ := completeHubNames(nil, nil, "")
	assert.Empty(t, names)
	slugs, _ := completeUseProjects(nil, nil, "")
	assert.Empty(t, slugs)
}

// TestDevAuthBanner_NotOnStdout: the dev auth warning goes to stderr, so
// JSON output on stdout stays parseable.
func TestDevAuthBanner_NotOnStdout(t *testing.T) {
	setupUseTest(t)
	saveTestContext(t, ctxTestHub, ctxTestProjectID)

	stdout, stderr, err := runScion(t, "use", "--format", "json")
	require.NoError(t, err)
	assert.Contains(t, stderr, "Development authentication enabled")
	assert.NotContains(t, stdout, "WARNING")
	var got map[string]interface{}
	assert.NoError(t, json.Unmarshal([]byte(stdout), &got), stdout)
}
