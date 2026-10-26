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

package provision

import (
	"errors"
	"fmt"
)

// PrepareStateDir is unsupported: preparing the provisioning state
// directory needs openat and fchown, and provisioning runs on a runtime
// broker, which never runs on this platform.
func PrepareStateDir(dir string, uid, gid int, fixOwnership bool) error {
	return fmt.Errorf("prepare state dir %s: %w on this platform", dir, errors.ErrUnsupported)
}
