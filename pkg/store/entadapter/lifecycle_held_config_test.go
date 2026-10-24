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

package entadapter

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/lifecycleevents"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	tc "github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetryemit"
)

// Held config edits (upstream #4088) write through
// store.AgentStore.UpdateAgentConfig, which the WP1 upmerge routes through
// the lifecycle observer (tele-em ruling, Option A). The observed outcome:
// a change to a snapshot field (applied_config) is metadata_changed, exactly
// once, after a successful compare-and-set and commit; a pending_config-only
// write, a no-op write, a failed compare-and-set and a rolled-back write
// emit nothing and leave the cursor (the observer's accepted snapshot)
// unchanged; never configured; no resynced(unobserved_change).

const heldSecret = "s3cr3t-held-value"

// heldState is the agent's committed lifecycle state: its outbox rows, its
// cursor (the accepted snapshot) and the enqueue count.
type heldState struct {
	rows                   int
	transitionRev, metaRev int64
	metaDigest, fp         string
	enqueued               int64
}

func heldStateOf(t *testing.T, f *lifecycleFixture, id string) heldState {
	t.Helper()
	_, rows := lifecycleEvents(t, f.cs, id)
	c, err := f.cs.client.AgentLifecycleCursor.Get(context.Background(), uuid.MustParse(id))
	require.NoError(t, err)
	st := heldState{rows: len(rows), transitionRev: c.TransitionRevision, metaRev: c.MetadataRevision,
		metaDigest: c.MetadataDigest, fp: c.LastFingerprint}
	for _, field := range strings.Fields(f.counters.String()) {
		if k, v, ok := strings.Cut(field, "="); ok && strings.HasPrefix(k, telemetryemit.MetricEventsEnqueued+"{") {
			n, err := strconv.ParseInt(v, 10, 64)
			require.NoError(t, err)
			st.enqueued += n
		}
	}
	return st
}

func newHeldFixture(t *testing.T, slug string) (*lifecycleFixture, *store.Agent) {
	t.Helper()
	f := newLifecycleFixture(t)
	a := f.create(slug, func(a *store.Agent) {
		a.Phase = "stopped"
		a.Labels = map[string]string{"team": "core"}
		a.AppliedConfig = &store.AgentAppliedConfig{
			Harness: "claude", Model: "m-a",
			Env: map[string]string{"API_TOKEN": heldSecret},
		}
	})
	return f, a
}

func heldPending(t *testing.T, model string) *store.AgentPendingConfig {
	t.Helper()
	raw, err := json.Marshal(model)
	require.NoError(t, err)
	secret, err := json.Marshal(map[string]string{"API_TOKEN": heldSecret + "-pending"})
	require.NoError(t, err)
	return &store.AgentPendingConfig{Config: map[string]json.RawMessage{"model": raw, "env": secret}, SavedBy: "user:u1"}
}

func heldApplied(model string) *store.AgentAppliedConfig {
	return &store.AgentAppliedConfig{Harness: "claude", Model: model, Env: map[string]string{"API_TOKEN": heldSecret}}
}

func heldVersion(t *testing.T, f *lifecycleFixture, id string) int64 {
	t.Helper()
	got, err := f.cs.GetAgent(context.Background(), id)
	require.NoError(t, err)
	return got.StateVersion
}

func countTransitions(evs []*tc.LifecycleEvent, tr string) int {
	n := 0
	for _, e := range evs {
		if e.Transition == tr {
			n++
		}
	}
	return n
}

