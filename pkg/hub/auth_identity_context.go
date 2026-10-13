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
	"log/slog"

	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// This file holds the request-context helpers that need hub domain types and
// so cannot live with the principal types in identity.go:
// GetIdentityFromContext falls back to the legacy per-middleware context keys
// (devauth.go's *DevUser, agenttoken.go's *AgentTokenClaims), and the request
// log attributes classify the principal through authz.go.

// GetIdentityFromContext returns the authenticated identity (user or agent).
func GetIdentityFromContext(ctx context.Context) Identity {
	// First check for identity set by unified auth middleware
	if identity, ok := contextIdentityValue(ctx); ok {
		// A typed-nil identity (for example an Identity holding
		// (*ScopedUserIdentity)(nil)) is treated as missing, the same as a
		// nil interface; see isNilIdentity.
		if isNilIdentity(identity) {
			return nil
		}
		return identity
	}
	// Fall back to checking individual context keys for backwards compatibility
	if user := GetUserFromContext(ctx); user != nil {
		return user
	}
	if agent := GetAgentFromContext(ctx); agent != nil {
		return &agentIdentityWrapper{agent}
	}
	return nil
}

// GetUserIdentityFromContext returns the user identity if present.
func GetUserIdentityFromContext(ctx context.Context) UserIdentity {
	identity := GetIdentityFromContext(ctx)
	// isNilIdentity, not a plain interface comparison: a typed-nil identity
	// (see isNilIdentity) is treated as missing here too, before the type
	// assertion below hands a nil concrete value to the caller.
	if isNilIdentity(identity) {
		return nil
	}
	if user, ok := identity.(UserIdentity); ok {
		return user
	}
	return nil
}

// GetAgentIdentityFromContext returns the agent identity if present.
func GetAgentIdentityFromContext(ctx context.Context) AgentIdentity {
	identity := GetIdentityFromContext(ctx)
	// isNilIdentity, not a plain interface comparison: a typed-nil identity
	// (see isNilIdentity) is treated as missing here too, before the type
	// assertion below hands a nil concrete value to the caller.
	if isNilIdentity(identity) {
		return nil
	}
	if agent, ok := identity.(AgentIdentity); ok {
		return agent
	}
	return nil
}

// contextWithAuthType returns a new context with the auth type set.
//
// E.2a (ptone/scion#2127, plan §3.1): every UnifiedAuthMiddleware branch calls
// this exactly once, after it has already called contextWithIdentity and (for
// branches that establish a credential) contextWithCredentialContext — so by
// the time this runs, ctx reflects the branch's full outcome. This is
// therefore also the single, centralized place to populate the request log's
// mutable auth fields (logging.SetRequestAuth) for every successful
// authentication branch, instead of one hand-written call per branch: a
// branch that is ever added or reordered cannot forget to log auth
// attribution, because the outcome is derived from ctx rather than
// hand-carried. Rejections (no identity ever gets set) are logged separately,
// at the point of rejection — see auth.go's UAT branch.
func contextWithAuthType(ctx context.Context, authType string) context.Context {
	ctx = context.WithValue(ctx, logging.AuthTypeKey{}, authType)
	logging.SetRequestAuth(ctx, authType, requestAuthAttrs(ctx)...)
	return ctx
}

// requestAuthAttrs builds the request-log attributes for the principal and
// credential established on ctx. See contextWithAuthType.
func requestAuthAttrs(ctx context.Context) []slog.Attr {
	var attrs []slog.Attr
	// isNilIdentity, not a plain interface comparison: a typed-nil identity
	// (see isNilIdentity) must not reach identity.ID() below.
	if identity := GetIdentityFromContext(ctx); !isNilIdentity(identity) {
		attrs = append(attrs, slog.String(logging.AttrUserID, identity.ID()))
		if pc := principalContextForIdentity(identity); pc.Kind != "" {
			attrs = append(attrs, slog.String("principal_kind", string(pc.Kind)))
		}
	}
	// The "credential" group is E.1's descriptive decoration (currently UAT
	// only); other credential kinds are already fully identified by
	// auth_type and user_id, per plan §3.1(2) ("emit them only when set").
	if cc := GetCredentialContextFromContext(ctx); cc.Decoration != nil {
		attrs = append(attrs, slog.Any("credential", *cc.Decoration))
	}
	return attrs
}
