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
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
)

func streamRecheck(t *testing.T, c *relay.HubClient, items ...relay.HubStreamRecheckItem) []relay.HubStreamRecheckResult {
	t.Helper()
	var resp relay.HubStreamRecheckResponse
	require.NoError(t, c.Call(context.Background(), relay.HubStreamRecheckPath, relay.HubStreamRecheckRequest{Items: items}, &resp))
	require.Len(t, resp.Results, len(items))
	return resp.Results
}

// TestRelayStreamRecheck_AdmissionToken (design §3.9): the relay-held
// entry point takes the stream's principal only from the admission token
// of the user session holding it. A token that does not authenticate, or
// that names a session of another principal kind, is a deny (4401); the
// grant is not evaluated.
func TestRelayStreamRecheck_AdmissionToken(t *testing.T) {
	f := newRelayRenewalFixture(t)
	tok, open := f.openGrant(t, grant.ViaProxy, tcpHeader("3000"), nil)
	require.True(t, f.relayRecheck(t, f.item(tok, open, 7)).Renew)

	t.Run("tampered", func(t *testing.T) {
		it := f.item(tok, open, 7)
		it.AdmissionToken = tamper(f.adm.AdmissionToken)
		res := f.relayRecheck(t, it)
		assert.True(t, res.Close)
		assert.Equal(t, conduit.CloseUnauthenticated, res.CloseCode)
	})
	t.Run("another session's token", func(t *testing.T) {
		it := f.item(tok, open, 7)
		it.UserSessionID = "user-other"
		res := f.relayRecheck(t, it)
		assert.True(t, res.Close)
		assert.Equal(t, conduit.CloseUnauthenticated, res.CloseCode)
	})
	t.Run("an agent session", func(t *testing.T) {
		agentTok, err := f.srv.GenerateAgentToken(f.agent.ID, f.agent.ProjectID, nil, AgentRoleFull, nil)
		require.NoError(t, err)
		h := http.Header{}
		h.Set("X-Scion-Agent-Token", agentTok)
		adm := f.ops.mustAdmit(t, h, "agent-s9")
		it := f.item(tok, open, 7)
		it.UserSessionID, it.AdmissionToken = "agent-s9", adm.AdmissionToken
		res := f.relayRecheck(t, it)
		assert.True(t, res.Close)
		assert.Equal(t, conduit.CloseUnauthenticated, res.CloseCode)
	})
}
