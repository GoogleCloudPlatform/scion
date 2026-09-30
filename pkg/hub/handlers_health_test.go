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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCheckColocatedBrokerHealth_NotExpected covers a purely distributed Hub
// that never calls ExpectEmbeddedBroker: there is no co-located broker to
// check, so no key is added (same "no-op when not configured" shape as
// checkWorkspaceStorageHealth).
func TestCheckColocatedBrokerHealth_NotExpected(t *testing.T) {
	srv := &Server{}
	checks := make(map[string]string)
	srv.checkColocatedBrokerHealth(checks)
	_, ok := checks["colocated_broker"]
	assert.False(t, ok, "expected no colocated_broker check when no embedded broker is expected")
}

// TestCheckColocatedBrokerHealth_Registered covers the happy path: the
// co-located broker registered successfully.
func TestCheckColocatedBrokerHealth_Registered(t *testing.T) {
	srv := &Server{}
	srv.ExpectEmbeddedBroker()
	srv.SetEmbeddedBrokerID(tid("broker-1"))

	checks := make(map[string]string)
	srv.checkColocatedBrokerHealth(checks)
	assert.Equal(t, "healthy", checks["colocated_broker"])
}

// TestCheckColocatedBrokerHealth_RegistrationFailed is the ptone/scion#2154
// regression case: a co-located broker that failed to register (e.g. a
// non-UUID broker_id rejected by the store) must surface as unhealthy
// instead of being silently absent from the checks map.
func TestCheckColocatedBrokerHealth_RegistrationFailed(t *testing.T) {
	srv := &Server{}
	srv.ExpectEmbeddedBroker()
	srv.EmbeddedBrokerRegistrationFailed(errors.New(`invalid UUID "not-a-broker-id"`))

	checks := make(map[string]string)
	srv.checkColocatedBrokerHealth(checks)
	// Fixed string, not the raw store/driver error: /healthz is
	// unauthenticated, and regErr can carry backend detail (table/constraint
	// names, values). The full error goes to the server log instead (see
	// EmbeddedBrokerRegistrationFailed's callers).
	assert.Equal(t, "unhealthy: registration failed", checks["colocated_broker"])
	assert.NotContains(t, checks["colocated_broker"], "invalid UUID",
		"the raw registration error must not reach the public health check value")
}

// TestCheckColocatedBrokerHealth_Pending covers the narrow startup race
// window: ExpectEmbeddedBroker has run but neither SetEmbeddedBrokerID nor
// EmbeddedBrokerRegistrationFailed has resolved it yet. The snapshot must
// not block (unlike waitForEmbeddedBroker) since /healthz is polled
// frequently (often with short client timeouts) and must not stall on this
// window.
func TestCheckColocatedBrokerHealth_Pending(t *testing.T) {
	srv := &Server{}
	srv.ExpectEmbeddedBroker()

	checks := make(map[string]string)
	srv.checkColocatedBrokerHealth(checks)
	assert.Contains(t, checks["colocated_broker"], "unhealthy")
	assert.Contains(t, checks["colocated_broker"], "pending")
}

// TestGetHealthInfo_DegradedWhenColocatedBrokerRegistrationFailed is the
// end-to-end regression test for ptone/scion#2154: previously, a Hub with a
// co-located broker that failed to register still reported top-level status
// "healthy" (with zero connected brokers) via GetHealthInfo/handleHealthz,
// and the health summary derived from it. The first symptom an operator saw
// was agent creation failing 422 (no runtime broker available). Now the
// status must go "degraded" with a reason as soon as registration fails.
func TestGetHealthInfo_DegradedWhenColocatedBrokerRegistrationFailed(t *testing.T) {
	srv, _ := testServer(t)

	// Sanity: healthy before anything embedded-broker related happens.
	info := srv.GetHealthInfo(context.Background())
	require.Equal(t, "healthy", info.Status)
	_, ok := info.Checks["colocated_broker"]
	assert.False(t, ok)

	srv.ExpectEmbeddedBroker()
	srv.EmbeddedBrokerRegistrationFailed(errors.New(`invalid UUID "not-a-broker-id"`))

	info = srv.GetHealthInfo(context.Background())
	assert.Equal(t, "degraded", info.Status)
	assert.Equal(t, "unhealthy: registration failed", info.Checks["colocated_broker"])
	assert.NotContains(t, info.Checks["colocated_broker"], "invalid UUID",
		"the raw registration error must not reach the public health check value")
}
