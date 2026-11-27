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
	"fmt"
	"io"
	"io/fs"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/util/rootopen"
)

// ---------------------------------------------------------------------------
// AttachmentStore interface
// ---------------------------------------------------------------------------

// AttachmentRef is the compact form embedded in message metadata (JSON).
type AttachmentRef struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	MimeType string `json:"mime"`
	Size     int64  `json:"size"`
	// artifact marks a reference to an attachment stored in the artifact
	// service (ptone/scion#4595) rather than in the AttachmentStore. It is
	// set by send validation only and never serialized.
	artifact bool
	// sha256 is the hex digest of an artifact attachment's file, set with
	// artifact by newArtifactAttachmentRef, for delivery into an agent's
	// home. Never serialized.
	sha256 string
	// projectedPath and projectedMount are the container path and shared
	// dir of an artifact attachment's projection into the project's
	// scratchpad (ptone/scion#5827), set by stageChatAttachments on
	// the send path and carried into AttachmentDelivery. Never serialized.
	projectedPath  string
	projectedMount string
}

// AttachmentStore abstracts file storage for chat attachments.
// v1 uses LocalDiskAttachmentStore; the interface allows swapping in
// object storage (GCS, S3) later without API changes.
type AttachmentStore interface {
	// Save stores file content and returns metadata including the generated ID.
	Save(ctx context.Context, projectID, filename string, content io.Reader, size int64, mime string) (AttachmentMeta, error)
	// Get returns a reader for the file content and its metadata.
	Get(ctx context.Context, projectID, id string) (io.ReadCloser, AttachmentMeta, error)
	// Delete removes the file from storage.
	Delete(ctx context.Context, projectID, id string) error
}

// ---------------------------------------------------------------------------
// Validation constants
// ---------------------------------------------------------------------------

const (
	// MaxAttachmentSize is the maximum allowed file size (10 MB).
	MaxAttachmentSize = 10 * 1024 * 1024
	// MaxAttachmentsPerMessage is the maximum number of attachments per message.
	MaxAttachmentsPerMessage = 10
	// MaxFilenameLength is the maximum length for sanitised filenames.
	MaxFilenameLength = 255
)

// AllowedMimeTypes is kept for backward compatibility with tests that
// assert specific entries. New code should call IsDangerousMimeType
// instead: the upload path now accepts everything except the deny-listed
// types below.
var AllowedMimeTypes = map[string]bool{
	// Images
	"image/jpeg": true,
	"image/png":  true,
	"image/gif":  true,
	"image/webp": true,
	// Documents
	"application/pdf": true,
	"text/plain":      true,
	"text/markdown":   true,
	// Archives
	"application/zip": true,
}

// DangerousMimeTypes lists MIME types that are rejected on upload. These
// are the types a browser will execute or render as a top-level document
// if they ever escape the download path. Everything else is accepted.
var DangerousMimeTypes = map[string]bool{
	"text/html":                true,
	"application/javascript":   true,
	"text/javascript":          true,
	"application/x-javascript": true,
	"application/ecmascript":   true,
	"text/ecmascript":          true,
}

// IsDangerousMimeType reports whether a sniffed MIME type should be
// rejected on upload.
func IsDangerousMimeType(mimeType string) bool {
	return DangerousMimeTypes[strings.ToLower(mimeType)]
}

