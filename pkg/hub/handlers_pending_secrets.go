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

// pendingSecretExistingChangedMessage is the refusal of an approval when
// the approved secret of the same name changed after the agent wrote the
// pending secret.
const pendingSecretExistingChangedMessage = "The existing secret with this name changed after the agent saved this pending secret. " +
	"Reject it and have the agent save it again."

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
	if pendingSecretBrowserRequestRefused(w, r) {
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

// pendingSecretBrowserRequestRefused refuses a decision request a browser
// sent from another site, in every auth mode, and reports whether it did
// (the response is then written and nothing is changed). It applies the
// checks proxyModeWriteGuard applies in proxy auth mode: a cross-site
// request (requestIsCrossSite) gets 403, and a request with a browser
// signal (Origin or Sec-Fetch-Site) whose body is not JSON
// (acceptableWriteBody) gets 415. First it removes any CORS response
// headers corsMiddleware has set, so these routes never grant another
// origin access to their responses.
func pendingSecretBrowserRequestRefused(w http.ResponseWriter, r *http.Request) bool {
	h := w.Header()
	for name := range h {
		if strings.HasPrefix(http.CanonicalHeaderKey(name), "Access-Control-Allow-") {
			h.Del(name)
		}
	}
	if requestIsCrossSite(r) {
		writeError(w, http.StatusForbidden, ErrCodeForbidden,
			"this request is not accepted on this endpoint", nil)
		return true
	}
	if requestHasOriginSignal(r) && !acceptableWriteBody(r) {
		writeError(w, http.StatusUnsupportedMediaType, errCodeUnsupportedMediaType,
			"this endpoint requires an application/json request body", nil)
		return true
	}
	return false
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

	// Read the held value before claiming the row, so a read failure
	// changes nothing.
	value, err := s.secretBackend.ReadPending(ctx, p.ValueRef)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Claim the row: of a concurrent approval and rejection exactly one
	// claims it, and the other answers 404. The claim also checks that the
	// approved secret of this name is still the one recorded when the
	// agent wrote the pending secret; if not, nothing is written and the
	// pending secret is kept.
	claimed, err := s.store.ClaimPendingSecretForApproval(ctx, p.ID, store.ScopeUser, ownerID, time.Now())
	switch {
	case err == nil:
	case errors.Is(err, store.ErrNotFound):
		NotFound(w, "Pending secret")
		return
	case errors.Is(err, store.ErrPendingSecretExistingChanged):
		writeError(w, http.StatusConflict, ErrCodeSecretPendingExistingChanged,
			pendingSecretExistingChangedMessage, map[string]interface{}{
				"id":    p.ID,
				"key":   p.Key,
				"scope": store.ScopeUser,
			})
		return
	default:
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
		Name:          claimed.Key,
		Value:         string(value),
		SecretType:    claimed.SecretType,
		Target:        claimed.Target,
		Scope:         store.ScopeUser,
		ScopeID:       ownerID,
		Description:   claimed.Description,
		InjectionMode: claimed.InjectionMode,
		AllowProgeny:  allowProgeny,
		CreatedBy:     ownerID,
		UpdatedBy:     "agent:" + claimed.AgentID,
	}
	if userIdent := GetUserIdentityFromContext(ctx); userIdent != nil {
		input.UserEmail = userIdent.Email()
	}
	if _, _, err := s.secretBackend.Set(ctx, input); err != nil {
		// Put the pending secret back so the owner can try again.
		s.restoreClaimedPendingSecret(ctx, claimed)
		writeErrorFromErr(w, err, "")
		return
	}
	// The approved secret is written: release the held value.
	s.dropPendingValues(ctx, []string{claimed.ValueRef})

	writeJSON(w, http.StatusOK, PendingSecretDecisionResponse{
		ID:    claimed.ID,
		Key:   claimed.Key,
		Scope: store.ScopeUser,
		State: "approved",
	})
}

// restoreClaimedPendingSecret puts back a pending secret an approval
// claimed but could not write, with its ID and held value. When the key
// has been taken since (the same agent wrote a replacement), the claimed
// value is no longer referenced by any row and is released.
func (s *Server) restoreClaimedPendingSecret(ctx context.Context, p *store.PendingSecret) {
	ctx = context.WithoutCancel(ctx)
	err := s.store.RestorePendingSecret(ctx, p)
	switch {
	case err == nil:
	case errors.Is(err, store.ErrAlreadyExists):
		slog.Warn("pending secret: a newer pending secret took the key; releasing the claimed value",
			"pending_id", p.ID)
		s.dropPendingValues(ctx, []string{p.ValueRef})
	default:
		slog.Warn("pending secret: putting back a claimed pending secret failed",
			"pending_id", p.ID, "error", err)
	}
}

func (s *Server) rejectPendingSecret(w http.ResponseWriter, r *http.Request, ownerID, id string) {
	ctx := r.Context()
	// Claim the row: of a concurrent approval and rejection exactly one
	// claims it, and the other answers 404.
	p, err := s.store.ClaimPendingSecret(ctx, id, store.ScopeUser, ownerID, time.Now())
	if err != nil {
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
