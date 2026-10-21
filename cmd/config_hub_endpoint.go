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
//     stays); in a project it removes the project's link entry.
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
// "current hub '<name>'". The name is left out when the hub list cannot be
// read or has no entry for the endpoint.
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
	if c, err := config.LoadCLIContext(); err == nil {
		if name, _ := c.HubByEndpoint(h.Endpoint); name != "" {
			return fmt.Sprintf("%s '%s'", kind, name)
		}
	}
	return kind
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

// printUnreachableHub prints the warning for a hub at endpoint that did not
// respond to the health check of 'hub add' or 'config set hub.endpoint'.
func printUnreachableHub(stderr io.Writer, endpoint string, err error) {
	_, _ = fmt.Fprintf(stderr, "Warning: the hub at %s did not respond: %v\n", endpoint, err)
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
			},
		})
	}
	_, _ = fmt.Fprintln(out, msg)
	return nil
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
		if !c.RemoveLinkEntry(localPath) {
			return errHubSettingUnchanged
		}
		return nil
	})
	if err != nil && !errors.Is(err, errHubSettingUnchanged) {
		return err
	}

	scope := "global"
	var msg string
	switch {
	case global && name == "":
		msg = "No hub is current; nothing changed."
	case global:
		msg = fmt.Sprintf("Hub '%s' (%s) is no longer the current hub; it stays in the hub list.", name, config.RedactURL(endpoint))
	case name == "":
		scope = "local"
		msg = "This project is not linked to a hub; nothing changed."
	default:
		scope = "local"
		msg = fmt.Sprintf("This project is no longer linked to hub '%s' (%s).", name, config.RedactURL(endpoint))
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
				"key":     configHubEndpointKey,
				"scope":   scope,
				"removed": name != "",
				"hub":     name,
				"value":   now.Value,
				"source":  now.Source,
			},
		})
	}
	_, _ = fmt.Fprintln(out, msg)
	_, _ = fmt.Fprintf(out, "Now: %s\n", formatSettingValue(now))
	return nil
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
