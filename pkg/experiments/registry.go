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

// Package experiments declares the hub-wide experiments (feature flags)
// registry. It has no dependency on pkg/hub, so a future broker or CLI
// design can import it directly.
package experiments

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"sync"
	"time"
)

// Layer identifies which part of the system an experiment gates.
type Layer string

const (
	// LayerWeb gates web UI code via isFeatureEnabled().
	LayerWeb Layer = "web"
	// LayerServer gates hub server code via Server.experimentEnabled().
	LayerServer Layer = "server"
)

// Stage is a cosmetic lifecycle badge shown in the Experiments tab. It has
// no behavioural effect.
type Stage string

const (
	StageAlpha Stage = "alpha"
	StageBeta  Stage = "beta"
)

// validStages is the exhaustive set of Stage values NewRegistry accepts.
var validStages = map[Stage]bool{StageAlpha: true, StageBeta: true}

// NamePattern is the required shape of an experiment name, e.g.
// "web.terminal_workspace" or "hub.foo.bar". pkg/config/opsettings uses the
// same pattern in its JSON schema for the "experiments" section, and pkg/hub
// uses ValidName to apply the identical rule to admin PUT payloads.
const NamePattern = `^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`

var namePatternRe = regexp.MustCompile(NamePattern)

// ValidName reports whether name matches NamePattern.
func ValidName(name string) bool { return namePatternRe.MatchString(name) }

// reviewByLayout is the required YYYY-MM-DD shape of Experiment.ReviewBy.
const reviewByLayout = "2006-01-02"

// Experiment describes one hub-wide feature flag.
type Experiment struct {
	// Name is the stable identifier, e.g. "web.terminal_workspace". It must
	// match NamePattern. "web." is the convention for UI-only gates, "hub."
	// for anything that gates server behaviour (Layers is the source of
	// truth; the prefix is only a readability aid).
	Name string
	// Title is a short UI label, e.g. "Persistent terminal workspace".
	Title string
	// Description is one or two sentences shown in the Experiments tab.
	Description string
	// Default is the value used when no admin override exists.
	Default bool
	// Layers lists which parts of the system this experiment gates.
	Layers []Layer
	// Stage is a cosmetic lifecycle badge; not behavioural.
	Stage Stage
	// Issue is the tracking issue, e.g. "ptone/scion#1662".
	Issue string
	// Owner is the team or handle responsible for graduating or removing it.
	Owner string
	// ReviewBy is a YYYY-MM-DD date; after it passes the tab shows "review
	// overdue".
	ReviewBy string
}

// HasLayer reports whether the experiment gates the given layer.
func (e Experiment) HasLayer(l Layer) bool {
	for _, layer := range e.Layers {
		if layer == l {
			return true
		}
	}
	return false
}

// ReviewOverdue reports whether now is strictly after the ReviewBy day: the
// tab shows "review overdue" starting the day after ReviewBy, not during
// ReviewBy itself. ReviewBy's layout is validated by NewRegistry; an
// unparsable value (only reachable by bypassing that validation) is treated
// as not overdue rather than panicking.
//
// The day boundary is UTC midnight regardless of now's Location: time.Parse
// yields ReviewBy at 00:00 UTC and the comparison is between instants, so
// the result depends only on the instant and hub replicas with different
// TZ settings agree.
func (e Experiment) ReviewOverdue(now time.Time) bool {
	t, err := time.Parse(reviewByLayout, e.ReviewBy)
	if err != nil {
		return false
	}
	return !now.Before(t.AddDate(0, 0, 1))
}

// compiled is the production experiment list. It is reachable only through
// Default(); there is no package-level Lookup/All, so hub code cannot bypass
// the Registry instance it was given (ptone/scion#2217).
var compiled = []Experiment{
	{
		Name:        "web.terminal_workspace",
		Title:       "Persistent terminal workspace",
		Description: "Opens agent terminals in the persistent /terminals workspace instead of the legacy single-terminal page.",
		Default:     true,
		Layers:      []Layer{LayerWeb},
		Stage:       StageBeta,
		Issue:       "ptone/scion#1662",
		Owner:       "web",
		ReviewBy:    "2026-12-31",
	},
	{
		Name:        "web.gcs_links",
		Title:       "gs:// link previews",
		Description: "Linkifies a gs://bucket/object URI an agent posts in chat and lets the viewer fetch and preview that object through the hub. Gates both the linkifier (LayerWeb) and the GET /api/v1/gcs/object endpoint itself (LayerServer): the endpoint also requires a configured GCP token generator, so this experiment alone does not make links fetchable on a hub without one.",
		Default:     false,
		Layers:      []Layer{LayerWeb, LayerServer},
		Stage:       StageAlpha,
		Issue:       "ptone/scion#2545",
		Owner:       "native-chat",
		ReviewBy:    "2026-12-30",
	},
	{
		Name:        "hub.conduit",
		Title:       "Conduit connection layer",
		Description: "Enables the hub surfaces of Conduit, the unified agent/broker connection layer: Ed25519 stream grants, the GET /api/v1/conduit/grant-keys endpoint, the agent conduit session endpoint GET /api/v1/conduit and the in-process relay (read at startup; turning it on or off for the relay needs a restart). No existing connection path changes.",
		Default:     false,
		Layers:      []Layer{LayerServer},
		Stage:       StageAlpha,
		Issue:       "ptone/scion#2774",
		Owner:       "conduit",
		ReviewBy:    "2027-03-31",
	},
}

