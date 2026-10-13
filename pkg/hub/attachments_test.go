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
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
// attachmentExt / blocklist normalisation tests
// ---------------------------------------------------------------------------

// The blocklist judges the file the recipient ends up with. Windows drops
// trailing dots and spaces when it creates a file, and a zero-width space or a
// NUL is invisible wherever the name is shown, so every spelling below is the
// same file to whoever receives it and must be the same extension here.
func TestAttachmentExt_NormalisesTrailingAndInvisibleCharacters(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"a.sh", ".sh"},
		{"a.SH", ".sh"},
		{"a.sh ", ".sh"},
		{"a.sh  ", ".sh"},
		{"a.sh.", ".sh"},
		{"a.sh...", ".sh"},
		{"a.sh. . ", ".sh"},
		{"a.sh\t", ".sh"},
		{"a.sh\n", ".sh"},
		{"a.sh\x00", ".sh"},
		{"a.sh\u200b", ".sh"}, // zero-width space
		{"a.sh ", ".sh"},      // no-break space
		{"a.s\u200bh", ".sh"}, // zero-width space inside the extension
		{"a.s\x00h", ".sh"},   // NUL inside the extension
		{"a.\u202esh", ".sh"}, // right-to-left override
		{"notes.txt", ".txt"}, // the ordinary case is unchanged
		{"archive.tar.gz", ".gz"},
		{"noext", ""},
		{"...", ""},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, attachmentExt(tt.input), "attachmentExt(%q)", tt.input)
	}
}

// Every spelling the security audit landed on disk. A control a trailing space
// defeats is not a control, and the decision to keep executables out of a
// scratchpad rests on this one.
func TestSanitizeFilename_TrailingCharactersDoNotDefeatTheBlocklist(t *testing.T) {
	variants := []string{
		"a.sh ", "a.sh.", "a.sh\t", "a.sh\n", "a.sh\x00",
		"a.sh\u200b", "a.sh ", "a.SH ", "a.s\x00h", "payload.sh...",
	}
	for _, v := range variants {
		_, err := SanitizeFilename(v)
		require.Error(t, err, "SanitizeFilename(%q) was accepted", v)
		assert.Contains(t, err.Error(), "dangerous file extension", "for %q", v)
	}
}

// The blocklist should have no hole immediately beside an entry it already
// carries: the same code, the same interpreter, a different suffix.
func TestSanitizeFilename_RejectsPeersOfBlockedExtensions(t *testing.T) {
	peers := []string{
		"mod.mjs", "mod.cjs", "enc.jse", // .js
		"enc.vbe", "run.wsf", "run.wsh", // .vbs / Windows Script Host
		"module.psm1",                                            // .ps1
		"run.bash", "run.zsh", "run.ksh", "run.csh", "x.command", // .sh
		"launch.desktop", // double-click launcher
		"app.hta",        // markup that mshta runs with full trust
	}
	for _, f := range peers {
		_, err := SanitizeFilename(f)
		require.Error(t, err, "SanitizeFilename(%q) was accepted", f)
	}
}

// SanitizeFilename is the gate both entry points share, so the markup refusal
// has to be enforced here and not only in ClassifyAttachment — otherwise the
// agent --attach path, which never classifies, still accepts .html.
func TestSanitizeFilename_RefusesMarkupExtensions(t *testing.T) {
	for _, f := range []string{
		"evil.html", "evil.htm", "evil.xhtml", "evil.shtml", "evil.mhtml", "evil.mht",
		"diagram.svg", "EVIL.HTML", "evil.html ", "evil.html.",
	} {
		_, err := SanitizeFilename(f)
		require.Error(t, err, "SanitizeFilename(%q) was accepted", f)
		assert.Contains(t, err.Error(), "not accepted", "for %q", f)
	}
}

// Widening the blocklist must not swallow the developer formats #1045 exists to
// let through. These are the near neighbours of the new entries.
func TestSanitizeFilename_KeepsAcceptedDeveloperFormats(t *testing.T) {
	for _, f := range []string{"app.jsx", "app.ts", "app.tsx", "main.go", "script.py", "notes.md", "data.json"} {
		name, err := SanitizeFilename(f)
		require.NoError(t, err, "SanitizeFilename(%q)", f)
		assert.Equal(t, f, name)
	}
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
	reader, getMeta, err := store.Get(ctx, "proj-1", meta.ID)
	require.NoError(t, err)
	defer func() { _ = reader.Close() }()

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
	err = store.Delete(ctx, "proj-1", meta.ID)
	require.NoError(t, err)

	// Verify the file is gone.
	_, _, err = store.Get(ctx, "proj-1", meta.ID)
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
// Symlink protection tests (C1 fix)
// ---------------------------------------------------------------------------

func TestLocalDiskAttachmentStore_SaveRejectsExistingFile(t *testing.T) {
	dir := t.TempDir()
	store, err := NewLocalDiskAttachmentStore(dir)
	require.NoError(t, err)

	ctx := context.Background()
	content := []byte("first")

	meta, err := store.Save(ctx, "proj-1", "file.txt", bytes.NewReader(content), int64(len(content)), "text/plain")
	require.NoError(t, err)

	// Attempting to save to the same path should fail because O_EXCL is used.
	filePath := filepath.Join(dir, "proj-1", meta.ID, "file.txt")
	_, statErr := os.Stat(filePath)
	require.NoError(t, statErr, "file should exist after save")
}

func TestLocalDiskAttachmentStore_GetRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	store, err := NewLocalDiskAttachmentStore(dir)
	require.NoError(t, err)

	ctx := context.Background()
	content := []byte("original")
	meta, err := store.Save(ctx, "proj-1", "real.txt", bytes.NewReader(content), int64(len(content)), "text/plain")
	require.NoError(t, err)

	// Replace the real file with a symlink.
	realPath := filepath.Join(dir, "proj-1", meta.ID, "real.txt")
	require.NoError(t, os.Remove(realPath))
	// Create a symlink pointing somewhere else.
	require.NoError(t, os.Symlink("/etc/hostname", realPath))

	// Get should refuse to open the symlink.
	_, _, err = store.Get(ctx, "proj-1", meta.ID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a regular file")
}

// ---------------------------------------------------------------------------
// Upload endpoint handler tests
// ---------------------------------------------------------------------------

func TestUploadHandler_DisallowedMime(t *testing.T) {
	// Verify the deny-list rejects dangerous browser-executable types.
	assert.True(t, IsDangerousMimeType("application/javascript"))
	assert.True(t, IsDangerousMimeType("text/html"))
	assert.True(t, IsDangerousMimeType("text/javascript"))
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
