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
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	"github.com/knadh/koanf/providers/env"

	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// Startup checks and value typing for the SCION_SERVER_* / SCION_SEED_* env
// mapping table (server_env_table.go, ptone/scion#3911).

// directServerBoolEnvNames lists the directly-read SCION_SERVER_* names
// (directServerEnvNames) that hold a boolean, parsed with util.ParseBool by
// their reader. ValidateServerEnv checks them with the same parser.
var directServerBoolEnvNames = map[string]bool{
	"SCION_SERVER_ADMIN_MODE": true, // cmd/server_foreground.go (break-glass admin mode)
}

// parseServerEnvValue parses an env value as the JSON type typ: boolean
// (strconv.ParseBool: true/false, 1/0, t/f), integer (strconv.ParseInt,
// base prefixes allowed) or number (strconv.ParseFloat). Any other type
// returns the value unchanged. Surrounding spaces are ignored.
func parseServerEnvValue(typ, value string) (interface{}, error) {
	s := strings.TrimSpace(value)
	switch typ {
	case "boolean":
		return strconv.ParseBool(s)
	case "integer":
		return strconv.ParseInt(s, 0, 64)
	case "number":
		return strconv.ParseFloat(s, 64)
	}
	return value, nil
}

// convertServerEnvValue returns an env value for the setting at path (a
// settings path) converted to the setting's type, so the opsettings layers
// hold false, not "false", for a boolean. A value that does not parse, or a
// path the table does not hold, is returned unchanged; ValidateServerEnv
// rejects such a value at server startup.
func convertServerEnvValue(path, value string) interface{} {
	t := getServerEnvTable()
	i, ok := t.byPath[path]
	if !ok {
		return value
	}
	v, err := parseServerEnvValue(t.entries[i].Type, value)
	if err != nil {
		return value
	}
	return v
}

// nonEmptyTypedEnvProvider is nonEmptyEnvProvider for the loaders of the
// opsettings keyspace (settings paths): the value is converted to the
// setting's type (convertServerEnvValue).
func nonEmptyTypedEnvProvider(prefix string, mapKey func(string) string) *env.Env {
	return env.ProviderWithValue(prefix, ".", func(name, value string) (string, interface{}) {
		if value == "" {
			return "", nil
		}
		key := mapKey(strings.TrimPrefix(name, prefix))
		if key == "" {
			return "", nil
		}
		return key, convertServerEnvValue(key, value)
	})
}

// lookupSettingsEnv returns the table entry of a SCION_SERVER_* or
// SCION_SEED_* name, and the name's form in the SCION_SERVER_* namespace.
// A SCION_SEED_* name must carry the SERVER_ segment exactly when the
// setting is a server setting. Inert and unknown names return false.
func lookupSettingsEnv(name string) (e ServerEnvEntry, alias bool, seed bool, ok bool) {
	up := strings.ToUpper(name)
	switch {
	case strings.HasPrefix(up, serverEnvPrefix):
		if isServerEnvInert(up) {
			return ServerEnvEntry{}, false, false, false
		}
		e, alias, ok = lookupServerEnv(up)
		return e, alias, false, ok
	case strings.HasPrefix(up, seedEnvPrefix):
		e, alias, ok = lookupServerEnv(serverEnvNameForSeed(up))
		if ok && strings.HasPrefix(up, seedEnvPrefix+"SERVER_") != strings.HasPrefix(e.Path, "server.") {
			ok = false
		}
		return e, alias, true, ok
	}
	return ServerEnvEntry{}, false, false, false
}

// settingsEnvSet returns the upper-cased names of the non-empty variables
// in environ.
func settingsEnvSet(environ []string) map[string]bool {
	set := map[string]bool{}
	for _, kv := range environ {
		if name, value, _ := strings.Cut(kv, "="); value != "" {
			set[strings.ToUpper(name)] = true
		}
	}
	return set
}

// readSettingsEnv returns the table entry of a SCION_SERVER_* or SCION_SEED_*
// name when a consumer reads its value: false for an inert or unknown name,
// for a SCION_SEED_* name of a setting seeding does not seed (isLayer1
// false), and for an alias another spelling of the same setting overrides
// (the precedence of resolveServerEnv, computed from set, the names set in
// the environment). overriddenBy names the winning variable when the name
// is an overridden alias.
func readSettingsEnv(name string, set map[string]bool, isLayer1 func(string) bool) (e ServerEnvEntry, alias, seed bool, overriddenBy string, ok bool) {
	up := strings.ToUpper(name)
	e, alias, seed, ok = lookupSettingsEnv(up)
	if !ok {
		return ServerEnvEntry{}, false, false, "", false
	}
	if seed && (isLayer1 == nil || !isLayer1(e.Path)) {
		return ServerEnvEntry{}, false, false, "", false
	}
	if !alias {
		return e, false, seed, "", true
	}
	spell := func(n string) string {
		if seed {
			return seedEnvNameFor(e, n)
		}
		return n
	}
	if c := spell(e.Canonical); set[c] {
		return e, true, seed, c, true
	}
	for _, a := range e.Aliases {
		if sa := spell(a); sa < up && set[sa] {
			return e, true, seed, sa, true
		}
	}
	return e, true, seed, "", true
}

