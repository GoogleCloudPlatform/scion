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

package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestChatSearchSettings_RoundTripAndEnv checks the chat search limits
// (server.hub.chat.search.timeout and max_concurrent, ptone/scion#7586):
// V1 to GlobalConfig and back, invalid and zero values ignored, and the
// schema's env names reach the loaded config.
func TestChatSearchSettings_RoundTripAndEnv(t *testing.T) {
	four := 4
	v1 := &V1ServerConfig{Hub: &V1ServerHubConfig{Chat: &V1ServerHubChatConfig{
		Search: &V1ServerHubChatSearchConfig{Timeout: "5s", MaxConcurrent: &four},
	}}}
	gc := ConvertV1ServerToGlobalConfig(v1)
	d, err := gc.Hub.Chat.Search.TimeoutDuration()
	require.NoError(t, err)
	assert.Equal(t, 5*time.Second, d)
	assert.Equal(t, 4, gc.Hub.Chat.Search.MaxConcurrent)

	back := ConvertGlobalToV1ServerConfig(gc)
	require.NotNil(t, back.Hub)
	require.NotNil(t, back.Hub.Chat)
	require.NotNil(t, back.Hub.Chat.Search)
	assert.Equal(t, "5s", back.Hub.Chat.Search.Timeout)
	require.NotNil(t, back.Hub.Chat.Search.MaxConcurrent)
	assert.Equal(t, 4, *back.Hub.Chat.Search.MaxConcurrent)

	empty := ConvertGlobalToV1ServerConfig(&GlobalConfig{})
	assert.Nil(t, empty.Hub.Chat, "unset limits are omitted")

	zero := 0
	bad := ConvertV1ServerToGlobalConfig(&V1ServerConfig{Hub: &V1ServerHubConfig{Chat: &V1ServerHubChatConfig{
		Search: &V1ServerHubChatSearchConfig{Timeout: "soon", MaxConcurrent: &zero},
	}}})
	_, err = bad.Hub.Chat.Search.TimeoutDuration()
	assert.Error(t, err, "an invalid duration is reported")
	assert.Zero(t, bad.Hub.Chat.Search.MaxConcurrent, "zero means the backend default")
	_, err = HubChatSearchConfig{Timeout: "-1s"}.TimeoutDuration()
	assert.Error(t, err, "a non-positive duration is reported")
	d, err = HubChatSearchConfig{}.TimeoutDuration()
	require.NoError(t, err)
	assert.Zero(t, d, "unset means the hub default")

	t.Setenv("HOME", t.TempDir())
	t.Setenv("SCION_SERVER_HUB_CHAT_SEARCH_TIMEOUT", "2s")
	t.Setenv("SCION_SERVER_HUB_CHAT_SEARCH_MAXCONCURRENT", "3")
	cfg, err := LoadGlobalConfig("")
	require.NoError(t, err)
	assert.Equal(t, "2s", cfg.Hub.Chat.Search.Timeout)
	assert.Equal(t, 3, cfg.Hub.Chat.Search.MaxConcurrent)
}
