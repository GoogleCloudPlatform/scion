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
	"strings"
	"sync"

	yamlv3 "gopkg.in/yaml.v3"
)

// Retention of built-in default profiles and runtimes (ptone/scion#5113).
//
// When the hub's stored profiles (or runtimes) replace the settings file's
// catalog, the built-in default profiles (pkg/config/embeds) vanish with it.
// Agents saved on them, and the default profile, then fail to resolve. The
// overlay therefore keeps (retains) each built-in profile that is still
// referenced, with its built-in definition, and each built-in runtime an
// effective profile names that the stored runtimes lack.
//
// Retention is off until the hub enables it (SetProfileReferences, or
// EnableConservativeRetention when references could not be counted), so a
// throwaway overlay, a flat host, or a hub without a co-located broker
// behaves exactly as before. Once a name is retained it stays retained for
// the life of the overlay (the process): a later reference count only adds
// names. Unreferenced built-ins drop at the next start.

// ProfileReferences counts, per profile name, the records that name it. The
// "" key of Agents counts agents with no saved profile; they resolve through
// active_profile. Computed by the hub (CountProfileReferences); the overlay
// never reads the store.
type ProfileReferences struct {
	Agents   map[string]int
	Projects map[string]int

	// AgentInfoMissing counts agents placed on the co-located broker whose
	// agent-info.json was not found, and AgentInfoUnreadable those whose
	// file exists but could not be read or parsed. Diagnostics only: a
	// non-zero AgentInfoMissing is printed in the retention WARN, because it
	// can mean the reference count looked in the wrong place.
	AgentInfoMissing    int
	AgentInfoUnreadable int
}

func (r ProfileReferences) clone() ProfileReferences {
	out := ProfileReferences{
		AgentInfoMissing:    r.AgentInfoMissing,
		AgentInfoUnreadable: r.AgentInfoUnreadable,
	}
	if r.Agents != nil {
		out.Agents = make(map[string]int, len(r.Agents))
		for k, v := range r.Agents {
			out.Agents[k] = v
		}
	}
	if r.Projects != nil {
		out.Projects = make(map[string]int, len(r.Projects))
		for k, v := range r.Projects {
			out.Projects[k] = v
		}
	}
	return out
}

// referenced reports whether an agent or project names profile.
func (r ProfileReferences) referenced(profile string) bool {
	return profile != "" && (r.Agents[profile] > 0 || r.Projects[profile] > 0)
}

// RetainedProfile describes one retained built-in profile.
type RetainedProfile struct {
	Name        string `json:"name"`
	Runtime     string `json:"runtime"`
	RuntimeType string `json:"runtimeType"`
	// Agents and Projects are the reference counts of the last successful
	// count; zero while Conservative.
	Agents   int `json:"agents"`
	Projects int `json:"projects"`
	// ActiveProfile is true when a settings load's active_profile named it
	// while it was retained.
	ActiveProfile bool `json:"activeProfile,omitempty"`
	// Conservative is true when the name was retained because references
	// could not be counted, and no successful count has seen it referenced
	// since.
	Conservative bool `json:"conservative,omitempty"`
}

// RetainedRuntime describes one retained built-in runtime.
type RetainedRuntime struct {
	Name     string   `json:"name"`
	Type     string   `json:"type"`
	NeededBy []string `json:"neededBy"`
}

// builtinCatalog is the built-in default profiles and runtimes, exactly as
// GetDefaultSettingsDataYAML returns them (with the OS adjustment of the
// local profile's runtime, and the Cloud Run sandbox variant).
type builtinCatalog struct {
	Runtimes map[string]V1RuntimeConfig `yaml:"runtimes"`
	Profiles map[string]V1ProfileConfig `yaml:"profiles"`
}

var (
	builtinCatalogOnce sync.Once
	builtinCatalogVal  *builtinCatalog
)

