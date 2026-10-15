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
	"errors"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
)

// ptyPingPong sends an application ping on c and requires a pong back,
// skipping any terminal output in between.
func ptyPingPong(t *testing.T, c *websocket.Conn) {
	t.Helper()
	require.NoError(t, c.WriteJSON(wsprotocol.NewPingMessage()))
	for {
		_, data, err := c.ReadMessage()
		require.NoError(t, err, "the session must still be open")
		var env struct {
			Type string `json:"type"`
		}
		require.NoError(t, json.Unmarshal(data, &env))
		if env.Type == wsprotocol.TypePong {
			return
		}
	}
}

// openTokenPTY dials the fixture agent's PTY with auth (a user access token
// or a session) on the broker path or the agent path, and waits until the
// upstream stream is open.
func openTokenPTY(t *testing.T, f *ptyConduitFixture, brokerPath bool, auth string) *websocket.Conn {
	t.Helper()
	f.userToken = auth
	if brokerPath {
		f.setBrokerRow(t, attachCaps(true), "")
		b := connectFakeBroker(t, f.srv, f.launched.RuntimeBrokerID)
		c, _, err := f.dialPTY(t, "")
		require.NoError(t, err)
		require.NoError(t, b.ws.SetReadDeadline(time.Now().Add(10*time.Second)))
		var open wsprotocol.StreamOpenMessage
		require.NoError(t, b.ws.ReadJSON(&open))
		require.Equal(t, wsprotocol.StreamTypePTY, open.StreamType)
		return c
	}
	f.setBrokerRow(t, attachCaps(false), "")
	f.startPTYAgent(t, f.public.URL)
	c, _, err := f.dialPTY(t, "")
	require.NoError(t, err)
	waitSpawn(t, f.spawned)
	echoRoundTrip(t, c, "open")
	return c
}

// TestAgentPTY_TokenSessionEndsWhenTokenStopsValidating requires a PTY
// session opened with a user access token, on the broker path and on the
// agent path, to close with 4401 authz_expired once the token is revoked
// or expires, and to stay open past many re-check intervals while the
// token validates. A session opened with a session credential is not
// re-checked and stays open.
func TestAgentPTY_TokenSessionEndsWhenTokenStopsValidating(t *testing.T) {
	shortenStreamRecheck(t)
	for _, path := range []struct {
		name   string
		broker bool
	}{{"broker path", true}, {"agent path", false}} {
		t.Run(path.name, func(t *testing.T) {
			mint := func(t *testing.T, f *ptyConduitFixture, expiresAt *time.Time) (string, string) {
				owner := f.launched.OwnerID
				key, tok, err := f.srv.uatService.CreateToken(rs4MintContext(owner), owner, "pty-"+tid("tok"), f.launched.ProjectID, []string{"agent:attach"}, expiresAt)
				require.NoError(t, err)
				return key, tok.ID
			}
			requireAuthzExpired := func(t *testing.T, c *websocket.Conn) {
				t.Helper()
				require.NoError(t, c.SetReadDeadline(time.Now().Add(10*time.Second)))
				ce := readUntilClose(t, c)
				assert.Equal(t, wsprotocol.ClosePTYAuthRequired, ce.Code)
				assert.Equal(t, conduitReasonAuthzExpired, ce.Text)
			}

			t.Run("revoked token", func(t *testing.T) {
				f := newPTYConduitFixture(t)
				key, tokenID := mint(t, f, nil)
				c := openTokenPTY(t, f, path.broker, key)
				time.Sleep(10 * streamCredentialRecheckInterval)
				ptyPingPong(t, c)
				owner := f.launched.OwnerID
				require.NoError(t, f.srv.uatService.RevokeToken(rs4MintContext(owner), owner, tokenID))
				requireAuthzExpired(t, c)
			})

			t.Run("expired token", func(t *testing.T) {
				f := newPTYConduitFixture(t)
				exp := time.Unix(0, 0).Add(time.Since(time.Unix(0, 0)) + 3*time.Second)
				key, _ := mint(t, f, &exp)
				c := openTokenPTY(t, f, path.broker, key)
				requireAuthzExpired(t, c)
			})

			t.Run("suspended user", func(t *testing.T) {
				f := newPTYConduitFixture(t)
				key, _ := mint(t, f, nil)
				c := openTokenPTY(t, f, path.broker, key)
				setUserStatus(t, f.store, f.launched.OwnerID, store.UserStatusSuspended)
				requireAuthzExpired(t, c)
			})

			t.Run("session stays open", func(t *testing.T) {
				f := newPTYConduitFixture(t)
				c := openTokenPTY(t, f, path.broker, f.userToken)
				time.Sleep(10 * streamCredentialRecheckInterval)
				ptyPingPong(t, c)
			})
		})
	}
}

// TestPTYSession_CredentialCheckClosesWithAuthzExpired requires a PTY
// session whose credential check fails to close with 4401 authz_expired,
// and the close cause of errPTYCredentialEnded to be that code and reason.
func TestPTYSession_CredentialCheckClosesWithAuthzExpired(t *testing.T) {
	code, reason := ptyCloseCause(errPTYCredentialEnded)
	assert.Equal(t, wsprotocol.ClosePTYAuthRequired, code)
	assert.Equal(t, conduitReasonAuthzExpired, reason)
	assert.True(t, isExpectedPTYEnd(errPTYCredentialEnded))
	assert.False(t, errors.Is(context.Canceled, errPTYCredentialEnded))
}
