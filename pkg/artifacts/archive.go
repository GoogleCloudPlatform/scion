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
	"archive/zip"
	"compress/flate"
	"context"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// The archive route (ptone/scion#7106) streams the files of one ready
// version as a zip: GET|HEAD /api/v1/artifacts/{id}/versions/{seq}/archive.
// It reads through the same access check as a file of the version, so a
// caller who may not read the artifact gets the same 404. The host turns it
// on with SetArchiveEnabled.

// Error codes of the archive route.
const (
	// CodeArchiveTooLarge: the version has more files or bytes than the
	// current publish limits allow in one archive.
	CodeArchiveTooLarge = "archive_too_large"
	// CodeArchiveBusy: too many archives are being downloaded, by the
	// caller or by everyone; Retry-After says when to try again.
	CodeArchiveBusy = "archive_busy"
	// CodeArchiveUnsupportedName: a file name of the version cannot be
	// written to an archive that extracts the same way everywhere.
	CodeArchiveUnsupportedName = "archive_unsupported_name"
)

// Reasons in ArchiveInfo when an archive is not available.
const (
	ArchiveReasonTooLarge        = "too_large"
	ArchiveReasonUnsupportedName = "unsupported_name"
)

// ArchiveInfo, on a GET of an artifact or one of its versions while the
// archive route is on, says whether the version can be downloaded as one
// zip, and why not.
type ArchiveInfo struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
}

const (
	// ArchiveMaxConcurrent caps the archives streamed at once by the
	// service, and ArchiveMaxConcurrentPerPrincipal those of one caller.
	ArchiveMaxConcurrent             = 4
	ArchiveMaxConcurrentPerPrincipal = 2
	// archiveRetryAfter is the Retry-After of a busy answer, in seconds.
	archiveRetryAfter = 10
	// archiveIdleWindow is how long a write may take: the write deadline
	// is moved this far ahead every archiveDeadlineStep bytes, so a client
	// that reads less than that much in the window is cut off.
	archiveIdleWindow   = 60 * time.Second
	archiveDeadlineStep = 1 << 20
	// The time an archive may take is archiveBaseTime plus archiveTimePerStep
	// for every archiveTimeStepBytes of files (rounded up).
	archiveBaseTime      = 2 * time.Minute
	archiveTimePerStep   = time.Minute
	archiveTimeStepBytes = 16 << 20
	// archiveMaxNameBytes caps the length of an entry name.
	archiveMaxNameBytes = 1024
	// archiveETagVersion is bumped when the archive layout changes.
	archiveETagVersion = "archive-v1"
	// archiveSlugBytes caps the title part of the download name.
	archiveSlugBytes = 64
)

// Failure classes in the archive route's log entries.
const (
	archiveFailListFiles   = "list_files"
	archiveFailEntryName   = "entry_name"
	archiveFailBlobOpen    = "blob_open"
	archiveFailBlobRead    = "blob_read"
	archiveFailSizeMatch   = "size_mismatch"
	archiveFailWrite       = "write"
	archiveFailTimeLimit   = "time_limit"
	archiveFailClientGone  = "client_gone"
	archiveFailEntryHeader = "entry_header"
)

// errArchiveTimeLimit ends an archive that ran past its time limit.
var errArchiveTimeLimit = errors.New("artifacts: archive time limit reached")

// SetArchiveEnabled sets the function that says, per request, whether the
// archive route is on. While it is unset or answers false, the route
// answers 404 and responses carry no archive field.
func (s *Service) SetArchiveEnabled(fn func(context.Context) bool) {
	s.mu.Lock()
	s.archiveEnabled = fn
	s.mu.Unlock()
}

func (s *Service) archiveOn(ctx context.Context) bool {
	s.mu.RLock()
	fn := s.archiveEnabled
	s.mu.RUnlock()
	return fn != nil && fn(ctx)
}

// archiveSlots counts the archives being streamed, in all and per
// principal.
type archiveSlots struct {
	mu           sync.Mutex
	total        int
	byPrincipal  map[string]int
	max, maxEach int
}

// acquire takes a slot for principal, or reports false when the caller or
// the service is at its cap. The returned release must be called once.
func (a *archiveSlots) acquire(principal string) (release func(), ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.total >= a.max || a.byPrincipal[principal] >= a.maxEach {
		return nil, false
	}
	a.total++
	a.byPrincipal[principal]++
	var once sync.Once
	return func() {
		once.Do(func() {
			a.mu.Lock()
			defer a.mu.Unlock()
			a.total--
			if a.byPrincipal[principal]--; a.byPrincipal[principal] <= 0 {
				delete(a.byPrincipal, principal)
			}
		})
	}, true
}

