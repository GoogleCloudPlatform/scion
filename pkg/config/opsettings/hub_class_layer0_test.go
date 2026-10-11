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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// server.hub.class (the test admin tier gate input) is Layer-0: no DB
// section can hold it, so it is never part of the shared settings rows.
func TestHubClass_IsLayer0(t *testing.T) {
	layer1, layer0, unclassified := ClassifyKeys([]string{config.HubClassKey})
	if len(layer1) != 0 || len(unclassified) != 0 || len(layer0) != 1 {
		t.Errorf("ClassifyKeys(%s): layer1=%v layer0=%v unclassified=%v", config.HubClassKey, layer1, layer0, unclassified)
	}
	if sec := OwningSection(config.HubClassKey); sec != "" {
		t.Errorf("OwningSection(%s) = %q, want none", config.HubClassKey, sec)
	}
}