// DangerousExtensions lists extensions that should be rejected even if
// the MIME type is spoofed. The subject is execution: a file that a recipient,
// or something running on the recipient's machine, will run by opening it.
//
// Entries are grouped with their peers on purpose. A blocklist whose holes sit
// immediately beside its entries is the worst kind: .js was blocked while .mjs
// — the same source, run by the same engines — was not, and the gap read as a
// judgement rather than as an omission.
var DangerousExtensions = map[string]bool{
	// Windows executables and installers.
	".exe": true, ".bat": true, ".cmd": true, ".com": true,
	".msi": true, ".scr": true, ".pif": true,
	// HTML Application: markup, but mshta runs it with full local trust, so it
	// belongs with the executables rather than with the markup extensions the
	// classifier refuses.
	".hta": true,
	// Script engines. .vbs and .js, their encoded forms, the ES module and
	// CommonJS spellings of the same JavaScript, and the Windows Script Host
	// files that exist to run them.
	".vbs": true, ".vbe": true, ".js": true, ".jse": true,
	".mjs": true, ".cjs": true, ".wsf": true, ".wsh": true,
	".jar": true,
	// PowerShell: the script and the module the same host loads and executes.
	".ps1": true, ".psm1": true,
	// Shell scripts under the names shells and desktops actually run, plus the
	// two launcher formats whose whole purpose is to run a command on
	// double-click.
	".sh": true, ".bash": true, ".zsh": true, ".ksh": true, ".csh": true,
	".command": true, ".desktop": true,
	// Windows shell-integration, shortcut, setup, console, and settings files
	// that trigger system actions or execute commands on open — none has a
	// legitimate chat attachment use.
	".lnk": true, ".url": true, ".website": true, ".scf": true,
	".reg": true, ".inf": true, ".cpl": true, ".msc": true,
	".settingcontent-ms": true, ".library-ms": true,
	".searchconnector-ms": true, ".search-ms": true,
	".application": true, ".appref-ms": true,
	".theme": true, ".themepack": true, ".desktopthemepackfile": true,
	".diagcab": true,
}

// attachmentExt returns the lower-cased extension after stripping any NTFS
// alternate data stream suffix (":" and everything after it).
//
// The name is normalised first, because the blocklist is a claim about the file
// a recipient ends up with rather than about the exact bytes of the upload's
// filename. Windows drops trailing dots and spaces when it creates a file, and
// treats ":" as an NTFS alternate data stream separator (so "payload.lnk::$DATA"
// or "payload.lnk:stream.txt" names "payload.lnk"); a zero-width space or a NUL
// inside the extension is invisible in every UI that will ever show the name.
// Stream suffixes and trailing noise are stripped and invisible runes are
// dropped so that every spelling of an extension is judged as the extension it
// will behave as.
//
// Blocklist refusal goes through refusedAttachmentExt (the single refusal gate
// shared by SanitizeFilename and ClassifyAttachment), while text-like MIME
// classification uses normalizeAttachmentExt on the full name.
func attachmentExt(name string) string {
	if before, _, ok := strings.Cut(name, ":"); ok {
		name = before
	}
	return normalizeAttachmentExt(name)
}

// normalizeAttachmentExt extracts and lower-cases the extension after trimming
// trailing dots and ignorable runes, without stripping ":" stream suffixes.
func normalizeAttachmentExt(name string) string {
	name = strings.TrimRightFunc(name, func(r rune) bool {
		return r == '.' || isIgnorableFilenameRune(r)
	})
	return strings.Map(func(r rune) rune {
		if isIgnorableFilenameRune(r) {
			return -1
		}
		return unicode.ToLower(r)
	}, filepath.Ext(name))
}

// refusedAttachmentExt returns the canonical blocked extension when name
// matches DangerousExtensions or refusedMarkupExtensions, checking both the
// stream-stripped name (for NTFS alternate data stream spellings) and the full
// name (for POSIX filenames containing ":" before a blocked extension).
func refusedAttachmentExt(name string) (string, bool) {
	if ext := attachmentExt(name); DangerousExtensions[ext] || refusedMarkupExtensions[ext] {
		return ext, true
	}
	if strings.Contains(name, ":") {
		if ext := normalizeAttachmentExt(name); DangerousExtensions[ext] || refusedMarkupExtensions[ext] {
			return ext, true
		}
	}
	return "", false
}

