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
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
	"github.com/spf13/cobra"
)

// settingsAPIFamilies are the path families of the Hub's settings API: the
// documents that configure the Hub, a project or a runtime broker, and the
// skill lists injected into agents. A route is in the settings API when it
// equals a family or lies below it ("family/..."), unless it is excluded
// below, so a new route inside a family is selected and must be listed.
//
// Out of scope on purpose (not families): the GitHub App configuration
// (/api/v1/github-app), a project's GitHub identity and permissions
// (/api/v1/projects/{id}/git-identity, /github-permissions) and the
// integrations' configuration (/api/v1/admin/integrations/{name}/config),
// which hold credentials and grants rather than settings; pre-start and
// lifecycle hooks (/api/v1/pre-start-hooks, /api/v1/admin/lifecycle-hooks,
// /api/v1/projects/{id}/pre-start-hooks), managed scripts with their own
// lifecycle (create, activate, delete); and shared directories
// (/api/v1/projects/{id}/shared-dirs), which hold files.
//
// Only catalogued routes are seen: a route the catalogue lists as an entry
// point exemption (for example the public settings at
// /api/v1/settings/public) is not checked here.
var settingsAPIFamilies = []string{
	"/api/v1/admin/server-config",
	"/api/v1/admin/project-defaults",
	"/api/v1/admin/experiments",
	"/api/v1/experiments",
	"/api/v1/profiling",
	"/api/v1/admin/messaging",
	"/api/v1/admin/profiling",
	"/api/v1/admin/chat-retention",
	"/api/v1/admin/maintenance",
	"/api/v1/hub/settings",
	"/api/v1/projects/{id}/settings",
	"/api/v1/projects/{id}/messaging-policy",
	"/api/v1/projects/{id}/injected-skills",
	"/api/v1/users/me/injected-skills",
	"/api/v1/runtime-brokers/{id}/settings",
}

// settingsAPIExcluded are the routes inside a settings family that are not
// settings: a route equal to an entry, or, for an entry ending in "/",
// below it.
var settingsAPIExcluded = []string{
	// The maintenance actions (check for updates, migrations, operations,
	// releases, restart, update status) run an action; only the admin mode
	// document at /api/v1/admin/maintenance itself is a setting.
	"/api/v1/admin/maintenance/",
	// A read-only diagnostics report on the messaging configuration.
	"/api/v1/admin/messaging/divergence",
}

// isSettingsAPIRoute reports whether an HTTP route pattern is in the
// settings API.
func isSettingsAPIRoute(pattern string, families, excluded []string) bool {
	for _, e := range excluded {
		if pattern == e || (strings.HasSuffix(e, "/") && strings.HasPrefix(pattern, e)) {
			return false
		}
	}
	for _, f := range families {
		if pattern == f || strings.HasPrefix(pattern, f+"/") {
			return true
		}
	}
	return false
}

// settingsAPIUse records how one settings API entry point is reached: the
// catalogue operation it belongs to, and either the CLI commands that call it
// or, for an entry point with no CLI command, the reason it is web-only.
type settingsAPIUse struct {
	op      authzop.OperationID
	cli     [][]string
	webOnly string
}

const settingsNoCLIYet = "no CLI command yet; ptone/scion#7371"

// settingsMethodAlias is the reason for a PATCH or POST entry point that
// accepts the same document as the PUT the CLI sends.
const settingsMethodAlias = "method alias of PUT; the CLI sends PUT"

