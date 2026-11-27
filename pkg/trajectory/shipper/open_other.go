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

// openArtifactNoFollow fails closed on platforms where safe openat/O_NOFOLLOW
// artifact opening is not supported.
func openArtifactNoFollow(_, _ string) (*os.File, os.FileInfo, error) {
	return nil, nil, fmt.Errorf("open artifact safely: %w on this platform", errors.ErrUnsupported)
}
