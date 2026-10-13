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

package runtime

import "time"

// The runtime broker's async launch durations. pkg/runtimebroker uses
// these constants in place of literals, so a runtime's cleanup horizon
// (AsyncCleanupHorizon) cannot drift from the broker's timing.
const (
	// AsyncLaunchAdmissionSkew is how far the runtime's ctx deadline
	// precedes the launch's deadline (the broker's launch admission).
	AsyncLaunchAdmissionSkew = 20 * time.Second
	// AsyncLaunchReportTTL is how long after the launch deadline a failed
	// launch keeps retrying its report to the hub (the terminal TTL).
	AsyncLaunchReportTTL = 10 * time.Minute
	// AsyncLaunchCleanupBudget bounds the cleanup of a failed launch's
	// resources, which runs after the report.
	AsyncLaunchCleanupBudget = 60 * time.Second
	// AsyncCleanupSlack covers cleanup's lock waits and handle loop.
	AsyncCleanupSlack = 60 * time.Second
)

// The terms of AsyncCleanupHorizon. Variables, defaulting to the
// constants above, so tests can shorten them; production never changes
// them.
var (
	asyncLaunchAdmissionSkew = AsyncLaunchAdmissionSkew
	asyncLaunchReportTTL     = AsyncLaunchReportTTL
	asyncLaunchCleanupBudget = AsyncLaunchCleanupBudget
	asyncCleanupSlack        = AsyncCleanupSlack
)

// AsyncCleanupHorizon returns the time by which the broker has finished
// with an async launch whose runtime ctx has deadline ctxDeadline: the
// launch deadline (ctxDeadline + the admission skew), then the failed
// report's retry window, then the cleanup budget, plus slack. A runtime
// must keep the record of a resource it may have created until then, so
// that the broker's cleanup can still find it.
func AsyncCleanupHorizon(ctxDeadline time.Time) time.Time {
	return ctxDeadline.
		Add(asyncLaunchAdmissionSkew).
		Add(asyncLaunchReportTTL).
		Add(asyncLaunchCleanupBudget).
		Add(asyncCleanupSlack)
}
