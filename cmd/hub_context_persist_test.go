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

// TestPersistHubFlagEndpoint covers the shared write path of hub enable and
// hub link: a hub from the current context is never written, at project or
// global scope, and an explicit --hub still is.
func TestPersistHubFlagEndpoint(t *testing.T) {
	for _, scope := range []string{"project", "global"} {
		t.Run(scope+": a context hub is not written", func(t *testing.T) {
			setupContextTest(t)
			writeHomeGlobalSettings(t, "image_registry: registry.example.test/team\n")
			saveTestContext(t, ctxTestHub, ctxTestProjectID)
			root := makeDirProject(t, "")
			globalDir := filepath.Join(os.Getenv("HOME"), ".scion")
			projectDir := filepath.Join(root, ".scion")
			require.NoError(t, resolveInvocationTarget(findCommand(t, "hub", "link"), ModeHuman))
			require.Equal(t, targetSourceContext, hubSource)
			require.Equal(t, ctxTestHub, hubEndpoint)
			require.Empty(t, hubFlagEndpoint())

			globalBefore := readFileT(t, filepath.Join(globalDir, "settings.yaml"))
			projectBefore := readFileT(t, filepath.Join(projectDir, "settings.yaml"))
			target, isGlobal := projectDir, false
			if scope == "global" {
				target, isGlobal = globalDir, true
			}
			require.NoError(t, persistHubFlagEndpoint(target, isGlobal))
			assert.Equal(t, globalBefore, readFileT(t, filepath.Join(globalDir, "settings.yaml")), "global settings unchanged")
			assert.Equal(t, projectBefore, readFileT(t, filepath.Join(projectDir, "settings.yaml")), "project settings unchanged")
		})

		t.Run(scope+": the --hub flag is written", func(t *testing.T) {
			setupContextTest(t)
			writeHomeGlobalSettings(t, "image_registry: registry.example.test/team\n")
			saveTestContext(t, ctxTestHub, ctxTestProjectID)
			root := makeDirProject(t, "")
			globalDir := filepath.Join(os.Getenv("HOME"), ".scion")
			projectDir := filepath.Join(root, ".scion")
			hubEndpoint = "https://flag.example.com"
			require.NoError(t, resolveInvocationTarget(findCommand(t, "hub", "link"), ModeHuman))
			require.Equal(t, "https://flag.example.com", hubFlagEndpoint())

			target, isGlobal := projectDir, false
			if scope == "global" {
				target, isGlobal = globalDir, true
			}
			require.NoError(t, persistHubFlagEndpoint(target, isGlobal))
			assert.Equal(t, "https://flag.example.com", settingsEndpoint(t, target))
		})
	}
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

// TestHubEnable_EndpointSource: 'scion hub enable' outside any project
// enables hub mode in the global settings (its own write) but writes the
// endpoint only when it came from --hub, never from the current context.
func TestHubEnable_EndpointSource(t *testing.T) {
	for _, tc := range []struct {
		name string
		flag bool
	}{{name: "context hub is not written"}, {name: "--hub is written", flag: true}} {
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
			_ = captureStdout(t, func() { require.NoError(t, runHubEnable(enable, nil)) })

			got := settingsEndpoint(t, filepath.Join(os.Getenv("HOME"), ".scion"))
			if tc.flag {
				assert.Equal(t, srv.URL, got)
			} else {
				assert.Empty(t, got, "the context hub must not be written to the global hub.endpoint")
			}
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

// TestCheckHubAvailability_ContextHubDoesNotRelink: in a directory project
// already linked to a hub (hub.projectId) and without an endpoint of its
// own, the hub selected with 'scion use' does not link or register the
// project: the command gets the existing not-linked error, the hub gets no
// write request, and the project's settings are unchanged.
func TestCheckHubAvailability_ContextHubDoesNotRelink(t *testing.T) {
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

	_, err := CheckHubAvailability(projectPath)
	require.Error(t, err)
	assert.Equal(t, "project must be linked to Hub to perform this operation\n\n"+
		"Link this project: scion hub link\n"+
		"Or use local-only mode: scion --no-hub <command>", err.Error())
	assert.Zero(t, writes.Load(), "no register or create request")
	assert.Equal(t, before, readFileT(t, settingsFile), "project settings unchanged")
}
