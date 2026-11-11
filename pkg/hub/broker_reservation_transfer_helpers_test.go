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

// Test-only helpers of the atomic broker reservation transfer
// (ptone/scion#5951). This file has no build constraint: its callers are
// both !no_sqlite and integration tests, and as a *_helpers_test.go file
// hack/hubshard keeps it common, so it compiles in every shard.
// transferBrokerReservationUnchecked moves into broker_quota.go when its
// first production callers land.

import (
	"context"
	"fmt"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// setMoveBrokerQuotaTestHook installs fn as moveBrokerQuotaTestHook and
// returns a func that restores the previous value. For tests only; a test
// that sets it must not call t.Parallel.
func setMoveBrokerQuotaTestHook(fn func(ctx context.Context)) (restore func()) {
	var p *func(ctx context.Context)
	if fn != nil {
		p = &fn
	}
	prev := moveBrokerQuotaTestHook.Swap(p)
	return func() { moveBrokerQuotaTestHook.Store(prev) }
}

// transferBrokerReservationUnchecked moves or records agentID's
// max_agents_per_broker reservation onto toBroker atomically
// (store.TransferBrokerReservation) with Cap 0 and no scope locks: it is
// accounting for a slot the agent already held (rollback, reconcile, sweep
// repair), not an admission, as reassertBrokerReservation is. The returned
// bool (limited) is false, and nothing is written, when no limit definition
// exists.
func (s *Server) transferBrokerReservationUnchecked(ctx context.Context, agentID, toBroker string, o TransferOpts) (store.BrokerReservationTransferResult, bool, error) {
	var res store.BrokerReservationTransferResult
	req, limited, err := brokerTransferRequest(ctx, s.store, agentID, toBroker, o)
	if err != nil || !limited {
		return res, false, err
	}
	res, err = s.store.TransferBrokerReservation(ctx, req)
	if err != nil {
		return res, true, fmt.Errorf("quota: transfer reservation for %q: %w", store.LimitMaxAgentsPerBroker, err)
	}
	return res, true, nil
}
