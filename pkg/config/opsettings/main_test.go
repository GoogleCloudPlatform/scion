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

package opsettings

import (
	"os"
	"strings"
	"testing"
)

// TestMain unsets every SCION_SERVER_* and SCION_SEED_* variable the
// environment running the tests exports, before any test runs. The env
// mappers in pkg/config read the process env to let a canonical name win
// over its aliases, so an ambient value would change what an alias maps to
// (and what FindUnmatchedSettingsEnv reports). Tests that need one set it
// with t.Setenv.
func TestMain(m *testing.M) {
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "SCION_SERVER_") || strings.HasPrefix(name, "SCION_SEED_") {
			_ = os.Unsetenv(name)
		}
	}
	os.Exit(m.Run())
}
