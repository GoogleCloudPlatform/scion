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

//go:build !linux && !darwin

package maintenance

import (
	"errors"
	"fmt"
)

// freeDiskSpaceSupported reports that freeDiskSpace always fails here, so
// SQLite snapshots are reported unsupported (SnapshotStatus).
const freeDiskSpaceSupported = false

// freeDiskSpace is not implemented on this platform, so the snapshot's
// disk check (and with it the snapshot) fails closed.
func freeDiskSpace(string) (uint64, error) {
	return 0, fmt.Errorf("reading free disk space: %w on this platform", errors.ErrUnsupported)
}
