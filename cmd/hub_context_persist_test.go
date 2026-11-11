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
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// settingsEndpoint returns hub.endpoint from the settings file in dir ("" if
// none).
func settingsEndpoint(t *testing.T, dir string) string {
	t.Helper()
	s, err := config.LoadSettingsFromDir(dir)
	require.NoError(t, err)
	if s.Hub == nil {
		return ""
	}
	return s.Hub.Endpoint
}

// healthOnlyHub is a fake hub that answers only the health check.
func healthOnlyHub(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/healthz" {
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestHubEnable_EndpointSource: 'scion hub enable' was removed with
// local-only mode. It fails with guidance and writes nothing, so neither the
// hub from the current context nor a --hub endpoint is written to settings.
func TestHubEnable_EndpointSource(t *testing.T) {
	for _, tc := range []struct {
		name string
		flag bool
	}{{name: "context hub is not written"}, {name: "--hub is not written either", flag: true}} {
		t.Run(tc.name, func(t *testing.T) {
			setupContextTest(t)
			srv := healthOnlyHub(t)
			writeHomeGlobalSettings(t, "image_registry: registry.example.test/team\n")
			saveTestContext(t, srv.URL, ctxTestProjectID)
			if tc.flag {
				hubEndpoint = srv.URL
			}

			enable := findCommand(t, "hub", "enable")
			require.NoError(t, resolveInvocationTarget(enable, ModeHuman))
			require.Equal(t, srv.URL, hubEndpoint)
			var err error
			_ = captureStdout(t, func() { err = runHubEnable(enable, nil) })
			require.ErrorIs(t, err, errHubEnableRemoved)

			assert.Empty(t, settingsEndpoint(t, filepath.Join(os.Getenv("HOME"), ".scion")),
				"the removed hub enable writes no hub.endpoint")
		})
	}
}

// TestHubLink_EndpointSource: 'scion --global hub link' writes link
// identity and a link entry in the hub list, and never pins the hub as
// hub.endpoint: not for the hub selected with 'scion use', and not for an
// explicit --hub either.
func TestHubLink_EndpointSource(t *testing.T) {
	for _, tc := range []struct {
		name string
		flag bool
	}{{name: "context hub"}, {name: "--hub", flag: true}} {
		t.Run(tc.name, func(t *testing.T) {
			setupContextTest(t)
			origYes, origOffer := autoConfirm, offerTemplateSyncOnLinkFn
			t.Cleanup(func() { autoConfirm, offerTemplateSyncOnLinkFn = origYes, origOffer })
			offerTemplateSyncOnLinkFn = func(_, _, _ string, _ bool) {}
			writeHomeGlobalSettings(t, "schema_version: \"1\"\n")
			t.Chdir(os.Getenv("HOME"))
			globalMode, autoConfirm = true, true
			t.Setenv("SCION_DEV_TOKEN", "scion_dev_test")
			server, registered := fakeLinkHub(t)
			saveTestContext(t, server.URL, ctxTestProjectID)
			if tc.flag {
				hubEndpoint = server.URL
			}

			require.NoError(t, resolveInvocationTarget(hubLinkCmd, ModeHuman))
			require.Equal(t, server.URL, hubEndpoint)
			_ = captureStdout(t, func() { require.NoError(t, runHubLink(hubLinkCmd, nil)) })
			require.True(t, *registered, "the project was linked")

			globalDir := filepath.Join(os.Getenv("HOME"), ".scion")
			assert.Empty(t, settingsEndpoint(t, globalDir), "hub link must not pin hub.endpoint")
			linkPath := config.LinkPathForProject(globalDir)
			entry, hub := readTestContext(t).LinkEntryFor(linkPath)
			require.NotNil(t, entry, "hub link records a link entry for %s", linkPath)
			assert.Equal(t, "prod", entry.Hub)
			assert.Equal(t, config.NormalizeEndpoint(server.URL), hub.Endpoint)
			assert.NotEmpty(t, entry.ID)
		})
	}
}

// TestRequireHub_ContextHubDoesNotRelink: in a directory project
// already linked to a hub (hub.projectId) and without an endpoint of its
// own, the hub selected with 'scion use' does not link or register the
// project: the command gets the existing not-linked error, the hub gets no
// write request, and the project's settings are unchanged.
func TestRequireHub_ContextHubDoesNotRelink(t *testing.T) {
	setupContextTest(t)
	t.Setenv("SCION_DEV_TOKEN", "scion_dev_test")
	var writes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet {
			writes.Add(1)
		}
		switch r.URL.Path {
		case "/healthz":
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		case "/api/v1/projects":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"projects": []interface{}{}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	saveTestContext(t, srv.URL, ctxTestProjectID)
	root := t.TempDir()
	settingsFile := filepath.Join(root, ".scion", "settings.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(settingsFile), 0o755))
	require.NoError(t, os.WriteFile(settingsFile,
		[]byte("project_id: dir-project-id\nhub:\n  enabled: true\n  projectId: linked-hub-id\n"), 0o644))
	t.Chdir(root)
	before := readFileT(t, settingsFile)

	require.NoError(t, resolveInvocationTarget(findCommand(t, "list"), ModeHuman))
	require.Equal(t, targetSourceContext, hubSource)
	require.Empty(t, projectPath, "a directory project is not replaced by the context project")

	_, err := requireHub(projectPath)
	require.Error(t, err)
	assert.Equal(t, "project must be linked to Hub to perform this operation\n\n"+
		"Link this project: scion hub link\n"+
		"If it is linked to another hub, pass --hub <name>, or run 'scion hub link --hub <name>' once so this directory stays on that hub.", err.Error())
	assert.Zero(t, writes.Load(), "no register or create request")
	assert.Equal(t, before, readFileT(t, settingsFile), "project settings unchanged")
}

// TestRequireHub_ContextHubAutoLinksNeverLinkedProject pins phase 3's
// auto-link through the hub selected with 'scion use' (the hub 'scion server
// start' registers): a directory project that was never linked, with the v1
// settings 'scion init' writes (its own local ID as hub.project_id), is
// linked to the local workstation hub without a prompt by any requireHub
// command, here 'scion list'.
func TestRequireHub_ContextHubAutoLinksNeverLinkedProject(t *testing.T) {
	setupContextTest(t)
	t.Setenv("SCION_DEV_TOKEN", "scion_dev_test")
	srv, registers := unregisteredLinkHub(t)
	saveTestContext(t, srv.URL, "")
	root := t.TempDir()
	settingsFile := filepath.Join(root, ".scion", "settings.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(settingsFile), 0o755))
	require.NoError(t, os.WriteFile(settingsFile,
		[]byte("schema_version: \"1\"\nhub:\n  project_id: dir-project-id\n"), 0o644))
	t.Chdir(root)
	origYes, origNI := autoConfirm, nonInteractive
	t.Cleanup(func() { autoConfirm, nonInteractive = origYes, origNI })
	autoConfirm, nonInteractive = false, false

	require.NoError(t, resolveInvocationTarget(findCommand(t, "list"), ModeHuman))
	require.Equal(t, targetSourceContext, hubSource)
	require.Empty(t, projectPath, "the directory project is used")

	// No terminal and no --yes: a link prompt would decline, so one
	// register call shows the project was linked without one. (The
	// "Linking project" line goes to hubsync's prompt writer, which is
	// pinned in hubsync's TestEnsureHubReady_HubFromContextV1Settings.)
	var ctx *HubContext
	var err error
	captureStdIO(t, func() { ctx, err = requireHub(projectPath) })
	require.NoError(t, err)
	require.NotNil(t, ctx)
	assert.EqualValues(t, 1, registers.Load(), "linked without a prompt (no terminal, no --yes)")
}
