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
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// TestApplyInlineConfigUpdate_RepoRootOverrideIsInert covers the
// "start-request inline path": applyInlineConfigUpdate (handlers.go), which
// rewrites an existing agent's scion-agent.json from a start request's
// inline config, must not be a channel for setting or overwriting the
// broker-provisioned worktree's persisted repo root.
//
// It can't be, structurally: applyInlineConfigUpdate only ever reads and
// writes scion-agent.json, never the broker-owned "provisioned-worktree.json"
// file in agentDir (pkg/agent's writeProvisionedWorktreeRepoRoot /
// readProvisionedWorktreeRepoRoot, which is kept off agent-info.json
// specifically because agentHome, where that file lives, is bind-mounted
// read-write into the container). ScionConfig also has no field for either
// the old ("provisioned_worktree_repo_root") or intermediate
// ("provisionedWorktreeRepoRoot") key names. This test proves it in
// practice: it calls the real handler method with an inline config built by
// unmarshaling untrusted JSON using both field names, then confirms the
// existing agent's persisted state file survives unchanged.
func TestApplyInlineConfigUpdate_RepoRootOverrideIsInert(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	tmpDir := t.TempDir()
	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	if err := os.MkdirAll(projectScionDir, 0o755); err != nil {
		t.Fatal(err)
	}

	agentName := "existing-agent"
	agentDir := config.GetAgentDir(projectScionDir, agentName, false)
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(`{"harness":"claude"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// The legitimate, broker-authored value already on disk from a real
	// worktree provisioning — must survive the inline update unchanged. The
	// state file's shape (provisioned-worktree.json, {"repoRoot": "..."}) is
	// owned by pkg/agent; reproduced here by hand since this test lives in a
	// different package and the writer/reader are unexported.
	legitimateRoot := filepath.Join(tmpDir, "legitimate-shared-base")
	stateFile := filepath.Join(agentDir, "provisioned-worktree.json")
	stateData, err := json.Marshal(map[string]string{"repoRoot": legitimateRoot})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stateFile, stateData, 0o644); err != nil {
		t.Fatal(err)
	}

	// An inline config (as sent by the hub on the start path) unmarshaled
	// from untrusted JSON trying both field names ScionConfig/AgentInfo used
	// to have, exercised against applyInlineConfigUpdate.
	var untrustedInline api.ScionConfig
	rawInline := []byte(`{
		"provisioned_worktree_repo_root": "/etc",
		"provisionedWorktreeRepoRoot": "/etc",
		"max_turns": 7
	}`)
	if err := json.Unmarshal(rawInline, &untrustedInline); err != nil {
		t.Fatalf("unmarshal untrusted inline config: %v", err)
	}

	srv.applyInlineConfigUpdate(agentName, projectScionDir, &untrustedInline, false)

	// scion-agent.json should have picked up the legitimate field
	// (MaxTurns), proving the update actually ran, not merely no-oped.
	updatedCfg, err := (&config.Template{Path: agentDir}).LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig after update: %v", err)
	}
	if updatedCfg.MaxTurns != 7 {
		t.Fatalf("MaxTurns = %d, want 7 (the update should have applied the legitimate field)", updatedCfg.MaxTurns)
	}

	// The persisted state file — the only place the repo root lives — must be
	// completely untouched by applyInlineConfigUpdate, which never reads or
	// writes it.
	rawState, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatalf("reading provisioned-worktree.json: %v", err)
	}
	var state struct {
		RepoRoot string `json:"repoRoot"`
	}
	if err := json.Unmarshal(rawState, &state); err != nil {
		t.Fatalf("unmarshal provisioned-worktree.json: %v", err)
	}
	if state.RepoRoot != legitimateRoot {
		t.Fatalf("persisted repoRoot = %q, want unchanged %q — the config-supplied value reached the state file", state.RepoRoot, legitimateRoot)
	}

	// Belt and suspenders: raw scion-agent.json bytes must never contain the
	// injected host path at all — proves there is no field, under any name,
	// that captured it.
	rawCfg, err := os.ReadFile(filepath.Join(agentDir, "scion-agent.json"))
	if err != nil {
		t.Fatalf("reading scion-agent.json: %v", err)
	}
	if strings.Contains(string(rawCfg), "/etc") {
		t.Fatalf("scion-agent.json unexpectedly contains the injected path: %s", rawCfg)
	}
}
