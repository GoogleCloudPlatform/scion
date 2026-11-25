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

package bundle

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func ciWriteFile(t *testing.T, rootDir, rel, content string, perm os.FileMode) {
	t.Helper()
	full := filepath.Join(rootDir, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
	require.NoError(t, os.WriteFile(full, []byte(content), perm))
}

type ciMemSink struct {
	parts map[int][]byte
}

func (s *ciMemSink) Put(_ context.Context, index int, part io.ReadSeeker, _ int64, _ string) error {
	if s.parts == nil {
		s.parts = make(map[int][]byte)
	}
	data, err := io.ReadAll(part)
	if err != nil {
		return err
	}
	s.parts[index] = data
	return nil
}

func (s *ciMemSink) Get(_ context.Context, p Part) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(s.parts[p.Index])), nil
}

func TestHomeExcluder_CaseInsensitiveAPFS(t *testing.T) {
	srcDir := t.TempDir()

	// Simulate files created with mixed casing on a case-insensitive APFS bind
	// mount (e.g. Apple Container on macOS where ~/.Docker already existed when
	// docker wrote ~/.docker/config.json).
	ciWriteFile(t, srcDir, ".Docker/config.json", `{"auths":{"secret":"tok"}}`, 0o600)
	ciWriteFile(t, srcDir, ".Kube/Config", "kube-secret", 0o600)
	ciWriteFile(t, srcDir, ".Scion/Secrets.json", `{"API_KEY":"secret"}`, 0o600)
	ciWriteFile(t, srcDir, ".Config/GCloud/credentials.db", "gcloud-creds", 0o600)
	ciWriteFile(t, srcDir, ".Cache/pip/wheel.whl", "cached", 0o644)
	ciWriteFile(t, srcDir, ".Claude/.Credentials.JSON", "claude-secret", 0o600)
	ciWriteFile(t, srcDir, "notes.txt", "keep me", 0o644)

	root, err := os.OpenRoot(srcDir)
	require.NoError(t, err)
	defer func() { _ = root.Close() }()

	ex := HomeExcluder(HomeExcluderOptions{
		ExtraLiterals:   []string{".claude/.credentials.json"},
		AgentHomeAbs:    "/home/scion",
		CaseInsensitive: true,
	})

	est, err := Estimate(context.Background(), root, ex, DefaultHomeLimits())
	require.NoError(t, err)
	assert.Equal(t, int64(len("keep me")), est.Bytes, "only notes.txt bytes should be counted")

	sink := &ciMemSink{}
	rm, err := Write(context.Background(), root, t.TempDir(), ex, DefaultHomeLimits(), sink)
	require.NoError(t, err)
	assert.Equal(t, int64(len("keep me")), rm.Bytes)

	dstDir := t.TempDir()
	dstRoot, err := os.OpenRoot(dstDir)
	require.NoError(t, err)
	defer func() { _ = dstRoot.Close() }()
	require.NoError(t, Extract(context.Background(), dstRoot, t.TempDir(), rm, DefaultHomeLimits(), sink))

	data, err := os.ReadFile(filepath.Join(dstDir, "notes.txt"))
	require.NoError(t, err)
	assert.Equal(t, "keep me", string(data))

	for _, excluded := range []string{
		".Docker/config.json",
		".Kube/Config",
		".Scion/Secrets.json",
		".Config/GCloud/credentials.db",
		".Cache/pip/wheel.whl",
		".Claude/.Credentials.JSON",
	} {
		_, statErr := os.Lstat(filepath.Join(dstDir, filepath.FromSlash(excluded)))
		assert.True(t, os.IsNotExist(statErr), "expected %s to be excluded", excluded)
	}
}

func TestWorkspaceExcluder_CaseInsensitiveExtraLiterals(t *testing.T) {
	srcDir := t.TempDir()
	ciWriteFile(t, srcDir, ".ENV", "SECRET=123", 0o600)
	ciWriteFile(t, srcDir, ".Scion/agent-info.json", "{}", 0o644)
	ciWriteFile(t, srcDir, "main.go", "package main\n", 0o644)

	root, err := os.OpenRoot(srcDir)
	require.NoError(t, err)
	defer func() { _ = root.Close() }()

	ex := WorkspaceExcluderWithOptions(WorkspaceExcluderOptions{
		ExtraLiterals:   []string{".env"},
		WorkspaceAbs:    "/workspace",
		CaseInsensitive: true,
	})

	sink := &ciMemSink{}
	rm, err := Write(context.Background(), root, t.TempDir(), ex, DefaultWorkspaceLimits(), sink)
	require.NoError(t, err)
	assert.Equal(t, int64(len("package main\n")), rm.Bytes)

	dstDir := t.TempDir()
	dstRoot, err := os.OpenRoot(dstDir)
	require.NoError(t, err)
	defer func() { _ = dstRoot.Close() }()
	require.NoError(t, Extract(context.Background(), dstRoot, t.TempDir(), rm, DefaultWorkspaceLimits(), sink))

	assert.FileExists(t, filepath.Join(dstDir, "main.go"))
	assert.NoFileExists(t, filepath.Join(dstDir, ".ENV"))
	assert.NoFileExists(t, filepath.Join(dstDir, ".Scion", "agent-info.json"))
}
