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

// Package attache2e holds an in-process end-to-end test of the agent attach
// chain: a WebSocket client, the hub PTY handler, the hub control-channel
// manager, the Runtime Broker control-channel client and the runtime exec.
// It has no non-test code.
//
// The test is a separate package so that its test binary compiles pkg/hub
// only as a library, never package hub's own (large) test package.
package attache2e
