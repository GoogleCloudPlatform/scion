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
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/util/urlredact"
)

// server.hub.content_url (config.ContentURLKey) on the server-config write
// path (ptone/scion#7107 phase A). It is an endpoints-section key, written
// in the same modes as server.hub.public_url, refused for user access
// tokens (serverConfigTokenKeys), audited on every change of the value in
// effect (auditEndpointURLChange), and applied without a restart
// (ApplySnapshot).

// contentURLSameSiteWarning is the warning logged when server.hub.content_url
// shares the hub's registrable domain and is accepted only because
// server.hub.content_url_allow_same_site is set (on save and at runtime).
const contentURLSameSiteWarning = config.ContentURLKey + " shares the hub's registrable domain, so it is not a separate security origin for cookies: the browser treats the two hosts as same-site and a cookie scoped to that domain reaches both; the supported setup is a content host on a different registrable domain (accepted because " + config.ContentURLAllowSameSiteKey + " is set)"

// validateContentURLRequest checks the server.hub.content_url a server-config
// PUT sends: config.ValidateContentURL, and the host-name rule against
// public, the hub's public URL (a value on that host name, at any scheme or
// port, is refused). It normalizes the value in place and writes a 422
// validation_error naming the key when it fails. A value on the hub's
// registrable domain is refused unless allowSameSite (the
// server.hub.content_url_allow_same_site the save leaves in effect,
// contentURLAllowSameSiteAfter) is true; with the override it is accepted
// and logs a warning. An omitted key or an
// explicit "" (clear) passes. A value on the host name of any of agents is
// refused the same way: the callers pass the hub's running agent endpoint
// (server.hub.agent_endpoint; "" when unset, which means the public URL)
// and the one the request sends ("" when none), since the guard would
// confine the host agents reach the hub on, now or after the restart that
// applies the sent one (S2). It reports whether the request may continue.
func validateContentURLRequest(w http.ResponseWriter, req *ServerConfigUpdateRequest, public string, allowSameSite bool, agents ...string) bool {
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
		if !allowSameSite {
			writeSettingsFieldError(w, config.ContentURLKey,
				config.ContentURLKey+": must not share the hub's registrable domain ("+config.PublicURLKey+"), because it would not be a separate security origin for cookies; use a content host on a different registrable domain, or set "+config.ContentURLAllowSameSiteKey)
			return false
		}
		slog.Warn(contentURLSameSiteWarning, "content_host", contentURLHostForLog(normalized), "hub_host", contentURLHostForLog(public))
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

// contentURLHostForLog returns only the host name of a URL for the
// same-site log lines, never the whole value, so no userinfo, path or query
// can reach the log; "" when it has none.
func contentURLHostForLog(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// contentURLForAudit returns a content URL value as its audit record and
// the audit's log line show it: the scheme, host and port of what
// urlredact.HostAndPath keeps (no user info, query or fragment), with the
// path dropped too, since a valid content URL is a bare origin; or
// urlredact.Mask when HostAndPath cannot show the value (it does not parse,
// or holds an '@' it cannot delimit) or when the value is not an http or
// https URL with a host (a scheme-less, opaque or "word:rest" value): only
// http and https origins are shown; anything else is the mask. A stored
// value that was never validated, such as a hand-edited settings file, is
// never recorded raw. Because the summary drops parts of the value, equal
// before and after summaries can still describe a real change.
func contentURLForAudit(raw string) string {
	if raw == "" {
		return ""
	}
	u, ok := urlredact.HostAndPath(raw)
	if !ok {
		return urlredact.Mask
	}
	// url.Parse already lower-cases the scheme; ToLower keeps this rule independent of that.
	if scheme := strings.ToLower(u.Scheme); (scheme != "http" && scheme != "https") || u.Host == "" {
		return urlredact.Mask
	}
	u.Path, u.RawPath, u.Opaque = "", "", ""
	return u.String()
}

// contentURLAllowSameSiteAfter returns the server.hub.content_url_allow_same_site
// a server-config save leaves in effect: the request's value when it sends
// one, else the value in effect now (current). The content URL the same save
// sends is validated against it, so one request can set both.
func contentURLAllowSameSiteAfter(req *ServerConfigUpdateRequest, current bool) bool {
	if req != nil && req.Server != nil && req.Server.Hub != nil && req.Server.Hub.ContentURLAllowSameSite != nil {
		return *req.Server.Hub.ContentURLAllowSameSite
	}
	return current
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

// The same-site override (server.hub.content_url_allow_same_site,
// ptone/scion#7107 phase B item 1) decides whether a content URL on the
// hub's registrable domain is used, so every change of the value in effect
// is audited like a content URL change, with "true" and "false" as the old
// and new values.

// contentURLAllowSameSiteFromSectionDoc returns content_url_allow_same_site
// from an endpoints section document, or nil when the document is empty or
// does not set it.
func contentURLAllowSameSiteFromSectionDoc(doc json.RawMessage) *bool {
	if len(doc) == 0 {
		return nil
	}
	var d struct {
		ContentURLAllowSameSite *bool `json:"content_url_allow_same_site"`
	}
	if err := json.Unmarshal(doc, &d); err != nil {
		return nil
	}
	return d.ContentURLAllowSameSite
}

// bootstrapContentURLAllowSameSite returns
// server.hub.content_url_allow_same_site from the bootstrap merge
// (settings.yaml and the environment): the value in effect when the
// endpoints row does not set it.
func (o *OperationalSettings) bootstrapContentURLAllowSameSite() bool {
	if o == nil || o.bootstrapKoanf == nil {
		return false
	}
	return o.bootstrapKoanf.Bool(config.ContentURLAllowSameSiteKey)
}

// contentURLAllowSameSiteInEffectWith returns the override in effect once
// doc is the endpoints row: doc's own value when it sets one, else the
// bootstrap value.
func contentURLAllowSameSiteInEffectWith(ops *OperationalSettings, doc json.RawMessage) bool {
	if v := contentURLAllowSameSiteFromSectionDoc(doc); v != nil {
		return *v
	}
	return ops.bootstrapContentURLAllowSameSite()
}

// fileContentURLAllowSameSite returns server.hub.content_url_allow_same_site
// from settings-file YAML as "true" or "false", false when the file does not
// set it or cannot be parsed. Like contentURLFileChange it reads the file
// only, so a value set only in the environment never looks like a change.
func fileContentURLAllowSameSite(data []byte) string {
	var doc struct {
		Server struct {
			Hub struct {
				ContentURLAllowSameSite bool `yaml:"content_url_allow_same_site"`
			} `yaml:"hub"`
		} `yaml:"server"`
	}
	_ = yamlv3.Unmarshal(data, &doc)
	return strconv.FormatBool(doc.Server.Hub.ContentURLAllowSameSite)
}

// endpointURLAudits records, before the write, the endpoint URL changes one
// section write makes (public_url, content_url and its same-site override).
// When a later record or the write itself fails, every change already
// recorded gets a not-applied record, so the audit trail never shows a
// change that was not made (ptone/scion#7107 sec S1).
type endpointURLAudits struct {
	s        *Server
	ctx      context.Context
	surface  string
	recorded []endpointURLAuditChange
}

type endpointURLAuditChange struct{ key, before, after string }

// record writes the change record for key. On failure it marks the changes
// already recorded as not applied and returns the error; the caller then
// makes no write.
func (a *endpointURLAudits) record(key, before, after string) error {
	if err := a.s.auditEndpointURLChange(a.ctx, key, a.surface, before, after); err != nil {
		a.notApplied(err)
		return err
	}
	a.recorded = append(a.recorded, endpointURLAuditChange{key: key, before: before, after: after})
	return nil
}

// notApplied writes a not-applied record for every change recorded so far.
func (a *endpointURLAudits) notApplied(cause error) {
	for _, c := range a.recorded {
		a.s.auditEndpointURLNotApplied(a.ctx, c.key, a.surface, c.before, c.after, cause)
	}
}

// ContentURLStatus reports, read-only, what this hub does with
// server.hub.content_url (ptone/scion#7107): whether the value is in
// effect, and if not, why it is ignored. GET server-config returns it as
// content_url_status; a PUT body that sends it back has it dropped.
type ContentURLStatus struct {
	// State is one of the ContentURLState* values.
	State string `json:"state"`
	// Effective is the content host in use; set only when State is
	// in_effect.
	Effective string `json:"effective,omitempty"`
	// OverrideKey names the setting that would let the value be used;
	// set only when State is ignored_same_site.
	OverrideKey string `json:"override_key,omitempty"`
}

// contentURLStatusKey is the response key of ContentURLStatus.
const contentURLStatusKey = "content_url_status"

// contentURLStatus returns the state of the content URL as applied now.
func (s *Server) contentURLStatus() *ContentURLStatus {
	// The classification derived at apply time, the same one the guard uses.
	c := s.contentURLView(hubEndpointsFromConfig(s.config).Public).class
	st := &ContentURLStatus{State: c.state}
	switch c.state {
	case ContentURLStateInEffect:
		st.Effective = c.normalized
	case ContentURLStateSameSite:
		st.OverrideKey = config.ContentURLAllowSameSiteKey
	}
	return st
}

// WarnContentURLNotInEffect logs, once at startup, why a configured
// server.hub.content_url is not in effect, for example the one ERROR for a
// same-site value stored without server.hub.content_url_allow_same_site.
// The guard would log it only on the first request that reads the content
// host; later changes are logged as they are read, once per value.
func (s *Server) WarnContentURLNotInEffect() {
	_ = s.HubEndpoints()
}
