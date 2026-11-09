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

package cmd

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// errPullNameNotLocal is returned by openPullDestination when a pull without
// --to would build its destination from a Hub name that is not exactly one
// path component. The text never contains the name.
var errPullNameNotLocal = errors.New("the Hub name is not a single path component, so it cannot name a local directory; pass --to to choose the destination")

// openPullDestination opens the directory a pull writes into and returns it
// as an os.Root with its path.
//
// With toPath set, that path (made absolute) is created and opened, as the
// user chose it. Otherwise the destination is name below parentDir (the
// global harness-configs or the project templates directory): name must be
// one path component (config.CheckResourceName), or errPullNameNotLocal is
// returned before anything is created. parentDir is created and opened
// first, and name is then created and opened through that root, so the
// destination can never be outside parentDir.
func openPullDestination(toPath, parentDir, name string) (*os.Root, string, error) {
	if toPath != "" {
		destPath, err := filepath.Abs(toPath)
		if err != nil {
			return nil, "", fmt.Errorf("failed to resolve path: %w", err)
		}
		if err := os.MkdirAll(destPath, 0755); err != nil {
			return nil, "", fmt.Errorf("failed to create destination directory: %w", err)
		}
		root, err := os.OpenRoot(destPath)
		if err != nil {
			return nil, "", fmt.Errorf("failed to open destination directory: %w", err)
		}
		return root, destPath, nil
	}

	if err := config.CheckResourceName(name); err != nil {
		return nil, "", errPullNameNotLocal
	}
	if err := os.MkdirAll(parentDir, 0755); err != nil {
		return nil, "", fmt.Errorf("failed to create destination directory: %w", err)
	}
	parent, err := os.OpenRoot(parentDir)
	if err != nil {
		return nil, "", fmt.Errorf("failed to open destination directory: %w", err)
	}
	defer func() { _ = parent.Close() }()
	if err := parent.Mkdir(name, 0755); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, "", fmt.Errorf("failed to create destination directory: %w", err)
	}
	root, err := parent.OpenRoot(name)
	if err != nil {
		return nil, "", fmt.Errorf("failed to open destination directory: %w", err)
	}
	return root, filepath.Join(parentDir, name), nil
}
