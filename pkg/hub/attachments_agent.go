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
	"log/slog"
	"mime"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Agents attach files by path: `scion message --attach` copies each file into
// the project's scratchpad shared dir and sends the container-visible path.
// Web chat, though, renders attachments from the W7 tables — a row in
// webchat_attachment for the file and a row in webchat_message_attachment
// linking it to a message. This file bridges the two: the paths on an agent's
// outbound message are copied into the attachment store and recorded, so an
// agent's screenshot or code file renders in the thread the same way a user's
// upload does.
//
// The linkage row needs the message ID, which does not exist until the message
// is persisted — in the outbound handler on the direct path, and in the
// broker's deliverToUser otherwise. The refs therefore travel in the message
// metadata under attachmentsMetadataKey (the same key the user-upload path
// uses), and whoever persists the message links them.

// attachmentsMetadataKey is the StructuredMessage.Metadata key carrying the
// JSON-encoded []AttachmentRef of a message's attachments.
const attachmentsMetadataKey = "attachments"

// agentAttachmentMimes maps the extensions agents commonly attach to a MIME
// type. Consulted before mime.TypeByExtension, whose answer depends on the
// host's mime.types file and gets code extensions wrong (.ts is video/mp2t
// there, which would cost the file its inline code preview).
var agentAttachmentMimes = map[string]string{
	".gif":  "image/gif",
	".jpeg": "image/jpeg",
	".jpg":  "image/jpeg",
	".png":  "image/png",
	".webp": "image/webp",

	".md":   "text/markdown",
	".txt":  "text/plain",
	".csv":  "text/csv",
	".log":  "text/plain",
	".json": "application/json",
	".toml": "application/toml",
	".yaml": "application/x-yaml",
	".yml":  "application/x-yaml",

	".go":  "text/plain",
	".py":  "text/plain",
	".rs":  "text/plain",
	".ts":  "text/plain",
	".tsx": "text/plain",
	".jsx": "text/plain",
	".sql": "text/plain",
	// Unreachable while SanitizeFilename refuses markup extensions: an .html
	// attachment never gets this far. Kept so the mapping is already right if
	// #1098 re-admits the type.
	".html": "text/html",
	".css":  "text/css",
	".xml":  "text/xml",

	".pdf": "application/pdf",
	".zip": "application/zip",
}

// attachmentMimeForName guesses a MIME type from a filename.
func attachmentMimeForName(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	if m, ok := agentAttachmentMimes[ext]; ok {
		return m
	}
	if m := mime.TypeByExtension(ext); m != "" {
		if idx := strings.Index(m, ";"); idx >= 0 {
			m = strings.TrimSpace(m[:idx])
		}
		return m
	}
	return "application/octet-stream"
}

// agentAttachmentHostPath maps a container-visible attachment path to its
// location on the hub host, given the host directory backing the project's
// scratchpad shared dir.
//
// Only paths inside that shared dir are accepted. An agent names the files it
// attaches, so anything else would let it have the hub read arbitrary files
// off its own disk and publish them into a chat.
func agentAttachmentHostPath(agentPath, sharedHostDir string) (string, bool) {
	if sharedHostDir == "" || agentPath == "" {
		return "", false
	}
	clean := path.Clean(agentPath)
	// Shared dirs mount at /scion-volumes/<name>, or under the workspace when
	// declared in-workspace. Both forms are accepted regardless of how the
	// project declares the dir: the sending CLI hardcodes the first.
	prefixes := []string{
		"/scion-volumes/" + attachmentSharedDirName + "/",
		"/workspace/.scion-volumes/" + attachmentSharedDirName + "/",
	}
	for _, prefix := range prefixes {
		rel, ok := strings.CutPrefix(clean, prefix)
		if !ok || rel == "" {
			continue
		}
		// Rejects "..", absolute paths and empty elements — the cleaned path
		// cannot escape the shared dir.
		if !filepath.IsLocal(rel) {
			return "", false
		}
		return filepath.Join(sharedHostDir, filepath.FromSlash(rel)), true
	}
	return "", false
}

// ingestAgentAttachments records the files an agent attached to an outbound
// message as chat attachments and returns their refs. Files that cannot be
// read or are not allowed are skipped with a log line rather than failing the
// message — an agent's reply is worth delivering without its attachment.
func (s *Server) ingestAgentAttachments(ctx context.Context, projectID, senderID string, paths []string) []AttachmentRef {
	if len(paths) == 0 {
		return nil
	}

	s.mu.RLock()
	wcs := s.webChatStore
	as := s.attachmentStore
	s.mu.RUnlock()
	if wcs == nil || as == nil {
		return nil
	}

	sharedHostDir, _ := s.sharedDirHostPath(ctx, projectID, attachmentSharedDirName)
	if sharedHostDir == "" {
		s.messageLog.Warn("Agent attachments dropped: no scratchpad shared dir on this host",
			"project_id", projectID, "count", len(paths))
		return nil
	}

	if len(paths) > MaxAttachmentsPerMessage {
		s.messageLog.Warn("Agent attachments truncated",
			"project_id", projectID, "count", len(paths), "max", MaxAttachmentsPerMessage)
		paths = paths[:MaxAttachmentsPerMessage]
	}

	refs := make([]AttachmentRef, 0, len(paths))
	for _, p := range paths {
		ref, err := s.storeAgentAttachment(ctx, wcs, as, projectID, senderID, sharedHostDir, p)
		if err != nil {
			s.messageLog.Warn("Failed to record agent attachment",
				"project_id", projectID, "path", p, "error", err)
			continue
		}
		refs = append(refs, ref)
	}
	return refs
}

