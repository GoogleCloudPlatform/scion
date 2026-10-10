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

package harness

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// LegacyBuiltinProvisionerType is the provisioner.type of harness-configs
// written before container-script provisioning. Its compiled-in Go
// implementation has been removed, so such a harness-config cannot
// provision a harness (see harnesses/README.md).
const LegacyBuiltinProvisionerType = "builtin"

// ErrUnusableProvisioner is the sentinel every UnusableProvisionerError
// wraps, so callers can classify the failure with errors.Is.
var ErrUnusableProvisioner = errors.New("harness-config provisioner is not usable")

// HarnessConfigScope says where an unusable harness-config lives, which
// decides how it is repaired.
type HarnessConfigScope string

const (
	// HarnessConfigScopeGlobal is <global dir>/harness-configs/<name>, the
	// only location `scion harness-config upgrade` operates on.
	HarnessConfigScopeGlobal HarnessConfigScope = "global"
	// HarnessConfigScopeProject is a project's harness-configs/<name>.
	HarnessConfigScopeProject HarnessConfigScope = "project"
	// HarnessConfigScopeTemplate is harness-configs/<name> inside one of
	// the agent's templates.
	HarnessConfigScopeTemplate HarnessConfigScope = "template"
	// HarnessConfigScopeHub is the hub's harness-config record, hydrated
	// for the dispatch.
	HarnessConfigScopeHub HarnessConfigScope = "hub"
)

// UnusableProvisionerError reports a harness-config whose provisioner block
// cannot provision the harness: provisioner.type "builtin", or a provisioner
// without a command. An agent launched from it would start without the
// provisioner's output (auth, harness-native config), so launch fails before
// any container is created (ptone/scion#611).
type UnusableProvisionerError struct {
	Name        string             // harness-config name
	HarnessType string             // harness type (config.yaml `harness:`)
	Path        string             // harness-config directory, if any
	Scope       HarnessConfigScope // where the harness-config lives
	Type        string             // provisioner.type as written
	Reason      string             // what is wrong with the provisioner
	// Bundled reports whether HarnessType has a bundled harness-config
	// (harnesses/<type>), which `upgrade` and a reinstall draw from.
	Bundled bool
}

// Error is the full message, including the harness-config directory. It is
// for logs and the local CLI; use PublicMessage in responses sent off the
// machine.
func (e *UnusableProvisionerError) Error() string {
	where := ""
	if e.Path != "" {
		where = fmt.Sprintf(" (%s)", e.Path)
	}
	return fmt.Sprintf("harness-config %q%s cannot be used: %s. To fix it, %s", e.Name, where, e.Reason, e.fix(true))
}

// PublicMessage is Error without filesystem paths, for the broker's answer
// to the hub. For a copy on the broker's own filesystem (global, project or
// template scope) it says so, since repairing a copy on the caller's
// workstation would not fix it.
func (e *UnusableProvisionerError) PublicMessage() string {
	label := ""
	switch e.Scope {
	case "":
	case HarnessConfigScopeHub:
		label = " (hub)"
	default:
		label = fmt.Sprintf(" (the broker's %s copy)", e.Scope)
	}
	msg := fmt.Sprintf("harness-config %q%s cannot be used: %s. ", e.Name, label, e.Reason)
	if e.Scope == HarnessConfigScopeHub || e.Scope == "" {
		return msg + "To fix it, " + e.fix(false)
	}
	return msg + "Repair it on the broker host: " + e.fix(false) +
		fmt.Sprintf(" Alternatively, upload a working copy to the hub with `scion harness-config sync %s`; the hub then sends its copy with each dispatch.", e.Name)
}

// Fix returns the action that repairs the harness-config, with paths.
func (e *UnusableProvisionerError) Fix() string { return e.fix(true) }

func (e *UnusableProvisionerError) Unwrap() error { return ErrUnusableProvisioner }

const provisionerEditHint = "set `provisioner.type: container-script` with a non-empty `provisioner.command`, or remove the `provisioner` block"

