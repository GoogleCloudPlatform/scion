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

// Package tmuxdeliver holds the steps that put a message into an agent's
// tmux session: the readiness wait, the bracketed paste through a named
// buffer, the interrupt and submit keys, and the text sanitiser. It is shared
// by the two delivery paths so they behave the same:
//
//   - the Runtime Broker, which runs each step in the agent container through
//     the runtime's exec (an exec-backed Runner), and
//   - sciontool's deliver-message control route, which runs the same steps
//     locally inside the container (a local Runner).
//
// The package runs every command through a Runner and never builds a shell
// command from caller input: the tmux target is the constant Target, keys
// must be on the allowlist (CheckKey), and message text reaches tmux only on
// load-buffer's stdin, after Sanitize. Design: conduit v2.22 (deliver-message)
// §3.1, §3.4 and §5.3.
//
// It imports only the standard library and pkg/agent/state, so sciontool can
// use it without pulling in the agent manager.
package tmuxdeliver
