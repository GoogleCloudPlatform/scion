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
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/brokeridentity"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// ScanFlatPlacementPreflight is the read-only pre-upgrade placement scan of
// one flat instance (config.PlacementPreflightReport): the stored
// configurations of the agents the instance owns, categorized against the
// placement of its runtime_target as the host file sets it, which is the
// policy the instance activates at its next start. It writes nothing,
// creates no identity or state, and reports counts only.
//
// The agents are the instance's ownership records under its state
// directory (DefaultStateDir of the Runtime Broker ID in its persisted
// identity, as the flat host uses), except deleted or deleting ones. An
// agent whose hub-managed project cannot be found unambiguously, or whose
// stored configuration cannot be read or parsed, is counted as incomplete,
// never as matching nothing. An identity or record list that cannot be
// read marks the report RecordsUnreadable. A target other than Kubernetes,
// or an instance that was never activated (no identity), has nothing to
// scan.
func ScanFlatPlacementPreflight(globalDir string, inst config.V1RuntimeBrokerInstanceConfig) config.PlacementPreflightReport {
	var report config.PlacementPreflightReport
	t := inst.RuntimeTarget
	if t == nil || t.Type != config.RuntimeTargetTypeKubernetes {
		return report
	}
	id, found, err := readFlatIdentity(globalDir, inst.Key)
	if err != nil {
		report.RecordsUnreadable = true
		return report
	}
	if !found {
		return report
	}
	stateDir, err := DefaultStateDir(id.RuntimeBrokerID)
	if err != nil {
		report.RecordsUnreadable = true
		return report
	}
	policy := &api.ResolvedPlacement{NodeSelector: t.NodeSelector, Tolerations: t.Tolerations,
		RuntimeClassName: t.RuntimeClassName, OperatorOwned: true}
	records, err := NewOwnershipStore(stateDir, id.RuntimeBrokerID).List()
	if err != nil {
		report.RecordsUnreadable = true
		return report
	}
	for _, rec := range records {
		if rec.State == OwnershipStateDeleted || rec.State == OwnershipStateDeleting {
			continue
		}
		block, ok := storedAgentKubernetes(rec)
		if !ok {
			report.AddIncomplete()
			continue
		}
		report.Add(block, policy)
	}
	return report
}

// storedAgentKubernetes returns the kubernetes block of rec's stored agent
// configuration (nil when it sets none), read from its hub-managed project,
// or false when the project cannot be found unambiguously or the
// configuration cannot be read or parsed. Both the agent's own directory
// and the shared-workspace configuration location are tried.
func storedAgentKubernetes(rec OwnershipRecord) (*api.KubernetesConfig, bool) {
	root := uniqueHubManagedProjectPath(rec.ProjectID)
	if root == "" || rec.AgentSlug == "" || !isSingleCleanPathElement(rec.AgentSlug) {
		return nil, false
	}
	for _, shared := range []bool{false, true} {
		loaded, err := readAgentScionConfig(rec.AgentSlug, root, shared)
		if err == nil {
			return loaded.cfg.Kubernetes, true
		}
		if loaded.stage != "read" || !errors.Is(err, os.ErrNotExist) {
			return nil, false
		}
	}
	return nil, false
}

// readFlatIdentity reads instance key's persisted identity without creating
// it; found is false when the instance has none yet.
func readFlatIdentity(globalDir, key string) (*brokeridentity.Identity, bool, error) {
	data, err := os.ReadFile(filepath.Join(brokeridentity.InstanceDir(globalDir, key), brokeridentity.IdentityFileName))
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var id brokeridentity.Identity
	if err := json.Unmarshal(data, &id); err != nil || id.RuntimeBrokerID == "" {
		return nil, false, errors.New("unreadable instance identity")
	}
	return &id, true, nil
}
