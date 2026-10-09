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
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRegisterProject_BrokerName: the broker embedded in a project
// registration carries the configured broker name, else the hostname. The
// hub matches the embedded broker by name, so the hostname must not
// replace a configured name.
func TestRegisterProject_BrokerName(t *testing.T) {
	// newHandler returns a fresh hub handler and the broker name it
	// captures, so the subtests share no state.
	newHandler := func() (http.HandlerFunc, *string) {
		var gotName string
		return func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Broker *struct {
					Name string `json:"name"`
				} `json:"broker"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Broker != nil {
				gotName = body.Broker.Name
			}
			w.WriteHeader(http.StatusNoContent)
		}, &gotName
	}

	t.Run("hostname by default", func(t *testing.T) {
		handler, gotName := newHandler()
		hubCtx, _ := newHintTestHubCtx(t, handler)
		_ = registerProject(context.Background(), hubCtx, "proj", true)
		host, err := os.Hostname()
		require.NoError(t, err)
		assert.Equal(t, host, *gotName)
	})

	t.Run("configured name", func(t *testing.T) {
		handler, gotName := newHandler()
		hubCtx, _ := newHintTestHubCtx(t, handler)
		globalDir := filepath.Join(os.Getenv("HOME"), ".scion")
		require.NoError(t, os.MkdirAll(globalDir, 0755))
		require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"),
			[]byte("schema_version: \"1\"\nserver:\n  broker:\n    broker_nickname: rig-broker-2\n"), 0644))
		_ = registerProject(context.Background(), hubCtx, "proj", true)
		assert.Equal(t, "rig-broker-2", *gotName)
	})
}
