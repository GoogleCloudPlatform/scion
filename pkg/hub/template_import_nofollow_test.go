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
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const importOutsideSecret = "outside-content-marker"

// TestImportTemplatesFromRemote_StoresNoLinkTargets imports a template whose
// fetched content carries absolute, relative, in-tree and chained file and
// directory links, some resolving outside the fetch, and asserts that the
// stored template holds only its regular files, never a link target's
// content, and that nothing is written outside the fetch.
func TestImportTemplatesFromRemote_StoresNoLinkTargets(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink fixtures require a POSIX file system")
	}
	t.Setenv("HOME", t.TempDir())
	outside := t.TempDir()
	secretPath := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secretPath, []byte(importOutsideSecret), 0o644); err != nil {
		t.Fatal(err)
	}

	srv, s, stor := testTemplateBootstrapServer(t)
	ctx := context.Background()
	projectID := tid("nofollow-project")
	if err := s.CreateProject(ctx, &store.Project{ID: projectID, Name: "nofollow", Slug: "nofollow"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(config.SetGitLsRemoteForTest(func(context.Context, string, string) ([]byte, error) {
		return nil, errors.New("ls-remote disabled in test")
	}))

	type entry struct {
		typ            byte
		name, linkname string
		body           string
	}
	const tpl = "repo-main/templates/my-template/"
	entries := []entry{
		{typ: tar.TypeReg, name: tpl + "scion-agent.yaml", body: "schema_version: \"1\"\n"},
		{typ: tar.TypeReg, name: tpl + "agents.md", body: "# Agents\n"},
		{typ: tar.TypeSymlink, name: tpl + "abs-file", linkname: secretPath},
		{typ: tar.TypeSymlink, name: tpl + "abs-dir", linkname: outside},
		{typ: tar.TypeSymlink, name: tpl + "rel-file", linkname: strings.Repeat("../", 32) + strings.TrimPrefix(secretPath, "/")},
		{typ: tar.TypeSymlink, name: tpl + "system-prompt.md", linkname: secretPath},
		{typ: tar.TypeLink, name: tpl + "hard", linkname: secretPath},
		// An in-tree link.
		{typ: tar.TypeSymlink, name: tpl + "inside", linkname: "agents.md"},
	}
	// A chain of relative links whose every target is lexically inside the
	// extracted tree, kept outside the imported sub-path: "deep" is a real
	// directory chain 30 levels down, and deep/.../b points back up to
	// deep's parent. "chain-file" (in the template) and "chain-dir" (at the
	// repository root) step through b, so their lexical targets stay inside
	// the tree while the resolved targets are the outside file and
	// directory. "chain-dir/extra.txt" is a regular entry below chain-dir.
	deep := "deep/"
	for i := range 30 {
		deep += fmt.Sprintf("d%d/", i)
		entries = append(entries, entry{typ: tar.TypeDir, name: "repo-main/" + deep})
	}
	up := strings.Repeat("../", 31)
	entries = append(entries,
		entry{typ: tar.TypeSymlink, name: "repo-main/" + deep + "b", linkname: strings.Repeat("../", 30)},
		entry{typ: tar.TypeSymlink, name: tpl + "chain-file", linkname: "../../" + deep + "b/" + up + strings.TrimPrefix(secretPath, "/")},
		entry{typ: tar.TypeSymlink, name: "repo-main/chain-dir", linkname: deep + "b/" + up + strings.TrimPrefix(outside, "/")},
		entry{typ: tar.TypeReg, name: "repo-main/chain-dir/extra.txt", body: "extra\n"},
	)
	// No real git: a sparse-checkout fallback is a test failure.
	t.Cleanup(config.SetGitSparseCheckoutForTest(func(context.Context, *config.GitHubURLParts, string, string) error {
		return errors.New("unexpected sparse-checkout fallback")
	}))
	old := http.DefaultClient.Transport
	t.Cleanup(func() { http.DefaultClient.Transport = old })
	http.DefaultClient.Transport = &mockRoundTripper{roundTrip: func(req *http.Request) (*http.Response, error) {
		var buf bytes.Buffer
		gzw := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gzw)
		for _, e := range entries {
			mode := int64(0o644)
			if e.typ == tar.TypeDir {
				mode = 0o755
			}
			hdr := &tar.Header{Typeflag: e.typ, Name: e.name, Linkname: e.linkname, Mode: mode, Size: int64(len(e.body))}
			if err := tw.WriteHeader(hdr); err != nil {
				return nil, err
			}
			if _, err := tw.Write([]byte(e.body)); err != nil {
				return nil, err
			}
		}
		if err := tw.Close(); err != nil {
			return nil, err
		}
		if err := gzw.Close(); err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(buf.Bytes()))}, nil
	}}

	imported, err := srv.importTemplatesFromRemote(ctx, projectID, "https://github.com/acme/repo/tree/main/templates")
	if err != nil {
		t.Fatalf("importTemplatesFromRemote: %v", err)
	}
	if len(imported) != 1 || imported[0] != "my-template" {
		t.Fatalf("imported = %v, want [my-template]", imported)
	}

	result, err := s.ListTemplates(ctx, store.TemplateFilter{ProjectID: projectID}, store.ListOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if result.TotalCount != 1 {
		t.Fatalf("expected 1 stored template, got %d", result.TotalCount)
	}
	var paths []string
	for _, f := range result.Items[0].Files {
		paths = append(paths, f.Path)
	}
	sort.Strings(paths)
	if want := []string{"agents.md", "scion-agent.yaml"}; !slices.Equal(paths, want) {
		t.Errorf("stored template files = %v, want %v", paths, want)
	}

	outsideEntries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(outsideEntries) != 1 || outsideEntries[0].Name() != "secret.txt" {
		t.Errorf("the directory outside the fetch was written to: %v", outsideEntries)
	}

	stor.mu.Lock()
	defer stor.mu.Unlock()
	if len(stor.objects) != 2 {
		t.Errorf("expected 2 stored objects, got %d", len(stor.objects))
	}
	for name, data := range stor.content {
		if strings.Contains(string(data), importOutsideSecret) {
			t.Errorf("stored object %s contains a link target's content", name)
		}
	}
}

