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

// HubClassKey is the settings key of the hub class (HubServerConfig.HubClass).
const HubClassKey = "server.hub.class"

// HubGCPProjectIDKey is the settings key of the hub's GCP project
// (HubServerConfig.GCPProjectID). It selects the service-account minting
// project, so like HubClassKey it is set only in the settings file or the
// environment (SCION_SERVER_HUB_GCPPROJECTID): the server-config API
// refuses it in every mode (ptone/scion#4942).
const HubGCPProjectIDKey = "server.hub.gcp_project_id"

// HubClassTest is the only recognised server.hub.class value. A hub whose
// class is anything else, including unset, is not a test hub.
const HubClassTest = "test"

// IsTestHubClass reports whether class is exactly HubClassTest. The match
// is exact and case-sensitive: the class is a positive, deliberate
// declaration, so near-misses ("Test", " test") do not count.
func IsTestHubClass(class string) bool {
	return class == HubClassTest
}
