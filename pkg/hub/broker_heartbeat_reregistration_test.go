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
	"database/sql"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// reregisterAfterBrokerReadStore commits a broker re-registration (a new
// profile set and endpoint) right after the first armed GetRuntimeBroker of
// brokerID returns, so the caller holds a copy read before the
// re-registration. The server is built around it, never swapped in.
type reregisterAfterBrokerReadStore struct {
	store.Store
	brokerID string
	armed    atomic.Bool
	fired    atomic.Bool
}

// DB forwards to the wrapped store's raw *sql.DB so New()'s D4
// membership-index migration runs against the real store.
func (s *reregisterAfterBrokerReadStore) DB() *sql.DB {
	if p, ok := s.Store.(interface{ DB() *sql.DB }); ok {
		return p.DB()
	}
	return nil
}

func (s *reregisterAfterBrokerReadStore) GetRuntimeBroker(ctx context.Context, id string) (*store.RuntimeBroker, error) {
	b, err := s.Store.GetRuntimeBroker(ctx, id)
	if err != nil || id != s.brokerID || !s.armed.CompareAndSwap(true, false) {
		return b, err
	}
	reg, err := s.Store.GetRuntimeBroker(ctx, id)
	if err != nil {
		return nil, err
	}
	reg.Profiles = []store.BrokerProfile{
		{Name: "reregistered-a", Type: "docker", Available: true},
		{Name: "reregistered-b", Type: "kubernetes", Available: true},
	}
	reg.Endpoint = "https://reregistered.example.test"
	if err := s.Store.UpdateRuntimeBroker(ctx, reg); err != nil {
		return nil, err
	}
	s.fired.Store(true)
	return b, nil
}

// A broker re-registration that commits between the heartbeat's broker read
// and its write keeps its profile set and endpoint; the heartbeat still
// stores the capabilities it reported (ptone/scion#3169).
func TestBrokerHeartbeat_ConcurrentReregistrationSurvives(t *testing.T) {
	ctx := context.Background()
	base, err := newTestStore(t, ":memory:")
	require.NoError(t, err)
	wrapped := &reregisterAfterBrokerReadStore{Store: base}
	srv, _ := testServerOnMigratedStore(t, wrapped)
	grantDevUserRuntimeBrokerAccess(t, base)

	broker := &store.RuntimeBroker{
		ID:       tid("hb-rereg-broker"),
		Name:     "hb-rereg-broker",
		Slug:     "hb-rereg-broker",
		Status:   store.BrokerStatusOnline,
		Endpoint: "https://original.example.test",
		Profiles: []store.BrokerProfile{{Name: "original", Type: "docker", Available: true}},
	}
	require.NoError(t, base.CreateRuntimeBroker(ctx, broker))
	wrapped.brokerID = broker.ID
	wrapped.armed.Store(true)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/runtime-brokers/"+broker.ID+"/heartbeat", brokerHeartbeatRequest{
		Status:       "online",
		Capabilities: &store.BrokerCapabilities{Attach: true, Sync: true},
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.True(t, wrapped.fired.Load(), "the re-registration must land between the heartbeat's read and write")

	got, err := base.GetRuntimeBroker(ctx, broker.ID)
	require.NoError(t, err)
	names := make([]string, 0, len(got.Profiles))
	for _, p := range got.Profiles {
		names = append(names, p.Name)
	}
	assert.Equal(t, []string{"reregistered-a", "reregistered-b"}, names,
		"the heartbeat must not write back the profile set it read before the re-registration")
	assert.Equal(t, "https://reregistered.example.test", got.Endpoint,
		"the heartbeat must not write back a column it does not refresh")
	require.NotNil(t, got.Capabilities)
	assert.True(t, got.Capabilities.Attach, "the heartbeat's capabilities are stored")
	assert.True(t, got.Capabilities.Sync, "the heartbeat's capabilities are stored")
}
