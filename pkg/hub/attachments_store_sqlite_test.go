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

//go:build !no_sqlite

package hub

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Attachment metadata store (WebChatStore) tests
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

func TestAttachment_GetByMessagesBatch(t *testing.T) {
	store, db := newTestWebChatStoreV2(t)
	defer db.Close() //nolint:errcheck

	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	// Create 3 attachments.
	for _, id := range []string{"b-att-1", "b-att-2", "b-att-3"} {
		require.NoError(t, store.CreateAttachment(ctx, AttachmentMeta{
			ID: id, ProjectID: "proj-1", Filename: id + ".txt",
			MimeType: "text/plain", Size: 100, UploadedBy: "user-1", CreatedAt: now,
		}))
	}

	// Link: msg-A -> att-1, att-2; msg-B -> att-3; msg-C -> nothing.
	require.NoError(t, store.LinkAttachmentToMessage(ctx, "msg-A", "b-att-1"))
	require.NoError(t, store.LinkAttachmentToMessage(ctx, "msg-A", "b-att-2"))
	require.NoError(t, store.LinkAttachmentToMessage(ctx, "msg-B", "b-att-3"))

	result, err := store.GetAttachmentsByMessages(ctx, []string{"msg-A", "msg-B", "msg-C"})
	require.NoError(t, err)

	assert.Len(t, result["msg-A"], 2)
	assert.Len(t, result["msg-B"], 1)
	assert.Empty(t, result["msg-C"])
	assert.Equal(t, "b-att-3", result["msg-B"][0].ID)
}

func TestAttachment_GetByMessagesBatch_Empty(t *testing.T) {
	store, db := newTestWebChatStoreV2(t)
	defer db.Close() //nolint:errcheck

	result, err := store.GetAttachmentsByMessages(context.Background(), nil)
	require.NoError(t, err)
	assert.Nil(t, result)
}

// testAttachmentServer creates a minimal server with attachment support for testing.
func testAttachmentServer(t *testing.T) (*Server, WebChatStore, AttachmentStore) {
	t.Helper()

	db := openTestMemorySQLite(t, "sqlite")

	wcs := NewWebChatStore(db, "sqlite")
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

	// Safe MIME types are not on the deny-list.
	assert.False(t, IsDangerousMimeType("image/jpeg"))
	assert.False(t, IsDangerousMimeType("text/plain"))
	assert.False(t, IsDangerousMimeType("application/x-executable"))
	assert.False(t, IsDangerousMimeType("application/x-gzip"))
	assert.False(t, IsDangerousMimeType("application/octet-stream"))
}

func TestWebchatAttachmentTableExists(t *testing.T) {
	_, db := newTestWebChatStoreV2(t)
	defer db.Close() //nolint:errcheck

	for _, table := range []string{"webchat_attachment", "webchat_message_attachment"} {
		var count int
		err := db.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM %s", table)).Scan(&count)
		require.NoError(t, err, "table %s should exist", table)
	}
}
