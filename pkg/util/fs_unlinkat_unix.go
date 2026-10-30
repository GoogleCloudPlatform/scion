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

//go:build unix

package util

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// unlinkAt removes the entry name from directory dir with unlinkat(2) on an
// open fd of dir, so the kernel does not resolve the full path. attempted is
// false when dir could not be opened; err is the unlinkat result otherwise.
func unlinkAt(dir, name string) (attempted bool, err error) {
	dirFD, err := syscall.Open(dir, syscall.O_RDONLY, 0)
	if err != nil {
		return false, err
	}
	err = unix.Unlinkat(dirFD, name, 0)
	_ = syscall.Close(dirFD)
	return true, err
}
