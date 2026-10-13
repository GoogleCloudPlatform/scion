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

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSettingsSchema_OpenObjectsDocumentGoFields checks that every Go field
// reachable from VersionedSettings has a schema property, including in the
// objects the schema leaves open (no additionalProperties: false).
// TestSettingsSchema_NoDriftFromGoTypes only reports a missing property on
// closed objects, because an open object still accepts the key; this test
// keeps the open objects documented too, without closing them.
func TestSettingsSchema_OpenObjectsDocumentGoFields(t *testing.T) {
	raw, err := GetSettingsSchemaJSON("1")
	require.NoError(t, err)
	var root map[string]any
	require.NoError(t, json.Unmarshal(raw, &root))

	w := &schemaDriftWalker{root: root, seen: map[string]bool{}, reportOpen: true}
	w.walk("", reflect.TypeOf(VersionedSettings{}), root)

	var missing []string
	for _, d := range w.drift {
		if !strings.HasPrefix(d, "go-only: ") {
			continue // other drift kinds are TestSettingsSchema_NoDriftFromGoTypes' job
		}
		if _, ok := schemaDriftAllowList[d]; ok {
			continue
		}
		missing = append(missing, d)
	}
	sort.Strings(missing)
	require.Empty(t, missing, "Go settings fields with no settings-v1 schema property (document them in the schema):\n  %s", strings.Join(missing, "\n  "))
}

// schemaOpenObjectsDoc sets every field documented for ptone/scion#3024
// with a valid value.
const schemaOpenObjectsDoc = `schema_version: "1"
server:
  hub:
    gcp_iam_check_mode: enforce
    gcp_iam_deny_unknown_policy: fail-closed
    disable_legacy_storage_fallback: true
  database:
    driver: postgres
    url: postgres://db.example.com/scion
    max_open_conns: 20
    max_idle_conns: 5
    conn_max_lifetime: 30m
    conn_max_idle_time: 5m
  auth:
    mode: proxy
    proxy:
      provider: jwt
      iap:
        audience: /projects/1/global/backendServices/2
        issuer: https://issuer.example.com
        jwks_url: https://issuer.example.com/jwks
      jwt:
        header: X-Assertion
        algorithm: RS256
        issuer: https://issuer.example.com
        audience: scion
        jwks_url: https://issuer.example.com/jwks
        jwks_file: /etc/scion/jwks.json
        public_key_file: /etc/scion/key.pem
        claims:
          email: email
          subject: sub
          display_name: name
          domain: hd
      require_trusted_proxy_ip: true
    transport:
      mode: iap
      oidc_audience: scion-hub
      platform_auth_sa: transport@example.iam.gserviceaccount.com
    username: dev
    display_name: Dev User
    email: dev@example.com
  secrets:
    backend: gcpsm
    gcp_replication_locations: [us-east1, europe-west1]
harness_configs:
  claude:
    harness: claude
    volumes:
      - target: /mnt/nfs
        type: nfs
        server: 10.0.0.2
        source: /exports/data
profiles:
  default:
    runtime: docker
    volumes:
      - target: /mnt/crv
        type: cloudrun-volume
        volume_name: shared
    harness_overrides:
      claude:
        volumes:
          - target: /mnt/gke
            type: gke-shared-volume
            volume_name: filestore
`

// TestValidateSettings_OpenObjectFieldsValidateAndLoad checks that the newly
// documented fields validate with valid values, and that the same document
// loads into the Go types with the values intact (so the schema keys are the
// keys the loader reads).
func TestValidateSettings_OpenObjectFieldsValidateAndLoad(t *testing.T) {
	errs, err := ValidateSettings([]byte(schemaOpenObjectsDoc), "1")
	require.NoError(t, err)
	assert.Empty(t, errs, "documented open-object fields should validate, got: %v", errs)

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "settings.yaml"), []byte(schemaOpenObjectsDoc), 0o644))
	vs, err := loadVersionedSettingsFileOnly(dir)
	require.NoError(t, err)
	require.NotNil(t, vs.Server)

	hub := vs.Server.Hub
	require.NotNil(t, hub)
	assert.Equal(t, "enforce", hub.GCPIAMCheckMode)
	assert.Equal(t, "fail-closed", hub.GCPIAMDenyUnknownPolicy)
	require.NotNil(t, hub.DisableLegacyStorageFallback)
	assert.True(t, *hub.DisableLegacyStorageFallback)

	db := vs.Server.Database
	require.NotNil(t, db)
	assert.Equal(t, 20, db.MaxOpenConns)
	assert.Equal(t, 5, db.MaxIdleConns)
	assert.Equal(t, "30m", db.ConnMaxLifetime)
	assert.Equal(t, "5m", db.ConnMaxIdleTime)

	auth := vs.Server.Auth
	require.NotNil(t, auth)
	assert.Equal(t, "proxy", auth.Mode)
	require.NotNil(t, auth.Proxy)
	assert.Equal(t, "jwt", auth.Proxy.Provider)
	assert.True(t, auth.Proxy.RequireTrustedProxyIP)
	require.NotNil(t, auth.Proxy.IAP)
	assert.Equal(t, "https://issuer.example.com/jwks", auth.Proxy.IAP.JWKSURL)
	require.NotNil(t, auth.Proxy.JWT)
	assert.Equal(t, "/etc/scion/key.pem", auth.Proxy.JWT.PublicKeyFile)
	require.NotNil(t, auth.Proxy.JWT.Claims)
	assert.Equal(t, "hd", auth.Proxy.JWT.Claims.Domain)
	require.NotNil(t, auth.Transport)
	assert.Equal(t, "iap", auth.Transport.Mode)
	assert.Equal(t, "transport@example.iam.gserviceaccount.com", auth.Transport.PlatformAuthSA)
	assert.Equal(t, "dev", auth.Username)
	assert.Equal(t, "Dev User", auth.DisplayName)
	assert.Equal(t, "dev@example.com", auth.Email)

	require.NotNil(t, vs.Server.Secrets)
	assert.Equal(t, []string{"us-east1", "europe-west1"}, vs.Server.Secrets.GCPReplicationLocations)

	hv := vs.HarnessConfigs["claude"].Volumes
	require.Len(t, hv, 1)
	assert.Equal(t, "nfs", hv[0].Type)
	assert.Equal(t, "10.0.0.2", hv[0].Server)
	pv := vs.Profiles["default"].Volumes
	require.Len(t, pv, 1)
	assert.Equal(t, "shared", pv[0].VolumeName)
	ov := vs.Profiles["default"].HarnessOverrides["claude"].Volumes
	require.Len(t, ov, 1)
	assert.Equal(t, "gke-shared-volume", ov[0].Type)
	assert.Equal(t, "filestore", ov[0].VolumeName)
}

