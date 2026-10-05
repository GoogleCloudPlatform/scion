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
	"fmt"
	"os"
	"path/filepath"
	"reflect"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	yamlv3 "gopkg.in/yaml.v3"
)

// maskedValue is the placeholder GET /api/v1/admin/server-config returns in
// place of a sensitive value. Clients (the admin UI) send the GET body back on
// save, so PUT must treat this value as "unchanged" and never persist it.
const maskedValue = "********"

// maskedServerField locates one masked string field inside a server config.
// get returns nil when the field's parent struct is absent; it never
// allocates. block returns the struct the secret sits in (an OAuth provider,
// the GitHub App, server.auth, ...), or nil when absent. Every entry must set
// it (enforced by a test; a missing block fails closed): a masked value is
// restored only if the incoming block is exactly what GET showed for the
// stored block (see restoreMaskedServerSecrets), so a secret is never
// carried over to a different owner.
type maskedServerField struct {
	name  string
	get   func(s *config.V1ServerConfig) *string
	block func(s *config.V1ServerConfig) any
}

func oauthSecretField(name string, client func(o *config.V1OAuthConfig) *config.V1OAuthClientConfig, provider func(c *config.V1OAuthClientConfig) *config.V1OAuthProviderConfig) maskedServerField {
	getProvider := func(s *config.V1ServerConfig) *config.V1OAuthProviderConfig {
		if s.OAuth == nil {
			return nil
		}
		c := client(s.OAuth)
		if c == nil {
			return nil
		}
		return provider(c)
	}
	return maskedServerField{
		name: name,
		get: func(s *config.V1ServerConfig) *string {
			if p := getProvider(s); p != nil {
				return &p.ClientSecret
			}
			return nil
		},
		block: func(s *config.V1ServerConfig) any {
			if p := getProvider(s); p != nil {
				return p
			}
			return nil
		},
	}
}

func oauthWeb(o *config.V1OAuthConfig) *config.V1OAuthClientConfig    { return o.Web }
func oauthCLI(o *config.V1OAuthConfig) *config.V1OAuthClientConfig    { return o.CLI }
func oauthDevice(o *config.V1OAuthConfig) *config.V1OAuthClientConfig { return o.Device }
func oauthGoogle(c *config.V1OAuthClientConfig) *config.V1OAuthProviderConfig {
	return c.Google
}
func oauthGitHub(c *config.V1OAuthClientConfig) *config.V1OAuthProviderConfig {
	return c.GitHub
}

func gitHubAppBlock(s *config.V1ServerConfig) any {
	if s.GitHubApp != nil {
		return s.GitHubApp
	}
	return nil
}

// maskedServerFields is the single list of scalar secrets in the server
// config. maskSensitiveFields masks exactly these (plus every non-empty
// notification channel param) and restoreMaskedServerSecrets restores
// exactly these (plus channel params), so the two cannot drift. Add new
// secrets here (and to the golden list in admin_settings_mask_test.go).
var maskedServerFields = []maskedServerField{
	oauthSecretField("server.oauth.web.google.client_secret", oauthWeb, oauthGoogle),
	oauthSecretField("server.oauth.web.github.client_secret", oauthWeb, oauthGitHub),
	oauthSecretField("server.oauth.cli.google.client_secret", oauthCLI, oauthGoogle),
	oauthSecretField("server.oauth.cli.github.client_secret", oauthCLI, oauthGitHub),
	oauthSecretField("server.oauth.device.google.client_secret", oauthDevice, oauthGoogle),
	oauthSecretField("server.oauth.device.github.client_secret", oauthDevice, oauthGitHub),
	{name: "server.auth.dev_token", get: func(s *config.V1ServerConfig) *string {
		if s.Auth == nil {
			return nil
		}
		return &s.Auth.DevToken
	}, block: func(s *config.V1ServerConfig) any {
		if s.Auth != nil {
			return s.Auth
		}
		return nil
	}},
	{name: "server.broker.broker_token", get: func(s *config.V1ServerConfig) *string {
		if s.Broker == nil {
			return nil
		}
		return &s.Broker.BrokerToken
	}, block: func(s *config.V1ServerConfig) any {
		if s.Broker != nil {
			return s.Broker
		}
		return nil
	}},
	// Database URL may contain credentials.
	{name: "server.database.url", get: func(s *config.V1ServerConfig) *string {
		if s.Database == nil {
			return nil
		}
		return &s.Database.URL
	}, block: func(s *config.V1ServerConfig) any {
		if s.Database != nil {
			return s.Database
		}
		return nil
	}},
	{name: "server.secrets.gcp_credentials", get: func(s *config.V1ServerConfig) *string {
		if s.Secrets == nil {
			return nil
		}
		return &s.Secrets.GCPCredentials
	}, block: func(s *config.V1ServerConfig) any {
		if s.Secrets != nil {
			return s.Secrets
		}
		return nil
	}},
	{name: "server.github_app.private_key", get: func(s *config.V1ServerConfig) *string {
		if s.GitHubApp == nil {
			return nil
		}
		return &s.GitHubApp.PrivateKey
	}, block: gitHubAppBlock},
	{name: "server.github_app.webhook_secret", get: func(s *config.V1ServerConfig) *string {
		if s.GitHubApp == nil {
			return nil
		}
		return &s.GitHubApp.WebhookSecret
	}, block: gitHubAppBlock},
}

