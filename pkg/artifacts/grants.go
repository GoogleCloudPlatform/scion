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

package artifacts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// Grants (design §5.2, D7, D16). A grant gives a principal (a user or an
// agent) or a scope (a project's members) read, write or admin on one
// artifact. Grants are read at request time by the same read check every
// route uses (canReadWith), so a grant never reaches past what the
// caller's credential permits (Host.Permits, for agents including the live
// delegation chain).
//
//	GET    /api/v1/artifacts/{id}/grants             the principal and scope grants
//	POST   /api/v1/artifacts/{id}/grants             add a grant, or change a subject's permission
//	DELETE /api/v1/artifacts/{id}/grants/{grantId}   remove a grant
//	PATCH  /api/v1/artifacts/{id}                    set or clear the expiry; move to another home project
//
// All of them need canAdminister. The home project's read grant (the
// scope grant naming the current home scope) is listed with home set; its
// permission may be read or write, and it cannot be removed. A scope grant
// to any other project needs the host to allow sharing across projects
// (CrossScopeSharing) when it is created; existing grants keep working if
// that is turned off later.

// MaxGrantsPerArtifact caps the principal and scope grants of one
// artifact, the home project's included.
const MaxGrantsPerArtifact = 100

const (
	// maxGrantRequestBytes caps the JSON body of a grant or patch request.
	maxGrantRequestBytes = 4 << 10
	// maxSubjectRefBytes caps a grant subject's id.
	maxSubjectRefBytes = 256
)

// CrossScopeSharing is an optional extension of Host. A scope grant to a
// scope other than the artifact's home is created only when the host
// implements it and it answers true; otherwise such a grant is refused
// (fail closed).
type CrossScopeSharing interface {
	// CrossScopeSharingAllowed reports whether artifacts may be shared
	// with scopes other than their home scope.
	CrossScopeSharingAllowed(ctx context.Context) bool
}

// GrantRequest is the body of POST /{id}/grants.
type GrantRequest struct {
	// SubjectKind is "principal" or "scope".
	SubjectKind string `json:"subjectKind"`
	// SubjectRef is "user:<id>" or "agent:<id>" for a principal and a
	// project id for a scope.
	SubjectRef string `json:"subjectRef"`
	// Permission is "read", "write" or "admin".
	Permission string `json:"permission"`
}

