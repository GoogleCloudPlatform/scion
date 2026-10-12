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
	"strings"
	"text/tabwriter"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubsync"
	"github.com/spf13/cobra"
)

var (
	useClear bool
	useForce bool
)

const projectUseLong = `Show or select the current project: a project on a hub that commands
target when they run outside a scion project directory.

  scion project use             show the effective hub and project and why
  scion project use <project>   select <project> on the effective hub
  scion project use --clear     forget the current project (the hub stays current)

<project> is a slug, name, UUID or git URL, as for --project.

The selection is saved per user in ~/.scion/context.yaml. Flags, environment
variables and the project of the current directory take precedence over it:
inside a scion project directory, commands keep targeting that project.

'scion use' is an alias of 'scion project use'.`

// projectUseCmd is 'scion project use'.
var projectUseCmd = &cobra.Command{
	Use:   "use [project]",
	Short: "Show or select the current project",
	Long:  projectUseLong,
	Args:  cobra.MaximumNArgs(1),
	RunE:  runProjectUse,
}

// useCmd is the top-level 'scion use', an alias of 'scion project use'.
// Cobra aliases cannot cross parents, so it is a second command that shares
// the flags and RunE.
var useCmd = &cobra.Command{
	Use:   "use [project]",
	Short: "Alias of 'scion project use'",
	Long:  projectUseLong,
	Args:  cobra.MaximumNArgs(1),
	RunE:  runProjectUse,
}

func addProjectUseFlags(c *cobra.Command) {
	c.Flags().BoolVar(&useClear, "clear", false, "Forget the current project on the current hub")
	c.Flags().BoolVar(&useForce, "force", false, "Replace an unreadable ~/.scion/context.yaml")
}

func init() {
	addProjectUseFlags(projectUseCmd)
	addProjectUseFlags(useCmd)
	projectCmd.AddCommand(projectUseCmd)
	rootCmd.AddCommand(useCmd)
}

// noHubSelectedMessage is printed by bare 'scion use' when no hub is
// configured. Name 'scion hub add' here once the hub registry exists.
const noHubSelectedMessage = "No hub selected. Start a local hub with 'scion server start', or log in to a hub with 'scion hub auth login --hub-url <url>' and run 'scion use <project>'."

// noHubSelectedError is the error 'scion use <project>' returns when no hub
// is configured: noHubSelectedMessage in Go error style.
const noHubSelectedError = "no hub selected; start a local hub with 'scion server start', or log in to a hub with 'scion hub auth login --hub-url <url>' and run 'scion use <project>'"

func runProjectUse(cmd *cobra.Command, args []string) error {
	if contextDisabled(resolveMode()) {
		return errors.New("'scion use' is not available inside an agent")
	}
	if useClear {
		if len(args) > 0 {
			return errors.New("--clear takes no project")
		}
		return runUseClear(os.Stdout)
	}
	if len(args) == 1 {
		return runUseSelect(os.Stdout, os.Stderr, args[0])
	}
	return runUseShow(os.Stdout)
}

// updateUseContext writes context.yaml for 'scion use', replacing an
// unreadable file only with --force.
func updateUseContext(fn func(*config.CLIContext) error) error {
	update := config.UpdateCLIContext
	if useForce {
		update = config.UpdateCLIContextForce
	}
	err := update(fn)
	if errors.Is(err, config.ErrCLIContextCorrupt) {
		return fmt.Errorf("%w\n\nFix or remove the file, or rerun with --force to replace it", err)
	}
	return err
}

// loadUseContext reads context.yaml for display, warning about an
// unreadable file unless resolveInvocationTarget already did.
func loadUseContext(stderr io.Writer) *config.CLIContext {
	c, err := config.LoadCLIContext()
	if err != nil && !contextWarned {
		_, _ = fmt.Fprintf(stderr, "Warning: ignoring the current context: %v\n", err)
	}
	return c
}

// Source codes of the effective hub and project (JSON "source"), and their
// display labels.
const (
	useSourceFlag            = "flag"
	useSourceEnv             = "env"
	useSourceProjectSettings = "project_settings"
	useSourceDirectory       = "directory"
	useSourceContext         = targetSourceContext
	useSourceGlobalSettings  = "global_settings"
	useSourceGlobal          = "global"
	useSourceNone            = "none"
)

