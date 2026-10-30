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

//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package transfer

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestDirectoryTransfersSkipSpecialFiles(t *testing.T) {
	const dirEnv = "SCION_TEST_TRANSFER_SPECIAL_DIR"
	const operationEnv = "SCION_TEST_TRANSFER_SPECIAL_OPERATION"
	const content = "hello\r\nworld\r\n"
	paths := []string{"a.txt", "nested/z.txt"}

	if dir := os.Getenv(dirEnv); dir != "" {
		switch operation := os.Getenv(operationEnv); operation {
		case "collect":
			manifest, err := NewManifestBuilder(dir).Build()
			if err != nil {
				t.Fatal(err)
			}
			if len(manifest.Files) != len(paths) {
				t.Fatalf("got %d files, want only the two regular files", len(manifest.Files))
			}
			for i, file := range manifest.Files {
				if file.Path != paths[i] || file.Hash != HashBytes([]byte(content)) || file.Size != int64(len(content)) {
					t.Errorf("unexpected manifest entry: %+v", file)
				}
			}
		case "normalize":
			if err := NormalizeDir(dir); err != nil {
				t.Fatal(err)
			}
			for _, path := range paths {
				data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(path)))
				if err != nil {
					t.Fatal(err)
				}
				if string(data) != "hello\nworld\n" {
					t.Errorf("%s was not normalized: %q", path, data)
				}
			}
		default:
			t.Fatalf("unknown helper operation: %q", operation)
		}
		return
	}

	// os.Args[0] may be relative and stop resolving after t.Chdir below.
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	for _, operation := range []string{"collect", "normalize"} {
		for _, kind := range []string{"fifo", "socket", "symlink"} {
			t.Run(operation+"/"+kind, func(t *testing.T) {
				dir := t.TempDir()
				for _, path := range paths {
					fullPath := filepath.Join(dir, filepath.FromSlash(path))
					if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(fullPath, []byte(content), 0644); err != nil {
						t.Fatal(err)
					}
				}

				specialPath := filepath.Join(dir, "special")
				switch kind {
				case "fifo":
					if err := syscall.Mkfifo(specialPath, 0600); err != nil {
						t.Fatal(err)
					}
				case "socket":
					// Keep the socket address short even when TMPDIR is long.
					t.Chdir(dir)
					listener, err := net.Listen("unix", "special")
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = listener.Close() })
				case "symlink":
					if err := os.Symlink("missing", specialPath); err != nil {
						t.Fatal(err)
					}
				}
				before, err := os.Lstat(specialPath)
				if err != nil {
					t.Fatal(err)
				}

				// Opening a FIFO without a writer blocks. Use a bounded child
				// process so a regression cannot hang or leak a goroutine.
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, testBinary, "-test.run=^TestDirectoryTransfersSkipSpecialFiles$", "-test.v")
				cmd.Env = append(os.Environ(), dirEnv+"="+dir, operationEnv+"="+operation)
				output, err := cmd.CombinedOutput()
				if ctx.Err() != nil {
					t.Fatalf("%s blocked on %s: %v\n%s", operation, kind, ctx.Err(), output)
				}
				if err != nil {
					t.Fatalf("%s failed: %v\n%s", operation, err, output)
				}

				after, err := os.Lstat(specialPath)
				if err != nil {
					t.Fatal(err)
				}
				if !os.SameFile(before, after) || before.Mode() != after.Mode() {
					t.Errorf("%s entry was replaced or changed", kind)
				}
			})
		}
	}
}
