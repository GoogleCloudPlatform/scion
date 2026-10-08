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
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts/critic"
)

// Review versions (design D11, D18, D20). A review is a version of kind
// review: a complete copy of the bundle whose text files carry CriticMarkup.
// Its base is the version the reviewer started from, which the client names
// when it finalizes the review; it must still be the current version.
// Finalize accepts a review only if it changes nothing outside marks:
// for every text file, clean(review file) equals the base's baseline text
// after NFC and LF normalisation. The baseline text of a publish version is
// its bytes; of a review version, its clean projection, so a review of a
// review is checked against the same baseline. A file byte-identical to the
// base's is unchanged. A non-text file must be identical, every base file
// must be present, no file may be added, and the entry must not move.
// Remote images the hub added at finalize are not part of the comparison.

// CodeUnmarkedChanges is the error code of a review finalize refused because
// the review changes text outside CriticMarkup. Details: "files", a list of
// UnmarkedFile, and "truncated", true when more files differed than listed.
const CodeUnmarkedChanges = "unmarked_changes"

// CodeStaleReview is the error code of a review finalize refused because a
// newer version was published while the review was in progress.
const CodeStaleReview = "stale_review"

// CodeBaseRequired is the error code of a review finalize that does not
// name the version the review was started from.
const CodeBaseRequired = "base_required"

// CodeNothingToReview is the error code of a review started on an artifact
// with no ready version.
const CodeNothingToReview = "nothing_to_review"

// Change kinds of an UnmarkedFile.
const (
	ChangeModified = "modified"
	ChangeAdded    = "added"
	ChangeRemoved  = "removed"
	ChangeEntry    = "entry"
)

// maxUnmarkedFiles is the most files a review rejection lists. The check
// stops reading files once it has found this many, so the response holds at
// most maxUnmarkedFiles × critic.MaxHunks hunks of bounded size.
const maxUnmarkedFiles = maxListedPath

// UnmarkedFile is one file of a rejected review. Hunks compare the base's
// baseline text with the review's clean projection, for text files only.
type UnmarkedFile struct {
	Path   string        `json:"path"`
	Change string        `json:"change"`
	Hunks  []critic.Hunk `json:"hunks,omitempty"`
}

// reviewCheck is the outcome of checking a review against its base.
type reviewCheck struct {
	files     []UnmarkedFile
	truncated bool
}

// checkReview compares review version v (its uploaded files) with base
// version baseSeq of artifact a. The caller has already established that
// it may read a, so it may read every ready version of a, including the
// base whose text the hunks quote. A read failure is an error, never a pass.
func (s *Service) checkReview(ctx context.Context, b backend, a *Artifact, v *Version, files []File, baseSeq int) (*reviewCheck, error) {
	base, err := b.store.GetVersion(ctx, a.ID, baseSeq)
	if err != nil {
		return nil, fmt.Errorf("read base version: %w", err)
	}
	if base.State != VersionStateReady {
		return nil, fmt.Errorf("base version %d is %s", baseSeq, base.State)
	}
	baseFiles, err := b.store.ListFiles(ctx, base.ID)
	if err != nil {
		return nil, fmt.Errorf("list base files: %w", err)
	}
	byPath := make(map[string]File, len(baseFiles))
	for _, f := range baseFiles {
		byPath[f.Path] = f
	}
	out := &reviewCheck{}
	add := func(u UnmarkedFile) bool {
		if len(out.files) == maxUnmarkedFiles {
			out.truncated = true
			return false
		}
		out.files = append(out.files, u)
		return true
	}
	if v.EntryPath != base.EntryPath && !add(UnmarkedFile{Path: v.EntryPath, Change: ChangeEntry}) {
		return out, nil
	}
	seen := make(map[string]bool, len(files))
	// At this point a review's manifest holds only uploaded files: the hub
	// adds its own rows after the check. Uploads may not use the reserved
	// _remote/ prefix, so no review path matches a base's remote row.
	for _, f := range files {
		seen[f.Path] = true
		bf, ok := byPath[f.Path]
		if !ok {
			if !add(UnmarkedFile{Path: f.Path, Change: ChangeAdded}) {
				return out, nil
			}
			continue
		}
		if f.SHA256 == bf.SHA256 {
			continue
		}
		if !isText(f.MediaType) || !isText(bf.MediaType) {
			if !add(UnmarkedFile{Path: f.Path, Change: ChangeModified}) {
				return out, nil
			}
			continue
		}
		baseline, err := s.readBlob(ctx, b, &bf)
		if err != nil {
			return nil, fmt.Errorf("read base file %q: %w", bf.Path, err)
		}
		if base.Kind == VersionKindReview {
			baseline = critic.CleanText(baseline)
		}
		reviewed, err := s.readBlob(ctx, b, &f)
		if err != nil {
			return nil, fmt.Errorf("read review file %q: %w", f.Path, err)
		}
		want := critic.Normalize(baseline)
		got := critic.Normalize(critic.CleanText(reviewed))
		if bytes.Equal(want, got) {
			continue
		}
		if !add(UnmarkedFile{Path: f.Path, Change: ChangeModified, Hunks: critic.Diff(want, got)}) {
			return out, nil
		}
	}
	for _, bf := range baseFiles {
		if bf.Origin != FileOriginUpload || seen[bf.Path] {
			continue
		}
		if !add(UnmarkedFile{Path: bf.Path, Change: ChangeRemoved}) {
			return out, nil
		}
	}
	return out, nil
}