// GrantInfo describes a principal or scope grant.
type GrantInfo struct {
	ID          string `json:"id"`
	SubjectKind string `json:"subjectKind"`
	SubjectRef  string `json:"subjectRef"`
	Permission  string `json:"permission"`
	// Home is true for the home project's grant.
	Home      bool      `json:"home,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	CreatedBy string    `json:"createdBy,omitempty"`
}

// GrantResponse answers POST /{id}/grants.
type GrantResponse struct {
	Grant GrantInfo `json:"grant"`
}

// GrantListResponse answers GET /{id}/grants.
type GrantListResponse struct {
	Grants []GrantInfo `json:"grants"`
}

// PatchArtifactRequest is the body of PATCH /{id}. Each field present is
// applied; at least one must be.
type PatchArtifactRequest struct {
	// ExpiresAt sets the expiry; JSON null clears it.
	ExpiresAt optionalTime `json:"expiresAt"`
	// ScopeRef moves the artifact to another home project.
	ScopeRef *string `json:"scopeRef,omitempty"`
}

// optionalTime tells an absent field from an explicit null.
type optionalTime struct {
	Set   bool
	Value *time.Time
}

func (o *optionalTime) UnmarshalJSON(b []byte) error {
	o.Set = true
	if string(b) == "null" {
		o.Value = nil
		return nil
	}
	var t time.Time
	if err := json.Unmarshal(b, &t); err != nil {
		return err
	}
	o.Value = &t
	return nil
}

// PatchArtifactResponse answers PATCH /{id}.
type PatchArtifactResponse struct {
	Artifact ArtifactInfo `json:"artifact"`
	// LinksCutShort counts the unexpired share links that will end when
	// the artifact expires, before their own expiry.
	LinksCutShort int `json:"linksCutShort,omitempty"`
	// GrantsRemoved counts the grants that will be removed with the
	// artifact when it expires.
	GrantsRemoved int `json:"grantsRemoved,omitempty"`
}

func grantInfo(a *Artifact, g *Grant) GrantInfo {
	return GrantInfo{
		ID: g.ID, SubjectKind: g.SubjectKind, SubjectRef: g.SubjectRef, Permission: g.Permission,
		Home:      g.SubjectKind == SubjectScope && g.SubjectRef == a.ScopeRef,
		CreatedAt: g.CreatedAt, CreatedBy: g.CreatedByRef,
	}
}

// decodeSmallJSON reads a JSON object body of at most
// maxGrantRequestBytes into v, refusing unknown fields and trailing data.
func decodeSmallJSON(w http.ResponseWriter, r *http.Request, v any, shape string) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxGrantRequestBytes+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "could not read the request body")
		return false
	}
	if len(body) > maxGrantRequestBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "the request body is too large")
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil || dec.More() {
		writeError(w, http.StatusBadRequest, "bad_request", "the body must be a JSON object such as "+shape)
		return false
	}
	return true
}

// validSubjectID accepts a non-empty id of printable characters without
// spaces or colons, at most maxSubjectRefBytes long.
func validSubjectID(id string) bool {
	if id == "" || len(id) > maxSubjectRefBytes || !utf8.ValidString(id) {
		return false
	}
	for _, r := range id {
		if r <= ' ' || r == 0x7f || r == ':' || (r >= 0x80 && r < 0xa0) {
			return false
		}
	}
	return true
}

// validGrantSubject checks a grant request's subject.
func validGrantSubject(kind, ref string) bool {
	switch kind {
	case SubjectPrincipal:
		pk, id, ok := strings.Cut(ref, ":")
		return ok && (pk == PrincipalKindUser || pk == PrincipalKindAgent) && validSubjectID(id)
	case SubjectScope:
		return validSubjectID(ref)
	}
	return false
}

// crossScopeAllowed asks the host whether sharing across scopes is on.
func (s *Service) crossScopeAllowed(ctx context.Context) bool {
	c, ok := s.host.(CrossScopeSharing)
	return ok && c.CrossScopeSharingAllowed(ctx)
}

// handleListGrants implements GET /{id}/grants: the principal and scope
// grants, the home project's first, then oldest first.
func (s *Service) handleListGrants(w http.ResponseWriter, r *http.Request, id string) {
	b, a, ok := s.adminArtifact(w, r, id)
	if !ok {
		return
	}
	grants, err := b.store.ListGrants(r.Context(), a.ID)
	if err != nil {
		slog.ErrorContext(r.Context(), "artifacts: list grants failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not read the artifact's grants")
		return
	}
	out := GrantListResponse{Grants: []GrantInfo{}}
	for i := range grants {
		g := &grants[i]
		if g.SubjectKind != SubjectPrincipal && g.SubjectKind != SubjectScope {
			continue
		}
		out.Grants = append(out.Grants, grantInfo(a, g))
	}
	sort.SliceStable(out.Grants, func(i, j int) bool {
		if out.Grants[i].Home != out.Grants[j].Home {
			return out.Grants[i].Home
		}
		return out.Grants[i].CreatedAt.Before(out.Grants[j].CreatedAt)
	})
	writeJSON(w, http.StatusOK, out)
}

// handlePutGrant implements POST /{id}/grants. It answers 201 when it
// created the grant and 200 when it changed an existing grant's
// permission.
func (s *Service) handlePutGrant(w http.ResponseWriter, r *http.Request, id string) {
	b, a, ok := s.adminArtifact(w, r, id)
	if !ok {
		return
	}
	var req GrantRequest
	if !decodeSmallJSON(w, r, &req, `{"subjectKind": "principal", "subjectRef": "user:<id>", "permission": "read"}`) {
		return
	}
	if !validGrantSubject(req.SubjectKind, req.SubjectRef) {
		writeError(w, http.StatusBadRequest, "bad_request",
			`subjectKind must be "principal" (subjectRef "user:<id>" or "agent:<id>") or "scope" (subjectRef a project id)`)
		return
	}
	if req.Permission != GrantRead && req.Permission != GrantWrite && req.Permission != GrantAdmin {
		writeError(w, http.StatusBadRequest, "bad_request", `permission must be "read", "write" or "admin"`)
		return
	}
	home := req.SubjectKind == SubjectScope && req.SubjectRef == a.ScopeRef
	if home && req.Permission == GrantAdmin {
		writeError(w, http.StatusBadRequest, "bad_request", "the home project's grant may be read or write")
		return
	}
	if req.SubjectKind == SubjectScope && !home && !s.crossScopeAllowed(r.Context()) {
		writeError(w, http.StatusForbidden, "cross_project_sharing_disabled",
			"sharing artifacts with other projects is turned off on this hub")
		return
	}
	kind, ref, _, _ := s.host.Principal(r.Context())
	g := &Grant{
		ID: uuid.NewString(), ArtifactID: a.ID, SubjectKind: req.SubjectKind, SubjectRef: req.SubjectRef,
		Permission: req.Permission, CreatedByRef: PrincipalRef(kind, ref), CreatedAt: time.Now().UTC(),
	}
	created, err := b.store.PutGrant(r.Context(), g, MaxGrantsPerArtifact)
	switch {
	case errors.Is(err, ErrTooManyGrants):
		writeError(w, http.StatusConflict, "too_many_grants",
			"the artifact already has "+strconv.Itoa(MaxGrantsPerArtifact)+" grants; remove one first")
		return
	case errors.Is(err, ErrNotFound):
		writeNotFound(w)
		return
	case err != nil:
		slog.ErrorContext(r.Context(), "artifacts: put grant failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not record the grant")
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, GrantResponse{Grant: grantInfo(a, g)})
}

// handleDeleteGrant implements DELETE /{id}/grants/{grantId}.
func (s *Service) handleDeleteGrant(w http.ResponseWriter, r *http.Request, id, grantID string) {
	if !canonicalID(grantID) {
		writeNotFound(w)
		return
	}
	b, a, ok := s.adminArtifact(w, r, id)
	if !ok {
		return
	}
	switch err := b.store.DeleteGrant(r.Context(), a.ID, grantID); {
	case errors.Is(err, ErrConflict):
		writeError(w, http.StatusConflict, "home_grant", "the home project's grant cannot be removed; move the artifact instead")
	case errors.Is(err, ErrNotFound):
		writeNotFound(w)
	case err != nil:
		slog.ErrorContext(r.Context(), "artifacts: delete grant failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not remove the grant")
	default:
		w.Header().Set("Cache-Control", "private, no-store")
		w.WriteHeader(http.StatusNoContent)
	}
}

// handlePatchArtifact implements PATCH /{id}.
//
// expiresAt sets or clears the expiry. An expiry in the past is refused;
// shortening it below active links is allowed, and the response counts the
// links cut short and the grants that will be removed (design A1, A2).
//
// scopeRef moves the artifact to another home project (D16). It needs, on
// top of canAdminister, exactly the gate publishing into that project has
// (Permits and Authorize for artifact.create there, for every caller), and
// because it gives that project's members access, the host's switch for
// sharing across projects. The old home project's grant is removed (a
// move is not a share; keeping access takes an explicit grant); the new
// home project gets a read grant unless it already has a scope grant.
// Both changes happen in one store transaction.
func (s *Service) handlePatchArtifact(w http.ResponseWriter, r *http.Request, id string) {
	b, a, ok := s.adminArtifact(w, r, id)
	if !ok {
		return
	}
	var req PatchArtifactRequest
	if !decodeSmallJSON(w, r, &req, `{"expiresAt": "2026-12-31T00:00:00Z"} or {"scopeRef": "<project-id>"}`) {
		return
	}
	if !req.ExpiresAt.Set && req.ScopeRef == nil {
		writeError(w, http.StatusBadRequest, "bad_request", "nothing to change: set expiresAt or scopeRef")
		return
	}
	ctx := r.Context()
	now := time.Now()
	if req.ExpiresAt.Set && req.ExpiresAt.Value != nil && !req.ExpiresAt.Value.After(now) {
		writeError(w, http.StatusBadRequest, "bad_request", "expiresAt must be in the future")
		return
	}
	if req.ScopeRef != nil {
		scope := *req.ScopeRef
		if !validSubjectID(scope) {
			writeError(w, http.StatusBadRequest, "bad_request", "scopeRef must be a project id")
			return
		}
		if !s.host.Permits(ctx, scope, PermissionCreate) || !s.host.Authorize(ctx, scope, PermissionCreate) {
			writeError(w, http.StatusForbidden, "forbidden", "not allowed to publish into that project")
			return
		}
		if scope != a.ScopeRef && !s.crossScopeAllowed(ctx) {
			writeError(w, http.StatusForbidden, "cross_project_sharing_disabled",
				"moving artifacts to other projects is turned off on this hub")
			return
		}
	}
	u := ArtifactUpdate{SetExpiry: req.ExpiresAt.Set, ExpiresAt: req.ExpiresAt.Value, MaxGrants: MaxGrantsPerArtifact}
	if req.ScopeRef != nil && *req.ScopeRef != a.ScopeRef {
		kind, ref, _, _ := s.host.Principal(ctx)
		u.HomeGrant = &Grant{
			ID: uuid.NewString(), ArtifactID: a.ID, SubjectKind: SubjectScope, SubjectRef: *req.ScopeRef,
			Permission: GrantRead, CreatedByRef: PrincipalRef(kind, ref), CreatedAt: now.UTC(),
		}
	}
	updated := a
	if u.HomeGrant != nil || u.SetExpiry {
		var err error
		updated, err = b.store.UpdateArtifact(ctx, a.ID, u)
		if !s.patchStored(w, r, err) {
			return
		}
	}
	resp := PatchArtifactResponse{Artifact: artifactInfo(updated)}
	if updated.ExpiresAt != nil {
		grants, err := b.store.ListGrants(ctx, a.ID)
		if err != nil {
			slog.ErrorContext(ctx, "artifacts: list grants failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal", "the change was saved, but the artifact's grants could not be read")
			return
		}
		for _, g := range grants {
			switch g.SubjectKind {
			case SubjectLink:
				if g.ExpiresAt != nil && now.Before(*g.ExpiresAt) && updated.ExpiresAt.Before(*g.ExpiresAt) {
					resp.LinksCutShort++
				}
			case SubjectPrincipal, SubjectScope:
				resp.GrantsRemoved++
			}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// patchStored writes the response for a failed store write of a patch and
// reports whether the write succeeded.
func (s *Service) patchStored(w http.ResponseWriter, r *http.Request, err error) bool {
	switch {
	case err == nil:
		return true
	case errors.Is(err, ErrConflict):
		writeError(w, http.StatusConflict, "conflict", "the owner already has an artifact with this key in that project")
	case errors.Is(err, ErrTooManyGrants):
		writeError(w, http.StatusConflict, "too_many_grants",
			"the artifact already has "+strconv.Itoa(MaxGrantsPerArtifact)+" grants; remove one first")
	case errors.Is(err, ErrNotFound):
		writeNotFound(w)
	default:
		slog.ErrorContext(r.Context(), "artifacts: update artifact failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not update the artifact")
	}
	return false
}

// retentionExpiry is the expiry of an artifact created at now under l: now
// plus the default retention, or none.
func retentionExpiry(l Limits, now time.Time) *time.Time {
	if l.DefaultRetention <= 0 {
		return nil
	}
	t := now.Add(l.DefaultRetention).UTC()
	return &t
}
