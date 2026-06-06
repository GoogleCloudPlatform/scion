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

//go:build integration

package integrationtest

import (
	"os"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

// TestMain provisions the per-package ephemeral Postgres database once (creating
// it under a unique name so parallel `go test` invocations never collide) and
// drops it when the package finishes. Both calls are no-ops when
// SCION_TEST_POSTGRES_URL is unset, in which case every test skips.
func TestMain(m *testing.M) {
	enttest.MainSetup()
	code := m.Run()
	enttest.MainTeardown()
	os.Exit(code)
}
