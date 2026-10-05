//go:build !no_sqlite

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

// Property-style test over the workstation server-config PUT (review
// phase4-r4 B): bodies are generated from combinations of Layer-0 / file-only
// leaves (zero, non-zero, null, absent) and block nulls, against several seed
// files, and every response must satisfy:
//
//	(i)   every sent leaf is reflected in the settings the hub loads, or the
//	      request returned 4xx naming keys (and wrote nothing);
//	(ii)  every leaf not sent (and not under a sent null) is unchanged,
//	      including the effective CORS switch when a cors block is created;
//	(iii) broker_id/broker_token survive;
//	(iv)  a pure echo of the GET body writes nothing.

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	yamlv3 "gopkg.in/yaml.v3"
)

type propLeaf struct {
	path    []string
	zero    interface{}
	nonZero interface{}
}

var propLeaves = []propLeaf{
	{[]string{"server", "log_format"}, "", "json"},
	{[]string{"server", "log_level"}, "", "debug"},
	{[]string{"server", "hub", "port"}, 0, 9999},
	{[]string{"server", "hub", "cors", "enabled"}, false, true},
	{[]string{"server", "hub", "cors", "allowed_origins"}, []string{}, []string{"https://x.example"}},
	{[]string{"server", "broker", "auto_provide"}, false, true},
	{[]string{"server", "broker", "port"}, 0, 9800},
	{[]string{"server", "auth", "dev_mode"}, false, true},
	{[]string{"server", "storage", "bucket"}, "", "bkt"},
	{[]string{"server", "message_broker", "enabled"}, false, true},
	{[]string{"active_profile"}, "", "dev"},
	{[]string{"workspace_path"}, "", "/ws"},
	{[]string{"auto_inject_gcloud_adc"}, false, true},
}

// Block nulls, including ancestors of the hub-owned broker identity.
var propBlockNulls = [][]string{
	{"server", "storage"},
	{"server", "hub", "cors"},
	{"server", "broker"},
	{"server"},
}

var propSeeds = map[string]string{
	"empty": "",
	"minimal": `schema_version: "1"
server:
  broker:
    broker_id: b-1
    broker_token: tok-1
`,
	"full": `schema_version: "1"
# operator comment
active_profile: local
workspace_path: /work
auto_inject_gcloud_adc: true
server:
  log_format: text
  log_level: info
  hub:
    port: 9810
    cors:
      enabled: true
      allowed_origins:
        - https://a.example
  broker:
    enabled: true
    port: 9810
    auto_provide: false
    broker_id: b-1
    broker_token: tok-1
  auth:
    dev_mode: true
  storage:
    provider: gcs
    bucket: my-bucket
  message_broker:
    enabled: true
    type: inprocess
`,
	"empty-blocks": `schema_version: "1"
server:
  hub:
    cors:
  auth: {}
  storage: {}
  broker:
    broker_id: b-1
    broker_token: tok-1
`,
}

type propSent struct {
	path  []string
	value interface{} // nil means JSON null
	null  bool
}

func propBody(sent []propSent) string {
	root := map[string]interface{}{}
	for _, s := range sent {
		m := root
		for _, k := range s.path[:len(s.path)-1] {
			next, ok := m[k].(map[string]interface{})
			if !ok {
				next = map[string]interface{}{}
				m[k] = next
			}
			m = next
		}
		if s.null {
			m[s.path[len(s.path)-1]] = nil
		} else {
			m[s.path[len(s.path)-1]] = s.value
		}
	}
	b, _ := json.Marshal(root)
	return string(b)
}

// propTyped decodes settings bytes into VersionedSettings.
func propTyped(t *testing.T, data []byte) *config.VersionedSettings {
	t.Helper()
	var vs config.VersionedSettings
	if err := yamlv3.Unmarshal(data, &vs); err != nil {
		t.Fatalf("decode: %v\n%s", err, data)
	}
	return &vs
}

func propValueAt(vs *config.VersionedSettings, path []string) reflect.Value {
	v, _ := valueAtJSONPath(reflect.ValueOf(vs).Elem(), path)
	return v
}

func propEquivalent(a reflect.Value, b interface{}) bool {
	return jsonValuesEquivalent(a, reflect.ValueOf(b))
}

