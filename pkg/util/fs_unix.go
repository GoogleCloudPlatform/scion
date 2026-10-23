//go:build unix

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

package util

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// unlinkInDir removes name from dir through an fd on dir (unlinkat), so the
// kernel never resolves name as a path.
func unlinkInDir(dir, name string) error {
	dirFD, err := syscall.Open(dir, syscall.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer func() { _ = syscall.Close(dirFD) }()
	return unix.Unlinkat(dirFD, name, 0)
}
