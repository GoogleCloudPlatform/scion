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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
)

// TestSharedDirsNotSharedWarning: a runtime that cannot mount the shared
// tree (Cloud Run) keeps node-local shared dirs on a shared backend, and
// the start carries a user-visible warning (ptone/scion#5926); runtimes
// that mount the tree, the local backend, the legacy split (refused
// instead) and a project without shared dirs get none.
func TestSharedDirsNotSharedWarning(t *testing.T) {
	nfs := projectfs.Layout{Backend: projectfs.BackendNFS}
	local := projectfs.Layout{Backend: projectfs.BackendLocal}
	dirs := []api.SharedDir{{Name: "scratchpad"}}
	for _, tc := range []struct {
		name    string
		layout  projectfs.Layout
		split   bool
		runtime string
		dirs    []api.SharedDir
		want    bool
	}{
		{"cloudrun on nfs", nfs, false, "cloudrun", dirs, true},
		{"cloudrun sandbox on gke volume", projectfs.Layout{Backend: projectfs.BackendGKESharedVolume}, false, "cloudrun-sandbox", dirs, true},
		{"docker on nfs", nfs, false, "docker", dirs, false},
		{"kubernetes on nfs", nfs, false, "kubernetes", dirs, false},
		{"cloudrun on local", local, false, "cloudrun", dirs, false},
		{"cloudrun on the legacy split", nfs, true, "cloudrun", dirs, false},
		{"no shared dirs", nfs, false, "cloudrun", nil, false},
	} {
		got := sharedDirsNotSharedWarning(tc.layout, tc.split, tc.runtime, tc.dirs)
		if (got != "") != tc.want {
			t.Errorf("%s: warning %q, want one: %v", tc.name, got, tc.want)
		}
		if got != "" && got != sharedDirsNotSharedMessage {
			t.Errorf("%s: unexpected message %q", tc.name, got)
		}
	}
}
