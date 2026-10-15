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
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
)

// Event actions recorded in artifact_event (design section 3.5, plus moved,
// added in phase 1 so a move's change of access is audited too).
const (
	EventExpirySet     = "expiry_set"
	EventExpiryCleared = "expiry_cleared"
	EventDeleted       = "deleted"
	EventOrphaned      = "orphaned"
	EventUnorphaned    = "unorphaned"
	EventGrantRemoved  = "grant_removed"
	EventLinkRevoked   = "link_revoked"
	EventMoved         = "moved"
	// EventGrantCreated, EventGrantChanged and EventLinkCreated record who
	// gave access, and by what authority (ptone/scion#4882). A link_created
	// event carries the link id and expiry, never the token or its hash.
	EventGrantCreated = "grant_created"
	EventGrantChanged = "grant_changed"
	EventLinkCreated  = "link_created"
)

// Event actor kinds: the principal kinds, and system for the hub's own
// maintenance pass.
const ActorKindSystem = "system"

// Event authorities: why the actor was allowed to make the change.
const (
	AuthorityOwner                 = "owner"
	AuthorityAdminGrant            = "admin_grant"
	AuthorityCustodianProjectAdmin = "custodian_project_admin"
	AuthorityRetention             = "retention"
	AuthorityReconciler            = "reconciler"
	// AuthorityStewardProjectOwner and AuthorityStewardDelegator are the
	// steward clauses (design D25): an owner of the home project of an
	// artifact owned by an agent (artifact.steward there), and the user who
	// delegated to that agent and holds a role that reads in the home
	// project. See Stewardship.
	AuthorityStewardProjectOwner = "steward_project_owner"
	AuthorityStewardDelegator    = "steward_delegator"
)

// isSteward reports whether authority is a steward clause. A steward may
// share within bounds: no admin grant, no move (design D25).
func isSteward(authority string) bool {
	return authority == AuthorityStewardProjectOwner || authority == AuthorityStewardDelegator
}

// Actor is who makes a change the store records in artifact_event, and by
// what authority. Every store write that changes an artifact's lifetime,
// home or access requires one, so no such change can be made without its
// event.
type Actor struct {
	Kind      string
	Ref       string
	Authority string
	// Expect, when set, is the artifact state Authority was decided
	// against. The store checks it under the artifact's lock and refuses
	// the write with ErrStewardPrecondition when the artifact has moved or
	// changed owner since. The service sets it for every steward authority
	// (callerActor) and leaves it zero for the owner and admin grantees,
	// whose authority does not depend on the home or owner (design 4.5).
	Expect Expect
}

// Expect is the precondition of a store write made under a steward
// authority: the artifact's home scope, owner kind and owner ref when the
// authority was decided. The zero value checks nothing.
type Expect struct {
	ScopeRef  string
	OwnerKind string
	OwnerRef  string
}

// set reports whether e asks for a check.
func (e Expect) set() bool { return e != Expect{} }

// expectFor returns the precondition of a steward write on a.
func expectFor(a *Artifact) Expect {
	return Expect{ScopeRef: a.ScopeRef, OwnerKind: a.OwnerKind, OwnerRef: a.OwnerRef}
}

// stewardActor reports whether actor acts under a steward authority.
func stewardActor(actor *Actor) bool { return actor != nil && isSteward(actor.Authority) }

// checkExpect checks, inside tx and after the artifact's lock is taken,
// that the artifact still matches actor's Expect. It answers
// ErrStewardPrecondition on a mismatch, and nil when the actor carries no
// Expect. A steward actor without one is refused (errNoExpect): a steward
// write never commits unchecked.
func (s *sqlStore) checkExpect(ctx context.Context, tx *sql.Tx, artifactID string, actor *Actor) error {
	if actor == nil {
		return nil
	}
	if !actor.Expect.set() {
		if stewardActor(actor) {
			return errNoExpect
		}
		return nil
	}
	var scope, ownerKind, ownerRef string
	if err := tx.QueryRowContext(ctx, s.rebind(`SELECT scope_ref, owner_kind, owner_ref FROM artifact WHERE id = ?`),
		artifactID).Scan(&scope, &ownerKind, &ownerRef); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("artifacts: read home and owner: %w", err)
	}
	if (Expect{ScopeRef: scope, OwnerKind: ownerKind, OwnerRef: ownerRef}) != actor.Expect {
		return ErrStewardPrecondition
	}
	return nil
}

var errNoExpect = errors.New("artifacts: a steward write needs the state its authority was decided against")

