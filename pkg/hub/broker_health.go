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
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// brokerHealthNormalization counts what api.NormalizeBrokerHealthReport
// discarded from a broker's report, for a log line. It holds counts only,
// never the broker's text.
type brokerHealthNormalization struct {
	// DroppedChecks is the number of checks dropped: invalid names, and
	// valid names past the api.BrokerHealthMaxChecks cap.
	DroppedChecks int
	// UnrecognisedValues is the number of kept checks whose value was not
	// a fixed word and was stored as unknown.
	UnrecognisedValues int
	// UnrecognisedStatus is true when the status was not a fixed word and
	// was stored as unknown.
	UnrecognisedStatus bool
}

// any reports whether normalisation discarded anything.
func (n brokerHealthNormalization) any() bool {
	return n.DroppedChecks > 0 || n.UnrecognisedValues > 0 || n.UnrecognisedStatus
}

// countBrokerHealthNormalization compares a report as the broker sent it
// (in) with its normalised form (out). A value the broker sent as
// "unknown" itself is not counted.
func countBrokerHealthNormalization(in, out *api.BrokerHealthReport) brokerHealthNormalization {
	var n brokerHealthNormalization
	if in == nil || out == nil {
		return n
	}
	sentUnknown := func(v string) bool {
		return strings.EqualFold(strings.TrimSpace(v), api.BrokerHealthUnknown)
	}
	n.UnrecognisedStatus = out.Status == api.BrokerHealthUnknown && !sentUnknown(in.Status)
	n.DroppedChecks = len(in.Checks) - len(out.Checks)
	for name, v := range out.Checks {
		if v == api.BrokerHealthUnknown && !sentUnknown(in.Checks[name]) {
			n.UnrecognisedValues++
		}
	}
	return n
}
