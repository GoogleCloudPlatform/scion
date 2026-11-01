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

package runtimebroker

import (
	"errors"
	"fmt"
)

// renameSiblingDir always fails with errors.ErrUnsupported: the atomic
// rename over an empty directory needs renameat(2). A Runtime Broker does
// not run on this platform. It changes nothing.
func renameSiblingDir(parent, oldName, newName string) error {
	return fmt.Errorf("rename the staged workspace into place: %w on this platform", errors.ErrUnsupported)
}
