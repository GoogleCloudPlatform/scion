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

//go:build unix

package runtimebroker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

func setupGetLogsTestServer(t *testing.T, fallbackLogs string) (*Server, string, *bool) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())

	base := t.TempDir()
	dotScion := filepath.Join(base, "project", config.DotScion)
	agentHome := config.GetAgentHomePath(dotScion, "log-agent")
	if err := os.MkdirAll(agentHome, 0o755); err != nil {
		t.Fatal(err)
	}

	getLogsCalled := false
	rt := &runtime.MockRuntime{
		NameFunc: func() string { return "mock" },
		GetLogsFunc: func(_ context.Context, _ string) (string, error) {
			getLogsCalled = true
			return fallbackLogs, nil
		},
	}

	mgr := &mockManager{
		agents: []api.AgentInfo{
			{
				ID:          "container-log-1",
				ContainerID: "container-log-1",
				Name:        "log-agent",
				Slug:        "log-agent",
				ProjectPath: dotScion,
				Phase:       "running",
			},
		},
	}

	cfg := testServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	cfg.ForceRuntime = "mock"
	srv := New(cfg, mgr, rt, config.RuntimeBuildRecord{})
	return srv, agentHome, &getLogsCalled
}

func TestGetLogs_AgentLogConfinement(t *testing.T) {
	t.Run("regular file succeeds", func(t *testing.T) {
		srv, agentHome, fallbackCalled := setupGetLogsTestServer(t, "container fallback")
		want := "regular agent log output\n"
		if err := os.WriteFile(filepath.Join(agentHome, "agent.log"), []byte(want), 0o644); err != nil {
			t.Fatal(err)
		}

		req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/log-agent/logs", nil)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
		}
		if w.Body.String() != want {
			t.Fatalf("body = %q, want %q", w.Body.String(), want)
		}
		if *fallbackCalled {
			t.Fatal("runtime.GetLogs should not be called when agent.log is readable")
		}
	})

	t.Run("missing agent.log falls through to container logs", func(t *testing.T) {
		srv, _, fallbackCalled := setupGetLogsTestServer(t, "container fallback logs")

		req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/log-agent/logs", nil)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
		}
		if w.Body.String() != "container fallback logs" {
			t.Fatalf("body = %q, want %q", w.Body.String(), "container fallback logs")
		}
		if !*fallbackCalled {
			t.Fatal("expected runtime.GetLogs fallback when agent.log is absent")
		}
	})

	t.Run("symlink refused with explicit error", func(t *testing.T) {
		srv, agentHome, fallbackCalled := setupGetLogsTestServer(t, "container fallback")
		outsideFile := filepath.Join(t.TempDir(), "outside.txt")
		outsideContent := "OUTSIDE_FILE_MARKER_CONTENT"
		if err := os.WriteFile(outsideFile, []byte(outsideContent), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outsideFile, filepath.Join(agentHome, "agent.log")); err != nil {
			t.Fatal(err)
		}

		req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/log-agent/logs", nil)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)

		assertAgentLogRefused(t, w, *fallbackCalled, outsideContent, agentHome)
	})

	t.Run("dangling symlink refused with explicit error", func(t *testing.T) {
		srv, agentHome, fallbackCalled := setupGetLogsTestServer(t, "container fallback")
		missingTarget := filepath.Join(t.TempDir(), "does-not-exist.txt")
		if err := os.Symlink(missingTarget, filepath.Join(agentHome, "agent.log")); err != nil {
			t.Fatal(err)
		}

		req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/log-agent/logs", nil)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)

		assertAgentLogRefused(t, w, *fallbackCalled, "container fallback", agentHome)
	})

	t.Run("hardlink refused with explicit error", func(t *testing.T) {
		srv, agentHome, fallbackCalled := setupGetLogsTestServer(t, "container fallback")
		outsideFile := filepath.Join(filepath.Dir(agentHome), "outside-hardlink.txt")
		outsideContent := "OUTSIDE_HARDLINK_MARKER_CONTENT"
		if err := os.WriteFile(outsideFile, []byte(outsideContent), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(outsideFile, filepath.Join(agentHome, "agent.log")); err != nil {
			t.Fatal(err)
		}

		req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/log-agent/logs", nil)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)

		assertAgentLogRefused(t, w, *fallbackCalled, outsideContent, agentHome)
	})

	t.Run("fifo refused without blocking", func(t *testing.T) {
		srv, agentHome, fallbackCalled := setupGetLogsTestServer(t, "container fallback")
		if err := syscall.Mkfifo(filepath.Join(agentHome, "agent.log"), 0o644); err != nil {
			t.Fatal(err)
		}

		req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/log-agent/logs", nil)
		w := httptest.NewRecorder()
		done := make(chan struct{})
		go func() {
			srv.Handler().ServeHTTP(w, req)
			close(done)
		}()

		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("FIFO at agent.log blocked getLogs")
		}

		assertAgentLogRefused(t, w, *fallbackCalled, "container fallback", agentHome)
	})

	t.Run("directory refused with explicit error", func(t *testing.T) {
		srv, agentHome, fallbackCalled := setupGetLogsTestServer(t, "container fallback")
		if err := os.Mkdir(filepath.Join(agentHome, "agent.log"), 0o755); err != nil {
			t.Fatal(err)
		}

		req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/log-agent/logs", nil)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)

		assertAgentLogRefused(t, w, *fallbackCalled, "container fallback", agentHome)
	})

	t.Run("oversized regular file refused with explicit error", func(t *testing.T) {
		srv, agentHome, fallbackCalled := setupGetLogsTestServer(t, "container fallback")
		logPath := filepath.Join(agentHome, "agent.log")
		f, err := os.Create(logPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Truncate(maxAgentLogBytes + 1); err != nil {
			_ = f.Close()
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}

		req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/log-agent/logs", nil)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)

		assertAgentLogRefused(t, w, *fallbackCalled, "container fallback", agentHome)
		if !strings.Contains(strings.ToLower(w.Body.String()), "size") {
			t.Errorf("expected size-related error message, got %s", w.Body.String())
		}
	})
}

func assertAgentLogRefused(t *testing.T, w *httptest.ResponseRecorder, fallbackCalled bool, forbiddenContent, hostPath string) {
	t.Helper()
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusUnprocessableEntity, w.Body.String())
	}
	if fallbackCalled {
		t.Fatal("runtime.GetLogs should not be called when agent.log is refused")
	}
	body := w.Body.String()
	if forbiddenContent != "" && strings.Contains(body, forbiddenContent) {
		t.Fatalf("response body leaked forbidden content %q", forbiddenContent)
	}
	if hostPath != "" && strings.Contains(body, hostPath) {
		t.Fatalf("response body leaked host path %q: %s", hostPath, body)
	}
	var errResp ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("response body is not valid ErrorResponse JSON: %v (body=%s)", err, body)
	}
	if errResp.Error.Code == "" || errResp.Error.Message == "" {
		t.Fatalf("expected non-empty error code and message, got %+v", errResp.Error)
	}
}
