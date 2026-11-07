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
//	POST   /api/v1/artifacts/{id}/links            create a link (owner, admin grant or steward; users only)
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
	// DefaultTTLHours is the lifetime, in hours, of a link created without
	// ttlHours.
	DefaultTTLHours int64 `json:"defaultTtlHours"`
	// MaxTTLHours is the longest lifetime, in hours, a link can be created
	// with; a longer ttlHours is refused with ttl_too_long.
	MaxTTLHours int64 `json:"maxTtlHours"`
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

// canAdministerWith reports whether the caller has full administration of
// a, an artifact it can read (so a is live and unexpired: readableArtifact
// checked it): manage its share links and grants, and change its expiry or
// home. It must be a user (sharing is user-only, design D15) whose
// credential permits artifact.manage in the home scope, and it must own
// the artifact or hold an unexpired admin grant (a principal grant for it,
// or a scope grant for a scope the host authorizes it to manage in).
//
// A failed grant read is an error, never a refusal, so the caller answers
// 500 rather than a 403 that a working read would not give. a's grants are
// read through grants, which is called only when the decision needs them.
// Stewards do not count: this is full administration (CanManage), which a
// steward does not have (no admin grant, no move); adminAuthorityWith
// admits them.
func (s *Service) canAdministerWith(ctx context.Context, a *Artifact, grants func() ([]Grant, error)) (bool, error) {
	authority, err := s.managerAuthorityWith(ctx, a, grants)
	return authority != "", err
}

// adminAuthorityWith reports the authority the caller administers a by:
// managerAuthorityWith's AuthorityOwner or AuthorityAdminGrant, then,
// only for an artifact owned by an agent, a steward clause
// (stewardAuthority), or "" when it may not. Events record it. A steward
// authority is bounded: the routes refuse it an admin grant and a move
// (steward_action_not_allowed), and its store writes carry the state it
// was decided against (callerActor).
func (s *Service) adminAuthorityWith(ctx context.Context, a *Artifact, grants func() ([]Grant, error)) (string, error) {
	authority, err := s.managerAuthorityWith(ctx, a, grants)
	if err != nil || authority != "" {
		return authority, err
	}
	return s.stewardAuthority(ctx, a), nil
}

// stewardAuthority reports the steward clause the caller holds on a, as
// an authority (AuthorityStewardProjectOwner, AuthorityStewardDelegator),
// or "". Only an artifact owned by an agent has stewards, and only a user
// whose credential permits artifact.manage in the home project can be one;
// a host without Stewardship has none.
func (s *Service) stewardAuthority(ctx context.Context, a *Artifact) string {
	if a.OwnerKind != PrincipalKindAgent {
		return ""
	}
	kind, _, _, ok := s.host.Principal(ctx)
	if !ok || kind != PrincipalKindUser || !s.host.Permits(ctx, a.ScopeRef, PermissionManage) {
		return ""
	}
	st, ok := s.host.(Stewardship)
	if !ok {
		return ""
	}
	switch st.StewardClause(ctx, a.OwnerRef, a.ScopeRef, a.SponsorRef, a.OrphanedAt != nil) {
	case StewardClauseProjectOwner:
		return AuthorityStewardProjectOwner
	case StewardClauseDelegator:
		return AuthorityStewardDelegator
	}
	return ""
}

// managerAuthorityWith reports the authority by which the caller has full
// administration of a: AuthorityOwner, AuthorityAdminGrant, or "".
func (s *Service) managerAuthorityWith(ctx context.Context, a *Artifact, grants func() ([]Grant, error)) (string, error) {
	kind, ref, _, ok := s.host.Principal(ctx)
	if !ok || kind != PrincipalKindUser {
		return "", nil
	}
	if !s.host.Permits(ctx, a.ScopeRef, PermissionManage) {
		return "", nil
	}
	if kind == a.OwnerKind && ref == a.OwnerRef {
		return AuthorityOwner, nil
	}
	gs, err := grants()
	if err != nil {
		return "", err
	}
	if grantAllows(ctx, s.host, a, gs, time.Now(), kind, ref, grantsForAdmin, PermissionManage, false) {
		return AuthorityAdminGrant, nil
	}
	return "", nil
}

// callerActor is the event actor for the caller of ctx acting on a by
// authority. For a steward authority it carries a's home and owner as
// they were when the authority was decided (Actor.Expect), so the store
// refuses the write if a move or an ownership transfer committed since
// (design 4.5). Every steward write gets its actor here.
func (s *Service) callerActor(ctx context.Context, a *Artifact, authority string) *Actor {
	kind, ref, _, _ := s.host.Principal(ctx)
	actor := &Actor{Kind: kind, Ref: ref, Authority: authority}
	if isSteward(authority) {
		actor.Expect = expectFor(a)
	}
	return actor
}

