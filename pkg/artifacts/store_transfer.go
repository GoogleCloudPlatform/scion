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
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// expirySourceOrphanPolicy is the expiry_source value set by an orphan
// retention policy; an ownership transfer clears such an expiry while
// leaving manual and retention expiries intact.
const expirySourceOrphanPolicy = "orphan_policy"

// TransferOwner implements Store.
func (s *sqlStore) TransferOwner(ctx context.Context, artifactID string, opts TransferOptions, actor *Actor, now time.Time) (*Artifact, TransferResult, error) {
	if !actor.valid() {
		return nil, TransferResult{}, errNoActor
	}
	if opts.ExpectOwner.Kind == "" || opts.ExpectOwner.Ref == "" {
		return nil, TransferResult{}, errors.New("artifacts: transfer needs expected owner")
	}
	if opts.To.Kind != PrincipalKindAgent || opts.To.Ref == "" {
		return nil, TransferResult{}, errors.New("artifacts: transfer target must be an agent")
	}
	if opts.Sponsor == "" {
		return nil, TransferResult{}, errors.New("artifacts: transfer to an agent needs a sponsor")
	}
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC().Truncate(time.Microsecond)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, TransferResult{}, fmt.Errorf("artifacts: begin transfer: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := s.lockLiveArtifact(ctx, tx, artifactID); err != nil {
		return nil, TransferResult{}, err
	}
	if err := s.refuseAttachmentTx(ctx, tx, artifactID); err != nil {
		return nil, TransferResult{}, err
	}

	var (
		scopeKind, scopeRef, ownerKind, ownerRef string
		key, sponsor, expirySource               sql.NullString
	)
	if err := tx.QueryRowContext(ctx, s.rebind(`SELECT scope_kind, scope_ref, owner_kind, owner_ref, "key", sponsor_ref, expiry_source
		FROM artifact WHERE id = ?`), artifactID).Scan(
		&scopeKind, &scopeRef, &ownerKind, &ownerRef, &key, &sponsor, &expirySource); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, TransferResult{}, ErrNotFound
		}
		return nil, TransferResult{}, fmt.Errorf("artifacts: read artifact for transfer: %w", err)
	}

	if ownerKind != opts.ExpectOwner.Kind || ownerRef != opts.ExpectOwner.Ref {
		return nil, TransferResult{}, ErrOwnerChanged
	}
	if err := s.checkExpect(ctx, tx, artifactID, actor); err != nil {
		return nil, TransferResult{}, err
	}

	res := TransferResult{
		FromOwner:     Owner{Kind: ownerKind, Ref: ownerRef},
		FormerSponsor: sponsor.String,
	}

	// Key collision check on (scope_kind, scope_ref, To.Kind, To.Ref, key).
	if key.Valid && key.String != "" {
		var clash int
		if err := tx.QueryRowContext(ctx, s.rebind(`SELECT COUNT(*) FROM artifact
			WHERE scope_kind = ? AND scope_ref = ? AND owner_kind = ? AND owner_ref = ? AND "key" = ?
			AND deleted_at IS NULL AND id <> ?`),
			scopeKind, scopeRef, opts.To.Kind, opts.To.Ref, key.String, artifactID).Scan(&clash); err != nil {
			return nil, TransferResult{}, fmt.Errorf("artifacts: check transfer key conflict: %w", err)
		}
		if clash > 0 {
			if !opts.ClearKey {
				return nil, TransferResult{}, ErrKeyConflict
			}
			res.KeyCleared = true
		}
	}

	at := s.timeArg(now)
	setClauses := []string{
		`owner_kind = ?`,
		`owner_ref = ?`,
		`former_owner_kind = ?`,
		`former_owner_ref = ?`,
		`transferred_at = ?`,
		`orphaned_at = NULL`,
		`sponsor_ref = ?`,
		`updated_at = ?`,
	}
	args := []any{
		opts.To.Kind, opts.To.Ref,
		ownerKind, ownerRef,
		at,
		opts.Sponsor,
		at,
	}
	if res.KeyCleared {
		setClauses = append(setClauses, `"key" = NULL`, `former_key = ?`)
		args = append(args, key.String)
	}
	if expirySource.Valid && expirySource.String == expirySourceOrphanPolicy {
		setClauses = append(setClauses, `expires_at = NULL`, `expiry_source = NULL`)
	}
	args = append(args, artifactID)

	if _, err := tx.ExecContext(ctx, s.rebind(`UPDATE artifact SET `+strings.Join(setClauses, ", ")+` WHERE id = ?`), args...); err != nil {
		if key.Valid && key.String != "" && !res.KeyCleared && isUniqueConstraintErr(err) {
			return nil, TransferResult{}, ErrKeyConflict
		}
		return nil, TransferResult{}, fmt.Errorf("artifacts: update owner: %w", err)
	}

	// Remove the target agent's own redundant principal grant, if any.
	toPrincipalRef := PrincipalRef(opts.To.Kind, opts.To.Ref)
	var grantID, grantPerm string
	err = tx.QueryRowContext(ctx, s.rebind(`SELECT id, permission FROM artifact_grant
		WHERE artifact_id = ? AND subject_kind = ? AND subject_ref = ?`),
		artifactID, SubjectPrincipal, toPrincipalRef).Scan(&grantID, &grantPerm)
	switch {
	case err == nil:
		if _, err := tx.ExecContext(ctx, s.rebind(`DELETE FROM artifact_grant WHERE id = ?`), grantID); err != nil {
			return nil, TransferResult{}, fmt.Errorf("artifacts: remove target grant: %w", err)
		}
		res.RemovedGrantID = grantID
		res.RemovedGrantPerm = grantPerm
		if err := s.insertEvent(ctx, tx, artifactID, EventGrantRemoved, actor, now, map[string]any{
			"grantId": grantID, "subjectKind": SubjectPrincipal, "subjectRef": toPrincipalRef, "permission": grantPerm,
		}); err != nil {
			return nil, TransferResult{}, err
		}
	case !errors.Is(err, sql.ErrNoRows):
		return nil, TransferResult{}, fmt.Errorf("artifacts: find target grant: %w", err)
	}

	// Automatic sponsor review grant for the new sponsor when enabled, different
	// from the former sponsor, and not already holding a principal grant.
	if opts.SponsorReviewGrant && opts.Sponsor != "" && opts.Sponsor != sponsor.String {
		sponsorPrincipalRef := PrincipalRef(PrincipalKindUser, opts.Sponsor)
		var existing int
		if err := tx.QueryRowContext(ctx, s.rebind(`SELECT COUNT(*) FROM artifact_grant
			WHERE artifact_id = ? AND subject_kind = ? AND subject_ref = ?`),
			artifactID, SubjectPrincipal, sponsorPrincipalRef).Scan(&existing); err != nil {
			return nil, TransferResult{}, fmt.Errorf("artifacts: check sponsor grant: %w", err)
		}
		if existing == 0 {
			sg := &Grant{
				ID:           uuid.NewString(),
				ArtifactID:   artifactID,
				SubjectKind:  SubjectPrincipal,
				SubjectRef:   sponsorPrincipalRef,
				Permission:   GrantReview,
				CreatedByRef: SponsorReviewCreator,
				CreatedAt:    now,
			}
			if err := s.insertGrant(ctx, tx, sg); err != nil {
				return nil, TransferResult{}, err
			}
			res.SponsorReviewGrantID = sg.ID
			if err := s.insertEvent(ctx, tx, artifactID, EventGrantCreated, &sponsorReviewActor, now, map[string]any{
				"grantId": sg.ID, "subjectKind": sg.SubjectKind, "subjectRef": sg.SubjectRef, "permission": sg.Permission,
			}); err != nil {
				return nil, TransferResult{}, err
			}
		}
	}

	detail := map[string]any{
		"from":            PrincipalRef(ownerKind, ownerRef),
		"to":              toPrincipalRef,
		"sponsor":         opts.Sponsor,
		"formerOwnerLive": opts.FormerOwnerLive,
		"keyCleared":      res.KeyCleared,
	}
	if sponsor.String != "" {
		detail["formerSponsor"] = sponsor.String
	}
	if res.RemovedGrantID != "" {
		detail["removedGrantId"] = res.RemovedGrantID
	}
	if res.SponsorReviewGrantID != "" {
		detail["sponsorReviewGrantId"] = res.SponsorReviewGrantID
	}
	if err := s.insertEvent(ctx, tx, artifactID, EventOwnerTransferred, actor, now, detail); err != nil {
		return nil, TransferResult{}, err
	}

	if err := tx.Commit(); err != nil {
		return nil, TransferResult{}, fmt.Errorf("artifacts: commit transfer: %w", err)
	}
	updated, err := s.GetArtifact(ctx, artifactID)
	if err != nil {
		return nil, TransferResult{}, err
	}
	return updated, res, nil
}

// isUniqueConstraintErr reports whether err is a unique-constraint violation
// from either SQLite or Postgres (SQLSTATE 23505).
func isUniqueConstraintErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "23505") ||
		strings.Contains(msg, "idx_artifact_owner_key")
}
