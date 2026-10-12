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
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/credentials"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
)

// The hub registry commands: 'scion hub add|list|rename|remove'. They manage
// the named hubs in ~/.scion/context.yaml. Logins stay in credentials.json,
// keyed by the hub's endpoint, so renaming a hub never touches them.

var (
	hubAddUse        bool
	hubListCheck     bool
	hubRemoveLogout  bool
	hubRegistryForce bool
)

// hubHealthTimeout bounds the health check of 'hub add' and 'hub list
// --check'.
var hubHealthTimeout = 5 * time.Second

var hubAddCmd = &cobra.Command{
	Use:   "add <name> <url>",
	Short: "Add a hub to the hub list",
	Long: `Add a hub to the hub list under a nickname.

The nickname uses lowercase letters, digits and '-' (at most 63 characters).
Each hub URL can be added once. After that, --hub <name> works anywhere a
hub URL is accepted, and 'scion use --hub <name>' makes it the current hub.

The hub's health is checked, but an unreachable hub is still added (with a
warning), so hubs can be added offline.

Example:
  scion hub add prod https://hub.example.com
  scion hub add prod https://hub.example.com --use`,
	Args: cobra.ExactArgs(2),
	RunE: runHubAdd,
}

var hubListCmd = &cobra.Command{
	Use:   "list",
	Short: "List the hubs in the hub list",
	Long: `List the hubs in the hub list, marking the current hub.

AUTH shows how the CLI would authenticate to each hub, from local state
only. --check also checks whether each hub responds.`,
	Args: cobra.NoArgs,
	RunE: runHubList,
}

var hubRenameCmd = &cobra.Command{
	Use:   "rename <old> <new>",
	Short: "Rename a hub in the hub list",
	Long: `Rename a hub in the hub list. The current hub and the projects known on
that hub follow the new name. Logins are unaffected.`,
	Args: cobra.ExactArgs(2),
	RunE: runHubRename,
}

var hubRemoveCmd = &cobra.Command{
	Use:   "remove <name>",
	Short: "Remove a hub from the hub list",
	Long: `Remove a hub from the hub list, together with the projects known on it.
If it was the current hub, no hub is current afterwards.

The stored login for the hub is kept, so the hub can be added again
without logging in; --logout removes it too.`,
	Args: cobra.ExactArgs(1),
	RunE: runHubRemove,
}

func init() {
	hubAddCmd.Flags().BoolVar(&hubAddUse, "use", false, "Also make the hub the current hub")
	hubListCmd.Flags().BoolVar(&hubListCheck, "check", false, "Check whether each hub responds")
	hubRemoveCmd.Flags().BoolVar(&hubRemoveLogout, "logout", false, "Also remove the stored login for the hub")
	for _, c := range []*cobra.Command{hubAddCmd, hubRenameCmd, hubRemoveCmd} {
		c.Flags().BoolVar(&hubRegistryForce, "force", false, "Replace an unreadable ~/.scion/context.yaml")
	}
	hubCmd.AddCommand(hubAddCmd, hubListCmd, hubRenameCmd, hubRemoveCmd)
}

// checkHubRegistryAvailable refuses the hub registry commands inside an
// agent, which never reads or writes the user's hub list.
func checkHubRegistryAvailable() error {
	if contextDisabled(resolveMode()) {
		return errors.New("the hub list is not available inside an agent")
	}
	return nil
}

// updateHubRegistry writes context.yaml for the hub registry commands,
// replacing an unreadable file only with --force.
func updateHubRegistry(fn func(*config.CLIContext) error) error {
	update := config.UpdateCLIContext
	if hubRegistryForce {
		update = config.UpdateCLIContextForce
	}
	err := update(fn)
	if errors.Is(err, config.ErrCLIContextCorrupt) {
		return fmt.Errorf("%w\n\nFix or remove the file, or rerun with --force to replace it", err)
	}
	return err
}

// hubResponds checks the health of the hub at endpoint within
// hubHealthTimeout.
var hubResponds = func(endpoint string) error {
	client, err := hubclient.New(endpoint, hubclient.WithTimeout(hubHealthTimeout))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), hubHealthTimeout)
	defer cancel()
	_, err = client.Health(ctx)
	return hubclient.HintProxyError(err)
}

