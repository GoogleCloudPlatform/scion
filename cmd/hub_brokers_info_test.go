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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

type fakeBrokersInfoHub struct {
	brokers        []hubclient.RuntimeBroker
	projectsByID   map[string][]hubclient.BrokerProjectInfo
	projectsStatus int

	mu       sync.Mutex
	requests []string
}

func (f *fakeBrokersInfoHub) record(r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	req := r.Method + " " + r.URL.Path
	if r.URL.RawQuery != "" {
		req += "?" + r.URL.RawQuery
	}
	f.requests = append(f.requests, req)
}

func (f *fakeBrokersInfoHub) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

func (f *fakeBrokersInfoHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.record(r)
	w.Header().Set("Content-Type", "application/json")

	if r.Method == http.MethodGet && r.URL.Path == "/api/v1/runtime-brokers" {
		name := r.URL.Query().Get("name")
		matches := make([]hubclient.RuntimeBroker, 0, len(f.brokers))
		for _, b := range f.brokers {
			if name == "" || strings.EqualFold(b.Name, name) {
				// GET /api/v1/runtime-brokers never populates Projects; mimic the real Hub.
				copyBroker := b
				copyBroker.Projects = nil
				matches = append(matches, copyBroker)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"brokers": matches})
		return
	}

	if rest, ok := strings.CutPrefix(r.URL.Path, "/api/v1/runtime-brokers/"); ok && r.Method == http.MethodGet {
		if brokerID, isProjects := strings.CutSuffix(rest, "/projects"); isProjects && brokerID != "" {
			if f.projectsStatus != 0 && f.projectsStatus != http.StatusOK {
				w.WriteHeader(f.projectsStatus)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error": map[string]any{
						"code":    "internal_error",
						"message": "project provider query failed",
					},
				})
				return
			}
			for _, b := range f.brokers {
				if b.ID == brokerID {
					projects := f.projectsByID[brokerID]
					if projects == nil {
						projects = []hubclient.BrokerProjectInfo{}
					}
					_ = json.NewEncoder(w).Encode(hubclient.ListBrokerProjectsResponse{
						Projects: projects,
					})
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"RuntimeBroker not found"}}`))
			return
		}

		for _, b := range f.brokers {
			if b.ID == rest {
				// GET /api/v1/runtime-brokers/{id} never populates Projects; mimic the real Hub.
				copyBroker := b
				copyBroker.Projects = nil
				_ = json.NewEncoder(w).Encode(copyBroker)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"RuntimeBroker not found"}}`))
		return
	}

	http.NotFound(w, r)
}

func setupBrokersInfoTest(t *testing.T, hub *fakeBrokersInfoHub) {
	t.Helper()
	hermeticCLIEnv(t)
	srv := httptest.NewServer(hub)
	t.Cleanup(srv.Close)

	workDir := t.TempDir()
	t.Chdir(workDir)
	t.Setenv("SCION_HUB_ENDPOINT", srv.URL)
	t.Setenv("SCION_DEV_TOKEN", "scion_dev_test")

	origProjectPath := projectPath
	origOutputFormat := outputFormat
	origHubJSON := hubOutputJSON
	origGlobal := globalMode
	t.Cleanup(func() {
		projectPath = origProjectPath
		outputFormat = origOutputFormat
		hubOutputJSON = origHubJSON
		globalMode = origGlobal
	})
	projectPath = ""
	outputFormat = ""
	hubOutputJSON = false
	globalMode = true
}

