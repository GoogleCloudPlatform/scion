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
	"os"
)

// The shared-directory backends that Resolved opens are served by the hub,
// which is unix-only (the Windows CLI gates the server off). These methods
// always fail with errors.ErrUnsupported: no path-based fallback. See
// open_unix.go.

// Open always fails with errors.ErrUnsupported on this platform.
func (r Resolved) Open(create bool) (*os.Root, error) {
	return nil, fmt.Errorf("open project directory: %w on this platform", errors.ErrUnsupported)
}

// ResolvedHostBase always fails with errors.ErrUnsupported on this platform.
func (r Resolved) ResolvedHostBase() (string, error) {
	return "", fmt.Errorf("resolve project host base: %w on this platform", errors.ErrUnsupported)
}

// VerifyNoSymlinks always fails with errors.ErrUnsupported on this platform.
func (r Resolved) VerifyNoSymlinks() (string, error) {
	return "", fmt.Errorf("verify project directory: %w on this platform", errors.ErrUnsupported)
}

// OpenProjectTree always fails with errors.ErrUnsupported on this platform.
func (l Layout) OpenProjectTree(projectID string) (*os.Root, string, error) {
	return nil, "", fmt.Errorf("open project tree: %w on this platform", errors.ErrUnsupported)
}

// EnsureProjectConfigDir always fails with errors.ErrUnsupported on this
// platform.
func (l Layout) EnsureProjectConfigDir(projectID string) (string, error) {
	return "", fmt.Errorf("create project configuration directory: %w on this platform", errors.ErrUnsupported)
}
