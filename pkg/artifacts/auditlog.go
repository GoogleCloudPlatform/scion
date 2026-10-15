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
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// The artifact decision log (ptone/scion#5120). The service writes one
// structured entry per decision to the logger set with SetLogger: each
// lifecycle change it records in artifact_event (and the grant, link and
// version changes it does not), each authorization refusal on a
// management or write route (WARN; a refused POST /{id}/grants from a
// caller who may read the artifact but not administer it, which
// putReviewGrant answers, is not logged here), and each read answered with
// the 404 of a hidden or missing artifact (DEBUG only, to bound volume).
//
// An entry carries ids, enums and counts only. It never carries a share
// link's token, its hash or its id (no segment under a link route), a
// title, a note, file content or names, or any credential, and its route
// is a template (routeTemplate), never the request path. The host's logger
// adds the request id of the entry's context, so an entry joins to the
// request log line of its request.

// Log entry field keys. pkg/util/logging promotes the cheap ones to labels
// under the same names; a test pins that the two agree.
const (
	logKeyArtifactID      = "artifact_id"
	logKeyVersionSeq      = "version_seq"
	logKeyProjectID       = "project_id"
	logKeyRoute           = "route"
	logKeyAction          = "action"
	logKeyOutcome         = "outcome"
	logKeyReason          = "reason"
	logKeyPrincipalKind   = "principal_kind"
	logKeyPrincipalID     = "principal_id"
	logKeyAuthority       = "authority"
	logKeySubjectKind     = "subject_kind"
	logKeySubjectRef      = "subject_ref"
	logKeyPermission      = "permission"
	logKeyOwnerKind       = "owner_kind"
	logKeyOrphaned        = "orphaned"
	logKeyCount           = "count"
	logKeyOwnersChecked   = "owners_checked"
	logKeyOrphanedCount   = "orphaned_count"
	logKeyUnorphanedCount = "unorphaned_count"
)

// Log outcomes.
const (
	logOutcomeAllowed = "allowed"
	logOutcomeRefused = "refused"
	logOutcomeError   = "error"
)

// Log actions. A lifecycle entry names the change; a refusal names what
// the caller attempted, and its route names the request.
const (
	logActionPublished        = "published"         // single-file publish: artifact and ready version 1
	logActionCreated          = "created"           // artifact with a pending first version
	logActionVersionCreated   = "version_created"   // pending version appended
	logActionVersionFinalized = "version_finalized" // version made ready
	logActionVersionFailed    = "version_failed"    // version could not be recorded or finalized, or was discarded
	logActionGrantCreated     = "grant_created"
	logActionGrantChanged     = "grant_changed"
	logActionGrantRemoved     = EventGrantRemoved
	logActionLinkCreated      = "link_created"
	logActionLinkRevoked      = EventLinkRevoked
	logActionMoved            = EventMoved
	logActionExpirySet        = EventExpirySet
	logActionExpiryCleared    = EventExpiryCleared
	logActionDeleted          = EventDeleted
	logActionOrphaned         = EventOrphaned
	logActionUnorphaned       = EventUnorphaned
	logActionOwnerPass        = "owner_pass"     // per-run summary of the owner liveness pass
	logActionRetentionPass    = "retention_pass" // per-run summary of the expiry sweep

	logActionRead       = "read"
	logActionAdminister = "administer"
	logActionWrite      = "write"
	logActionCreate     = "create"
	logActionDelete     = "delete"
	logActionMove       = "move"
	logActionShare      = "share"
)

