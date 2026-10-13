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
	"sort"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// The health summary's hub section describes the whole fleet of hub
// instances (health dashboard F3 design §5.6, §5.8). It is built from the
// hub_instances rows only, like the hub_instances section, so every
// replica returns the same hub section: the serving replica runs no probe
// of its own (no store Ping, no plugin call, no pool read).

const (
	// HubVersionMixed is hub.version when the live instances report
	// different versions.
	HubVersionMixed = "mixed"
	// HubStatusUnknown is hub.status when the hub-instance registry could
	// not be read, so the fleet status cannot be computed.
	HubStatusUnknown = "unknown"
	// hubInstanceStoppedReportingWindow is how long after its last write a
	// stale instance keeps its "stopped reporting" attention item.
	hubInstanceStoppedReportingWindow = 15 * time.Minute
)

// HealthSummaryHubFleet counts the live hub instances by their last
// reported status. Stale and stopped instances are not counted.
type HealthSummaryHubFleet struct {
	Live      int `json:"live"`
	Healthy   int `json:"healthy"`
	Degraded  int `json:"degraded"`
	Unhealthy int `json:"unhealthy"`
}

// HealthSummaryHubCheck is one non-healthy check reported by one live hub
// instance.
type HealthSummaryHubCheck struct {
	InstanceID    string `json:"instance_id"`
	InstanceLabel string `json:"instance_label"`
	// Name is the check name, e.g. database.
	Name string `json:"name"`
	// Value is the check's reported value; fixed words only (see
	// api.NormalizeHubInstanceChecks).
	Value string `json:"value"`
}

// fleetHubStatus applies the fleet rule over the live instances (n live,
// u unhealthy, d degraded):
//   - n = 0: unhealthy (no hub instance is reporting);
//   - 2u > n: unhealthy (more than half of the live instances unhealthy);
//   - u > 0 or d > 0: degraded;
//   - otherwise healthy.
//
// So one unhealthy instance out of three is degraded, two out of three is
// unhealthy, and a single unhealthy instance is unhealthy.
func fleetHubStatus(f HealthSummaryHubFleet) string {
	switch {
	case f.Live == 0:
		return HealthStatusUnhealthy
	case f.Unhealthy*2 > f.Live:
		return HealthStatusUnhealthy
	case f.Unhealthy > 0 || f.Degraded > 0:
		return HealthStatusDegraded
	default:
		return HealthStatusHealthy
	}
}

// hubInstanceDisplayLabel is the name a message uses for a hub instance.
func hubInstanceDisplayLabel(label, id string) string {
	if label != "" {
		return label
	}
	return id
}

// buildHealthSummaryFleetHub builds the fleet part of the hub section from
// every listed registry row (before the hub_instances item cap), against
// the store clock now. ok is false when the registry could not be read:
// the status is then unknown, Instances is nil and no check is listed. A
// pure function of its inputs; the store counts and the serving marker are
// set by the caller.
func buildHealthSummaryFleetHub(rows []store.HubInstance, now time.Time, ok bool) HealthSummaryHub {
	hub := HealthSummaryHub{UnhealthyChecks: []HealthSummaryHubCheck{}}
	if !ok {
		hub.Status = HubStatusUnknown
		return hub
	}
	fleet := HealthSummaryHubFleet{}
	versions := map[string]bool{}
	for _, r := range rows {
		if hubInstanceState(r, now) != HubInstanceStateLive {
			continue
		}
		fleet.Live++
		versions[r.Version] = true
		switch healthStatusRank(r.Status) {
		case 0:
			fleet.Healthy++
		case 2:
			fleet.Unhealthy++
		default:
			fleet.Degraded++
		}
		label := hubInstanceDisplayLabel(r.Label, r.ID)
		// Normalised again: the row may have been written by another
		// version of the hub, and check names reach attention messages.
		for name, value := range api.NormalizeHubInstanceChecks(r.Checks) {
			if value == HealthStatusHealthy {
				continue
			}
			hub.UnhealthyChecks = append(hub.UnhealthyChecks, HealthSummaryHubCheck{
				InstanceID: r.ID, InstanceLabel: label, Name: name, Value: value,
			})
		}
	}
	sortHealthSummaryHubChecks(hub.UnhealthyChecks)
	hub.Instances = &fleet
	hub.Status = fleetHubStatus(fleet)
	switch len(versions) {
	case 0:
	case 1:
		for v := range versions {
			hub.Version = v
		}
	default:
		hub.Version = HubVersionMixed
	}
	return hub
}

// sortHealthSummaryHubChecks orders checks critical first
// (criticalHealthChecks), then by check name, instance label and instance
// ID, so the order does not depend on which replica built the summary.
func sortHealthSummaryHubChecks(checks []HealthSummaryHubCheck) {
	sort.Slice(checks, func(i, j int) bool {
		a, b := checks[i], checks[j]
		if ca, cb := criticalHealthChecks[a.Name], criticalHealthChecks[b.Name]; ca != cb {
			return ca
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		if a.InstanceLabel != b.InstanceLabel {
			return a.InstanceLabel < b.InstanceLabel
		}
		return a.InstanceID < b.InstanceID
	})
}