func (s *Service) archiveSlotsFor() *archiveSlots {
	s.archiveOnce.Do(func() {
		if s.archiveSlots == nil {
			s.archiveSlots = &archiveSlots{byPrincipal: map[string]int{},
				max: ArchiveMaxConcurrent, maxEach: ArchiveMaxConcurrentPerPrincipal}
		}
	})
	return s.archiveSlots
}

func (s *Service) archiveClock() time.Time {
	if s.archiveNow != nil {
		return s.archiveNow()
	}
	return time.Now()
}

// archiveFiles returns the files of a version an archive holds, sorted by
// path: the uploaded files with bytes. Remote images are left out, as on
// the Files tab.
func archiveFiles(files []File) []File {
	out := make([]File, 0, len(files))
	for _, f := range files {
		if f.Origin == FileOriginRemote || f.SHA256 == "" || f.Pending {
			continue
		}
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// archiveEntryNameOK reports whether name may be written as an entry name.
// Every version path already passed cleanFilePath; this check is narrower,
// so the archive extracts to the same plain tree on every system.
func archiveEntryNameOK(name string) bool {
	if clean, err := cleanFilePath(name); err != nil || clean != name {
		return false
	}
	if name == "" || len(name) > archiveMaxNameBytes || !utf8.ValidString(name) || name[0] == '/' {
		return false
	}
	for _, r := range name {
		if r == '\\' || r == ':' || r == 0 || unicode.IsControl(r) {
			return false
		}
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// archiveSize is the number of files and bytes of an archive.
func archiveSize(files []File) (count int, bytes int64) {
	for _, f := range files {
		bytes += f.Size
	}
	return len(files), bytes
}

// archiveRefusal is why files cannot be archived under limits: one of the
// ArchiveReason constants, or "" when they can.
func archiveRefusal(files []File, limits Limits) string {
	for _, f := range files {
		if !archiveEntryNameOK(f.Path) {
			return ArchiveReasonUnsupportedName
		}
	}
	count, bytes := archiveSize(files)
	if count > limits.MaxFiles || bytes > limits.MaxBundleBytes {
		return ArchiveReasonTooLarge
	}
	return ""
}

// archiveInfo is the archive field of a response for a version with files,
// or nil while the route is off.
func (s *Service) archiveInfo(ctx context.Context, b backend, files []File) *ArchiveInfo {
	if !s.archiveOn(ctx) {
		return nil
	}
	reason := archiveRefusal(archiveFiles(files), b.currentLimits(ctx))
	return &ArchiveInfo{Available: reason == "", Reason: reason}
}

// archiveTimeLimit is how long an archive of the given size may take.
func archiveTimeLimit(bytes int64) time.Duration {
	steps := (bytes + archiveTimeStepBytes - 1) / archiveTimeStepBytes
	return archiveBaseTime + time.Duration(steps)*archiveTimePerStep
}

// archiveStored reports whether a file of mediaType is stored in the zip
// as it is, because it is already compressed.
func archiveStored(mediaType string) bool {
	mt, _, _ := strings.Cut(mediaType, ";")
	mt = strings.ToLower(strings.TrimSpace(mt))
	switch {
	case mt == "image/svg+xml":
		return false
	case strings.HasPrefix(mt, "image/"), strings.HasPrefix(mt, "video/"), strings.HasPrefix(mt, "audio/"):
		return true
	}
	switch mt {
	case "application/zip", "application/gzip", "application/pdf", "font/woff2":
		return true
	}
	return false
}

// archiveName is the download name of version seq of a: the title as an
// ASCII slug, then -v<seq>.zip.
func archiveName(a *Artifact, seq int) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(a.Title) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			dash = false
			b.WriteRune(r)
			continue
		}
		dash = true
	}
	slug := b.String()
	if len(slug) > archiveSlugBytes {
		slug = strings.TrimRight(slug[:archiveSlugBytes], "-")
	}
	if slug == "" {
		id := a.ID
		if len(id) > 8 {
			id = id[:8]
		}
		slug = "artifact-" + id
	}
	return slug + "-v" + strconv.Itoa(seq) + ".zip"
}

// archiveWriter is the response writer of an archive. Every
// archiveDeadlineStep bytes it moves the write deadline archiveIdleWindow
// ahead, and it fails every write after the archive's time limit.
type archiveWriter struct {
	w           io.Writer
	rc          *http.ResponseController
	now         func() time.Time
	limit       time.Time
	sinceExtend int
	err         error
}

func (aw *archiveWriter) extend() {
	// A writer without deadlines (a test recorder) is left as it is.
	_ = aw.rc.SetWriteDeadline(aw.now().Add(archiveIdleWindow))
}

func (aw *archiveWriter) Write(p []byte) (int, error) {
	if aw.err != nil {
		return 0, aw.err
	}
	if aw.now().After(aw.limit) {
		aw.err = errArchiveTimeLimit
		return 0, aw.err
	}
	n, err := aw.w.Write(p)
	aw.sinceExtend += n
	if aw.sinceExtend >= archiveDeadlineStep {
		aw.sinceExtend = 0
		aw.extend()
	}
	if err != nil {
		aw.err = err
	}
	return n, err
}

// handleGetArchive implements GET|HEAD /{id}/versions/{seq}/archive.
//
// Every check runs before any byte of the archive: the route switch, the
// read check and the ready version (the same 404 as a file read), the entry
// names, the size limits (413), the ETag (304) and the concurrency caps
// (429). HEAD stops there and opens no blob. A failure after the 200 status
// aborts the connection, so the client never keeps a truncated archive.
func (s *Service) handleGetArchive(w http.ResponseWriter, r *http.Request, id string, seq int) {
	ctx := r.Context()
	if !s.archiveOn(ctx) {
		writeNotFound(w)
		return
	}
	b, a, ok := s.readableArtifact(w, r, id)
	if !ok {
		return
	}
	// readableArtifact admits only a caller with a principal, so this
	// holds; it is checked so the concurrency cap is never keyed on an
	// empty principal.
	kind, ref, _, ok := s.host.Principal(ctx)
	if !ok {
		writeNotFound(w)
		return
	}
	principal := PrincipalRef(kind, ref)
	v, ok := readyVersion(w, r, b, a, seq)
	if !ok {
		return
	}
	all, err := b.store.ListFiles(ctx, v.ID)
	if err != nil {
		archiveLogFailure(ctx, a, v, archiveFailListFiles, err)
		writeError(w, http.StatusInternalServerError, "internal", "could not read the version")
		return
	}
	files := archiveFiles(all)
	for _, f := range files {
		if !archiveEntryNameOK(f.Path) {
			archiveLogFailure(ctx, a, v, archiveFailEntryName, nil)
			slog.DebugContext(ctx, "artifacts: archive entry name refused", "artifact", a.ID, "version", v.ID, "path", f.Path)
			writeError(w, http.StatusUnprocessableEntity, CodeArchiveUnsupportedName,
				"a file name of this version cannot be written to an archive; download the files individually")
			return
		}
	}
	limits := b.currentLimits(ctx)
	count, total := archiveSize(files)
	if count > limits.MaxFiles || total > limits.MaxBundleBytes {
		writeError(w, http.StatusRequestEntityTooLarge, CodeArchiveTooLarge,
			"this version is too large for one archive; download the files individually")
		return
	}

	// The file headers go on the answers that describe the archive: 304,
	// 200 and HEAD. Refusals (413, 422, 429, and the 404s above) are plain
	// JSON errors, like every other refusal of the service.
	etag := `"` + archiveETagVersion + `:` + v.ID + `"`
	h := w.Header()
	if match := r.Header.Get("If-None-Match"); match != "" && match == etag {
		setArchiveFileHeaders(h, etag)
		w.WriteHeader(http.StatusNotModified)
		return
	}

	release, ok := s.archiveSlotsFor().acquire(principal)
	if !ok {
		h.Set("Retry-After", strconv.Itoa(archiveRetryAfter))
		writeError(w, http.StatusTooManyRequests, CodeArchiveBusy, "too many archive downloads; try again shortly")
		return
	}
	defer release()

	setZipHeaders := func() {
		setArchiveFileHeaders(h, etag)
		h.Set("Content-Type", "application/zip")
		h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": archiveName(a, v.Seq)}))
	}
	if r.Method == http.MethodHead {
		setZipHeaders()
		w.WriteHeader(http.StatusOK)
		return
	}

	limit := archiveTimeLimit(total)
	// reqCtx ends only when the client goes away (or the hub stops); ctx
	// also ends at the archive's time limit.
	reqCtx := ctx
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()

	// The first blob is opened before the status, so a storage outage is
	// a clean 500 instead of a broken download.
	var first io.ReadCloser
	if len(files) > 0 {
		first, _, err = b.blobs.Download(ctx, BlobPath(b.hubID, files[0].SHA256))
		if err != nil {
			archiveLogFailure(ctx, a, v, archiveFailBlobOpen, err)
			writeError(w, http.StatusInternalServerError, "internal", "could not read the files")
			return
		}
	}
	defer func() {
		if first != nil {
			_ = first.Close()
		}
	}()

	// The archive headers are set only now, so every answer before this
	// point is a plain JSON error.
	setZipHeaders()
	aw := &archiveWriter{w: w, rc: http.NewResponseController(w), now: s.archiveClock}
	aw.limit = aw.now().Add(limit)
	aw.extend()
	w.WriteHeader(http.StatusOK)

	zw := zip.NewWriter(aw)
	zw.RegisterCompressor(zip.Deflate, func(out io.Writer) (io.WriteCloser, error) {
		return flate.NewWriter(out, flate.BestSpeed)
	})
	buf := make([]byte, 32<<10)
	for i, f := range files {
		rc := first
		first = nil
		if i > 0 {
			rc, _, err = b.blobs.Download(ctx, BlobPath(b.hubID, f.SHA256))
			if err != nil {
				abortArchive(reqCtx, a, v, f, archiveFailBlobOpen, err)
			}
		}
		class, werr := writeArchiveEntry(zw, rc, f, v.CreatedAt, buf)
		_ = rc.Close()
		if werr != nil {
			if aw.err != nil {
				class, werr = archiveWriteClass(aw.err), aw.err
			}
			abortArchive(reqCtx, a, v, f, class, werr)
		}
	}
	if cerr := zw.Close(); cerr != nil {
		abortArchive(reqCtx, a, v, File{}, archiveWriteClass(cerr), cerr)
	}
}

// setArchiveFileHeaders sets the headers of a response that describes the
// archive itself (200, HEAD and 304).
func setArchiveFileHeaders(h http.Header, etag string) {
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "private, no-cache")
	h.Set("Content-Security-Policy", fileCSP)
	h.Set("ETag", etag)
}

