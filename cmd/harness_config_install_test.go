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

package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDeriveHarnessConfigName(t *testing.T) {
	tests := []struct {
		name   string
		source string
		want   string
	}{
		{
			name:   "file URL absolute",
			source: "file:///path/to/my-config",
			want:   "my-config",
		},
		{
			name:   "file URL trailing slash",
			source: "file:///path/to/my-config/",
			want:   "my-config",
		},
		{
			name:   "github URL with tree path",
			source: "https://github.com/org/repo/tree/main/harness-configs/custom-claude",
			want:   "custom-claude",
		},
		{
			name:   "github shorthand",
			source: "github.com/org/repo/tree/main/harness-configs/prod-config",
			want:   "prod-config",
		},
		{
			name:   "rclone GCS URI",
			source: ":gcs:my-bucket/harness-configs/prod-claude",
			want:   "prod-claude",
		},
		{
			name:   "rclone GCS trailing slash",
			source: ":gcs:my-bucket/harness-configs/prod-claude/",
			want:   "prod-claude",
		},
		{
			name:   "plain local path",
			source: "/home/user/configs/my-harness",
			want:   "my-harness",
		},
		{
			name:   "relative local path",
			source: "configs/my-harness",
			want:   "my-harness",
		},
		{
			name:   "https archive URL",
			source: "https://example.com/downloads/custom-harness.tgz",
			want:   "custom-harness.tgz",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := deriveHarnessConfigName(tt.source)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestNormalizeHarnessConfigSourceURL(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "already https",
			raw:  "https://github.com/org/repo/tree/main/configs",
			want: "https://github.com/org/repo/tree/main/configs",
		},
		{
			name: "bare github.com",
			raw:  "github.com/org/repo/tree/main/configs",
			want: "https://github.com/org/repo/tree/main/configs",
		},
		{
			name: "rclone prefix preserved",
			raw:  ":gcs:bucket/path",
			want: ":gcs:bucket/path",
		},
		{
			name: "file URL preserved",
			raw:  "file:///path/to/dir",
			want: "file:///path/to/dir",
		},
		{
			name: "plain local path not changed",
			raw:  "/absolute/path/to/dir",
			want: "/absolute/path/to/dir",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeHarnessConfigSourceURL(tt.raw)
			assert.Equal(t, tt.want, got)
		})
	}
}

func createTestHarnessConfig(t *testing.T, dir string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0755))
	configYAML := []byte("harness: claude\nimage: test:latest\nuser: scion\n")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), configYAML, 0644))
	homeDir := filepath.Join(dir, "home")
	require.NoError(t, os.MkdirAll(homeDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(homeDir, ".bashrc"), []byte("# test"), 0644))
}

func TestInstallLocally(t *testing.T) {
	tmpDir := t.TempDir()

	origHome := os.Getenv("HOME")
	defer os.Setenv("HOME", origHome)
	os.Setenv("HOME", tmpDir)

	srcDir := filepath.Join(tmpDir, "source", "test-hc")
	createTestHarnessConfig(t, srcDir)

	destDir := filepath.Join(tmpDir, ".scion", "harness-configs", "test-hc")

	err := installLocally("test-hc", srcDir, "", false, "claude")
	require.NoError(t, err)

	assert.DirExists(t, destDir)
	assert.FileExists(t, filepath.Join(destDir, "config.yaml"))
	assert.FileExists(t, filepath.Join(destDir, "home", ".bashrc"))

	hcDir, err := config.LoadHarnessConfigDir(destDir)
	require.NoError(t, err)
	assert.Equal(t, "claude", hcDir.Config.Harness)
	assert.Equal(t, "test:latest", hcDir.Config.Image)
}

func TestInstallLocally_Force(t *testing.T) {
	tmpDir := t.TempDir()

	origHome := os.Getenv("HOME")
	defer os.Setenv("HOME", origHome)
	os.Setenv("HOME", tmpDir)

	srcDir := filepath.Join(tmpDir, "source", "test-hc")
	createTestHarnessConfig(t, srcDir)

	err := installLocally("test-hc", srcDir, "", false, "claude")
	require.NoError(t, err)

	err = installLocally("test-hc", srcDir, "", false, "claude")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "already exists")

	err = installLocally("test-hc", srcDir, "", true, "claude")
	assert.NoError(t, err)
}

func TestInstallLocally_GroveScope(t *testing.T) {
	tmpDir := t.TempDir()

	origHome := os.Getenv("HOME")
	defer os.Setenv("HOME", origHome)
	os.Setenv("HOME", tmpDir)

	srcDir := filepath.Join(tmpDir, "source", "test-hc")
	createTestHarnessConfig(t, srcDir)

	projectPath := filepath.Join(tmpDir, "my-project", ".scion")
	require.NoError(t, os.MkdirAll(projectPath, 0755))

	err := installLocally("test-hc", srcDir, projectPath, false, "claude")
	require.NoError(t, err)

	destDir := filepath.Join(projectPath, "harness-configs", "test-hc")
	assert.DirExists(t, destDir)
	assert.FileExists(t, filepath.Join(destDir, "config.yaml"))
}

func TestResolveInstallSource_FileURL(t *testing.T) {
	tmpDir := t.TempDir()
	srcDir := filepath.Join(tmpDir, "my-config")
	createTestHarnessConfig(t, srcDir)

	localPath, cleanup, err := resolveInstallSource("file://" + srcDir)
	require.NoError(t, err)
	assert.Nil(t, cleanup)
	assert.Equal(t, srcDir, localPath)
}

func TestResolveInstallSource_LocalPath(t *testing.T) {
	tmpDir := t.TempDir()
	srcDir := filepath.Join(tmpDir, "my-config")
	createTestHarnessConfig(t, srcDir)

	localPath, cleanup, err := resolveInstallSource(srcDir)
	require.NoError(t, err)
	assert.Nil(t, cleanup)
	assert.Equal(t, srcDir, localPath)
}

func TestResolveInstallSource_NotFound(t *testing.T) {
	_, _, err := resolveInstallSource("file:///nonexistent/path")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "source not found")
}

func TestResolveInstallSource_NotADirectory(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "not-a-dir")
	require.NoError(t, os.WriteFile(filePath, []byte("hello"), 0644))

	_, _, err := resolveInstallSource("file://" + filePath)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not a directory")
}
