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

package api

// BrokerHealthReport is a runtime broker's report of its own health, sent
// on every heartbeat and stored by the hub on the broker record. It is the
// broker's view of itself (for example, its default runtime failed to
// start). It is separate from the broker's liveness status (online or
// offline), which only the hub decides from heartbeats; a degraded broker
// stays online and keeps being reconciled.
//
// A nil report means the broker did not send one (an older broker), which
// is shown as "not reported", never as healthy.
type BrokerHealthReport struct {
	// Status is the broker's overall health: "healthy", "degraded" or
	// "unhealthy".
	Status string `json:"status"`
	// Checks maps each health check the broker ran to its result, for
	// example {"docker": "available"} or {"runtime": "unavailable",
	// "nfs_mounts": "healthy"}.
	Checks map[string]string `json:"checks,omitempty"`
}
