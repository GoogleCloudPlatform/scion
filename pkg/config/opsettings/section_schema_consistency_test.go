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

package opsettings

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// sectionSchemaDiffAllowList lists intentional differences between a
// section schema and its settings-v1.schema.json counterpart, keyed as
// reported by TestSectionSchemas_MatchRootSchema. Each entry says why.
var sectionSchemaDiffAllowList = map[string]string{
	// Pre-existing: the hand-written map-of-objects section schemas are
	// looser than the root defs. Tightening them changes which PUT
	// server-config bodies are accepted; tracked in ptone/scion#3053.
	"harness_configs.*.auth_selected_type: enum [], root [api-key auth-file none oauth-token vertex-ai]":                         "section looser than root; ptone/scion#3053",
	"harness_configs.*.name: pattern <nil>, root ^[a-zA-Z0-9][a-zA-Z0-9_-]*$":                                                    "section looser than root; ptone/scion#3053",
	"profiles.*.harness_overrides.*.auth_selected_type: enum [], root [api-key auth-file oauth-token vertex-ai]":                 "section looser than root; ptone/scion#3053",
	"runtimes.*.type: enum [], root [cloudrun cloudrun-instances cloudrun-sandbox container docker kubernetes podman substrate]": "section looser than root; ptone/scion#3053",

	// Pre-existing: the profiles section schema accepts profiles.*.env, but
	// V1ProfileConfig has no env field, so a PUT carrying it gets a 200 and
	// the value is dropped when the doc decodes. Tracked in ptone/scion#3048.
	"profiles.*.env: missing from root schema": "section-only key with no Go field; ptone/scion#3048",
}

// TestSectionSchemas_MatchRootSchema checks every Layer-1 section schema
// that has a settings.yaml representation against settings-v1.schema.json:
// for each section property (recursively, through properties, items and
// map values) the root schema must have the property, with the same JSON
// type, enum and pattern. Sections derived from the root schema pass by
// construction; this pins the hand-written ones, and catches a derived
// section whose root property went missing.
//
// A section property that keeps one typed branch of a root anyOf is
// compared against that branch.
//
// Limits (what this test does not compare):
//   - only "type", "enum" and "pattern"; not minLength/maxLength,
//     minimum/maximum, "required" or "additionalProperties": false;
//   - "items" and "additionalProperties" sub-schemas only when both sides
//     declare them as objects;
//   - root properties missing from a section are not reported, since a
//     section may deliberately be a subset (github_app omits its secret
//     fields).
func TestSectionSchemas_MatchRootSchema(t *testing.T) {
	raw, err := config.GetSettingsSchemaJSON("1")
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]interface{}
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatal(err)
	}
	c := &schemaComparer{root: root}

	for _, sec := range Registry {
		if len(sec.KoanfPaths) == 0 {
			continue // runtime/API-owned state, no settings.yaml key
		}
		secSchema, ok := rawSchemas[sec.Name]
		if !ok {
			t.Errorf("%s: no section schema", sec.Name)
			continue
		}
		if len(sec.KoanfPaths) == 1 && sec.KoanfPaths[0] == sec.Name {
			// Map-of-objects section: the document is the whole subtree.
			rootNode, ok := c.lookup(sec.Name)
			if !ok {
				c.diff(sec.Name, "missing from root schema")
				continue
			}
			c.compare(sec.Name, secSchema, rootNode)
			continue
		}
		if sec.Name == "telemetry" {
			rootNode, _ := c.lookup("telemetry")
			c.compare(sec.Name, secSchema, rootNode)
			continue
		}
		props, _ := c.resolve(secSchema)["properties"].(map[string]interface{})
		if len(props) == 0 {
			c.diff(sec.Name, "section schema has no properties")
		}
		for key, sub := range props {
			kp := sectionKoanfPath(sec.Name, key)
			path := sec.Name + "." + key
			rootNode, ok := c.lookup(kp)
			if !ok {
				c.diff(path, "missing from root schema at "+kp)
				continue
			}
			c.compare(path, sub, rootNode)
		}
	}

	var unexpected, stale []string
	seen := map[string]bool{}
	for _, d := range c.diffs {
		seen[d] = true
		if _, ok := sectionSchemaDiffAllowList[d]; !ok {
			unexpected = append(unexpected, d)
		}
	}
	for k := range sectionSchemaDiffAllowList {
		if !seen[k] {
			stale = append(stale, k)
		}
	}
	sort.Strings(unexpected)
	sort.Strings(stale)
	if len(unexpected) > 0 {
		t.Errorf("section schemas differ from settings-v1.schema.json (derive them with getSchemaProperty, or allow-list in sectionSchemaDiffAllowList):\n  %s", strings.Join(unexpected, "\n  "))
	}
	if len(stale) > 0 {
		t.Errorf("stale sectionSchemaDiffAllowList entries:\n  %s", strings.Join(stale, "\n  "))
	}
}

