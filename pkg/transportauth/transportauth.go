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

// Package transportauth provides shared transport-layer OIDC authentication
// for all hub-bound HTTP and WebSocket paths. It extracts and generalises
// the proven implementation from pkg/sciontool/hub/oidc.go so that every
// client stack in the repo can attach a Google OIDC identity token.
package transportauth

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"cloud.google.com/go/compute/metadata"
)

const (
	// EnvTransportToken is the env var for the hub-provided transport OIDC token.
	EnvTransportToken = "SCION_TRANSPORT_TOKEN"

	// EnvTransportAudience is the env var for the transport token audience.
	EnvTransportAudience = "SCION_TRANSPORT_AUDIENCE"

	// EnvTransportTokenExpiry is the env var for the transport token expiry.
	EnvTransportTokenExpiry = "SCION_TRANSPORT_TOKEN_EXPIRY"

	// EnvHubOIDCAudience is the legacy env var for the OIDC audience.
	EnvHubOIDCAudience = "SCION_HUB_OIDC_AUDIENCE"

	// EnvTransportMode selects the header placement mode (iap, cloudrun_invoker).
	EnvTransportMode = "SCION_TRANSPORT_MODE"

	// EnvMetadataMode is checked to detect whether the scion metadata server
	// has hijacked 169.254.169.254, making the real GCE metadata server
	// unreachable.
	EnvMetadataMode = "SCION_METADATA_MODE"

	RefreshMargin = 5 * time.Minute
	DefaultTTL    = 1 * time.Hour
	FetchTimeout  = 2 * time.Second

	gcpMetadataBaseURL = "http://metadata.google.internal"
)

// HeaderMode controls which HTTP header carries the transport OIDC token.
type HeaderMode int

const (
	// HeaderAuthorization sets Authorization: Bearer <oidc> only if the header
	// is empty. This is the default and covers agent/broker paths where the
	// app-layer credential uses a custom header.
	HeaderAuthorization HeaderMode = iota

	// HeaderProxyAuthorization sends the OIDC token as Proxy-Authorization.
	// Google IAP supports this for cases where Authorization is occupied by
	// an app-layer credential, and strips the header before forwarding.
	HeaderProxyAuthorization

	// HeaderServerlessAuthorization sends X-Serverless-Authorization, which is
	// Cloud Run's equivalent when the guard is invoker IAM rather than IAP.
	HeaderServerlessAuthorization
)

// TokenSource yields transport-layer Google OIDC ID tokens. Thread-safe.
type TokenSource interface {
	// Token returns a valid OIDC token, refreshing if necessary.
	Token() (string, error)
	// SetToken updates the cached token and expiry. Used by refresh paths
	// (hub tokens[] array) to push new tokens in.
	SetToken(token string, expiry time.Time)
	// Expiry returns the current token expiry (zero if unknown).
	Expiry() time.Time
}

// IsOnGCEFunc detects whether we're running on GCP. Override in tests.
var IsOnGCEFunc = func() bool { return metadata.OnGCE() }

