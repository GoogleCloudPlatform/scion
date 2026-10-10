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

// Property-style test over the workstation server-config PUT, with an oracle
// independent of the implementation.
//
// Bodies are generated from combinations of Layer-0 / file-only leaves
// (zero, non-zero, null, absent) and block nulls against several seed files.
// Every generated value is valid, so every PUT must return 200. The oracle
// does not use the implementation's own model: for each body it builds the
// expected settings.yaml by plain map edits on the seed (set the value as
// sent; null deletes, except that a null on a block holding the hub-owned
// broker identity keeps it; creating a cors block without enabled keeps the
// current CORS switch, read from the real loader), then loads the actual
// file and the expected file with the real loaders (config.LoadGlobalConfig,
// the hub's server config, and config.LoadVersionedSettings, the settings
// the rest of scion reads) and requires identical results. It also checks:
//
//	(iii) the loaded broker ID and token are unchanged;
//	(iv)  a pure echo of the GET body leaves the file bytes identical.

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	{[]string{"server", "mode"}, "", "workstation"},
	{[]string{"server", "log_format"}, "", "json"},
	{[]string{"server", "log_level"}, "", "debug"},
	{[]string{"server", "hub", "port"}, 0, 9999},
	{[]string{"server", "hub", "cors", "enabled"}, false, true},
	{[]string{"server", "hub", "cors", "allowed_origins"}, []string{}, []string{"https://x.example"}},
	{[]string{"server", "hub", "cors", "allowed_methods"}, []string{}, []string{"GET"}},
	{[]string{"server", "hub", "cors", "allowed_headers"}, []string{}, []string{"X-A"}},
	{[]string{"server", "broker", "auto_provide"}, false, true},
	{[]string{"server", "broker", "port"}, 0, 9800},
	{[]string{"server", "broker", "cors", "enabled"}, false, true},
	{[]string{"server", "broker", "cors", "allowed_origins"}, []string{}, []string{"https://b.example"}},
	{[]string{"server", "auth", "dev_mode"}, false, true},
	{[]string{"server", "auth", "dev_token"}, "", "tok-x"},
	{[]string{"server", "storage", "bucket"}, "", "bkt"},
	{[]string{"server", "secrets", "gcp_replication_locations"}, []string{}, []string{"us-central1"}},
	{[]string{"server", "message_broker", "enabled"}, false, true},
	{[]string{"server", "message_broker", "types"}, []string{}, []string{"inprocess"}},
	{[]string{"server", "native_chat", "enabled"}, false, true},
	{[]string{"server", "scheduler", "max_concurrency"}, 0, 4},
	{[]string{"active_profile"}, "", "dev"},
	{[]string{"workspace_path"}, "", "/ws"},
	{[]string{"auto_inject_gcloud_adc"}, false, true},
}

// Block nulls, including ancestors of the hub-owned broker identity.
var propBlockNulls = [][]string{
	{"server", "storage"},
	{"server", "hub", "cors"},
	{"server", "broker", "cors"},
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
  mode: workstation
  log_format: text
  log_level: info
  hub:
    port: 9810
    cors:
      enabled: true
      allowed_origins:
        - https://a.example
      allowed_methods: [GET, POST]
  broker:
    enabled: true
    port: 9810
    auto_provide: false
    broker_id: b-1
    broker_token: tok-1
    cors:
      enabled: false
  auth:
    dev_mode: true
    dev_token: s3cret
  storage:
    provider: gcs
    bucket: my-bucket
  secrets:
    gcp_replication_locations: [europe-west1]
  message_broker:
    enabled: true
    type: inprocess
    types: [inprocess]
  native_chat:
    enabled: false
  scheduler:
    max_concurrency: 8
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
	value interface{}
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
		var body []propSent
		for _, idx := range rng.Perm(len(propLeaves))[:k] {
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
		if rng.Intn(4) == 0 {
			body = append(body, propSent{path: propBlockNulls[rng.Intn(len(propBlockNulls))], null: true})
		}
		out = append(out, dedupePropBody(body))
	}
	return out
}

// dedupePropBody drops entries that conflict with an earlier one (a JSON
// object cannot carry both a leaf and a null on one of its ancestors).
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

// --- Reference model: plain map edits on the seed ---

func refMapAt(m map[string]interface{}, path []string) (map[string]interface{}, bool) {
	cur := m
	for _, k := range path {
		next, ok := cur[k].(map[string]interface{})
		if !ok {
			return nil, false
		}
		cur = next
	}
	return cur, true
}

func refSet(m map[string]interface{}, path []string, v interface{}) {
	cur := m
	for _, k := range path[:len(path)-1] {
		next, ok := cur[k].(map[string]interface{})
		if !ok {
			next = map[string]interface{}{}
			cur[k] = next
		}
		cur = next
	}
	cur[path[len(path)-1]] = v
}