// defaultBuiltinCatalog parses the embedded defaults once per process. A
// parse failure yields an empty catalog: nothing is retained.
func defaultBuiltinCatalog() *builtinCatalog {
	builtinCatalogOnce.Do(func() {
		builtinCatalogVal = &builtinCatalog{}
		data, err := GetDefaultSettingsDataYAML()
		if err != nil {
			slog.Error("Built-in default settings could not be read; built-in profiles will not be retained", "error", err)
			return
		}
		if err := yamlv3.Unmarshal(data, builtinCatalogVal); err != nil {
			slog.Error("Built-in default settings could not be parsed; built-in profiles will not be retained", "error", err)
			builtinCatalogVal = &builtinCatalog{}
		}
	})
	return builtinCatalogVal
}

// runtimeTypeOf returns a runtime entry's effective type: its Type, else
// its map key (as ResolveRuntime does).
func runtimeTypeOf(key string, rt V1RuntimeConfig) string {
	if rt.Type != "" {
		return rt.Type
	}
	return key
}

// retainState is the overlay-level record of retained names. It only grows.
type retainState struct {
	mu sync.Mutex
	// profiles maps a retained profile name to whether it is still marked
	// conservative.
	profiles map[string]bool
	// runtimes maps a retained runtime name to the profiles that needed it.
	runtimes map[string]map[string]struct{}
	// activeProfiles records retained names some load's active_profile
	// named, and selfPerpetuating those that were the default with two or
	// more stored profiles (new agents without a profile save them).
	activeProfiles   map[string]struct{}
	selfPerpetuating map[string]struct{}
	// warnLogged holds the WARN keys already logged (bounded).
	warnLogged map[string]struct{}
}

// SetProfileReferences BOTH enables built-in retention on this overlay AND
// replaces its reference input (deep copy); retention cannot be disabled
// again. Until retention is enabled, Apply behaves exactly as before
// ptone/scion#5113 (no profile or runtime retention, no WARN). Call it only
// with references from CountProfileReferences whose ok is true. It ends
// conservative mode, and clears the conservative mark of every retained
// name refs references.
func (o *SettingsOverlay) SetProfileReferences(refs ProfileReferences) {
	if o == nil {
		return
	}
	o.mu.Lock()
	o.retain = true
	o.conservative = false
	o.refs = refs.clone()
	o.mu.Unlock()

	o.retained.mu.Lock()
	defer o.retained.mu.Unlock()
	for name, conservative := range o.retained.profiles {
		if conservative && refs.referenced(name) {
			o.retained.profiles[name] = false
		}
	}
}

// EnableConservativeRetention enables retention without references, for a
// hub whose reference count failed at start: every built-in profile is
// retained except those on a kubernetes-type built-in runtime (unless a
// known reference or the active_profile names them), until a
// SetProfileReferences call; retained names stay retained (they are sticky
// like any other). A no-op when retention is already enabled.
func (o *SettingsOverlay) EnableConservativeRetention() {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.retain {
		return
	}
	o.retain = true
	o.conservative = true
}

// AddProjectReference adds one project reference to profile at once, so a
// project newly pointed at a built-in (a project activeProfile save, or a
// project newly provided by the co-located broker) resolves it before the
// next reference count. Safe because retention only grows. A no-op when
// retention is not enabled or profile is empty.
func (o *SettingsOverlay) AddProjectReference(profile string) {
	o.addReference(profile, false)
}

// AddAgentReference adds one agent reference to profile at once, for an
// agent newly placed on the co-located broker with that saved profile (a
// reincarnate move). Same rules as AddProjectReference.
func (o *SettingsOverlay) AddAgentReference(profile string) {
	o.addReference(profile, true)
}

