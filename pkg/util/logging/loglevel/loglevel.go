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

// Package loglevel is the single source of truth for Scion log verbosity.
//
// It parses SCION_LOG_LEVEL (and the deprecated SCION_DEBUG alias) and holds
// the process-wide level state that the server log handlers, the CLI debug
// output (util.Debugf) and sciontool's agent.log all consult.
//
// The level spec syntax is a comma-separated list. The first bare token is
// the default level; the remaining entries are component=level pairs:
//
//	debug
//	warn
//	info,hub.auth=debug,hubsync=debug
//
// Levels are debug, info, warn (or warning) and error, case-insensitive.
// Component keys are matched against logging subsystem names (for example
// "hub.auth") and CLI debug tags (for example "hubsync"). Matching is
// hierarchical on dot boundaries: "hub=debug" applies to "hub.auth" and
// "hub.maintenance.pull-images" unless a longer key is also present.
// Unknown component keys are kept; they simply never match.
//
// Precedence between sources is flag > environment > setting > default; a
// lower-precedence source never overrides a level set by a higher one.
//
// The package depends only on the standard library so that lightweight
// packages (pkg/util, pkg/sciontool/log) can import it without pulling in
// the cloud logging dependencies of pkg/util/logging.
package loglevel

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// Environment variable names.
const (
	// EnvLogLevel holds a level spec (see the package documentation).
	EnvLogLevel = "SCION_LOG_LEVEL"
	// EnvDebug is the deprecated alias for SCION_LOG_LEVEL=debug. Any
	// non-empty value enables it.
	EnvDebug = "SCION_DEBUG"
)

// DeprecationWarning is printed (once per process) when SCION_DEBUG is used.
const DeprecationWarning = "Warning: SCION_DEBUG is deprecated and will be removed in a future release; use SCION_LOG_LEVEL=debug instead."

// Source identifies where a level came from. Higher values take precedence.
type Source int

const (
	// SourceDefault is the built-in default (info).
	SourceDefault Source = iota
	// SourceSetting is a settings-file value such as server.log_level.
	SourceSetting
	// SourceEnv is SCION_LOG_LEVEL or the deprecated SCION_DEBUG alias.
	SourceEnv
	// SourceFlag is an explicit command-line flag.
	SourceFlag
)

// String returns a human-readable name for the source.
func (s Source) String() string {
	switch s {
	case SourceDefault:
		return "default"
	case SourceSetting:
		return "setting"
	case SourceEnv:
		return "env"
	case SourceFlag:
		return "flag"
	}
	return fmt.Sprintf("Source(%d)", int(s))
}

// Spec is a parsed level spec: a default level plus optional per-component
// overrides.
type Spec struct {
	Default    slog.Level
	Components map[string]slog.Level
}

// DefaultSpec returns the built-in spec: info, no component overrides.
func DefaultSpec() Spec {
	return Spec{Default: slog.LevelInfo}
}

// ParseLevel parses a single level name (debug, info, warn, warning, error),
// case-insensitively and ignoring surrounding whitespace.
func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return slog.LevelInfo, fmt.Errorf("invalid log level %q (want debug, info, warn or error)", strings.TrimSpace(s))
}

