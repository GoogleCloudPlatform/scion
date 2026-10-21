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
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/router"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
)

// backstopRows is the per-row renewal seam of the PTY backstop test: row
// A (the launched agent's live session) renews and row B does not. Once
// armed, the first decision on A evicts it, as an agent reconnecting
// elsewhere does: it inserts B, a session of the same agent on another
// relay, with the next connection epoch, so A is no longer eligible and the
// open's resolution lands on B.
type backstopRows struct {
	t   *testing.T
	reg *registry.Registry

	mu      sync.Mutex
	armed   bool
	evicted bool
	seen    []string // session ids, in decision order
}

const backstopRowB = "s-backstop-b"

func (b *backstopRows) canRenew(res router.Resolved) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seen = append(b.seen, res.Record.SessionID)
	if res.Record.SessionID == backstopRowB {
		return false
	}
	if b.armed && !b.evicted {
		b.evicted = true
		b.evict(res.Record)
	}
	return res.Session != nil && res.Legacy == nil
}

// evict registers relay-b and inserts row B for a's principal with the next
// epoch. It runs inside the hub's decision, so a failure is reported
// without stopping the hub's goroutine.
func (b *backstopRows) evict(a registry.SessionRecord) {
	ctx := context.Background()
	gen, err := b.reg.RegisterRelay(ctx, registry.RelayInstance{InstanceID: "relay-b", InternalEndpoint: "http://relay-b.invalid"})
	if err != nil {
		b.t.Errorf("registering relay-b: %v", err)
		return
	}
	now := time.Now()
	rec := a
	rec.SessionID, rec.RelayInstanceID, rec.RelayGeneration = backstopRowB, "relay-b", gen
	rec.ConnectedAt, rec.LastSeen, rec.Draining = now, now, false
	if _, err := b.reg.InsertSessionWithNextEpoch(ctx, rec); err != nil {
		b.t.Errorf("inserting row B: %v", err)
	}
}

func (b *backstopRows) arm() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.armed = true
}

func (b *backstopRows) decisions() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.seen...)
}

// TestPTYRenewalBackstop_ReResolutionToUnrenewableRow is a BACKSTOP test
// (design §3.3.1 4504 row, v2.21.42: stream_renewal_unavailable is reached
// only if routes disagree). Under consistent routes (6.6b-3b-1 onward,
// conduitRouteCanRenew) the pre-upgrade check refuses first, so the
// disagreement is made through the conduitRouteCanRenewTest seam: with
// enforce, the preflight and the WebSocket open's pre-upgrade check land
// on row A, which renews; the open's re-resolution then lands on row B,
// which does not. The WebSocket is upgraded, the mint's backstop refuses
// B, and the client gets 4504 stream_renewal_unavailable (retry with
// backoff). No grant is minted, nothing is spawned, and the refusal is
// counted once, by the mint, with the route cause.
func TestPTYRenewalBackstop_ReResolutionToUnrenewableRow(t *testing.T) {
	o := &openSetup{enforce: true}
	rows := &backstopRows{t: t}
	f := newPTYConduitFixtureServer(t, func(srv *Server) {
		o.mod(srv)
		srv.conduitRouteCanRenewTest = rows.canRenew
	})
	rows.reg = f.reg
	f.setBrokerRow(t, attachCaps(false), "")
	f.startPTYAgent(t, f.public.URL)
	o.keys.arm(f.srv)

	// The preflight lands on row A, which renews: the agent path.
	status, path, _ := f.preflight(t)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, string(ptyPathAgent), path)
	decided := rows.decisions()
	require.NotEmpty(t, decided)
	rowA := decided[0]
	require.NotEqual(t, backstopRowB, rowA)

	// The open's pre-upgrade check lands on A too and evicts it; the
	// open's resolution lands on B.
	rows.arm()
	c, _, err := f.dialPTY(t, "")
	require.NoError(t, err, "the pre-upgrade check passed on row A: the WebSocket is upgraded")
	t.Cleanup(func() { _ = c.Close() })
	ce := readUntilClose(t, c)
	assert.Equal(t, wsprotocol.ClosePTYUpstreamTimeout, ce.Code, "4504")
	assert.Equal(t, conduitReasonStreamRenewalUnavailable, ce.Text)

	got := rows.decisions()
	require.GreaterOrEqual(t, len(got), 3)
	assert.Equal(t, backstopRowB, got[len(got)-1], "the last decision, the mint's, is on B")
	for _, id := range got[:len(got)-1] {
		assert.Equal(t, rowA, id, "every decision before the mint (preflight, pre-upgrade check) is on A")
	}
	assert.Zero(t, o.keys.reads.Load(), "no grant was minted")
	assert.Empty(t, f.spawned, "nothing was spawned")
	assert.Equal(t, []string{conduitRoutePTY + "/" + conduitReasonStreamRenewalUnavailable + "/" + conduitCauseRenewalRouteUnavailable}, o.rec.list(),
		"refused once, by the mint's backstop")
}

// TestPTYRenewalBackstop_RowBAdvertisesRenewal pins the precondition of the
// backstop test: row B is a stream_renewal.v2 row, so its refusal comes
// from the route (canRenew), not from a target without renewal (which is
// minted unstamped and never refused here).
func TestPTYRenewalBackstop_RowBAdvertisesRenewal(t *testing.T) {
	f := newPTYConduitFixture(t)
	f.startPTYAgent(t, f.public.URL)
	rows := &backstopRows{t: t, reg: f.reg, armed: true}
	res, err := f.srv.conduitRouter().Resolve(context.Background(), agentPTYRequest(f.launched), nil)
	require.NoError(t, err)
	require.True(t, conduitTargetRenews(res), "row A advertises stream_renewal.v2")
	require.True(t, rows.canRenew(res), "row A renews")

	res, err = f.srv.conduitRouter().Resolve(context.Background(), agentPTYRequest(f.launched), nil)
	require.NoError(t, err)
	assert.Equal(t, backstopRowB, res.Record.SessionID, "after the eviction the resolution lands on B")
	assert.True(t, res.Record.Capabilities.HasFeature(conduit.FeatureStreamRenewal), "row B advertises stream_renewal.v2")
	assert.False(t, rows.canRenew(res), "row B does not renew")
}