// writeArchiveEntry writes file f, read from rc, as one entry of zw. It
// reads at most one byte past the manifest size, and fails when the blob
// is shorter or longer. It returns the failure class with the error.
func writeArchiveEntry(zw *zip.Writer, rc io.Reader, f File, modified time.Time, buf []byte) (string, error) {
	fh := &zip.FileHeader{Name: f.Path, Modified: modified, Method: zip.Deflate}
	if archiveStored(f.MediaType) {
		fh.Method = zip.Store
	}
	fh.SetMode(0o644)
	entry, err := zw.CreateHeader(fh)
	if err != nil {
		return archiveFailEntryHeader, err
	}
	n, err := io.CopyBuffer(entry, io.LimitReader(rc, f.Size+1), buf)
	if err != nil {
		return archiveFailBlobRead, err
	}
	if n != f.Size {
		return archiveFailSizeMatch, errBlobSize
	}
	return "", nil
}

// archiveWriteClass names the failure class of a write error.
func archiveWriteClass(err error) string {
	if errors.Is(err, errArchiveTimeLimit) {
		return archiveFailTimeLimit
	}
	return archiveFailWrite
}

// abortArchive logs a failure after the archive's status was sent and
// aborts the response, so the client sees a failed download rather than a
// short archive. The deferred releases of the handler still run.
func abortArchive(ctx context.Context, a *Artifact, v *Version, f File, class string, err error) {
	if archiveClientGone(ctx, class, err) {
		class = archiveFailClientGone
	}
	archiveLogFailure(ctx, a, v, class, err)
	if f.Path != "" {
		slog.DebugContext(ctx, "artifacts: archive aborted at file", "artifact", a.ID, "version", v.ID, "path", f.Path)
	}
	panic(http.ErrAbortHandler)
}

