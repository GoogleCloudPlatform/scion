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

// Package runtimebroker provides the Scion Runtime Broker API server.
package runtimebroker

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/ptone/scion-agent/pkg/apiclient"
)

// BrokerAuthConfig configures host-side HMAC authentication.
type BrokerAuthConfig struct {
	// Enabled controls whether authentication is enforced.
	Enabled bool
	// MaxClockSkew is the maximum allowed time difference between client and server.
	MaxClockSkew time.Duration
	// SecretKey is the shared secret for HMAC verification.
	SecretKey []byte
	// AllowUnauthenticated allows requests without HMAC headers to pass through.
	// This is useful for development or when mixing authenticated and unauthenticated endpoints.
	AllowUnauthenticated bool
}

// DefaultBrokerAuthConfig returns the default broker authentication configuration.
func DefaultBrokerAuthConfig() BrokerAuthConfig {
	return BrokerAuthConfig{
		Enabled:              false,
		MaxClockSkew:         5 * time.Minute,
		AllowUnauthenticated: true,
	}
}

// BrokerAuthMiddleware provides HMAC-based authentication for incoming requests.
// This verifies that requests from the Hub are properly signed.
type BrokerAuthMiddleware struct {
	config BrokerAuthConfig
}

// NewBrokerAuthMiddleware creates a new broker authentication middleware.
func NewBrokerAuthMiddleware(cfg BrokerAuthConfig) *BrokerAuthMiddleware {
	return &BrokerAuthMiddleware{config: cfg}
}

// Middleware returns an HTTP middleware handler that validates HMAC signatures.
func (m *BrokerAuthMiddleware) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !m.config.Enabled {
			next.ServeHTTP(w, r)
			return
		}

		// Extract HMAC headers
		brokerID := r.Header.Get(apiclient.HeaderBrokerID)
		timestamp := r.Header.Get(apiclient.HeaderTimestamp)
		nonce := r.Header.Get(apiclient.HeaderNonce)
		signature := r.Header.Get(apiclient.HeaderSignature)

		// If no HMAC headers present, check if unauthenticated requests are allowed
		if brokerID == "" && timestamp == "" && signature == "" {
			if m.config.AllowUnauthenticated {
				next.ServeHTTP(w, r)
				return
			}
			m.writeError(w, "missing authentication headers")
			return
		}

		// Validate required headers are all present
		if brokerID == "" {
			m.writeError(w, "missing X-Scion-Broker-ID header")
			return
		}
		if timestamp == "" {
			m.writeError(w, "missing X-Scion-Timestamp header")
			return
		}
		if signature == "" {
			m.writeError(w, "missing X-Scion-Signature header")
			return
		}

		// Validate timestamp
		ts, err := strconv.ParseInt(timestamp, 10, 64)
		if err != nil {
			m.writeError(w, "invalid timestamp format")
			return
		}

		requestTime := time.Unix(ts, 0)
		clockSkew := time.Since(requestTime)
		if clockSkew < 0 {
			clockSkew = -clockSkew
		}
		if clockSkew > m.config.MaxClockSkew {
			m.writeError(w, fmt.Sprintf("timestamp outside acceptable range (skew: %v)", clockSkew))
			return
		}

		// Decode the signature
		sigBytes, err := base64.StdEncoding.DecodeString(signature)
		if err != nil {
			m.writeError(w, "invalid signature encoding")
			return
		}

		// Build canonical string and verify signature
		canonical := apiclient.BuildCanonicalString(r, timestamp, nonce)
		if !apiclient.VerifyHMAC(m.config.SecretKey, canonical, sigBytes) {
			m.writeError(w, "invalid signature")
			return
		}

		// Signature valid, continue to handler
		next.ServeHTTP(w, r)
	})
}

// writeError writes an authentication error response.
func (m *BrokerAuthMiddleware) writeError(w http.ResponseWriter, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	fmt.Fprintf(w, `{"error":{"code":"broker_auth_failed","message":%q}}`, message)
}

// UpdateSecretKey updates the secret key used for verification.
// This can be used when credentials are rotated.
func (m *BrokerAuthMiddleware) UpdateSecretKey(key []byte) {
	m.config.SecretKey = key
}

// SetEnabled enables or disables authentication.
func (m *BrokerAuthMiddleware) SetEnabled(enabled bool) {
	m.config.Enabled = enabled
}
