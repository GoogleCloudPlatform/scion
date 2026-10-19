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

package relayrole

import (
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// Scheduling hints (design §3.9 "Scheduling hints"; contracts §3,
// relay.HubTunnelOpen). The hub copies a relay-held tunnel's authz_until
// and authz_interval from the open grant it minted into the tunnel open,
// unsigned. The relay uses them only to time the stream's re-checks; it
// never parses the grant and never trusts the hints for authorization (the
// target enforces the signed values). A tunnel without usable hints is
// re-checked by the sweep alone.

// Reasons a tunnel is scheduled by the sweep alone (HintFallbacks).
const (
	// HintFallbackAbsent: the tunnel open carries no hint (a target
	// without renewal, or an older hub).
	HintFallbackAbsent = "absent"
	// HintFallbackInvalid: only one hint is present, or they are out of
	// range.
	HintFallbackInvalid = "invalid"
)

// hintSkew is the allowance past receipt plus interval for authz_until.
const hintSkew = 30 * time.Second

// hintSchedule reads a tunnel open's scheduling hints, received at
// receipt (taken as the mint time). It returns when the first re-check is
// due and the interval to re-arm with, or the fallback reason when the
// stream is left to the sweep. The hints are usable when both are present,
// the interval is within the stream_authz_max bounds and authz_until is
// after receipt and no later than receipt plus the interval plus the skew.
func hintSchedule(receipt time.Time, untilUnix, intervalS int64) (firstRecheck time.Time, interval time.Duration, fallback string) {
	switch {
	case untilUnix == 0 && intervalS == 0:
		return time.Time{}, 0, HintFallbackAbsent
	case untilUnix == 0 || intervalS == 0:
		return time.Time{}, 0, HintFallbackInvalid
	}
	if intervalS < 0 || intervalS > int64(config.ConduitMaxStreamAuthzMax/time.Second) {
		return time.Time{}, 0, HintFallbackInvalid
	}
	interval = time.Duration(intervalS) * time.Second
	until := time.Unix(untilUnix, 0)
	switch {
	case interval < config.ConduitMinStreamAuthzMax:
		return time.Time{}, 0, HintFallbackInvalid
	case !until.After(receipt):
		return time.Time{}, 0, HintFallbackInvalid
	case until.After(receipt.Add(interval + hintSkew)):
		return time.Time{}, 0, HintFallbackInvalid
	}
	// The first re-check at authz_until minus the part of the interval
	// past RenewalLead: about 80% of the interval after mint.
	return until.Add(-time.Duration(float64(interval) * (1 - RenewalLead))), interval, ""
}

// countHintFallback counts one tunnel scheduled by the sweep alone.
func (s *StreamScheduler) countHintFallback(reason string) {
	switch reason {
	case HintFallbackAbsent:
		s.hintAbsent.Add(1)
	case HintFallbackInvalid:
		s.hintInvalid.Add(1)
	}
}

// HintFallbacks reports how many tunnels were scheduled by the sweep alone
// for reason (HintFallbackAbsent or HintFallbackInvalid).
func (s *StreamScheduler) HintFallbacks(reason string) int64 {
	switch reason {
	case HintFallbackAbsent:
		return s.hintAbsent.Load()
	case HintFallbackInvalid:
		return s.hintInvalid.Load()
	}
	return 0
}
