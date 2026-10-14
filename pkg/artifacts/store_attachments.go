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
	"strconv"
	"strings"
	"time"
)

// Store queries of the attachment scopes (ptone/scion#4595).

// Authorities of the attachment retirements recorded in artifact_event.
const (
	// AuthorityMessageDeleted: the message the attachment was sent in was
	// deleted or is gone.
	AuthorityMessageDeleted = "message_deleted"
	// AuthorityAttachmentUnsent: the upload was never sent within
	// UploadTTL.
	AuthorityAttachmentUnsent = "attachment_unsent"
)

// messageGoneActor is the actor of the deleted event of an attachment
// whose message is gone.
var messageGoneActor = Actor{Kind: ActorKindSystem, Ref: AuthorityMessageDeleted, Authority: AuthorityMessageDeleted}

// unsentActor is the actor of the deleted event of an upload that was
// never sent.
var unsentActor = Actor{Kind: ActorKindSystem, Ref: AuthorityAttachmentUnsent, Authority: AuthorityAttachmentUnsent}

// notAttachmentScope is the condition, on an artifact column named col,
// that keeps rows of the attachment scopes out. The kinds are constants,
// so they are written into the query rather than bound.
func notAttachmentScope(col string) string {
	return col + ` NOT IN ('` + ScopeKindUpload + `', '` + ScopeKindMessage + `')`
}

// refuseAttachmentTx returns ErrAttachmentNotShareable when artifactID,
// already locked in tx, is a row of an attachment scope. Every store write
// that shares, versions or changes an artifact calls it under the lock, so
// the refusal does not depend on what the caller checked before.
func (s *sqlStore) refuseAttachmentTx(ctx context.Context, tx *sql.Tx, artifactID string) error {
	var kind string
	err := tx.QueryRowContext(ctx, s.rebind(`SELECT scope_kind FROM artifact WHERE id = ?`), artifactID).Scan(&kind)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("artifacts: read scope: %w", err)
	}
	if IsAttachmentScope(kind) {
		return ErrAttachmentNotShareable
	}
	return nil
}

// GetAttachment implements Store.
func (s *sqlStore) GetAttachment(ctx context.Context, id string) (*Artifact, error) {
	if id == "" {
		return nil, ErrNotFound
	}
	return s.getArtifact(ctx, `(id = ? OR legacy_attachment_id = ?) AND scope_kind IN (?, ?)`,
		id, id, ScopeKindUpload, ScopeKindMessage)
}

