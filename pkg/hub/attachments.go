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
	"os"
	"path/filepath"
	"strings"
	"time"

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
	Get(ctx context.Context, id string) (io.ReadCloser, AttachmentMeta, error)
	// Delete removes the file from storage.
	Delete(ctx context.Context, id string) error
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
// the MIME type is spoofed.
var DangerousExtensions = map[string]bool{
	".exe": true, ".bat": true, ".cmd": true, ".com": true,
	".msi": true, ".scr": true, ".pif": true, ".vbs": true,
	".js": true, ".jar": true, ".sh": true, ".ps1": true,
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
// dangerous extensions.
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
	// Check for dangerous extensions.
	ext := strings.ToLower(filepath.Ext(name))
	if DangerousExtensions[ext] {
		return "", fmt.Errorf("dangerous file extension: %s", ext)
	}
	// Truncate if too long (preserve extension).
	if len(name) > MaxFilenameLength {
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
	f, err := os.Create(filePath)
	if err != nil {
		return AttachmentMeta{}, fmt.Errorf("attachment store: create file: %w", err)
	}
	defer f.Close()

	written, err := io.Copy(f, io.LimitReader(content, MaxAttachmentSize+1))
	if err != nil {
		// Clean up on write failure.
		os.Remove(filePath)
		return AttachmentMeta{}, fmt.Errorf("attachment store: write file: %w", err)
	}
	if written > MaxAttachmentSize {
		os.Remove(filePath)
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
func (s *LocalDiskAttachmentStore) Get(_ context.Context, id string) (io.ReadCloser, AttachmentMeta, error) {
	// We need to find the file in <baseDir>/*/<id>/*
	// Since we know the structure is <baseDir>/<projectID>/<id>/<filename>,
	// we scan for the id directory.
	pattern := filepath.Join(s.baseDir, "*", id)
	matches, err := filepath.Glob(pattern)
	if err != nil || len(matches) == 0 {
		return nil, AttachmentMeta{}, fmt.Errorf("attachment not found: %s", id)
	}

	dir := matches[0]
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) == 0 {
		return nil, AttachmentMeta{}, fmt.Errorf("attachment not found: %s", id)
	}

	filename := entries[0].Name()
	filePath := filepath.Join(dir, filename)
	info, err := entries[0].Info()
	if err != nil {
		return nil, AttachmentMeta{}, fmt.Errorf("attachment stat: %w", err)
	}

	f, err := os.Open(filePath)
	if err != nil {
		return nil, AttachmentMeta{}, fmt.Errorf("attachment open: %w", err)
	}

	// Extract projectID from the path.
	projectID := filepath.Base(filepath.Dir(dir))

	meta := AttachmentMeta{
		ID:        id,
		ProjectID: projectID,
		Filename:  filename,
		Size:      info.Size(),
		CreatedAt: info.ModTime(),
	}

	return f, meta, nil
}

// Delete removes the attachment directory and all its contents.
func (s *LocalDiskAttachmentStore) Delete(_ context.Context, id string) error {
	pattern := filepath.Join(s.baseDir, "*", id)
	matches, err := filepath.Glob(pattern)
	if err != nil || len(matches) == 0 {
		return nil // Already deleted or not found — idempotent.
	}
	return os.RemoveAll(matches[0])
}

// FilePath returns the on-disk path for an attachment file, used when
// passing attachments to agent containers.
func (s *LocalDiskAttachmentStore) FilePath(projectID, id, filename string) string {
	return filepath.Join(s.baseDir, projectID, id, filename)
}
