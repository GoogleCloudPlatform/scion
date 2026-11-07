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
	"encoding/json"
	"log/slog"
	"net/http"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// server.hub.content_url (config.ContentURLKey) on the server-config write
// path (ptone/scion#7107 phase A). It is an endpoints-section key, written
// in the same modes as server.hub.public_url, refused for user access
// tokens (serverConfigTokenKeys), audited on every change of the value in
// effect (auditEndpointURLChange), and applied without a restart
// (ApplySnapshot).

// validateContentURLRequest checks the server.hub.content_url a server-config
// PUT sends: config.ValidateContentURL, and the host-name rule against
// public, the hub's public URL (a value on that host name, at any scheme or
// port, is refused). It normalizes the value in place and writes a 422
// validation_error naming the key when it fails. A value on the hub's
// registrable domain is accepted with a warning. An omitted key or an
// explicit "" (clear) passes. A value on the host name of any of agents is
// refused the same way: the callers pass the hub's running agent endpoint
// (server.hub.agent_endpoint; "" when unset, which means the public URL)
// and the one the request sends ("" when none), since the guard would
// confine the host agents reach the hub on, now or after the restart that
// applies the sent one (S2). It reports whether the request may continue.
func validateContentURLRequest(w http.ResponseWriter, req *ServerConfigUpdateRequest, public string, agents ...string) bool {
	if req == nil || req.Server == nil || req.Server.Hub == nil || req.Server.Hub.ContentURL == "" {
		return true
	}
	normalized, err := config.ValidateContentURL(req.Server.Hub.ContentURL)
	if err != nil {
		writeSettingsFieldError(w, config.ContentURLKey, err.Error())
		return false
	}
	sameHost, sameSite := config.ContentURLHubConflict(normalized, public)
	if sameHost {
		writeSettingsFieldError(w, config.ContentURLKey,
			config.ContentURLKey+": must not be on the hub's own host name ("+config.PublicURLKey+"), at any scheme or port; use a separate host name, ideally on a separate registrable domain")
		return false
	}
	for _, agent := range agents {
		if config.ContentURLSameHost(normalized, agent) {
			writeSettingsFieldError(w, config.ContentURLKey,
				config.ContentURLKey+": must not be on the host name of server.hub.agent_endpoint, at any scheme or port; agents reach the hub there")
			return false
		}
	}
	if sameSite {
		slog.Warn(config.ContentURLKey+" shares the hub's registrable domain, which gives little isolation; use a separate registrable domain",
			"content_url", normalized, "hub_public_url", public)
	}
	req.Server.Hub.ContentURL = normalized
	return true
}

// contentURLFromSectionDoc returns content_url from an endpoints section
// document, or "" when the document is empty or does not set it.
func contentURLFromSectionDoc(doc json.RawMessage) string {
	if len(doc) == 0 {
		return ""
	}
	var d struct {
		ContentURL string `json:"content_url"`
	}
	if err := json.Unmarshal(doc, &d); err != nil {
		return ""
	}
	return d.ContentURL
}

// bootstrapContentURL returns server.hub.content_url from the bootstrap
// merge (settings.yaml and the environment), the value in effect when the
// endpoints row does not set it, or "" when there is none.
func (o *OperationalSettings) bootstrapContentURL() string {
	if o == nil || o.bootstrapKoanf == nil {
		return ""
	}
	return o.bootstrapKoanf.String(config.ContentURLKey)
}

// contentURLInEffectWith returns the content URL in effect once doc is the
// endpoints row: doc's own content_url, else the bootstrap value. The audit
// compares it with the value in effect before a save or reset
// (Snapshot().ContentURL), so a change is recorded whichever layer it
// comes from.
func contentURLInEffectWith(ops *OperationalSettings, doc json.RawMessage) string {
	if v := contentURLFromSectionDoc(doc); v != "" {
		return v
	}
	return ops.bootstrapContentURL()
}

// requestAgentEndpoint returns the server.hub.agent_endpoint a
// server-config save sends, or "" when it sends none.
func requestAgentEndpoint(req *ServerConfigUpdateRequest) string {
	if req == nil || req.Server == nil || req.Server.Hub == nil {
		return ""
	}
	return req.Server.Hub.AgentEndpoint
}

// contentURLFileChange returns, for a settings-file save, the content_url
// of the settings file before the merge (prev, the file as read) and of the
// merged settings file the save writes (merged, the YAML it marshals), each
// normalized when valid and "" when absent. Both come from the file, so
// they compare like with like (S1): a content URL set only in the
// environment is in neither and never looks like a change. Deriving the
// second from the merged file, not from the request, records every change
// the write makes: a value, a clear, an explicit null or a "hub": null
// (SEC-A2). The value in effect is not compared.
func contentURLFileChange(prev, merged []byte) (before, after string) {
	norm := func(v string) string {
		if n, err := config.ValidateContentURL(v); err == nil {
			return n
		}
		return v
	}
	return norm(fileContentURL(prev)), norm(fileContentURL(merged))
}

// fileContentURL returns server.hub.content_url from settings-file YAML,
// or "" when the file does not set it or cannot be parsed.
func fileContentURL(data []byte) string {
	var doc struct {
		Server struct {
			Hub struct {
				ContentURL string `yaml:"content_url"`
			} `yaml:"hub"`
		} `yaml:"server"`
	}
	_ = yamlv3.Unmarshal(data, &doc)
	return doc.Server.Hub.ContentURL
}