// TestLifecycleHeldConfig_TwoStepFoldEmitsOnce is the explicit two-step
// held-edit path: the pending-only save is silent; the application at
// start emits metadata_changed exactly once with the applied model; no
// configured, no resynced; a later observed write gives no resync either.
func TestLifecycleHeldConfig_TwoStepFoldEmitsOnce(t *testing.T) {
	f, a := newHeldFixture(t, "held-fold")
	ctx := context.Background()
	before := heldStateOf(t, f, a.ID)

	// Step 1: save the held edit (pending only).
	_, err := f.cs.UpdateAgentConfig(ctx, a.ID, heldVersion(t, f, a.ID), heldApplied("m-a"), heldPending(t, "m-b"))
	require.NoError(t, err)
	assert.Equal(t, before, heldStateOf(t, f, a.ID), "a pending-only write is silent and leaves the accepted snapshot")

	// Step 2: apply it at start (fold into applied_config, clear pending).
	_, err = f.cs.UpdateAgentConfig(ctx, a.ID, heldVersion(t, f, a.ID), heldApplied("m-b"), nil)
	require.NoError(t, err)
	after := heldStateOf(t, f, a.ID)
	assert.Equal(t, before.rows+1, after.rows, "exactly one event")
	assert.Equal(t, before.enqueued+1, after.enqueued, "counted once, after commit")
	evs := f.events(a.ID)
	require.Equal(t, 1, countTransitions(evs, tc.TransitionMetadataChanged), lcFull(evs))
	assert.Zero(t, countTransitions(evs, tc.TransitionConfigured), "never configured: %s", lcFull(evs))
	assert.Zero(t, countTransitions(evs, tc.TransitionResynced), "no resynced: %s", lcFull(evs))
	last := evs[len(evs)-1]
	require.Equal(t, tc.TransitionMetadataChanged, last.Transition)
	require.NotNil(t, last.Metadata)
	assert.Equal(t, "m-b", last.Metadata.RequestedModel)
	assert.Equal(t, "claude", last.Metadata.Harness)

	// A later observed write finds the accepted snapshot current.
	f.update(ctx, a.ID, func(a *store.Agent) { a.Labels = map[string]string{"team": "core", "x": "y"} })
	evs = f.events(a.ID)
	assert.Zero(t, countTransitions(evs, tc.TransitionResynced), "no resynced(unobserved_change) on this path: %s", lcFull(evs))
}

// TestLifecycleHeldConfig_UnobservedControl: a truly unobserved write of
// applied_config (bypassing the store method) still produces
// resynced(unobserved_change) at the next observed write, so the test
// above would see a missing observation.
func TestLifecycleHeldConfig_UnobservedControl(t *testing.T) {
	f, a := newHeldFixture(t, "held-control")
	ctx := context.Background()
	raw, err := json.Marshal(heldApplied("m-b"))
	require.NoError(t, err)
	require.NoError(t, f.cs.client.Agent.UpdateOneID(uuid.MustParse(a.ID)).SetAppliedConfig(string(raw)).Exec(ctx))
	f.update(ctx, a.ID, func(a *store.Agent) { a.Labels = map[string]string{"team": "core", "x": "y"} })
	evs := f.events(a.ID)
	require.Equal(t, 1, countTransitions(evs, tc.TransitionResynced), lcFull(evs))
	for _, e := range evs {
		if e.Transition == tc.TransitionResynced {
			assert.Equal(t, tc.CauseUnobservedChange, e.Cause)
		}
	}
}

// TestLifecycleHeldConfig_DiscardIsSilent: discarding the pending edits
// (agent_pending_config.go: UpdateAgentConfig with the applied config
// unchanged and no pending) emits nothing: pending_config is not a snapshot
// field and applied_config did not change, so the metadata digest and the
// fingerprint are unchanged.
func TestLifecycleHeldConfig_DiscardIsSilent(t *testing.T) {
	f, a := newHeldFixture(t, "held-discard")
	ctx := context.Background()
	_, err := f.cs.UpdateAgentConfig(ctx, a.ID, heldVersion(t, f, a.ID), heldApplied("m-a"), heldPending(t, "m-b"))
	require.NoError(t, err)
	before := heldStateOf(t, f, a.ID)
	_, err = f.cs.UpdateAgentConfig(ctx, a.ID, heldVersion(t, f, a.ID), heldApplied("m-a"), nil)
	require.NoError(t, err)
	assert.Equal(t, before, heldStateOf(t, f, a.ID))
	got, err := f.cs.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.True(t, got.PendingConfig.IsEmpty(), "the discard itself was written")
}

// TestLifecycleHeldConfig_NoOpWriteIsSilent: a write that changes no
// snapshot field (same applied config, no pending) emits nothing and
// leaves the accepted snapshot.
func TestLifecycleHeldConfig_NoOpWriteIsSilent(t *testing.T) {
	f, a := newHeldFixture(t, "held-noop")
	before := heldStateOf(t, f, a.ID)
	_, err := f.cs.UpdateAgentConfig(context.Background(), a.ID, heldVersion(t, f, a.ID), heldApplied("m-a"), nil)
	require.NoError(t, err)
	assert.Equal(t, before, heldStateOf(t, f, a.ID))
}

// TestLifecycleHeldConfig_FailedCASIsSilent: a stale expected version
// writes nothing, emits nothing and leaves the accepted snapshot.
func TestLifecycleHeldConfig_FailedCASIsSilent(t *testing.T) {
	f, a := newHeldFixture(t, "held-cas")
	before := heldStateOf(t, f, a.ID)
	v := heldVersion(t, f, a.ID)
	_, err := f.cs.UpdateAgentConfig(context.Background(), a.ID, v-1, heldApplied("m-b"), nil)
	require.ErrorIs(t, err, store.ErrVersionConflict)
	assert.Equal(t, before, heldStateOf(t, f, a.ID))
	got, err := f.cs.GetAgent(context.Background(), a.ID)
	require.NoError(t, err)
	assert.Equal(t, "m-a", got.AppliedConfig.Model)
	assert.Equal(t, v, got.StateVersion)
}

