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

package provision

import (
	"errors"
	"fmt"
	"os"
)

// readSmallFileNoFollow always fails with errors.ErrUnsupported. The unix
// version opens the file with O_NOFOLLOW and O_NONBLOCK and checks the
// opened descriptor; this platform offers neither, and its callers are
// runtime broker provisioning paths, which are not supported here. There is
// no path-based fallback.
func readSmallFileNoFollow(path string, max int64) ([]byte, error) {
	return nil, fmt.Errorf("read %s: %w on this platform", path, errors.ErrUnsupported)
}

// openDirNoFollow always fails with errors.ErrUnsupported (see
// readSmallFileNoFollow).
func openDirNoFollow(path string) (*os.File, error) {
	return nil, fmt.Errorf("open directory %s: %w on this platform", path, errors.ErrUnsupported)
}
