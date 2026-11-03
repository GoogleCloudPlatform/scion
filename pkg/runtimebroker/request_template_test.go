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

package runtimebroker

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// templateValueCountingManager counts Start and Provision calls.
type templateValueCountingManager struct {
	mockManager
	mu         sync.Mutex
	starts     int
	provisions int
}

func (m *templateValueCountingManager) Start(ctx context.Context, opts api.StartOptions) (*api.AgentInfo, error) {
	m.mu.Lock()
	m.starts++
	m.mu.Unlock()
	return m.mockManager.Start(ctx, opts)
}

func (m *templateValueCountingManager) Provision(ctx context.Context, opts api.StartOptions) (*api.ScionConfig, error) {
	m.mu.Lock()
	m.provisions++
	m.mu.Unlock()
	return m.mockManager.Provision(ctx, opts)
}

func (m *templateValueCountingManager) calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.starts + m.provisions
}

type requestTemplateFixture struct {
	srv        *Server
	mgr        *templateValueCountingManager
	projectDir string
	outsideDir string
}

func newRequestTemplateFixture(t *testing.T) requestTemplateFixture {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	cfg := testServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	cfg.StateDir = t.TempDir()
	cfg.ForceRuntime = "mock"
	projectDir := t.TempDir()
	settingsYAML := "schema_version: \"1\"\nactive_profile: local\nprofiles:\n    local:\n        runtime: mock\nruntimes:\n    mock:\n        type: mock\n"
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "settings.yaml"), []byte(settingsYAML), 0o644))
	outside := filepath.Join(t.TempDir(), "host-dir")
	require.NoError(t, os.MkdirAll(filepath.Join(outside, "home"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(outside, "scion-agent.yaml"), []byte("harness_config: claude\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(outside, "home", "secret.txt"), []byte("host file"), 0o644))
	mgr := &templateValueCountingManager{}
	srv := New(cfg, mgr, &runtime.MockRuntime{NameFunc: func() string { return "mock" }}, config.RuntimeBuildRecord{})
	return requestTemplateFixture{srv: srv, mgr: mgr, projectDir: projectDir, outsideDir: outside}
}

// refusedValues are template values a request must never carry.
func (fx requestTemplateFixture) refusedValues() map[string]string {
	return map[string]string{
		"absolute host directory": fx.outsideDir,
		"absolute parent":         filepath.Dir(fx.outsideDir),
		"escaping relative path":  "../../" + filepath.Base(fx.outsideDir),
		"nested relative path":    "team/reviewer",
		"rclone local backend":    ":local:" + fx.outsideDir,
		"rclone gcs backend":      ":gcs:bucket/x",
		"http archive":            "http://169.254.169.254/latest/x.tar.gz",
		"https archive":           "https://127.0.0.1/x.tgz",
	}
}

func (fx requestTemplateFixture) createBody(t *testing.T, name, template string) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"name":        name,
		"id":          "agent-uuid-" + name,
		"projectPath": fx.projectDir,
		"config":      map[string]any{"template": template},
	})
	require.NoError(t, err)
	return body
}

func assertTemplateRefused(t *testing.T, code int, body, value string) {
	t.Helper()
	assert.Equal(t, http.StatusBadRequest, code, body)
	assert.Contains(t, body, requestTemplateRefusal)
	assert.NotContains(t, body, value, "the refusal must not echo the value")
	assert.NotContains(t, body, "host file")
}

// A create whose template is a host directory, a path or a remote source is
// refused before anything is provisioned; a template name is accepted.
func TestCreateAgentRefusesNonSlugTemplate(t *testing.T) {
	fx := newRequestTemplateFixture(t)
	for name, value := range fx.refusedValues() {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(string(fx.createBody(t, "refused", value))))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			fx.srv.Handler().ServeHTTP(w, req)
			assertTemplateRefused(t, w.Code, w.Body.String(), value)
		})
	}
	assert.Equal(t, 0, fx.mgr.calls(), "a refused template must not start or provision anything")
	_, err := os.Stat(filepath.Join(fx.projectDir, "agents", "refused"))
	assert.True(t, os.IsNotExist(err), "no agent directory is created for a refused template")

	// Control: a template name passes the check (whatever happens after it).
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(string(fx.createBody(t, "accepted", "claude"))))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	fx.srv.Handler().ServeHTTP(w, req)
	assert.NotContains(t, w.Body.String(), requestTemplateRefusal, w.Body.String())
}

// The combined hub-broker server and remote hubs reach the broker's handlers
// through the control-channel tunnel; the refusal applies there too.
func TestCreateAgentTunneledRefusesNonSlugTemplate(t *testing.T) {
	fx := newRequestTemplateFixture(t)
	for name, value := range fx.refusedValues() {
		t.Run(name, func(t *testing.T) {
			var resp *wsprotocol.ResponseEnvelope
			serveTunneledRequest(context.Background(), fx.srv.Handler(), "", "test.tunnel", slog.Default(), wsprotocol.RequestEnvelope{
				Type:      "request",
				RequestID: "req-" + strings.ReplaceAll(name, " ", "-"),
				Method:    http.MethodPost,
				Path:      "/api/v1/agents",
				Headers:   map[string]string{"Content-Type": "application/json"},
				Body:      fx.createBody(t, "tunneled", value),
			}, func(r *wsprotocol.ResponseEnvelope) error {
				resp = r
				return nil
			})
			require.NotNil(t, resp)
			assertTemplateRefused(t, resp.StatusCode, string(resp.Body), value)
		})
	}
	assert.Equal(t, 0, fx.mgr.calls())
}

// Start and restart carry the agent's template name; it must be a template
// name too.
func TestStartAndRestartRefuseNonSlugTemplate(t *testing.T) {
	fx := newRequestTemplateFixture(t)
	for _, op := range []string{"start", "restart"} {
		for name, value := range fx.refusedValues() {
			t.Run(op+"/"+name, func(t *testing.T) {
				body, err := json.Marshal(map[string]any{"projectPath": fx.projectDir, "templateName": value})
				require.NoError(t, err)
				req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/some-agent/"+op, strings.NewReader(string(body)))
				req.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				fx.srv.Handler().ServeHTTP(w, req)
				assertTemplateRefused(t, w.Code, w.Body.String(), value)
			})
		}
	}
	assert.Equal(t, 0, fx.mgr.calls())
}