// maskSensitiveFields redacts secrets from the response before sending to the
// client. Its inverse for the PUT path is restoreMaskedServerSecrets; both are
// driven by maskedServerFields.
func maskSensitiveFields(resp *ServerConfigResponse) {
	if resp.Server == nil {
		return
	}
	for _, f := range maskedServerFields {
		if p := f.get(resp.Server); p != nil && *p != "" {
			*p = maskedValue
		}
	}
	// Notification channel params may hold webhook URLs or tokens: mask every
	// non-empty one (empty values stay empty, as for the scalar fields).
	for i := range resp.Server.NotificationChannels {
		for k, v := range resp.Server.NotificationChannels[i].Params {
			if v != "" {
				resp.Server.NotificationChannels[i].Params[k] = maskedValue
			}
		}
	}
}

// restoreMaskedServerSecrets is the inverse of maskSensitiveFields for the
// PUT path. A field in incoming that still holds maskedValue (because the
// client sent back what GET returned) is replaced with the value from
// stored, the server config the GET was built from. A real value is left
// alone, so it replaces the stored one as usual.
//
// A masked value is restored only into the provably same block: the
// incoming block must be exactly equal (as JSON) to the stored block after
// the same masking GET applies. So every non-secret field must be
// unchanged, a placeholder in a non-secret field never matches a real
// stored value, and new fields are covered without being listed. The block
// is the struct the secret sits in (OAuth provider, GitHub App, auth,
// broker, database, secrets) or the notification channel (matched by
// matchStoredChannel; each stored channel at most once). Editing any field
// of such a block means sending its secrets in clear.
//
// Anything else is an error (the caller answers 400): persisting the literal
// placeholder would silently break the setting, restoring into a different
// owner would attach one secret to another, and dropping the value would
// hide that the client's input was not applied.
func restoreMaskedServerSecrets(incoming, stored *config.V1ServerConfig) error {
	if incoming == nil {
		return nil
	}
	if stored == nil {
		stored = &config.V1ServerConfig{}
	}
	// What GET showed for stored.
	shown, err := maskedCopy(stored)
	if err != nil {
		return err
	}

	// Check every field before restoring any: two secrets can share a block
	// (the GitHub App), and restoring one would change the block the other
	// is compared by.
	type restore struct {
		dst *string
		val string
	}
	var restores []restore
	for _, f := range maskedServerFields {
		p := f.get(incoming)
		if p == nil || *p != maskedValue {
			continue
		}
		sp := f.get(stored)
		if err := checkStoredSecret(f.name, sp); err != nil {
			return err
		}
		// Fail closed: a field without a block cannot prove it is unchanged.
		if f.block == nil || !jsonEqual(f.block(incoming), f.block(shown)) {
			return fmt.Errorf("%s is the masked placeholder %q but other fields of its block changed; send the real value", f.name, maskedValue)
		}
		restores = append(restores, restore{p, *sp})
	}

	// Match every channel against the unmodified request first: the
	// position rule compares the whole list.
	used := make(map[int]bool)
	matches := make(map[int]int)
	for i := range incoming.NotificationChannels {
		ch := &incoming.NotificationChannels[i]
		if !channelHasMaskedParam(ch) {
			continue
		}
		j := matchStoredChannel(incoming.NotificationChannels, shown.NotificationChannels, i)
		if j < 0 || used[j] {
			return fmt.Errorf("server.notification_channels[%d] (type %q) has masked params but does not match a stored channel unchanged; send all params in clear when changing a channel", i, ch.Type)
		}
		used[j] = true
		matches[i] = j
		for k, v := range ch.Params {
			if v == maskedValue {
				sv := stored.NotificationChannels[j].Params[k]
				if err := checkStoredSecret(fmt.Sprintf("server.notification_channels[%d].params.%s", i, k), &sv); err != nil {
					return err
				}
			}
		}
	}

	// Everything checked: restore.
	for i, j := range matches {
		ch := &incoming.NotificationChannels[i]
		// Build a fresh map so the request never aliases stored state.
		params := make(map[string]string, len(ch.Params))
		for k, v := range ch.Params {
			if v == maskedValue {
				v = stored.NotificationChannels[j].Params[k]
			}
			params[k] = v
		}
		ch.Params = params
	}
	for _, r := range restores {
		*r.dst = r.val
	}
	return nil
}

