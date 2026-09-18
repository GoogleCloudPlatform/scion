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

package grpcbroker

import (
	"context"
	"fmt"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// --- Client-side: per-RPC credentials from a TokenSource ---

// TokenSourceCredentials implements PerCallAuthenticator (and
// grpc.PerRPCCredentials) by delegating to a transportauth.TokenSource.
// Each RPC call gets a fresh-or-cached token — the underlying source
// handles refresh, so credentials survive rotation without restart.
type TokenSourceCredentials struct {
	source             transportauth.TokenSource
	requireTLSSecurity bool
}

// NewTokenSourceCredentials wraps a TokenSource as gRPC per-RPC credentials.
// When requireTLS is true, gRPC enforces transport security (TLS) on every
// call. Set to false only for local/h2c development connections.
func NewTokenSourceCredentials(source transportauth.TokenSource, requireTLS bool) *TokenSourceCredentials {
	return &TokenSourceCredentials{
		source:             source,
		requireTLSSecurity: requireTLS,
	}
}

// GetRequestMetadata returns the authorization header for each RPC call.
func (c *TokenSourceCredentials) GetRequestMetadata(ctx context.Context, uri ...string) (map[string]string, error) {
	token, err := c.source.Token()
	if err != nil {
		return nil, fmt.Errorf("grpc auth: token fetch: %w", err)
	}
	return map[string]string{
		"authorization": "Bearer " + token,
	}, nil
}

// RequireTransportSecurity reports whether TLS is required.
func (c *TokenSourceCredentials) RequireTransportSecurity() bool {
	return c.requireTLSSecurity
}

// --- Server-side: gRPC interceptor for bearer token validation ---

// TokenValidator validates a bearer token and returns an error if invalid.
// Implementations should verify signature, audience, issuer, and expiry.
type TokenValidator interface {
	// ValidateToken checks a bearer token and returns a non-nil error if
	// the token is invalid, expired, or not authorized for the operation.
	ValidateToken(ctx context.Context, token string) error
}

// TokenValidatorFunc adapts a plain function to the TokenValidator interface.
type TokenValidatorFunc func(ctx context.Context, token string) error

// ValidateToken implements TokenValidator.
func (f TokenValidatorFunc) ValidateToken(ctx context.Context, token string) error {
	return f(ctx, token)
}

// extractBearerToken extracts a bearer token from gRPC request metadata.
func extractBearerToken(ctx context.Context) (string, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", status.Error(codes.Unauthenticated, "missing metadata")
	}

	values := md.Get("authorization")
	if len(values) == 0 {
		return "", status.Error(codes.Unauthenticated, "missing authorization header")
	}

	auth := values[0]
	const prefix = "Bearer "
	if !strings.HasPrefix(auth, prefix) {
		return "", status.Error(codes.Unauthenticated, "invalid authorization header format")
	}

	token := strings.TrimPrefix(auth, prefix)
	if token == "" {
		return "", status.Error(codes.Unauthenticated, "empty bearer token")
	}

	return token, nil
}
