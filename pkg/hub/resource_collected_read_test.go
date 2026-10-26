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
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

// collectedReadSite uploads files, collected from the directory root is
// opened at, through one of the hub's collected-file readers, and returns
// the storage object path each file is written to.
type collectedReadSite struct {
	name   string
	upload func(ctx context.Context, stor storage.Storage, root *os.Root, files []transfer.FileInfo) error
	object func(fi transfer.FileInfo) string
}

const collectedReadBase = "hubs/h/templates/global/t"

// collectedReadSites lists every hub site that reads collected resource
// files: the blob upload of templates and harness-configs, and the
// one-object-per-file fallback.
var collectedReadSites = []collectedReadSite{
	{
		name: "uploadDirBlobs",
		upload: func(ctx context.Context, stor storage.Storage, root *os.Root, files []transfer.FileInfo) error {
			_, err := uploadDirBlobs(ctx, stor, root, nil, collectedReadBase, files, "test")
			return err
		},
		object: func(fi transfer.FileInfo) string {
			hex, _ := templateBlobHex(fi.Hash)
			return templateBlobPath(collectedReadBase, hex)
		},
	},
	{
		name: "uploadResourceFiles",
		upload: func(ctx context.Context, stor storage.Storage, root *os.Root, files []transfer.FileInfo) error {
			_, _, err := uploadResourceFiles(ctx, stor, root, collectedReadBase, files, "test")
			return err
		},
		object: func(fi transfer.FileInfo) string { return collectedReadBase + "/" + fi.Path },
	},
}

// collectForRead writes files (relative path to content) under a new
// directory, collects it and opens a root there, as the ResourceStore does.
func collectForRead(t *testing.T, files map[string]string) (string, *os.Root, []transfer.FileInfo) {
	t.Helper()
	dir := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	collected, err := transfer.CollectFiles(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return dir, root, collected
}

func newCollectedReadStorage(t *testing.T) storage.Storage {
	t.Helper()
	stor, err := storage.NewLocal(storage.Config{Provider: storage.ProviderLocal, Bucket: "b", LocalPath: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	return stor
}

func collectedEntry(t *testing.T, files []transfer.FileInfo, rel string) transfer.FileInfo {
	t.Helper()
	for _, fi := range files {
		if fi.Path == rel {
			return fi
		}
	}
	t.Fatalf("%s was not collected", rel)
	return transfer.FileInfo{}
}

// assertNotStored fails t if the object fi would be written to exists.
func assertNotStored(t *testing.T, stor storage.Storage, site collectedReadSite, fi transfer.FileInfo) {
	t.Helper()
	exists, err := stor.Exists(context.Background(), site.object(fi))
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Errorf("%s: %s was stored although its read failed", site.name, fi.Path)
	}
}

func TestCollectedReadSites_UploadUnchanged(t *testing.T) {
	for _, site := range collectedReadSites {
		t.Run(site.name, func(t *testing.T) {
			_, root, files := collectForRead(t, map[string]string{"scion-agent.yaml": "harness: claude\n", "home/.bashrc": "export A=1\n"})
			stor := newCollectedReadStorage(t)
			if err := site.upload(context.Background(), stor, root, files); err != nil {
				t.Fatalf("upload: %v", err)
			}
			for _, fi := range files {
				rc, _, err := stor.Download(context.Background(), site.object(fi))
				if err != nil {
					t.Fatalf("download %s: %v", fi.Path, err)
				}
				got, err := io.ReadAll(rc)
				_ = rc.Close()
				if err != nil {
					t.Fatalf("read %s: %v", fi.Path, err)
				}
				if transfer.HashBytes(got) != fi.Hash {
					t.Errorf("%s: stored content of %s differs from the collected entry", site.name, fi.Path)
				}
			}
		})
	}
}

func TestCollectedReadSites_ChangedAfterCollectFails(t *testing.T) {
	changes := map[string]string{
		"size":      "harness: claude\nextra: true\n",
		"same size": "harness: gemini\n",
		"truncated": "",
	}
	for _, site := range collectedReadSites {
		for change, content := range changes {
			t.Run(site.name+"/"+change, func(t *testing.T) {
				dir, root, files := collectForRead(t, map[string]string{"scion-agent.yaml": "harness: claude\n"})
				if err := os.WriteFile(filepath.Join(dir, "scion-agent.yaml"), []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
				stor := newCollectedReadStorage(t)
				err := site.upload(context.Background(), stor, root, files)
				if err == nil {
					t.Fatalf("%s: upload of a file changed after collection succeeded", site.name)
				}
				t.Logf("%s: %v", site.name, err)
				assertNotStored(t, stor, site, collectedEntry(t, files, "scion-agent.yaml"))
			})
		}
	}
}

func TestCollectedReadSites_RemovedAfterCollectFails(t *testing.T) {
	for _, site := range collectedReadSites {
		t.Run(site.name, func(t *testing.T) {
			dir, root, files := collectForRead(t, map[string]string{"scion-agent.yaml": "harness: claude\n"})
			if err := os.Remove(filepath.Join(dir, "scion-agent.yaml")); err != nil {
				t.Fatal(err)
			}
			stor := newCollectedReadStorage(t)
			if err := site.upload(context.Background(), stor, root, files); err == nil {
				t.Fatalf("%s: upload of a file removed after collection succeeded", site.name)
			}
		})
	}
}
