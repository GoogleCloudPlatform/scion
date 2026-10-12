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

//go:build !no_sqlite

package hub

import (
	"testing"
)

// TestTestServerDisablesCloudLogQuery pins the testServer half of
// ptone/scion#3188: the shared helper builds its server with
// DisableCloudLogQuery set, so cloudLogQueryProjectID (see
// TestCloudLogQueryProjectID) never hands New() a project, whatever the
// environment. Asserting the config rather than srv.logQueryService keeps
// the test meaningful on runners without GCP credentials, where client
// construction fails and leaves logQueryService nil regardless.
func TestTestServerDisablesCloudLogQuery(t *testing.T) {
	srv, _ := testServer(t)
	if !srv.config.DisableCloudLogQuery {
		t.Fatal("testServer must set ServerConfig.DisableCloudLogQuery")
	}
	if got := cloudLogQueryProjectID(srv.config); got != "" {
		t.Fatalf("cloudLogQueryProjectID(testServer config) = %q, want \"\"", got)
	}
}
