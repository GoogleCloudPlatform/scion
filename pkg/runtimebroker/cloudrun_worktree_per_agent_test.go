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
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func TestResolveWorktreeProvision_CloudRunRejected(t *testing.T) {
	for _, rtName := range []string{cloudRunRuntimeType, cloudRunInstancesRuntimeType} {
		t.Run(rtName, func(t *testing.T) {
			projectDir := t.TempDir()
			result := resolveWorktreeProvision(worktreeProvisionInput{
				WorkspaceMode:       store.WorkspaceModeWorktreePerAgent,
				GitClone:            &api.GitCloneConfig{URL: "https://github.com/org/repo.git"},
				ProjectPath:         projectDir,
				ProjectID:           "proj-1",
				ProjectSlug:         "my-project",
				AgentID:             "agent-1",
				AgentName:           "test-agent",
				RuntimeName:         rtName,
				eligibilityOverride: func() (bool, string) { return true, "" },
			})
			if result.ShouldProvision {
				t.Fatalf("resolveWorktreeProvision(%q): expected ShouldProvision=false", rtName)
			}
			if !strings.Contains(result.Reason, "Cloud Run") {
				t.Errorf("resolveWorktreeProvision(%q) Reason = %q, want mention of Cloud Run", rtName, result.Reason)
			}
		})
	}
}

func TestBuildStartContext_CloudRunRefusesWorktreePerAgent_NoSideEffects(t *testing.T) {
	for _, op := range []startOperation{opCreate, opHTTPStart, opHTTPRestart} {
		t.Run(string(op), func(t *testing.T) {
			srv := newTestServerForStartContextRuntime(t, ServerConfig{}, cloudRunRuntimeType)
			home, err := os.UserHomeDir()
			if err != nil {
				t.Fatal(err)
			}
			projectDir := t.TempDir()
			homeBefore := e10TreeSnapshot(t, home)
			projBefore := e10TreeSnapshot(t, projectDir)

			in := startContextInputs{
				Name:        "cr-wt-agent",
				Slug:        "cr-wt-agent",
				AgentID:     "agent-cr-wt",
				ProjectID:   "proj-cr-wt",
				ProjectSlug: "proj-cr-wt",
				ProjectPath: projectDir,
				Operation:   op,
				Config: &CreateAgentConfig{
					GitClone: &api.GitCloneConfig{URL: "https://github.com/org/repo.git"},
				},
				ResolvedEnv: map[string]string{
					"SCION_WORKSPACE_MODE": string(store.SharingModeWorktreePerAgent),
					"SCION_WORKSPACE_GIT":  "true",
				},
			}
			if op != opHTTPRestart {
				// Restart carries the mode via ResolvedEnv only; create and start
				// also set WorkspaceMode on the wire.
				in.WorkspaceMode = store.WorkspaceModeWorktreePerAgent
			}

			sc, err := srv.buildStartContext(context.Background(), in)
			if sc != nil {
				t.Fatalf("expected nil startContext, got %+v", sc)
			}
			sce := requireStartContextError(t, err)
			if sce.Status != http.StatusPreconditionFailed {
				t.Errorf("status = %d, want %d (412)", sce.Status, http.StatusPreconditionFailed)
			}
			if sce.Code != api.BrokerErrCodeUnsupportedCapability {
				t.Errorf("code = %q, want %q", sce.Code, api.BrokerErrCodeUnsupportedCapability)
			}
			for _, want := range []string{"worktree-per-agent", "clone-per-agent", "shared-plain"} {
				if !strings.Contains(sce.Message, want) {
					t.Errorf("message %q must mention %q", sce.Message, want)
				}
			}
			if got := e10TreeSnapshot(t, projBeforeRoot(projectDir)); !maps.Equal(got, projBefore) {
				t.Errorf("projectDir modified on refusal: before=%d paths, after=%d paths", len(projBefore), len(got))
			}
			if got := e10TreeSnapshot(t, home); !maps.Equal(got, homeBefore) {
				t.Errorf("HOME modified on refusal: before=%d paths, after=%d paths", len(homeBefore), len(got))
			}
		})
	}

	// Regression: clone-per-agent and shared-plain remain allowed on Cloud Run.
	for _, mode := range []string{string(store.SharingModeClonePerAgent), store.WorkspaceModePerAgent, store.WorkspaceModeShared, string(store.SharingModeSharedPlain)} {
		t.Run("allowed/"+mode, func(t *testing.T) {
			srv := newTestServerForStartContextRuntime(t, ServerConfig{}, cloudRunRuntimeType)
			projectDir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(projectDir, ".scion"), 0755); err != nil {
				t.Fatal(err)
			}
			sc, err := srv.buildStartContext(context.Background(), startContextInputs{
				Name:          "cr-ok-agent",
				Slug:          "cr-ok-agent",
				AgentID:       "agent-cr-ok",
				ProjectID:     "proj-cr-ok",
				ProjectSlug:   "proj-cr-ok",
				ProjectPath:   projectDir,
				Operation:     opCreate,
				WorkspaceMode: mode,
				Config: &CreateAgentConfig{
					Template: "default",
					GitClone: &api.GitCloneConfig{URL: "https://github.com/org/repo.git"},
				},
			})
			if err != nil {
				t.Fatalf("buildStartContext(%q) unexpected error: %v", mode, err)
			}
			if sc == nil {
				t.Fatalf("buildStartContext(%q) returned nil startContext", mode)
			}
		})
	}
}

