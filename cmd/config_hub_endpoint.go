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

package cmd

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// hub.endpoint in the config commands. The hub list (context.yaml) is the
// CLI's only hub configuration, so these commands are a front end to it and
// never read or write hub.endpoint in a settings file:
//
//   - config set --global hub.endpoint URL adds the hub to the hub list
//     (or reuses the entry with that endpoint) and makes it the current
//     hub; in a project, config set hub.endpoint URL links the project
//     directory to that hub (its link entry);
//   - config get and config list report the hub this invocation uses
//     (resolveCLIHub) and where it comes from;
//   - config unset --global hub.endpoint clears the current hub (the entry
//     stays) and removes a legacy hub.endpoint from the global settings file:
//     the CLI never reads it, but the server and the broker still fall back
//     to it (deprecated); in a project it removes the project's link entry.
//
// Stored logins are keyed by endpoint and are never changed here.

// configHubEndpointKey is the config key handled by the hub list.
const configHubEndpointKey = config.HubEndpointConfigKey

// errHubSettingUnchanged stops the hub list update of config unset when
// there is nothing to remove, so the file is not rewritten.
var errHubSettingUnchanged = errors.New("nothing to remove")

// effectiveHubSetting returns hub.endpoint as config get and config list
// report it: the hub this invocation uses (resolveCLIHub) with its source,
// or no value when no hub is selected.
func effectiveHubSetting() config.SettingValue {
	h := resolveCLIHub()
	v := config.SettingValue{Key: configHubEndpointKey}
	if h.Endpoint == "" {
		return v
	}
	v.Value = config.RedactURL(h.Endpoint)
	v.Source = effectiveHubSource(h)
	return v
}

// effectiveHubSource is the source config get and config list show for the
// hub h: "flag --hub", "env <VARIABLE>", "link hub '<name>'" or
// "current hub '<name>'", with the entry name the resolver recorded
// (hubEntryName). The hub list is not read again, so the name is the one of
// the context the resolver used.
func effectiveHubSource(h cliHub) string {
	switch h.Source {
	case useSourceFlag:
		if hubFlagName != "" {
			return fmt.Sprintf("flag --hub '%s'", hubFlagName)
		}
		return "flag --hub"
	case useSourceEnv:
		return "env " + h.Label
	}
	kind := "current hub"
	if h.Source == useSourceDirectory {
		kind = "link hub"
	}
	if hubEntryName != "" {
		return fmt.Sprintf("%s '%s'", kind, hubEntryName)
	}
	return kind
}

// configGetValue returns the value and source config get prints for key:
// the effective hub for hub.endpoint, else the setting's value from the
// settings layers.
func configGetValue(key string) (config.SettingValue, error) {
	if key == configHubEndpointKey {
		return effectiveHubSetting(), nil
	}
	return config.ResolveSettingValue(configReadProjectDir(), key)
}

// replaceHubSetting returns values with the hub.endpoint entry replaced by
// the effective hub (effectiveHubSetting).
func replaceHubSetting(values []config.SettingValue) []config.SettingValue {
	for i := range values {
		if values[i].Key == configHubEndpointKey {
			values[i] = effectiveHubSetting()
		}
	}
	return values
}

// warnUnreachableHub checks the health of the hub at endpoint as 'hub add'
// does and prints its warning when the hub does not respond. It returns
// whether the hub responded.
func warnUnreachableHub(stderr io.Writer, endpoint string) bool {
	// The hub the user named: transport auth from the environment
	// applies, as for 'hub add'.
	if err := hubResponds(endpoint, false); err != nil {
		printUnreachableHub(stderr, endpoint, err)
		return false
	}
	return true
}

