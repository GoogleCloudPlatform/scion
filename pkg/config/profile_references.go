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

package config

import "path/filepath"

// ProfileRefAgent is one agent row for CountProfileReferences.
type ProfileRefAgent struct {
	// SavedProfile is the profile the agent's next start uses: the Profile
	// in its agent-info.json when the file exists and can be read; else the
	// hub's AppliedConfig.Profile (what the first start sends and provision
	// saves). "" means no profile: the agent resolves through active_profile.
	SavedProfile    string
	RuntimeBrokerID string
	Deleted         bool
	OnFlatBroker    bool
}

// ProfileRefProject is one project row for CountProfileReferences.
type ProfileRefProject struct {
	ID string
	// ActiveProfile is the project's active-profile annotation (its activeProfile setting).
	ActiveProfile string
	// ProvidedByColocated is true when the co-located Runtime Broker is
	// one of the project's providers.
	ProvidedByColocated bool
}

// CountProfileReferences returns the references a co-located broker's
// settings overlay uses to retain built-in profiles (ptone/scion#5113).
// It is pure; the hub and the rebuild preflight both call it, so they count
// identically.
//
// Agents are skipped when Deleted or OnFlatBroker. An agent is counted when
// its RuntimeBrokerID equals colocatedBrokerID OR is empty (an unplaced
// agent may still be placed on the co-located broker). An empty
// SavedProfile is counted under "". Projects are counted only when
// ProvidedByColocated and ActiveProfile is non-empty.
//
// ok is false, and refs the zero value, when colocatedBrokerID is empty: the
// caller must then not call SettingsOverlay.SetProfileReferences.
func CountProfileReferences(colocatedBrokerID string, agents []ProfileRefAgent, projects []ProfileRefProject) (refs ProfileReferences, ok bool) {
	if colocatedBrokerID == "" {
		return ProfileReferences{}, false
	}
	refs = ProfileReferences{Agents: map[string]int{}, Projects: map[string]int{}}
	for _, a := range agents {
		if a.Deleted || a.OnFlatBroker {
			continue
		}
		if a.RuntimeBrokerID != "" && a.RuntimeBrokerID != colocatedBrokerID {
			continue
		}
		refs.Agents[a.SavedProfile]++
	}
	for _, p := range projects {
		if p.ProvidedByColocated && p.ActiveProfile != "" {
			refs.Projects[p.ActiveProfile]++
		}
	}
	return refs, true
}

// GlobalProjectSlug is the slug of a broker's global project on the hub:
// the CLI registers it under the name "global", and the combined hub and
// broker server creates it with this slug. Project slugs are unique on the
// hub, and only the slug identifies the global project.
const GlobalProjectSlug = "global"

// ProjectDispatchLocation returns the project location the hub sends a
// broker when dispatching an agent: the broker's provider LocalPath when it
// has one, else the project slug, which the broker resolves itself. The
// global project's slug travels with its provider path, so the broker can
// tell the global project apart from another project whose path points at
// its global directory. Pure; shared by the hub's dispatch and its
// reference count so both resolve the same agent directories.
func ProjectDispatchLocation(providerLocalPath, projectSlug string, globalProject bool) (projectPath, slug string) {
	if providerLocalPath == "" {
		return "", projectSlug
	}
	if globalProject {
		return providerLocalPath, projectSlug
	}
	return providerLocalPath, ""
}

// BrokerProjectPath returns the project path a broker uses for a dispatch
// location (ProjectDispatchLocation): projectPath when set, else the
// hub-managed project directory <globalDir>/projects/<slug>, else "". Pure;
// shared by the broker's start path and the hub's reference count.
func BrokerProjectPath(globalDir, projectPath, slug string) string {
	if projectPath != "" {
		return projectPath
	}
	if slug == "" {
		return ""
	}
	return filepath.Join(globalDir, "projects", slug)
}
