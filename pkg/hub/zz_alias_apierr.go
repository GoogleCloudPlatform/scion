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
// github.com/GoogleCloudPlatform/scion/pkg/hub/apierr
// by hack/pkgmove, so existing references in package hub keep compiling.

import (
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/apierr"
)

type (
	APIError             = apierr.APIError
	ErrorResponse        = apierr.ErrorResponse
	RuntimeBrokerSummary = apierr.RuntimeBrokerSummary
)

const (
	ErrCodeAddrAmbiguous                    = apierr.ErrCodeAddrAmbiguous
	ErrCodeAddrMalformed                    = apierr.ErrCodeAddrMalformed
	ErrCodeAddrUnknown                      = apierr.ErrCodeAddrUnknown
	ErrCodeAgentNotFound                    = apierr.ErrCodeAgentNotFound
	ErrCodeAgentNotRunning                  = apierr.ErrCodeAgentNotRunning
	ErrCodeBrokerAuthFailed                 = apierr.ErrCodeBrokerAuthFailed
	ErrCodeBrokerTimeout                    = apierr.ErrCodeBrokerTimeout
	ErrCodeClockSkew                        = apierr.ErrCodeClockSkew
	ErrCodeCloneFailed                      = apierr.ErrCodeCloneFailed
	ErrCodeConflict                         = apierr.ErrCodeConflict
	ErrCodeConversationNotResolved          = apierr.ErrCodeConversationNotResolved
	ErrCodeDeleteInProgress                 = apierr.ErrCodeDeleteInProgress
	ErrCodeDeliveryFailed                   = apierr.ErrCodeDeliveryFailed
	ErrCodeDiscoverFailed                   = apierr.ErrCodeDiscoverFailed
	ErrCodeEmptyRoleSet                     = apierr.ErrCodeEmptyRoleSet
	ErrCodeExperimentDisabled               = apierr.ErrCodeExperimentDisabled
	ErrCodeExpiredJoinToken                 = apierr.ErrCodeExpiredJoinToken
	ErrCodeForbidden                        = apierr.ErrCodeForbidden
	ErrCodeIdentityAmbiguous                = apierr.ErrCodeIdentityAmbiguous
	ErrCodeInsufficientRelaxationAuthority  = apierr.ErrCodeInsufficientRelaxationAuthority
	ErrCodeInternalError                    = apierr.ErrCodeInternalError
	ErrCodeInvalidCursor                    = apierr.ErrCodeInvalidCursor
	ErrCodeInvalidDMKey                     = apierr.ErrCodeInvalidDMKey
	ErrCodeInvalidJoinToken                 = apierr.ErrCodeInvalidJoinToken
	ErrCodeInvalidRequest                   = apierr.ErrCodeInvalidRequest
	ErrCodeInvalidRoleSet                   = apierr.ErrCodeInvalidRoleSet
	ErrCodeInvalidSignature                 = apierr.ErrCodeInvalidSignature
	ErrCodeLastOwner                        = apierr.ErrCodeLastOwner
	ErrCodeMembershipChanged                = apierr.ErrCodeMembershipChanged
	ErrCodeMessageDenied                    = apierr.ErrCodeMessageDenied
	ErrCodeMissingEnvVars                   = apierr.ErrCodeMissingEnvVars
	ErrCodeMutationPermissionLost           = apierr.ErrCodeMutationPermissionLost
	ErrCodeNoRuntimeBroker                  = apierr.ErrCodeNoRuntimeBroker
	ErrCodeNotFound                         = apierr.ErrCodeNotFound
	ErrCodeNotImplemented                   = apierr.ErrCodeNotImplemented
	ErrCodePermissionRegistryChanged        = apierr.ErrCodePermissionRegistryChanged
	ErrCodePullFailed                       = apierr.ErrCodePullFailed
	ErrCodeQuotaExceeded                    = apierr.ErrCodeQuotaExceeded
	ErrCodeRateLimited                      = apierr.ErrCodeRateLimited
	ErrCodeRecoveryDisabledImmutable        = apierr.ErrCodeRecoveryDisabledImmutable
	ErrCodeReplayDetected                   = apierr.ErrCodeReplayDetected
	ErrCodeResolutionFailed                 = apierr.ErrCodeResolutionFailed
	ErrCodeRevisionConflict                 = apierr.ErrCodeRevisionConflict
	ErrCodeRoleAssignmentForbidden          = apierr.ErrCodeRoleAssignmentForbidden
	ErrCodeRuntimeBrokerAmbiguous           = apierr.ErrCodeRuntimeBrokerAmbiguous
	ErrCodeRuntimeBrokerLinkPathUnsupported = apierr.ErrCodeRuntimeBrokerLinkPathUnsupported
	ErrCodeRuntimeBrokerNameConflict        = apierr.ErrCodeRuntimeBrokerNameConflict
	ErrCodeRuntimeBrokerNotFlat             = apierr.ErrCodeRuntimeBrokerNotFlat
	ErrCodeRuntimeBrokerNotFound            = apierr.ErrCodeRuntimeBrokerNotFound
	ErrCodeRuntimeBrokerNotLinked           = apierr.ErrCodeRuntimeBrokerNotLinked
	ErrCodeRuntimeBrokerUnavail             = apierr.ErrCodeRuntimeBrokerUnavail
	ErrCodeRuntimeError                     = apierr.ErrCodeRuntimeError
	ErrCodeRuntimeProfileUnsupported        = apierr.ErrCodeRuntimeProfileUnsupported
	ErrCodeRuntimeTargetChanged             = apierr.ErrCodeRuntimeTargetChanged
	ErrCodeRuntimeTargetMismatch            = apierr.ErrCodeRuntimeTargetMismatch
	ErrCodeRuntimeTargetMoveUnsupported     = apierr.ErrCodeRuntimeTargetMoveUnsupported
	ErrCodeRuntimeTargetPinStale            = apierr.ErrCodeRuntimeTargetPinStale
	ErrCodeRuntimeTargetRequired            = apierr.ErrCodeRuntimeTargetRequired
	ErrCodeScopeMismatch                    = apierr.ErrCodeScopeMismatch
	ErrCodeScopeNotFound                    = apierr.ErrCodeScopeNotFound
	ErrCodeSecretScopeRestricted            = apierr.ErrCodeSecretScopeRestricted
	ErrCodeSecurityReviewRequired           = apierr.ErrCodeSecurityReviewRequired
	ErrCodeSendInProgress                   = apierr.ErrCodeSendInProgress
	ErrCodeStaleAuthorizationPreview        = apierr.ErrCodeStaleAuthorizationPreview
	ErrCodeSubjectNotFound                  = apierr.ErrCodeSubjectNotFound
	ErrCodeTargetRoleProtected              = apierr.ErrCodeTargetRoleProtected
	ErrCodeTemplateConflict                 = apierr.ErrCodeTemplateConflict
	ErrCodeThreadProjectRequired            = apierr.ErrCodeThreadProjectRequired
	ErrCodeUnauthorized                     = apierr.ErrCodeUnauthorized
	ErrCodeUnavailable                      = apierr.ErrCodeUnavailable
	ErrCodeUnprocessable                    = apierr.ErrCodeUnprocessable
	ErrCodeUnsupportedCapability            = apierr.ErrCodeUnsupportedCapability
	ErrCodeUserNotFound                     = apierr.ErrCodeUserNotFound
	ErrCodeValidationError                  = apierr.ErrCodeValidationError
	ErrCodeVersionConflict                  = apierr.ErrCodeVersionConflict
	deleteInProgressMessage                 = apierr.DeleteInProgressMessage
	storeMembersGroupPrincipalMessage       = apierr.StoreMembersGroupPrincipalMessage
)

