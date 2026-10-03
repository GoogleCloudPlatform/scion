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
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/idtoken"
)

type staticTokenSource struct {
	tok string
	err error
}

func (s staticTokenSource) Token() (string, error)   { return s.tok, s.err }
func (staticTokenSource) SetToken(string, time.Time) {}
func (staticTokenSource) Expiry() time.Time          { return time.Time{} }

const testPeerSecret = "shared-signing-secret-0123456789ab"

func TestResolveConduitPeerAuthMode(t *testing.T) {
	tests := []struct {
		mode    string
		onGCP   bool
		want    string
		wantErr bool
	}{
		{mode: "", onGCP: true, want: config.ConduitPeerAuthOIDC},
		{mode: "", onGCP: false, want: config.ConduitPeerAuthHMAC},
		{mode: "auto", onGCP: true, want: config.ConduitPeerAuthOIDC},
		{mode: "AUTO", onGCP: false, want: config.ConduitPeerAuthHMAC},
		{mode: "oidc", onGCP: false, want: config.ConduitPeerAuthOIDC},
		{mode: "HMAC", onGCP: true, want: config.ConduitPeerAuthHMAC},
		{mode: "mtls", wantErr: true},
	}
	for _, tt := range tests {
		got, err := ResolveConduitPeerAuthMode(tt.mode, tt.onGCP)
		if tt.wantErr {
			assert.Error(t, err, tt.mode)
			continue
		}
		require.NoError(t, err, tt.mode)
		assert.Equal(t, tt.want, got, "mode %q onGCP %v", tt.mode, tt.onGCP)
	}
}