// TestLifecycleHeldConfig_ObserverErrorRollsBack: an observer failure
// rolls the configuration write back with it: nothing written, nothing
// emitted, the accepted snapshot unchanged.
func TestLifecycleHeldConfig_ObserverErrorRollsBack(t *testing.T) {
	f, a := newHeldFixture(t, "held-obserr")
	before := heldStateOf(t, f, a.ID)
	v := heldVersion(t, f, a.ID)
	errObs := errors.New("injected observer failure")
	f.cs.SetAgentWriteObserver(failingObserver{err: errObs})
	_, err := f.cs.UpdateAgentConfig(context.Background(), a.ID, v, heldApplied("m-b"), nil)
	f.cs.SetAgentWriteObserver(f.obs)
	require.ErrorIs(t, err, errObs)
	assert.Equal(t, before, heldStateOf(t, f, a.ID))
	got, err := f.cs.GetAgent(context.Background(), a.ID)
	require.NoError(t, err)
	assert.Equal(t, "m-a", got.AppliedConfig.Model)
	assert.Equal(t, v, got.StateVersion)
}

// TestLifecycleHeldConfig_PayloadCarriesNoRawConfig: no raw config or
// secret value reaches an event payload through this trigger: neither the
// applied env, nor the pending edit's values, nor its saver.
func TestLifecycleHeldConfig_PayloadCarriesNoRawConfig(t *testing.T) {
	f, a := newHeldFixture(t, "held-redact")
	ctx := context.Background()
	_, err := f.cs.UpdateAgentConfig(ctx, a.ID, heldVersion(t, f, a.ID), heldApplied("m-a"), heldPending(t, "m-b"))
	require.NoError(t, err)
	_, err = f.cs.UpdateAgentConfig(ctx, a.ID, heldVersion(t, f, a.ID), heldApplied("m-b"), nil)
	require.NoError(t, err)
	_, rows := lifecycleEvents(t, f.cs, a.ID)
	require.NotEmpty(t, rows)
	for _, r := range rows {
		p := string(r.Payload)
		for _, banned := range []string{heldSecret, "API_TOKEN", "user:u1", `"env"`, "pending"} {
			assert.NotContains(t, p, banned, "event %d carries %q", r.TransitionRevision, banned)
		}
	}
}

// TestLifecycleHeldConfig_MarkedContextNeverConfigured: "never configured"
// is structural (upmerge2 r1 O1). A caller context carrying a
// config-resolution mark or a live sync dispatch result (one naming the
// agent's current run, which the observer would otherwise treat as config
// resolution; upmerge2 r2 R2-1) does not make the fold a config-resolution
// write: it is still exactly one metadata_changed.
func TestLifecycleHeldConfig_MarkedContextNeverConfigured(t *testing.T) {
	const run = "run-held"
	live := lifecycleevents.DispatchResult{RunID: run, BrokerEvidence: true}
	for _, tt := range []struct {
		name string
		mark func(context.Context) context.Context
	}{
		{"WithConfigResolution", lifecycleevents.WithConfigResolution},
		{"WithDispatchResult for the current run", func(ctx context.Context) context.Context {
			return lifecycleevents.WithDispatchResult(ctx, live)
		}},
		{"both", func(ctx context.Context) context.Context {
			return lifecycleevents.WithDispatchResult(lifecycleevents.WithConfigResolution(ctx), live)
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f, a := newHeldFixture(t, "held-marked")
			f.mint(a.ID, run) // the dispatch result names the agent's current run
			ctx := tt.mark(context.Background())
			before := heldStateOf(t, f, a.ID)
			_, err := f.cs.UpdateAgentConfig(ctx, a.ID, heldVersion(t, f, a.ID), heldApplied("m-b"), nil)
			require.NoError(t, err)
			assert.Equal(t, before.rows+1, heldStateOf(t, f, a.ID).rows, "exactly one event")
			evs := f.events(a.ID)
			assert.Zero(t, countTransitions(evs, tc.TransitionConfigured), "a marked context must not make it configured: %s", lcFull(evs))
			assert.Equal(t, 1, countTransitions(evs, tc.TransitionMetadataChanged), lcFull(evs))
		})
	}
}
