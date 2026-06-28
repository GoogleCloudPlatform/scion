<<<<<<< HEAD
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

=======
>>>>>>> 696da747 (Harden Cloud Run NFS provisioning)
package runtime

import (
	"context"
<<<<<<< HEAD
	"os"
=======
>>>>>>> 696da747 (Harden Cloud Run NFS provisioning)
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

<<<<<<< HEAD
<<<<<<< HEAD
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

func TestCloudRunRuntime_NewFromInstances(t *testing.T) {
	cfg := &config.V1CloudRunInstancesConfig{
		ProjectID: "instances-project",
		Region:    "us-west1",
	}
	rt := NewCloudRunRuntimeFromInstances(cfg)
	if rt.Project != "instances-project" {
		t.Errorf("Project = %q, want %q", rt.Project, "instances-project")
	}
	if rt.Region != "us-west1" {
		t.Errorf("Region = %q, want %q", rt.Region, "us-west1")
	}
	if rt.Name() != "cloudrun" {
		t.Errorf("Name() = %q, want %q", rt.Name(), "cloudrun")
	}
}

func TestCloudRunRuntime_NewFromInstancesNil(t *testing.T) {
	rt := NewCloudRunRuntimeFromInstances(nil)
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
		agents, err := rt.List(ctx, nil)
		if err != nil {
			t.Errorf("List() error = %v, want nil", err)
		}
		if agents != nil {
			t.Errorf("List() agents = %v, want nil", agents)
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
	defer func() { _ = os.Chdir(oldWd) }()

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
	defer func() { _ = os.Chdir(oldWd) }()

	r := GetRuntime("", "cloudrun")
	if _, ok := r.(*CloudRunRuntime); !ok {
		t.Fatalf("expected *CloudRunRuntime from direct profile name, got %T", r)
=======
=======
func TestNewCloudRunRuntimeValidatesConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  *config.CloudRunInstancesConfig
		want string
	}{
		{
			name: "nil config",
			cfg:  nil,
			want: "cannot be nil",
		},
		{
			name: "missing project id",
			cfg: &config.CloudRunInstancesConfig{
				Location: "us-central1",
			},
			want: "ProjectID must be non-empty",
		},
		{
			name: "missing location",
			cfg: &config.CloudRunInstancesConfig{
				ProjectID: "gcp-project",
			},
			want: "Location must be a valid GCP region",
		},
		{
			name: "invalid location",
			cfg: &config.CloudRunInstancesConfig{
				ProjectID: "gcp-project",
				Location:  "moon",
			},
			want: "Location must be a valid GCP region",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewCloudRunRuntime(tt.cfg)
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %q, want substring %q", err, tt.want)
			}
		})
	}
}

func TestNewCloudRunRuntimeValidConfig(t *testing.T) {
	rt, err := NewCloudRunRuntime(&config.CloudRunInstancesConfig{
		ProjectID: "gcp-project",
		Location:  "us-central1",
	})
	if err != nil {
		t.Fatalf("NewCloudRunRuntime: %v", err)
	}
	if rt.Name() != "cloudrun" {
		t.Fatalf("Name() = %q, want cloudrun", rt.Name())
	}
}

func TestCloudRunInstanceIDStableFromAgentID(t *testing.T) {
	got := cloudRunInstanceID("Agent 123")
	if got != cloudRunInstanceID("Agent 123") {
		t.Fatal("cloudRunInstanceID is not stable for the same agent ID")
	}
	if got == cloudRunInstanceID("agent-123") {
		t.Fatal("distinct raw agent IDs should not collapse to the same instance ID")
	}
	if len(got) > cloudRunInstanceIDMaxLength {
		t.Fatalf("instance ID length = %d, want <= %d", len(got), cloudRunInstanceIDMaxLength)
	}
	if !regexp.MustCompile(`^agent-[a-z0-9][a-z0-9-]*[a-z0-9]$`).MatchString(got) {
		t.Fatalf("instance ID %q is not a valid lowercase hyphenated name", got)
	}
}

func TestCloudRunInstanceIDHandlesLongAndUnsafeAgentID(t *testing.T) {
	got := cloudRunInstanceID(strings.Repeat("Agent_With_Unsafe_Characters_", 8))
	if len(got) > cloudRunInstanceIDMaxLength {
		t.Fatalf("instance ID length = %d, want <= %d", len(got), cloudRunInstanceIDMaxLength)
	}
	if !strings.HasPrefix(got, "agent-agent-with-unsafe-characters") {
		t.Fatalf("instance ID = %q, want readable slug prefix", got)
	}
}

func TestCloudRunImageOperationsAreRemoteNoops(t *testing.T) {
	rt := &CloudRunRuntime{}

	exists, err := rt.ImageExists(context.Background(), "us-docker.pkg.dev/project/repo/image:tag")
	if err != nil {
		t.Fatalf("ImageExists returned error: %v", err)
	}
	if !exists {
		t.Fatal("ImageExists = false, want true because Cloud Run resolves remote images")
	}
	if err := rt.PullImage(context.Background(), "us-docker.pkg.dev/project/repo/image:tag"); err != nil {
		t.Fatalf("PullImage returned error: %v", err)
	}
}

func TestCloudRunDeferredRuntimeMethodsReturnExplicitErrors(t *testing.T) {
	rt := &CloudRunRuntime{}

	if err := rt.Sync(context.Background(), "agent-1", SyncTo); err == nil || !strings.Contains(err.Error(), "Hub workspace API") {
		t.Fatalf("Sync error = %v, want Hub workspace API guidance", err)
	}
	if _, err := rt.GetWorkspacePath(context.Background(), "agent-1"); err == nil || !strings.Contains(err.Error(), "host workspace paths are not available") {
		t.Fatalf("GetWorkspacePath error = %v, want explicit unsupported error", err)
	}
}

>>>>>>> c3337c25 (fix(runtime): ErrorRuntime on failure, implement stubs, align Dockerfile Go version)
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
	r := &CloudRunRuntime{config: &config.CloudRunInstancesConfig{
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
	r := &CloudRunRuntime{config: &config.CloudRunInstancesConfig{
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
>>>>>>> 696da747 (Harden Cloud Run NFS provisioning)
	}
}
