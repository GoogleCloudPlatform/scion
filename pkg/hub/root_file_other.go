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

package hub

import (
	"errors"
	"fmt"
	"os"
)

// writeRootFile always fails with errors.ErrUnsupported on this platform:
// it needs O_NOFOLLOW to refuse a link at the leaf (see root_file_unix.go),
// and the hub server never runs here.
func writeRootFile(root *os.Root, name, content string) error {
	return fmt.Errorf("write %s: %w on this platform", name, errors.ErrUnsupported)
}
