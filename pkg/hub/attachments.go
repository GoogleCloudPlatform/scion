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
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// AttachmentStore interface
// ---------------------------------------------------------------------------

// AttachmentMeta holds metadata about a stored attachment.
type AttachmentMeta struct {
	ID         string    `json:"id"`
	ProjectID  string    `json:"projectId"`
	Filename   string    `json:"name"`
	MimeType   string    `json:"mime"`
	Size       int64     `json:"size"`
	UploadedBy string    `json:"uploadedBy"`
	CreatedAt  time.Time `json:"createdAt"`
}

// AttachmentRef is the compact form embedded in message metadata (JSON).
type AttachmentRef struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	MimeType string `json:"mime"`
	Size     int64  `json:"size"`
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

// AllowedMimeTypes maps allowed MIME types to their canonical extensions.
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
}

// attachmentExt returns the lower-cased extension that the blocklist above and
// the classifier's text-like list both judge. It is deliberately the only such
// parse: two copies of "lower-case filepath.Ext" is how two answers to the same
// question begin to disagree.
//
// The name is normalised first, because the blocklist is a claim about the file
// a recipient ends up with rather than about the exact bytes of the upload's
// filename. Windows drops trailing dots and spaces when it creates a file, so
// "payload.sh " arrives on disk as "payload.sh"; a zero-width space or a NUL
// inside the extension is invisible in every UI that will ever show the name.
// Both spellings reached the disk as an accepted text file before this. Trailing
// noise is trimmed and invisible runes are dropped so that every spelling of an
// extension is judged as the extension it will behave as.
func attachmentExt(name string) string {
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
	// Markup is refused because it would be served back as its own document
	// type; #1098 tracks re-admitting it safely.
	switch ext := attachmentExt(name); {
	case DangerousExtensions[ext]:
		return "", fmt.Errorf("dangerous file extension: %s", ext)
	case refusedMarkupExtensions[ext]:
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
}

// NewLocalDiskAttachmentStore creates a new local-disk attachment store.
// The baseDir is created if it does not exist.
func NewLocalDiskAttachmentStore(baseDir string) (*LocalDiskAttachmentStore, error) {
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return nil, fmt.Errorf("attachment store: create base dir: %w", err)
	}
	return &LocalDiskAttachmentStore{baseDir: baseDir}, nil
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

	written, err := io.Copy(f, io.LimitReader(content, MaxAttachmentSize+1))
	if err != nil {
		// Clean up on write failure.
		_ = os.Remove(filePath)
		return AttachmentMeta{}, fmt.Errorf("attachment store: write file: %w", err)
	}
	if written > MaxAttachmentSize {
		_ = os.Remove(filePath)
		return AttachmentMeta{}, fmt.Errorf("file exceeds maximum size of %d bytes", MaxAttachmentSize)
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

// Get opens the file for reading and returns its metadata.
func (s *LocalDiskAttachmentStore) Get(_ context.Context, projectID, id string) (io.ReadCloser, AttachmentMeta, error) {
	dir := filepath.Join(s.baseDir, projectID, id)
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) == 0 {
		return nil, AttachmentMeta{}, fmt.Errorf("attachment not found: %s", id)
	}

	filename := entries[0].Name()
	filePath := filepath.Join(dir, filename)

	// Use Lstat (not Stat) to detect symlinks — refuse to open anything
	// that is not a regular file, preventing symlink-based path traversal.
	info, err := os.Lstat(filePath)
	if err != nil {
		return nil, AttachmentMeta{}, fmt.Errorf("attachment stat: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, AttachmentMeta{}, fmt.Errorf("attachment not a regular file: %s", id)
	}

	f, err := os.Open(filePath)
	if err != nil {
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

// Delete removes the attachment directory and all its contents.
func (s *LocalDiskAttachmentStore) Delete(_ context.Context, projectID, id string) error {
	dir := filepath.Join(s.baseDir, projectID, id)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return nil // Already deleted — idempotent.
	}
	return os.RemoveAll(dir)
}

// FilePath returns the on-disk path for an attachment file, used when
// passing attachments to agent containers.
func (s *LocalDiskAttachmentStore) FilePath(projectID, id, filename string) string {
	return filepath.Join(s.baseDir, projectID, id, filename)
}
