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
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// Automatic review access for the delegating user (ptone/scion#7134).
//
// When an agent creates a new artifact in a project, the service adds a
// review grant for the delegating user recorded on it (Artifact.SponsorRef,
// ptone/scion#6028), when the host's SponsorReviewPolicy allows it. The
// grant is one more entry in the list the creation passes to the store, so
// it commits in the creation transaction with its grant_created event
// (actor and authority sponsor_review): it is written exactly once per
// artifact and never again. An append under an existing key, a later
// version, a move, an artifact created by a user, an attachment, and the
// sponsor backfill (BackfillSponsors) never add it, so a removed grant
// stays removed.
//
// A grant in its automatic form (CreatedByRef == SponsorReviewCreator)
// confers access only while its subject holds a role that reads in the
// artifact's home project (grantAllows). The first level change made by a
// person rewrites CreatedByRef to that person (putGrant), and the grant is
// then an ordinary grant.

// SponsorReviewCreator is the CreatedByRef of an automatic review grant
// while it is in its automatic form. Only the creation path writes it:
// putGrant refuses a grant that carries it, and the grant request body has
// no field for it.
const SponsorReviewCreator = ActorKindSystem + ":" + AuthoritySponsorReview

// SponsorReviewPolicy is an optional extension of Host (ptone/scion#7134).
// The service asks it, when an agent with a recorded delegating user
// creates an artifact in a project scope, whether that user gets a review
// grant on it. A host without it adds no grant.
type SponsorReviewPolicy interface {
	// SponsorReviewGrant answers nil when a new artifact homed in the
	// project scopeRef gets a review grant for the user sponsorUserID: the
	// feature is on, and the user is active and holds a role that reads in
	// scopeRef now, the artifact's own home, whatever project the agent
	// belongs to. Any other answer means no grant: ErrSponsorReviewOff when
	// the feature is off, a *SponsorReviewRoleError when the user holds no
	// such role there or is not an active user, and any other error for a
	// failed lookup (fail closed).
	SponsorReviewGrant(ctx context.Context, scopeRef, sponsorUserID string) error
}

// ErrSponsorReviewOff is SponsorReviewPolicy's answer when automatic review
// grants are turned off.
var ErrSponsorReviewOff = errors.New("artifacts: automatic review grants are turned off")

// SponsorReviewRoleError is SponsorReviewPolicy's answer when the delegating
// user holds no role with Permission in the project Scope, the artifact's
// home. Its message names the scope and the permission only.
type SponsorReviewRoleError struct {
	Scope      string
	Permission string
}

func (e *SponsorReviewRoleError) Error() string {
	return fmt.Sprintf("artifacts: no active role with %s in project %s", e.Permission, e.Scope)
}

// SponsorReviewSkips reports how many automatic review grants the service
// skipped because the policy refused them or could not decide, since it
// started. Grants skipped because the feature is off are not counted.
func (s *Service) SponsorReviewSkips() int64 {
	return s.sponsorReviewSkips.Load()
}

// sponsorReviewGrant returns the automatic review grant for the new artifact
// a, created by the caller of r, and true when it gets one: a is owned by
// an agent, homed in a project scope, records a delegating user, and the
// host's SponsorReviewPolicy answers nil for a's home scope and that user.
// The subject is always a.SponsorRef, never anything from the request. A
// refusal other than "off" is counted and logged with a fixed reason code:
// the general log line carries the reason and the artifact id; the
// decision log entry carries the reason with the usual request and artifact
// fields (route, the calling agent, the home project, owner_kind). Neither
// ever carries the delegating user's id or the policy's error text.
func (s *Service) sponsorReviewGrant(r *http.Request, a *Artifact, now time.Time) (Grant, bool) {
	if a.OwnerKind != PrincipalKindAgent || a.ScopeKind != ScopeKindProject || a.ScopeRef == "" || a.SponsorRef == "" {
		return Grant{}, false
	}
	policy, ok := s.host.(SponsorReviewPolicy)
	if !ok {
		return Grant{}, false
	}
	ctx := r.Context()
	err := policy.SponsorReviewGrant(ctx, a.ScopeRef, a.SponsorRef)
	if err != nil {
		if errors.Is(err, ErrSponsorReviewOff) {
			return Grant{}, false
		}
		s.sponsorReviewSkips.Add(1)
		// Logged with a fixed reason code, never the error text, which may
		// name the user or carry a store error.
		reason := logReasonCheckerError
		var roleErr *SponsorReviewRoleError
		if errors.As(err, &roleErr) {
			reason = logReasonSponsorNoRole
		}
		slog.InfoContext(ctx, "artifacts: automatic review grant skipped", "artifact_id", a.ID, "reason", reason)
		s.logDecision(r, slog.LevelInfo, logEntry{action: logActionGrantCreated, outcome: logOutcomeRefused,
			reason: reason, artifact: a, authority: AuthoritySponsorReview})
		return Grant{}, false
	}
	return Grant{
		ID: uuid.NewString(), ArtifactID: a.ID, SubjectKind: SubjectPrincipal,
		SubjectRef: PrincipalRef(PrincipalKindUser, a.SponsorRef), Permission: GrantReview,
		CreatedByRef: SponsorReviewCreator, CreatedAt: now,
	}, true
}

// logSponsorReviewGrants writes the decision log entry of each automatic
// review grant in grants, which the creation of a just committed.
func (s *Service) logSponsorReviewGrants(r *http.Request, a *Artifact, grants []Grant) {
	for i := range grants {
		g := &grants[i]
		if g.CreatedByRef != SponsorReviewCreator {
			continue
		}
		s.logAllowed(r, logEntry{action: logActionGrantCreated, artifact: a, authority: AuthoritySponsorReview,
			subjectKind: g.SubjectKind, subjectRef: g.SubjectRef, permission: g.Permission})
	}
}

// validSponsorReviewGrant reports whether g, a grant carrying
// SponsorReviewCreator, has the one shape an automatic review grant may
// have: a review grant to a user principal.
func validSponsorReviewGrant(g *Grant) bool {
	return g.SubjectKind == SubjectPrincipal && g.Permission == GrantReview && g.ExpiresAt == nil &&
		reviewSubjectOK(g.Permission, g.SubjectKind, g.SubjectRef)
}