// ParseTokenExpiry extracts the expiry time from a JWT token without
// validating the signature. This is safe for scheduling purposes since
// the Hub will validate the token on each request.
func ParseTokenExpiry(tokenString string) (time.Time, error) {
	parts := strings.Split(tokenString, ".")
	if len(parts) != 3 {
		return time.Time{}, fmt.Errorf("invalid JWT format: expected 3 parts, got %d", len(parts))
	}

	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
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

// ModeFromString converts a transport mode string to a HeaderMode constant.
// Returns HeaderAuthorization for empty or unrecognised values.
func ModeFromString(mode string) HeaderMode {
	switch mode {
	case "iap":
		return HeaderProxyAuthorization
	case "cloudrun_invoker":
		return HeaderServerlessAuthorization
	default:
		return HeaderAuthorization
	}
}

// ModeFromEnv reads SCION_TRANSPORT_MODE and returns the corresponding
// HeaderMode. Returns HeaderAuthorization when unset or unrecognised.
func ModeFromEnv() HeaderMode {
	return ModeFromString(os.Getenv(EnvTransportMode))
}

// FromEnv resolves a TokenSource from environment variables. Returns
// (nil, nil) when transport auth is not configured — callers then behave
// exactly as before this change.
//
// Resolution order:
//  1. SCION_TRANSPORT_TOKEN set → InjectedSource
//  2. On GCE && SCION_METADATA_MODE unset && audience configured
//     (SCION_TRANSPORT_AUDIENCE or SCION_HUB_OIDC_AUDIENCE) → MetadataSource
//  3. Otherwise → nil (no transport auth)
func FromEnv() (TokenSource, error) {
	if tok := os.Getenv(EnvTransportToken); tok != "" {
		source := NewInjectedSource()
		expiry, err := ParseTokenExpiry(tok)
		if err != nil {
			expiry = time.Now().Add(DefaultTTL)
		}
		source.SetToken(tok, expiry)
		return source, nil
	}

	if !IsOnGCEFunc() {
		return nil, nil
	}
	if mode := os.Getenv(EnvMetadataMode); mode != "" {
		return nil, nil
	}

	audience := os.Getenv(EnvTransportAudience)
	if audience == "" {
		audience = os.Getenv(EnvHubOIDCAudience)
	}
	if audience == "" {
		return nil, nil
	}

	return NewMetadataSource(audience), nil
}

// TransportSettings holds transport auth settings read from settings.yaml.
type TransportSettings struct {
	Mode     string
	Audience string
}

// FromSettings resolves a TokenSource from settings when env vars are absent.
// The adcNew constructor is optional — when nil, ADC-backed sources are not
// available (keeping the sciontool binary lean).
//
// Resolution order:
//  1. SCION_TRANSPORT_TOKEN set → InjectedSource (env always wins)
//  2. On GCE && SCION_METADATA_MODE unset && audience available → MetadataSource
//  3. Settings audience + adcNew → ADCSource
//  4. Otherwise → nil
func FromSettings(settings *TransportSettings, adcNew ADCSourceConstructor) (TokenSource, HeaderMode, error) {
	// Env vars always take precedence.
	src, err := FromEnv()
	if err != nil {
		return nil, HeaderAuthorization, err
	}
	if src != nil {
		return src, ModeFromEnv(), nil
	}

	// Check settings when env vars produced nothing.
	if settings == nil || settings.Audience == "" {
		return nil, HeaderAuthorization, nil
	}

	// Env-var mode takes precedence over settings mode.
	mode := ModeFromEnv()
	if envMode := os.Getenv(EnvTransportMode); envMode == "" && settings.Mode != "" {
		mode = ModeFromString(settings.Mode)
	}

	// Try metadata server first when on GCE.
	if IsOnGCEFunc() {
		if metaMode := os.Getenv(EnvMetadataMode); metaMode == "" {
			return NewMetadataSource(settings.Audience), mode, nil
		}
	}

	// Fall back to ADC.
	if adcNew != nil {
		adcSrc, err := adcNew(settings.Audience)
		if err != nil {
			return nil, HeaderAuthorization, err
		}
		return adcSrc, mode, nil
	}

	return nil, HeaderAuthorization, nil
}

// Wrap returns rt wrapped so that each outgoing request carries the
// transport OIDC token in the header specified by mode.
func Wrap(rt http.RoundTripper, src TokenSource, mode HeaderMode) http.RoundTripper {
	if rt == nil {
		rt = http.DefaultTransport
	}
	return &roundTripper{
		base:   rt,
		source: src,
		mode:   mode,
	}
}

// ApplyHeaders sets the transport token on h — for WebSocket dialers and
// other non-RoundTripper paths.
func ApplyHeaders(h http.Header, src TokenSource, mode HeaderMode) error {
	tok, err := src.Token()
	if err != nil {
		return err
	}
	applyToken(h, tok, mode)
	return nil
}

func applyToken(h http.Header, tok string, mode HeaderMode) {
	bearer := "Bearer " + tok
	switch mode {
	case HeaderProxyAuthorization:
		h.Set("Proxy-Authorization", bearer)
	case HeaderServerlessAuthorization:
		h.Set("X-Serverless-Authorization", bearer)
	default:
		if h.Get("Authorization") == "" {
			h.Set("Authorization", bearer)
		}
	}
}

// roundTripper is an http.RoundTripper that injects a transport OIDC token.
type roundTripper struct {
	base   http.RoundTripper
	source TokenSource
	mode   HeaderMode
}

func (t *roundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.mode == HeaderAuthorization && req.Header.Get("Authorization") != "" {
		return t.base.RoundTrip(req)
	}
	tok, err := t.source.Token()
	if err != nil {
		slog.Debug("OIDC transport token fetch failed, proceeding without header", "error", err)
		return t.base.RoundTrip(req)
	}
	req = req.Clone(req.Context())
	applyToken(req.Header, tok, t.mode)
	return t.base.RoundTrip(req)
}
