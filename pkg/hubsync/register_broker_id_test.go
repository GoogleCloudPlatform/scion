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

package hubsync

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// registerAnswering answers a project registration naming brokerID as the
// project's broker.
func registerAnswering(brokerID string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"project":     map[string]any{"id": "project-hint", "name": "proj", "slug": "proj"},
			"broker":      map[string]any{"id": brokerID, "name": "b"},
			"created":     true,
			"brokerToken": "tok-1",
		})
	}
}

// savedBrokerSettings are the global hub.brokerId and hub.brokerToken
// settings ("" when unset).
func savedBrokerSettings(t *testing.T) (brokerID, token string) {
	t.Helper()
	globalDir, err := config.GetGlobalDir()
	require.NoError(t, err)
	gs, err := config.LoadSettings(globalDir)
	require.NoError(t, err)
	if gs == nil || gs.Hub == nil {
		return "", ""
	}
	return gs.Hub.BrokerID, gs.Hub.BrokerToken
}

// TestRegisterProject_BrokerIDPathElement: the broker ID of a registration
// and token are saved to hub.brokerId and hub.brokerToken only when the ID is
// a single path element; otherwise neither is saved, with a warning.
func TestRegisterProject_BrokerIDPathElement(t *testing.T) {
	for name, id := range map[string]string{"dot dot": "..", "nested": "a/b", "parent prefix": "../x", "inner dot dot": "a..b", "backslash": `a\b`} {
		t.Run(name, func(t *testing.T) {
			hubCtx, _ := newHintTestHubCtx(t, registerAnswering(id))
			var out bytes.Buffer
			prev := promptOut
			promptOut = &out
			t.Cleanup(func() { promptOut = prev })
			require.NoError(t, registerProject(context.Background(), hubCtx, "proj", true))
			gotID, gotToken := savedBrokerSettings(t)
			assert.Empty(t, gotID, "broker ID %q was saved", id)
			assert.Empty(t, gotToken, "broker token saved with broker ID %q", id)
			assert.Contains(t, out.String(), "Warning: not saving the broker token or broker ID")
		})
	}
	hubCtx, _ := newHintTestHubCtx(t, registerAnswering("11111111-2222-3333-4444-555555555555"))
	require.NoError(t, registerProject(context.Background(), hubCtx, "proj", true))
	gotID, gotToken := savedBrokerSettings(t)
	assert.Equal(t, "11111111-2222-3333-4444-555555555555", gotID)
	assert.Equal(t, "tok-1", gotToken)
}
