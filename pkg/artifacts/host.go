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
	"strings"
)

// Principal kinds returned by Host.Principal.
const (
	PrincipalKindUser  = "user"
	PrincipalKindAgent = "agent"
)

// Scope kinds. A project scope's ref is the project's stable id. The two
// attachment scopes (ptone/scion#4595) hold web-chat attachments: an upload
// scope's ref is the uploader as PrincipalRef(kind, id), and a message
// scope's ref is the id of the message the attachment was sent in.
const (
	ScopeKindProject = "project"
	// ScopeKindUpload holds a file uploaded for a message that has not been
	// sent yet. Only the uploader reads it.
	ScopeKindUpload = "upload"
	// ScopeKindMessage holds a file sent in a message. It is readable
	// exactly by the readers of that message (MessageScopeHost).
	ScopeKindMessage = "message"
)

// IsAttachmentScope reports whether kind is one of the attachment scopes.
// Rows in them are never listed, never shared and never versioned, and the
// artifact routes answer for them as for an id that does not exist.
func IsAttachmentScope(kind string) bool {
	return kind == ScopeKindUpload || kind == ScopeKindMessage
}

// Permission IDs passed to Host.Authorize. They match the hub permission
// registry rows of the same name.
const (
	PermissionRead   = "artifact.read"
	PermissionCreate = "artifact.create"
	PermissionUpdate = "artifact.update"
	PermissionDelete = "artifact.delete"
	PermissionManage = "artifact.manage"
	// PermissionSteward is held by a home project's owners (and
	// super-admins): it makes them stewards of the artifacts agents own in
	// that project (design D25). It is never agent-callable.
	PermissionSteward = "artifact.steward"
)

// Host is the service's only view of the system it runs in. It answers
// three questions: who is calling, whether that caller may act in a scope,
// and whether the caller's credential allows an action at all.
//
// Every input and output is a string or a bool, so the interface can later be
// served over a wire protocol by a standalone deployment. Do not add hub
// types, identity objects or callbacks to it.
type Host interface {
	// Principal identifies the caller of the request whose context is ctx.
	// kind is one of the PrincipalKind constants. ref is the principal's
	// stable id (a user id or an agent id), never a display name, email or
	// slug, because it is stored as the artifact owner. homeScope is the
	// scope ref an artifact the caller publishes is homed in by default, or
	// "" when the caller has none (a user). ok is false when the request is
	// unauthenticated or the caller is of a kind the service does not serve.
	Principal(ctx context.Context) (kind, ref, homeScope string, ok bool)

	// Authorize reports whether the caller may exercise permission (one of
	// the Permission constants) on artifacts in the scope scopeRef. It fails
	// closed: an unknown permission, an empty scope or an unauthenticated
	// caller yields false. Authorize true implies Permits true.
	Authorize(ctx context.Context, scopeRef, permission string) bool

	// Permits reports whether the caller's credential itself allows
	// permission on artifacts homed in scopeRef: the restrictions a token
	// carries (its scopes, ceiling and boundary), checked before ownership,
	// grants or role bindings. The service asks it first on every path, so
	// neither ownership nor a grant reaches past what the credential allows.
	// It fails closed like Authorize.
	Permits(ctx context.Context, scopeRef, permission string) bool

	// MemberScopes returns the scope refs the caller belongs to (for a
	// user, the projects it holds a role in directly or through a group;
	// for an agent, its own project). The list endpoint uses it only to
	// bound which scope grants make an artifact a candidate: every
	// candidate is still checked like a GET (Permits, then owner or
	// Authorize, then grants), so this decides completeness, never
	// visibility.
	MemberScopes(ctx context.Context) ([]string, error)

	// SealCursor turns a list resume position into an opaque,
	// authenticated cursor bound to the caller and to binding (the
	// normalized query it was issued for). OpenCursor reverses it, and
	// fails for a cursor that was tampered with or issued to another
	// caller or query. A cursor may carry the position of a row the
	// caller cannot read, so it must reveal nothing about it.
	SealCursor(ctx context.Context, position, binding string) (string, error)
	OpenCursor(ctx context.Context, cursor, binding string) (string, error)
}

