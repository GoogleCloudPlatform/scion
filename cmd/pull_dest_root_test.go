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
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/require"
)

// newPullDestServer serves a download manifest listing paths for the given
// resource collection (e.g. "templates") and id, and returns "content-<i>"
// for each listed file.
func newPullDestServer(t *testing.T, collection, id string, paths []string) *httptest.Server {
	t.Helper()
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/"+collection+"/"+id+"/download" && r.Method == http.MethodGet:
			files := make([]map[string]interface{}, 0, len(paths))
			for i, p := range paths {
				files = append(files, map[string]interface{}{
					"path": p,
					"url":  fmt.Sprintf("%s/blob/%d", server.URL, i),
				})
			}
			w.Header().Set("Content-Type", "application/json")
			require.NoError(t, json.NewEncoder(w).Encode(map[string]interface{}{"files": files}))
		case strings.HasPrefix(r.URL.Path, "/blob/"):
			_, _ = w.Write([]byte("content-" + strings.TrimPrefix(r.URL.Path, "/blob/")))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// pullDestLayout returns a base directory, a destination path inside it and
// an empty sibling directory next to the destination.
func pullDestLayout(t *testing.T) (base, dest, sibling string) {
	t.Helper()
	base = t.TempDir()
	dest = filepath.Join(base, "dest")
	sibling = filepath.Join(base, "sibling")
	require.NoError(t, os.MkdirAll(sibling, 0755))
	return base, dest, sibling
}

// requireOnlyDestAndSibling checks that base holds only dest and sibling,
// that sibling is empty, and that dest holds no files.
func requireOnlyDestAndSibling(t *testing.T, base, dest, sibling string) {
	t.Helper()
	entries, err := os.ReadDir(base)
	require.NoError(t, err)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	require.Equal(t, []string{"dest", "sibling"}, names)

	siblingEntries, err := os.ReadDir(sibling)
	require.NoError(t, err)
	require.Empty(t, siblingEntries)

	destEntries, err := os.ReadDir(dest)
	require.NoError(t, err)
	for _, e := range destEntries {
		require.Truef(t, e.Type()&os.ModeSymlink != 0, "unexpected entry %s in destination", e.Name())
	}
}

var nonCanonicalPullEntries = []string{
	"../sibling/file.txt",
	"a/../../sibling/file.txt",
	"../file.txt",
	"./file.txt",
	"a//file.txt",
	`a\file.txt`,
	"file\x00.txt",
	"",
}

type pullFunc func(t *testing.T, hubCtx *HubContext, dest string) error

func pullTemplateForTest(t *testing.T, hubCtx *HubContext, dest string) error {
	t.Helper()
	match := &TemplateMatch{
		Name:        "tpl",
		Location:    LocationHubGlobal,
		HubTemplate: &hubclient.Template{ID: "tpl-id", Name: "tpl"},
	}
	return pullTemplateFromHubMatch(hubCtx, match, dest)
}

func pullHarnessConfigForTest(t *testing.T, hubCtx *HubContext, dest string) error {
	t.Helper()
	hc := &hubclient.HarnessConfig{ID: "hc-id", Name: "hc", Harness: "claude"}
	return pullHarnessConfigFromHub(hubCtx, hc, dest)
}

var pullConsumers = []struct {
	name       string
	collection string
	id         string
	pull       pullFunc
}{
	{name: "template", collection: "templates", id: "tpl-id", pull: pullTemplateForTest},
	{name: "harness-config", collection: "harness-configs", id: "hc-id", pull: pullHarnessConfigForTest},
}

func newPullHubCtx(t *testing.T, server *httptest.Server) *HubContext {
	t.Helper()
	client, err := hubclient.New(server.URL)
	require.NoError(t, err)
	return &HubContext{Client: client, Endpoint: server.URL}
}

func TestPullFromHub_RejectsNonCanonicalEntry(t *testing.T) {
	for _, consumer := range pullConsumers {
		t.Run(consumer.name, func(t *testing.T) {
			for _, entry := range nonCanonicalPullEntries {
				t.Run(entry, func(t *testing.T) {
					base, dest, sibling := pullDestLayout(t)
					server := newPullDestServer(t, consumer.collection, consumer.id, []string{"ok.txt", entry})
					err := consumer.pull(t, newPullHubCtx(t, server), dest)
					require.Error(t, err)
					require.Contains(t, err.Error(), "entry 1")
					requireOnlyDestAndSibling(t, base, dest, sibling)
				})
			}
			t.Run("absolute", func(t *testing.T) {
				base, dest, sibling := pullDestLayout(t)
				abs := filepath.ToSlash(filepath.Join(sibling, "file.txt"))
				server := newPullDestServer(t, consumer.collection, consumer.id, []string{abs})
				err := consumer.pull(t, newPullHubCtx(t, server), dest)
				require.Error(t, err)
				requireOnlyDestAndSibling(t, base, dest, sibling)
			})
		})
	}
}

func TestPullFromHub_StaysInsideDestThroughSymlinkedDir(t *testing.T) {
	for _, consumer := range pullConsumers {
		t.Run(consumer.name, func(t *testing.T) {
			base, dest, sibling := pullDestLayout(t)
			require.NoError(t, os.MkdirAll(dest, 0755))
			if err := os.Symlink(sibling, filepath.Join(dest, "link")); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			server := newPullDestServer(t, consumer.collection, consumer.id, []string{"link/file.txt"})
			err := consumer.pull(t, newPullHubCtx(t, server), dest)
			require.Error(t, err)
			requireOnlyDestAndSibling(t, base, dest, sibling)
		})
	}
}

func TestPullFromHub_WritesNestedEntries(t *testing.T) {
	for _, consumer := range pullConsumers {
		t.Run(consumer.name, func(t *testing.T) {
			_, dest, sibling := pullDestLayout(t)
			paths := []string{"a/b/c.txt", "top.txt", ".claude/settings.json"}
			server := newPullDestServer(t, consumer.collection, consumer.id, paths)
			require.NoError(t, consumer.pull(t, newPullHubCtx(t, server), dest))

			for i, p := range paths {
				got, err := os.ReadFile(filepath.Join(dest, filepath.FromSlash(p)))
				require.NoError(t, err)
				require.Equal(t, fmt.Sprintf("content-%d", i), string(got))
			}
			siblingEntries, err := os.ReadDir(sibling)
			require.NoError(t, err)
			require.Empty(t, siblingEntries)
		})
	}
}
