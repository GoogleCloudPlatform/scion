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
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
)

// The SCION_SERVER_* env mapping table (ptone/scion#3911).
//
// Every server setting has one table entry, built from the embedded
// settings-v1 schema: each scalar or scalar-list leaf under server, each leaf
// under serverEnvTopLevelKeys, and serverEnvExtraKeys. An entry names the
// setting's settings path (the key VersionedSettings, the opsettings
// registry and settings.yaml use), its GlobalConfig koanf path, its one
// canonical env name and its legacy alias names.
//
// The canonical name is SCION_SERVER_ followed by the path's segments, each
// written without its inner underscores, joined with "_" (a leading
// "server." is dropped): server.hub.admin_emails ->
// SCION_SERVER_HUB_ADMINEMAILS. serverEnvCanonicalOverrides lists the
// documented names that differ from that rule.
//
// Every env consumer resolves a name through the table first
// (KeyForServerEnv): the opsettings env layer (LoadEnvKoanf, the bootstrap
// merge), the seed layer (LoadSeedEnvKoanf), GlobalConfig (LoadGlobalConfig)
// and VersionedSettings (LoadVersionedSettings). A canonical name and each
// of its aliases therefore reach the same setting in all of them. A name
// the table does not hold falls through to serverEnvResidue, which only
// accepts paths the table does not own, such as leaves under a named map
// entry (profiles.<name>.*, runtimes.<name>.*, harness_configs.<name>.*).

// ServerEnvEntry is one setting in the SCION_SERVER_* env mapping table.
type ServerEnvEntry struct {
	// Path is the settings path, e.g. server.hub.admin_emails.
	Path string
	// GlobalConfig is the setting's GlobalConfig koanf path, e.g.
	// hub.adminEmails, or "" when GlobalConfig has no koanf field for it
	// (no field at all, or a koanf:"-" field applied from the env by
	// applyEnvToOutsideKoanfFields).
	GlobalConfig string
	// Canonical is the canonical env name, e.g. SCION_SERVER_HUB_ADMINEMAILS.
	Canonical string
	// Type is the setting's JSON type when the schema gives it exactly one
	// (boolean, integer, number, string, array), else "". The env loaders
	// that feed the opsettings keyspace convert a boolean, integer or number
	// value to that type, and ValidateServerEnv checks it at server startup.
	Type string
	// Aliases are the legacy env names that still set the setting, sorted:
	// the spelling that keeps each field's underscores
	// (SCION_SERVER_HUB_ADMIN_EMAILS), the GlobalConfig spelling
	// (SCION_SERVER_HUB_CORSENABLED), the schema's x-env-var or
	// x-env-var-alias when it differs, and for a renamed section the names
	// under its settings name (SCION_SERVER_BROKER_PORT).
	Aliases []string
}

// serverEnvCanonicalOverride is a documented canonical env name that
// differs from the derived one.
type serverEnvCanonicalOverride struct {
	// Path is the settings path the override applies to.
	Path string
	// Env replaces the env segments of Path. With Subtree it replaces the
	// segments of the Path prefix of every key under Path, and the rest of
	// the key is derived as usual.
	Env     string
	Subtree bool
}

// serverEnvCanonicalOverrides lists every canonical env name that is not the
// derived spelling. The settings schema's x-env-var names agree with it
// (TestSchemaEnvVarDerivation in pkg/config/opsettings).
var serverEnvCanonicalOverrides = []serverEnvCanonicalOverride{
	// server.hub.public_url is set by SCION_SERVER_HUB_ENDPOINT, the
	// variable the hub's endpoint resolution reads (GlobalConfig
	// hub.endpoint), pinned in ptone/scion#3905. The derived spellings are
	// inert (serverEnvInertNames).
	{Path: "server.hub.public_url", Env: "HUB_ENDPOINT"},
	// server.broker.* is documented under the GlobalConfig section name
	// runtimeBroker (SCION_SERVER_RUNTIMEBROKER_PORT). The names under the
	// settings section name (SCION_SERVER_BROKER_PORT,
	// SCION_SERVER_BROKER_BROKER_ID) are aliases.
	{Path: "server.broker", Env: "RUNTIMEBROKER", Subtree: true},
	// server.keyring_file is named SCION_SERVER_KEYRING_FILE by its design
	// (ptone/scion#1530 §2), not the derived SCION_SERVER_KEYRINGFILE.
	{Path: KeyringFileKey, Env: "KEYRING_FILE"},
	{Path: KeyringFileAllowWorldReadableKey, Env: "KEYRING_FILE_ALLOW_WORLD_READABLE"},
}

