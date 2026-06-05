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
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"google.golang.org/api/iamcredentials/v1"
	"google.golang.org/api/option"
)

// TransportTokenMinter mints Google OIDC ID tokens for the transport layer.
// The hub uses this to issue tokens that let agents traverse platform guards
// (IAP or Cloud Run invoker) on outbound requests.
type TransportTokenMinter interface {
	// MintIDToken mints a Google OIDC ID token for the given audience.
	// Returns the token string, its expiry time, and any error.
	MintIDToken(ctx context.Context, audience string) (token string, expiry time.Time, err error)
}

// RefreshTokenEntry represents a single token in the generalized refresh response.
// Used in both the refresh endpoint response and internally for dispatch payload construction.
type RefreshTokenEntry struct {
	Layer     string `json:"layer"`               // "app" | "transport"
	Type      string `json:"type"`                // "scion_access" | "scion_refresh" | "google_oidc"
	Value     string `json:"value"`               // the token value
	ExpiresIn int    `json:"expiresIn"`           // seconds until expiry
	Audience  string `json:"audience,omitempty"`  // only for transport tokens
}

// noopTransportMinter is used when transport auth is disabled (mode == "none").
// It always returns an error indicating transport auth is not configured.
type noopTransportMinter struct{}

func (m *noopTransportMinter) MintIDToken(_ context.Context, _ string) (string, time.Time, error) {
	return "", time.Time{}, fmt.Errorf("transport auth is disabled (mode=none)")
}

// gcpTransportMinter mints Google OIDC ID tokens by impersonating a dedicated
// service account via the IAM Credentials API (generateIdToken).
// The hub's runtime SA must hold serviceAccountTokenCreator on the target SA.
type gcpTransportMinter struct {
	// serviceAccountEmail is the email of the SA to impersonate.
	serviceAccountEmail string
	// iamEndpoint overrides the IAM Credentials API endpoint (for testing).
	// Empty uses the default Google endpoint.
	iamEndpoint string

	// svcOnce guards lazy initialization of the cached IAM credentials service.
	svcOnce sync.Once
	svc     *iamcredentials.Service
	svcErr  error
}

// NewGCPTransportMinter creates a new GCP transport token minter.
// serviceAccountEmail is the dedicated platform-auth SA to impersonate.
// iamEndpoint overrides the IAM Credentials API endpoint (empty uses the default).
func NewGCPTransportMinter(serviceAccountEmail, iamEndpoint string) *gcpTransportMinter {
	return &gcpTransportMinter{
		serviceAccountEmail: serviceAccountEmail,
		iamEndpoint:         iamEndpoint,
	}
}

// getOrCreateService lazily creates and caches the IAM credentials service client.
// Uses context.Background() for the long-lived client; per-call ctx is passed to .Do().
func (m *gcpTransportMinter) getOrCreateService() (*iamcredentials.Service, error) {
	m.svcOnce.Do(func() {
		var opts []option.ClientOption
		if m.iamEndpoint != "" {
			opts = append(opts, option.WithEndpoint(m.iamEndpoint))
		}
		m.svc, m.svcErr = iamcredentials.NewService(context.Background(), opts...)
	})
	return m.svc, m.svcErr
}

// MintIDToken impersonates the configured SA to mint a Google OIDC ID token
// with the given audience via the IAM Credentials API.
func (m *gcpTransportMinter) MintIDToken(ctx context.Context, audience string) (string, time.Time, error) {
	if m.serviceAccountEmail == "" {
		return "", time.Time{}, fmt.Errorf("transport minter: service account email not configured")
	}

	svc, err := m.getOrCreateService()
	if err != nil {
		return "", time.Time{}, fmt.Errorf("transport minter: failed to create IAM credentials client: %w", err)
	}

	name := fmt.Sprintf("projects/-/serviceAccounts/%s", m.serviceAccountEmail)
	req := &iamcredentials.GenerateIdTokenRequest{
		Audience:     audience,
		IncludeEmail: true,
	}

	resp, err := svc.Projects.ServiceAccounts.GenerateIdToken(name, req).Context(ctx).Do()
	if err != nil {
		return "", time.Time{}, fmt.Errorf("transport minter: generateIdToken failed: %w", err)
	}

	if resp.Token == "" {
		return "", time.Time{}, fmt.Errorf("transport minter: empty token in response")
	}

	// Parse expiry from the JWT token
	expiry, err := parseJWTExpiry(resp.Token)
	if err != nil {
		// Fall back to 1 hour default TTL if we can't parse the expiry
		expiry = time.Now().Add(1 * time.Hour)
	}

	return resp.Token, expiry, nil
}

// parseJWTExpiry extracts the expiry time from a JWT without validating the signature.
// This is safe for scheduling purposes since the token will be validated by the platform.
func parseJWTExpiry(tokenString string) (time.Time, error) {
	parts := strings.Split(tokenString, ".")
	if len(parts) != 3 {
		return time.Time{}, fmt.Errorf("invalid JWT format: expected 3 parts, got %d", len(parts))
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, fmt.Errorf("failed to decode JWT payload: %w", err)
	}

	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return time.Time{}, fmt.Errorf("failed to parse JWT claims: %w", err)
	}

	if claims.Exp == 0 {
		return time.Time{}, fmt.Errorf("token has no expiry claim")
	}

	return time.Unix(claims.Exp, 0), nil
}

// fakeTransportMinter is a test double for TransportTokenMinter.
// Exported for use in other test packages.
type FakeTransportMinter struct {
	Token     string
	Expiry    time.Time
	Err       error
	CallCount int
}

func (f *FakeTransportMinter) MintIDToken(_ context.Context, _ string) (string, time.Time, error) {
	f.CallCount++
	return f.Token, f.Expiry, f.Err
}
