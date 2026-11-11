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
	"context"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// Tests for ptone/scion#1819: slug re-resolution in AgentManager must never
// silently pick a same-slug agent from another project.

func TestSelectAgentTarget(t *testing.T) {
	// Entries identified by their project ID (label or field; a Cloud Run
	// entry carries only the field).
	inA := api.AgentInfo{Name: "dev", ContainerID: "cid-a", Labels: map[string]string{"scion.project": "proj-a", "scion.project_id": identityProjA}}
	inB := api.AgentInfo{Name: "dev", ContainerID: "cid-b", Labels: map[string]string{"scion.project": "proj-b", "scion.project_id": identityProjB}}
	idOnlyA := api.AgentInfo{Name: "dev", ContainerID: "cid-id-a", ProjectID: identityProjA}
	// A project name label alone is not an identity: such an entry is
	// treated like one with no identity.
	nameOnlyB := api.AgentInfo{Name: "dev", ContainerID: "cid-name-b", Labels: map[string]string{"scion.project": "proj-b"}}
	nameOnlyBWithPath := api.AgentInfo{Name: "dev", ContainerID: "cid-name-b-path", ProjectPath: identityTestProjectPath(t, identityProjB),
		Labels: map[string]string{"scion.project": "proj-b"}}
	// Entries with no project identity are accepted only as containers whose
	// recorded path identifies as the requested project's ID.
	legacy := api.AgentInfo{Name: "dev", ContainerID: "cid-legacy", ProjectPath: identityTestProjectPath(t, identityProjB)}
	legacyOther := api.AgentInfo{Name: "dev", ContainerID: "cid-legacy-a", ProjectPath: identityTestProjectPath(t, identityProjA)}
	legacyNoPath := api.AgentInfo{Name: "dev", ContainerID: "cid-legacy-nopath"}

	tests := []struct {
		name        string
		agents      []api.AgentInfo
		projectName string
		projectID   string
		wantID      string
		wantFound   bool
		wantErr     bool
	}{
		{name: "scoped picks own project even when listed second", agents: []api.AgentInfo{inA, inB}, projectName: "proj-b", projectID: identityProjB, wantID: "cid-b", wantFound: true},
		{name: "scoped never falls back to other project", agents: []api.AgentInfo{inA}, projectName: "proj-b", projectID: identityProjB},
		{name: "scoped compares the project ID field of an ID-only entry", agents: []api.AgentInfo{idOnlyA}, projectName: "proj-b", projectID: identityProjB},
		{name: "scoped accepts an ID-only entry of its project", agents: []api.AgentInfo{idOnlyA}, projectName: "proj-a", projectID: identityProjA, wantID: "cid-id-a", wantFound: true},
		{name: "scoped refuses a project name label alone", agents: []api.AgentInfo{nameOnlyB}, projectName: "proj-b", projectID: identityProjB},
		{name: "scoped accepts a name-labelled entry whose path identifies as the project", agents: []api.AgentInfo{nameOnlyBWithPath}, projectName: "proj-b", projectID: identityProjB, wantID: "cid-name-b-path", wantFound: true},
		{name: "scoped without a project ID accepts no labelled entry", agents: []api.AgentInfo{inB}, projectName: "proj-b"},
		{name: "scoped prefers labelled over unlabelled", agents: []api.AgentInfo{legacy, inB}, projectName: "proj-b", projectID: identityProjB, wantID: "cid-b", wantFound: true},
		{name: "scoped accepts unlabelled legacy when nothing labelled matches", agents: []api.AgentInfo{inA, legacy}, projectName: "proj-b", projectID: identityProjB, wantID: "cid-legacy", wantFound: true},
		{name: "scoped picks the unlabelled entry of its project among two", agents: []api.AgentInfo{legacyOther, legacy}, projectName: "proj-b", projectID: identityProjB, wantID: "cid-legacy", wantFound: true},
		{name: "scoped skips unlabelled entry of another project", agents: []api.AgentInfo{inA, legacyOther}, projectName: "proj-b", projectID: identityProjB},
		{name: "scoped skips unlabelled entry without a path", agents: []api.AgentInfo{legacyNoPath}, projectName: "proj-b", projectID: identityProjB},
		{name: "scoped without a project ID accepts no unlabelled entry", agents: []api.AgentInfo{legacy}, projectName: "proj-b"},
		{name: "unscoped multi-match fails closed", agents: []api.AgentInfo{inA, inB}, wantErr: true},
		{name: "unscoped single match", agents: []api.AgentInfo{inA}, wantID: "cid-a", wantFound: true},
		{name: "duplicate reports of same container are one match", agents: []api.AgentInfo{inA, inA}, wantID: "cid-a", wantFound: true},
		{name: "no match", agents: []api.AgentInfo{{Name: "other", ContainerID: "x"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, found, err := selectAgentTarget(tt.agents, "dev", tt.projectName, tt.projectID)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if found != tt.wantFound {
				t.Fatalf("found = %v, want %v", found, tt.wantFound)
			}
			if found && got.ContainerID != tt.wantID {
				t.Errorf("ContainerID = %q, want %q", got.ContainerID, tt.wantID)
			}
		})
	}
}

func TestDelete_AmbiguousSlugFailsClosed(t *testing.T) {
	var deleted, stopped []string
	mock := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{
				{Name: "dev", ContainerID: "cid-a", Labels: map[string]string{"scion.project": "proj-a"}},
				{Name: "dev", ContainerID: "cid-b", Labels: map[string]string{"scion.project": "proj-b"}},
			}, nil
		},
		StopFunc:   func(ctx context.Context, ref runtime.RunRef) error { stopped = append(stopped, ref.ID); return nil },
		DeleteFunc: func(ctx context.Context, ref runtime.RunRef) error { deleted = append(deleted, ref.ID); return nil },
	}
	mgr := &AgentManager{Runtime: mock}
	if _, err := mgr.Delete(context.Background(), "dev", false, "", "", false); err == nil {
		t.Fatal("expected ambiguity error, got nil")
	}
	if len(deleted) != 0 || len(stopped) != 0 {
		t.Errorf("expected no runtime side effects, got stop=%v delete=%v", stopped, deleted)
	}
}

