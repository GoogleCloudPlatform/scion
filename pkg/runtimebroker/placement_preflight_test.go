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

package runtimebroker

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/brokeridentity"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

const preflightProjectID = "3b4c5d6e-7f80-4a91-b2c3-d4e5f6a7b8c9"

// The flat scan reads the instance's ownership records and their stored
// configurations: it counts each category per agent, counts an unparseable
// or missing configuration as incomplete, skips deleting agents, and writes
// nothing. A docker target or an instance with no identity has nothing to
// scan; an unreadable identity marks the records unreadable.
func TestScanFlatPlacementPreflight_CountsAndIncomplete(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	globalDir, err := config.GetGlobalDir()
	require.NoError(t, err)
	root := filepath.Join(globalDir, "projects", "preflight")
	scionDir := filepath.Join(root, config.DotScion)
	require.NoError(t, os.MkdirAll(filepath.Join(scionDir, "agents"), 0o755))
	require.NoError(t, config.WriteProjectID(scionDir, preflightProjectID))

	inst := config.V1RuntimeBrokerInstanceConfig{Key: "agents", Name: "agents", RuntimeTarget: &config.V1RuntimeTargetConfig{
		Type: "kubernetes", NodeSelector: map[string]string{config.PoolNodeSelectorKey: "agents"}}}
	report := ScanFlatPlacementPreflight(globalDir, inst)
	require.Equal(t, config.PlacementPreflightReport{}, report, "no identity yet: nothing to scan")

	idDir := brokeridentity.InstanceDir(globalDir, "agents")
	require.NoError(t, os.MkdirAll(idDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(idDir, brokeridentity.IdentityFileName), []byte(`{"schemaVersion":1,"instanceKey":"agents","runtimeBrokerId":"rb-agents"}`), 0o600))
	stateDir, err := DefaultStateDir("rb-agents")
	require.NoError(t, err)
	store := NewOwnershipStore(stateDir, "rb-agents")

	agents := map[string]string{
		"tol-agent":     `{"kubernetes": {"tolerations": [{"key": "dedicated", "operator": "Exists"}]}}`,
		"rc-agent":      `{"kubernetes": {"runtimeClassName": "kata"}}`,
		"both-agent":    `{"kubernetes": {"runtimeClassName": "kata", "tolerations": [{"key": "a", "operator": "Exists"}]}}`,
		"clean-agent":   `{"kubernetes": {"nodeSelector": {"disk": "ssd"}}}`,
		"broken-agent":  `{"kubernetes": `,
		"missing-agent": "",
		"gone-agent":    `{"kubernetes": {"runtimeClassName": "kata"}}`,
	}
	i := 0
	for slug, content := range agents {
		i++
		agentID := fmt.Sprintf("0d2c8a6e-5f1b-4c3a-9e7d-%012x", i)
		require.NoError(t, store.BeginRun(preflightProjectID, agentID, slug, "run-"+slug))
		if content != "" {
			dir := config.GetAgentDir(scionDir, slug, false)
			require.NoError(t, os.MkdirAll(dir, 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "scion-agent.json"), []byte(content), 0o644))
		}
		if slug == "gone-agent" {
			require.NoError(t, store.SetRecordState(preflightProjectID, agentID, OwnershipStateDeleting))
		}
	}
	before, err := store.List()
	require.NoError(t, err)

	report = ScanFlatPlacementPreflight(globalDir, inst)
	require.Equal(t, 4, report.Scanned, "%+v", report)
	require.Equal(t, 2, report.Incomplete, "%+v", report)
	require.False(t, report.RecordsUnreadable)
	require.Equal(t, 2, report.Counts[config.PreflightAgentTolerationsWithoutOperatorPolicy])
	require.Equal(t, 2, report.Counts[config.PreflightAgentRuntimeClassWithoutOperatorPolicy])

	after, err := store.List()
	require.NoError(t, err)
	require.Equal(t, before, after, "the scan writes no ownership state")

	// The same agents against a target that sets tolerations and a class
	// fall into no category.
	covered := inst
	covered.RuntimeTarget = &config.V1RuntimeTargetConfig{Type: "kubernetes", RuntimeClassName: "gvisor",
		Tolerations: []api.K8sToleration{{Key: "dedicated", Operator: "Exists"}}}
	report = ScanFlatPlacementPreflight(globalDir, covered)
	require.Equal(t, 0, report.Counts[config.PreflightAgentTolerationsWithoutOperatorPolicy])
	require.Equal(t, 0, report.Counts[config.PreflightAgentRuntimeClassWithoutOperatorPolicy])
	require.Equal(t, 2, report.Incomplete)

	docker := inst
	docker.RuntimeTarget = &config.V1RuntimeTargetConfig{Type: "docker"}
	require.Equal(t, config.PlacementPreflightReport{}, ScanFlatPlacementPreflight(globalDir, docker))

	require.NoError(t, os.WriteFile(filepath.Join(idDir, brokeridentity.IdentityFileName), []byte(`{`), 0o600))
	require.True(t, ScanFlatPlacementPreflight(globalDir, inst).RecordsUnreadable)
}
