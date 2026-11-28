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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const (
	uploadSessionSweepJobName = "upload-session-sweep"
	uploadSweepBatchLimit     = 100
	uploadSweepMaxBatches     = 20
)

// uploadSessionSweepHandler returns the recurring singleton job that expires
// idle/hard-expired upload sessions, fails stuck finalizing sessions, and
// removes staging + rows for terminal sessions past the 15-minute retention
// window (ptone/scion#8654 §3.7).
//
// It runs regardless of whether hub.large_uploads is enabled so sessions
// created before the experiment was turned off are still cleaned up.
func (s *Server) uploadSessionSweepHandler() func(ctx context.Context) {
	return func(ctx context.Context) {
		s.sweepUploadSessions(ctx, uploadNowFn())
	}
}

func (s *Server) sweepUploadSessions(ctx context.Context, now time.Time) {
	if s.store == nil {
		return
	}
	for batch := 0; batch < uploadSweepMaxBatches; batch++ {
		if ctx.Err() != nil {
			return
		}
		sessions, err := s.store.ListExpiredUploadSessions(ctx, now, uploadSweepBatchLimit)
		if err != nil {
			slog.WarnContext(ctx, "upload-session-sweep: failed to list expired sessions", "error", err)
			return
		}
		if len(sessions) == 0 {
			return
		}

		progressed := false
		for _, sess := range sessions {
			if ctx.Err() != nil {
				return
			}
			switch {
			case sess.State == store.UploadStateUploading:
				if err := s.store.TransitionUploadSession(ctx, sess.ID, []store.UploadSessionState{store.UploadStateUploading}, store.UploadStateExpired, "", nil); err != nil {
					if !errors.Is(err, store.ErrBusy) && !errors.Is(err, store.ErrStateChanged) && !errors.Is(err, store.ErrNotFound) {
						slog.WarnContext(ctx, "upload-session-sweep: failed to expire uploading session",
							"session_id", sess.ID, "error", err)
					}
					continue
				}
				progressed = true
				if cerr := s.cleanupUploadSessionStaging(ctx, sess); cerr != nil {
					slog.WarnContext(ctx, "upload-session-sweep: best-effort staging cleanup after expiry failed",
						"session_id", sess.ID, "error", cerr)
				}

			case sess.State == store.UploadStateFinalizing:
				if err := s.store.TransitionUploadSession(ctx, sess.ID, []store.UploadSessionState{store.UploadStateFinalizing}, store.UploadStateFailed, "", nil); err != nil {
					if !errors.Is(err, store.ErrBusy) && !errors.Is(err, store.ErrStateChanged) && !errors.Is(err, store.ErrNotFound) {
						slog.WarnContext(ctx, "upload-session-sweep: failed to mark stuck finalizing session failed",
							"session_id", sess.ID, "error", err)
					}
					continue
				}
				progressed = true
				if cerr := s.cleanupUploadSessionStaging(ctx, sess); cerr != nil {
					slog.WarnContext(ctx, "upload-session-sweep: best-effort staging cleanup after finalizing failure failed",
						"session_id", sess.ID, "error", cerr)
				}

			case sess.State.IsTerminal():
				if err := s.cleanupUploadSessionStaging(ctx, sess); err != nil {
					slog.WarnContext(ctx, "upload-session-sweep: staging cleanup failed; backing off row for retry",
						"session_id", sess.ID, "state", sess.State, "error", err)
					if terr := s.store.TouchUploadSessionUpdatedAt(ctx, sess.ID, now); terr != nil && !errors.Is(terr, store.ErrNotFound) {
						slog.WarnContext(ctx, "upload-session-sweep: failed to back off terminal session row",
							"session_id", sess.ID, "error", terr)
					} else {
						progressed = true
					}
					continue
				}
				if err := s.store.DeleteUploadSession(ctx, sess.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
					slog.WarnContext(ctx, "upload-session-sweep: failed to delete terminal session row",
						"session_id", sess.ID, "error", err)
					if terr := s.store.TouchUploadSessionUpdatedAt(ctx, sess.ID, now); terr == nil {
						progressed = true
					}
				} else {
					progressed = true
				}
			}
		}

		if len(sessions) < uploadSweepBatchLimit || !progressed {
			return
		}
	}
}

// cleanupUploadSessionStaging removes staged bytes for sess by re-resolving
// the target's sink. Returns nil if the target or staging is already absent.
func (s *Server) cleanupUploadSessionStaging(ctx context.Context, sess *store.UploadSession) error {
	switch sess.TargetKind {
	case store.UploadTargetWorkspace:
		project, err := s.store.GetProject(ctx, sess.ScopeProjectID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil
			}
			return err
		}
		resolved, err := s.resolveProjectWorkspace(ctx, project)
		if err != nil {
			if errors.Is(err, errLinkedWorkspaceRemote) {
				return nil
			}
			return err
		}
		root, err := resolved.Open(false)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		defer func() { _ = root.Close() }()
		return newFileUploadSink(root).Cleanup(ctx, sess)

	case store.UploadTargetSharedDir:
		project, err := s.store.GetProject(ctx, sess.ScopeProjectID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil
			}
			return err
		}
		resolution, err := s.resolveSharedDirPath(ctx, project, sess.ScopeSharedDir)
		if err != nil {
			if errors.Is(err, errSharedDirNotDeclared) || errors.Is(err, errSharedDirRequiresColocatedBroker) {
				return nil
			}
			return err
		}
		root, err := s.openSharedDirRoot(resolution, false)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		defer func() { _ = root.Close() }()
		return newFileUploadSink(root).Cleanup(ctx, sess)

	case store.UploadTargetChatAttachment:
		blobs := s.artifactBackend().Blobs
		if blobs == nil {
			return nil
		}
		return newObjectUploadSink(blobs, nil).Cleanup(ctx, sess)

	default:
		return nil
	}
}