func (a *Actor) valid() bool {
	return a != nil && a.Kind != "" && a.Ref != "" && a.Authority != ""
}

var errNoActor = errors.New("artifacts: the change needs an actor with a kind, a ref and an authority")

// reconcilerActor is the actor of orphaned and unorphaned events.
var reconcilerActor = Actor{Kind: ActorKindSystem, Ref: AuthorityReconciler, Authority: AuthorityReconciler}

// retentionActor is the actor of the expiry sweep's deleted events.
var retentionActor = Actor{Kind: ActorKindSystem, Ref: AuthorityRetention, Authority: AuthorityRetention}

// Event is a row of the append-only artifact_event table.
type Event struct {
	ID         string
	ArtifactID string
	At         time.Time
	Action     string
	ActorKind  string
	ActorRef   string
	Authority  string
	// Detail is the event's JSON detail (old and new expiry, scopes,
	// counts), or nil.
	Detail map[string]any
}

// Owner is one distinct owner of live artifacts.
type Owner struct {
	Kind string
	Ref  string
	// Marked and Unmarked count the owner's live artifacts with and
	// without orphaned_at, as ListLiveOwners found them, so the reconciler
	// writes only where the mark must change.
	Marked   int
	Unmarked int
}

// DeleteOptions qualifies DeleteArtifact.
type DeleteOptions struct {
	// Actor is who deletes the artifact. Required.
	Actor *Actor
	// ExpiredAt, when set, deletes the artifact only if it expires at or
	// before this instant (the expiry sweep: the expiry may have moved
	// since the artifact was found). Otherwise it returns ErrNotFound.
	ExpiredAt *time.Time
	// ScopeRef, when set, deletes the artifact only if it is still homed
	// in this scope: the home the caller's authority was decided on. A
	// move committed since then makes the delete return ErrNotFound and
	// change nothing.
	ScopeRef string
	// ScopeKind, when set, deletes the artifact only if its scope is still
	// of this kind (an attachment upload that was sent meanwhile is left
	// alone).
	ScopeKind string
	// Now is when the deletion happens; zero means time.Now().
	Now time.Time
}

// DeleteResult reports what DeleteArtifact removed with the artifact.
type DeleteResult struct {
	Grants int
	Links  int
}

// insertEvent writes one artifact_event row inside tx.
func (s *sqlStore) insertEvent(ctx context.Context, tx *sql.Tx, artifactID, action string, actor *Actor, at time.Time, detail map[string]any) error {
	if !actor.valid() {
		return errNoActor
	}
	var d any
	if len(detail) > 0 {
		b, err := json.Marshal(detail)
		if err != nil {
			return fmt.Errorf("artifacts: encode event detail: %w", err)
		}
		d = string(b)
	}
	if _, err := tx.ExecContext(ctx, s.rebind(`INSERT INTO artifact_event
		(id, artifact_id, at, action, actor_kind, actor_ref, authority, detail) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`),
		uuid.NewString(), artifactID, s.timeArg(at), action, actor.Kind, actor.Ref, actor.Authority, d); err != nil {
		return fmt.Errorf("artifacts: record event: %w", err)
	}
	return nil
}

