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
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Share links (design D5, D12, D19). A share link is a link grant: an
// artifact_grant row with subject kind "link" whose subject ref is the
// SHA-256 of a random token. The token itself is returned once, when the
// link is created, and is never stored, logged or echoed afterwards.
//
//	POST   /api/v1/artifacts/{id}/links            create a link (owner or admin grant, users only)
//	GET    /api/v1/artifacts/{id}/links            the unexpired links, without tokens
//	DELETE /api/v1/artifacts/{id}/links/{linkId}   revoke a link
//	GET    /api/v1/artifacts/shared/{token}[/files/{path}]
//
// A shared read carries no session. The service resolves the token and
// answers 303 to the view route (see view.go) under a view capability
// bound to the link, so every byte a link reaches is served by the one
// view handler, with its sandbox and Content-Security-Policy. The view
// route checks the link again on every request, so revoking a link, its
// expiry, or the artifact's deletion or expiry ends access at once.
//
// Every refusal of a shared read (malformed, unknown, expired or revoked
// token, deleted, expired or unpublished artifact) is the same 404 with the
// same body and headers, reached through the same path: the token is
// hashed first and looked up by its hash in one query whose conditions
// include every one of those cases.

const (
	// DefaultLinkTTL is the lifetime of a link created without one.
	DefaultLinkTTL = 7 * 24 * time.Hour
	// DefaultLinkMaxTTL is the longest lifetime a link may be given.
	DefaultLinkMaxTTL = 30 * 24 * time.Hour

	// MaxLinksPerArtifact caps the unexpired links of one artifact.
	MaxLinksPerArtifact = 50

	// linkTokenBytes is the entropy of a link token: 256 bits from the
	// system CSPRNG.
	linkTokenBytes = 32
	// linkTokenLen is the length of a token in unpadded base64url.
	linkTokenLen = 43

	// maxLinkRequestBytes caps the JSON body of a create request.
	maxLinkRequestBytes = 4 << 10
)

// CreateLinkRequest is the optional body of POST /{id}/links.
type CreateLinkRequest struct {
	// TTLHours is the link's lifetime in hours. 0 or absent means the
	// hub's default; more than the hub's maximum is refused.
	TTLHours int `json:"ttlHours,omitempty"`
}