// hubAuthLabel is the AUTH column of 'hub list' for info, one of the four
// labels: "oauth (<user>)", "dev", "env token" or "none". A token that is
// not a login (SCION_HUB_TOKEN, SCION_AUTH_TOKEN or the agent token file)
// is "env token"; transport-only proxy auth carries no CLI credential and
// is "none".
func hubAuthLabel(info authInfo) string {
	switch {
	case info.MethodType == "oauth":
		if u := useAuthUser(info); u != "" {
			return "oauth (" + u + ")"
		}
		return "oauth"
	case info.IsDevAuth:
		return "dev"
	case info.MethodType == "bearer", info.MethodType == "agent_token":
		return "env token"
	}
	return "none"
}

// hubAuthFor computes, from local state only, how the CLI authenticates to
// endpoint.
func hubAuthFor(endpoint string) authInfo {
	return getAuthInfo(&config.Settings{}, endpoint)
}

// hubAddJSON is the JSON result of 'hub add'.
type hubAddJSON struct {
	Name      string `json:"name"`
	Endpoint  string `json:"endpoint"`
	Current   bool   `json:"current"`
	Reachable bool   `json:"reachable"`
	Auth      string `json:"auth"`
}

func runHubAdd(cmd *cobra.Command, args []string) error {
	if err := checkHubRegistryAvailable(); err != nil {
		return err
	}
	return hubAdd(cmd.OutOrStdout(), cmd.ErrOrStderr(), args[0], args[1], hubAddUse)
}

func hubAdd(out, stderr io.Writer, name, url string, use bool) error {
	var endpoint string
	var current bool
	err := updateHubRegistry(func(c *config.CLIContext) error {
		if err := c.AddHub(name, url); err != nil {
			return err
		}
		endpoint = c.Hubs[name].Endpoint
		if use {
			c.CurrentHub = name
		}
		current = c.CurrentHub == name
		return nil
	})
	if err != nil {
		return err
	}

	healthErr := hubResponds(endpoint)
	info := hubAuthFor(endpoint)
	if isJSONOutput() {
		return outputJSON(hubAddJSON{
			Name: name, Endpoint: endpoint, Current: current,
			Reachable: healthErr == nil, Auth: info.MethodType,
		})
	}
	_, _ = fmt.Fprintf(out, "Added hub '%s' (%s).\n", name, endpoint)
	if use {
		_, _ = fmt.Fprintf(out, "Now using hub '%s'.\n", name)
	}
	if healthErr != nil {
		_, _ = fmt.Fprintf(stderr, "Warning: the hub at %s did not respond: %v\n", endpoint, healthErr)
	}
	if info.MethodType == "none" {
		_, _ = fmt.Fprintf(out, "Not logged in to hub '%s'. Run: scion hub auth login --hub %s\n", name, name)
	}
	return nil
}

// hubListJSON is one entry of the JSON result of 'hub list'.
type hubListJSON struct {
	Current   bool        `json:"current"`
	Name      string      `json:"name"`
	Endpoint  string      `json:"endpoint"`
	Auth      useAuthJSON `json:"auth"`
	Reachable *bool       `json:"reachable,omitempty"`
}

func runHubList(cmd *cobra.Command, args []string) error {
	if err := checkHubRegistryAvailable(); err != nil {
		return err
	}
	return hubList(cmd.OutOrStdout(), cmd.ErrOrStderr(), hubListCheck)
}

