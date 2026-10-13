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

package hubprofile

import (
	"context"
	"crypto/subtle"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
)

// StatusIdentity describes the identity the session's bearer represents.
// Every field is decoded locally or read from the cache; no token.
type StatusIdentity struct {
	Email          string     `json:"email,omitempty"`
	Kind           string     `json:"kind"`
	Tier           string     `json:"tier,omitempty"`
	IdentityID     string     `json:"identityId,omitempty"`
	DelegationID   string     `json:"delegationId,omitempty"`
	ExpiresAt      *time.Time `json:"expiresAt,omitempty"`
	TokenExpiresAt *time.Time `json:"tokenExpiresAt,omitempty"`
}

// StatusTransport describes the transport credential seen on the wire.
type StatusTransport struct {
	Mode           string `json:"mode"`
	PrincipalEmail string `json:"principalEmail,omitempty"`
	Audience       string `json:"audience,omitempty"`
}

// StatusCheck is one gate check.
type StatusCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// StatusReport is the profile part of 'scion hub status'.
type StatusReport struct {
	Profile        string          `json:"profile"`
	Endpoint       string          `json:"endpoint"`
	Run            string          `json:"run,omitempty"`
	BearerSource   string          `json:"bearerSource"`
	Identity       StatusIdentity  `json:"identity"`
	Transport      StatusTransport `json:"transport"`
	IssuerAudience string          `json:"issuerAudience,omitempty"`
	HubStatusCode  int             `json:"hubStatusCode,omitempty"`
	Checks         []StatusCheck   `json:"checks"`
	OK             bool            `json:"ok"`
}

// Status probes the target hub with the session's credential and checks,
// on the request actually sent, that the bearer is the profile's own and
// the transport credential matches the profile: with transport iap, an ID
// token for the profile's IAP service account with exactly the profile's
// IAP audience; with transport none, no transport token. The probe is
// GET /api/v1/auth/me for an alias, and GET /api/v1/test-identities
// (limit 1) for the issuer, the only routes the workload credential is
// accepted on.
func (s *Session) Status(ctx context.Context) *StatusReport {
	p := s.opts.Profile
	r := &StatusReport{
		Profile:      p.Name,
		Endpoint:     p.Endpoint,
		Run:          s.opts.Run,
		BearerSource: s.BearerSource(),
		Transport:    StatusTransport{Mode: p.Transport},
	}
	add := func(name string, ok bool, detail string) {
		r.Checks = append(r.Checks, StatusCheck{Name: name, OK: ok, Detail: detail})
	}

	alias := s.opts.Alias != ""
	path := "/api/v1/test-identities?limit=1"
	if alias {
		path = "/api/v1/auth/me"
	} else if aud, err := s.IssuerAudience(ctx); err == nil {
		r.IssuerAudience = aud
	}

	// The expected bearer, from the profile alone.
	expected, _, bearerErr := s.bearer(ctx, alias)
	if bearerErr != nil {
		add("profile credential", false, bearerErr.Error())
	}
	r.Identity = s.identityOf(expected)

	s.recorder.take()
	if bearerErr == nil {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.Endpoint+path, nil)
		if err == nil {
			var resp *http.Response
			resp, err = s.HTTPClient().Do(req)
			if err == nil {
				_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
				_ = resp.Body.Close()
				r.HubStatusCode = resp.StatusCode
				add("hub accepts the credential", resp.StatusCode == http.StatusOK, fmt.Sprintf("GET %s returned %d", strings.SplitN(path, "?", 2)[0], resp.StatusCode))
			}
		}
		if err != nil {
			add("hub reachable", false, err.Error())
		}
	}
	wire := s.recorder.take()
	r.Checks = append(r.Checks, evaluateWire(p, expected, wire, &r.Transport)...)

	r.OK = len(r.Checks) > 0
	for _, c := range r.Checks {
		if !c.OK {
			r.OK = false
		}
	}
	return r
}

// evaluateWire checks the facts recorded from the request sent to the
// target against the profile. It fills t with the transport principal.
func evaluateWire(p *Profile, expectedBearer string, wire wireFacts, t *StatusTransport) []StatusCheck {
	var out []StatusCheck
	if !wire.seen {
		return append(out, StatusCheck{Name: "bearer from profile", OK: false, Detail: "no request reached the target"})
	}
	bearerOK := expectedBearer != "" && wire.authorization != "" &&
		subtle.ConstantTimeCompare([]byte(expectedBearer), []byte(wire.authorization)) == 1 && !wire.otherCredential
	detail := "the request carried the profile's bearer and no other credential"
	if !bearerOK {
		detail = "the request did not carry the profile's bearer"
		if wire.otherCredential {
			detail = "the request carried another credential header"
		}
	}
	out = append(out, StatusCheck{Name: "bearer from profile", OK: bearerOK, Detail: detail})

	switch p.Transport {
	case TransportIAP:
		if wire.transport == "" {
			return append(out, StatusCheck{Name: "transport principal", OK: false, Detail: "no transport token on the request"})
		}
		c, err := transportauth.DecodeIDTokenClaims(wire.transport)
		if err != nil {
			return append(out, StatusCheck{Name: "transport principal", OK: false, Detail: err.Error()})
		}
		t.PrincipalEmail = c.Email
		t.Audience = strings.Join(c.Audiences, ",")
		ok := strings.EqualFold(c.Email, p.UATServiceAccount) && c.HasAudience(p.IAPAudience) && len(c.Audiences) == 1
		d := "the transport principal is the profile's IAP service account"
		if !ok {
			d = fmt.Sprintf("the transport principal is %q for audience %q, not the profile's %q for %q", c.Email, t.Audience, p.UATServiceAccount, p.IAPAudience)
		}
		out = append(out, StatusCheck{Name: "transport principal", OK: ok, Detail: d})
	default:
		ok := wire.transport == ""
		d := "no transport token (transport none)"
		if !ok {
			d = "a transport token was sent although the profile has transport none"
		}
		out = append(out, StatusCheck{Name: "transport principal", OK: ok, Detail: d})
	}
	return out
}

// identityOf describes the identity behind token (decoded locally).
func (s *Session) identityOf(token string) StatusIdentity {
	if e := s.Entry(); e != nil {
		id := StatusIdentity{Email: e.Email, Kind: e.Kind, Tier: e.Role, IdentityID: e.IdentityID, DelegationID: e.DelegationID}
		exp, texp := e.ExpiresAt, e.TokenExpiresAt
		id.ExpiresAt, id.TokenExpiresAt = &exp, &texp
		if token != "" {
			if c, err := DecodeHubTokenClaims(token); err == nil {
				if c.Email != "" {
					id.Email = c.Email
				}
				if !c.Expiry.IsZero() {
					te := c.Expiry.UTC()
					id.TokenExpiresAt = &te
				}
			}
		}
		return id
	}
	id := StatusIdentity{Kind: "workload issuer", Tier: TierMember, DelegationID: s.opts.Profile.MemberDelegation(), Email: s.opts.Profile.IssuerServiceAccount}
	if token != "" {
		if c, err := transportauth.DecodeIDTokenClaims(token); err == nil {
			if c.Email != "" {
				id.Email = c.Email
			}
			if !c.Expiry.IsZero() {
				te := c.Expiry.UTC()
				id.TokenExpiresAt = &te
			}
		}
	}
	return id
}
