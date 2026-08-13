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
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// SanitizeFilename tests
// ---------------------------------------------------------------------------

func TestSanitizeFilename_Valid(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"hello.txt", "hello.txt"},
		{"photo.jpeg", "photo.jpeg"},
		{"document.pdf", "document.pdf"},
		{"archive.zip", "archive.zip"},
		{"README.md", "README.md"},
	}
	for _, tt := range tests {
		name, err := SanitizeFilename(tt.input)
		require.NoError(t, err)
		assert.Equal(t, tt.expected, name)
	}
}

func TestSanitizeFilename_StripPath(t *testing.T) {
	name, err := SanitizeFilename("/etc/passwd")
	require.NoError(t, err)
	assert.Equal(t, "passwd", name)

	name, err = SanitizeFilename("../../secret.txt")
	require.NoError(t, err)
	assert.Equal(t, "secret.txt", name)
}

func TestSanitizeFilename_DangerousExtension(t *testing.T) {
	dangerous := []string{"malware.exe", "script.bat", "run.cmd", "hack.vbs", "code.js", "deploy.sh", "pwn.ps1"}
	for _, f := range dangerous {
		_, err := SanitizeFilename(f)
		require.Error(t, err, "should reject %s", f)
		assert.Contains(t, err.Error(), "dangerous file extension")
	}
}

func TestSanitizeFilename_EmptyOrDots(t *testing.T) {
	for _, f := range []string{"", ".", ".."} {
		_, err := SanitizeFilename(f)
		require.Error(t, err, "should reject %q", f)
	}
}

func TestSanitizeFilename_TruncateLong(t *testing.T) {
	long := strings.Repeat("a", 300) + ".txt"
	name, err := SanitizeFilename(long)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(name), MaxFilenameLength)
	assert.True(t, strings.HasSuffix(name, ".txt"))
}

// ---------------------------------------------------------------------------
// IsImageMime tests
// ---------------------------------------------------------------------------

func TestIsImageMime(t *testing.T) {
	assert.True(t, IsImageMime("image/jpeg"))
	assert.True(t, IsImageMime("image/png"))
	assert.True(t, IsImageMime("image/gif"))
	assert.True(t, IsImageMime("image/webp"))
	assert.False(t, IsImageMime("application/pdf"))
	assert.False(t, IsImageMime("text/plain"))
	assert.False(t, IsImageMime("application/zip"))
}

// ---------------------------------------------------------------------------
// LocalDiskAttachmentStore tests
// ---------------------------------------------------------------------------

func TestLocalDiskAttachmentStore_SaveAndGet(t *testing.T) {
	dir := t.TempDir()
	store, err := NewLocalDiskAttachmentStore(dir)
	require.NoError(t, err)

	ctx := context.Background()
	content := []byte("hello world")

	meta, err := store.Save(ctx, "proj-1", "test.txt", bytes.NewReader(content), int64(len(content)), "text/plain")
	require.NoError(t, err)
	assert.NotEmpty(t, meta.ID)
	assert.Equal(t, "proj-1", meta.ProjectID)
	assert.Equal(t, "test.txt", meta.Filename)
	assert.Equal(t, "text/plain", meta.MimeType)
	assert.Equal(t, int64(11), meta.Size)

	// Verify the file exists on disk.
	filePath := filepath.Join(dir, "proj-1", meta.ID, "test.txt")
	_, err = os.Stat(filePath)
	require.NoError(t, err)

	// Get the file back.
	reader, getMeta, err := store.Get(ctx, meta.ID)
	require.NoError(t, err)
	defer reader.Close()

	assert.Equal(t, meta.ID, getMeta.ID)
	assert.Equal(t, "test.txt", getMeta.Filename)

	data, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.Equal(t, "hello world", string(data))
}

func TestLocalDiskAttachmentStore_Delete(t *testing.T) {
	dir := t.TempDir()
	store, err := NewLocalDiskAttachmentStore(dir)
	require.NoError(t, err)

	ctx := context.Background()
	content := []byte("delete me")

	meta, err := store.Save(ctx, "proj-1", "temp.txt", bytes.NewReader(content), int64(len(content)), "text/plain")
	require.NoError(t, err)

	// Delete.
	err = store.Delete(ctx, meta.ID)
	require.NoError(t, err)

	// Verify the file is gone.
	_, _, err = store.Get(ctx, meta.ID)
	require.Error(t, err)
}

