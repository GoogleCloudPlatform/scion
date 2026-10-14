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

package cloudrun

import (
	"log/slog"

	"github.com/GoogleCloudPlatform/scion/pkg/util"
	"github.com/GoogleCloudPlatform/scion/pkg/util/cloudrunenv"
)

// CPUAlwaysAllocatedEnv is the environment variable an operator sets to true
// to assert that the Cloud Run service hosting the broker keeps CPU allocated
// outside requests (deployed with --no-cpu-throttling). It silences the
// startup warning logged by WarnCPUAllocationIfNeeded.
const CPUAlwaysAllocatedEnv = "SCION_CLOUDRUN_CPU_ALWAYS_ALLOCATED"

// CPUAllocationWarning is the startup warning logged on a Cloud Run service
// host that has not asserted always-allocated CPU.
const CPUAllocationWarning = "If this Cloud Run service allocates CPU only during requests, async agent launches will stall after the create response and be reaped. Deploy with --no-cpu-throttling and set SCION_CLOUDRUN_CPU_ALWAYS_ALLOCATED=true to silence this."

// WarnCPUAllocationIfNeeded logs CPUAllocationWarning once on logger when
// cpuAllocationWarningWanted reports that it applies. It reads the
// environment only through getenv and makes no API calls: the CPU allocation
// setting of the service is not detected, only the operator's assertion.
func WarnCPUAllocationIfNeeded(logger *slog.Logger, getenv func(string) string, brokerEnabled bool) {
	if logger == nil || getenv == nil {
		return
	}
	if cpuAllocationWarningWanted(getenv, brokerEnabled) {
		logger.Warn(CPUAllocationWarning)
	}
}

// cpuAllocationWarningWanted reports whether the CPU allocation warning
// applies. All of these must hold:
//   - the broker function is enabled (async launches run in the broker);
//   - K_SERVICE is set (a Cloud Run service, where CPU may be allocated only
//     during requests);
//   - CLOUD_RUN_INSTANCE is unset (Cloud Run Instances are not request-driven
//     and are excluded explicitly);
//   - SCION_CLOUDRUN_CPU_ALWAYS_ALLOCATED is not true.
func cpuAllocationWarningWanted(getenv func(string) string, brokerEnabled bool) bool {
	if !brokerEnabled {
		return false
	}
	if !cloudrunenv.IsServiceFrom(getenv) {
		return false
	}
	if cloudrunenv.IsInstanceFrom(getenv) {
		return false
	}
	// Same spellings as the server's other boolean env vars (util.ParseBool).
	// An unrecognized value asserts nothing, so the warning stays.
	v, ok := util.ParseBool(getenv(CPUAlwaysAllocatedEnv))
	return !ok || !v
}
