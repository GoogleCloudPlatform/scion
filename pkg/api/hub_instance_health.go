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

// HubInstanceMaxCheckNameChars caps the length of a hub instance check name.
// It matches BrokerHealthMaxNameChars, so every existing hub check name
// (for example workspace_storage_mount_verification, 36 characters) fits.
const HubInstanceMaxCheckNameChars = 64

// NormalizeHubInstanceChecks returns a bounded copy of a hub instance's check
// map for the hub-instance registry, or nil when no check is kept. It uses
// the same rule as broker self-health (see NormalizeBrokerHealthReport):
// each value is reduced to its leading fixed word (healthy, degraded,
// unhealthy, available, unavailable or unknown; anything else becomes
// unknown), and at most BrokerHealthMaxChecks checks are kept, the first in
// sorted name order. Names must match ^[a-z0-9_]{1,64}$; any other name is
// dropped.
func NormalizeHubInstanceChecks(checks map[string]string) map[string]string {
	return normalizeHealthChecks(checks, validHubInstanceCheckName)
}

// validHubInstanceCheckName reports whether name matches ^[a-z0-9_]{1,64}$.
func validHubInstanceCheckName(name string) bool {
	if name == "" || len(name) > HubInstanceMaxCheckNameChars {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '_':
		default:
			return false
		}
	}
	return true
}
