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
	"log/slog"
	"reflect"
	"sort"
	"strings"
)

const (
	serverEnvPrefix = "SCION_SERVER_"
	seedEnvPrefix   = "SCION_SEED_"
)

// directServerEnvNames lists SCION_SERVER_* names that are read directly with
// os.Getenv rather than through a settings loader, so no settings struct or
// registry key matches them. FindUnmatchedSettingsEnv never flags these.
// Keep this list in step with the os.Getenv call sites it names;
// TestDirectServerEnvNames_MatchGetenvCallSites guards the drift.
var directServerEnvNames = map[string]bool{
	"SCION_SERVER_ADMIN_MODE":          true, // cmd/server_foreground.go (break-glass admin mode)
	"SCION_SERVER_MAINTENANCE_MESSAGE": true, // cmd/server_foreground.go
	"SCION_SERVER_BASE_URL":            true, // cmd/server_foreground.go (hub endpoint, OAuth redirects)
	"SCION_SERVER_SESSION_SECRET":      true, // cmd/server_foreground.go (web session secret)
	"SCION_SERVER_HUB_HUBNAME":         true, // cmd/server_foreground.go, also a settings key
	"SCION_SERVER_HUB_HUBID":           true, // pkg/config/hub_config.go, also a settings key
	"SCION_SERVER_REQUEST_LOG_PATH":    true, // pkg/util/logging/request_log.go
}

