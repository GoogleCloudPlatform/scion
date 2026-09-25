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

package bridge

import (
	"context"
	"net/url"
)

// CallerIdentity holds the per-request caller identity extracted from a
// validated hub credential. Absent for legacy apiKey/bearer/none modes.
type CallerIdentity struct {
	// --- Existing fields (user callers) ---
	UserID   string
	Email    string
	Role     string
	RawToken string // The original bearer token for passthrough
	// TokenType is "uat" (Scion scion_pat_* user access token), "bearer" (any
	// other bearer credential the Hub accepted via /auth/me, e.g. a forwarded
	// Google token under the hubBearer scheme), "jwt", "federation", or
	// "ge_exchange" (a token exchanged for a Hub JWT under the geGoogle scheme).
	TokenType string

	// --- New fields (agent/federation callers) ---

	// AgentID is the agent's UUID (from JWT `sub` claim).
	// Non-empty only for agent callers (TokenType == "federation").
	AgentID string

	// IssuerURL is the OIDC issuer URL (from JWT `iss` claim).
	// Identifies which hub signed the token.
	IssuerURL string

	// ProjectID is the agent's project (from `project_id` claim).
	ProjectID string

	// Ancestry is the agent's lineage chain (from `ancestry` claim).
	Ancestry []string
}

// IsAgent returns true if this identity represents an agent caller
// (federation auth, not a local user).
func (c *CallerIdentity) IsAgent() bool {
	return c.AgentID != ""
}

// SenderLabel returns the formatted sender string for Hub messages.
// Agent callers use "agent:<id>", user callers use "user:<email>".
func (c *CallerIdentity) SenderLabel() string {
	if c.IsAgent() {
		return "agent:" + c.AgentID
	}
	return "user:" + c.Email
}

// CallerKey returns a stable identifier for task-store isolation.
// For user callers: UserID.
// For agent callers: "agent:<issuer-host>:<agent-id>".
func (c *CallerIdentity) CallerKey() string {
	if c.IsAgent() {
		u, err := url.Parse(c.IssuerURL)
		if err != nil || u.Host == "" {
			return "agent:" + c.AgentID
		}
		return "agent:" + u.Host + ":" + c.AgentID
	}
	return c.UserID
}

type callerContextKey struct{}

// WithCallerIdentity injects a CallerIdentity into the context.
// Exported for use in test infrastructure (subprocess servers).
func WithCallerIdentity(ctx context.Context, id *CallerIdentity) context.Context {
	return context.WithValue(ctx, callerContextKey{}, id)
}

// withCallerIdentity is the internal alias kept for existing callers.
func withCallerIdentity(ctx context.Context, id *CallerIdentity) context.Context {
	return WithCallerIdentity(ctx, id)
}

// callerIdentityFromContext retrieves the CallerIdentity from the context.
// Returns nil for legacy auth modes (apiKey/bearer/none).
func callerIdentityFromContext(ctx context.Context) *CallerIdentity {
	v, _ := ctx.Value(callerContextKey{}).(*CallerIdentity)
	return v
}
