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

// ---------------------------------------------------------------------------
// U10 (ptone/scion#2257, design auto-offload-large-dm §5, §8.1): the
// offload_threshold_runes operational setting hot-reloads, defaults to 0
// (disabled) in every absent/omitted/malformed/negative case, and P1/P2
// never wire anything but a literal FetchByID: false (checked separately by
// grep/code review — see agent_dm_operation.go and handlers_agent_messaging.go).
// ---------------------------------------------------------------------------

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOffloadThresholdRunes_AbsentRow(t *testing.T) {
	fakeStore := newFakeHubSettingStore()
	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	_, err := ops.Refresh(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, ops.OffloadThresholdRunes())
}

func TestOffloadThresholdRunes_KeyOmitted(t *testing.T) {
	fakeStore := newFakeHubSettingStore()
	fakeStore.seed("messaging", json.RawMessage(`{}`))
	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	_, err := ops.Refresh(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, ops.OffloadThresholdRunes())
}

func TestOffloadThresholdRunes_ExplicitValue(t *testing.T) {
	fakeStore := newFakeHubSettingStore()
	fakeStore.seed("messaging", json.RawMessage(`{"offload_threshold_runes":4000}`))
	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	_, err := ops.Refresh(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 4000, ops.OffloadThresholdRunes())
}

func TestOffloadThresholdRunes_NegativeTreatedAsZero(t *testing.T) {
	fakeStore := newFakeHubSettingStore()
	fakeStore.seed("messaging", json.RawMessage(`{"offload_threshold_runes":-5}`))
	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	_, err := ops.Refresh(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, ops.OffloadThresholdRunes())
}

func TestOffloadThresholdRunes_MalformedJSON(t *testing.T) {
	fakeStore := newFakeHubSettingStore()
	fakeStore.seed("messaging", json.RawMessage(`not valid json`))
	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	_, err := ops.Refresh(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, ops.OffloadThresholdRunes())
}

func TestOffloadThresholdRunes_HotReload(t *testing.T) {
	fakeStore := newFakeHubSettingStore()
	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	_, err := ops.Refresh(context.Background())
	require.NoError(t, err)
	require.Equal(t, 0, ops.OffloadThresholdRunes())

	rev, err := ops.Update(context.Background(), "messaging",
		[]byte(`{"conversation_envelope_switch":true,"offload_threshold_runes":4000}`), "test", 0, "managed")
	require.NoError(t, err)
	require.Greater(t, rev, int64(0))
	assert.Equal(t, 4000, ops.OffloadThresholdRunes(), "Update must take effect without a restart")

	// Explicit null resets to the compiled default via the admin PUT path
	// (see handlePutMessaging); Update itself just writes whatever doc it's
	// given, so simulate the reset doc directly here.
	_, err = ops.Update(context.Background(), "messaging",
		[]byte(`{"conversation_envelope_switch":true,"offload_threshold_runes":0}`), "test", rev, "managed")
	require.NoError(t, err)
	assert.Equal(t, 0, ops.OffloadThresholdRunes())
}

func TestOffloadPolicy_DerivedFromOperationalSettings(t *testing.T) {
	srv, _ := testServer(t)
	// No OperationalSettings configured: fail-safe default (disabled).
	assert.Equal(t, 0, srv.offloadPolicy().ThresholdRunes)

	fakeStore := newFakeHubSettingStore()
	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	_, err := ops.Update(context.Background(), "messaging",
		[]byte(`{"conversation_envelope_switch":true,"offload_threshold_runes":4000}`), "test", 0, "managed")
	require.NoError(t, err)
	srv.SetOperationalSettings(ops)
	assert.Equal(t, 4000, srv.offloadPolicy().ThresholdRunes)
}