// readBlob reads the whole content of a stored file. Its size is bounded by
// the file size limit it was published under.
func (s *Service) readBlob(ctx context.Context, b backend, f *File) ([]byte, error) {
	if f.SHA256 == "" {
		return nil, io.ErrUnexpectedEOF
	}
	rc, _, err := b.blobs.Download(ctx, BlobPath(b.hubID, f.SHA256))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	return readExactly(rc, f.Size)
}

// finalizeReviewCheck runs the review check inside a finalize that holds
// claim on review version v. It returns the base version to finalize
// against, or done=true when it has written the response: the review was
// rejected and discarded (422 or 409), or the check could not run (500,
// claim released so the publisher may retry).
//
// base is the version the reviewer started from. A review is checked only
// against that version, and only while it is still current: if another
// version became current since, or (checked again under the lock that
// advances the current version, see Store.FinalizeVersion) another version
// newer than base is pending, the review is discarded with 409
// stale_review, so it can neither be compared with text its reviewer never
// saw nor end up current over someone else's newer version.
func (s *Service) finalizeReviewCheck(w http.ResponseWriter, r *http.Request, b backend, a *Artifact, v *Version, files []File, claim time.Time, base int) (int, bool) {
	ctx := r.Context()
	if base != a.CurrentSeq || base >= v.Seq {
		s.discardStaleReview(w, r, b, a.ID, v.Seq, claim)
		return 0, true
	}
	check, err := s.checkReview(ctx, b, a, v, files, base)
	if err != nil {
		slog.ErrorContext(ctx, "artifacts: review check failed", "error", err)
		if rerr := b.store.ReleaseFinalize(context.WithoutCancel(ctx), a.ID, v.Seq, claim); rerr != nil {
			slog.ErrorContext(ctx, "artifacts: release finalize failed", "error", rerr)
		}
		writeError(w, http.StatusInternalServerError, "internal", "could not check the review")
		return 0, true
	}
	if len(check.files) == 0 {
		return base, false
	}
	if !s.discard(w, r, b, a.ID, v.Seq, claim) {
		return 0, true
	}
	writeJSON(w, http.StatusUnprocessableEntity, errorResponse{Error: errorBody{
		Code: CodeUnmarkedChanges,
		Message: fmt.Sprintf("the review changes %d file(s) outside CriticMarkup marks; it was discarded. "+
			"A review may only add marks", len(check.files)),
		Details: map[string]any{"files": check.files, "truncated": check.truncated, "base": base},
	}})
	return 0, true
}

// discardStaleReview discards a review overtaken by a newer version and
// answers 409.
func (s *Service) discardStaleReview(w http.ResponseWriter, r *http.Request, b backend, id string, seq int, claim time.Time) {
	if !s.discard(w, r, b, id, seq, claim) {
		return
	}
	writeError(w, http.StatusConflict, CodeStaleReview,
		"the version the review was started from is no longer the latest; the review was discarded. Review the current version")
}

// discard fails a claimed version. ok=false means the response was written.
func (s *Service) discard(w http.ResponseWriter, r *http.Request, b backend, id string, seq int, claim time.Time) bool {
	err := b.store.DiscardFinalize(context.WithoutCancel(r.Context()), id, seq, claim)
	switch {
	case errors.Is(err, ErrConflict):
		writeError(w, http.StatusConflict, "conflict", "the version is not pending or not complete")
		return false
	case err != nil:
		slog.ErrorContext(r.Context(), "artifacts: discard version failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not finalize the version")
		return false
	}
	return true
}
