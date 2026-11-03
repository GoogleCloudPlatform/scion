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
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// doctorSharedDirStorage returns what the Kubernetes runtime diagnostics need
// to check the shared-dir storage class, resolved as an agent start resolves
// it: the settings-level shared_dir_storage_class for profile
// (VersionedSettings.ResolveSharedDirDefaults on the effective settings vs),
// and whether shared dirs are served from a shared project storage backend
// instead of per-dir claims (config.SharedDirStorageLayout on srv, the server
// section of the global settings). A Layout that does not resolve is reported
// as not shared: the start then refuses on the settings error itself.
func doctorSharedDirStorage(vs *config.VersionedSettings, srv *config.V1ServerConfig, profile string) (class string, shared bool) {
	if vs != nil {
		class, _ = vs.ResolveSharedDirDefaults(profile)
	}
	if layout, _, err := config.SharedDirStorageLayout(srv); err == nil {
		shared = layout.Backend.Shared()
	}
	return class, shared
}