// serverEnvTopLevelKeys lists the top-level (outside server) settings, or
// settings sections, that a SCION_SERVER_* name sets: the keys the
// opsettings registry owns outside server. The opsettings completeness test
// checks the list against the registry in both directions.
var serverEnvTopLevelKeys = []string{
	"agent_secrets",
	"auto_expose_ports",
	"default_agent_role",
	"default_gcp_identity_mode",
	"default_gcp_identity_service_account_id",
	"default_harness_config",
	"default_max_agent_role",
	"default_max_duration",
	"default_max_model_calls",
	"default_max_turns",
	"default_model",
	"default_runtime_broker",
	"default_template",
	"default_thinking_level",
	"default_timezone",
	"image_registry",
	"project_defaults",
	"quotas",
	"telemetry",
}

// serverEnvExtraKeys lists the server settings the settings schema has no
// node for, with their schema type.
// None today: server.hub.gcp_project_id, the last one, has a schema node
// since ptone/scion#3830.
var serverEnvExtraKeys = map[string]string{}

// ServerEnvExcludedKeys documents settings that no SCION_SERVER_* name sets,
// with the reason. None of them is in the table.
var ServerEnvExcludedKeys = map[string]string{
	"default_harness_auth": "client settings overlay key (SCION_DEFAULT_HARNESS_AUTH); no opsettings registry " +
		"section owns it, so the hub has no setting for a SCION_SERVER_ name to set",
	"roles": "not implemented as a setting: the settings schema and the opsettings registry have no roles key",
}

// serverEnvTable is the built table with its indexes.
type serverEnvTable struct {
	entries []ServerEnvEntry
	byPath  map[string]int
	// byName maps an upper-case env name to its entry.
	byName map[string]serverEnvName
	// gcPaths holds every entry's GlobalConfig path.
	gcPaths map[string]bool
	// collisions describes names that more than one entry claims. Such an
	// alias is dropped; TestServerEnvTable_NoCollisions keeps this empty.
	collisions []string
}

type serverEnvName struct {
	idx   int
	alias bool
}

var (
	serverEnvTableOnce sync.Once
	serverEnvTableData *serverEnvTable
)

// getServerEnvTable returns the table, building it on first use.
func getServerEnvTable() *serverEnvTable {
	serverEnvTableOnce.Do(func() { serverEnvTableData = buildServerEnvTable() })
	return serverEnvTableData
}

// ServerEnvEntries returns a copy of every table entry, sorted by path.
func ServerEnvEntries() []ServerEnvEntry {
	t := getServerEnvTable()
	out := make([]ServerEnvEntry, len(t.entries))
	for i, e := range t.entries {
		e.Aliases = append([]string(nil), e.Aliases...)
		out[i] = e
	}
	return out
}

// ServerEnvKey returns the canonical SCION_SERVER_* name of the setting at
// path, or false when no SCION_SERVER_* name sets it.
func ServerEnvKey(path string) (string, bool) {
	t := getServerEnvTable()
	i, ok := t.byPath[path]
	if !ok {
		return "", false
	}
	return t.entries[i].Canonical, true
}

