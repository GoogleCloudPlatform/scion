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
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

// openTestRoot opens an os.Root at dir for the duration of the test.
func openTestRoot(t *testing.T, dir string) *os.Root {
	t.Helper()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root
}

// recordingUploader records every signed-URL upload it receives.
type recordingUploader struct {
	sent map[string]string // URL -> content
}

func (u *recordingUploader) UploadFile(_ context.Context, url, _ string, _ map[string]string, content io.Reader) error {
	data, err := io.ReadAll(content)
	if err != nil {
		return err
	}
	if u.sent == nil {
		u.sent = map[string]string{}
	}
	u.sent[url] = string(data)
	return nil
}

// TestUploadCollectedFiles_MissingAndSent pins the callbacks of the shared
// upload loop: a URL with no collected file is reported and skipped, and
// each uploaded file is reported after it is sent.
func TestUploadCollectedFiles_MissingAndSent(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	files, err := transfer.CollectFiles(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	var up recordingUploader
	var missing, sent []string
	err = uploadCollectedFiles(context.Background(), &up, openTestRoot(t, dir), collectedByPath(files),
		[]hubclient.UploadURLInfo{{Path: "gone.txt", URL: "u/gone"}, {Path: "a.txt", URL: "u/a"}},
		func(p string) { missing = append(missing, p) },
		func(p string) { sent = append(sent, p) })
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 1 || missing[0] != "gone.txt" {
		t.Errorf("missing = %v, want [gone.txt]", missing)
	}
	if len(sent) != 1 || sent[0] != "a.txt" {
		t.Errorf("sent = %v, want [a.txt]", sent)
	}
	if len(up.sent) != 1 || up.sent["u/a"] != "a\n" {
		t.Errorf("uploaded %q, want only u/a = %q", up.sent, "a\n")
	}
}
