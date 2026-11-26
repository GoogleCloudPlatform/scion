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

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/msgaction"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// secretRequestPromptSend holds a validated secret request send on its way
// through the outbound-message handler.
type secretRequestPromptSend struct {
	requests []messages.SecretRequest
	update   bool
	intro    string
	result   *store.SecretRequestPromptResult
}

// secretRequestStoreError wraps a store-level refusal of CreateSecretRequestPrompt
// so the outbound-message handler maps it to a 4xx status instead of 500.
type secretRequestStoreError struct{ err error }

func (e *secretRequestStoreError) Error() string { return e.err.Error() }
func (e *secretRequestStoreError) Unwrap() error { return e.err }

// validateAgentSecretWriteConstraints applies the shared agent-scope secret
// write rules (reserved env targets via secret.IsReservedEnvTarget, System One
// and webhook reserved key prefixes with _ -> - folding, file target path
// safety, and internal secret mutation refusal). Both prepareSecretRequestSend
// and handleSecretRequestFulfill call it so secret requests cannot bypass
// agent-scope write guards (R3).
func (s *Server) validateAgentSecretWriteConstraints(w http.ResponseWriter, ctx context.Context, agentID, key, secretType, target string) bool {
	effectiveTarget := target
	if effectiveTarget == "" {
		effectiveTarget = key
	}
	if secretType == "" || secretType == store.SecretTypeEnvironment {
		if secret.IsReservedEnvTarget(effectiveTarget) {
			ValidationError(w, "target is reserved for scion's own control-plane environment variables", map[string]interface{}{
				"field": "target",
				"value": effectiveTarget,
			})
			return false
		}
	}
	if secretType == store.SecretTypeOnDemand && target != "" && target != key {
		ValidationError(w, "target is not supported for on_demand secrets", map[string]interface{}{
			"field": "target",
			"value": target,
		})
		return false
	}
	if secretType == store.SecretTypeFile {
		if err := messages.ValidateSecretRequestFileTarget(target); err != nil {
			ValidationError(w, err.Error(), map[string]interface{}{
				"field": "target",
				"value": target,
			})
			return false
		}
		if !s.validateFileSecretTarget(w, store.ScopeAgent, secretType, target) {
			return false
		}
	}
	if rejectSystemOneReservedSecretWrite(w, key) {
		return false
	}
	if rejectWebhookEndpointReservedSecretWrite(w, key) {
		return false
	}
	if s.refuseInternalSecretMutation(w, ctx, key, store.ScopeAgent, agentID) {
		return false
	}
	return true
}

// prepareSecretRequestSend validates a send that carries secret_requests or
// secret_requests_update before any routing: experiment gate (400
// experiment_disabled when off), mutual exclusion with decisions, update
// without entries, agent/broadcast recipient, attachments, entry validation,
// and agent-scope secret write constraints.
// Returns (nil, true) for an ordinary send, or (nil, false) after writing the error.
func (s *Server) prepareSecretRequestSend(w http.ResponseWriter, ctx context.Context, agentID string, req *OutboundMessageRequest) (*secretRequestPromptSend, bool) {
	if req.SecretRequests == nil && !req.SecretRequestsUpdate {
		return nil, true
	}
	if !s.experimentEnabled(experiments.AgentSecrets) {
		writeError(w, http.StatusBadRequest, ErrCodeExperimentDisabled,
			"experiment_disabled: "+experiments.AgentSecrets+" is off; secret requests are not available on this hub",
			map[string]interface{}{"experiment": experiments.AgentSecrets})
		return nil, false
	}
	if req.Decisions != nil || req.DecisionsUpdate {
		ValidationError(w, "cannot combine secret_requests with decisions in the same message",
			map[string]interface{}{"field": "secret_requests"})
		return nil, false
	}
	if req.SecretRequests == nil {
		ValidationError(w, "secret_requests_update requires secret_requests",
			map[string]interface{}{"field": "secret_requests_update"})
		return nil, false
	}
	recipient := strings.TrimSpace(req.Recipient)
	if strings.HasPrefix(recipient, "agent:") || isBroadcastRecipient(recipient) {
		ValidationError(w, "secret requests can be sent only to a user or a conversation, not to an agent or a broadcast",
			map[string]interface{}{"field": "recipient"})
		return nil, false
	}
	if len(req.Attachments) > 0 || len(req.AttachmentIDs) > 0 {
		ValidationError(w, "a message with secret_requests cannot carry attachments",
			map[string]interface{}{"field": "attachments"})
		return nil, false
	}
	normalized, err := messages.NormalizeSecretRequests("secret_requests", req.SecretRequests)
	if err != nil {
		writeSecretRequestValidationErrors(w, err)
		return nil, false
	}
	for _, entry := range normalized {
		if !s.validateAgentSecretWriteConstraints(w, ctx, agentID, entry.Key, entry.SecretType, entry.Target) {
			return nil, false
		}
	}
	return &secretRequestPromptSend{requests: normalized, update: req.SecretRequestsUpdate}, true
}