func TestStop_AmbiguousSlugFailsClosed(t *testing.T) {
	var stopped []string
	mock := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{
				{Name: "dev", ContainerID: "cid-a", Labels: map[string]string{"scion.project": "proj-a"}},
				{Name: "dev", ContainerID: "cid-b", Labels: map[string]string{"scion.project": "proj-b"}},
			}, nil
		},
		StopFunc: func(ctx context.Context, ref runtime.RunRef) error { stopped = append(stopped, ref.ID); return nil },
	}
	mgr := &AgentManager{Runtime: mock}
	if err := mgr.Stop(context.Background(), "dev", "", "", ""); err == nil {
		t.Fatal("expected ambiguity error, got nil")
	}
	if len(stopped) != 0 {
		t.Errorf("expected no runtime side effects, got stop=%v", stopped)
	}
}

func TestDeleteTarget_UsesResolvedContainerWithoutRelisting(t *testing.T) {
	var deleted, stopped []string
	listed := false
	mock := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			listed = true
			return []api.AgentInfo{{Name: "dev", ContainerID: "cid-other-project"}}, nil
		},
		StopFunc:   func(ctx context.Context, ref runtime.RunRef) error { stopped = append(stopped, ref.ID); return nil },
		DeleteFunc: func(ctx context.Context, ref runtime.RunRef) error { deleted = append(deleted, ref.ID); return nil },
	}
	mgr := &AgentManager{Runtime: mock}
	if _, err := mgr.DeleteTarget(context.Background(), "dev", "", runtime.RunRef{ID: "cid-b"}, false, "", false); err != nil {
		t.Fatalf("DeleteTarget: %v", err)
	}
	if listed {
		t.Error("DeleteTarget must not re-resolve by slug via Runtime.List")
	}
	if len(deleted) != 1 || deleted[0] != "cid-b" || len(stopped) != 1 || stopped[0] != "cid-b" {
		t.Errorf("expected stop+delete of cid-b only, got stop=%v delete=%v", stopped, deleted)
	}
}

func TestDeleteTarget_FileOnlyAgentSkipsRuntime(t *testing.T) {
	called := false
	mock := &runtime.MockRuntime{
		StopFunc:   func(ctx context.Context, ref runtime.RunRef) error { called = true; return nil },
		DeleteFunc: func(ctx context.Context, ref runtime.RunRef) error { called = true; return nil },
	}
	mgr := &AgentManager{Runtime: mock}
	if _, err := mgr.DeleteTarget(context.Background(), "dev", "", runtime.RunRef{}, false, "", false); err != nil {
		t.Fatalf("DeleteTarget: %v", err)
	}
	if called {
		t.Error("expected no runtime call for a file-only agent")
	}
}

// UAT F5 (#1846): StopProjectContainers must actually remove each matching
// container. The runtime mock filters like a real runtime (every label in the
// filter must match), so resolving the container by passing its ID where an
// agent name is expected would find nothing and remove nothing.
func TestStopProjectContainers_RemovesMatchingContainers(t *testing.T) {
	all := []api.AgentInfo{
		{Name: "dev", ContainerID: "cid-dev", Labels: map[string]string{"scion.agent": "true", "scion.name": "dev", "scion.project": "proj-a"}},
		{Name: "qa", ContainerID: "cid-qa", Labels: map[string]string{"scion.agent": "true", "scion.name": "qa", "scion.project": "proj-a"}},
		{Name: "dev", ContainerID: "cid-dev-b", Labels: map[string]string{"scion.agent": "true", "scion.name": "dev", "scion.project": "proj-b"}},
	}
	var deleted []string
	mock := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
			var out []api.AgentInfo
			for _, a := range all {
				ok := true
				for k, v := range filter {
					if a.Labels[k] != v {
						ok = false
						break
					}
				}
				if ok {
					out = append(out, a)
				}
			}
			return out, nil
		},
		StopFunc:   func(ctx context.Context, ref runtime.RunRef) error { return nil },
		DeleteFunc: func(ctx context.Context, ref runtime.RunRef) error { deleted = append(deleted, ref.ID); return nil },
	}
	mgr := &AgentManager{Runtime: mock}

	stopped := StopProjectContainers(context.Background(), mgr, "proj-a", []string{"dev"})

	if len(stopped) != 1 || stopped[0] != "dev" {
		t.Errorf("stopped = %v, want [dev]", stopped)
	}
	if len(deleted) != 1 || deleted[0] != "cid-dev" {
		t.Errorf("deleted containers = %v, want [cid-dev] only", deleted)
	}
}