// adminArtifact loads an artifact the caller may administer
// (adminAuthorityWith: the owner, an admin grantee or a steward). An
// artifact the caller cannot read answers 404 like a missing one (the read
// check runs first); one it can read but not administer answers 403.
//
// actor is the caller, with the authority it administers the artifact by,
// for the events the change records.
func (s *Service) adminArtifact(w http.ResponseWriter, r *http.Request, id string) (b backend, a *Artifact, actor *Actor, ok bool) {
	b, a, ok = s.readableArtifact(w, r, id)
	if !ok {
		return b, nil, nil, false
	}
	authority, err := s.adminAuthorityWith(r.Context(), a, func() ([]Grant, error) { return b.store.ListGrants(r.Context(), a.ID) })
	if err != nil {
		slog.ErrorContext(r.Context(), "artifacts: list grants failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not read the artifact's grants")
		return b, nil, nil, false
	}
	if authority == "" {
		s.logAdminRefusal(r, logActionAdminister, a)
		s.writeAdminForbidden(w, r)
		return b, nil, nil, false
	}
	return b, a, s.callerActor(r.Context(), a, authority), true
}

// adminForbiddenMessage is the text of every 403 for a user that may read
// an artifact but not administer it. It states the rule, the same for every
// artifact and every refusal, and never why this caller was refused. One
// narrow exception (ptone/scion#5119): a caller that holds review-grant
// authority on an artifact owned by an agent, asking for a well-formed
// review grant, learns when the user cannot read the artifact
// (CodeReviewSubjectCannotRead) or already holds a read, write or admin
// grant (CodeReviewReplacesAccess); see putReviewGrant.
const adminForbiddenMessage = "only the artifact's owner or a user with an admin grant may share or change it; " +
	"on an artifact owned by an agent, the agent's delegating user and the owners of its home project may also share it, " +
	"and an admin of the artifact's home project may grant review access"

// agentAdminForbiddenMessage is the same 403's text for an agent, which may
// never administer an artifact, its own included (sharing is user-only), so
// the user text would read as if it were not the owner (ptone/scion#4576).
// It too is the same for every artifact and every refusal.
const agentAdminForbiddenMessage = "agents cannot share artifacts or change an artifact's grants, share links, expiry or home project; " +
	"users do that: the artifact's owner when the owner is a user, or a user with an admin grant; " +
	"on an artifact owned by an agent, the agent's delegating user and the owners of its home project may also share it, " +
	"and an admin of the artifact's home project may grant review access"

// writeAdminForbidden writes the 403 of a caller that may read an artifact
// but not administer it: the agent text for an agent, the user text
// otherwise. Its callers run it only after the read check, so a caller that
// cannot read the artifact still gets the 404 of a missing one.
func (s *Service) writeAdminForbidden(w http.ResponseWriter, r *http.Request) {
	msg := adminForbiddenMessage
	if kind, _, _, ok := s.host.Principal(r.Context()); ok && kind == PrincipalKindAgent {
		msg = agentAdminForbiddenMessage
	}
	writeError(w, http.StatusForbidden, "forbidden", msg)
}

// manageable reports the authority by which the caller may administer a,
// which it can read, for a response that offers sharing and changing it
// (adminAuthorityWith: "" when it may not). A failed grants read answers
// 500 and returns ok false.
func (s *Service) manageable(w http.ResponseWriter, r *http.Request, b backend, a *Artifact) (authority string, ok bool) {
	authority, err := s.adminAuthorityWith(r.Context(), a, func() ([]Grant, error) { return b.store.ListGrants(r.Context(), a.ID) })
	if err != nil {
		slog.ErrorContext(r.Context(), "artifacts: list grants failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not read the artifact's grants")
		return "", false
	}
	return authority, true
}

// shareAuthorityOf is ArtifactResponse.ShareAuthority for a steward
// authority: the steward clause, or "" for any other authority.
func shareAuthorityOf(authority string) string {
	switch authority {
	case AuthorityStewardProjectOwner:
		return StewardClauseProjectOwner
	case AuthorityStewardDelegator:
		return StewardClauseDelegator
	}
	return ""
}

