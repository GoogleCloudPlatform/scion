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

package cmd

import (
	"os"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	scionruntime "github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// legacyPlacementNotEnumerated is the fixed phrase for the agent source the
// host-local legacy scan cannot list: linked projects are known to the Hub,
// not to this host's directories.
const legacyPlacementNotEnumerated = "linked projects other than the current one (run scion doctor inside each)"

// legacyPlacementPreflightRemedy is the remediation of the legacy placement
// pre-upgrade scan: the migration steps, in short.
const legacyPlacementPreflightRemedy = "Settings counted here are refused when runtimes or profiles are next saved; agents counted here are refused at their next start or restart on a Kubernetes runtime. " +
	"Repair the named settings sections first. For agent tolerations or a runtime class with no runtime or profile policy, add the intended values to the runtime entry or profile, or remove them from the agent's configuration; " +
	"an agent may add node selector keys but not change ones the runtime or profile sets. Resolve incomplete coverage (unreadable records, agents whose profile or runtime cannot be resolved offline, linked projects) first. " +
	"Settings stored in the Hub are reported by its server-config view. Run scion doctor again after each change."

// checkDoctorLegacyPlacementPreflightHere runs the legacy placement
// pre-upgrade scan on this host: its global settings file's runtime entries
// and profiles, and the stored configurations of the agents in the global
// project, the hub-managed projects under the global directory and the
// current project. It runs on every legacy host, including one that
// configures no Kubernetes runtime entry (its Kubernetes agents run on the
// built-in entry, with an empty operator policy). ok is false only on a
// host with flat instances, which the flat check covers. Settings that
// cannot be read give a warning line with incomplete coverage, never a
// missing line.
func checkDoctorLegacyPlacementPreflightHere() (scionruntime.CheckResult, bool) {
	instances, err := config.LoadRuntimeBrokerInstances("")
	if err != nil {
		return scionruntime.CheckResult{Name: "legacy-placement-preflight", Status: "warn",
			Message:     "Could not load server.broker.instances to tell a legacy host from a flat one; Kubernetes placement coverage incomplete",
			Remediation: "Fix server.broker.instances (scion config validate), then run scion doctor again"}, true
	}
	if len(instances) > 0 {
		return scionruntime.CheckResult{}, false
	}
	vs, _, err := config.LoadGlobalSettings()
	if err != nil || vs == nil {
		return scionruntime.CheckResult{Name: "legacy-placement-preflight", Status: "warn",
			Message:     "Could not load the global settings to scan Kubernetes placement; coverage incomplete",
			Remediation: "Fix the global settings (scion config validate), then run scion doctor again"}, true
	}
	configured, known := config.ConfiguredRuntimeNames()
	isConfigured := func(name string) bool { return !known || configured[name] }
	// Kubernetes use the settings show: a configured Kubernetes runtime
	// entry, or an operator profile naming a Kubernetes runtime entry.
	kubernetesUse := false
	for name, rt := range vs.Runtimes {
		if config.IsKubernetesRuntimeEntry(name, rt) && isConfigured(name) {
			kubernetesUse = true
		}
	}
	for name, p := range vs.Profiles {
		if rt, ok := vs.Runtimes[p.Runtime]; ok && config.IsKubernetesRuntimeEntry(p.Runtime, rt) && !config.IsShippedDefaultProfile(name, p) {
			kubernetesUse = true
		}
	}

	settings := config.ScanSettingsPlacement(vs.Runtimes, vs.Profiles)
	settings.AddGlobalSettingsFileRaw()

	agents := config.PlacementPreflightReport{NotEnumerated: []string{legacyPlacementNotEnumerated}}
	policy := func(profile string) (*api.ResolvedPlacement, bool) {
		return config.ResolveLegacyPolicyOffline(vs.Runtimes, vs.Profiles, isConfigured, profile)
	}
	paths := []string{"global"}
	if globalDir, err := config.GetGlobalDir(); err != nil {
		agents.RecordsUnreadable = true
	} else {
		entries, err := os.ReadDir(filepath.Join(globalDir, "projects"))
		switch {
		case err == nil:
			for _, e := range entries {
				if e.IsDir() {
					paths = append(paths, filepath.Join(globalDir, "projects", e.Name()))
				}
			}
		case !os.IsNotExist(err):
			agents.RecordsUnreadable = true
		}
	}
	if root, ok := config.FindProjectRoot(); ok {
		paths = append(paths, root)
	}
	agent.ScanLocalLegacyPlacement(&agents, paths, policy)
	return checkDoctorLegacyPlacementPreflight(settings, agents, kubernetesUse), true
}

// checkDoctorLegacyPlacementPreflight reports the settings and agent scans
// as one line of counts (never names or values), always with the scan's
// coverage limits (the sources it cannot list). It warns when the settings
// need attention; when the agent scan has a finding, incomplete or
// unreadable records, or agents it does not cover; or when it cannot list a
// source and the host shows Kubernetes use (kubernetesUse from the
// settings, or a Kubernetes agent found). A host with no Kubernetes use
// passes, still naming what it could not list, so the line never claims
// complete coverage.
func checkDoctorLegacyPlacementPreflight(settings config.SettingsPlacementReport, agents config.PlacementPreflightReport, kubernetesUse bool) scionruntime.CheckResult {
	res := scionruntime.CheckResult{Name: "legacy-placement-preflight", Status: "pass",
		Message: "Kubernetes placement on this host (settings file; agents in the global, hub-managed and current projects): settings: " +
			settings.Summary() + " | agents: " + agents.Summary()}
	findings := agents.Incomplete > 0 || agents.RecordsUnreadable || agents.NotCovered > 0
	for _, n := range agents.Counts {
		if n > 0 {
			findings = true
		}
	}
	kubernetesUse = kubernetesUse || agents.Scanned > 0
	if settings.NeedsAttention() || findings || (len(agents.NotEnumerated) > 0 && kubernetesUse) {
		res.Status = "warn"
		res.Remediation = legacyPlacementPreflightRemedy
	}
	return res
}
