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

//go:build !unix

package rootopen

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// ErrRefused is returned when a path under an *os.Root is refused.
var ErrRefused = errors.New("rootopen: path refused")

// SetBeforeOpenHookForTest is a no-op on platforms without openat support.
func SetBeforeOpenHookForTest(fn func(name string)) func() {
	return func() {}
}

// OpenAt fails closed on platforms without openat(2) support.
func OpenAt(dir *os.Root, name string, flags int, perm uint32) (*os.File, error) {
	return nil, fmt.Errorf("rootopen: open %s: %w on this platform", name, errors.ErrUnsupported)
}

// OpenRegularForRead fails closed on platforms without openat(2) support.
func OpenRegularForRead(dir *os.Root, name string, expected fs.FileInfo) (*os.File, fs.FileInfo, error) {
	return nil, nil, fmt.Errorf("rootopen: open %s: %w on this platform", name, errors.ErrUnsupported)
}

// OpenFileForWrite fails closed on platforms without openat(2) support.
func OpenFileForWrite(dir *os.Root, name string, perm uint32, exists bool) (*os.File, error) {
	return nil, fmt.Errorf("rootopen: open %s for write: %w on this platform", name, errors.ErrUnsupported)
}

// OpenDir fails closed on platforms without openat(2) support.
func OpenDir(dir *os.Root, name string) (*os.File, error) {
	return nil, fmt.Errorf("rootopen: open dir %s: %w on this platform", name, errors.ErrUnsupported)
}

// MkdirAll fails closed on platforms without openat(2) support.
func MkdirAll(dir *os.Root, name string, perm uint32) (*os.File, error) {
	return nil, fmt.Errorf("rootopen: mkdir %s: %w on this platform", name, errors.ErrUnsupported)
}

// OpenFileForWriteInDir fails closed on platforms without openat(2) support.
func OpenFileForWriteInDir(dir *os.File, name string, perm uint32, exists bool) (*os.File, error) {
	return nil, fmt.Errorf("rootopen: open %s for write: %w on this platform", name, errors.ErrUnsupported)
}

// UnlinkInDir fails closed on platforms without unlinkat(2) support.
func UnlinkInDir(dir *os.File, name string) error {
	return fmt.Errorf("rootopen: unlink %s: %w on this platform", name, errors.ErrUnsupported)
}
