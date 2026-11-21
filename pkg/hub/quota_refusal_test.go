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
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestQuotaRefusal_MaxAgentsPerBroker(t *testing.T) {
	t.Run("with broker scope id", func(t *testing.T) {
		msg, details := quotaRefusal(store.LimitMaxAgentsPerBroker, &QuotaExceededError{
			Limit:     store.LimitMaxAgentsPerBroker,
			ScopeType: store.QuotaScopeBroker,
			ScopeID:   "broker-123",
			Count:     4,
			Max:       4,
		})
		assert.Equal(t,
			"quota exceeded: max_agents_per_broker (broker broker-123: 4 of 4 active agents (starting, running, stopping, or stop-queued)); stop, suspend, or delete an active agent on this broker to free a slot",
			msg)
		assert.Equal(t, map[string]interface{}{
			"runtimeBrokerId": "broker-123",
			"count":           int64(4),
			"limit":           int64(4),
		}, details)
	})

	t.Run("without broker scope id", func(t *testing.T) {
		msg, details := quotaRefusal(store.LimitMaxAgentsPerBroker, &QuotaExceededError{
			Limit:     store.LimitMaxAgentsPerBroker,
			ScopeType: store.QuotaScopeBroker,
			Count:     2,
			Max:       2,
		})
		assert.Equal(t,
			"quota exceeded: max_agents_per_broker (2 of 2 active agents (starting, running, stopping, or stop-queued)); stop, suspend, or delete an active agent on this broker to free a slot",
			msg)
		assert.Equal(t, map[string]interface{}{
			"count": int64(2),
			"limit": int64(2),
		}, details)
	})

	t.Run("unpopulated count and max omits 0 of 0", func(t *testing.T) {
		msg, details := quotaRefusal(store.LimitMaxAgentsPerBroker, &QuotaExceededError{
			Limit:     store.LimitMaxAgentsPerBroker,
			ScopeType: store.QuotaScopeBroker,
			ScopeID:   "broker-123",
		})
		assert.Equal(t, "quota exceeded: max_agents_per_broker", msg)
		assert.Equal(t, map[string]interface{}{
			"runtimeBrokerId": "broker-123",
		}, details)
	})

	t.Run("sentinel error fallback", func(t *testing.T) {
		msg, details := quotaRefusal(store.LimitMaxAgentsPerBroker, store.ErrQuotaExceeded)
		assert.Equal(t, "quota exceeded: max_agents_per_broker", msg)
		assert.Nil(t, details)
	})

	t.Run("apiclient surfaces full 429 message", func(t *testing.T) {
		rec := httptest.NewRecorder()
		writeQuotaReserveError(rec, store.LimitMaxAgentsPerBroker, &QuotaExceededError{
			Limit:     store.LimitMaxAgentsPerBroker,
			ScopeType: store.QuotaScopeBroker,
			ScopeID:   "broker-xyz",
			Count:     3,
			Max:       3,
		})
		require.Equal(t, http.StatusTooManyRequests, rec.Code)

		apiErr := apiclient.ParseErrorResponse(rec.Result())
		require.NotNil(t, apiErr)
		assert.Equal(t, http.StatusTooManyRequests, apiErr.StatusCode)
		assert.Equal(t, ErrCodeQuotaExceeded, apiErr.Code)
		assert.Contains(t, apiErr.Error(), "broker broker-xyz: 3 of 3 active agents (starting, running, stopping, or stop-queued)")
		assert.Contains(t, apiErr.Error(), "stop, suspend, or delete an active agent on this broker to free a slot")
	})
}

func TestBrokerQuotaCountedPhase_ExcludesStoppedAndSuspended(t *testing.T) {
	excluded := []state.Phase{
		state.PhaseStopped,
		state.PhaseSuspended,
		state.PhaseError,
		state.PhaseHalted,
	}
	for _, p := range excluded {
		assert.False(t, isBrokerQuotaCountedPhase(string(p)), fmt.Sprintf("phase %q must not count toward max_agents_per_broker", p))
	}

	counted := []state.Phase{
		state.PhaseCreated,
		state.PhaseProvisioning,
		state.PhaseCloning,
		state.PhaseStarting,
		state.PhaseRunning,
		state.PhaseStopping,
	}
	for _, p := range counted {
		assert.True(t, isBrokerQuotaCountedPhase(string(p)), fmt.Sprintf("phase %q must count toward max_agents_per_broker", p))
	}

	assert.True(t, store.AgentHoldsBrokerCapacity(&store.Agent{
		Phase:           string(state.PhaseStopped),
		ContainerStatus: store.ContainerStatusStopQueued,
	}), "stop_queued agent must still hold broker capacity")
}
