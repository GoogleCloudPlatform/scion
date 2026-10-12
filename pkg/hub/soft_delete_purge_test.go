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
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
)

// purgeCountingStore records the purge calls the soft-delete-purge job
// makes. Every other store method is unused here.
type purgeCountingStore struct {
	store.Store
	mu           sync.Mutex
	agentCutoffs []time.Time
	eventCalls   int
}

func (p *purgeCountingStore) PurgeDeletedAgents(_ context.Context, cutoff time.Time) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.agentCutoffs = append(p.agentCutoffs, cutoff)
	return 0, nil
}

func (p *purgeCountingStore) PurgeOldScheduledEvents(_ context.Context, _ time.Time) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.eventCalls++
	return 0, nil
}

func (p *purgeCountingStore) counts() (agents, events int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.agentCutoffs), p.eventCalls
}

// The scheduled-event purge keeps its startup gate: a hub started with no
// soft-delete retention does not purge old scheduled events, even after a
// retention is saved; a hub started with one does (ptone/scion#3904).
func TestPurgeHandler_ScheduledEventPurgeKeepsStartupGate(t *testing.T) {
	st := &purgeCountingStore{}
	srv := &Server{store: st}
	srv.recordStartupLayer1Values() // startup retention 0
	ApplySnapshot(srv, Layer1Snapshot{SoftDeleteRetention: "1h"})

	srv.purgeHandler()(context.Background())
	agents, events := st.counts()
	assert.Equal(t, 1, agents, "the saved retention runs the agent purge")
	assert.Equal(t, 0, events, "no scheduled-event purge on a hub started with retention 0")

	st2 := &purgeCountingStore{}
	srv2 := &Server{store: st2}
	srv2.config.SoftDeleteRetention = 72 * time.Hour
	srv2.recordStartupLayer1Values()
	srv2.purgeHandler()(context.Background())
	agents, events = st2.counts()
	assert.Equal(t, 1, agents)
	assert.Equal(t, 1, events, "a hub started with a retention purges old scheduled events")
}

// A retention saved as 0 skips the agent purge, so agents soft-deleted
// under an earlier retention stay, as on a hub started with retention 0.
func TestPurgeHandler_ZeroRetentionSkipsAgentPurge(t *testing.T) {
	st := &purgeCountingStore{}
	srv := &Server{store: st}
	srv.config.SoftDeleteRetention = 72 * time.Hour
	srv.recordStartupLayer1Values()

	ApplySnapshot(srv, Layer1Snapshot{SoftDeleteRetention: "0s"})
	retention, _ := srv.softDeleteSettings()
	assert.Zero(t, retention, "a saved 0 applies")

	srv.purgeHandler()(context.Background())
	agents, _ := st.counts()
	assert.Equal(t, 0, agents, "retention 0 does not purge soft-deleted agents")
}
