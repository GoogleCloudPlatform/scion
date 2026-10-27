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

package hub

import (
	"errors"
	"fmt"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/dirfd"
)

// ChownTokenFile is unsupported: the agent token file is owned inside an
// agent container, which never runs on this platform.
func ChownTokenFile(uid, gid int) error {
	return fmt.Errorf("chown token file: %w on this platform", errors.ErrUnsupported)
}

// readTokenAt is unsupported on this platform; see ChownTokenFile.
func readTokenAt(dirFd int, leaf, path string) (string, error) {
	return "", fmt.Errorf("read token file %s: %w on this platform", path, errors.ErrUnsupported)
}

// readTokenAtStat is unsupported on this platform; see ChownTokenFile.
func readTokenAtStat(dirFd int, leaf, path string) (string, dirfd.Stat, error) {
	return "", dirfd.Stat{}, fmt.Errorf("read token file %s: %w on this platform", path, errors.ErrUnsupported)
}
