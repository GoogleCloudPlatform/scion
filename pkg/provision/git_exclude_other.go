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
	"os"
)

// openGitExclude always fails with errors.ErrUnsupported. The unix version
// walks hostPath/.git/info with openat and O_NOFOLLOW so a planted link
// cannot redirect the write; this platform offers neither, and provisioning
// runs on a runtime broker, which is not supported here. There is no
// path-based fallback.
func openGitExclude(hostPath string) (*os.File, error) {
	return nil, fmt.Errorf("open git exclude under %s: %w on this platform", hostPath, errors.ErrUnsupported)
}
