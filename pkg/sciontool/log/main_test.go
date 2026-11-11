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

package log

import (
	"os"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/util/logging/loglevel"
)

// TestMain clears the log level variables so tests that read agent.log do
// not depend on the caller's SCION_LOG_LEVEL / SCION_DEBUG.
func TestMain(m *testing.M) {
	_ = os.Unsetenv(loglevel.EnvLogLevel)
	_ = os.Unsetenv(loglevel.EnvDebug)
	os.Exit(m.Run())
}