// configSetHubEndpoint is config set hub.endpoint URL. global selects the
// hub as the current hub; otherwise the project at projectDir is linked to
// it.
func configSetHubEndpoint(out, stderr io.Writer, url string, global bool, projectDir string) error {
	if err := checkHubRegistryAvailable(); err != nil {
		return err
	}
	var name, endpoint string
	var created bool
	err := updateHubRegistry(func(c *config.CLIContext) error {
		var err error
		if global {
			name, created, err = c.SelectHubURL(url, useHubBaseName(url))
		} else {
			name, created, err = c.LinkPathToHubURL(url, useHubBaseName(url),
				config.ProjectLinkID(projectDir), config.LinkPathForProject(projectDir), time.Now())
		}
		if err != nil {
			return err
		}
		endpoint = c.Hubs[name].Endpoint
		return nil
	})
	if err != nil {
		if errors.Is(err, config.ErrLinkNoProjectID) {
			return errors.New("cannot link this project to a hub: it has no project ID; run 'scion hub link' instead")
		}
		return err
	}

	reachable := warnUnreachableHub(stderr, endpoint)
	now, overridden := hubSettingAfterSet(global, name, endpoint)
	if overridden {
		if v, env := strings.CutPrefix(now.Source, "env "); env {
			_, _ = fmt.Fprintf(stderr, "Warning: %s is still set by %s\n", configHubEndpointKey, v)
		} else {
			_, _ = fmt.Fprintf(stderr, "Note: here %s still comes from %s: %s\n", configHubEndpointKey, now.Source, now.Value)
		}
	}
	scope := "global"
	msg := fmt.Sprintf("Hub '%s' (%s) is now the current hub.", name, config.RedactURL(endpoint))
	if !global {
		scope = "local"
		msg = fmt.Sprintf("This project is now linked to hub '%s' (%s).", name, config.RedactURL(endpoint))
	}
	if isJSONOutput() {
		return outputJSON(ActionResult{
			Status:  "success",
			Command: "config set",
			Message: msg,
			Details: map[string]interface{}{
				"key":       configHubEndpointKey,
				"hub":       name,
				"endpoint":  config.RedactURL(endpoint),
				"scope":     scope,
				"created":   created,
				"reachable": reachable,
				"value":     now.Value,
				"source":    now.Source,
			},
		})
	}
	_, _ = fmt.Fprintln(out, msg)
	return nil
}

// describeConfigHub is a hub for config set/unset messages: "'name' (url)",
// or "'name'" when the hub has no entry any more.
func describeConfigHub(name, endpoint string) string {
	if endpoint == "" {
		return fmt.Sprintf("'%s'", name)
	}
	return fmt.Sprintf("'%s' (%s)", name, config.RedactURL(endpoint))
}

// removeLegacyGlobalHubSetting removes hub.endpoint from the global settings
// file when it is there. The CLI never reads it, but the server and the
// broker still fall back to it (deprecated), so a value left behind would
// keep a server pointed at a hub the user just cleared. It reports what it
// did on stderr and returns whether the key was removed.
func removeLegacyGlobalHubSetting(stderr io.Writer) bool {
	res, err := config.UnsetSetting("", configHubEndpointKey, true)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "Warning: could not remove the legacy %s from the global settings: %v\n", configHubEndpointKey, err)
		return false
	}
	for _, w := range res.MigrationWarnings {
		_, _ = fmt.Fprintf(stderr, "  Migration warning: %s\n", w)
	}
	if res.Removed {
		_, _ = fmt.Fprintf(stderr, "Removed the legacy %s from %s (the server and the Runtime Broker no longer fall back to it).\n", configHubEndpointKey, res.Path)
	}
	// The global file alone, without the environment: a variable that sets
	// the hub is reported separately.
	if file := config.GlobalSettingsFileWithHubEndpoint(); file != "" {
		_, _ = fmt.Fprintf(stderr, "Warning: %s is still set in %s in a form config unset does not edit; remove it by hand (the server and the Runtime Broker still fall back to it).\n", configHubEndpointKey, file)
	}
	return res.Removed
}

