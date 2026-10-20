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
	"context"
	"log/slog"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Host identity states: the configured GCP host service account compared
// with the identity a co-located server last read from the metadata server.
// The state is derived at read time, so an update to the configured value
// is reflected immediately.
const (
	// HostIdentityUnobserved: no observation recorded (remote brokers).
	HostIdentityUnobserved = "unobserved"
	// HostIdentityUnknown: the metadata server could not be read.
	HostIdentityUnknown = "unknown"
	// HostIdentityUnsetDetected: no configured account; the host reports one.
	HostIdentityUnsetDetected = "unset_detected"
	// HostIdentityMatch: the configured account is the host's account.
	HostIdentityMatch = "match"
	// HostIdentityMismatch: the host reports a different account.
	HostIdentityMismatch = "mismatch"
	// HostIdentityPinnedAbsent: an account is configured, but the host
	// reports none.
	HostIdentityPinnedAbsent = "pinned_absent"
	// HostIdentityNone: no configured account and none on the host.
	HostIdentityNone = "none"
)

// hostIdentityState derives the host identity state of b.
func hostIdentityState(b *store.RuntimeBroker) string {
	obs := b.HostObservation
	if obs == nil || obs.Outcome == "" {
		return HostIdentityUnobserved
	}
	configured := b.GCPHostServiceAccountEmail
	switch obs.Outcome {
	case store.HostObservationUnavailable:
		return HostIdentityUnknown
	case store.HostObservationAbsent:
		if configured == "" {
			return HostIdentityNone
		}
		return HostIdentityPinnedAbsent
	case store.HostObservationPresent:
		switch {
		case configured == "":
			return HostIdentityUnsetDetected
		case strings.EqualFold(configured, obs.Email):
			return HostIdentityMatch
		default:
			return HostIdentityMismatch
		}
	}
	return HostIdentityUnknown
}

// hostIdentityIsProblem reports whether a host identity state needs
// attention.
func hostIdentityIsProblem(state string) bool {
	return state == HostIdentityMismatch || state == HostIdentityPinnedAbsent
}

// withHostIdentity sets b.HostIdentity for display; it stays nil while
// unobserved (every remote broker). The observed account and time are
// included only when detail is true (callers allowed to update the broker).
func withHostIdentity(b store.RuntimeBroker, detail bool) store.RuntimeBroker {
	state := hostIdentityState(&b)
	if state == HostIdentityUnobserved {
		b.HostIdentity = nil
		return b
	}
	st := &store.HostIdentityStatus{State: state}
	if detail && b.HostObservation != nil {
		st.ObservedEmail = b.HostObservation.Email
		if !b.HostObservation.ObservedAt.IsZero() {
			at := b.HostObservation.ObservedAt
			st.ObservedAt = &at
		}
	}
	b.HostIdentity = st
	return b
}

// brokerForDisplayWithCap is brokerForDisplay plus the host identity state,
// with detail for callers whose capabilities include update.
func brokerForDisplayWithCap(b store.RuntimeBroker, caps *Capabilities) store.RuntimeBroker {
	return withHostIdentity(brokerForDisplay(b), caps != nil && capabilityAllows(caps, ActionUpdate))
}

// recordHostObservation is RecordHostObservation on the server's store.
func (s *Server) recordHostObservation(ctx context.Context, brokerID string, obs store.HostIdentityObservation) {
	RecordHostObservation(ctx, s.store, brokerID, obs)
}

// RecordHostObservation stores a co-located server's host identity
// observation for brokerID and logs a warning while the derived state needs
// attention. A failure is logged, not returned. Both co-located
// registrations (legacy and flat) use it.
func RecordHostObservation(ctx context.Context, st store.Store, brokerID string, obs store.HostIdentityObservation) {
	if err := st.RecordRuntimeBrokerHostObservation(ctx, brokerID, obs); err != nil {
		slog.Warn("failed to record the host identity observation", "runtime_broker_id", brokerID, "error", err)
		return
	}
	b, err := st.GetRuntimeBroker(ctx, brokerID)
	if err != nil {
		return
	}
	LogHostIdentityState(b)
}

// LogHostIdentityState logs a warning while b's host identity state needs
// attention, and an info line when the metadata server could not be read.
func LogHostIdentityState(b *store.RuntimeBroker) {
	state := hostIdentityState(b)
	switch state {
	case HostIdentityMismatch:
		slog.Warn("the configured GCP host service account differs from the host's identity; the configured value is used (change it through the Runtime Broker update API)",
			"runtime_broker_id", b.ID, "configured", b.GCPHostServiceAccountEmail, "observed", b.HostObservation.Email)
	case HostIdentityPinnedAbsent:
		slog.Warn("the configured GCP host service account is not present on the host; the configured value is used (change it through the Runtime Broker update API)",
			"runtime_broker_id", b.ID, "configured", b.GCPHostServiceAccountEmail)
	case HostIdentityUnknown:
		slog.Info("the host's GCP identity could not be read from the metadata server", "runtime_broker_id", b.ID)
	}
}
