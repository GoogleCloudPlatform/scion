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
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// ptone/scion#5264: Run refuses a workspace backend other than NFS before
// anything else happens. Without the share the instance would have no
// workspace or home and could never run. Nothing is provisioned, no
// Instances API call is made and no async checkpoint or handle is
// reported.
func TestCloudRunRun_RefusesNonNFSWorkspaceBackend(t *testing.T) {
	for _, backend := range []string{"", "local", "cloudrun-volume", "gke-shared-volume"} {
		t.Run("backend="+backend, func(t *testing.T) {
			fake := &fakeInstancesClient{getErr: notFoundErr()}
			rt := newFakeCloudRunRuntime(t, fake)
			provisioned := 0
			rt.provisionNFS = func(ctx context.Context, cfg RunConfig, agentID string, uid, gid int) (*cloudRunNFSProvisionPaths, error) {
				provisioned++
				return stubCloudRunNFSProvision(ctx, cfg, agentID, uid, gid)
			}
			cfg := runConfigForTest()
			cfg.WorkspaceBackendName = backend
			cfg.Labels[api.LabelRunID] = "run-a"
			rec := &sandboxHookRecorder{}
			rec.install(&cfg)

			_, err := rt.Run(context.Background(), cfg)
			if !errors.Is(err, errCloudRunNeedsNFSWorkspace) {
				t.Fatalf("Run(backend %q) = %v, want errCloudRunNeedsNFSWorkspace", backend, err)
			}
			wantName := backend
			if wantName == "" {
				wantName = "none"
			}
			if msg := err.Error(); !strings.Contains(msg, "(workspace backend: "+wantName+")") ||
				!strings.Contains(msg, `server.workspace_storage.backend to "nfs"`) {
				t.Errorf("error %q does not name the backend %q and the nfs setting", msg, wantName)
			}
			if provisioned != 0 {
				t.Errorf("NFS provisioning ran %d times, want 0", provisioned)
			}
			if n := len(fake.getReqs) + len(fake.createReqs) + len(fake.startReqs); n != 0 {
				t.Errorf("Instances API calls: get %d create %d start %d, want none",
					len(fake.getReqs), len(fake.createReqs), len(fake.startReqs))
			}
			if s := rec.gotSteps(); len(s) != 0 {
				t.Errorf("checkpoints = %v, want none", s)
			}
			if h := rec.gotHandles(); len(h) != 0 {
				t.Errorf("handles = %+v, want none", h)
			}
		})
	}
}

// The NFS backend is accepted: Run provisions the share once and creates
// the instance with the share's workspace volume.
func TestCloudRunRun_AcceptsNFSWorkspaceBackend(t *testing.T) {
	fake := &fakeInstancesClient{getErr: notFoundErr()}
	rt := newFakeCloudRunRuntime(t, fake)
	provisioned := 0
	rt.provisionNFS = func(ctx context.Context, cfg RunConfig, agentID string, uid, gid int) (*cloudRunNFSProvisionPaths, error) {
		provisioned++
		return stubCloudRunNFSProvision(ctx, cfg, agentID, uid, gid)
	}

	if _, err := rt.Run(context.Background(), runConfigForTest()); err != nil {
		t.Fatalf("Run(nfs): %v", err)
	}
	if provisioned != 1 {
		t.Errorf("NFS provisioning ran %d times, want 1", provisioned)
	}
	if len(fake.createReqs) != 1 {
		t.Fatalf("CreateInstance calls = %d, want 1", len(fake.createReqs))
	}
	var workspace bool
	for _, v := range fake.createReqs[0].GetInstance().GetVolumes() {
		if v.GetName() == "workspace" && v.GetNfs().GetServer() == "10.0.0.2" {
			workspace = true
		}
	}
	if !workspace {
		t.Error("created instance has no NFS workspace volume")
	}
}
