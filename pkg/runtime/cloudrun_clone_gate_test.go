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
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
)

// stubCloudRunNFSMount makes provisionCloudRunNFS accept a plain directory
// as its NFS mount, and wraps cloudRunProvisionShared with record (which
// decides what the provisioning call does).
func stubCloudRunNFSMount(t *testing.T, record func(ProvisionInput) error) {
	t.Helper()
	origNFS, origShared := cloudRunRequireNFSFilesystem, cloudRunProvisionShared
	cloudRunRequireNFSFilesystem = func(string) error { return nil }
	cloudRunProvisionShared = record
	t.Cleanup(func() { cloudRunRequireNFSFilesystem, cloudRunProvisionShared = origNFS, origShared })
}

func provisionCloudRunNFSWithURL(t *testing.T, cloneURL string) error {
	t.Helper()
	r := &CloudRunRuntime{config: &config.CloudRunConfig{
		ProjectID: "gcp-project",
		Location:  "us-central1",
		NFSServer: "10.0.0.2",
		NFSExport: "/scion-workspaces",
	}}
	_, err := r.provisionCloudRunNFS(context.Background(), RunConfig{
		WorkspaceBackendName: "nfs",
		Labels:               crTestLabels,
		ProjectID:            "proj-123",
		Workspace:            filepath.Join(t.TempDir(), "projects", "proj-123", "workspace"),
		GitClone:             &api.GitCloneConfig{URL: cloneURL, Branch: "main"},
	}, crTestAgentID, os.Getuid(), os.Getgid())
	return err
}

// The Cloud Run runtime's host-side workspace clone goes through the real
// ProvisionShared, which refuses a local path, file://, http://, a
// <transport>::<address> URL and a value starting with "-", and every call
// is made with HostGitHardened set.
func TestCloudRunProvisionNFS_RefusesHostCloneURLs(t *testing.T) {
	local := filepath.Join(t.TempDir(), "repo.git")
	for name, cloneURL := range map[string]string{
		"local path":   local,
		"file":         "file://" + local,
		"http":         "http://git.example.com/org/repo.git",
		"transport":    "ext::sh -c touch% /tmp/x",
		"leading dash": "--upload-pack=touch /tmp/x",
	} {
		t.Run(name, func(t *testing.T) {
			var inputs []ProvisionInput
			stubCloudRunNFSMount(t, func(in ProvisionInput) error {
				inputs = append(inputs, in)
				return ProvisionShared(in)
			})
			err := provisionCloudRunNFSWithURL(t, cloneURL)
			if !errors.Is(err, provision.ErrHostCloneURLRefused) {
				t.Fatalf("provisionCloudRunNFS error = %v, want the clone URL refused", err)
			}
			if len(inputs) != 1 || !inputs[0].HostGitHardened {
				t.Fatalf("provisioning inputs = %d, hardened = %v; want one hardened call", len(inputs), len(inputs) == 1 && inputs[0].HostGitHardened)
			}
		})
	}
}

// Pins HostGitHardened on the Cloud Run runtime's provisioning input.
func TestCloudRunProvisionNFS_HostCloneHardened(t *testing.T) {
	var inputs []ProvisionInput
	stubCloudRunNFSMount(t, func(in ProvisionInput) error {
		inputs = append(inputs, in)
		return nil
	})
	if err := provisionCloudRunNFSWithURL(t, "https://github.com/org/repo.git"); err != nil {
		t.Fatalf("provisionCloudRunNFS: %v", err)
	}
	if len(inputs) != 1 {
		t.Fatalf("provisioning ran %d times, want 1", len(inputs))
	}
	if !inputs[0].HostGitHardened {
		t.Error("the Cloud Run runtime's host-side clone must be hardened (HostGitHardened)")
	}
}
