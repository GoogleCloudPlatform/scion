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

package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

func TestCloudRunRuntime_Name(t *testing.T) {
	rt := NewCloudRunRuntime(nil)
	if rt.Name() != "cloudrun" {
		t.Errorf("Name() = %q, want %q", rt.Name(), "cloudrun")
	}
}

func TestCloudRunRuntime_ExecUser(t *testing.T) {
	rt := NewCloudRunRuntime(nil)
	if rt.ExecUser() != "scion" {
		t.Errorf("ExecUser() = %q, want %q", rt.ExecUser(), "scion")
	}
}

func TestCloudRunRuntime_NewWithConfig(t *testing.T) {
	cfg := &config.V1CloudRunConfig{
		Project: "my-gcp-project",
		Region:  "us-central1",
	}
	rt := NewCloudRunRuntime(cfg)
	if rt.Project != "my-gcp-project" {
		t.Errorf("Project = %q, want %q", rt.Project, "my-gcp-project")
	}
	if rt.Region != "us-central1" {
		t.Errorf("Region = %q, want %q", rt.Region, "us-central1")
	}
}

func TestCloudRunRuntime_NewWithNilConfig(t *testing.T) {
	rt := NewCloudRunRuntime(nil)
	if rt.Project != "" {
		t.Errorf("Project = %q, want empty", rt.Project)
	}
	if rt.Region != "" {
		t.Errorf("Region = %q, want empty", rt.Region)
	}
}

func TestCloudRunRuntime_LifecycleMethodsReturnNotImplemented(t *testing.T) {
	rt := NewCloudRunRuntime(nil)
	ctx := context.Background()

	methods := []struct {
		name string
		fn   func() error
	}{
		{"Stop", func() error { return rt.Stop(ctx, "x") }},
		{"Delete", func() error { return rt.Delete(ctx, "x") }},
		{"Attach", func() error { return rt.Attach(ctx, "x") }},
		{"PullImage", func() error { return rt.PullImage(ctx, "x") }},
		{"Sync", func() error { return rt.Sync(ctx, "x", SyncTo) }},
	}

	for _, m := range methods {
		t.Run(m.name, func(t *testing.T) {
			err := m.fn()
			if err == nil || !strings.Contains(err.Error(), "not yet implemented") {
				t.Errorf("%s() error = %v, want 'not yet implemented'", m.name, err)
			}
		})
	}

	t.Run("List", func(t *testing.T) {
		_, err := rt.List(ctx, nil)
		if err == nil || !strings.Contains(err.Error(), "not yet implemented") {
			t.Errorf("List() error = %v, want 'not yet implemented'", err)
		}
	})

	t.Run("GetLogs", func(t *testing.T) {
		_, err := rt.GetLogs(ctx, "x")
		if err == nil || !strings.Contains(err.Error(), "not yet implemented") {
			t.Errorf("GetLogs() error = %v, want 'not yet implemented'", err)
		}
	})

	t.Run("ImageExists", func(t *testing.T) {
		_, err := rt.ImageExists(ctx, "x")
		if err == nil || !strings.Contains(err.Error(), "not yet implemented") {
			t.Errorf("ImageExists() error = %v, want 'not yet implemented'", err)
		}
	})

	t.Run("Exec", func(t *testing.T) {
		_, err := rt.Exec(ctx, "x", []string{"ls"})
		if err == nil || !strings.Contains(err.Error(), "not yet implemented") {
			t.Errorf("Exec() error = %v, want 'not yet implemented'", err)
		}
	})

	t.Run("GetWorkspacePath", func(t *testing.T) {
		_, err := rt.GetWorkspacePath(ctx, "x")
		if err == nil || !strings.Contains(err.Error(), "not yet implemented") {
			t.Errorf("GetWorkspacePath() error = %v, want 'not yet implemented'", err)
		}
	})
}

