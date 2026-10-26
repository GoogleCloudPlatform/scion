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

package brokercredentials

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/util/privatefs"
)

// readCredentialsFile reads the broker credentials file through privatefs,
// anchored at its directory (normally ~/.scion): a reparse point is
// refused, and the file's ACL is verified on the open handle. A file other
// users can read is REFUSED, not repaired, by the same rule as the CLI
// credentials: the broker HMAC secret must
// not be used from a file others can read. Writing broker credentials is
// gated on Windows (the broker commands are refused), so this read is the
// only Windows-reachable access. A missing file reads as fs.ErrNotExist.
func readCredentialsFile(path string) ([]byte, error) {
	tree, err := privatefs.OpenTree(filepath.Dir(path), privatefs.TreeOptions{})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tree.Close() }()
	data, err := tree.ReadPrivate(filepath.Base(path), 0)
	if errors.Is(err, privatefs.ErrNotPrivate) {
		return nil, fmt.Errorf("refusing to read broker credentials from %s: %w. Delete the file and register the broker again", path, err)
	}
	return data, err
}
