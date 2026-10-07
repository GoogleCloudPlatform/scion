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
	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// brokerHealthValueMaxChars bounds each string of a stored broker health
// report (the status, and every check name and value), so a broker cannot
// grow its row without limit.
const brokerHealthValueMaxChars = 120

// boundBrokerHealthReport returns a copy of a heartbeat health report with
// the status and every check name and value truncated to
// brokerHealthValueMaxChars characters. An empty check map is stored as
// nil, so a report that differs only in that way is not a change.
func boundBrokerHealthReport(r *api.BrokerHealthReport) *api.BrokerHealthReport {
	if r == nil {
		return nil
	}
	out := &api.BrokerHealthReport{Status: truncateChars(r.Status, brokerHealthValueMaxChars)}
	if len(r.Checks) > 0 {
		out.Checks = make(map[string]string, len(r.Checks))
		for k, v := range r.Checks {
			out.Checks[truncateChars(k, brokerHealthValueMaxChars)] = truncateChars(v, brokerHealthValueMaxChars)
		}
	}
	return out
}

// truncateChars returns s cut to at most n characters (runes, not bytes),
// so a multi-byte character is never split.
func truncateChars(s string, n int) string {
	if len(s) <= n {
		return s
	}
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}