func (o *SettingsOverlay) addReference(profile string, agent bool) {
	if o == nil || profile == "" {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.retain {
		return
	}
	counts := &o.refs.Projects
	if agent {
		counts = &o.refs.Agents
	}
	if *counts == nil {
		*counts = make(map[string]int)
	}
	(*counts)[profile]++

	// A built-in named here becomes sticky at once, so a reference count
	// whose store read predates this call cannot drop it before the
	// caller's dispatch loads the settings. An added reference is a known
	// reference, so it also clears a conservative mark. A stored name is
	// not retained (the stored entry wins), so it is not recorded.
	if _, builtin := o.builtins().Profiles[profile]; !builtin {
		return
	}
	if _, stored := o.profiles[profile]; stored {
		return
	}
	o.retained.mu.Lock()
	defer o.retained.mu.Unlock()
	if o.retained.profiles == nil {
		o.retained.profiles = make(map[string]bool)
	}
	o.retained.profiles[profile] = false
}

// RetainedProfileNames returns the sorted built-in profiles the hub
// settings overlay retained in this settings load (ptone/scion#5113), nil
// when none. They resolve by name but are not stored profiles, so callers
// that advertise selectable profiles skip them.
func (vs *VersionedSettings) RetainedProfileNames() []string {
	if vs == nil || len(vs.retainedProfileNames) == 0 {
		return nil
	}
	return append([]string(nil), vs.retainedProfileNames...)
}

// RetainedRuntimeNames returns the sorted built-in runtimes the overlay
// retained in this settings load, nil when none.
func (vs *VersionedSettings) RetainedRuntimeNames() []string {
	if vs == nil || len(vs.retainedRuntimeNames) == 0 {
		return nil
	}
	return append([]string(nil), vs.retainedRuntimeNames...)
}

// ConservativeRetention reports whether retention is in conservative mode
// (enabled because references could not be counted, and no count has
// succeeded since).
func (o *SettingsOverlay) ConservativeRetention() bool {
	if o == nil {
		return false
	}
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.conservative
}

// RetentionEnabled reports whether built-in retention is enabled.
func (o *SettingsOverlay) RetentionEnabled() bool {
	if o == nil {
		return false
	}
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.retain
}

// builtins returns the catalog retention uses (a test override, else the
// embedded defaults).
func (o *SettingsOverlay) builtins() *builtinCatalog {
	if o.builtinsOverride != nil {
		return o.builtinsOverride
	}
	return defaultBuiltinCatalog()
}

// retainProfiles adds the retained built-in profiles to vs.Profiles, which
// holds the stored profiles. configured is the load's active_profile. It
// returns the sorted retained names (nil when none). Called with o.mu held
// for reading.
func (o *SettingsOverlay) retainProfiles(vs *VersionedSettings, configured string) []string {
	b := o.builtins()
	stored := len(o.profiles)

	o.retained.mu.Lock()
	defer o.retained.mu.Unlock()

	var names []string
	for _, name := range sortedNames(b.Profiles) {
		if _, ok := o.profiles[name]; ok {
			continue // a stored entry always wins
		}
		def := b.Profiles[name]
		normal := o.refs.referenced(name) || (name == configured && stored != 1)
		_, sticky := o.retained.profiles[name]
		conservative := o.conservative && !o.builtinRuntimeIsKubernetes(def.Runtime)
		if !normal && !sticky && !conservative {
			continue
		}
		vs.Profiles[name] = deepCloneJSON(def)
		names = append(names, name)
		if o.retained.profiles == nil {
			o.retained.profiles = make(map[string]bool)
		}
		if !sticky {
			o.retained.profiles[name] = !normal
		} else if normal && o.refs.referenced(name) {
			o.retained.profiles[name] = false
		}
		if name == configured {
			if o.retained.activeProfiles == nil {
				o.retained.activeProfiles = make(map[string]struct{})
			}
			o.retained.activeProfiles[name] = struct{}{}
			if stored >= 2 {
				if o.retained.selfPerpetuating == nil {
					o.retained.selfPerpetuating = make(map[string]struct{})
				}
				o.retained.selfPerpetuating[name] = struct{}{}
			}
		}
	}
	return names
}

// builtinRuntimeIsKubernetes reports whether the built-in runtime key names
// a kubernetes-type built-in runtime.
func (o *SettingsOverlay) builtinRuntimeIsKubernetes(key string) bool {
	rt, ok := o.builtins().Runtimes[key]
	return ok && runtimeTypeOf(key, rt) == "kubernetes"
}

// retainRuntimes adds, when stored runtimes replaced vs.Runtimes, the
// built-in runtime of every effective profile whose runtime key the stored
// runtimes lack. A kubernetes-type built-in runtime is retained only when a
// profile needing it is referenced (or it was retained before). It returns
// the sorted retained runtime names. Called with o.mu held for reading.
func (o *SettingsOverlay) retainRuntimes(vs *VersionedSettings) []string {
	if len(o.runtimes) == 0 {
		return nil
	}
	b := o.builtins()

	o.retained.mu.Lock()
	defer o.retained.mu.Unlock()

	retained := make(map[string]struct{})
	for _, name := range sortedNames(vs.Profiles) {
		key := vs.Profiles[name].Runtime
		if key == "" {
			continue
		}
		if _, ok := o.runtimes[key]; ok {
			continue // a stored runtime always wins
		}
		rt, ok := b.Runtimes[key]
		if !ok {
			continue // only built-in runtimes are retained
		}
		_, sticky := o.retained.runtimes[key]
		_, stickyProfile := o.retained.profiles[name]
		if runtimeTypeOf(key, rt) == "kubernetes" && !sticky && !stickyProfile {
			// Known references win even in conservative mode: refs then
			// hold only the references added since (AddProjectReference).
			// A sticky profile was referenced once (conservative mode
			// never makes a kubernetes-runtime profile sticky by itself).
			if !o.refs.referenced(name) && name != vs.ActiveProfile {
				continue
			}
		}
		if _, done := retained[key]; !done {
			vs.Runtimes[key] = deepCloneJSON(rt)
			retained[key] = struct{}{}
		}
		if o.retained.runtimes == nil {
			o.retained.runtimes = make(map[string]map[string]struct{})
		}
		if o.retained.runtimes[key] == nil {
			o.retained.runtimes[key] = make(map[string]struct{})
		}
		o.retained.runtimes[key][name] = struct{}{}
	}
	if len(retained) == 0 {
		return nil
	}
	return sortedNames(retained)
}

// RetainedState returns the overlay-level retained profiles and runtimes,
// the union over every settings load since the process started, sorted by
// name. Both are nil when nothing is retained.
func (o *SettingsOverlay) RetainedState() ([]RetainedProfile, []RetainedRuntime) {
	if o == nil {
		return nil, nil
	}
	o.mu.RLock()
	defer o.mu.RUnlock()
	b := o.builtins()

	o.retained.mu.Lock()
	defer o.retained.mu.Unlock()
	return o.retainedStateLocked(b)
}

// retainedStateLocked builds the retained state, leaving out names that
// are now stored (a stored entry wins, so it is not retained). Profiles are
// reported only while stored profiles replace the catalog, and runtimes only
// while stored runtimes do: without a stored set nothing is retained (the
// file and built-in catalog applies), even if a name is sticky. Called with
// o.mu held (for reading or writing) and o.retained.mu held.
func (o *SettingsOverlay) retainedStateLocked(b *builtinCatalog) ([]RetainedProfile, []RetainedRuntime) {
	refs := o.refs
	var profiles []RetainedProfile
	for _, name := range sortedNames(o.retained.profiles) {
		if _, stored := o.profiles[name]; stored || len(o.profiles) == 0 {
			continue
		}
		def := b.Profiles[name]
		_, active := o.retained.activeProfiles[name]
		p := RetainedProfile{
			Name:          name,
			Runtime:       def.Runtime,
			RuntimeType:   runtimeTypeOf(def.Runtime, b.Runtimes[def.Runtime]),
			Agents:        refs.Agents[name],
			Projects:      refs.Projects[name],
			ActiveProfile: active,
			Conservative:  o.retained.profiles[name],
		}
		profiles = append(profiles, p)
	}
	var runtimes []RetainedRuntime
	for _, name := range sortedNames(o.retained.runtimes) {
		if _, stored := o.runtimes[name]; stored || len(o.runtimes) == 0 {
			continue
		}
		runtimes = append(runtimes, RetainedRuntime{
			Name:     name,
			Type:     runtimeTypeOf(name, b.Runtimes[name]),
			NeededBy: sortedNames(o.retained.runtimes[name]),
		})
	}
	return profiles, runtimes
}

// logRetention logs the retention WARN once per distinct retained state:
// the retained profile names with their conservative marks, the retained
// runtime names and the self-perpetuating defaults. Counts are printed but
// are not part of the key, so reference changes do not log again. Called
// with o.mu held for reading.
func (o *SettingsOverlay) logRetention() {
	refs := o.refs
	b := o.builtins()

	o.retained.mu.Lock()
	defer o.retained.mu.Unlock()
	profiles, runtimes := o.retainedStateLocked(b)
	if len(profiles) == 0 && len(runtimes) == 0 {
		return
	}
	var selfPerpetuating []string
	for _, p := range profiles {
		if _, ok := o.retained.selfPerpetuating[p.Name]; ok {
			selfPerpetuating = append(selfPerpetuating, p.Name)
		}
	}
	var key strings.Builder
	for _, p := range profiles {
		fmt.Fprintf(&key, "p:%s:%t\x00", p.Name, p.Conservative)
	}
	for _, r := range runtimes {
		fmt.Fprintf(&key, "r:%s\x00", r.Name)
	}
	for _, name := range selfPerpetuating {
		fmt.Fprintf(&key, "s:%s\x00", name)
	}
	k := key.String()
	if _, ok := o.retained.warnLogged[k]; ok {
		return
	}
	if o.retained.warnLogged == nil || len(o.retained.warnLogged) >= maxFallbackLogKeys {
		o.retained.warnLogged = make(map[string]struct{})
	}
	o.retained.warnLogged[k] = struct{}{}

	attrs := []any{}
	conservative := false
	for _, p := range profiles {
		conservative = conservative || p.Conservative
		desc := fmt.Sprintf("agents=%d projects=%d active_profile=%t runtime=%s runtime_type=%s",
			p.Agents, p.Projects, p.ActiveProfile, p.Runtime, p.RuntimeType)
		if p.Conservative {
			desc = fmt.Sprintf("conservative=true (references unknown) active_profile=%t runtime=%s runtime_type=%s",
				p.ActiveProfile, p.Runtime, p.RuntimeType)
		}
		attrs = append(attrs, slog.String("profile."+p.Name, desc))
	}
	for _, r := range runtimes {
		attrs = append(attrs, slog.String("runtime."+r.Name,
			fmt.Sprintf("type=%s needed_by=[%s]", r.Type, strings.Join(r.NeededBy, " "))))
	}
	if n := refs.Agents[""]; n > 0 {
		attrs = append(attrs, slog.Int("agents_without_profile", n))
	}
	if refs.AgentInfoMissing > 0 {
		attrs = append(attrs, slog.Int("agent_info_missing", refs.AgentInfoMissing))
	}
	for _, name := range selfPerpetuating {
		attrs = append(attrs, slog.String("default."+name,
			fmt.Sprintf("new agents without a profile save %q and keep it in use; set active_profile to a stored profile, or store %q", name, name)))
	}
	attrs = append(attrs, slog.String("remediation",
		"store the profile or runtime in the hub's settings to make it explicit (store the runtime too if this host's local runtime is not the type shown); "+
			"or move its agents and projects to a stored profile and set active_profile to a stored profile to retire it; retired built-ins drop at the next restart"))
	msg := "Retaining built-in default profiles or runtimes that the hub's stored settings do not include but are still used"
	if conservative {
		msg = "References could not be counted; retaining every built-in default profile except those on the unscoped kubernetes runtime until they can be"
	}
	slog.Warn(msg, attrs...)
}

// sortedNames returns the sorted keys of m.
func sortedNames[V any](m map[string]V) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
