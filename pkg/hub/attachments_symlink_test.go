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

// Symlink confinement for attachment ingest and staging.
//
// Both directions cross the same boundary: a project's scratchpad shared dir
// is bind-mounted read-write into every agent in the project, so every path
// component under it is content the hub does not control. Ingest reads a file
// an agent names there and publishes it into a chat; staging writes a file
// there for an agent to read. A link planted in between turns the first into
// an arbitrary read and the second into an arbitrary write.
//
// A leaf check is not enough for either. Lstat declines to follow only the
// final component, so a link at any directory above it is still traversed by
// the very syscall that was supposed to be refusing links. The cases below
// therefore plant the link at a directory component as well as at the leaf.
//
// The link shapes and the outside tree come from the file-browser matrix in
// project_workspace_symlink_test.go; the assertions are the same two. Nothing
// outside the shared dir may be read — checked against the bytes that reach
// the attachment store, which is where a leak would surface here rather than
// in a response body — and nothing outside it may change, checked against a
// full content snapshot.

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// agentMountPath is the container-visible path an agent would send for a file
// at rel inside the project's scratchpad shared dir.
func agentMountPath(rel string) string {
	return "/scion-volumes/" + attachmentSharedDirName + "/" + rel
}

// TestIngestAgentAttachments_SymlinkMatrix drives the real ingest path over
// every link shape. Refusal is asserted as "no ref was produced"; on the cases
// that are allowed, the stored bytes are checked, because a ref alone does not
// say which file was read.
func TestIngestAgentAttachments_SymlinkMatrix(t *testing.T) {
	cases := []struct {
		name string
		rel  string
		// want is the content ingest should publish, or "" when the path must
		// be refused outright.
		want string
	}{
		{
			name: "escaping leaf link",
			rel:  "esc_leaf",
		},
		{
			// The finding. Lstat on the leaf says "regular file" because the
			// kernel already followed esc_dir to get there.
			name: "escaping intermediate directory link",
			rel:  "esc_dir/secret.txt",
		},
		{
			name: "dangling absolute link",
			rel:  "dangling",
		},
		{
			name: "dangling relative link",
			rel:  "dangling_rel",
		},
		{
			// Refused for being a link rather than for escaping: ingest has
			// always required a regular file at the leaf, and that is left
			// alone here. Pinned so the confinement work does not quietly
			// widen what an agent can publish.
			name: "inside leaf link",
			rel:  "in_link",
		},
		{
			// A link that resolves back inside the shared dir is an ordinary
			// path component and still works.
			name: "inside intermediate directory link",
			rel:  "in_dir/inside.txt",
			want: "inside content",
		},
		{
			name: "no link at all",
			rel:  "real/inside.txt",
			want: "inside content",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, project, sharedDir := agentAttachmentServer(t)
			outside := newOutsideTree(t)
			plantSymlinks(t, sharedDir, outside)
			before := outside.snapshot(t)
			ctx := context.Background()

			refs := srv.ingestAgentAttachments(ctx, project.ID, "agent-1",
				[]string{agentMountPath(tc.rel)})

			if tc.want == "" {
				assert.Empty(t, refs, "%s must not be published as an attachment", tc.rel)
			} else if assert.Len(t, refs, 1, "%s should still be publishable", tc.rel) {
				assert.Equal(t, tc.want, readStoredAttachment(t, srv, project.ID, refs[0].ID))
			}

			// Belt and braces: whatever the handler decided, nothing it stored
			// may carry content from outside, and the outside tree is unchanged.
			for _, ref := range refs {
				assert.NotContains(t, readStoredAttachment(t, srv, project.ID, ref.ID), outsideSecret,
					"ingest published content from outside the shared dir")
			}
			outside.assertIntact(t, before)
		})
	}
}

// readStoredAttachment returns the bytes ingest actually copied into the
// attachment store, which is the channel a leak would travel down.
func readStoredAttachment(t *testing.T, srv *Server, projectID, attachmentID string) string {
	t.Helper()
	reader, _, err := srv.attachmentStore.Get(context.Background(), projectID, attachmentID)
	require.NoError(t, err)
	defer func() { _ = reader.Close() }()
	var buf bytes.Buffer
	_, err = buf.ReadFrom(reader)
	require.NoError(t, err)
	return buf.String()
}

// TestAttachmentStaging_SymlinkMatrix plants a link at each component of the
// staging destination. Staging creates its own directories, so the link is not
// something it finds in its way by accident — an agent can put one exactly
// where the next MkdirAll will land.
func TestAttachmentStaging_SymlinkMatrix(t *testing.T) {
	cases := []struct {
		name string
		// link is created inside the shared dir, pointing at the outside
		// tree's landing directory, before staging runs.
		link string
	}{
		{
			name: "link at the .attachments component",
			link: ".attachments",
		},
		{
			name: "link at the staging subdirectory component",
			link: ".attachments/_webchat",
		},
		{
			name: "link at the per-attachment directory component",
			link: ".attachments/_webchat/att-1",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sharedDir := t.TempDir()
			outside := newOutsideTree(t)
			require.NoError(t, os.MkdirAll(filepath.Join(sharedDir, filepath.Dir(tc.link)), 0o755))
			require.NoError(t, os.Symlink(filepath.Join(outside.dir, "landing"), filepath.Join(sharedDir, tc.link)))
			before := outside.snapshot(t)

			src := writeTempFile(t, "notes.txt", "hello agent")
			st := newAttachmentStaging(sharedDir, false)
			require.NotNil(t, st)

			_, err := st.stage(src, "att-1", "notes.txt")
			assert.Error(t, err, "staging through a link at %q must be refused", tc.link)
			outside.assertIntact(t, before)
		})
	}
}

// A shared dir that is itself a link is refused before any path below it is
// resolved: os.OpenRoot would follow it and anchor the root on the target.
func TestAttachmentStaging_RefusesSymlinkedSharedDir(t *testing.T) {
	outside := newOutsideTree(t)
	sharedDir := filepath.Join(t.TempDir(), "scratchpad")
	require.NoError(t, os.Symlink(outside.dir, sharedDir))
	before := outside.snapshot(t)

	src := writeTempFile(t, "notes.txt", "hello agent")
	st := newAttachmentStaging(sharedDir, false)
	require.NotNil(t, st)

	_, err := st.stage(src, "att-1", "notes.txt")
	assert.Error(t, err)
	outside.assertIntact(t, before)
}

// The same shape on the read side: ingest opens the shared dir as its root, so
// a linked shared dir drops the batch rather than reading through it.
func TestIngestAgentAttachments_RefusesSymlinkedSharedDir(t *testing.T) {
	srv, _, project, sharedDir := agentAttachmentServer(t)
	outside := newOutsideTree(t)

	// Replace the provisioned shared dir with a link to the outside tree.
	require.NoError(t, os.RemoveAll(sharedDir))
	require.NoError(t, os.Symlink(outside.dir, sharedDir))
	before := outside.snapshot(t)

	refs := srv.ingestAgentAttachments(context.Background(), project.ID, "agent-1",
		[]string{agentMountPath("secret.txt")})

	assert.Empty(t, refs)
	outside.assertIntact(t, before)
}