// propBodies returns every single-leaf body (zero, non-zero, null) and every
// block null, plus n random combinations of up to 5 leaves.
func propBodies(rng *rand.Rand, n int) [][]propSent {
	var out [][]propSent
	for _, l := range propLeaves {
		out = append(out,
			[]propSent{{path: l.path, value: l.zero}},
			[]propSent{{path: l.path, value: l.nonZero}},
			[]propSent{{path: l.path, null: true}},
		)
	}
	for _, b := range propBlockNulls {
		out = append(out, []propSent{{path: b, null: true}})
	}
	for i := 0; i < n; i++ {
		k := 1 + rng.Intn(5)
		perm := rng.Perm(len(propLeaves))[:k]
		var body []propSent
		for _, idx := range perm {
			l := propLeaves[idx]
			switch rng.Intn(3) {
			case 0:
				body = append(body, propSent{path: l.path, value: l.zero})
			case 1:
				body = append(body, propSent{path: l.path, value: l.nonZero})
			default:
				body = append(body, propSent{path: l.path, null: true})
			}
		}
		// A JSON object cannot carry both a leaf and a null on one of its
		// ancestors; keep the first of any such conflict.
		out = append(out, dedupePropBody(body))
	}
	return out
}

func dedupePropBody(body []propSent) []propSent {
	var out []propSent
	for _, s := range body {
		conflict := false
		for _, o := range out {
			if (o.null && pathHasPrefixPath(s.path, o.path)) || (s.null && pathHasPrefixPath(o.path, s.path)) {
				conflict = true
			}
		}
		if !conflict {
			out = append(out, s)
		}
	}
	return out
}

func propUnderSentNull(path []string, sent []propSent) bool {
	for _, s := range sent {
		if s.null && pathHasPrefixPath(path, s.path) {
			return true
		}
	}
	return false
}