// KeyForServerEnv returns the settings path a SCION_SERVER_* name sets and
// whether the name is a legacy alias rather than the canonical name. The
// name is matched without regard to case. ok is false for a name the table
// does not hold.
func KeyForServerEnv(name string) (path string, alias, ok bool) {
	e, alias, ok := lookupServerEnv(name)
	if !ok {
		return "", false, false
	}
	return e.Path, alias, true
}

// lookupServerEnv returns the table entry for a SCION_SERVER_* name.
func lookupServerEnv(name string) (ServerEnvEntry, bool, bool) {
	t := getServerEnvTable()
	n, ok := t.byName[strings.ToUpper(name)]
	if !ok {
		return ServerEnvEntry{}, false, false
	}
	return t.entries[n.idx], n.alias, true
}

// resolveServerEnv returns the table entry a SCION_SERVER_* name sets, as
// the env consumers apply it: false for a name the table does not hold, and
// false for an alias another name of the same setting takes precedence
// over. The canonical name wins over every alias, and among aliases the one
// that sorts first wins, so the result never depends on the order of the
// process environment. With seed true, name is a SCION_SEED_* name and
// precedence is checked among the SCION_SEED_* spellings.
func resolveServerEnv(name string, seed bool) (ServerEnvEntry, bool) {
	lookup := name
	if seed {
		lookup = serverEnvNameForSeed(name)
	}
	e, alias, ok := lookupServerEnv(lookup)
	if !ok {
		return ServerEnvEntry{}, false
	}
	if !alias {
		return e, true
	}
	spell := func(n string) string {
		if seed {
			return seedEnvNameFor(e, n)
		}
		return n
	}
	if os.Getenv(spell(e.Canonical)) != "" {
		return ServerEnvEntry{}, false
	}
	self := strings.ToUpper(lookup)
	for _, a := range e.Aliases {
		if a < self && os.Getenv(spell(a)) != "" {
			return ServerEnvEntry{}, false
		}
	}
	return e, true
}

// serverEnvNameForSeed returns the SCION_SERVER_* spelling of a SCION_SEED_*
// name: SCION_SEED_SERVER_<x> is SCION_SERVER_<x>, and SCION_SEED_<x> is
// SCION_SERVER_<x>.
func serverEnvNameForSeed(name string) string {
	rest := strings.ToUpper(strings.TrimPrefix(strings.ToUpper(name), seedEnvPrefix))
	return serverEnvPrefix + strings.TrimPrefix(rest, "SERVER_")
}

// seedEnvNameFor returns the SCION_SEED_* spelling of the SCION_SERVER_* name
// n of entry e: SCION_SEED_SERVER_<x> for a server setting, SCION_SEED_<x>
// otherwise.
func seedEnvNameFor(e ServerEnvEntry, n string) string {
	rest := strings.TrimPrefix(n, serverEnvPrefix)
	if strings.HasPrefix(e.Path, "server.") {
		return seedEnvPrefix + "SERVER_" + rest
	}
	return seedEnvPrefix + rest
}

// isServerEnvInert reports whether a SCION_SERVER_* name is listed in
// serverEnvInertNames.
func isServerEnvInert(name string) bool {
	return serverEnvInertNames[strings.ToUpper(name)]
}

// ownsServerEnvPath reports whether path is a table entry's settings path.
func ownsServerEnvPath(path string) bool {
	_, ok := getServerEnvTable().byPath[path]
	return ok
}

// ownsGlobalConfigEnvPath reports whether path is a table entry's
// GlobalConfig path.
func ownsGlobalConfigEnvPath(path string) bool {
	return getServerEnvTable().gcPaths[path]
}

