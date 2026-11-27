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

package shipper

import (
	"errors"
	"fmt"
	"os"
)

// openStateLockFile fails closed on platforms where pinned dirfd state locking
// is not supported.
func openStateLockFile(_ string) (*os.File, error) {
	return nil, fmt.Errorf("open shipper lock: %w on this platform", errors.ErrUnsupported)
}

// readStateFile fails closed on platforms where pinned dirfd state reading is
// not supported.
func readStateFile(_ string) ([]byte, bool, error) {
	return nil, false, fmt.Errorf("read shipper state: %w on this platform", errors.ErrUnsupported)
}

// writeStateFile fails closed on platforms where pinned dirfd state writing is
// not supported.
func writeStateFile(_ string, _ []byte) error {
	return fmt.Errorf("write shipper state: %w on this platform", errors.ErrUnsupported)
}