// isIgnorableFilenameRune reports whether a rune carries no visible weight in a
// filename: spaces and other whitespace, control characters including NUL, and
// the format characters — zero-width spaces, joiners, bidi overrides.
//
// Homoglyphs are a different problem and are not addressed here: "a.ѕh" with a
// Cyrillic es is a visually confusable name, not an invisible character, and
// nothing on the receiving end treats it as ".sh".
func isIgnorableFilenameRune(r rune) bool {
	return unicode.IsSpace(r) || unicode.IsControl(r) || unicode.Is(unicode.Cf, r)
}

// IsImageMime returns true if the MIME type is an image type that should
// be rendered inline.
func IsImageMime(mime string) bool {
	switch mime {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		return true
	default:
		return false
	}
}

// SanitizeFilename strips path components, limits length, and rejects
// dangerous and markup extensions.
func SanitizeFilename(name string) (string, error) {
	// Strip any directory components.
	name = filepath.Base(name)
	// Reject empty or dot-only names.
	if name == "" || name == "." || name == ".." {
		return "", fmt.Errorf("invalid filename")
	}
	// Replace any remaining path separators.
	name = strings.ReplaceAll(name, "/", "_")
	name = strings.ReplaceAll(name, "\\", "_")
	// Check for refused extensions. This is the one gate both entry points
	// share — the browser upload handler and the agent --attach path — so both
	// classes of refusal are judged here, off the same canonical extension.
	// Both lists share one refusal message. Markup is refused because it
	// would be served back as its own document type; #1098 tracks
	// re-admitting it safely.
	if ext, refused := refusedAttachmentExt(name); refused {
		return "", fmt.Errorf("files with a %s extension are not accepted", ext)
	}
	// Truncate if too long (preserve extension). This parse is about keeping a
	// recognisable suffix on a shortened name, not about judging it, so it uses
	// the extension as written.
	if len(name) > MaxFilenameLength {
		ext := strings.ToLower(filepath.Ext(name))
		base := strings.TrimSuffix(name, ext)
		maxBase := MaxFilenameLength - len(ext)
		if maxBase < 1 {
			maxBase = 1
		}
		if len(base) > maxBase {
			base = base[:maxBase]
		}
		name = base + ext
		if len(name) > MaxFilenameLength {
			name = name[:MaxFilenameLength]
		}
	}
	// Re-check the blocklist on the final stored name and the reduced delivery
	// name so truncation cannot expose a refused extension.
	for _, candidate := range []string{name, reduceAttachmentName(name)} {
		if ext, refused := refusedAttachmentExt(candidate); refused {
			return "", fmt.Errorf("files with a %s extension are not accepted", ext)
		}
	}
	return name, nil
}

// ---------------------------------------------------------------------------
// LocalDiskAttachmentStore
// ---------------------------------------------------------------------------

// LocalDiskAttachmentStore implements AttachmentStore using the local filesystem.
//
// Storage path: <baseDir>/<projectID>/<uuid>/<sanitizedName>
//
// HA limitation: local-disk storage is single-node only. An attachment
// uploaded via one hub replica is not available on another replica's disk.
// Multi-replica deployments require an object-storage implementation of
// the AttachmentStore interface; this implementation does not attempt
// shared-nothing replication.
type LocalDiskAttachmentStore struct {
	baseDir string
	// maxSize is the explicit configured per-file cap in bytes. Zero or
	// negative means MaxAttachmentSize. Save enforces this cap directly and
	// never trusts the caller-supplied size argument to raise it.
	maxSize int64
}

// NewLocalDiskAttachmentStore creates a new local-disk attachment store with
// the default MaxAttachmentSize per-file cap.
// The baseDir is created if it does not exist.
func NewLocalDiskAttachmentStore(baseDir string) (*LocalDiskAttachmentStore, error) {
	return NewLocalDiskAttachmentStoreWithMaxSize(baseDir, MaxAttachmentSize)
}

// NewLocalDiskAttachmentStoreWithMaxSize creates a new local-disk attachment
// store enforcing maxSize bytes per file (falling back to MaxAttachmentSize
// when maxSize <= 0). The baseDir is created if it does not exist.
func NewLocalDiskAttachmentStoreWithMaxSize(baseDir string, maxSize int64) (*LocalDiskAttachmentStore, error) {
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return nil, fmt.Errorf("attachment store: create base dir: %w", err)
	}
	return &LocalDiskAttachmentStore{baseDir: baseDir, maxSize: maxSize}, nil
}

