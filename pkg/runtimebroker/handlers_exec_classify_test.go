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
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"testing"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8sexecutil "k8s.io/client-go/util/exec"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// realExitError runs a shell that prints a "not found" line on stderr and
// exits 127, returning the genuine *exec.ExitError (as docker/podman exec
// produce through runSimpleCommand).
func realExitError(t *testing.T) *exec.ExitError {
	t.Helper()
	err := exec.Command("sh", "-c", `echo "sh: 1: foo: not found" >&2; exit 127`).Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected *exec.ExitError, got %v", err)
	}
	return exitErr
}

// TestExecCommand_ErrorClassification pins how execCommand maps rt.Exec
// errors (ptone/scion#3470). The hub treats a broker exec agent_not_found
// as the container being gone and marks a running agent error /
// container_missing, so only a genuinely missing container may produce it:
// a command that ran in a live container and exited non-zero — even one
// whose output says "not found", which on k8s is part of the error text —
// is a command result (200 with its exit code).
func TestExecCommand_ErrorClassification(t *testing.T) {
	k8sCmdNotFound := fmt.Errorf("exec failed: %w (stderr: %s)",
		k8sexecutil.CodeExitError{Err: errors.New("command terminated with exit code 127"), Code: 127},
		"sh: 1: foo: not found")

	tests := []struct {
		name       string
		execErr    error
		wantStatus int
		wantCode   string // broker error code for non-200 responses
		wantExit   int    // exit code for 200 responses
		cancelled  bool   // the request context is already done
	}{
		{
			name:       "docker command exits non-zero",
			execErr:    fmt.Errorf("docker failed: %w", realExitError(t)),
			wantStatus: http.StatusOK,
			wantExit:   127,
		},
		{
			name:       "docker command exit error whose message says not found",
			execErr:    fmt.Errorf("docker failed: %w: sh: 1: foo: not found", realExitError(t)),
			wantStatus: http.StatusOK,
			wantExit:   127,
		},
		{
			name:       "k8s command exits non-zero with not found on stderr",
			execErr:    k8sCmdNotFound,
			wantStatus: http.StatusOK,
			wantExit:   127,
		},
		{
			name: "k8s stream error carrying command stderr with not found",
			execErr: fmt.Errorf("exec failed: %w (stderr: %s)",
				errors.New("error reading from error stream: connection reset"),
				"sh: 1: foo: not found"),
			wantStatus: http.StatusInternalServerError,
			wantCode:   ErrCodeRuntimeError,
		},
		{
			name: "substrate command exits non-zero with not found on stderr",
			execErr: &runtime.CommandExitError{
				Runtime: "substrate", Target: "ns/actor", Code: 127,
				Output: "sh: 1: foo: not found",
			},
			wantStatus: http.StatusOK,
			wantExit:   127,
		},
		{
			name: "substrate command output says container not found",
			execErr: &runtime.CommandExitError{
				Runtime: "substrate", Target: "ns/actor", Code: 1,
				Output: "Error: container not found",
			},
			wantStatus: http.StatusOK,
			wantExit:   1,
		},
		{
			name: "substrate refused setup or timeout",
			execErr: &runtime.CommandExitError{
				Runtime: "substrate", Target: "ns/actor", Code: -1,
				Output: "exec timed out",
			},
			wantStatus: http.StatusOK,
			wantExit:   -1,
		},
		{
			// A missing substrate actor is caught by the lookup miss (its
			// List is complete); a non-200 router response has no
			// documented container wording and stays a runtime error.
			name:       "substrate non-200 router response",
			execErr:    errors.New("substrate: exec on ns/actor failed: status 404: actor not found"),
			wantStatus: http.StatusInternalServerError,
			wantCode:   ErrCodeRuntimeError,
		},
		{
			name:       "runtime config file not found",
			execErr:    errors.New("failed to resolve Cloud Run config: config file /etc/scion/cloudrun.yaml not found"),
			wantStatus: http.StatusInternalServerError,
			wantCode:   ErrCodeRuntimeError,
		},
		{
			name:       "runtime binary missing",
			execErr:    fmt.Errorf("docker failed: %w", &exec.Error{Name: "docker", Err: exec.ErrNotFound}),
			wantStatus: http.StatusInternalServerError,
			wantCode:   ErrCodeRuntimeError,
		},
		{
			name: "k8s pod gone",
			execErr: fmt.Errorf("exec failed: %w (stderr: )",
				k8serrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "agent-pod")),
			wantStatus: http.StatusNotFound,
			wantCode:   ErrCodeAgentNotFound,
		},
		{
			name:       "request context done during exec",
			execErr:    fmt.Errorf("agent 'worker' container not found: %w", context.Canceled),
			cancelled:  true,
			wantStatus: http.StatusInternalServerError,
			wantCode:   ErrCodeRuntimeError,
		},
		{
			// docker/podman/apple: the container was removed after the
			// lookup (ptone/scion#3655).
			name:       "container removed after lookup",
			execErr:    fmt.Errorf("agent 'worker' %w, it may have exited and been removed", runtime.ErrContainerNotFound),
			wantStatus: http.StatusNotFound,
			wantCode:   ErrCodeAgentNotFound,
		},
		{
			// The structured signal alone, with text no wording fallback
			// matches.
			name:       "container not found sentinel only",
			execErr:    fmt.Errorf("exec: %w", sentinelOnlyErr{}),
			wantStatus: http.StatusNotFound,
			wantCode:   ErrCodeAgentNotFound,
		},
		{
			name:       "runtime reports container not found",
			execErr:    errors.New(`cloudrun-sandbox: sandbox "agent" not found in state store`),
			wantStatus: http.StatusNotFound,
			wantCode:   ErrCodeAgentNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr := &filteringMockManager{}
			mgr.agents = []api.AgentInfo{{
				ContainerID: "container-A",
				Name:        "worker",
				Labels:      map[string]string{"scion.name": "worker", "scion.project_id": "project-A"},
			}}
			rt := &runtime.MockRuntime{
				NameFunc: func() string { return "docker" },
				ExecFunc: func(_ context.Context, _ string, _ []string) (string, error) {
					return "partial-output", tt.execErr
				},
			}
			srv := New(DefaultServerConfig(), mgr, rt)

			body, _ := json.Marshal(map[string]any{"command": []string{"foo"}})
			r := httptest.NewRequest(http.MethodPost, "/api/v1/agents/worker/exec?projectId=project-A", bytes.NewReader(body))
			if tt.cancelled {
				ctx, cancel := context.WithCancel(r.Context())
				cancel()
				r = r.WithContext(ctx)
			}
			w := httptest.NewRecorder()
			srv.handleAgentByID(w, r)

			if w.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (%s)", w.Code, tt.wantStatus, w.Body.String())
			}
			if tt.wantStatus == http.StatusOK {
				var resp ExecResponse
				if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
					t.Fatalf("decode: %v", err)
				}
				if resp.ExitCode != tt.wantExit || resp.Output != "partial-output" {
					t.Errorf("got exit=%d output=%q, want exit=%d output=%q", resp.ExitCode, resp.Output, tt.wantExit, "partial-output")
				}
				return
			}
			var resp ErrorResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if resp.Error.Code != tt.wantCode {
				t.Errorf("error code = %q, want %q (%s)", resp.Error.Code, tt.wantCode, w.Body.String())
			}
		})
	}
}

