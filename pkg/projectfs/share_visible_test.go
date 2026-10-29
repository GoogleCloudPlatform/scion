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

package projectfs

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestShareDirVisible(t *testing.T) {
	dir := t.TempDir()
	if err := ShareDirVisible(context.Background(), dir); err != nil {
		t.Errorf("a visible directory: %v", err)
	}
	missing := filepath.Join(dir, "absent")
	if err := ShareDirVisible(context.Background(), missing); err == nil || !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a missing path: err = %v, want fs.ErrNotExist", err)
	}
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ShareDirVisible(context.Background(), file); err == nil {
		t.Error("a regular file must not count as a visible share")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// A done context may race the stat; either a result or the context error is fine, never a hang.
	_ = ShareDirVisible(ctx, dir)
}
