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

import "github.com/GoogleCloudPlatform/scion/pkg/store"

// defaultAgentMessageMode is the message mode of a new agent when neither
// the create request, its template nor its parent agent sets one: the hub
// default (server.hub.default_message_mode), or "project" when it is unset.
// The startup preflight refuses a value that is not a valid mode or that is
// "hub"; a value that still reaches here invalid falls back to "project".
func (s *Server) defaultAgentMessageMode() string {
	mode := s.config.DefaultMessageMode
	if mode == "" || mode == store.MessageModeHub || !store.IsValidMessageMode(mode) {
		return store.MessageModeProject
	}
	return mode
}
