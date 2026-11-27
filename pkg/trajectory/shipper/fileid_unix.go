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

package shipper

import (
	"fmt"
	"os"
	"syscall"
)

type fileID struct {
	Dev uint64
	Ino uint64
}

func (f fileID) String() string {
	return fmt.Sprintf("%d:%d", f.Dev, f.Ino)
}

// fileIdentity returns the Unix device and inode numbers for fi.
// Rotation checks compare Ino only because a Runtime Broker reading the same
// agent home over virtiofs or NFS may observe a different st_dev than the
// container.
func fileIdentity(fi os.FileInfo) (fileID, error) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st == nil {
		return fileID{}, fmt.Errorf("file identity: missing unix stat info")
	}
	return fileID{
		Dev: uint64(st.Dev),
		Ino: st.Ino,
	}, nil
}
