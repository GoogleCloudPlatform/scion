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

package metadata

import (
	"errors"
)

// readShutdownToken is unsupported on this platform; see shutdown_token_unix.go.
func readShutdownToken(path string) ([]byte, error) {
	return nil, errors.ErrUnsupported
}

// writeShutdownToken is unsupported on this platform; see shutdown_token_unix.go.
func writeShutdownToken(path, token string) error {
	return errors.ErrUnsupported
}
