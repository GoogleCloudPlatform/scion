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

//go:build linux

package transportauth

import (
	"os"
	"syscall"
	"time"
)

// fileStatExtra returns the change time and link count from an Lstat
// result. ok is false when they are not available, in which case
// FileSource never reuses an earlier read.
func fileStatExtra(fi os.FileInfo) (ctime time.Time, nlink uint64, ok bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return time.Time{}, 0, false
	}
	return time.Unix(st.Ctim.Unix()), uint64(st.Nlink), true
}
