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
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/clitime"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
)

var (
	projectListLocal   bool
	projectListRefresh bool
)

var projectListCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List projects across hubs, or the project directories on this machine",
	Long: `List the projects on all registered hubs, each with its hub, together with
the project directories on this machine.

The hub projects come from the known-project list in ~/.scion/context.yaml:
projects selected with 'scion use', and the hub listings fetched with
--refresh. A project directory linked to one of them is shown in the same
row, with its local path; a directory that is not linked to a hub shows
hub '-'. Without --refresh the command makes no network calls.

  --hub <hub>  only the projects on <hub> (a nickname or URL)
  --refresh    fetch the project list of each logged-in hub (or of --hub) first
  --local      list the project directories on this machine instead, with
               their type, agent count and status

Scripts that parse the output of 'scion project list' should use
'scion project list --local', which keeps the format this command had
before it listed projects across hubs, JSON included.

--local flags orphaned project directories (where the workspace no longer
exists). Use 'scion project prune' to clean them up.`,
	RunE: runProjectList,
}

func init() {
	projectListCmd.Flags().BoolVar(&projectListLocal, "local", false, "List the project directories on this machine")
	projectListCmd.Flags().BoolVar(&projectListRefresh, "refresh", false, "Fetch the project list of each logged-in hub first")
	projectCmd.AddCommand(projectListCmd)
}

func runProjectList(cmd *cobra.Command, args []string) error {
	if projectListLocal {
		if projectListRefresh {
			return errors.New("--local cannot be combined with --refresh")
		}
		if hubFlagEndpoint() != "" {
			return errors.New("--local cannot be combined with --hub")
		}
		return runProjectListLocal()
	}
	return runProjectListHubs(cmd)
}

// runProjectListLocal lists the project directories on this machine. Its
// output, text and JSON, is pinned by a golden test: scripts parse it.
func runProjectListLocal() error {
	projects, err := config.DiscoverProjects()
	if err != nil {
		return fmt.Errorf("failed to discover projects: %w", err)
	}

	if isJSONOutput() {
		if projects == nil {
			projects = []config.ProjectInfo{}
		}
		return outputJSON(projects)
	}

	if len(projects) == 0 {
		fmt.Println("No projects found. Run 'scion init' to create one.")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "NAME\tTYPE\tAGENTS\tSTATUS\tWORKSPACE")
	for _, g := range projects {
		workspace := g.WorkspacePath
		if workspace == "" {
			workspace = "-"
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\n",
			g.Name, g.Type, g.AgentCount, g.Status, workspace)
	}
	_ = w.Flush()

	return nil
}

// projectListSourceDirectory is the source of a row that comes only from a
// project directory on this machine.
const projectListSourceDirectory = "directory"

// projectListRow is one row of the list across hubs.
type projectListRow struct {
	Current bool
	// HubName is the hub nickname, or "" when the hub has no entry.
	HubName string
	// Endpoint is the hub endpoint, or "" for a directory not linked to a
	// hub.
	Endpoint  string
	ID        string
	Slug      string
	Name      string
	LocalPath string
	LastUsed  time.Time
	Source    string
}

// hubLabel is the HUB column: the nickname, else the endpoint, else "-".
func (r projectListRow) hubLabel() string {
	switch {
	case r.HubName != "":
		return r.HubName
	case r.Endpoint != "":
		return r.Endpoint
	}
	return "-"
}

// projectListHubJSON and projectListRowJSON are the JSON shape of the list
// across hubs.
type projectListHubJSON struct {
	Name     string `json:"name"`
	Endpoint string `json:"endpoint"`
}

type projectListRowJSON struct {
	Current   bool                `json:"current"`
	Hub       *projectListHubJSON `json:"hub"`
	ID        string              `json:"id"`
	Slug      string              `json:"slug"`
	Name      string              `json:"name"`
	LocalPath string              `json:"local_path"`
	LastUsed  *time.Time          `json:"last_used"`
	Source    string              `json:"source"`
}

