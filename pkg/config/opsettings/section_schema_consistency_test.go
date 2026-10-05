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
	// The settings file loader decodes map[string]string weakly, so the
	// root schema accepts unquoted numbers and booleans. A DB section doc
	// is decoded with encoding/json into map[string]string, where a number
	// fails, so the section keeps string-only values.
	"notifications.notification_channels.[].params.*: type [string], root [boolean number string]": "DB docs decode strictly",

	// Pre-existing: the hand-written map-of-objects section schemas are
	// looser than the root defs. Tightening them changes which PUT
	// server-config bodies are accepted, so it is left to a separate change.
	"harness_configs.*.auth_selected_type: enum [], root [api-key auth-file none oauth-token vertex-ai]":                         "section looser than root; pre-existing",
	"harness_configs.*.name: pattern <nil>, root ^[a-zA-Z0-9][a-zA-Z0-9_-]*$":                                                    "section looser than root; pre-existing",
	"profiles.*.harness_overrides.*.auth_selected_type: enum [], root [api-key auth-file oauth-token vertex-ai]":                 "section looser than root; pre-existing",
	"runtimes.*.type: enum [], root [cloudrun cloudrun-instances cloudrun-sandbox container docker kubernetes podman substrate]": "section looser than root; pre-existing",

	// Pre-existing: the profiles section schema accepts profiles.*.env, but
	// V1ProfileConfig has no env field, so the value is dropped when the
	// doc decodes. Left to the env-source work rather than changed here.
	"profiles.*.env: missing from root schema": "section-only key with no Go field; pre-existing",
}

// TestSectionSchemas_MatchRootSchema checks every Layer-1 section schema
// that has a settings.yaml representation against settings-v1.schema.json:
// for each section property (recursively, through properties, items and
// map values) the root schema must have the property, with the same JSON
// type, enum and pattern. Sections derived from the root schema pass by
// construction; this pins the hand-written ones, and catches a derived
// section whose root property went missing.
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