// ScopeChecker is an optional extension of Host. The artifact list asks it
// which home projects still exist, so it can mark the rows of artifacts
// whose project was deleted. A host without it reports none deleted. The
// maintenance pass asks it which messages still exist, so it can retire
// the attachments of messages that are gone (ptone/scion#4595).
type ScopeChecker interface {
	// ScopesExist reports, for each ref, whether its scope exists. A ref
	// is a project id, or MessageScopeRef(id) for a message, which exists
	// while it is stored and not deleted. A ref missing from the answer
	// counts as existing.
	ScopesExist(ctx context.Context, refs []string) (map[string]bool, error)
}

// messageRefPrefix marks a message ref in a ScopesExist question. Project
// ids are UUIDs, so the two kinds of ref never collide.
const messageRefPrefix = ScopeKindMessage + ":"

// MessageScopeRef is the ScopesExist ref of message messageID.
func MessageScopeRef(messageID string) string { return messageRefPrefix + messageID }

// ParseMessageScopeRef returns the message id of a ScopesExist ref made by
// MessageScopeRef, and false for any other ref.
func ParseMessageScopeRef(ref string) (string, bool) {
	id, ok := strings.CutPrefix(ref, messageRefPrefix)
	return id, ok && id != ""
}

// MessageScopeHost is an optional extension of Host (ptone/scion#4595).
// The read rule of the attachment scopes asks it about the caller of the
// request; a host without it makes every upload and message scoped row
// unreadable (fail closed).
//
// Like every Host method, it answers only for the identity of the request
// whose context is ctx.
type MessageScopeHost interface {
	// PermitsMessageRead reports whether the caller's credential allows
	// reading messages at all. It is asked first, so nothing reaches past
	// what the credential allows.
	PermitsMessageRead(ctx context.Context) bool

	// MayReadMessage reports whether the caller may read the message
	// messageID now: a reader of its conversation, or one of its
	// addressees. A deleted or unknown message, and any lookup failure,
	// answer false. messageProjectID is the project the attachment rows of
	// the message record (Artifact.MessageProjectID): nil when unknown,
	// "" for a message without a project. A host may require it for some
	// credentials (ptone/scion#4596); nil and "" are distinct.
	MayReadMessage(ctx context.Context, messageID string, messageProjectID *string) bool

	// PermitsUploadRead reports whether the caller's credential allows the
	// uploader to read its own unsent upload (an upload-scoped row). It is
	// asked after the uploader matches (ptone/scion#4596, section 9, clause
	// 0): a project-bounded user access token does not read its holder's
	// unsent upload, which belongs to no project.
	PermitsUploadRead(ctx context.Context) bool
}

// ScopeExplainer is an optional extension of Host. When a credential
// lacks a scope that publishing needs, the service asks it which one, so
// the caller gets a 403 naming the scope instead of an answer that looks
// like an expired credential.
//
// The service consults it only on the publish path, which names no
// existing artifact. Reads keep their uniform 404 and never call it.
type ScopeExplainer interface {
	// MissingScope reports the name of the credential scope the caller of
	// ctx lacks for permission (one of the Permission constants), or ""
	// when the caller is unauthenticated, holds every scope the permission
	// needs, or is refused for another reason. It is a pure description:
	// a non-empty answer never grants anything.
	MissingScope(ctx context.Context, permission string) string
}

// ReviewGrantAuthority is an optional extension of Host (design D24,
// ptone/scion#4014). For an artifact owned by an agent, the agent cannot
// administer it (sharing is user-only), so without this extension no one
// but an admin grantee can give a human review access. With it, the
// service lets two more users give a user a review grant, and nothing else:
// the owning agent's delegating user and an admin of the artifact's home
// project. A host without it allows neither (fail closed).
type ReviewGrantAuthority interface {
	// MayGrantReview reports whether the caller of ctx, a user, may give a
	// user a review grant on an artifact owned by agent ownerAgentID and
	// homed in project homeScope: the caller holds project administration
	// in homeScope, or is the agent's delegating user. sponsorRef is the
	// delegating user recorded on the artifact (Artifact.SponsorRef); when
	// it is set it decides who that is, and the agent's live delegation
	// chain is consulted only when it is "" (an artifact with nothing
	// recorded). Any lookup failure answers false.
	MayGrantReview(ctx context.Context, ownerAgentID, homeScope, sponsorRef string) bool
}