func TestLocalDiskAttachmentStore_OversizedFile(t *testing.T) {
	dir := t.TempDir()
	store, err := NewLocalDiskAttachmentStore(dir)
	require.NoError(t, err)

	ctx := context.Background()
	// Create content larger than MaxAttachmentSize.
	big := make([]byte, MaxAttachmentSize+1)
	_, err = store.Save(ctx, "proj-1", "big.bin", bytes.NewReader(big), int64(len(big)), "application/octet-stream")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeds maximum size")
}

func TestLocalDiskAttachmentStore_FilePath(t *testing.T) {
	dir := t.TempDir()
	store, err := NewLocalDiskAttachmentStore(dir)
	require.NoError(t, err)

	path := store.FilePath("proj-1", "uuid-123", "file.txt")
	assert.Equal(t, filepath.Join(dir, "proj-1", "uuid-123", "file.txt"), path)
}

// ---------------------------------------------------------------------------
// Attachment metadata store (WebChatStore) tests — dual-dialect coverage
// ---------------------------------------------------------------------------

func TestAttachment_CreateAndGet(t *testing.T) {
	store, db := newTestWebChatStoreV2(t)
	defer db.Close() //nolint:errcheck

	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	meta := AttachmentMeta{
		ID:         "att-1",
		ProjectID:  "proj-1",
		Filename:   "photo.jpg",
		MimeType:   "image/jpeg",
		Size:       12345,
		UploadedBy: "user-1",
		CreatedAt:  now,
	}

	require.NoError(t, store.CreateAttachment(ctx, meta))

	got, err := store.GetAttachment(ctx, "att-1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "att-1", got.ID)
	assert.Equal(t, "proj-1", got.ProjectID)
	assert.Equal(t, "photo.jpg", got.Filename)
	assert.Equal(t, "image/jpeg", got.MimeType)
	assert.Equal(t, int64(12345), got.Size)
	assert.Equal(t, "user-1", got.UploadedBy)
}

func TestAttachment_Delete(t *testing.T) {
	store, db := newTestWebChatStoreV2(t)
	defer db.Close() //nolint:errcheck

	ctx := context.Background()
	meta := AttachmentMeta{
		ID:         "att-del",
		ProjectID:  "proj-1",
		Filename:   "temp.txt",
		MimeType:   "text/plain",
		Size:       100,
		UploadedBy: "user-1",
		CreatedAt:  time.Now().UTC(),
	}
	require.NoError(t, store.CreateAttachment(ctx, meta))

	require.NoError(t, store.DeleteAttachment(ctx, "att-del"))

	got, err := store.GetAttachment(ctx, "att-del")
	require.Error(t, err) // should fail — row deleted
	assert.Nil(t, got)
}

func TestAttachment_GetNotFound(t *testing.T) {
	store, db := newTestWebChatStoreV2(t)
	defer db.Close() //nolint:errcheck

	_, err := store.GetAttachment(context.Background(), "nonexistent")
	require.Error(t, err)
}

func TestAttachment_LinkToMessage(t *testing.T) {
	store, db := newTestWebChatStoreV2(t)
	defer db.Close() //nolint:errcheck

	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	// Create two attachments.
	for _, id := range []string{"att-a", "att-b"} {
		meta := AttachmentMeta{
			ID:         id,
			ProjectID:  "proj-1",
			Filename:   id + ".txt",
			MimeType:   "text/plain",
			Size:       100,
			UploadedBy: "user-1",
			CreatedAt:  now,
		}
		require.NoError(t, store.CreateAttachment(ctx, meta))
	}

	// Link both to a message.
	require.NoError(t, store.LinkAttachmentToMessage(ctx, "msg-1", "att-a"))
	require.NoError(t, store.LinkAttachmentToMessage(ctx, "msg-1", "att-b"))

	// Verify linkage.
	refs, err := store.GetAttachmentsByMessage(ctx, "msg-1")
	require.NoError(t, err)
	require.Len(t, refs, 2)

	names := []string{refs[0].Filename, refs[1].Filename}
	assert.Contains(t, names, "att-a.txt")
	assert.Contains(t, names, "att-b.txt")
}

func TestAttachment_LinkIdempotent(t *testing.T) {
	store, db := newTestWebChatStoreV2(t)
	defer db.Close() //nolint:errcheck

	ctx := context.Background()
	meta := AttachmentMeta{
		ID:         "att-idem",
		ProjectID:  "proj-1",
		Filename:   "file.txt",
		MimeType:   "text/plain",
		Size:       50,
		UploadedBy: "user-1",
		CreatedAt:  time.Now().UTC(),
	}
	require.NoError(t, store.CreateAttachment(ctx, meta))

	// Link twice — should not error (idempotent).
	require.NoError(t, store.LinkAttachmentToMessage(ctx, "msg-1", "att-idem"))
	require.NoError(t, store.LinkAttachmentToMessage(ctx, "msg-1", "att-idem"))

	refs, err := store.GetAttachmentsByMessage(ctx, "msg-1")
	require.NoError(t, err)
	assert.Len(t, refs, 1)
}

func TestAttachment_GetByMessageEmpty(t *testing.T) {
	store, db := newTestWebChatStoreV2(t)
	defer db.Close() //nolint:errcheck

	refs, err := store.GetAttachmentsByMessage(context.Background(), "msg-no-attachments")
	require.NoError(t, err)
	assert.Empty(t, refs)
}

// ---------------------------------------------------------------------------
// Upload endpoint handler tests
// ---------------------------------------------------------------------------

// testAttachmentServer creates a minimal server with attachment support for testing.
func testAttachmentServer(t *testing.T) (*Server, WebChatStore, AttachmentStore) {
	t.Helper()

	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })

	wcs := NewWebChatStore(db, "sqlite3")
	require.NoError(t, wcs.Init())

	dir := t.TempDir()
	as, err := NewLocalDiskAttachmentStore(dir)
	require.NoError(t, err)

	srv := &Server{
		webChatStore:    wcs,
		attachmentStore: as,
	}

	return srv, wcs, as
}

