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

// fileIdentity returns a stable platform file identifier ("dev:ino" on Unix)
// used to detect file replacement or rotation at the same path.
func fileIdentity(fi os.FileInfo) (string, error) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st == nil {
		return "", fmt.Errorf("file identity: missing unix stat info")
	}
	return fmt.Sprintf("%d:%d", st.Dev, st.Ino), nil
}
