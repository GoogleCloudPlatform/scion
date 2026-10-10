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

package hub

import (
	"context"
	"net/http"
	"sort"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
)

// Integration config writes by credentials other than an interactive
// session.
//
// PUT /api/v1/admin/integrations/{name}/config carries a secrets map and a
// settings map. A credential other than an interactive session or a dev
// credential (sessionCredentialAllowed) may write only settings keys listed
// as configuration for that integration in integrationTokenSettingsKeys:
//   - any entry in secrets is refused (the integration's credentials);
//   - a settings key that carries or selects credential material, sets
//     how the integration authenticates or maps identities, names an
//     endpoint, network address or host path, or selects the integration's
//     inbound endpoint and its authentication (for telegram, the inbound
//     mode and webhook registration settings) is refused;
//   - a settings key not listed for the integration, or any key of an
//     integration not listed, is refused.
//
// A key counts when it is present, whatever its value.

// integrationSecretsKey is the refused-key name reported for a non-empty
// secrets map.
const integrationSecretsKey = "secrets"

// integrationTokenSettingsKeys lists, per integration, the settings keys a
// non-session credential may write. Every other key refuses such a
// credential.
var integrationTokenSettingsKeys = map[string]map[string]bool{
	"telegram": {
		"agent_cache_ttl": true,
		"send_queue_size": true,
		"send_min_delay":  true,
	},
	"discord": {},
	"slack": {
		"socket_mode":     true,
		"agent_cache_ttl": true,
	},
	"a2a-bridge": {
		"rate_limit_enabled":   true,
		"rate_limit_rps":       true,
		"rate_limit_burst":     true,
		"send_message_timeout": true,
		"sse_keepalive":        true,
		"push_retry_max":       true,
		"provider_org":         true,
	},
	"chat-app": {},
	"teams":    {},
}

// tokenRefusedIntegrationConfigKeys returns the sorted keys of an
// integration config update that a non-session credential may not write:
// "secrets" when the secrets map has any entry, and "settings.<key>" for
// every settings key that is not configuration for the named integration.
func tokenRefusedIntegrationConfigKeys(name string, req IntegrationConfigUpdateRequest) []string {
	var refused []string
	if len(req.Secrets) > 0 {
		refused = append(refused, integrationSecretsKey)
	}
	allowed := integrationTokenSettingsKeys[name]
	for key := range req.Settings {
		if !allowed[key] {
			refused = append(refused, "settings."+key)
		}
	}
	sort.Strings(refused)
	return refused
}

// writeTokenRefusedIntegrationKeys refuses an integration config update
// that carries refused keys from any credential other than an interactive
// session or a dev credential: 403 with the session-only details (reason
// CREDENTIAL_MANAGEMENT) and details.keys listing the refused keys. An
// unknown or missing credential kind is refused too. It writes nothing and
// returns false for a session or dev credential, or when keys is empty.
func writeTokenRefusedIntegrationKeys(w http.ResponseWriter, ctx context.Context, keys []string) bool {
	if sessionCredentialAllowed(ctx) || len(keys) == 0 {
		return false
	}
	details := sessionOnlyDenialDetails(authzop.ReasonCredentialManagement)
	details["keys"] = keys
	writeError(w, http.StatusForbidden, ErrCodeForbidden,
		"these keys require an interactive session", details)
	return true
}