func (e *UnusableProvisionerError) fix(withPaths bool) string {
	configFile := "its config.yaml"
	if withPaths && e.Path != "" {
		configFile = filepath.Join(e.Path, "config.yaml")
	}
	edit := fmt.Sprintf("edit %s: %s", configFile, provisionerEditHint)

	switch e.Scope {
	case HarnessConfigScopeGlobal:
		if e.Bundled {
			return fmt.Sprintf("run `scion harness-config upgrade %s --activate-script`, or reinstall the bundled %s harness-config over it with `scion harness-config install --force --global --name %s <scion-checkout>/harnesses/%s`.", e.Name, e.HarnessType, e.Name, e.HarnessType)
		}
		return fmt.Sprintf("%s (no bundled harness-config exists for harness type %q, so `scion harness-config upgrade` cannot repair it).", edit, e.HarnessType)
	case HarnessConfigScopeProject:
		// `upgrade` operates only on the global directory, and `install`
		// without --global targets the hub project scope in Hub mode, so
		// editing the file is the one repair that always applies here.
		return edit + "."
	case HarnessConfigScopeTemplate:
		file := fmt.Sprintf("harness-configs/%s/config.yaml in the agent's template", e.Name)
		if withPaths && e.Path != "" {
			file = filepath.Join(e.Path, "config.yaml") + " (bundled in the agent's template)"
		}
		return fmt.Sprintf("edit %s: %s. For a template from the hub, repair the template itself and upload it to the hub again.", file, provisionerEditHint)
	case HarnessConfigScopeHub:
		return fmt.Sprintf("pull it (`scion harness-config pull %s`), %s, and upload it to the scope it came from with `scion harness-config sync %s` (add `--global` for a global record).", e.Name, provisionerEditHint, e.Name)
	default:
		return edit + "."
	}
}

// CheckProvisionerUsable returns an *UnusableProvisionerError when entry's
// provisioner block cannot provision the harness, nil otherwise (including
// when there is no provisioner block).
func CheckProvisionerUsable(name string, hcDir *config.HarnessConfigDir, entry config.HarnessConfigEntry) *UnusableProvisionerError {
	prov := entry.Provisioner
	if prov == nil {
		return nil
	}
	var reason string
	switch {
	case prov.Type == LegacyBuiltinProvisionerType:
		reason = `provisioner.type "builtin" is no longer supported (the built-in provisioner was removed)`
	case len(prov.Command) == 0:
		reason = "provisioner.command is empty, so the container-script provisioner has nothing to run"
	default:
		return nil
	}
	harnessType := entry.Harness
	if harnessType == "" {
		harnessType = name // as EffectiveConfig defaults it
	}
	e := &UnusableProvisionerError{
		Name:        name,
		HarnessType: harnessType,
		Type:        prov.Type,
		Reason:      reason,
		Bundled:     hasBundledHarnessConfig(harnessType),
	}
	if hcDir != nil {
		e.Path = hcDir.Path
		e.Scope = harnessConfigScope(name, hcDir)
	}
	return e
}

// hasBundledHarnessConfig reports whether harnesses/<harnessType> exists in
// the embedded bundle set.
func hasBundledHarnessConfig(harnessType string) bool {
	if harnessType == "" {
		return false
	}
	_, err := fs.Stat(HarnessesFS(), harnessType+"/config.yaml")
	return err == nil
}

// harnessConfigScope maps a resolved directory to its scope. A broker-local
// directory is global when it is <global dir>/harness-configs/<name>,
// project-level otherwise.
func harnessConfigScope(name string, hcDir *config.HarnessConfigDir) HarnessConfigScope {
	switch hcDir.Source {
	case config.HarnessConfigSourceHubHydrated:
		return HarnessConfigScopeHub
	case config.HarnessConfigSourceTemplateBundled:
		return HarnessConfigScopeTemplate
	}
	if globalDir, err := config.GetGlobalDir(); err == nil && hcDir.Path != "" {
		if samePath(hcDir.Path, filepath.Join(globalDir, "harness-configs", name)) {
			return HarnessConfigScopeGlobal
		}
	}
	return HarnessConfigScopeProject
}

func samePath(a, b string) bool {
	if ea, err := filepath.EvalSymlinks(a); err == nil {
		a = ea
	}
	if eb, err := filepath.EvalSymlinks(b); err == nil {
		b = eb
	}
	return filepath.Clean(a) == filepath.Clean(b)
}
