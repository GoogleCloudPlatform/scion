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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// Made-up values; never real credentials.
const (
	naHubCredential = "fake.na.hub.credential"
	naSecretEnv     = "na-canary-secret-env"
	naSecretFile    = "na-canary-secret-file"
	naGitToken      = "na-git-token"
	naAppToken      = "na-app-token"
	naPlain         = "na-plain"
)

// naStartBody is a start, resume or restart body as the hub sends it, with
// a secret-store env value, a file secret, the git token, a GitHub App
// token, a plain setting and the hub credential. classified sends the env
// classifications; noAuth sends the no-credential intent.
func naStartBody(t *testing.T, noAuth, classified, resume bool) string {
	t.Helper()
	body := map[string]any{
		"resolvedEnv": map[string]string{
			"SCION_AUTH_TOKEN": naHubCredential,
			"SCION_AGENT_ID":   "hub-agent-id",
			"NA_SECRET_ENV":    naSecretEnv,
			"GITHUB_TOKEN":     naGitToken,
			"NA_APP_TOKEN":     naAppToken,
			"NA_PLAIN":         naPlain,
		},
		"resolvedSecrets": []api.ResolvedSecret{
			{Name: "NA_SECRET_FILE", Type: "file", Target: "/home/scion/na-secret-file", Value: naSecretFile, Source: "project"},
		},
	}
	if classified {
		body["envClassifications"] = map[string]api.EnvKind{
			"SCION_AUTH_TOKEN": api.EnvKindSecretBootstrap,
			"SCION_AGENT_ID":   api.EnvKindPlain,
			"NA_SECRET_ENV":    api.EnvKindSecretFetchable,
			"GITHUB_TOKEN":     api.EnvKindSecretFetchable,
			"NA_APP_TOKEN":     api.EnvKindSecretInjected,
			"NA_PLAIN":         api.EnvKindPlain,
		}
	}
	if noAuth {
		body["noAuth"] = true
	}
	if resume {
		body["resume"] = true
	}
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

var naPaths = []struct {
	name, path string
	resume     bool
}{
	{"start", "/api/v1/agents/test-agent-2/start", false},
	{"resume", "/api/v1/agents/test-agent-2/start", true},
	{"restart", "/api/v1/agents/test-agent-1/restart", false},
}

// naPost posts body to path and returns what the agent manager received.
func naPost(t *testing.T, path, body string) api.StartOptions {
	t.Helper()
	srv := newTestServer(t)
	mgr := srv.manager.(*mockManager)
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if mgr.StartCalls() != 1 {
		t.Fatalf("Manager.Start calls = %d, want 1 (status %d: %s)", mgr.StartCalls(), w.Code, w.Body.String())
	}
	return mgr.LastStartOpts()
}

// TestStartAgentHandler_CarriesNoAuth (B1, B2) and
// TestBuildStartContext_NoAuthDropsSecretEnv (B3): a start, resume or
// restart body with the intent reaches the agent manager as
// StartOptions.NoAuth, without the resolved secrets and without the env
// values the hub classified secret-fetchable, except the git token. Plain
// settings, the GitHub App token and the hub credential are unchanged.
func TestStartAgentHandler_CarriesNoAuth(t *testing.T) {
	for _, tc := range naPaths {
		t.Run(tc.name, func(t *testing.T) {
			opts := naPost(t, tc.path, naStartBody(t, true, true, tc.resume))
			if !opts.NoAuth {
				t.Error("StartOptions.NoAuth not set from the start body")
			}
			if len(opts.ResolvedSecrets) != 0 {
				t.Errorf("resolved secrets reached the agent manager: %d", len(opts.ResolvedSecrets))
			}
			if _, ok := opts.Env["NA_SECRET_ENV"]; ok {
				t.Error("a secret-fetchable env value reached the agent env")
			}
			for k, v := range opts.Env {
				if v == naSecretEnv || v == naSecretFile {
					t.Errorf("a secret value reached the agent env as %s", k)
				}
			}
			if opts.Env["GITHUB_TOKEN"] != naGitToken {
				t.Error("the git token is kept")
			}
			if opts.Env["NA_APP_TOKEN"] != naAppToken {
				t.Error("a secret-injected value is kept")
			}
			if opts.Env["NA_PLAIN"] != naPlain {
				t.Error("a plain setting is kept")
			}
			if opts.HubIssuedToken != naHubCredential {
				t.Error("the classified hub credential is still the agent's hub credential")
			}
			if opts.Resume != tc.resume {
				t.Errorf("Resume = %v, want %v", opts.Resume, tc.resume)
			}
		})
	}
}

// TestBuildStartContext_WithAuthUnchanged (B5): without the intent the
// resolved secrets and every env value pass through as before.
func TestBuildStartContext_WithAuthUnchanged(t *testing.T) {
	for _, tc := range naPaths {
		t.Run(tc.name, func(t *testing.T) {
			opts := naPost(t, tc.path, naStartBody(t, false, true, tc.resume))
			if opts.NoAuth {
				t.Error("StartOptions.NoAuth set without the intent")
			}
			if len(opts.ResolvedSecrets) != 1 || opts.ResolvedSecrets[0].Value != naSecretFile {
				t.Errorf("resolved secrets = %d, want the one sent", len(opts.ResolvedSecrets))
			}
			if opts.Env["NA_SECRET_ENV"] != naSecretEnv {
				t.Error("a secret-fetchable env value passes through for an agent with credentials")
			}
		})
	}
}

// TestBuildStartContext_NoAuthNilClassifications (B4): without
// classifications (an older hub) the env is copied as sent; the resolved
// secrets are still left out.
func TestBuildStartContext_NoAuthNilClassifications(t *testing.T) {
	opts := naPost(t, "/api/v1/agents/test-agent-2/start", naStartBody(t, true, false, false))
	if !opts.NoAuth {
		t.Error("StartOptions.NoAuth not set")
	}
	if len(opts.ResolvedSecrets) != 0 {
		t.Errorf("resolved secrets reached the agent manager: %d", len(opts.ResolvedSecrets))
	}
	if opts.Env["NA_SECRET_ENV"] != naSecretEnv {
		t.Error("without classifications the env is copied as sent")
	}
}

// TestBuildStartContext_NoAuthSkipsADC (B6): in workstation mode with the
// gcloud login file auto-inject turned on, an agent without credentials
// does not get the file; an agent with credentials does.
func TestBuildStartContext_NoAuthSkipsADC(t *testing.T) {
	clearSCIONEnv(t)
	// The condition does not depend on the operation; create is the one
	// buildStartContext runs here without an existing agent.
	for _, op := range []startOperation{opCreate} {
		for _, noAuth := range []bool{true, false} {
			name := string(op) + "/with credentials"
			if noAuth {
				name = string(op) + "/without credentials"
			}
			t.Run(name, func(t *testing.T) {
				cfg := DefaultServerConfig()
				cfg.StateDir = t.TempDir()
				srv := newTestServerForStartContext(t, cfg)
				srv.hubMu.Lock()
				srv.hubConnections["hub-1"] = &HubConnection{Name: "hub-1", IsColocated: true, Hydrator: newTestHydrator(t)}
				srv.hubMu.Unlock()

				home, err := os.UserHomeDir()
				if err != nil {
					t.Fatal(err)
				}
				naEnableADCAutoInject(t, home)

				r := httptest.NewRequest(http.MethodPost, "/api/v1/agents", nil)
				r.Header.Set("X-Scion-Hub-Connection", "hub-1")
				in := startContextInputs{
					Name:        "agent-na-adc",
					HubEndpoint: "http://localhost:8080",
					HTTPRequest: r,
					Operation:   op,
					NoAuth:      noAuth,
				}
				if op == opCreate {
					in.Config = &CreateAgentConfig{}
				}
				sc, err := srv.buildStartContext(context.Background(), in)
				if err != nil {
					t.Fatalf("buildStartContext: %v", err)
				}
				var got bool
				for _, s := range sc.Opts.ResolvedSecrets {
					if s.Name == "gcloud-adc" {
						got = true
					}
				}
				if got == noAuth {
					t.Errorf("gcloud-adc injected = %v for NoAuth = %v", got, noAuth)
				}
			})
		}
	}
}

// naEnableADCAutoInject turns on the gcloud login file auto-inject in the
// global settings under home and writes a made-up login file.
func naEnableADCAutoInject(t *testing.T, home string) {
	t.Helper()
	adcDir := filepath.Join(home, ".config", "gcloud")
	if err := os.MkdirAll(adcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(adcDir, "application_default_credentials.json"), []byte(`{"type":"fake"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	globalDir := filepath.Join(home, ".scion")
	if err := os.MkdirAll(globalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	settingsPath := filepath.Join(globalDir, "settings.yaml")
	existing, err := os.ReadFile(settingsPath)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	content := string(existing)
	if content == "" {
		content = "schema_version: \"1\"\n"
	} else if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	content += "auto_inject_gcloud_adc: true\n"
	if err := os.WriteFile(settingsPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestRestartAgent_NoAuthReprovision (B2): a restart of an agent with no
// container, which the broker starts again from the request's inputs (as
// after its pod or directory is gone), carries the intent and leaves out
// secret material the same way.
func TestRestartAgent_NoAuthReprovision(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv := newTestServer(t)
	mgr := srv.manager.(*mockManager)

	projectDir := filepath.Join(t.TempDir(), ".scion")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := map[string]any{
		"resolvedEnv": map[string]string{
			"SCION_AGENT_ID": "agent-uuid-na-reprovision",
			"NA_SECRET_ENV":  naSecretEnv,
			"GITHUB_TOKEN":   naGitToken,
		},
		"envClassifications": map[string]api.EnvKind{
			"SCION_AGENT_ID": api.EnvKindPlain,
			"NA_SECRET_ENV":  api.EnvKindSecretFetchable,
			"GITHUB_TOKEN":   api.EnvKindSecretFetchable,
		},
		"resolvedSecrets": []api.ResolvedSecret{
			{Name: "NA_SECRET_FILE", Type: "file", Target: "/home/scion/na-secret-file", Value: naSecretFile, Source: "project"},
		},
		"projectPath":   projectDir,
		"harnessConfig": "claude",
		"noAuth":        true,
	}
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/gone-na-agent/restart", strings.NewReader(string(data)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if mgr.StartCalls() != 1 {
		t.Fatalf("Manager.Start calls = %d, want 1", mgr.StartCalls())
	}
	opts := mgr.LastStartOpts()
	if opts.ProjectPath != projectDir {
		t.Fatalf("precondition: the restart started the agent from the request's project (got %q)", opts.ProjectPath)
	}
	if !opts.NoAuth {
		t.Error("the restart without a container carries the intent")
	}
	if len(opts.ResolvedSecrets) != 0 {
		t.Errorf("resolved secrets reached the agent manager: %d", len(opts.ResolvedSecrets))
	}
	if _, ok := opts.Env["NA_SECRET_ENV"]; ok {
		t.Error("a secret-fetchable env value reached the agent env")
	}
	if opts.Env["GITHUB_TOKEN"] != naGitToken {
		t.Error("the git token is kept")
	}
}
