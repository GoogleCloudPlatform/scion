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
	"context"
	"sort"
	"sync"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
)

// Re-check triggers (for logs).
const (
	TriggerSweep    = "sweep"
	TriggerNotify   = "notify"
	TriggerResync   = "resync"
	TriggerInterval = "interval"
)

// Start runs the session and stream re-checks until ctx ends: a sweep every
// RecheckInterval, and with Events a re-check on every revocation event and
// whenever the event connection is (re)established (design §3.9: a missed
// event is covered by the next reconnect or sweep).
func (ro *Role) Start(ctx context.Context) {
	var mu sync.Mutex
	var cur clock.Timer
	var arm func()
	arm = func() {
		mu.Lock()
		defer mu.Unlock()
		if ctx.Err() != nil {
			return
		}
		cur = ro.clk.AfterFunc(ro.cfg.RecheckInterval, func() {
			if ctx.Err() != nil {
				return
			}
			// The next tick is armed first, so a slow sweep does not push
			// the following ones back.
			arm()
			ro.recheckAll(ctx, TriggerSweep)
		})
	}
	arm()
	go func() {
		<-ctx.Done()
		mu.Lock()
		defer mu.Unlock()
		if cur != nil {
			cur.Stop()
		}
	}()
	if ro.cfg.Events == nil {
		return
	}
	events, unsubscribe := ro.cfg.Events.Subscribe(ro.cfg.Subjects...)
	removeListen := ro.cfg.Events.AddOnListen(func() { ro.recheckAll(ctx, TriggerResync) })
	go func() {
		defer unsubscribe()
		defer removeListen()
		for {
			select {
			case <-ctx.Done():
				return
			case _, ok := <-events:
				if !ok {
					return
				}
				ro.recheckAll(ctx, TriggerNotify)
			}
		}
	}()
}

// recheckAll re-checks every held session and every held stream. Runs are
// coalesced: while one runs, later triggers leave at most one run pending,
// which starts when the current one ends (with the latest trigger).
func (ro *Role) recheckAll(ctx context.Context, trigger string) {
	ro.recheckMu.Lock()
	if ro.recheckRunning {
		ro.recheckPending = trigger
		ro.recheckMu.Unlock()
		return
	}
	ro.recheckRunning = true
	ro.recheckMu.Unlock()
	for {
		ro.RecheckSessions(ctx, trigger)
		ro.Streams.RecheckAll(ctx, trigger)
		ro.recheckMu.Lock()
		trigger, ro.recheckPending = ro.recheckPending, ""
		if trigger == "" || ctx.Err() != nil {
			ro.recheckRunning = false
			ro.recheckMu.Unlock()
			return
		}
		ro.recheckMu.Unlock()
	}
}

// RecheckSessions asks the hub to re-check every held session, in batches,
// and acts on each answer: close the session with the hub's code, replace
// its admission token, or (not evaluated) leave it. An unreachable hub
// closes nothing.
func (ro *Role) RecheckSessions(ctx context.Context, trigger string) {
	ro.mu.Lock()
	items := make([]relay.HubSessionRecheckItem, 0, len(ro.sessions))
	for id, h := range ro.sessions {
		items = append(items, relay.HubSessionRecheckItem{SessionID: id, AdmissionToken: h.token})
	}
	ro.mu.Unlock()
	sort.Slice(items, func(i, j int) bool { return items[i].SessionID < items[j].SessionID })
	for start := 0; start < len(items); start += relay.MaxHubRecheckBatch {
		batch := items[start:min(start+relay.MaxHubRecheckBatch, len(items))]
		var resp relay.HubSessionRecheckResponse
		if err := ro.cfg.Hub.Call(ctx, relay.HubSessionRecheckPath, relay.HubSessionRecheckRequest{Items: batch}, &resp); err != nil {
			ro.log.Warn("Conduit relay: session re-check unavailable; nothing closes", "trigger", trigger, "sessions", len(batch), "error", err)
			continue
		}
		for i, res := range resp.Results {
			if i >= len(batch) || res.SessionID != batch[i].SessionID {
				continue
			}
			switch {
			case res.Close:
				ro.log.Info("Conduit relay: session closed by re-check", "trigger", trigger, "session_id", res.SessionID, "code", res.CloseCode, "reason", res.Reason)
				ro.closeSession(res.SessionID, res.CloseCode, closeReasonFor(res.CloseCode))
			case res.OK:
				ro.setToken(res.SessionID, res.AdmissionToken)
			}
		}
	}
}

// closeReasonFor is the close reason token of a hub-ordered close code.
func closeReasonFor(code uint32) string {
	if code == relay.CloseTargetNotFound {
		return relay.ReasonTargetNotFound
	}
	return relay.ReasonUnauthenticated
}
