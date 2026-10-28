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
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Web chat attachment copies in local scratchpads, removed once.
//
// A web chat attachment is copied into a project's scratchpad
// (.attachments/_webchat/<attachment-id>/) only when the whole project
// reads its message. Copies made before that rule applied on the local
// backend, for direct and group conversations, are removed once, in the
// background, at the first hub start that has this code: every
// _webchat/<id> whose attachment row exists and none of whose referencing
// messages is read by the whole project. Copies with no attachment row, or
// whose referencing messages cannot all be read, are left to the
// projection sweep (sweepAttachmentProjections). The canonical bytes stay
// in the hub's attachment store, so downloads from history are unaffected.
// A hub setting records completion; a run that could not finish is retried
// at the next maintenance pass.

// webChatCopyCleanupMarker is the hub setting that records a completed
// run.
const webChatCopyCleanupMarker = "attachments.webchat_copy_cleanup.v1"

// webChatCopyCleanupEntryLimit bounds the entries one run examines per
// project; a project with more is finished by a later run.
const webChatCopyCleanupEntryLimit = 10000

// webChatCopyCleanupOnce runs runWebChatCopyCleanup from the artifact
// maintenance pass (sweepAttachmentRetention) until a run completes, and
// then not again in this process. It runs there, not at start, because
// the hub's chat and attachment stores are installed after the background
// services start; the first pass comes after them.
func (s *Server) webChatCopyCleanupOnce(ctx context.Context) {
	r := &s.attachmentRetention
	r.mu.Lock()
	done := r.copyCleanupDone
	r.mu.Unlock()
	if done {
		return
	}
	removed, done := s.runWebChatCopyCleanup(ctx)
	if !done {
		if ctx.Err() == nil {
			slog.WarnContext(ctx, "attachments: web chat copy cleanup did not finish; it runs again at the next maintenance pass", "removed", removed)
		}
		return
	}
	r.mu.Lock()
	r.copyCleanupDone = true
	r.mu.Unlock()
}

// runWebChatCopyCleanup removes, from every project scratchpad on the local
// backend, the web chat attachment copies webChatCopyRemovable allows, and
// records completion. It returns how many copies it removed and whether
// the cleanup is complete (now or by an earlier run).
func (s *Server) runWebChatCopyCleanup(ctx context.Context) (int, bool) {
	if s.store == nil {
		return 0, false
	}
	if _, err := s.store.GetHubSetting(ctx, webChatCopyCleanupMarker); err == nil {
		return 0, true
	} else if !errors.Is(err, store.ErrNotFound) {
		slog.WarnContext(ctx, "attachments: web chat copy cleanup could not read its marker", "error", err)
		return 0, false
	}
	s.mu.RLock()
	wcs := s.webChatStore
	s.mu.RUnlock()
	if wcs == nil {
		return 0, false
	}

	removed := 0
	complete := true
	cursor := ""
	for {
		page, err := s.store.ListProjects(ctx, store.ProjectFilter{}, store.ListOptions{
			Limit: attachmentRetentionProjectPage, Cursor: cursor,
		})
		if err != nil {
			slog.WarnContext(ctx, "attachments: web chat copy cleanup could not list projects", "error", err)
			return removed, false
		}
		for i := range page.Items {
			if ctx.Err() != nil {
				return removed, false
			}
			project := &page.Items[i]
			if _, err := projectfs.DeclaredSharedDir(project.SharedDirs, attachmentSharedDirName, ""); err != nil {
				continue
			}
			sp := s.resolveProjectScratchpad(ctx, project)
			if sp == nil || sp.resolution.Resolved.Backend != projectfs.BackendLocal {
				continue
			}
			n, kept, ok := s.cleanupWebChatCopiesIn(ctx, wcs, project.ID, sp.resolution.Resolved)
			removed += n
			if !ok {
				complete = false
			}
			if n > 0 || !ok {
				slog.InfoContext(ctx, "attachments: removed web chat attachment copies of conversations not read by the whole project",
					"project_id", project.ID, "removed", n, "kept", kept, "complete", ok)
			}
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if !complete {
		return removed, false
	}
	doc, _ := json.Marshal(map[string]interface{}{"completed_at": time.Now().UTC().Format(time.RFC3339), "removed": removed})
	if _, err := s.store.UpsertHubSetting(ctx, webChatCopyCleanupMarker, doc, "system", -1, "seeded"); err != nil {
		slog.WarnContext(ctx, "attachments: web chat copy cleanup could not record completion", "error", err)
		return removed, false
	}
	return removed, true
}

// cleanupWebChatCopiesIn removes the web chat copies of one project's
// resolved scratchpad that webChatCopyRemovable allows, through
// pkg/projectfs (confined, symlinks refused). It returns how many it
// removed and kept, and whether every entry was examined. A .attachments
// or _webchat that is not a real directory is skipped: the hub did not
// write it.
func (s *Server) cleanupWebChatCopiesIn(ctx context.Context, wcs WebChatStore, projectID string, resolved projectfs.Resolved) (removed, kept int, ok bool) {
	names, err := projectfs.ListAttachmentEntries(resolved, attachmentStagingSubdir)
	if err != nil {
		if errors.Is(err, projectfs.ErrNotRealDir) {
			return 0, 0, true
		}
		slog.WarnContext(ctx, "attachments: web chat copy cleanup could not list a scratchpad", "project_id", projectID, "error", err)
		return 0, 0, false
	}
	ok = true
	if len(names) > webChatCopyCleanupEntryLimit {
		names = names[:webChatCopyCleanupEntryLimit]
		ok = false
	}
	for _, id := range names {
		if ctx.Err() != nil {
			return removed, kept, false
		}
		if !s.webChatCopyRemovable(ctx, wcs, projectID, id) {
			kept++
			continue
		}
		gone, err := projectfs.RemoveAttachmentEntry(resolved, attachmentStagingSubdir, id)
		if err != nil {
			if errors.Is(err, projectfs.ErrNotRealDir) {
				return removed, kept, true
			}
			ok = false
			continue
		}
		if gone {
			removed++
		}
	}
	return removed, kept, ok
}

// webChatCopyRemovable reports whether the web chat copy named id in
// project projectID's scratchpad is to be removed: id names an attachment
// row, at least one message references it (fewer than
// attachmentRetentionLinkProbe), every referencing message exists and was
// read, and none of them is read by the whole of projectID
// (projectionAudienceIsProject). Any lookup failure, and any referencing
// message that is missing, keeps the copy.
func (s *Server) webChatCopyRemovable(ctx context.Context, wcs WebChatStore, projectID, id string) bool {
	meta, err := wcs.GetAttachment(ctx, id)
	if err != nil || meta == nil || meta.ID != id {
		return false
	}
	msgIDs, err := wcs.ListMessageIDsForAttachment(ctx, id, attachmentRetentionLinkProbe)
	if err != nil || len(msgIDs) == 0 || len(msgIDs) >= attachmentRetentionLinkProbe {
		return false
	}
	msgs, err := s.store.GetMessagesByIDs(ctx, msgIDs)
	if err != nil {
		return false
	}
	// GetMessagesByIDs skips ids it cannot find: a referencing message that
	// is gone (deleted, or a dangling link) leaves the copy unattributable,
	// so it is kept.
	for _, mid := range msgIDs {
		m := msgs[mid]
		if m == nil || s.projectionAudienceIsProject(ctx, m.ThreadID, projectID) {
			return false
		}
	}
	return true
}
