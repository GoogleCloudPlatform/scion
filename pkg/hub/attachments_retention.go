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

package hub

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Attachment retention (ptone/scion#5832), in the artifact maintenance
// pass, at most once an hour:
//   - scratchpad projections (<scratchpad>/.attachments/<channel>/<id>/, every
//     channel) are swept on every topology once older than
//     messaging.attachment_inbox_retention_days: they are the agent-facing
//     copy, the same thing the inbox is, and keep the inbox's retention;
//   - legacy AttachmentStore blobs are swept once older than the artifact
//     gc_grace_hours, but only when the upload was never bound to a message
//     (an orphan) or the hub now has object storage (the store is legacy:
//     new uploads go to the artifact service), and never while a message
//     that references the blob is still undispatched. On a hub without
//     object storage a blob a sent message references is kept until the
//     message is deleted (lifecycle phase).

// attachmentRetentionInterval is the shortest time between two attachment
// retention sweeps; the maintenance pass runs more often.
const attachmentRetentionInterval = time.Hour

// attachmentRetentionProjectPage is the page size of the project listing.
const attachmentRetentionProjectPage = 500

// attachmentRetentionLinkProbe bounds the referencing messages checked per
// legacy blob.
const attachmentRetentionLinkProbe = 100

// attachmentRetention paces the attachment retention sweep.
type attachmentRetention struct {
	mu   sync.Mutex
	last time.Time
}

// due reports whether a sweep should run at now, and records it as run.
func (r *attachmentRetention) due(now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.last.IsZero() && now.Sub(r.last) < attachmentRetentionInterval {
		return false
	}
	r.last = now
	return true
}

// projectionRetentionCutoff returns the time before which projections are
// swept: now minus messaging.attachment_inbox_retention_days.
func (s *Server) projectionRetentionCutoff(now time.Time) time.Time {
	days := s.attachmentDelivery().InboxRetentionDays()
	if days <= 0 {
		days = DefaultAttachmentInboxRetentionDays
	}
	return now.Add(-time.Duration(days) * 24 * time.Hour)
}

// legacyBlobRetentionCutoff returns the time before which eligible legacy
// blobs are swept: now minus the artifact gc_grace_hours.
func (s *Server) legacyBlobRetentionCutoff(now time.Time) time.Time {
	return now.Add(-time.Duration(s.artifactsConfig().GCGraceHours) * time.Hour)
}

// sweepAttachmentRetention runs one attachment retention sweep when one is
// due (attachmentRetentionInterval); part of the artifact maintenance pass.
// Failures are logged; the next sweep retries.
func (s *Server) sweepAttachmentRetention(ctx context.Context, now time.Time) {
	if !s.attachmentRetention.due(now) {
		return
	}
	if n := s.sweepAttachmentProjections(ctx, s.projectionRetentionCutoff(now)); n > 0 {
		slog.InfoContext(ctx, "attachments: swept scratchpad projections older than the inbox retention", "count", n)
	}
	if n := s.sweepLegacyAttachmentBlobs(ctx, s.legacyBlobRetentionCutoff(now)); n > 0 {
		slog.InfoContext(ctx, "attachments: swept legacy attachment blobs older than the grace period", "count", n)
	}
}

// sweepLegacyAttachmentBlobs removes the hub-private AttachmentStore blobs
// older than cutoff that legacyBlobSweepable allows, and returns how many it
// removed. Only the bytes are removed; a metadata row left behind answers as
// not found, as a deleted blob always has.
func (s *Server) sweepLegacyAttachmentBlobs(ctx context.Context, cutoff time.Time) int {
	s.mu.RLock()
	as := s.attachmentStore
	wcs := s.webChatStore
	s.mu.RUnlock()
	local, ok := as.(*LocalDiskAttachmentStore)
	if !ok || wcs == nil {
		return 0
	}
	objectStorage := s.objectStorageConfigured()
	n, err := local.sweep(cutoff, func(projectID, id string) bool {
		return ctx.Err() == nil && s.legacyBlobSweepable(ctx, wcs, projectID, id, objectStorage)
	})
	if err != nil {
		slog.WarnContext(ctx, "attachments: legacy blob sweep failed", "removed", n, "error", err)
	}
	return n
}

// legacyBlobSweepable reports whether the legacy blob directory of
// candidate (projectID, id), already older than the cutoff, may be removed.
// The candidate must be the attachment's recorded location: a metadata row
// for id must exist and record exactly projectID ("" for an upload with no
// project), so a directory is never removed for being where an attachment
// could be. Then the attachment must be referenced by no message (an orphan
// upload), or, when the hub has object storage, every message that
// references it must have been dispatched (or be gone). Any lookup failure
// keeps the blob: a blob is swept as an orphan only when the link lookup
// succeeded and returned no message. A send whose link insert failed
// (logged on the send path) leaves the blob unlinked; history lists
// attachments through the same links, so such a blob is already not shown
// with its message.
//
// The object-storage condition is deliberately not widened to hubs whose
// attachment artifacts use local blob storage: there, blobs that sent
// messages reference are what history downloads of attachments sent
// before artifacts were on still read, so they are kept until they can be
// moved or their messages are deleted.
func (s *Server) legacyBlobSweepable(ctx context.Context, wcs WebChatStore, projectID, id string, objectStorage bool) bool {
	meta, err := wcs.GetAttachment(ctx, id)
	if err != nil || meta == nil || meta.ID != id || meta.ProjectID != projectID {
		return false
	}
	msgIDs, err := wcs.ListMessageIDsForAttachment(ctx, id, attachmentRetentionLinkProbe)
	if err != nil {
		return false
	}
	if len(msgIDs) == 0 {
		return true
	}
	if !objectStorage || len(msgIDs) >= attachmentRetentionLinkProbe {
		return false
	}
	msgs, err := s.store.GetMessagesByIDs(ctx, msgIDs)
	if err != nil {
		return false
	}
	for _, m := range msgs {
		if m != nil && messageUndispatched(m) {
			return false
		}
	}
	return true
}

// messageUndispatched reports whether m may still be handed to a
// dispatcher (pending) or was saved for a later catch-up (deferred).
func messageUndispatched(m *store.Message) bool {
	switch m.DispatchState {
	case store.MessageDispatchPending, store.MessageDispatchDeferred:
		return true
	}
	return false
}

// sweepAttachmentProjections removes, for every project with a scratchpad
// the hub reaches (resolveProjectScratchpad), each projection older than
// cutoff: every entry under <scratchpad>/.attachments/<channel>/, for every
// channel (web chat, the chat plugins, `scion message --attach`). A
// projection is a copy: the bytes stay in the recipient's inbox, the
// artifact service or the attachment store. It returns the number removed.
func (s *Server) sweepAttachmentProjections(ctx context.Context, cutoff time.Time) int {
	removed := 0
	cursor := ""
	for {
		page, err := s.store.ListProjects(ctx, store.ProjectFilter{}, store.ListOptions{
			Limit: attachmentRetentionProjectPage, Cursor: cursor,
		})
		if err != nil {
			slog.WarnContext(ctx, "attachments: retention sweep could not list projects", "error", err)
			return removed
		}
		for i := range page.Items {
			if ctx.Err() != nil {
				return removed
			}
			project := &page.Items[i]
			if _, err := projectfs.DeclaredSharedDir(project.SharedDirs, attachmentSharedDirName, ""); err != nil {
				continue
			}
			sp := s.resolveProjectScratchpad(ctx, project)
			if sp == nil {
				continue
			}
			n, err := sweepProjectionsIn(sp.resolution.Resolved, cutoff)
			removed += n
			if err != nil {
				slog.WarnContext(ctx, "attachments: retention sweep of a scratchpad failed",
					"project_id", project.ID, "removed", n, "error", err)
			}
		}
		if page.NextCursor == "" {
			return removed
		}
		cursor = page.NextCursor
	}
}

// sweepProjectionsIn removes every entry under
// <scratchpad>/.attachments/<channel>/ of resolved whose modification time
// is before cutoff, and returns how many it removed. The scratchpad is
// opened through the layout authority (Resolved.Open, without create).
// Agents mount the dir read-write, so any component may be a planted
// symlink, including .attachments itself (`.attachments -> .` would
// otherwise turn the sweep on the whole scratchpad).
// .attachments and each channel are therefore Lstat'ed without following,
// skipped unless they are real directories, and opened as their own
// os.Root whose identity must equal what was Lstat'ed (os.SameFile). Every
// later operation is relative to that descriptor, so swapping a name for a
// link after the check changes nothing. An entry that is a symlink is
// removed as the link itself. A missing scratchpad or .attachments dir is
// not an error.
func sweepProjectionsIn(resolved projectfs.Resolved, cutoff time.Time) (int, error) {
	root, err := resolved.Open(false)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	defer func() { _ = root.Close() }()

	attachments, err := openRealSubdir(root, projectfs.AttachmentsDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, errNotRealDir) {
			return 0, nil
		}
		return 0, err
	}
	defer func() { _ = attachments.Close() }()

	channels, err := readRootDir(attachments, ".")
	if err != nil {
		return 0, err
	}
	removed := 0
	var firstErr error
	for _, ch := range channels {
		chRoot, err := openRealSubdir(attachments, ch.Name())
		if err != nil {
			if !errors.Is(err, errNotRealDir) && !errors.Is(err, fs.ErrNotExist) && firstErr == nil {
				firstErr = err
			}
			continue
		}
		entries, err := readRootDir(chRoot, ".")
		if err != nil {
			_ = chRoot.Close()
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for _, e := range entries {
			info, err := chRoot.Lstat(e.Name())
			if err != nil || !info.ModTime().Before(cutoff) {
				continue
			}
			if err := chRoot.RemoveAll(e.Name()); err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			removed++
		}
		_ = chRoot.Close()
	}
	return removed, firstErr
}

// errNotRealDir: a path component is not a real directory (a symlink, a
// file), so it is not walked.
var errNotRealDir = errors.New("not a real directory")

// openRealSubdir opens name, a single component inside parent, as an
// os.Root only when it is a real directory: Lstat (never following) must
// report a directory, and the opened root must be that same directory
// (os.SameFile), so a name swapped for a link between the two is refused.
func openRealSubdir(parent *os.Root, name string) (*os.Root, error) {
	info, err := parent.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, errNotRealDir
	}
	sub, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	got, err := sub.Stat(".")
	if err != nil || !os.SameFile(got, info) {
		_ = sub.Close()
		return nil, errNotRealDir
	}
	return sub, nil
}

// readRootDir lists directory name inside root.
func readRootDir(root *os.Root, name string) ([]os.DirEntry, error) {
	d, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = d.Close() }()
	return d.ReadDir(-1)
}