func TestCloudRunRuntime_Run_BrokerSideProvisioning(t *testing.T) {
	tmpDir := t.TempDir()
	mountRoot := filepath.Join(tmpDir, "nfs")
	shareDir := filepath.Join(mountRoot, "share1")
	if err := os.MkdirAll(shareDir, 0755); err != nil {
		t.Fatal(err)
	}

	rt := NewCloudRunRuntime(&config.V1CloudRunConfig{
		Project: "test-project",
		Region:  "us-central1",
	})
	rt.WorkspaceStorage = &config.V1WorkspaceStorageConfig{
		Backend: "nfs",
		NFS: &config.V1NFSConfig{
			MountRoot:   mountRoot,
			SubPathRoot: "projects",
			Shares: []config.V1NFSShare{
				{ID: "share1", Server: "10.0.0.2", Export: "/ws"},
			},
		},
	}

	cfg := RunConfig{
		Name:      "test-agent",
		ProjectID: "proj-123",
		Workspace: tmpDir,
		Labels:    map[string]string{"agent_id": "agent-1"},
	}

	// Run will provision the workspace then fail with "not yet implemented"
	// for the deploy step — that's expected.
	_, err := rt.Run(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected 'not yet implemented' error from Run")
	}
	if !strings.Contains(err.Error(), "not yet implemented") {
		t.Fatalf("Run() error = %q, want containing 'not yet implemented'", err.Error())
	}

	// Verify workspace was provisioned (directory created + sentinel)
	wsPath := filepath.Join(mountRoot, "share1", "projects", "proj-123", "workspace")
	if _, err := os.Stat(wsPath); os.IsNotExist(err) {
		t.Errorf("workspace directory %q was not created by broker-side provisioning", wsPath)
	}

	sentinelPath := filepath.Join(mountRoot, "share1", "projects", "proj-123", ".scion-provisioned")
	if _, err := os.Stat(sentinelPath); os.IsNotExist(err) {
		t.Errorf("sentinel %q was not written — ProvisionShared did not run", sentinelPath)
	}
}

func TestCloudRunRuntime_Run_CloudRunVolume_SkipsProvisionShared(t *testing.T) {
	rt := NewCloudRunRuntime(&config.V1CloudRunConfig{
		Project: "test-project",
		Region:  "us-central1",
	})
	rt.WorkspaceStorage = &config.V1WorkspaceStorageConfig{
		Backend: "cloudrun-volume",
		CloudRunVolume: &config.V1CloudRunVolumeConfig{
			VolumeName:  "workspace-vol",
			SubPathRoot: "projects",
		},
	}

	cfg := RunConfig{
		Name:      "test-agent",
		ProjectID: "proj-456",
		Labels:    map[string]string{"agent_id": "agent-2"},
	}

	// With cloudrun-volume backend, Resolve returns no HostPath, so
	// ProvisionShared is skipped (platform provisions the volume).
	// Run still fails at the deploy step.
	_, err := rt.Run(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected 'not yet implemented' error from Run")
	}
	if !strings.Contains(err.Error(), "not yet implemented") {
		t.Fatalf("Run() error = %q, want containing 'not yet implemented'", err.Error())
	}
}

func TestCloudRunRuntime_Run_MissingProjectID(t *testing.T) {
	rt := NewCloudRunRuntime(nil)
	_, err := rt.Run(context.Background(), RunConfig{})
	if err == nil || !strings.Contains(err.Error(), "ProjectID is required") {
		t.Errorf("Run() without ProjectID: error = %v, want 'ProjectID is required'", err)
	}
}

func TestGetRuntime_CloudRun(t *testing.T) {
	t.Setenv("PATH", "")

	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	globalDir := filepath.Join(tmpHome, ".scion")
	if err := os.MkdirAll(globalDir, 0755); err != nil {
		t.Fatal(err)
	}

	settings := `{
		"schema_version": "1",
		"active_profile": "cloud",
		"runtimes": {
			"cloudrun": {
				"type": "cloudrun",
				"cloudrun": {
					"project": "my-project",
					"region": "us-east1"
				}
			}
		},
		"profiles": {
			"cloud": {
				"runtime": "cloudrun"
			}
		}
	}`
	if err := os.WriteFile(filepath.Join(globalDir, "settings.json"), []byte(settings), 0644); err != nil {
		t.Fatal(err)
	}

	oldWd, _ := os.Getwd()
	tmpWd := t.TempDir()
	if err := os.Chdir(tmpWd); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldWd)

	r := GetRuntime("", "")
	cr, ok := r.(*CloudRunRuntime)
	if !ok {
		t.Fatalf("expected *CloudRunRuntime, got %T", r)
	}
	if cr.Project != "my-project" {
		t.Errorf("Project = %q, want %q", cr.Project, "my-project")
	}
	if cr.Region != "us-east1" {
		t.Errorf("Region = %q, want %q", cr.Region, "us-east1")
	}
}

func TestGetRuntime_CloudRun_DirectProfileName(t *testing.T) {
	t.Setenv("PATH", "")

	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("SCION_GROVE", "")

	globalDir := filepath.Join(tmpHome, ".scion")
	if err := os.MkdirAll(globalDir, 0755); err != nil {
		t.Fatal(err)
	}

	oldWd, _ := os.Getwd()
	tmpWd := t.TempDir()
	if err := os.Chdir(tmpWd); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldWd)

	r := GetRuntime("", "cloudrun")
	if _, ok := r.(*CloudRunRuntime); !ok {
		t.Fatalf("expected *CloudRunRuntime from direct profile name, got %T", r)
	}
}