// compiledRetired lists names that PUT rejects true/false for, ignores and
// prunes if still stored, and that must never be reused.
var compiledRetired = []string{
	"web.access_boundaries_read",
	"web.access_boundaries_authoring",
}

// Registry is an immutable, validated set of experiments. It is the only way
// to read them: there are no package-level Lookup/All functions, so hub code
// cannot bypass the instance it was given, and tests using t.Parallel()
// cannot affect each other through a shared global.
type Registry struct {
	byName  map[string]Experiment
	ordered []Experiment
	retired map[string]bool
}

// NewRegistry validates active and retired together and returns an error on
// the first invariant violation:
//   - names are unique and match NamePattern
//   - no active name appears in the retired list
//   - every entry has a non-empty Title, Description, Issue and Owner, at
//     least one Layer, a ReviewBy matching YYYY-MM-DD, and a recognized Stage
//
// The returned Registry owns a private copy of each entry's Layers slice, so
// mutating a slice the caller passed in afterward cannot affect the Registry.
func NewRegistry(active []Experiment, retired []string) (*Registry, error) {
	retiredSet := make(map[string]bool, len(retired))
	for _, name := range retired {
		retiredSet[name] = true
	}

	byName := make(map[string]Experiment, len(active))
	ordered := make([]Experiment, 0, len(active))
	for _, e := range active {
		if !ValidName(e.Name) {
			return nil, fmt.Errorf("experiments: invalid name %q: must match %s", e.Name, NamePattern)
		}
		if _, dup := byName[e.Name]; dup {
			return nil, fmt.Errorf("experiments: duplicate name %q", e.Name)
		}
		if retiredSet[e.Name] {
			return nil, fmt.Errorf("experiments: %q is both active and retired", e.Name)
		}
		if e.Title == "" || e.Description == "" || e.Issue == "" || e.Owner == "" || e.ReviewBy == "" {
			return nil, fmt.Errorf("experiments: %q is missing a required field (title, description, issue, owner, review_by)", e.Name)
		}
		if len(e.Layers) == 0 {
			return nil, fmt.Errorf("experiments: %q has no layers", e.Name)
		}
		if _, err := time.Parse(reviewByLayout, e.ReviewBy); err != nil {
			return nil, fmt.Errorf("experiments: %q has an invalid review_by %q: must match YYYY-MM-DD", e.Name, e.ReviewBy)
		}
		if !validStages[e.Stage] {
			return nil, fmt.Errorf("experiments: %q has an invalid stage %q", e.Name, e.Stage)
		}
		e.Layers = slices.Clone(e.Layers)
		byName[e.Name] = e
		ordered = append(ordered, e)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Name < ordered[j].Name })

	return &Registry{byName: byName, ordered: ordered, retired: retiredSet}, nil
}

// All returns every active experiment in stable order (by Name). The
// returned slice, and each entry's Layers slice, are copies: mutating either
// does not affect the Registry or any other caller's result.
func (r *Registry) All() []Experiment {
	out := slices.Clone(r.ordered)
	for i := range out {
		out[i].Layers = slices.Clone(out[i].Layers)
	}
	return out
}

// Lookup returns the named active experiment, or false if it is unknown or
// retired. The returned Experiment's Layers slice is a copy; mutating it
// does not affect the Registry or any other caller's result.
func (r *Registry) Lookup(name string) (Experiment, bool) {
	e, ok := r.byName[name]
	if ok {
		e.Layers = slices.Clone(e.Layers)
	}
	return e, ok
}

// IsRetired reports whether name was retired and must never resolve again.
func (r *Registry) IsRetired(name string) bool { return r.retired[name] }

// defaultRegistry builds the production registry exactly once.
var defaultRegistry = sync.OnceValue(func() *Registry {
	r, err := NewRegistry(compiled, compiledRetired)
	if err != nil {
		// The invariant tests run this exact construction and fail first,
		// so an invalid compiled list never ships.
		panic(fmt.Sprintf("experiments: invalid compiled registry: %v", err))
	}
	return r
})

// Default returns the compiled production registry, built once. It panics if
// the compiled list is invalid.
func Default() *Registry { return defaultRegistry() }
