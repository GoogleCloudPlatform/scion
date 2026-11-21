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
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRemoteCreateAgentRequest_HomeFilesJSONRoundTrip(t *testing.T) {
	t.Run("omitted when empty", func(t *testing.T) {
		req := RemoteCreateAgentRequest{
			Slug:      "agent-1",
			Name:      "agent-1",
			ProjectID: "proj-1",
		}
		raw, err := json.Marshal(req)
		require.NoError(t, err)
		assert.NotContains(t, string(raw), "homeFiles")

		var got RemoteCreateAgentRequest
		require.NoError(t, json.Unmarshal(raw, &got))
		assert.Empty(t, got.HomeFiles)
	})

	t.Run("base64 content round-trip when populated", func(t *testing.T) {
		payload := []byte("{\"event\":\"push\",\"raw\":true}\n\x00\xff")
		req := RemoteCreateAgentRequest{
			Slug:      "agent-1",
			Name:      "agent-1",
			ProjectID: "proj-1",
			HomeFiles: []api.HomeFile{
				{
					Path:    ".scion/trigger-event.json",
					Mode:    0o600,
					Content: payload,
				},
				{
					Path:    ".scion/meta/context.txt",
					Mode:    0o644,
					Content: []byte("hello"),
				},
			},
		}

		raw, err := json.Marshal(req)
		require.NoError(t, err)
		assert.Contains(t, string(raw), `"homeFiles"`)
		assert.Contains(t, string(raw), base64.StdEncoding.EncodeToString(payload))

		var got RemoteCreateAgentRequest
		require.NoError(t, json.Unmarshal(raw, &got))
		require.Len(t, got.HomeFiles, 2)
		assert.Equal(t, ".scion/trigger-event.json", got.HomeFiles[0].Path)
		assert.Equal(t, uint32(0o600), got.HomeFiles[0].Mode)
		assert.Equal(t, payload, got.HomeFiles[0].Content)
		assert.Equal(t, ".scion/meta/context.txt", got.HomeFiles[1].Path)
		assert.Equal(t, uint32(0o644), got.HomeFiles[1].Mode)
		assert.Equal(t, []byte("hello"), got.HomeFiles[1].Content)
	})
}

func TestCapabilitiesFromStrings_HomeFiles(t *testing.T) {
	assert.True(t, capabilitiesFromStrings([]string{"exec", "homeFiles"}).HomeFiles)
	assert.True(t, capabilitiesFromStrings([]string{"home_files"}).HomeFiles)
	assert.False(t, capabilitiesFromStrings([]string{"exec", "pty", "attach"}).HomeFiles)
}

func TestCheckBrokerHomeFilesCapability(t *testing.T) {
	for name, broker := range map[string]*store.RuntimeBroker{
		"nil broker":      nil,
		"nil caps":        {ID: "b-1", Name: "legacy-broker"},
		"homeFiles false": {ID: "b-2", Name: "old-broker", Capabilities: &store.BrokerCapabilities{WebPTY: true}},
		"id fallback":     {ID: "b-only-id", Capabilities: &store.BrokerCapabilities{}},
	} {
		t.Run(name, func(t *testing.T) {
			err := checkBrokerHomeFilesCapability(broker)
			require.Error(t, err)
			assert.True(t, errors.Is(err, ErrBrokerLacksHomeFiles), "expected ErrBrokerLacksHomeFiles, got %v", err)
			assert.True(t, isConfirmedStartNotActedOnError(err), "capability refusal must be classified not-acted-on")
			assert.Contains(t, err.Error(), "does not support home files")
			if broker != nil && broker.Name != "" {
				assert.Contains(t, err.Error(), broker.Name)
			} else if broker != nil && broker.ID != "" {
				assert.Contains(t, err.Error(), broker.ID)
			} else {
				assert.Equal(t, ErrBrokerLacksHomeFiles.Error(), err.Error())
			}
		})
	}

	capable := &store.RuntimeBroker{
		ID:           "b-ok",
		Name:         "capable-broker",
		Capabilities: &store.BrokerCapabilities{HomeFiles: true},
	}
	assert.NoError(t, checkBrokerHomeFilesCapability(capable))
}

func TestDispatchArgs_HomeFilesRoundTrip(t *testing.T) {
	rawEmptyCreate, err := MarshalDispatchArgs(&CreateWithGatherDispatchArgs{})
	require.NoError(t, err)
	assert.Equal(t, "{}", rawEmptyCreate)

	files := []api.HomeFile{{Path: ".scion/trigger-event.json", Mode: 0o600, Content: []byte(`{"id":"ev-1"}`)}}
	rawCreate, err := MarshalDispatchArgs(&CreateWithGatherDispatchArgs{HomeFiles: files})
	require.NoError(t, err)
	gotCreate, err := UnmarshalCreateArgs(rawCreate)
	require.NoError(t, err)
	assert.Equal(t, files, gotCreate.HomeFiles)

	rawFinalize, err := MarshalDispatchArgs(&FinalizeEnvDispatchArgs{
		Env:       map[string]string{"K": "V"},
		HomeFiles: files,
	})
	require.NoError(t, err)
	gotFinalize, err := UnmarshalFinalizeEnvArgs(rawFinalize)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"K": "V"}, gotFinalize.Env)
	assert.Equal(t, files, gotFinalize.HomeFiles)
}

func TestDispatchFailureResult_HomeFilesSentinelsRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name           string
		err            error
		sentinel       error
		requestNotSent bool
	}{
		{"lacks capability", &brokerLacksHomeFilesError{broker: "b-1"}, ErrBrokerLacksHomeFiles, true},
		{"env gather deferred", &homeFilesEnvGatherDeferredError{needs: []string{"SECRET_A"}}, ErrHomeFilesEnvGatherDeferred, false},
		{"create not deferrable", ErrHomeFilesCreateNotDeferrable, ErrHomeFilesCreateNotDeferrable, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.requestNotSent {
				assert.ErrorIs(t, tc.err, errStartRequestNotSent)
			} else {
				assert.NotErrorIs(t, tc.err, errStartRequestNotSent, "deferral sentinel must not masquerade as errStartRequestNotSent")
			}
			assert.True(t, isConfirmedStartNotActedOnError(tc.err))
			assert.False(t, isUnknownDispatchOutcome(tc.err))

			res := dispatchFailureResult(tc.err)
			require.NotEmpty(t, res)
			rebuilt := dispatchFailureError(&store.BrokerDispatch{
				Op:     "create",
				State:  store.DispatchStateFailed,
				Error:  tc.err.Error(),
				Result: res,
			})
			assert.ErrorIs(t, rebuilt, tc.sentinel)
			if tc.requestNotSent {
				assert.ErrorIs(t, rebuilt, errStartRequestNotSent)
			} else {
				assert.NotErrorIs(t, rebuilt, errStartRequestNotSent, "deferral sentinel must not masquerade as errStartRequestNotSent")
			}
			assert.True(t, isConfirmedStartNotActedOnError(rebuilt), "rebuilt cross-node error must remain confirmed not-acted-on")
			assert.False(t, isUnknownDispatchOutcome(rebuilt))
		})
	}
}