func hubList(out, stderr io.Writer, check bool) error {
	c := loadUseContext(stderr)
	names := c.HubNames()

	var reachable []bool
	if check {
		reachable = make([]bool, len(names))
		var wg sync.WaitGroup
		for i, name := range names {
			wg.Add(1)
			go func(i int, endpoint string) {
				defer wg.Done()
				reachable[i] = hubResponds(endpoint) == nil
			}(i, c.Hubs[name].Endpoint)
		}
		wg.Wait()
	}

	rows := make([]hubListJSON, 0, len(names))
	for i, name := range names {
		h := c.Hubs[name]
		info := hubAuthFor(h.Endpoint)
		row := hubListJSON{
			Current:  name == c.CurrentHub,
			Name:     name,
			Endpoint: h.Endpoint,
			Auth:     useAuthJSON{Method: info.MethodType, User: useAuthUser(info)},
		}
		if check {
			r := reachable[i]
			row.Reachable = &r
		}
		rows = append(rows, row)
	}
	if isJSONOutput() {
		return outputJSON(rows)
	}
	if len(rows) == 0 {
		_, _ = fmt.Fprintln(out, "No hubs registered. Add one with 'scion hub add <name> <url>'.")
		return nil
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	header := "CURRENT\tNAME\tENDPOINT\tAUTH"
	if check {
		header += "\tREACHABLE"
	}
	_, _ = fmt.Fprintln(w, header)
	for _, row := range rows {
		mark := ""
		if row.Current {
			mark = "*"
		}
		line := fmt.Sprintf("%s\t%s\t%s\t%s", mark, row.Name, row.Endpoint, hubAuthLabel(hubAuthFor(row.Endpoint)))
		if check {
			line += "\t" + yesNo(*row.Reachable)
		}
		_, _ = fmt.Fprintln(w, line)
	}
	return w.Flush()
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func runHubRename(cmd *cobra.Command, args []string) error {
	if err := checkHubRegistryAvailable(); err != nil {
		return err
	}
	return hubRename(cmd.OutOrStdout(), args[0], args[1])
}

func hubRename(out io.Writer, oldName, newName string) error {
	err := updateHubRegistry(func(c *config.CLIContext) error {
		if c.Hubs[oldName] == nil {
			return unknownHubError(oldName, c)
		}
		return c.RenameHub(oldName, newName)
	})
	if err != nil {
		return err
	}
	if isJSONOutput() {
		return outputJSON(map[string]string{"old": oldName, "new": newName})
	}
	_, _ = fmt.Fprintf(out, "Renamed hub '%s' to '%s'.\n", oldName, newName)
	return nil
}

// hubRemoveJSON is the JSON result of 'hub remove'.
type hubRemoveJSON struct {
	Name       string `json:"name"`
	Endpoint   string `json:"endpoint"`
	WasCurrent bool   `json:"was_current"`
	LoggedOut  bool   `json:"logged_out"`
}

func runHubRemove(cmd *cobra.Command, args []string) error {
	if err := checkHubRegistryAvailable(); err != nil {
		return err
	}
	return hubRemove(cmd.OutOrStdout(), args[0], hubRemoveLogout)
}

func hubRemove(out io.Writer, name string, logout bool) error {
	var removed *config.HubEntry
	var wasCurrent bool
	err := updateHubRegistry(func(c *config.CLIContext) error {
		if c.Hubs[name] == nil {
			return unknownHubError(name, c)
		}
		var err error
		removed, wasCurrent, err = c.RemoveHub(name)
		return err
	})
	if err != nil {
		return err
	}
	if logout {
		if err := credentials.Remove(removed.Endpoint); err != nil {
			return fmt.Errorf("removed hub '%s', but could not remove its login: %w", name, err)
		}
	}
	if isJSONOutput() {
		return outputJSON(hubRemoveJSON{Name: name, Endpoint: removed.Endpoint, WasCurrent: wasCurrent, LoggedOut: logout})
	}
	_, _ = fmt.Fprintf(out, "Removed hub '%s' (%s).\n", name, removed.Endpoint)
	if wasCurrent {
		_, _ = fmt.Fprintln(out, "It was the current hub; pick another with 'scion use --hub <name>' ('scion hub list' shows them).")
	}
	switch {
	case logout:
		_, _ = fmt.Fprintf(out, "Removed the stored login for %s.\n", removed.Endpoint)
	case credentials.IsAuthenticated(removed.Endpoint):
		_, _ = fmt.Fprintf(out, "Note: the stored login for %s was kept, so adding the hub again needs no login; 'scion hub auth logout --hub %s' removes it.\n", removed.Endpoint, removed.Endpoint)
	}
	return nil
}
