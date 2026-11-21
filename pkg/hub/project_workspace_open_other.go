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
	"io/fs"
	"os"
)

// openWorkspaceRegularForRead always fails with errors.ErrUnsupported on this
// platform: workspace file opens require O_NONBLOCK and O_NOFOLLOW (see
// project_workspace_open_unix.go), and the hub server does not run here.
func openWorkspaceRegularForRead(dir *os.Root, name string, expected fs.FileInfo) (*os.File, fs.FileInfo, error) {
	return nil, nil, fmt.Errorf("open workspace file %s: %w on this platform", name, errors.ErrUnsupported)
}

// openWorkspaceFileForWrite always fails with errors.ErrUnsupported on this
// platform: workspace file opens require O_NONBLOCK and O_NOFOLLOW (see
// project_workspace_open_unix.go), and the hub server does not run here.
func openWorkspaceFileForWrite(dir *os.Root, name string, exists bool) (*os.File, error) {
	return nil, fmt.Errorf("open workspace file %s for write: %w on this platform", name, errors.ErrUnsupported)
}
