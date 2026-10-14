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
	"context"
	"errors"
	"strings"
)

// The subject read check of a review grant (design D24, ptone/scion#4538).
//
// A review grant is given only to a user who can already read the
// artifact through the home project or a grant to one of the user's
// projects. A review grant is never itself a read path, and the write
// decision asks again at every use, so the check here refuses a grant
// that could never be used. That is a question
// about a user other than the caller. The service answers it with the one
// read decision, canReadWith, asked through subjectReadHost, a Host whose
// caller is the subject. Nothing here grants access, serves content or
// changes who the caller of a request is.

// ReviewSubjectAuthority is an optional extension of Host for the subject
// read check of a review grant. Unlike every Host method, it answers for a
// named principal (kind, ref) rather than for the caller of ctx: the
// subject of a grant has no credential in the granter's request.
type ReviewSubjectAuthority interface {
	// ReviewSubjectAuthorize reports whether host policy lets the
	// principal (kind, ref) exercise permission on artifacts in the scope
	// scopeRef: the Authorize question asked of the principal, without a
	// credential step. It fails closed (false) for a kind other than user,
	// an unknown or inactive user, an empty scope or an unknown
	// permission, and returns an error only when a lookup failed.
	ReviewSubjectAuthorize(ctx context.Context, kind, ref, scopeRef, permission string) (bool, error)
}

// reviewSubjectCanRead reports whether the user principal subject
// ("user:<id>") can read a through a path the review grant leaves in
// place: canReadWith, with subjectReadHost as the host and a's current
// grants except the subject's own principal grant. A subject has one
// principal grant per artifact, and the review grant takes its row, so
// that grant can never be the read a review grant counts with; the paths
// left are the home project and grants to the subject's projects. A host
// without ReviewSubjectAuthority answers no (fail closed). A failed lookup
// is an error, never a refusal.
//
// By design, whether the grant is made follows this answer, so the outcome
// of a review-access request reflects whether the named user can already
// read the artifact. That is accepted: only the delegating user and
// home-project admins reach this check, and every grant made is recorded.
func (s *Service) reviewSubjectCanRead(ctx context.Context, b backend, a *Artifact, subject string) (bool, error) {
	auth, ok := s.host.(ReviewSubjectAuthority)
	if !ok {
		return false, nil
	}
	kind, ref, found := strings.Cut(subject, ":")
	if !found || kind != PrincipalKindUser || ref == "" {
		return false, nil
	}
	h := &subjectReadHost{auth: auth, ref: ref}
	var loadErr error
	readable := canReadWith(ctx, h, a, func() ([]Grant, error) {
		gs, err := b.store.ListGrants(ctx, a.ID)
		loadErr = err
		return withoutPrincipal(gs, subject), err
	})
	if err := errors.Join(loadErr, h.err); err != nil {
		return false, err
	}
	return readable, nil
}

// withoutPrincipal returns gs without the principal grant naming ref.
func withoutPrincipal(gs []Grant, ref string) []Grant {
	out := make([]Grant, 0, len(gs))
	for _, g := range gs {
		if g.SubjectKind == SubjectPrincipal && g.SubjectRef == ref {
			continue
		}
		out = append(out, g)
	}
	return out
}

// subjectReadHost is the Host canReadWith asks for reviewSubjectCanRead:
// its caller is the user ref. A user has no credential in this question,
// so Permits allows every artifact permission in a named scope, as for a
// session user; Authorize asks ReviewSubjectAuthority. It is used for that
// one read check and never serves a request.
type subjectReadHost struct {
	auth ReviewSubjectAuthority
	ref  string
	// err is the first lookup failure, which makes the answer an error.
	err error
}

func (h *subjectReadHost) Principal(context.Context) (kind, ref, homeScope string, ok bool) {
	return PrincipalKindUser, h.ref, "", true
}

func (h *subjectReadHost) Permits(_ context.Context, scopeRef, _ string) bool {
	return scopeRef != ""
}

func (h *subjectReadHost) Authorize(ctx context.Context, scopeRef, permission string) bool {
	if scopeRef == "" || h.err != nil {
		return false
	}
	ok, err := h.auth.ReviewSubjectAuthorize(ctx, PrincipalKindUser, h.ref, scopeRef, permission)
	if err != nil {
		h.err = err
		return false
	}
	return ok
}

// errSubjectHostUnused is returned by the subjectReadHost methods the read
// check never calls.
var errSubjectHostUnused = errors.New("artifacts: not available for a review subject")

func (h *subjectReadHost) MemberScopes(context.Context) ([]string, error) {
	return nil, errSubjectHostUnused
}

func (h *subjectReadHost) SealCursor(context.Context, string, string) (string, error) {
	return "", errSubjectHostUnused
}

func (h *subjectReadHost) OpenCursor(context.Context, string, string) (string, error) {
	return "", errSubjectHostUnused
}
