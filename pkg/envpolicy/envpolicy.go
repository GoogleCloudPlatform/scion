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

// Package envpolicy defines the shared, code-owned environment filtering policy
// used by both the runtime broker and sciontool.
package envpolicy

import (
	"slices"
	"sort"
	"strings"
)

// HookWithheldEnvVar is the runtime-owned environment variable that carries the
// sorted, comma-separated list of per-start environment variable names withheld
// from lifecycle hooks.
const HookWithheldEnvVar = "SCION_HOOK_WITHHELD_ENV"

// HookEnvModeDenylist is the default hook environment filtering mode.
const HookEnvModeDenylist = "denylist"

// ReservedHarnessConfigPrefix is the environment variable prefix reserved for
// Scion itself. Harness-config layers cannot set keys with this prefix, except
// for the time-boxed entries in LegacyHarnessHints which are rewritten to
// HarnessHintPrefix.
const ReservedHarnessConfigPrefix = "SCION_"

// HarnessHintPrefix is the prefix for safe, harness-publishable hints.
const HarnessHintPrefix = "HARNESS_HINT_"

// Code-owned harness hint environment variable names.
const (
	HintLivenessRootDepth   = "HARNESS_HINT_LIVENESS_ROOT_DEPTH"
	HintAutoExposePorts     = "HARNESS_HINT_AUTO_EXPOSE_PORTS"
	HintAutoExposeMode      = "HARNESS_HINT_AUTO_EXPOSE_MODE"
	HintAutoExposePortsList = "HARNESS_HINT_AUTO_EXPOSE_PORTS_LIST"
)

// HarnessHint describes a code-owned harness-publishable hint variable and its
// container-side reader.
type HarnessHint struct {
	Name   string
	Reader string
}

// harnessHints is the closed, code-owned list of harness-publishable hints.
// Adding a new hint requires a reader and a test.
var harnessHints = []HarnessHint{
	{Name: HintAutoExposeMode, Reader: "pkg/sciontool/autoexpose"},
	{Name: HintAutoExposePorts, Reader: "pkg/sciontool/autoexpose"},
	{Name: HintAutoExposePortsList, Reader: "pkg/sciontool/autoexpose"},
	{Name: HintLivenessRootDepth, Reader: "pkg/agent/state"},
}

// HarnessHints returns a sorted copy of all code-owned harness hints.
func HarnessHints() []HarnessHint {
	out := make([]HarnessHint, len(harnessHints))
	copy(out, harnessHints)
	return out
}

// legacyHarnessHints maps grandfathered SCION_* names that shipped harness
// configs set onto their HarnessHintPrefix replacements during the transition
// window (removed in the release after this ships).
var legacyHarnessHints = map[string]string{
	"SCION_LIVENESS_HARNESS_ROOT_DEPTH": HintLivenessRootDepth,
	"SCION_AUTO_EXPOSE_PORTS":           HintAutoExposePorts,
	"SCION_AUTO_EXPOSE_MODE":            HintAutoExposeMode,
	"SCION_AUTO_EXPOSE_PORTS_LIST":      HintAutoExposePortsList,
}

// LegacyHarnessHints returns a copy of the grandfathered SCION_* to
// HARNESS_HINT_* rewrite map.
func LegacyHarnessHints() map[string]string {
	out := make(map[string]string, len(legacyHarnessHints))
	for k, v := range legacyHarnessHints {
		out[k] = v
	}
	return out
}

// LegacyHarnessHint reports whether name is a grandfathered SCION_* key and
// returns its HARNESS_HINT_* replacement.
func LegacyHarnessHint(name string) (string, bool) {
	hint, ok := legacyHarnessHints[name]
	return hint, ok
}

// HarnessConfigKeyDisposition classifies an environment variable name supplied
// by a harness-config layer (bundle env, bundle env_template, or settings/profile
// harness_configs.<name>.env).
type HarnessConfigKeyDisposition int

const (
	// HarnessConfigKeyAllow permits the key under its original name.
	HarnessConfigKeyAllow HarnessConfigKeyDisposition = iota
	// HarnessConfigKeyRewrite rewrites a grandfathered SCION_* key to its
	// HARNESS_HINT_* equivalent with a deprecation warning.
	HarnessConfigKeyRewrite
	// HarnessConfigKeyRefuse strips a reserved SCION_* key with a warning.
	HarnessConfigKeyRefuse
)

// ClassifyHarnessConfigEnvKey classifies a key coming from a harness-config
// layer. Any SCION_* key is refused except the grandfathered entries in
// LegacyHarnessHints, which are rewritten to their HARNESS_HINT_* names.
func ClassifyHarnessConfigEnvKey(name string) (effectiveName string, disp HarnessConfigKeyDisposition) {
	if hint, ok := legacyHarnessHints[name]; ok {
		return hint, HarnessConfigKeyRewrite
	}
	if strings.HasPrefix(name, ReservedHarnessConfigPrefix) {
		return "", HarnessConfigKeyRefuse
	}
	return name, HarnessConfigKeyAllow
}

