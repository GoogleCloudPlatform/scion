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

package config

import (
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// The git token remote is hub-only: no broker-local layer may supply it.
func TestIsHubOnlyEnvKey_TokenRemote(t *testing.T) {
	if !IsHubOnlyEnvKey(api.EnvGitTokenRemote) {
		t.Errorf("%s must be a hub-only env key", api.EnvGitTokenRemote)
	}
}

// The in-container clone URL is hub-only too.
func TestIsHubOnlyEnvKey_CloneURL(t *testing.T) {
	if !IsHubOnlyEnvKey("SCION_GIT_CLONE_URL") {
		t.Error("SCION_GIT_CLONE_URL must be a hub-only env key")
	}
}