// ParseLevelSpec parses a level spec. It always returns a usable Spec: an
// invalid default level falls back to info and an invalid component entry is
// dropped. The returned error, when non-nil, describes every problem found.
// An empty spec parses to DefaultSpec with no error.
func ParseLevelSpec(s string) (Spec, error) {
	spec := DefaultSpec()
	var errs []error
	first := true
	for _, raw := range strings.Split(s, ",") {
		tok := strings.TrimSpace(raw)
		if tok == "" {
			continue
		}
		isFirst := first
		first = false
		key, val, isPair := strings.Cut(tok, "=")
		if !isPair {
			if !isFirst {
				errs = append(errs, fmt.Errorf("unexpected bare level %q: only the first entry may omit a component", tok))
				continue
			}
			lvl, err := ParseLevel(tok)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			spec.Default = lvl
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		if key == "" {
			errs = append(errs, fmt.Errorf("missing component name in %q", tok))
			continue
		}
		lvl, err := ParseLevel(val)
		if err != nil {
			errs = append(errs, fmt.Errorf("component %q: %w", key, err))
			continue
		}
		if spec.Components == nil {
			spec.Components = make(map[string]slog.Level)
		}
		spec.Components[key] = lvl
	}
	return spec, errors.Join(errs...)
}

// Level returns the effective level for component: the level of the longest
// matching component key (exact or dotted-prefix match), else the default.
// An empty component returns the default.
func (s Spec) Level(component string) slog.Level {
	if lvl, ok := s.ComponentLevel(component); ok {
		return lvl
	}
	return s.Default
}

// ComponentLevel returns the level of the longest component key matching
// component, and whether any key matched.
func (s Spec) ComponentLevel(component string) (slog.Level, bool) {
	if component == "" || len(s.Components) == 0 {
		return 0, false
	}
	name := strings.ToLower(component)
	for {
		if lvl, ok := s.Components[name]; ok {
			return lvl, true
		}
		i := strings.LastIndexByte(name, '.')
		if i < 0 {
			return 0, false
		}
		name = name[:i]
	}
}

// MinLevel returns the most verbose level in the spec (the default or any
// component override). Handlers use it as a cheap pre-filter.
func (s Spec) MinLevel() slog.Level {
	m := s.Default
	for _, lvl := range s.Components {
		if lvl < m {
			m = lvl
		}
	}
	return m
}

// String renders the spec in canonical form, e.g. "info,hub.auth=debug".
func (s Spec) String() string {
	var b strings.Builder
	b.WriteString(levelName(s.Default))
	keys := make([]string, 0, len(s.Components))
	for k := range s.Components {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteString(",")
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(levelName(s.Components[k]))
	}
	return b.String()
}

func levelName(l slog.Level) string {
	return strings.ToLower(l.String())
}

func (s Spec) clone() Spec {
	c := Spec{Default: s.Default}
	if len(s.Components) > 0 {
		c.Components = make(map[string]slog.Level, len(s.Components))
		for k, v := range s.Components {
			c.Components[k] = v
		}
	}
	return c
}

// SpecFromEnv resolves the level spec from the environment using getenv
// (os.Getenv when nil). SCION_LOG_LEVEL wins over SCION_DEBUG when both are
// set. ok reports whether either variable was set. warnings lists messages
// that should be shown to the user (deprecation, parse problems); SpecFromEnv
// itself prints nothing.
func SpecFromEnv(getenv func(string) string) (spec Spec, ok bool, warnings []string) {
	if getenv == nil {
		getenv = os.Getenv
	}
	if v := strings.TrimSpace(getenv(EnvLogLevel)); v != "" {
		spec, err := ParseLevelSpec(v)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("Warning: %s: %v; using %q", EnvLogLevel, err, spec.String()))
		}
		return spec, true, warnings
	}
	if getenv(EnvDebug) != "" {
		return Spec{Default: slog.LevelDebug}, true, []string{DeprecationWarning}
	}
	return DefaultSpec(), false, nil
}

// --- process-wide state ---

type state struct {
	spec   Spec
	source Source
}

var (
	// defaultVar mirrors the current default level for handlers that want
	// a slog.Leveler.
	defaultVar slog.LevelVar
	// minVar mirrors the current minimum level across default and
	// components.
	minVar slog.LevelVar
	// cur holds the current *state; replaced atomically on change.
	cur atomic.Pointer[state]

	initMu   sync.Mutex
	initDone atomic.Bool

	// ignoreDebugAlias makes the process-wide state skip the deprecated
	// SCION_DEBUG alias; see SetIgnoreDebugAlias.
	ignoreDebugAlias atomic.Bool

	warnMu  sync.Mutex
	warnOut io.Writer = os.Stderr
	warned            = map[string]bool{}
)

func init() {
	storeLocked(&state{spec: DefaultSpec(), source: SourceDefault})
}

func storeLocked(s *state) {
	cur.Store(s)
	defaultVar.Set(s.spec.Default)
	minVar.Set(s.spec.MinLevel())
}

// ensureInit lazily applies the environment the first time the state is
// read or written, so every consumer sees the same env-derived levels.
func ensureInit() {
	if initDone.Load() {
		return
	}
	initMu.Lock()
	defer initMu.Unlock()
	if initDone.Load() {
		return
	}
	spec, ok, warnings := SpecFromEnv(processGetenv())
	if ok {
		storeLocked(&state{spec: spec, source: SourceEnv})
	}
	initDone.Store(true)
	for _, w := range warnings {
		warnOnce(w)
	}
}

// processGetenv returns the lookup used for the process-wide state:
// os.Getenv, with SCION_DEBUG hidden while SetIgnoreDebugAlias is on.
func processGetenv() func(string) string {
	if !ignoreDebugAlias.Load() {
		return os.Getenv
	}
	return func(key string) string {
		if key == EnvDebug {
			return ""
		}
		return os.Getenv(key)
	}
}

// SetIgnoreDebugAlias controls whether the process-wide state honours the
// deprecated SCION_DEBUG alias. The scion CLI turns this on inside agent
// containers: there SCION_DEBUG is usually inherited from an older broker
// that set it in every agent it started, not chosen for the command being
// run. SCION_LOG_LEVEL and explicit flags are not affected, and nothing is
// printed for an ignored SCION_DEBUG.
//
// If the environment has already been applied, the level is re-resolved so
// the change takes effect; a level set by a flag is left alone.
func SetIgnoreDebugAlias(ignore bool) {
	initMu.Lock()
	defer initMu.Unlock()
	ignoreDebugAlias.Store(ignore)
	if !initDone.Load() {
		return // the lazy environment read will apply it
	}
	s := cur.Load()
	if s.source > SourceEnv {
		return
	}
	spec, ok, warnings := SpecFromEnv(processGetenv())
	switch {
	case ok:
		storeLocked(&state{spec: spec, source: SourceEnv})
	case s.source == SourceEnv:
		// The environment no longer sets a level.
		storeLocked(&state{spec: DefaultSpec(), source: SourceDefault})
	}
	for _, w := range warnings {
		warnOnce(w)
	}
}

