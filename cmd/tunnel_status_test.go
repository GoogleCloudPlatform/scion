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
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/tunnelclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tunnelMemStateStore is an in-memory tunnelclient.StateStore.
type tunnelMemStateStore struct {
	records []tunnelclient.Record
	err     error
}

func (s *tunnelMemStateStore) Open() (tunnelclient.RecordWriter, error) {
	return nil, errors.New("not supported")
}

func (s *tunnelMemStateStore) List() ([]tunnelclient.Record, error) { return s.records, s.err }

func tunnelSampleRecords() []tunnelclient.Record {
	opened := time.Date(2026, 10, 11, 10, 0, 0, 0, time.UTC)
	deadline := opened.Add(time.Hour)
	mk := func(ep, subject, session, agent string) tunnelclient.Record {
		return tunnelclient.Record{
			Schema: tunnelclient.RecordSchema, PID: 1, StartedAt: opened, Endpoint: ep,
			Credential: tunnelclient.Identity{Kind: tunnelclient.CredentialOAuth, Subject: subject, Scope: tunnelclient.ScopeHub},
			SessionIDs: []string{session},
			Tunnels:    []tunnelclient.TunnelRecord{{Agent: agent, Project: "proj", Kind: "tcp", Port: 5173, OpenedAt: opened, AuthzDeadline: &deadline}},
		}
	}
	return []tunnelclient.Record{
		mk("https://hub.example.com/", "u1", "s1", "a1"),
		mk("https://HUB.example.com", "u1", "s2", "a2"),
		mk("https://other.example.com", "u1", "s3", "b1"),
	}
}

func TestTunnelStatusJSON(t *testing.T) {
	var out bytes.Buffer
	require.NoError(t, writeTunnelStatus(&out, &tunnelMemStateStore{records: tunnelSampleRecords()}, "", true))
	var st tunnelclient.Status
	require.NoError(t, json.Unmarshal(out.Bytes(), &st))
	require.Len(t, st.Entries, 2)
	assert.Equal(t, "https://hub.example.com", st.Entries[0].Endpoint)
	assert.Equal(t, []string{"s1", "s2"}, st.Entries[0].SessionIDs)
	assert.Len(t, st.Entries[0].Tunnels, 2)
	assert.Equal(t, tunnelclient.Identity{Kind: "oauth", Subject: "u1", Scope: "hub"}, st.Entries[0].Credential)
}

func TestTunnelStatusFiltersByHub(t *testing.T) {
	var out bytes.Buffer
	require.NoError(t, writeTunnelStatus(&out, &tunnelMemStateStore{records: tunnelSampleRecords()}, "https://other.example.com/", true))
	var st tunnelclient.Status
	require.NoError(t, json.Unmarshal(out.Bytes(), &st))
	require.Len(t, st.Entries, 1)
	assert.Equal(t, []string{"s3"}, st.Entries[0].SessionIDs)
}

func TestTunnelStatusEmptyAndText(t *testing.T) {
	var out bytes.Buffer
	require.NoError(t, writeTunnelStatus(&out, &tunnelMemStateStore{}, "", true))
	assert.JSONEq(t, `{"entries":[]}`, out.String())

	out.Reset()
	require.NoError(t, writeTunnelStatus(&out, &tunnelMemStateStore{}, "", false))
	assert.Equal(t, "No open tunnels.\n", out.String())

	out.Reset()
	require.NoError(t, writeTunnelStatus(&out, &tunnelMemStateStore{records: tunnelSampleRecords()}, "", false))
	assert.Contains(t, out.String(), "Hub:        https://hub.example.com")
	assert.Contains(t, out.String(), "AGENT")
	assert.Contains(t, out.String(), "a2")
}

func TestTunnelStatusStoreErrorExits1(t *testing.T) {
	err := writeTunnelStatus(&bytes.Buffer{}, &tunnelMemStateStore{err: errors.New("refusing tunnel state: group- or world-accessible")}, "", true)
	assert.Equal(t, tunnelclient.ExitError, tunnelclient.CodeOf(err))
}

func TestTunnelStatusCLIExtraArgExits2(t *testing.T) {
	code, stderr := tunnelRunCLI(t, nil, "tunnel", "status", "extra")
	assert.Equal(t, tunnelclient.ExitUsage, code)
	tunnelAssertOneLine(t, stderr)
}

func TestTunnelStatusCLIJSONNoRecords(t *testing.T) {
	code, _ := tunnelRunCLI(t, nil, "tunnel", "status", "--json")
	assert.Equal(t, 0, code)
}

// Status output never shows the endpoint's userinfo, query or fragment,
// even for a record that carries them.
func TestTunnelStatusRedactsEndpoint(t *testing.T) {
	recs := tunnelSampleRecords()
	recs[0].Endpoint = "https://alice:s3cret@hub.example.com/?token=TOK123#frag"
	for _, asJSON := range []bool{true, false} {
		var out bytes.Buffer
		require.NoError(t, writeTunnelStatus(&out, &tunnelMemStateStore{records: recs}, "", asJSON))
		for _, secret := range []string{"alice", "s3cret", "TOK123", "frag"} {
			assert.NotContains(t, out.String(), secret)
		}
	}
}