// staticExactDenied is the closed, code-owned set of exact environment variable
// names always withheld from lifecycle hooks.
var staticExactDenied = map[string]struct{}{
	"BASH_ENV":                         {},
	"ENV":                              {},
	"ZDOTDIR":                          {},
	"PROMPT_COMMAND":                   {},
	"SHELLOPTS":                        {},
	"BASHOPTS":                         {},
	"PS4":                              {},
	"IFS":                              {},
	"CDPATH":                           {},
	"GLOBIGNORE":                       {},
	"PYTHONPATH":                       {},
	"PYTHONHOME":                       {},
	"PYTHONSTARTUP":                    {},
	"PYTHONUSERBASE":                   {},
	"PYTHONINSPECT":                    {},
	"NODE_OPTIONS":                     {},
	"NODE_PATH":                        {},
	"PERL5LIB":                         {},
	"PERL5OPT":                         {},
	"PERLLIB":                          {},
	"RUBYLIB":                          {},
	"RUBYOPT":                          {},
	"JAVA_TOOL_OPTIONS":                {},
	"_JAVA_OPTIONS":                    {},
	"JDK_JAVA_OPTIONS":                 {},
	"GCONV_PATH":                       {},
	"XDG_CONFIG_HOME":                  {},
	"XDG_DATA_DIRS":                    {},
	"GIT_EXEC_PATH":                    {},
	"GIT_TEMPLATE_DIR":                 {},
	"GIT_SSH":                          {},
	"GIT_SSH_COMMAND":                  {},
	"GIT_PROXY_COMMAND":                {},
	"GIT_ASKPASS":                      {},
	"SSH_ASKPASS":                      {},
	"GIT_EXTERNAL_DIFF":                {},
	"GIT_PAGER":                        {},
	"GIT_EDITOR":                       {},
	"PAGER":                            {},
	"EDITOR":                           {},
	"VISUAL":                           {},
	"LESSOPEN":                         {},
	"LESSCLOSE":                        {},
	"GIT_DIR":                          {},
	"GIT_WORK_TREE":                    {},
	"GIT_INDEX_FILE":                   {},
	"GIT_OBJECT_DIRECTORY":             {},
	"GIT_ALTERNATE_OBJECT_DIRECTORIES": {},
	"GIT_CEILING_DIRECTORIES":          {},
	"SCION_EXTRA_PATH":                 {},
}

// staticPrefixDenied is the closed, code-owned list of environment variable
// name prefixes always withheld from lifecycle hooks.
var staticPrefixDenied = []string{
	"LD_",
	"DYLD_",
	"GIT_CONFIG",
	"BASH_FUNC_",
}

// HookDenied reports whether name is in the static hook denylist (either by
// exact name or by prefix).
func HookDenied(name string) bool {
	if _, ok := staticExactDenied[name]; ok {
		return true
	}
	for _, prefix := range staticPrefixDenied {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// StaticDenylist returns a sorted copy of all entries in the static denylist.
// Prefix entries are represented with a trailing "*".
func StaticDenylist() []string {
	out := make([]string, 0, len(staticExactDenied)+len(staticPrefixDenied))
	for name := range staticExactDenied {
		out = append(out, name)
	}
	for _, prefix := range staticPrefixDenied {
		out = append(out, prefix+"*")
	}
	sort.Strings(out)
	return out
}

// Set represents the effective set of environment variable names withheld from
// lifecycle hooks: the static denylist plus any per-start dynamic names.
// The zero value is valid and enforces the static denylist.
type Set struct {
	extra map[string]struct{}
}

// NewSet constructs a withheld Set combining the static denylist with extra
// exact variable names.
func NewSet(extra ...string) Set {
	var m map[string]struct{}
	for _, name := range extra {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if m == nil {
			m = make(map[string]struct{}, len(extra))
		}
		m[name] = struct{}{}
	}
	return Set{extra: m}
}

// Withholds reports whether name is withheld by either the static denylist or
// the dynamic per-start set.
func (s Set) Withholds(name string) bool {
	if HookDenied(name) {
		return true
	}
	if len(s.extra) == 0 {
		return false
	}
	_, ok := s.extra[name]
	return ok
}

// Extra returns a sorted copy of the dynamic per-start names in the set.
func (s Set) Extra() []string {
	if len(s.extra) == 0 {
		return nil
	}
	out := make([]string, 0, len(s.extra))
	for name := range s.extra {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// FormatWithheldMarker encodes keys as a sorted, deduplicated, comma-separated
// string suitable for HookWithheldEnvVar. Empty or whitespace-only entries are
// skipped.
func FormatWithheldMarker(keys []string) string {
	if len(keys) == 0 {
		return ""
	}
	cleaned := make([]string, 0, len(keys))
	for _, k := range keys {
		k = strings.TrimSpace(k)
		if k != "" {
			cleaned = append(cleaned, k)
		}
	}
	if len(cleaned) == 0 {
		return ""
	}
	sort.Strings(cleaned)
	cleaned = slices.Compact(cleaned)
	return strings.Join(cleaned, ",")
}

// ValidEnvName reports whether name is a single, unambiguous environment
// variable name (non-empty, no surrounding whitespace, and containing no '=',
// ',', or NUL byte).
func ValidEnvName(name string) bool {
	return name != "" && strings.TrimSpace(name) == name &&
		!strings.ContainsAny(name, "=,\x00")
}

// ParseWithheldMarker decodes a comma-separated HookWithheldEnvVar value into a
// sorted, deduplicated slice of non-empty names. If an entry contains '=', only
// the name portion before the first '=' is kept.
func ParseWithheldMarker(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if k, _, ok := strings.Cut(p, "="); ok {
			p = k
		}
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	return slices.Compact(out)
}
