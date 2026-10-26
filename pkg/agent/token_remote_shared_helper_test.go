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

package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// provisionSharedAgentGitconfig provisions a shared-workspace agent with ctx
// and returns the content of its .gitconfig ("" when there is none).
func provisionSharedAgentGitconfig(t *testing.T, ctx context.Context) string {
	t.Helper()
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	t.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	_ = os.MkdirAll(filepath.Join(globalScionDir, "templates"), 0755)
	seedTestHarnessConfig(t, globalScionDir, "claude", "claude")
	tplDir := filepath.Join(globalScionDir, "templates", "claude")
	_ = os.MkdirAll(tplDir, 0755)
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config":"claude"}`), 0644)

	projectScionDir := filepath.Join(tmpDir, "project", ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)
	sharedWorkspace := filepath.Join(tmpDir, "shared-ws")
	_ = os.MkdirAll(sharedWorkspace, 0755)

	home, _, _, err := ProvisionAgent(ctx, "shared-agent", "claude", "", "", projectScionDir, "", "", "", sharedWorkspace)
	if err != nil {
		t.Fatalf("ProvisionAgent failed: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(home, ".gitconfig"))
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// The shared-workspace credential helper is written only for the host of the
// project's recorded git remote: without one there is no helper at all
// (fail closed), and an unusable remote gets none either.
func TestProvisionAgent_SharedWorkspaceHelperNeedsTokenRemote(t *testing.T) {
	for name, ctx := range map[string]context.Context{
		"no token remote":       api.ContextWithSharedWorkspace(context.Background()),
		"empty token remote":    api.ContextWithGitTokenRemote(api.ContextWithSharedWorkspace(context.Background()), ""),
		"remote without a repo": api.ContextWithGitTokenRemote(api.ContextWithSharedWorkspace(context.Background()), "github.com"),
	} {
		t.Run(name, func(t *testing.T) {
			if content := provisionSharedAgentGitconfig(t, ctx); strings.Contains(content, "credential") {
				t.Errorf("no credential helper may be written, got:\n%s", content)
			}
		})
	}
}

// buildProvisionContext carries the shared workspace's token remote from the
// hub's SharedWorkspaceClone to provisioning.
func TestBuildProvisionContext_SharedWorkspaceTokenRemote(t *testing.T) {
	ctx, _ := buildProvisionContext(context.Background(), api.StartOptions{
		SharedWorkspace:      true,
		SharedWorkspaceClone: &api.GitCloneConfig{URL: "https://mirror.example/org/shared.git", TokenRemote: "github.com/org/shared"},
	})
	if got := api.GitTokenRemoteFromContext(ctx); got != "github.com/org/shared" {
		t.Errorf("token remote = %q, want the hub's recorded remote", got)
	}
	ctx, _ = buildProvisionContext(context.Background(), api.StartOptions{SharedWorkspace: true})
	if got := api.GitTokenRemoteFromContext(ctx); got != "" {
		t.Errorf("token remote = %q, want none without SharedWorkspaceClone", got)
	}
}