func TestNewConduitPeerAuth_Errors(t *testing.T) {
	tests := []struct {
		name    string
		opts    ConduitPeerAuthOptions
		wantErr string
	}{
		{name: "unknown mode", opts: ConduitPeerAuthOptions{Mode: "mtls"}, wantErr: "unknown mode"},
		{name: "hmac without secret", opts: ConduitPeerAuthOptions{Mode: "hmac", SelfID: "a"}, wantErr: "no shared signing secret"},
		{name: "oidc without secret", opts: ConduitPeerAuthOptions{Mode: "oidc", SelfID: "a", OwnServiceAccount: "hub@p.iam.gserviceaccount.com",
			TokenSource: staticTokenSource{tok: "t"}}, wantErr: "no shared signing secret"},
		{name: "auto on GCP without secret", opts: ConduitPeerAuthOptions{OnGCP: true, SelfID: "a", OwnServiceAccount: "hub@p.iam.gserviceaccount.com",
			TokenSource: staticTokenSource{tok: "t"}}, wantErr: "no shared signing secret"},
		{name: "oidc without allowed accounts", opts: ConduitPeerAuthOptions{Mode: "oidc", SelfID: "a", SharedSecret: testPeerSecret,
			TokenSource: staticTokenSource{tok: "t"}}, wantErr: "no allowed service accounts"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := NewConduitPeerAuth(tt.opts)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

// TestConduitPeerAuth_HMAC: two nodes sharing the signing secret
// authenticate each other; a node with another secret is refused.
func TestConduitPeerAuth_HMAC(t *testing.T) {
	newAuth := func(id, secret string) relay.PeerAuth {
		a, mode, err := NewConduitPeerAuth(ConduitPeerAuthOptions{OnGCP: false, SelfID: id, SharedSecret: secret})
		require.NoError(t, err)
		require.Equal(t, config.ConduitPeerAuthHMAC, mode)
		return a
	}
	a, b, other := newAuth("hub-a", testPeerSecret), newAuth("hub-b", testPeerSecret), newAuth("hub-c", "another-signing-secret-0123456789")

	req := httptest.NewRequest(http.MethodGet, "/internal/v1/conduit/self", nil)
	require.NoError(t, a.Sign(req))
	id, err := b.Verify(req)
	require.NoError(t, err)
	assert.Equal(t, "hub-a", id)

	req = httptest.NewRequest(http.MethodGet, "/internal/v1/conduit/self", nil)
	require.NoError(t, other.Sign(req))
	_, err = b.Verify(req)
	assert.ErrorIs(t, err, relay.ErrPeerUnauthenticated)
}

// fakeIDTokens validates the fixed test tokens below.
func fakeIDTokens(gotAudience *string) func(context.Context, string, string) (*idtoken.Payload, error) {
	const own = "hub@p.iam.gserviceaccount.com"
	claims := map[string]map[string]any{
		"own":        {"email": own, "email_verified": true},
		"own-upper":  {"email": "HUB@p.iam.gserviceaccount.com", "email_verified": true},
		"stranger":   {"email": "other@p.iam.gserviceaccount.com", "email_verified": true},
		"unverified": {"email": own, "email_verified": false},
		"no-email":   {"email_verified": true},
	}
	return func(_ context.Context, tok, aud string) (*idtoken.Payload, error) {
		if gotAudience != nil {
			*gotAudience = aud
		}
		c, ok := claims[tok]
		if !ok {
			return nil, errors.New("bad signature")
		}
		return &idtoken.Payload{Audience: aud, Claims: c}, nil
	}
}

// newOIDCModeAuth returns an oidc-mode authenticator for selfID that
// presents token and checks tokens with fakeIDTokens.
func newOIDCModeAuth(t *testing.T, selfID, token string, gotAudience *string, mod func(*ConduitPeerAuthOptions)) relay.PeerAuth {
	t.Helper()
	o := ConduitPeerAuthOptions{
		Mode: config.ConduitPeerAuthOIDC, SelfID: selfID, SharedSecret: testPeerSecret,
		OwnServiceAccount: "hub@p.iam.gserviceaccount.com",
		TokenSource:       staticTokenSource{tok: token}, Validate: fakeIDTokens(gotAudience),
	}
	if mod != nil {
		mod(&o)
	}
	a, mode, err := NewConduitPeerAuth(o)
	require.NoError(t, err)
	require.Equal(t, config.ConduitPeerAuthOIDC, mode)
	return a
}

// TestConduitPeerAuth_OIDC: in oidc mode relay-peer requests are signed
// and also carry an ID token; both are required. It covers the ID token
// policy: audience, verified email and the allow-list (own SA by default).
func TestConduitPeerAuth_OIDC(t *testing.T) {
	var gotAudience string
	server := newOIDCModeAuth(t, "hub-b", "own", &gotAudience, nil)
	hmacOnly, err := relay.NewHMACPeerAuthFromSecret(relay.HMACPeerAuthConfig{Secret: []byte(testPeerSecret), SelfID: "hub-a"})
	require.NoError(t, err)

	newReq := func() *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/internal/v1/conduit/sessions/s1/rpc", nil)
		req.Header.Set(relay.HeaderBodySHA256, "abc")
		return req
	}
	// signedWith signs with the HMAC key and then presents bearer.
	signedWith := func(t *testing.T, bearer string) *http.Request {
		t.Helper()
		req := newReq()
		require.NoError(t, hmacOnly.Sign(req))
		if bearer != "" {
			req.Header.Set("Authorization", bearer)
		}
		return req
	}

	t.Run("both credentials", func(t *testing.T) {
		req := newReq()
		require.NoError(t, newOIDCModeAuth(t, "hub-a", "own", nil, nil).Sign(req))
		assert.Equal(t, "Bearer own", req.Header.Get("Authorization"))
		assert.NotEmpty(t, req.Header.Get(relay.HeaderPeerSignature))
		id, err := server.Verify(req)
		require.NoError(t, err)
		assert.Equal(t, "hub-a", id, "the peer id is the signed relay instance id")
		assert.Equal(t, DefaultConduitPeerAudience, gotAudience)
	})

	t.Run("ID token without signature is refused", func(t *testing.T) {
		req := newReq()
		req.Header.Set("Authorization", "Bearer own")
		_, err := server.Verify(req)
		assert.ErrorIs(t, err, relay.ErrPeerUnauthenticated)
	})

	t.Run("signature without ID token is refused", func(t *testing.T) {
		_, err := server.Verify(signedWith(t, ""))
		assert.ErrorIs(t, err, relay.ErrPeerUnauthenticated)
	})

	t.Run("tampered signed header is refused", func(t *testing.T) {
		req := signedWith(t, "Bearer own")
		req.Header.Set(relay.HeaderBodySHA256, "def")
		_, err := server.Verify(req)
		assert.ErrorIs(t, err, relay.ErrPeerUnauthenticated)
	})

	t.Run("replayed nonce is refused", func(t *testing.T) {
		req := signedWith(t, "Bearer own")
		replay := req.Clone(context.Background())
		_, err := server.Verify(req)
		require.NoError(t, err)
		_, err = server.Verify(replay)
		assert.ErrorIs(t, err, relay.ErrPeerUnauthenticated)
	})

	t.Run("other signing secret is refused", func(t *testing.T) {
		req := newReq()
		other := newOIDCModeAuth(t, "hub-c", "own", nil, func(o *ConduitPeerAuthOptions) { o.SharedSecret = "another-signing-secret-0123456789" })
		require.NoError(t, other.Sign(req))
		_, err := server.Verify(req)
		assert.ErrorIs(t, err, relay.ErrPeerUnauthenticated)
	})

	t.Run("sign fails closed", func(t *testing.T) {
		for _, src := range []staticTokenSource{{err: errors.New("no ADC")}, {tok: ""}} {
			a := newOIDCModeAuth(t, "hub-a", "", nil, func(o *ConduitPeerAuthOptions) { o.TokenSource = src })
			assert.Error(t, a.Sign(newReq()))
		}
	})

	t.Run("custom audience", func(t *testing.T) {
		a := newOIDCModeAuth(t, "hub-b", "own", &gotAudience, func(o *ConduitPeerAuthOptions) { o.Audience = "custom" })
		_, err := a.Verify(signedWith(t, "Bearer own"))
		require.NoError(t, err)
		assert.Equal(t, "custom", gotAudience)
	})

	explicit := newOIDCModeAuth(t, "hub-b", "own", nil, func(o *ConduitPeerAuthOptions) {
		o.ServiceAccounts = []string{"other@p.iam.gserviceaccount.com"}
	})
	tests := []struct {
		name   string
		auth   relay.PeerAuth
		bearer string
		ok     bool
	}{
		{name: "own SA", auth: server, bearer: "Bearer own", ok: true},
		{name: "email case-insensitive", auth: server, bearer: "Bearer own-upper", ok: true},
		{name: "SA not allowed", auth: server, bearer: "Bearer stranger"},
		{name: "explicit list replaces own SA", auth: explicit, bearer: "Bearer own"},
		{name: "explicit list admits its SA", auth: explicit, bearer: "Bearer stranger", ok: true},
		{name: "unverified email", auth: server, bearer: "Bearer unverified"},
		{name: "no email", auth: server, bearer: "Bearer no-email"},
		{name: "invalid token", auth: server, bearer: "Bearer forged"},
		{name: "not a bearer token", auth: server, bearer: "Basic own"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, err := tt.auth.Verify(signedWith(t, tt.bearer))
			if !tt.ok {
				assert.ErrorIs(t, err, relay.ErrPeerUnauthenticated)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "hub-a", id)
		})
	}
}
