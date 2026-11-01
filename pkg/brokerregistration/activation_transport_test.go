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

package brokerregistration

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// routeAllDialsTo makes http.DefaultTransport (which the hub client uses
// when no transport is set) connect every dial to addr, whatever host the
// URL names. A request to a non-loopback URL then reaches the fake hub, so
// a missing transport check shows up as a recorded request rather than as
// a dial error.
func routeAllDialsTo(t *testing.T, addr string) {
	t.Helper()
	orig := http.DefaultTransport
	tr := orig.(*http.Transport).Clone()
	tr.Proxy = nil
	tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}
	http.DefaultTransport = tr
	t.Cleanup(func() { http.DefaultTransport = orig })
}

func TestValidateActivation_PlaintextNonLoopbackHubRefusedBeforeClient(t *testing.T) {
	for _, endpoint := range []string{
		"http://10.0.0.1:9810",
		"http://hub.example.com",
		"ws://hub.example.com",
		"http://[2001:db8::1]:9810",
		"http://user:hunter2-secret@10.0.0.1:9810",
	} {
		t.Run(endpoint, func(t *testing.T) {
			ti := newTestInstance(t)
			hub := newFakeReplica(t)
			hub.get = func(string) (int, any) { return http.StatusOK, selfRow(ti, &ti.id.RuntimeTarget) }
			routeAllDialsTo(t, hub.Listener.Addr().String())
			creds := savedInstanceCredentials(t, ti, hub.URL)
			creds.HubEndpoint = endpoint

			err := ValidateActivation(context.Background(), nil, ti.id, creds)
			require.Error(t, err)
			assert.True(t, errors.Is(err, config.ErrPlaintextNonLoopback), "got %v", err)
			assert.NotContains(t, err.Error(), "hunter2-secret", "the refusal does not echo URL credentials")
			assert.Empty(t, hub.recordedRequests(), "no request (and no HMAC-signed header) reaches the hub")
		})
	}
}

func TestValidateActivation_LoopbackAndHTTPSHubAllowed(t *testing.T) {
	// The loopback fake hub is reached directly; a loopback http endpoint
	// passes the transport rule and the self-read proceeds.
	ti := newTestInstance(t)
	hub := newFakeReplica(t)
	hub.get = func(string) (int, any) { return http.StatusOK, selfRow(ti, &ti.id.RuntimeTarget) }
	creds := savedInstanceCredentials(t, ti, hub.URL)
	require.NoError(t, ValidateActivation(context.Background(), nil, ti.id, creds))
	assert.Len(t, hub.recordedRequests(), 1)

	// An https endpoint passes the rule: the error, if any, is not the
	// transport refusal (nothing listens there in this test).
	creds.HubEndpoint = "https://127.0.0.1:1"
	err := ValidateActivation(context.Background(), nil, ti.id, creds)
	require.Error(t, err)
	assert.False(t, errors.Is(err, config.ErrPlaintextNonLoopback), "https is accepted by the transport rule: %v", err)
}
