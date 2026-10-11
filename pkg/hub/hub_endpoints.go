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
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// The hub resolves two endpoint values at startup (ptone/scion#3905):
//
//   - The operator endpoint: resolved from the environment, startup flags
//     and settings.yaml, with the existing fallbacks (ServerConfig.HubEndpoint).
//     The settings database is never consulted for it.
//   - The public endpoint: server.hub.public_url resolved with operator pins
//     first, then the settings database, then settings.yaml. When any operator
//     pin is set (SCION_SERVER_HUB_ENDPOINT, --base-url or
//     SCION_SERVER_BASE_URL) it is the operator endpoint. Otherwise a valid
//     value saved in the settings database wins, and with none it is the
//     operator endpoint.
//
// hubEndpointUses decides which of the two each use gets.

// HubEndpointUse names a place where the hub hands out its own URL.
type HubEndpointUse string

const (
	// HubEndpointForAgents is the SCION_HUB_ENDPOINT value stamped into
	// dispatched agents (unless server.hub.agent_endpoint overrides it),
	// and the input to the co-located broker's container-bridge rewrite.
	HubEndpointForAgents HubEndpointUse = "agents"
	// HubEndpointForInviteLinks is the base URL of invite links.
	HubEndpointForInviteLinks HubEndpointUse = "invite_links"
	// HubEndpointForChatBridge is the hub URL written into chat-bridge
	// configuration.
	HubEndpointForChatBridge HubEndpointUse = "chat_bridge"
	// HubEndpointForPluginBrokers is the hub URL handed to in-process
	// broker plugins together with their credentials.
	HubEndpointForPluginBrokers HubEndpointUse = "plugin_brokers"
	// HubEndpointForProjectSettings is the hub.endpoint written into the
	// settings of hub-managed project workspaces.
	HubEndpointForProjectSettings HubEndpointUse = "project_settings"
	// HubEndpointForJoinResponse is the informational hubEndpoint field of
	// a broker join response.
	HubEndpointForJoinResponse HubEndpointUse = "join_response"
	// HubEndpointForOIDCIssuer is the OIDC issuer default, used when
	// oidc.issuer_url is unset.
	HubEndpointForOIDCIssuer HubEndpointUse = "oidc_issuer"
	// HubEndpointForTransportAudience is the cloudrun_invoker transport
	// audience default, used when auth.transport.oidc_audience is unset.
	HubEndpointForTransportAudience HubEndpointUse = "transport_audience"
	// HubEndpointForConduitCheck is the hub host the conduit relay refuses
	// as its internal_advertise address in hosted HA.
	HubEndpointForConduitCheck HubEndpointUse = "conduit_check"
)

// hubEndpointKind selects one of the two resolved endpoints.
type hubEndpointKind int

const (
	// hubEndpointOperator is the operator endpoint.
	hubEndpointOperator hubEndpointKind = iota
	// hubEndpointPublic is the public endpoint.
	hubEndpointPublic
)

// hubEndpointUses is the single table that decides, per use, which
// resolved endpoint the hub hands out. Change an entry here to move one use
// between the operator and the public endpoint. Every use reads the
// endpoints resolved once at startup, before the hub is constructed, so an
// entry takes effect wherever the use is derived.
var hubEndpointUses = map[HubEndpointUse]hubEndpointKind{
	HubEndpointForAgents:            hubEndpointPublic,
	HubEndpointForInviteLinks:       hubEndpointPublic,
	HubEndpointForChatBridge:        hubEndpointPublic,
	HubEndpointForPluginBrokers:     hubEndpointPublic,
	HubEndpointForProjectSettings:   hubEndpointPublic,
	HubEndpointForJoinResponse:      hubEndpointPublic,
	HubEndpointForOIDCIssuer:        hubEndpointPublic,
	HubEndpointForTransportAudience: hubEndpointPublic,
	HubEndpointForConduitCheck:      hubEndpointPublic,
}

// Public endpoint sources reported by HubEndpoints.PublicSource besides the
// operator source labels (see HubEndpoints.OperatorSource).
const (
	HubEndpointSourceDatabase = "database"
)

// HubEndpoints holds the two endpoint values this hub resolved at startup.
type HubEndpoints struct {
	// Operator is the operator endpoint (ServerConfig.HubEndpoint).
	Operator string
	// OperatorSource names where Operator came from: "env", "flag",
	// "file", or a fallback label ("settings", "iap-audience",
	// "localhost").
	OperatorSource string
	// Pin names the operator setting that pins the public endpoint
	// (SCION_SERVER_HUB_ENDPOINT, SCION_SERVER_BASE_URL or --base-url), or
	// "" when none is set.
	Pin string
	// Public is the public endpoint.
	Public string
	// PublicSource is HubEndpointSourceDatabase when the database value
	// applies, otherwise OperatorSource.
	PublicSource string
}

// For returns the endpoint the hub hands out for use.
func (e HubEndpoints) For(use HubEndpointUse) string {
	if hubEndpointUses[use] == hubEndpointPublic && e.Public != "" {
		return e.Public
	}
	return e.Operator
}

