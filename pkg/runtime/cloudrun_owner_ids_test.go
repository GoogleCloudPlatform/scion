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
	"testing"
)

// An unset (0) NFS uid or gid means the default 1000 on Cloud Run, the
// same as in buildCommonRunArgs and the Kubernetes runtime; each id is
// defaulted on its own.
func TestCloudRunOwnerIDs_NFSDefaults(t *testing.T) {
	tests := []struct {
		name             string
		uid, gid         int
		wantUID, wantGID int
	}{
		{"both unset", 0, 0, 1000, 1000},
		{"gid unset", 2000, 0, 2000, 1000},
		{"uid unset", 0, 3000, 1000, 3000},
		{"both set", 2000, 3000, 2000, 3000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			uid, gid := cloudRunOwnerIDs(RunConfig{
				WorkspaceBackendName: "nfs",
				NFSUID:               tt.uid,
				NFSGID:               tt.gid,
			})
			if uid != tt.wantUID || gid != tt.wantGID {
				t.Errorf("cloudRunOwnerIDs(nfs, %d:%d) = %d:%d, want %d:%d",
					tt.uid, tt.gid, uid, gid, tt.wantUID, tt.wantGID)
			}
		})
	}
}

// A non-NFS backend keeps the broker's own ids; the NFS settings are
// ignored.
func TestCloudRunOwnerIDs_NonNFSUsesProcessIDs(t *testing.T) {
	uid, gid := cloudRunOwnerIDs(RunConfig{NFSUID: 2000, NFSGID: 3000})
	if uid != os.Getuid() || gid != os.Getgid() {
		t.Errorf("cloudRunOwnerIDs(local) = %d:%d, want %d:%d", uid, gid, os.Getuid(), os.Getgid())
	}
}
