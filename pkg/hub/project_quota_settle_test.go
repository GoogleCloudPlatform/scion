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

//go:build !no_sqlite

package hub

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// The project reservation of an agent whose stop is queued for an offline
// broker follows the broker one (ptone/scion#6033): it is kept while the
// stop is only queued (the container may still run), released when the
// queued stop is applied or settled, and kept for a start that claims the
// agent in between.

// A stop_queued agent holds capacity: the sweep keeps its project row,
// however old.
func TestProjectQuota_QueuedStopKeepsSlotUntilApplied(t *testing.T) {
	sf := newSettleFixture(t, "pq-qs-keep")
	seedProjectRowAt(t, sf.s, sf.agent, time.Now().Add(-2*reconcileMinReservationAge))
	sf.srv.ReconcileStaleProjectQuotaReservations(context.Background())
	assert.True(t, hasProjectRow(t, sf.s, sf.agent.ID), "a stop_queued agent keeps its project slot")
}

// The drain applying the queued stop releases the project row with the
// broker row (reconcile.go, the queued-stop result).
func TestProjectQuota_QueuedStopAppliedByDrainFreesSlot(t *testing.T) {
	sf := newSettleFixture(t, "pq-qs-drain")
	seedProjectRow(t, sf.s, sf.agent)
	sf.drain()
	assert.Nil(t, sf.held(t), "the broker row is released")
	assert.False(t, hasProjectRow(t, sf.s, sf.agent.ID), "the project row is released with it")
}

// The heartbeat settle of the queued stop releases the project row in its
// own conditional write beside the broker one.
func TestProjectQuota_QueuedStopSettleFreesSlot(t *testing.T) {
	sf := newSettleFixture(t, "pq-qs-settle")
	seedProjectRow(t, sf.s, sf.agent)
	sf.fault.Arm()
	sf.settle()
	assert.Nil(t, sf.held(t))
	assert.False(t, hasProjectRow(t, sf.s, sf.agent.ID))
}

// A start claim written before the settle's release keeps the project row:
// the queued-stop condition no longer holds on the locked row.
func TestProjectQuota_QueuedStopSettle_StartClaimKeepsSlot(t *testing.T) {
	sf := newSettleFixture(t, "pq-qs-claim")
	seedProjectRow(t, sf.s, sf.agent)
	sf.hook.beforeRelease = func() {
		_, err := sf.s.ClaimAgentStart(context.Background(), sf.agent.ID, "other-hub", store.StartClaimUser, "", time.Minute)
		require.NoError(t, err)
	}
	sf.fault.Arm()
	sf.settle()
	require.Nil(t, sf.hook.beforeRelease, "the claim was injected before the release")
	assert.True(t, hasProjectRow(t, sf.s, sf.agent.ID), "the start keeps its project slot")
}

// The settle's two backstop re-asserts restore a project row as
// they restore the broker row: after a failed re-read, and after a start
// superseded the queued stop during the release.
func TestProjectQuota_SettleBackstopsRestoreRow(t *testing.T) {
	t.Run("reread-failed", func(t *testing.T) {
		sf := newSettleFixture(t, "pq-qs-reread")
		seedProjectRow(t, sf.s, sf.agent)
		sf.hook.afterRelease = func() { sf.hook.failNextGetAgent.Store(true) }
		sf.fault.Arm()
		sf.settle()
		require.Nil(t, sf.hook.afterRelease, "the settle released")
		require.False(t, sf.hook.failNextGetAgent.Load(), "the settle's re-read failed")
		assert.True(t, hasProjectRow(t, sf.s, sf.agent.ID), "the project row is put back")
		audits := capacityAudits(t, sf.s, auditQuotaProjectBackstopReassert, sf.agent.ID)
		require.Len(t, audits, 1)
		assert.Contains(t, audits[0].AfterSummary, backstopSiteSettle)
	})
	t.Run("superseded-during-release", func(t *testing.T) {
		sf := newSettleFixture(t, "pq-qs-revert")
		seedProjectRow(t, sf.s, sf.agent)
		require.NotNil(t, sf.agent.RunIntentAt)
		sf.hook.afterRelease = func() {
			reverted, err := sf.s.RevertRunIntent(context.Background(), sf.agent.ID, store.RunIntentStopped, *sf.agent.RunIntentAt, store.RunIntentRunning)
			require.NoError(t, err)
			require.True(t, reverted)
			// An unguarded path frees the project row as well.
			sf.srv.quotaService.Release(context.Background(), store.LimitMaxAgentsPerProject, sf.agent.ID)
		}
		sf.fault.Arm()
		sf.settle()
		require.Nil(t, sf.hook.afterRelease)
		assert.True(t, hasProjectRow(t, sf.s, sf.agent.ID), "the project row is put back")
		audits := capacityAudits(t, sf.s, auditQuotaProjectBackstopReassert, sf.agent.ID)
		require.Len(t, audits, 1)
		assert.Contains(t, audits[0].AfterSummary, backstopSiteSettle)
	})
}
