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
	"testing"

	"cloud.google.com/go/run/apiv2/runpb"
)

// TestCloudRunList_NameIsAgentSlug: List reports an instance's Name as the
// agent slug from its GCP-sanitized scion_name label and its ProjectID from
// the scion_project_id label, so the broker's name and project matching
// finds it (ptone/scion#5268). ContainerID stays the short instance ID. An
// instance without the name label falls back to its resource name.
func TestCloudRunList_NameIsAgentSlug(t *testing.T) {
	const (
		labelled   = "projects/test-project/locations/us-central1/instances/agent-dev-0000000000"
		unlabelled = "projects/test-project/locations/us-central1/instances/agent-legacy-1111111111"
		projectID  = "8f0c2a4e-1b2d-4c6e-9a7b-3d5f6e7a8b9c"
	)
	fake := &fakeInstancesClient{listResult: []*runpb.Instance{
		{Name: labelled, Labels: map[string]string{
			"agent_id": "agent-1", "scion_agent": "true", "scion_name": "dev", "scion_project_id": projectID,
		}},
		{Name: unlabelled, Labels: map[string]string{"agent_id": "agent-2", "scion_agent": "true"}},
	}}
	rt := newFakeCloudRunRuntime(t, fake)

	agents, err := rt.List(context.Background(), map[string]string{"scion.agent": "true"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(agents) != 2 {
		t.Fatalf("List returned %d agents, want 2", len(agents))
	}
	got := map[string]struct{ name, projectID string }{}
	for _, a := range agents {
		got[a.ContainerID] = struct{ name, projectID string }{a.Name, a.ProjectID}
	}
	if g := got["agent-dev-0000000000"]; g.name != "dev" || g.projectID != projectID {
		t.Errorf("labelled instance: Name = %q, ProjectID = %q; want %q, %q", g.name, g.projectID, "dev", projectID)
	}
	if g := got["agent-legacy-1111111111"]; g.name != unlabelled || g.projectID != "" {
		t.Errorf("instance without the name label: Name = %q, ProjectID = %q; want the resource name %q and no project", g.name, g.projectID, unlabelled)
	}
}
