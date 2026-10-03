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
		{name: "oidc without allowed accounts", opts: ConduitPeerAuthOptions{Mode: "oidc", TokenSource: staticTokenSource{tok: "t"}}, wantErr: "no allowed service accounts"},
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
	a, b, other := newAuth("hub-a", "shared-signing-secret-0123456789ab"), newAuth("hub-b", "shared-signing-secret-0123456789ab"), newAuth("hub-c", "another-signing-secret-0123456789")

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

// TestConduitPeerAuth_OIDC covers signing and the verification policy:
// audience, verified email and the allow-list (own SA by default).
func TestConduitPeerAuth_OIDC(t *testing.T) {
	const own = "hub@p.iam.gserviceaccount.com"
	var gotAudience string
	claims := map[string]map[string]any{
		"own":        {"email": own, "email_verified": true},
		"own-upper":  {"email": "HUB@p.iam.gserviceaccount.com", "email_verified": true},
		"stranger":   {"email": "other@p.iam.gserviceaccount.com", "email_verified": true},
		"unverified": {"email": own, "email_verified": false},
		"no-email":   {"email_verified": true},
	}
	validate := func(_ context.Context, tok, aud string) (*idtoken.Payload, error) {
		gotAudience = aud
		c, ok := claims[tok]
		if !ok {
			return nil, errors.New("bad signature")
		}
		return &idtoken.Payload{Audience: aud, Claims: c}, nil
	}
	newAuth := func(t *testing.T, o ConduitPeerAuthOptions) relay.PeerAuth {
		t.Helper()
		o.OnGCP, o.Validate = true, validate
		if o.TokenSource == nil {
			o.TokenSource = staticTokenSource{tok: "own"}
		}
		a, mode, err := NewConduitPeerAuth(o)
		require.NoError(t, err)
		require.Equal(t, config.ConduitPeerAuthOIDC, mode)
		return a
	}
	verify := func(a relay.PeerAuth, header string) (string, error) {
		req := httptest.NewRequest(http.MethodGet, "/internal/v1/conduit/self", nil)
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		return a.Verify(req)
	}

	t.Run("sign attaches the bearer token", func(t *testing.T) {
		a := newAuth(t, ConduitPeerAuthOptions{OwnServiceAccount: own})
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		require.NoError(t, a.Sign(req))
		assert.Equal(t, "Bearer own", req.Header.Get("Authorization"))
		id, err := a.Verify(req)
		require.NoError(t, err)
		assert.Equal(t, own, id)
		assert.Equal(t, DefaultConduitPeerAudience, gotAudience)
	})

	t.Run("sign fails closed", func(t *testing.T) {
		for _, src := range []staticTokenSource{{err: errors.New("no ADC")}, {tok: ""}} {
			a := newAuth(t, ConduitPeerAuthOptions{OwnServiceAccount: own, TokenSource: src})
			assert.Error(t, a.Sign(httptest.NewRequest(http.MethodGet, "/", nil)))
		}
	})

	t.Run("custom audience", func(t *testing.T) {
		a := newAuth(t, ConduitPeerAuthOptions{OwnServiceAccount: own, Audience: "custom"})
		_, err := verify(a, "Bearer own")
		require.NoError(t, err)
		assert.Equal(t, "custom", gotAudience)
	})

	defaultAllow := newAuth(t, ConduitPeerAuthOptions{OwnServiceAccount: own})
	explicitAllow := newAuth(t, ConduitPeerAuthOptions{OwnServiceAccount: own, ServiceAccounts: []string{"other@p.iam.gserviceaccount.com"}})
	tests := []struct {
		name   string
		auth   relay.PeerAuth
		header string
		wantID string
	}{
		{name: "own SA", auth: defaultAllow, header: "Bearer own", wantID: own},
		{name: "email case-insensitive", auth: defaultAllow, header: "Bearer own-upper", wantID: "HUB@p.iam.gserviceaccount.com"},
		{name: "SA not allowed", auth: defaultAllow, header: "Bearer stranger"},
		{name: "explicit list replaces own SA", auth: explicitAllow, header: "Bearer own"},
		{name: "explicit list admits its SA", auth: explicitAllow, header: "Bearer stranger", wantID: "other@p.iam.gserviceaccount.com"},
		{name: "unverified email", auth: defaultAllow, header: "Bearer unverified"},
		{name: "no email", auth: defaultAllow, header: "Bearer no-email"},
		{name: "invalid token", auth: defaultAllow, header: "Bearer forged"},
		{name: "no header", auth: defaultAllow},
		{name: "not a bearer token", auth: defaultAllow, header: "Basic own"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, err := verify(tt.auth, tt.header)
			if tt.wantID == "" {
				assert.ErrorIs(t, err, relay.ErrPeerUnauthenticated)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantID, id)
		})
	}
}