// sectionKoanfPath maps a section document key to its koanf path.
func sectionKoanfPath(section, key string) string {
	if kp := KoanfPathFromSectionKey(section, key); kp != "" {
		return kp
	}
	if section == "notifications" {
		return "server." + key
	}
	return key // agent_defaults: top-level keys
}

type schemaComparer struct {
	root  map[string]interface{}
	diffs []string
}

func (c *schemaComparer) diff(path, msg string) {
	c.diffs = append(c.diffs, path+": "+msg)
}

func (c *schemaComparer) resolve(v interface{}) map[string]interface{} {
	node, _ := v.(map[string]interface{})
	for i := 0; i < 16 && node != nil; i++ {
		ref, ok := node["$ref"].(string)
		if !ok {
			break
		}
		next, _ := resolveRef(c.root, ref).(map[string]interface{})
		node = next
	}
	return node
}

// lookup walks a dotted koanf path through the root schema's properties.
func (c *schemaComparer) lookup(koanfPath string) (map[string]interface{}, bool) {
	node := c.root
	for _, seg := range strings.Split(koanfPath, ".") {
		props, _ := c.resolve(node)["properties"].(map[string]interface{})
		next, ok := props[seg].(map[string]interface{})
		if !ok {
			return nil, false
		}
		node = next
	}
	return c.resolve(node), true
}

func typeSet(v interface{}) []string {
	switch t := v.(type) {
	case string:
		return []string{t}
	case []interface{}:
		var out []string
		for _, x := range t {
			out = append(out, fmt.Sprint(x))
		}
		sort.Strings(out)
		return out
	case []string:
		out := append([]string(nil), t...)
		sort.Strings(out)
		return out
	}
	return nil
}

func enumSet(v interface{}) []string {
	var out []string
	switch t := v.(type) {
	case []interface{}:
		for _, x := range t {
			out = append(out, fmt.Sprint(x))
		}
	case []string:
		out = append(out, t...)
	}
	sort.Strings(out)
	return out
}

func (c *schemaComparer) compare(path string, secV, rootV interface{}) {
	sec := c.resolve(secV)
	root := c.resolve(rootV)
	if sec == nil || root == nil {
		return
	}
	// A section may keep one typed branch of a root anyOf (the federation
	// intervals keep the string branch: DB docs decode strictly, while the
	// file loader also takes an unquoted 0). Compare against that branch.
	if branches, ok := root["anyOf"].([]interface{}); ok && root["type"] == nil {
		if st, ok := sec["type"].(string); ok {
			matched := false
			for _, b := range branches {
				if bm := c.resolve(b); bm != nil && bm["type"] == st {
					root, matched = bm, true
					break
				}
			}
			if !matched {
				c.diff(path, "type "+st+" matches no root anyOf branch")
				return
			}
		}
	}
	if st, rt := typeSet(sec["type"]), typeSet(root["type"]); st != nil && !reflect.DeepEqual(st, rt) {
		c.diff(path, fmt.Sprintf("type %v, root %v", st, rt))
	}
	if se, re := enumSet(sec["enum"]), enumSet(root["enum"]); !reflect.DeepEqual(se, re) {
		c.diff(path, fmt.Sprintf("enum %v, root %v", se, re))
	}
	if sp, rp := sec["pattern"], root["pattern"]; sp != rp {
		c.diff(path, fmt.Sprintf("pattern %v, root %v", sp, rp))
	}
	if props, ok := sec["properties"].(map[string]interface{}); ok {
		rootProps, _ := root["properties"].(map[string]interface{})
		for key, sub := range props {
			rsub, ok := rootProps[key]
			if !ok {
				c.diff(path+"."+key, "missing from root schema")
				continue
			}
			c.compare(path+"."+key, sub, rsub)
		}
	}
	if items, ok := sec["items"].(map[string]interface{}); ok {
		if ritems, ok := root["items"].(map[string]interface{}); ok {
			c.compare(path+".[]", items, ritems)
		}
	}
	if ap, ok := sec["additionalProperties"].(map[string]interface{}); ok {
		if rap, ok := root["additionalProperties"].(map[string]interface{}); ok {
			c.compare(path+".*", ap, rap)
		}
	}
}

