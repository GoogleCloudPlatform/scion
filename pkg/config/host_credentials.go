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

package config

// HostCredentialsPolicy reports whether a co-located workstation broker may
// inject the host's harness credential files into its agents. It is on only
// in workstation mode (hosted is false) with dev auth enabled, and only when
// use_host_credentials is unset (default true) or explicitly true. Hosted
// mode is always off, whatever the setting says.
func HostCredentialsPolicy(hosted, devAuthEnabled bool, useHostCredentials *bool) bool {
	if hosted || !devAuthEnabled {
		return false
	}
	if useHostCredentials != nil {
		return *useHostCredentials
	}
	return true
}

// HostCredentialsEnabled evaluates HostCredentialsPolicy against the
// use_host_credentials key in the settings file under globalDir. A settings
// file that cannot be read leaves the key unset (the workstation default).
func HostCredentialsEnabled(globalDir string, hosted, devAuthEnabled bool) bool {
	var use *bool
	if globalDir != "" {
		if vs, err := LoadSingleFileVersioned(globalDir); err == nil && vs != nil {
			use = vs.UseHostCredentials
		}
	}
	return HostCredentialsPolicy(hosted, devAuthEnabled, use)
}