// useTarget is the effective hub and project of the invocation, computed
// from local state only.
type useTarget struct {
	HubEndpoint string
	HubSource   string
	// HubLabel is the display label of HubSource.
	HubLabel string

	ProjectID     string
	ProjectSlug   string
	ProjectName   string
	ProjectSource string
	ProjectLabel  string
	// ProjectDir is the directory project's display path, if any.
	ProjectDir string
}

// describeUseTarget computes the effective target the way
// resolveInvocationTarget and the commands resolve it, without network
// calls.
func describeUseTarget(c *config.CLIContext) useTarget {
	var t useTarget
	envHub, envName := envHubEndpoint()
	dir, inDir := filesystemProject()
	dirIsGlobal := inDir && config.IsGlobalProjectDir(dir)

	switch {
	case hubSource == targetSourceContext:
		t.HubEndpoint, t.HubSource, t.HubLabel = hubEndpoint, useSourceContext, "current context"
	case hubEndpoint != "":
		t.HubEndpoint, t.HubSource, t.HubLabel = hubEndpoint, useSourceFlag, "--hub flag"
	case envHub != "":
		t.HubEndpoint, t.HubSource, t.HubLabel = envHub, useSourceEnv, envName
	case inDir:
		if ep, scope := fileHubEndpoint(dir, dirIsGlobal); ep != "" {
			t.HubEndpoint = ep
			if scope == "project" {
				t.HubSource, t.HubLabel = useSourceProjectSettings, "project settings: "+displayProjectDir(dir)
			} else {
				t.HubSource, t.HubLabel = useSourceGlobalSettings, "global settings"
			}
		}
	default:
		if ep, _ := fileHubEndpoint("", true); ep != "" {
			t.HubEndpoint, t.HubSource, t.HubLabel = ep, useSourceGlobalSettings, "global settings"
		}
	}
	if t.HubEndpoint == "" {
		t.HubSource, t.HubLabel = useSourceNone, "none"
	}

	switch {
	case projectSource == targetSourceContext:
		t.setContextProject(c, &config.ProjectRef{ID: projectPath})
	case globalMode || projectPath == "global" || projectPath == "home":
		t.ProjectSource, t.ProjectLabel = useSourceGlobal, "global project"
		t.ProjectSlug = "global"
		t.ProjectDir = displayProjectDir(dir)
	case projectPath != "" && hubsync.IsHubProjectRef(projectPath):
		t.ProjectSource, t.ProjectLabel = useSourceFlag, "--project flag"
		t.ProjectSlug = projectPath
	case projectPath != "" || (inDir && !dirIsGlobal):
		if projectPath != "" {
			t.ProjectSource, t.ProjectLabel = useSourceFlag, "--project flag"
		} else {
			t.ProjectSource, t.ProjectLabel = useSourceDirectory, "directory: "+displayProjectDir(dir)
		}
		t.ProjectDir = displayProjectDir(dir)
		t.ProjectSlug = config.GetProjectName(dir)
		if s, err := config.LoadSettings(dir); err == nil && s != nil {
			t.ProjectID = s.GetHubProjectID()
			if t.ProjectID == "" {
				t.ProjectID = s.ProjectID
			}
		}
	default:
		if p := c.ProjectFor(t.HubEndpoint); p != nil {
			t.setContextProject(c, p)
		}
	}
	if t.ProjectSource == "" {
		t.ProjectSource, t.ProjectLabel = useSourceNone, "none"
	}
	return t
}

// setContextProject fills the project fields from a context project.
func (t *useTarget) setContextProject(c *config.CLIContext, p *config.ProjectRef) {
	t.ProjectSource, t.ProjectLabel = useSourceContext, "current context"
	t.ProjectID, t.ProjectSlug = p.ID, p.Slug
	hubName, _ := c.HubByEndpoint(t.HubEndpoint)
	for _, kp := range c.Projects {
		if kp.Hub == hubName && kp.ID == p.ID {
			if t.ProjectSlug == "" {
				t.ProjectSlug = kp.Slug
			}
			t.ProjectName = kp.Name
		}
	}
}