// LinkInfo describes a share link. It never carries the token.
type LinkInfo struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"createdAt"`
	ExpiresAt time.Time `json:"expiresAt"`
	CreatedBy string    `json:"createdBy,omitempty"`
}

// CreateLinkResponse answers POST /{id}/links. URL is the only place the
// token ever appears; it is not retrievable later.
type CreateLinkResponse struct {
	Link LinkInfo `json:"link"`
	// URL is the hub-relative path of the link.
	URL string `json:"url"`
	// ClampedToArtifactExpiry is true when the link expires earlier than
	// asked because the artifact itself expires then.
	ClampedToArtifactExpiry bool `json:"clampedToArtifactExpiry,omitempty"`
}

// LinkListResponse answers GET /{id}/links.
type LinkListResponse struct {
	Links []LinkInfo `json:"links"`
}

// newLinkToken returns a fresh link token: linkTokenBytes from
// crypto/rand in unpadded base64url.
func newLinkToken() (string, error) {
	b := make([]byte, linkTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// validLinkToken reports whether t has the shape of a token newLinkToken
// returns. It only bounds what is hashed; it decides nothing about access.
func validLinkToken(t string) bool {
	if len(t) != linkTokenLen {
		return false
	}
	for i := 0; i < len(t); i++ {
		if c := t[i]; !isASCIIAlnum(c) && c != '-' && c != '_' {
			return false
		}
	}
	return true
}

// LinkTokenHash returns the subject ref a link token is stored under: the
// lowercase hex SHA-256 of the token.
func LinkTokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func linkInfo(g *Grant) LinkInfo {
	info := LinkInfo{ID: g.ID, CreatedAt: g.CreatedAt, CreatedBy: g.CreatedByRef}
	if g.ExpiresAt != nil {
		info.ExpiresAt = *g.ExpiresAt
	}
	return info
}

// linkLifetimes returns the default and maximum link lifetimes from the
// limits, defaulting unset values and never letting the default exceed
// the maximum.
func (l Limits) linkLifetimes() (def, maxTTL time.Duration) {
	def, maxTTL = l.LinkDefaultTTL, l.LinkMaxTTL
	if maxTTL <= 0 {
		maxTTL = DefaultLinkMaxTTL
	}
	if def <= 0 {
		def = DefaultLinkTTL
	}
	return min(def, maxTTL), maxTTL
}

// canAdminister reports whether the caller may administer a, an artifact
// it can read (so a is live and unexpired: readableArtifact checked it):
// manage its share links and grants, and change its expiry or home. It
// must be a user (sharing is user-only, design D15) whose credential
// permits artifact.manage in the home scope, and it must own the artifact
// or hold an unexpired admin grant (a principal grant for it, or a scope
// grant for a scope the host authorizes it to manage in).
//
// A failed grant read is an error, never a refusal, so the caller answers
// 500 rather than a 403 that a working read would not give.
func (s *Service) canAdminister(r *http.Request, b backend, a *Artifact) (bool, error) {
	ctx := r.Context()
	kind, ref, _, ok := s.host.Principal(ctx)
	if !ok || kind != PrincipalKindUser {
		return false, nil
	}
	if !s.host.Permits(ctx, a.ScopeRef, PermissionManage) {
		return false, nil
	}
	if kind == a.OwnerKind && ref == a.OwnerRef {
		return true, nil
	}
	grants, err := b.store.ListGrants(ctx, a.ID)
	if err != nil {
		return false, err
	}
	return grantAllows(ctx, s.host, a, grants, time.Now(), kind, ref, grantsForAdmin, PermissionManage, false), nil
}

// adminArtifact loads an artifact the caller may administer (see
// canAdminister). An artifact the caller cannot read answers 404 like a
// missing one (the read check runs first); one it can read but not
// administer answers 403.
func (s *Service) adminArtifact(w http.ResponseWriter, r *http.Request, id string) (backend, *Artifact, bool) {
	b, a, ok := s.readableArtifact(w, r, id)
	if !ok {
		return b, nil, false
	}
	allowed, err := s.canAdminister(r, b, a)
	if err != nil {
		slog.ErrorContext(r.Context(), "artifacts: list grants failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not read the artifact's grants")
		return b, nil, false
	}
	if !allowed {
		writeError(w, http.StatusForbidden, "forbidden", "only the artifact's owner or an admin user may share or change it")
		return b, nil, false
	}
	return b, a, true
}

// handleCreateLink implements POST /{id}/links.
func (s *Service) handleCreateLink(w http.ResponseWriter, r *http.Request, id string) {
	b, a, ok := s.adminArtifact(w, r, id)
	if !ok {
		return
	}
	req, ok := decodeLinkRequest(w, r)
	if !ok {
		return
	}
	if a.CurrentSeq == 0 {
		writeError(w, http.StatusConflict, "conflict", "the artifact has no published version to share yet")
		return
	}
	def, maxTTL := b.currentLimits(r.Context()).linkLifetimes()
	ttl := def
	if req.TTLHours < 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "ttlHours must be positive")
		return
	}
	if req.TTLHours > 0 {
		if int64(req.TTLHours) > int64(maxTTL/time.Hour) {
			writeError(w, http.StatusBadRequest, "ttl_too_long",
				"ttlHours exceeds the maximum link lifetime of "+strconv.FormatInt(int64(maxTTL/time.Hour), 10)+" hours")
			return
		}
		ttl = time.Duration(req.TTLHours) * time.Hour
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	exp := now.Add(ttl)
	clamped := false
	if a.ExpiresAt != nil && a.ExpiresAt.Before(exp) {
		exp, clamped = a.ExpiresAt.UTC(), true
	}
	token, err := newLinkToken()
	if err != nil {
		slog.ErrorContext(r.Context(), "artifacts: generate link token failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not create the link")
		return
	}
	kind, ref, _, _ := s.host.Principal(r.Context())
	g := &Grant{
		ID: uuid.NewString(), ArtifactID: a.ID, SubjectKind: SubjectLink, SubjectRef: LinkTokenHash(token),
		Permission: GrantRead, ExpiresAt: &exp, CreatedByRef: PrincipalRef(kind, ref), CreatedAt: now,
	}
	switch err := b.store.CreateLink(r.Context(), g, MaxLinksPerArtifact, now); {
	case errors.Is(err, ErrTooManyLinks):
		writeError(w, http.StatusConflict, "too_many_links",
			"the artifact already has "+strconv.Itoa(MaxLinksPerArtifact)+" active share links; revoke one first")
		return
	case errors.Is(err, ErrNotFound):
		writeNotFound(w)
		return
	case err != nil:
		slog.ErrorContext(r.Context(), "artifacts: create link failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not create the link")
		return
	}
	w.Header().Set("Referrer-Policy", "no-referrer")
	writeJSON(w, http.StatusCreated, CreateLinkResponse{
		Link: linkInfo(g), URL: RouteShared + token, ClampedToArtifactExpiry: clamped,
	})
}

// decodeLinkRequest reads the optional JSON body of a create request. An
// empty body asks for the defaults.
func decodeLinkRequest(w http.ResponseWriter, r *http.Request) (*CreateLinkRequest, bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxLinkRequestBytes+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "could not read the request body")
		return nil, false
	}
	if len(body) > maxLinkRequestBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "the request body is too large")
		return nil, false
	}
	var req CreateLinkRequest
	if len(bytes.TrimSpace(body)) == 0 {
		return &req, true
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || dec.More() {
		writeError(w, http.StatusBadRequest, "bad_request", "the body must be a JSON object such as {\"ttlHours\": 24}")
		return nil, false
	}
	return &req, true
}

// handleListLinks implements GET /{id}/links: the unexpired links, oldest
// first.
func (s *Service) handleListLinks(w http.ResponseWriter, r *http.Request, id string) {
	b, a, ok := s.adminArtifact(w, r, id)
	if !ok {
		return
	}
	grants, err := b.store.ListGrants(r.Context(), a.ID)
	if err != nil {
		slog.ErrorContext(r.Context(), "artifacts: list grants failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not read the artifact's links")
		return
	}
	now := time.Now()
	out := LinkListResponse{Links: []LinkInfo{}}
	for i := range grants {
		g := &grants[i]
		if g.SubjectKind != SubjectLink || g.ExpiresAt == nil || !now.Before(*g.ExpiresAt) {
			continue
		}
		out.Links = append(out.Links, linkInfo(g))
	}
	sort.SliceStable(out.Links, func(i, j int) bool { return out.Links[i].CreatedAt.Before(out.Links[j].CreatedAt) })
	writeJSON(w, http.StatusOK, out)
}

// handleRevokeLink implements DELETE /{id}/links/{linkId}.
func (s *Service) handleRevokeLink(w http.ResponseWriter, r *http.Request, id, linkID string) {
	if !canonicalID(linkID) {
		writeNotFound(w)
		return
	}
	b, a, ok := s.adminArtifact(w, r, id)
	if !ok {
		return
	}
	switch err := b.store.RevokeLink(r.Context(), a.ID, linkID); {
	case errors.Is(err, ErrNotFound):
		writeNotFound(w)
	case err != nil:
		slog.ErrorContext(r.Context(), "artifacts: revoke link failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not revoke the link")
	default:
		w.Header().Set("Cache-Control", "private, no-store")
		w.WriteHeader(http.StatusNoContent)
	}
}

// handleShared implements GET /api/v1/artifacts/shared/{token} (the
// entry of the current version) and .../shared/{token}/files/{path} (a
// file of it). segs are the path segments after "shared".
//
// The request is charged to the rate limiter before anything else, so a
// refused request costs the same whatever its token. Then the token is
// hashed and resolved in one query; every refusal from there on is
// writeSharedNotFound. A resolved link answers 303 to the view route
// under a view capability bound to the link.
func (s *Service) handleShared(w http.ResponseWriter, r *http.Request, segs []string) {
	// Every answer of this route, whatever it is, tells the browser not
	// to send the URL (which holds the token) onwards.
	w.Header().Set("Referrer-Policy", "no-referrer")
	if wait, ok := s.sharedLimiter().allow(s.clientKey(r)); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(wait))
		writeError(w, http.StatusTooManyRequests, "rate_limited", "too many requests")
		return
	}
	var token, filePath string
	switch {
	case len(segs) == 1:
		token = segs[0]
	case len(segs) >= 3 && segs[1] == "files" && segs[2] != "":
		token, filePath = segs[0], strings.Join(segs[2:], "/")
	default:
		writeSharedNotFound(w)
		return
	}
	if !validLinkToken(token) {
		writeSharedNotFound(w)
		return
	}
	b, ok := s.backend()
	if !ok || len(b.viewKey) == 0 {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "artifact sharing is not configured")
		return
	}
	hash := LinkTokenHash(token)
	ctx := r.Context()
	now := time.Now()
	a, g, err := b.store.ResolveLink(ctx, hash, now)
	if errors.Is(err, ErrNotFound) {
		writeSharedNotFound(w)
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "artifacts: resolve link failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not read the link")
		return
	}
	if subtle.ConstantTimeCompare([]byte(g.SubjectRef), []byte(hash)) != 1 {
		writeSharedNotFound(w)
		return
	}
	if filePath == "" {
		// current_seq only ever names a ready version (FinalizeVersion).
		v, err := b.store.GetVersion(ctx, a.ID, a.CurrentSeq)
		if errors.Is(err, ErrNotFound) {
			writeSharedNotFound(w)
			return
		}
		if err != nil {
			slog.ErrorContext(ctx, "artifacts: get version failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal", "could not read the link")
			return
		}
		filePath = v.EntryPath
	} else if _, err := cleanFilePath(filePath); err != nil {
		writeSharedNotFound(w)
		return
	}
	exp := now.Add(ViewTTL)
	if g.ExpiresAt.Before(exp) {
		exp = *g.ExpiresAt
	}
	if a.ExpiresAt != nil && a.ExpiresAt.Before(exp) {
		exp = *a.ExpiresAt
	}
	capability := mintLinkViewCapability(b.viewKey, a.ID, a.CurrentSeq, exp.Truncate(time.Second), g.ID)
	h := w.Header()
	h.Set("Location", RouteView+capability+"/"+escapePath(filePath))
	h.Set("Cache-Control", "private, no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusSeeOther)
}

// writeSharedNotFound is the one refusal of a shared read: the service's
// ordinary 404, with the Referrer-Policy handleShared set for every
// answer, so no status, header or body tells the cases apart.
func writeSharedNotFound(w http.ResponseWriter) {
	writeNotFound(w)
}
