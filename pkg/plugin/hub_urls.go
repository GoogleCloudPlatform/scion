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

// Hub URL keys the hub injects into a broker plugin's configuration.
const (
	// ConfigKeyHubURL is the hub's public endpoint. Plugins build the links
	// people open from it (account-linking buttons, agent terminal links).
	ConfigKeyHubURL = "hub_url"
	// ConfigKeyHubAPIURL is the URL a broker plugin uses for its own API
	// calls to the hub. The hub sets it only for plugins it runs on its own
	// host (hub-managed: not self-managed, not HA), to its loopback URL, so
	// those calls never meet a proxy in front of the public endpoint (such
	// as IAP, which rejects the HMAC-signed requests). When it is absent
	// plugins call hub_url, as older hubs expect.
	ConfigKeyHubAPIURL = "hub_api_url"
)

// HubAPIURL returns the base URL for a broker plugin's API calls to the hub:
// apiURL (hub_api_url) when set, else hubURL (hub_url).
func HubAPIURL(hubURL, apiURL string) string {
	if apiURL != "" {
		return apiURL
	}
	return hubURL
}