var (
	BadRequest               = apierr.BadRequest
	Conflict                 = apierr.Conflict
	Forbidden                = apierr.Forbidden
	GatewayTimeout           = apierr.GatewayTimeout
	InternalError            = apierr.InternalError
	MethodNotAllowed         = apierr.MethodNotAllowed
	NoRuntimeBroker          = apierr.NoRuntimeBroker
	NotFound                 = apierr.NotFound
	RuntimeBrokerNotFound    = apierr.RuntimeBrokerNotFound
	RuntimeBrokerUnavailable = apierr.RuntimeBrokerUnavailable
	RuntimeError             = apierr.RuntimeError
	ServiceNotReady          = apierr.ServiceNotReady
	Unauthorized             = apierr.Unauthorized
	ValidationError          = apierr.ValidationError
)

func readJSON(p0 *http.Request, p1 interface{}) error {
	return apierr.ReadJSON(p0, p1)
}

func writeError(p0 http.ResponseWriter, p1 int, p2 string, p3 string, p4 map[string]interface{}) {
	apierr.WriteError(p0, p1, p2, p3, p4)
}

func writeErrorFromErr(p0 http.ResponseWriter, p1 error, p2 string) {
	apierr.WriteErrorFromErr(p0, p1, p2)
}

func writeJSON(p0 http.ResponseWriter, p1 int, p2 interface{}) {
	apierr.WriteJSON(p0, p1, p2)
}

func writeStoreErr(p0 http.ResponseWriter, p1 error, p2 string) {
	apierr.WriteStoreErr(p0, p1, p2)
}