// archiveLogFailure logs an archive failure by artifact and version id and
// failure class. It never logs file contents or paths.
func archiveLogFailure(ctx context.Context, a *Artifact, v *Version, class string, err error) {
	attrs := []any{"artifact", a.ID, "version", v.ID, "seq", v.Seq, "class", class}
	if err != nil {
		attrs = append(attrs, "error", err)
	}
	level := slog.LevelWarn
	if class == archiveFailClientGone {
		// The client cancelled the download or its connection broke: an
		// ordinary event, not a fault of the hub.
		level = slog.LevelInfo
	}
	slog.Log(ctx, level, "artifacts: archive failed", attrs...)
}

// archiveClientGone reports whether a failure of class after the archive
// started came from the client going away. Only the write path qualifies:
//   - a write failure while the request context has ended (the client
//     disconnected; the archive's own time limit uses a separate context),
//     or on a closed, broken or reset connection (connGone, per OS);
//   - a blob open or read cancelled because the request context ended.
//
// Size mismatches, entry headers, the time limit, and blob opens and reads
// that fail for any other reason are never relabelled: they stay at Warn
// whether or not the client is still there.
func archiveClientGone(reqCtx context.Context, class string, err error) bool {
	switch class {
	case archiveFailWrite:
		return reqCtx.Err() != nil || connGone(err) || errors.Is(err, net.ErrClosed)
	case archiveFailBlobOpen, archiveFailBlobRead:
		return reqCtx.Err() != nil && errors.Is(err, context.Canceled)
	}
	return false
}
