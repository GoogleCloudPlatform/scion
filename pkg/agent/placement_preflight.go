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
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// Read-only pre-upgrade placement scan of the stored configurations of a
// legacy Runtime Broker's agents (config.PlacementPreflightReport). It
// writes nothing and reports counts only.

// PlacementPolicyFunc resolves the operator placement of an agent from its
// recorded profile ("" for none); ok false means it cannot be resolved
// (config.ResolveLegacyPolicyOffline).
type PlacementPolicyFunc func(profile string) (*api.ResolvedPlacement, bool)

// AddLegacyAgentPlacement categorizes one agent of the project at
// projectPath into r. Its agent-info.json gives the runtime it runs on and
// its recorded profile, and its scion-agent.json the kubernetes block
// checked against policy's result. An agent on a runtime other than
// Kubernetes is skipped (placement does not apply to it). A missing or
// unreadable agent-info.json or scion-agent.json, an unknown runtime, or a
// policy that cannot be resolved counts as incomplete, never as matching
// nothing.
func AddLegacyAgentPlacement(r *config.PlacementPreflightReport, projectPath, agentName string, sharedWorkspace bool, policy PlacementPolicyFunc) {
	info, state := readSavedAgentInfo(agentName, projectPath)
	if state != SavedProfileFound || info.Runtime == "" {
		r.AddIncomplete()
		return
	}
	if info.Runtime != "kubernetes" {
		return
	}
	rp, ok := policy(info.Profile)
	if !ok {
		r.AddIncomplete()
		return
	}
	projectDir, err := config.GetResolvedProjectDir(projectPath)
	if err != nil {
		r.AddIncomplete()
		return
	}
	data, err := os.ReadFile(filepath.Join(config.GetAgentDir(projectDir, agentName, sharedWorkspace), "scion-agent.json"))
	if err != nil {
		r.AddIncomplete()
		return
	}
	var cfg api.ScionConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		r.AddIncomplete()
		return
	}
	r.Add(cfg.Kubernetes, rp)
}

// ScanLocalLegacyPlacement scans every agent of each project path (as
// config.GetResolvedProjectDir takes it) that has an agent directory on
// this host, in the project's own and its shared-workspace agent roots. An
// agent root that exists but cannot be listed marks the records
// unreadable.
func ScanLocalLegacyPlacement(r *config.PlacementPreflightReport, projectPaths []string, policy PlacementPolicyFunc) {
	seenProject := map[string]bool{}
	for _, projectPath := range projectPaths {
		projectDir, err := config.GetResolvedProjectDir(projectPath)
		if err != nil {
			r.RecordsUnreadable = true
			continue
		}
		if seenProject[projectDir] {
			continue
		}
		seenProject[projectDir] = true
		seenAgent := map[string]bool{}
		for _, shared := range []bool{false, true} {
			names, err := agentDirNames(config.SelectAgentsRoot(projectDir, shared))
			if err != nil {
				r.RecordsUnreadable = true
				continue
			}
			for _, name := range names {
				if seenAgent[name] {
					continue
				}
				seenAgent[name] = true
				AddLegacyAgentPlacement(r, projectPath, name, shared, policy)
			}
		}
	}
}

// agentDirNames lists the directory names in root, sorted; none when root
// does not exist.
func agentDirNames(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// ScanColocatedLegacyPlacement scans the agents of the co-located legacy
// Runtime Broker colocatedBrokerID from the hub's rows (as the profile
// reference count reads them), on this host. An agent on another Runtime
// Broker, or not placed on one yet, is counted as not covered: its pod
// takes the placement of whichever Runtime Broker starts it. An agent of
// the co-located broker whose project row is missing or resolves to no
// path is incomplete.
func ScanColocatedLegacyPlacement(globalDir, colocatedBrokerID string, in ProfileRefInputs, policy PlacementPolicyFunc) config.PlacementPreflightReport {
	var r config.PlacementPreflightReport
	localPaths := make(map[string]string, len(in.Providers))
	for _, pv := range in.Providers {
		localPaths[pv.ProjectID] = pv.LocalPath
	}
	projects := make(map[string]*ProfileRefProject, len(in.Projects))
	for i := range in.Projects {
		projects[in.Projects[i].ID] = &in.Projects[i]
	}
	for _, a := range in.Agents {
		if colocatedBrokerID == "" || a.RuntimeBrokerID != colocatedBrokerID {
			r.NotCovered++
			continue
		}
		pr := projects[a.ProjectID]
		if pr == nil {
			r.AddIncomplete()
			continue
		}
		path, slug := config.ProjectDispatchLocation(localPaths[pr.ID], pr.Slug, config.IsGlobalProjectSlug(pr.Slug))
		projectPath := config.BrokerProjectPath(globalDir, path, slug)
		if projectPath == "" {
			r.AddIncomplete()
			continue
		}
		AddLegacyAgentPlacement(&r, projectPath, a.Slug, pr.SharedWorkspace, policy)
	}
	return r
}
