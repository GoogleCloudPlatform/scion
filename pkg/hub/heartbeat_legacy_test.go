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
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBrokerHeartbeatRequest_UnmarshalJSON(t *testing.T) {
	t.Run("HandleProjectsKey", func(t *testing.T) {
		data := `{"status":"online","projects":[{"projectId":"p1","agentCount":1}]}`
		var hb brokerHeartbeatRequest
		err := json.Unmarshal([]byte(data), &hb)
		require.NoError(t, err)
		assert.Equal(t, "online", hb.Status)
		require.Len(t, hb.Projects, 1)
		assert.Equal(t, "p1", hb.Projects[0].ProjectID)
	})

	t.Run("HandleGrovesKey", func(t *testing.T) {
		data := `{"status":"online","groves":[{"projectId":"p1","agentCount":1}]}`
		var hb brokerHeartbeatRequest
		err := json.Unmarshal([]byte(data), &hb)
		require.NoError(t, err)
		assert.Equal(t, "online", hb.Status)
		require.Len(t, hb.Projects, 1)
		assert.Equal(t, "p1", hb.Projects[0].ProjectID)
	})
}

func TestBrokerProjectHeartbeat_UnmarshalJSON(t *testing.T) {
	t.Run("HandleProjectIDKey", func(t *testing.T) {
		data := `{"projectId":"p1","agentCount":1}`
		var p brokerProjectHeartbeat
		err := json.Unmarshal([]byte(data), &p)
		require.NoError(t, err)
		assert.Equal(t, "p1", p.ProjectID)
	})

	t.Run("HandleGroveIDKey", func(t *testing.T) {
		data := `{"groveId":"p1","agentCount":1}`
		var p brokerProjectHeartbeat
		err := json.Unmarshal([]byte(data), &p)
		require.NoError(t, err)
		assert.Equal(t, "p1", p.ProjectID)
	})
}
