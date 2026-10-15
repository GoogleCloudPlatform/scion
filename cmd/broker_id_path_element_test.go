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

package cmd

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/brokercredentials"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// brokerIDsNotPathElement are broker IDs that are not a single path element.
var brokerIDsNotPathElement = map[string]string{
	"empty":         "",
	"dot":           ".",
	"dot dot":       "..",
	"slash":         "/",
	"absolute":      "/abs",
	"nested":        "a/b",
	"parent prefix": "../x",
	"inner dot dot": "a..b",
	"backslash":     `a\b`,
}

// joinTestClient is a client for a test server that completes a join with
// brokerID.
func joinTestClient(t *testing.T, brokerID string) hubclient.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"brokerId":  brokerID,
			"secretKey": base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")),
		})
	}))
	t.Cleanup(srv.Close)
	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)
	return client
}

// TestCompleteJoinAndPersist_BrokerIDPathElement: a broker ID that is not a
// single path element is refused before anything is saved.
func TestCompleteJoinAndPersist_BrokerIDPathElement(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for name, id := range brokerIDsNotPathElement {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			store := brokercredentials.NewMultiStore(dir)
			res, err := completeJoinAndPersist(context.Background(), joinTestClient(t, id), brokerJoinParams{
				Endpoint: "https://hub.example.com", HubName: "hub-example-com", BrokerID: "11111111-2222-3333-4444-555555555555",
				JoinToken: "scion_join_x", Hostname: "h", CredStore: store,
			})
			require.Error(t, err, "join accepted broker ID %q", id)
			assert.True(t, errors.Is(err, brokercredentials.ErrBrokerIDNotPathElement))
			assert.Nil(t, res)
			entries, readErr := os.ReadDir(dir)
			if readErr == nil {
				assert.Empty(t, entries, "credentials were saved")
			}
		})
	}
	dir := t.TempDir()
	res, err := completeJoinAndPersist(context.Background(), joinTestClient(t, "11111111-2222-3333-4444-555555555555"), brokerJoinParams{
		Endpoint: "https://hub.example.com", HubName: "hub-example-com", BrokerID: "11111111-2222-3333-4444-555555555555",
		JoinToken: "scion_join_x", Hostname: "h", CredStore: brokercredentials.NewMultiStore(dir),
	})
	require.NoError(t, err)
	require.NoError(t, res.SaveErr)
	assert.FileExists(t, filepath.Join(dir, "hub-example-com.json"))
}

// TestBrokerLocalStatePaths_BrokerIDPathElement: the state directory is
// listed only for a broker ID that is a single path element; the log and
// the template cache are always listed.
func TestBrokerLocalStatePaths_BrokerIDPathElement(t *testing.T) {
	for name, id := range brokerIDsNotPathElement {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, []string{"/g/broker.log", "/h/.scion/cache/templates"}, brokerLocalStatePaths("/g", "/h/.scion", id),
				"state path listed for broker ID %q", id)
		})
	}
	assert.Equal(t, []string{"/g/broker.log", "/h/.scion/runtime-broker-state/b-1", "/h/.scion/cache/templates"}, brokerLocalStatePaths("/g", "/h/.scion", "b-1"))
}
