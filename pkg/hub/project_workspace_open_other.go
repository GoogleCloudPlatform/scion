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
	"io/fs"
	"os"

	"github.com/GoogleCloudPlatform/scion/pkg/util/rootopen"
)

// openWorkspaceRegularForRead delegates to rootopen.OpenRegularForRead, which
// fails closed with errors.ErrUnsupported on platforms without openat(2).
func openWorkspaceRegularForRead(dir *os.Root, name string, expected fs.FileInfo) (*os.File, fs.FileInfo, error) {
	return rootopen.OpenRegularForRead(dir, name, expected)
}

// openWorkspaceFileForWrite delegates to rootopen.OpenFileForWrite, which
// fails closed with errors.ErrUnsupported on platforms without openat(2).
func openWorkspaceFileForWrite(dir *os.Root, name string, exists bool) (*os.File, error) {
	return rootopen.OpenFileForWrite(dir, name, 0o644, exists)
}
