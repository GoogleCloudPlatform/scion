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

import (
	"sort"
	"strings"
)

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

// Values a stored or displayed BrokerHealthReport may hold. The broker's
// own checks can carry free text (for example a mount error with the NFS
// server, export and command output); only these fixed values are kept.
const (
	BrokerHealthHealthy     = "healthy"
	BrokerHealthDegraded    = "degraded"
	BrokerHealthUnhealthy   = "unhealthy"
	BrokerHealthAvailable   = "available"
	BrokerHealthUnavailable = "unavailable"
	BrokerHealthUnknown     = "unknown"
)

const (
	// BrokerHealthMaxChecks caps the checks kept from one report. The
	// first BrokerHealthMaxChecks valid names in sorted order are kept.
	BrokerHealthMaxChecks = 16
	// BrokerHealthMaxNameChars caps the length of a check name.
	BrokerHealthMaxNameChars = 64
)

var (
	brokerHealthStatuses = map[string]bool{
		BrokerHealthHealthy:   true,
		BrokerHealthDegraded:  true,
		BrokerHealthUnhealthy: true,
	}
	brokerHealthCheckValues = map[string]bool{
		BrokerHealthHealthy:     true,
		BrokerHealthDegraded:    true,
		BrokerHealthUnhealthy:   true,
		BrokerHealthAvailable:   true,
		BrokerHealthUnavailable: true,
		BrokerHealthUnknown:     true,
	}
)

// NormalizeBrokerHealthReport returns a bounded copy of r that holds only
// fixed values, or nil when r is nil:
//
//   - Status is kept when it is healthy, degraded or unhealthy; anything
//     else becomes unknown.
//   - Each check value is reduced to its leading word (so
//     "unhealthy: <share>: <error>" becomes "unhealthy") and kept when that
//     whole word is one of the fixed check values; anything else, including
//     a fixed word run on into other characters ("healthy_x"), becomes
//     unknown.
//     No other text from the broker is kept.
//   - A check name must be 1-64 characters of [A-Za-z0-9_.-]; any other
//     name is dropped. At most BrokerHealthMaxChecks checks are kept, the
//     first in sorted name order, so the result is the same for the same
//     input on every call.
//   - An empty check map becomes nil.
//
// The broker applies it before sending and the hub again before storing,
// so the hub never relies on the broker having done so.
func NormalizeBrokerHealthReport(r *BrokerHealthReport) *BrokerHealthReport {
	if r == nil {
		return nil
	}
	out := &BrokerHealthReport{Status: leadingHealthWord(r.Status, brokerHealthStatuses)}
	names := make([]string, 0, len(r.Checks))
	for name := range r.Checks {
		if validBrokerHealthCheckName(name) {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return out
	}
	sort.Strings(names)
	if len(names) > BrokerHealthMaxChecks {
		names = names[:BrokerHealthMaxChecks]
	}
	out.Checks = make(map[string]string, len(names))
	for _, name := range names {
		out.Checks[name] = leadingHealthWord(r.Checks[name], brokerHealthCheckValues)
	}
	return out
}

// leadingHealthWord returns the lower-cased leading word of v when it is in
// allowed and is a whole word: it ends v, or is followed by whitespace,
// ':', ';', ',' or '(' (the start of a cause, which is discarded).
// Anything else, such as "healthy_x" or "available-not", is unknown.
func leadingHealthWord(v string, allowed map[string]bool) string {
	v = strings.TrimSpace(v)
	end := 0
	for end < len(v) && end <= len(BrokerHealthUnavailable) {
		c := v[end]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
			break
		}
		end++
	}
	if end < len(v) {
		switch v[end] {
		case ' ', '\t', '\n', '\r', ':', ';', ',', '(':
		default:
			return BrokerHealthUnknown
		}
	}
	if w := strings.ToLower(v[:end]); allowed[w] {
		return w
	}
	return BrokerHealthUnknown
}

// validBrokerHealthCheckName reports whether name is 1-64 characters of
// [A-Za-z0-9_.-].
func validBrokerHealthCheckName(name string) bool {
	if name == "" || len(name) > BrokerHealthMaxNameChars {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '.', c == '-':
		default:
			return false
		}
	}
	return true
}
