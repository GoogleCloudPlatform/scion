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
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// googleValidatorAnswering returns a validator whose every upstream call is
// answered with status and body.
func googleValidatorAnswering(status int, body string) *googleCredentialValidator {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return httpJSONResponse(status, body), nil
	})}
	return NewGoogleCredentialValidator(client).(*googleCredentialValidator)
}

// A tokeninfo or userinfo answer that fails to decode carries the upstream
// value in its error only as upstreamText renders it. The flexible decoders
// quote the raw value in their error; a JSON string may carry raw bidi
// runes and any length. External bearer auth logs the error and answers
// fixed text (auth_external_bearer.go).
func TestGoogleValidatorDecodeErrorRendered(t *testing.T) {
	ctx := context.Background()
	// JSON forbids raw control characters in a string, so the planted value
	// carries the marker, a raw bidi override and the padding.
	value := upstreamMarker + "\u202e" + strings.Repeat("A", 8000)
	tests := []struct {
		name string
		body string
		call func(v *googleCredentialValidator) error
	}{
		{name: "tokeninfo", body: `{"expires_in":"` + value + `"}`, call: func(v *googleCredentialValidator) error {
			_, err := v.getTokenInfo(ctx, "token")
			return err
		}},
		{name: "userinfo", body: `{"email_verified":"` + value + `"}`, call: func(v *googleCredentialValidator) error {
			_, err := v.getUserInfo(ctx, "token")
			return err
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call(googleValidatorAnswering(http.StatusOK, tc.body))
			if !errors.Is(err, ErrGoogleUpstreamError) {
				t.Fatalf("err = %v, want ErrGoogleUpstreamError", err)
			}
			assertUpstreamTextRendered(t, err.Error())
		})
	}
}

// The body-level error field of a 200 tokeninfo answer is rendered too, and
// stays a credential verdict.
func TestGoogleValidatorTokenInfoErrorFieldRendered(t *testing.T) {
	body, err := json.Marshal(map[string]string{"error_description": upstreamMarkerBody})
	if err != nil {
		t.Fatal(err)
	}
	_, err = googleValidatorAnswering(http.StatusOK, string(body)).getTokenInfo(context.Background(), "token")
	if !errors.Is(err, ErrGoogleInvalidCredential) {
		t.Fatalf("err = %v, want ErrGoogleInvalidCredential", err)
	}
	assertUpstreamTextRendered(t, err.Error())
}

// External bearer auth answers a tokeninfo answer that fails to decode with
// its fixed 503 text; the upstream value reaches only the server log,
// rendered, in one record.
//
// Must not run in parallel: it swaps the default logger.
func TestExternalBearer_TokenInfoDecodeError_LoggedNotReturned(t *testing.T) {
	body, err := json.Marshal(map[string]string{"expires_in": upstreamMarkerBody})
	if err != nil {
		t.Fatal(err)
	}
	endpoints := newTestEndpoints(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(body)
		}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Error("userinfo must not be called when tokeninfo fails")
		}),
	)
	defer endpoints.close()
	// The config takes slog.Default() as its logger, so capture first.
	logs := captureUpstreamLog(t)
	resolver := NewGoogleIdentityResolver(newFakeUserStore(), newMemExtIDStore(), alwaysAuthorized, nil, slog.Default())
	cfg := newExternalBearerConfig(t, newTestValidator(endpoints), resolver)

	rec, result := doExternalBearerRequest(cfg, "opaque-access-token-decode")

	require.False(t, result.reached, "handler must not be reached")
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"upstream_unavailable"`)
	assert.Contains(t, rec.Body.String(), "external identity provider unavailable")
	assert.NotContains(t, rec.Body.String(), upstreamMarker)
	assertLoggedOnceRendered(t, logs, "External bearer: upstream verification unavailable")
	assert.Equal(t, 1, strings.Count(logs.String(), upstreamMarker), "the marker reaches exactly one log record")
}

// A JWKS answer that fails to decode carries the upstream value in its error
// only as upstreamText renders it. The JWK decoder quotes an unsupported EC
// "crv" value in its error, so the planted value goes there.
func TestGoogleJWKSDecodeErrorRendered(t *testing.T) {
	crv := upstreamMarker + string(rune(0x202e)) + strings.Repeat("A", 8000)
	body, err := json.Marshal(map[string]any{"keys": []map[string]string{{"kty": "EC", "crv": crv}}})
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return httpJSONResponse(http.StatusOK, string(body)), nil
	})}
	c := newGoogleJWKSCache(client)
	c.mu.Lock()
	_, err = c.fetchLocked(context.Background())
	c.mu.Unlock()
	if err == nil {
		t.Fatal("fetchLocked: want a decode error")
	}
	if !strings.Contains(err.Error(), "JWKS decode failed") {
		t.Fatalf("err = %v, want a JWKS decode error", err)
	}
	assertUpstreamTextRendered(t, err.Error())
}
