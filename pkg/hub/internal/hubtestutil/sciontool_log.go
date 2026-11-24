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

package hubtestutil

import (
	"os"
	"path/filepath"

	sciontoollog "github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
)

// SilenceSciontoolLog configures pkg/sciontool/log so in-process sciontool
// components (such as conduit agents and port-forward managers started during
// hub tests) do not leak "[sciontool] INFO:" lines to stderr or append to the
// host/container's real /home/scion/agent.log. Call after testutil.IsolateHome
// so HOME points at the test's isolated temporary directory.
func SilenceSciontoolLog() {
	sciontoollog.SetQuiet(true)
	if home := os.Getenv("HOME"); home != "" {
		sciontoollog.SetLogPath(filepath.Join(home, "agent.log"))
	}
}
