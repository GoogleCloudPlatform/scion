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

package config

import "os"

// agentConfigSingleLink is the link-count rule of the scion-agent config
// reader on this platform: agenthome's reader here checks only that the
// entry is a regular file (it has no link count), so the probe matches it.
// Agent provisioning, where the probe guards writes, runs only on unix.
func agentConfigSingleLink(info os.FileInfo) bool {
	return true
}