func TestRunHubBrokersInfo_Projects(t *testing.T) {
	const (
		brokerIDA = "11111111-1111-4111-8111-111111111111"
		brokerIDB = "22222222-2222-4222-8222-222222222222"
	)
	created := time.Date(2026, time.November, 1, 0, 0, 0, 0, time.UTC)

	baseBrokers := []hubclient.RuntimeBroker{
		{
			ID:      brokerIDA,
			Name:    "alpha-broker",
			Slug:    "alpha-broker",
			Version: "0.9.0",
			Status:  "online",
			Created: created,
			Updated: created,
		},
		{
			ID:      brokerIDB,
			Name:    "empty-broker",
			Slug:    "empty-broker",
			Version: "0.9.0",
			Status:  "online",
			Created: created,
			Updated: created,
		},
	}

	projects := map[string][]hubclient.BrokerProjectInfo{
		brokerIDA: {
			{
				ProjectID:   "proj-1",
				ProjectName: "payments",
				GitRemote:   "https://github.com/example/payments.git",
				AgentCount:  3,
			},
			{
				ProjectID:   "proj-2",
				ProjectName: "billing",
				AgentCount:  0,
			},
			{
				ProjectID:  "proj-orphan-id",
				AgentCount: 1,
			},
		},
	}

	t.Run("text output lists provided projects when looked up by name", func(t *testing.T) {
		hub := &fakeBrokersInfoHub{
			brokers:      baseBrokers,
			projectsByID: projects,
		}
		setupBrokersInfoTest(t, hub)

		var runErr error
		out := captureStdout(t, func() {
			runErr = runHubBrokersInfo(hubBrokersInfoCmd, []string{"alpha-broker"})
		})
		require.NoError(t, runErr)
		assert.Contains(t, out, "Projects\n--------\n")
		assert.Contains(t, out, "  - payments (3 agents)\n")
		assert.Contains(t, out, "  - billing (0 agents)\n")
		assert.Contains(t, out, "  - proj-orphan-id (1 agents)\n")
		assert.NotContains(t, out, "Projects: none")
		assert.Equal(t, []string{
			"GET /api/v1/runtime-brokers/alpha-broker",
			"GET /api/v1/runtime-brokers?name=alpha-broker",
			"GET /api/v1/runtime-brokers/" + brokerIDA + "/projects",
		}, hub.recorded())
	})

	t.Run("json output includes provided projects when looked up by ID", func(t *testing.T) {
		hub := &fakeBrokersInfoHub{
			brokers:      baseBrokers,
			projectsByID: projects,
		}
		setupBrokersInfoTest(t, hub)
		hubOutputJSON = true

		var runErr error
		out := captureStdout(t, func() {
			runErr = runHubBrokersInfo(hubBrokersInfoCmd, []string{brokerIDA})
		})
		require.NoError(t, runErr)

		var got struct {
			ID       string                        `json:"id"`
			Name     string                        `json:"name"`
			Projects []hubclient.BrokerProjectInfo `json:"projects"`
		}
		require.NoError(t, json.Unmarshal([]byte(out), &got), "output: %s", out)
		assert.Equal(t, brokerIDA, got.ID)
		assert.Equal(t, "alpha-broker", got.Name)
		assert.Equal(t, projects[brokerIDA], got.Projects)
		assert.Equal(t, []string{
			"GET /api/v1/runtime-brokers/" + brokerIDA,
			"GET /api/v1/runtime-brokers/" + brokerIDA + "/projects",
		}, hub.recorded())
	})

	t.Run("broker with no provided projects prints none in text and omits projects in json", func(t *testing.T) {
		hub := &fakeBrokersInfoHub{
			brokers:      baseBrokers,
			projectsByID: projects,
		}
		setupBrokersInfoTest(t, hub)

		var runErr error
		textOut := captureStdout(t, func() {
			runErr = runHubBrokersInfo(hubBrokersInfoCmd, []string{brokerIDB})
		})
		require.NoError(t, runErr)
		assert.Contains(t, textOut, "Projects: none\n")

		hubOutputJSON = true
		jsonOut := captureStdout(t, func() {
			runErr = runHubBrokersInfo(hubBrokersInfoCmd, []string{brokerIDB})
		})
		require.NoError(t, runErr)
		var raw map[string]any
		require.NoError(t, json.Unmarshal([]byte(jsonOut), &raw))
		assert.NotContains(t, raw, "projects")
	})

	t.Run("projects endpoint error fails command instead of printing none", func(t *testing.T) {
		hub := &fakeBrokersInfoHub{
			brokers:        baseBrokers,
			projectsByID:   projects,
			projectsStatus: http.StatusInternalServerError,
		}
		setupBrokersInfoTest(t, hub)

		var runErr error
		out := captureStdout(t, func() {
			runErr = runHubBrokersInfo(hubBrokersInfoCmd, []string{brokerIDA})
		})
		require.Error(t, runErr)
		assert.Contains(t, runErr.Error(), "failed to list broker projects")
		assert.NotContains(t, out, "Projects: none")
	})

	t.Run("missing broker does not query projects endpoint", func(t *testing.T) {
		hub := &fakeBrokersInfoHub{
			brokers:      baseBrokers,
			projectsByID: projects,
		}
		setupBrokersInfoTest(t, hub)

		err := runHubBrokersInfo(hubBrokersInfoCmd, []string{"unknown-broker"})
		require.Error(t, err)
		assert.Equal(t, "broker 'unknown-broker' not found", err.Error())
		for _, req := range hub.recorded() {
			assert.NotContains(t, req, "/projects")
		}
	})
}
