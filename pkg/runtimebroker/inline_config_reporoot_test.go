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

// TestApplyInlineConfigUpdate_RepoRootInjectionIsInert is the C1 regression
// test for the "start-request inline path" the round-1 review called out
// specifically: applyInlineConfigUpdate (handlers.go), which rewrites an
// existing agent's scion-agent.json from a start request's inline config,
// must not be a channel for setting or overwriting the broker-authored
// AgentInfo.ProvisionedWorktreeRepoRoot value.
//
// It can't be, structurally: applyInlineConfigUpdate only ever reads and
// writes scion-agent.json, never agent-info.json, and ScionConfig has no
// field for either the old ("provisioned_worktree_repo_root") or new
// ("provisionedWorktreeRepoRoot") key names. This test proves that in
// practice: it calls the real handler method with an inline config built by
// unmarshaling the review's PoC-shaped JSON (the same "/etc" example used
// against the old, now-removed ScionConfig field), then confirms the
// existing agent's persisted AgentInfo.ProvisionedWorktreeRepoRoot survives
// unchanged.
func TestApplyInlineConfigUpdate_RepoRootInjectionIsInert(t *testing.T) {
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
	agentHome := config.GetAgentHomePath(projectScionDir, agentName)
	if err := os.MkdirAll(agentHome, 0o755); err != nil {
		t.Fatal(err)
	}
	// The legitimate, broker-authored value already on disk from a real
	// worktree provisioning — must survive the inline update unchanged.
	legitimateRoot := filepath.Join(tmpDir, "legitimate-shared-base")
	existingInfo := api.AgentInfo{ProvisionedWorktreeRepoRoot: legitimateRoot}
	infoData, err := json.Marshal(existingInfo)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentHome, "agent-info.json"), infoData, 0o644); err != nil {
		t.Fatal(err)
	}

	// The review's PoC, replayed against applyInlineConfigUpdate: an inline
	// config (as sent by the hub on the start path) unmarshaled from
	// untrusted JSON trying both the old and new field names.
	var maliciousInline api.ScionConfig
	rawInline := []byte(`{
		"provisioned_worktree_repo_root": "/etc",
		"provisionedWorktreeRepoRoot": "/etc",
		"max_turns": 7
	}`)
	if err := json.Unmarshal(rawInline, &maliciousInline); err != nil {
		t.Fatalf("unmarshal malicious inline config: %v", err)
	}

	srv.applyInlineConfigUpdate(agentName, projectScionDir, &maliciousInline, false)

	// scion-agent.json should have picked up the legitimate field
	// (MaxTurns), proving the update actually ran, not merely no-oped.
	updatedCfg, err := (&config.Template{Path: agentDir}).LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig after update: %v", err)
	}
	if updatedCfg.MaxTurns != 7 {
		t.Fatalf("MaxTurns = %d, want 7 (the update should have applied the legitimate field)", updatedCfg.MaxTurns)
	}

	// The persisted AgentInfo — the only place ProvisionedWorktreeRepoRoot
	// lives — must be completely untouched by applyInlineConfigUpdate, which
	// never reads or writes agent-info.json.
	rawInfo, err := os.ReadFile(filepath.Join(agentHome, "agent-info.json"))
	if err != nil {
		t.Fatalf("reading agent-info.json: %v", err)
	}
	var info api.AgentInfo
	if err := json.Unmarshal(rawInfo, &info); err != nil {
		t.Fatalf("unmarshal agent-info.json: %v", err)
	}
	if info.ProvisionedWorktreeRepoRoot != legitimateRoot {
		t.Fatalf("AgentInfo.ProvisionedWorktreeRepoRoot = %q, want unchanged %q — the inline config injection reached agent-info.json", info.ProvisionedWorktreeRepoRoot, legitimateRoot)
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
