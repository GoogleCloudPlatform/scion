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
)

// isNFSLeafPermissionError is unsupported on this platform; see nfs_leaf_unix.go.
func isNFSLeafPermissionError(err error) bool {
	return false
}

// ensureNFSLeaf is unsupported on this platform; see nfs_leaf_unix.go.
func ensureNFSLeaf(hostBase, rel string) (prepared bool, err error) {
	return false, errors.ErrUnsupported
}

// ensureNFSLeafClaim always fails with errors.ErrUnsupported; see
// nfs_leaf_unix.go.
func ensureNFSLeafClaim(hostBase, rel string) (prepared, created bool, err error) {
	return false, false, fmt.Errorf("ensure NFS leaf %s: %w on this platform", rel, errors.ErrUnsupported)
}