func writeSecretRequestValidationErrors(w http.ResponseWriter, err error) {
	var errs messages.DecisionValidationErrors
	if errors.As(err, &errs) {
		ValidationError(w, "invalid secret_requests: "+errs.Error(), map[string]interface{}{"errors": errs})
		return
	}
	ValidationError(w, "invalid secret_requests: "+err.Error(), nil)
}

// writeSecretRequestStoreError answers a CreateSecretRequestPrompt failure.
// Returns false if err is not a secret-request refusal.
func (s *Server) writeSecretRequestStoreError(w http.ResponseWriter, ctx context.Context, agent *store.Agent, err error) bool {
	var pendingErr *store.SecretRequestKeyPendingError
	var submittingErr *store.SecretRequestSubmittingError
	switch {
	case errors.As(err, &pendingErr):
		existing := make([]map[string]interface{}, len(pendingErr.Existing))
		for i, k := range pendingErr.Existing {
			existing[i] = map[string]interface{}{
				"key":       k.Key,
				"version":   k.Version,
				"messageId": k.MessageID,
				"status":    k.Status,
			}
			s.emitSecretRequestRefused(ctx, &MessageActionEvent{
				RowID:     k.RowID,
				MessageID: k.MessageID,
				ProjectID: agentProjectID(agent),
				AgentID:   agentIDOrEmpty(agent),
				AgentName: agentDisplayName(agent),
				Key:       k.Key,
				Version:   k.Version,
				ActorKind: MessageActionActorAgent,
				ActorID:   agentIDOrEmpty(agent),
				Reason:    ErrCodeSecretRequestKeyPending,
			})
		}
		writeError(w, http.StatusConflict, ErrCodeSecretRequestKeyPending, pendingErr.Error(),
			map[string]interface{}{"existing": existing})
		return true
	case errors.As(err, &submittingErr):
		keys := make([]map[string]interface{}, len(submittingErr.Keys))
		for i, k := range submittingErr.Keys {
			keys[i] = map[string]interface{}{
				"key":       k.Key,
				"version":   k.Version,
				"messageId": k.MessageID,
				"status":    k.Status,
			}
			s.emitSecretRequestRefused(ctx, &MessageActionEvent{
				RowID:     k.RowID,
				MessageID: k.MessageID,
				ProjectID: agentProjectID(agent),
				AgentID:   agentIDOrEmpty(agent),
				AgentName: agentDisplayName(agent),
				Key:       k.Key,
				Version:   k.Version,
				ActorKind: MessageActionActorAgent,
				ActorID:   agentIDOrEmpty(agent),
				Reason:    ErrCodeSecretRequestSubmitting,
			})
		}
		writeError(w, http.StatusConflict, ErrCodeSecretRequestSubmitting, submittingErr.Error(),
			map[string]interface{}{"keys": keys})
		return true
	case errors.Is(err, store.ErrInvalidInput):
		ValidationError(w, err.Error(), nil)
		return true
	}
	return false
}

func agentProjectID(a *store.Agent) string {
	if a == nil {
		return ""
	}
	return a.ProjectID
}

func agentIDOrEmpty(a *store.Agent) string {
	if a == nil {
		return ""
	}
	return a.ID
}

func (s *Server) secretRequestStandaloneURL(messageID string) string {
	base := strings.TrimRight(s.HubEndpoints().Public, "/")
	if base == "" || messageID == "" {
		return ""
	}
	return fmt.Sprintf("%s/secret-requests/%s", base, messageID)
}

