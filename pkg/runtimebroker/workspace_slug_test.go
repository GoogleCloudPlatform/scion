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

package runtimebroker

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A slug is reserved host-wide: instances sharing a project directory never
// both hold one (project, slug), which names one agent directory.

func slugInstances(t *testing.T) (a, b *partitionInstance) {
	t.Helper()
	setupTestScionEnv(t)
	d := &sharedDaemon{}
	locks := NewWorkspaceLocks()
	return newPartitionInstanceWithLocks(t, d, "docker-a", t.TempDir(), locks),
		newPartitionInstanceWithLocks(t, d, "docker-b", t.TempDir(), locks)
}

// TestWorkspaceLocks_SlugHeldByAnotherInstanceIsRefused: while instance A
// holds (proj-1, worker), live or deleting, instance B cannot begin a run of
// another agent with that slug in proj-1; the same slug in another project
// is free. Once A's delete finishes and releases the slug, B can take it,
// and then A cannot.
func TestWorkspaceLocks_SlugHeldByAnotherInstanceIsRefused(t *testing.T) {
	a, b := slugInstances(t)
	require.NoError(t, a.srv.beginOwnedRun("proj-1", "agent-a1", "worker", "run-a1", true))

	err := b.srv.beginOwnedRun("proj-1", "agent-b1", "worker", "run-b1", true)
	require.ErrorIs(t, err, ErrOwnershipSlugReservedElsewhere)
	assert.NotErrorIs(t, err, ErrOwnershipSlugPending)
	assert.Contains(t, err.Error(), a.identity.RuntimeBrokerID)
	_, ok, gerr := b.srv.ownership.Get("proj-1", "agent-b1")
	require.NoError(t, gerr)
	assert.False(t, ok, "a refused run records nothing")

	require.NoError(t, b.srv.beginOwnedRun("proj-2", "agent-b2", "worker", "run-b2", true), "the same slug in another project is free")

	require.NoError(t, a.srv.ownership.SetRecordState("proj-1", "agent-a1", OwnershipStateDeleting))
	err = b.srv.beginOwnedRun("proj-1", "agent-b1", "worker", "run-b1", true)
	require.ErrorIs(t, err, ErrOwnershipSlugReservedElsewhere)
	require.ErrorIs(t, err, ErrOwnershipSlugPending, "a deleting agent still holds its slug")

	require.NoError(t, a.srv.ownership.SetRecordState("proj-1", "agent-a1", OwnershipStateDeleted))
	require.NoError(t, a.srv.ownership.ReleaseSlug("proj-1", "agent-a1"))
	require.NoError(t, b.srv.beginOwnedRun("proj-1", "agent-b1", "worker", "run-b1", true))
	err = a.srv.beginOwnedRun("proj-1", "agent-a2", "worker", "run-a2", true)
	require.ErrorIs(t, err, ErrOwnershipSlugReservedElsewhere)
	assert.Contains(t, err.Error(), b.identity.RuntimeBrokerID)
}

// TestWorkspaceLocks_ConcurrentSlugReservationsOneWins: two instances
// beginning runs of different agents with the same slug in one project at
// the same moment never both succeed.
func TestWorkspaceLocks_ConcurrentSlugReservationsOneWins(t *testing.T) {
	a, b := slugInstances(t)
	for i := 0; i < 50; i++ {
		slug := fmt.Sprintf("worker-%d", i)
		var wg sync.WaitGroup
		errs := make([]error, 2)
		start := make(chan struct{})
		for k, p := range []*partitionInstance{a, b} {
			wg.Add(1)
			go func(k int, p *partitionInstance) {
				defer wg.Done()
				<-start
				errs[k] = p.srv.beginOwnedRun("proj-1", fmt.Sprintf("agent-%s-%d", p.key, i), slug, fmt.Sprintf("run-%s-%d", p.key, i), true)
			}(k, p)
		}
		close(start)
		wg.Wait()
		won := 0
		for _, err := range errs {
			if err == nil {
				won++
			} else if !errors.Is(err, ErrOwnershipSlugReservedElsewhere) {
				t.Fatalf("round %d: unexpected error: %v", i, err)
			}
		}
		if won != 1 {
			t.Fatalf("round %d: %d instances reserved slug %s, want exactly 1 (errors %v)", i, won, slug, errs)
		}
	}
}

// TestWorkspaceLocks_CreateOfASlugAnotherInstanceHoldsIsAConflict: a create
// on instance B of a slug instance A holds in the same project is refused
// with 409 naming the slug and A, before any agent file is written.
func TestWorkspaceLocks_CreateOfASlugAnotherInstanceHoldsIsAConflict(t *testing.T) {
	a, b := slugInstances(t)
	require.NoError(t, a.srv.ownership.BeginRun(flatTestProjectID, "agent-a1", "flat-agent", "run-a1"))
	w := serveFlat(b.srv, http.MethodPost, "/api/v1/agents", flatCreateBody("req-b", "flat-agent",
		map[string]interface{}{"expectedRuntimeTargetId": b.identity.RuntimeTarget.ID}))
	require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	for _, want := range []string{"flat-agent", a.identity.RuntimeBrokerID} {
		assert.True(t, strings.Contains(w.Body.String(), want), "conflict message %q lacks %q", w.Body.String(), want)
	}
	_, ok, err := b.srv.ownership.Get(flatTestProjectID, "agent-id-flat-agent")
	require.NoError(t, err)
	assert.False(t, ok)

	// Pending: A's delete of the slug is unfinished.
	require.NoError(t, a.srv.ownership.SetRecordState(flatTestProjectID, "agent-a1", OwnershipStateDeleting))
	w = serveFlat(b.srv, http.MethodPost, "/api/v1/agents", flatCreateBody("req-b2", "flat-agent",
		map[string]interface{}{"expectedRuntimeTargetId": b.identity.RuntimeTarget.ID}))
	require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	for _, want := range []string{"flat-agent", a.identity.RuntimeBrokerID, "not yet confirmed"} {
		assert.True(t, strings.Contains(w.Body.String(), want), "conflict message %q lacks %q", w.Body.String(), want)
	}
}

// TestWorkspaceLocks_UnreadableReservationCountsAsHeld: when another
// instance's reservation cannot be read, the slug counts as held.
func TestWorkspaceLocks_UnreadableReservationCountsAsHeld(t *testing.T) {
	locks := NewWorkspaceLocks()
	self, other := &Server{}, &Server{}
	locks.registerWorkspaceUser(other, &workspaceUser{instance: "rb-other",
		slugReservation: func(string, string) (string, bool, error) { return "", false, errors.New("unreadable") }})
	called := false
	err := locks.reserveSlug(self, "proj-1", "worker", func() error { called = true; return nil })
	require.ErrorIs(t, err, ErrOwnershipSlugReservedElsewhere)
	assert.False(t, called)
	locks.registerWorkspaceUser(other, nil)
	require.NoError(t, locks.reserveSlug(self, "proj-1", "worker", func() error { called = true; return nil }))
	assert.True(t, called)
}
