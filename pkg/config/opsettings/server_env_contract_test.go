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
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// Env contract test (ptone/scion#3910).
//
// For every server setting (each leaf registry KoanfPath and each leaf under
// a Layer-0 prefix) the canonical env spelling is the settings schema's
// x-env-var name when it is a SCION_SERVER_ name. Otherwise it is derived
// from the koanf path: drop a leading "server.", remove the underscores
// inside each segment, upper-case, join the segments with "_", and prefix
// SCION_SERVER_ (server.hub.admin_emails -> SCION_SERVER_HUB_ADMINEMAILS).
// TestSchemaEnvVarDerivation keeps the derivation, the schema and the env
// mapping table (config.ServerEnvKey) aligned. The seed spelling puts
// SCION_SEED_ in front of the server path (SCION_SEED_SERVER_HUB_ADMINEMAILS).
//
// The test sets that spelling and checks it reaches the key through every
// env consumer that has a field for it, and that an exported but empty
// value is treated as unset: for the struct consumers it seeds a YAML value
// first and requires the empty variable to leave it in place. Every
// (consumer, key) pair must reach: the single mapping table
// (ptone/scion#3911) drained the former known-gap list, and a new gap
// fails. TestServerEnvAliasesReach does the same for every legacy alias. A
// struct consumer without a field for a key is listed in
// envContractNoField, never skipped silently; a key GlobalConfig holds in a
// koanf:"-" field is checked through that field.

// Env consumers checked by the contract test.
const (
	// consumerOpsettings is LoadEnvKoanf (serverEnvToOpsettingsKey, through
	// the env mapping table). It feeds the opsettings
	// env layer, the bootstrap merge and the admin UI env badges. Every key
	// is checked.
	consumerOpsettings = "opsettings"
	// consumerSeed is LoadSeedEnvKoanf (seedEnvToOpsettingsKey) with the
	// SCION_SEED_ spelling. Seed values only seed Layer-1 keys, so only
	// Layer-1 keys are checked.
	consumerSeed = "seed"
	// consumerGlobalConfig is LoadGlobalConfig (envKeyToConfigKey, and
	// applyEnvToOutsideKoanfFields for koanf:"-" fields), on both the
	// settings.yaml and the legacy
	// server.yaml load path. Checked for keys GlobalConfig has a field for.
	consumerGlobalConfig = "globalconfig"
	// consumerVersioned is LoadVersionedSettings (versionedEnvKeyMapper:
	// the SCION_SERVER_* branch, serverEnvToVersionedKey). Checked for server.* keys
	// VersionedSettings has a field for.
	consumerVersioned = "versioned"
)

var envContractConsumers = []string{consumerOpsettings, consumerSeed, consumerGlobalConfig, consumerVersioned}

// envContractStructuredKeys lists keys whose value is a map or a list of
// objects. No single env string can set them, so they are outside the
// contract. The test checks this list against the schema in both directions.
var envContractStructuredKeys = map[string]bool{
	"default_resources":                 true, // object
	"harness_configs":                   true, // map of objects
	"profiles":                          true, // map of objects
	"runtimes":                          true, // map of objects
	"server.broker.instances":           true, // list of objects
	"server.federation.trusted_issuers": true, // list of objects (ptone/scion#3836)
	"server.hub.test_identities.workload_issuers": true, // list of objects (ptone/scion#4240)
	"server.notification_channels":                true, // list of objects
	"server.plugins.broker":                       true, // map of objects
	"server.project_storage.nfs.shares":           true, // list of objects
	"server.shared_dir_storage.nfs.shares":        true, // list of objects
	"server.workspace_storage.nfs.shares":         true, // list of objects
	"telemetry.cloud.headers":                     true, // map
	"telemetry.filter.sampling.rates":             true, // map
	"telemetry.resource":                          true, // map
}

// schemaNode is a settings-v1 schema property with local $refs merged in.
type schemaNode map[string]interface{}

// collectSchemaNodes walks settings-v1.schema.json and returns every
// property node by dotted path, with a $ref's definition merged under the
// node's own keywords.
func collectSchemaNodes(t *testing.T) map[string]schemaNode {
	t.Helper()
	data, err := config.GetSettingsSchemaJSON("1")
	if err != nil {
		t.Fatalf("read settings schema: %v", err)
	}
	var root map[string]interface{}
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatalf("parse settings schema: %v", err)
	}
	defs, _ := root["$defs"].(map[string]interface{})
	out := map[string]schemaNode{}
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
		if path != "" {
			out[path] = node
		}
		props, _ := node["properties"].(map[string]interface{})
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
	return out
}

func (n schemaNode) hasProperties() bool {
	_, ok := n["properties"]
	return ok
}

