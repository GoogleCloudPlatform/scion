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
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A failed start of a provisioned agent leaves it at rest with a message:
// phase created, intent stopped.
func TestProvisionedStart_DefiniteFailureLeavesMessageAndRestingIntent(t *testing.T) {
	f, d, _ := newClaimFixture(t)
	a := f.addAgent("provisioned", "created", "")
	_, err := f.s.SetRunIntent(context.Background(), a.ID, store.RunIntentStopped)
	require.NoError(t, err)
	d.start = func(ctx context.Context, cur *store.Agent) error {
		return fmt.Errorf("x: %w", errStartRequestNotSent)
	}
	code, _ := lifecycle(t, f, a.ID, "start")
	require.NotEqual(t, http.StatusOK, code)
	got := getAgent(t, f.s, a.ID)
	assert.Equal(t, "created", got.Phase)
	assert.Equal(t, store.RunIntentStopped, got.RunIntent, "back at rest")
	assert.True(t, strings.HasPrefix(got.Message, "Start failed:"), got.Message)
	assert.Contains(t, got.Message, provisionedRestingNote)
	assert.Empty(t, got.StartClaimID)
}

// fastCreateHold makes the backstop consider rows changed more than a
// moment ago.
func fastCreateHold(srv *Server) {
	c := srv.startClaimSettings()
	c.CreateUnconfirmedHold = time.Millisecond
	srv.startClaimCfg.Store(&c)
}

// The reaper backstop: a provisioned agent left with intent running and no
// claim, whose target a fresh complete inventory shows without a container,
// gets the message and goes back to rest. A provisioned agent at rest, or
// one with a start in flight, is untouched.
func TestProvisionedStart_ReaperBackstop(t *testing.T) {
	f, _, _ := newClaimFixture(t)
	ctx := context.Background()
	fastCreateHold(f.srv)
	left := f.addAgent("left-running", "created", "")
	_, err := f.s.SetRunIntent(ctx, left.ID, store.RunIntentRunning)
	require.NoError(t, err)
	atRest := f.addAgent("at-rest", "created", "")
	_, err = f.s.SetRunIntent(ctx, atRest.ID, store.RunIntentStopped)
	require.NoError(t, err)
	inFlight := f.addAgent("in-flight", "created", "")
	_, err = f.s.SetRunIntent(ctx, inFlight.ID, store.RunIntentRunning)
	require.NoError(t, err)
	time.Sleep(5 * time.Millisecond)

	f.send(brokerHeartbeatRequest{
		Status:         store.BrokerStatusOnline,
		Inventory:      completeInventory(),
		Capabilities:   &store.BrokerCapabilities{StartsInFlight: true},
		StartsInFlight: []brokerStartInFlight{{ProjectID: f.projectID, Slug: inFlight.Slug}},
		Projects:       []brokerProjectHeartbeat{{ProjectID: f.projectID}},
	})
	f.srv.reapStartClaims(ctx)

	got := getAgent(t, f.s, left.ID)
	assert.Equal(t, provisionedStartNotCompletedMessage, got.Message)
	assert.Equal(t, store.RunIntentStopped, got.RunIntent)
	assert.Equal(t, "created", got.Phase)
	got = getAgent(t, f.s, atRest.ID)
	assert.Empty(t, got.Message, "a provisioned agent at rest is untouched")
	got = getAgent(t, f.s, inFlight.ID)
	assert.Empty(t, got.Message, "a start in flight is not judged")
	assert.Equal(t, store.RunIntentRunning, got.RunIntent)
}

// A broker that created the container while the phase write failed: the
// heartbeat moves the row from created to running, and the backstop leaves
// it alone.
func TestProvisionedStart_RunningContainerHealsCreatedRow(t *testing.T) {
	f, _, _ := newClaimFixture(t)
	ctx := context.Background()
	fastCreateHold(f.srv)
	a := f.addAgent("phase-write-lost", "created", "")
	_, err := f.s.SetRunIntent(ctx, a.ID, store.RunIntentRunning)
	require.NoError(t, err)
	f.heartbeat(completeInventory(), a.Slug)
	f.srv.reapStartClaims(ctx)
	got := getAgent(t, f.s, a.ID)
	assert.Equal(t, "running", got.Phase, "the broker report moves created to running")
	assert.Empty(t, got.Message)
	assert.Equal(t, store.RunIntentRunning, got.RunIntent)
}

// failingDeleteStore fails every agent row delete, and every transaction
// (so a create's compensation fails too).
type failingDeleteStore struct {
	store.Store
	deletes *atomic.Int32
}

func (s failingDeleteStore) DeleteAgent(context.Context, string) error {
	s.deletes.Add(1)
	return errors.New("database is locked")
}

func (failingDeleteStore) WithTx(context.Context, func(tx store.Store) error) error {
	return errors.New("database is locked")
}

