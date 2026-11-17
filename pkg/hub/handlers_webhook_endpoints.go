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
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const (
	maxWebhookEndpointNameLen = 128
	maxWebhookEndpointDescLen = 1024
)

var errWebhookEndpointLimitExceeded = errors.New("webhook endpoint limit exceeded")

// CreateWebhookEndpointRequest is the request body for POST /webhook-endpoints.
type CreateWebhookEndpointRequest struct {
	Name          string `json:"name"`
	Description   string `json:"description,omitempty"`
	SignatureMode string `json:"signatureMode,omitempty"`
}

// handleWebhookEndpoints routes requests under
// /api/v1/projects/{projectId}/webhook-endpoints[/{id}].
func (s *Server) handleWebhookEndpoints(w http.ResponseWriter, r *http.Request, projectID, endpointPath string) {
	s.requireExperiment(experiments.Triggers, func(w http.ResponseWriter, r *http.Request) {
		identity := GetIdentityFromContext(r.Context())
		if identity == nil {
			Unauthorized(w)
			return
		}
		if !checkAgentReadScope(w, r) {
			return
		}
		if agentIdentity := GetAgentIdentityFromContext(r.Context()); agentIdentity != nil {
			if agentIdentity.ProjectID() != projectID {
				Forbidden(w)
				return
			}
		}

		var authzAction Action
		authoring := false
		if endpointPath == "" {
			switch r.Method {
			case http.MethodGet:
				authzAction = ActionList
			case http.MethodPost:
				authzAction = ActionCreate
				authoring = true
			default:
				MethodNotAllowed(w, http.MethodGet, http.MethodPost)
				return
			}
		} else {
			if strings.Contains(endpointPath, "/") {
				NotFound(w, "Webhook endpoint action")
				return
			}
			switch r.Method {
			case http.MethodGet:
				authzAction = ActionRead
			case http.MethodDelete:
				authzAction = ActionDelete
			default:
				MethodNotAllowed(w, http.MethodGet, http.MethodDelete)
				return
			}
		}

		if authoring && !authorizeDeferredAuthoringCredential(w, r) {
			return
		}
		if !s.authorizeWebhookEndpointAccess(w, r, projectID, authzAction) {
			return
		}

		if endpointPath == "" {
			switch r.Method {
			case http.MethodGet:
				s.listWebhookEndpoints(w, r, projectID)
			case http.MethodPost:
				s.createWebhookEndpoint(w, r, projectID)
			}
			return
		}
		switch r.Method {
		case http.MethodGet:
			s.getWebhookEndpoint(w, r, projectID, endpointPath)
		case http.MethodDelete:
			s.deleteWebhookEndpoint(w, r, projectID, endpointPath)
		}
	})(w, r)
}

func validateWebhookEndpointName(raw string) (string, error) {
	return validateTriggerResourceName(raw, maxWebhookEndpointNameLen)
}