// DeleteArtifact implements Store.
func (s *sqlStore) DeleteArtifact(ctx context.Context, artifactID string, opts DeleteOptions) (DeleteResult, error) {
	if !opts.Actor.valid() {
		return DeleteResult{}, errNoActor
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DeleteResult{}, fmt.Errorf("artifacts: begin delete: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := s.deleteArtifactTx(ctx, tx, artifactID, opts, now)
	if err != nil {
		return DeleteResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return DeleteResult{}, fmt.Errorf("artifacts: commit delete: %w", err)
	}
	return res, nil
}

// deleteArtifactTx is the one soft delete: inside tx it sets deleted_at,
// removes every grant and share link of the artifact, and records the
// deleted event with what was removed.
func (s *sqlStore) deleteArtifactTx(ctx context.Context, tx *sql.Tx, artifactID string, opts DeleteOptions, now time.Time) (DeleteResult, error) {
	at := s.timeArg(now)
	q := `UPDATE artifact SET deleted_at = ? WHERE id = ? AND deleted_at IS NULL`
	args := []any{at, artifactID}
	if opts.ExpiredAt != nil {
		q += ` AND expires_at IS NOT NULL AND expires_at <= ?`
		args = append(args, s.timeArg(*opts.ExpiredAt))
	}
	if opts.ScopeRef != "" {
		q += ` AND scope_ref = ?`
		args = append(args, opts.ScopeRef)
	}
	if opts.ScopeKind != "" {
		q += ` AND scope_kind = ?`
		args = append(args, opts.ScopeKind)
	}
	if stewardActor(opts.Actor) && !opts.Actor.Expect.set() {
		return DeleteResult{}, errNoExpect
	}
	if e := opts.Actor.Expect; e.set() {
		// The steward precondition (design 4.5), in the same conditional
		// update that deletes, so it is checked under the row's lock.
		q += ` AND scope_ref = ? AND owner_kind = ? AND owner_ref = ?`
		args = append(args, e.ScopeRef, e.OwnerKind, e.OwnerRef)
	}
	r, err := tx.ExecContext(ctx, s.rebind(q), args...)
	if err != nil {
		return DeleteResult{}, fmt.Errorf("artifacts: delete artifact: %w", err)
	}
	n, err := r.RowsAffected()
	if err != nil {
		return DeleteResult{}, fmt.Errorf("artifacts: delete artifact: %w", err)
	}
	if n != 1 {
		if opts.Actor.Expect.set() {
			var live int
			if err := tx.QueryRowContext(ctx, s.rebind(`SELECT COUNT(*) FROM artifact WHERE id = ? AND deleted_at IS NULL`),
				artifactID).Scan(&live); err != nil {
				return DeleteResult{}, fmt.Errorf("artifacts: delete artifact: %w", err)
			}
			if live > 0 {
				return DeleteResult{}, ErrStewardPrecondition
			}
		}
		return DeleteResult{}, ErrNotFound
	}
	var res DeleteResult
	if err := tx.QueryRowContext(ctx, s.rebind(`SELECT
		COALESCE(SUM(CASE WHEN subject_kind = ? THEN 0 ELSE 1 END), 0),
		COALESCE(SUM(CASE WHEN subject_kind = ? THEN 1 ELSE 0 END), 0)
		FROM artifact_grant WHERE artifact_id = ?`), SubjectLink, SubjectLink, artifactID).Scan(&res.Grants, &res.Links); err != nil {
		return DeleteResult{}, fmt.Errorf("artifacts: count grants: %w", err)
	}
	if _, err := tx.ExecContext(ctx, s.rebind(`DELETE FROM artifact_grant WHERE artifact_id = ?`), artifactID); err != nil {
		return DeleteResult{}, fmt.Errorf("artifacts: drop grants: %w", err)
	}
	if err := s.insertEvent(ctx, tx, artifactID, EventDeleted, opts.Actor, now,
		map[string]any{"grantsRemoved": res.Grants, "linksRevoked": res.Links}); err != nil {
		return DeleteResult{}, err
	}
	return res, nil
}

// ListLiveOwners implements Store.
func (s *sqlStore) ListLiveOwners(ctx context.Context, after *Owner, limit int) ([]Owner, error) {
	if limit <= 0 {
		return nil, errors.New("artifacts: ListLiveOwners needs a positive limit")
	}
	where, args := ownersAfter(after)
	rows, err := s.db.QueryContext(ctx, s.rebind(`SELECT owner_kind, owner_ref,
		COALESCE(SUM(CASE WHEN orphaned_at IS NULL THEN 0 ELSE 1 END), 0),
		COALESCE(SUM(CASE WHEN orphaned_at IS NULL THEN 1 ELSE 0 END), 0)
		FROM artifact WHERE deleted_at IS NULL AND `+notAttachmentScope("scope_kind")+where+`
		GROUP BY owner_kind, owner_ref ORDER BY owner_kind, owner_ref LIMIT `+strconv.Itoa(limit)), args...)
	if err != nil {
		return nil, fmt.Errorf("artifacts: list owners: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Owner
	for rows.Next() {
		var o Owner
		if err := rows.Scan(&o.Kind, &o.Ref, &o.Marked, &o.Unmarked); err != nil {
			return nil, fmt.Errorf("artifacts: scan owner: %w", err)
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("artifacts: list owners: %w", err)
	}
	return out, nil
}

// CountLiveOwners implements Store.
func (s *sqlStore) CountLiveOwners(ctx context.Context, after *Owner) (int, error) {
	where, args := ownersAfter(after)
	var n int
	if err := s.db.QueryRowContext(ctx, s.rebind(`SELECT COUNT(*) FROM (SELECT DISTINCT owner_kind, owner_ref
		FROM artifact WHERE deleted_at IS NULL AND `+notAttachmentScope("scope_kind")+where+`) o`), args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("artifacts: count owners: %w", err)
	}
	return n, nil
}

// ownersAfter is the keyset condition for owners strictly after after in
// (owner_kind, owner_ref) order.
func ownersAfter(after *Owner) (string, []any) {
	if after == nil {
		return "", nil
	}
	return ` AND (owner_kind > ? OR (owner_kind = ? AND owner_ref > ?))`, []any{after.Kind, after.Kind, after.Ref}
}

// SetOwnerOrphaned implements Store.
func (s *sqlStore) SetOwnerOrphaned(ctx context.Context, owner Owner, orphaned bool, now time.Time, limit int) ([]ArtifactRef, error) {
	var changed []ArtifactRef
	if _, err := s.setOwnerOrphaned(ctx, owner, orphaned, now, limit, &changed); err != nil {
		return nil, err
	}
	return changed, nil
}

// setOwnerOrphaned is SetOwnerOrphaned, appending each artifact it changes,
// with its home scope, to changed (valid only when it returns no error) and
// returning how many it changed. It keeps the count result so this change
// leaves the function's other lines as they were.
func (s *sqlStore) setOwnerOrphaned(ctx context.Context, owner Owner, orphaned bool, now time.Time, limit int, changed *[]ArtifactRef) (int, error) {
	if limit <= 0 {
		return 0, errors.New("artifacts: SetOwnerOrphaned needs a positive limit")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("artifacts: begin orphan mark: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	cond, action := `orphaned_at IS NULL`, EventOrphaned
	if !orphaned {
		cond, action = `orphaned_at IS NOT NULL`, EventUnorphaned
	}
	rows, err := tx.QueryContext(ctx, s.rebind(`SELECT id, scope_ref FROM artifact
		WHERE owner_kind = ? AND owner_ref = ? AND deleted_at IS NULL AND `+notAttachmentScope("scope_kind")+` AND `+cond+`
		ORDER BY id LIMIT `+strconv.Itoa(limit)), owner.Kind, owner.Ref)
	if err != nil {
		return 0, fmt.Errorf("artifacts: find owner's artifacts: %w", err)
	}
	var ids []string
	scopeOf := map[string]string{}
	for rows.Next() {
		var id, scope string
		if err := rows.Scan(&id, &scope); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("artifacts: scan artifact: %w", err)
		}
		ids = append(ids, id)
		scopeOf[id] = scope
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("artifacts: find owner's artifacts: %w", err)
	}
	var value any
	if orphaned {
		value = s.timeArg(now)
	}
	n := 0
	for _, id := range ids {
		// The condition is checked again so a concurrent pass cannot mark
		// one artifact twice.
		r, err := tx.ExecContext(ctx, s.rebind(`UPDATE artifact SET orphaned_at = ?
			WHERE id = ? AND deleted_at IS NULL AND `+notAttachmentScope("scope_kind")+` AND `+cond), value, id)
		if err != nil {
			return 0, fmt.Errorf("artifacts: mark orphan: %w", err)
		}
		if c, err := r.RowsAffected(); err != nil || c != 1 {
			if err != nil {
				return 0, fmt.Errorf("artifacts: mark orphan: %w", err)
			}
			continue
		}
		if err := s.insertEvent(ctx, tx, id, action, &reconcilerActor, now,
			map[string]any{"owner": PrincipalRef(owner.Kind, owner.Ref)}); err != nil {
			return 0, err
		}
		*changed = append(*changed, ArtifactRef{ID: id, ScopeRef: scopeOf[id]})
		n++
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("artifacts: commit orphan mark: %w", err)
	}
	return n, nil
}

// ListEvents implements Store.
func (s *sqlStore) ListEvents(ctx context.Context, artifactID string) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, s.rebind(`SELECT id, artifact_id, at, action, actor_kind, actor_ref, authority, detail
		FROM artifact_event WHERE artifact_id = ? ORDER BY at, id`), artifactID)
	if err != nil {
		return nil, fmt.Errorf("artifacts: list events: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Event
	for rows.Next() {
		var (
			e      Event
			at     dbTime
			detail sql.NullString
		)
		if err := rows.Scan(&e.ID, &e.ArtifactID, &at, &e.Action, &e.ActorKind, &e.ActorRef, &e.Authority, &detail); err != nil {
			return nil, fmt.Errorf("artifacts: scan event: %w", err)
		}
		e.At = at.Time
		if detail.Valid && detail.String != "" {
			if err := json.Unmarshal([]byte(detail.String), &e.Detail); err != nil {
				return nil, fmt.Errorf("artifacts: decode event detail: %w", err)
			}
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("artifacts: list events: %w", err)
	}
	return out, nil
}
