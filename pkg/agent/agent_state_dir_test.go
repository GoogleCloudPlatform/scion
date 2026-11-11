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
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// TestStart_PreCheckUsesEffectiveSharedDir pins final-agent #2: a broker start
// that does not carry sharedWorkspace for an agent whose state is in the
// external (Hub-project-ID) agents root checks the external provenance record
// before any side effect, as GetAgent resolves that same dir. An unusable
// external record therefore fails the start before the stopped container is
// cleaned up.
func TestStart_PreCheckUsesEffectiveSharedDir(t *testing.T) {
	projectScionDir := stateRootFixture(t, "")
	want := provisionAndResolve(t, projectScionDir)
	if err := os.WriteFile(filepath.Join(want, imageProvenanceFile), []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}
	deleted := false
	mgr := NewManager(&runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{{Name: rootAgentName, ContainerID: "stopped-1", Phase: "stopped"}}, nil
		},
		DeleteFunc: func(ctx context.Context, ref runtime.RunRef) error {
			deleted = true
			return nil
		},
		RunFunc: func(ctx context.Context, cfg runtime.RunConfig) (string, error) {
			t.Fatal("runtime must not be called with an unusable provenance record")
			return "", nil
		},
	})
	opts := strictSharedOpts(projectScionDir)
	opts.SharedWorkspace = false // the request does not say so; the state is external
	_, err := mgr.Start(context.Background(), opts)
	if !errors.Is(err, config.ErrAgentStateConflict) {
		t.Fatalf("expected the provenance error, got %v", err)
	}
	if deleted {
		t.Error("the stopped container must not be cleaned up before the provenance check")
	}
}
