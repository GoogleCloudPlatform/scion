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

package plugin

import "testing"

func TestHubAPIURL(t *testing.T) {
	if got := HubAPIURL("https://hub.example.com", "http://localhost:8080"); got != "http://localhost:8080" {
		t.Errorf("HubAPIURL with hub_api_url = %q, want the API URL", got)
	}
	if got := HubAPIURL("https://hub.example.com", ""); got != "https://hub.example.com" {
		t.Errorf("HubAPIURL without hub_api_url = %q, want hub_url", got)
	}
}