// warnOnce prints msg to the warning output unless it was already printed
// in this process.
func warnOnce(msg string) {
	warnMu.Lock()
	defer warnMu.Unlock()
	if warned[msg] {
		return
	}
	warned[msg] = true
	if warnOut != nil {
		_, _ = fmt.Fprintln(warnOut, msg)
	}
}

// SetWarningOutput sets where one-time warnings (SCION_DEBUG deprecation,
// invalid SCION_LOG_LEVEL) are written. The default is os.Stderr; pass
// io.Discard (or nil) to silence them, e.g. in subprocesses whose stderr is
// captured by a caller.
func SetWarningOutput(w io.Writer) {
	warnMu.Lock()
	defer warnMu.Unlock()
	warnOut = w
}

// Apply installs spec as the process-wide level spec if src has at least
// the precedence of the source that set the current spec. It reports
// whether the spec was applied.
func Apply(spec Spec, src Source) bool {
	ensureInit()
	initMu.Lock()
	defer initMu.Unlock()
	if src < cur.Load().source {
		return false
	}
	storeLocked(&state{spec: spec.clone(), source: src})
	return true
}

// ApplyString parses s with ParseLevelSpec and applies the result at src.
// A parse problem is returned (and the fallback spec is still applied), so
// callers can surface it; ApplyString does not print it.
func ApplyString(s string, src Source) (applied bool, err error) {
	spec, err := ParseLevelSpec(s)
	return Apply(spec, src), err
}

// SetSetting applies a settings-file level spec (for example the
// server.log_level setting) at SourceSetting precedence: it takes effect
// only when neither a flag nor the environment chose a level. It is safe to
// call again on settings reload.
func SetSetting(s string) (applied bool, err error) {
	return ApplyString(s, SourceSetting)
}

// EnableDebug raises the default level to debug at src precedence, keeping
// any per-component overrides. It is the mapping for --debug style flags.
func EnableDebug(src Source) bool {
	ensureInit()
	initMu.Lock()
	defer initMu.Unlock()
	s := cur.Load()
	if src < s.source {
		return false
	}
	next := s.spec.clone()
	next.Default = slog.LevelDebug
	storeLocked(&state{spec: next, source: src})
	return true
}

// Current returns a copy of the current spec and the source that set it.
func Current() (Spec, Source) {
	ensureInit()
	s := cur.Load()
	return s.spec.clone(), s.source
}

// Effective returns the effective level for component (see Spec.Level).
func Effective(component string) slog.Level {
	ensureInit()
	return cur.Load().spec.Level(component)
}

// HasComponents reports whether the current spec has any per-component
// levels. Handlers use it to skip looking for a subsystem attribute on
// records when no component could change the outcome.
func HasComponents() bool {
	ensureInit()
	return len(cur.Load().spec.Components) > 0
}

// ComponentLevel returns the explicitly configured level for component, if
// any component key matches it.
func ComponentLevel(component string) (slog.Level, bool) {
	ensureInit()
	return cur.Load().spec.ComponentLevel(component)
}

// Enabled reports whether a record at level for component should be
// emitted under the current spec.
func Enabled(component string, level slog.Level) bool {
	return level >= Effective(component)
}

// DebugEnabled reports whether debug output is enabled for component
// (the default level when component is empty).
func DebugEnabled(component string) bool {
	return Enabled(component, slog.LevelDebug)
}

// DefaultLevel returns a slog.Leveler that tracks the current default level.
func DefaultLevel() slog.Leveler {
	ensureInit()
	return &defaultVar
}

// MinLevel returns a slog.Leveler that tracks the most verbose level in the
// current spec (default or any component).
func MinLevel() slog.Leveler {
	ensureInit()
	return &minVar
}

// Reset restores the built-in default and makes the next read re-resolve
// the environment; it also turns SetIgnoreDebugAlias off. One-time warnings
// already printed stay suppressed unless resetWarnings is true. It exists
// for tests and for processes that change their environment deliberately
// before configuring logging.
func Reset(resetWarnings bool) {
	initMu.Lock()
	storeLocked(&state{spec: DefaultSpec(), source: SourceDefault})
	initDone.Store(false)
	ignoreDebugAlias.Store(false)
	initMu.Unlock()
	if resetWarnings {
		warnMu.Lock()
		warned = map[string]bool{}
		warnMu.Unlock()
	}
}
