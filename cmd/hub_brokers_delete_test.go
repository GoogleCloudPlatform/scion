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
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

const (
	brokersDeleteHelperEnv   = "SCION_TEST_BROKERS_DELETE_HELPER"
	brokersDeleteArgsEnv     = "SCION_TEST_BROKERS_DELETE_ARGS"
	brokersDeleteEndpointEnv = "SCION_TEST_BROKERS_DELETE_ENDPOINT"
)

type fakeBrokersDeleteHub struct {
	brokers        []hubclient.RuntimeBroker
	projectsByID   map[string][]hubclient.BrokerProjectInfo
	projectsStatus int

	mu       sync.Mutex
	requests []string
	deletes  []string
}

func (f *fakeBrokersDeleteHub) record(r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	req := r.Method + " " + r.URL.Path
	if r.URL.RawQuery != "" {
		req += "?" + r.URL.RawQuery
	}
	f.requests = append(f.requests, req)
}

func (f *fakeBrokersDeleteHub) recordDelete(brokerID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes = append(f.deletes, brokerID)
}

func (f *fakeBrokersDeleteHub) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

func (f *fakeBrokersDeleteHub) deleted() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.deletes...)
}

func (f *fakeBrokersDeleteHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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

	if rest, ok := strings.CutPrefix(r.URL.Path, "/api/v1/runtime-brokers/"); ok {
		if brokerID, isProjects := strings.CutSuffix(rest, "/projects"); isProjects && brokerID != "" && r.Method == http.MethodGet {
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

		if r.Method == http.MethodGet && !strings.Contains(rest, "/") {
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

		if r.Method == http.MethodDelete && !strings.Contains(rest, "/") {
			for _, b := range f.brokers {
				if b.ID == rest {
					f.recordDelete(rest)
					w.WriteHeader(http.StatusNoContent)
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"RuntimeBroker not found"}}`))
			return
		}
	}

	http.NotFound(w, r)
}

func setupBrokersDeleteTest(t *testing.T, hub *fakeBrokersDeleteHub, confirm bool) *httptest.Server {
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
	origConfirm := autoConfirm
	origNonInteractive := nonInteractive
	t.Cleanup(func() {
		projectPath = origProjectPath
		outputFormat = origOutputFormat
		hubOutputJSON = origHubJSON
		globalMode = origGlobal
		autoConfirm = origConfirm
		nonInteractive = origNonInteractive
	})
	projectPath = ""
	outputFormat = ""
	hubOutputJSON = false
	globalMode = true
	autoConfirm = confirm
	nonInteractive = false
	return srv
}

// TestBrokersDeletePromptHelperProcess runs Execute() in a child process so
// hubsync's promptOut (initialised from os.Stderr at package startup) writes
// to the child's stderr pipe. TestMain clears inherited SCION_* variables
// except SCION_TEST_*, so the endpoint is passed via SCION_TEST_* and set here.
func TestBrokersDeletePromptHelperProcess(t *testing.T) {
	if os.Getenv(brokersDeleteHelperEnv) != "1" {
		return
	}
	if err := os.Setenv("SCION_HUB_ENDPOINT", os.Getenv(brokersDeleteEndpointEnv)); err != nil {
		t.Fatal(err)
	}
	if err := os.Setenv("SCION_DEV_TOKEN", "scion_dev_test"); err != nil {
		t.Fatal(err)
	}
	os.Args = append([]string{"scion"}, strings.Fields(os.Getenv(brokersDeleteArgsEnv))...)
	Execute()
	os.Exit(0)
}

func runBrokersDeleteCLIProcess(t *testing.T, endpoint string, cliArgs ...string) (stdout, stderr string, runErr error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestBrokersDeletePromptHelperProcess$", "-test.count=1")
	child.Dir = t.TempDir()
	env := []string{
		brokersDeleteHelperEnv + "=1",
		brokersDeleteArgsEnv + "=" + strings.Join(cliArgs, " "),
		brokersDeleteEndpointEnv + "=" + endpoint,
		"HOME=" + t.TempDir(),
	}
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if name == "HOME" || strings.HasPrefix(name, "SCION_") {
			continue
		}
		env = append(env, kv)
	}
	child.Env = env

	var outBuf, errBuf bytes.Buffer
	child.Stdout = &outBuf
	child.Stderr = &errBuf
	child.WaitDelay = 5 * time.Second

	err := child.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("scion %s timed out\nstdout:\n%s\nstderr:\n%s", strings.Join(cliArgs, " "), outBuf.String(), errBuf.String())
	}
	return outBuf.String(), errBuf.String(), err
}

func TestRunHubBrokersDelete_Projects(t *testing.T) {
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

	t.Run("confirmation prompt lists provided projects and warns before declining without --yes", func(t *testing.T) {
		hub := &fakeBrokersDeleteHub{
			brokers:      baseBrokers,
			projectsByID: projects,
		}
		srv := httptest.NewServer(hub)
		t.Cleanup(srv.Close)

		stdout, stderr, err := runBrokersDeleteCLIProcess(t, srv.URL, "--global", "hub", "brokers", "delete", "alpha-broker")
		require.Error(t, err)
		assert.Empty(t, stdout)
		assert.Contains(t, stderr, "This will permanently delete broker 'alpha-broker' from the Hub.")
		assert.Contains(t, stderr, "This broker provides for 3 project(s):\n  - payments\n  - billing\n  - proj-orphan-id\n")
		assert.Contains(t, stderr, "The broker will be removed as a provider from all projects.")
		assert.Contains(t, stderr, "deletion cancelled")
		assert.Empty(t, hub.deleted())
	})

	t.Run("confirmed delete lists provided projects on stderr and reports removed count on stdout", func(t *testing.T) {
		hub := &fakeBrokersDeleteHub{
			brokers:      baseBrokers,
			projectsByID: projects,
		}
		srv := httptest.NewServer(hub)
		t.Cleanup(srv.Close)

		stdout, stderr, err := runBrokersDeleteCLIProcess(t, srv.URL, "--global", "hub", "brokers", "delete", "alpha-broker", "-y")
		require.NoError(t, err, "stderr:\n%s", stderr)
		assert.Contains(t, stderr, "This broker provides for 3 project(s):\n  - payments\n  - billing\n  - proj-orphan-id\n")
		assert.Contains(t, stderr, "The broker will be removed as a provider from all projects.")
		assert.Contains(t, stdout, "Broker 'alpha-broker' deleted successfully.\n")
		assert.Contains(t, stdout, "Removed from 3 project(s).\n")
		assert.Equal(t, []string{brokerIDA}, hub.deleted())
		assert.Equal(t, []string{
			"GET /api/v1/runtime-brokers/alpha-broker",
			"GET /api/v1/runtime-brokers?name=alpha-broker",
			"GET /api/v1/runtime-brokers/" + brokerIDA + "/projects",
			"DELETE /api/v1/runtime-brokers/" + brokerIDA,
		}, hub.recorded())
	})

	t.Run("json output reports projectsRemoved when looked up by ID", func(t *testing.T) {
		hub := &fakeBrokersDeleteHub{
			brokers:      baseBrokers,
			projectsByID: projects,
		}
		setupBrokersDeleteTest(t, hub, true)
		outputFormat = "json"

		var runErr error
		out := captureStdout(t, func() {
			runErr = runHubBrokersDelete(hubBrokersDeleteCmd, []string{brokerIDA})
		})
		require.NoError(t, runErr)

		var got ActionResult
		require.NoError(t, json.Unmarshal([]byte(out), &got), "output: %s", out)
		assert.Equal(t, "success", got.Status)
		assert.Equal(t, "hub brokers delete", got.Command)
		assert.Equal(t, brokerIDA, got.Details["brokerId"])
		assert.Equal(t, "alpha-broker", got.Details["brokerName"])
		assert.Equal(t, float64(3), got.Details["projectsRemoved"])
		assert.Equal(t, []string{brokerIDA}, hub.deleted())
		assert.Equal(t, []string{
			"GET /api/v1/runtime-brokers/" + brokerIDA,
			"GET /api/v1/runtime-brokers/" + brokerIDA + "/projects",
			"DELETE /api/v1/runtime-brokers/" + brokerIDA,
		}, hub.recorded())
	})

	t.Run("broker with no provided projects omits project warning and removed line", func(t *testing.T) {
		hub := &fakeBrokersDeleteHub{
			brokers:      baseBrokers,
			projectsByID: projects,
		}
		srv := httptest.NewServer(hub)
		t.Cleanup(srv.Close)

		stdout, stderr, err := runBrokersDeleteCLIProcess(t, srv.URL, "--global", "hub", "brokers", "delete", brokerIDB, "-y")
		require.NoError(t, err, "stderr:\n%s", stderr)
		assert.Contains(t, stderr, "This will permanently delete broker 'empty-broker' from the Hub.")
		assert.NotContains(t, stderr, "This broker provides for")
		assert.NotContains(t, stderr, "removed as a provider")
		assert.Contains(t, stdout, "Broker 'empty-broker' deleted successfully.\n")
		assert.NotContains(t, stdout, "Removed from")
		assert.Equal(t, []string{brokerIDB}, hub.deleted())
	})

	t.Run("projects endpoint error aborts before prompting or deleting", func(t *testing.T) {
		hub := &fakeBrokersDeleteHub{
			brokers:        baseBrokers,
			projectsByID:   projects,
			projectsStatus: http.StatusInternalServerError,
		}
		srv := httptest.NewServer(hub)
		t.Cleanup(srv.Close)

		stdout, stderr, err := runBrokersDeleteCLIProcess(t, srv.URL, "--global", "hub", "brokers", "delete", brokerIDA, "-y")
		require.Error(t, err)
		assert.Empty(t, stdout)
		assert.Contains(t, stderr, "failed to list broker projects")
		assert.NotContains(t, stderr, "This will permanently delete")
		assert.NotContains(t, stderr, "Delete this broker?")
		assert.Empty(t, hub.deleted())
		assert.Equal(t, []string{
			"GET /api/v1/runtime-brokers/" + brokerIDA,
			"GET /api/v1/runtime-brokers/" + brokerIDA + "/projects",
		}, hub.recorded())
	})

	t.Run("missing broker does not query projects endpoint or delete", func(t *testing.T) {
		hub := &fakeBrokersDeleteHub{
			brokers:      baseBrokers,
			projectsByID: projects,
		}
		setupBrokersDeleteTest(t, hub, true)

		err := runHubBrokersDelete(hubBrokersDeleteCmd, []string{"unknown-broker"})
		require.Error(t, err)
		assert.Equal(t, "broker 'unknown-broker' not found", err.Error())
		assert.Empty(t, hub.deleted())
		for _, req := range hub.recorded() {
			assert.NotContains(t, req, "/projects")
			assert.False(t, strings.HasPrefix(req, "DELETE "))
		}
	})
}
