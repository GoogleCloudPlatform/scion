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

func TestPersistHubFlagEndpoint(t *testing.T) {
	t.Run("a context hub is not written", func(t *testing.T) {
		setupContextTest(t)
		writeGlobalSettings(t, "image_registry: registry.example.test/team\n")
		saveTestContext(t, ctxTestHub, ctxTestProjectID)
		resolveInvocationTarget(findCommand(t, "hub", "link"), ModeHuman)
		require.Equal(t, targetSourceContext, hubSource)
		require.Empty(t, hubFlagEndpoint())

		globalDir := filepath.Join(os.Getenv("HOME"), ".scion")
		before := readFileT(t, filepath.Join(globalDir, "settings.yaml"))
		require.NoError(t, persistHubFlagEndpoint(globalDir, true))
		assert.Equal(t, before, readFileT(t, filepath.Join(globalDir, "settings.yaml")))
	})

	t.Run("the --hub flag is written", func(t *testing.T) {
		setupContextTest(t)
		writeGlobalSettings(t, "image_registry: registry.example.test/team\n")
		saveTestContext(t, ctxTestHub, ctxTestProjectID)
		hubEndpoint = "https://flag.example.com"
		resolveInvocationTarget(findCommand(t, "hub", "link"), ModeHuman)
		require.Equal(t, "https://flag.example.com", hubFlagEndpoint())

		globalDir := filepath.Join(os.Getenv("HOME"), ".scion")
		require.NoError(t, persistHubFlagEndpoint(globalDir, true))
		s, err := config.LoadSettingsFromDir(globalDir)
		require.NoError(t, err)
		require.NotNil(t, s.Hub)
		assert.Equal(t, "https://flag.example.com", s.Hub.Endpoint)
	})
}

// TestHubEnable_ContextHubNotPersisted: 'scion hub enable' outside any
// project, on the hub selected with 'scion use', enables hub mode but does
// not write that hub into the global hub.endpoint.
func TestHubEnable_ContextHubNotPersisted(t *testing.T) {
	setupContextTest(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/healthz" {
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	writeGlobalSettings(t, "image_registry: registry.example.test/team\n")
	saveTestContext(t, srv.URL, ctxTestProjectID)

	enable := findCommand(t, "hub", "enable")
	resolveInvocationTarget(enable, ModeHuman)
	require.Equal(t, srv.URL, hubEndpoint)
	_ = captureStdout(t, func() { require.NoError(t, runHubEnable(enable, nil)) })

	s, err := config.LoadSettingsFromDir(filepath.Join(os.Getenv("HOME"), ".scion"))
	require.NoError(t, err)
	if s.Hub != nil {
		assert.Empty(t, s.Hub.Endpoint, "the context hub must not be written to the global hub.endpoint")
	}
}