// displayProjectDir returns the directory to show for a project directory:
// the directory holding .scion (the workspace, for a project whose settings
// live under ~/.scion/project-configs), with the home directory as "~".
func displayProjectDir(dir string) string {
	if dir == "" {
		return ""
	}
	shown := filepath.Dir(dir)
	if strings.Contains(dir, string(filepath.Separator)+config.ProjectConfigsDir+string(filepath.Separator)) {
		if marker := config.FindProjectMarkerPath(); marker != "" {
			shown = filepath.Dir(marker)
		} else if s, err := config.LoadSettings(dir); err == nil && s != nil && s.WorkspacePath != "" {
			shown = s.WorkspacePath
		}
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if shown == home {
			return "~"
		}
		if rel, err := filepath.Rel(home, shown); err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.Join("~", rel)
		}
	}
	return shown
}

// useHubJSON, useProjectJSON and the types below are the JSON shape of
// bare 'scion use'.
type useHubJSON struct {
	Name     string `json:"name,omitempty"`
	Endpoint string `json:"endpoint,omitempty"`
	Source   string `json:"source"`
}

type useProjectJSON struct {
	ID     string `json:"id,omitempty"`
	Slug   string `json:"slug,omitempty"`
	Name   string `json:"name,omitempty"`
	Path   string `json:"path,omitempty"`
	Source string `json:"source"`
}

type useAuthJSON struct {
	Method string `json:"method"`
	User   string `json:"user,omitempty"`
}

type useEffectiveJSON struct {
	Hub     useHubJSON     `json:"hub"`
	Project useProjectJSON `json:"project"`
	Auth    *useAuthJSON   `json:"auth,omitempty"`
}

type useContextJSON struct {
	Hub     string             `json:"hub"`
	Project *config.ProjectRef `json:"project"`
}

type useShowJSON struct {
	Effective  useEffectiveJSON `json:"effective"`
	Context    *useContextJSON  `json:"context"`
	Overridden bool             `json:"overridden"`
}

// useAuthUser returns the logged-in user to show for info.
func useAuthUser(info authInfo) string {
	if info.OAuthCreds != nil && info.OAuthCreds.User != nil {
		return info.OAuthCreds.User.Email
	}
	return ""
}

// useAuthLine is the human "Auth:" text.
func useAuthLine(info authInfo, endpoint string) string {
	switch info.MethodType {
	case "none":
		return fmt.Sprintf("not logged in; run 'scion hub auth login --hub-url %s'", endpoint)
	case "oauth":
		if u := useAuthUser(info); u != "" {
			return fmt.Sprintf("logged in as %s (OAuth)", u)
		}
		return "logged in (OAuth)"
	default:
		return fmt.Sprintf("%s (%s)", info.Method, info.Source)
	}
}