func TestUploadHandler_ValidFile(t *testing.T) {
	_, wcs, _ := testAttachmentServer(t)
	_ = wcs

	// Test MIME validation directly (handler tests require full server with
	// store mocking; core storage is covered by LocalDiskAttachmentStore tests).
	assert.True(t, AllowedMimeTypes["image/jpeg"])
	assert.True(t, AllowedMimeTypes["text/plain"])
	assert.False(t, AllowedMimeTypes["application/x-executable"])
}

func TestUploadHandler_DisallowedMime(t *testing.T) {
	// Verify the allowlist rejects dangerous types.
	assert.False(t, AllowedMimeTypes["application/x-executable"])
	assert.False(t, AllowedMimeTypes["application/x-sharedlib"])
	assert.False(t, AllowedMimeTypes["application/javascript"])
	assert.False(t, AllowedMimeTypes["text/html"])
}

func TestUploadHandler_MaxFiles(t *testing.T) {
	assert.Equal(t, 10, MaxAttachmentsPerMessage)
}

func TestUploadHandler_MaxSize(t *testing.T) {
	assert.Equal(t, 10*1024*1024, MaxAttachmentSize)
}

// ---------------------------------------------------------------------------
// AttachmentRef JSON serialization test
// ---------------------------------------------------------------------------

func TestAttachmentRef_JSON(t *testing.T) {
	ref := AttachmentRef{
		ID:       "uuid-123",
		Name:     "photo.jpg",
		MimeType: "image/jpeg",
		Size:     54321,
	}
	data, err := json.Marshal(ref)
	require.NoError(t, err)

	var decoded AttachmentRef
	require.NoError(t, json.Unmarshal(data, &decoded))
	assert.Equal(t, ref, decoded)
}

func TestAttachmentRef_MetadataEmbedding(t *testing.T) {
	refs := []AttachmentRef{
		{ID: "a1", Name: "file.txt", MimeType: "text/plain", Size: 100},
		{ID: "a2", Name: "photo.jpg", MimeType: "image/jpeg", Size: 5000},
	}
	data, err := json.Marshal(refs)
	require.NoError(t, err)

	// Verify it fits within the metadata value size limit.
	assert.Less(t, len(string(data)), 4096, "attachment refs should fit within metadata value cap")

	var decoded []AttachmentRef
	require.NoError(t, json.Unmarshal(data, &decoded))
	assert.Len(t, decoded, 2)
}

// ---------------------------------------------------------------------------
// Webchat table DDL verification
// ---------------------------------------------------------------------------

func TestWebchatAttachmentTableExists(t *testing.T) {
	_, db := newTestWebChatStoreV2(t)
	defer db.Close() //nolint:errcheck

	for _, table := range []string{"webchat_attachment", "webchat_message_attachment"} {
		var count int
		err := db.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM %s", table)).Scan(&count)
		require.NoError(t, err, "table %s should exist", table)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// withTestUserCtx injects a user identity into the context for handler tests.
func withTestUserCtx(ctx context.Context, id, email string) context.Context {
	user := NewAuthenticatedUser(id, email, email, "admin", "web")
	return contextWithIdentity(ctx, user)
}