// A failed create whose compensation fails and whose row then still cannot
// be removed (the delete is retried) is left visibly failed.
func TestProvisionedStart_FailedCreateRowThatCannotBeRemoved(t *testing.T) {
	f, _, _ := newClaimFixture(t)
	a := f.addAgent("orphan", "created", "")
	var deletes atomic.Int32
	f.srv.store = failingDeleteStore{Store: f.s, deletes: &deletes}
	corrID := f.srv.cleanupFailedCreate(context.Background(), createRollback{
		Agent:           a,
		RuntimeBrokerID: f.brokerID,
		Stage:           createStageDispatch,
		Cause:           errors.New("dispatch failed"),
	})
	assert.NotEmpty(t, corrID, "a failed compensation is reported")
	assert.Equal(t, int32(createCleanupDeleteAttempts), deletes.Load(), "the row delete is retried")
	got := getAgent(t, f.s, a.ID)
	assert.Equal(t, "error", got.Phase)
	assert.Equal(t, createRowRemoveFailedMessage, got.Message)
}

// A provisioned agent's start that reached the broker keeps intent running
// and gets no failure message, even when the status write after it fails:
// the agent is starting, and the heartbeat records it running.
func TestProvisionedStart_StartedButStatusWriteFailedKeepsIntent(t *testing.T) {
	f, _, _ := newClaimFixture(t)
	a := f.addAgent("started-unrecorded", "created", "")
	_, err := f.s.SetRunIntent(context.Background(), a.ID, store.RunIntentStopped)
	require.NoError(t, err)
	f.srv.store = failRunningStatusStore{Store: f.s, agentID: a.ID}
	err = f.srv.startAgentCore(context.Background(), getAgent(t, f.s, a.ID), StartOpts{Kind: store.StartClaimUser})
	require.ErrorIs(t, err, errStartedStatusWrite)
	got := getAgent(t, f.s, a.ID)
	assert.Equal(t, store.RunIntentRunning, got.RunIntent, "a start that reached the broker is not reverted")
	assert.Empty(t, got.Message)
}

// Only a start that definitely did not happen sends a provisioned agent
// back to rest. An ambiguous dispatch error, or a launch in flight that may
// still start a container, keeps intent running and leaves no message.
func TestProvisionedStart_UncertainFailureKeepsIntent(t *testing.T) {
	for name, startErr := range map[string]error{
		"ambiguous":        errors.New("connection reset by peer"),
		"launch in flight": fmt.Errorf("x: %w", ErrLaunchInFlight),
	} {
		t.Run(name, func(t *testing.T) {
			f, d, _ := newClaimFixture(t)
			a := f.addAgent("uncertain", "created", "")
			_, err := f.s.SetRunIntent(context.Background(), a.ID, store.RunIntentStopped)
			require.NoError(t, err)
			d.start = func(ctx context.Context, cur *store.Agent) error { return startErr }
			err = f.srv.startAgentCore(context.Background(), getAgent(t, f.s, a.ID), StartOpts{Kind: store.StartClaimUser})
			require.Error(t, err)
			got := getAgent(t, f.s, a.ID)
			assert.Equal(t, store.RunIntentRunning, got.RunIntent)
			assert.Empty(t, got.Message)
		})
	}
}

// A start refused by another start's claim leaves that claimant's intent
// and the row's message alone.
func TestProvisionedStart_ClaimRefusalLeavesTheClaimantsIntent(t *testing.T) {
	f, _, _ := newClaimFixture(t)
	a := f.addAgent("claimed-elsewhere", "created", "")
	claim, err := f.s.ClaimAgentStart(context.Background(), a.ID, "other-hub", store.StartClaimUser, "", time.Minute)
	require.NoError(t, err)
	code, body := lifecycle(t, f, a.ID, "start")
	require.Equal(t, http.StatusConflict, code, body)
	got := getAgent(t, f.s, a.ID)
	assert.Equal(t, store.RunIntentRunning, got.RunIntent)
	require.NotNil(t, got.RunIntentAt)
	assert.True(t, got.RunIntentAt.Equal(claim.RunIntentAt), "the claimant's intent is untouched")
	assert.Empty(t, got.Message)
}

// The settle writes its message only when its intent compare-and-set wins:
// a newer start keeps its intent and gets no message.
func TestProvisionedStart_SettleLeavesANewerStartAlone(t *testing.T) {
	f, _, _ := newClaimFixture(t)
	ctx := context.Background()
	a := f.addAgent("newer-start", "created", "")
	old, err := f.s.SetRunIntent(ctx, a.ID, store.RunIntentRunning)
	require.NoError(t, err)
	time.Sleep(5 * time.Millisecond)
	_, err = f.s.SetRunIntent(ctx, a.ID, store.RunIntentRunning)
	require.NoError(t, err)
	f.srv.settleFailedProvisionedStart(ctx, a.ID, old, provisionedStartNotCompletedMessage)
	got := getAgent(t, f.s, a.ID)
	assert.Equal(t, store.RunIntentRunning, got.RunIntent)
	assert.Empty(t, got.Message, "no message for a start the settle did not revert")
}

// The backstop leaves a provisioned agent alone until the create hold has
// passed since its last change.
func TestProvisionedStart_ReaperBackstopWaitsForTheCreateHold(t *testing.T) {
	f, _, _ := newClaimFixture(t)
	ctx := context.Background()
	young := f.addAgent("young", "created", "")
	_, err := f.s.SetRunIntent(ctx, young.ID, store.RunIntentRunning)
	require.NoError(t, err)
	f.heartbeat(completeInventory())
	f.srv.reapStartClaims(ctx)
	got := getAgent(t, f.s, young.ID)
	assert.Equal(t, store.RunIntentRunning, got.RunIntent)
	assert.Empty(t, got.Message)
}