// DirectServerEnvNameList returns the directly-read SCION_SERVER_* names,
// sorted.
func DirectServerEnvNameList() []string {
	out := make([]string, 0, len(directServerEnvNames))
	for n := range directServerEnvNames {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// knownInertServerEnvNames maps SCION_SERVER_* names known to reach no
// config the hub reads to the spelling that does, or "" if none does.
// Most are names the settings schema used to advertise (ptone/scion#1081).
// Some bind to a LoadVersionedSettings field the hub ignores, which the
// generic match would accept (see serverEnvMatches), so they are flagged
// explicitly.
var knownInertServerEnvNames = map[string]string{
	"SCION_SERVER_HUB_ADMINEMAIL":                       "SCION_SERVER_HUB_ADMINEMAILS",
	"SCION_SERVER_HUB_SOFT_DELETE_RETENTION":            "SCION_SERVER_HUB_SOFTDELETERETENTION",
	"SCION_SERVER_HUB_SOFT_DELETE_RETAIN_FILES":         "SCION_SERVER_HUB_SOFTDELETERETAINFILES",
	"SCION_SERVER_HUB_AUTO_SUSPEND_STALLED":             "SCION_SERVER_HUB_AUTOSUSPENDSTALLED",
	"SCION_SERVER_HUB_STALLED_THRESHOLD":                "SCION_SERVER_HUB_STALLEDTHRESHOLD",
	"SCION_SERVER_AUTH_USER_ACCESS_MODE":                "SCION_SERVER_AUTH_USERACCESSMODE",
	"SCION_SERVER_SCHEDULER_INTERVAL_SECONDS":           "SCION_SERVER_SCHEDULER_INTERVALSECONDS",
	"SCION_SERVER_SCHEDULER_MAX_CONCURRENCY":            "SCION_SERVER_SCHEDULER_MAXCONCURRENCY",
	"SCION_SERVER_BROKER_ENABLED":                       "SCION_SERVER_RUNTIMEBROKER_ENABLED",
	"SCION_SERVER_BROKER_PORT":                          "SCION_SERVER_RUNTIMEBROKER_PORT",
	"SCION_SERVER_BROKER_HOST":                          "SCION_SERVER_RUNTIMEBROKER_HOST",
	"SCION_SERVER_BROKER_READTIMEOUT":                   "SCION_SERVER_RUNTIMEBROKER_READTIMEOUT",
	"SCION_SERVER_BROKER_READ_TIMEOUT":                  "SCION_SERVER_RUNTIMEBROKER_READTIMEOUT",
	"SCION_SERVER_BROKER_WRITETIMEOUT":                  "SCION_SERVER_RUNTIMEBROKER_WRITETIMEOUT",
	"SCION_SERVER_BROKER_WRITE_TIMEOUT":                 "SCION_SERVER_RUNTIMEBROKER_WRITETIMEOUT",
	"SCION_SERVER_BROKER_HUBENDPOINT":                   "SCION_SERVER_RUNTIMEBROKER_HUBENDPOINT",
	"SCION_SERVER_BROKER_HUB_ENDPOINT":                  "SCION_SERVER_RUNTIMEBROKER_HUBENDPOINT",
	"SCION_SERVER_BROKER_CONTAINERHUBENDPOINT":          "SCION_SERVER_RUNTIMEBROKER_CONTAINERHUBENDPOINT",
	"SCION_SERVER_BROKER_CONTAINER_HUB_ENDPOINT":        "SCION_SERVER_RUNTIMEBROKER_CONTAINERHUBENDPOINT",
	"SCION_SERVER_BROKER_ALLOWCONTAINERSCRIPTHARNESSES": "SCION_SERVER_RUNTIMEBROKER_ALLOWCONTAINERSCRIPTHARNESSES",
	"SCION_SERVER_BROKER_BROKERID":                      "SCION_SERVER_BROKER_BROKER_ID",
	"SCION_SERVER_BROKER_BROKERNAME":                    "SCION_SERVER_BROKER_BROKER_NAME",
	"SCION_SERVER_BROKER_BROKERNICKNAME":                "SCION_SERVER_BROKER_BROKER_NICKNAME",
	"SCION_SERVER_BROKER_BROKERTOKEN":                   "SCION_SERVER_BROKER_BROKER_TOKEN",
	"SCION_SERVER_BROKER_AUTOPROVIDE":                   "SCION_SERVER_BROKER_AUTO_PROVIDE",
	"SCION_SERVER_HUB_PUBLIC_URL":                       "SCION_SERVER_HUB_ENDPOINT",
	// No SCION_SERVER_* spelling sets the boot log level or format; see
	// knownInertServerEnvNotes.
	"SCION_SERVER_LOG_LEVEL":  "SCION_SERVER_LOGLEVEL",
	"SCION_SERVER_LOG_FORMAT": "",
	// server.env binds in VersionedSettings, but nothing reads it.
	"SCION_SERVER_ENV": "",
}

// knownInertServerEnvNotes adds a short explanation to the warning for
// known-inert names whose suggestion alone would mislead.
var knownInertServerEnvNotes = map[string]string{
	"SCION_SERVER_LOG_LEVEL":  "no boot-time override: SCION_SERVER_LOGLEVEL only affects file-mode reload; at startup use --debug or SCION_LOG_LEVEL=debug",
	"SCION_SERVER_LOG_FORMAT": "server.log_format is not read by the hub",
	"SCION_SERVER_ENV":        "server.env is informational and not read by the hub",
}

// UnmatchedEnvName is a SCION_SERVER_* or SCION_SEED_* environment variable
// name that no settings loader maps to a setting.
type UnmatchedEnvName struct {
	Name string
	// Suggestion is a spelling that does match, or "" if none is known.
	Suggestion string
	// Note is an optional short explanation, logged with the warning.
	Note string
}

// FindUnmatchedSettingsEnv returns the SCION_SERVER_* and SCION_SEED_* names
// in environ (KEY=VALUE pairs, as from os.Environ; values are ignored) that
// match nothing the hub reads.
//
// The policy is accept-by-default: a warning is only issued when the name
// provably matches nothing, because a false warning on a working override
// is worse than a missing one.
//
// A SCION_SERVER_* name matches when it is read directly with os.Getenv
// (directServerEnvNames), or its mapped key resolves to a Layer-1 registry
// key (isLayer1), a GlobalConfig field, or a VersionedSettings field that the
// hub does not take from GlobalConfig or the opsettings snapshot instead. A
// name in knownInertServerEnvNames is always reported. A SCION_SEED_* name
// matches only when its mapped key is a Layer-1 registry key, since seed
// values only seed Layer-1 settings.
//
// Known gaps (accepted but still without effect at the hub):
//   - SCION_SERVER_HUB_PUBLICURL maps to the Layer-1 server.hub.public_url,
//     which only feeds the admin server-config view; the hub's endpoint is
//     GlobalConfig Hub.Endpoint (SCION_SERVER_HUB_ENDPOINT).
//   - SCION_SERVER_OIDCLOGIN_* binds GlobalConfig only on the settings.yaml
//     path; on the legacy server.yaml path the oidcLogin.* defaults shadow
//     it (ptone/scion#3038).
//   - Other VersionedSettings-only matches whose server reader is unknown
//     to this helper are accepted (see serverEnvMatches).
//
// isLayer1 reports whether an opsettings koanf key is owned by a registry
// section (opsettings.IsLayer1Key); it is a parameter because pkg/config
// cannot import pkg/config/opsettings.
func FindUnmatchedSettingsEnv(environ []string, isLayer1 func(string) bool) []UnmatchedEnvName {
	if isLayer1 == nil {
		isLayer1 = func(string) bool { return false }
	}
	seen := map[string]bool{}
	var out []UnmatchedEnvName
	for _, kv := range environ {
		name := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			name = kv[:i]
		}
		if seen[name] {
			continue
		}
		seen[name] = true

		switch {
		case strings.HasPrefix(name, serverEnvPrefix):
			if s, ok := knownInertServerEnvNames[name]; ok {
				out = append(out, UnmatchedEnvName{Name: name, Suggestion: s, Note: knownInertServerEnvNotes[name]})
				continue
			}
			if serverEnvMatches(name, isLayer1) {
				continue
			}
			out = append(out, UnmatchedEnvName{
				Name:       name,
				Suggestion: suggestEnvName(name, serverEnvPrefix, func(n string) bool { return serverEnvMatches(n, isLayer1) }),
			})
		case strings.HasPrefix(name, seedEnvPrefix):
			if seedEnvMatches(name, isLayer1) {
				continue
			}
			out = append(out, UnmatchedEnvName{
				Name:       name,
				Suggestion: suggestEnvName(name, seedEnvPrefix, func(n string) bool { return seedEnvMatches(n, isLayer1) }),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// WarnUnmatchedSettingsEnv logs one warning per name returned by
// FindUnmatchedSettingsEnv. Only names are logged, never values.
func WarnUnmatchedSettingsEnv(logger *slog.Logger, environ []string, isLayer1 func(string) bool) {
	if logger == nil {
		logger = slog.Default()
	}
	for _, u := range FindUnmatchedSettingsEnv(environ, isLayer1) {
		attrs := []any{"env_var", u.Name}
		if u.Suggestion != "" {
			attrs = append(attrs, "did_you_mean", u.Suggestion)
		}
		if u.Note != "" {
			attrs = append(attrs, "note", u.Note)
		}
		logger.Warn("Environment variable matches no setting and is ignored", attrs...)
	}
}

func serverEnvMatches(name string, isLayer1 func(string) bool) bool {
	if directServerEnvNames[name] {
		return true
	}
	if _, renamed := knownInertServerEnvNames[name]; renamed {
		return false
	}
	rest := strings.TrimPrefix(name, serverEnvPrefix)
	if rest == "" {
		return false
	}
	if isLayer1(serverEnvToOpsettingsKey(rest)) {
		return true
	}
	if structPathExists(reflect.TypeOf(GlobalConfig{}), strings.Split(envKeyToConfigKey(rest), "."), matchFoldCase) {
		return true
	}
	// A VersionedSettings match is accepted unless the hub is known to read
	// that key from GlobalConfig or the opsettings snapshot instead (accept
	// by default). Underscored spellings such as
	// SCION_SERVER_HUB_ADMIN_EMAILS bind in VersionedSettings, but the hub
	// takes that key from the snapshot, so they do nothing and are flagged.
	if vsKey := versionedEnvKeyMapper(name); vsKey != "" {
		if structPathExists(reflect.TypeOf(VersionedSettings{}), strings.Split(vsKey, "."), matchExact) {
			return VersionedSettingsReadServerKeys[vsKey] || !hubReadsKeyElsewhere(vsKey, isLayer1)
		}
	}
	return false
}

// VersionedSettingsReadServerKeys lists the server.* keys whose effective
// value is taken from LoadVersionedSettings even though GlobalConfig has a
// field for them: broker identity, resolved at hub/broker startup in
// cmd/server_foreground.go (resolveBrokerID, resolveBrokerName, the
// auto_provide lookup). broker_token is read only by CLI paths
// (Settings.Hub.BrokerToken via convertVersionedToLegacy, e.g. cmd/root.go
// and cmd/hub.go), not hub startup; its VersionedSettings spelling is still
// the only one that reaches any reader.
var VersionedSettingsReadServerKeys = map[string]bool{
	"server.broker.broker_id":       true,
	"server.broker.broker_name":     true,
	"server.broker.broker_nickname": true,
	"server.broker.broker_token":    true,
	"server.broker.auto_provide":    true,
}

// hubReadsKeyElsewhere reports whether the hub reads the settings key
// (VersionedSettings keyspace, e.g. server.hub.admin_emails) from the
// opsettings snapshot or from GlobalConfig rather than from
// LoadVersionedSettings.
func hubReadsKeyElsewhere(key string, isLayer1 func(string) bool) bool {
	if isLayer1(key) {
		return true
	}
	rest, ok := strings.CutPrefix(key, "server.")
	if !ok {
		return false
	}
	segs := strings.Split(rest, ".")
	if segs[0] == "broker" {
		segs[0] = "runtimeBroker"
	}
	// GlobalConfig flattens cors.* into corsEnabled, corsMaxAge, ...
	if n := len(segs); n >= 2 && segs[n-2] == "cors" {
		segs = append(segs[:n-2], "cors"+segs[n-1])
	}
	return structPathExists(reflect.TypeOf(GlobalConfig{}), segs, matchIgnoringUnderscores)
}

func seedEnvMatches(name string, isLayer1 func(string) bool) bool {
	rest := strings.TrimPrefix(name, seedEnvPrefix)
	if rest == "" {
		return false
	}
	return isLayer1(envKeyToOpsettingsKey(rest))
}

// suggestEnvName proposes a matching spelling for an unmatched name: first
// with the BROKER segment replaced by RUNTIMEBROKER (SCION_SERVER_ only),
// then with the trailing segments collapsed into one (HUB_ADMIN_EMAILS ->
// HUB_ADMINEMAILS, IMAGE_REGISTRY -> IMAGEREGISTRY), trying the shortest
// collapse first.
func suggestEnvName(name, prefix string, matches func(string) bool) string {
	rest := strings.TrimPrefix(name, prefix)
	bases := []string{rest}
	if prefix == serverEnvPrefix && strings.HasPrefix(rest, "BROKER_") {
		bases = append(bases, "RUNTIME"+rest)
	}
	for _, base := range bases {
		segs := strings.Split(base, "_")
		for i := len(segs) - 1; i >= 0; i-- {
			cand := strings.Join(segs[i:], "")
			if i > 0 {
				cand = strings.Join(segs[:i], "_") + "_" + cand
			}
			if cand == rest {
				continue
			}
			if matches(prefix + cand) {
				return prefix + cand
			}
		}
	}
	return ""
}

// tagMatcher reports whether a path segment selects a field with koanf tag.
type tagMatcher func(tag, seg string) bool

// matchExact is how VersionedSettings' snake_case keys bind.
func matchExact(tag, seg string) bool { return tag == seg }

// matchFoldCase is how GlobalConfig's mapstructure decode binds the keys
// envKeyToConfigKey produces (hub.adminEmails, scheduler.intervalseconds).
func matchFoldCase(tag, seg string) bool { return strings.EqualFold(tag, seg) }

// matchIgnoringUnderscores relates a snake_case settings key to a
// GlobalConfig field whatever that field's tag style (adminEmails,
// max_open_conns), to ask whether GlobalConfig carries the key at all.
func matchIgnoringUnderscores(tag, seg string) bool {
	return strings.EqualFold(strings.ReplaceAll(tag, "_", ""), strings.ReplaceAll(seg, "_", ""))
}

// structPathExists reports whether path names a field reachable from t via
// koanf tags, comparing each segment with match. A map field accepts any
// remaining path.
func structPathExists(t reflect.Type, path []string, match tagMatcher) bool {
	for len(path) > 0 {
		t = derefType(t)
		switch t.Kind() {
		case reflect.Map:
			return true
		case reflect.Struct:
		default:
			return false
		}
		next, ok := findKoanfField(t, path[0], match)
		if !ok {
			return false
		}
		t = next
		path = path[1:]
	}
	return true
}

func findKoanfField(t reflect.Type, seg string, match tagMatcher) (reflect.Type, bool) {
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
			if f.Anonymous {
				if ft, ok := findKoanfField(derefType(f.Type), seg, match); ok {
					return ft, true
				}
				continue
			}
			tag = f.Name
		}
		if match(tag, seg) {
			return f.Type, true
		}
	}
	return nil, false
}

func derefType(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}
