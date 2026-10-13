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

package hubclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Heartbeat returns the Hub's response body, nil for an older Hub's empty
// body, and an error for an error status.
func TestHeartbeat_Response(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		want    *BrokerHeartbeatResponse
		wantErr bool
	}{
		{name: "older hub, empty body", status: http.StatusOK, body: "", want: nil},
		{name: "hashes read, nothing requested", status: http.StatusOK, body: `{"profileSAMappingsHashes":true}`, want: &BrokerHeartbeatResponse{ProfileSAMappingsHashes: true}},
		{name: "full report requested", status: http.StatusOK, body: `{"profileSAMappingsHashes":true,"profileSAMappingsRequested":true}`, want: &BrokerHeartbeatResponse{ProfileSAMappingsHashes: true, ProfileSAMappingsRequested: true}},
		{name: "undecodable body is accepted as older hub", status: http.StatusOK, body: "ok", want: nil},
		{name: "error status", status: http.StatusInternalServerError, body: `{"error":{"code":"internal","message":"x"}}`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got BrokerHeartbeat
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/api/v1/runtime-brokers/b1/heartbeat", r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			client, err := New(server.URL)
			require.NoError(t, err)
			resp, err := client.RuntimeBrokers().Heartbeat(context.Background(), "b1", &got)
			if tc.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, resp)
		})
	}
}
