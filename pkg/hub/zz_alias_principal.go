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

// Aliases for symbols moved to
// github.com/GoogleCloudPlatform/scion/pkg/hub/internal/principal
// by hack/pkgmove, so existing references in package hub keep compiling.

import (
	"context"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/internal/principal"
)

type (
	AgentIdentity        = principal.AgentIdentity
	AgentTokenScope      = principal.AgentTokenScope
	AuthenticatedUser    = principal.AuthenticatedUser
	BoundaryKind         = principal.BoundaryKind
	BrokerIdentity       = principal.BrokerIdentity
	BrokerOnBehalfOf     = principal.BrokerOnBehalfOf
	CredentialContext    = principal.CredentialContext
	CredentialDecoration = principal.CredentialDecoration
	CredentialKind       = principal.CredentialKind
	ExecutorContext      = principal.ExecutorContext
	FederatedIdentity    = principal.FederatedIdentity
	Identity             = principal.Identity
	ScopedUserIdentity   = principal.ScopedUserIdentity
	TokenBoundary        = principal.TokenBoundary
	UserIdentity         = principal.UserIdentity
	decorationBoundary   = principal.DecorationBoundary
)

const (
	AuthTypeAgent                = principal.AuthTypeAgent
	AuthTypeAgentDelegation      = principal.AuthTypeAgentDelegation
	AuthTypeBroker               = principal.AuthTypeBroker
	AuthTypeDevToken             = principal.AuthTypeDevToken
	AuthTypeExternalBearer       = principal.AuthTypeExternalBearer
	AuthTypeFederation           = principal.AuthTypeFederation
	AuthTypeJWT                  = principal.AuthTypeJWT
	AuthTypeProxy                = principal.AuthTypeProxy
	AuthTypeSignedURL            = principal.AuthTypeSignedURL
	AuthTypeUAT                  = principal.AuthTypeUAT
	BoundaryKindHub              = principal.BoundaryKindHub
	BoundaryKindProject          = principal.BoundaryKindProject
	CredentialKindAgentJWT       = principal.CredentialKindAgentJWT
	CredentialKindBroker         = principal.CredentialKindBroker
	CredentialKindDelegatedAgent = principal.CredentialKindDelegatedAgent
	CredentialKindDev            = principal.CredentialKindDev
	CredentialKindFederation     = principal.CredentialKindFederation
	CredentialKindHubDelivery    = principal.CredentialKindHubDelivery
	CredentialKindInteractive    = principal.CredentialKindInteractive
	CredentialKindUAT            = principal.CredentialKindUAT
	uatMaxLabelCount             = principal.UatMaxLabelCount
	uatMaxLabelKeyBytes          = principal.UatMaxLabelKeyBytes
	uatMaxLabelValueBytes        = principal.UatMaxLabelValueBytes
	uatMaxNameBytes              = principal.UatMaxNameBytes
	uatMaxPurposeBytes           = principal.UatMaxPurposeBytes
)

var (
	BrokerOnBehalfOfFromContext                    = principal.BrokerOnBehalfOfFromContext
	ContextWithExecutor                            = principal.ContextWithExecutor
	ExecutorContextFromContext                     = principal.ExecutorContextFromContext
	GetCredentialContextFromContext                = principal.GetCredentialContextFromContext
	IsScopedUserIdentity                           = principal.IsScopedUserIdentity
	IsUnscopedLocalPlatformAdmin                   = principal.IsUnscopedLocalPlatformAdmin
	NewAuthenticatedUser                           = principal.NewAuthenticatedUser
	NewScopedUserIdentity                          = principal.NewScopedUserIdentity
	NewScopedUserIdentityWithBoundary              = principal.NewScopedUserIdentityWithBoundary
	NewScopedUserIdentityWithBoundaryAndDecoration = principal.NewScopedUserIdentityWithBoundaryAndDecoration
	NewScopedUserIdentityWithCeiling               = principal.NewScopedUserIdentityWithCeiling
	NewScopedUserIdentityWithCeilingAndDecoration  = principal.NewScopedUserIdentityWithCeilingAndDecoration
	NewScopedUserIdentityWithCredentialID          = principal.NewScopedUserIdentityWithCredentialID
	NewScopedUserIdentityWithDecoration            = principal.NewScopedUserIdentityWithDecoration
)

func contextIdentityValue(p0 context.Context) (principal.Identity, bool) {
	return principal.IdentityFrom(p0)
}

func contextWithBrokerOnBehalfOf(p0 context.Context, p1 principal.BrokerOnBehalfOf) context.Context {
	return principal.ContextWithBrokerOnBehalfOf(p0, p1)
}

func contextWithCredentialContext(p0 context.Context, p1 principal.CredentialContext) context.Context {
	return principal.ContextWithCredentialContext(p0, p1)
}

func contextWithIdentity(p0 context.Context, p1 principal.Identity) context.Context {
	return principal.ContextWithIdentity(p0, p1)
}

func isDisplayUnsafeRune(p0 rune) bool {
	return principal.IsDisplayUnsafeRune(p0)
}

func isNilIdentity(p0 principal.Identity) bool {
	return principal.IsNilIdentity(p0)
}

func isValidLabelKeyShape(p0 string) bool {
	return principal.IsValidLabelKeyShape(p0)
}

func requestCredentialBindsIdentity(p0 context.Context) bool {
	return principal.RequestCredentialBindsIdentity(p0)
}

func sanitizeForLog(p0 string, p1 int) string {
	return principal.SanitizeForLog(p0, p1)
}