func projBeforeRoot(dir string) string { return dir }

func TestBuildStartContext_MultiProfile_CloudRunRefusedDockerAllowed(t *testing.T) {
	srv, dotScion := newTestServerForStartContextMultiProfile(t, ServerConfig{}, "docker", "cr-prof", cloudRunRuntimeType)
	projectDir := filepath.Dir(dotScion)

	// Cloud Run profile on a Docker-default broker is refused before worktree provisioning.
	_, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:          "mp-cr-agent",
		Slug:          "mp-cr-agent",
		AgentID:       "agent-mp-cr",
		ProjectID:     "proj-mp",
		ProjectSlug:   "proj-mp",
		ProjectPath:   projectDir,
		Operation:     opCreate,
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		Config: &CreateAgentConfig{
			Profile:  "cr-prof",
			Template: "default",
			GitClone: &api.GitCloneConfig{URL: "https://github.com/org/repo.git"},
		},
	})
	sce := requireStartContextError(t, err)
	if sce.Status != http.StatusPreconditionFailed || sce.Code != api.BrokerErrCodeUnsupportedCapability {
		t.Fatalf("Cloud Run profile: got status=%d code=%q, want 412 %q (%v)", sce.Status, sce.Code, api.BrokerErrCodeUnsupportedCapability, err)
	}
	if _, statErr := os.Stat(filepath.Join(projectDir, "workspace")); !os.IsNotExist(statErr) {
		t.Errorf("host workspace/worktree must not be created for Cloud Run profile, stat err = %v", statErr)
	}

	// Docker profile on the same broker is NOT refused with 412 unsupported_capability
	// (we pass GitClone: nil so tryProvisionWorktree skips network git clone while
	// proving the Cloud Run workspace-mode gate does not block Docker).
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:          "mp-docker-agent",
		Slug:          "mp-docker-agent",
		AgentID:       "agent-mp-docker",
		ProjectID:     "proj-mp",
		ProjectSlug:   "proj-mp",
		ProjectPath:   projectDir,
		Operation:     opCreate,
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		Config:        &CreateAgentConfig{Profile: "local", Template: "default"},
	})
	if err != nil {
		t.Fatalf("Docker profile unexpected error: %v", err)
	}
	if sc == nil {
		t.Fatal("Docker profile expected non-nil startContext")
	}
}

