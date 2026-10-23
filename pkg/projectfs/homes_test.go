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

package projectfs

import (
	"strings"
	"testing"
)

const (
	homesProjectID = "0f9e8d7c-6b5a-4321-8fed-cba987654321"
	homesAgentID   = "6f1c2b3a-4d5e-4f60-8a9b-0c1d2e3f4a5b"
)

func TestAgentHomeAndSecrets(t *testing.T) {
	l := Layout{Backend: BackendNFS, HostBase: "/mnt/nfs/a", SubPathRoot: "trees", Share: Share{Export: "/vol"}}
	h, err := l.AgentHome(homesProjectID, "dev", homesAgentID)
	if err != nil {
		t.Fatal(err)
	}
	wantRel := "trees/" + homesProjectID + "/agents/dev/home-" + homesAgentID
	if h.RelPath != wantRel || h.SubPath != wantRel || h.HostPath != "/mnt/nfs/a/"+wantRel || h.ServerPath != "/vol/"+wantRel {
		t.Errorf("AgentHome = %+v, want rel %q", h, wantRel)
	}
	s, err := l.AgentSecrets(homesProjectID, "dev", homesAgentID)
	if err != nil {
		t.Fatal(err)
	}
	if want := "trees/" + homesProjectID + "/agents/dev/secrets-" + homesAgentID; s.RelPath != want {
		t.Errorf("AgentSecrets rel = %q, want %q", s.RelPath, want)
	}

	// On the transitional split, homes are on the shared-dir share.
	split := Layout{Backend: BackendLocal, SharedDirs: &l}
	if h2, err := split.AgentHome(homesProjectID, "dev", homesAgentID); err != nil || h2.HostPath != h.HostPath {
		t.Errorf("split AgentHome = (%+v, %v), want the shared-dir share's %q", h2, err, h.HostPath)
	}
}

func TestAgentHomeRejectsBadInputs(t *testing.T) {
	l := Layout{Backend: BackendNFS, HostBase: "/mnt/nfs/a"}
	cases := []struct{ name, project, agent, agentID, want string }{
		{"agent ID not a UUID", homesProjectID, "dev", "agent-1", "invalid agent ID"},
		{"agent ID upper case", homesProjectID, "dev", strings.ToUpper(homesAgentID), "invalid agent ID"},
		{"agent ID traversal", homesProjectID, "dev", "../" + homesAgentID, "invalid agent ID"},
		{"agent ID is the slug", homesProjectID, homesAgentID, homesAgentID, "invalid agent ID"},
		{"slug not a slug", homesProjectID, "Dev_1", homesAgentID, "not an agent slug"},
		{"slug traversal", homesProjectID, "../dev", homesAgentID, "not an agent slug"},
		{"bad project", "../p", "dev", homesAgentID, "invalid project ID"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := l.AgentHome(tc.project, tc.agent, tc.agentID); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("AgentHome error = %v, want %q", err, tc.want)
			}
		})
	}
	if _, err := (Layout{Backend: BackendLocal}).AgentHome(homesProjectID, "dev", homesAgentID); err == nil {
		t.Error("a local Layout has no shared home")
	}
}
