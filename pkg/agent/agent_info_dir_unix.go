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

package agent

import (
	"os"

	"golang.org/x/sys/unix"
)

// openAgentInfoDir opens dir read-only for a directory fsync. O_DIRECTORY
// and O_NONBLOCK keep the open from blocking on something that is not a
// directory (a FIFO, say); it fails with ENOTDIR instead.
func openAgentInfoDir(dir string) (*os.File, error) {
	return os.OpenFile(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
}
