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

package api

import "testing"

func nfsStorage(server, export, subPathRoot string) *BrokerWorkspaceStorage {
	return &BrokerWorkspaceStorage{
		Backend: WorkspaceStorageBackendNFS,
		NFS:     &BrokerNFSWorkspaceStorage{Server: server, Export: export, SubPathRoot: subPathRoot},
	}
}

func TestSameWorkspaceExport(t *testing.T) {
	base := nfsStorage("10.0.0.2", "/vol1", "projects")
	unhealthy := nfsStorage("10.0.0.2", "/vol1", "projects")
	unhealthy.NFS.Healthy = false
	healthy := nfsStorage("10.0.0.2", "/vol1", "projects")
	healthy.NFS.Healthy = true

	cases := []struct {
		name string
		a, b *BrokerWorkspaceStorage
		want bool
	}{
		{"identical", base, nfsStorage("10.0.0.2", "/vol1", "projects"), true},
		{"trailing slash on export ignored", base, nfsStorage("10.0.0.2", "/vol1/", "projects"), true},
		{"health is not identity", healthy, unhealthy, true},
		{"different server", base, nfsStorage("10.0.0.3", "/vol1", "projects"), false},
		{"different export", base, nfsStorage("10.0.0.2", "/vol2", "projects"), false},
		{"different subpath root", base, nfsStorage("10.0.0.2", "/vol1", "other"), false},
		{"local backend", base, &BrokerWorkspaceStorage{Backend: WorkspaceStorageBackendLocal}, false},
		{"nfs backend without share", base, &BrokerWorkspaceStorage{Backend: WorkspaceStorageBackendNFS}, false},
		{"non-nfs backend with nfs block", base, &BrokerWorkspaceStorage{Backend: "gke-shared-volume", NFS: base.NFS}, false},
		{"nil descriptor", base, nil, false},
		{"both nil", nil, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SameWorkspaceExport(tc.a, tc.b); got != tc.want {
				t.Errorf("SameWorkspaceExport(a, b) = %v, want %v", got, tc.want)
			}
			if got := SameWorkspaceExport(tc.b, tc.a); got != tc.want {
				t.Errorf("SameWorkspaceExport(b, a) = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNormalizeExportPath_KeepsRoot(t *testing.T) {
	if got := normalizeExportPath("/"); got != "/" {
		t.Errorf("normalizeExportPath(\"/\") = %q, want \"/\"", got)
	}
}