// TestExecCommand_LookupMissIsAgentNotFound pins the other agent_not_found
// source: the container is absent from the runtime's (complete) listing, so
// rt.Exec is never called.
func TestExecCommand_LookupMissIsAgentNotFound(t *testing.T) {
	mgr := &filteringMockManager{}
	execed := false
	rt := &runtime.MockRuntime{
		NameFunc: func() string { return "docker" },
		ExecFunc: func(_ context.Context, _ string, _ []string) (string, error) {
			execed = true
			return "", nil
		},
	}
	srv := New(DefaultServerConfig(), mgr, rt)

	body, _ := json.Marshal(map[string]any{"command": []string{"foo"}})
	r := httptest.NewRequest(http.MethodPost, "/api/v1/agents/worker/exec?projectId=project-A", bytes.NewReader(body))
	w := httptest.NewRecorder()
	srv.handleAgentByID(w, r)

	var resp ErrorResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if w.Code != http.StatusNotFound || resp.Error.Code != ErrCodeAgentNotFound {
		t.Fatalf("got %d %q, want 404 %q (%s)", w.Code, resp.Error.Code, ErrCodeAgentNotFound, w.Body.String())
	}
	if execed {
		t.Error("rt.Exec must not run when the lookup finds no container")
	}
}

// TestIsExecTargetNotFound pins the agent_not_found fallback to
// container-specific wording: a runtime error that merely says "not found"
// (a missing config file, user, binary or image) must not read as the
// container being gone, while each runtime's own missing-container wording
// must.
func TestIsExecTargetNotFound(t *testing.T) {
	if execContainerNotFoundRe.MatchString(sentinelOnlyErr{}.Error()) {
		t.Fatalf("sentinelOnlyErr text %q must not match the wording fallback", sentinelOnlyErr{}.Error())
	}
	tests := []struct {
		name string
		err  error
		want bool
	}{
		// Not a missing container.
		{"config file", errors.New("open /home/scion/.scion/settings.yaml: config file not found"), false},
		{"runtime binary (wrapped exec.ErrNotFound)", fmt.Errorf("docker failed: %w", &exec.Error{Name: "docker", Err: exec.ErrNotFound}), false},
		{"binary as plain text", errors.New(`sandbox failed: exec: "sandbox": executable file not found in $PATH`), false},
		{"user", errors.New("unable to find user scion: no matching entries in passwd file: user not found"), false},
		{"image", errors.New("container image not found"), false},
		{"substrate router 404", errors.New("substrate: exec on ns/actor failed: status 404: actor not found"), false},
		{"substrate no cached token", errors.New("substrate: no control token cached for ns/actor"), false},
		{"container wording only in k8s command stderr", errors.New("exec failed: stream reset (stderr: Error: container not found)"), false},
		{"k8s other status", fmt.Errorf("exec failed: %w (stderr: )", k8serrors.NewBadRequest(`container agent is not valid for pod p`)), false},

		// A missing container, in each runtime's own wording.
		{"runtime.ErrContainerNotFound", fmt.Errorf("exec: %w", runtime.ErrContainerNotFound), true},
		{"runtime.ErrContainerNotFound without its wording", fmt.Errorf("exec: %w", sentinelOnlyErr{}), true},
		{"k8s NotFound status", fmt.Errorf("exec failed: %w (stderr: )", k8serrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "agent-pod")), true},
		{"k8s NotFound as text", errors.New(`exec failed: pods "agent-pod" not found (stderr: )`), true},
		{"k8s runtime lookup", errors.New("agent 'worker' pod not found, it may have been deleted"), true},
		{"docker/podman/apple runtime lookup", errors.New("agent 'worker' container not found, it may have exited and been removed"), true},
		{"cloudrun-sandbox state store", errors.New(`cloudrun-sandbox: sandbox "agent" not found in state store`), true},
		{"docker daemon", errors.New("Error response from daemon: No such container: proj--worker"), true},
		{"podman", errors.New(`Error: no container with name or ID "proj--worker" found: no such container`), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isExecTargetNotFound(tt.err); got != tt.want {
				t.Errorf("isExecTargetNotFound(%q) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// sentinelOnlyErr is runtime.ErrContainerNotFound by errors.Is, but its text
// matches no wording fallback, so only the structured check classifies it.
type sentinelOnlyErr struct{}

func (sentinelOnlyErr) Error() string        { return "exec target gone" }
func (sentinelOnlyErr) Is(target error) bool { return target == runtime.ErrContainerNotFound }