// typ returns the node's JSON type, ignoring "null" in a type list
// (["array", "null"] is "array").
func (n schemaNode) typ() string {
	switch v := n["type"].(type) {
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

// isStructured reports whether a leaf node holds a map or a list of objects.
func (n schemaNode) isStructured() bool {
	switch n.typ() {
	case "object":
		return true
	case "array":
		items, _ := n["items"].(map[string]interface{})
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

// entry converts a node to the shape sampleEnvValues takes.
func (n schemaNode) entry(path string) schemaEnvVarEntry {
	e := schemaEnvVarEntry{Path: path, Default: n["default"]}
	e.Type = n.typ()
	if items, ok := n["items"].(map[string]interface{}); ok {
		e.Items, _ = items["type"].(string)
	}
	if enum, ok := n["enum"].([]interface{}); ok {
		for _, v := range enum {
			e.Enum = append(e.Enum, fmt.Sprint(v))
		}
	}
	return e
}

// envContractKey is one setting the contract covers.
type envContractKey struct {
	Path   string
	Layer1 bool
	Node   schemaNode // nil when the schema has no node for the key
}

// envContractKeys returns the leaf registry KoanfPaths and the leaf keys
// under every Layer-0 prefix, sorted by path. A registry KoanfPath that is a
// parent of another registry KoanfPath (telemetry.cloud) is not a leaf.
func envContractKeys(t *testing.T, nodes map[string]schemaNode) []envContractKey {
	t.Helper()
	var registry []string
	for _, s := range Registry {
		registry = append(registry, s.KoanfPaths...)
	}
	isParent := func(p string, all []string) bool {
		for _, q := range all {
			if strings.HasPrefix(q, p+".") {
				return true
			}
		}
		return false
	}
	seen := map[string]bool{}
	var out []envContractKey
	add := func(p string, layer1 bool) {
		if seen[p] {
			return
		}
		seen[p] = true
		out = append(out, envContractKey{Path: p, Layer1: layer1, Node: nodes[p]})
	}
	for _, p := range registry {
		if !isParent(p, registry) {
			add(p, true)
		}
	}
	for _, prefix := range layer0Prefixes {
		n, ok := nodes[prefix]
		if !ok || !n.hasProperties() {
			add(prefix, false)
			continue
		}
		for p, child := range nodes {
			if strings.HasPrefix(p, prefix+".") && !child.hasProperties() {
				add(p, false)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// canonicalEnvSegments spells a dotted path as env segments: underscores
// inside a segment are removed, the segments are joined with "_".
func canonicalEnvSegments(path string) string {
	segs := strings.Split(path, ".")
	for i, s := range segs {
		segs[i] = strings.ToUpper(strings.ReplaceAll(s, "_", ""))
	}
	return strings.Join(segs, "_")
}

// derivedServerEnv is the SCION_SERVER_ spelling derived from a key's path:
// drop a leading "server.", then canonicalEnvSegments.
func derivedServerEnv(path string) string {
	return "SCION_SERVER_" + canonicalEnvSegments(strings.TrimPrefix(path, "server."))
}

// schemaEnvVarDerivationExceptions lists the settings-schema SCION_SERVER_
// x-env-var names that derivedServerEnv does not produce, keyed by settings
// path, with the reason. They are exactly the keys the env mapping table's
// canonical overrides cover (serverEnvCanonicalOverrides in
// pkg/config/server_env_table.go). TestSchemaEnvVarDerivation checks the
// list in both directions, so the derivation, the schema and the table
// cannot drift apart unnoticed. x-env-var names outside SCION_SERVER_ (the
// client settings overlay, e.g. SCION_IMAGE_REGISTRY) are not
// SCION_SERVER_ spellings and are not checked here.
var schemaEnvVarDerivationExceptions = map[string]string{
	"server.broker.allow_container_script_harnesses": envNameRuntimeBroker,
	"server.broker.auto_provide":                     envNameRuntimeBroker,
	"server.broker.broker_id":                        envNameRuntimeBroker,
	"server.broker.broker_name":                      envNameRuntimeBroker,
	"server.broker.broker_nickname":                  envNameRuntimeBroker,
	"server.broker.broker_token":                     envNameRuntimeBroker,
	"server.broker.container_hub_endpoint":           envNameRuntimeBroker,
	"server.broker.cors.allowed_headers":             envNameRuntimeBroker,
	"server.broker.cors.allowed_methods":             envNameRuntimeBroker,
	"server.broker.cors.allowed_origins":             envNameRuntimeBroker,
	"server.broker.cors.enabled":                     envNameRuntimeBroker,
	"server.broker.cors.max_age":                     envNameRuntimeBroker,
	"server.broker.enabled":                          envNameRuntimeBroker,
	"server.broker.host":                             envNameRuntimeBroker,
	"server.broker.hub_endpoint":                     envNameRuntimeBroker,
	"server.broker.port":                             envNameRuntimeBroker,
	"server.broker.read_timeout":                     envNameRuntimeBroker,
	"server.broker.state_dir":                        envNameRuntimeBroker,
	"server.broker.write_timeout":                    envNameRuntimeBroker,
	"server.hub.public_url":                          "named after the GlobalConfig field hub.endpoint (ptone/scion#3905)",
	"server.keyring_file":                            "named SCION_SERVER_KEYRING_FILE by its design (ptone/scion#1530)",
	"server.keyring_file_allow_world_readable":       "named after server.keyring_file's design spelling (ptone/scion#1530)",
}

const envNameRuntimeBroker = "server.broker is documented under the GlobalConfig section name runtimeBroker " +
	"(SCION_SERVER_RUNTIMEBROKER_*); the SCION_SERVER_BROKER_* spellings are aliases"

// canonicalServerEnv is the canonical SCION_SERVER_ spelling of a key: the
// schema's x-env-var name when it is a SCION_SERVER_ name, else the table's
// override for a key listed in schemaEnvVarDerivationExceptions (a key in a
// shared schema definition, such as server.broker.cors.*, has no x-env-var
// of its own), else the derived spelling.
func canonicalServerEnv(key envContractKey) string {
	if ev, _ := key.Node["x-env-var"].(string); strings.HasPrefix(ev, "SCION_SERVER_") {
		return ev
	}
	if _, listed := schemaEnvVarDerivationExceptions[key.Path]; listed {
		if name, ok := config.ServerEnvKey(key.Path); ok {
			return name
		}
	}
	return derivedServerEnv(key.Path)
}

// canonicalSeedEnv is the canonical SCION_SEED_ spelling of a key: the
// canonical SCION_SERVER_ spelling with SCION_ replaced by SCION_SEED_ for
// server.* keys (SCION_SEED_SERVER_HUB_ADMINEMAILS), and SCION_SERVER_
// replaced by SCION_SEED_ for the rest (SCION_SEED_TELEMETRY_ENABLED).
func canonicalSeedEnv(key envContractKey) string {
	name := canonicalServerEnv(key)
	if strings.HasPrefix(key.Path, "server.") {
		return "SCION_SEED_" + strings.TrimPrefix(name, "SCION_")
	}
	return "SCION_SEED_" + strings.TrimPrefix(name, "SCION_SERVER_")
}

// TestSchemaEnvVarDerivation checks the derived SCION_SERVER_ spelling
// against every x-env-var name in the settings schema: each one either
// matches the derivation or is listed in schemaEnvVarDerivationExceptions,
// and each listed exception still differs from the derivation.
func TestSchemaEnvVarDerivation(t *testing.T) {
	nodes := collectSchemaNodes(t)
	count := 0
	for path, n := range nodes {
		ev, _ := n["x-env-var"].(string)
		if !strings.HasPrefix(ev, "SCION_SERVER_") {
			continue
		}
		count++
		_, listed := schemaEnvVarDerivationExceptions[path]
		derived := derivedServerEnv(path)
		switch {
		case ev != derived && !listed:
			t.Errorf("%s: x-env-var %s, derived spelling %s; rename one, or list %q in schemaEnvVarDerivationExceptions",
				path, ev, derived, path)
		case ev == derived && listed:
			t.Errorf("%s: x-env-var now matches the derived spelling; remove it from schemaEnvVarDerivationExceptions", path)
		}
		if canonical, ok := config.ServerEnvKey(path); ok && canonical != ev {
			t.Errorf("%s: x-env-var %s, env mapping table canonical name %s; they must agree", path, ev, canonical)
		}
	}
	// The exceptions are exactly the table's canonical overrides: every
	// table entry whose canonical name is not the derived spelling.
	for _, e := range config.ServerEnvEntries() {
		_, listed := schemaEnvVarDerivationExceptions[e.Path]
		if overridden := e.Canonical != derivedServerEnv(e.Path); overridden != listed {
			t.Errorf("%s: table canonical name %s, derived %s, listed in schemaEnvVarDerivationExceptions: %v",
				e.Path, e.Canonical, derivedServerEnv(e.Path), listed)
		}
	}
	if count < 50 {
		t.Fatalf("found only %d x-env-var names; the schema walker is broken", count)
	}
}

// globalConfigContractRenames maps the keys GlobalConfig models under a
// path that globalConfigPathFor does not produce.
var globalConfigContractRenames = map[string]string{
	"server.federation.debounce_interval": "federation.cache.debounce_interval",
	"server.federation.refresh_interval":  "federation.cache.refresh_interval",
	"telemetry.enabled":                   "telemetryEnabled",
}

// globalConfigContractPath maps a koanf path to the GlobalConfig path
// (matched loosely by structLeaf): globalConfigContractRenames, else
// globalConfigPathFor plus the flattened cors.* fields.
func globalConfigContractPath(path string) string {
	if p, ok := globalConfigContractRenames[path]; ok {
		return p
	}
	p := globalConfigPathFor(path)
	segs := strings.Split(p, ".")
	if n := len(segs); n >= 2 && segs[n-2] == "cors" {
		segs = append(segs[:n-2], "cors"+segs[n-1])
	}
	return strings.Join(segs, ".")
}

// structTagPath returns the koanf tag path of the field of t at the dotted
// path, matching tags as structLeaf does (ignoring case and underscores),
// or "" when t has no such field.
func structTagPath(t reflect.Type, path string) string {
	var tags []string
	for _, seg := range strings.Split(path, ".") {
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		if t.Kind() != reflect.Struct {
			return ""
		}
		found := false
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			tag := strings.Split(f.Tag.Get("koanf"), ",")[0]
			if tag == "" || tag == "-" {
				continue
			}
			if normKey(tag) == normKey(seg) {
				t = f.Type
				tags = append(tags, tag)
				found = true
				break
			}
		}
		if !found {
			return ""
		}
	}
	return strings.Join(tags, ".")
}

// structTypeHas reports whether t has a field at the dotted path.
func structTypeHas(t reflect.Type, path string) bool {
	return structTagPath(t, path) != ""
}

// globalConfigOutsideKoanf (keep in step with globalConfigEnvOverlay in
// pkg/config/hub_config.go) maps the keys GlobalConfig holds outside koanf to
// the field that holds them, which must be tagged koanf:"-". A key is
// either listed itself or sits under a listed prefix, in which case the
// rest of the key is the koanf path inside the field's struct
// (telemetry.cloud.enabled is TelemetryConfig's cloud.enabled). The test
// checks the tag, so an entry whose field gains a koanf tag fails.
var globalConfigOutsideKoanf = map[string]string{
	"agent_secrets.user_scope_only":           "AgentSecretsUserScopeOnly",
	"default_gcp_identity_mode":               "DefaultGCPIdentityMode",
	"default_gcp_identity_service_account_id": "DefaultGCPIdentityServiceAccountID",
	"default_harness_config":                  "DefaultHarnessConfig",
	"default_timezone":                        "DefaultTimezone",
	"project_defaults.default_scratchpad":     "DefaultScratchpad",
	"quotas.enforce_broker_quotas":            "EnforceBrokerQuotas",
	"server.native_chat":                      "NativeChat",
	"server.project_storage":                  "ProjectStorage",
	"server.workspace_storage":                "WorkspaceStorage",
	"telemetry":                               "TelemetryConfig",
}

// globalConfigOutsideKoanfField returns the globalConfigOutsideKoanf field
// for path and the koanf path inside it ("" for the field itself).
func globalConfigOutsideKoanfField(path string) (field, rest string, ok bool) {
	best := ""
	for p := range globalConfigOutsideKoanf {
		if (path == p || strings.HasPrefix(path, p+".")) && len(p) > len(best) {
			best = p
		}
	}
	if best == "" {
		return "", "", false
	}
	return globalConfigOutsideKoanf[best], strings.TrimPrefix(strings.TrimPrefix(path, best), "."), true
}

// outsideKoanfTagProblem reports why field is not a GlobalConfig field
// tagged koanf:"-", or "" when it is.
func outsideKoanfTagProblem(field string) string {
	f, ok := reflect.TypeOf(config.GlobalConfig{}).FieldByName(field)
	if !ok {
		return fmt.Sprintf("GlobalConfig has no field %s", field)
	}
	if tag := strings.Split(f.Tag.Get("koanf"), ",")[0]; tag != "-" {
		return fmt.Sprintf("GlobalConfig.%s is tagged koanf:%q, not koanf:\"-\"", field, tag)
	}
	return ""
}

// globalConfigLeaf returns a function that reads key's leaf from a loaded
// *config.GlobalConfig: the koanf field at the contract path, else the
// field globalConfigOutsideKoanf names.
func globalConfigLeaf(key envContractKey) (leaf func(gc interface{}) (reflect.Value, bool), desc string) {
	gcPath := globalConfigContractPath(key.Path)
	if structTypeHas(reflect.TypeOf(config.GlobalConfig{}), gcPath) {
		return func(gc interface{}) (reflect.Value, bool) { return structLeaf(reflect.ValueOf(gc), gcPath) }, gcPath
	}
	field, rest, _ := globalConfigOutsideKoanfField(key.Path)
	desc = field
	if rest != "" {
		desc += "." + rest
	}
	return func(gc interface{}) (reflect.Value, bool) {
		v := reflect.ValueOf(gc)
		for v.Kind() == reflect.Pointer {
			if v.IsNil() {
				return reflect.Value{}, false
			}
			v = v.Elem()
		}
		v = v.FieldByName(field)
		if !v.IsValid() {
			return reflect.Value{}, false
		}
		if rest != "" {
			return structLeaf(v, rest)
		}
		for v.Kind() == reflect.Pointer {
			if v.IsNil() {
				return reflect.Value{}, false
			}
			v = v.Elem()
		}
		return v, true
	}, desc
}

// Reasons used in envContractNoField.
const (
	noFieldGlobalConfigNotModeled = "GlobalConfig has no field for this key"
)

// envContractNoField lists the (consumer, key) pairs a struct consumer
// (globalconfig, versioned) does not check because its struct has no field
// for the key at all. Keyed "<consumer> <koanf path>". Every struct consumer skip
// must be listed: a key the struct has no field for fails unless listed,
// and so does a listed pair whose field exists. When the struct models the
// key under another path, map it in globalConfigContractRenames; when it
// holds the key in a koanf:"-" field, list the field in
// globalConfigOutsideKoanf (the pair is then checked through that field).
var envContractNoField = map[string]string{
	"globalconfig auto_expose_ports.enabled":                    noFieldGlobalConfigNotModeled,
	"globalconfig default_agent_role":                           noFieldGlobalConfigNotModeled,
	"globalconfig default_max_agent_role":                       noFieldGlobalConfigNotModeled,
	"globalconfig default_max_duration":                         noFieldGlobalConfigNotModeled,
	"globalconfig default_max_model_calls":                      noFieldGlobalConfigNotModeled,
	"globalconfig default_max_turns":                            noFieldGlobalConfigNotModeled,
	"globalconfig default_model":                                noFieldGlobalConfigNotModeled,
	"globalconfig default_runtime_broker":                       noFieldGlobalConfigNotModeled,
	"globalconfig default_template":                             noFieldGlobalConfigNotModeled,
	"globalconfig default_thinking_level":                       noFieldGlobalConfigNotModeled,
	"globalconfig image_registry":                               noFieldGlobalConfigNotModeled,
	"globalconfig server.broker.auto_provide":                   noFieldGlobalConfigNotModeled,
	"globalconfig server.broker.broker_nickname":                noFieldGlobalConfigNotModeled,
	"globalconfig server.broker.broker_token":                   noFieldGlobalConfigNotModeled,
	"globalconfig server.env":                                   noFieldGlobalConfigNotModeled,
	"globalconfig server.home_storage.allow_incomplete_phases":  noFieldGlobalConfigNotModeled,
	"globalconfig server.home_storage.backend":                  noFieldGlobalConfigNotModeled,
	"globalconfig server.home_storage.leaf":                     noFieldGlobalConfigNotModeled,
	"globalconfig server.home_storage.skeleton_max_bytes":       noFieldGlobalConfigNotModeled,
	"globalconfig server.home_storage.stop_grace_seconds":       noFieldGlobalConfigNotModeled,
	"globalconfig server.home_storage.termination_wait_seconds": noFieldGlobalConfigNotModeled,
	"globalconfig server.log_format":                            noFieldGlobalConfigNotModeled,
	"globalconfig server.message_broker.enabled":                noFieldGlobalConfigNotModeled,
	"globalconfig server.message_broker.type":                   noFieldGlobalConfigNotModeled,
	"globalconfig server.message_broker.types":                  noFieldGlobalConfigNotModeled,
	"globalconfig server.shared_dir_storage.backend":            noFieldGlobalConfigNotModeled,
	"globalconfig server.shared_dir_storage.nfs.gid":            noFieldGlobalConfigNotModeled,
	"globalconfig server.shared_dir_storage.nfs.mount_options":  noFieldGlobalConfigNotModeled,
	"globalconfig server.shared_dir_storage.nfs.mount_root":     noFieldGlobalConfigNotModeled,
	"globalconfig server.shared_dir_storage.nfs.storage_class":  noFieldGlobalConfigNotModeled,
	"globalconfig server.shared_dir_storage.nfs.subpath_root":   noFieldGlobalConfigNotModeled,
	"globalconfig server.shared_dir_storage.nfs.uid":            noFieldGlobalConfigNotModeled,
}

// consumerInScope reports whether consumer reads key at all. The versioned
// consumer's SCION_SERVER_* branch maps only under server, and seed values
// only seed Layer-1 keys.
func consumerInScope(consumer string, key envContractKey) bool {
	switch consumer {
	case consumerOpsettings, consumerGlobalConfig:
		return true
	case consumerSeed:
		return key.Layer1
	case consumerVersioned:
		return strings.HasPrefix(key.Path, "server.")
	}
	return false
}

// consumerHasField reports whether consumer has a field for key. The koanf
// consumers take any key.
func consumerHasField(consumer string, key envContractKey) bool {
	switch consumer {
	case consumerGlobalConfig:
		if structTypeHas(reflect.TypeOf(config.GlobalConfig{}), globalConfigContractPath(key.Path)) {
			return true
		}
		_, _, ok := globalConfigOutsideKoanfField(key.Path)
		return ok
	case consumerVersioned:
		return structTypeHas(reflect.TypeOf(config.VersionedSettings{}), key.Path)
	}
	return true
}

// consumerApplies reports whether the contract checks key through consumer.
func consumerApplies(consumer string, key envContractKey) bool {
	return consumerInScope(consumer, key) && consumerHasField(consumer, key)
}

// clearSettingsEnv unsets every SCION_SERVER_* and SCION_SEED_* variable
// for the rest of the test, so a value exported by the surrounding
// environment cannot mask or fake a result.
func clearSettingsEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "SCION_SERVER_") || strings.HasPrefix(name, "SCION_SEED_") {
			t.Setenv(name, "")
			_ = os.Unsetenv(name)
		}
	}
}

// settingsYAMLBase is the settings.yaml the settings-mode load paths read:
// a server key makes LoadGlobalConfig take the settings.yaml path.
func settingsYAMLBase() map[string]interface{} {
	return map[string]interface{}{
		"schema_version": "1",
		"server":         map[string]interface{}{"mode": "workstation"},
	}
}

// setNested sets v at the dotted path in doc, creating maps on the way.
func setNested(doc map[string]interface{}, path string, v interface{}) {
	segs := strings.Split(path, ".")
	cur := doc
	for _, seg := range segs[:len(segs)-1] {
		next, ok := cur[seg].(map[string]interface{})
		if !ok {
			next = map[string]interface{}{}
			cur[seg] = next
		}
		cur = next
	}
	cur[segs[len(segs)-1]] = v
}

// writeYAML writes doc as YAML to path, creating its directory.
func writeYAML(t *testing.T, path string, doc map[string]interface{}) {
	t.Helper()
	data, err := yaml.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal %s: %v", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// yamlValue converts an env sample for key to the typed value a YAML file
// holds ("4243,17" for an integer array is [4243, 17]).
func yamlValue(key envContractKey, s string) interface{} {
	typ, items := "string", ""
	if key.Node != nil {
		e := key.Node.entry(key.Path)
		typ, items = e.Type, e.Items
	}
	scalar := func(typ, s string) interface{} {
		switch typ {
		case "boolean":
			if b, err := strconv.ParseBool(s); err == nil {
				return b
			}
		case "integer":
			if n, err := strconv.Atoi(s); err == nil {
				return n
			}
		case "number":
			if f, err := strconv.ParseFloat(s, 64); err == nil {
				return f
			}
		}
		return s
	}
	if typ == "array" {
		var out []interface{}
		for _, part := range strings.Split(s, ",") {
			out = append(out, scalar(items, part))
		}
		return out
	}
	return scalar(typ, s)
}

// envContractHarness holds the load paths and baselines one run uses.
type envContractHarness struct {
	configDir    string
	gcModes      []struct{ name, home string }
	gcBaseline   map[string]*config.GlobalConfig
	vsBaseline   *config.VersionedSettings
	versionsHome string
}

func newEnvContractHarness(t *testing.T) *envContractHarness {
	t.Helper()
	h := &envContractHarness{configDir: t.TempDir(), gcBaseline: map[string]*config.GlobalConfig{}}
	legacyHome := t.TempDir()
	settingsHome := t.TempDir()
	writeYAML(t, filepath.Join(settingsHome, ".scion", "settings.yaml"), settingsYAMLBase())
	h.gcModes = []struct{ name, home string }{{"legacy", legacyHome}, {"settings", settingsHome}}
	for _, m := range h.gcModes {
		t.Setenv("HOME", m.home)
		gc, err := config.LoadGlobalConfig(h.configDir)
		if err != nil {
			t.Fatalf("baseline LoadGlobalConfig (%s): %v", m.name, err)
		}
		h.gcBaseline[m.name] = gc
	}
	h.versionsHome = settingsHome
	t.Setenv("HOME", settingsHome)
	vs, err := config.LoadVersionedSettings("")
	if err != nil {
		t.Fatalf("baseline LoadVersionedSettings: %v", err)
	}
	h.vsBaseline = vs
	return h
}

// pickValue returns a sample value for key that differs from base (when
// base is valid), so a reached value is distinguishable from the default.
func pickValue(key envContractKey, base reflect.Value) string {
	node := key.Node
	if node == nil {
		node = schemaNode{"type": "string"}
	}
	// An empty sample (an enum may allow "") would read as unset.
	var samples []string
	for _, s := range sampleEnvValues(node.entry(key.Path)) {
		if s != "" {
			samples = append(samples, s)
		}
	}
	if len(samples) == 0 {
		return "envtest"
	}
	for _, s := range samples {
		if !base.IsValid() || !leafEquals(base, s) {
			return s
		}
	}
	return samples[0]
}

// koanfReach checks one env-only koanf loader: a set value lands on path
// verbatim, an empty one leaves path absent.
func koanfReach(t *testing.T, name, path, value string, load func() koanfGetter) (reached bool, emptyProblem string) {
	t.Helper()
	t.Setenv(name, value)
	k := load()
	reached = k.Exists(path) && fmt.Sprint(k.Get(path)) == value
	t.Setenv(name, "")
	if k := load(); k.Exists(path) {
		emptyProblem = fmt.Sprintf("%s=\"\" sets %s to %v; an empty value must be treated as unset", name, path, k.Get(path))
	}
	_ = os.Unsetenv(name)
	return reached, emptyProblem
}

// koanfGetter is the part of *koanf.Koanf koanfReach uses.
type koanfGetter interface {
	Exists(string) bool
	Get(string) interface{}
}

// leafString renders a struct leaf for messages and comparisons.
func leafString(v reflect.Value, ok bool) string {
	if !ok || !v.IsValid() {
		return "<absent>"
	}
	return fmt.Sprintf("%v", v.Interface())
}

// reachResult is the outcome of one (consumer, key) check.
type reachResult struct {
	reached bool
	detail  string // why it did not reach
	empty   []string
}

// versionedReach sets name and reports whether its value reaches key
// through LoadVersionedSettings.
func (h *envContractHarness) versionedReach(t *testing.T, key envContractKey, name string) (reached bool, detail string) {
	t.Helper()
	t.Setenv("HOME", h.versionsHome)
	base, _ := structLeaf(reflect.ValueOf(h.vsBaseline), key.Path)
	v := pickValue(key, base)
	t.Setenv(name, v)
	defer func() { _ = os.Unsetenv(name) }()
	vs, err := config.LoadVersionedSettings("")
	if err != nil {
		return false, fmt.Sprintf("LoadVersionedSettings with %s=%q: %v", name, v, err)
	}
	if got, ok := structLeaf(reflect.ValueOf(vs), key.Path); !ok || !leafEquals(got, v) {
		return false, fmt.Sprintf("%s=%q: VersionedSettings %s = %s", name, v, key.Path, leafString(got, ok))
	}
	return true, ""
}

// envContractSeedContext lists other values the empty-value check writes
// next to a key's YAML seed, keyed by koanf path, for loaders that would
// otherwise overwrite the seed (the sqlite driver pins max_open_conns to 1).
var envContractSeedContext = map[string]map[string]interface{}{
	"server.database.max_open_conns": {"server.database.driver": "postgres"},
}

// envContractEnvContext lists other variables a GlobalConfig reach check
// sets next to the key's variable, keyed by koanf path, for loaders that
// would otherwise overwrite the value (the sqlite driver pins
// max_open_conns to 1).
var envContractEnvContext = map[string]map[string]string{
	"server.database.max_open_conns": {"SCION_SERVER_DATABASE_DRIVER": "postgres"},
}

// setEnvContext sets key's envContractEnvContext variables for the rest of
// the (sub)test.
func setEnvContext(t *testing.T, key envContractKey) {
	t.Helper()
	for name, v := range envContractEnvContext[key.Path] {
		t.Setenv(name, v)
	}
}

// noSeedLegacyServerOutsideKoanf: the legacy load path reads koanf fields
// from server.yaml and fills koanf:"-" fields from top-level settings.yaml
// sections only. A server key in settings.yaml would switch to the
// settings.yaml path, so no legacy file can hold this server.* key.
const noSeedLegacyServerOutsideKoanf = "legacy mode has no file that holds a koanf:\"-\" server.* field"

// envContractNoYAMLSeed lists the "<check label> <key>" pairs where no YAML
// file the loader reads can hold the key, so the empty-value check cannot
// seed a value. For these it only checks that name="" leaves the loaded
// value unchanged. Checked in both directions.
var envContractNoYAMLSeed = map[string]string{
	"[legacy] GlobalConfig server.native_chat.enabled":                                    noSeedLegacyServerOutsideKoanf,
	"[legacy] GlobalConfig server.project_storage.backend":                                noSeedLegacyServerOutsideKoanf,
	"[legacy] GlobalConfig server.project_storage.homes":                                  noSeedLegacyServerOutsideKoanf,
	"[legacy] GlobalConfig server.project_storage.cloudrun_volume.subpath_root":           noSeedLegacyServerOutsideKoanf,
	"[legacy] GlobalConfig server.project_storage.cloudrun_volume.volume_name":            noSeedLegacyServerOutsideKoanf,
	"[legacy] GlobalConfig server.project_storage.gke_shared_volume.pv_claim_name":        noSeedLegacyServerOutsideKoanf,
	"[legacy] GlobalConfig server.project_storage.gke_shared_volume.subpath_root":         noSeedLegacyServerOutsideKoanf,
	"[legacy] GlobalConfig server.project_storage.gke_shared_volume.volume_name":          noSeedLegacyServerOutsideKoanf,
	"[legacy] GlobalConfig server.project_storage.homes_options.allow_incomplete_phases":  noSeedLegacyServerOutsideKoanf,
	"[legacy] GlobalConfig server.project_storage.homes_options.leaf":                     noSeedLegacyServerOutsideKoanf,
	"[legacy] GlobalConfig server.project_storage.homes_options.skeleton_max_bytes":       noSeedLegacyServerOutsideKoanf,
	"[legacy] GlobalConfig server.project_storage.homes_options.stop_grace_seconds":       noSeedLegacyServerOutsideKoanf,
	"[legacy] GlobalConfig server.project_storage.homes_options.termination_wait_seconds": noSeedLegacyServerOutsideKoanf,
	"[legacy] GlobalConfig server.project_storage.nfs.auto_mount":                         noSeedLegacyServerOutsideKoanf,
	"[legacy] GlobalConfig server.project_storage.nfs.gid":                                noSeedLegacyServerOutsideKoanf,
	"[legacy] GlobalConfig server.project_storage.nfs.mount_options":                      noSeedLegacyServerOutsideKoanf,
	"[legacy] GlobalConfig server.project_storage.nfs.mount_root":                         noSeedLegacyServerOutsideKoanf,
	"[legacy] GlobalConfig server.project_storage.nfs.storage_class":                      noSeedLegacyServerOutsideKoanf,
	"[legacy] GlobalConfig server.project_storage.nfs.subpath_root":                       noSeedLegacyServerOutsideKoanf,
	"[legacy] GlobalConfig server.project_storage.nfs.uid":                                noSeedLegacyServerOutsideKoanf,
	"[legacy] GlobalConfig server.workspace_storage.backend":                              noSeedLegacyServerOutsideKoanf,
	"[legacy] GlobalConfig server.workspace_storage.cloudrun_volume.subpath_root":         noSeedLegacyServerOutsideKoanf,
	"[legacy] GlobalConfig server.workspace_storage.cloudrun_volume.volume_name":          noSeedLegacyServerOutsideKoanf,
	"[legacy] GlobalConfig server.workspace_storage.gke_shared_volume.pv_claim_name":      noSeedLegacyServerOutsideKoanf,
	"[legacy] GlobalConfig server.workspace_storage.gke_shared_volume.subpath_root":       noSeedLegacyServerOutsideKoanf,
	"[legacy] GlobalConfig server.workspace_storage.gke_shared_volume.volume_name":        noSeedLegacyServerOutsideKoanf,
	"[legacy] GlobalConfig server.workspace_storage.nfs.auto_mount":                       noSeedLegacyServerOutsideKoanf,
	"[legacy] GlobalConfig server.workspace_storage.nfs.gid":                              noSeedLegacyServerOutsideKoanf,
	"[legacy] GlobalConfig server.workspace_storage.nfs.mount_options":                    noSeedLegacyServerOutsideKoanf,
	"[legacy] GlobalConfig server.workspace_storage.nfs.mount_root":                       noSeedLegacyServerOutsideKoanf,
	"[legacy] GlobalConfig server.workspace_storage.nfs.storage_class":                    noSeedLegacyServerOutsideKoanf,
	"[legacy] GlobalConfig server.workspace_storage.nfs.subpath_root":                     noSeedLegacyServerOutsideKoanf,
	"[legacy] GlobalConfig server.workspace_storage.nfs.uid":                              noSeedLegacyServerOutsideKoanf,
}

// structEmptyCheck checks that name="" is treated as unset by a struct
// loader: it seeds a non-empty YAML value for the key (setup writes the
// file and returns the config dir to load), then loads once without the
// variable, to confirm the seed lands, and once with name="", which must
// leave the seeded value in place.
func structEmptyCheck(t *testing.T, label string, key envContractKey, name, leafPath, seed string,
	leafOf func(interface{}) (reflect.Value, bool),
	setup func() string, load func(dir string) (interface{}, error)) []string {
	t.Helper()
	noSeedEntry := label + " " + key.Path
	_, noSeed := envContractNoYAMLSeed[noSeedEntry]
	dir := setup()
	_ = os.Unsetenv(name)
	got, err := load(dir)
	if err != nil {
		return []string{fmt.Sprintf("%s: load with the YAML seed %q: %v", label, seed, err)}
	}
	seeded, seededOK := leafOf(got)
	seedLands := seededOK && leafEquals(seeded, seed)
	switch {
	case seedLands && noSeed:
		return []string{fmt.Sprintf("%s: the YAML seed now reaches %s; remove %q from envContractNoYAMLSeed",
			label, leafPath, noSeedEntry)}
	case !seedLands && !noSeed:
		return []string{fmt.Sprintf("%s: the YAML seed %q does not reach %s (%s), so the empty-value check "+
			"cannot run; fix the seed, or add %q to envContractNoYAMLSeed",
			label, seed, leafPath, leafString(seeded, seededOK), noSeedEntry)}
	}
	want := seed
	if noSeed {
		want = leafString(seeded, seededOK)
	}
	t.Setenv(name, "")
	defer func() { _ = os.Unsetenv(name) }()
	got, err = load(dir)
	if err != nil {
		return []string{fmt.Sprintf("%s: load with %s=\"\": %v", label, name, err)}
	}
	leaf, ok := leafOf(got)
	if (noSeed && leafString(leaf, ok) != want) || (!noSeed && (!ok || !leafEquals(leaf, want))) {
		return []string{fmt.Sprintf("%s: %s=\"\" replaces the loaded value %s of %s with %s; an empty value "+
			"must be treated as unset", label, name, want, leafPath, leafString(leaf, ok))}
	}
	return nil
}

// seedDoc adds the YAML seed for key, and its envContractSeedContext, to
// doc. pathOf maps a koanf path to the path the file uses.
func seedDoc(doc map[string]interface{}, key envContractKey, seed string, pathOf func(string) string) {
	for p, v := range envContractSeedContext[key.Path] {
		setNested(doc, pathOf(p), v)
	}
	setNested(doc, pathOf(key.Path), yamlValue(key, seed))
}

func (h *envContractHarness) check(t *testing.T, consumer string, key envContractKey) reachResult {
	t.Helper()
	var r reachResult
	switch consumer {
	case consumerOpsettings, consumerSeed:
		name, load := canonicalServerEnv(key), func() koanfGetter { return config.LoadEnvKoanf() }
		if consumer == consumerSeed {
			name, load = canonicalSeedEnv(key), func() koanfGetter { return config.LoadSeedEnvKoanf() }
		}
		v := pickValue(key, reflect.Value{})
		reached, empty := koanfReach(t, name, key.Path, v, load)
		r.reached = reached
		if !reached {
			r.detail = fmt.Sprintf("%s=%q does not land on %s", name, v, key.Path)
		}
		if empty != "" {
			r.empty = append(r.empty, empty)
		}

	case consumerGlobalConfig:
		name := canonicalServerEnv(key)
		leafOf, gcPath := globalConfigLeaf(key)
		_, _, outsideKoanf := globalConfigOutsideKoanfField(key.Path)
		outsideKoanf = outsideKoanf && !structTypeHas(reflect.TypeOf(config.GlobalConfig{}), globalConfigContractPath(key.Path))
		gcTagPathOf := func(p string) string {
			return structTagPath(reflect.TypeOf(config.GlobalConfig{}), globalConfigContractPath(p))
		}
		loadGC := func(dir string) (interface{}, error) { return config.LoadGlobalConfig(dir) }
		r.reached = true
		for _, m := range h.gcModes {
			base, _ := leafOf(h.gcBaseline[m.name])
			v := pickValue(key, base)
			t.Setenv("HOME", m.home)
			t.Setenv(name, v)
			setEnvContext(t, key)
			gc, err := config.LoadGlobalConfig(h.configDir)
			for ctx := range envContractEnvContext[key.Path] {
				_ = os.Unsetenv(ctx)
			}
			if err != nil {
				r.reached = false
				r.detail += fmt.Sprintf("[%s] LoadGlobalConfig with %s=%q: %v; ", m.name, name, v, err)
			} else if got, ok := leafOf(gc); !ok || !leafEquals(got, v) {
				r.reached = false
				r.detail += fmt.Sprintf("[%s] %s=%q: GlobalConfig %s = %s; ", m.name, name, v, gcPath, leafString(got, ok))
			}
			_ = os.Unsetenv(name)

			// Empty value: seed v in the mode's YAML file. A koanf field is
			// seeded in server.yaml at its GlobalConfig path (legacy) or in
			// settings.yaml at the koanf path. A koanf:"-" field is filled
			// from settings.yaml only, so it is seeded there in both modes;
			// in legacy mode only a top-level key can be, since a server key
			// would switch LoadGlobalConfig to the settings.yaml path. A
			// server.* one is written to server.yaml at its path under the
			// server section instead. The legacy loader ignores it today
			// (envContractNoYAMLSeed), and the "seed now reaches" guard
			// fires if it ever reads it.
			mode := m.name
			setup := func() string {
				home, dir := t.TempDir(), t.TempDir()
				t.Setenv("HOME", home)
				switch {
				case mode == "legacy" && outsideKoanf:
					if rest, ok := strings.CutPrefix(key.Path, "server."); ok {
						doc := map[string]interface{}{}
						setNested(doc, rest, yamlValue(key, v))
						writeYAML(t, filepath.Join(dir, "server.yaml"), doc)
					} else {
						doc := map[string]interface{}{"schema_version": "1"}
						seedDoc(doc, key, v, func(p string) string { return p })
						writeYAML(t, filepath.Join(home, ".scion", "settings.yaml"), doc)
					}
				case mode == "legacy":
					doc := map[string]interface{}{}
					seedDoc(doc, key, v, gcTagPathOf)
					writeYAML(t, filepath.Join(dir, "server.yaml"), doc)
				default:
					doc := settingsYAMLBase()
					seedDoc(doc, key, v, func(p string) string { return p })
					writeYAML(t, filepath.Join(home, ".scion", "settings.yaml"), doc)
				}
				return dir
			}
			r.empty = append(r.empty, structEmptyCheck(t, "["+mode+"] GlobalConfig", key, name, gcPath, v,
				leafOf, setup, loadGC)...)
		}

	case consumerVersioned:
		name := canonicalServerEnv(key)
		r.reached, r.detail = h.versionedReach(t, key, name)

		base, _ := structLeaf(reflect.ValueOf(h.vsBaseline), key.Path)
		v := pickValue(key, base)
		setup := func() string {
			home := t.TempDir()
			t.Setenv("HOME", home)
			doc := settingsYAMLBase()
			seedDoc(doc, key, v, func(p string) string { return p })
			writeYAML(t, filepath.Join(home, ".scion", "settings.yaml"), doc)
			return ""
		}
		load := func(string) (interface{}, error) { return config.LoadVersionedSettings("") }
		r.empty = append(r.empty, structEmptyCheck(t, "VersionedSettings", key, name, key.Path, v,
			func(vs interface{}) (reflect.Value, bool) { return structLeaf(reflect.ValueOf(vs), key.Path) },
			setup, load)...)
	}
	return r
}

// TestServerEnvContract checks, for every registry KoanfPath and Layer-0
// key, that the canonical SCION_SERVER_ (and, for Layer-1 keys, SCION_SEED_)
// spelling reaches the key through every env consumer, and that an empty
// value is treated as unset (ptone/scion#3910). Every pair must reach
// (ptone/scion#3911); struct consumers without a field for a key are in
// envContractNoField.
func TestServerEnvContract(t *testing.T) {
	clearSettingsEnv(t)
	nodes := collectSchemaNodes(t)
	keys := envContractKeys(t, nodes)
	if len(keys) < 150 {
		t.Fatalf("found only %d contract keys; the key walker is broken", len(keys))
	}

	// The structured-key list must match the schema exactly.
	known := map[string]bool{}
	for _, k := range keys {
		known[k.Path] = true
		structured := k.Node != nil && k.Node.isStructured()
		if structured && !envContractStructuredKeys[k.Path] {
			t.Errorf("%s holds a map or a list of objects; add it to envContractStructuredKeys", k.Path)
		}
		if !structured && envContractStructuredKeys[k.Path] {
			t.Errorf("envContractStructuredKeys entry %q is not a map or a list of objects; remove it", k.Path)
		}
	}
	for p := range envContractStructuredKeys {
		if !known[p] {
			t.Errorf("envContractStructuredKeys entry %q is not a contract key; remove it", p)
		}
	}

	byPath := map[string]envContractKey{}
	for _, k := range keys {
		byPath[k.Path] = k
	}

	// Every globalConfigOutsideKoanf entry must name a koanf:"-" field and
	// hold at least one key GlobalConfig has no koanf field for.
	for prefix, field := range globalConfigOutsideKoanf {
		if problem := outsideKoanfTagProblem(field); problem != "" {
			t.Errorf("globalConfigOutsideKoanf entry %q: %s", prefix, problem)
		}
		used := false
		for _, k := range keys {
			if (k.Path == prefix || strings.HasPrefix(k.Path, prefix+".")) && !envContractStructuredKeys[k.Path] &&
				!structTypeHas(reflect.TypeOf(config.GlobalConfig{}), globalConfigContractPath(k.Path)) {
				used = true
			}
		}
		if !used {
			t.Errorf("globalConfigOutsideKoanf entry %q holds no contract key; remove it", prefix)
		}
	}

	// Every struct consumer skip must be listed, and only those.
	for entry := range envContractNoField {
		consumer, path, _ := strings.Cut(entry, " ")
		k, ok := byPath[path]
		if !ok || envContractStructuredKeys[path] || !consumerInScope(consumer, k) ||
			(consumer != consumerGlobalConfig && consumer != consumerVersioned) {
			t.Errorf("envContractNoField entry %q names no struct consumer skip; remove it", entry)
		}
	}
	for _, k := range keys {
		if envContractStructuredKeys[k.Path] {
			continue
		}
		for _, consumer := range []string{consumerGlobalConfig, consumerVersioned} {
			if !consumerInScope(consumer, k) {
				continue
			}
			entry := consumer + " " + k.Path
			_, listed := envContractNoField[entry]
			has := consumerHasField(consumer, k)
			switch {
			case !has && !listed:
				t.Errorf("%s: the %s struct has no field at the key's path, so the key is not checked "+
					"through it; map the path, or add %q to envContractNoField", k.Path, consumer, entry)
			case has && listed:
				t.Errorf("%s: the %s struct now has a field for the key; remove %q from envContractNoField",
					k.Path, consumer, entry)
			}
		}
	}

	for entry := range envContractNoYAMLSeed {
		label, path := entry[:strings.LastIndex(entry, " ")], entry[strings.LastIndex(entry, " ")+1:]
		consumer := consumerGlobalConfig
		if label == "VersionedSettings" {
			consumer = consumerVersioned
		}
		if k, ok := byPath[path]; !ok || envContractStructuredKeys[path] || !consumerApplies(consumer, k) {
			t.Errorf("envContractNoYAMLSeed entry %q names no checked key; remove it", entry)
		}
	}

	h := newEnvContractHarness(t)
	for _, k := range keys {
		if envContractStructuredKeys[k.Path] {
			continue
		}
		t.Run(k.Path, func(t *testing.T) {
			for _, consumer := range envContractConsumers {
				if !consumerApplies(consumer, k) {
					continue
				}
				r := h.check(t, consumer, k)
				if !r.reached {
					t.Errorf("%s: canonical spelling does not reach the key through %s: %s; "+
						"add the key to the env mapping table (pkg/config/server_env_table.go)",
						k.Path, consumer, r.detail)
				}
				for _, e := range r.empty {
					t.Errorf("%s (%s): %s", k.Path, consumer, e)
				}
			}
		})
	}
}