// BindAttachment implements Store.
func (s *sqlStore) BindAttachment(ctx context.Context, artifactID, ownerKind, ownerRef, messageID string, now time.Time) error {
	if artifactID == "" || ownerKind == "" || ownerRef == "" || messageID == "" {
		return errors.New("artifacts: BindAttachment needs an artifact, an owner and a message")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("artifacts: begin bind: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	// The lock first: two sends of the same upload decide one at a time.
	if err := s.lockLiveArtifact(ctx, tx, artifactID); err != nil {
		return err
	}
	var kind, ref, oKind, oRef string
	if err := tx.QueryRowContext(ctx, s.rebind(`SELECT scope_kind, scope_ref, owner_kind, owner_ref
		FROM artifact WHERE id = ?`), artifactID).Scan(&kind, &ref, &oKind, &oRef); err != nil {
		return fmt.Errorf("artifacts: read attachment: %w", err)
	}
	sender := PrincipalRef(ownerKind, ownerRef)
	switch {
	case !IsAttachmentScope(kind):
		return ErrNotFound
	case kind != ScopeKindUpload:
		return ErrAttachmentBound
	case ref != sender || oKind != ownerKind || oRef != ownerRef:
		return ErrAttachmentNotOwned
	}
	res, err := tx.ExecContext(ctx, s.rebind(`UPDATE artifact SET scope_kind = ?, scope_ref = ?, updated_at = ?
		WHERE id = ? AND scope_kind = ? AND scope_ref = ? AND deleted_at IS NULL`),
		ScopeKindMessage, messageID, s.timeArg(now), artifactID, ScopeKindUpload, sender)
	if err != nil {
		return fmt.Errorf("artifacts: bind attachment: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		if err != nil {
			return fmt.Errorf("artifacts: bind attachment: %w", err)
		}
		return ErrAttachmentBound
	}
	return commit(tx)
}

// ListMessageAttachments implements Store.
func (s *sqlStore) ListMessageAttachments(ctx context.Context, messageIDs []string) (map[string][]AttachmentRow, error) {
	out := make(map[string][]AttachmentRow)
	for start := 0; start < len(messageIDs); start += maxMessageRefQueryIDs {
		end := min(start+maxMessageRefQueryIDs, len(messageIDs))
		if err := s.listMessageAttachmentsChunk(ctx, messageIDs[start:end], out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// listMessageAttachmentsChunk adds the attachments of one chunk of message
// ids to out, in one query.
func (s *sqlStore) listMessageAttachmentsChunk(ctx context.Context, chunk []string, out map[string][]AttachmentRow) error {
	args := []any{ScopeKindMessage, VersionStateReady}
	for _, id := range chunk {
		args = append(args, id)
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(chunk)), ", ")
	rows, err := s.db.QueryContext(ctx, s.rebind(`SELECT
		a.id, a.scope_kind, a.scope_ref, a.owner_kind, a.owner_ref, a.title, a.current_seq, a.created_at, a.updated_at,
		f.version_id, f.path, f.size, f.sha256, f.media_type, f.origin, f.source_url, f.fetch_status, f.fetch_error, f.received
		FROM artifact a
		JOIN artifact_version v ON v.artifact_id = a.id AND v.seq = a.current_seq
		JOIN artifact_file f ON f.version_id = v.id AND f.path = v.entry_path
		WHERE a.scope_kind = ? AND a.deleted_at IS NULL AND v.state = ? AND a.scope_ref IN (`+placeholders+`)
		ORDER BY a.scope_ref, a.created_at, a.id`), args...)
	if err != nil {
		return fmt.Errorf("artifacts: list message attachments: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			row                              AttachmentRow
			seq                              sql.NullInt64
			created, updated                 dbTime
			digest, src, status, fetchErrMsg sql.NullString
			received                         bool
		)
		a, f := &row.Artifact, &row.File
		if err := rows.Scan(&a.ID, &a.ScopeKind, &a.ScopeRef, &a.OwnerKind, &a.OwnerRef, &a.Title, &seq, &created, &updated,
			&f.VersionID, &f.Path, &f.Size, &digest, &f.MediaType, &f.Origin, &src, &status, &fetchErrMsg, &received); err != nil {
			return fmt.Errorf("artifacts: scan message attachment: %w", err)
		}
		a.CurrentSeq = int(seq.Int64)
		a.CreatedAt, a.UpdatedAt = created.Time, updated.Time
		f.SHA256, f.SourceURL, f.FetchStatus, f.FetchError = digest.String, src.String, status.String, fetchErrMsg.String
		f.Pending = !received
		out[a.ScopeRef] = append(out[a.ScopeRef], row)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("artifacts: list message attachments: %w", err)
	}
	return nil
}

// RetireScope implements Store.
func (s *sqlStore) RetireScope(ctx context.Context, kind, ref string, actor *Actor, limit int) (int, error) {
	if kind == "" || ref == "" || limit <= 0 {
		return 0, errors.New("artifacts: RetireScope needs a scope and a positive limit")
	}
	if !actor.valid() {
		return 0, errNoActor
	}
	ids, err := s.liveIDs(ctx, `scope_kind = ? AND scope_ref = ?`, `id`, limit, kind, ref)
	if err != nil {
		return 0, err
	}
	return s.deleteEach(ctx, ids, DeleteOptions{Actor: actor, ScopeKind: kind, ScopeRef: ref})
}

// RetireStaleUploads implements Store.
func (s *sqlStore) RetireStaleUploads(ctx context.Context, cutoff time.Time, limit int) (int, error) {
	if limit <= 0 {
		return 0, nil
	}
	ids, err := s.liveIDs(ctx, `scope_kind = ? AND created_at < ?`, `created_at, id`, limit, ScopeKindUpload, s.timeArg(cutoff))
	if err != nil {
		return 0, err
	}
	// ScopeKind is checked again at the delete, so an upload sent since it
	// was found is left alone.
	return s.deleteEach(ctx, ids, DeleteOptions{Actor: &unsentActor, ScopeKind: ScopeKindUpload})
}

// liveIDs returns the ids of up to limit live artifacts matching where, in
// the given order.
func (s *sqlStore) liveIDs(ctx context.Context, where, order string, limit int, args ...any) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, s.rebind(`SELECT id FROM artifact
		WHERE deleted_at IS NULL AND `+where+` ORDER BY `+order+` LIMIT `+strconv.Itoa(limit)), args...)
	if err != nil {
		return nil, fmt.Errorf("artifacts: find artifacts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("artifacts: scan artifact: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("artifacts: find artifacts: %w", err)
	}
	return ids, nil
}

// deleteEach deletes each of ids through DeleteArtifact with opts, skipping
// those that no longer match, and returns how many it deleted.
func (s *sqlStore) deleteEach(ctx context.Context, ids []string, opts DeleteOptions) (int, error) {
	n := 0
	for _, id := range ids {
		_, err := s.DeleteArtifact(ctx, id, opts)
		switch {
		case errors.Is(err, ErrNotFound):
			// Deleted or moved meanwhile.
		case err != nil:
			return n, err
		default:
			n++
		}
	}
	return n, nil
}

// ListLiveScopeRefs implements Store.
func (s *sqlStore) ListLiveScopeRefs(ctx context.Context, kind, after string, limit int) ([]string, error) {
	if kind == "" || limit <= 0 {
		return nil, errors.New("artifacts: ListLiveScopeRefs needs a kind and a positive limit")
	}
	rows, err := s.db.QueryContext(ctx, s.rebind(`SELECT DISTINCT scope_ref FROM artifact
		WHERE scope_kind = ? AND deleted_at IS NULL AND scope_ref > ?
		ORDER BY scope_ref LIMIT `+strconv.Itoa(limit)), kind, after)
	if err != nil {
		return nil, fmt.Errorf("artifacts: list scope refs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			return nil, fmt.Errorf("artifacts: scan scope ref: %w", err)
		}
		out = append(out, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("artifacts: list scope refs: %w", err)
	}
	return out, nil
}