// refDelete removes path; when hub-owned keys live under it, it removes
// everything else and keeps them (the documented null semantics).
func refDelete(m map[string]interface{}, path []string) {
	parent, ok := refMapAt(m, path[:len(path)-1])
	if !ok {
		return
	}
	last := path[len(path)-1]
	var keep [][]string
	for _, owned := range hubOwnedBrokerPaths {
		if len(owned) > len(path) && pathHasPrefixPath(owned, path) {
			keep = append(keep, owned[len(path):])
		}
	}
	if len(keep) == 0 {
		delete(parent, last)
		return
	}
	block, ok := parent[last].(map[string]interface{})
	if !ok {
		delete(parent, last)
		return
	}
	refKeepOnly(block, keep)
}

func refKeepOnly(block map[string]interface{}, keep [][]string) {
	for k, v := range block {
		var sub [][]string
		whole := false
		for _, kp := range keep {
			if kp[0] == k {
				if len(kp) == 1 {
					whole = true
				} else {
					sub = append(sub, kp[1:])
				}
			}
		}
		switch {
		case whole:
		case len(sub) > 0:
			if child, ok := v.(map[string]interface{}); ok {
				refKeepOnly(child, sub)
			} else {
				delete(block, k)
			}
		default:
			delete(block, k)
		}
	}
}

