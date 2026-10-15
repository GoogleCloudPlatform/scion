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
	"bytes"
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
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
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
			got := entryPaths(brokerLocalStatePaths("/g", "/h/.scion", "/h/.scion/runtime-broker-state", id))
			assert.Equal(t, []string{"/g/broker.log", "/h/.scion/cache/templates"}, got, "state path listed for broker ID %q", id)
		})
	}
	assert.Equal(t, []string{"/g/broker.log", "/h/.scion/runtime-broker-state/b-1", "/h/.scion/cache/templates"}, entryPaths(brokerLocalStatePaths("/g", "/h/.scion", "/h/.scion/runtime-broker-state", "b-1")))
}

// isolatedGlobalDir points HOME at a fresh temporary directory and returns
// the global settings directory under it, where UpdateSetting(..., true)
// writes. It does not create the directory.
func isolatedGlobalDir(t *testing.T) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	dir, err := config.GetGlobalDir()
	require.NoError(t, err)
	return dir
}

// assertNothingWritten fails when dir holds any file, so no settings file
// of any name or format was created there. A missing dir holds nothing.
func assertNothingWritten(t *testing.T, dir, msg string, args ...any) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return
	}
	require.NoError(t, err)
	assert.Empty(t, entries, append([]any{msg}, args...)...)
}

// TestAdoptDedupedBrokerID_BrokerIDPathElement: a deduplicated broker ID
// that is not a single path element is an error and nothing is saved; a
// valid one is saved to hub.brokerId.
func TestAdoptDedupedBrokerID_BrokerIDPathElement(t *testing.T) {
	for name, id := range brokerIDsNotPathElement {
		if id == "" {
			continue
		}
		t.Run(name, func(t *testing.T) {
			globalDir := isolatedGlobalDir(t)
			err := adoptDedupedBrokerID(globalDir, id, func(string, ...any) {})
			require.Error(t, err, "deduplicated broker ID %q adopted", id)
			assert.True(t, errors.Is(err, brokercredentials.ErrBrokerIDNotPathElement))
			assertNothingWritten(t, globalDir, "settings written for deduplicated broker ID %q", id)
		})
	}
	globalDir := isolatedGlobalDir(t)
	require.NoError(t, adoptDedupedBrokerID(globalDir, "11111111-2222-3333-4444-555555555555", func(string, ...any) {}))
	gs, err := config.LoadSettings(globalDir)
	require.NoError(t, err)
	require.NotNil(t, gs.Hub)
	assert.Equal(t, "11111111-2222-3333-4444-555555555555", gs.Hub.BrokerID)
}

// TestRecoverEmbeddedBrokerID_BrokerIDPathElement: a recovered broker ID
// that is not a single path element is an error and nothing is saved; a
// valid one is returned and saved to hub.brokerId.
func TestRecoverEmbeddedBrokerID_BrokerIDPathElement(t *testing.T) {
	for name, id := range brokerIDsNotPathElement {
		if id == "" {
			continue
		}
		t.Run(name, func(t *testing.T) {
			globalDir := isolatedGlobalDir(t)
			got, err := recoverEmbeddedBrokerID(globalDir, id, func(string, ...any) {})
			require.Error(t, err, "recovered broker ID %q was used", id)
			assert.True(t, errors.Is(err, brokercredentials.ErrBrokerIDNotPathElement))
			assert.Empty(t, got)
			assertNothingWritten(t, globalDir, "settings written for recovered broker ID %q", id)
		})
	}
	globalDir := isolatedGlobalDir(t)
	got, err := recoverEmbeddedBrokerID(globalDir, "11111111-2222-3333-4444-555555555555", func(string, ...any) {})
	require.NoError(t, err)
	assert.Equal(t, "11111111-2222-3333-4444-555555555555", got)
	gs, err := config.LoadSettings(globalDir)
	require.NoError(t, err)
	require.NotNil(t, gs.Hub)
	assert.Equal(t, "11111111-2222-3333-4444-555555555555", gs.Hub.BrokerID)
}

// embeddedBrokerStore is a store whose single embedded broker has ID id;
// resolveBrokerID calls only FindEmbeddedBroker.
type embeddedBrokerStore struct {
	store.Store
	id string
}

func (s embeddedBrokerStore) FindEmbeddedBroker(context.Context) (*store.RuntimeBroker, error) {
	return &store.RuntimeBroker{ID: s.id}, nil
}

