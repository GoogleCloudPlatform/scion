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

import "github.com/GoogleCloudPlatform/scion/pkg/api"

// defaultProjectSharedDirs returns the hub-configured default shared dirs
// for new projects. Returns a scratchpad shared dir when enabled (the
// compiled default), or nil when the operator has explicitly disabled it.
//
// Thread-safe: reads from OperationalSettings under its internal lock.
func (s *Server) defaultProjectSharedDirs() []api.SharedDir {
	enabled := true // compiled default: ON

	if ops := s.GetOperationalSettings(); ops != nil {
		enabled = ops.ProjectDefaultScratchpad()
	}
	// File/SQLite mode: ops is nil → compiled default (ON) applies.

	if !enabled {
		return nil
	}
	return []api.SharedDir{{Name: "scratchpad"}}
}
