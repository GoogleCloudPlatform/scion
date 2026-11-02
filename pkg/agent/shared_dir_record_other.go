//go:build !unix

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

package agent

import (
	"errors"
	"fmt"
	"os"
)

// readSharedDirRecord fails closed off unix: the bounded read needs
// O_NOFOLLOW (see shared_dir_record_unix.go). A missing record is still
// (nil, nil), so a start with no record is unaffected; a present one is an
// error wrapping errors.ErrUnsupported.
func readSharedDirRecord(path string) ([]byte, error) {
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return nil, nil
	}
	return nil, fmt.Errorf("read shared-dir storage record %s: %w on this platform", path, errors.ErrUnsupported)
}
