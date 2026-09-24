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
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// ---------------------------------------------------------------------------
// A load-time diagnostic: a Google issuer configured as issuer_type "user"
// with an empty expected_audience leaves the external-bearer path disabled
// for that issuer (googleTrust). warnIfExternalBearerDisabled
// (federation_auth.go) logs that once per NewFederationAuthenticator call —
// i.e. once at initial load and once per hot reload, since both call sites
// (server.go, operational_settings.go) construct a new authenticator — and
// never on the request path.
// ---------------------------------------------------------------------------

const externalBearerDisabledWarningMsg = `external-bearer path disabled for this issuer: issuer_type is "user" but expected_audience is empty`

// federationAuthCaptureBuffer builds a *slog.Logger that writes JSON lines to
// a buffer, so a test can assert on NewFederationAuthenticator's load-time
// log output directly, without touching the global default logger (the
// function takes its logger as an explicit parameter).
func federationAuthCaptureBuffer() (*slog.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

// countWarnLines returns how many JSON log lines in buf are level WARN with
// the given message.
func countWarnLines(t *testing.T, buf *bytes.Buffer, msg string) int {
	t.Helper()
	count := 0
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("unmarshal log line %q: %v", line, err)
		}
		if rec["level"] == "WARN" && rec["msg"] == msg {
			count++
		}
	}
	return count
}

// newFederationAuthWithLogger builds a FederationAuthenticator with a single
// trusted issuer, using the given logger — like newGoogleFederationAuthWithIssuerType
// (auth_external_bearer_test.go), but with a caller-supplied logger instead of
// slog.Default(), so a test can capture NewFederationAuthenticator's own log
// output in isolation.
func newFederationAuthWithLogger(t *testing.T, issuerURL, issuerType, expectedAudience, jwksURL string, log *slog.Logger) *FederationAuthenticator {
	t.Helper()
	fedCfg := config.FederationConfig{
		Enabled: true,
		TrustedIssuers: []config.TrustedIssuerConfig{
			{
				IssuerURL:        issuerURL,
				JWKSURL:          jwksURL,
				ExpectedAudience: expectedAudience,
				IssuerType:       issuerType,
			},
		},
	}
	fa, err := NewFederationAuthenticator(fedCfg, "https://hub.example.com", http.DefaultClient, "hosted", log)
	if err != nil {
		t.Fatalf("NewFederationAuthenticator: %v", err)
	}
	return fa
}

func TestNewFederationAuthenticator_GoogleUserEmptyAudience_WarnsOnce(t *testing.T) {
	log, buf := federationAuthCaptureBuffer()
	newFederationAuthWithLogger(t, googleIssuerHTTPS, "user", "", "http://unused.invalid/jwks", log)

	if got := countWarnLines(t, buf, externalBearerDisabledWarningMsg); got != 1 {
		t.Errorf("warning count after one load = %d, want 1; log:\n%s", got, buf.String())
	}
	if !strings.Contains(buf.String(), googleIssuerHTTPS) {
		t.Errorf("warning does not name the issuer_url; log:\n%s", buf.String())
	}
}

// TestNewFederationAuthenticator_GoogleUserEmptyAudience_WarnsAgainOnReload
// simulates the hot-reload path (operational_settings.go calls
// NewFederationAuthenticator again with the same shape of config on every
// reload): the warning must fire again, not just once ever.
func TestNewFederationAuthenticator_GoogleUserEmptyAudience_WarnsAgainOnReload(t *testing.T) {
	log, buf := federationAuthCaptureBuffer()

	newFederationAuthWithLogger(t, googleIssuerHTTPS, "user", "", "http://unused.invalid/jwks", log)
	if got := countWarnLines(t, buf, externalBearerDisabledWarningMsg); got != 1 {
		t.Fatalf("warning count after first load = %d, want 1", got)
	}

	// Simulated reload: a second, independent NewFederationAuthenticator call
	// with the same still-misconfigured issuer.
	newFederationAuthWithLogger(t, googleIssuerHTTPS, "user", "", "http://unused.invalid/jwks", log)
	if got := countWarnLines(t, buf, externalBearerDisabledWarningMsg); got != 2 {
		t.Errorf("warning count after reload = %d, want 2 (once per load); log:\n%s", got, buf.String())
	}
}

func TestNewFederationAuthenticator_GoogleUserWithAudience_NoWarning(t *testing.T) {
	log, buf := federationAuthCaptureBuffer()
	newFederationAuthWithLogger(t, googleIssuerHTTPS, "user", "client-id.apps.googleusercontent.com", "http://unused.invalid/jwks", log)

	if got := countWarnLines(t, buf, externalBearerDisabledWarningMsg); got != 0 {
		t.Errorf("warning count for a correctly configured issuer = %d, want 0; log:\n%s", got, buf.String())
	}
}

func TestNewFederationAuthenticator_NonGoogleUserEmptyAudience_NoWarning(t *testing.T) {
	log, buf := federationAuthCaptureBuffer()
	newFederationAuthWithLogger(t, "https://firebase.example.com", "user", "", "https://firebase.example.com/jwks", log)

	if got := countWarnLines(t, buf, externalBearerDisabledWarningMsg); got != 0 {
		t.Errorf("warning count for a non-Google issuer = %d, want 0; log:\n%s", got, buf.String())
	}
}