// propReference builds the expected settings file for body sent on seed.
// corsOn holds the CORS switches of the seed as the real loader sees them.
func propReference(t *testing.T, seed string, sent []propSent, hubCORSOn, brokerCORSOn bool) []byte {
	t.Helper()
	m := map[string]interface{}{}
	if strings.TrimSpace(seed) != "" {
		if err := yamlv3.Unmarshal([]byte(seed), &m); err != nil {
			t.Fatal(err)
		}
		if m == nil {
			m = map[string]interface{}{}
		}
	}
	sentPaths := map[string]bool{}
	for _, s := range sent {
		sentPaths[strings.Join(s.path, ".")] = true
	}
	for _, block := range []struct {
		path    []string
		current bool
	}{{[]string{"server", "hub", "cors"}, hubCORSOn}, {[]string{"server", "broker", "cors"}, brokerCORSOn}} {
		_, exists := refMapAt(m, block.path)
		creates := false
		for _, s := range sent {
			if !s.null && len(s.path) > len(block.path) && pathHasPrefixPath(s.path, block.path) {
				creates = true
			}
		}
		if !exists && creates && !sentPaths[strings.Join(block.path, ".")+".enabled"] {
			refSet(m, append(append([]string{}, block.path...), "enabled"), block.current)
		}
	}
	for _, s := range sent {
		if s.null {
			refDelete(m, s.path)
		} else {
			refSet(m, s.path, s.value)
		}
	}
	if _, ok := m["schema_version"]; !ok && len(m) > 0 {
		m["schema_version"] = "1"
	}
	out, err := yamlv3.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// propLoaded is what the real loaders produce from the global settings file.
type propLoaded struct {
	Server   *config.GlobalConfig
	Settings *config.VersionedSettings
}

func propLoad(t *testing.T, globalDir string) propLoaded {
	t.Helper()
	gc, err := config.LoadGlobalConfig("")
	if err != nil {
		t.Fatalf("LoadGlobalConfig: %v", err)
	}
	vs, err := config.LoadVersionedSettings(globalDir)
	if err != nil {
		t.Fatalf("LoadVersionedSettings: %v", err)
	}
	return propLoaded{Server: gc, Settings: vs}
}

// propPrune turns a decoded JSON value into its canonical form: empty
// arrays and maps become nil and keys holding nil are dropped, recursively.
// Scalars are kept as they are, so false, 0 and "" stay distinct from an
// absent key (a *bool such as auto_provide must keep false).
func propPrune(v interface{}) interface{} {
	switch t := v.(type) {
	case map[string]interface{}:
		out := map[string]interface{}{}
		for k, c := range t {
			if p := propPrune(c); p != nil {
				out[k] = p
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	case []interface{}:
		if len(t) == 0 {
			return nil
		}
		out := make([]interface{}, len(t))
		for i, c := range t {
			out[i] = propPrune(c)
		}
		return out
	}
	return v
}

// propDiff lists the JSON paths where two loaded results differ, after
// propPrune. Paths in ignore (loader-path artifacts) are skipped.
func propDiff(a, b interface{}, ignore map[string]bool) []string {
	var am, bm interface{}
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	_ = json.Unmarshal(ab, &am)
	_ = json.Unmarshal(bb, &bm)
	am, bm = propPrune(am), propPrune(bm)
	var out []string
	var walk func(p string, x, y interface{})
	walk = func(p string, x, y interface{}) {
		if ignore[p] {
			return
		}
		xm, xok := x.(map[string]interface{})
		ym, yok := y.(map[string]interface{})
		if xok || yok {
			keys := map[string]bool{}
			for k := range xm {
				keys[k] = true
			}
			for k := range ym {
				keys[k] = true
			}
			for k := range keys {
				walk(p+"."+k, xm[k], ym[k])
			}
			return
		}
		if !reflect.DeepEqual(x, y) {
			out = append(out, fmt.Sprintf("%s: %v != %v", p, x, y))
		}
	}
	walk("", am, bm)
	sort.Strings(out)
	return out
}

// propDiffPaths returns the set of paths where a and b differ.
func propDiffPaths(a, b interface{}) map[string]bool {
	out := map[string]bool{}
	for _, d := range propDiff(a, b, nil) {
		out[d[:strings.Index(d, ":")]] = true
	}
	return out
}

// propHasServerKey reports whether settings bytes have a non-null server
// key, which decides whether LoadGlobalConfig reads settings.yaml or falls
// back to its legacy path.
func propHasServerKey(data []byte) bool {
	var m map[string]interface{}
	_ = yamlv3.Unmarshal(data, &m)
	return m["server"] != nil
}

// propBrokerIdentity returns the loaded broker ID and token.
func propBrokerIdentity(vs *config.VersionedSettings) [2]string {
	if vs == nil || vs.Server == nil || vs.Server.Broker == nil {
		return [2]string{}
	}
	return [2]string{vs.Server.Broker.BrokerID, vs.Server.Broker.BrokerToken}
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
			globalDir := filepath.Dir(settingsPath)
			srv, _, _ := newSQLiteHubInMode(t, true, nil)
			writeFile := func(data string) {
				if data == "" {
					_ = os.Remove(settingsPath)
					return
				}
				if err := os.WriteFile(settingsPath, []byte(data), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			read := func() string {
				data, _ := os.ReadFile(settingsPath)
				return string(data)
			}

			// Loader-path artifacts, measured from the real loader: what
			// differs between no server key (legacy path) and an empty one.
			// They are ignored only when exactly one of the actual and the
			// expected file has a server key, e.g. when the hub skipped an
			// edit that changes nothing but the reference still writes it.
			writeFile("")
			noServer := propLoad(t, globalDir)
			writeFile("schema_version: \"1\"\nserver: {}\n")
			emptyServer := propLoad(t, globalDir)
			artifactsServer := propDiffPaths(noServer.Server, emptyServer.Server)
			artifactsSettings := propDiffPaths(noServer.Settings, emptyServer.Settings)

			writeFile(seed)
			seedLoaded := propLoad(t, globalDir)

			// (iv) a pure echo of the GET body writes nothing.
			getRR := httptest.NewRecorder()
			srv.handleAdminServerConfig(getRR, adminRequest(http.MethodGet, "/api/v1/admin/server-config", ""))
			before := read()
			if rr := putServerConfig(t, srv, getRR.Body.String()); rr.Code != http.StatusOK {
				t.Fatalf("echo: %d %s", rr.Code, rr.Body.String())
			}
			if after := read(); after != before {
				t.Errorf("(iv) echo rewrote settings.yaml:\n%s", after)
			}
			cases++

			for _, sent := range bodies {
				cases++
				writeFile(seed)
				body := propBody(sent)
				rr := putServerConfig(t, srv, body)
				// Every generated value is valid: a 4xx is a false rejection.
				if rr.Code != http.StatusOK {
					t.Errorf("%s: expected 200, got %d: %s", body, rr.Code, rr.Body.String())
					continue
				}
				actualBytes := []byte(read())
				actual := propLoad(t, globalDir)

				ref := propReference(t, seed, sent, seedLoaded.Server.Hub.CORSEnabled, seedLoaded.Server.RuntimeBroker.CORSEnabled)
				writeFile(string(ref))
				expected := propLoad(t, globalDir)

				var ignoreServer, ignoreSettings map[string]bool
				if propHasServerKey(actualBytes) != propHasServerKey(ref) {
					ignoreServer, ignoreSettings = artifactsServer, artifactsSettings
				}
				if d := propDiff(actual.Server, expected.Server, ignoreServer); len(d) > 0 {
					t.Errorf("%s: loaded server config differs from the reference:\n  %s", body, strings.Join(d, "\n  "))
				}
				if d := propDiff(actual.Settings, expected.Settings, ignoreSettings); len(d) > 0 {
					t.Errorf("%s: loaded settings differ from the reference:\n  %s", body, strings.Join(d, "\n  "))
				}
				// (iii) the hub-owned broker identity survives.
				if a, b := propBrokerIdentity(actual.Settings), propBrokerIdentity(seedLoaded.Settings); a != b {
					t.Errorf("%s: (iii) broker identity changed: %v -> %v", body, b, a)
				}
			}
		})
	}
	t.Logf("property cases: %d (bodies per seed: %d + 1 echo, seeds: %d, leaves: %d, block nulls: %d)",
		cases, len(bodies), len(propSeeds), len(propLeaves), len(propBlockNulls))
}
