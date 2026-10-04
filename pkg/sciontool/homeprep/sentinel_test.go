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

package homeprep

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testAgentID = "0b9f6a52-3c1e-4f43-9d8e-2a6f1c7b5e10"

// The full sentinel decision table, including every fail-closed row.
func TestDecide(t *testing.T) {
	seeded := &Sentinel{Version: 1, AgentID: testAgentID, State: StateSeeded, UID: 1000}
	seeding := &Sentinel{Version: 1, AgentID: testAgentID, State: StateSeeding, UID: 1000}
	tests := []struct {
		name      string
		read      SentinelRead
		uid       int
		names     []string
		want      Decision
		wantClass string
		wantMsg   string
	}{
		{name: "read error", read: SentinelRead{Err: errors.New("permission denied")}, uid: 1000, wantClass: ErrClassPrepare, wantMsg: "permission denied"},
		{name: "no sentinel, empty home", read: SentinelRead{NotExist: true}, uid: 1000, want: Decision{Mode: ModeSeed}},
		{
			name: "no sentinel, only reserved leftovers", read: SentinelRead{NotExist: true}, uid: 1000,
			names: []string{".scion-home-probe.s1.abc", ".scion-home-seed.s1.def"},
			want:  Decision{Mode: ModeSeed, RemoveReserved: []string{".scion-home-probe.s1.abc", ".scion-home-seed.s1.def"}},
		},
		{
			name: "no sentinel, content", read: SentinelRead{NotExist: true}, uid: 1000,
			names: []string{".scion-home-probe.s1.abc", ".bashrc"}, wantClass: ErrClassPrepare, wantMsg: "content but no sentinel",
		},
		{name: "seeding, same agent", read: SentinelRead{Sentinel: seeding}, uid: 1000, names: []string{SentinelName, "x"}, want: Decision{Mode: ModeSeed, CleanInterrupted: true}},
		{name: "seeded, same agent", read: SentinelRead{Sentinel: seeded}, uid: 1000, names: []string{SentinelName}, want: Decision{Mode: ModeSeedOver}},
		{
			name: "other agent", uid: 1000,
			read:      SentinelRead{Sentinel: &Sentinel{Version: 1, AgentID: "11111111-2222-4333-8444-555555555555", State: StateSeeded, UID: 1000}},
			wantClass: ErrClassPrepare, wantMsg: "belongs to agent",
		},
		{name: "other uid", read: SentinelRead{Sentinel: seeded}, uid: 1001, wantClass: ErrClassPrepare, wantMsg: "uid 1000"},
		{name: "no result", read: SentinelRead{}, uid: 1000, wantClass: ErrClassPrepare},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Decide(tt.read, testAgentID, tt.uid, tt.names)
			if tt.wantClass != "" {
				var ce *ClassError
				require.ErrorAs(t, err, &ce)
				assert.Equal(t, tt.wantClass, ce.Class)
				assert.Contains(t, err.Error(), tt.wantMsg)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestParseSentinel(t *testing.T) {
	_, err := parseSentinel([]byte(`{"version":1,"agent_id":"a","state":"seeded","uid":1000}`))
	require.NoError(t, err)
	for _, bad := range []string{`not json`, `{"version":2,"state":"seeded"}`, `{"version":1,"state":"done"}`, `{}`} {
		_, err := parseSentinel([]byte(bad))
		assert.Error(t, err, bad)
	}
}

func TestParseLinks(t *testing.T) {
	links, err := ParseLinks(`[{"target":".config/gcloud/creds.json","source":"/run/scion/agent-secrets/creds","mode":"0600"},{"target":"a/../b","source":"/run/scion/x","mode":"0644"}]`)
	require.NoError(t, err)
	require.Len(t, links, 2)
	assert.Equal(t, "b", links[1].Target)

	none, err := ParseLinks("")
	require.NoError(t, err)
	assert.Nil(t, none)

	for _, bad := range []string{
		`not json`,
		`[{"target":"/etc/passwd","source":"/run/scion/x"}]`,
		`[{"target":"../x","source":"/run/scion/x"}]`,
		`[{"target":".","source":"/run/scion/x"}]`,
		`[{"target":".scion-home-seed.json","source":"/run/scion/x"}]`,
		`[{"target":".scion/.home-links.json","source":"/run/scion/x"}]`,
		`[{"target":"a","source":"relative"}]`,
		`[{"target":"a","source":"/run/scion/../x"}]`,
		`[{"target":"a","source":"/x"},{"target":"./a","source":"/y"}]`,
	} {
		_, err := ParseLinks(bad)
		assert.Error(t, err, bad)
	}
}

func TestValidAgentID(t *testing.T) {
	assert.True(t, ValidAgentID(testAgentID))
	for _, bad := range []string{"", "agent", "0B9F6A52-3C1E-4F43-9D8E-2A6F1C7B5E10", "0b9f6a523c1e4f439d8e2a6f1c7b5e10", "0b9f6a52-3c1e-4f43-9d8e-2a6f1c7b5e1/", "../f6a52-3c1e-4f43-9d8e-2a6f1c7b5e10"} {
		assert.False(t, ValidAgentID(bad), bad)
	}
}
