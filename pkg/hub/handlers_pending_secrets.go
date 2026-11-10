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
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Agent-written user-scope secrets wait for the owning user's approval
// before they are delivered. The agent route records them in the pending
// secret store (never in the secrets table every delivery path reads), and
// the owner lists, approves or rejects them through the endpoints below.

// pendingSecretState is the state reported for a pending secret.
const pendingSecretState = "pending"

// pendingSecretsPath is the prefix of the owner's pending secret endpoints.
const pendingSecretsPath = "/api/v1/pending-secrets"

// pendingSecretHeldByOtherAgentMessage is the refusal for a write to a name
// another agent's pending secret holds. It names neither that agent nor its
// project.
const pendingSecretHeldByOtherAgentMessage = "A pending secret with this name is awaiting the owner's decision. " +
	"It cannot be replaced until the owner approves or rejects it."

// AgentPendingSecretResponse is the 202 Accepted body of an agent's
// user-scope secret write.
type AgentPendingSecretResponse struct {
	Key              string    `json:"key"`
	Scope            string    `json:"scope"`
	State            string    `json:"state"`
	PendingID        string    `json:"pendingId"`
	ExpiresAt        time.Time `json:"expiresAt"`
	ReplacesExisting bool      `json:"replacesExisting"`
	Message          string    `json:"message"`
}

// PendingSecretView is one pending secret as the owner sees it: metadata
// and provenance, never the value or its backend reference.
type PendingSecretView struct {
	ID               string    `json:"id"`
	Key              string    `json:"key"`
	Type             string    `json:"type"`
	Target           string    `json:"target"`
	Scope            string    `json:"scope"`
	AllowProgeny     bool      `json:"allowProgeny"`
	InjectionMode    string    `json:"injectionMode,omitempty"`
	Description      string    `json:"description,omitempty"`
	ValueLen         int       `json:"valueLen"`
	AgentID          string    `json:"agentId"`
	AgentSlug        string    `json:"agentSlug"`
	ProjectID        string    `json:"projectId"`
	ProjectName      string    `json:"projectName"`
	Created          time.Time `json:"created"`
	Updated          time.Time `json:"updated"`
	ExpiresAt        time.Time `json:"expiresAt"`
	ReplacesExisting bool      `json:"replacesExisting"`
	ReplacesSecretID string    `json:"replacesSecretId,omitempty"`
	ReplacesVersion  int       `json:"replacesVersion,omitempty"`
	State            string    `json:"state"`
}

// ListPendingSecretsResponse is the body of GET /api/v1/pending-secrets.
type ListPendingSecretsResponse struct {
	PendingSecrets []PendingSecretView `json:"pendingSecrets"`
}

// ApprovePendingSecretRequest is the optional body of an approval. The
// approval always writes the user-scope secret the agent asked for; the
// only change the owner can make is turning progeny access off.
type ApprovePendingSecretRequest struct {
	AllowProgeny *bool `json:"allowProgeny,omitempty"`
}

// PendingSecretDecisionResponse is the body of an approval or rejection.
type PendingSecretDecisionResponse struct {
	ID    string `json:"id"`
	Key   string `json:"key"`
	Scope string `json:"scope"`
	State string `json:"state"` // "approved" or "rejected"
}

func pendingSecretToView(p store.PendingSecret) PendingSecretView {
	return PendingSecretView{
		ID:               p.ID,
		Key:              p.Key,
		Type:             p.SecretType,
		Target:           p.Target,
		Scope:            p.Scope,
		AllowProgeny:     p.AllowProgeny,
		InjectionMode:    p.InjectionMode,
		Description:      p.Description,
		ValueLen:         p.ValueLen,
		AgentID:          p.AgentID,
		AgentSlug:        p.AgentSlug,
		ProjectID:        p.ProjectID,
		ProjectName:      p.ProjectName,
		Created:          p.Created,
		Updated:          p.Updated,
		ExpiresAt:        p.ExpiresAt,
		ReplacesExisting: p.ReplacesSecretID != "",
		ReplacesSecretID: p.ReplacesSecretID,
		ReplacesVersion:  p.ReplacesVersion,
		State:            pendingSecretState,
	}
}

