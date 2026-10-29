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
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	scionruntime "github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/runtimebroker"
)

// flatPlacementPreflightRemedy is the remediation of the flat placement
// pre-upgrade scan: the migration steps, in short.
const flatPlacementPreflightRemedy = "Agents counted here are refused at their next start or restart on a flat instance. " +
	"For agent tolerations with no runtime target tolerations, add the intended tolerations to the instance's runtime_target " +
	"or remove them from the agent's configuration; for an agent runtime class with no runtime target class, set " +
	"runtime_class_name on the runtime_target or remove the agent's class. Resolve incomplete coverage (unreadable records " +
	"or configurations) first. Restart the Runtime Broker to activate runtime_target changes, and run scion doctor again."

// checkDoctorFlatPlacementPreflightHere runs the flat placement pre-upgrade
// scan for every flat instance in this host's global settings. ok is false
// when the host has no flat instances, so legacy hosts get no line.
func checkDoctorFlatPlacementPreflightHere() (scionruntime.CheckResult, bool) {
	instances, err := config.LoadRuntimeBrokerInstances("")
	if err != nil {
		return scionruntime.CheckResult{Name: "flat-placement-preflight", Status: "warn",
			Message:     "Could not load server.broker.instances to scan agent placement; coverage incomplete",
			Remediation: "Fix server.broker.instances (scion config validate), then run scion doctor again"}, true
	}
	if len(instances) == 0 {
		return scionruntime.CheckResult{}, false
	}
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		return scionruntime.CheckResult{Name: "flat-placement-preflight", Status: "warn",
			Message: "Could not resolve the global directory to scan agent placement; coverage incomplete"}, true
	}
	return checkDoctorFlatPlacementPreflight(instances, func(inst config.V1RuntimeBrokerInstanceConfig) config.PlacementPreflightReport {
		return runtimebroker.ScanFlatPlacementPreflight(globalDir, inst)
	}), true
}

// checkDoctorFlatPlacementPreflight reports scan's result for each flat
// instance, one "instance <key>: <counts>" part each (counts only, never
// agent names or configuration values). It warns when any instance has an
// agent in a category or incomplete coverage, and passes otherwise.
func checkDoctorFlatPlacementPreflight(instances []config.V1RuntimeBrokerInstanceConfig, scan func(config.V1RuntimeBrokerInstanceConfig) config.PlacementPreflightReport) scionruntime.CheckResult {
	parts := make([]string, 0, len(instances))
	attention := false
	for _, inst := range instances {
		r := scan(inst)
		if r.NeedsAttention() {
			attention = true
		}
		parts = append(parts, "instance "+inst.Key+": "+r.Summary())
	}
	res := scionruntime.CheckResult{Name: "flat-placement-preflight", Status: "pass",
		Message: "Flat instance agent placement (this release checks flat instances): " + strings.Join(parts, " | ")}
	if attention {
		res.Status = "warn"
		res.Remediation = flatPlacementPreflightRemedy
	}
	return res
}