func TestNewFederationAuthenticator_GoogleServiceAccountEmptyAudience_NoWarning(t *testing.T) {
	log, buf := federationAuthCaptureBuffer()
	newFederationAuthWithLogger(t, googleIssuerHTTPS, "service_account", "", "http://unused.invalid/jwks", log)

	if got := countWarnLines(t, buf, externalBearerDisabledWarningMsg); got != 0 {
		t.Errorf("warning count for issuer_type service_account = %d, want 0; log:\n%s", got, buf.String())
	}
}

// TestExternalBearer_DisabledPathRequests_NoAddedWarnings proves the warning
// is a load-time diagnostic only: N requests through the real
// UnifiedAuthMiddleware, all landing on the disabled external-bearer path
// (googleTrust returns not-ok, since expected_audience is empty), must not
// add any further warnings beyond the one NewFederationAuthenticator already
// logged at construction.
func TestExternalBearer_DisabledPathRequests_NoAddedWarnings(t *testing.T) {
	log, buf := federationAuthCaptureBuffer()
	fa := newFederationAuthWithLogger(t, googleIssuerHTTPS, "user", "", "http://unused.invalid/jwks", log)
	if got := countWarnLines(t, buf, externalBearerDisabledWarningMsg); got != 1 {
		t.Fatalf("warning count after construction = %d, want 1", got)
	}

	cfg := AuthConfig{
		Mode:           "production",
		FederationAuth: federationAuthPointer(fa),
		// The same capture logger as the constructor call above, not
		// slog.Default(): a regression that logged the warning on the
		// request path via cfg.Logger (rather than only from
		// NewFederationAuthenticator) must still be caught by this test.
		Logger: log,
	}
	for i := 0; i < 5; i++ {
		doExternalBearerRequest(cfg, "not-a-jwt-opaque-token")
	}

	if got := countWarnLines(t, buf, externalBearerDisabledWarningMsg); got != 1 {
		t.Errorf("warning count after 5 requests = %d, want still 1 (load-time only); log:\n%s", got, buf.String())
	}
}

// erroringRoundTripper fails every request, so JWKS OIDC discovery fails fast
// and deterministically, without a real network call.
type erroringRoundTripper struct{}

func (erroringRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("network disabled for this test")
}

// TestNewFederationAuthenticator_ConstructionFails_NoWarning proves the
// warning is logged only for an issuer that is actually going to be stored,
// not merely inspected: a Google user issuer with an empty expected_audience
// (which would otherwise warn) whose JWKS discovery also fails must not log
// the warning, since that issuer never takes effect and NewFederationAuthenticator
// returns an error for the whole config.
func TestNewFederationAuthenticator_ConstructionFails_NoWarning(t *testing.T) {
	log, buf := federationAuthCaptureBuffer()
	fedCfg := config.FederationConfig{
		Enabled: true,
		TrustedIssuers: []config.TrustedIssuerConfig{
			{
				IssuerURL:  googleIssuerHTTPS,
				IssuerType: "user",
				// JWKSURL deliberately empty: forces OIDC discovery, which
				// fails via the erroring transport below.
			},
		},
	}
	_, err := NewFederationAuthenticator(fedCfg, "https://hub.example.com",
		&http.Client{Transport: erroringRoundTripper{}}, "hosted", log)
	if err == nil {
		t.Fatal("expected NewFederationAuthenticator to fail (JWKS discovery error)")
	}

	if got := countWarnLines(t, buf, externalBearerDisabledWarningMsg); got != 0 {
		t.Errorf("warning count for an issuer whose construction failed = %d, want 0; log:\n%s", got, buf.String())
	}
}

// TestNewFederationAuthenticator_MultiIssuer_MisconfiguredBeforeFailing_NoWarning
// is the multi-issuer shape of the test above: the disabled-shaped Google
// user issuer comes FIRST in the list, followed by a second issuer that
// fails construction. Collecting warnings and only emitting them after the
// whole loop succeeds (rather than per issuer, inline) is what makes this
// case behave the same as the single-issuer one — logging the first
// issuer's warning as soon as it is seen, before the second issuer's later
// failure is known, would have logged a warning about a config that was
// then rejected in full.
func TestNewFederationAuthenticator_MultiIssuer_MisconfiguredBeforeFailing_NoWarning(t *testing.T) {
	log, buf := federationAuthCaptureBuffer()
	fedCfg := config.FederationConfig{
		Enabled: true,
		TrustedIssuers: []config.TrustedIssuerConfig{
			{
				IssuerURL:  googleIssuerHTTPS,
				IssuerType: "user",
				JWKSURL:    "http://unused.invalid/jwks",
				// ExpectedAudience deliberately empty: this issuer alone
				// would warn.
			},
			{
				// issuer_type defaults to "hub"; an http issuer_url is
				// rejected outright in "hosted" mode, before JWKS
				// resolution, so no network call is needed to fail this
				// issuer deterministically.
				IssuerURL: "http://insecure.example.com",
			},
		},
	}
	_, err := NewFederationAuthenticator(fedCfg, "https://hub.example.com", http.DefaultClient, "hosted", log)
	if err == nil {
		t.Fatal("expected NewFederationAuthenticator to fail (second issuer uses http in hosted mode)")
	}

	if got := countWarnLines(t, buf, externalBearerDisabledWarningMsg); got != 0 {
		t.Errorf("warning count when a later issuer fails construction = %d, want 0 (whole config rejected); log:\n%s", got, buf.String())
	}
}