// agentWritePendingUserSecret records an agent's user-scope secret write as
// a pending secret for the owner (ownerID) to approve, and answers 202. It
// never writes the secrets table, so the request's force flag has no effect
// here: an existing approved secret of the same name stays in effect and
// the pending secret records that it would replace it.
//
// The provenance (agent, project, time) comes from the verified agent token
// (agentID, projectID, ownerID) and the server clock; the slug and project
// name are snapshots looked up by those IDs.
func (s *Server) agentWritePendingUserSecret(w http.ResponseWriter, r *http.Request, agentID, projectID, ownerID, key string, value []byte, secretType, target string, allowProgeny bool) {
	ctx := r.Context()

	agentSlug, projectName, err := s.pendingSecretProvenanceNames(ctx, agentID, projectID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	var replacesID string
	var replacesVersion int
	existing, err := s.secretBackend.GetMeta(ctx, key, store.ScopeUser, ownerID)
	switch {
	case err == nil:
		replacesID, replacesVersion = existing.ID, existing.Version
	case errors.Is(err, store.ErrNotFound):
	default:
		writeErrorFromErr(w, err, "")
		return
	}

	pendingID := api.NewUUID()
	ref, err := s.secretBackend.HoldPending(ctx, pendingID, value)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	res, err := s.store.CreateOrReplacePendingSecret(ctx, &store.PendingSecret{
		ID:               pendingID,
		Key:              key,
		SecretType:       secretType,
		Target:           target,
		Scope:            store.ScopeUser,
		ScopeID:          ownerID,
		AllowProgeny:     allowProgeny,
		ValueRef:         ref,
		ValueLen:         len(value),
		AgentID:          agentID,
		AgentSlug:        agentSlug,
		ProjectID:        projectID,
		ProjectName:      projectName,
		ReplacesSecretID: replacesID,
		ReplacesVersion:  replacesVersion,
	}, time.Now())
	if err != nil {
		// The held value was never recorded; release it.
		s.dropPendingValues(ctx, []string{ref})
		if errors.Is(err, store.ErrPendingSecretHeldByOtherAgent) {
			writeError(w, http.StatusConflict, ErrCodeSecretPendingHeldByOtherAgent,
				pendingSecretHeldByOtherAgentMessage, map[string]interface{}{
					"key":   key,
					"scope": store.ScopeUser,
				})
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}
	// The write has committed: release the values of the rows it removed
	// (this agent's earlier pending write, or an expired one).
	s.dropPendingValues(ctx, res.DroppedValueRefs)

	writeJSON(w, http.StatusAccepted, AgentPendingSecretResponse{
		Key:              key,
		Scope:            store.ScopeUser,
		State:            pendingSecretState,
		PendingID:        res.Secret.ID,
		ExpiresAt:        res.Secret.ExpiresAt,
		ReplacesExisting: res.Secret.ReplacesSecretID != "",
		Message:          "awaiting user approval",
	})
}

// pendingSecretProvenanceNames returns the writing agent's slug and its
// project's name at write time, falling back to the IDs when a record has
// no name, so the pending secret stays readable after either is deleted.
func (s *Server) pendingSecretProvenanceNames(ctx context.Context, agentID, projectID string) (agentSlug, projectName string, err error) {
	agentSlug, projectName = agentID, projectID
	agent, err := s.store.GetAgent(ctx, agentID)
	switch {
	case err == nil:
		if agent.Slug != "" {
			agentSlug = agent.Slug
		} else if agent.Name != "" {
			agentSlug = agent.Name
		}
	case errors.Is(err, store.ErrNotFound):
	default:
		return "", "", err
	}
	project, err := s.store.GetProject(ctx, projectID)
	switch {
	case err == nil:
		if project.Name != "" {
			projectName = project.Name
		}
	case errors.Is(err, store.ErrNotFound):
	default:
		return "", "", err
	}
	return agentSlug, projectName, nil
}

// dropPendingValues releases held pending values after the pending rows
// that referenced them are gone. A failure is logged (never the value or
// its reference) and does not fail the request.
func (s *Server) dropPendingValues(ctx context.Context, refs []string) {
	for _, ref := range refs {
		if ref == "" {
			continue
		}
		if err := s.secretBackend.DropPending(ctx, ref); err != nil {
			slog.Warn("pending secret: releasing a held value failed", "error", err)
		}
	}
}

// handlePendingSecrets serves the owner's pending secret endpoints:
//
//	GET  /api/v1/pending-secrets              list own pending secrets
//	POST /api/v1/pending-secrets/{id}/approve approve (body {allowProgeny?})
//	POST /api/v1/pending-secrets/{id}/reject  reject
//
// Only the owning user, on an interactive session or dev credential, is
// served (resolveEnvSecretAccess for a user-scope write): agents, user
// access tokens and federated callers are refused, and every request
// resolves the caller's own user ID only, so no other user, hub admins
// included, can see or decide on a pending secret. A pending secret past
// its expiry is treated as gone.
func (s *Server) handlePendingSecrets(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, pendingSecretsPath)
	rest = strings.Trim(rest, "/")

	if rest == "" {
		if r.Method != http.MethodGet {
			MethodNotAllowed(w, http.MethodGet)
			return
		}
		ownerID, ok := s.resolveEnvSecretAccess(w, r, store.ScopeUser, "", true)
		if !ok {
			return
		}
		s.listPendingSecrets(w, r, ownerID)
		return
	}

	parts := strings.Split(rest, "/")
	if len(parts) != 2 || parts[0] == "" || (parts[1] != "approve" && parts[1] != "reject") {
		NotFound(w, "Pending secret")
		return
	}
	if r.Method != http.MethodPost {
		MethodNotAllowed(w, http.MethodPost)
		return
	}
	ownerID, ok := s.resolveEnvSecretAccess(w, r, store.ScopeUser, "", true)
	if !ok {
		return
	}
	if s.secretBackend == nil {
		writeErrorFromErr(w, secret.ErrNoSecretBackend, "")
		return
	}
	id, action := parts[0], parts[1]
	if action == "approve" {
		s.approvePendingSecret(w, r, ownerID, id)
	} else {
		s.rejectPendingSecret(w, r, ownerID, id)
	}
}

func (s *Server) listPendingSecrets(w http.ResponseWriter, r *http.Request, ownerID string) {
	rows, err := s.store.ListPendingSecretsByOwner(r.Context(), store.ScopeUser, ownerID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	now := time.Now()
	out := ListPendingSecretsResponse{PendingSecrets: make([]PendingSecretView, 0, len(rows))}
	for _, p := range rows {
		if p.Expired(now) {
			continue
		}
		out.PendingSecrets = append(out.PendingSecrets, pendingSecretToView(p))
	}
	writeJSON(w, http.StatusOK, out)
}

// ownPendingSecret returns the live pending secret id of ownerID, or writes
// 404 when there is none (absent, another user's, or expired: the three are
// indistinguishable).
func (s *Server) ownPendingSecret(w http.ResponseWriter, ctx context.Context, ownerID, id string) (*store.PendingSecret, bool) {
	p, err := s.store.GetPendingSecret(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Pending secret")
		} else {
			writeErrorFromErr(w, err, "")
		}
		return nil, false
	}
	if p.Scope != store.ScopeUser || p.ScopeID != ownerID || p.Expired(time.Now()) {
		NotFound(w, "Pending secret")
		return nil, false
	}
	return p, true
}

func (s *Server) approvePendingSecret(w http.ResponseWriter, r *http.Request, ownerID, id string) {
	ctx := r.Context()

	var req ApprovePendingSecretRequest
	if r.ContentLength != 0 {
		r.Body = http.MaxBytesReader(w, r.Body, 4*1024)
		if err := readJSON(r, &req); err != nil {
			BadRequest(w, "Invalid request body: "+err.Error())
			return
		}
	}

	p, ok := s.ownPendingSecret(w, ctx, ownerID, id)
	if !ok {
		return
	}

	allowProgeny := p.AllowProgeny
	if req.AllowProgeny != nil {
		if *req.AllowProgeny && !p.AllowProgeny {
			ValidationError(w, "allowProgeny can only be turned off at approval", map[string]interface{}{
				"field": "allowProgeny",
			})
			return
		}
		allowProgeny = *req.AllowProgeny
	}

	value, err := s.secretBackend.ReadPending(ctx, p.ValueRef)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Attribution. The approved secret belongs to the owner whose scope it
	// lives in, even though an agent wrote it. Progeny lookup matches
	// created_by against the agent's ancestry chain (ListProgenySecrets ->
	// CreatedByIn), whose entries are bare UUIDs, so an "agent:<uuid>"
	// creator could never be inherited, and an agent creator would only
	// reach that one agent's descendants rather than the owner's agents.
	// The writing agent is recorded as the updater.
	input := &secret.SetSecretInput{
		Name:          p.Key,
		Value:         string(value),
		SecretType:    p.SecretType,
		Target:        p.Target,
		Scope:         store.ScopeUser,
		ScopeID:       ownerID,
		Description:   p.Description,
		InjectionMode: p.InjectionMode,
		AllowProgeny:  allowProgeny,
		CreatedBy:     ownerID,
		UpdatedBy:     "agent:" + p.AgentID,
	}
	if userIdent := GetUserIdentityFromContext(ctx); userIdent != nil {
		input.UserEmail = userIdent.Email()
	}
	if _, _, err := s.secretBackend.Set(ctx, input); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	if err := s.store.DeletePendingSecret(ctx, p.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
		// The secret is approved; the pending row is left for expiry.
		slog.Warn("pending secret: deleting the approved pending row failed",
			"pending_id", p.ID, "error", err)
	} else {
		s.dropPendingValues(ctx, []string{p.ValueRef})
	}

	writeJSON(w, http.StatusOK, PendingSecretDecisionResponse{
		ID:    p.ID,
		Key:   p.Key,
		Scope: store.ScopeUser,
		State: "approved",
	})
}

func (s *Server) rejectPendingSecret(w http.ResponseWriter, r *http.Request, ownerID, id string) {
	ctx := r.Context()
	p, ok := s.ownPendingSecret(w, ctx, ownerID, id)
	if !ok {
		return
	}
	if err := s.store.DeletePendingSecret(ctx, p.ID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Pending secret")
		} else {
			writeErrorFromErr(w, err, "")
		}
		return
	}
	s.dropPendingValues(ctx, []string{p.ValueRef})
	writeJSON(w, http.StatusOK, PendingSecretDecisionResponse{
		ID:    p.ID,
		Key:   p.Key,
		Scope: store.ScopeUser,
		State: "rejected",
	})
}