// TestSchemaInfo_SelfContained checks that the schemas served by
// GET /admin/server-config/schema carry no unresolved $ref: they are served
// without the root $defs, so every reference must be inlined.
func TestSchemaInfo_SelfContained(t *testing.T) {
	info := SchemaInfo()
	if info == nil {
		t.Fatal("SchemaInfo() returned nil")
	}
	for name, sec := range info {
		data, err := json.Marshal(sec.Schema)
		if err != nil {
			t.Fatalf("%s: marshal: %v", name, err)
		}
		if strings.Contains(string(data), `"$ref"`) {
			t.Errorf("%s: served schema has an unresolved $ref: %s", name, data)
		}
	}
	// The sections that used to carry refs are expanded, not emptied.
	for _, name := range []string{"telemetry", "federation"} {
		data, _ := json.Marshal(info[name].Schema)
		if !strings.Contains(string(data), `"properties"`) {
			t.Errorf("%s: served schema lost its properties: %s", name, data)
		}
	}
	fed, _ := json.Marshal(info["federation"].Schema)
	if !strings.Contains(string(fed), `"issuer_url"`) {
		t.Errorf("federation: trusted_issuers items not inlined: %s", fed)
	}
}

// The federation section (DB docs, strict decode) rejects an integer
// interval, while the settings file schema accepts an unquoted 0.
func TestFederationSection_IntervalsStringOnly(t *testing.T) {
	if errs := Validate("federation", json.RawMessage(`{"refresh_interval":0}`)); len(errs) == 0 {
		t.Error("an integer refresh_interval should fail the federation section schema")
	}
	if errs := Validate("federation", json.RawMessage(`{"refresh_interval":"0","debounce_interval":""}`)); len(errs) != 0 {
		t.Errorf("string intervals should validate: %v", errs)
	}
	if errs := Validate("federation", json.RawMessage(`{"refresh_interval":"3600"}`)); len(errs) == 0 {
		t.Error("a duration without a unit should fail")
	}
}

// The federation section's string-only intervals keep the root property's
// description (it lives on the anyOf parent, not on the string branch).
func TestFederationSection_IntervalDescriptionCarried(t *testing.T) {
	fed, _ := rawSchemas["federation"]["properties"].(map[string]interface{})
	for _, key := range []string{"refresh_interval", "debounce_interval"} {
		prop, _ := fed[key].(map[string]interface{})
		if desc, _ := prop["description"].(string); !strings.Contains(desc, "Go duration") {
			t.Errorf("%s description not carried: %v", key, prop)
		}
		if prop["type"] != "string" || prop["anyOf"] != nil {
			t.Errorf("%s should be the string branch only: %v", key, prop)
		}
	}
}

// withStringOnlyIntervals must not write into the shared root anyOf branch.
func TestWithStringOnlyIntervals_LeavesRootUntouched(t *testing.T) {
	raw, err := config.GetSettingsSchemaJSON("1")
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]interface{}
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatal(err)
	}
	fed := schemaObject(getSchemaProperty(root, "server", "federation"))
	_ = withStringOnlyIntervals(fed)
	c := &schemaComparer{root: root}
	node, _ := c.lookup("server.federation.refresh_interval")
	branches, _ := node["anyOf"].([]interface{})
	if len(branches) != 2 {
		t.Fatalf("root refresh_interval anyOf changed: %v", node)
	}
	for _, b := range branches {
		if bm, _ := b.(map[string]interface{}); bm["description"] != nil {
			t.Errorf("root anyOf branch gained a description: %v", bm)
		}
	}
}
