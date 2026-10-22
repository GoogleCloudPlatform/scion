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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fileFederationSeed is a settings.yaml with every federation key set,
// next to another server section that a federation save must not touch.
// Federation is stored disabled so the reload after the save installs no
// authenticator.
const fileFederationSeed = `schema_version: "1"
server:
  hub:
    public_url: https://hub.example.com
  federation:
    enabled: false
    trusted_issuers:
      - issuer_url: https://issuer-a.example.com
        issuer_type: hub
      - issuer_url: https://issuer-b.example.com
        issuer_type: hub
    algorithms:
      - RS256
      - ES256
    refresh_interval: 2h
    debounce_interval: 10s
`

// storedFileFederation returns server.federation from a decoded
// settings.yaml, or nil when it is absent.
func storedFileFederation(t *testing.T, doc map[string]interface{}) map[string]interface{} {
	t.Helper()
	srv, ok := doc["server"].(map[string]interface{})
	require.True(t, ok, "server section missing: %v", doc)
	fed, _ := srv["federation"].(map[string]interface{})
	return fed
}

// A file-mode federation save that sends one key keeps the keys it omits,
// as the DB-mode merge does: the Federation page sends only the fields
// that changed (ptone/scion#3913).
func TestPutServerConfig_FileMode_FederationSaveKeepsOmittedKeys(t *testing.T) {
	doc := putFileSettings(t, fileFederationSeed, `{"federation":{"refresh_interval":"3h"}}`)
	fed := storedFileFederation(t, doc)
	require.NotNil(t, fed)

	assert.Equal(t, "3h", fed["refresh_interval"])
	assert.Equal(t, false, fed["enabled"])
	assert.Equal(t, []interface{}{"RS256", "ES256"}, fed["algorithms"])
	assert.Equal(t, "10s", fed["debounce_interval"])
	issuers, ok := fed["trusted_issuers"].([]interface{})
	require.True(t, ok)
	assert.Len(t, issuers, 2)

	srv := doc["server"].(map[string]interface{})
	hub, ok := srv["hub"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "https://hub.example.com", hub["public_url"])
}

// trusted_issuers is a list and is replaced as a whole when sent.
func TestPutServerConfig_FileMode_FederationIssuersReplacedWhole(t *testing.T) {
	doc := putFileSettings(t, fileFederationSeed,
		`{"federation":{"trusted_issuers":[{"issuer_url":"https://issuer-c.example.com","issuer_type":"hub"}]}}`)
	fed := storedFileFederation(t, doc)
	require.NotNil(t, fed)

	issuers, ok := fed["trusted_issuers"].([]interface{})
	require.True(t, ok)
	require.Len(t, issuers, 1)
	assert.Equal(t, "https://issuer-c.example.com", issuers[0].(map[string]interface{})["issuer_url"])
	assert.Equal(t, "2h", fed["refresh_interval"])
	assert.Equal(t, false, fed["enabled"])
}

// A sent false is stored, not dropped; a zero value or null clears the key
// and leaves the others.
func TestPutServerConfig_FileMode_FederationExplicitFalseAndClears(t *testing.T) {
	doc := putFileSettings(t, fileFederationSeed,
		`{"federation":{"enabled":false,"algorithms":[],"debounce_interval":null,"refresh_interval":""}}`)
	fed := storedFileFederation(t, doc)
	require.NotNil(t, fed)

	assert.Equal(t, false, fed["enabled"])
	assert.NotContains(t, fed, "algorithms")
	assert.NotContains(t, fed, "debounce_interval")
	assert.NotContains(t, fed, "refresh_interval")
	issuers, ok := fed["trusted_issuers"].([]interface{})
	require.True(t, ok)
	assert.Len(t, issuers, 2)
}

// A save without a federation object leaves the stored section alone.
func TestPutServerConfig_FileMode_FederationNotSentUnchanged(t *testing.T) {
	doc := putFileSettings(t, fileFederationSeed, `{"default_template":"claude"}`)
	fed := storedFileFederation(t, doc)
	require.NotNil(t, fed)

	assert.Equal(t, false, fed["enabled"])
	assert.Equal(t, "2h", fed["refresh_interval"])
	assert.Equal(t, "10s", fed["debounce_interval"])
	assert.Equal(t, []interface{}{"RS256", "ES256"}, fed["algorithms"])
}

// A federation save on a settings.yaml with no federation section stores
// only the keys sent.
func TestPutServerConfig_FileMode_FederationSaveWithNoStoredSection(t *testing.T) {
	doc := putFileSettings(t, "schema_version: \"1\"\n", `{"federation":{"refresh_interval":"2h"}}`)
	fed := storedFileFederation(t, doc)
	require.NotNil(t, fed)

	assert.Equal(t, map[string]interface{}{"refresh_interval": "2h"}, fed)
}
