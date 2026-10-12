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

import "log/slog"

// HostCredentialsEligible reports whether a server may give agents on its
// co-located broker the host's harness credential files at all: only in
// workstation mode (hosted is false) with dev auth enabled. Hosted mode is
// always ineligible, whatever use_host_credentials says. The setting itself
// is read per start through UseHostCredentials, so turning it off takes
// effect on the next agent start without a server restart.
func HostCredentialsEligible(hosted, devAuthEnabled bool) bool {
	return !hosted && devAuthEnabled
}

// UseHostCredentials reads the use_host_credentials key from the settings
// file under globalDir. With no settings file, or the key unset, it returns
// true (the workstation default). It fails closed: a settings file that
// cannot be read or parsed returns false, so a broken file can never turn
// an explicit "false" back into the default.
func UseHostCredentials(globalDir string) bool {
	if globalDir == "" {
		slog.Warn("Host credential files disabled: no global settings directory")
		return false
	}
	vs, err := LoadSingleFileVersioned(globalDir)
	if err != nil || vs == nil {
		slog.Warn("Host credential files disabled: cannot read global settings", "error", err)
		return false
	}
	if vs.UseHostCredentials != nil {
		return *vs.UseHostCredentials
	}
	return true
}
