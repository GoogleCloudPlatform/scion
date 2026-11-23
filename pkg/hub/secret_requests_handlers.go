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
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/msgaction"
)

// Wire type aliases shared with pkg/messages so pkg/hub and pkg/hubclient share
// one canonical JSON contract for secret requests (ptone/scion#8747).
type (
	SecretRequestSendResult        = messages.SecretRequestSendResult
	SecretRequestReplaced          = messages.SecretRequestReplaced
	SecretRequestReplacedBy        = messages.SecretRequestReplacedBy
	SecretRequestFulfilled         = messages.SecretRequestFulfilled
	SecretRequestWithdrawn         = messages.SecretRequestWithdrawn
	SecretRequestDismissed         = messages.SecretRequestDismissed
	SecretRequestExpired           = messages.SecretRequestExpired
	SecretRequestView              = messages.SecretRequestView
	SecretRequestCardResponse      = messages.SecretRequestCardResponse
	FulfillSecretRequestBody       = messages.FulfillSecretRequestBody
	FulfillSecretRequestResponse   = messages.FulfillSecretRequestResponse
	DismissSecretRequestBody       = messages.DismissSecretRequestBody
	DismissSecretRequestResponse   = messages.DismissSecretRequestResponse
	WithdrawSecretRequestsRequest  = messages.WithdrawSecretRequestsRequest
	WithdrawSecretRequestsResponse = messages.WithdrawSecretRequestsResponse
	AgentSecretRequestListResponse = messages.AgentSecretRequestListResponse
)

// Error codes of the secret request endpoints (§4.6).
const (
	ErrCodeSecretRequestKeyPending = msgaction.ConflictSecretRequestKeyPending
	ErrCodeSecretRequestSubmitting = msgaction.ConflictSecretRequestSubmitting
	ErrCodeSecretRequestReplaced   = msgaction.ConflictReplaced
	ErrCodeSecretRequestWithdrawn  = msgaction.ConflictWithdrawn
	ErrCodeSecretRequestDismissed  = msgaction.ConflictDismissed
	ErrCodeSecretRequestExpired    = msgaction.ConflictExpired
	ErrCodeSecretRequestFulfilled  = msgaction.ConflictAlreadyFulfilled
	ErrCodeSecretRequestBusy       = msgaction.ConflictSubmitting
)

const (
	secretRequestsRoutePrefix      = "/api/v1/secret-requests/"
	agentSecretRequestsExactPath   = "/api/v1/agent/secret-requests"
	agentSecretRequestsRoutePrefix = "/api/v1/agent/secret-requests/"
)

// handleSecretRequestRoutes serves the responder secret-request endpoints,
// 404 while hub.agent_secrets is off:
//
//	GET  /api/v1/secret-requests/{messageId}
//	POST /api/v1/secret-requests/{messageId}/fulfill
//	POST /api/v1/secret-requests/{messageId}/dismiss
func (s *Server) handleSecretRequestRoutes(w http.ResponseWriter, r *http.Request) {
	s.requireExperiment(experiments.AgentSecrets, func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotImplemented, "not_implemented", "secret requests responder API not yet implemented", nil)
	})(w, r)
}

// handleAgentSecretRequestRoutes serves the agent self-service secret-request
// endpoints, 404 while hub.agent_secrets is off:
//
//	GET  /api/v1/agent/secret-requests
//	POST /api/v1/agent/secret-requests/withdraw
func (s *Server) handleAgentSecretRequestRoutes(w http.ResponseWriter, r *http.Request) {
	s.requireExperiment(experiments.AgentSecrets, func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotImplemented, "not_implemented", "agent secret requests API not yet implemented", nil)
	})(w, r)
}