func (s *LocalDiskAttachmentStore) effectiveMaxSize() int64 {
	if s.maxSize > 0 {
		return s.maxSize
	}
	return MaxAttachmentSize
}

// Save stores file content to disk and returns the metadata.
func (s *LocalDiskAttachmentStore) Save(_ context.Context, projectID, filename string, content io.Reader, size int64, mime string) (AttachmentMeta, error) {
	id := uuid.New().String()
	now := time.Now().UTC()

	// Build storage path: <baseDir>/<projectID>/<uuid>/<filename>
	dir := filepath.Join(s.baseDir, projectID, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return AttachmentMeta{}, fmt.Errorf("attachment store: create dir: %w", err)
	}

	filePath := filepath.Join(dir, filename)
	// Use O_EXCL to prevent following symlinks — fails if the name already
	// exists (including as a symlink), mitigating symlink-based path traversal.
	f, err := os.OpenFile(filePath, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
	if err != nil {
		return AttachmentMeta{}, fmt.Errorf("attachment store: create file: %w", err)
	}
	defer func() { _ = f.Close() }()

	// Enforce the store's explicit configured cap; never trust caller-supplied
	// size to raise the limit.
	limit := s.effectiveMaxSize()
	written, err := io.Copy(f, io.LimitReader(content, limit+1))
	if err != nil {
		// Clean up on write failure.
		_ = os.Remove(filePath)
		return AttachmentMeta{}, fmt.Errorf("attachment store: write file: %w", err)
	}
	if written > limit {
		_ = os.Remove(filePath)
		return AttachmentMeta{}, fmt.Errorf("file exceeds maximum size of %d bytes", limit)
	}

	return AttachmentMeta{
		ID:        id,
		ProjectID: projectID,
		Filename:  filename,
		MimeType:  mime,
		Size:      written,
		CreatedAt: now,
	}, nil
}

// storeRelDir returns the directory of attachment id of projectID relative
// to the store's base: <projectID>/<id>, or <id> for an upload with no
// project (a DM). Both must be single path components.
func storeRelDir(projectID, id string) (string, error) {
	if err := validStagingComponent(id); err != nil {
		return "", fmt.Errorf("attachment id: %w", err)
	}
	if projectID == "" {
		return id, nil
	}
	if err := validStagingComponent(projectID); err != nil {
		return "", fmt.Errorf("attachment project: %w", err)
	}
	return filepath.Join(projectID, id), nil
}

// Get opens the file for reading and returns its metadata. The file is
// opened through an os.Root anchored on the store's base, and must be a
// regular file, not a symlink (downloads serve from the
// hub-private canonical store only).
func (s *LocalDiskAttachmentStore) Get(_ context.Context, projectID, id string) (io.ReadCloser, AttachmentMeta, error) {
	rel, err := storeRelDir(projectID, id)
	if err != nil {
		return nil, AttachmentMeta{}, fmt.Errorf("attachment not found: %s", id)
	}
	root, err := os.OpenRoot(s.baseDir)
	if err != nil {
		return nil, AttachmentMeta{}, fmt.Errorf("attachment store: open base: %w", err)
	}
	defer func() { _ = root.Close() }()

	dir, err := rootopen.OpenDir(root, filepath.ToSlash(rel))
	if err != nil {
		return nil, AttachmentMeta{}, fmt.Errorf("attachment not found: %s", id)
	}
	entries, err := dir.ReadDir(-1)
	_ = dir.Close()
	if err != nil || len(entries) == 0 {
		return nil, AttachmentMeta{}, fmt.Errorf("attachment not found: %s", id)
	}

	filename := entries[0].Name()
	filePath := filepath.ToSlash(filepath.Join(rel, filename))

	f, info, err := rootopen.OpenRegularForRead(root, filePath, nil)
	if err != nil {
		if errors.Is(err, rootopen.ErrRefused) {
			return nil, AttachmentMeta{}, fmt.Errorf("attachment not a regular file: %s", id)
		}
		return nil, AttachmentMeta{}, fmt.Errorf("attachment open: %w", err)
	}

	meta := AttachmentMeta{
		ID:        id,
		ProjectID: projectID,
		Filename:  filename,
		MimeType:  mime.TypeByExtension(filepath.Ext(filename)), // best-effort from extension
		Size:      info.Size(),
		// UploadedBy not available from filesystem; callers needing it should use the DB store.
		CreatedAt: info.ModTime(),
	}

	return f, meta, nil
}

// Delete removes the attachment directory and all its contents. projectID
// and id are validated exactly as Get validates them (storeRelDir), and the
// removal goes through an os.Root anchored on the store's base, so nothing
// outside the store can be removed: an ID that is empty, "." or ".." or holds
// a path separator is refused, and a symlink in the path is not followed out
// of the base. A missing store or attachment is not an error (idempotent).
func (s *LocalDiskAttachmentStore) Delete(_ context.Context, projectID, id string) error {
	rel, err := storeRelDir(projectID, id)
	if err != nil {
		return fmt.Errorf("attachment delete refused: %s", id)
	}
	root, err := os.OpenRoot(s.baseDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil // No store yet: nothing to delete.
	}
	if err != nil {
		return fmt.Errorf("attachment store: open base: %w", err)
	}
	defer func() { _ = root.Close() }()

	if _, err := root.Lstat(rel); errors.Is(err, fs.ErrNotExist) {
		return nil // Already deleted — idempotent.
	}
	return root.RemoveAll(rel)
}

// sweep removes attachment directories older than cutoff (by their
// modification time, set when the file was saved) for which
// sweepable(projectID, id) answers true, and returns how many it removed.
// Candidates are taken structurally from the store layout, never from what a
// directory contains: <base>/<projectID>/<id> (depth 2) is a candidate for
// (projectID, id), and <base>/<id> (depth 1, an upload with no project) for
// ("", id). sweepable must confirm the candidate against the attachment's
// own record (sweepLegacyAttachmentBlobs requires the recorded project to
// equal projectID), so a project directory is never taken for an
// attachment. Every operation goes through an os.Root on the base, so
// nothing outside the store is touched.
func (s *LocalDiskAttachmentStore) sweep(cutoff time.Time, sweepable func(projectID, id string) bool) (int, error) {
	root, err := os.OpenRoot(s.baseDir)
	if err != nil {
		return 0, fmt.Errorf("attachment store: open base: %w", err)
	}
	defer func() { _ = root.Close() }()

	removed := 0
	var firstErr error
	consider := func(rel, projectID, id string) bool {
		info, err := root.Lstat(rel)
		if err != nil || !info.IsDir() || !info.ModTime().Before(cutoff) || !sweepable(projectID, id) {
			return false
		}
		if err := root.RemoveAll(rel); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			return false
		}
		removed++
		return true
	}
	top, err := readRootDir(root, ".")
	if err != nil {
		return 0, fmt.Errorf("attachment store: list base: %w", err)
	}
	for _, e := range top {
		if !e.IsDir() {
			continue
		}
		// Depth 1: <base>/<id>, an upload with no project.
		if consider(e.Name(), "", e.Name()) {
			continue
		}
		// Depth 2: <base>/<projectID>/<id>.
		children, err := readRootDir(root, e.Name())
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for _, c := range children {
			if c.IsDir() {
				consider(filepath.Join(e.Name(), c.Name()), e.Name(), c.Name())
			}
		}
	}
	return removed, firstErr
}

// FilePath returns the on-disk path for an attachment file, used when
// passing attachments to agent containers.
func (s *LocalDiskAttachmentStore) FilePath(projectID, id, filename string) string {
	return filepath.Join(s.baseDir, projectID, id, filename)
}