// buildProjectListRows merges the known-project list with the project
// directories on this machine. The key is the normalised hub endpoint and
// the hub project ID: a directory linked to a known project adds its local
// path to that row. A directory whose endpoint matches no row joins the row
// of a project ID known on exactly one hub. A linked directory whose hub
// project is not known gets a row of its own, and so does a directory not
// linked to a hub (with no hub). Known entries on a hub that has no hub
// entry are left out.
func buildProjectListRows(c *config.CLIContext, dirs []config.ProjectInfo) []projectListRow {
	var rows []projectListRow
	index := map[[2]string]int{}
	byID := map[string][]int{}
	add := func(r projectListRow) {
		if r.ID != "" {
			index[[2]string{config.NormalizeEndpoint(r.Endpoint), r.ID}] = len(rows)
			byID[r.ID] = append(byID[r.ID], len(rows))
		}
		rows = append(rows, r)
	}

	for _, kp := range c.Projects {
		h := c.Hubs[kp.Hub]
		if h == nil || kp.ID == "" {
			continue
		}
		if i, ok := index[[2]string{config.NormalizeEndpoint(h.Endpoint), kp.ID}]; ok {
			// One row per project: a second entry adds what the first
			// lacks.
			if rows[i].LocalPath == "" {
				rows[i].LocalPath = kp.LocalPath
			}
			if kp.LastUsed.After(rows[i].LastUsed) {
				rows[i].LastUsed = kp.LastUsed
			}
			continue
		}
		add(projectListRow{
			Current:   kp.Hub == c.CurrentHub && h.CurrentProject != nil && h.CurrentProject.ID == kp.ID,
			HubName:   kp.Hub,
			Endpoint:  h.Endpoint,
			ID:        kp.ID,
			Slug:      kp.Slug,
			Name:      kp.Name,
			LocalPath: kp.LocalPath,
			LastUsed:  kp.LastUsed,
			Source:    kp.Source,
		})
	}

	for _, d := range dirs {
		local := projectListLocalPath(d)
		if d.HubProjectID != "" {
			i := -1
			if j, ok := index[[2]string{d.HubEndpoint, d.HubProjectID}]; ok && d.HubEndpoint != "" {
				i = j
			} else if l := byID[d.HubProjectID]; len(l) == 1 {
				// No endpoint in its settings, or one that names no known
				// row (SCION_HUB_ENDPOINT overrides the settings endpoint
				// in this shell): a project ID known on exactly one hub is
				// that project.
				i = l[0]
			}
			if i >= 0 {
				if rows[i].LocalPath == "" {
					rows[i].LocalPath = local
				}
				continue
			}
			name, _ := c.HubByEndpoint(d.HubEndpoint)
			add(projectListRow{
				HubName: name, Endpoint: d.HubEndpoint, ID: d.HubProjectID,
				Slug: d.Name, Name: d.Name, LocalPath: local, Source: projectListSourceDirectory,
			})
			continue
		}
		add(projectListRow{Slug: d.Name, Name: d.Name, LocalPath: local, Source: projectListSourceDirectory})
	}
	return rows
}

// projectListLocalPath is the local path of a project directory: its
// workspace, or the global directory for the global project.
func projectListLocalPath(d config.ProjectInfo) string {
	if d.Type == config.ProjectTypeGlobal {
		return d.ConfigPath
	}
	return d.WorkspacePath
}

// sortProjectListRows orders rows by last use, most recent first, then
// rows never used; ties by hub and slug.
func sortProjectListRows(rows []projectListRow) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if !a.LastUsed.Equal(b.LastUsed) {
			return a.LastUsed.After(b.LastUsed)
		}
		if a.hubLabel() != b.hubLabel() {
			// Rows without a hub go last.
			if a.hubLabel() == "-" || b.hubLabel() == "-" {
				return b.hubLabel() == "-"
			}
			return a.hubLabel() < b.hubLabel()
		}
		return a.Slug < b.Slug
	})
}

