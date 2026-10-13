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
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TestAdmissionToken_Confidential (criterion 30): the token's bytes carry
// no readable claims (principal, session, scope, credential id), a token
// sealed under another key or label is refused, and any altered byte is
// refused (the AEAD tag check fails closed).
func TestAdmissionToken_Confidential(t *testing.T) {
	f, cf := newUserRelayOps(t)
	key, row, err := cf.srv.uatService.CreateToken(rs4MintContext(cf.owner.ID()), cf.owner.ID(), "relay", cf.agent.ProjectID, []string{store.UATScopeAgentPortAccess}, nil)
	require.NoError(t, err)
	adm := f.mustAdmit(t, bearer(key), "s-confidential")

	raw, err := base64.RawURLEncoding.DecodeString(adm.AdmissionToken)
	require.NoError(t, err, "the token is opaque base64url")
	for _, secret := range []string{cf.owner.ID(), "s-confidential", row.ID, store.UATScopeAgentPortAccess, cf.agent.ProjectID, "relay-0", `"pk"`, `"user"`} {
		assert.False(t, bytes.Contains(raw, []byte(secret)), "the token bytes reveal %q", secret)
	}
	var probe map[string]any
	assert.Error(t, json.Unmarshal(raw, &probe), "the token is not readable JSON")

	_, err = rpcCall(t, f.hub, "s-confidential", adm.AdmissionToken, "/v1/unknown")
	require.NoError(t, err, "the hub opens its own token")

	// Every single-byte change is refused.
	for _, i := range []int{0, 11, 12, len(raw) / 2, len(raw) - 1} {
		altered := append([]byte(nil), raw...)
		altered[i] ^= 0x01
		_, err := rpcCall(t, f.hub, "s-confidential", base64.RawURLEncoding.EncodeToString(altered), "/v1/unknown")
		assert.Equal(t, http.StatusUnauthorized, refusalStatus(t, err), "byte %d altered", i)
	}

	// A token sealed with the right claims but another label or key.
	claims, err := json.Marshal(conduitAdmissionClaims{V: conduitAdmissionTokenVersion, SessionID: "s-confidential", Relay: "relay-0", RelayGen: f.relayGen,
		Kind: "user", ID: cf.owner.ID(), AdmittedAt: time.Now().Unix(), IssuedAt: time.Now().Unix(), Expiry: time.Now().Add(time.Hour).Unix()})
	require.NoError(t, err)
	seal := func(secret, label string) string {
		k, err := hkdf.Key(sha256.New, []byte(secret), nil, label, 32)
		require.NoError(t, err)
		block, err := aes.NewCipher(k)
		require.NoError(t, err)
		aead, err := cipher.NewGCM(block)
		require.NoError(t, err)
		nonce := make([]byte, aead.NonceSize())
		_, _ = rand.Read(nonce)
		return base64.RawURLEncoding.EncodeToString(aead.Seal(nonce, nonce, claims, []byte(conduitAdmissionAAD)))
	}
	_, err = rpcCall(t, f.hub, "s-confidential", seal(testRelaySessionSecret, conduitAdmissionKeyInfo), "/v1/unknown")
	require.NoError(t, err, "the same key and label open: the forgery checks below are meaningful")
	_, err = rpcCall(t, f.hub, "s-confidential", seal(testRelaySessionSecret, "scion conduit relay-peer secret hmac v1"), "/v1/unknown")
	assert.Equal(t, http.StatusUnauthorized, refusalStatus(t, err), "another label")
	_, err = rpcCall(t, f.hub, "s-confidential", seal("another-session-secret-0123456789abcdef", conduitAdmissionKeyInfo), "/v1/unknown")
	assert.Equal(t, http.StatusUnauthorized, refusalStatus(t, err), "another key")
}

// TestRelayAdmit_ForwardedHeadersDefaultDeny (criterion 30): under a
// trusted-proxy setting that covers the forwarded request's address, proxy
// identity headers named in X-Scion-Signed-Headers have no effect and never
// reach the middleware, with or without a broker id; an allowlisted
// credential header still passes.
func TestRelayAdmit_ForwardedHeadersDefaultDeny(t *testing.T) {
	f, cf := newUserRelayOps(t)
	cf.srv.authConfig.TrustedProxies = []string{"192.0.2.0/24", "0.0.0.0/0"}
	var mu sync.Mutex
	var seen []http.Header
	conduitRelayAdmitObserve = func(r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, r.Header.Clone())
	}
	t.Cleanup(func() { conduitRelayAdmitObserve = nil })

	spoofed := map[string]string{
		"X-Forwarded-User-Id": cf.owner.ID(), "X-Forwarded-User-Email": "owner@conduit.test", "X-Forwarded-For": "10.0.0.1",
		"Forwarded": "for=10.0.0.1", "X-Real-Ip": "10.0.0.1", "Cookie": "session=x", "X-Goog-Iap-Jwt-Assertion": "x.y.z",
		"Content-Type": "application/json",
	}
	signedList := "X-Forwarded-User-Id;X-Forwarded-User-Email;X-Forwarded-For;Forwarded;X-Real-Ip;Cookie;X-Goog-Iap-Jwt-Assertion;Content-Type"
	for _, broker := range []bool{false, true} {
		h := http.Header{}
		for k, v := range spoofed {
			h.Set(k, v)
		}
		h.Set("X-Scion-Signed-Headers", signedList)
		if broker {
			h.Set("X-Scion-Broker-Id", "no-such-broker")
		}
		mu.Lock()
		seen = nil
		mu.Unlock()
		// Sent unfiltered, as a relay that does not filter would: the
		// hub's own allowlist decides.
		var resp relay.HubAdmitResponse
		err := f.hub.Call(context.Background(), relay.HubAdmitPath, relay.HubAdmitRequest{
			RelayInstanceID: "relay-0", RelayGeneration: f.relayGen, SessionID: "s-spoof",
			Host: relayPublicHost, Headers: map[string][]string(h),
		}, &resp)
		require.Error(t, err, "broker=%v: admitted as %+v", broker, resp)
		mu.Lock()
		require.Len(t, seen, 1)
		got := seen[0]
		mu.Unlock()
		for k := range spoofed {
			if k == "Content-Type" && broker {
				assert.Equal(t, "application/json", got.Get(k), "a broker-signed header is forwarded for a broker request")
				continue
			}
			assert.Empty(t, got.Get(k), "broker=%v: %s reached the middleware", broker, k)
		}
	}

	// An allowlisted credential still passes and admits.
	adm := f.mustAdmit(t, bearer(ownerJWT(t, cf)), "s-allowed")
	assert.Equal(t, cf.owner.ID(), adm.PrincipalID)
}

