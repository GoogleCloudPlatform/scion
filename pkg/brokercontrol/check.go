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

package brokercontrol

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/util/pathcanon"
)

// ErrControlPathMount is wrapped by every refusal from this package.
var ErrControlPathMount = errors.New("mount source reaches Runtime Broker control files")

// ErrorCode is the stable code for a refused mount, for API error bodies and
// agent status reasons.
const ErrorCode = "control_path_mount"

// Rule names why a source was refused. It is logged for the operator only.
type Rule string

const (
	RuleShape      Rule = "not an absolute path"
	RuleUnresolved Rule = "could not be resolved"
	RuleEqual      Rule = "equals a control path"
	RuleAncestor   Rule = "contains a control path"
	RuleInside     Rule = "is inside a control directory"
	RuleIdentity   Rule = "is the same file as a control path or one of its parents"
	RuleFloor      Rule = "contains the global directory"
	RuleNotAllowed Rule = "is inside the global directory but not on the allow list"
)

// RefusalError is a refused mount source. Error() is safe to return to the
// requester: it names the source as given and the class of what it reaches,
// never a resolved host path or a cause from resolution. Detail() adds those
// for the broker log.
type RefusalError struct {
	Source   string
	Class    Class
	Rule     Rule
	Resolved string
	Entry    string
	Cause    error
}

func (e *RefusalError) Error() string {
	if e.Rule == RuleShape || e.Rule == RuleUnresolved {
		return fmt.Sprintf("mount source %q is refused: it could not be verified to stay clear of Runtime Broker control files on this host; use an existing absolute path with no dangling links", e.Source)
	}
	return fmt.Sprintf("mount source %q is refused: it equals or contains Runtime Broker %s on this host; mount a directory that does not contain the Scion global directory (~/.scion) or its control files", e.Source, e.Class)
}

// Unwrap lets errors.Is match ErrControlPathMount.
func (e *RefusalError) Unwrap() error { return ErrControlPathMount }

// Detail is the operator-facing description, with resolved paths.
func (e *RefusalError) Detail() string {
	s := fmt.Sprintf("mount source %q (resolved %q) %s", e.Source, e.Resolved, e.Rule)
	if e.Entry != "" {
		s += fmt.Sprintf(" %q (%s)", e.Entry, e.Class)
	}
	if e.Cause != nil {
		s += ": " + e.Cause.Error()
	}
	return s
}

// AllowFunc decides whether resolved, which is exactly (case-preserving)
// globalDir or below it, may be mounted. Both are resolved, clean and
// absolute.
type AllowFunc func(resolved, globalDir string) bool

// CheckNamed refuses src when it reaches a named or registered control path,
// or equals or contains the global directory. A source inside the global
// directory is not judged here; callers that already apply their own allow
// list for the global directory (the workspace and agent-home guards) use
// this. It returns the resolved source, which the caller must use in place
// of src.
func CheckNamed(src string) (string, error) {
	return check(src, nil)
}

// Check is CheckNamed plus the global-directory allow list: a source inside
// the global directory must satisfy allow. A nil allow refuses every such
// source. It returns the resolved source, which the caller must use in
// place of src.
func Check(src string, allow AllowFunc) (string, error) {
	if allow == nil {
		allow = func(string, string) bool { return false }
	}
	return check(src, allow)
}