// Review grant clauses, as ReviewGrantClauser names them.
const (
	ReviewClauseProjectAdmin   = "project_admin"
	ReviewClauseDelegatingUser = "delegating_user"
)

// ReviewGrantClauser is an optional host interface that replaces
// ReviewGrantAuthority when a host has it (ptone/scion#5120): the service
// asks it instead of MayGrantReview. It makes the same decision as
// MayGrantReview, in the same single evaluation, and names the clause that
// allowed it, so the decision log can record it without asking the host a
// second question.
type ReviewGrantClauser interface {
	// ReviewGrantClause answers like MayGrantReview: ReviewClauseProjectAdmin
	// when the caller holds project administration in homeScope,
	// ReviewClauseDelegatingUser when it is the delegating user of agent
	// ownerAgentID (the recorded sponsorRef when set, else the live chain
	// root), or "" when MayGrantReview would answer false.
	ReviewGrantClause(ctx context.Context, ownerAgentID, homeScope, sponsorRef string) string
}

// PrincipalChecker is an optional extension of Host (ptone/scion#4529).
// The owner liveness reconciler asks it which artifact owners still exist,
// so it can mark the artifacts of a deleted agent or user. A host without
// it reports every owner live, and nothing is ever marked.
//
// It is asked outside any request, so it must not depend on a caller.
type PrincipalChecker interface {
	// PrincipalsExist reports, for each "kind:id" in refs (PrincipalRef),
	// whether the principal is live: an agent row that exists and is not
	// soft-deleted, or an active user. A ref missing from the answer
	// counts as live. An error means nothing about any ref.
	PrincipalsExist(ctx context.Context, refs []string) (map[string]bool, error)
}

// ScopeAdministrator is an optional extension of Host (ptone/scion#4529).
// The delete route asks it whether the caller administers an artifact's
// home project, the first custodian clause: the project owners and admins
// of the artifact's home project, and super-admins, may delete artifacts
// homed there. A host without it allows
// no one by this clause (fail closed).
type ScopeAdministrator interface {
	// AdministersScope reports whether the caller of ctx, a user, holds
	// project administration (project.manage) in the project scopeRef,
	// with the caller's credential limits applied. Any lookup failure
	// answers false.
	AdministersScope(ctx context.Context, scopeRef string) bool
}

// Steward clauses, as Stewardship names them (design D25).
const (
	// StewardClauseProjectOwner: the caller holds artifact.steward in the
	// artifact's home project (its owners, and super-admins).
	StewardClauseProjectOwner = "project_owner"
	// StewardClauseDelegator: the caller is the owning agent's delegating
	// user (the user recorded on the artifact at creation, or for an
	// artifact with nothing recorded the root of the agent's live
	// delegation chain) and reads in the home project through a role.
	StewardClauseDelegator = "delegator"
)

// Stewardship is an optional extension of Host (design D25,
// ptone/scion#5144). For an artifact owned by an agent, the agent cannot
// share it (sharing is user-only), so the humans accountable for it are
// its stewards: they may share it within bounds (no admin grant, no move),
// change its expiry and delete it. The service asks it only for an
// artifact owned by an agent, after the owner and admin-grant checks, for
// a user whose credential permits artifact.manage in the home project. A
// host without it has no stewards (fail closed).
type Stewardship interface {
	// StewardClause reports which steward clause the calling user
	// satisfies for an artifact owned by agent ownerAgentID and homed in
	// project homeScope: StewardClauseProjectOwner,
	// StewardClauseDelegator, or "" for none. sponsorRef and orphaned come
	// from the artifact row: sponsorRef is the delegating user recorded at
	// creation (Artifact.SponsorRef), which decides the delegator clause
	// when set, with the agent's live delegation chain consulted only when
	// it is ""; orphaned is not consulted yet. A grant on the artifact
	// never makes the caller a steward: the clauses are decided from
	// roles, the recorded delegating user and the delegation chain only.
	// Any lookup failure answers "".
	StewardClause(ctx context.Context, ownerAgentID, homeScope, sponsorRef string, orphaned bool) string
}
