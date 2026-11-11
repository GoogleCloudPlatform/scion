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
	"encoding/json"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// deletionJSON returns the JSON object of a deletion view, or nil for a nil
// view.
func deletionJSON(t *testing.T, info *store.DeletionInfo) map[string]json.RawMessage {
	t.Helper()
	if info == nil {
		return nil
	}
	raw, err := json.Marshal(info)
	require.NoError(t, err)
	var m map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &m))
	return m
}

// rawDeletionObject decodes a deletion value taken from a response body:
// nil for an explicit null.
func rawDeletionObject(t *testing.T, raw json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	if string(raw) == "null" {
		return nil
	}
	var m map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &m), string(raw))
	return m
}

// assertGenericDeletionOf asserts that generic is exactly the admin view
// without the detail keys: same presence (both null or both objects), no
// code, error or claim, and every other key byte-identical.
func assertGenericDeletionOf(t *testing.T, admin, generic map[string]json.RawMessage, label string) {
	t.Helper()
	if admin == nil {
		assert.Nil(t, generic, "%s: deletion is null for admin, so it must be null for this caller", label)
		return
	}
	require.NotNil(t, generic, "%s: deletion is an object for admin, so it must be an object for this caller", label)
	for _, k := range deletionDetailKeys {
		_, present := generic[k]
		assert.False(t, present, "%s: %q must be absent from the generic view", label, k)
	}
	want := map[string]string{}
	for k, v := range admin {
		want[k] = string(v)
	}
	for _, k := range deletionDetailKeys {
		delete(want, k)
	}
	got := map[string]string{}
	for k, v := range generic {
		got[k] = string(v)
	}
	assert.Equal(t, want, got, "%s: state, stage, soft and timestamps match the admin view", label)
}

// seedDeletionWithError seeds a delete marker and then sets its error text.
func seedDeletionWithError(t *testing.T, s store.Store, agentID string, d deleteSeed, errText string) {
	t.Helper()
	seedAgentDeletion(t, s, agentID, d)
	if errText == "" {
		return
	}
	n, err := s.UpdateAgentDeletion(context.Background(), agentID, store.DeletionPredicate{}, store.DeletionFields{Error: &errText})
	require.NoError(t, err)
	require.Equal(t, 1, n)
}

// deletionDetailKeys are the DeletionInfo JSON keys only admins see.
var deletionDetailKeys = []string{"code", "error", "claim"}
