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

package hooks

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
)

// TestMain points the sciontool log at a temp file for the whole package and
// removes it afterwards, so no test appends to a real agent log.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "hooks-test-log")
	if err != nil {
		fmt.Fprintln(os.Stderr, "hooks tests: creating log dir:", err)
		os.Exit(1)
	}
	log.SetLogPath(filepath.Join(dir, "agent.log"))
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