// Log reasons: why a refusal or a failure happened. Never free text.
const (
	logReasonNotFound              = "not_found" // no live artifact with the id
	logReasonHidden                = "hidden"    // the caller cannot read it (answered as not found)
	logReasonCredentialNotPermit   = "credential_not_permitted"
	logReasonAgentCannotAdminister = "agent_cannot_administer"
	logReasonNotAdministrator      = "not_owner_or_admin_grant"
	logReasonNotCustodian          = "not_owner_admin_grant_or_project_admin"
	logReasonNoWriteAccess         = "no_write_access"
	logReasonExpired               = "expired"
	logReasonReviewOnly            = "review_only"
	logReasonNotVersionPublisher   = "not_version_publisher"
	logReasonNotAuthorizedInScope  = "not_authorized_in_scope"
	logReasonCrossProjectDisabled  = "cross_project_sharing_disabled"
	logReasonUnauthenticated       = "unauthenticated"
	logReasonStoreError            = "store_error"
	logReasonCheckerError          = "principal_check_failed"
)

// Log authorities: why the actor was allowed. The values follow the planned
// share-authority vocabulary. owner, admin_grant and custodian_project_admin
// are also the artifact_event authorities of the same name.
// steward_project_owner is a review grant given as a holder of project
// administration in the home project (its owners and admins, and
// super-admins); steward_delegator is a review grant given as the owning
// agent's delegating user (the clause a ReviewGrantClauser names). A review
// grant on a host that does not name the clause is logged without an
// authority. The maintenance pass logs the event authorities retention and
// reconciler.
const (
	logAuthorityOwner                 = AuthorityOwner
	logAuthorityAdminGrant            = AuthorityAdminGrant
	logAuthorityCustodianProjectAdmin = AuthorityCustodianProjectAdmin
	logAuthorityStewardProjectOwner   = "steward_project_owner"
	logAuthorityStewardDelegator      = "steward_delegator"
)

// SetLogger sets the function that yields the artifact decision logger.
// It is asked on every entry, so it may be set before the host's logger
// exists; nil, or a function that answers nil, discards the entries.
func (s *Service) SetLogger(fn func() *slog.Logger) {
	s.mu.Lock()
	s.logFn = fn
	s.mu.Unlock()
}

// decisionLogger returns the logger entries go to.
func (s *Service) decisionLogger() *slog.Logger {
	s.mu.RLock()
	fn := s.logFn
	s.mu.RUnlock()
	return loggerOrDiscard(fn)
}

func loggerOrDiscard(fn func() *slog.Logger) *slog.Logger {
	if fn != nil {
		if l := fn(); l != nil {
			return l
		}
	}
	return discardLogger
}

var discardLogger = slog.New(slog.DiscardHandler)

// logEntry is one decision. Only the fields that apply are set.
type logEntry struct {
	action    string
	outcome   string
	reason    string
	artifact  *Artifact
	seq       int
	authority string
	// subjectKind and subjectRef name a grant's subject. A share link is
	// logged with its kind only: its ref is the token's hash.
	subjectKind string
	subjectRef  string
	permission  string
	count       int
}

// logDecision writes e for the request r, as its caller.
func (s *Service) logDecision(r *http.Request, level slog.Level, e logEntry) {
	ctx := r.Context()
	l := s.decisionLogger()
	if !l.Enabled(ctx, level) {
		return
	}
	kind, ref, _, _ := s.host.Principal(ctx)
	writeDecision(ctx, l, level, r.Method+" "+routeTemplate(r), kind, ref, e)
}

// logRefusal writes a refusal of the request r at WARN.
func (s *Service) logRefusal(r *http.Request, action, reason string, a *Artifact) {
	s.logDecision(r, slog.LevelWarn, logEntry{action: action, outcome: logOutcomeRefused, reason: reason, artifact: a})
}

// logHidden writes, at DEBUG, a request answered with the 404 of a
// missing or hidden artifact.
func (s *Service) logHidden(r *http.Request, a *Artifact, reason string) {
	s.logDecision(r, slog.LevelDebug, logEntry{action: logActionRead, outcome: logOutcomeRefused, reason: reason, artifact: a})
}

// logAllowed writes a lifecycle change made by the caller of r at INFO.
func (s *Service) logAllowed(r *http.Request, e logEntry) {
	e.outcome = logOutcomeAllowed
	s.logDecision(r, slog.LevelInfo, e)
}

