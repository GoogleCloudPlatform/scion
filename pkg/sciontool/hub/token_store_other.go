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

package hub

import (
	"errors"
	"fmt"
)

func ensureRuntimeTokenDir(runtimePath string) error {
	return fmt.Errorf("runtime token dir %s: %w on this platform", runtimePath, errors.ErrUnsupported)
}

// TakeHandoffToken is unsupported on non-unix platforms.
func TakeHandoffToken() (string, error) {
	return "", fmt.Errorf("take handoff token: %w on this platform", errors.ErrUnsupported)
}

// RemoveStaleHandoffTmpFile is unsupported on non-unix platforms.
func RemoveStaleHandoffTmpFile() error {
	return fmt.Errorf("remove stale handoff tmp file: %w on this platform", errors.ErrUnsupported)
}

// VerifyHandoffTokenAbsent is unsupported on non-unix platforms.
func VerifyHandoffTokenAbsent() error {
	return fmt.Errorf("verify handoff token absent: %w on this platform", errors.ErrUnsupported)
}

// TakeHandoffTransportToken is unsupported on non-unix platforms.
func TakeHandoffTransportToken() (string, error) {
	return "", fmt.Errorf("take handoff transport token: %w on this platform", errors.ErrUnsupported)
}

// RemoveStaleHandoffTransportTmpFile is unsupported on non-unix platforms.
func RemoveStaleHandoffTransportTmpFile() error {
	return fmt.Errorf("remove stale handoff transport tmp file: %w on this platform", errors.ErrUnsupported)
}

// VerifyHandoffTransportTokenAbsent is unsupported on non-unix platforms.
func VerifyHandoffTransportTokenAbsent() error {
	return fmt.Errorf("verify handoff transport token absent: %w on this platform", errors.ErrUnsupported)
}