// ValidateServerEnv checks, for the server's startup, every non-empty
// SCION_SERVER_* and SCION_SEED_* variable in environ (KEY=VALUE pairs, as
// from os.Environ) whose value a consumer reads, for a boolean, integer or
// number setting: the value must parse as that type. It skips a SCION_SEED_*
// name of a setting seeding does not seed (isLayer1 false; pass
// opsettings.IsLayer1Key) and an alias another spelling of the same setting
// overrides, since nothing reads those values. Lists (for example the
// integer list server.hub.conduit.tcp_allowed_ports) are not checked here.
// It also checks each directly-read boolean (SCION_SERVER_ADMIN_MODE) with
// its reader's parser. The error names each variable and its setting, never
// the value. An empty or blank value is unset and is not checked.
func ValidateServerEnv(environ []string, isLayer1 func(string) bool) error {
	set := settingsEnvSet(environ)
	var bad []string
	seen := map[string]bool{}
	for _, kv := range environ {
		name, value, _ := strings.Cut(kv, "=")
		if strings.TrimSpace(value) == "" || seen[name] {
			continue
		}
		seen[name] = true
		if directServerBoolEnvNames[name] {
			if _, ok := util.ParseBool(value); !ok {
				bad = append(bad, fmt.Sprintf("%s: the value is not a boolean (true, 1, t, yes, y, on, or false, 0, f, no, n, off)", name))
			}
			continue
		}
		e, _, _, overriddenBy, ok := readSettingsEnv(name, set, isLayer1)
		if !ok || overriddenBy != "" {
			continue
		}
		if _, err := parseServerEnvValue(e.Type, value); err != nil {
			bad = append(bad, fmt.Sprintf("%s (%s): the value does not parse as %s", name, e.Path, e.Type))
		}
	}
	if len(bad) == 0 {
		return nil
	}
	sort.Strings(bad)
	return fmt.Errorf("invalid settings environment: %s", strings.Join(bad, "; "))
}

// ServerEnvAliasUse is a legacy alias set in the environment.
type ServerEnvAliasUse struct {
	// Name is the alias as set, e.g. SCION_SERVER_HUB_ADMIN_EMAILS.
	Name string
	// Use is the canonical name in the same namespace, e.g.
	// SCION_SERVER_HUB_ADMINEMAILS (SCION_SEED_SERVER_HUB_ADMINEMAILS for a
	// seed alias).
	Use string
	// Path is the setting both set.
	Path string
	// IgnoredFor names the variable that sets the setting instead (the
	// canonical name, or an alias that sorts first), or "" when this alias
	// sets it.
	IgnoredFor string
}

// FindServerEnvAliases returns the legacy aliases set (non-empty) in
// environ whose setting a consumer reads, sorted by name, with the variable
// that wins over each one (see resolveServerEnv for the precedence). A
// SCION_SEED_* alias of a setting seeding does not seed (isLayer1 false) is
// left out: FindUnmatchedSettingsEnv reports it.
func FindServerEnvAliases(environ []string, isLayer1 func(string) bool) []ServerEnvAliasUse {
	set := settingsEnvSet(environ)
	var out []ServerEnvAliasUse
	for name := range set {
		e, alias, seed, overriddenBy, ok := readSettingsEnv(name, set, isLayer1)
		if !ok || !alias {
			continue
		}
		use := e.Canonical
		if seed {
			use = seedEnvNameFor(e, e.Canonical)
		}
		out = append(out, ServerEnvAliasUse{Name: name, Use: use, Path: e.Path, IgnoredFor: overriddenBy})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// WarnDeprecatedServerEnvAliases logs one warning for each legacy alias set
// in environ whose setting a consumer reads (FindServerEnvAliases): the alias still works but is removed
// after the next release. An alias another variable overrides is reported
// as ignored. Call once at server startup. Only names are logged, never
// values.
func WarnDeprecatedServerEnvAliases(logger *slog.Logger, environ []string, isLayer1 func(string) bool) {
	if logger == nil {
		logger = slog.Default()
	}
	for _, u := range FindServerEnvAliases(environ, isLayer1) {
		if u.IgnoredFor != "" {
			logger.Warn("Deprecated environment variable is ignored: another spelling of the same setting is set",
				"env_var", u.Name, "setting", u.Path, "set_by", u.IgnoredFor, "use", u.Use)
			continue
		}
		logger.Warn("Deprecated environment variable spelling; it still works but will be removed after the next release",
			"env_var", u.Name, "setting", u.Path, "use", u.Use)
	}
}
