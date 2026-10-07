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

package chatapp

import (
	"fmt"

	scionruntime "github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// loadSharedDirSettings loads the global settings that choose each shared
// dir's storage backend (local or nfs). Tests replace it with fake config.
var loadSharedDirSettings = scionruntime.LoadSharedDirStorageSettings

// resolveSharedDirHost resolves a shared dir's host path. Tests replace it
// to return results the real resolver does not produce.
var resolveSharedDirHost = scionruntime.ResolveSharedDirHostPath

// resolveSharedDirHostPath returns the host directory agents on this
// machine mount for the project's shared dir, through the same
// backend-aware resolution agent start uses. For an nfs-backed dir whose
// mount is unavailable it returns an error wrapping
// scionruntime.ErrSharedDirStorageUnavailable; callers must not fall back
// to the local layout, which no agent mounts in that case. An empty
// resolved path is treated the same way, so it is never used as a path
// relative to the working directory.
func resolveSharedDirHostPath(home, slug, projectID, name string) (string, error) {
	gs, err := loadSharedDirSettings()
	if err != nil {
		return "", err
	}
	res, err := resolveSharedDirHost(gs, home, slug, projectID, name)
	if err != nil {
		return "", err
	}
	if res.Path == "" {
		return "", fmt.Errorf("%w: shared dir %q resolved to an empty path",
			scionruntime.ErrSharedDirStorageUnavailable, name)
	}
	return res.Path, nil
}
