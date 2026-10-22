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

import "github.com/GoogleCloudPlatform/scion/pkg/config"

// The built-in profile reference count (ptone/scion#5113) from plain rows,
// shared by the hub's settings overlay and the rebuild preflight
// (ptone/scion#5057), so both count the same agents under the same
// profiles.

// ProfileRefProvider is one provider row of the co-located Runtime Broker.
type ProfileRefProvider struct {
	ProjectID string
	LocalPath string
}

// ProfileRefProject is one project row.
type ProfileRefProject struct {
	ID   string
	Slug string
	// ActiveProfile is the project's active-profile annotation.
	ActiveProfile string
	// SharedWorkspace is store.Project.IsSharedWorkspace for the project,
	// evaluated by the caller.
	SharedWorkspace bool
}

// ProfileRefAgentRow is one non-deleted agent row.
type ProfileRefAgentRow struct {
	Slug            string
	ProjectID       string
	RuntimeBrokerID string
	// AppliedProfile is the hub's AppliedConfig.Profile: "" when the
	// applied config is absent or does not decode
	// (store.AppliedConfigProfile; the hub then has no AppliedConfig).
	AppliedProfile string
}

// ProfileRefInputs is the store data the reference count reads.
type ProfileRefInputs struct {
	// Providers are the co-located broker's provider rows.
	Providers []ProfileRefProvider
	Projects  []ProfileRefProject
	Agents    []ProfileRefAgentRow
}

// CountedAgent is one agent the count included, with the profile it was
// counted under ("" means none: it resolves through active_profile).
// ProjectSlug is "" when the agent's project row is missing.
type CountedAgent struct {
	ProjectID   string
	ProjectSlug string
	AgentSlug   string
	Profile     string
}

// CountsOnBroker reports whether an agent placed on runtimeBrokerID counts
// toward the profile references of the co-located broker colocatedBrokerID:
// an unplaced agent ("") or one on that broker does; an agent on any other
// broker resolves profiles against its own settings and does not. It is the
// one rule BuildProfileReferencesDetailed and the rebuild preflight's agent
// reader apply.
func CountsOnBroker(runtimeBrokerID, colocatedBrokerID string) bool {
	return runtimeBrokerID == "" || runtimeBrokerID == colocatedBrokerID
}

// BuildProfileReferences counts the references the co-located broker's
// settings overlay uses to retain built-in profiles. It is
// BuildProfileReferencesDetailed without the list of counted agents.
func BuildProfileReferences(globalDir, colocatedBrokerID string, in ProfileRefInputs) (config.ProfileReferences, bool) {
	refs, _, ok := BuildProfileReferencesDetailed(globalDir, colocatedBrokerID, in)
	return refs, ok
}

// BuildProfileReferencesDetailed counts the references for the co-located
// broker colocatedBrokerID (config.CountProfileReferences) and returns the
// agents it counted, in input order. ok is false, and the results empty,
// when colocatedBrokerID is empty.
//
// An agent on another broker is skipped before any disk read. An agent is
// counted under its AppliedProfile, except that for an agent on the
// co-located broker whose project row exists and resolves to a project
// path (config.ProjectDispatchLocation, then config.BrokerProjectPath
// under globalDir), its agent-info.json on this host decides
// (SavedProfileOnDisk):
//   - found: its profile;
//   - unreadable: the provisioned profile, else "", tallied in
//     AgentInfoUnreadable;
//   - missing: AppliedProfile, tallied in AgentInfoMissing.
//
// An unplaced agent, or one whose project is missing or resolves to no
// path, is counted under AppliedProfile with no tally.
func BuildProfileReferencesDetailed(globalDir, colocatedBrokerID string, in ProfileRefInputs) (config.ProfileReferences, []CountedAgent, bool) {
	if colocatedBrokerID == "" {
		return config.ProfileReferences{}, nil, false
	}
	localPaths := make(map[string]string, len(in.Providers))
	for _, pv := range in.Providers {
		localPaths[pv.ProjectID] = pv.LocalPath
	}
	projects := make(map[string]*ProfileRefProject, len(in.Projects))
	projectRows := make([]config.ProfileRefProject, 0, len(in.Projects))
	for i := range in.Projects {
		pr := &in.Projects[i]
		projects[pr.ID] = pr
		_, provided := localPaths[pr.ID]
		projectRows = append(projectRows, config.ProfileRefProject{
			ID:                  pr.ID,
			ActiveProfile:       pr.ActiveProfile,
			ProvidedByColocated: provided,
		})
	}

	var agentRows []config.ProfileRefAgent
	var counted []CountedAgent
	missing, unreadable := 0, 0
	for _, a := range in.Agents {
		if !CountsOnBroker(a.RuntimeBrokerID, colocatedBrokerID) {
			continue // counted nowhere; skip before any disk read
		}
		profile := a.AppliedProfile
		pr := projects[a.ProjectID]
		if a.RuntimeBrokerID == colocatedBrokerID && pr != nil {
			path, slug := config.ProjectDispatchLocation(localPaths[pr.ID], pr.Slug, config.IsGlobalProjectSlug(pr.Slug))
			if projectPath := config.BrokerProjectPath(globalDir, path, slug); projectPath != "" {
				disk, state := SavedProfileOnDisk(projectPath, a.Slug, pr.SharedWorkspace, pr.ID)
				switch state {
				case SavedProfileFound:
					profile = disk
				case SavedProfileUnreadable:
					// What start's runtime selection uses: the
					// provisioned profile, else "" (GetSavedProfile).
					unreadable++
					profile = disk
				default:
					missing++
				}
			}
		}
		agentRows = append(agentRows, config.ProfileRefAgent{SavedProfile: profile, RuntimeBrokerID: a.RuntimeBrokerID})
		c := CountedAgent{ProjectID: a.ProjectID, AgentSlug: a.Slug, Profile: profile}
		if pr != nil {
			c.ProjectSlug = pr.Slug
		}
		counted = append(counted, c)
	}

	refs, ok := config.CountProfileReferences(colocatedBrokerID, agentRows, projectRows)
	if !ok {
		return config.ProfileReferences{}, nil, false
	}
	refs.AgentInfoMissing = missing
	refs.AgentInfoUnreadable = unreadable
	return refs, counted, true
}