func buildServerEnvTable() *serverEnvTable {
	t := &serverEnvTable{
		byPath:  map[string]int{},
		byName:  map[string]serverEnvName{},
		gcPaths: map[string]bool{},
	}
	leaves, err := settingsSchemaLeaves()
	if err != nil {
		// The schema is embedded; a parse failure is a build defect.
		panic(fmt.Sprintf("server env table: %v", err))
	}
	for p, typ := range serverEnvExtraKeys {
		if _, ok := leaves[p]; !ok {
			leaves[p] = map[string]interface{}{"type": typ}
		}
	}
	var paths []string
	for p := range leaves {
		if serverEnvInScope(p) {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	for _, p := range paths {
		e := ServerEnvEntry{Path: p, GlobalConfig: globalConfigPathForSetting(p), Type: schemaNodeSingleType(leaves[p])}
		e.Canonical = serverEnvPrefix + canonicalServerEnvSuffix(p)
		e.Aliases = serverEnvAliasCandidates(e, leaves[p])
		t.byPath[p] = len(t.entries)
		t.entries = append(t.entries, e)
		if e.GlobalConfig != "" {
			t.gcPaths[e.GlobalConfig] = true
		}
	}

	// Canonical names first, so an alias never shadows one.
	for i, e := range t.entries {
		if prev, ok := t.byName[e.Canonical]; ok {
			t.collisions = append(t.collisions, fmt.Sprintf("%s is the canonical name of %s and %s",
				e.Canonical, t.entries[prev.idx].Path, e.Path))
			continue
		}
		t.byName[e.Canonical] = serverEnvName{idx: i}
	}
	ambiguous := map[string]bool{}
	for i, e := range t.entries {
		for _, a := range e.Aliases {
			prev, ok := t.byName[a]
			switch {
			case !ok:
				t.byName[a] = serverEnvName{idx: i, alias: true}
			case prev.idx == i:
			case prev.alias:
				ambiguous[a] = true
				t.collisions = append(t.collisions, fmt.Sprintf("%s is an alias of %s and %s",
					a, t.entries[prev.idx].Path, e.Path))
			default:
				t.collisions = append(t.collisions, fmt.Sprintf("%s, an alias of %s, is the canonical name of %s",
					a, e.Path, t.entries[prev.idx].Path))
			}
		}
	}
	for a := range ambiguous {
		delete(t.byName, a)
	}
	// Keep only the aliases that resolve to their own entry.
	for i := range t.entries {
		var kept []string
		for _, a := range t.entries[i].Aliases {
			if n, ok := t.byName[a]; ok && n.alias && n.idx == i {
				kept = append(kept, a)
			}
		}
		t.entries[i].Aliases = kept
	}
	sort.Strings(t.collisions)
	return t
}

// serverEnvInScope reports whether a SCION_SERVER_* name sets the setting at
// path: every server setting, and the settings under serverEnvTopLevelKeys.
func serverEnvInScope(path string) bool {
	if strings.HasPrefix(path, "server.") {
		return true
	}
	for _, k := range serverEnvTopLevelKeys {
		if path == k || strings.HasPrefix(path, k+".") {
			return true
		}
	}
	return false
}

// canonicalOverrideFor returns the serverEnvCanonicalOverrides entry that
// applies to path.
func canonicalOverrideFor(path string) (serverEnvCanonicalOverride, bool) {
	for _, o := range serverEnvCanonicalOverrides {
		if (!o.Subtree && path == o.Path) || (o.Subtree && strings.HasPrefix(path, o.Path+".")) {
			return o, true
		}
	}
	return serverEnvCanonicalOverride{}, false
}

// canonicalServerEnvSuffix returns the canonical env name of path after
// SCION_SERVER_.
func canonicalServerEnvSuffix(path string) string {
	if o, ok := canonicalOverrideFor(path); ok {
		if !o.Subtree {
			return o.Env
		}
		return o.Env + "_" + collapsedEnvSegments(strings.TrimPrefix(path, o.Path+"."))
	}
	return collapsedEnvSegments(strings.TrimPrefix(path, "server."))
}

// collapsedEnvSegments writes a dotted path as env segments: each segment
// upper-cased without its underscores, joined with "_".
func collapsedEnvSegments(path string) string {
	segs := strings.Split(path, ".")
	for i, s := range segs {
		segs[i] = strings.ToUpper(strings.ReplaceAll(s, "_", ""))
	}
	return strings.Join(segs, "_")
}

// underscoredEnvSegments writes a dotted path as env segments that keep
// each segment's underscores.
func underscoredEnvSegments(path string) string {
	return strings.ToUpper(strings.ReplaceAll(path, ".", "_"))
}

// serverEnvAliasCandidates returns the legacy names of e (see
// ServerEnvEntry.Aliases), before collisions are resolved.
func serverEnvAliasCandidates(e ServerEnvEntry, node map[string]interface{}) []string {
	set := map[string]bool{}
	add := func(suffix string) {
		if suffix != "" {
			set[serverEnvPrefix+suffix] = true
		}
	}
	rel := strings.TrimPrefix(e.Path, "server.")
	if o, ok := canonicalOverrideFor(e.Path); ok {
		// An exact-key override keeps only its own name; a renamed section
		// also accepts the names under the settings section name.
		if o.Subtree {
			add(o.Env + "_" + underscoredEnvSegments(strings.TrimPrefix(e.Path, o.Path+".")))
			add(collapsedEnvSegments(rel))
			add(underscoredEnvSegments(rel))
		}
	} else {
		add(underscoredEnvSegments(rel))
	}
	if e.GlobalConfig != "" {
		add(collapsedEnvSegments(e.GlobalConfig))
	}
	for _, k := range []string{"x-env-var", "x-env-var-alias"} {
		if s, _ := node[k].(string); strings.HasPrefix(s, serverEnvPrefix) {
			set[s] = true
		}
	}
	delete(set, e.Canonical)
	out := make([]string, 0, len(set))
	for n := range set {
		if !serverEnvInertNames[n] {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// globalConfigSettingRenames maps the settings whose GlobalConfig path is
// not the settings path with "server." dropped (and the broker and cors
// rules of globalConfigPathForSetting applied).
var globalConfigSettingRenames = map[string]string{
	"server.hub.public_url":               "hub.endpoint",
	"server.federation.debounce_interval": "federation.cache.debounce_interval",
	"server.federation.refresh_interval":  "federation.cache.refresh_interval",
	"telemetry.enabled":                   "telemetryEnabled",
}

// globalConfigPathForSetting returns the GlobalConfig koanf path (exact
// koanf tags) of the setting at path, or "" when GlobalConfig has no koanf
// field for it. server.broker is GlobalConfig's runtimeBroker section, and
// GlobalConfig flattens cors.<field> into cors<Field>.
func globalConfigPathForSetting(path string) string {
	var segs []string
	if r, ok := globalConfigSettingRenames[path]; ok {
		segs = strings.Split(r, ".")
	} else {
		rel, ok := strings.CutPrefix(path, "server.")
		if !ok {
			return ""
		}
		segs = strings.Split(rel, ".")
		if segs[0] == "broker" {
			segs[0] = "runtimeBroker"
		}
		if n := len(segs); n >= 2 && segs[n-2] == "cors" {
			segs = append(segs[:n-2:n-2], "cors"+segs[n-1])
		}
	}
	tags, ok := koanfTagPath(reflect.TypeOf(GlobalConfig{}), segs)
	if !ok {
		return ""
	}
	return strings.Join(tags, ".")
}

// normEnvKey folds a koanf tag or env segment for comparison: lower case,
// underscores removed.
func normEnvKey(s string) string {
	return strings.ToLower(strings.ReplaceAll(s, "_", ""))
}

// fieldByNormTag returns the field of struct type t whose koanf name folds
// to want (see normEnvKey), and that name. An untagged embedded struct is
// searched in place, as findKoanfField does.
func fieldByNormTag(t reflect.Type, want string) (reflect.StructField, string, bool) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		tag := strings.Split(f.Tag.Get("koanf"), ",")[0]
		if tag == "-" {
			continue
		}
		if tag == "" {
			if f.Anonymous && derefType(f.Type).Kind() == reflect.Struct {
				if inner, innerTag, ok := fieldByNormTag(derefType(f.Type), want); ok {
					return inner, innerTag, true
				}
				continue
			}
			tag = f.Name
		}
		if normEnvKey(tag) == want {
			return f, tag, true
		}
	}
	return reflect.StructField{}, "", false
}

// koanfTagPath follows segs (one per field, matched by normEnvKey) through
// t and returns the exact koanf tags. A map consumes one segment as its key.
func koanfTagPath(t reflect.Type, segs []string) ([]string, bool) {
	tags, _, ok := koanfTagPathEnd(t, segs)
	return tags, ok
}

// koanfTagPathEnd is koanfTagPath that also returns the type the path ends
// on.
func koanfTagPathEnd(t reflect.Type, segs []string) ([]string, reflect.Type, bool) {
	out := make([]string, 0, len(segs))
	for _, seg := range segs {
		t = derefType(t)
		switch t.Kind() {
		case reflect.Struct:
			f, tag, ok := fieldByNormTag(t, normEnvKey(seg))
			if !ok {
				return nil, nil, false
			}
			out = append(out, tag)
			t = f.Type
		case reflect.Map:
			out = append(out, seg)
			t = t.Elem()
		default:
			return nil, nil, false
		}
	}
	return out, t, true
}

// envSegmentsKoanfPath resolves env segments (an env name after its prefix,
// split on "_") against the koanf tags of t. A field name may span several
// segments (ADMIN_EMAILS or ADMINEMAILS for admin_emails; the longest match
// is tried first), a map consumes one segment as its key, and the path must
// end on a value that is neither a struct, a map nor a list of objects.
func envSegmentsKoanfPath(t reflect.Type, segs []string) ([]string, bool) {
	t = derefType(t)
	if len(segs) == 0 {
		switch t.Kind() {
		case reflect.Struct, reflect.Map:
			return nil, false
		}
		return nil, !isObjectListType(t)
	}
	switch t.Kind() {
	case reflect.Struct:
		for k := len(segs); k >= 1; k-- {
			f, tag, ok := fieldByNormTag(t, normEnvKey(strings.Join(segs[:k], "")))
			if !ok {
				continue
			}
			if rest, ok := envSegmentsKoanfPath(f.Type, segs[k:]); ok {
				return append([]string{tag}, rest...), true
			}
		}
	case reflect.Map:
		if t.Key().Kind() != reflect.String {
			return nil, false
		}
		if rest, ok := envSegmentsKoanfPath(t.Elem(), segs[1:]); ok {
			return append([]string{strings.ToLower(segs[0])}, rest...), true
		}
	}
	return nil, false
}

// isObjectListType reports whether t is a slice or array of structs or maps,
// which no single env string can set.
func isObjectListType(t reflect.Type) bool {
	t = derefType(t)
	if t.Kind() != reflect.Slice && t.Kind() != reflect.Array {
		return false
	}
	switch derefType(t.Elem()).Kind() {
	case reflect.Struct, reflect.Map:
		return true
	}
	return false
}

// envSegmentsKoanfPrefix follows env segments through t as far as they
// match (a struct field by its koanf name, longest match first; a map entry
// by one segment as its key) and returns the koanf path of the matched
// prefix, the unmatched rest and the type the prefix ends on.
func envSegmentsKoanfPrefix(t reflect.Type, segs []string) (tags, rest []string, end reflect.Type) {
	for len(segs) > 0 {
		t = derefType(t)
		switch t.Kind() {
		case reflect.Struct:
			matched := false
			for k := len(segs); k >= 1; k-- {
				if f, tag, ok := fieldByNormTag(t, normEnvKey(strings.Join(segs[:k], ""))); ok {
					tags = append(tags, tag)
					segs = segs[k:]
					t = f.Type
					matched = true
					break
				}
			}
			if !matched {
				return tags, segs, t
			}
		case reflect.Map:
			if t.Key().Kind() != reflect.String {
				return tags, segs, t
			}
			tags = append(tags, strings.ToLower(segs[0]))
			segs = segs[1:]
			t = t.Elem()
		default:
			return tags, segs, t
		}
	}
	return tags, segs, t
}

// settingsSchemaLeaves returns every leaf property of the embedded
// settings-v1 schema that one env string can set (a scalar or a list of
// scalars), by dotted path, with local $refs merged in.
func settingsSchemaLeaves() (map[string]map[string]interface{}, error) {
	data, err := GetSettingsSchemaJSON("1")
	if err != nil {
		return nil, err
	}
	var root map[string]interface{}
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, err
	}
	defs, _ := root["$defs"].(map[string]interface{})
	out := map[string]map[string]interface{}{}
	var walk func(node map[string]interface{}, path string, depth int)
	walk = func(node map[string]interface{}, path string, depth int) {
		if depth > 25 {
			return
		}
		if ref, ok := node["$ref"].(string); ok && strings.HasPrefix(ref, "#/$defs/") {
			if def, ok := defs[strings.TrimPrefix(ref, "#/$defs/")].(map[string]interface{}); ok {
				merged := map[string]interface{}{}
				for k, v := range def {
					merged[k] = v
				}
				for k, v := range node {
					if k != "$ref" {
						merged[k] = v
					}
				}
				node = merged
			}
		}
		props, hasProps := node["properties"].(map[string]interface{})
		if !hasProps {
			if path != "" && !schemaNodeStructured(node) {
				out[path] = node
			}
			return
		}
		for name, child := range props {
			if cm, ok := child.(map[string]interface{}); ok {
				p := name
				if path != "" {
					p = path + "." + name
				}
				walk(cm, p, depth+1)
			}
		}
	}
	walk(root, "", 0)
	return out, nil
}

// schemaNodeType returns a schema node's JSON type, ignoring "null" in a
// type list.
func schemaNodeType(node map[string]interface{}) string {
	switch v := node["type"].(type) {
	case string:
		return v
	case []interface{}:
		for _, t := range v {
			if s, ok := t.(string); ok && s != "null" {
				return s
			}
		}
	}
	return ""
}

// schemaNodeSingleType returns a schema node's JSON type when it has exactly
// one besides "null", else "".
func schemaNodeSingleType(node map[string]interface{}) string {
	switch v := node["type"].(type) {
	case string:
		return v
	case []interface{}:
		one := ""
		for _, t := range v {
			s, ok := t.(string)
			if !ok || s == "null" {
				continue
			}
			if one != "" {
				return ""
			}
			one = s
		}
		return one
	}
	return ""
}

// schemaNodeStructured reports whether a leaf node holds a map or a list of
// objects, which no single env string can set.
func schemaNodeStructured(node map[string]interface{}) bool {
	switch schemaNodeType(node) {
	case "object":
		return true
	case "array":
		items, _ := node["items"].(map[string]interface{})
		if items == nil {
			return false
		}
		if items["type"] == "object" {
			return true
		}
		_, hasProps := items["properties"]
		_, hasRef := items["$ref"]
		return hasProps || hasRef
	}
	return false
}

// serverEnvResidue maps a SCION_SERVER_* name the table does not hold, given
// as its env segments after the prefix (lower case), to a path in the
// keyspace of t. It accepts a struct match (envSegmentsKoanfPath) only when
// owned reports false for the result, so a table setting is reached only
// through its canonical name or an alias. Otherwise it returns false.
func serverEnvResidue(t reflect.Type, segs []string, owned func(string) bool) (string, bool) {
	p, ok := envSegmentsKoanfPath(t, segs)
	if !ok {
		return "", false
	}
	path := strings.Join(p, ".")
	if owned(path) {
		return "", false
	}
	return path, true
}
