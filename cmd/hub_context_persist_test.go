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
			writeGlobalSettings(t, "image_registry: registry.example.test/team\n")
			saveTestContext(t, ctxTestHub, ctxTestProjectID)
			root := makeDirProject(t, "")
			globalDir := filepath.Join(os.Getenv("HOME"), ".scion")
			projectDir := filepath.Join(root, ".scion")
			resolveInvocationTarget(findCommand(t, "hub", "link"), ModeHuman)
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
			writeGlobalSettings(t, "image_registry: registry.example.test/team\n")
			saveTestContext(t, ctxTestHub, ctxTestProjectID)
			root := makeDirProject(t, "")
			globalDir := filepath.Join(os.Getenv("HOME"), ".scion")
			projectDir := filepath.Join(root, ".scion")
			hubEndpoint = "https://flag.example.com"
			resolveInvocationTarget(findCommand(t, "hub", "link"), ModeHuman)
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
			writeGlobalSettings(t, "image_registry: registry.example.test/team\n")
			saveTestContext(t, srv.URL, ctxTestProjectID)
			if tc.flag {
				hubEndpoint = srv.URL
			}

			enable := findCommand(t, "hub", "enable")
			resolveInvocationTarget(enable, ModeHuman)
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

// TestHubLink_EndpointSource: 'scion --global hub link' on the hub selected
// with 'scion use' links (writing link identity) but does not pin that hub
// as hub.endpoint; with --hub the endpoint is still saved.
func TestHubLink_EndpointSource(t *testing.T) {
	for _, tc := range []struct {
		name string
		flag bool
	}{{name: "context hub is not written"}, {name: "--hub is written", flag: true}} {
		t.Run(tc.name, func(t *testing.T) {
			setupContextTest(t)
			origYes, origOffer := autoConfirm, offerTemplateSyncOnLinkFn
			t.Cleanup(func() { autoConfirm, offerTemplateSyncOnLinkFn = origYes, origOffer })
			offerTemplateSyncOnLinkFn = func(_, _, _ string, _ bool) {}
			writeGlobalSettings(t, "schema_version: \"1\"\n")
			t.Chdir(os.Getenv("HOME"))
			globalMode, autoConfirm = true, true
			t.Setenv("SCION_DEV_TOKEN", "scion_dev_test")
			server, registered := fakeLinkHub(t)
			saveTestContext(t, server.URL, ctxTestProjectID)
			if tc.flag {
				hubEndpoint = server.URL
			}

			resolveInvocationTarget(hubLinkCmd, ModeHuman)
			require.Equal(t, server.URL, hubEndpoint)
			_ = captureStdout(t, func() { require.NoError(t, runHubLink(hubLinkCmd, nil)) })
			require.True(t, *registered, "the project was linked")

			got := settingsEndpoint(t, filepath.Join(os.Getenv("HOME"), ".scion"))
			if tc.flag {
				assert.Equal(t, server.URL, got)
			} else {
				assert.Empty(t, got, "the context hub must not be pinned as hub.endpoint")
			}
		})
	}
}

// TestHubShadow_EndpointSource: 'scion hub shadow' on the hub selected with
// 'scion use' writes the shadow's project ID but no hub.endpoint (the shadow
// directory then resolves to the current hub); with --hub the endpoint is
// still written.
func TestHubShadow_EndpointSource(t *testing.T) {
	for _, tc := range []struct {
		name string
		flag bool
	}{{name: "context hub is not written"}, {name: "--hub is written", flag: true}} {
		t.Run(tc.name, func(t *testing.T) {
			setupContextTest(t)
			srv, _ := useFakeHub(t)
			t.Setenv("SCION_DEV_TOKEN", "scion_dev_test")
			saveTestContext(t, srv.URL, ctxTestProjectID)
			if tc.flag {
				hubEndpoint = srv.URL
			}
			shadow := findCommand(t, "hub", "shadow")
			resolveInvocationTarget(shadow, ModeHuman)
			require.Equal(t, srv.URL, hubEndpoint)
			if !tc.flag {
				require.Equal(t, targetSourceContext, hubSource)
			}

			_ = captureStdout(t, func() { require.NoError(t, runHubShadow(shadow, []string{"foo"})) })

			wd, err := os.Getwd()
			require.NoError(t, err)
			marker, err := config.ReadProjectMarker(filepath.Join(wd, config.DotScion))
			require.NoError(t, err)
			settingsFile := filepath.Join(os.Getenv("HOME"), config.GlobalDir, config.ProjectConfigsDir, marker.DirName(), config.DotScion, "settings.yaml")
			written := readFileT(t, settingsFile)
			assert.Contains(t, written, ctxTestProjectID, "the shadow records its hub project")
			if tc.flag {
				assert.Contains(t, written, srv.URL)
			} else {
				assert.NotContains(t, written, srv.URL, "the context hub must not be written into the shadow's settings")
				assert.NotContains(t, written, "endpoint")
			}
		})
	}
}