// settingsAPICoverage maps every settings API entry point ("METHOD pattern")
// to its CLI commands or a web-only reason. A command is listed only for the
// method it sends. A new settings route fails TestSettingsAPICoverage until
// it is added here, with a command or a reason.
var settingsAPICoverage = map[string]settingsAPIUse{
	"GET /api/v1/admin/server-config":                  {op: "hub.config.read", cli: [][]string{{"hub", "settings", "get"}, {"hub", "settings", "set"}, {"hub", "settings", "unset"}}},
	"GET /api/v1/admin/server-config/schema":           {op: "hub.config.read", cli: [][]string{{"hub", "settings", "schema"}, {"hub", "settings", "get"}, {"hub", "settings", "set"}, {"hub", "settings", "unset"}}},
	"PUT /api/v1/admin/server-config":                  {op: "hub.config.update", cli: [][]string{{"hub", "settings", "set"}, {"hub", "settings", "unset"}}},
	"PATCH /api/v1/admin/server-config":                {op: "hub.config.update", webOnly: settingsMethodAlias},
	"POST /api/v1/admin/server-config":                 {op: "hub.config.update", webOnly: settingsMethodAlias},
	"DELETE /api/v1/admin/server-config/sections/{id}": {op: "hub.config.update", cli: [][]string{{"hub", "settings", "unset"}}},

	"GET /api/v1/admin/project-defaults":   {op: "hub.projectdefaults.read", cli: [][]string{{"hub", "project-defaults", "get"}, {"hub", "project-defaults", "set"}, {"hub", "project-defaults", "unset"}}},
	"PUT /api/v1/admin/project-defaults":   {op: "hub.projectdefaults.update", cli: [][]string{{"hub", "project-defaults", "set"}, {"hub", "project-defaults", "unset"}}},
	"PATCH /api/v1/admin/project-defaults": {op: "hub.projectdefaults.update", webOnly: settingsMethodAlias},
	"POST /api/v1/admin/project-defaults":  {op: "hub.projectdefaults.update", webOnly: settingsMethodAlias},

	"GET /api/v1/admin/experiments":    {op: "hub.experiments.update", cli: [][]string{{"hub", "experiments", "list"}, {"hub", "experiments", "enable"}, {"hub", "experiments", "disable"}, {"hub", "experiments", "reset"}}},
	"PUT /api/v1/admin/experiments":    {op: "hub.experiments.update", cli: [][]string{{"hub", "experiments", "enable"}, {"hub", "experiments", "disable"}, {"hub", "experiments", "reset"}}},
	"DELETE /api/v1/admin/experiments": {op: "hub.experiments.update", cli: [][]string{{"hub", "experiments", "reset"}}},
	"GET /api/v1/experiments":          {op: "hub.experiments.read", cli: [][]string{{"whoami"}}},

	"GET /api/v1/admin/messaging":                {op: "hub.messaging.update", cli: [][]string{{"hub", "messaging", "get"}, {"hub", "messaging", "set"}}},
	"PUT /api/v1/admin/messaging":                {op: "hub.messaging.update", cli: [][]string{{"hub", "messaging", "set"}}},
	"GET /api/v1/projects/{id}/messaging-policy": {op: "project.messagingpolicy.read", cli: [][]string{{"project", "messaging", "get"}, {"project", "messaging", "set"}}},
	"PUT /api/v1/projects/{id}/messaging-policy": {op: "project.messagingpolicy.update", cli: [][]string{{"project", "messaging", "set"}}},

	"GET /api/v1/projects/{id}/settings":          {op: "project.settings.read", cli: [][]string{{"project", "settings", "get"}, {"project", "settings", "set"}, {"project", "settings", "unset"}, {"project", "identity", "default", "set"}, {"project", "identity", "default", "unset"}, {"runtime-broker", "convert"}}},
	"GET /api/v1/projects/{id}/settings/resolved": {op: "project.settings.read", cli: [][]string{{"project", "settings", "get"}, {"project", "identity", "default", "show"}}},
	"PUT /api/v1/projects/{id}/settings":          {op: "project.settings.update", cli: [][]string{{"project", "settings", "set"}, {"project", "settings", "unset"}, {"project", "identity", "default", "set"}, {"project", "identity", "default", "unset"}}},

	"GET /api/v1/projects/{id}/injected-skills":              {op: "project.injectedskills.read", cli: [][]string{{"project", "skills", "list"}, {"project", "skills", "remove"}}},
	"POST /api/v1/projects/{id}/injected-skills":             {op: "project.injectedskills.write", cli: [][]string{{"project", "skills", "add"}}},
	"DELETE /api/v1/projects/{id}/injected-skills/{entryId}": {op: "project.injectedskills.write", cli: [][]string{{"project", "skills", "remove"}}},
	"PUT /api/v1/projects/{id}/injected-skills":              {op: "project.injectedskills.write", webOnly: "replaces the whole list; the CLI edits entries with project skills add and remove"},

	"GET /api/v1/users/me/injected-skills":         {op: "user.skillinjection.read", cli: [][]string{{"user", "skills", "list"}, {"user", "skills", "remove"}}},
	"POST /api/v1/users/me/injected-skills":        {op: "user.skillinjection.update", cli: [][]string{{"user", "skills", "add"}}},
	"DELETE /api/v1/users/me/injected-skills/{id}": {op: "user.skillinjection.update", cli: [][]string{{"user", "skills", "remove"}}},
	"PUT /api/v1/users/me/injected-skills":         {op: "user.skillinjection.update", webOnly: "replaces the whole list; the CLI edits entries with user skills add and remove"},

	"GET /api/v1/runtime-brokers/{id}/settings": {op: "broker.read", cli: [][]string{{"runtime-broker", "convert"}}},
	"PUT /api/v1/runtime-brokers/{id}/settings": {op: "quota.brokersettings.update", webOnly: settingsNoCLIYet},

	"GET /api/v1/hub/settings/injected-skills": {op: "hub.injectedskills.read", webOnly: settingsNoCLIYet},
	"PUT /api/v1/hub/settings/injected-skills": {op: "hub.settings.update", webOnly: settingsNoCLIYet},
	"GET /api/v1/admin/maintenance":            {op: "hub.adminmode.read", webOnly: settingsNoCLIYet},
	"PUT /api/v1/admin/maintenance":            {op: "hub.adminmode.update", webOnly: settingsNoCLIYet},
	"GET /api/v1/admin/profiling":              {op: "hub.profiling.update", webOnly: settingsNoCLIYet},
	"PUT /api/v1/admin/profiling":              {op: "hub.profiling.update", webOnly: settingsNoCLIYet},
	"GET /api/v1/profiling":                    {op: "hub.profiling.read", webOnly: "the resolved readiness-marks switch; the browser takes the same value from the page's initial data, and no command acts on it"},
	"GET /api/v1/admin/chat-retention":         {op: "hub.chatretention.update", webOnly: settingsNoCLIYet},
	"PUT /api/v1/admin/chat-retention":         {op: "hub.chatretention.update", webOnly: settingsNoCLIYet},
}