func (s *Server) createWebhookEndpoint(w http.ResponseWriter, r *http.Request, projectID string) {
	ctx := r.Context()
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Unauthorized(w)
		return
	}

	var req CreateWebhookEndpointRequest
	if !readTriggerRequestJSON(w, r, &req) {
		return
	}

	name, err := validateWebhookEndpointName(req.Name)
	if err != nil {
		ValidationError(w, err.Error(), nil)
		return
	}
	if err := validateTriggerResourceDescription(req.Description, maxWebhookEndpointDescLen); err != nil {
		ValidationError(w, err.Error(), nil)
		return
	}
	sigMode := strings.TrimSpace(req.SignatureMode)
	if sigMode == "" {
		sigMode = store.WebhookSignatureModeScionV1
	} else if sigMode != store.WebhookSignatureModeScionV1 {
		ValidationError(w, fmt.Sprintf("unsupported signatureMode %q (supported: %s)", sigMode, store.WebhookSignatureModeScionV1), nil)
		return
	}

	if _, err := s.store.GetProject(ctx, projectID); err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrInvalidInput) {
			NotFound(w, "Project")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	rawSecret, err := generateWebhookEndpointSecret()
	if err != nil {
		InternalError(w)
		return
	}

	attr := newInitiatorAttribution(ctx)
	ownerKind := store.DelegationPrincipalUser
	if identity.Type() == "agent" {
		ownerKind = store.DelegationPrincipalAgent
	}

	ep := store.WebhookEndpoint{
		ID:                  api.NewUUID(),
		ProjectID:           projectID,
		Name:                name,
		Description:         req.Description,
		OwnerPrincipalKind:  ownerKind,
		OwnerPrincipalID:    identity.ID(),
		OwnerCredentialKind: attr.InitiatorCredentialKind,
		SignatureMode:       sigMode,
		SecretVersion:       1,
		Status:              store.WebhookEndpointStatusActive,
		CreatedBy:           identity.ID(),
	}

	secretRow, err := s.newWebhookEndpointSecretRow(projectID, ep.ID, 1, rawSecret, identity.ID())
	if err != nil {
		InternalError(w)
		return
	}

	audit := newWebhookEndpointAudit(ctx, mutationTypeWebhookEndpointCreate, ep.ID)
	afterBytes, _ := json.Marshal(map[string]any{
		"project_id":     projectID,
		"endpoint_id":    ep.ID,
		"name":           ep.Name,
		"signature_mode": ep.SignatureMode,
		"secret_version": ep.SecretVersion,
	})
	audit.AfterSummary = string(afterBytes)

	fp := fixturePrincipalFromContext(ctx)
	err = s.store.WithTx(ctx, func(tx store.Store) error {
		if lErr := tx.LockTriggerQuota(ctx, projectID); lErr != nil {
			return lErr
		}
		if sErr := stampWebhookEndpoint(ctx, tx, &ep, fp); sErr != nil {
			return sErr
		}
		count, cErr := tx.CountWebhookEndpoints(ctx, projectID)
		if cErr != nil {
			return cErr
		}
		if count >= store.MaxWebhookEndpointsPerProject {
			return errWebhookEndpointLimitExceeded
		}
		if cErr := tx.CreateWebhookEndpoint(ctx, &ep); cErr != nil {
			return cErr
		}
		if sErr := tx.CreateSecret(ctx, secretRow); sErr != nil {
			return sErr
		}
		return tx.CreateMutationAudit(ctx, audit)
	})
	if err != nil {
		if writeFixtureRootError(w, err) {
			return
		}
		if errors.Is(err, errWebhookEndpointLimitExceeded) {
			writeError(w, http.StatusConflict, ErrCodeLimitExceeded,
				fmt.Sprintf("project has reached the maximum of %d webhook endpoints", store.MaxWebhookEndpointsPerProject), nil)
			return
		}
		if errors.Is(err, store.ErrAlreadyExists) {
			writeError(w, http.StatusConflict, ErrCodeConflict,
				"a webhook endpoint with this name already exists in this project", nil)
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusCreated, WebhookEndpointCreatedResponse{
		WebhookEndpointResponse: s.buildWebhookEndpointResponse(ep),
		Secret:                  rawSecret,
	})
}

func (s *Server) listWebhookEndpoints(w http.ResponseWriter, r *http.Request, projectID string) {
	ctx := r.Context()
	query := r.URL.Query()
	filter := store.WebhookEndpointFilter{
		ProjectID: projectID,
		Name:      strings.TrimSpace(query.Get("name")),
		Status:    strings.TrimSpace(query.Get("status")),
	}
	scope := ""
	if isAgentCaller(ctx) {
		identity := GetIdentityFromContext(ctx)
		if identity == nil || identity.ID() == "" {
			Unauthorized(w)
			return
		}
		filter.OwnerPrincipalKind = store.DelegationPrincipalAgent
		filter.OwnerPrincipalID = identity.ID()
		scope = "caller"
	}

	result, err := s.store.ListWebhookEndpoints(ctx, filter, listOptionsFromQuery(query))
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, ListWebhookEndpointsResponse{
		Scope:      scope,
		Endpoints:  s.buildWebhookEndpointResponses(result.Items),
		NextCursor: result.NextCursor,
		TotalCount: result.TotalCount,
		ServerTime: time.Now().UTC(),
	})
}

func (s *Server) loadWebhookEndpointForProject(w http.ResponseWriter, r *http.Request, projectID, endpointID string) (*store.WebhookEndpoint, bool) {
	ep, err := s.store.GetWebhookEndpoint(r.Context(), endpointID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrInvalidInput) {
			writeWebhookEndpointNotFound(w)
			return nil, false
		}
		writeErrorFromErr(w, err, "")
		return nil, false
	}
	if ep.ProjectID != projectID {
		writeWebhookEndpointNotFound(w)
		return nil, false
	}
	return ep, true
}

func (s *Server) getWebhookEndpoint(w http.ResponseWriter, r *http.Request, projectID, endpointID string) {
	ep, ok := s.loadWebhookEndpointForProject(w, r, projectID, endpointID)
	if !ok {
		return
	}
	if !s.authorizeWebhookEndpointRow(w, r, projectID, ActionRead, ep) {
		return
	}
	writeJSON(w, http.StatusOK, s.buildWebhookEndpointResponse(*ep))
}

func (s *Server) deleteWebhookEndpoint(w http.ResponseWriter, r *http.Request, projectID, endpointID string) {
	ctx := r.Context()
	ep, ok := s.loadWebhookEndpointForProject(w, r, projectID, endpointID)
	if !ok {
		return
	}
	if !s.authorizeWebhookEndpointRow(w, r, projectID, ActionDelete, ep) {
		return
	}

	audit := newWebhookEndpointAudit(ctx, mutationTypeWebhookEndpointDelete, ep.ID)
	beforeBytes, _ := json.Marshal(map[string]any{
		"endpoint_id": ep.ID,
		"project_id":  projectID,
		"name":        ep.Name,
	})
	audit.BeforeSummary = string(beforeBytes)

	if err := s.store.WithTx(ctx, func(tx store.Store) error {
		if lErr := tx.LockTriggerQuota(ctx, projectID); lErr != nil {
			return lErr
		}
		pausedTriggerIDs, _, dErr := tx.DeleteWebhookEndpoint(ctx, projectID, ep.ID)
		if dErr != nil {
			return dErr
		}
		afterBytes, _ := json.Marshal(map[string]any{
			"project_id":           projectID,
			"paused_trigger_count": len(pausedTriggerIDs),
		})
		audit.AfterSummary = string(afterBytes)
		return tx.CreateMutationAudit(ctx, audit)
	}); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeWebhookEndpointNotFound(w)
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	if s.webhookIngress != nil {
		s.webhookIngress.InvalidateEndpoint(ep.ID)
	}
	w.WriteHeader(http.StatusNoContent)
}
