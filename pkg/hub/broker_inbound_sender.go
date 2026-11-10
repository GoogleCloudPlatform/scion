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
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Sender resolution and authorization shared by the broker plugin routes:
// POST /api/v1/broker/inbound, /api/v1/broker/inbound/routed, and the
// attachment check and upload beside them. Each route resolves the sender a
// plugin names the same way, so they cannot drift.

// resolveBrokerInboundSender resolves the "user:<email>" sender a broker
// plugin names to the hub user it stands for. The identity is synthesized
// below the auth middleware, so the checks that middleware applies to a
// session are made here: a hub test identity is refused (it is reachable
// only through its own hub-issued token), an unknown email is refused, and
// the user must be active. Every refusal is 403, except a store fault (500)
// and a sender without the "user:" prefix (400). The caller logs the
// refusal; no store write happens here.
func (s *Server) resolveBrokerInboundSender(ctx context.Context, sender string) (*AuthenticatedUser, *apiError) {
	if !strings.HasPrefix(sender, "user:") {
		return nil, &apiError{status: http.StatusBadRequest, code: ErrCodeValidationError,
			message: "sender must use user: prefix for mapped identity"}
	}
	senderEmail := strings.TrimPrefix(sender, "user:")
	if emailResolvedPrincipalRefused(senderEmail, nil) {
		return nil, &apiError{status: http.StatusForbidden, code: ErrCodeForbidden,
			message: "sender identity not eligible", details: map[string]interface{}{"sender": sender}}
	}
	senderUser, err := s.store.GetUserByEmail(ctx, senderEmail)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, &apiError{status: http.StatusForbidden, code: ErrCodeForbidden,
				message: "sender identity could not be resolved", details: map[string]interface{}{"sender": sender}}
		}
		return nil, &apiError{status: http.StatusInternalServerError, code: ErrCodeInternalError,
			message: "internal error resolving sender identity"}
	}
	// Require active status before constructing an AuthenticatedUser from
	// broker-supplied sender data.
	if senderUser.Status != store.UserStatusActive {
		return nil, &apiError{status: http.StatusForbidden, code: ErrCodeForbidden,
			message: "sender identity is not active",
			details: map[string]interface{}{"sender": sender, "status": senderUser.Status}}
	}
	if emailResolvedPrincipalRefused(senderEmail, senderUser) {
		return nil, &apiError{status: http.StatusForbidden, code: ErrCodeForbidden,
			message: "sender identity not eligible", details: map[string]interface{}{"sender": sender}}
	}
	return NewAuthenticatedUser(senderUser.ID, senderUser.Email, senderUser.DisplayName, senderUser.Role, "integration"), nil
}

// authorizeBrokerAttachmentSender decides whether sender, already resolved
// by resolveBrokerInboundSender, may send attachments to project projectID:
//   - with targets (agent slugs of projectID), the sender must be allowed to
//     message every one of them (authorizeAgentMessage); an unknown slug or
//     any refusal refuses the whole request;
//   - without targets, the sender must hold read on the project; each
//     recipient is then authorized again when the message is routed.
//
// It writes nothing. A refusal is 403; a store fault is 500.
func (s *Server) authorizeBrokerAttachmentSender(ctx context.Context, sender *AuthenticatedUser, projectID string, targets []string) *apiError {
	if sender == nil {
		return &apiError{status: http.StatusForbidden, code: ErrCodeForbidden, message: "sender identity could not be resolved"}
	}
	if len(targets) > 0 {
		for _, slug := range targets {
			agent, err := s.store.GetAgentBySlug(ctx, projectID, slug)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return &apiError{status: http.StatusForbidden, code: ErrCodeMessageDenied,
						message: "Message delivery denied", details: map[string]interface{}{"agent_slug": slug}}
				}
				return &apiError{status: http.StatusInternalServerError, code: ErrCodeInternalError,
					message: "internal error resolving target agent"}
			}
			if allowed, reason, _ := s.authorizeAgentMessage(ctx, sender, agent, false); !allowed {
				return &apiError{status: http.StatusForbidden, code: ErrCodeMessageDenied,
					message: "Message delivery denied", details: map[string]interface{}{
						"reason":     mapReasonToCode(reason),
						"agent_slug": slug,
					}}
			}
		}
		return nil
	}
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return &apiError{status: http.StatusForbidden, code: ErrCodeForbidden, message: "project access denied"}
		}
		return &apiError{status: http.StatusInternalServerError, code: ErrCodeInternalError,
			message: "internal error resolving project"}
	}
	if decision := s.authzService.CheckAccess(ctx, sender, projectResource(project), ActionRead); !decision.Allowed {
		return &apiError{status: http.StatusForbidden, code: ErrCodeForbidden, message: "project access denied"}
	}
	return nil
}

// brokerSenderContext returns ctx with sender as its identity, for the
// artifact service calls of a broker attachment request: an upload is owned
// by, and attachment ids are checked against, the resolved sender user,
// never the broker. The credential context is left as the broker's.
func brokerSenderContext(ctx context.Context, sender *AuthenticatedUser) context.Context {
	return contextWithIdentity(ctx, sender)
}

// brokerRefusalLogText formats a refusal for a log line: code and message
// only, never request content.
func brokerRefusalLogText(e *apiError) string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("%s: %s", e.code, e.message)
}
