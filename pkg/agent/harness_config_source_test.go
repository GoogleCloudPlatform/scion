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
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// TestProvisionAgent_RecordsHarnessConfigSource pins the ptone/scion#620
// provenance: agent info records whether the harness-config came from the
// hub-hydrated copy or a broker-local directory.
func TestProvisionAgent_RecordsHarnessConfigSource(t *testing.T) {
	mockRuntimeForTest(t)
	tmpDir := t.TempDir()
	oldWd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldWd) })
	t.Setenv("HOME", tmpDir)

	if err := config.InitMachine(getTestHarnesses()); err != nil {
		t.Fatalf("InitMachine failed: %v", err)
	}
	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	if err := config.InitProject(projectScionDir, getTestHarnesses()); err != nil {
		t.Fatalf("InitProject failed: %v", err)
	}
	if err := os.Chdir(projectDir); err != nil {
		t.Fatal(err)
	}

	// Unstamped: resolves from the broker's own disk.
	_, _, cfg, err := ProvisionAgent(context.Background(), "local-src", "default", "", "claude", projectScionDir, "", "", "", "")
	if err != nil {
		t.Fatalf("ProvisionAgent (local) failed: %v", err)
	}
	if cfg.Info == nil || cfg.Info.HarnessConfigSource != string(config.HarnessConfigSourceBrokerLocal) {
		t.Errorf("local provision: HarnessConfigSource = %q, want %q", infoSource(cfg), config.HarnessConfigSourceBrokerLocal)
	}

	// Hub-hydrated: the dispatch context carries the hydrated path.
	hcDir, err := config.FindHarnessConfigDir("claude", projectScionDir)
	if err != nil {
		t.Fatalf("FindHarnessConfigDir: %v", err)
	}
	hydrated := filepath.Join(t.TempDir(), "claude")
	if err := os.CopyFS(hydrated, os.DirFS(hcDir.Path)); err != nil {
		t.Fatalf("copy harness-config: %v", err)
	}
	ctx := api.ContextWithHarnessConfigPath(context.Background(), hydrated)
	_, _, cfg, err = ProvisionAgent(ctx, "hub-src", "default", "", "claude", projectScionDir, "", "", "", "")
	if err != nil {
		t.Fatalf("ProvisionAgent (hydrated) failed: %v", err)
	}
	if cfg.Info == nil || cfg.Info.HarnessConfigSource != string(config.HarnessConfigSourceHubHydrated) {
		t.Errorf("hydrated provision: HarnessConfigSource = %q, want %q", infoSource(cfg), config.HarnessConfigSourceHubHydrated)
	}
}

func infoSource(cfg *api.ScionConfig) string {
	if cfg == nil || cfg.Info == nil {
		return "<nil info>"
	}
	return cfg.Info.HarnessConfigSource
}

// TestHarnessConfigResolution_SingleResolvedProjectDir pins that
// harness-config resolution uses a single resolved project dir
// (config.GetResolvedProjectDir) for provisioning and for Start's harness
// construction. The project path is given as the project root; a
// harness-config of the same name exists both at <root>/harness-configs
// and in the resolved <root>/.scion, with different content. Provisioning
// (agent-info revision) and Start (returned revision) both use the
// resolved copy.
func TestHarnessConfigResolution_SingleResolvedProjectDir(t *testing.T) {
	mockRuntimeForTest(t)
	tmpDir := t.TempDir()
	oldWd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldWd) })
	t.Setenv("HOME", tmpDir)
	if err := config.InitMachine(getTestHarnesses()); err != nil {
		t.Fatalf("InitMachine failed: %v", err)
	}
	root := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(root, ".scion")
	if err := config.InitProject(projectScionDir, getTestHarnesses()); err != nil {
		t.Fatalf("InitProject failed: %v", err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}

	global, err := config.FindHarnessConfigDir("claude", "")
	if err != nil {
		t.Fatalf("global claude harness-config: %v", err)
	}
	writeVariant := func(dir, variant string) {
		t.Helper()
		if err := os.CopyFS(dir, os.DirFS(global.Path)); err != nil {
			t.Fatalf("copy harness-config: %v", err)
		}
		cfgPath := filepath.Join(dir, "config.yaml")
		data, err := os.ReadFile(cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(cfgPath, append(data, []byte("\n# variant: "+variant+"\n")...), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	resolvedHC := filepath.Join(projectScionDir, "harness-configs", "claude")
	rawHC := filepath.Join(root, "harness-configs", "claude")
	_ = os.RemoveAll(resolvedHC)
	writeVariant(resolvedHC, "resolved")
	writeVariant(rawHC, "raw")
	wantRev := config.ComputeHarnessConfigRevision(resolvedHC)
	if wantRev == config.ComputeHarnessConfigRevision(rawHC) {
		t.Fatal("fixture: variants must differ")
	}

	mgr := NewManager(&runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, cfg runtime.RunConfig) (string, error) {
			return "mock-id", nil
		},
	})
	info, err := mgr.Start(context.Background(), api.StartOptions{
		Name:          "single-dir",
		ProjectPath:   root,
		HarnessConfig: "claude",
		NoAuth:        true,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if info.HarnessConfigRevision != wantRev {
		t.Errorf("Start resolved a different harness-config than <root>/.scion: revision %q, want %q", info.HarnessConfigRevision, wantRev)
	}

	data, err := os.ReadFile(filepath.Join(config.GetAgentHomePath(projectScionDir, "single-dir"), "agent-info.json"))
	if err != nil {
		t.Fatalf("read agent-info.json: %v", err)
	}
	var saved api.AgentInfo
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.HarnessConfigRevision != wantRev {
		t.Errorf("provisioning resolved a different harness-config than <root>/.scion: revision %q, want %q", saved.HarnessConfigRevision, wantRev)
	}
}