// writeDecision writes one entry.
func writeDecision(ctx context.Context, l *slog.Logger, level slog.Level, route, principalKind, principalID string, e logEntry) {
	attrs := make([]slog.Attr, 0, 16)
	str := func(k, v string) {
		if v != "" {
			attrs = append(attrs, slog.String(k, v))
		}
	}
	str(logKeyAction, e.action)
	str(logKeyOutcome, e.outcome)
	str(logKeyReason, e.reason)
	str(logKeyRoute, route)
	str(logKeyPrincipalKind, principalKind)
	str(logKeyPrincipalID, principalID)
	if a := e.artifact; a != nil {
		str(logKeyArtifactID, a.ID)
		str(logKeyProjectID, a.ScopeRef)
		str(logKeyOwnerKind, a.OwnerKind)
		if a.OwnerKind != "" {
			attrs = append(attrs, slog.Bool(logKeyOrphaned, a.OrphanedAt != nil))
		}
	}
	if e.seq > 0 {
		attrs = append(attrs, slog.Int(logKeyVersionSeq, e.seq))
	}
	str(logKeyAuthority, e.authority)
	str(logKeySubjectKind, e.subjectKind)
	if e.subjectKind != SubjectLink {
		str(logKeySubjectRef, e.subjectRef)
	}
	str(logKeyPermission, e.permission)
	if e.count > 0 {
		attrs = append(attrs, slog.Int(logKeyCount, e.count))
	}
	l.LogAttrs(ctx, level, "artifacts: "+e.action, attrs...)
}

// writeSystemDecision writes an entry for a change the hub's own
// maintenance pass made, as the system actor actor.
func writeSystemDecision(ctx context.Context, l *slog.Logger, actor Actor, e logEntry) {
	if e.outcome == "" {
		e.outcome = logOutcomeAllowed
	}
	if e.authority == "" {
		e.authority = actor.Authority
	}
	writeDecision(ctx, l, slog.LevelInfo, "", actor.Kind, actor.Ref, e)
}

// routeTemplate is the route r was served by, with every variable segment
// replaced by its name (for example /api/v1/artifacts/{id}/grants). It
// never returns a segment of the request path, so a share link's token or
// id, a file name or an id cannot reach the log through it.
func routeTemplate(r *http.Request) string {
	rest, ok := strings.CutPrefix(r.URL.EscapedPath(), RouteCollection)
	if !ok {
		return "unknown"
	}
	rest = strings.Trim(rest, "/")
	if rest == "" {
		return RouteCollection
	}
	segs := strings.Split(rest, "/")
	switch segs[0] {
	case "shared":
		return RouteCollection + "/shared/{token}"
	case "view":
		return RouteCollection + "/view/{capability}"
	}
	base := RouteCollection + "/{id}"
	sub := segs[1:]
	switch {
	case len(sub) == 0:
		return base
	case sub[0] == "files":
		return base + "/files/{path}"
	case sub[0] == "grants" && len(sub) == 1:
		return base + "/grants"
	case sub[0] == "grants" && len(sub) == 2:
		return base + "/grants/{grantId}"
	case sub[0] == "links" && len(sub) == 1:
		return base + "/links"
	case sub[0] == "links" && len(sub) == 2:
		return base + "/links/{linkId}"
	case sub[0] == "versions" && len(sub) == 1:
		return base + "/versions"
	case sub[0] == "versions" && len(sub) == 2:
		return base + "/versions/{seq}"
	case sub[0] == "versions" && len(sub) == 3 && (sub[2] == "finalize" || sub[2] == "view"):
		return base + "/versions/{seq}/" + sub[2]
	case sub[0] == "versions" && len(sub) >= 4 && sub[2] == "files":
		return base + "/versions/{seq}/files/{path}"
	}
	return base + "/unknown"
}

// logAdminRefusal writes the refusal of a caller that may read a but not
// administer it (adminArtifact), with the reason adminAuthorityWith
// answered "". The reason is worked out only when the entry is written.
func (s *Service) logAdminRefusal(r *http.Request, action string, a *Artifact) {
	if !s.decisionLogger().Enabled(r.Context(), slog.LevelWarn) {
		return
	}
	s.logRefusal(r, action, s.adminRefusalReason(r.Context(), a), a)
}