// runUseShow is bare 'scion use': it reads local state only.
func runUseShow(out io.Writer) error {
	c := loadUseContext(os.Stderr)
	t := describeUseTarget(c)
	hubName, _ := c.HubByEndpoint(t.HubEndpoint)

	var saved *useContextJSON
	if h := c.CurrentHubEntry(); h != nil {
		saved = &useContextJSON{Hub: c.CurrentHub, Project: h.CurrentProject}
	}
	overridden := false
	if saved != nil {
		savedHub := c.Hubs[saved.Hub]
		if !sameHubURL(savedHub.Endpoint, t.HubEndpoint) {
			overridden = true
		} else if saved.Project != nil && saved.Project.ID != t.ProjectID {
			overridden = true
		}
	}

	var info authInfo
	if t.HubEndpoint != "" {
		settings, _ := config.LoadSettings(useSettingsPath())
		info = getAuthInfo(settings, t.HubEndpoint)
	}

	if isJSONOutput() {
		res := useShowJSON{
			Effective: useEffectiveJSON{
				Hub: useHubJSON{Name: hubName, Endpoint: t.HubEndpoint, Source: t.HubSource},
				Project: useProjectJSON{
					ID: t.ProjectID, Slug: t.ProjectSlug, Name: t.ProjectName,
					Path: t.ProjectDir, Source: t.ProjectSource,
				},
			},
			Context:    saved,
			Overridden: overridden,
		}
		if t.HubEndpoint != "" {
			res.Effective.Auth = &useAuthJSON{Method: info.MethodType, User: useAuthUser(info)}
		}
		return outputJSON(res)
	}

	if t.HubEndpoint == "" {
		_, _ = fmt.Fprintln(out, noHubSelectedMessage)
	} else {
		shownHub := hubName
		if shownHub == "" {
			shownHub = "-"
		}
		shownProject, projectValue := "-", ""
		if t.ProjectSource != useSourceNone {
			shownProject = t.ProjectSlug
			if shownProject == "" {
				shownProject = "-"
			}
			projectValue = t.ProjectID
		}
		w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintf(w, "Hub:\t%s\t%s\t(%s)\n", shownHub, t.HubEndpoint, t.HubLabel)
		_, _ = fmt.Fprintf(w, "Project:\t%s\t%s\t(%s)\n", shownProject, projectValue, t.ProjectLabel)
		_ = w.Flush()
		_, _ = fmt.Fprintf(out, "Auth:     %s\n", useAuthLine(info, t.HubEndpoint))
	}
	if overridden {
		project := "-"
		if saved.Project != nil {
			project = saved.Project.Slug
			if project == "" {
				project = saved.Project.ID
			}
		}
		_, _ = fmt.Fprintf(out, "Saved context (applies outside scion project directories): %s / %s\n", saved.Hub, project)
	}
	if envHub, envName := envHubEndpoint(); envHub != "" && saved != nil && !sameHubURL(envHub, c.Hubs[saved.Hub].Endpoint) {
		_, _ = fmt.Fprintf(out, "Note: %s is set and overrides the current hub in this shell.\n", envName)
	}
	return nil
}

// useSettingsPath is the project path whose settings 'scion use' reads for
// the hub: the filesystem project, else the global directory.
func useSettingsPath() string {
	if dir, ok := filesystemProject(); ok {
		return dir
	}
	g, _ := config.GetGlobalDir()
	return g
}

// useSelectJSON is the JSON result of 'scion use <project>'.
type useSelectJSON struct {
	Hub     useHubJSON     `json:"hub"`
	Project useProjectJSON `json:"project"`
}

// useCandidateJSON is one candidate of an ambiguous 'scion use <project>'.
type useCandidateJSON struct {
	Hub      string `json:"hub,omitempty"`
	Endpoint string `json:"endpoint"`
	ID       string `json:"id"`
	Slug     string `json:"slug"`
}

// useHubBaseName returns the nickname a new hub entry for endpoint gets:
// "local" for the local workstation hub, else derived from the host.
var useHubBaseName = func(endpoint string) string {
	if hubsync.IsLocalWorkstationEndpoint(endpoint) {
		return "local"
	}
	return config.DeriveHubName(endpoint)
}