// capabilities fills the caller's rights on a, an artifact it can read
// (readableArtifact checked it), into resp: CanManage (owner or admin
// grant), CanShare (CanManage or a steward) and ShareAuthority (the steward
// clause) from adminAuthorityWith, CanRetire from the same authority
// through retireAuthorityFrom (the delete route's decision),
// CanPublish and CanReview from canWriteErr (reviewCapability), and
// CanGrantReview from mayGrantReview (only when CanManage is false), the
// decisions the management, write and grant routes make for the same
// request. A failed grants read answers 500 and returns false.
//
// It must run only after readableArtifact: a caller that cannot read the
// artifact gets the 404 of a missing one and learns none of these. The
// flags are advice: each route decides again when it is called (the
// delete route through retireAuthority), so a flag that went stale since
// the GET never authorizes anything.
func (s *Service) capabilities(w http.ResponseWriter, r *http.Request, b backend, a *Artifact, resp *ArtifactResponse) bool {
	authority, ok := s.manageable(w, r, b, a)
	if !ok {
		return false
	}
	resp.CanManage = authority == AuthorityOwner || authority == AuthorityAdminGrant
	resp.CanShare = authority != ""
	resp.ShareAuthority = shareAuthorityOf(authority)
	resp.CanRetire = s.retireAuthorityFrom(r.Context(), a, authority) != ""
	access, err := s.canWriteErr(r.Context(), b, a)
	if err != nil {
		writeGrantsReadFailed(w, r, err)
		return false
	}
	resp.CanPublish, resp.CanReview = reviewCapability(true, access)
	resp.CanGrantReview = !resp.CanManage && s.mayGrantReview(r.Context(), a)
	return true
}

// reviewCapability maps the write decision to CanPublish and CanReview for
// a caller whose read check answered readable. CanReview is true only for
// a reader whose access allows review versions and nothing more: it adds
// nothing when CanPublish is true, and it is never true without the read.
// capabilities runs only after readableArtifact, so it always passes true;
// the parameter states that precondition where the capability is
// decided, and the kill check for "canReview without the read check"
// mutates it.
func reviewCapability(readable bool, access writeAccess) (canPublish, canReview bool) {
	if !readable {
		return false, false
	}
	return access.canPublish(), access == accessReviewGrant
}

// writeGrantsReadFailed logs a failed grant read and answers 500.
func writeGrantsReadFailed(w http.ResponseWriter, r *http.Request, err error) {
	slog.ErrorContext(r.Context(), "artifacts: list grants failed", "error", err)
	writeError(w, http.StatusInternalServerError, "internal", "could not read the artifact's grants")
}

// handleCreateLink implements POST /{id}/links.
func (s *Service) handleCreateLink(w http.ResponseWriter, r *http.Request, id string) {
	b, a, actor, ok := s.adminArtifact(w, r, id)
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
	g := &Grant{
		ID: uuid.NewString(), ArtifactID: a.ID, SubjectKind: SubjectLink, SubjectRef: LinkTokenHash(token),
		Permission: GrantRead, ExpiresAt: &exp, CreatedByRef: PrincipalRef(actor.Kind, actor.Ref), CreatedAt: now,
	}
	switch err := b.store.CreateLink(r.Context(), g, MaxLinksPerArtifact, now, actor); {
	case errors.Is(err, ErrAttachmentNotShareable):
		writeAttachmentNotShareable(w)
		return
	case errors.Is(err, ErrTooManyLinks):
		writeError(w, http.StatusConflict, "too_many_links",
			"the artifact already has "+strconv.Itoa(MaxLinksPerArtifact)+" active share links; revoke one first")
		return
	case errors.Is(err, ErrNotFound):
		s.logStewardPrecondition(r, logActionShare, a, err)
		writeNotFound(w)
		return
	case err != nil:
		slog.ErrorContext(r.Context(), "artifacts: create link failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not create the link")
		return
	}
	s.logLink(r, a, logActionLinkCreated, actor)
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
// first, and the link lifetimes POST /{id}/links applies.
func (s *Service) handleListLinks(w http.ResponseWriter, r *http.Request, id string) {
	b, a, _, ok := s.adminArtifact(w, r, id)
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
	def, maxTTL := b.currentLimits(r.Context()).linkLifetimes()
	out := LinkListResponse{
		Links:           []LinkInfo{},
		DefaultTTLHours: int64(def / time.Hour),
		MaxTTLHours:     int64(maxTTL / time.Hour),
	}
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
	b, a, actor, ok := s.adminArtifact(w, r, id)
	if !ok {
		return
	}
	switch err := b.store.RevokeLink(r.Context(), a.ID, linkID, actor); {
	case errors.Is(err, ErrNotFound):
		s.logStewardPrecondition(r, logActionAdminister, a, err)
		writeNotFound(w)
	case err != nil:
		slog.ErrorContext(r.Context(), "artifacts: revoke link failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not revoke the link")
	default:
		s.logLink(r, a, logActionLinkRevoked, actor)
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
	if !ok || !b.viewKeys.configured() {
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
	// ResolveLink only returns unexpired links, so a link grant always has
	// an expiry here; a row without one is refused like any other.
	if g.ExpiresAt == nil {
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
	capability, err := mintView(b.viewKeys, a.ID, a.CurrentSeq, exp.Truncate(time.Second), g.ID)
	if err != nil {
		// No key may sign now (the active generation's sign_until
		// passed): answer as when sharing is not configured.
		s.warnViewIssuanceClosed(r)
		writeError(w, http.StatusServiceUnavailable, "unavailable", "artifact sharing is not configured")
		return
	}
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