// adminRefusalReason is why adminAuthorityWith refuses the caller of ctx
// on a, which it asked in the same order.
func (s *Service) adminRefusalReason(ctx context.Context, a *Artifact) string {
	kind, _, _, ok := s.host.Principal(ctx)
	switch {
	case !ok:
		return logReasonUnauthenticated
	case kind != PrincipalKindUser:
		return logReasonAgentCannotAdminister
	case !s.host.Permits(ctx, a.ScopeRef, PermissionManage):
		return logReasonCredentialNotPermit
	}
	return logReasonNotAdministrator
}

// logGrantPut writes a principal or scope grant created (created) or
// changed by the caller of r, which acted by authority.
func (s *Service) logGrantPut(r *http.Request, a *Artifact, g *Grant, created bool, authority string) {
	action := logActionGrantChanged
	if created {
		action = logActionGrantCreated
	}
	s.logAllowed(r, logEntry{action: action, artifact: a, authority: authority,
		subjectKind: g.SubjectKind, subjectRef: g.SubjectRef, permission: g.Permission})
}

// grantForLog returns grant grantID of a, read before it is removed so its
// removal can be logged with its subject, or nil when no entry would be
// written or the grant cannot be read.
func (s *Service) grantForLog(r *http.Request, b backend, a *Artifact, grantID string) *Grant {
	if !s.decisionLogger().Enabled(r.Context(), slog.LevelInfo) {
		return nil
	}
	grants, err := b.store.ListGrants(r.Context(), a.ID)
	if err != nil {
		return nil
	}
	for i := range grants {
		if grants[i].ID == grantID {
			return &grants[i]
		}
	}
	return nil
}

// logGrantRemoved writes the removal of grant g (nil when it could not be
// read beforehand) by actor.
func (s *Service) logGrantRemoved(r *http.Request, a *Artifact, g *Grant, actor *Actor) {
	e := logEntry{action: logActionGrantRemoved, artifact: a, authority: actor.Authority}
	if g != nil {
		e.subjectKind, e.subjectRef, e.permission = g.SubjectKind, g.SubjectRef, g.Permission
	}
	s.logAllowed(r, e)
}

// logPatch writes the changes update u made to before, which is now
// after: a move (logged in the old home project, with the new home as its
// scope subject) and an expiry set or cleared.
func (s *Service) logPatch(r *http.Request, before, after *Artifact, u ArtifactUpdate) {
	authority := u.Actor.Authority
	if u.HomeGrant != nil {
		s.logAllowed(r, logEntry{action: logActionMoved, artifact: before, authority: authority,
			subjectKind: SubjectScope, subjectRef: u.HomeGrant.SubjectRef, permission: u.HomeGrant.Permission})
	}
	if u.SetExpiry {
		action := logActionExpirySet
		if u.ExpiresAt == nil {
			action = logActionExpiryCleared
		}
		s.logAllowed(r, logEntry{action: action, artifact: after, authority: authority})
	}
}

// logLink writes a share link created or revoked by actor. The link is
// logged by kind only: never its token, the token's hash or its id.
func (s *Service) logLink(r *http.Request, a *Artifact, action string, actor *Actor) {
	s.logAllowed(r, logEntry{action: action, artifact: a, authority: actor.Authority,
		subjectKind: SubjectLink, permission: GrantRead})
}

// logRetireRefusal writes the refusal of a delete by a caller that may
// read a, with the reason retireAuthority answered "".
func (s *Service) logRetireRefusal(r *http.Request, a *Artifact) {
	if !s.decisionLogger().Enabled(r.Context(), slog.LevelWarn) {
		return
	}
	reason := s.adminRefusalReason(r.Context(), a)
	if reason == logReasonNotAdministrator {
		reason = logReasonNotCustodian
	}
	s.logRefusal(r, logActionDelete, reason, a)
}