// storeAgentAttachment copies one agent-attached file into the attachment
// store and writes its metadata row.
func (s *Server) storeAgentAttachment(ctx context.Context, wcs WebChatStore, as AttachmentStore,
	projectID, senderID, sharedHostDir, agentPath string) (AttachmentRef, error) {

	hostPath, ok := agentAttachmentHostPath(agentPath, sharedHostDir)
	if !ok {
		return AttachmentRef{}, errAttachmentOutsideSharedDir
	}

	// Lstat, not Stat: an agent writes into the same shared dir, so a symlink
	// planted there would otherwise hand it any file the hub can read.
	info, err := os.Lstat(hostPath)
	if err != nil {
		return AttachmentRef{}, err
	}
	if !info.Mode().IsRegular() {
		return AttachmentRef{}, errAttachmentNotRegular
	}
	if info.Size() > MaxAttachmentSize {
		return AttachmentRef{}, errAttachmentTooLarge
	}

	name, err := SanitizeFilename(filepath.Base(hostPath))
	if err != nil {
		return AttachmentRef{}, err
	}

	f, err := os.Open(hostPath)
	if err != nil {
		return AttachmentRef{}, err
	}
	defer func() { _ = f.Close() }()
	// Re-check through the open handle: Lstat and Open are separate syscalls.
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		return AttachmentRef{}, errAttachmentNotRegular
	}

	mimeType := attachmentMimeForName(name)
	meta, err := as.Save(ctx, projectID, name, f, info.Size(), mimeType)
	if err != nil {
		return AttachmentRef{}, err
	}
	meta.UploadedBy = senderID

	if err := wcs.CreateAttachment(ctx, meta); err != nil {
		// Leave no orphaned bytes behind when the metadata row fails.
		if delErr := as.Delete(ctx, projectID, meta.ID); delErr != nil {
			// The blob is orphaned after all. Say so: nothing else will.
			s.messageLog.Error("Failed to delete orphaned attachment blob",
				"project_id", projectID, "attachment", meta.ID, "error", delErr)
		}
		return AttachmentRef{}, err
	}

	return AttachmentRef{
		ID:       meta.ID,
		Name:     meta.Filename,
		MimeType: meta.MimeType,
		Size:     meta.Size,
	}, nil
}

// attachmentRefsMetadata encodes refs for transport in message metadata,
// returning false when there is nothing to carry.
func attachmentRefsMetadata(refs []AttachmentRef) (string, bool) {
	if len(refs) == 0 {
		return "", false
	}
	encoded, err := json.Marshal(refs)
	if err != nil {
		return "", false
	}
	return string(encoded), true
}

// parseAttachmentRefs decodes the refs a message carries in its metadata.
func parseAttachmentRefs(metadata map[string]string) []AttachmentRef {
	raw := metadata[attachmentsMetadataKey]
	if raw == "" {
		return nil
	}
	var refs []AttachmentRef
	if err := json.Unmarshal([]byte(raw), &refs); err != nil {
		return nil
	}
	return refs
}

// linkAttachmentRefs attaches recorded files to a persisted message. Failures
// are logged, not returned: the message itself is already delivered.
func linkAttachmentRefs(ctx context.Context, wcs WebChatStore, messageID string, refs []AttachmentRef, log *slog.Logger) {
	if wcs == nil || messageID == "" {
		return
	}
	for _, ref := range refs {
		if ref.ID == "" {
			continue
		}
		if err := wcs.LinkAttachmentToMessage(ctx, messageID, ref.ID); err != nil && log != nil {
			log.Error("Failed to link attachment to message",
				"message_id", messageID, "attachment", ref.ID, "error", err)
		}
	}
}

// Sentinel errors for skipped attachments, reported in the ingest log line.
type attachmentSkipError string

func (e attachmentSkipError) Error() string { return string(e) }

const (
	errAttachmentOutsideSharedDir attachmentSkipError = "path is outside the project's scratchpad shared dir"
	errAttachmentNotRegular       attachmentSkipError = "not a regular file"
	errAttachmentTooLarge         attachmentSkipError = "file exceeds the maximum attachment size"
)