// shortenHome shows path with the home directory as "~".
func shortenHome(path string) string {
	if path == "" {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if path == home {
		return "~"
	}
	if rel, err := filepath.Rel(home, path); err == nil && !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel) {
		return filepath.Join("~", rel)
	}
	return path
}

// runProjectListHubs is the default 'scion project list': the projects
// across hubs merged with the project directories on this machine.
func runProjectListHubs(cmd *cobra.Command) error {
	out, stderr := cmd.OutOrStdout(), cmd.ErrOrStderr()
	disabled := contextDisabled(resolveMode())
	if projectListRefresh && disabled {
		return checkHubRegistryAvailable()
	}
	c := &config.CLIContext{Version: config.CLIContextVersion}
	if !disabled {
		c = loadUseContext(stderr)
	}

	filter := hubFlagEndpoint()
	filterName := ""
	if filter != "" {
		filterName, _ = c.HubByEndpoint(filter)
	}
	if projectListRefresh {
		if filter != "" && filterName == "" {
			return fmt.Errorf("hub %s is not in the hub list; add it with 'scion hub add <name> %s' to refresh its projects", filter, filter)
		}
		ctx := cmd.Context()
		if ctx == nil {
			ctx = context.Background()
		}
		refreshed, err := refreshKnownProjects(ctx, stderr, c, filterName)
		if err != nil {
			return err
		}
		c = refreshed
	}

	dirs, err := config.DiscoverProjects()
	if err != nil {
		return fmt.Errorf("failed to discover projects: %w", err)
	}
	rows := buildProjectListRows(c, dirs)
	if filter != "" {
		key := config.NormalizeEndpoint(filter)
		kept := rows[:0]
		for _, r := range rows {
			if r.Endpoint != "" && config.NormalizeEndpoint(r.Endpoint) == key {
				kept = append(kept, r)
			}
		}
		rows = kept
	}
	sortProjectListRows(rows)

	if isJSONOutput() {
		res := make([]projectListRowJSON, 0, len(rows))
		for _, r := range rows {
			j := projectListRowJSON{
				Current: r.Current, ID: r.ID, Slug: r.Slug, Name: r.Name,
				LocalPath: r.LocalPath, Source: r.Source,
			}
			if r.Endpoint != "" {
				j.Hub = &projectListHubJSON{Name: r.HubName, Endpoint: r.Endpoint}
			}
			if !r.LastUsed.IsZero() {
				t := r.LastUsed.UTC()
				j.LastUsed = &t
			}
			res = append(res, j)
		}
		return outputJSON(res)
	}

	if len(rows) == 0 {
		switch {
		case filter != "":
			_, _ = fmt.Fprintf(out, "No projects known on hub '%s'. Run 'scion project list --refresh --hub %s' to fetch its projects.\n", projectListFilterLabel(filter, filterName), projectListFilterLabel(filter, filterName))
		default:
			_, _ = fmt.Fprintln(out, "No projects found. Select one with 'scion use <project>', fetch the projects of your hubs with 'scion project list --refresh', or run 'scion init' to create one.")
		}
		return nil
	}

	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "CURRENT\tHUB\tPROJECT\tNAME\tLOCAL PATH\tLAST USED")
	for _, r := range rows {
		current, local, last := "", "-", "-"
		if r.Current {
			current = "*"
		}
		if r.LocalPath != "" {
			local = shortenHome(r.LocalPath)
		}
		if !r.LastUsed.IsZero() {
			last = clitime.Ago(r.LastUsed)
		}
		slug, name := r.Slug, r.Name
		if slug == "" {
			slug = "-"
		}
		if name == "" {
			name = slug
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", current, r.hubLabel(), slug, name, local, last)
	}
	return w.Flush()
}

// projectListFilterLabel names the --hub filter: its nickname, else the
// value given.
func projectListFilterLabel(filter, name string) string {
	if name != "" {
		return name
	}
	return filter
}

