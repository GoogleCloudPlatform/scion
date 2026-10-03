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
	"fmt"
	"net/http"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth/adcsource"
	"google.golang.org/api/idtoken"
)

// Relay-peer authentication for the internal relay API (design v2.4 §3.2,
// §3.10): a service identity, never an end-user credential. On GCP the
// caller presents a Google-signed OIDC ID token for its service account;
// elsewhere both sides share an HMAC key derived from the hub's shared
// signing secret (relay.NewHMACPeerAuthFromSecret derives it with HKDF).
//
// Once a mode is selected it is the only one: an OIDC token that cannot be
// minted fails the call, and a token that does not validate is refused.
// There is no fallback to HMAC.

// DefaultConduitPeerAudience is the OIDC audience of relay-peer ID tokens.
// It is a fixed string, not derived from the hub id or the node, so every
// replica mints and expects the same audience.
const DefaultConduitPeerAudience = "scion-conduit-relay-peer"

// ConduitPeerAuthOptions selects and configures relay-peer auth.
type ConduitPeerAuthOptions struct {
	// Mode is config.ConduitPeerAuth{Auto,OIDC,HMAC} ("" = auto).
	Mode string
	// OnGCP reports whether this node runs on GCP (auto picks OIDC).
	OnGCP bool
	// SelfID is this relay's instance id (HMAC caller identity).
	SelfID string
	// SharedSecret is the hub's shared signing secret (HMAC). Required for
	// HMAC.
	SharedSecret string
	// Audience is the OIDC audience ("" = DefaultConduitPeerAudience).
	Audience string
	// ServiceAccounts is the OIDC caller allow-list. Empty: OwnServiceAccount.
	ServiceAccounts []string
	// OwnServiceAccount is this node's service-account email (the default
	// allow-list, since every hub replica runs as the same account).
	OwnServiceAccount string
	// TokenSource mints this node's ID tokens (nil: ADC for Audience).
	TokenSource transportauth.TokenSource
	// Validate validates an ID token (nil: idtoken.Validate).
	Validate func(ctx context.Context, token, audience string) (*idtoken.Payload, error)
}

// ResolveConduitPeerAuthMode returns the effective mode: hmac or oidc.
func ResolveConduitPeerAuthMode(mode string, onGCP bool) (string, error) {
	switch strings.ToLower(mode) {
	case "", config.ConduitPeerAuthAuto:
		if onGCP {
			return config.ConduitPeerAuthOIDC, nil
		}
		return config.ConduitPeerAuthHMAC, nil
	case config.ConduitPeerAuthOIDC:
		return config.ConduitPeerAuthOIDC, nil
	case config.ConduitPeerAuthHMAC:
		return config.ConduitPeerAuthHMAC, nil
	default:
		return "", fmt.Errorf("conduit peer auth: unknown mode %q (want auto, oidc or hmac)", mode)
	}
}

// NewConduitPeerAuth builds the relay-peer authenticator and returns the
// selected mode.
func NewConduitPeerAuth(o ConduitPeerAuthOptions) (relay.PeerAuth, string, error) {
	mode, err := ResolveConduitPeerAuthMode(o.Mode, o.OnGCP)
	if err != nil {
		return nil, "", err
	}
	switch mode {
	case config.ConduitPeerAuthHMAC:
		if o.SharedSecret == "" {
			return nil, mode, errors.New("conduit peer auth (hmac): no shared signing secret; set --session-secret or SCION_SERVER_SESSION_SECRET on every hub node")
		}
		a, err := relay.NewHMACPeerAuthFromSecret(relay.HMACPeerAuthConfig{Secret: []byte(o.SharedSecret), SelfID: o.SelfID})
		if err != nil {
			return nil, mode, fmt.Errorf("conduit peer auth (hmac): %w", err)
		}
		return a, mode, nil
	default:
		a, err := newOIDCPeerAuth(o)
		if err != nil {
			return nil, mode, err
		}
		return a, mode, nil
	}
}

// oidcPeerAuth authenticates relay peers with Google-signed ID tokens.
type oidcPeerAuth struct {
	audience string
	allowed  map[string]bool
	src      transportauth.TokenSource
	validate func(ctx context.Context, token, audience string) (*idtoken.Payload, error)
}

func newOIDCPeerAuth(o ConduitPeerAuthOptions) (*oidcPeerAuth, error) {
	aud := o.Audience
	if aud == "" {
		aud = DefaultConduitPeerAudience
	}
	accounts := o.ServiceAccounts
	if len(accounts) == 0 && o.OwnServiceAccount != "" {
		accounts = []string{o.OwnServiceAccount}
	}
	if len(accounts) == 0 {
		return nil, errors.New("conduit peer auth (oidc): no allowed service accounts; this node's service account is unknown, so set server.hub.conduit.peer_service_accounts")
	}
	allowed := make(map[string]bool, len(accounts))
	for _, a := range accounts {
		allowed[strings.ToLower(strings.TrimSpace(a))] = true
	}
	src := o.TokenSource
	if src == nil {
		s, err := adcsource.New(aud)
		if err != nil {
			return nil, fmt.Errorf("conduit peer auth (oidc): %w", err)
		}
		src = s
	}
	validate := o.Validate
	if validate == nil {
		validate = idtoken.Validate
	}
	return &oidcPeerAuth{audience: aud, allowed: allowed, src: src, validate: validate}, nil
}

// Sign attaches this node's ID token as a bearer token (the form Cloud Run
// invoker IAM also checks).
func (a *oidcPeerAuth) Sign(req *http.Request) error {
	tok, err := a.src.Token()
	if err != nil {
		return fmt.Errorf("conduit peer auth (oidc): mint ID token: %w", err)
	}
	if tok == "" {
		return errors.New("conduit peer auth (oidc): empty ID token")
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	return nil
}

// Verify validates the bearer ID token: Google signature, expiry, the
// relay-peer audience, a verified email and membership of the allow-list.
func (a *oidcPeerAuth) Verify(req *http.Request) (string, error) {
	h := req.Header.Get("Authorization")
	tok, ok := strings.CutPrefix(h, "Bearer ")
	if !ok || tok == "" {
		return "", relay.ErrPeerUnauthenticated
	}
	p, err := a.validate(req.Context(), tok, a.audience)
	if err != nil {
		return "", fmt.Errorf("%w: %v", relay.ErrPeerUnauthenticated, err)
	}
	email, _ := p.Claims["email"].(string)
	verified, _ := p.Claims["email_verified"].(bool)
	if email == "" || !verified {
		return "", fmt.Errorf("%w: ID token has no verified email", relay.ErrPeerUnauthenticated)
	}
	if !a.allowed[strings.ToLower(email)] {
		return "", fmt.Errorf("%w: service account %q is not an allowed relay peer", relay.ErrPeerUnauthenticated, email)
	}
	return email, nil
}
