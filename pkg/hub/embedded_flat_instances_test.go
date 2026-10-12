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
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEmbeddedFlatInstances_SeveralCoLocated: several co-located flat
// instances are all embedded; waiters are released only once every expected
// instance has an outcome; one refused instance degrades the co-located
// health check even though its siblings activated (ptone/scion#3272).
func TestEmbeddedFlatInstances_SeveralCoLocated(t *testing.T) {
	s := &Server{}
	s.ExpectEmbeddedBroker()
	s.ExpectEmbeddedFlatInstances([]string{"docker-a", "docker-b", "docker-c"})

	s.mu.Lock()
	s.recordEmbeddedFlatActivatedLocked("docker-a", "id-a")
	s.mu.Unlock()
	assert.True(t, s.embeddedBrokerSnapshot().pending, "still pending while other instances have no outcome")

	s.mu.Lock()
	s.recordEmbeddedFlatActivatedLocked("docker-b", "id-b")
	s.mu.Unlock()
	s.EmbeddedFlatInstanceFailed("docker-c", errors.New("runtime_target_binding_conflict (embedded): refused"))

	state := s.waitForEmbeddedBroker(context.Background())
	assert.False(t, state.pending, "released once every instance has an outcome")
	assert.True(t, s.isEmbeddedBroker("id-a"))
	assert.True(t, s.isEmbeddedBroker("id-b"))
	assert.False(t, s.isEmbeddedBroker("id-c"))
	assert.False(t, s.isEmbeddedBroker(""))
	assert.True(t, state.has("id-b"))
	assert.Empty(t, s.GetEmbeddedBrokerID(), "no single embedded broker ID with several instances")
	assert.Equal(t, []string{"id-a", "id-b"}, s.GetEmbeddedBrokerIDs())
	assert.True(t, s.hasEmbeddedBroker())
	require.Contains(t, state.regErr, `instance "docker-c"`)
	assert.Contains(t, state.regErr, "runtime_target_binding_conflict")

	checks := map[string]string{}
	s.checkColocatedBrokerHealth(checks)
	assert.Equal(t, "unhealthy: registration failed", checks["colocated_broker"], "a refused sibling is visible")
}

// TestEmbeddedFlatInstances_SingleInstanceKeepsP1Semantics: one co-located
// instance is the embedded broker exactly as in P1.
func TestEmbeddedFlatInstances_SingleInstanceKeepsP1Semantics(t *testing.T) {
	s := &Server{}
	s.ExpectEmbeddedBroker()
	s.mu.Lock()
	s.recordEmbeddedFlatActivatedLocked("local-docker", "id-1")
	s.mu.Unlock()

	state := s.embeddedBrokerSnapshot()
	assert.False(t, state.pending)
	assert.Equal(t, "id-1", s.GetEmbeddedBrokerID())
	checks := map[string]string{}
	s.checkColocatedBrokerHealth(checks)
	assert.Equal(t, "healthy", checks["colocated_broker"])

	f := &Server{}
	f.ExpectEmbeddedBroker()
	err := errors.New("experiment_disabled: refused")
	f.EmbeddedFlatInstanceFailed("local-docker", err)
	assert.Equal(t, err.Error(), f.embeddedBrokerSnapshot().regErr, "a single refusal is reported verbatim")
	assert.False(t, f.embeddedBrokerSnapshot().pending)
}

// TestEmbeddedFlatInstances_FailureAfterActivationIsNotEmbedded: an
// instance recorded as embedded and then reported failed (its services did
// not start) is no longer embedded, and health shows the failure.
func TestEmbeddedFlatInstances_FailureAfterActivationIsNotEmbedded(t *testing.T) {
	s := &Server{}
	s.mu.Lock()
	s.recordEmbeddedFlatActivatedLocked("docker-a", "id-a")
	s.mu.Unlock()
	require.True(t, s.isEmbeddedBroker("id-a"))

	s.EmbeddedFlatInstanceFailed("docker-a", errors.New("services failed to start"))
	assert.False(t, s.isEmbeddedBroker("id-a"))
	assert.Empty(t, s.GetEmbeddedBrokerIDs())
	checks := map[string]string{}
	s.checkColocatedBrokerHealth(checks)
	assert.Equal(t, "unhealthy: registration failed", checks["colocated_broker"])
}