// TestResolveBrokerID_RecoveredIDNotPathElementRefuses: with no configured
// ID, a recovered broker ID that is not a single path element stops broker
// ID resolution: no ID is generated in its place and no settings are
// written.
func TestResolveBrokerID_RecoveredIDNotPathElementRefuses(t *testing.T) {
	globalDir := isolatedGlobalDir(t)
	got, err := resolveBrokerID(context.Background(), &config.GlobalConfig{}, &config.Settings{}, nil, globalDir, "", embeddedBrokerStore{id: "../x"})
	require.Error(t, err, "broker ID resolution went on with recovered ID %q", "../x")
	assert.True(t, errors.Is(err, brokercredentials.ErrBrokerIDNotPathElement))
	assert.Empty(t, got, "an ID was substituted for the recovered one")
	assertNothingWritten(t, globalDir, "settings written after a refused recovery")

	got, err = resolveBrokerID(context.Background(), &config.GlobalConfig{}, &config.Settings{}, nil, globalDir, "", embeddedBrokerStore{id: "11111111-2222-3333-4444-555555555555"})
	require.NoError(t, err)
	assert.Equal(t, "11111111-2222-3333-4444-555555555555", got)
}

// TestResolveBrokerID_ConfiguredIDNotPathElementRefuses: a configured or
// deterministic broker ID that is not a single path element stops broker ID
// resolution, and no settings are written.
func TestResolveBrokerID_ConfiguredIDNotPathElementRefuses(t *testing.T) {
	bad := "../x"
	for name, tc := range map[string]struct {
		cfg       *config.GlobalConfig
		settings  *config.Settings
		vsBroker  *config.V1BrokerConfig
		defaultID string
	}{
		"settings":              {&config.GlobalConfig{}, &config.Settings{Hub: &config.HubClientConfig{BrokerID: bad}}, nil, ""},
		"server broker config":  {&config.GlobalConfig{}, &config.Settings{}, &config.V1BrokerConfig{BrokerID: bad}, ""},
		"global config":         {&config.GlobalConfig{RuntimeBroker: config.RuntimeBrokerConfig{BrokerID: bad}}, &config.Settings{}, nil, ""},
		"deterministic default": {&config.GlobalConfig{}, &config.Settings{}, nil, bad},
	} {
		t.Run(name, func(t *testing.T) {
			globalDir := isolatedGlobalDir(t)
			got, err := resolveBrokerID(context.Background(), tc.cfg, tc.settings, tc.vsBroker, globalDir, tc.defaultID, nil)
			require.Error(t, err, "broker ID resolution returned %q", got)
			assert.True(t, errors.Is(err, brokercredentials.ErrBrokerIDNotPathElement))
			assert.Empty(t, got)
			assertNothingWritten(t, globalDir, "settings written for configured broker ID %q", bad)
		})
	}

	// A valid deterministic default is returned unchanged and not saved.
	globalDir := isolatedGlobalDir(t)
	got, err := resolveBrokerID(context.Background(), &config.GlobalConfig{}, &config.Settings{}, nil, globalDir, "11111111-2222-3333-4444-555555555555", nil)
	require.NoError(t, err)
	assert.Equal(t, "11111111-2222-3333-4444-555555555555", got)
	assertNothingWritten(t, globalDir, "settings written for a deterministic broker ID")
}

// TestPersistBrokerHubSettings_BrokerIDPathElement: a broker ID that is not
// a single path element leaves the settings file unchanged: no broker ID,
// hub endpoint or hub connection is written.
func TestPersistBrokerHubSettings_BrokerIDPathElement(t *testing.T) {
	for name, id := range brokerIDsNotPathElement {
		if id == "" {
			continue
		}
		t.Run(name, func(t *testing.T) {
			globalDir := isolateServerHubEndpointEnv(t)
			path := writeServerHubEndpointTestSettings(t, globalDir, "")
			before, err := os.ReadFile(path)
			require.NoError(t, err)
			var w bytes.Buffer
			persistBrokerHubSettings(&w, globalDir, "https://hub.example.com", id, "hub-example-com")
			after, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, string(before), string(after), "settings were written for broker ID %q", id)
			assert.Contains(t, w.String(), "Warning: not saving the broker ID or hub settings")
		})
	}
}