// configUnsetHubEndpoint is config unset hub.endpoint. global clears the
// current hub (its entry stays); otherwise the link entry of the project at
// projectDir is removed.
func configUnsetHubEndpoint(out, stderr io.Writer, global bool, projectDir string) error {
	if err := checkHubRegistryAvailable(); err != nil {
		return err
	}
	var name, endpoint string
	err := updateHubRegistry(func(c *config.CLIContext) error {
		if global {
			name = c.ClearCurrentHub()
			if name == "" {
				return errHubSettingUnchanged
			}
			if h := c.Hubs[name]; h != nil {
				endpoint = h.Endpoint
			}
			return nil
		}
		localPath := config.LinkPathForProject(projectDir)
		for _, p := range c.Projects {
			if p.Source == config.KnownProjectSourceLink && p.LocalPath == localPath {
				name = p.Hub
				if h := c.Hubs[p.Hub]; h != nil {
					endpoint = h.Endpoint
				}
				break
			}
		}
		if !c.UnlinkPath(localPath, time.Now()) {
			return errHubSettingUnchanged
		}
		return nil
	})
	if err != nil && !errors.Is(err, errHubSettingUnchanged) {
		return err
	}

	legacyRemoved := false
	if global {
		legacyRemoved = removeLegacyGlobalHubSetting(stderr)
	}

	scope := "global"
	var msg string
	switch {
	case global && name == "":
		msg = "No hub is current; the hub list is unchanged."
	case global:
		msg = fmt.Sprintf("Hub %s is no longer the current hub; it stays in the hub list.", describeConfigHub(name, endpoint))
	case name == "":
		scope = "local"
		msg = "This project is not linked to a hub; nothing changed."
	default:
		scope = "local"
		msg = fmt.Sprintf("This project is no longer linked to hub %s.", describeConfigHub(name, endpoint))
	}

	// The hub the next invocation uses: resolveCLIHub still holds this
	// invocation's resolution, so it is recomputed from the saved list.
	now := hubSettingAfterUnset(global, name)
	if v, ok := strings.CutPrefix(now.Source, "env "); ok {
		_, _ = fmt.Fprintf(stderr, "Warning: %s is still set by %s\n", configHubEndpointKey, v)
	}
	if isJSONOutput() {
		return outputJSON(ActionResult{
			Status:  "success",
			Command: "config unset",
			Message: msg,
			Details: map[string]interface{}{
				"key":                    configHubEndpointKey,
				"scope":                  scope,
				"removed":                name != "",
				"hub":                    name,
				"legacy_setting_removed": legacyRemoved,
				"value":                  now.Value,
				"source":                 now.Source,
			},
		})
	}
	_, _ = fmt.Fprintln(out, msg)
	_, _ = fmt.Fprintf(out, "Now: %s\n", formatSettingValue(now))
	return nil
}

// hubSettingAfterSet returns the effective hub.endpoint after config set
// selected the hub name at endpoint as the current hub (global) or linked
// the project to it. The --hub flag and the environment still win over
// both, and a project link wins over the current hub, so the hub just set
// is not always the one that applies (resolveCLIHub's order); for a global
// set, the link of the directory it runs in counts too. overridden reports
// that another hub applies.
func hubSettingAfterSet(global bool, name, endpoint string) (v config.SettingValue, overridden bool) {
	same := func(ep string) bool { return config.NormalizeEndpoint(ep) == config.NormalizeEndpoint(endpoint) }
	h := resolveCLIHub()
	fixed := h.Source == useSourceFlag || h.Source == useSourceEnv || (global && h.Source == useSourceDirectory)
	if fixed && !same(h.Endpoint) {
		return effectiveHubSetting(), true
	}
	// With --global the invocation does not resolve the directory's link,
	// but later commands in this directory do (without --hub or the
	// environment), and the link wins over the current hub. This also holds
	// when --hub or the environment names the hub just set.
	if global && h.Source != useSourceDirectory {
		if c, err := config.LoadCLIContext(); err == nil {
			if ep, linkHub := directoryLinkedHub(targetResolution{stderr: io.Discard}, c); ep != "" && !same(ep) {
				return config.SettingValue{
					Key:    configHubEndpointKey,
					Value:  config.RedactURL(ep),
					Source: fmt.Sprintf("link hub '%s'", linkHub),
				}, true
			}
		}
	}
	if fixed {
		return effectiveHubSetting(), false
	}
	kind := "current hub"
	if !global {
		kind = "link hub"
	}
	return config.SettingValue{
		Key:    configHubEndpointKey,
		Value:  config.RedactURL(endpoint),
		Source: fmt.Sprintf("%s '%s'", kind, name),
	}, false
}

// hubSettingAfterUnset returns the effective hub.endpoint after config
// unset removed the current hub (global) or the project's link to the hub
// named removed. A hub from this invocation's resolution that came from
// what was removed no longer applies; the --hub flag and the environment
// still do, and a project link still wins over a cleared current hub.
func hubSettingAfterUnset(global bool, removed string) config.SettingValue {
	h := resolveCLIHub()
	if removed == "" {
		return effectiveHubSetting()
	}
	if global && h.Source == useSourceContext {
		return config.SettingValue{Key: configHubEndpointKey}
	}
	if !global && h.Source == useSourceDirectory {
		// The current hub, if any, applies again.
		if c, err := config.LoadCLIContext(); err == nil {
			if e := c.CurrentHubEntry(); e != nil && e.Endpoint != "" {
				return config.SettingValue{
					Key:    configHubEndpointKey,
					Value:  config.RedactURL(e.Endpoint),
					Source: fmt.Sprintf("current hub '%s'", c.CurrentHub),
				}
			}
		}
		return config.SettingValue{Key: configHubEndpointKey}
	}
	return effectiveHubSetting()
}
