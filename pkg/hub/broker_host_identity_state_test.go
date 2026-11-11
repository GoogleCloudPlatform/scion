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
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func TestHostIdentityState_Table(t *testing.T) {
	const x = "host-x@proj.iam.gserviceaccount.com"
	const y = "host-y@proj.iam.gserviceaccount.com"
	obs := func(outcome, email string) *store.HostIdentityObservation {
		return &store.HostIdentityObservation{Outcome: outcome, Email: email}
	}
	for _, tc := range []struct {
		name       string
		configured string
		obs        *store.HostIdentityObservation
		want       string
		problem    bool
	}{
		{"never observed", x, nil, HostIdentityUnobserved, false},
		{"never observed, unset", "", nil, HostIdentityUnobserved, false},
		{"metadata unavailable", x, obs(store.HostObservationUnavailable, ""), HostIdentityUnknown, false},
		{"unset, host reports one", "", obs(store.HostObservationPresent, x), HostIdentityUnsetDetected, false},
		{"match", x, obs(store.HostObservationPresent, x), HostIdentityMatch, false},
		{"match ignores case", x, obs(store.HostObservationPresent, "Host-X@Proj.iam.gserviceaccount.com"), HostIdentityMatch, false},
		{"mismatch", x, obs(store.HostObservationPresent, y), HostIdentityMismatch, true},
		{"configured, host reports none", x, obs(store.HostObservationAbsent, ""), HostIdentityPinnedAbsent, true},
		{"none", "", obs(store.HostObservationAbsent, ""), HostIdentityNone, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &store.RuntimeBroker{GCPHostServiceAccountEmail: tc.configured, HostObservation: tc.obs}
			got := hostIdentityState(b)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.problem, hostIdentityIsProblem(got))
		})
	}
}

func TestHostIdentityState_ProblemRowInSummary(t *testing.T) {
	for _, tc := range []struct {
		state   string
		problem bool
	}{
		{HostIdentityMismatch, true},
		{HostIdentityPinnedAbsent, true},
		{HostIdentityUnknown, false},
		{HostIdentityUnsetDetected, false},
		{HostIdentityMatch, false},
	} {
		row := HealthSummaryBroker{Status: store.BrokerStatusOnline, HostIdentity: &HealthBrokerHostIdentity{State: tc.state}}
		assert.Equal(t, tc.problem, healthSummaryBrokerHasProblem(row), tc.state)
	}
}
