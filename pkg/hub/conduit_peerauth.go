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
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth/adcsource"
	"google.golang.org/api/idtoken"
)

// Relay-peer authentication for the internal relay API (design v2.4 §3.2,
// §3.10): a service identity, never an end-user credential.
//
// Relay-peer requests are signed in all modes. Every request carries an
// HMAC signature over the method, request URI, timestamp, nonce, caller id,
// body digest and Want header, keyed with an HKDF derivation of the hub's
// shared signing secret (relay.NewHMACPeerAuthFromSecret), and the serving
// relay refuses a stale timestamp or a replayed nonce. On GCP (peer_auth
// "oidc", or "auto" on GCP) the caller also presents a Google-signed OIDC
// ID token for its service account, checked in addition to the signature.
//
// Both checks fail closed: a request missing either configured credential,
// or with one that does not verify, is refused. There is no fallback from
// one mechanism to the other.

// DefaultConduitPeerAudience is the OIDC audience of relay-peer ID tokens.
// It is a fixed string, not derived from the hub id or the node, so every
// replica mints and expects the same audience.
const DefaultConduitPeerAudience = "scion-conduit-relay-peer"

// ConduitPeerAuthOptions selects and configures relay-peer auth.
type ConduitPeerAuthOptions struct {
	// Mode is config.ConduitPeerAuth{Auto,OIDC,HMAC} ("" = auto).
	Mode string
	// OnGCP reports whether this node runs on GCP (auto adds OIDC).
	OnGCP bool
	// SelfID is this relay's instance id (HMAC caller identity).
	SelfID string
	// SharedSecret is the hub's shared signing secret, from which the HMAC
	// key is derived. Required in every mode.
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

// ResolveConduitPeerAuthMode returns the effective mode: hmac (signed
// requests) or oidc (signed requests plus an OIDC ID token).
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
// selected mode. Every mode signs requests with HMAC; oidc adds the ID
// token check.
func NewConduitPeerAuth(o ConduitPeerAuthOptions) (relay.PeerAuth, string, error) {
	mode, err := ResolveConduitPeerAuthMode(o.Mode, o.OnGCP)
	if err != nil {
		return nil, "", err
	}
	if o.SharedSecret == "" {
		return nil, mode, fmt.Errorf("conduit peer auth (%s): no shared signing secret; set --session-secret or SCION_SERVER_SESSION_SECRET on every hub node", mode)
	}
	signer, err := relay.NewHMACPeerAuthFromSecret(relay.HMACPeerAuthConfig{Secret: []byte(o.SharedSecret), SelfID: o.SelfID})
	if err != nil {
		return nil, mode, fmt.Errorf("conduit peer auth (%s): %w", mode, err)
	}
	if mode == config.ConduitPeerAuthHMAC {
		return signer, mode, nil
	}
	id, err := newOIDCPeerAuth(o)
	if err != nil {
		return nil, mode, err
	}
	return &signedOIDCPeerAuth{signer: signer, oidc: id}, mode, nil
}

// signedOIDCPeerAuth requires both the HMAC request signature and the
// OIDC ID token on every request.
type signedOIDCPeerAuth struct {
	signer *relay.HMACPeerAuth
	oidc   *oidcPeerAuth
}

// Sign implements relay.PeerAuth.
func (a *signedOIDCPeerAuth) Sign(req *http.Request) error {
	if err := a.signer.Sign(req); err != nil {
		return err
	}
	return a.oidc.Sign(req)
}

// Verify implements relay.PeerAuth. The peer id is the signed caller id
// (the relay instance id); the ID token must belong to an allowed service
// account.
func (a *signedOIDCPeerAuth) Verify(req *http.Request) (string, error) {
	peer, err := a.signer.Verify(req)
	if err != nil {
		return "", err
	}
	if _, err := a.oidc.Verify(req); err != nil {
		return "", err
	}
	return peer, nil
}

// conduitDefaultSAWarnOnce limits the default compute service-account
// warning to once per process.
var conduitDefaultSAWarnOnce sync.Once

// isDefaultComputeServiceAccount reports whether email is a Compute Engine
// default service account (PROJECT_NUMBER-compute@developer.gserviceaccount.com).
func isDefaultComputeServiceAccount(email string) bool {
	return strings.HasSuffix(strings.ToLower(strings.TrimSpace(email)), "-compute@developer.gserviceaccount.com")
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
		if isDefaultComputeServiceAccount(o.OwnServiceAccount) {
			conduitDefaultSAWarnOnce.Do(func() {
				slog.Warn("Conduit relay-peer OIDC allow-list defaults to this node's service account, which is a Compute Engine default service account; "+
					"run the hub as a dedicated service account or set server.hub.conduit.peer_service_accounts explicitly",
					"service_account", o.OwnServiceAccount)
			})
		}
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
