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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	scionrt "github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// resetAuthAgents returns a single-agent manager fixture used by the
// reset-auth handler tests.
func resetAuthAgents() *filteringMockManager {
	mgr := &filteringMockManager{}
	mgr.agents = []api.AgentInfo{
		{
			ContainerID: "container-A",
			Name:        "coordinator",
			Labels:      map[string]string{"scion.name": "coordinator", "scion.grove_id": "grove-A"},
		},
	}
	return mgr
}

func doResetAuth(t *testing.T, srv *Server, token string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(ResetAuthRequest{Token: token})
	r := httptest.NewRequest(http.MethodPost,
		"/api/v1/agents/coordinator/reset-auth?projectId=grove-A", bytes.NewReader(body))
	w := httptest.NewRecorder()
	srv.handleAgentByID(w, r)
	return w
}

// TestResetAuth_SignalFailureStillReturns200 verifies that when the SIGUSR2
// signal to PID 1 fails (e.g. EPERM in rootless containers), the handler still
// returns 200 OK because the token was successfully written — the agent's
// file poller will pick it up within seconds.
func TestResetAuth_SignalFailureStillReturns200(t *testing.T) {
	mgr := resetAuthAgents()

	var wroteToken bool
	rt := &scionrt.MockRuntime{
		NameFunc: func() string { return "docker" },
		ExecFunc: func(_ context.Context, _ string, cmd []string) (string, error) {
			if len(cmd) > 0 && cmd[0] == "kill" {
				return "", fmt.Errorf("kill: (1) - Operation not permitted")
			}
			wroteToken = true
			return "", nil
		},
	}
	srv := New(DefaultServerConfig(), mgr, rt)

	w := doResetAuth(t, srv, "fresh-token")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 even when the reset signal fails, got %d (%s)", w.Code, w.Body.String())
	}
	if !wroteToken {
		t.Error("token should still be written to disk even when the signal fails")
	}
	if !strings.Contains(w.Body.String(), "signal failed") {
		t.Errorf("response should mention signal failure, got %q", w.Body.String())
	}
}

// TestResetAuth_SignalSuccessReturns200 verifies the happy path: token written
// and PID 1 signaled successfully yields a 200.
func TestResetAuth_SignalSuccessReturns200(t *testing.T) {
	mgr := resetAuthAgents()

	var signaled bool
	rt := &scionrt.MockRuntime{
		NameFunc: func() string { return "docker" },
		ExecFunc: func(_ context.Context, _ string, cmd []string) (string, error) {
			if len(cmd) > 0 && cmd[0] == "kill" {
				signaled = true
			}
			return "", nil
		},
	}
	srv := New(DefaultServerConfig(), mgr, rt)

	w := doResetAuth(t, srv, "fresh-token")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 on success, got %d (%s)", w.Code, w.Body.String())
	}
	if !signaled {
		t.Error("expected PID 1 to be signaled via kill -USR2 1")
	}
}

// TestResetAuth_TokenDeliveredViaStdinNotArgv is the regression test for
// ptone/scion#1355: the token must reach the container over the exec's
// stdin, never as a substring of the exec's cmd slice. The cmd slice is what
// runtimes append to a host process's argv (e.g. `docker exec ... sh -c
// "<cmd>"`), which is readable via /proc/<pid>/cmdline for the lifetime of
// the exec — a heredoc embedded in cmd does not change that, since the
// heredoc body is still part of cmd's text.
func TestResetAuth_TokenDeliveredViaStdinNotArgv(t *testing.T) {
	mgr := resetAuthAgents()
	const token = "super-secret-reset-auth-token"

	var (
		writeCalled    bool
		stdinDelivered string
	)
	rt := &scionrt.MockRuntime{
		NameFunc: func() string { return "docker" },
		// Exec (argv-only) must never see the token. If the fix regresses to
		// embedding the token in cmd and calling Exec instead of
		// ExecWithStdin, this fires and fails the test.
		ExecFunc: func(_ context.Context, _ string, cmd []string) (string, error) {
			for _, arg := range cmd {
				if strings.Contains(arg, token) {
					t.Errorf("token leaked into argv-only Exec: %q", arg)
				}
			}
			if len(cmd) > 0 && cmd[0] == "kill" {
				return "", nil
			}
			t.Error("token write should go through ExecWithStdin, not Exec")
			return "", nil
		},
		ExecWithStdinFunc: func(_ context.Context, _ string, cmd []string, stdin io.Reader) (string, error) {
			writeCalled = true
			for _, arg := range cmd {
				if strings.Contains(arg, token) {
					t.Errorf("token embedded in ExecWithStdin's cmd (argv): %q", arg)
				}
			}
			b, err := io.ReadAll(stdin)
			if err != nil {
				t.Fatalf("failed to read stdin: %v", err)
			}
			stdinDelivered = string(b)
			return "", nil
		},
	}
	srv := New(DefaultServerConfig(), mgr, rt)

	w := doResetAuth(t, srv, token)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}
	if !writeCalled {
		t.Error("expected token write to go through ExecWithStdin")
	}
	if stdinDelivered != token {
		t.Errorf("token not delivered via stdin verbatim: got %q, want %q", stdinDelivered, token)
	}
}

// TestResetAuth_MissingTokenIsValidationError verifies an empty token is
// rejected before any container interaction.
func TestResetAuth_MissingTokenIsValidationError(t *testing.T) {
	mgr := resetAuthAgents()
	rt := &scionrt.MockRuntime{
		NameFunc: func() string { return "docker" },
		ExecFunc: func(_ context.Context, _ string, _ []string) (string, error) {
			t.Error("Exec must not be called when token is missing")
			return "", nil
		},
	}
	srv := New(DefaultServerConfig(), mgr, rt)

	w := doResetAuth(t, srv, "")

	if w.Code != http.StatusBadRequest && w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected a client error for missing token, got %d (%s)", w.Code, w.Body.String())
	}
}
