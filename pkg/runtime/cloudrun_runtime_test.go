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
	rt, err := NewCloudRunRuntime(&config.CloudRunConfig{
		ProjectID: "p", Location: "us-central1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if rt.Name() != "cloudrun" {
		t.Errorf("Name() = %q, want %q", rt.Name(), "cloudrun")
	}
}

func TestCloudRunRuntime_ExecUser(t *testing.T) {
	rt, err := NewCloudRunRuntime(&config.CloudRunConfig{
		ProjectID: "p", Location: "us-central1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if rt.ExecUser() != "scion" {
		t.Errorf("ExecUser() = %q, want %q", rt.ExecUser(), "scion")
	}
}

func TestCloudRunRuntime_NewWithConfig(t *testing.T) {
	cfg := &config.CloudRunConfig{
		ProjectID: "my-gcp-project",
		Location:  "us-central1",
	}
	rt, err := NewCloudRunRuntime(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if rt.Name() != "cloudrun" {
		t.Errorf("Name() = %q, want %q", rt.Name(), "cloudrun")
	}
}

func TestCloudRunRuntime_NewWithNilConfig(t *testing.T) {
	_, err := NewCloudRunRuntime(nil)
	if err == nil {
		t.Error("expected error for nil config")
	}
}

func TestCloudRunRuntime_NewFromInstances(t *testing.T) {
	cfg := &config.V1CloudRunInstancesConfig{
		ProjectID: "instances-project",
		Region:    "us-west1",
	}
	rt, err := NewCloudRunRuntimeFromInstances(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if rt.Name() != "cloudrun" {
		t.Errorf("Name() = %q, want %q", rt.Name(), "cloudrun")
	}
}

func TestCloudRunRuntime_NewFromInstancesNil(t *testing.T) {
	_, err := NewCloudRunRuntimeFromInstances(nil)
	if err == nil {
		t.Error("expected error for nil config")
	}
}

func TestCloudRunRuntime_NewFromInstancesMissingProjectID(t *testing.T) {
	_, err := NewCloudRunRuntimeFromInstances(&config.V1CloudRunInstancesConfig{
		Region: "us-west1",
	})
	if err == nil {
		t.Error("expected error for empty ProjectID")
	}
}

func TestCloudRunRuntime_NewFromInstancesMissingRegion(t *testing.T) {
	_, err := NewCloudRunRuntimeFromInstances(&config.V1CloudRunInstancesConfig{
		ProjectID: "my-project",
	})
	if err == nil {
		t.Error("expected error for empty Region")
	}
}

func TestCloudRunRuntime_LifecycleMethods(t *testing.T) {
	rt, err := NewCloudRunRuntime(&config.CloudRunConfig{
		ProjectID: "test-project", Location: "us-central1",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Methods that make real GCP API calls — expect errors in a test
	// environment without valid credentials / API access.
	apiMethods := []struct {
		name string
		fn   func() error
	}{
		{"Stop", func() error { return rt.Stop(ctx, "x") }},
		{"Delete", func() error { return rt.Delete(ctx, "x") }},
		{"Attach", func() error { return rt.Attach(ctx, "x") }},
		{"Exec", func() error { _, e := rt.Exec(ctx, "x", []string{"ls"}); return e }},
		{"List", func() error { _, e := rt.List(ctx, nil); return e }},
		{"GetLogs", func() error { _, e := rt.GetLogs(ctx, "x"); return e }},
	}
	for _, m := range apiMethods {
		t.Run(m.name, func(t *testing.T) {
			if err := m.fn(); err == nil {
				t.Errorf("%s() expected error in test env, got nil", m.name)
			}
		})
	}

	// PullImage is a no-op for Cloud Run (images are pulled by the platform).
	t.Run("PullImage", func(t *testing.T) {
		if err := rt.PullImage(ctx, "gcr.io/test/image"); err != nil {
			t.Errorf("PullImage() error = %v, want nil", err)
		}
	})

	// ImageExists always returns true for Cloud Run (assumes valid image ref).
	t.Run("ImageExists", func(t *testing.T) {
		exists, err := rt.ImageExists(ctx, "gcr.io/test/image")
		if err != nil {
			t.Errorf("ImageExists() error = %v, want nil", err)
		}
		if !exists {
			t.Error("ImageExists() = false, want true")
		}
	})

	// Sync returns a specific unsupported-operation message.
	t.Run("Sync", func(t *testing.T) {
		err := rt.Sync(ctx, "x", SyncTo)
		if err == nil || !strings.Contains(err.Error(), "sync is not supported") {
			t.Errorf("Sync() error = %v, want 'sync is not supported'", err)
		}
	})

	// GetWorkspacePath returns a specific unsupported-operation message.
	t.Run("GetWorkspacePath", func(t *testing.T) {
		_, err := rt.GetWorkspacePath(ctx, "x")
		if err == nil || !strings.Contains(err.Error(), "host workspace paths are not available") {
			t.Errorf("GetWorkspacePath() error = %v, want 'host workspace paths are not available'", err)
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

	rt, err := NewCloudRunRuntime(&config.CloudRunConfig{
		ProjectID: "test-project",
		Location:  "us-central1",
		NFSServer: "10.0.0.2",
		NFSExport: "/ws",
	})
	if err != nil {
		t.Fatal(err)
	}

	cfg := RunConfig{
		Name:                 "test-agent",
		ProjectID:            "proj-123",
		Workspace:            tmpDir,
		WorkspaceBackendName: "nfs",
		Labels:               map[string]string{"agent_id": "agent-1"},
	}

	// Run will attempt to provision NFS then create a Cloud Run instance.
	// It will fail because we don't have a real Cloud Run API endpoint.
	_, err = rt.Run(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected error from Run in test environment")
	}
}

func TestCloudRunRuntime_Run_MissingAgentID(t *testing.T) {
	rt, err := NewCloudRunRuntime(&config.CloudRunConfig{
		ProjectID: "test-project",
		Location:  "us-central1",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = rt.Run(context.Background(), RunConfig{
		Labels: map[string]string{},
	})
	if err == nil || !strings.Contains(err.Error(), "agent_id label is required") {
		t.Errorf("Run() without agent_id: error = %v, want 'agent_id label is required'", err)
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
					"project_id": "my-project",
					"location": "us-east1"
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
	defer func() { _ = os.Chdir(oldWd) }()

	r := GetRuntime("", "")
	if _, ok := r.(*CloudRunRuntime); !ok {
		t.Fatalf("expected *CloudRunRuntime, got %T", r)
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

	// Provide a settings file with a cloudrun profile and runtime so that
	// NewCloudRunRuntime receives a valid config when "cloudrun" is
	// passed as a direct profile name.
	settings := `{
		"schema_version": "1",
		"runtimes": {
			"cloudrun": {
				"type": "cloudrun",
				"cloudrun": {
					"project_id": "my-project",
					"location": "us-east1"
				}
			}
		},
		"profiles": {
			"cloudrun": {
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
	defer func() { _ = os.Chdir(oldWd) }()

	r := GetRuntime("", "cloudrun")
	if _, ok := r.(*CloudRunRuntime); !ok {
		t.Fatalf("expected *CloudRunRuntime from direct profile name, got %T", r)
	}
}

func TestCloudRunNFSExportPaths(t *testing.T) {
	paths, err := cloudRunNFSExportPaths("/scion-workspaces/", "proj-123", "agent-456")
	if err != nil {
		t.Fatalf("cloudRunNFSExportPaths: %v", err)
	}

	if paths.workspaceExportPath != "/scion-workspaces/projects/proj-123/workspace" {
		t.Errorf("workspaceExportPath = %q", paths.workspaceExportPath)
	}
	if paths.homeExportPath != "/scion-workspaces/projects/proj-123/agents/agent-456/home" {
		t.Errorf("homeExportPath = %q", paths.homeExportPath)
	}
	if paths.secretsExportPath != "/scion-workspaces/projects/proj-123/agents/agent-456/secrets" {
		t.Errorf("secretsExportPath = %q", paths.secretsExportPath)
	}
}

func TestCloudRunNFSExportPathsRejectUnsafeInputs(t *testing.T) {
	tests := []struct {
		name      string
		export    string
		projectID string
		agentID   string
		want      string
	}{
		{
			name:      "relative export",
			export:    "scion-workspaces",
			projectID: "proj-123",
			agentID:   "agent-456",
			want:      "nfs_export must be an absolute server path",
		},
		{
			name:      "project traversal",
			export:    "/scion-workspaces",
			projectID: "../proj-123",
			agentID:   "agent-456",
			want:      "project_id",
		},
		{
			name:      "agent slash",
			export:    "/scion-workspaces",
			projectID: "proj-123",
			agentID:   "agents/agent-456",
			want:      "agent_id",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := cloudRunNFSExportPaths(tt.export, tt.projectID, tt.agentID)
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %q, want substring %q", err, tt.want)
			}
		})
	}
}

func TestCloudRunNFSHostPaths(t *testing.T) {
	hostWorkspace := filepath.Join(string(filepath.Separator), "mnt", "nfs", "share1", "projects", "proj-123", "workspace")

	paths, err := cloudRunNFSHostPaths(hostWorkspace, "proj-123", "agent-456")
	if err != nil {
		t.Fatalf("cloudRunNFSHostPaths: %v", err)
	}

	wantHostBase := filepath.Join(string(filepath.Separator), "mnt", "nfs", "share1")
	if paths.hostBase != wantHostBase {
		t.Errorf("hostBase = %q, want %q", paths.hostBase, wantHostBase)
	}
	if paths.homeHostPath != filepath.Join(wantHostBase, "projects", "proj-123", "agents", "agent-456", "home") {
		t.Errorf("homeHostPath = %q", paths.homeHostPath)
	}
	if paths.secretsHostPath != filepath.Join(wantHostBase, "projects", "proj-123", "agents", "agent-456", "secrets") {
		t.Errorf("secretsHostPath = %q", paths.secretsHostPath)
	}
}

func TestCloudRunNFSHostPathsRejectLocalAssumptions(t *testing.T) {
	tests := []struct {
		name          string
		hostWorkspace string
		want          string
	}{
		{
			name:          "relative",
			hostWorkspace: filepath.Join("mnt", "nfs", "share1", "projects", "proj-123", "workspace"),
			want:          "must be absolute",
		},
		{
			name:          "wrong suffix",
			hostWorkspace: filepath.Join(string(filepath.Separator), "tmp", "proj-123"),
			want:          "must end with",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := cloudRunNFSHostPaths(tt.hostWorkspace, "proj-123", "agent-456")
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %q, want substring %q", err, tt.want)
			}
		})
	}
}

func TestCloudRunProvisionNFSRequiresWorkspaceHostPath(t *testing.T) {
	r := &CloudRunRuntime{config: &config.CloudRunConfig{
		ProjectID: "gcp-project",
		Location:  "us-central1",
		NFSServer: "10.0.0.2",
		NFSExport: "/scion-workspaces",
	}}

	_, err := r.provisionCloudRunNFS(context.Background(), RunConfig{
		WorkspaceBackendName: "nfs",
		ProjectID:            "proj-123",
	}, "agent-456", 1000, 1000)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "RunConfig.Workspace is empty") {
		t.Fatalf("error = %q", err)
	}
}

func TestCloudRunProvisionNFSFailsWhenHubLacksNFSMount(t *testing.T) {
	root := t.TempDir()
	missingMount := filepath.Join(root, "missing-share")
	workspace := filepath.Join(missingMount, "projects", "proj-123", "workspace")
	r := &CloudRunRuntime{config: &config.CloudRunConfig{
		ProjectID: "gcp-project",
		Location:  "us-central1",
		NFSServer: "10.0.0.2",
		NFSExport: "/scion-workspaces",
	}}

	_, err := r.provisionCloudRunNFS(context.Background(), RunConfig{
		WorkspaceBackendName: "nfs",
		ProjectID:            "proj-123",
		Workspace:            workspace,
	}, "agent-456", 1000, 1000)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "Hub cannot access NFS export") {
		t.Fatalf("error = %q", err)
	}
}

func TestCloudRunShortInstanceID(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"already short", "agent-foo-0123456789", "agent-foo-0123456789"},
		{"full resource name", "projects/p/locations/us-central1/instances/agent-foo-0123456789", "agent-foo-0123456789"},
		{"empty", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := cloudRunShortInstanceID(tc.in); got != tc.want {
				t.Errorf("cloudRunShortInstanceID(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestCloudRunInstanceResourceNameIsIdempotent guards the round-trip bug where
// a full resource name was qualified a second time, producing
// "projects/p/locations/l/instances/projects/p/locations/l/instances/x".
func TestCloudRunInstanceResourceNameIsIdempotent(t *testing.T) {
	rt, err := NewCloudRunRuntime(&config.CloudRunConfig{
		ProjectID: "test-project", Location: "us-central1",
	})
	if err != nil {
		t.Fatal(err)
	}

	want := "projects/test-project/locations/us-central1/instances/agent-foo-0123456789"

	fromShort := rt.instanceResourceName("agent-foo-0123456789")
	if fromShort != want {
		t.Errorf("instanceResourceName(short) = %q, want %q", fromShort, want)
	}

	fromFull := rt.instanceResourceName(want)
	if fromFull != want {
		t.Errorf("instanceResourceName(full) = %q, want %q", fromFull, want)
	}
	if strings.Count(fromFull, "/instances/") != 1 {
		t.Errorf("instanceResourceName(full) double-qualified the name: %q", fromFull)
	}
}

// TestCloudRunRuntime_IDRoundTrip covers the Run -> List -> Stop/Delete path:
// the ContainerID that List reports for an instance must be accepted by
// Stop/Delete and resolve back to that instance's own resource name.
func TestCloudRunRuntime_IDRoundTrip(t *testing.T) {
	rt, err := NewCloudRunRuntime(&config.CloudRunConfig{
		ProjectID: "test-project", Location: "us-central1",
	})
	if err != nil {
		t.Fatal(err)
	}

	// The ID Run returns for an agent.
	runID := cloudRunInstanceID("agent-1")

	// The resource name the Instances API reports for that instance, and the
	// ContainerID List derives from it.
	instanceName := rt.instanceResourceName(runID)
	listContainerID := cloudRunShortInstanceID(instanceName)

	if listContainerID != runID {
		t.Fatalf("List ContainerID = %q, want the ID Run returned (%q)", listContainerID, runID)
	}

	// Stop/Delete qualify the ContainerID they are handed; it must address the
	// same instance List reported.
	if got := rt.instanceResourceName(listContainerID); got != instanceName {
		t.Errorf("Stop/Delete target = %q, want %q", got, instanceName)
	}
}
