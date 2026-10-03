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
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// newEmptyPerAgentStartContextServer returns a start-context test server and
// the global dir (~/.scion) in effect for it; newTestServerForStartContext
// points HOME at its own temp dir.
func newEmptyPerAgentStartContextServer(t *testing.T) (*Server, string) {
	t.Helper()
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		t.Fatal(err)
	}
	return srv, globalDir
}

// TestBuildStartContext_EmptyPerAgent covers design #2703 P2's start-context
// half: the mode reaches the agent as SCION_WORKSPACE_MODE=empty-per-agent
// with no SCION_WORKSPACE_GIT (even when a stale resolvedEnv says git), and
// StartOptions carries EmptyPerAgentWorkspace for ProvisionAgent. Both the
// create shape (wire workspaceMode) and the start shape (canonical value
// pre-resolved into resolvedEnv) are covered.
func TestBuildStartContext_EmptyPerAgent(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   startContextInputs
	}{
		{
			name: "create",
			in: startContextInputs{
				Name:          "worker",
				AgentID:       "agent-1",
				ProjectSlug:   "notes",
				ProjectID:     "proj-1",
				WorkspaceMode: "empty-per-agent",
				Config:        &CreateAgentConfig{Template: "claude"},
				ResolvedEnv:   map[string]string{"SCION_WORKSPACE_GIT": "true"},
				EnvClassifications: map[string]api.EnvKind{
					"SCION_WORKSPACE_GIT": api.EnvKindPlain,
				},
				Operation: opCreate,
			},
		},
		{
			name: "start",
			in: startContextInputs{
				Name:        "worker",
				AgentID:     "agent-1",
				ProjectSlug: "notes",
				ProjectID:   "proj-1",
				ResolvedEnv: map[string]string{
					"SCION_WORKSPACE_MODE": "empty-per-agent",
					"SCION_WORKSPACE_GIT":  "true",
				},
				Operation: opHTTPStart,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newEmptyPerAgentStartContextServer(t)
			sc, err := srv.buildStartContext(context.Background(), tc.in)
			if err != nil {
				t.Fatalf("buildStartContext: %v", err)
			}
			if got := sc.Opts.Env["SCION_WORKSPACE_MODE"]; got != "empty-per-agent" {
				t.Errorf("SCION_WORKSPACE_MODE = %q, want empty-per-agent", got)
			}
			if got, ok := sc.Opts.Env["SCION_WORKSPACE_GIT"]; ok {
				t.Errorf("SCION_WORKSPACE_GIT = %q, want unset (empty-per-agent is never git)", got)
			}
			if _, ok := sc.EnvClassifications["SCION_WORKSPACE_GIT"]; ok {
				t.Error("SCION_WORKSPACE_GIT left in env classifications")
			}
			if !sc.Opts.EmptyPerAgentWorkspace {
				t.Error("StartOptions.EmptyPerAgentWorkspace = false, want true")
			}
			if sc.Opts.Workspace != "" {
				t.Errorf("StartOptions.Workspace = %q, want empty", sc.Opts.Workspace)
			}
		})
	}
}

// TestBuildStartContext_EmptyPerAgentRefusesOtherWorkspaceSources pins that
// a request carrying both the mode and another workspace source is refused
// with a 400 instead of one silently winning.
func TestBuildStartContext_EmptyPerAgentRefusesOtherWorkspaceSources(t *testing.T) {
	for name, tc := range map[string]struct {
		cfg         *CreateAgentConfig
		storagePath string
	}{
		"workspace":        {cfg: &CreateAgentConfig{Workspace: "/srv/projects/notes"}},
		"git clone":        {cfg: &CreateAgentConfig{GitClone: &api.GitCloneConfig{URL: "https://example.com/r.git"}}},
		"shared workspace": {cfg: &CreateAgentConfig{SharedWorkspace: true}},
		"workspace upload": {storagePath: "workspaces/proj-1/agent-1"},
	} {
		t.Run(name, func(t *testing.T) {
			srv, _ := newEmptyPerAgentStartContextServer(t)
			_, err := srv.buildStartContext(context.Background(), startContextInputs{
				Name:                 "worker",
				AgentID:              "agent-1",
				ProjectSlug:          "notes",
				ProjectID:            "proj-1",
				WorkspaceMode:        "empty-per-agent",
				Config:               tc.cfg,
				WorkspaceStoragePath: tc.storagePath,
				Operation:            opCreate,
			})
			var sce *startContextError
			if !errors.As(err, &sce) || sce.Status != http.StatusBadRequest {
				t.Fatalf("error = %v, want 400 startContextError", err)
			}
			if !strings.Contains(sce.Message, "empty-per-agent") {
				t.Errorf("message %q should name the mode", sce.Message)
			}
		})
	}
}

// TestBuildStartContext_AmbiguousHubManagedCreateRefused pins the broker
// half of the P1 carry-over: a create for a hub-managed project with no
// workspace mode, path or clone is refused rather than falling back to the
// shared project directory. Start (which legitimately omits config), a
// linked project's own path, and a create carrying a workspace or a GCS
// workspace upload (#2760 r1 B1) are not.
func TestBuildStartContext_AmbiguousHubManagedCreateRefused(t *testing.T) {
	srv, globalDir := newEmptyPerAgentStartContextServer(t)
	hubManaged := filepath.Join(globalDir, "projects", "notes")

	base := startContextInputs{
		Name:        "worker",
		AgentID:     "agent-1",
		ProjectSlug: "notes",
		ProjectID:   "proj-1",
		Config:      &CreateAgentConfig{Template: "claude"},
		Operation:   opCreate,
	}
	for _, tc := range []struct {
		name    string
		mutate  func(*startContextInputs)
		refused bool
	}{
		{name: "slug only", mutate: func(*startContextInputs) {}, refused: true},
		{name: "pre-resolved hub-managed path", mutate: func(in *startContextInputs) { in.ProjectPath = hubManaged }, refused: true},
		{name: "nil config", mutate: func(in *startContextInputs) { in.Config = nil }, refused: true},
		{name: "workspace sent", mutate: func(in *startContextInputs) { in.Config.Workspace = hubManaged }},
		{name: "GCS workspace upload sent", mutate: func(in *startContextInputs) {
			in.ProjectPath = hubManaged
			in.WorkspaceStoragePath = "workspaces/proj-1/agent-1"
		}},
		{name: "mode sent", mutate: func(in *startContextInputs) { in.WorkspaceMode = "shared-plain" }},
		{name: "linked project path", mutate: func(in *startContextInputs) { in.ProjectPath = filepath.Join(t.TempDir(), "linked") }},
		{name: "start", mutate: func(in *startContextInputs) { in.Operation = opHTTPStart; in.Config = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := base
			cfg := *base.Config
			in.Config = &cfg
			tc.mutate(&in)
			_, err := srv.buildStartContext(context.Background(), in)
			var sce *startContextError
			isAmbiguous := errors.As(err, &sce) && sce.Status == http.StatusBadRequest &&
				strings.Contains(sce.Message, "ambiguous workspace")
			if tc.refused && !isAmbiguous {
				t.Fatalf("error = %v, want 400 ambiguous workspace", err)
			}
			if !tc.refused && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}