// runUseSelect is 'scion use <project>': it resolves ref on the effective
// hub and saves it as the current project.
func runUseSelect(out, stderr io.Writer, ref string) error {
	settings, err := config.LoadSettings(useSettingsPath())
	if err != nil {
		settings = &config.Settings{}
	}
	endpoint := GetHubEndpoint(settings)
	if endpoint == "" {
		return errors.New(noHubSelectedError)
	}
	if getAuthInfo(settings, endpoint).MethodType == "none" {
		return fmt.Errorf("not logged in to the hub at %s\n\nLog in with: scion hub auth login --hub-url %s", endpoint, endpoint)
	}
	client, err := getHubClient(settings)
	if err != nil {
		return fmt.Errorf("failed to create hub client: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := client.Health(ctx); err != nil {
		return wrapHubError(fmt.Errorf("hub at %s is not responding: %w", endpoint, hubclient.HintProxyError(err)))
	}

	project, err := hubsync.ResolveProjectOnHub(ctx, client, ref)
	var notFound *hubsync.ProjectNotFoundError
	var ambiguous *hubsync.AmbiguousProjectError
	switch {
	case errors.As(err, &notFound):
		return fmt.Errorf("project '%s' not found on the hub at %s", ref, endpoint)
	case errors.As(err, &ambiguous):
		return useAmbiguousError(out, ref, endpoint, ambiguous.Projects)
	case err != nil:
		return wrapHubError(err)
	}

	slug := project.Slug
	var hubName string
	err = updateUseContext(func(c *config.CLIContext) error {
		hubName, _ = c.EnsureHub(endpoint, useHubBaseName(endpoint))
		return c.SelectProject(hubName, config.KnownProject{ID: project.ID, Slug: slug, Name: project.Name}, time.Now())
	})
	if err != nil {
		return err
	}

	shown := slug
	if shown == "" {
		shown = project.Name
	}
	if isJSONOutput() {
		return outputJSON(useSelectJSON{
			Hub:     useHubJSON{Name: hubName, Endpoint: endpoint, Source: useSourceContext},
			Project: useProjectJSON{ID: project.ID, Slug: slug, Name: project.Name, Source: useSourceContext},
		})
	}
	_, _ = fmt.Fprintf(out, "Now using project '%s' on hub '%s' (%s).\n", shown, hubName, endpoint)
	if dir, ok := walkedProject(); ok && projectPath == "" && !globalMode {
		dirHub := mergedHubEndpoint(dir)
		c, _ := config.LoadCLIContext()
		if name, _ := c.HubByEndpoint(dirHub); name != "" {
			dirHub = name
		}
		if dirHub == "" {
			dirHub = "-"
		}
		_, _ = fmt.Fprintf(out, "Note: in this directory, project '%s' (%s, hub '%s') takes precedence; '%s' applies outside scion project directories.\n",
			config.GetProjectName(dir), displayProjectDir(dir), dirHub, shown)
	}
	if envHub, envName := envHubEndpoint(); envHub != "" {
		if hubEndpoint == "" {
			// No --hub and no context fill: the effective hub is the
			// environment's, and it is now the saved current hub.
			_, _ = fmt.Fprintf(stderr, "Note: selected on the hub from %s; it is now the current hub.\n", envName)
		} else if !sameHubURL(envHub, endpoint) {
			_, _ = fmt.Fprintf(stderr, "Note: %s is set and overrides the current hub in this shell.\n", envName)
		}
	}
	return nil
}

// useAmbiguousError reports several projects on one hub matching ref by
// name. It lists their slugs, which select one each.
func useAmbiguousError(out io.Writer, ref, endpoint string, projects []hubclient.Project) error {
	if isJSONOutput() {
		var cands []useCandidateJSON
		for _, p := range projects {
			cands = append(cands, useCandidateJSON{Endpoint: endpoint, ID: p.ID, Slug: p.Slug})
		}
		return outputJSONResult(map[string]interface{}{"error": "ambiguous", "candidates": cands}, true,
			fmt.Sprintf("project '%s' matches several projects on the hub at %s", ref, endpoint))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "project '%s' matches several projects on the hub at %s; choose one by slug:", ref, endpoint)
	for _, p := range projects {
		fmt.Fprintf(&b, "\n  scion use %s   (id %s)", p.Slug, p.ID)
	}
	return errors.New(b.String())
}

// useClearJSON is the JSON result of 'scion use --clear'.
type useClearJSON struct {
	Hub     string             `json:"hub,omitempty"`
	Cleared *config.ProjectRef `json:"cleared"`
}

// errUseNothingToClear stops the context update of 'scion use --clear'
// when there is no project to forget, so the file is not rewritten.
var errUseNothingToClear = errors.New("nothing to clear")

// runUseClear is 'scion use --clear'.
func runUseClear(out io.Writer) error {
	var hubName string
	var cleared *config.ProjectRef
	err := updateUseContext(func(c *config.CLIContext) error {
		hubName, cleared = c.ClearCurrentProject()
		if cleared == nil {
			return errUseNothingToClear
		}
		return nil
	})
	if err != nil && !errors.Is(err, errUseNothingToClear) {
		return err
	}
	if isJSONOutput() {
		return outputJSON(useClearJSON{Hub: hubName, Cleared: cleared})
	}
	switch {
	case hubName == "":
		_, _ = fmt.Fprintln(out, "No hub selected; nothing to clear.")
	case cleared == nil:
		_, _ = fmt.Fprintf(out, "No project selected on hub '%s'.\n", hubName)
	default:
		shown := cleared.Slug
		if shown == "" {
			shown = cleared.ID
		}
		_, _ = fmt.Fprintf(out, "Cleared the current project '%s' on hub '%s'.\n", shown, hubName)
	}
	return nil
}