// hubEndpointsFromConfig returns cfg.HubEndpoints, or, when the caller did
// not resolve them (tests, embedded servers), endpoints whose public value
// is the operator endpoint cfg.HubEndpoint.
func hubEndpointsFromConfig(cfg ServerConfig) HubEndpoints {
	if cfg.HubEndpoints != nil {
		return *cfg.HubEndpoints
	}
	return HubEndpoints{Operator: cfg.HubEndpoint, Public: cfg.HubEndpoint}
}

// ResolveHubEndpoints computes the public endpoint from the operator
// endpoint fields of base (Operator, OperatorSource, Pin) and dbValue, the server.hub.public_url stored in the
// settings database ("" when none), and logs the result: one line for each
// endpoint, plus a line when the database value replaces a settings.yaml
// value. An invalid database value is logged as an error and not used.
func ResolveHubEndpoints(base HubEndpoints, dbValue string, logger *slog.Logger) HubEndpoints {
	if logger == nil {
		logger = slog.Default()
	}
	e := base
	e.Public = base.Operator
	e.PublicSource = base.OperatorSource

	switch {
	case dbValue == "":
	case base.Pin != "":
		if strings.TrimRight(dbValue, "/") != strings.TrimRight(base.Operator, "/") {
			logger.Info("Hub public URL saved in the settings database is not used: an operator setting pins the hub endpoint",
				"pinned_by", base.Pin)
		}
	case strings.TrimRight(dbValue, "/") == strings.TrimRight(base.Operator, "/"):
		// The stored value is the operator value (for example a row seeded
		// from settings.yaml); it is not an override.
	default:
		normalized, err := config.ValidatePublicURL(dbValue)
		if err != nil {
			logger.Error("Hub public URL saved in the settings database is invalid and is not used; falling back to the operator-configured endpoint",
				"error", err, "fallback_source", base.OperatorSource)
			break
		}
		if base.OperatorSource == hubEndpointSourceLabelFile {
			logger.Info("Hub public URL saved in the settings database overrides the settings.yaml value",
				"database_value", normalized, "file_value", base.Operator)
		}
		e.Public = normalized
		e.PublicSource = HubEndpointSourceDatabase
	}

	opAttrs := []any{"source", e.OperatorSource, "endpoint", e.Operator}
	logger.Info("Hub operator endpoint (environment, flags and settings.yaml)", opAttrs...)
	pubAttrs := []any{"source", e.PublicSource, "endpoint", e.Public}
	if e.Pin != "" {
		pubAttrs = append(pubAttrs, "pinned_by", e.Pin)
	}
	logger.Info("Hub endpoint in effect", pubAttrs...)
	return e
}

// hubEndpointSourceLabelFile is the operator source label for a value read
// from settings.yaml.
const hubEndpointSourceLabelFile = "file"

// HubEndpoints returns the endpoints this hub resolved at startup
// (ServerConfig.HubEndpoints, else ServerConfig.HubEndpoint for both). They
// never change while the hub runs: server.hub.public_url is
// restart-required.
func (s *Server) HubEndpoints() HubEndpoints {
	return hubEndpointsFromConfig(s.config)
}

// HubEndpointFor returns the endpoint the hub hands out for use (see
// hubEndpointUses).
func (s *Server) HubEndpointFor(use HubEndpointUse) string {
	return s.HubEndpoints().For(use)
}

// StoredPublicURL returns the server.hub.public_url saved in the settings
// database, or "" when none is saved. Call it at startup, before the hub is
// constructed, so every endpoint derivation sees the same value.
//
// Only a value an admin saved counts (a managed endpoints row). A seeded row
// is rewritten from the bootstrap material (settings.yaml and the
// environment) at every start, so its value is the file value, not a saved
// one; at this point in startup it may also still hold the previous start's
// bootstrap value (see storedRowIsManaged).
//
// A missing row is not an error. A row that cannot be decoded is logged and
// ignored (the operator endpoint then applies); any other read error is
// returned, since settings that cannot be read stop the hub from starting
// anyway.
func StoredPublicURL(ctx context.Context, st store.HubSettingStore) (string, error) {
	row, err := st.GetHubSetting(ctx, "endpoints")
	if errors.Is(err, store.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading the endpoints settings: %w", err)
	}
	if !storedRowIsManaged(row) {
		return "", nil
	}
	var doc struct {
		PublicURL string `json:"public_url"`
	}
	if err := json.Unmarshal(row.Value, &doc); err != nil {
		slog.Error("Endpoints settings row cannot be decoded; its public URL is not used", "error", err)
		return "", nil
	}
	return doc.PublicURL, nil
}

// storedRowIsManaged reports whether a settings row was written by an admin
// rather than the startup seeder. It applies the BackfillOrigin rule, which
// has not yet run at this point in startup: a row the seeder did not write
// (updated_by other than "seed") is managed whatever its origin column says.
func storedRowIsManaged(row *store.HubSetting) bool {
	return row.Origin == "managed" || row.UpdatedBy != "seed"
}
