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

package projectfs

import (
	"errors"
	"fmt"

	"github.com/GoogleCloudPlatform/scion/pkg/shareddirs"
)

// Leaf creation is done by the hub and the brokers, which are unix-only
// (the Windows CLI gates the server off): the leaf walk opens every
// component without following symlinks (shareddirs). These always fail with
// errors.ErrUnsupported: no path-based fallback. See leaf_unix.go.

// EnsureSharedLeaf always fails with errors.ErrUnsupported on this platform.
func EnsureSharedLeaf(hostBase, rel string) (fd int, existed bool, err error) {
	return -1, false, fmt.Errorf("create shared leaf: %w on this platform", errors.ErrUnsupported)
}

// EnsureSharedLeafOwned always fails with errors.ErrUnsupported on this
// platform.
func EnsureSharedLeafOwned(hostBase, rel string, owner shareddirs.Owner) (fd int, existed bool, err error) {
	return -1, false, fmt.Errorf("create shared leaf: %w on this platform", errors.ErrUnsupported)
}

// EnsureSharedLeafExpecting always fails with errors.ErrUnsupported on this
// platform.
func EnsureSharedLeafExpecting(hostBase, rel string, expected shareddirs.Owner) (fd int, existed bool, err error) {
	return -1, false, fmt.Errorf("create shared leaf: %w on this platform", errors.ErrUnsupported)
}

// EnsureLeaf always fails with errors.ErrUnsupported on this platform.
func (r Resolved) EnsureLeaf() (existed bool, err error) {
	return false, fmt.Errorf("create directory: %w on this platform", errors.ErrUnsupported)
}

// ProjectTreeIDs always fails with errors.ErrUnsupported on this platform.
func (l Layout) ProjectTreeIDs() ([]string, error) {
	return nil, fmt.Errorf("list project trees: %w on this platform", errors.ErrUnsupported)
}