// TestImportTemplatesFromRemote_RefusesLinkedFetchResult covers the sparse
// fallback: a fetch result that still holds a link is refused before anything
// is stored.
func TestImportTemplatesFromRemote_RefusesLinkedFetchResult(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink fixtures require a POSIX file system")
	}
	t.Setenv("HOME", t.TempDir())
	outside := t.TempDir()
	secretPath := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secretPath, []byte(importOutsideSecret), 0o644); err != nil {
		t.Fatal(err)
	}

	srv, s, stor := testTemplateBootstrapServer(t)
	ctx := context.Background()
	projectID := tid("nofollow-sparse-project")
	if err := s.CreateProject(ctx, &store.Project{ID: projectID, Name: "nofollow-sparse", Slug: "nofollow-sparse"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(config.SetGitLsRemoteForTest(func(context.Context, string, string) ([]byte, error) {
		return nil, errors.New("ls-remote disabled in test")
	}))
	old := http.DefaultClient.Transport
	t.Cleanup(func() { http.DefaultClient.Transport = old })
	http.DefaultClient.Transport = &mockRoundTripper{roundTrip: func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader(""))}, nil
	}}
	t.Cleanup(config.SetGitSparseCheckoutForTest(func(_ context.Context, _ *config.GitHubURLParts, destPath, _ string) error {
		dir := filepath.Join(destPath, "my-template")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "scion-agent.yaml"), []byte("schema_version: \"1\"\n"), 0o644); err != nil {
			return err
		}
		return os.Symlink(secretPath, filepath.Join(dir, "system-prompt.md"))
	}))

	if _, err := srv.importTemplatesFromRemote(ctx, projectID, "https://github.com/acme/repo/tree/main/templates"); err == nil {
		t.Fatal("expected the import to be refused")
	}
	result, err := s.ListTemplates(ctx, store.TemplateFilter{ProjectID: projectID}, store.ListOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if result.TotalCount != 0 {
		t.Errorf("expected no stored template, got %d", result.TotalCount)
	}
	stor.mu.Lock()
	defer stor.mu.Unlock()
	if len(stor.objects) != 0 {
		t.Errorf("expected no stored objects, got %d", len(stor.objects))
	}
}