func TestFlatCloudRun_RefusesWorktreePerAgent_NoSideEffects(t *testing.T) {
	for _, op := range e10Ops {
		t.Run("buildStartContext/"+string(op), func(t *testing.T) {
			e := newE10Server(t)
			if op != opCreate {
				e.seedLiveRecord(t)
			}
			recordsBefore := e.records(t)
			homeBefore := e10TreeSnapshot(t, e.home)

			in := e10Inputs(op, nil, map[string]string{
				"SCION_WORKSPACE_MODE": string(store.SharingModeWorktreePerAgent),
				"SCION_WORKSPACE_GIT":  "true",
			}, false)
			if op != opHTTPRestart {
				in.WorkspaceMode = store.WorkspaceModeWorktreePerAgent
			}
			if op == opCreate {
				in.ResolvedEnv = map[string]string{
					"SCION_WORKSPACE_MODE": string(store.SharingModeWorktreePerAgent),
					"SCION_WORKSPACE_GIT":  "true",
				}
			}

			sc, err := e.srv.buildStartContext(context.Background(), in)
			if sc != nil {
				t.Fatal("expected refusal, got startContext")
			}
			sce := requireStartContextError(t, err)
			if sce.Status != http.StatusPreconditionFailed || sce.Code != api.BrokerErrCodeUnsupportedCapability {
				t.Fatalf("got status=%d code=%q, want 412 %q (%v)", sce.Status, sce.Code, api.BrokerErrCodeUnsupportedCapability, err)
			}
			if got := e.records(t); !reflect.DeepEqual(got, recordsBefore) {
				t.Errorf("ownership records changed:\nbefore %+v\nafter  %+v", recordsBefore, got)
			}
			if got := e10TreeSnapshot(t, e.home); !maps.Equal(got, homeBefore) {
				t.Errorf("HOME tree modified (%d paths before, %d after)", len(homeBefore), len(got))
			}
			if _, statErr := os.Lstat(e.projectPath()); !os.IsNotExist(statErr) {
				t.Errorf("fresh project path exists after refusal: %v", statErr)
			}
			if *e.runs != 0 {
				t.Errorf("runtime Run calls = %d, want 0", *e.runs)
			}
		})
	}

	// HTTP createAgent handler (both GatherEnv=false and GatherEnv=true): must
	// refuse with 412 unsupported_capability before project-path alignment,
	// ownership recording, or 202 env-gather.
	for _, gatherEnv := range []bool{false, true} {
		t.Run(fmt.Sprintf("http-create/gatherEnv=%t", gatherEnv), func(t *testing.T) {
			e := newE10Server(t)
			recordsBefore := e.records(t)
			homeBefore := e10TreeSnapshot(t, e.home)

			body, err := json.Marshal(CreateAgentRequest{
				ID:                      e10AgentID,
				Name:                    e10Agent,
				Slug:                    e10Agent,
				ProjectID:               e10ProjectID,
				ProjectSlug:             e10ProjectSlug,
				ExpectedRuntimeTargetID: "flat-cr-target",
				WorkspaceMode:           store.WorkspaceModeWorktreePerAgent,
				GatherEnv:               gatherEnv,
				Config: &CreateAgentConfig{
					GitClone: &api.GitCloneConfig{URL: "https://github.com/org/repo.git"},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()

			e.srv.createAgent(rec, req)

			if rec.Code != http.StatusPreconditionFailed {
				t.Fatalf("HTTP status = %d, want 412: %s", rec.Code, rec.Body.String())
			}
			var errResp ErrorResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
				t.Fatalf("unmarshal ErrorResponse: %v (%s)", err, rec.Body.String())
			}
			if errResp.Error.Code != api.BrokerErrCodeUnsupportedCapability {
				t.Errorf("error.code = %q, want %q", errResp.Error.Code, api.BrokerErrCodeUnsupportedCapability)
			}
			for _, want := range []string{"worktree-per-agent", "clone-per-agent", "shared-plain"} {
				if !strings.Contains(errResp.Error.Message, want) {
					t.Errorf("error.message %q must mention %q", errResp.Error.Message, want)
				}
			}
			if got := e.records(t); !reflect.DeepEqual(got, recordsBefore) {
				t.Errorf("ownership records changed: %+v -> %+v", recordsBefore, got)
			}
			if got := e10TreeSnapshot(t, e.home); !maps.Equal(got, homeBefore) {
				t.Errorf("HOME tree modified (%d paths before, %d after)", len(homeBefore), len(got))
			}
		})
	}
}
