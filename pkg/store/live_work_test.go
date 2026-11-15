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

package store

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAgentHasRecentLiveWork(t *testing.T) {
	now := time.Now()
	at := func(d time.Duration) *time.Time { t := now.Add(-d); return &t }

	assert.False(t, (*Agent)(nil).HasRecentLiveWork(now), "nil agent")
	assert.False(t, (&Agent{}).HasRecentLiveWork(now), "never reported")
	assert.True(t, (&Agent{LastLiveProcessAt: at(0)}).HasRecentLiveWork(now), "just now")
	assert.True(t, (&Agent{LastLiveProcessAt: at(LiveWorkFreshness - time.Second)}).HasRecentLiveWork(now), "inside the window")
	assert.False(t, (&Agent{LastLiveProcessAt: at(LiveWorkFreshness)}).HasRecentLiveWork(now), "at the window edge")
	assert.False(t, (&Agent{LastLiveProcessAt: at(10 * time.Minute)}).HasRecentLiveWork(now), "stale")
	// The stall column alone does not count: the guard reads any live work.
	assert.False(t, (&Agent{LastLiveWorkAt: at(0)}).HasRecentLiveWork(now), "stall column only")
}

// The Hub decodes the heartbeat sciontool sends (ptone/scion#7855), and a
// heartbeat from an older sciontool, without the field, decodes as before.
func TestAgentStatusUpdate_DecodesLiveWork(t *testing.T) {
	var su AgentStatusUpdate
	require.NoError(t, json.Unmarshal([]byte(
		`{"heartbeat":true,"liveWork":{"kind":"process","suppressStall":true,"detail":"go (pid 7, 1.0s cpu)"}}`), &su))
	assert.True(t, su.Heartbeat)
	require.NotNil(t, su.LiveWork)
	assert.Equal(t, LiveWork{Kind: "process", SuppressStall: true, Detail: "go (pid 7, 1.0s cpu)"}, *su.LiveWork)

	su = AgentStatusUpdate{}
	require.NoError(t, json.Unmarshal([]byte(`{"heartbeat":true}`), &su))
	assert.True(t, su.Heartbeat)
	assert.Nil(t, su.LiveWork)
}