// projectListRefreshTimeout bounds the listing of one hub's projects for
// 'scion project list --refresh'.
var projectListRefreshTimeout = 30 * time.Second

// listAllHubProjects lists every project on the hub that the caller can
// see, following the pages. The hub's global project is left out (see
// isHubGlobalProject).
func listAllHubProjects(ctx context.Context, client hubclient.Client) ([]config.KnownProject, error) {
	var all []config.KnownProject
	opts := &hubclient.ListProjectsOptions{}
	seen := map[string]bool{}
	for {
		resp, err := client.Projects().List(ctx, opts)
		if err != nil {
			return nil, err
		}
		for _, p := range withoutHubGlobalProjects(resp.Projects) {
			all = append(all, config.KnownProject{ID: p.ID, Slug: p.Slug, Name: p.Name})
		}
		next := resp.Page.NextCursor
		if next == "" || seen[next] {
			return all, nil
		}
		seen[next] = true
		opts.Page = apiclient.PageOptions{Cursor: next}
	}
}

// refreshKnownProjects fetches the project list of each registered hub
// that has a usable credential (or only of onlyHub), in parallel, and
// stores each complete listing in the known-project list (see
// config.CLIContext.ReplaceRefreshed). Hubs that are not logged in or do
// not answer keep their cached rows and are named in one warning. It
// returns the updated context.
func refreshKnownProjects(ctx context.Context, stderr io.Writer, c *config.CLIContext, onlyHub string) (*config.CLIContext, error) {
	settings, err := config.LoadSettings(useSettingsPath())
	if err != nil {
		settings = &config.Settings{}
	}
	var names []string
	for _, name := range c.HubNames() {
		if onlyHub == "" || name == onlyHub {
			names = append(names, name)
		}
	}
	type result struct {
		projects []config.KnownProject
		err      error
		skipped  bool
	}
	results := make([]result, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		endpoint := c.Hubs[name].Endpoint
		// A hub gets only a credential bound to it unless it was given with
		// --hub or is the effective hub (hubGetsFullChain).
		fanOut := !hubGetsFullChain(settings, endpoint, onlyHub != "")
		if !hubUsable(settings, endpoint, fanOut) {
			results[i].skipped = true
			continue
		}
		wg.Add(1)
		go func(i int, endpoint string, fanOut bool) {
			defer wg.Done()
			client, err := hubClientFor(settings, endpoint, fanOut)
			if err != nil {
				results[i].err = err
				return
			}
			ctx, cancel := context.WithTimeout(ctx, projectListRefreshTimeout)
			defer cancel()
			results[i].projects, results[i].err = listAllHubProjects(ctx, client)
		}(i, endpoint, fanOut)
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	listed := map[string][]config.KnownProject{}
	var skipped []string
	for i, name := range names {
		r := results[i]
		switch {
		case r.skipped:
			skipped = append(skipped, name+": not logged in")
		case apiclient.IsUnauthorizedError(r.err):
			skipped = append(skipped, name+": login refused")
		case r.err != nil:
			skipped = append(skipped, fmt.Sprintf("%s: unreachable: %v", name, hubclient.HintProxyError(r.err)))
		default:
			listed[name] = r.projects
		}
	}
	if len(skipped) > 0 {
		_, _ = fmt.Fprintf(stderr, "Warning: not refreshed: %s\n", strings.Join(skipped, "; "))
	}
	if len(listed) == 0 {
		return c, nil
	}
	now := time.Now()
	err = config.UpdateCLIContext(func(c *config.CLIContext) error {
		for _, name := range names {
			if projects, ok := listed[name]; ok && c.Hubs[name] != nil {
				c.ReplaceRefreshed(name, projects, now)
			}
		}
		return nil
	})
	if errors.Is(err, config.ErrCLIContextCorrupt) {
		return nil, fmt.Errorf("%w\n\nFix or remove the file", err)
	}
	if err != nil {
		return nil, err
	}
	return config.LoadCLIContext()
}