// TestValidateSettings_VolumeMountTypes checks that every volume type
// api.VolumeMount.Validate accepts also passes the schema, and that an
// unknown type is still rejected.
func TestValidateSettings_VolumeMountTypes(t *testing.T) {
	for _, vt := range []string{"local", "gcs", "nfs", "cloudrun-volume", "gke-shared-volume"} {
		t.Run(vt, func(t *testing.T) {
			doc := "schema_version: \"1\"\nprofiles:\n  p:\n    runtime: docker\n    volumes:\n      - target: /mnt/x\n        type: " + vt + "\n"
			errs, err := ValidateSettings([]byte(doc), "1")
			require.NoError(t, err)
			assert.Empty(t, errs, "volume type %q should validate, got: %v", vt, errs)
		})
	}
	t.Run("unknown type rejected", func(t *testing.T) {
		doc := "schema_version: \"1\"\nprofiles:\n  p:\n    runtime: docker\n    volumes:\n      - target: /mnt/x\n        type: hostpath\n"
		errs, err := ValidateSettings([]byte(doc), "1")
		require.NoError(t, err)
		assert.NotEmpty(t, errs, "an unknown volume type should be rejected")
	})
}

// TestValidateSettings_OpenObjectFieldsRejectWrongType checks that the newly
// documented fields are typed: a value of the wrong JSON type is reported,
// naming the key.
func TestValidateSettings_OpenObjectFieldsRejectWrongType(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{"database.max_open_conns non-integer", "server:\n  database:\n    max_open_conns: ten\n", "server/database/max_open_conns"},
		{"database.max_idle_conns non-integer", "server:\n  database:\n    max_idle_conns: ten\n", "server/database/max_idle_conns"},
		{"database.conn_max_lifetime non-string", "server:\n  database:\n    conn_max_lifetime: [30m]\n", "server/database/conn_max_lifetime"},
		{"hub.disable_legacy_storage_fallback non-boolean", "server:\n  hub:\n    disable_legacy_storage_fallback: \"yes\"\n", "server/hub/disable_legacy_storage_fallback"},
		{"hub.gcp_iam_check_mode non-string", "server:\n  hub:\n    gcp_iam_check_mode: [enforce]\n", "server/hub/gcp_iam_check_mode"},
		{"auth.mode non-string", "server:\n  auth:\n    mode: [proxy]\n", "server/auth/mode"},
		{"auth.proxy non-object", "server:\n  auth:\n    proxy: iap\n", "server/auth/proxy"},
		{"auth.proxy.require_trusted_proxy_ip non-boolean", "server:\n  auth:\n    proxy:\n      require_trusted_proxy_ip: \"yes\"\n", "server/auth/proxy/require_trusted_proxy_ip"},
		{"auth.transport non-object", "server:\n  auth:\n    transport: iap\n", "server/auth/transport"},
		{"secrets.gcp_replication_locations non-string item", "server:\n  secrets:\n    gcp_replication_locations: [{region: us-east1}]\n", "server/secrets/gcp_replication_locations/0"},
		{"volume server non-string", "profiles:\n  p:\n    runtime: docker\n    volumes:\n      - target: /mnt/x\n        type: nfs\n        server: [10.0.0.2]\n", "profiles/p/volumes/0/server"},
		{"volume volume_name non-string", "profiles:\n  p:\n    runtime: docker\n    volumes:\n      - target: /mnt/x\n        type: cloudrun-volume\n        volume_name: [shared]\n", "profiles/p/volumes/0/volume_name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := []byte("schema_version: \"1\"\n" + tt.yaml)
			errs, err := ValidateSettings(data, "1")
			require.NoError(t, err)
			require.NotEmpty(t, errs, "expected a validation error")
			found := false
			for _, e := range errs {
				if strings.Contains(e.Path+" "+e.Message, tt.want) {
					found = true
				}
			}
			assert.True(t, found, "expected an error naming %q, got: %v", tt.want, errs)
		})
	}
}