// TestRelayAdmit_SessionIDOnce: Admit refuses a session id already in the
// registry.
func TestRelayAdmit_SessionIDOnce(t *testing.T) {
	f, cf := newUserRelayOps(t)
	_, err := f.reg.InsertSessionWithNextEpoch(context.Background(), userSessionRecord("s-taken", f.relayGen, cf.owner.ID()))
	require.NoError(t, err)
	_, err = f.admit(t, bearer(ownerJWT(t, cf)), "s-taken")
	var refusal *relay.HubRefusal
	require.ErrorAs(t, err, &refusal)
	assert.Equal(t, http.StatusConflict, refusal.Status)
	assert.Equal(t, "session_exists", refusal.Header.Get(relay.HeaderStaleReason))
	f.mustAdmit(t, bearer(ownerJWT(t, cf)), "s-free")
}

// TestRelayAgentSession_RevokedOrHeldRefused (criterion 30): an agent token
// revoked, or an agent held, after admission is refused at the next
// operation while the admission token is still valid: the session
// re-check closes it with 4401 and a refresh is refused.
func TestRelayAgentSession_RevokedOrHeldRefused(t *testing.T) {
	for _, tc := range []string{"revoked", "held"} {
		t.Run(tc, func(t *testing.T) {
			f, cf := newUserRelayOps(t)
			relayClock(t, time.Now())
			agentTok, err := cf.srv.GenerateAgentToken(cf.agent.ID, cf.agent.ProjectID, nil, AgentRoleFull, nil)
			require.NoError(t, err)
			adm := f.mustAdmit(t, http.Header{"X-Scion-Agent-Token": {agentTok}}, "s-agent")
			res := sessionRecheck(t, f.hub, relay.HubSessionRecheckItem{SessionID: "s-agent", AdmissionToken: adm.AdmissionToken})
			require.True(t, res[0].OK, "%+v", res[0])

			ctx := context.Background()
			if tc == "revoked" {
				_, err := cf.store.RevokeAgentCredentialsByAgent(ctx, cf.agent.ID, "test", "test")
				require.NoError(t, err)
			} else {
				holdAgentFor(t, cf.store, cf.agent.ID, cf.agent.ProjectID, cf.owner.ID())
			}
			res = sessionRecheck(t, f.hub, relay.HubSessionRecheckItem{SessionID: "s-agent", AdmissionToken: adm.AdmissionToken})
			assert.True(t, res[0].Close, "%+v", res[0])
			assert.Equal(t, uint32(4401), res[0].CloseCode)

			fresh, err := cf.srv.GenerateAgentToken(cf.agent.ID, cf.agent.ProjectID, nil, AgentRoleFull, nil)
			require.NoError(t, err)
			err = f.hub.Call(ctx, relay.HubSessionPath("s-agent", "refresh"), relay.HubRefreshRequest{AdmissionToken: adm.AdmissionToken, Credential: []byte(fresh)}, nil)
			var refusal *relay.HubRefusal
			require.ErrorAs(t, err, &refusal, "refresh after the agent was %s", tc)
			assert.Equal(t, http.StatusUnauthorized, refusal.Status)
		})
	}
}

// TestRelaySessionRecheck_NoReissueAfterCredentialExpiry: a re-check keeps a
// session whose credential has expired (as in process, an expired user
// credential only stops new tunnels) but never re-issues its token.
func TestRelaySessionRecheck_NoReissueAfterCredentialExpiry(t *testing.T) {
	f, cf := newUserRelayOps(t)
	start := time.Now()
	now := relayClock(t, start)
	jwt := ownerJWT(t, cf)
	adm := f.mustAdmit(t, bearer(jwt), "s1")
	require.False(t, adm.CredentialExpiry.IsZero())
	*now = adm.CredentialExpiry.Add(time.Minute)
	res := sessionRecheck(t, f.hub, relay.HubSessionRecheckItem{SessionID: "s1", AdmissionToken: adm.AdmissionToken})
	require.True(t, res[0].OK, "%+v", res[0])
	assert.Empty(t, res[0].AdmissionToken, "no re-issue past the credential's expiry")
}

// userSessionRecord is a registry row of a user session on relay-0.
func userSessionRecord(id string, gen int64, userID string) registry.SessionRecord {
	return registry.SessionRecord{
		SessionID: id, PrincipalKind: registry.PrincipalUser, PrincipalID: userID,
		RelayInstanceID: "relay-0", RelayGeneration: gen, Transport: registry.TransportWS, EndpointIncarnation: "user",
	}
}