func TestWorkstation_PutServerConfig_Property(t *testing.T) {
	rng := rand.New(rand.NewSource(20261005))
	bodies := propBodies(rng, 120)
	seedNames := make([]string, 0, len(propSeeds))
	for name := range propSeeds {
		seedNames = append(seedNames, name)
	}
	sort.Strings(seedNames)

	cases := 0
	for _, seedName := range seedNames {
		seed := propSeeds[seedName]
		t.Run(seedName, func(t *testing.T) {
			settingsPath := tempSettingsHome(t)
			srv, _, _ := newSQLiteHubInMode(t, true, nil)
			write := func() {
				if seed == "" {
					_ = os.Remove(settingsPath)
					return
				}
				if err := os.WriteFile(settingsPath, []byte(seed), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			read := func() []byte {
				data, err := os.ReadFile(settingsPath)
				if err != nil {
					return nil
				}
				return data
			}

			// (iv) a pure echo of the GET body writes nothing.
			write()
			getRR := httptest.NewRecorder()
			srv.handleAdminServerConfig(getRR, adminRequest(http.MethodGet, "/api/v1/admin/server-config", ""))
			before := read()
			if rr := putServerConfig(t, srv, getRR.Body.String()); rr.Code != http.StatusOK {
				t.Fatalf("echo: %d %s", rr.Code, rr.Body.String())
			}
			if after := read(); string(after) != string(before) {
				t.Errorf("(iv) echo rewrote settings.yaml:\n%s", after)
			}
			cases++

			for _, sent := range bodies {
				write()
				before := read()
				beforeTyped := propTyped(t, before)
				beforeEff, err := config.SettingsFileEffective(before)
				if err != nil {
					t.Fatal(err)
				}
				body := propBody(sent)
				rr := putServerConfig(t, srv, body)
				after := read()
				cases++

				if rr.Code != http.StatusOK {
					// (i) a 4xx must name keys and write nothing.
					var resp struct {
						Keys  []string    `json:"keys"`
						Error interface{} `json:"error"`
					}
					_ = json.Unmarshal(rr.Body.Bytes(), &resp)
					if rr.Code < 400 || rr.Code >= 500 || len(resp.Keys) == 0 {
						t.Errorf("%s: %d without naming keys: %s", body, rr.Code, rr.Body.String())
					}
					if string(after) != string(before) {
						t.Errorf("%s: rejected PUT wrote settings.yaml", body)
					}
					continue
				}

				afterTyped := propTyped(t, after)
				afterEff, err := config.SettingsFileEffective(after)
				if err != nil {
					t.Fatal(err)
				}

				// (i) every sent leaf is reflected.
				for _, s := range sent {
					got := propValueAt(afterTyped, s.path)
					switch {
					case s.null && strings.Join(s.path, ".") != "server" && strings.Join(s.path, ".") != "server.broker":
						if !propEquivalent(got, nil) {
							t.Errorf("%s: (i) %s should be cleared, got %v", body, strings.Join(s.path, "."), got)
						}
					case !s.null:
						if !propEquivalent(got, s.value) {
							t.Errorf("%s: (i) %s = %v, sent %v", body, strings.Join(s.path, "."), got, s.value)
						}
					}
				}
				// Effective checks for the leaves whose meaning the loader
				// changes (cors switch and lists, koanf-merged top keys).
				for _, s := range sent {
					if s.null {
						continue
					}
					switch strings.Join(s.path, ".") {
					case "server.hub.cors.enabled":
						if afterEff.Server.Hub.CORSEnabled != s.value.(bool) {
							t.Errorf("%s: (i) effective hub CORS = %v, sent %v", body, afterEff.Server.Hub.CORSEnabled, s.value)
						}
					case "server.hub.cors.allowed_origins":
						if fmt.Sprint(afterEff.Server.Hub.CORSAllowedOrigins) != fmt.Sprint(s.value) {
							t.Errorf("%s: (i) effective origins = %v, sent %v", body, afterEff.Server.Hub.CORSAllowedOrigins, s.value)
						}
					case "active_profile":
						if afterEff.ActiveProfile != s.value.(string) {
							t.Errorf("%s: (i) effective active_profile = %q, sent %q", body, afterEff.ActiveProfile, s.value)
						}
					case "workspace_path":
						if afterEff.WorkspacePath != s.value.(string) {
							t.Errorf("%s: (i) effective workspace_path = %q, sent %q", body, afterEff.WorkspacePath, s.value)
						}
					}
				}

				// (ii) every unsent leaf is unchanged.
				sentPaths := map[string]bool{}
				for _, s := range sent {
					sentPaths[strings.Join(s.path, ".")] = true
				}
				for _, l := range propLeaves {
					if sentPaths[strings.Join(l.path, ".")] || propUnderSentNull(l.path, sent) {
						continue
					}
					// cors.enabled is checked on the effective switch below:
					// creating the block writes the current value (on), which
					// differs from the typed zero of the absent block.
					if strings.Join(l.path, ".") == "server.hub.cors.enabled" && (beforeTyped.Server == nil || beforeTyped.Server.Hub == nil || beforeTyped.Server.Hub.CORS == nil) {
						continue
					}
					b, a := propValueAt(beforeTyped, l.path), propValueAt(afterTyped, l.path)
					if !jsonValuesEquivalent(b, a) {
						t.Errorf("%s: (ii) unsent %s changed %v -> %v", body, strings.Join(l.path, "."), b, a)
					}
				}
				if !sentPaths["server.hub.cors.enabled"] && !propUnderSentNull([]string{"server", "hub", "cors", "enabled"}, sent) &&
					afterEff.Server.Hub.CORSEnabled != beforeEff.Server.Hub.CORSEnabled {
					t.Errorf("%s: (ii) effective hub CORS changed %v -> %v without enabled being sent",
						body, beforeEff.Server.Hub.CORSEnabled, afterEff.Server.Hub.CORSEnabled)
				}

				// (iii) hub-owned broker identity survives.
				for _, p := range hubOwnedBrokerPaths {
					b, a := propValueAt(beforeTyped, p), propValueAt(afterTyped, p)
					if !jsonValuesEquivalent(b, a) {
						t.Errorf("%s: (iii) %s changed %v -> %v", body, strings.Join(p, "."), b, a)
					}
				}
			}
		})
	}
	t.Logf("property cases: %d (bodies per seed: %d + 1 echo, seeds: %d)", cases, len(bodies), len(propSeeds))
}
