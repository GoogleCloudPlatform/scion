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
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
)

// ptone/scion#5825: the GKE (Kubernetes NFS home) and Cloud Run agent
// runtimes resolve an agent's home to the same relative path for the same
// (project, agent), agents/<slug>/home-<agentUUID>, under every subpath root.
func TestAgentHomeSameOnKubernetesAndCloudRun(t *testing.T) {
	const projectID, slug, agentID = "proj-123", "dev", "6f1c2b3a-4d5e-4f60-8a9b-0c1d2e3f4a5b"
	for _, root := range []string{"projects", "team/trees"} {
		t.Run(root, func(t *testing.T) {
			_, k8sHome, err := NFSHomeSubPaths(root, projectID, slug, agentID)
			if err != nil {
				t.Fatal(err)
			}
			export := "/scion-workspaces"
			cr, err := cloudRunNFSExportPaths(export, root, projectID, slug, agentID)
			if err != nil {
				t.Fatal(err)
			}
			crHome := strings.TrimPrefix(cr.homeExportPath, export+"/")
			if filepath.ToSlash(k8sHome) != crHome {
				t.Fatalf("home: kubernetes %q, cloud run %q", k8sHome, crHome)
			}
			if want := path.Join(root, projectID, "agents", slug, "home-"+agentID); crHome != want {
				t.Fatalf("home = %q, want %q", crHome, want)
			}
			hostBase := filepath.Join(string(filepath.Separator), "mnt", "nfs", "share1")
			host, err := cloudRunNFSHostPaths(filepath.Join(hostBase, filepath.FromSlash(root), projectID, "workspace"), root, projectID, slug, agentID)
			if err != nil {
				t.Fatal(err)
			}
			if host.homeHostPath != filepath.Join(hostBase, k8sHome) {
				t.Fatalf("cloud run host home %q, want %q", host.homeHostPath, filepath.Join(hostBase, k8sHome))
			}
			if want := path.Join(export, root, projectID, "agents", slug, "secrets-"+agentID); cr.secretsExportPath != want {
				t.Fatalf("secrets = %q, want %q", cr.secretsExportPath, want)
			}
		})
	}
}

// A Cloud Run agent whose home is still at the pre-upgrade location is not
// started with a new empty home: the start is refused until the migration
// moved it. A moved or brand-new home starts normally.
func TestRefuseUnmigratedCloudRunHome(t *testing.T) {
	const projectID, slug, agentID = "proj-123", "dev", "6f1c2b3a-4d5e-4f60-8a9b-0c1d2e3f4a5b"
	hostBase := t.TempDir()
	hp, err := cloudRunNFSHostPaths(filepath.Join(hostBase, "projects", projectID, "workspace"), "", projectID, slug, agentID)
	if err != nil {
		t.Fatal(err)
	}
	if err := refuseUnmigratedCloudRunHome(hp, "", projectID, agentID); err != nil {
		t.Fatalf("a new agent must start: %v", err)
	}
	legacy := filepath.Join(hostBase, "projects", projectID, "agents", agentID, "home")
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}
	err = refuseUnmigratedCloudRunHome(hp, "", projectID, agentID)
	if err == nil || !strings.Contains(err.Error(), "project-storage-migrate") {
		t.Fatalf("an unmigrated home must refuse with the remedy, got %v", err)
	}
	if err := os.MkdirAll(hp.homeHostPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := refuseUnmigratedCloudRunHome(hp, "", projectID, agentID); err != nil {
		t.Fatalf("a moved home must start: %v", err)
	}
}