func check(src string, allow AllowFunc) (string, error) {
	if src == "" || !filepath.IsAbs(src) {
		return "", &RefusalError{Source: src, Rule: RuleShape}
	}
	r, err := resolvePhysical(src)
	if err != nil {
		return "", &RefusalError{Source: src, Rule: RuleUnresolved, Cause: err}
	}
	set, err := Resolve()
	if err != nil {
		return "", &RefusalError{Source: src, Resolved: r, Rule: RuleUnresolved, Cause: err}
	}
	if rerr := set.check(src, r); rerr != nil {
		return "", rerr
	}
	g := set.GlobalDir
	refuse := func(rule Rule) error {
		return &RefusalError{Source: src, Resolved: r, Rule: rule, Entry: g, Class: ClassGlobalDir}
	}
	if pathEqual(r, g) || strictlyUnder(g, r) || identityAtOrAbove(r, g) {
		return "", refuse(RuleFloor)
	}
	if allow == nil {
		return r, nil
	}
	if exactlyAtOrUnder(r, g) {
		if !allow(r, g) {
			return "", refuse(RuleNotAllowed)
		}
		return r, nil
	}
	// Inside the global directory by another spelling only (case variant or
	// another mount of it): component names cannot be trusted for the allow
	// list, so refuse.
	if strictlyUnder(r, g) || identityAtOrAbove(g, r) {
		return "", refuse(RuleNotAllowed)
	}
	return r, nil
}

// check applies the named and registered entries to the resolved source r.
func (s *Set) check(src, r string) error {
	for _, e := range s.Entries {
		var rule Rule
		switch {
		case pathEqual(r, e.Path):
			rule = RuleEqual
		case strictlyUnder(e.Path, r):
			rule = RuleAncestor
		case e.Kind == Dir && strictlyUnder(r, e.Path):
			rule = RuleInside
		case identityAtOrAbove(r, e.Path):
			rule = RuleIdentity
		case e.Kind == Dir && identityAtOrAbove(e.Path, r):
			rule = RuleIdentity
		default:
			continue
		}
		return &RefusalError{Source: src, Resolved: r, Rule: rule, Entry: e.Path, Class: e.Class}
	}
	return nil
}

// VolumeAllowed is the allow list for configured volumes and shared dirs
// inside the global directory. Every subtree it admits holds agent or user
// data only, never a control file:
//
//	workspace[/...]                                     the global project's workspace
//	projects/<slug>/workspace[/...]                     a Hub-managed project's workspace
//	projects/<slug>/worktrees/<name>[/...]              a per-agent worktree
//	projects/<slug>/.scion/agents/<id>/workspace[/...]  one agent's workspace
//	project-configs/<dir>/.scion/agents/<id>/workspace[/...]
//	project-configs/<dir>/shared-dirs/<name>[/...]      one shared dir
//
// A bare project directory is deliberately not admitted: its .scion holds the
// project settings and every agent's home.
//
// resolved and globalDir must already be physically resolved (as Check
// passes them): the agent-data shape is matched on resolved's spelling.
// Containment in globalDir is decided by pathcanon.Within (per component,
// with every alias form resolved; case-insensitive on Windows). Any error,
// or a path outside globalDir, is not allowed.
func VolumeAllowed(resolved, globalDir string) bool {
	in, err := pathcanon.Within(globalDir, resolved)
	if err != nil || !in {
		return false
	}
	rel, err := filepath.Rel(globalDir, resolved) //winsec:not-containment containment is decided by pathcanon.Within above; Rel only splits the components
	if err != nil || rel == "." {
		return false
	}
	parts := strings.Split(rel, sep)
	at := func(i int) string {
		if i < len(parts) {
			return parts[i]
		}
		return ""
	}
	agentWorkspace := func(i int) bool {
		return at(i) == ".scion" && at(i+1) == "agents" && at(i+2) != "" && at(i+3) == "workspace"
	}
	switch parts[0] {
	case "workspace":
		return true
	case config.ProjectsDir:
		if at(1) == "" {
			return false
		}
		switch {
		case at(2) == "workspace":
			return true
		case at(2) == "worktrees" && at(3) != "":
			return true
		default:
			return agentWorkspace(2)
		}
	case config.ProjectConfigsDir:
		if at(1) == "" {
			return false
		}
		if at(2) == config.SharedDirsSubdir && at(3) != "" {
			return true
		}
		return agentWorkspace(2)
	}
	return false
}
