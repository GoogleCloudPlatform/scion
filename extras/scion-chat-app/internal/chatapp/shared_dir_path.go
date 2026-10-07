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
	scionruntime "github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// loadSharedDirSettings loads the global settings that choose each shared
// dir's storage backend (local or nfs). Tests replace it with fake config.
var loadSharedDirSettings = scionruntime.LoadSharedDirStorageSettings

// resolveSharedDirHostPath returns the host directory agents on this
// machine mount for the project's shared dir, through the same
// backend-aware resolution agent start uses. For an nfs-backed dir whose
// mount is unavailable it returns an error wrapping
// scionruntime.ErrSharedDirStorageUnavailable; callers must not fall back
// to the local layout, which no agent mounts in that case.
func resolveSharedDirHostPath(home, slug, projectID, name string) (string, error) {
	gs, err := loadSharedDirSettings()
	if err != nil {
		return "", err
	}
	res, err := scionruntime.ResolveSharedDirHostPath(gs, home, slug, projectID, name)
	if err != nil {
		return "", err
	}
	return res.Path, nil
}
