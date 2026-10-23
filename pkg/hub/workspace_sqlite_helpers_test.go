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
	"bytes"
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

func nfsConfig(mountRoot string) *config.V1WorkspaceStorageConfig {
	return &config.V1WorkspaceStorageConfig{
		Backend: "nfs",
		NFS: &config.V1NFSConfig{
			MountRoot: mountRoot,
			Shares:    []config.V1NFSShare{{ID: "share1", Server: "10.0.0.2", Export: "/scion"}},
		},
	}
}

// createTestProject creates a project for tests that need to create agents.
// It uses projectID to generate unique slug and git remote to avoid unique constraint violations.
func createTestProject(t *testing.T, s store.Store, projectID string) {
	t.Helper()
	project := &store.Project{
		ID:        projectID,
		Slug:      projectID, // Use projectID as slug to ensure uniqueness
		Name:      "Test Project " + projectID,
		GitRemote: "https://github.com/test/" + projectID, // Unique git remote per project
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	if err := s.CreateProject(context.Background(), project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}
}

// setVolumeMountBase points the platform volume mount base at dir for the
// duration of the test, so a test can create the mount root that a real
// deployment gets from Cloud Run or a Kubernetes pod spec. Tests in this
// package do not run in parallel, so mutating the package-level seam is safe.
func setVolumeMountBase(t *testing.T, dir string) {
	t.Helper()
	prev := volumeMountBase
	volumeMountBase = dir
	t.Cleanup(func() { volumeMountBase = prev })
}

// captureProjectsLog redirects the projects subsystem logger into a buffer the
// test can assert on.
func captureProjectsLog(t *testing.T, srv *Server) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := srv.projectsLog
	srv.projectsLog = slog.New(slog.NewTextHandler(buf, nil))
	t.Cleanup(func() { srv.projectsLog = prev })
	return buf
}

// newGCSContentMockStorage is a content mock storage that reports the GCS
// provider, for tests that exercise the workspace upload.
func newGCSContentMockStorage(bucket string) *contentMockStorage {
	stor := newContentMockStorage(bucket)
	stor.provider = storage.ProviderGCS
	return stor
}
