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
	"sort"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// heartbeatWorkloadIdentityTargets returns, sorted by profile name, one
// Workload Identity target per Kubernetes profile (by resolved runtime
// type) whose runtime entry sets workload_identity_pool or
// create_service_accounts in the broker's global settings, for the
// heartbeat's WorkloadIdentityTargets field (ptone/scion#3329 phase 5,
// ptone/scion#7966). The namespace is the one a dispatch on the profile
// resolves for the Workload Identity principal (resolveAssignNamespace),
// the same as the profile SA report.
//
// Both settings are read from the settings the profile SA report reads
// (global settings plus the hub settings overlay on a co-located server;
// design amendment A-1), never from a project's settings. A pool that fails
// ValidateWorkloadIdentityPool is cleared with a rate-limited warning (the
// target is still emitted with an empty pool when create_service_accounts
// is true, so the hub precheck knows KSA creation is enabled).
//
// With ForceRuntime set, dispatch ignores the profiles and runs on the
// forced runtime, so no target is reported (an empty list). It returns nil
// when the settings cannot be read, so the hub keeps what it has.
func (s *Server) heartbeatWorkloadIdentityTargets() []hubclient.WorkloadIdentityTarget {
	vs, err := s.loadHeartbeatMappingSettings()
	if err != nil || vs == nil {
		return nil
	}
	out := []hubclient.WorkloadIdentityTarget{}
	if s.config.ForceRuntime != "" {
		return out
	}
	return workloadIdentityTargetsFrom(vs, func(profile, pool string) {
		s.saDiscovery().warnRateLimited("wipool\x00"+profile+"\x00"+pool,
			"workload_identity_pool is not a Workload Identity pool (<cluster-project-id>.svc.id.goog); the profile is left out of the heartbeat Workload Identity targets",
			"profile", profile, "workload_identity_pool", pool)
	})
}

// workloadIdentityTargetsFrom builds the targets of vs (see
// heartbeatWorkloadIdentityTargets), calling invalid for each profile whose
// pool is malformed. The result is never nil.
func workloadIdentityTargetsFrom(vs *config.VersionedSettings, invalid func(profile, pool string)) []hubclient.WorkloadIdentityTarget {
	out := []hubclient.WorkloadIdentityTarget{}
	names := make([]string, 0, len(vs.Profiles))
	for name := range vs.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if _, isKubernetes, _ := vs.ProfileKubernetesSAMappings(name); !isKubernetes {
			continue
		}
		entry := vs.Profiles[name].Runtime
		rt := vs.Runtimes[entry]
		ksaCreation := rt.CreateServiceAccounts
		pool := strings.TrimSpace(rt.WorkloadIdentityPool)
		if pool != "" && config.ValidateWorkloadIdentityPool(pool) != nil {
			if invalid != nil {
				invalid(name, pool)
			}
			pool = ""
		}
		if pool == "" && !ksaCreation {
			continue
		}
		out = append(out, hubclient.WorkloadIdentityTarget{
			Profile:     name,
			Pool:        pool,
			Namespace:   resolveAssignNamespace(vs, entry),
			KSACreation: ksaCreation,
		})
	}
	return out
}