func (s *Server) storeSecretRequestPrompt(ctx context.Context, sr *secretRequestPromptSend, agent *store.Agent, storeMsg *store.Message, structuredMsg *messages.StructuredMessage) error {
	projectName := ""
	if storeMsg.ProjectID != "" {
		if proj, err := s.store.GetProject(ctx, storeMsg.ProjectID); err == nil && proj != nil {
			projectName = proj.Name
			if projectName == "" {
				projectName = proj.Slug
			}
		}
	}
	res, err := s.store.CreateSecretRequestPrompt(ctx, &store.SecretRequestPromptInput{
		Message:       storeMsg,
		Intro:         sr.intro,
		ProjectName:   projectName,
		StandaloneURL: s.secretRequestStandaloneURL(storeMsg.ID),
		AgentID:       agent.ID,
		AgentName:     agentDisplayName(agent),
		Requests:      sr.requests,
		Update:        sr.update,
	})
	if err != nil {
		return &secretRequestStoreError{err: err}
	}
	sr.result = res
	structuredMsg.Msg = storeMsg.Msg
	structuredMsg.Type = storeMsg.Type
	return nil
}

func (s *Server) publishReplacedSecretRequests(ctx context.Context, res *store.SecretRequestPromptResult) {
	if res == nil {
		return
	}
	hinted := make(map[string]struct{})
	for _, cr := range res.Requests {
		if cr.Replaced == nil || cr.Replaced.ID == "" {
			continue
		}
		old := cr.Replaced
		chatKey := s.publishSecretRequestRowUpdated(ctx, old, cr.Row)
		s.publishSecretRequestsDMHintsOnce(ctx, hinted, old.MessageID, chatKey)
	}
}

func (s *Server) emitSecretRequestSendAudit(ctx context.Context, res *store.SecretRequestPromptResult) {
	if res == nil {
		return
	}
	for _, cr := range res.Requests {
		row := cr.Row
		if row == nil {
			continue
		}
		replacesID := ""
		if cr.Replaced != nil {
			replacesID = cr.Replaced.ID
		}
		details := map[string]string{
			"secret_type": row.SecretType,
		}
		if row.Target != "" {
			details["target"] = row.Target
		}
		s.emitMessageActionEvent(ctx, &MessageActionEvent{
			Kind:          msgaction.KindSecretRequest,
			EventType:     MessageActionEventRequested,
			RowID:         row.ID,
			MessageID:     row.MessageID,
			ProjectID:     row.ProjectID,
			AgentID:       row.AgentID,
			AgentName:     row.AgentName,
			Key:           row.Key,
			Version:       row.Version,
			ReplacesRowID: replacesID,
			ActorKind:     MessageActionActorAgent,
			ActorID:       row.AgentID,
			Details:       details,
			Timestamp:     row.CreatedAt,
		})
		if cr.Replaced != nil && cr.Replaced.Status == store.SecretRequestStatusReplaced {
			old := cr.Replaced
			oldDetails := map[string]string{
				"secret_type": old.SecretType,
			}
			if old.Target != "" {
				oldDetails["target"] = old.Target
			}
			s.emitMessageActionEvent(ctx, &MessageActionEvent{
				Kind:          msgaction.KindSecretRequest,
				EventType:     MessageActionEventReplaced,
				RowID:         old.ID,
				MessageID:     old.MessageID,
				ProjectID:     old.ProjectID,
				AgentID:       old.AgentID,
				AgentName:     old.AgentName,
				Key:           old.Key,
				Version:       old.Version,
				ReplacesRowID: row.ID,
				ActorKind:     MessageActionActorAgent,
				ActorID:       old.AgentID,
				Details:       oldDetails,
				Timestamp:     row.CreatedAt,
			})
		}
	}
}

func secretRequestSendResults(res *store.SecretRequestPromptResult) []SecretRequestSendResult {
	if res == nil {
		return nil
	}
	out := make([]SecretRequestSendResult, len(res.Requests))
	for i, cr := range res.Requests {
		item := SecretRequestSendResult{
			Key:     cr.Row.Key,
			Version: cr.Row.Version,
			RowID:   cr.Row.ID,
		}
		if cr.Replaced != nil {
			item.Replaced = &SecretRequestReplaced{
				Version: cr.Replaced.Version,
				Status:  cr.Replaced.Status,
			}
		}
		out[i] = item
	}
	return out
}
