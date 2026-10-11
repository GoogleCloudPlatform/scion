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
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// agentAttachmentServer wires a server with attachment storage plus a project
// whose scratchpad shared dir exists on this host, and returns that host dir.
func agentAttachmentServer(t *testing.T) (*Server, store.Store, *store.Project, string) {
	t.Helper()

	srv, s := testServer(t)

	db := openTestMemorySQLite(t, "sqlite3")

	wcs := NewWebChatStore(db, "sqlite3")
	if err := wcs.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	srv.SetWebChatStore(wcs)

	as, err := NewLocalDiskAttachmentStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalDiskAttachmentStore: %v", err)
	}
	srv.SetAttachmentStore(as)

	project := &store.Project{
		ID:         api.NewUUID(),
		Name:       "attach-project",
		Slug:       "attach-project",
		SharedDirs: []api.SharedDir{{Name: attachmentSharedDirName}},
	}
	if err := s.CreateProject(context.Background(), project); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	// sharedDirHostPath falls back to the conventional project-configs layout
	// under the home directory when no co-located broker reports a path.
	home := t.TempDir()
	t.Setenv("HOME", home)
	sharedDir := config.SharedDirHostPath(home, project.Slug, project.ID, attachmentSharedDirName)
	if err := os.MkdirAll(sharedDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	return srv, s, project, sharedDir
}

// stageAgentFile writes a file where the CLI would have staged it and returns
// the container-visible path the agent would send.
func stageAgentFile(t *testing.T, sharedDir, name, content string) string {
	t.Helper()

	dir := filepath.Join(sharedDir, ".attachments", "sender", "msg1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return "/scion-volumes/" + attachmentSharedDirName + "/.attachments/sender/msg1/" + name
}

// attachmentTestServer wires a chat-capable server with attachment storage.
func attachmentTestServer(t *testing.T) (*Server, store.Store) {
	t.Helper()

	srv, s := testServer(t)

	db := openTestMemorySQLite(t, "sqlite3")

	wcs := NewWebChatStore(db, "sqlite3")
	if err := wcs.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	srv.SetWebChatStore(wcs)

	as, err := NewLocalDiskAttachmentStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalDiskAttachmentStore: %v", err)
	}
	srv.SetAttachmentStore(as)

	return srv, s
}

// writeTempFile writes content to a new file under t.TempDir() and returns its path.
func writeTempFile(t *testing.T, name, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
	return p
}
