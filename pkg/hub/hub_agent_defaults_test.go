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

package hub

import (
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
)

// TestApplySnapshot_AgentDefaults_PopulatesServerConfig covers the missing hop
// the design names in §3.2.4: Snapshot() already fills Layer1Snapshot.Default*,
// but ApplySnapshot did not write those six fields into ServerConfig, so they
// were display-only.
func TestApplySnapshot_AgentDefaults_PopulatesServerConfig(t *testing.T) {
	srv := &Server{maintenance: NewMaintenanceState(false, "")}

	snap := Layer1Snapshot{
		DefaultTemplate:      "team-default",
		DefaultHarnessConfig: "claude-vertex",
		DefaultMaxTurns:      50,
		DefaultMaxModelCalls: 200,
		DefaultMaxDuration:   "2h",
		DefaultResources: &api.ResourceSpec{
			Requests: api.ResourceList{CPU: "1", Memory: "2Gi"},
			Disk:     "10Gi",
		},
	}

	results := ApplySnapshot(srv, snap)

	got := srv.hubAgentDefaults()
	if got.DefaultTemplate != "team-default" {
		t.Errorf("DefaultTemplate: want team-default, got %q", got.DefaultTemplate)
	}
	if got.DefaultHarnessConfig != "claude-vertex" {
		t.Errorf("DefaultHarnessConfig: want claude-vertex, got %q", got.DefaultHarnessConfig)
	}
	if got.DefaultMaxTurns != 50 {
		t.Errorf("DefaultMaxTurns: want 50, got %d", got.DefaultMaxTurns)
	}
	if got.DefaultMaxModelCalls != 200 {
		t.Errorf("DefaultMaxModelCalls: want 200, got %d", got.DefaultMaxModelCalls)
	}
	if got.DefaultMaxDuration != "2h" {
		t.Errorf("DefaultMaxDuration: want 2h, got %q", got.DefaultMaxDuration)
	}
	if got.DefaultResources == nil || got.DefaultResources.Disk != "10Gi" {
		t.Errorf("DefaultResources: want disk 10Gi, got %+v", got.DefaultResources)
	}

	applied, _ := results["applied"].([]string)
	found := false
	for _, f := range applied {
		if f == "agent_defaults" {
			found = true
		}
	}
	if !found {
		t.Errorf("want agent_defaults in applied list, got %v", applied)
	}
}

// TestHubAgentDefaults_ReturnsDeepCopy proves the accessor does not hand callers
// a live pointer into s.config: a caller mutating the returned ResourceSpec must
// not change the server's copy.
func TestHubAgentDefaults_ReturnsDeepCopy(t *testing.T) {
	srv := &Server{maintenance: NewMaintenanceState(false, "")}
	ApplySnapshot(srv, Layer1Snapshot{
		DefaultResources: &api.ResourceSpec{Disk: "10Gi"},
	})

	first := srv.hubAgentDefaults()
	first.DefaultResources.Disk = "999Gi"

	second := srv.hubAgentDefaults()
	if second.DefaultResources.Disk != "10Gi" {
		t.Errorf("accessor leaked its pointee: want 10Gi, got %q", second.DefaultResources.Disk)
	}
}

// TestApplySnapshot_AgentDefaults_ClearedWhenSnapshotEmpty verifies the write is
// unconditional: deleting the DB section must clear the cached value rather than
// leaving a stale default in place forever.
func TestApplySnapshot_AgentDefaults_ClearedWhenSnapshotEmpty(t *testing.T) {
	srv := &Server{maintenance: NewMaintenanceState(false, "")}
	ApplySnapshot(srv, Layer1Snapshot{DefaultTemplate: "team-default", DefaultMaxTurns: 50})
	if srv.hubAgentDefaults().DefaultTemplate != "team-default" {
		t.Fatal("setup: DefaultTemplate not applied")
	}

	ApplySnapshot(srv, Layer1Snapshot{})

	got := srv.hubAgentDefaults()
	if got.DefaultTemplate != "" || got.DefaultMaxTurns != 0 {
		t.Errorf("want zeroed agent defaults after empty snapshot, got %+v", got)
	}
}

// TestApplySnapshot_FileMode_AgentDefaultsStayZero is the file-mode-parity guard
// (acceptance criterion 12 / rejected alternative A7). BuildLayer1SnapshotFromFile
// must keep leaving the six agent-defaults fields at zero even when settings.yaml
// sets them, so the hub-side rungs never fire in file mode and the co-located
// broker keeps applying them at the BOTTOM of its own chain.
func TestApplySnapshot_FileMode_AgentDefaultsStayZero(t *testing.T) {
	// GlobalConfig does not even carry the agent-defaults keys (they live in the
	// versioned-settings tier the broker reads), so there is nothing for
	// BuildLayer1SnapshotFromFile to copy. Assert that explicitly, so the guard
	// survives someone adding the fields to GlobalConfig later.
	gc := &config.GlobalConfig{
		Hub:  config.HubServerConfig{AdminEmails: []string{"admin@file.com"}},
		Auth: config.DevAuthConfig{UserAccessMode: "open"},
	}

	snap := BuildLayer1SnapshotFromFile(gc)

	if snap.DefaultTemplate != "" || snap.DefaultHarnessConfig != "" || snap.DefaultMaxTurns != 0 ||
		snap.DefaultMaxModelCalls != 0 || snap.DefaultMaxDuration != "" || snap.DefaultResources != nil {
		t.Fatalf("file-mode snapshot must leave agent defaults zero, got %+v", snap)
	}

	srv := &Server{maintenance: NewMaintenanceState(false, "")}
	ApplySnapshot(srv, snap)

	if got := srv.hubAgentDefaults(); got != (opsettings.AgentDefaultsSettings{}) {
		t.Errorf("file mode: want zero agent defaults in ServerConfig, got %+v", got)
	}
}

// TestHubAgentDefaults_ConcurrentWithApplySnapshot is design §5.2 risk (e): the
// propagation goroutine calls ApplySnapshot (which writes s.config under s.mu)
// while request paths read the defaults. Run under -race; without the RLock in
// hubAgentDefaults this fails with a data race.
func TestHubAgentDefaults_ConcurrentWithApplySnapshot(t *testing.T) {
	srv := &Server{maintenance: NewMaintenanceState(false, "")}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			ApplySnapshot(srv, Layer1Snapshot{
				DefaultTemplate:  "t",
				DefaultMaxTurns:  i,
				DefaultResources: &api.ResourceSpec{Disk: "10Gi"},
			})
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			_ = srv.hubAgentDefaults()
		}
	}()

	wg.Wait()
}