// settingsAPIEntryPoints returns the settings API entry points of ops, keyed
// "METHOD pattern" (with " [variant]" when the entry point has one), with the
// operation each belongs to.
func settingsAPIEntryPoints(ops []authzop.OperationSpec, families, excluded []string) map[string]authzop.OperationID {
	out := map[string]authzop.OperationID{}
	for _, op := range ops {
		for _, ep := range op.EntryPoints {
			if ep.Kind != authzop.EntryPointHTTPRoute || !isSettingsAPIRoute(ep.Pattern, families, excluded) {
				continue
			}
			key := ep.Method + " " + ep.Pattern
			if ep.Variant != "" {
				key += " [" + ep.Variant + "]"
			}
			out[key] = op.ID
		}
	}
	return out
}

// settingsAPICoverageProblems checks table against the settings API entry
// points of ops; resolve reports whether a CLI command path exists exactly.
func settingsAPICoverageProblems(ops []authzop.OperationSpec, families, excluded []string, table map[string]settingsAPIUse, resolve func([]string) bool) []string {
	var problems []string
	eps := settingsAPIEntryPoints(ops, families, excluded)
	for key, op := range eps {
		use, ok := table[key]
		if !ok {
			problems = append(problems, fmt.Sprintf("%s (operation %s) is a settings API entry point with neither a CLI command nor a web-only exemption: add it to settingsAPICoverage", key, op))
			continue
		}
		if use.op != op {
			problems = append(problems, fmt.Sprintf("%s: the table names operation %s, the catalogue %s", key, use.op, op))
		}
	}
	for key, use := range table {
		if _, ok := eps[key]; !ok {
			problems = append(problems, fmt.Sprintf("%s is in settingsAPICoverage but is not a settings API entry point of the catalogue (stale)", key))
		}
		switch {
		case len(use.cli) > 0 && use.webOnly != "":
			problems = append(problems, fmt.Sprintf("%s has both CLI commands and a web-only reason", key))
		case len(use.cli) == 0 && strings.TrimSpace(use.webOnly) == "":
			problems = append(problems, fmt.Sprintf("%s has no CLI command and no web-only reason", key))
		}
		for _, path := range use.cli {
			if !resolve(path) {
				problems = append(problems, fmt.Sprintf("%s: CLI command %q does not exist", key, strings.Join(path, " ")))
			}
		}
	}
	sort.Strings(problems)
	return problems
}

// resolveCLIPath reports whether path names a command of root exactly (no
// prefix match onto a parent).
func resolveCLIPath(root *cobra.Command, path []string) bool {
	if len(path) == 0 {
		return false
	}
	c, rest, err := root.Find(path)
	return err == nil && len(rest) == 0 && c.CommandPath() == strings.Join(append([]string{root.Name()}, path...), " ")
}