// logCreateRefusal writes the refusal of a publish into scope
// (canCreateIn false): the credential does not permit artifact.create
// there, or the host does not authorize the caller there.
func (s *Service) logCreateRefusal(r *http.Request, scope string) {
	if !s.decisionLogger().Enabled(r.Context(), slog.LevelWarn) {
		return
	}
	reason := logReasonNotAuthorizedInScope
	if !s.host.Permits(r.Context(), scope, PermissionCreate) {
		reason = logReasonCredentialNotPermit
	}
	// The scope is caller text here (refused before any store lookup):
	// log it only when it has a project id's shape.
	logged := &Artifact{}
	if projectIDShaped(scope) {
		logged.ScopeRef = scope
	}
	s.logRefusal(r, logActionCreate, reason, logged)
}

// maxLoggedScopeBytes bounds a caller-supplied scope written to the log.
const maxLoggedScopeBytes = 64

// projectIDShaped reports whether s looks like a project id: 1 to
// maxLoggedScopeBytes ASCII letters, digits, '-' or '_' (a UUID passes).
func projectIDShaped(s string) bool {
	if s == "" || len(s) > maxLoggedScopeBytes {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// logWriteRefusal writes the refusal of a version write on a by a caller
// that may read it (canWritePermittedErr answered accessNone), with the
// reason it did, asked in the same order.
func (s *Service) logWriteRefusal(r *http.Request, a *Artifact) {
	ctx := r.Context()
	if !s.decisionLogger().Enabled(ctx, slog.LevelWarn) {
		return
	}
	_, _, _, ok := s.host.Principal(ctx)
	var reason string
	switch {
	case !ok:
		reason = logReasonUnauthenticated
	case a.ExpiresAt != nil && !time.Now().Before(*a.ExpiresAt):
		reason = logReasonExpired
	case !s.host.Permits(ctx, a.ScopeRef, PermissionCreate):
		reason = logReasonCredentialNotPermit
	default:
		reason = logReasonNoWriteAccess
	}
	s.logRefusal(r, logActionWrite, reason, a)
}

// logVersion writes a version of a created or finalized by the caller of
// r, which writes it with access: as its owner (authority owner) or
// through a write or review grant (logged as the grant's permission).
func (s *Service) logVersion(r *http.Request, action string, a *Artifact, seq int, access writeAccess) {
	e := logEntry{action: action, artifact: a, seq: seq}
	switch access {
	case accessOwner:
		e.authority = logAuthorityOwner
	case accessWriteGrant:
		e.permission = GrantWrite
	case accessReviewGrant:
		e.permission = GrantReview
	}
	s.logAllowed(r, e)
}

// logVersionError writes a version of a that failed: it could not be
// recorded or finalized (outcome error), or it was discarded (outcome
// refused, reason a FailReason constant).
func (s *Service) logVersionError(r *http.Request, a *Artifact, seq int, reason string) {
	outcome := logOutcomeRefused
	if reason == logReasonStoreError {
		outcome = logOutcomeError
	}
	s.logDecision(r, slog.LevelInfo, logEntry{action: logActionVersionFailed, outcome: outcome, reason: reason, artifact: a, seq: seq})
}

// reviewClauseNote is the context key under which putReviewGrant passes a
// *string for reviewGrantAuthorized to record the review clause in, from
// the host's single evaluation.
type reviewClauseNote struct{}

// reviewClauseUnnamed is the clause recorded for a host that allows a
// review grant through ReviewGrantAuthority alone, which does not name it.
const reviewClauseUnnamed = "unnamed"

// reviewGrantLogAuthority is the log authority of a review grant allowed by
// clause: steward_project_owner or steward_delegator as the host named it,
// and none ("", the field is left out) when the host did not name it.
func reviewGrantLogAuthority(clause string) string {
	switch clause {
	case ReviewClauseProjectAdmin:
		return logAuthorityStewardProjectOwner
	case ReviewClauseDelegatingUser:
		return logAuthorityStewardDelegator
	}
	return ""
}
