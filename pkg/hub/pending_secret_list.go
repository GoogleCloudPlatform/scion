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
	"net/http"
	"sort"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Merged user secret list: GET /api/v1/secrets?scope=user&includePending=true.
// It returns the owner's approved user secrets and live pending secrets as
// one list, with a state per item, for the owner's own views (web profile,
// CLI). Delivery never calls it. Values are never included.

// Values of SecretListItem.State.
const (
	SecretStateApproved = "approved"
	SecretStatePending  = pendingSecretState
)

// SecretListItem is one entry of the merged user secret list. Exactly one
// of Secret (state "approved") and Pending (state "pending") is set. A key
// with an approved secret and a pending replacement appears twice, approved
// first.
type SecretListItem struct {
	State   string             `json:"state"`
	Key     string             `json:"key"`
	Secret  *store.Secret      `json:"secret,omitempty"`
	Pending *PendingSecretItem `json:"pending,omitempty"`
}

// PendingSecretItem is a pending secret in the merged list: the owner's
// view of it (PendingSecretView) plus what the approval would shadow.
type PendingSecretItem struct {
	PendingSecretView
	// OverridesProjectSecrets lists the project secrets of the same key,
	// in projects the caller can read, that this secret would override
	// once approved (user scope takes precedence over project scope). It
	// is empty, never null, when there are none.
	OverridesProjectSecrets []ProjectSecretOverride `json:"overridesProjectSecrets"`
}

// ProjectSecretOverride names a project secret a pending user secret would
// override.
type ProjectSecretOverride struct {
	ProjectID   string `json:"projectId"`
	ProjectName string `json:"projectName"`
}

// listSecretsWithPending writes the merged list for ownerID. The caller has
// checked that the request is ownerID's own session (resolveEnvSecretAccess
// for a user-scope write). The key and type query filters apply to both
// states.
func (s *Server) listSecretsWithPending(w http.ResponseWriter, r *http.Request, ownerID string) {
	ctx := r.Context()
	query := r.URL.Query()
	keyFilter, typeFilter := query.Get("key"), query.Get("type")

	metas, err := s.secretBackend.List(ctx, secret.Filter{
		Scope:   store.ScopeUser,
		ScopeID: ownerID,
		Name:    keyFilter,
		Type:    typeFilter,
	})
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	pending, err := s.store.ListPendingSecretsByOwner(ctx, store.ScopeUser, ownerID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	approved := make([]store.Secret, len(metas))
	items := make([]SecretListItem, 0, len(metas)+len(pending))
	for i, m := range metas {
		approved[i] = metaToStoreSecret(m)
		items = append(items, SecretListItem{State: SecretStateApproved, Key: m.Name, Secret: &approved[i]})
	}

	overrides := newProjectOverrideLookup(s)
	now := time.Now()
	for _, p := range pending {
		if p.Expired(now) || (keyFilter != "" && p.Key != keyFilter) || (typeFilter != "" && p.SecretType != typeFilter) {
			continue
		}
		over, err := overrides.forKey(ctx, p.Key)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		items = append(items, SecretListItem{
			State: SecretStatePending,
			Key:   p.Key,
			Pending: &PendingSecretItem{
				PendingSecretView:       pendingSecretToView(p),
				OverridesProjectSecrets: over,
			},
		})
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Key != items[j].Key {
			return items[i].Key < items[j].Key
		}
		return items[i].State == SecretStateApproved && items[j].State != SecretStateApproved
	})

	writeJSON(w, http.StatusOK, ListSecretsResponse{
		Secrets: approved,
		Scope:   store.ScopeUser,
		ScopeID: ownerID,
		Items:   items,
	})
}

// projectOverrideLookup finds, per key, the project secrets of that key in
// projects the caller can read. It costs one secrets query per key plus
// one project lookup and access check per distinct project holding such a
// key; the caller's pending secrets are capped
// (store.PendingSecretOwnerCap), so the work is bounded.
type projectOverrideLookup struct {
	s       *Server
	visible map[string]*store.Project // project ID -> project, nil when not readable
}

func newProjectOverrideLookup(s *Server) *projectOverrideLookup {
	return &projectOverrideLookup{s: s, visible: map[string]*store.Project{}}
}

func (l *projectOverrideLookup) forKey(ctx context.Context, key string) ([]ProjectSecretOverride, error) {
	out := []ProjectSecretOverride{}
	rows, err := l.s.store.ListSecrets(ctx, store.SecretFilter{Scope: store.ScopeProject, Key: key})
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		project, err := l.readableProject(ctx, row.ScopeID)
		if err != nil {
			return nil, err
		}
		if project == nil {
			continue
		}
		out = append(out, ProjectSecretOverride{ProjectID: project.ID, ProjectName: project.Name})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ProjectName < out[j].ProjectName })
	return out, nil
}

// readableProject returns the project when the caller may read it, nil
// when it is gone or not readable.
func (l *projectOverrideLookup) readableProject(ctx context.Context, projectID string) (*store.Project, error) {
	if p, seen := l.visible[projectID]; seen {
		return p, nil
	}
	l.visible[projectID] = nil
	userIdent := GetUserIdentityFromContext(ctx)
	if userIdent == nil {
		return nil, nil
	}
	project, err := l.s.store.GetProject(ctx, projectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	decision := l.s.authzService.CheckAccess(ctx, userIdent, Resource{
		Type:    "project",
		ID:      project.ID,
		OwnerID: project.OwnerID,
	}, ActionRead)
	if !decision.Allowed {
		return nil, nil
	}
	l.visible[projectID] = project
	return project, nil
}
