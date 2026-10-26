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

package credentials

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/util/privatefs"
)

// readCredentialsFile reads the credentials file through privatefs, anchored
// at its directory (normally ~/.scion): the file must not be a reparse point,
// and its ACL is verified on the open handle. A file other users can read
// is REFUSED, not repaired: the user deletes
// it and logs in again, and the next save creates it with an owner-only
// ACL. Its contents are never read, so a login (Store) replaces it with a
// file holding only the new hub: logins to other hubs must be repeated,
// which the refusal says. The read paths print the refusal on stderr once
// per process, and a login over a refused file warns before replacing it
// (see notice.go). A missing file reads as fs.ErrNotExist.
func readCredentialsFile(path string) ([]byte, error) {
	tree, err := privatefs.OpenTree(filepath.Dir(path), privatefs.TreeOptions{})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tree.Close() }()
	data, err := tree.ReadPrivate(filepath.Base(path), 0)
	if errors.Is(err, privatefs.ErrNotPrivate) {
		return nil, fmt.Errorf("refusing to read credentials from %s: %w. Delete the file and log in again with 'scion hub auth login'; logins to other hubs stored in it must be repeated too", path, err)
	}
	return data, err
}