// TestSettingsAPICoverage fails when a Hub settings API entry point in the
// authorization operation catalogue has neither a CLI command nor an explicit
// web-only exemption, or when the table names a command that does not exist.
func TestSettingsAPICoverage(t *testing.T) {
	resolve := func(p []string) bool { return resolveCLIPath(rootCmd, p) }
	for _, p := range settingsAPICoverageProblems(authzop.Catalog, settingsAPIFamilies, settingsAPIExcluded, settingsAPICoverage, resolve) {
		t.Error(p)
	}
	if n := len(settingsAPIEntryPoints(authzop.Catalog, settingsAPIFamilies, settingsAPIExcluded)); n < 30 {
		t.Errorf("found only %d settings API entry points in the catalogue; the family list no longer matches it", n)
	}
}

// TestSettingsAPICoverage_Controls checks that the coverage check reports a
// new route inside a settings family, a wrong operation, a command that does
// not exist, an entry with no command and no reason, an entry with both, and
// a stale entry; that excluded routes stay out; and that a family does not
// match a sibling path that only shares a prefix.
func TestSettingsAPICoverage_Controls(t *testing.T) {
	resolve := func(p []string) bool { return resolveCLIPath(rootCmd, p) }
	planted := append(append([]authzop.OperationSpec{}, authzop.Catalog...), authzop.OperationSpec{
		ID: "project.settings.planted",
		EntryPoints: []authzop.EntryPoint{
			{Kind: authzop.EntryPointHTTPRoute, Pattern: "/api/v1/projects/{id}/settings/history", Method: "GET"},
			{Kind: authzop.EntryPointHTTPRoute, Pattern: "/api/v1/admin/server-config/sections/{id}/x", Method: "GET"},
			{Kind: authzop.EntryPointHTTPRoute, Pattern: "/api/v1/admin/maintenance/planted", Method: "GET"},
			{Kind: authzop.EntryPointHTTPRoute, Pattern: "/api/v1/admin/messaging/divergence", Method: "POST"},
			{Kind: authzop.EntryPointHTTPRoute, Pattern: "/api/v1/projects/{id}/settingsx", Method: "GET"},
		},
	})
	got := strings.Join(settingsAPICoverageProblems(planted, settingsAPIFamilies, settingsAPIExcluded, settingsAPICoverage, resolve), "\n")
	for _, want := range []string{
		"GET /api/v1/projects/{id}/settings/history (operation project.settings.planted) is a settings API entry point",
		"GET /api/v1/admin/server-config/sections/{id}/x (operation project.settings.planted) is a settings API entry point",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("a new route inside a family must be reported: %q in:\n%s", want, got)
		}
	}
	for _, notWant := range []string{"maintenance/planted", "messaging/divergence", "settingsx"} {
		if strings.Contains(got, notWant) {
			t.Errorf("%s must not be selected:\n%s", notWant, got)
		}
	}

	table := map[string]settingsAPIUse{}
	for k, v := range settingsAPICoverage {
		table[k] = v
	}
	table["GET /api/v1/admin/server-config"] = settingsAPIUse{op: "hub.config.read", cli: [][]string{{"hub", "settings", "no-such-command"}}}
	table["GET /api/v1/admin/server-config/schema"] = settingsAPIUse{op: "hub.config.read"}
	table["PUT /api/v1/admin/server-config"] = settingsAPIUse{op: "hub.config.read", cli: [][]string{{"hub", "settings", "set"}}}
	table["GET /api/v1/admin/project-defaults"] = settingsAPIUse{op: "hub.projectdefaults.read", cli: [][]string{{"hub", "project-defaults", "get"}}, webOnly: "x"}
	table["GET /api/v1/no-such-route"] = settingsAPIUse{op: "hub.config.read", webOnly: "x"}
	got = strings.Join(settingsAPICoverageProblems(authzop.Catalog, settingsAPIFamilies, settingsAPIExcluded, table, resolve), "\n")
	for _, want := range []string{
		`CLI command "hub settings no-such-command" does not exist`,
		"GET /api/v1/admin/server-config/schema has no CLI command and no web-only reason",
		"PUT /api/v1/admin/server-config: the table names operation hub.config.read, the catalogue hub.config.update",
		"GET /api/v1/admin/project-defaults has both CLI commands and a web-only reason",
		"GET /api/v1/no-such-route is in settingsAPICoverage but is not a settings API entry point",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("control not detected: %q in:\n%s", want, got)
		}
	}
	if resolveCLIPath(rootCmd, []string{"hub", "settings", "get", "extra"}) {
		t.Error("a path longer than a command must not resolve")
	}
	if resolveCLIPath(rootCmd, []string{"broker", "convert"}) {
		t.Error("an alias must not resolve; name the command by its Use name")
	}
	if resolveCLIPath(rootCmd, []string{}) {
		t.Error("an empty command path must not resolve")
	}
}