// maskedCopy returns a deep copy of s with GET's masking applied.
func maskedCopy(s *config.V1ServerConfig) (*config.V1ServerConfig, error) {
	data, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	var c config.V1ServerConfig
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	maskSensitiveFields(&ServerConfigResponse{Server: &c})
	return &c, nil
}

// checkStoredSecret reports why a stored value cannot stand in for a masked
// one: there is none, or it is itself the placeholder (written by a save
// before masked values were restored).
func checkStoredSecret(name string, sp *string) error {
	switch {
	case sp == nil || *sp == "":
		return fmt.Errorf("%s is the masked placeholder %q but no value is stored; send the real value", name, maskedValue)
	case *sp == maskedValue:
		return fmt.Errorf("%s: the stored value is also the masked placeholder (lost by an earlier save); send the real value", name)
	}
	return nil
}

func channelHasMaskedParam(ch *config.V1NotificationChannelConfig) bool {
	for _, v := range ch.Params {
		if v == maskedValue {
			return true
		}
	}
	return false
}

// jsonEqual reports whether a and b have equal JSON forms. Comparing the
// JSON form covers every serialized field, including ones added later. A
// nil block never matches.
func jsonEqual(a, b any) bool {
	if a == nil || b == nil {
		return false
	}
	av, err1 := toJSONValue(a)
	bv, err2 := toJSONValue(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return reflect.DeepEqual(av, bv)
}

func toJSONValue(v any) (any, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out any
	err = json.Unmarshal(data, &out)
	return out, err
}

// matchStoredChannel returns the index in shown (the stored channels as GET
// showed them, i.e. masked) of the channel incoming[i] was read from, or -1.
// Channels have no name or ID (#444/#387), so a match must be the provably
// same channel, compared exactly with jsonEqual:
//  1. Position: only when the whole list is unchanged, i.e. same length and
//     every incoming[j] equal to shown[j]. This is the unedited round trip
//     and the only way to tell two channels of the same type apart.
//  2. Otherwise the single shown channel of the same type, and only if
//     incoming[i] equals it.
//
// Anything else (an edited channel, a delete plus add, or a reorder of
// same-type channels) returns -1 and the caller rejects the request:
// guessing could attach one channel's webhook to another. The caller also
// rejects a second incoming channel matching the same stored one.
func matchStoredChannel(incoming, shown []config.V1NotificationChannelConfig, i int) int {
	if len(incoming) == len(shown) {
		unchanged := true
		for j := range incoming {
			if !jsonEqual(&incoming[j], &shown[j]) {
				unchanged = false
				break
			}
		}
		if unchanged {
			return i
		}
	}
	found := -1
	for j := range shown {
		if shown[j].Type != incoming[i].Type {
			continue
		}
		if found >= 0 {
			return -1
		}
		found = j
	}
	if found < 0 || !jsonEqual(&incoming[i], &shown[found]) {
		return -1
	}
	return found
}

// readSettingsFileServer returns the server section of the global
// settings.yaml as GET /api/v1/admin/server-config reads it, or nil when the
// file does not exist.
func readSettingsFileServer() (*config.V1ServerConfig, error) {
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(globalDir, "settings.yaml"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var vs config.VersionedSettings
	if err := yamlv3.Unmarshal(data, &vs); err != nil {
		return nil, err
	}
	return vs.Server, nil
}

// storedServerConfigDB rebuilds, unmasked, the server config that
// handleGetServerConfigDB returns: settings.yaml overlaid with the
// operational settings snapshot. It is what masked PUT values are restored
// from in DB mode.
func storedServerConfigDB(ops *OperationalSettings) (*config.V1ServerConfig, error) {
	server, err := readSettingsFileServer()
	if err != nil {
		return nil, err
	}
	resp := ServerConfigResponse{Server: server}
	applySnapshotToResponse(&resp, ops.Snapshot())
	return resp.Server, nil
}

// serverConfigFromRaw decodes the server section of an already-parsed
// settings.yaml map, the way readSettingsFileServer (and GET) decode it.
func serverConfigFromRaw(raw map[string]interface{}) (*config.V1ServerConfig, error) {
	srv, ok := raw["server"]
	if !ok || srv == nil {
		return nil, nil
	}
	data, err := yamlv3.Marshal(srv)
	if err != nil {
		return nil, err
	}
	var out config.V1ServerConfig
	if err := yamlv3.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
